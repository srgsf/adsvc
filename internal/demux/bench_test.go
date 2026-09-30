package demux

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srgsf/adsvc/internal/testmedia"
)

// BenchmarkContainers demuxes a 20 s file per container, fed in the proxy's 128 KiB
// chunks: the per-byte and per-frame cost on the streaming path.
func BenchmarkContainers(b *testing.B) {
	testmedia.NeedFFmpeg(b)
	for _, c := range []struct {
		name, ext string
		args      []string
	}{
		{"mkv-aac", "mkv", []string{"-c:a", "aac", "-b:a", "128k"}},
		{"ts-ac3", "ts", []string{"-c:a", "ac3", "-b:a", "192k"}},
		{"mp4-aac", "mp4", []string{"-c:a", "aac", "-b:a", "128k"}},
	} {
		b.Run(c.name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "in."+c.ext)
			testmedia.Tone(b, path, c.args...)
			data, err := os.ReadFile(path)
			if err != nil {
				b.Fatal(err)
			}
			typ := Sniff(data)
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				ct, err := New(typ, nopSink{})
				if err != nil {
					b.Fatal(err)
				}
				ct.Range(0)
				for p := data; len(p) > 0; {
					n := min(len(p), 128<<10)
					if err := ct.Write(p[:n]); err != nil {
						b.Fatal(err)
					}
					p = p[n:]
				}
				ct.Close()
			}
		})
	}
}
