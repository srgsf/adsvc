package avi

// Frame header scanners for the self-framed codecs found in AVI. Durations are kept in
// samples (not rounded microseconds) so timestamps don't drift over long files.

type parseStatus int

const (
	frameOK parseStatus = iota
	frameNeedMore
	frameInvalid
)

type scanner interface {
	// parse inspects a candidate frame header at b[0:].
	parse(b []byte) (size, samples int, st parseStatus)
	// syncCandidate returns the index of the next possible sync word, or -1.
	syncCandidate(b []byte) int
}

func newScanner(a *Audio) scanner {
	switch a.Codec {
	case "mp1", "mp2", "mp3":
		return &mpegScanner{sampleRate: a.SampleRate, channels: a.Channels}
	case "ac3", "eac3":
		return &ac3Scanner{}
	case "dts":
		return &dtsScanner{}
	}
	return nil
}

// ---- MPEG-1/2/2.5 layer I/II/III ----

var mpegBitrates = [2][3][16]int{ // [v1|v2][L1,L2,L3][idx] kbps
	{
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, -1},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, -1},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, -1},
	},
	{
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, -1},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1},
	},
}

type mpegScanner struct{ sampleRate, channels int }

func (s *mpegScanner) syncCandidate(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0xFF && b[i+1]&0xE0 == 0xE0 && b[i+1]&0x06 != 0 {
			return i
		}
	}
	return -1
}

// mpegHeader decodes a 4-byte MPEG audio header.
func mpegHeader(b []byte) (size, samples, sampleRate, channels, layer int, ok bool) {
	if b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return
	}
	ver := int(b[1]>>3) & 3 // 0: 2.5, 1: reserved, 2: v2, 3: v1
	lay := int(b[1]>>1) & 3 // 1: L3, 2: L2, 3: L1
	bri := int(b[2] >> 4)
	sri := int(b[2]>>2) & 3
	pad := int(b[2]>>1) & 1
	if ver == 1 || lay == 0 || bri == 0 || bri == 15 || sri == 3 {
		return
	}
	layer = 4 - lay
	sampleRate = [3]int{44100, 48000, 32000}[sri]
	vi := 0
	switch ver {
	case 2:
		sampleRate /= 2
		vi = 1
	case 0:
		sampleRate /= 4
		vi = 1
	}
	br := mpegBitrates[vi][layer-1][bri] * 1000
	switch layer {
	case 1:
		samples = 384
		size = (12*br/sampleRate + pad) * 4
	case 2:
		samples = 1152
		size = 144*br/sampleRate + pad
	default:
		samples = 1152
		if vi == 1 {
			samples = 576
		}
		size = samples/8*br/sampleRate + pad
	}
	channels = 2
	if b[3]>>6 == 3 {
		channels = 1
	}
	return size, samples, sampleRate, channels, layer, size >= 21
}

func (s *mpegScanner) parse(b []byte) (int, int, parseStatus) {
	if len(b) < 4 {
		return 0, 0, frameNeedMore
	}
	size, samples, sr, ch, _, ok := mpegHeader(b)
	// like Mp3Scanner: reject headers that disagree with the stream format (false syncs)
	if !ok || (s.sampleRate > 0 && sr != s.sampleRate) || (s.channels > 0 && ch != s.channels) {
		return 0, 0, frameInvalid
	}
	return size, samples, frameOK
}

// ---- AC-3 and E-AC-3 ----

var ac3Size441 = [19]int{69, 87, 104, 121, 139, 174, 208, 243, 278, 348, 417, 487, 557, 696, 835, 975, 1114, 1253, 1393}
var ac3Bitrate = [19]int{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}

type ac3Scanner struct{}

func (ac3Scanner) syncCandidate(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0x0B && b[i+1] == 0x77 {
			return i
		}
	}
	return -1
}

func (ac3Scanner) parse(b []byte) (int, int, parseStatus) {
	if len(b) < 6 {
		return 0, 0, frameNeedMore
	}
	if b[0] != 0x0B || b[1] != 0x77 {
		return 0, 0, frameInvalid
	}
	bsid := int(b[5] >> 3)
	switch {
	case bsid <= 10: // AC-3
		fscod, frmsizecod := int(b[4]>>6), int(b[4]&0x3F)
		if fscod == 3 || frmsizecod/2 >= len(ac3Bitrate) {
			return 0, 0, frameInvalid
		}
		var size int
		switch fscod {
		case 0: // 48 kHz
			size = 4 * ac3Bitrate[frmsizecod/2]
		case 1: // 44.1 kHz
			size = 2 * (ac3Size441[frmsizecod/2] + frmsizecod%2)
		case 2: // 32 kHz
			size = 6 * ac3Bitrate[frmsizecod/2]
		}
		return size, 1536, frameOK
	case bsid <= 16: // E-AC-3
		strmtyp := int(b[2] >> 6)
		size := ((int(b[2]&7)<<8 | int(b[3])) + 1) * 2
		fscod := int(b[4] >> 6)
		blocks := 6
		if fscod != 3 {
			blocks = [4]int{1, 2, 3, 6}[int(b[4]>>4)&3]
		}
		samples := 256 * blocks
		if strmtyp == 1 { // dependent substream: same time span as its independent frame
			samples = 0
		}
		if strmtyp == 3 || size < 6 {
			return 0, 0, frameInvalid
		}
		return size, samples, frameOK
	}
	return 0, 0, frameInvalid
}

// ---- DTS core (16-bit big/little endian; 14-bit packing is not supported) ----

type dtsScanner struct{}

func (dtsScanner) syncCandidate(b []byte) int {
	for i := 0; i+3 < len(b); i++ {
		if (b[i] == 0x7F && b[i+1] == 0xFE && b[i+2] == 0x80 && b[i+3] == 0x01) ||
			(b[i] == 0xFE && b[i+1] == 0x7F && b[i+2] == 0x01 && b[i+3] == 0x80) {
			return i
		}
	}
	return -1
}

func (dtsScanner) parse(b []byte) (int, int, parseStatus) {
	if len(b) < 10 {
		return 0, 0, frameNeedMore
	}
	var h [10]byte
	switch {
	case b[0] == 0x7F && b[1] == 0xFE && b[2] == 0x80 && b[3] == 0x01:
		copy(h[:], b[:10])
	case b[0] == 0xFE && b[1] == 0x7F && b[2] == 0x01 && b[3] == 0x80:
		for i := 0; i < 10; i += 2 {
			h[i], h[i+1] = b[i+1], b[i]
		}
	default:
		return 0, 0, frameInvalid
	}
	nblks := int(h[4]&0x01)<<6 | int(h[5]>>2)
	fsize := (int(h[5]&0x03)<<12 | int(h[6])<<4 | int(h[7]>>4)) + 1
	if fsize < 96 || fsize > 16384 || nblks < 5 {
		return 0, 0, frameInvalid
	}
	return fsize, (nblks + 1) * 32, frameOK
}
