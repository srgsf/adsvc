package avi

import (
	"time"

	"github.com/srgsf/adsvc/internal/mediatime"
)

// Frame is one audio frame (or, for PCM, a piece of a chunk) with its presentation time.
// Data is only valid during the OnFrame callback.
type Frame struct {
	Time    time.Duration // on the player's timeline
	Samples int           // samples in this frame (0 for PCM pieces)
	Data    []byte
}

// Extractor turns contiguous byte ranges of an AVI file into timed audio frames.
//
//	x := NewExtractor(hdr, audio, index)
//	x.Reset(rangeStart)          // on every new (seeked) byte range
//	x.Write(bytes)               // as they flow through the proxy
//
// OnFrame receives frames. OnDiscontinuity, if set, is told when the timeline jumps (seek,
// gap, resync after corruption): diagnostics for tests; the decoder downstream restarts on
// its own when frame times jump (decode.Sink).
type Extractor struct {
	Audio           *Audio
	Index           *Index
	OnFrame         func(Frame)
	OnDiscontinuity func(t time.Duration)

	hdr *Header
	sc  scanner
	pcm bool

	pos     int64 // absolute offset of the next byte passed to Write
	k       int   // current chunk (index into Index)
	inChunk bool
	remain  int // bytes left in the current chunk

	buf      []byte // payload bytes not yet emitted as frames
	marks    []mark // chunk starts inside buf
	sync     bool   // frame-synced
	anchored bool   // timeline anchored for the current range

	// MaxDrift is the largest |frame time - index time| seen at chunk boundaries
	// (diagnostics only; like the Java extractor the timeline is not re-anchored).
	MaxDrift time.Duration
	t0       time.Duration // anchor time
	n        int64         // samples since anchor
	pcmT     time.Duration // PCM: time of the anchor
	pcmN     int64         // PCM: bytes since the anchor
	seq      seqParser     // no-index fallback
}

type mark struct {
	at int // position in buf
	k  int // chunk
}

// NewExtractor creates an extractor for stream a of the file described by h, timed by ix.
func NewExtractor(h *Header, a *Audio, ix *Index) *Extractor {
	x := &Extractor{hdr: h, Audio: a, Index: ix, sc: newScanner(a)}
	x.pcm = len(a.Codec) > 4 && a.Codec[:4] == "pcm_"
	return x
}

// Supported reports whether frames can be produced for this stream.
func (x *Extractor) Supported() bool { return x.sc != nil || x.pcm }

// Reset starts a new contiguous range at absolute offset off.
func (x *Extractor) Reset(off int64) {
	x.pos, x.inChunk, x.remain = off, false, 0
	x.buf, x.marks, x.sync, x.anchored = x.buf[:0], x.marks[:0], false, false
	x.k = x.Index.ChunkAtOrAfter(off)
	x.seq = seqParser{}
	if x.Index.Len() == 0 && x.hdr != nil && off <= x.hdr.MoviStart+12 {
		// no index: only playback from the start can be timed; build the index on the fly
		x.seq = seqParser{active: true, next: x.hdr.MoviStart + 12}
	}
}

// Write feeds bytes located at the current position.
func (x *Extractor) Write(p []byte) {
	for len(p) > 0 {
		if x.seq.active && !x.inChunk && !x.seq.pending {
			n := x.seq.feed(x, p)
			p = p[n:]
			x.pos += int64(n)
			continue
		}
		if !x.inChunk {
			if x.k >= x.Index.Len() {
				x.pos += int64(len(p))
				return
			}
			dataStart := x.Index.Chunks[x.k].Off + 8
			if x.pos < dataStart {
				skip := dataStart - x.pos
				if skip > int64(len(p)) {
					x.pos += int64(len(p))
					return
				}
				p, x.pos = p[skip:], dataStart
			} else if x.pos > dataStart { // range began inside this chunk: skip to the next
				x.k++
				continue
			}
			x.inChunk, x.remain, x.seq.pending = true, int(x.Index.Chunks[x.k].Size), false
			x.chunkStart()
		}
		n := min(x.remain, len(p))
		x.payload(p[:n])
		p, x.pos, x.remain = p[n:], x.pos+int64(n), x.remain-n
		if x.remain == 0 {
			x.inChunk = false
			x.k++
		}
	}
}

func (x *Extractor) chunkTime(k int) time.Duration { return x.Audio.Time(x.Index.Chunks[k].Time) }

func (x *Extractor) chunkStart() {
	if x.pcm {
		if !x.anchored {
			x.pcmT, x.pcmN = x.chunkTime(x.k), 0
			x.anchored = true
			if x.OnDiscontinuity != nil {
				x.OnDiscontinuity(x.pcmT)
			}
		}
		return
	}
	if x.sync && len(x.buf) == 0 {
		if d := (x.chunkTime(x.k) - x.now()).Abs(); d > x.MaxDrift {
			x.MaxDrift = d
		}
	}
	x.marks = append(x.marks, mark{at: len(x.buf), k: x.k})
}

func (x *Extractor) now() time.Duration { return x.t0 + mediatime.Samples(x.n, x.Audio.SampleRate) }

// anchor fixes the timeline once per range (after Reset), like needAdjustTimestamp in the
// Java extractor. Later resyncs (junk bytes, corrupt data) continue the running timeline.
func (x *Extractor) anchor(t time.Duration) {
	x.t0, x.n, x.sync, x.anchored = t, 0, true, true
	if x.OnDiscontinuity != nil {
		x.OnDiscontinuity(t)
	}
}

func (x *Extractor) payload(p []byte) {
	if x.pcm {
		x.OnFrame(Frame{Time: x.pcmT + x.pcmTime(x.pcmN), Data: p})
		x.pcmN += int64(len(p))
		return
	}
	if x.sc == nil {
		return
	}
	x.buf = append(x.buf, p...)
	x.frames()
}

// pcmTime is how long n bytes of PCM play.
func (x *Extractor) pcmTime(n int64) time.Duration {
	switch {
	case x.Audio.BlockAlign > 0 && x.Audio.SampleRate > 0:
		return mediatime.Ticks(n, 1, int64(x.Audio.BlockAlign*x.Audio.SampleRate))
	case x.Audio.SampleSize > 0:
		return x.Audio.Time(n)
	}
	return 0
}

// timeAt maps a position in buf to index time (chunk start + proportional offset).
func (x *Extractor) timeAt(pos int) time.Duration {
	m := mark{at: 0, k: x.k}
	for _, mk := range x.marks {
		if mk.at <= pos {
			m = mk
		}
	}
	size := int64(x.Index.Chunks[m.k].Size)
	t := x.Index.Chunks[m.k].Time
	if size > 0 {
		t += int64(pos-m.at) * x.Audio.ChunkDuration(size) / size
	}
	return x.Audio.Time(t)
}

func (x *Extractor) frames() {
	p := 0
	for p < len(x.buf) {
		if !x.sync {
			i := x.sc.syncCandidate(x.buf[p:])
			if i < 0 {
				p = max(
					// keep a tail that may hold a split sync word
					len(x.buf)-5, 0)
				break
			}
			p += i
			size, _, st := x.sc.parse(x.buf[p:])
			if st == frameNeedMore {
				break
			}
			if st == frameInvalid {
				p++
				continue
			}
			// strong sync: the next header must be valid too (when we already have it)
			if p+size+10 <= len(x.buf) {
				if _, _, st2 := x.sc.parse(x.buf[p+size:]); st2 == frameInvalid {
					p++
					continue
				}
			} else if p+size > len(x.buf) {
				break
			}
			if x.anchored {
				x.sync = true
			} else {
				x.anchor(x.timeAt(p))
			}
		}
		size, samples, st := x.sc.parse(x.buf[p:])
		if st == frameNeedMore {
			break
		}
		if st == frameInvalid {
			x.sync = false
			p++
			continue
		}
		if p+size > len(x.buf) {
			break
		}
		x.OnFrame(Frame{Time: x.now(), Samples: samples, Data: x.buf[p : p+size]})
		x.n += int64(samples)
		p += size
	}
	// drop consumed bytes; keep the last chunk mark at/before the new start and later ones
	x.buf = append(x.buf[:0], x.buf[p:]...)
	first := 0
	for i := range x.marks {
		x.marks[i].at -= p
		if x.marks[i].at <= 0 {
			first = i
		}
	}
	x.marks = append(x.marks[:0], x.marks[first:]...)
}

// seqParser walks movi chunk headers when there is no index (playback from the start).
type seqParser struct {
	active  bool
	pending bool  // an audio chunk was found; Write must read its payload next
	next    int64 // absolute offset of the next chunk header
	hdr     [8]byte
	have    int
}

// feed consumes bytes up to the payload of the next audio chunk and returns how many bytes
// it used. When it meets an audio chunk it appends it to the index, so Write continues with
// the payload exactly at the current position.
func (s *seqParser) feed(x *Extractor, p []byte) int {
	used := 0
	for used < len(p) {
		pos := x.pos + int64(used)
		if s.have == 0 && pos < s.next {
			skip := int(s.next - pos)
			if skip > len(p)-used {
				return len(p)
			}
			used += skip
			continue
		}
		n := copy(s.hdr[s.have:], p[used:])
		s.have += n
		used += n
		if s.have < 8 {
			return used
		}
		hdrStart := s.next
		id, size := le.Uint32(s.hdr[:]), int64(le.Uint32(s.hdr[4:]))
		s.have = 0
		switch id {
		case ccLIST, ccRIFF:
			s.next = hdrStart + 12 // descend into movi / rec / AVIX lists
		case x.Audio.ChunkID:
			s.next = hdrStart + 8 + size + size&1
			if !x.Index.append(hdrStart, size) {
				continue // empty or corrupt chunk: step over it like any other
			}
			x.k = x.Index.Len() - 1
			s.pending = true
			return used // == payload start
		case ccIdx1:
			s.active = false
			return len(p)
		default:
			s.next = hdrStart + 8 + size + size&1
		}
	}
	return used
}
