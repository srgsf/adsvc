// Package avi is a push-based AVI audio demuxer for the pass-through proxy.
//
// It is a Go port of the timing rules of the (modified) Media3 AviExtractor ("avi2"):
// chunk timestamps come from the index (idx1 or OpenDML ix##), frames inside a chunk are
// timed by scanning MP3/AC-3/E-AC-3/DTS frame headers. Unlike the Java extractor it never
// needs to seek: callers feed whatever byte ranges the player downloaded.
package avi

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/srgsf/adsvc/internal/mediatime"
)

var le = binary.LittleEndian

func fourcc(s string) uint32 { return le.Uint32([]byte(s)) }

var (
	ccRIFF = fourcc("RIFF")
	ccAVI  = fourcc("AVI ")
	ccLIST = fourcc("LIST")
	ccHdrl = fourcc("hdrl")
	ccAvih = fourcc("avih")
	ccStrl = fourcc("strl")
	ccStrh = fourcc("strh")
	ccStrf = fourcc("strf")
	ccIndx = fourcc("indx")
	ccMovi = fourcc("movi")
	ccIdx1 = fourcc("idx1")
	ccAuds = fourcc("auds")
)

// ErrNeedMore means the buffer ends before the structure being parsed.
type ErrNeedMore struct{ Need int64 }

func (e ErrNeedMore) Error() string { return fmt.Sprintf("avi: need %d bytes", e.Need) }

// Audio describes one audio stream (strh + strf) with Media3-style validated timing params.
type Audio struct {
	StreamID       int
	ChunkID        uint32 // "NNwb"
	FormatTag      uint16
	Codec          string // mp1, mp2, mp3, ac3, eac3, dts, pcm_s16le, pcm_u8, aac, ... ("" = unknown)
	Channels       int
	SampleRate     int
	AvgBytesPerSec int
	BlockAlign     int
	BitsPerSample  int

	// timing (validated like AudioStreamParams.from(...).validate(...))
	Scale, Rate, Start, SampleSize int64

	ODMLIndex []int64 // offsets of ix## chunks from the stream's indx super index
	ODMLDur   []int64 // dwDuration of each ix## (stream ticks), parallel to ODMLIndex
}

// Header is the parsed hdrl plus movi location.
type Header struct {
	MoviStart int64 // offset of the "LIST" header of movi
	MoviEnd   int64 // end of the movi list (idx1 usually starts here)
	HasIdx1   bool  // AVIF_HASINDEX
	Audio     []*Audio
}

// ParseHeader parses the file start (offset 0). b must reach the movi LIST header;
// ErrNeedMore tells how many bytes are required.
func ParseHeader(b []byte) (*Header, error) {
	if len(b) < 12 {
		return nil, ErrNeedMore{12}
	}
	if le.Uint32(b) != ccRIFF || le.Uint32(b[8:]) != ccAVI {
		return nil, errors.New("avi: not a RIFF/AVI file")
	}
	h := &Header{}
	pos := int64(12)
	for {
		if int64(len(b)) < pos+12 {
			return nil, ErrNeedMore{pos + 12}
		}
		id, size := le.Uint32(b[pos:]), int64(le.Uint32(b[pos+4:]))
		if id == ccLIST {
			typ := le.Uint32(b[pos+8:])
			switch typ {
			case ccHdrl:
				end := pos + 8 + size
				if int64(len(b)) < end {
					return nil, ErrNeedMore{end}
				}
				h.parseHdrl(b[pos+12 : end])
			case ccMovi:
				h.MoviStart = pos
				h.MoviEnd = pos + 8 + size
				return h, nil
			}
		}
		pos += 8 + size + size&1
	}
}

func (h *Header) parseHdrl(b []byte) {
	streamID := 0
	walk(b, func(id uint32, body []byte, listType uint32) {
		switch {
		case id == ccAvih && len(body) >= 16:
			h.HasIdx1 = le.Uint32(body[12:])&0x10 != 0
		case id == ccLIST && listType == ccStrl:
			if a := parseStrl(body, streamID); a != nil {
				h.Audio = append(h.Audio, a)
			}
			streamID++ // stream ids count every strl, even ignored ones
		}
	})
}

// walk iterates RIFF chunks; for LIST it passes the list body (after the type).
func walk(b []byte, fn func(id uint32, body []byte, listType uint32)) {
	for p := 0; p+8 <= len(b); {
		id, size := le.Uint32(b[p:]), int(le.Uint32(b[p+4:]))
		end := p + 8 + size
		if end > len(b) || size < 0 {
			end = len(b)
		}
		if id == ccLIST && end-p >= 12 {
			fn(id, b[p+12:end], le.Uint32(b[p+8:]))
		} else {
			fn(id, b[p+8:end], 0)
		}
		p = end + (size & 1)
	}
}

func parseStrl(b []byte, streamID int) *Audio {
	var strh, strf, indx []byte
	walk(b, func(id uint32, body []byte, _ uint32) { // a truncated strl still yields its chunks
		switch id {
		case ccStrh:
			strh = body
		case ccStrf:
			strf = body
		case ccIndx:
			indx = body
		}
	})
	if len(strh) < 48 || le.Uint32(strh) != ccAuds || len(strf) < 16 {
		return nil
	}
	a := &Audio{
		StreamID:       streamID,
		ChunkID:        uint32('0'+streamID/10) | uint32('0'+streamID%10)<<8 | uint32('w')<<16 | uint32('b')<<24,
		FormatTag:      le.Uint16(strf),
		Channels:       int(le.Uint16(strf[2:])),
		SampleRate:     int(le.Uint32(strf[4:])),
		AvgBytesPerSec: int(le.Uint32(strf[8:])),
		BlockAlign:     int(le.Uint16(strf[12:])),
		BitsPerSample:  int(le.Uint16(strf[14:])),
	}
	if a.FormatTag == 0xFFFE && len(strf) >= 18+22 && le.Uint16(strf[16:]) >= 22 { // WAVEFORMATEXTENSIBLE
		a.BitsPerSample = int(le.Uint16(strf[18:]))
		a.FormatTag = le.Uint16(strf[24:])
	}
	a.Codec = codecFromTag(a.FormatTag, a.BitsPerSample)

	scale, rate := int64(le.Uint32(strh[20:])), int64(le.Uint32(strh[24:]))
	start, sampleSize := int64(le.Uint32(strh[28:])), int64(le.Uint32(strh[44:]))
	a.setParams(scale, rate, start, sampleSize)

	if len(indx) >= 24 && indx[3] == 0 { // AVI_INDEX_OF_INDEXES
		longsPerEntry, n := int(le.Uint16(indx)), int(le.Uint32(indx[4:]))
		es := longsPerEntry * 4
		type sub struct{ off, dur int64 }
		var subs []sub
		for i, p := 0, 24; i < n && p+8 <= len(indx) && es >= 8; i, p = i+1, p+es {
			off := int64(le.Uint64(indx[p:]))
			if off <= 0 {
				continue
			}
			var dur int64 // dwDuration, in stream ticks (only in 16-byte entries)
			if es >= 16 && p+16 <= len(indx) {
				dur = int64(le.Uint32(indx[p+12:]))
			}
			subs = append(subs, sub{off, dur})
		}
		slices.SortFunc(subs, func(a, b sub) int { return cmp.Compare(a.off, b.off) })
		for _, s := range subs {
			a.ODMLIndex = append(a.ODMLIndex, s.off)
			a.ODMLDur = append(a.ODMLDur, s.dur)
		}
	}
	return a
}

// setParams mirrors AudioStreamParams.from() + validate().
func (a *Audio) setParams(scale, rate, start, sampleSize int64) {
	blockAlign := int64(a.BlockAlign)
	if scale == 0 || rate == 0 {
		scale, rate = 1, 48000
	}
	if sampleSize != 0 && blockAlign != 0 && sampleSize != blockAlign {
		sampleSize = blockAlign
	}
	if a.Codec != "pcm_s16le" && a.Codec != "pcm_u8" && a.Codec != "pcm_s24le" && a.Codec != "pcm_s32le" && a.Codec != "pcm_f32le" {
		if blockAlign <= 4 {
			blockAlign = 0
		}
		if (a.Codec == "mp3") && blockAlign == 1152 && sampleSize == 1152 {
			sampleSize = 0
		}
		if a.Codec == "dts" && blockAlign <= 12 {
			blockAlign = 0
		}
	}
	a.Scale, a.Rate, a.Start, a.SampleSize = scale, rate, start, sampleSize
	a.BlockAlign = int(blockAlign)
}

func codecFromTag(tag uint16, bits int) string {
	switch tag {
	case 0x0001:
		switch bits {
		case 8:
			return "pcm_u8"
		case 24:
			return "pcm_s24le"
		case 32:
			return "pcm_s32le"
		}
		return "pcm_s16le"
	case 0x0003:
		return "pcm_f32le"
	case 0x0055, 0x5500:
		return "mp3" // refined per frame header (layer 1/2/3)
	case 0x0050:
		return "mp2"
	case 0x2000:
		return "ac3"
	case 0x2001:
		return "dts"
	case 0x2006:
		return "eac3"
	case 0x00FF, 0x1610:
		return "aac"
	}
	return ""
}

// Time converts stream time units to a time.
func (a *Audio) Time(t int64) time.Duration {
	ss := max(a.SampleSize, 1)
	return mediatime.Ticks(t, a.Scale, a.Rate*ss)
}

// ChunkDuration is the length of one chunk in stream time units (AudioChunkReader.getDuration).
func (a *Audio) ChunkDuration(size int64) int64 {
	if a.SampleSize > 0 {
		return size
	}
	if a.BlockAlign > 0 {
		return (size + int64(a.BlockAlign) - 1) / int64(a.BlockAlign)
	}
	return 1
}

// Index is the per-audio-stream chunk table: offset of the chunk header, payload size and
// start time in stream units. Times use int64 (the Java version overflows int for long
// CBR/DTS files, where time units are bytes).
type Index struct {
	a      *Audio
	Chunks []Chunk // by offset
	cum    int64
}

// Chunk is one audio chunk of the index.
type Chunk struct {
	Off  int64 // file offset of the chunk header
	Time int64 // stream time of its first sample (Audio.Time)
	Size int32 // payload bytes
}

// NewIndex creates an empty chunk index for stream a; feed it with AddIdx1 or AddIx.
func NewIndex(a *Audio) *Index {
	ss := max(a.SampleSize, 1)
	return &Index{a: a, cum: a.Start * ss}
}

// MaxChunk bounds the size of one audio chunk. Index entries beyond it are corrupt (sizes
// come from unsigned 32-bit fields and would otherwise turn negative).
const MaxChunk = 16 << 20

// append adds a chunk and reports whether it was added (not a duplicate, empty or corrupt).
func (ix *Index) append(offset, size int64) bool {
	if size < 0 || size > MaxChunk || offset < 0 {
		return false
	}
	t := ix.cum
	ix.cum += ix.a.ChunkDuration(size)
	if n := len(ix.Chunks); n > 0 && ix.Chunks[n-1].Off == offset {
		return false
	}
	if size == 0 {
		return false
	}
	ix.Chunks = append(ix.Chunks, Chunk{Off: offset, Time: t, Size: int32(size)})
	return true
}

// AddIdx1 parses the body of an idx1 chunk (without its 8-byte header).
func (ix *Index) AddIdx1(body []byte, moviStart int64) {
	if len(body) < 16 {
		return
	}
	// offsets are relative to the "movi" fourcc, or (some muxers) absolute
	var base int64
	if int64(le.Uint32(body[8:])) <= moviStart {
		base = moviStart + 8
	}
	for p := 0; p+16 <= len(body); p += 16 {
		if le.Uint32(body[p:]) != ix.a.ChunkID {
			continue
		}
		ix.append(int64(le.Uint32(body[p+8:]))+base, int64(le.Uint32(body[p+12:])))
	}
}

// AddIx parses the body of an OpenDML ix## standard index chunk (without its 8-byte header).
func (ix *Index) AddIx(body []byte) {
	if len(body) < 24 || body[3] != 1 { // AVI_INDEX_OF_CHUNKS
		return
	}
	longsPerEntry, n := int(le.Uint16(body)), int(le.Uint32(body[4:]))
	base := int64(le.Uint64(body[12:]))
	es := longsPerEntry * 4
	for i, p := 0, 24; i < n && p+es <= len(body) && es >= 8; i, p = i+1, p+es {
		off := int64(le.Uint32(body[p:]))
		size := int32(le.Uint32(body[p+4:]) & 0x7FFFFFFF)
		ix.append(base+off-8, int64(size)) // ix offsets point at chunk data; we store the header position
	}
}

// Len returns the number of indexed chunks.
func (ix *Index) Len() int { return len(ix.Chunks) }

// ChunkAtOrAfter returns the first chunk whose header starts at or after off.
func (ix *Index) ChunkAtOrAfter(off int64) int {
	i, _ := slices.BinarySearchFunc(ix.Chunks, off, func(c Chunk, off int64) int { return cmp.Compare(c.Off, off) })
	return i
}

// ODMLStart returns the stream time of the first chunk indexed by the i-th ix## chunk,
// summing the durations the super index carries. It lets a proxy add the ix## chunks it
// happens to see (after a seek the earlier ones are never downloaded) without having to
// know the ones before them.
func (a *Audio) ODMLStart(i int) int64 {
	ss := max(a.SampleSize, 1)
	t := a.Start
	for j := 0; j < i && j < len(a.ODMLDur); j++ {
		t += a.ODMLDur[j]
	}
	return t * ss
}

// AddIxAt adds an OpenDML ix## standard index whose first chunk starts at stream time
// start (see Audio.ODMLStart). Unlike AddIx it may be called out of file order.
func (ix *Index) AddIxAt(body []byte, start int64) {
	saved := ix.cum
	ix.cum = start
	ix.AddIx(body)
	if ix.cum < saved {
		ix.cum = saved
	}
	ix.sortByOffset()
}

func (ix *Index) sortByOffset() {
	byOff := func(a, b Chunk) int { return cmp.Compare(a.Off, b.Off) }
	if !slices.IsSortedFunc(ix.Chunks, byOff) {
		slices.SortStableFunc(ix.Chunks, byOff)
	}
}
