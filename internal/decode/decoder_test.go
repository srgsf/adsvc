package decode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/demux"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/store"
	"github.com/srgsf/adsvc/internal/testmedia"
)

// TestDecodeFormats runs every kind of audio the container parsers hand to ffmpeg (raw
// self-framed, raw PCM, Matroska rewrap) through the decoder and checks the whole 20 s
// tone comes out. With ADSVC_TEST_FFMPEG set it is the contract of the minimal ffmpeg
// build (scripts/build-ffmpeg.sh): a component missing there fails here.
func TestDecodeFormats(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	tests := []struct {
		name   string
		ext    string
		args   []string
		rewrap bool
	}{
		{"avi-mp3", ".avi", []string{"-c:a", "libmp3lame"}, false},
		{"avi-mp2", ".avi", []string{"-c:a", "mp2"}, false},
		{"avi-ac3", ".avi", []string{"-c:a", "ac3"}, false},
		{"avi-dts", ".avi", []string{"-c:a", "dca", "-strict", "-2"}, false},
		{"avi-pcm_s16le", ".avi", []string{"-c:a", "pcm_s16le"}, false},
		{"avi-pcm_u8", ".avi", []string{"-c:a", "pcm_u8"}, false},
		{"avi-pcm_s24le", ".avi", []string{"-c:a", "pcm_s24le"}, false},
		{"avi-pcm_f32le", ".avi", []string{"-c:a", "pcm_f32le"}, false},

		{"mkv-aac", ".mkv", []string{"-c:a", "aac"}, true},
		{"mkv-opus", ".mkv", []string{"-c:a", "libopus"}, true},
		{"mkv-vorbis", ".mkv", []string{"-c:a", "vorbis", "-strict", "-2"}, true},
		{"mkv-flac", ".mkv", []string{"-c:a", "flac"}, true},
		{"mkv-mp3", ".mkv", []string{"-c:a", "libmp3lame"}, false},
		{"mkv-mp2", ".mkv", []string{"-c:a", "mp2"}, false},
		{"mkv-ac3", ".mkv", []string{"-c:a", "ac3"}, false},
		{"mkv-eac3", ".mkv", []string{"-c:a", "eac3"}, false},
		{"mkv-dts", ".mkv", []string{"-c:a", "dca", "-strict", "-2"}, false},
		{"mkv-pcm_s16le", ".mkv", []string{"-c:a", "pcm_s16le"}, false},
		{"mkv-pcm_s24le", ".mkv", []string{"-c:a", "pcm_s24le"}, false},
		{"mkv-pcm_s16be", ".mkv", []string{"-c:a", "pcm_s16be"}, false},
		{"mkv-pcm_f32le", ".mkv", []string{"-c:a", "pcm_f32le"}, false},

		{"mp4-aac", ".mp4", []string{"-c:a", "aac"}, true},
		{"mp4-opus", ".mp4", []string{"-c:a", "libopus"}, true},
		{"mp4-flac", ".mp4", []string{"-c:a", "flac", "-strict", "-2"}, true},
		{"m4a-alac", ".m4a", []string{"-vn", "-c:a", "alac"}, true},
		{"mp4-mp3", ".mp4", []string{"-c:a", "libmp3lame"}, false},
		{"mp4-ac3", ".mp4", []string{"-c:a", "ac3"}, false},
		{"mp4-eac3", ".mp4", []string{"-c:a", "eac3"}, false},
		{"mov-sowt", ".mov", []string{"-c:a", "pcm_s16le"}, false},
		{"mov-twos", ".mov", []string{"-c:a", "pcm_s16be"}, false},

		{"ts-aac", ".ts", []string{"-c:a", "aac"}, false},
		{"ts-latm", ".ts", []string{"-c:a", "aac", "-mpegts_flags", "latm"}, false},
		{"ts-mp2", ".ts", []string{"-c:a", "mp2"}, false},
		{"ts-mp3", ".ts", []string{"-c:a", "libmp3lame"}, false},
		{"ts-ac3", ".ts", []string{"-c:a", "ac3"}, false},
		{"ts-eac3", ".ts", []string{"-c:a", "eac3"}, false},
		{"ts-dts", ".ts", []string{"-c:a", "dca", "-strict", "-2"}, false},
	}
	dir := t.TempDir()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(dir, tt.name+tt.ext)
			testmedia.Tone(t, path, tt.args...)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			typ := demux.Sniff(data)
			var ranges []span
			if moov, ok := testmedia.TopBoxes(data)["moov"]; typ == "mp4" && ok && moov > 64<<10 {
				// a player reads a trailing moov before it plays anything
				ranges = []span{{0, 64 << 10}, {moov, int64(len(data))}, {0, int64(len(data))}}
			}
			rec := demuxRanges(t, typ, data, ranges...)
			if len(rec.frames) == 0 {
				t.Fatalf("no frames (format %+v)", rec.format)
			}
			if (rec.format.Matroska != nil) != tt.rewrap {
				t.Fatalf("format %s, want rewrap=%v", formatName(rec.format), tt.rewrap)
			}
			st, err := store.Open(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			lib := st.Lib
			ring := NewPCMRing(time.Minute)
			dec := New(t.Context(), Options{FFmpeg: testmedia.Decoder(), Lib: lib, MinScore: 20, Format: rec.format,
				OnBlock: func(detect.BlockResult) {}, Tap: ring})
			t0 := rec.frames[0].Time
			if err := dec.Start(t0); err != nil {
				t.Fatal(err)
			}
			for _, f := range rec.frames {
				if err := dec.WriteFrame(f.Time, f.Data); err != nil {
					t.Fatalf("%s: %v", formatName(rec.format), err)
				}
			}
			dec.Close()
			from, to := ring.Span()
			if from != t0 || (to-20*time.Second).Abs() > 150*time.Millisecond {
				t.Fatalf("%s: decoded %v..%v, want %v..20s", formatName(rec.format), from, to, t0)
			}
		})
	}
}

// recSink records what a container produces.
type recSink struct {
	format demux.AudioFormat
	frames []demux.Frame
}

func (r *recSink) Configure(f demux.AudioFormat) error { r.format = f; return nil }

func (r *recSink) Frame(f demux.Frame) error {
	f.Data = append([]byte(nil), f.Data...)
	r.frames = append(r.frames, f)
	return nil
}

func (r *recSink) Close() {}

// span is a byte range [from, to) of a file, as a player would request it.
type span struct{ from, to int64 }

// demuxRanges runs a container over the given ranges of data (all of it by default), fed
// in proxy-sized pieces.
func demuxRanges(t *testing.T, typ string, data []byte, ranges ...span) *recSink {
	t.Helper()
	sink := &recSink{}
	c, err := demux.New(typ, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) == 0 {
		ranges = []span{{0, int64(len(data))}}
	}
	for _, r := range ranges {
		c.Range(r.from)
		for p := r.from; p < r.to; p += 64 << 10 {
			if err := c.Write(data[p:min(p+64<<10, r.to)]); err != nil {
				t.Fatalf("write at %d: %v", p, err)
			}
		}
	}
	c.Close()
	return sink
}

func formatName(f demux.AudioFormat) string {
	if f.Matroska != nil {
		return "matroska " + f.Matroska.CodecID
	}
	return "raw " + f.Args[len(f.Args)-1]
}

// A run whose context ends is killed: it does not wait for input that will never come.
func TestDecoderRunEndsWithContext(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	st, err := store.Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(t.Context())
	dec := New(ctx, Options{FFmpeg: testmedia.Decoder(), Lib: st.Lib, MinScore: 20,
		Format: demux.AudioFormat{Args: []string{"-f", "s16le", "-ar", "8000", "-ac", "1"}}, OnBlock: func(detect.BlockResult) {}})
	if err := dec.Start(0); err != nil {
		t.Fatal(err)
	}
	done := dec.cur.done
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the ffmpeg run outlived its context")
	}
	if err := dec.Start(time.Second); err == nil {
		t.Fatal("a run started after the context ended")
	}
}
