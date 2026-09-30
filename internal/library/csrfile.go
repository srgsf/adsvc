package library

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

// Index file: the csr arrays at 8-byte aligned offsets, little-endian, after a header.
// Mapped read-only, the index lives in the page cache rather than the Go heap.
//
//	header   magic, format, fp version, hash bits, low bits, ads, distinct hashes,
//	         postings, digest of the ad set, build time
//	ids      n × 16 B       sizes  n × u32      start  (n+1) × u32
//	top      (2^16+1) × u32 post   p × u32      low    d × u8      off  (d+1) × u32
//
// The postings come before low and off because they are written first, through a
// writable mapping, while d is not known yet.
const (
	csrMagic   = "ADSVCSR1"
	csrFormat  = 1
	headerSize = 80
)

// littleEndian: the index file is used in place, so the host must share its byte order
// (every target of adsvc does). Elsewhere the index stays on the heap.
var littleEndian = binary.NativeEndian.Uint16([]byte{1, 0}) == 1

type header struct {
	fpVersion         uint32
	ads, distinct     uint32
	postings          uint64
	digest            [32]byte
	builtAt           int64
	hashBits, lowBits uint32
}

type layout struct{ ids, sizes, start, top, post, low, off, end uint64 }

func align8(v uint64) uint64 { return (v + 7) &^ 7 }

func layoutFor(n, d, p uint64) layout {
	var l layout
	l.ids = headerSize
	l.sizes = align8(l.ids + 16*n)
	l.start = align8(l.sizes + 4*n)
	l.top = align8(l.start + 4*(n+1))
	l.post = align8(l.top + 4*(1<<topBits+1))
	l.low = align8(l.post + 4*p)
	l.off = align8(l.low + d)
	l.end = l.off + 4*(d+1)
	return l
}

// setDigest identifies a set of ads (and the index format), so that an index file can be
// reused when the set has not changed.
func setDigest(ids []fingerprint.ID) [32]byte {
	h := sha256.New()
	var b [8]byte
	binary.LittleEndian.PutUint32(b[:4], csrFormat)
	binary.LittleEndian.PutUint32(b[4:], fingerprint.Version)
	h.Write([]byte(csrMagic))
	h.Write(b[:])
	for _, id := range ids {
		h.Write(id[:])
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d
}

func u32View(b []byte) []uint32 {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(&b[0])), len(b)/4) //nolint:gosec // G103: views of index sections, sized and 8-byte aligned by layoutFor
}

func idView(b []byte) []fingerprint.ID {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Slice((*fingerprint.ID)(unsafe.Pointer(&b[0])), len(b)/16) //nolint:gosec // G103: views of index sections, sized and 8-byte aligned by layoutFor
}

func u32Bytes(s []uint32) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), 4*len(s)) //nolint:gosec // G103: views of index sections, sized and 8-byte aligned by layoutFor
}

func idBytes(s []fingerprint.ID) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), 16*len(s)) //nolint:gosec // G103: views of index sections, sized and 8-byte aligned by layoutFor
}

func mappable(size uint64) error {
	if size > math.MaxInt || (unsafe.Sizeof(uintptr(0)) == 4 && size > 1<<31) {
		return fmt.Errorf("index of %d bytes does not fit the address space", size)
	}
	return nil
}

// writeCSR builds the index of ids from src into the file path (through a temporary file
// and a rename) and returns it opened.
func writeCSR(path string, ids []fingerprint.ID, src adSource) (c *csr, dropped int, err error) {
	if !littleEndian {
		return nil, 0, errors.New("index files need a little-endian host")
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
		if err != nil {
			os.Remove(tmp)
		}
	}()
	n := uint64(len(ids))
	var m []byte
	c, dropped, err = buildCSR(ids, src, func(p int) ([]uint32, error) {
		l := layoutFor(n, 0, uint64(p))
		if err := mappable(l.low); err != nil {
			return nil, err
		}
		if err := preallocate(f, int64(l.low)); err != nil {
			return nil, err
		}
		var err error
		if m, err = mapFile(f, int(l.low), true); err != nil {
			return nil, err
		}
		return u32View(m[l.post : l.post+4*uint64(p)]), nil // not to l.low: that is padded
	})
	if m != nil {
		if err == nil {
			l := layoutFor(n, uint64(len(c.low)), uint64(len(c.post)))
			h := header{fpVersion: fingerprint.Version, ads: uint32(n), distinct: uint32(len(c.low)), postings: uint64(len(c.post)),
				digest: setDigest(ids), hashBits: fingerprint.HashBits, lowBits: lowBits, builtAt: time.Now().UnixMilli()}
			putHeader(m[:headerSize], h)
			copy(m[l.ids:], idBytes(c.ids))
			copy(m[l.sizes:], u32Bytes(c.sizes))
			copy(m[l.start:], u32Bytes(c.start))
			copy(m[l.top:], u32Bytes(c.top))
		}
		err = errors.Join(err, unmapFile(f, m, true))
	}
	if err != nil {
		return nil, 0, err
	}
	l := layoutFor(n, uint64(len(c.low)), uint64(len(c.post)))
	if _, err = f.WriteAt(c.low, int64(l.low)); err != nil {
		return nil, 0, err
	}
	if _, err = f.WriteAt(u32Bytes(c.off), int64(l.off)); err != nil {
		return nil, 0, err
	}
	if err = f.Sync(); err != nil {
		return nil, 0, err
	}
	if err = f.Close(); err != nil {
		f = nil
		return nil, 0, err
	}
	f = nil
	if err = os.Rename(tmp, path); err != nil {
		return nil, 0, err
	}
	syncDir(filepath.Dir(path))
	c, h, err := openCSR(path)
	if err != nil {
		return nil, 0, fmt.Errorf("reopening the new index: %w", err)
	}
	if h.digest != setDigest(ids) {
		c.release()
		return nil, 0, errors.New("new index: digest mismatch")
	}
	return c, dropped, nil
}

// preallocate sizes f to size bytes with its blocks allocated. The postings are written
// through a mapping, where a full disk is a SIGBUS that kills the process rather than an
// error: a sparse file (Truncate alone) would only allocate on the first write of a page.
func preallocate(f *os.File, size int64) error {
	if ok, err := fallocate(f, size); ok || err != nil {
		return err
	}
	zeros := make([]byte, 1<<20)
	for off := int64(0); off < size; off += int64(len(zeros)) {
		if _, err := f.WriteAt(zeros[:min(int64(len(zeros)), size-off)], off); err != nil {
			return err
		}
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync() //nolint:errcheck // best effort: not supported everywhere
		d.Close()
	}
}

func putHeader(b []byte, h header) {
	le := binary.LittleEndian
	copy(b[0:8], csrMagic)
	le.PutUint32(b[8:], csrFormat)
	le.PutUint32(b[12:], h.fpVersion)
	le.PutUint32(b[16:], h.hashBits)
	le.PutUint32(b[20:], h.lowBits)
	le.PutUint32(b[24:], h.ads)
	le.PutUint32(b[28:], h.distinct)
	le.PutUint64(b[32:], h.postings)
	copy(b[40:72], h.digest[:])
	le.PutUint64(b[72:], uint64(h.builtAt))
}

var errBadIndex = errors.New("not an index file of this adsvc")

// openCSR maps the index file at path. It checks the layout but not the contents: call
// validate before using an index that is not known to be good.
func openCSR(path string) (*csr, header, error) {
	var h header
	if !littleEndian {
		return nil, h, errors.New("index files need a little-endian host")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, h, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, h, err
	}
	size := uint64(fi.Size())
	if size < headerSize {
		return nil, h, errBadIndex
	}
	if err := mappable(size); err != nil {
		return nil, h, err
	}
	m, err := mapFile(f, int(size), false)
	if err != nil {
		return nil, h, err
	}
	le := binary.LittleEndian
	h = header{
		fpVersion: le.Uint32(m[12:]), hashBits: le.Uint32(m[16:]), lowBits: le.Uint32(m[20:]),
		ads: le.Uint32(m[24:]), distinct: le.Uint32(m[28:]), postings: le.Uint64(m[32:]),
		builtAt: int64(le.Uint64(m[72:])),
	}
	copy(h.digest[:], m[40:72])
	l := layoutFor(uint64(h.ads), uint64(h.distinct), h.postings)
	if string(m[:8]) != csrMagic || le.Uint32(m[8:]) != csrFormat || h.hashBits != fingerprint.HashBits ||
		h.lowBits != lowBits || h.ads > MaxAds || h.postings > 1<<32-1 || h.distinct > 1<<fingerprint.HashBits || l.end != size {
		unmapFile(f, m, false) //nolint:errcheck // already failing
		return nil, h, errBadIndex
	}
	n := uint64(h.ads)
	c := &csr{
		ids:    idView(m[l.ids : l.ids+16*n]),
		sizes:  u32View(m[l.sizes : l.sizes+4*n]),
		start:  u32View(m[l.start : l.start+4*(n+1)]),
		top:    u32View(m[l.top : l.top+4*(1<<topBits+1)]),
		post:   u32View(m[l.post : l.post+4*h.postings]),
		low:    m[l.low : l.low+uint64(h.distinct)],
		off:    u32View(m[l.off:l.end]),
		mapped: m,
	}
	return c, h, nil
}
