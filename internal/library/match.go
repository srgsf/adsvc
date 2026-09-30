package library

import (
	"math"
	"sync"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

// maxCells bounds the dense tally (2 bytes a cell). A bigger index votes in a map instead.
// A var so that tests can take the map path.
var maxCells uint64 = 1 << 28

// tally is a dense vote accumulator for one Match over the CSR. Ad d owns a run of cells,
// one per offset a query can align it at, with a guard cell at each end so that the ±1
// neighbours of a cell always belong to the same ad. Voting is an array increment instead
// of a map insert (the map's growth was most of Match), and only touched cells are read and
// reset. At 10k ads of 30 s a tally is about 27 MB; there is one per concurrent Match.
type tally struct {
	cells   []uint16
	touched []uint64 // dense<<32 | cell of every cell's first vote
}

var tallies = sync.Pool{New: func() any { return new(tally) }}

// match appends to out the local maxima of the votes of q on c that reach minScore,
// skipping dead ads.
func (c *csr) match(q []fingerprint.Point, dead []bool, minScore int, out []Match) []Match {
	c.each(q, dead, minScore, func(m Match) { out = append(out, m) })
	return out
}

// each calls fn with every local maximum of the votes of q on c that reaches minScore,
// skipping dead ads.
func (c *csr) each(q []fingerprint.Point, dead []bool, minScore int, fn func(Match)) {
	if len(c.ids) == 0 || len(q) == 0 {
		return
	}
	qmin, qmax := q[0].T, q[0].T
	for _, p := range q[1:] {
		qmin, qmax = min(qmin, p.T), max(qmax, p.T)
	}
	// Ad d's cells start at start[d] + d*margin; offset o (ad frame - query frame) is cell
	// base + o + shift, with o in [-qmax, span-1-qmin].
	margin := uint64(qmax-qmin) + 2
	shift := int64(qmax) + 1
	n := uint64(c.start[len(c.ids)]) + uint64(len(c.ids))*margin
	if n > maxCells {
		votes := c.votes(q, dead)
		eachPeak(votes, c.ids, minScore, fn)
		putVotes(votes)
		return
	}

	t := tallies.Get().(*tally)
	defer tallies.Put(t)
	if uint64(len(t.cells)) < n {
		t.cells = make([]uint16, n)
	}
	cells, touched := t.cells, t.touched[:0]
	for _, p := range q {
		for _, e := range c.bucket(p.H) {
			d := e >> tBits
			if int(d) >= len(c.ids) || e&tMask >= c.start[d+1]-c.start[d] || dead != nil && dead[d] {
				continue // out of range: a damaged file (validate does not read the postings)
			}
			i := int64(c.start[d]) + int64(d)*int64(margin) + int64(e&tMask) + shift - int64(p.T)
			switch cells[i] {
			case 0:
				touched = append(touched, uint64(d)<<32|uint64(i))
			case math.MaxUint16:
				continue
			}
			cells[i]++
		}
	}
	for _, k := range touched {
		d, i := uint32(k>>32), uint32(k)
		v, prev, next := cells[i], cells[i-1], cells[i+1]
		// local maximum only (ties broken towards the lower offset)
		if prev >= v || next > v {
			continue
		}
		if s := int(v) + int(prev) + int(next); s >= minScore {
			base := int64(c.start[d]) + int64(d)*int64(margin)
			fn(Match{AdID: c.ids[d], Offset: int32(int64(i) - base - shift), Score: s})
		}
	}
	for _, k := range touched {
		cells[uint32(k)] = 0
	}
	t.touched = touched
}

// votes is the map form of the tally, for an index too big for one. Keys are
// key(dense index, offset).
func (c *csr) votes(q []fingerprint.Point, dead []bool) map[uint64]int {
	votes := getVotes()
	for _, p := range q {
		for _, e := range c.bucket(p.H) {
			d := e >> tBits
			if int(d) >= len(c.ids) || dead != nil && dead[d] {
				continue
			}
			votes[key(d, int32(e&tMask)-p.T)]++
		}
	}
	return votes
}

// overlayVotes votes q on the overlay. Keys are key(overlay index, offset).
// The caller returns the map with putVotes.
func overlayVotes(idx map[uint32][]occ, q []fingerprint.Point) map[uint64]int {
	votes := getVotes()
	for _, p := range q {
		for _, o := range idx[p.H] {
			votes[key(o.ad, o.t-p.T)]++
		}
	}
	return votes
}

// peaks appends to out the local maxima of votes that reach minScore; ids maps the ad
// index of a key to its ID.
func peaks(votes map[uint64]int, ids []fingerprint.ID, minScore int, out []Match) []Match {
	eachPeak(votes, ids, minScore, func(m Match) { out = append(out, m) })
	return out
}

// eachPeak calls fn with every local maximum of votes that reaches minScore.
func eachPeak(votes map[uint64]int, ids []fingerprint.ID, minScore int, fn func(Match)) {
	for k, v := range votes {
		ad, off := uint32(k>>32), int32(uint32(k))
		prev, next := votes[key(ad, off-1)], votes[key(ad, off+1)]
		// local maximum only (ties broken towards the lower offset)
		if prev >= v || next > v {
			continue
		}
		if s := v + prev + next; s >= minScore {
			fn(Match{AdID: ids[ad], Offset: off, Score: s})
		}
	}
}

// voteMaps are the maps of the map-vote paths (the overlay, an index too big for a
// tally): a map grown for one Match keeps its buckets for the next, rather than growing
// again from nothing.
var voteMaps = sync.Pool{New: func() any { return map[uint64]int{} }}

func getVotes() map[uint64]int { return voteMaps.Get().(map[uint64]int) }

func putVotes(m map[uint64]int) {
	clear(m)
	voteMaps.Put(m)
}

func key(ad uint32, off int32) uint64 { return uint64(ad)<<32 | uint64(uint32(off)) }
