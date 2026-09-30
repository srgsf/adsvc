package demux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/srgsf/adsvc/internal/demux/avi"
	"github.com/srgsf/adsvc/internal/mediatime"
)

const (
	aviMaxHead     = 1 << 20  // give up looking for the movi list after this many bytes
	aviMaxIndex    = 32 << 20 // sanity cap for one idx1 / ix## chunk
	aviMaxRetarget = 8        // JUNK chunks tolerated before idx1
)

var le32 = binary.LittleEndian.Uint32

var ccIdx1 = le32([]byte("idx1"))

// aviContainer drives the push-based AVI demuxer from the byte ranges a player downloads.
//
// A player opens an AVI by reading the file header, then (to be able to seek) the idx1
// chunk after movi or the OpenDML ix## chunks, then the media data from wherever playback
// starts. All three arrive here as plain byte ranges, in whatever order the player asks
// for them, so the container keeps the file header, collects index chunks whenever a range
// happens to cover one, and feeds everything else to the extractor.
type aviContainer struct {
	sink Sink

	head []byte // contiguous bytes from offset 0, kept until a real index is known
	hdr  *avi.Header
	aud  *avi.Audio
	ix   *avi.Index
	x    *avi.Extractor

	regs    []*aviRegion // index chunks we are waiting for (done ones are dropped)
	pending []pendingIx  // parsed index chunks, applied while the extractor is idle

	pos       int64 // absolute offset of the next byte
	active    bool  // the extractor is following a contiguous run
	haveIndex bool  // a real (file) index has been parsed
	seqBuilt  bool  // the current index was guessed by the sequential parser
	err       error
	warned    bool
}

type pendingIx struct {
	body []byte
	idx1 bool
	sub  int // position in Audio.ODMLIndex
}

func newAVIContainer(sink Sink) *aviContainer {
	return &aviContainer{sink: sink}
}

func (c *aviContainer) Range(off int64) {
	c.pos, c.active = off, false
}

func (c *aviContainer) Close() error {
	c.sink.Close()
	c.head, c.regs, c.pending = nil, nil, nil
	return c.err
}

// Duration is the end of the last chunk of the file's own index, once one has been
// captured. (With a partial OpenDML index it covers only the segments seen so far.)
func (c *aviContainer) Duration() time.Duration {
	if !c.haveIndex || c.aud == nil || c.ix.Len() == 0 {
		return 0
	}
	n := c.ix.Len() - 1
	return c.aud.Time(c.ix.Chunks[n].Time + c.aud.ChunkDuration(int64(c.ix.Chunks[n].Size)))
}

func (c *aviContainer) Write(p []byte) error {
	if c.err != nil {
		return c.err
	}
	off := c.pos
	c.pos += int64(len(p))

	c.growHead(off, p)
	if c.hdr == nil {
		if c.parseHead(); c.hdr == nil {
			return c.err
		}
	}
	c.feedRegions(off, p)
	if c.aud == nil {
		return nil // nothing here we can demux
	}
	if !c.active {
		c.applyIndex()
		if !c.start(off) {
			return nil
		}
	}
	c.x.Write(p)
	return c.err
}

func (c *aviContainer) fail(err error) {
	if c.err == nil {
		c.err = fmt.Errorf("avi: %w", err)
	}
}

// growHead keeps the bytes from offset 0 while they arrive contiguously.
func (c *aviContainer) growHead(off int64, p []byte) {
	have := int64(len(c.head))
	if c.haveIndex || off > have || off+int64(len(p)) <= have || have >= aviMaxHead {
		return
	}
	p = p[have-off:]
	if n := aviMaxHead - len(c.head); len(p) > n {
		p = p[:n]
	}
	c.head = append(c.head, p...)
}

func (c *aviContainer) parseHead() {
	h, err := avi.ParseHeader(c.head)
	if err != nil {
		if _, ok := errors.AsType[avi.ErrNeedMore](err); ok {
			if len(c.head) >= aviMaxHead {
				c.fail(fmt.Errorf("no movi list in the first %d bytes", aviMaxHead))
			}
			return
		}
		c.fail(err)
		return
	}
	c.hdr = h
	for _, a := range h.Audio {
		format, err := aviFormat(a)
		if err != nil {
			slog.Warn("avi: audio stream not supported", "stream", a.StreamID, "err", err)
			continue
		}
		ix := avi.NewIndex(a)
		x := avi.NewExtractor(h, a, ix)
		if !x.Supported() {
			slog.Warn("avi: no frame scanner for audio stream", "stream", a.StreamID, "codec", a.Codec)
			continue
		}
		if err := c.sink.Configure(format); err != nil {
			c.fail(err)
			return
		}
		c.aud, c.ix, c.x = a, ix, x
		x.OnFrame = c.onFrame
		slog.Info("avi: audio track", "codec", a.Codec, "rate", a.SampleRate, "channels", a.Channels,
			"stream", a.StreamID, "movi_start", h.MoviStart, "movi_end", h.MoviEnd)
		break
	}
	if c.aud == nil {
		slog.Warn("avi: no usable audio stream")
		return
	}
	// Prefer the OpenDML indexes when the file has them: unlike idx1 (a single chunk after
	// movi) they sit inside movi, so a player that never seeks to the end still passes over
	// them, and each one carries its own start time in the super index.
	if len(c.aud.ODMLIndex) > 0 {
		for i, off := range c.aud.ODMLIndex {
			r := newAVIRegion(off)
			r.sub = i
			c.regs = append(c.regs, r)
		}
	} else {
		r := newAVIRegion(h.MoviEnd)
		r.idx1 = true
		c.regs = append(c.regs, r)
	}
}

func (c *aviContainer) onFrame(f avi.Frame) {
	if c.err != nil {
		return
	}
	var dur time.Duration
	switch {
	case f.Samples > 0 && c.aud.SampleRate > 0:
		dur = mediatime.Samples(int64(f.Samples), c.aud.SampleRate)
	case c.aud.BlockAlign > 0 && c.aud.SampleRate > 0:
		// PCM: the extractor hands out pieces of a chunk, timed by their byte count
		dur = mediatime.Ticks(int64(len(f.Data)), 1, int64(c.aud.BlockAlign*c.aud.SampleRate))
	}
	if err := c.sink.Frame(Frame{Time: f.Time, Dur: dur, Data: f.Data}); err != nil {
		c.fail(err)
	}
}

// start points the extractor at off. Without a real index only a range that can be followed
// from the start of movi can be timed, so the bytes kept in head are replayed first.
func (c *aviContainer) start(off int64) bool {
	switch {
	case c.haveIndex:
		c.x.Reset(off)
		c.head = nil
	case int64(len(c.head)) >= off:
		c.x.Reset(0)
		c.x.Write(c.head[:off])
		c.seqBuilt = true
	default:
		if !c.warned {
			c.warned = true
			slog.Warn("avi: no index yet, skipping bytes (seek before the index was read)", "offset", off)
		}
		return false
	}
	c.active = true
	return true
}

// applyIndex merges captured index chunks. It runs only while the extractor is idle,
// because the extractor holds a position inside the index array.
func (c *aviContainer) applyIndex() {
	if len(c.pending) == 0 {
		return
	}
	if c.seqBuilt {
		// drop what the sequential parser appended: the file's own index supersedes it
		c.ix = avi.NewIndex(c.aud)
		c.x = avi.NewExtractor(c.hdr, c.aud, c.ix)
		c.x.OnFrame = c.onFrame
		c.seqBuilt = false
	}
	for _, p := range c.pending {
		if p.idx1 {
			c.ix.AddIdx1(p.body, c.hdr.MoviStart)
		} else {
			c.ix.AddIxAt(p.body, c.aud.ODMLStart(p.sub))
		}
	}
	c.pending = nil
	if c.ix.Len() > 0 && !c.haveIndex {
		c.haveIndex = true
		slog.Info("avi: index ready", "audio_chunks", c.ix.Len())
	}
}

// aviRegion is an index chunk (idx1 or one OpenDML ix##) we are waiting for.
type aviRegion struct {
	capture
	idx1  bool
	sub   int // ix##: position in Audio.ODMLIndex
	tries int
}

func newAVIRegion(off int64) *aviRegion {
	return &aviRegion{off: off, size: riffSize, max: aviMaxIndex}
}

func (c *aviContainer) feedRegions(off int64, p []byte) {
	c.regs = feedCaptures(c.regs, off, p, c.parseRegion)
}

func (c *aviContainer) parseRegion(r *aviRegion) {
	id, size := le32(r.buf), int64(le32(r.buf[4:]))
	body := r.buf[8:]
	if 8+size <= int64(len(r.buf)) {
		body = r.buf[8 : 8+size]
	}
	if r.idx1 && id != ccIdx1 {
		// JUNK (or padding) between movi and idx1: step over it and try the next chunk
		r.tries++
		if r.tries > aviMaxRetarget {
			r.done, r.buf = true, nil
			return
		}
		r.retarget(r.off + r.want)
		return
	}
	c.pending = append(c.pending, pendingIx{body: body, idx1: r.idx1, sub: r.sub})
	r.buf, r.done = nil, true
}

// aviFormat is how ffmpeg gets the frames of a demuxed AVI audio stream.
func aviFormat(a *avi.Audio) (AudioFormat, error) {
	switch a.Codec {
	case "mp1", "mp2", "mp3":
		return rawFormat(codecMP3), nil
	case "ac3", "eac3", "dts":
		return rawFormat(a.Codec), nil
	case "pcm_s16le", "pcm_u8", "pcm_s24le", "pcm_s32le", "pcm_f32le":
		return pcmFormat(a.Codec[4:], a.SampleRate, a.Channels), nil
	}
	return AudioFormat{}, fmt.Errorf("unsupported AVI audio codec %q (tag 0x%04x)", a.Codec, a.FormatTag)
}
