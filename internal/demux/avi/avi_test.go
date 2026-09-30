package avi

import (
	"encoding/csv"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type probePkt struct {
	pts float64
	pos int64
}

func ffprobePackets(t *testing.T, path string) []probePkt {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries",
		"packet=pts_time,pos", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := csv.NewReader(strings.NewReader(string(out))).ReadAll()
	var pk []probePkt
	for _, r := range recs {
		if len(r) < 2 {
			continue
		}
		pts, e1 := strconv.ParseFloat(r[0], 64)
		pos, e2 := strconv.ParseInt(r[1], 10, 64)
		if e1 == nil && e2 == nil {
			pk = append(pk, probePkt{pts, pos})
		}
	}
	return pk
}

func makeAVI(t *testing.T, dir, name string, audioArgs ...string) string {
	out := filepath.Join(dir, name)
	args := []string{"-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=s=320x240:r=25",
		"-f", "lavfi", "-i", "sine=f=440:r=48000,aformat=channel_layouts=stereo",
		"-t", "40", "-c:v", "mpeg4", "-q:v", "10"}
	args = append(args, audioArgs...)
	args = append(args, out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("cannot build %s: %v %s", name, err, b)
	}
	return out
}

// loadIndex parses header + index the way the proxy would (from the bytes a player reads).
func loadIndex(t *testing.T, data []byte, useODML bool) (*Header, *Audio, *Index) {
	h, err := ParseHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Audio) == 0 {
		t.Fatal("no audio stream")
	}
	a := h.Audio[0]
	ix := NewIndex(a)
	if useODML && len(a.ODMLIndex) > 0 {
		for _, off := range a.ODMLIndex {
			size := int64(le.Uint32(data[off+4:]))
			ix.AddIx(data[off+8 : off+8+size])
		}
	} else if h.HasIdx1 && h.MoviEnd+8 <= int64(len(data)) && le.Uint32(data[h.MoviEnd:]) == ccIdx1 {
		size := int64(le.Uint32(data[h.MoviEnd+4:]))
		ix.AddIdx1(data[h.MoviEnd+8:h.MoviEnd+8+size], h.MoviStart)
	}
	return h, a, ix
}

type rec struct {
	t    float64
	size int
}

func extract(h *Header, a *Audio, ix *Index, data []byte, from int64) (frames []rec, discont int) {
	x := NewExtractor(h, a, ix)
	x.OnFrame = func(f Frame) { frames = append(frames, rec{f.Time.Seconds(), len(f.Data)}) }
	x.OnDiscontinuity = func(time.Duration) { discont++ }
	x.Reset(from)
	for p := from; p < int64(len(data)); p += 64 * 1024 { // proxy-like pieces
		end := min(p+64*1024, int64(len(data)))
		x.Write(data[p:end])
	}
	return
}

func TestAVITimeline(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg missing")
	}
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
	}{
		{"mp3cbr48.avi", []string{"-c:a", "libmp3lame", "-b:a", "128k"}},
		{"mp3vbr44.avi", []string{"-c:a", "libmp3lame", "-q:a", "2", "-ar", "44100"}},
		{"mp2.avi", []string{"-c:a", "mp2", "-b:a", "192k"}},
		{"ac3.avi", []string{"-c:a", "ac3", "-b:a", "448k", "-ac", "6"}},
		{"ac3_44.avi", []string{"-c:a", "ac3", "-b:a", "192k", "-ar", "44100"}},
		{"dts.avi", []string{"-c:a", "dca", "-strict", "-2", "-ac", "2"}},
		{"pcm.avi", []string{"-c:a", "pcm_s16le"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := makeAVI(t, dir, c.name, c.args...)
			data, _ := os.ReadFile(path)
			pk := ffprobePackets(t, path)
			for _, odml := range []bool{false, true} {
				h, a, ix := loadIndex(t, data, odml)
				if ix.Len() == 0 {
					t.Logf("odml=%v: no index of this kind", odml)
					continue
				}
				// 1. chunk times from the index match ffmpeg's packet pts
				isDTS := a.Codec == "dts" // ffmpeg's own AVI demuxer mis-times DTS chunks (bytes/blockAlign); the Media3 rule is right
				if ix.Len() != len(pk) && !isDTS {
					t.Errorf("odml=%v: %d index chunks vs %d ffprobe packets", odml, ix.Len(), len(pk))
				}
				maxErr := 0.0
				for i := 0; i < ix.Len() && i < len(pk); i++ {
					maxErr = math.Max(maxErr, math.Abs(a.Time(ix.Chunks[i].Time).Seconds()-pk[i].pts))
				}
				if maxErr > 0.002 && !isDTS {
					t.Errorf("odml=%v: chunk time error vs ffprobe %.4fs", odml, maxErr)
				}
				// 2. frames from a full read vs. from a mid-file "seek" agree
				full, _ := extract(h, a, ix, data, 0)
				if len(full) == 0 {
					t.Fatalf("odml=%v: no frames (codec %q)", odml, a.Codec)
				}
				from := int64(len(data)) * 37 / 100
				part, disc := extract(h, a, ix, data, from)
				byTime := map[int64]bool{}
				for _, f := range full {
					byTime[int64(math.Round(f.t*1000))] = true
				}
				miss := 0
				for _, f := range part {
					if !byTime[int64(math.Round(f.t*1000))] {
						miss++
					}
				}
				last := full[len(full)-1].t
				t.Logf("odml=%v codec=%s scale=%d rate=%d sampleSize=%d blockAlign=%d chunks=%d frames=%d last=%.3fs | seek@37%%: %d frames from %.3fs, %d discontinuities, %d mismatched",
					odml, a.Codec, a.Scale, a.Rate, a.SampleSize, a.BlockAlign, ix.Len(), len(full), last, len(part), part[0].t, disc, miss)
				if miss > 0 && !strings.HasPrefix(a.Codec, "pcm_") { // PCM pieces follow Write boundaries
					t.Errorf("odml=%v: %d frame times differ after seek", odml, miss)
				}
				if math.Abs(last-39.9) > 0.2 {
					t.Errorf("odml=%v: last frame at %.3f, expected ~40s", odml, last)
				}
			}
			// 3. no-index fallback, playback from start
			h, a, _ := loadIndex(t, data, false)
			seqIx := NewIndex(a)
			seq, _ := extract(h, a, seqIx, data, 0)
			if len(seq) == 0 {
				t.Errorf("sequential (no index) mode produced no frames")
			} else if math.Abs(seq[len(seq)-1].t-39.9) > 0.2 {
				t.Errorf("sequential mode last frame %.3f", seq[len(seq)-1].t)
			}
		})
	}
}
