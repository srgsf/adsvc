package demux

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/srgsf/adsvc/internal/testmedia"
)

func TestMP4Timeline(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	tests := []struct {
		name    string
		ext     string
		args    []string
		chunked bool // PCM: frames are whole chunks, ffmpeg splits them differently
	}{
		{"aac-moov-at-end", ".mp4", []string{"-c:a", "aac", "-b:a", "96k"}, false},
		{"aac-faststart", ".mp4", []string{"-c:a", "aac", "-movflags", "+faststart"}, false},
		{"ac3", ".mp4", []string{"-c:a", "ac3"}, false},
		{"eac3", ".mp4", []string{"-c:a", "eac3"}, false},
		{"mp3", ".mp4", []string{"-c:a", "libmp3lame"}, false},
		{"opus", ".mp4", []string{"-c:a", "libopus"}, false},
		{"flac", ".mp4", []string{"-c:a", "flac", "-strict", "-2"}, false},
		{"alac", ".m4a", []string{"-vn", "-c:a", "alac"}, false},
		{"mov-aac", ".mov", []string{"-c:a", "aac"}, false},
		{"mov-pcm", ".mov", []string{"-c:a", "pcm_s16le"}, true},
		{"fragmented", ".mp4", []string{"-c:a", "aac", "-movflags", "frag_keyframe+empty_moov"}, false},
		{"fragmented-base-moof", ".mp4", []string{"-c:a", "aac", "-movflags", "frag_keyframe+empty_moov+default_base_moof"}, false},
		{"cmaf", ".mp4", []string{"-c:a", "aac", "-movflags", "cmaf+frag_keyframe+empty_moov+separate_moof"}, false},
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
			size := int64(len(data))
			// a player reads a trailing moov before it plays anything
			moov, hasMoov := testmedia.TopBoxes(data)["moov"]
			var index []span
			if hasMoov && moov > 64<<10 {
				index = []span{{moov, size}}
			}
			reference := []span{{0, size}}
			if index != nil { // header (locates moov), moov, then everything
				reference = append(append([]span{{0, 64 << 10}}, index...), span{0, size})
			}
			full := demux(t, "mp4", data, reference...)
			if full.configured != 1 {
				t.Fatalf("configured %d times", full.configured)
			}
			if tt.chunked {
				checkBytesAndTimes(t, full.frames, probeAudio(t, path), 48000*4)
			} else {
				checkAgainstProbe(t, full.frames, probeAudio(t, path), 0.001)
			}

			// a player: the start of the file, moov wherever it is, then a seek to 37%
			ranges := append([]span{{0, 64 << 10}}, index...)
			ranges = append(ranges, span{size * 37 / 100, size})
			part := demux(t, "mp4", data, ranges...)
			var after []Frame
			for _, f := range part.frames {
				if f.Time.Seconds() > 5 {
					after = append(after, f)
				}
			}
			checkSeekSuffix(t, full.frames, after)
			t.Logf("%d frames, %s; after the seek from %.3fs", len(full.frames), formatName(full.format), after[0].Time.Seconds())
		})
	}
}

// TestMP4MoovNotRead: a player that seeks without having read a trailing moov gets no
// analysis for that range - and nothing wrong either.
func TestMP4MoovNotRead(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	path := filepath.Join(t.TempDir(), "a.mp4")
	testmedia.Tone(t, path, "-c:a", "aac")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if testmedia.TopBoxes(data)["moov"] < 64<<10 {
		t.Skip("moov is at the start")
	}
	size := int64(len(data))
	rec := demux(t, "mp4", data, span{0, 64 << 10}, span{size / 3, size * 2 / 3})
	if len(rec.frames) != 0 || rec.configured != 0 {
		t.Fatalf("got %d frames without a moov", len(rec.frames))
	}
}

func TestOpusHead(t *testing.T) {
	// dOps: version 0, 2 channels, pre-skip 312, 48000 Hz, gain 0, family 0
	dops := []byte{0, 2, 0x01, 0x38, 0, 0, 0xBB, 0x80, 0, 0, 0}
	got, err := opusHead(dops)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xBB, 0, 0, 0, 0, 0)
	if string(got) != string(want) {
		t.Fatalf("opusHead = %x, want %x", got, want)
	}
	if _, err := opusHead(dops[:5]); err == nil {
		t.Fatal("short dOps accepted")
	}
}

// checkBytesAndTimes compares a constant-rate stream packetised differently from ffmpeg:
// same bytes in total, and every frame starts at the time of its first byte.
func checkBytesAndTimes(t *testing.T, frames []Frame, pk []probePacket, bytesPerSec float64) {
	t.Helper()
	var ours, theirs int
	for _, p := range pk {
		theirs += p.size
	}
	for _, f := range frames {
		if want := pk[0].pts + float64(ours)/bytesPerSec; math.Abs(f.Time.Seconds()-want) > 1e-6 {
			t.Fatalf("frame at byte %d: %.6fs, want %.6fs", ours, f.Time.Seconds(), want)
		}
		ours += len(f.Data)
	}
	if ours != theirs {
		t.Fatalf("%d bytes, ffprobe %d", ours, theirs)
	}
}
