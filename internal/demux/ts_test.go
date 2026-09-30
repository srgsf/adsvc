package demux

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/srgsf/adsvc/internal/testmedia"
)

func TestTSTimeline(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	tests := []struct {
		name string
		ext  string
		args []string
	}{
		{"aac-adts", ".ts", []string{"-c:a", "aac"}},
		{"aac-latm", ".ts", []string{"-c:a", "aac", "-mpegts_flags", "latm"}},
		{"mp2", ".ts", []string{"-c:a", "mp2"}},
		{"mp3", ".ts", []string{"-c:a", "libmp3lame"}},
		{"ac3", ".ts", []string{"-c:a", "ac3"}},
		{"ac3-dvb", ".ts", []string{"-c:a", "ac3", "-mpegts_flags", "system_b"}},
		{"eac3", ".ts", []string{"-c:a", "eac3"}},
		{"eac3-dvb", ".ts", []string{"-c:a", "eac3", "-mpegts_flags", "system_b"}},
		{"dts", ".ts", []string{"-c:a", "dca", "-strict", "-2"}},
		{"m2ts", ".m2ts", []string{"-c:a", "ac3", "-mpegts_m2ts_mode", "1"}},
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
			if typ := Sniff(data[:512]); typ != "mpegts" {
				t.Fatalf("Sniff = %q", typ)
			}
			full := demux(t, "mpegts", data)
			if full.configured != 1 {
				t.Fatalf("configured %d times", full.configured)
			}
			checkPESAgainstProbe(t, full.frames, probeAudio(t, path))

			// a player: the start of the file, then a seek to 37% (TS has no index)
			size := int64(len(data))
			part := demux(t, "mpegts", data, span{0, 64 << 10}, span{size * 37 / 100, size})
			var after []Frame
			for _, f := range part.frames {
				if f.Time.Seconds() > 5 {
					after = append(after, f)
				}
			}
			checkSeekSuffix(t, full.frames, after)
			t.Logf("%d PES, %s; after the seek from %.3fs", len(full.frames), formatName(full.format), after[0].Time.Seconds())
		})
	}
}

// checkPESAgainstProbe: a PES holds one or more of ffprobe's (parsed) frames. Every PES
// must start at the time of one of them, and the bytes must add up.
func checkPESAgainstProbe(t *testing.T, frames []Frame, pk []probePacket) {
	t.Helper()
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	var ours, theirs int
	for _, p := range pk {
		theirs += p.size
	}
	j := 0
	for i, f := range frames {
		for j < len(pk) && pk[j].pts < f.Time.Seconds()-0.0005 {
			j++
		}
		if j == len(pk) || math.Abs(pk[j].pts-f.Time.Seconds()) > 0.0005 {
			t.Fatalf("PES %d at %.4fs matches no ffprobe frame", i, f.Time.Seconds())
		}
		ours += len(f.Data)
	}
	if ours != theirs {
		t.Fatalf("%d audio bytes, ffprobe %d", ours, theirs)
	}
}

// TestTSNoStart: a player that seeks before ever reading the start of the file cannot be
// timed (the start PTS is unknown) - so nothing is emitted, rather than wrong times.
func TestTSNoStart(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	path := filepath.Join(t.TempDir(), "a.ts")
	testmedia.Tone(t, path, "-c:a", "aac")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(data))
	if rec := demux(t, "mpegts", data, span{size / 2, size}); len(rec.frames) != 0 {
		t.Fatalf("%d frames without the start of the file", len(rec.frames))
	}
}

func TestCRC32MPEG(t *testing.T) {
	// a PAT section with its CRC checks to zero
	pat := []byte{0x00, 0xB0, 0x0D, 0x00, 0x01, 0xC1, 0x00, 0x00, 0x00, 0x01, 0xF0, 0x00}
	c := crc32MPEG(pat)
	sec := append(pat, byte(c>>24), byte(c>>16), byte(c>>8), byte(c))
	if crc32MPEG(sec) != 0 {
		t.Fatal("section with its CRC does not check to zero")
	}
	sec[5] ^= 1
	if crc32MPEG(sec) == 0 {
		t.Fatal("corrupted section checks to zero")
	}
}
