package demux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"
)

const (
	mkvMaxHeaderElem = 16 << 20  // Info / Tracks
	mkvMaxBlock      = 16 << 20  // one audio block
	mkvPeek          = 12        // enough of a block for track number, timecode and flags
	mkvClusterMarker = 4 + 8 + 1 // resync: Cluster ID, its size (8 bytes at most), a child's first byte
)

var errMkvLaced = errors.New("bad lacing")

// mkvTrack is the subset of a TrackEntry the demuxer needs.
type mkvTrack struct {
	number      uint64
	typ         uint64
	codecID     string
	private     []byte
	rate        float64
	channels    int
	bitDepth    int
	defaultDur  uint64 // ns per frame, 0 = unknown
	codecDelay  uint64 // ns of encoder priming; block timestamps are shifted back by it
	flagDefault bool
	strip       []byte // header-stripping compression: bytes removed from every frame
	unsupported string // why the track cannot be decoded (other content encodings)
}

type mkvLevel struct {
	id  uint32
	end int64 // absolute end offset, -1 = unknown size
}

type mkvState int

const (
	mkvStHeader mkvState = iota // reading an element header
	mkvStBody                   // collecting an element's body into buf
	mkvStPeek                   // collecting the first bytes of a block
)

// mkvContainer is a push-based Matroska/WebM demuxer for the proxy.
//
// From offset 0 it walks the file in order: EBML header, Segment, Info (timestamp scale),
// Tracks, then Clusters. Every Cluster carries its absolute timestamp, so after a seek it
// resyncs on the next Cluster ID and needs no index (Cues) at all. Video blocks are skipped
// after peeking at their track number, so only audio is ever buffered.
type mkvContainer struct {
	sink Sink

	pos    int64 // absolute offset of the next byte for the parser
	synced bool
	st     mkvState
	buf    []byte // by st: the element header (mkvStHeader), its body (mkvStBody), a block head (mkvStPeek)
	need   int64  // mkvStBody/mkvStPeek: bytes wanted in buf
	id     uint32
	size   int64 // size of the element being collected
	skip   int64 // bytes to discard
	stack  []mkvLevel

	rs resync // finds the next Cluster while not synced

	scale     time.Duration // per timestamp unit (TimecodeScale)
	prime     int64         // the track's CodecDelay in timestamp units (setPrime)
	duration  time.Duration
	tracks    bool
	track     *mkvTrack
	clusterTC int64
	haveTC    bool
	start     time.Duration // timestamp of the first cluster: the player's timeline starts there
	haveStart bool
	fromStart bool // walking in order from offset 0 (so the next cluster may be the first)

	bgBlock []byte // BlockGroup: Block waiting for a possible BlockDuration
	bgDur   int64

	delay frameDelay
	lace  [][]byte // delace scratch
	err   error
}

func newMKVContainer(sink Sink) *mkvContainer {
	c := &mkvContainer{sink: sink, scale: time.Millisecond, bgDur: -1}
	c.rs = resync{pattern: []byte{0x1F, 0x43, 0xB6, 0x75}, size: mkvClusterMarker, check: validCluster}
	c.delay.emit = sink.Frame
	return c
}

func (c *mkvContainer) Range(off int64) {
	c.endGroup()
	if err := c.delay.flush(); err != nil {
		c.fail(err)
	}
	c.st, c.buf, c.skip, c.stack, c.haveTC = mkvStHeader, c.buf[:0], 0, c.stack[:0], false
	c.pos = off
	c.rs.reset()
	c.synced = off == 0
	c.fromStart = off == 0
}

func (c *mkvContainer) Close() error {
	c.endGroup()
	if err := c.delay.flush(); err != nil {
		c.fail(err)
	}
	c.sink.Close()
	return c.err
}

func (c *mkvContainer) Duration() time.Duration { return c.duration }

// setPrime computes c.prime from the track's CodecDelay and the timestamp scale, once
// either is known: like ffmpeg, the priming delay is rounded to timestamp units and taken
// off every block, so the first (priming) samples of the stream sit before zero.
func (c *mkvContainer) setPrime() {
	c.prime = 0
	if c.track != nil && c.scale > 0 {
		c.prime = int64(math.Round(float64(c.track.codecDelay) / float64(c.scale)))
	}
}

func (c *mkvContainer) fail(err error) {
	if c.err == nil {
		c.err = fmt.Errorf("mkv: %w", err)
	}
}

func (c *mkvContainer) Write(p []byte) error {
	for len(p) > 0 && c.err == nil {
		if !c.synced {
			p = c.resync(p)
			continue
		}
		if c.skip > 0 {
			n := min(int64(len(p)), c.skip)
			c.skip -= n
			c.pos += n
			p = p[n:]
			continue
		}
		switch c.st {
		case mkvStHeader:
			p = c.header(p)
		case mkvStBody, mkvStPeek:
			n := min(c.need-int64(len(c.buf)), int64(len(p)))
			c.buf = append(c.buf, p[:n]...)
			c.pos += n
			p = p[n:]
			if int64(len(c.buf)) == c.need {
				if c.st == mkvStPeek {
					c.peeked()
				} else {
					c.st = mkvStHeader
					c.body(c.id, c.buf)
					c.buf = c.buf[:0]
				}
			}
		}
	}
	return c.err
}

// header parses one element header (it may straddle writes) and returns the rest of p.
func (c *mkvContainer) header(p []byte) []byte {
	old := len(c.buf)
	take := min(12-old, len(p))
	c.buf = append(c.buf, p[:take]...)
	id, n1 := readID(c.buf)
	if n1 < 0 {
		c.pos += int64(take)
		c.lostSync("bad element ID")
		return p[take:]
	}
	if n1 == 0 {
		c.pos += int64(take)
		return p[take:]
	}
	size, n2, unknown := readVint(c.buf[n1:])
	if n2 < 0 {
		c.pos += int64(take)
		c.lostSync("bad element size")
		return p[take:]
	}
	if n2 == 0 {
		c.pos += int64(take)
		return p[take:]
	}
	hlen := n1 + n2
	used := hlen - old
	hdrStart := c.pos - int64(old)
	c.pos += int64(used)
	c.buf = c.buf[:0]
	sz := int64(size)
	if unknown || size > math.MaxInt64/2 {
		sz = -1
	}
	c.element(id, hdrStart, sz)
	return p[used:]
}

func (c *mkvContainer) lostSync(why string) {
	slog.Debug("mkv: lost sync, resyncing", "reason", why, "offset", c.pos)
	c.endGroup()
	c.synced, c.st, c.buf, c.skip, c.stack, c.haveTC = false, mkvStHeader, c.buf[:0], 0, c.stack[:0], false
	c.rs.reset()
	c.fromStart = false
}

// element dispatches an element whose header ends at c.pos. size < 0: unknown.
func (c *mkvContainer) element(id uint32, hdrStart, size int64) {
	c.popLevels(id, hdrStart)
	top := uint32(0)
	if n := len(c.stack); n > 0 {
		top = c.stack[n-1].id
	}
	end := int64(-1)
	if size >= 0 {
		end = c.pos + size
	}
	switch {
	case top == 0 && id == elHeader:
		c.collect(id, size, 4096)
	case top == 0 && id == elSegment:
		c.stack = append(c.stack, mkvLevel{id, end})
	case top == elSegment && (id == elInfo || id == elTracks):
		c.collect(id, size, mkvMaxHeaderElem)
	case top == elSegment && id == elCluster:
		c.stack = append(c.stack, mkvLevel{id, end})
		c.haveTC = false
	case top == elCluster && id == elTimecode:
		c.collect(id, size, 8)
	case top == elCluster && id == elSimpleBlock, top == elBlockGroup && id == elBlock:
		c.startPeek(id, size)
	case top == elCluster && id == elBlockGroup:
		c.stack = append(c.stack, mkvLevel{id, end})
		c.bgBlock, c.bgDur = c.bgBlock[:0], -1
	case top == elBlockGroup && id == elBlockDuration:
		c.collect(id, size, 8)
	default:
		if size < 0 {
			c.lostSync(fmt.Sprintf("element %X of unknown size", id))
			return
		}
		c.skip = size
	}
}

// popLevels closes the master elements that end before an element starting at off.
func (c *mkvContainer) popLevels(id uint32, off int64) {
	for n := len(c.stack); n > 0; n = len(c.stack) {
		l := c.stack[n-1]
		switch {
		case l.end >= 0 && off >= l.end:
		case l.end < 0 && l.id == elCluster && isMkvTopLevel(id):
		case l.end < 0 && l.id == elBlockGroup && (isMkvTopLevel(id) || isMkvClusterChild(id)):
		default:
			return
		}
		if l.id == elBlockGroup {
			c.endGroup()
		}
		c.stack = c.stack[:n-1]
	}
}

func isMkvTopLevel(id uint32) bool {
	switch id {
	case elCluster, elCues, elTags, elChapters, elAttachments, elSeekHead, elInfo,
		elTracks, elSegment, elHeader:
		return true
	}
	return false
}

func isMkvClusterChild(id uint32) bool {
	return id == elTimecode || id == elSimpleBlock || id == elBlockGroup
}

func (c *mkvContainer) collect(id uint32, size, max int64) {
	if size < 0 || size > max {
		if size < 0 {
			c.lostSync(fmt.Sprintf("element %X of unknown size", id))
			return
		}
		c.skip = size
		return
	}
	c.id, c.size, c.need, c.st = id, size, size, mkvStBody
	c.buf = c.buf[:0]
	if size == 0 {
		c.st = mkvStHeader
		c.body(id, nil)
	}
}

func (c *mkvContainer) startPeek(id uint32, size int64) {
	if size < 0 {
		c.lostSync("block of unknown size")
		return
	}
	if c.track == nil || !c.haveTC {
		c.skip = size
		return
	}
	c.id, c.size, c.st = id, size, mkvStPeek
	c.need = min(size, mkvPeek)
	c.buf = c.buf[:0]
	if size == 0 {
		c.st = mkvStHeader
	}
}

// peeked decides from the first bytes of a block whether it belongs to our track.
func (c *mkvContainer) peeked() {
	track, n, _ := readVint(c.buf)
	if n <= 0 || track != c.track.number || c.size > mkvMaxBlock {
		c.st = mkvStHeader
		c.skip = c.size - int64(len(c.buf))
		c.buf = c.buf[:0]
		return
	}
	c.need, c.st = c.size, mkvStBody
	if int64(len(c.buf)) == c.need {
		c.st = mkvStHeader
		c.body(c.id, c.buf)
		c.buf = c.buf[:0]
	}
}

func (c *mkvContainer) body(id uint32, b []byte) {
	switch id {
	case elHeader:
		if dt := ebmlString(b, elDocType); dt != "" && dt != "matroska" && dt != "webm" {
			c.fail(fmt.Errorf("doc type %q", dt))
		}
	case elInfo:
		c.parseInfo(b)
	case elTracks:
		c.parseTracks(b)
	case elTimecode:
		c.clusterTC, c.haveTC = int64(beUint(b)), true
		if c.fromStart && !c.haveStart {
			c.start, c.haveStart = time.Duration(c.clusterTC)*c.scale, true
		}
	case elSimpleBlock:
		c.block(b, -1)
	case elBlock:
		c.bgBlock = append(c.bgBlock[:0], b...)
	case elBlockDuration:
		c.bgDur = int64(beUint(b))
	}
}

// endGroup emits a BlockGroup's Block once the group (and its BlockDuration) is complete.
func (c *mkvContainer) endGroup() {
	if len(c.bgBlock) > 0 {
		c.block(c.bgBlock, c.bgDur)
	}
	c.bgBlock, c.bgDur = c.bgBlock[:0], -1
}

// block handles one SimpleBlock/Block of our track. dur is the BlockDuration (-1 = none).
func (c *mkvContainer) block(b []byte, dur int64) {
	_, n, _ := readVint(b)
	if n <= 0 || len(b) < n+3 {
		return
	}
	rel := int64(int16(binary.BigEndian.Uint16(b[n:])))
	flags := b[n+2]
	frames, err := delace(c.lace[:0], flags, b[n+3:])
	c.lace = frames
	if err != nil || len(frames) == 0 {
		slog.Debug("mkv: skipping bad block", "offset", c.pos, "err", err)
		return
	}
	t := time.Duration(c.clusterTC+rel-c.prime)*c.scale - c.start
	var per time.Duration
	switch {
	case dur > 0:
		per = time.Duration(dur) * c.scale / time.Duration(len(frames))
	case c.track.defaultDur > 0:
		per = time.Duration(c.track.defaultDur)
	}
	if err := c.delay.push(t, per, c.track.strip, frames...); err != nil {
		c.fail(err)
	}
}

// resync looks for the next Cluster after a seek (or corrupt data). Once found, parsing
// continues from the Cluster's ID as a child of the Segment; it returns what is left of p.
func (c *mkvContainer) resync(p []byte) []byte {
	head, rest, ok := c.rs.find(p)
	if !ok {
		c.pos += int64(len(p))
		return nil
	}
	c.pos += int64(len(p)-len(rest)) - int64(len(head)) // the Cluster's offset
	c.synced, c.st, c.haveTC = true, mkvStHeader, false
	c.stack = append(c.stack[:0], mkvLevel{elSegment, -1})
	if len(head) > 0 {
		// The Cluster began in the carried bytes: parse them first. They are copied, as
		// losing sync again inside them reuses the carry.
		var b [mkvClusterMarker]byte
		c.Write(append(b[:0], head...)) //nolint:errcheck // the error is c.err, returned by the caller
	}
	return rest
}

// validCluster checks a Cluster ID candidate: a valid size, then a first child that a
// cluster starts with (a resync.check).
func validCluster(b []byte) (ok, more bool) {
	if len(b) < 5 {
		return false, true
	}
	size, n, unknown := readVint(b[4:])
	if n == 0 {
		return false, true
	}
	if n < 0 || (!unknown && size < 3) {
		return false, false
	}
	hlen := 4 + n
	if len(b) < hlen+1 {
		return false, true
	}
	switch b[hlen] {
	case elTimecode, elCRC32, elPosition, elPrevSize, elVoid:
		return true, false
	}
	return false, false
}

func (c *mkvContainer) parseInfo(b []byte) {
	scale := uint64(1_000_000)
	var dur float64
	ebmlChildren(b, func(id uint32, v []byte) {
		switch id {
		case elTimecodeScale:
			if s := beUint(v); s > 0 {
				scale = s
			}
		case elDuration:
			dur = beFloat(v)
		}
	})
	c.scale = time.Duration(scale)
	c.setPrime()
	if dur > 0 && !math.IsInf(dur, 0) && !math.IsNaN(dur) {
		c.duration = time.Duration(math.Round(dur * float64(scale)))
	}
}

func (c *mkvContainer) parseTracks(b []byte) {
	if c.tracks {
		return
	}
	c.tracks = true
	var audio []*mkvTrack
	ebmlChildren(b, func(id uint32, v []byte) {
		if id != elTrackEntry {
			return
		}
		if t := parseMkvTrack(v); t.typ == mkvTrackAudio {
			audio = append(audio, t)
		}
	})
	// the player plays the default audio track; ads are dubbed into that one
	pick := func(wantDefault bool) *mkvTrack {
		for _, t := range audio {
			if t.unsupported != "" || (wantDefault && !t.flagDefault) {
				continue
			}
			return t
		}
		return nil
	}
	t := pick(true)
	if t == nil {
		t = pick(false)
	}
	if t == nil {
		for _, a := range audio {
			slog.Warn("mkv: audio track not supported", "track", a.number, "codec", a.codecID, "reason", a.unsupported)
		}
		slog.Warn("mkv: no usable audio track")
		return
	}
	if err := c.sink.Configure(mkvFormat(t)); err != nil {
		c.fail(err)
		return
	}
	c.track = t
	c.setPrime()
	slog.Info("mkv: audio track", "track", t.number, "codec", t.codecID, "rate", t.rate, "channels", t.channels)
}

func parseMkvTrack(b []byte) *mkvTrack {
	t := &mkvTrack{flagDefault: true}
	ebmlChildren(b, func(id uint32, v []byte) {
		switch id {
		case elTrackNumber:
			t.number = beUint(v)
		case elTrackType:
			t.typ = beUint(v)
		case elFlagDefault:
			t.flagDefault = beUint(v) != 0
		case elCodecID:
			t.codecID = string(bytes.TrimRight(v, "\x00"))
		case elCodecPrivate:
			t.private = append([]byte(nil), v...)
		case elDefaultDur:
			t.defaultDur = beUint(v)
		case elCodecDelay:
			t.codecDelay = beUint(v)
		case elAudio:
			ebmlChildren(v, func(id uint32, v []byte) {
				switch id {
				case elSamplingFreq:
					t.rate = beFloat(v)
				case elChannels:
					t.channels = int(beUint(v))
				case elBitDepth:
					t.bitDepth = int(beUint(v))
				}
			})
		case elContentEncs:
			ebmlChildren(v, func(id uint32, enc []byte) {
				if id != elContentEnc {
					return
				}
				comp, algo := false, uint64(0)
				var settings []byte
				ebmlChildren(enc, func(id uint32, v []byte) {
					switch id {
					case elContentComp:
						comp = true
						ebmlChildren(v, func(id uint32, v []byte) {
							switch id {
							case elContentCompAl:
								algo = beUint(v)
							case elContentCompSt:
								settings = append([]byte(nil), v...)
							}
						})
					case elContentEncr:
						t.unsupported = "encrypted"
					}
				})
				switch {
				case comp && algo == 3: // header stripping
					t.strip = append(t.strip, settings...)
				case comp:
					t.unsupported = fmt.Sprintf("compression algorithm %d", algo)
				}
			})
		}
	})
	if t.number == 0 {
		t.unsupported = "no track number"
	}
	return t
}

// mkvCodecs are the Matroska codec IDs of self-framed codecs (rawFormat).
var mkvCodecs = map[string]string{
	"A_AC3": codecAC3, "A_AC3/BSID9": codecAC3, "A_AC3/BSID10": codecAC3,
	"A_EAC3": codecEAC3,
	"A_DTS":  codecDTS, "A_DTS/EXPRESS": codecDTS, "A_DTS/LOSSLESS": codecDTS,
	"A_MPEG/L1": codecMP3, "A_MPEG/L2": codecMP3, "A_MPEG/L3": codecMP3,
}

// mkvFormat maps a Matroska audio track to the way ffmpeg gets its frames: self-framed
// codecs raw, everything else rewrapped into a minimal audio-only Matroska stream.
func mkvFormat(t *mkvTrack) AudioFormat {
	if f := rawFormat(mkvCodecs[t.codecID]); f.Valid() {
		return f
	}
	if pcm := mkvPCM(t); pcm != "" && t.rate > 0 && t.channels > 0 {
		return pcmFormat(pcm, int(t.rate), t.channels)
	}
	return AudioFormat{Matroska: &MatroskaTrack{CodecID: t.codecID, CodecPrivate: t.private,
		SampleRate: t.rate, Channels: t.channels, BitDepth: t.bitDepth}}
}

func mkvPCM(t *mkvTrack) string {
	switch t.codecID {
	case "A_PCM/INT/LIT":
		switch t.bitDepth {
		case 8:
			return "u8"
		case 16:
			return "s16le"
		case 24:
			return "s24le"
		case 32:
			return "s32le"
		}
	case "A_PCM/INT/BIG":
		switch t.bitDepth {
		case 16:
			return "s16be"
		case 24:
			return "s24be"
		case 32:
			return "s32be"
		}
	case "A_PCM/FLOAT/IEEE":
		switch t.bitDepth {
		case 32:
			return "f32le"
		case 64:
			return "f64le"
		}
	}
	return ""
}

// delace splits a block payload into frames according to its lacing flags, appending
// them to dst.
func delace(dst [][]byte, flags byte, b []byte) ([][]byte, error) {
	lacing := flags >> 1 & 3
	if lacing == 0 {
		return append(dst, b), nil
	}
	if len(b) < 1 {
		return dst, errMkvLaced
	}
	count := int(b[0]) + 1
	b = b[1:]
	var sizesBuf [256]int // count is at most 256
	sizes := sizesBuf[:count]
	switch lacing {
	case 1: // Xiph
		for i := 0; i < count-1; i++ {
			for {
				if len(b) == 0 {
					return dst, errMkvLaced
				}
				v := int(b[0])
				b = b[1:]
				sizes[i] += v
				if v != 255 {
					break
				}
			}
		}
	case 3: // EBML
		first, n, _ := readVint(b)
		if n <= 0 {
			return dst, errMkvLaced
		}
		b = b[n:]
		sizes[0] = int(first)
		for i := 1; i < count-1; i++ {
			raw, n, _ := readVint(b)
			if n <= 0 {
				return dst, errMkvLaced
			}
			b = b[n:]
			diff := int64(raw) - (int64(1)<<(7*n-1) - 1) // signed: subtract the bias
			sizes[i] = sizes[i-1] + int(diff)
			if sizes[i] < 0 {
				return dst, errMkvLaced
			}
		}
	case 2: // fixed
		if len(b)%count != 0 {
			return dst, errMkvLaced
		}
		for i := range sizes {
			sizes[i] = len(b) / count
		}
	}
	if lacing != 2 {
		used := 0
		for _, s := range sizes[:count-1] {
			used += s
		}
		if used > len(b) {
			return dst, errMkvLaced
		}
		sizes[count-1] = len(b) - used
	}
	for _, s := range sizes {
		if s > len(b) {
			return dst[:0], errMkvLaced
		}
		dst, b = append(dst, b[:s]), b[s:]
	}
	return dst, nil
}

// readID reads an EBML element ID (1-4 bytes, marker bits kept, as IDs are written).
// n == 0: more bytes needed; n < 0: invalid.
func readID(b []byte) (uint32, int) {
	if len(b) == 0 {
		return 0, 0
	}
	l := 1
	for l <= 4 && b[0]&(0x80>>(l-1)) == 0 {
		l++
	}
	if l > 4 {
		return 0, -1
	}
	if len(b) < l {
		return 0, 0
	}
	var id uint32
	for i := 0; i < l; i++ {
		id = id<<8 | uint32(b[i])
	}
	return id, l
}

// readVint reads an EBML variable-size integer (marker bit removed). n == 0: more bytes
// needed; n < 0: invalid. unknown reports the reserved all-ones value.
func readVint(b []byte) (v uint64, n int, unknown bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	l := 1
	for l <= 8 && b[0]&(0x80>>(l-1)) == 0 {
		l++
	}
	if l > 8 {
		return 0, -1, false
	}
	if len(b) < l {
		return 0, 0, false
	}
	v = uint64(b[0]) & (0xFF >> l)
	all := v == 0xFF>>l
	for i := 1; i < l; i++ {
		v = v<<8 | uint64(b[i])
		all = all && b[i] == 0xFF
	}
	return v, l, all
}

// ebmlChildren calls fn for each complete child element of a master element's body.
func ebmlChildren(b []byte, fn func(id uint32, body []byte)) {
	for len(b) > 0 {
		id, n1 := readID(b)
		if n1 <= 0 {
			return
		}
		size, n2, unknown := readVint(b[n1:])
		if n2 <= 0 || unknown || size > uint64(len(b)-n1-n2) {
			return
		}
		start := n1 + n2
		fn(id, b[start:start+int(size)])
		b = b[start+int(size):]
	}
}

func ebmlString(b []byte, want uint32) string {
	var s string
	ebmlChildren(b, func(id uint32, v []byte) {
		if id == want {
			s = string(bytes.TrimRight(v, "\x00"))
		}
	})
	return s
}

func beUint(b []byte) uint64 {
	var v uint64
	for i, x := range b {
		if i == 8 {
			break
		}
		v = v<<8 | uint64(x)
	}
	return v
}

func beFloat(b []byte) float64 {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b))
	}
	return 0
}
