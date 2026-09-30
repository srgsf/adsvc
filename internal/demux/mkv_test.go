package demux

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/srgsf/adsvc/internal/testmedia"
)

func TestMKVTimeline(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	tests := []struct {
		name  string
		ext   string
		args  []string
		video bool
		tol   float64
	}{
		{"aac", ".mkv", []string{"-c:a", "aac", "-b:a", "96k"}, true, 0.001},
		{"ac3", ".mkv", []string{"-c:a", "ac3"}, true, 0.001},
		{"eac3", ".mkv", []string{"-c:a", "eac3"}, true, 0.001},
		{"mp3", ".mkv", []string{"-c:a", "libmp3lame"}, true, 0.001},
		{"flac", ".mkv", []string{"-c:a", "flac"}, true, 0.001},
		{"vorbis", ".mkv", []string{"-c:a", "vorbis", "-strict", "-2"}, true, 0.001},
		{"pcm", ".mkv", []string{"-c:a", "pcm_s16le"}, true, 0.001},
		{"opus", ".mkv", []string{"-c:a", "libopus"}, true, 0.001},
		{"opus-webm", ".webm", []string{"-vn", "-c:a", "libopus"}, false, 0.001},
	}
	dir := t.TempDir()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name+tt.ext)
			testmedia.Tone(t, path, tt.args...)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			full := demux(t, "matroska", data)
			if full.configured != 1 {
				t.Fatalf("configured %d times", full.configured)
			}
			checkAgainstProbe(t, full.frames, probeAudio(t, path), tt.tol)

			// a player: the file header, then a seek to 37% (no Cues needed)
			seek := int64(len(data)) * 37 / 100
			part := demux(t, "matroska", data, span{0, 64 << 10}, span{seek, int64(len(data))})
			var after []Frame
			for _, f := range part.frames {
				if f.Time.Seconds() > 5 {
					after = append(after, f)
				}
			}
			checkSeekSuffix(t, full.frames, after)
			t.Logf("%d frames, seek at byte %d -> first frame %.3fs, format %+v",
				len(full.frames), seek, after[0].Time.Seconds(), formatName(full.format))
		})
	}
}

func formatName(f AudioFormat) string {
	if f.Matroska != nil {
		return "matroska " + f.Matroska.CodecID
	}
	return "raw " + f.Args[len(f.Args)-1]
}

// TestMKVLacing covers what ffmpeg never writes but mkvmerge does: laced blocks (Xiph,
// EBML, fixed), with and without DefaultDuration, and header-stripping compression.
func TestMKVLacing(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	testmedia.Tone(t, src, "-c:a", "ac3", "-b:a", "192k") // CBR: equal frame sizes, fixed lacing works
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	orig := demux(t, "matroska", data).frames
	// the source starts 5 ms before zero (AC-3 priming, stored as CodecDelay); the crafted
	// files have no CodecDelay, so they start at zero
	base := orig[0].Time
	for i := range orig {
		orig[i].Time -= base
	}

	tests := []struct {
		name       string
		lacing     byte
		defaultDur bool
		strip      []byte
	}{
		{"xiph", 1, true, nil},
		{"ebml", 3, true, nil},
		{"fixed", 2, true, nil},
		{"xiph-no-default-duration", 1, false, nil},
		{"header-stripping", 0, true, []byte{0x0B, 0x77}},
		{"ebml-and-header-stripping", 3, false, []byte{0x0B, 0x77}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name+".mkv")
			file := writeLacedMKV(orig, 6, tt.lacing, tt.defaultDur, tt.strip)
			if err := os.WriteFile(path, file, 0o644); err != nil {
				t.Fatal(err)
			}
			// the file must be valid for ffmpeg before it can judge our demuxer
			pk := probeAudio(t, path)
			if len(pk) != len(orig) {
				t.Fatalf("ffprobe sees %d packets in the crafted file, want %d", len(pk), len(orig))
			}
			got := demux(t, "matroska", file).frames
			if len(got) != len(orig) {
				t.Fatalf("%d frames, want %d", len(got), len(orig))
			}
			for i := range orig {
				if !bytes.Equal(got[i].Data, orig[i].Data) {
					t.Fatalf("frame %d differs from the original", i)
				}
				if d := math.Abs(got[i].Time.Seconds() - orig[i].Time.Seconds()); d > 0.001 {
					t.Fatalf("frame %d at %.4fs, want %.4fs", i, got[i].Time.Seconds(), orig[i].Time.Seconds())
				}
			}
		})
	}
}

// writeLacedMKV writes frames (AC-3, 48 kHz, 32 ms each) into a Matroska file, perBlock
// frames per SimpleBlock with the given lacing, optionally with header stripping.
func writeLacedMKV(frames []Frame, perBlock int, lacing byte, defaultDur bool, strip []byte) []byte {
	var entry []byte
	entry = appendUint(entry, elTrackNumber, 1)
	entry = appendUint(entry, elTrackUID, 1)
	entry = appendUint(entry, elTrackType, 2)
	entry = appendElement(entry, elCodecID, []byte("A_AC3"))
	if defaultDur {
		entry = appendUint(entry, elDefaultDur, 32_000_000)
	}
	var audio []byte
	audio = appendFloat(audio, elSamplingFreq, 48000)
	audio = appendUint(audio, elChannels, 2)
	entry = appendElement(entry, elAudio, audio)
	if len(strip) > 0 {
		var comp []byte
		comp = appendUint(comp, elContentCompAl, 3)
		comp = appendElement(comp, elContentCompSt, strip)
		enc := appendElement(nil, elContentComp, comp)
		entry = appendElement(entry, elContentEncs, appendElement(nil, elContentEnc, enc))
	}
	var seg []byte
	seg = appendElement(seg, elInfo, appendUint(nil, elTimecodeScale, 1_000_000))
	seg = appendElement(seg, elTracks, appendElement(nil, elTrackEntry, entry))
	for i := 0; i < len(frames); i += perBlock {
		group := frames[i:min(i+perBlock, len(frames))]
		ms := int64(math.Round(group[0].Time.Seconds() * 1000))
		payload := []byte{0x81, 0, 0, 0x80}
		stripped := make([][]byte, len(group))
		for j, f := range group {
			stripped[j] = bytes.TrimPrefix(f.Data, strip)
		}
		if lacing != 0 && len(group) > 1 {
			payload[3] |= lacing << 1
			payload = append(payload, byte(len(group)-1))
			switch lacing {
			case 1:
				for _, f := range stripped[:len(group)-1] {
					n := len(f)
					for ; n >= 255; n -= 255 {
						payload = append(payload, 255)
					}
					payload = append(payload, byte(n))
				}
			case 3:
				payload = appendSize(payload, uint64(len(stripped[0])))
				for j := 1; j < len(group)-1; j++ {
					payload = putSignedVint(payload, int64(len(stripped[j])-len(stripped[j-1])))
				}
			}
		} else if len(group) > 1 {
			// no lacing: one frame per block
			for _, f := range group {
				ms := int64(math.Round(f.Time.Seconds() * 1000))
				cl := appendUint(nil, elTimecode, uint64(ms))
				cl = appendElement(cl, elSimpleBlock, append([]byte{0x81, 0, 0, 0x80}, bytes.TrimPrefix(f.Data, strip)...))
				seg = appendElement(seg, elCluster, cl)
			}
			continue
		}
		for _, f := range stripped {
			payload = append(payload, f...)
		}
		cl := appendUint(nil, elTimecode, uint64(ms))
		cl = appendElement(cl, elSimpleBlock, payload)
		seg = appendElement(seg, elCluster, cl)
	}
	var head []byte
	head = appendElement(head, elDocType, []byte("matroska"))
	head = appendUint(head, elDocTypeVer, 4)
	head = appendUint(head, elDocTypeReadV, 2)
	return appendElement(appendElement(nil, elHeader, head), elSegment, seg)
}

// putSignedVint writes an EBML-lacing size difference: the shortest length whose bias
// covers v, value v+bias.
func putSignedVint(b []byte, v int64) []byte {
	for l := 1; l <= 8; l++ {
		bias := int64(1)<<(7*l-1) - 1
		if v >= -bias && v <= bias {
			u := uint64(v+bias) | 1<<(7*l)
			for i := l - 1; i >= 0; i-- {
				b = append(b, byte(u>>(8*i)))
			}
			return b
		}
	}
	panic("difference too large")
}

// TestMatroskaRewrap checks that the codecs ffmpeg cannot read raw reach it through the
// audio-only Matroska stream, and decode to the full length of the file.
// TestMKVUnsupportedTrack: an encrypted or zlib-compressed track is reported, not decoded.
func TestMKVUnsupportedTrack(t *testing.T) {
	var comp []byte
	comp = appendUint(comp, elContentCompAl, 0) // zlib
	var entry []byte
	entry = appendUint(entry, elTrackNumber, 1)
	entry = appendUint(entry, elTrackType, 2)
	entry = appendElement(entry, elCodecID, []byte("A_AAC"))
	entry = appendElement(entry, elContentEncs, appendElement(nil, elContentEnc, appendElement(nil, elContentComp, comp)))
	if tr := parseMkvTrack(entry); tr.unsupported == "" {
		t.Fatalf("zlib-compressed track accepted: %+v", tr)
	}
}

func TestReadVint(t *testing.T) {
	tests := []struct {
		name    string
		in      []byte
		v       uint64
		n       int
		unknown bool
	}{
		{"one byte", []byte{0x81}, 1, 1, false},
		{"two bytes", []byte{0x40, 0x02}, 2, 2, false},
		{"unknown 1", []byte{0xFF}, 0x7F, 1, true},
		{"unknown 8", ebmlUnknownSize, 1<<56 - 1, 8, true},
		{"truncated", []byte{0x40}, 0, 0, false},
		{"invalid", []byte{0x00}, 0, -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, n, unknown := readVint(tt.in)
			if n != tt.n || (n > 0 && (v != tt.v || unknown != tt.unknown)) {
				t.Errorf("readVint(%x) = %d,%d,%v want %d,%d,%v", tt.in, v, n, unknown, tt.v, tt.n, tt.unknown)
			}
		})
	}
	for _, n := range []uint64{0, 1, 126, 127, 16382, 16383, 1 << 40} {
		b := appendSize(nil, n)
		if v, l, unk := readVint(b); v != n || l != len(b) || unk {
			t.Errorf("appendSize(%d) = %x reads back as %d (len %d, unknown %v)", n, b, v, l, unk)
		}
	}
}
