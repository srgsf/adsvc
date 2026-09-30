// Package demux turns the byte ranges a player downloads, in whatever order it asks for
// them, into timed audio frames. It supports AVI, Matroska/WebM, MP4/MOV (progressive and
// fragmented) and MPEG-TS/M2TS, parses only what passes by, and never reads the source.
package demux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Frame is one encoded audio frame with its time on the player's timeline.
// Data is only valid for the duration of the Sink.Frame call.
type Frame struct {
	Time time.Duration
	Dur  time.Duration // 0 when unknown (e.g. pieces of a PCM chunk)
	Data []byte
}

// Sink consumes the frames a Container demuxes. Frames of one continuous run arrive
// in order; a jump in Frame.Time means the player seeked and the decoder must restart.
type Sink interface {
	// Configure is called once, as soon as the audio format is known.
	Configure(f AudioFormat) error
	Frame(f Frame) error
	Close()
}

// Container turns the byte ranges a player downloads - in whatever order it asks for them -
// into timed audio frames. Implementations never read from the source themselves: the proxy
// only ever hands them bytes that are already on their way to the player.
type Container interface {
	// Range announces that the following Write calls carry the bytes at absolute offset off;
	// it must be called before the first Write. Frames that the discontinuity completes
	// (those a container holds back to learn their duration) are emitted to the sink here.
	Range(off int64)
	// Write feeds bytes located at the current position. It must not keep p after it
	// returns (the proxy reuses the buffer).
	Write(p []byte) error
	// Duration is the media duration as far as the container knows it (0 = unknown).
	Duration() time.Duration
	// Close emits what is still held back, closes the sink and returns the container's
	// error, if any (also the sink's, from the last frames).
	Close() error
}

// ErrUnsupported means we recognise the file but have no demuxer for it yet;
// the proxy keeps forwarding bytes and simply does not analyse them.
var ErrUnsupported = errors.New("unsupported container")

// Sniff names the container in head (the first bytes of the file), or "".
func Sniff(head []byte) string {
	if len(head) < 16 {
		return ""
	}
	be := binary.BigEndian
	switch {
	case string(head[0:4]) == "RIFF" && string(head[8:12]) == "AVI ":
		return "avi"
	case be.Uint32(head) == 0x1A45DFA3:
		return "matroska"
	case isMP4Box(string(head[4:8])):
		return "mp4"
	case head[0] == 0x47 && len(head) > 188 && head[188] == 0x47:
		return "mpegts"
	case head[4] == 0x47 && len(head) > 196 && head[196] == 0x47: // M2TS (Blu-ray, AVCHD)
		return "mpegts"
	case be.Uint32(head)&0xFFFFFFE0 == 0xFFFA0000 || string(head[0:3]) == "ID3":
		return "mp3"
	}
	return ""
}

// New creates the demuxer for a container type reported by Sniff.
func New(typ string, sink Sink) (Container, error) {
	switch typ {
	case "avi":
		return newAVIContainer(sink), nil
	case "matroska":
		return newMKVContainer(sink), nil
	case "mp4":
		return newMP4Container(sink), nil
	case "mpegts":
		return newTSContainer(sink), nil
	case "":
		return nil, fmt.Errorf("%w: unknown format", ErrUnsupported)
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, typ)
}

// maxGroupSpan bounds the gap between two groups that is still taken as their duration;
// anything longer is a seek or a hole, not a frame.
const maxGroupSpan = 2 * time.Second

// frameDelay emits frames one group late so that each can carry a duration: containers
// that store only start times (Matroska blocks, MPEG-TS PES packets) take it from the start
// of the next group. A group is several frames that share one timestamp (a laced block);
// its frames are spread evenly over the group's span.
type frameDelay struct {
	emit    func(Frame) error
	t       time.Duration
	per     time.Duration // known duration per frame for the pending group, 0 = unknown
	data    []byte        // the pending group's frames back to back (reused across groups)
	sizes   []int         // their sizes
	lastPer time.Duration // last duration per frame that was known or derived
}

// push queues a group starting at t. per is the known duration of each frame (0 = derive it
// from the next group). Each frame is copied, after prefix (Matroska header stripping).
func (d *frameDelay) push(t, per time.Duration, prefix []byte, frames ...[]byte) error {
	err := d.release(t - d.t)
	d.t, d.per = t, per
	for _, f := range frames {
		d.data = append(append(d.data, prefix...), f...)
		d.sizes = append(d.sizes, len(prefix)+len(f))
	}
	return err
}

// flush emits the pending group; call it when the byte stream is discontinued.
func (d *frameDelay) flush() error { return d.release(-1) }

func (d *frameDelay) release(span time.Duration) error {
	n := len(d.sizes)
	if n == 0 {
		return nil
	}
	per := d.per
	if per <= 0 && span > 0 && span <= maxGroupSpan {
		per = span / time.Duration(n)
	}
	if per <= 0 {
		per = d.lastPer
	}
	d.lastPer = per
	var err error
	off := 0
	for i, size := range d.sizes {
		f := d.data[off : off+size : off+size]
		off += size
		if e := d.emit(Frame{Time: d.t + time.Duration(i)*per, Dur: per, Data: f}); e != nil && err == nil {
			err = e
		}
	}
	d.data, d.sizes = d.data[:0], d.sizes[:0]
	return err
}

// isMP4Box reports box types that can start an MP4/QuickTime file.
func isMP4Box(typ string) bool {
	switch typ {
	case "ftyp", "styp", "moov", "mdat", "wide", "free", "skip":
		return true
	}
	return false
}

// resync finds the next marker (a Matroska Cluster, an MP4 moof) in bytes that arrive in
// writes of any size, after a seek or corrupt data. Each write is scanned in place; only
// the last size-1 bytes are carried to the next, where a marker may begin that check needs
// more of.
type resync struct {
	pattern []byte
	lead    int // bytes of the marker before pattern
	size    int // at most this many bytes, from the marker's start, are passed to check
	// check tells whether b (from a candidate marker's start) is a marker, or that it
	// cannot tell before more bytes arrive (more only when len(b) < size).
	check func(b []byte) (ok, more bool)
	carry []byte
}

// reset forgets the carried bytes (a new range starts).
func (r *resync) reset() { r.carry = r.carry[:0] }

// find looks for the first marker in the carried bytes and p. When it finds one, head is
// the carried bytes from the marker on (nil when it starts in p; valid until the next
// find) and rest is p from where the marker, or head, left off.
func (r *resync) find(p []byte) (head, rest []byte, ok bool) {
	n := len(r.carry)
	w := append(r.carry, p[:min(len(p), r.size)]...)
	whole := len(w) == n+len(p) // w holds all of p
	for s := r.next(w, 0); s >= 0 && s < n; s = r.next(w, s+1) {
		switch found, more := r.check(w[s:]); {
		case found:
			r.carry = w[:0]
			return w[s:n], p, true
		case more && whole:
			r.carry = w[:copy(w, w[s:])]
			return nil, nil, false
		}
	}
	for s := r.next(p, 0); s >= 0; s = r.next(p, s+1) {
		found, more := r.check(p[s:])
		if found {
			r.carry = w[:0]
			return nil, p[s:], true
		}
		if more {
			r.carry = append(w[:0], p[s:]...)
			return nil, nil, false
		}
	}
	if whole {
		r.carry = w[:copy(w, w[max(0, len(w)-(r.size-1)):])]
	} else {
		r.carry = append(w[:0], p[len(p)-(r.size-1):]...)
	}
	return nil, nil, false
}

// next is the start of the first candidate marker in b at or after from, or -1.
func (r *resync) next(b []byte, from int) int {
	if from+r.lead > len(b) {
		return -1
	}
	i := bytes.Index(b[from+r.lead:], r.pattern)
	if i < 0 {
		return -1
	}
	return from + i
}
