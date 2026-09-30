package demux

import (
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/testmedia"
)

// nopSink accepts everything; the fuzz targets only look for panics and hangs.
type nopSink struct{}

func (nopSink) Configure(AudioFormat) error { return nil }
func (nopSink) Frame(Frame) error           { return nil }
func (nopSink) Close()                      {}

// fuzzContainer feeds data to a container in chunks of varying size, with a seek (a new
// range) wherever a chunk-size byte says so.
func fuzzContainer(t *testing.T, typ string, data []byte, split []byte) {
	c, err := New(typ, nopSink{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck // only panics and stalls matter here
	c.Range(0)
	pos := 0
	for i := 0; pos < len(data); i++ {
		n := 1 + 997*(i+1)%4096
		if len(split) > 0 {
			b := split[i%len(split)]
			n = 1 + int(b)*37
			if b%11 == 0 { // seek
				pos = (pos + int(b)*131) % len(data)
				c.Range(int64(pos))
			}
		}
		end := min(pos+n, len(data))
		if c.Write(data[pos:end]) != nil {
			return
		}
		pos = end
	}
}

// seedFiles generates small real files for the fuzz corpus (skipped without ffmpeg).
func seedFiles(f *testing.F, ext string, args ...[]string) {
	dir := f.TempDir()
	for i, a := range args {
		path := filepath.Join(dir, "seed"+strconv.Itoa(i)+ext)
		full := append([]string{"-nostdin", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc=s=64x48:r=10", "-f", "lavfi", "-i", "sine=r=48000",
			"-t", "2", "-c:v", "mpeg4"}, a...)
		if err := testmedia.Run(append(full, path)...); err != nil {
			continue
		}
		if data, err := os.ReadFile(path); err == nil {
			f.Add(data, []byte{3, 200, 17, 22, 90})
		}
	}
}

func FuzzMKV(f *testing.F) {
	seedFiles(f, ".mkv", []string{"-c:a", "aac"}, []string{"-c:a", "ac3"})
	f.Add(writeLacedMKV([]Frame{{Time: 0, Data: []byte{0x0B, 0x77, 1, 2}}, {Time: 32 * time.Millisecond, Data: []byte{0x0B, 0x77, 3}}}, 2, 3, false, []byte{0x0B, 0x77}), []byte{1})
	f.Fuzz(func(t *testing.T, data, split []byte) { fuzzContainer(t, "matroska", data, split) })
}

func FuzzMP4(f *testing.F) {
	seedFiles(f, ".mp4", []string{"-c:a", "aac", "-movflags", "+faststart"},
		[]string{"-c:a", "aac", "-movflags", "frag_keyframe+empty_moov"})
	f.Fuzz(func(t *testing.T, data, split []byte) { fuzzContainer(t, "mp4", data, split) })
}

func FuzzTS(f *testing.F) {
	seedFiles(f, ".ts", []string{"-c:a", "aac"}, []string{"-c:a", "ac3", "-mpegts_m2ts_mode", "1"})
	f.Fuzz(func(t *testing.T, data, split []byte) { fuzzContainer(t, "mpegts", data, split) })
}

func FuzzAVI(f *testing.F) {
	seedFiles(f, ".avi", []string{"-c:a", "mp2"}, []string{"-c:a", "ac3"})
	f.Fuzz(func(t *testing.T, data, split []byte) { fuzzContainer(t, "avi", data, split) })
}

// TestContainersMutated runs every demuxer over randomly corrupted real files, chunked and
// seeked at random, with a watchdog: no input may panic or stall a demuxer. (Deterministic,
// so it runs in every `go test`; the Fuzz targets explore further with -fuzz.)
func TestContainersMutated(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	cases := []struct {
		name, ext, typ string
		args           []string
	}{
		{"avi-mp2", ".avi", "avi", []string{"-c:a", "mp2"}},
		{"avi-ac3", ".avi", "avi", []string{"-c:a", "ac3"}},
		{"mkv-aac", ".mkv", "matroska", []string{"-c:a", "aac"}},
		{"mkv-ac3", ".mkv", "matroska", []string{"-c:a", "ac3"}},
		{"mp4-faststart", ".mp4", "mp4", []string{"-c:a", "aac", "-movflags", "+faststart"}},
		{"mp4-fragmented", ".mp4", "mp4", []string{"-c:a", "aac", "-movflags", "frag_keyframe+empty_moov"}},
		{"ts-aac", ".ts", "mpegts", []string{"-c:a", "aac"}},
		{"m2ts-ac3", ".m2ts", "mpegts", []string{"-c:a", "ac3", "-mpegts_m2ts_mode", "1"}},
	}
	dir := t.TempDir()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+c.ext)
			args := append([]string{"-nostdin", "-loglevel", "error", "-y",
				"-f", "lavfi", "-i", "testsrc=s=64x48:r=10", "-f", "lavfi", "-i", "sine=r=48000",
				"-t", "2", "-c:v", "mpeg4"}, c.args...)
			if err := testmedia.Run(append(args, path)...); err != nil {
				t.Skip(err)
			}
			seed, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewSource(int64(len(c.name))))
			for iter := range 1500 {
				data := append([]byte(nil), seed...)
				for m := 0; m < 1+rng.Intn(8); m++ {
					data[rng.Intn(len(data))] = [3]byte{byte(rng.Intn(256)), 0xFF, 0}[rng.Intn(3)]
				}
				split := []byte{byte(rng.Intn(256)), byte(rng.Intn(256)), byte(rng.Intn(256))}
				done := make(chan any, 1)
				go func() {
					defer func() { done <- recover() }()
					fuzzContainer(t, c.typ, data, split)
				}()
				select {
				case r := <-done:
					if r != nil {
						t.Fatalf("input %d: panic: %v", iter, r)
					}
				case <-time.After(5 * time.Second):
					buf := make([]byte, 1<<16)
					t.Fatalf("input %d: demuxer stalled\n%s", iter, buf[:runtime.Stack(buf, true)])
				}
			}
		})
	}
}
