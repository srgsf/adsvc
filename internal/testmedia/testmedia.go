// Package testmedia generates the media files tests run on, with the ffmpeg on PATH, and
// locates the ffmpeg the decode path is tested with. It is imported by tests only.
package testmedia

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/ffmpeg"
	"github.com/srgsf/adsvc/internal/fingerprint"
)

// Decoder is the ffmpeg of the decode path, what the proxy runs. ADSVC_TEST_FFMPEG points
// it at another binary, e.g. the minimal build (make test-minimal); media generation,
// ffprobe oracles and enrollment keep using the ffmpeg on PATH.
func Decoder() ffmpeg.Tools {
	return ffmpeg.Tools{FFmpegPath: os.Getenv("ADSVC_TEST_FFMPEG")}
}

// PCM decodes [from, from+dur) of the audio of path with the ffmpeg on PATH, as the
// fingerprinter wants it (dur <= 0: to the end).
func PCM(t testing.TB, path string, from, dur time.Duration) []float32 {
	t.Helper()
	s, err := ffmpeg.Tools{}.DecodePCM(t.Context(), path, from, dur)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(s)
	if err := errors.Join(err, s.Wait()); err != nil {
		t.Fatal(err)
	}
	return fingerprint.FromS16LE(raw)
}

// NeedFFmpeg skips the test when ffmpeg is not installed.
func NeedFFmpeg(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

// Run runs the ffmpeg on PATH.
func Run(args ...string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return err
	}
	return exec.Command("ffmpeg", args...).Run()
}

// Tone encodes a 20 s test file: tone + video, with the given codec/format args.
func Tone(t testing.TB, out string, args ...string) {
	t.Helper()
	NeedFFmpeg(t) // a missing ffmpeg skips; one that cannot build the file fails
	full := []string{"-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=s=160x120:r=25",
		"-f", "lavfi", "-i", "sine=f=440:r=48000,aformat=channel_layouts=stereo",
		"-t", "20", "-c:v", "mpeg4", "-q:v", "10"}
	full = append(full, args...)
	full = append(full, out)
	if b, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		t.Fatalf("cannot build %s: %v %s", out, err, b)
	}
}

// adSource is the audio of the test ad: a stepped melody over a stepped high tone.
const adSource = "aevalsrc='0.3*sin(2*PI*(330*pow(2,floor(mod(t*t*1.7,7))/6))*t)+0.2*sin(2*PI*(1800-150*floor(mod(t*t*0.9+3*t,6)))*t)':s=48000"

// content is programme audio: two gliding tones plus noise, different for each seed.
func content(seed int) string {
	s := strconv.Itoa(seed)
	return "aevalsrc='0.25*sin(2*PI*(300+40*" + s + "+150*sin(2*PI*0.13*t))*t)+0.15*sin(2*PI*(700+50*" + s + "+80*sin(2*PI*0.31*t))*t)+0.05*(random(0)-0.5)':s=48000"
}

// ftoa writes d as seconds for ffmpeg.
func ftoa(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) }

// WithAd builds content + ad + content in any container ffmpeg can write: MPEG-4 video
// and stereo audio encoded with codecArgs. The ad lasts adDur from pre.
func WithAd(t testing.TB, out string, pre, adDur, post time.Duration, codecArgs ...string) {
	t.Helper()
	NeedFFmpeg(t) // a missing ffmpeg skips; one that cannot build the file fails
	args := []string{"-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-t", ftoa(pre), "-i", content(1),
		"-f", "lavfi", "-t", ftoa(adDur), "-i", adSource,
		"-f", "lavfi", "-t", ftoa(post), "-i", content(8),
		"-f", "lavfi", "-i", "testsrc=s=160x120:r=25",
		"-filter_complex", "[0][1][2]concat=n=3:v=0:a=1,aformat=channel_layouts=stereo[a]",
		"-map", "3:v", "-map", "[a]", "-t", ftoa(pre + adDur + post), "-c:v", "mpeg4", "-q:v", "10"}
	args = append(args, codecArgs...)
	if b, err := exec.Command("ffmpeg", append(args, out)...).CombinedOutput(); err != nil {
		t.Fatalf("cannot build %s: %v: %s", out, err, b)
	}
}

// AVIWithAd builds content + ad + content as MPEG-4 video + MP2 audio in an AVI.
func AVIWithAd(t testing.TB, out string, pre, adDur, post time.Duration) {
	t.Helper()
	NeedFFmpeg(t) // a missing ffmpeg skips; one that cannot build the file fails
	cmd := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-t", ftoa(pre), "-i", content(1),
		"-f", "lavfi", "-t", ftoa(adDur), "-i", adSource,
		"-f", "lavfi", "-t", ftoa(post), "-i", content(8),
		"-f", "lavfi", "-i", "testsrc=s=160x120:r=25",
		"-filter_complex", "[0][1][2]concat=n=3:v=0:a=1[a]",
		"-map", "3:v", "-map", "[a]", "-t", ftoa(pre+adDur+post),
		"-c:v", "mpeg4", "-q:v", "10", "-c:a", "mp2", "-b:a", "128k", "-ac", "2", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cannot build test AVI: %v: %s", err, b)
	}
}

// MKVAd is how long the ad of MKVWithAd lasts.
const MKVAd = 15 * time.Second

// MKVWithAd builds content(pre) + an MKVAd ad + content(post) as AVC + audioCodec MKV. The
// seed varies the programme audio between files.
func MKVWithAd(t testing.TB, out, audioCodec string, pre, post time.Duration, seed int) {
	t.Helper()
	NeedFFmpeg(t) // a missing ffmpeg skips; one that cannot build the file fails
	cmd := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-t", ftoa(pre), "-i", content(seed),
		"-f", "lavfi", "-t", ftoa(MKVAd), "-i", adSource,
		"-f", "lavfi", "-t", ftoa(post), "-i", content(seed+7),
		"-f", "lavfi", "-i", "testsrc=s=320x180:r=25",
		"-filter_complex", "[0][1][2]concat=n=3:v=0:a=1[a]",
		"-map", "3:v", "-map", "[a]", "-t", ftoa(pre+MKVAd+post),
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-c:a", audioCodec, out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, b)
	}
}

// TopBoxes lists the top-level boxes of an MP4 file: type -> offset (first occurrence).
func TopBoxes(data []byte) map[string]int64 {
	out := map[string]int64{}
	for off := int64(0); off+8 <= int64(len(data)); {
		size := int64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		if size == 1 {
			size = int64(binary.BigEndian.Uint64(data[off+8:]))
		}
		if _, ok := out[typ]; !ok {
			out[typ] = off
		}
		if size < 8 {
			break
		}
		off += size
	}
	return out
}
