// Matroska element IDs and the EBML writing primitives, shared by the Matroska demuxer
// (mkv.go) and the audio-only Matroska writer (mkvwrite.go).

package demux

import (
	"encoding/binary"
	"math"
)

// Matroska element IDs.
const (
	elHeader        = 0x1A45DFA3
	elEBMLVersion   = 0x4286
	elEBMLReadVer   = 0x42F7
	elEBMLMaxIDLen  = 0x42F2
	elEBMLMaxSzLen  = 0x42F3
	elDocType       = 0x4282
	elDocTypeVer    = 0x4287
	elDocTypeReadV  = 0x4285
	elSegment       = 0x18538067
	elInfo          = 0x1549A966
	elTimecodeScale = 0x2AD7B1
	elDuration      = 0x4489
	elMuxingApp     = 0x4D80
	elWritingApp    = 0x5741
	elTracks        = 0x1654AE6B
	elTrackEntry    = 0xAE
	elTrackNumber   = 0xD7
	elTrackUID      = 0x73C5
	elTrackType     = 0x83
	elFlagDefault   = 0x88
	elCodecID       = 0x86
	elCodecPrivate  = 0x63A2
	elDefaultDur    = 0x23E383
	elCodecDelay    = 0x56AA
	elAudio         = 0xE1
	elSamplingFreq  = 0xB5
	elChannels      = 0x9F
	elBitDepth      = 0x6264
	elContentEncs   = 0x6D80
	elContentEnc    = 0x6240
	elContentComp   = 0x5034
	elContentCompAl = 0x4254
	elContentCompSt = 0x4255
	elCluster       = 0x1F43B675
	elTimecode      = 0xE7
	elSimpleBlock   = 0xA3
	elBlockGroup    = 0xA0
	elBlock         = 0xA1
	elBlockDuration = 0x9B
	elCRC32         = 0xBF
	elVoid          = 0xEC
	elPosition      = 0xA7
	elPrevSize      = 0xAB
	elContentEncr   = 0x5035

	mkvTrackAudio = 2 // TrackType of an audio track
	elCues        = 0x1C53BB6B
	elSeekHead    = 0x114D9B74
	elTags        = 0x1254C367
	elChapters    = 0x1043A770
	elAttachments = 0x1941A469
)

// ebmlUnknownSize is the reserved "size unknown" value (all ones, 8 bytes).
var ebmlUnknownSize = []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

// appendID appends an element ID in its natural width.
func appendID(b []byte, id uint32) []byte {
	switch {
	case id >= 1<<24:
		return append(b, byte(id>>24), byte(id>>16), byte(id>>8), byte(id))
	case id >= 1<<16:
		return append(b, byte(id>>16), byte(id>>8), byte(id))
	case id >= 1<<8:
		return append(b, byte(id>>8), byte(id))
	}
	return append(b, byte(id))
}

// appendSize appends n as the shortest EBML variable-size integer (the all-ones value of each
// length is reserved for "unknown", hence the -1).
func appendSize(b []byte, n uint64) []byte {
	l := 1
	for l < 8 && n >= 1<<(7*l)-1 {
		l++
	}
	v := n | 1<<(7*l)
	for i := l - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*i)))
	}
	return b
}

// appendElement appends a whole element: ID, size and payload.
func appendElement(b []byte, id uint32, payload []byte) []byte {
	b = appendID(b, id)
	b = appendSize(b, uint64(len(payload)))
	return append(b, payload...)
}

// appendUint appends an unsigned integer element in the fewest bytes.
func appendUint(b []byte, id uint32, v uint64) []byte {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	i := 0
	for i < 7 && tmp[i] == 0 {
		i++
	}
	return appendElement(b, id, tmp[i:])
}

// appendFloat appends a 64-bit float element.
func appendFloat(b []byte, id uint32, v float64) []byte {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], math.Float64bits(v))
	return appendElement(b, id, tmp[:])
}
