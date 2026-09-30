package library

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

// Posting layout: a posting is one landmark of one ad, packed in a uint32 as the ad's dense
// index (its position in csr.ids) and the landmark's frame within the ad.
const (
	tBits  = 14
	tMask  = 1<<tBits - 1
	adBits = 32 - tBits

	// MaxAds is the number of ads an index can hold.
	MaxAds = 1 << adBits
	// MaxDuration is the longest ad an index can hold (frames fit in tBits).
	MaxDuration = tMask * fingerprint.FrameDur
)

// Hash lookup is two-level: the top bits of a hash select a range of distinct hashes in
// csr.low (sorted by their low bits), which gives the postings range. A flat table over
// every 24-bit hash would cost 64 MB whatever the library size; this costs 5 bytes per
// distinct hash plus 256 KB.
const (
	lowBits = 8
	topBits = fingerprint.HashBits - lowBits
)

var (
	errTooManyAds = fmt.Errorf("more than %d ads", MaxAds)
	// errChanged means the ads changed between the two passes of a build.
	errChanged = errors.New("ads changed during the index build")
)

// csr is an immutable inverted index in compressed sparse row form. Its slices are on the
// heap or views of a mapped index file (see csrfile.go).
type csr struct {
	ids   []fingerprint.ID // dense index -> ad ID, ascending
	sizes []uint32         // dense index -> postings of that ad
	start []uint32         // len(ids)+1 entries: prefix sums of each ad's span (last frame + 1)
	top   []uint32         // 1<<topBits + 1 entries: hashes with top bits i are low[top[i]:top[i+1]]
	low   []uint8          // low bits of each distinct hash, ascending within a top range
	off   []uint32         // len(low)+1 entries: distinct hash k has postings post[off[k]:off[k+1]]
	post  []uint32         // dense<<tBits | frame, ascending (by ad, then frame) within a hash

	digest [32]byte // setDigest of ids
	mapped []byte   // the file mapping the slices view, if any

	built time.Time     // when it was built
	took  time.Duration // how long the build took (0 if loaded from a file)
}

// release unmaps the index file. The csr must not be used afterwards.
func (c *csr) release() {
	if c == nil || c.mapped == nil {
		return
	}
	b := c.mapped
	c.mapped = nil
	if err := unmapFile(nil, b, false); err != nil {
		slog.Warn("unmapping the ad index", "err", err) // leaks the mapping, nothing else
	}
}

// bucket returns the postings of hash h.
func (c *csr) bucket(h uint32) []uint32 {
	t := h >> lowBits
	if int(t) >= len(c.top)-1 {
		return nil
	}
	lo, hi := c.top[t], c.top[t+1]
	k, ok := slices.BinarySearch(c.low[lo:hi], uint8(h))
	if !ok {
		return nil
	}
	k += int(lo)
	return c.post[c.off[k]:c.off[k+1]]
}

// dense returns the dense index of ad id.
func (c *csr) dense(id fingerprint.ID) (uint32, bool) {
	d, ok := slices.BinarySearchFunc(c.ids, id, compareID)
	return uint32(d), ok
}

func compareID(a, b fingerprint.ID) int { return bytes.Compare(a[:], b[:]) }

func fits(p fingerprint.Point) bool {
	return p.H>>fingerprint.HashBits == 0 && p.T >= 0 && p.T <= tMask
}

// adSource visits the landmarks of the ads being indexed: it calls fn(d, pts) for ad
// ids[d], in increasing d (ads without landmarks may be left out). A build visits it
// twice, and both visits must see the same ads.
type adSource func(fn func(d int, pts []fingerprint.Point) error) error

// buildCSR indexes the ads ids from src. alloc, if not nil, provides the postings array
// (a view of the index file being written); otherwise it is allocated on the heap. Other
// arrays are small (a few bytes per ad and per distinct hash) and on the heap; the only
// other large allocation is one byte per posting until the build ends. Landmarks outside
// the posting layout (frames beyond MaxDuration, which Add refuses) are left out and
// counted in dropped.
func buildCSR(ids []fingerprint.ID, src adSource, alloc func(n int) ([]uint32, error)) (c *csr, dropped int, err error) {
	if len(ids) > MaxAds {
		return nil, 0, errTooManyAds
	}
	c = &csr{
		ids:   ids,
		sizes: make([]uint32, len(ids)),
		start: make([]uint32, len(ids)+1),
		top:   make([]uint32, 1<<topBits+1),
	}

	// Pass 1: count postings per top range and per ad, and each ad's span.
	var n uint64
	if err := src(func(d int, pts []fingerprint.Point) error {
		span := uint32(0)
		for _, p := range pts {
			if !fits(p) {
				dropped++
				continue
			}
			c.top[p.H>>lowBits+1]++
			c.sizes[d]++
			span = max(span, uint32(p.T)+1)
		}
		n += uint64(c.sizes[d])
		c.start[d+1] = span
		return nil
	}); err != nil {
		return nil, 0, err
	}
	var frames uint64
	for d := range ids {
		frames += uint64(c.start[d+1])
		if frames > 1<<32-1 {
			return nil, 0, errors.New("more than 2^32 frames")
		}
		c.start[d+1] = uint32(frames)
	}
	if n > 1<<32-1 {
		return nil, 0, errors.New("more than 2^32 landmarks")
	}
	for i := 1; i < len(c.top); i++ {
		c.top[i] += c.top[i-1]
	}
	if alloc == nil {
		c.post = make([]uint32, n)
	} else if c.post, err = alloc(int(n)); err != nil {
		return nil, 0, err
	}

	// Pass 2: counting sort by the top bits (scatter). Ads and their landmarks are visited
	// in (ad, frame) order and the scatter is stable, so every range stays sorted.
	low := make([]uint8, n) // low bits of each posting's hash, until the ranges are sorted
	next := slices.Clone(c.top[:len(c.top)-1])
	if err := src(func(d int, pts []fingerprint.Point) error {
		for _, p := range pts {
			if !fits(p) {
				continue
			}
			t := p.H >> lowBits
			i := next[t]
			if i >= c.top[t+1] {
				return errChanged
			}
			c.post[i], low[i] = uint32(d)<<tBits|uint32(p.T), uint8(p.H)
			next[t]++
		}
		return nil
	}); err != nil {
		return nil, 0, err
	}
	for t, i := range next {
		if i != c.top[t+1] {
			return nil, 0, errChanged
		}
	}

	// Within each top range, a stable counting sort by the low bits gives the distinct
	// hashes and their offsets. The first sweep only counts distinct hashes, so that low and
	// off are allocated at their exact size.
	var hist [1 << lowBits]uint32
	distinct := 0
	for t := range len(c.top) - 1 {
		clear(hist[:])
		for _, b := range low[c.top[t]:c.top[t+1]] {
			hist[b]++
		}
		for _, v := range hist {
			if v > 0 {
				distinct++
			}
		}
	}
	c.low = make([]uint8, 0, distinct)
	c.off = make([]uint32, 0, distinct+1)
	var scratch []uint32
	var at [1 << lowBits]uint32 // next position per low bits; only set entries are read
	for t := range len(c.top) - 1 {
		start, end := c.top[t], c.top[t+1]
		c.top[t] = uint32(len(c.low))
		if start == end {
			continue
		}
		seg, post := low[start:end], c.post[start:end]
		clear(hist[:])
		for _, b := range seg {
			hist[b]++
		}
		pos := uint32(0)
		for b, v := range hist {
			if v == 0 {
				continue
			}
			at[b] = pos
			c.low = append(c.low, uint8(b))
			c.off = append(c.off, start+pos)
			pos += v
		}
		scratch = append(scratch[:0], post...)
		for i, b := range seg {
			post[at[b]] = scratch[i]
			at[b]++
		}
	}
	c.top[len(c.top)-1] = uint32(len(c.low))
	c.off = append(c.off, uint32(n))
	return c, dropped, nil
}

// validate checks every invariant Match relies on, so that a corrupt index file cannot
// make it index out of range.
func (c *csr) validate() error {
	n := len(c.ids)
	switch {
	case n > MaxAds:
		return errTooManyAds
	case len(c.sizes) != n || len(c.start) != n+1 || len(c.top) != 1<<topBits+1 || len(c.off) != len(c.low)+1:
		return errors.New("section sizes")
	case c.start[0] != 0 || c.top[0] != 0 || c.off[0] != 0:
		return errors.New("sections do not start at 0")
	case int(c.top[len(c.top)-1]) != len(c.low) || int(c.off[len(c.off)-1]) != len(c.post):
		return errors.New("sections do not end at their size")
	}
	for d := 1; d < n; d++ {
		if compareID(c.ids[d-1], c.ids[d]) >= 0 {
			return errors.New("ad ids not ascending")
		}
	}
	for d := range n {
		if c.start[d+1] < c.start[d] || c.start[d+1]-c.start[d] > tMask+1 {
			return fmt.Errorf("span of ad %d", d)
		}
	}
	// Ranges first: with them ascending and ending at len(low), every range is in bounds.
	for t := range len(c.top) - 1 {
		if c.top[t+1] < c.top[t] {
			return errors.New("top ranges not ascending")
		}
	}
	for t := range len(c.top) - 1 {
		lo, hi := c.top[t], c.top[t+1]
		for k := lo + 1; k < hi; k++ {
			if c.low[k] <= c.low[k-1] {
				return errors.New("hashes not ascending")
			}
		}
	}
	for k := 1; k < len(c.off); k++ {
		if c.off[k] <= c.off[k-1] {
			return errors.New("empty or reversed hash")
		}
	}
	return nil
}

// validatePostings checks every posting against its ad (validate leaves them out: at 10k
// ads that is reading the whole file, at every start, for a file this process wrote and
// whose digest matched). Match skips a posting out of range instead.
func (c *csr) validatePostings() error {
	n := len(c.ids)
	counts := make([]uint32, n)
	for _, e := range c.post {
		d := e >> tBits
		if int(d) >= n || e&tMask >= c.start[d+1]-c.start[d] {
			return errors.New("posting out of range")
		}
		counts[d]++
	}
	if !slices.Equal(counts, c.sizes) {
		return errors.New("postings per ad")
	}
	return nil
}

// file is path when c lives in the index file, "" when it is on the heap.
func (c *csr) file(path string) string {
	if c.mapped == nil {
		return ""
	}
	return path
}
