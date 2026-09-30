package demux

import (
	"encoding/binary"
	"slices"
)

// capture collects one object - a RIFF chunk, an MP4 box - that starts at a known offset,
// from whatever byte ranges happen to pass by. The object's total size is read from its
// own header once the first bytes have arrived. Only bytes that extend what has been
// collected contiguously are used.
type capture struct {
	off  int64
	want int64 // total size including the header; 0 until the header has been read
	buf  []byte
	done bool // complete and handed over, or given up
	// size returns the total size from the header bytes: ok=false asks for more bytes,
	// total<0 rejects the object.
	size func(hdr []byte) (total int64, ok bool)
	max  int64
}

// feed offers the bytes at absolute offset off and reports whether the object is complete.
func (c *capture) feed(off int64, p []byte) bool {
	if c.done {
		return false
	}
	have := c.off + int64(len(c.buf))
	if off > have || off+int64(len(p)) <= have {
		return false
	}
	p = p[have-off:]
	if c.want > 0 {
		if n := c.want - int64(len(c.buf)); int64(len(p)) > n {
			p = p[:n]
		}
	}
	c.buf = append(c.buf, p...)
	if c.want == 0 {
		total, ok := c.size(c.buf)
		if !ok {
			return false
		}
		if total <= 0 || (c.max > 0 && total > c.max) {
			c.done, c.buf = true, nil
			return false
		}
		c.want = total
		if int64(len(c.buf)) > c.want {
			c.buf = c.buf[:c.want]
		}
		// One allocation instead of doubling up to the size (a moov can be tens of MB),
		// but not more up front than captureReserve: the header may promise bytes that
		// never come.
		c.buf = slices.Grow(c.buf, int(min(c.want, captureReserve)-int64(len(c.buf))))
	}
	return int64(len(c.buf)) >= c.want
}

// captureReserve is the most a capture allocates before the bytes arrive.
const captureReserve = 16 << 20

// capturer is anything built on a capture (MP4 boxes, AVI index regions).
type capturer interface{ captured() *capture }

func (c *capture) captured() *capture { return c }

// feedCaptures offers the bytes at off to each of cs; complete gets each one that the
// bytes complete, and may mark it done or retarget it (then it is fed the same bytes
// again). It returns the captures that are not done, in cs's storage.
func feedCaptures[C capturer](cs []C, off int64, p []byte, complete func(C)) []C {
	live := cs[:0]
	for _, c := range cs {
		k := c.captured()
		for !k.done && k.feed(off, p) {
			complete(c)
		}
		if !k.done {
			live = append(live, c)
		}
	}
	clear(cs[len(live):])
	return live
}

// retarget moves the capture to a new offset (e.g. past padding before the wanted object).
func (c *capture) retarget(off int64) {
	c.off, c.want, c.buf = off, 0, nil
}

// riffSize is the size function for RIFF chunks: 8-byte header, body, pad to even.
func riffSize(hdr []byte) (int64, bool) {
	if len(hdr) < 8 {
		return 0, false
	}
	size := int64(binary.LittleEndian.Uint32(hdr[4:]))
	return 8 + size + size&1, true
}

// boxSize is the size function for ISO BMFF (MP4) boxes, including 64-bit sizes.
// A box that runs "to the end of the file" (size 0) is rejected: it cannot be collected.
func boxSize(hdr []byte) (int64, bool) {
	if len(hdr) < 8 {
		return 0, false
	}
	switch size := binary.BigEndian.Uint32(hdr); size {
	case 0:
		return -1, true
	case 1:
		if len(hdr) < 16 {
			return 0, false
		}
		n := binary.BigEndian.Uint64(hdr[8:])
		if n < 16 || n > 1<<62 {
			return -1, true
		}
		return int64(n), true
	default:
		if size < 8 {
			return -1, true
		}
		return int64(size), true
	}
}
