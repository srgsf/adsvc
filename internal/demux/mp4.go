package demux

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/srgsf/adsvc/internal/mediatime"
)

const (
	mp4MaxMoov = 64 << 20 // a 3-hour 4K file's is about 15 MB
	mp4MaxMoof = 16 << 20

	// tfhd and trun flags (ISO/IEC 14496-12 8.8.7, 8.8.8)
	tfhdBaseOffset  = 0x1
	tfhdDescription = 0x2
	tfhdDuration    = 0x8
	tfhdSize        = 0x10
	tfhdBaseIsMoof  = 0x20000
	trunDataOffset  = 0x1
	trunFirstFlags  = 0x4
	trunDuration    = 0x100
	trunSize        = 0x200
	trunFlags       = 0x400
	trunCTO         = 0x800
	mp4MaxSamples   = 4 << 20  // audio samples in one sample table (~26 h of AAC)
	mp4MaxFragQueue = 1 << 18  // fragment samples kept around before pruning
	mp4MaxSample    = 16 << 20 // one audio sample
)

var be = binary.BigEndian

// mp4Sample is one audio sample (frame) located by absolute file offset.
type mp4Sample struct {
	off  int64
	size uint32
	dur  uint32
	t    int64 // decode time, media timescale units
}

// mp4Track is the audio track chosen from moov.
type mp4Track struct {
	id        uint32
	timescale uint32
	format    AudioFormat
	codec     string
	shift     int64         // edit list: media time shown at the start of the track
	delay     time.Duration // edit list: empty edit before the track starts
	trexDur   uint32        // fragment defaults (mvex/trex)
	trexSize  uint32
}

// mp4Container is a push-based MP4/MOV demuxer for the proxy.
//
// An offset-driven walker follows the top-level boxes as the player's reads cover their
// headers and captures moov and moof boxes wherever they are: at the start of the file, at
// the end (the mdat header says where), or between fragments. Progressive files get their
// whole sample table from moov, so any byte range can be timed, like AVI with an index;
// fragmented files get samples from each moof, and after a seek the demuxer resyncs on the
// next moof, like Matroska clusters.
type mp4Container struct {
	sink Sink

	pos      int64
	walkNext int64 // offset of the next top-level box header, -1 = not following the boxes
	walkHdr  []byte
	resync   bool // fragmented: look for the next moof (rs)
	rs       resync
	caps     []*capture

	moov       bool
	fragmented bool
	track      *mp4Track
	duration   time.Duration
	samples    []mp4Sample // sorted by offset
	k          int         // next sample to read, -1 = look it up at the next write
	inSample   bool
	cur        []byte
	fragNext   int64 // decode time after the last fragment parsed, -1 = unknown
	err        error
}

func newMP4Container(sink Sink) *mp4Container {
	c := &mp4Container{sink: sink, fragNext: -1, k: -1}
	c.rs = resync{pattern: []byte("moof"), lead: 4, size: 16, check: validMoof}
	return c
}

func (c *mp4Container) Range(off int64) {
	c.pos, c.k, c.inSample = off, -1, false
	if c.fragmented && off != c.walkNext {
		c.resync = true
		c.rs.reset()
	}
}

func (c *mp4Container) Close() error {
	c.sink.Close()
	c.caps, c.samples = nil, nil
	return c.err
}

func (c *mp4Container) Duration() time.Duration { return c.duration }

func (c *mp4Container) fail(err error) {
	if c.err == nil {
		c.err = fmt.Errorf("mp4: %w", err)
	}
}

func (c *mp4Container) Write(p []byte) error {
	if c.err != nil {
		return c.err
	}
	off := c.pos
	c.pos += int64(len(p))
	if c.resync {
		c.findMoof(off, p)
	}
	c.walk(off, p)
	c.feedCaptures(off, p)
	if c.track != nil && c.err == nil {
		c.readSamples(off, p)
	}
	return c.err
}

// walk follows top-level box headers that fall inside p.
func (c *mp4Container) walk(off int64, p []byte) {
	end := off + int64(len(p))
	for c.walkNext >= 0 {
		have := c.walkNext + int64(len(c.walkHdr))
		if have < off || have >= end {
			return
		}
		take := min(int64(16-len(c.walkHdr)), end-have)
		c.walkHdr = append(c.walkHdr, p[have-off:have-off+take]...)
		size, ok := boxSize(c.walkHdr)
		if !ok {
			continue // need the rest of a 64-bit size
		}
		at, typ := c.walkNext, string(c.walkHdr[4:8])
		c.walkHdr = c.walkHdr[:0]
		if size < 0 { // runs to the end of the file (a final mdat)
			c.walkNext = -1
			return
		}
		switch typ {
		case "moov":
			if !c.moov {
				c.addCapture(at, mp4MaxMoov)
			}
		case "moof":
			// captures complete in file order, so this one is parsed after the moov
			// before it, even when both arrive in the same write
			c.addCapture(at, mp4MaxMoof)
		}
		c.walkNext = at + size
	}
}

func (c *mp4Container) addCapture(at, max int64) {
	for _, k := range c.caps {
		if k.off == at && !k.done {
			return
		}
	}
	c.caps = append(c.caps, &capture{off: at, size: boxSize, max: max})
}

func (c *mp4Container) feedCaptures(off int64, p []byte) {
	c.caps = feedCaptures(c.caps, off, p, func(k *capture) {
		k.done = true
		c.parseTopBox(k.off, k.buf)
		k.buf = nil
	})
}

// findMoof resyncs a fragmented file after a seek: the walk continues at the next "moof"
// whose first child is an mfhd box. When it began in bytes carried from the previous
// write, those are walked and captured first.
func (c *mp4Container) findMoof(off int64, p []byte) {
	head, rest, ok := c.rs.find(p)
	if !ok {
		return
	}
	at := off + int64(len(p)-len(rest)) - int64(len(head))
	c.walkNext, c.walkHdr, c.resync = at, c.walkHdr[:0], false
	if len(head) > 0 {
		c.walk(at, head)
		c.feedCaptures(at, head)
	}
}

// validMoof checks a moof candidate (a resync.check): a plausible size, and mfhd first.
func validMoof(b []byte) (ok, more bool) {
	if len(b) < 16 {
		return false, true
	}
	size := be.Uint32(b)
	return size >= 24 && size <= mp4MaxMoof && string(b[12:16]) == "mfhd", false
}

func bySampleOffset(a, b mp4Sample) int { return cmp.Compare(a.off, b.off) }

func (c *mp4Container) parseTopBox(at int64, b []byte) {
	h := boxHeaderLen(b)
	if h == 0 {
		return
	}
	switch string(b[4:8]) {
	case "moov":
		if err := c.parseMoov(b[h:]); err != nil {
			c.fail(err)
		}
	case "moof":
		c.parseMoof(at, b[h:])
	}
}

func boxHeaderLen(b []byte) int {
	if len(b) >= 8 && be.Uint32(b) == 1 {
		if len(b) < 16 {
			return 0
		}
		return 16
	}
	if len(b) < 8 {
		return 0
	}
	return 8
}

// mp4Boxes calls fn for each child box in b. It stops at the first malformed box.
func mp4Boxes(b []byte, fn func(typ string, body []byte)) {
	for len(b) >= 8 {
		size := uint64(be.Uint32(b))
		h := uint64(8)
		switch size {
		case 0:
			size = uint64(len(b))
		case 1:
			if len(b) < 16 {
				return
			}
			size, h = be.Uint64(b[8:]), 16
		}
		if size < h || size > uint64(len(b)) {
			return
		}
		fn(string(b[4:8]), b[h:size])
		b = b[size:]
	}
}

func (c *mp4Container) parseMoov(b []byte) error {
	if c.moov {
		return nil
	}
	c.moov = true
	var movieScale uint32
	var tracks [][]byte
	mp4Boxes(b, func(typ string, body []byte) {
		switch typ {
		case "mvhd":
			scale, dur := mvhd(body)
			movieScale = scale
			if scale > 0 && dur > 0 {
				c.duration = mediatime.Ticks(int64(dur), 1, int64(scale))
			}
		case "trak":
			tracks = append(tracks, body)
		case "mvex":
			c.fragmented = true
		}
	})
	for _, tb := range tracks {
		t, samples, err := parseTrak(tb, movieScale)
		if err != nil {
			slog.Warn("mp4: skipping track", "err", err)
			continue
		}
		if t == nil {
			continue
		}
		mp4Boxes(b, func(typ string, body []byte) {
			if typ != "mvex" {
				return
			}
			mp4Boxes(body, func(typ string, body []byte) {
				if typ == "trex" && len(body) >= 24 && be.Uint32(body[4:]) == t.id {
					t.trexDur, t.trexSize = be.Uint32(body[12:]), be.Uint32(body[16:])
				}
			})
		})
		if err := c.sink.Configure(t.format); err != nil {
			return err
		}
		c.track, c.samples, c.k = t, samples, -1
		slog.Info("mp4: audio track", "track", t.id, "codec", t.codec, "moov_samples", len(samples),
			"fragmented", c.fragmented)
		return nil
	}
	slog.Warn("mp4: no usable audio track")
	return nil
}

// mvhd returns the movie timescale and duration.
func mvhd(b []byte) (uint32, uint64) {
	if len(b) >= 32 && b[0] == 1 {
		return be.Uint32(b[20:]), be.Uint64(b[24:])
	}
	if len(b) >= 20 {
		return be.Uint32(b[12:]), uint64(be.Uint32(b[16:]))
	}
	return 0, 0
}

// parseTrak returns the track (nil if it is not a usable audio track) and its samples.
func parseTrak(b []byte, movieScale uint32) (*mp4Track, []mp4Sample, error) {
	t := &mp4Track{}
	var mdia, edts []byte
	mp4Boxes(b, func(typ string, body []byte) {
		switch typ {
		case "tkhd":
			if len(body) >= 24 && body[0] == 1 {
				t.id = be.Uint32(body[20:])
			} else if len(body) >= 16 {
				t.id = be.Uint32(body[12:])
			}
		case "mdia":
			mdia = body
		case "edts":
			edts = body
		}
	})
	var handler string
	var stbl []byte
	mp4Boxes(mdia, func(typ string, body []byte) {
		switch typ {
		case "mdhd":
			if len(body) >= 28 && body[0] == 1 {
				t.timescale = be.Uint32(body[20:])
			} else if len(body) >= 16 {
				t.timescale = be.Uint32(body[12:])
			}
		case "hdlr":
			if len(body) >= 12 {
				handler = string(body[8:12])
			}
		case "minf":
			mp4Boxes(body, func(typ string, body []byte) {
				if typ == "stbl" {
					stbl = body
				}
			})
		}
	})
	if handler != "soun" || stbl == nil {
		return nil, nil, nil
	}
	if t.timescale == 0 {
		return nil, nil, errors.New("no timescale")
	}
	var tables mp4Tables
	mp4Boxes(stbl, func(typ string, body []byte) {
		switch typ {
		case "stsd":
			tables.stsd = body
		case "stts":
			tables.stts = body
		case "stsc":
			tables.stsc = body
		case "stsz":
			tables.stsz = body
		case "stz2":
			tables.stz2 = body
		case "stco":
			tables.stco = body
		case "co64":
			tables.co64 = body
		}
	})
	format, codec, err := mp4Format(tables.stsd)
	if err != nil {
		return nil, nil, err
	}
	t.format, t.codec = format, codec
	t.shift, t.delay = editList(edts, movieScale, t.timescale)
	samples, err := tables.samples()
	if err != nil {
		return nil, nil, err
	}
	return t, samples, nil
}

// editList reads the simple edit lists muxers write: empty edits delaying the track, then
// one edit whose media time says where the track starts (e.g. skipping AAC priming).
func editList(edts []byte, movieScale, mediaScale uint32) (shift int64, delay time.Duration) {
	mp4Boxes(edts, func(typ string, b []byte) {
		if typ != "elst" || len(b) < 8 {
			return
		}
		v1 := b[0] == 1
		n := int(be.Uint32(b[4:]))
		es := 12
		if v1 {
			es = 20
		}
		b = b[8:]
		for i := 0; i < n && len(b) >= es; i, b = i+1, b[es:] {
			var dur uint64
			var mt int64
			if v1 {
				dur, mt = be.Uint64(b), int64(be.Uint64(b[8:]))
			} else {
				dur, mt = uint64(be.Uint32(b)), int64(int32(be.Uint32(b[4:])))
			}
			if mt == -1 {
				if movieScale > 0 {
					delay += mediatime.Ticks(int64(dur), 1, int64(movieScale))
				}
				continue
			}
			shift = mt
			return
		}
	})
	return shift, delay
}

type mp4Tables struct {
	stsd, stts, stsc, stsz, stz2, stco, co64 []byte
}

// samples builds the sample list from the sample tables, checking every count against the
// size of its box before allocating.
func (m mp4Tables) samples() ([]mp4Sample, error) {
	sizes, err := m.sampleSizes()
	if err != nil {
		return nil, err
	}
	n := len(sizes)
	if n == 0 {
		return nil, nil // fragmented: samples come with each moof
	}
	chunks, err := m.chunkOffsets()
	if err != nil {
		return nil, err
	}
	// PCM stores every audio sample as an MP4 sample of a few bytes; like ffmpeg, hand
	// out whole chunks instead
	perChunk := len(m.stsz) >= 8 && be.Uint32(m.stsz[4:]) != 0 && be.Uint32(m.stsz[4:]) <= 64
	out := make([]mp4Sample, 0, min(n, 1<<16))
	// sample times (stts)
	if len(m.stts) < 8 {
		return nil, errors.New("no stts")
	}
	var durs []uint32
	sttsN := int(be.Uint32(m.stts[4:]))
	if sttsN > (len(m.stts)-8)/8 {
		return nil, errors.New("stts truncated")
	}
	for i := 0; i < sttsN && len(durs) < n; i++ {
		cnt, delta := int(be.Uint32(m.stts[8+8*i:])), be.Uint32(m.stts[12+8*i:])
		for j := 0; j < cnt && len(durs) < n; j++ {
			durs = append(durs, delta)
		}
	}
	// samples per chunk (stsc)
	if len(m.stsc) < 8 {
		return nil, errors.New("no stsc")
	}
	stscN := int(be.Uint32(m.stsc[4:]))
	if stscN > (len(m.stsc)-8)/12 || stscN == 0 {
		return nil, errors.New("stsc truncated")
	}
	var t int64
	s := 0
	for e := 0; e < stscN && s < n; e++ {
		first := int(be.Uint32(m.stsc[8+12*e:])) - 1
		per := int(be.Uint32(m.stsc[12+12*e:]))
		last := len(chunks)
		if e+1 < stscN {
			last = int(be.Uint32(m.stsc[8+12*(e+1):])) - 1
		}
		if first < 0 || last > len(chunks) || per <= 0 {
			return nil, fmt.Errorf("stsc entry %d out of range", e)
		}
		for ch := first; ch < last && s < n; ch++ {
			off := chunks[ch]
			chunk := mp4Sample{off: off, t: t}
			for j := 0; j < per && s < n; j++ {
				d := uint32(0)
				if s < len(durs) {
					d = durs[s]
				}
				if !perChunk {
					out = append(out, mp4Sample{off: off, size: sizes[s], dur: d, t: t})
				}
				chunk.size += sizes[s]
				chunk.dur += d
				off += int64(sizes[s])
				t += int64(d)
				s++
			}
			if perChunk {
				out = append(out, chunk)
			}
		}
	}
	if !slices.IsSortedFunc(out, bySampleOffset) {
		slices.SortStableFunc(out, bySampleOffset)
	}
	return out, nil
}

func (m mp4Tables) sampleSizes() ([]uint32, error) {
	switch {
	case len(m.stsz) >= 12:
		fixed, n := be.Uint32(m.stsz[4:]), int(be.Uint32(m.stsz[8:]))
		if n > mp4MaxSamples {
			return nil, fmt.Errorf("%d samples", n)
		}
		out := make([]uint32, n)
		if fixed != 0 {
			for i := range out {
				out[i] = fixed
			}
			return out, nil
		}
		if n > (len(m.stsz)-12)/4 {
			return nil, errors.New("stsz truncated")
		}
		for i := range out {
			out[i] = be.Uint32(m.stsz[12+4*i:])
		}
		return out, nil
	case len(m.stz2) >= 12:
		field, n := int(m.stz2[7]), int(be.Uint32(m.stz2[8:]))
		if n > mp4MaxSamples || (field != 4 && field != 8 && field != 16) {
			return nil, errors.New("bad stz2")
		}
		if n > (len(m.stz2)-12)*8/field {
			return nil, errors.New("stz2 truncated")
		}
		out := make([]uint32, n)
		d := m.stz2[12:]
		for i := range out {
			switch field {
			case 4:
				v := d[i/2]
				if i%2 == 0 {
					v >>= 4
				}
				out[i] = uint32(v & 15)
			case 8:
				out[i] = uint32(d[i])
			case 16:
				out[i] = uint32(be.Uint16(d[2*i:]))
			}
		}
		return out, nil
	}
	return nil, nil
}

func (m mp4Tables) chunkOffsets() ([]int64, error) {
	switch {
	case len(m.stco) >= 8:
		n := int(be.Uint32(m.stco[4:]))
		if n > (len(m.stco)-8)/4 {
			return nil, errors.New("stco truncated")
		}
		out := make([]int64, n)
		for i := range out {
			out[i] = int64(be.Uint32(m.stco[8+4*i:]))
		}
		return out, nil
	case len(m.co64) >= 8:
		n := int(be.Uint32(m.co64[4:]))
		if n > (len(m.co64)-8)/8 {
			return nil, errors.New("co64 truncated")
		}
		out := make([]int64, n)
		for i := range out {
			out[i] = int64(be.Uint64(m.co64[8+8*i:]) & math.MaxInt64)
		}
		return out, nil
	}
	return nil, errors.New("no chunk offsets")
}

// mp4Format maps the first sample description to the way ffmpeg gets the frames.
func mp4Format(stsd []byte) (AudioFormat, string, error) {
	if len(stsd) < 8 || be.Uint32(stsd[4:]) == 0 {
		return AudioFormat{}, "", errors.New("no sample description")
	}
	var entry []byte
	var format string
	mp4Boxes(stsd[8:], func(typ string, body []byte) {
		if entry == nil {
			format, entry = typ, body
		}
	})
	if len(entry) < 28 {
		return AudioFormat{}, format, fmt.Errorf("short %q sample entry", format)
	}
	version := be.Uint16(entry[8:])
	channels := int(be.Uint16(entry[16:]))
	rate := float64(be.Uint32(entry[24:])) / 65536
	children := entry[28:]
	switch version {
	case 1:
		if len(entry) < 44 {
			return AudioFormat{}, format, errors.New("short v1 sound description")
		}
		children = entry[44:]
	case 2:
		if len(entry) < 64 {
			return AudioFormat{}, format, errors.New("short v2 sound description")
		}
		rate = math.Float64frombits(be.Uint64(entry[32:]))
		channels = int(be.Uint32(entry[40:]))
		children = entry[64:]
	}
	child := func(want string) []byte {
		var found []byte
		var look func(b []byte)
		look = func(b []byte) {
			mp4Boxes(b, func(typ string, body []byte) {
				switch {
				case found != nil:
				case typ == want:
					found = body
				case typ == "wave": // QuickTime keeps esds inside wave
					look(body)
				}
			})
		}
		look(children)
		return found
	}
	raw := func(codec string) (AudioFormat, string, error) {
		return rawFormat(codec), format, nil
	}
	mkv := func(id string, private []byte) (AudioFormat, string, error) {
		return AudioFormat{Matroska: &MatroskaTrack{CodecID: id, CodecPrivate: private,
			SampleRate: rate, Channels: channels}}, format + " " + id, nil
	}
	switch format {
	case "mp4a":
		oti, asc, err := esds(child("esds"))
		if err != nil {
			return AudioFormat{}, format, err
		}
		switch oti {
		case 0x40, 0x66, 0x67, 0x68:
			if len(asc) < 2 {
				return AudioFormat{}, format, errors.New("AAC without AudioSpecificConfig")
			}
			return mkv("A_AAC", asc)
		case 0x69, 0x6B:
			return raw(codecMP3)
		case 0xA5:
			return raw(codecAC3)
		case 0xA6:
			return raw(codecEAC3)
		case 0xA9:
			return raw(codecDTS)
		}
		return AudioFormat{}, format, fmt.Errorf("mp4a object type 0x%02x", oti)
	case "ac-3":
		return raw(codecAC3)
	case "ec-3":
		return raw(codecEAC3)
	case ".mp3", "mp3 ":
		return raw(codecMP3)
	case "dtsc", "dtsh", "dtsl", "dtse":
		return raw(codecDTS)
	case "Opus":
		head, err := opusHead(child("dOps"))
		if err != nil {
			return AudioFormat{}, format, err
		}
		return mkv("A_OPUS", head)
	case "fLaC":
		d := child("dfLa")
		if len(d) < 4 {
			return AudioFormat{}, format, errors.New("fLaC without dfLa")
		}
		return mkv("A_FLAC", append([]byte("fLaC"), d[4:]...))
	case "alac":
		d := child("alac")
		if len(d) < 28 {
			return AudioFormat{}, format, errors.New("alac without its config")
		}
		return mkv("A_ALAC", d[4:28])
	case "sowt", "twos":
		f := "s16le"
		if format == "twos" {
			f = "s16be"
		}
		return pcmFormat(f, int(rate), channels), format, nil
	}
	return AudioFormat{}, format, fmt.Errorf("unsupported sample entry %q", format)
}

// esds returns the object type and decoder-specific info of an ES descriptor box.
func esds(b []byte) (oti byte, asc []byte, err error) {
	if len(b) < 4 {
		return 0, nil, errors.New("no esds")
	}
	b = b[4:] // FullBox
	tag, body, _ := mp4Descriptor(b)
	if tag != 0x03 || len(body) < 3 {
		return 0, nil, errors.New("no ES descriptor")
	}
	flags := body[2]
	body = body[3:]
	if flags&0x80 != 0 {
		body = skipBytes(body, 2)
	}
	if flags&0x40 != 0 && len(body) > 0 {
		body = skipBytes(body, 1+int(body[0]))
	}
	if flags&0x20 != 0 {
		body = skipBytes(body, 2)
	}
	for len(body) > 0 {
		tag, d, rest := mp4Descriptor(body)
		if tag == 0 {
			break
		}
		if tag == 0x04 && len(d) >= 13 {
			oti = d[0]
			if t5, info, _ := mp4Descriptor(d[13:]); t5 == 0x05 {
				asc = info
			}
			return oti, asc, nil
		}
		body = rest
	}
	return 0, nil, errors.New("no decoder config")
}

func skipBytes(b []byte, n int) []byte {
	if n > len(b) {
		return nil
	}
	return b[n:]
}

// mp4Descriptor reads one MPEG-4 descriptor (tag, expandable size, body).
func mp4Descriptor(b []byte) (tag byte, body, rest []byte) {
	if len(b) < 2 {
		return 0, nil, nil
	}
	tag = b[0]
	size, i := 0, 1
	for ; i < 5 && i < len(b); i++ {
		size = size<<7 | int(b[i]&0x7F)
		if b[i]&0x80 == 0 {
			i++
			break
		}
	}
	if size > len(b)-i {
		return 0, nil, nil
	}
	return tag, b[i : i+size], b[i+size:]
}

// opusHead converts an MP4 dOps box (big-endian) into the OpusHead that Matroska and
// ffmpeg expect (little-endian, with magic).
func opusHead(d []byte) ([]byte, error) {
	if len(d) < 11 || d[0] != 0 {
		return nil, errors.New("bad dOps")
	}
	h := []byte("OpusHead")
	h = append(h, 1, d[1])
	h = binary.LittleEndian.AppendUint16(h, be.Uint16(d[2:]))
	h = binary.LittleEndian.AppendUint32(h, be.Uint32(d[4:]))
	h = binary.LittleEndian.AppendUint16(h, be.Uint16(d[8:]))
	h = append(h, d[10])
	if d[10] != 0 {
		if len(d) < 13+int(d[1]) {
			return nil, errors.New("bad dOps channel mapping")
		}
		h = append(h, d[11:13+int(d[1])]...)
	}
	return h, nil
}

// parseMoof adds the audio samples of one movie fragment.
func (c *mp4Container) parseMoof(moofStart int64, b []byte) {
	if c.track == nil {
		return
	}
	nextBase := moofStart
	var added []mp4Sample
	mp4Boxes(b, func(typ string, box []byte) {
		if typ != "traf" {
			return
		}
		f := parseTraf(box)
		base := f.base
		if !f.haveBase {
			base = nextBase
			if f.baseIsMoof {
				base = moofStart
			}
		}
		f.defDur = cmp.Or(f.defDur, c.track.trexDur)
		f.defSize = cmp.Or(f.defSize, c.track.trexSize)
		mine := f.id == c.track.id
		if mine && !f.haveT {
			f.t, f.haveT = c.fragNext, c.fragNext >= 0
		}
		keep := mine && f.haveT
		end, ok := f.samples(base, keep, &added)
		if !ok {
			return // truncated or corrupt trun
		}
		nextBase = end
		if keep {
			c.fragNext = f.t
		}
	})
	c.addSamples(added)
}

// traf is what a track fragment says: its header (tfhd), its decode time (tfdt) and its
// runs of samples (trun, not parsed yet).
type traf struct {
	id, defDur, defSize  uint32
	base                 int64
	haveBase, baseIsMoof bool
	t                    int64 // decode time of the first sample
	haveT                bool
	truns                [][]byte
}

func parseTraf(b []byte) traf {
	var f traf
	mp4Boxes(b, func(typ string, x []byte) {
		switch typ {
		case "tfhd":
			f.parseTfhd(x)
		case "tfdt":
			if len(x) >= 12 && x[0] == 1 {
				f.t, f.haveT = int64(be.Uint64(x[4:])&math.MaxInt64), true
			} else if len(x) >= 8 {
				f.t, f.haveT = int64(be.Uint32(x[4:])), true
			}
		case "trun":
			f.truns = append(f.truns, x)
		}
	})
	return f
}

func (f *traf) parseTfhd(x []byte) {
	if len(x) < 8 {
		return
	}
	flags := be.Uint32(x) & 0xFFFFFF
	f.id = be.Uint32(x[4:])
	r := x[8:]
	if flags&tfhdBaseOffset != 0 && len(r) >= 8 {
		f.base, f.haveBase, r = int64(be.Uint64(r)&math.MaxInt64), true, r[8:]
	}
	if flags&tfhdDescription != 0 {
		r = skipBytes(r, 4)
	}
	if flags&tfhdDuration != 0 && len(r) >= 4 {
		f.defDur, r = be.Uint32(r), r[4:]
	}
	if flags&tfhdSize != 0 && len(r) >= 4 {
		f.defSize = be.Uint32(r)
	}
	f.baseIsMoof = flags&tfhdBaseIsMoof != 0
}

// samples walks the truns from base: it appends their samples to added when keep, moves
// f.t past them, and returns the offset after the last one; ok is false for a corrupt
// trun.
func (f *traf) samples(base int64, keep bool, added *[]mp4Sample) (end int64, ok bool) {
	data := base
	for _, r := range f.truns {
		if len(r) < 8 {
			break
		}
		flags := be.Uint32(r) & 0xFFFFFF
		n := int(be.Uint32(r[4:]))
		x := r[8:]
		if flags&trunDataOffset != 0 && len(x) >= 4 {
			data, x = base+int64(int32(be.Uint32(x))), x[4:]
		}
		if flags&trunFirstFlags != 0 {
			x = skipBytes(x, 4)
		}
		per := 0
		for _, fl := range []uint32{trunDuration, trunSize, trunFlags, trunCTO} {
			if flags&fl != 0 {
				per += 4
			}
		}
		// the count is only bounded by the data when samples carry fields; with
		// defaults only, bound it by what a fragment can plausibly hold
		if (per > 0 && n > len(x)/per) || n > mp4MaxFragQueue || n < 0 {
			return 0, false
		}
		for range n {
			dur, size := f.defDur, f.defSize
			if flags&trunDuration != 0 {
				dur, x = be.Uint32(x), x[4:]
			}
			if flags&trunSize != 0 {
				size, x = be.Uint32(x), x[4:]
			}
			if flags&trunFlags != 0 {
				x = x[4:]
			}
			if flags&trunCTO != 0 {
				x = x[4:]
			}
			if keep {
				*added = append(*added, mp4Sample{off: data, size: size, dur: dur, t: f.t})
			}
			data += int64(size)
			f.t += int64(dur)
		}
	}
	return data, true
}

// addSamples merges fragment samples into the (offset-sorted) list.
func (c *mp4Container) addSamples(s []mp4Sample) {
	if len(s) == 0 {
		return
	}
	if n := len(c.samples); n > 0 {
		i := sort.Search(n, func(i int) bool { return c.samples[i].off >= s[0].off })
		if i < n && c.samples[i].off == s[0].off {
			return // this fragment is already known (the player re-read it)
		}
	}
	// keep the read cursor on the sample it is on: a sample of the previous fragment may
	// still be half collected when the next moof completes in the same write
	curOff := int64(-1)
	if c.k >= 0 && c.k < len(c.samples) {
		curOff = c.samples[c.k].off
	}
	if drop := len(c.samples) - mp4MaxFragQueue; drop > 0 {
		if c.k >= 0 && c.k < drop {
			drop = c.k // never drop what has not been read yet
		}
		c.samples = append(c.samples[:0], c.samples[drop:]...)
	}
	sorted := len(c.samples) == 0 || c.samples[len(c.samples)-1].off < s[0].off
	c.samples = append(c.samples, s...)
	if !sorted {
		slices.SortStableFunc(c.samples, bySampleOffset)
	}
	switch {
	case c.k < 0:
	case curOff >= 0:
		c.k = sort.Search(len(c.samples), func(i int) bool { return c.samples[i].off >= curOff })
	default: // the cursor was past the end: continue with the first new sample
		c.k = sort.Search(len(c.samples), func(i int) bool { return c.samples[i].off >= s[0].off })
	}
}

// readSamples emits every sample that lies inside the contiguous bytes of this range.
func (c *mp4Container) readSamples(off int64, p []byte) {
	pos := off
	if c.k < 0 {
		c.k = sort.Search(len(c.samples), func(i int) bool { return c.samples[i].off >= off })
		c.inSample = false
	}
	for len(p) > 0 && c.k < len(c.samples) {
		s := c.samples[c.k]
		if !c.inSample {
			if s.size > mp4MaxSample || pos > s.off {
				c.k++ // started inside this sample, or absurd: skip it
				continue
			}
			if skip := s.off - pos; skip > 0 {
				if skip >= int64(len(p)) {
					return
				}
				p, pos = p[skip:], s.off
			}
			if int64(len(p)) >= int64(s.size) { // whole in this write: no copy
				c.k++
				c.emit(s, p[:s.size])
				p, pos = p[s.size:], pos+int64(s.size)
				continue
			}
			c.inSample, c.cur = true, c.cur[:0]
		}
		n := min(int64(s.size)-int64(len(c.cur)), int64(len(p)))
		c.cur = append(c.cur, p[:n]...)
		p, pos = p[n:], pos+n
		if int64(len(c.cur)) == int64(s.size) {
			c.inSample = false
			c.k++
			c.emit(s, c.cur)
		}
	}
}

func (c *mp4Container) emit(s mp4Sample, data []byte) {
	ts := int64(c.track.timescale)
	f := Frame{Time: mediatime.Ticks(s.t-c.track.shift, 1, ts) + c.track.delay, Dur: mediatime.Ticks(int64(s.dur), 1, ts), Data: data}
	if err := c.sink.Frame(f); err != nil {
		c.fail(err)
	}
}
