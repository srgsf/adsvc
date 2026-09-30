package bench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/store"
	"github.com/srgsf/adsvc/internal/testmedia"
)

func TestLoadBenchSet(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr string
	}{
		{"ok", `{"data_dir":"data","files":[{"paths":["a.mkv","/abs/b.avi"],"ads":[{"ad":"01000000-0000-0000-0000-000000000000","startMs":10000,"endMs":40000}]}]}`, ""},
		{"no paths", `{"files":[{"paths":[],"ads":[]}]}`, "no paths"},
		{"backwards", `{"files":[{"paths":["a.mkv"],"ads":[{"ad":"01000000-0000-0000-0000-000000000000","startMs":40000,"endMs":10000}]}]}`, "ends before it starts"},
		{"numeric ad id", `{"files":[{"paths":["a.mkv"],"ads":[{"ad":1,"startMs":10000,"endMs":40000}]}]}`, "cannot unmarshal"},
		{"bad json", `{"files":`, "unexpected end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "labels.json")
			if err := os.WriteFile(p, []byte(tt.json), 0o644); err != nil {
				t.Fatal(err)
			}
			set, err := Load(p)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// relative paths are relative to the labels file, absolute ones are kept
			if got, want := set.DataDir, filepath.Join(dir, "data"); got != want {
				t.Errorf("data_dir = %q, want %q", got, want)
			}
			if got := set.Files[0].Paths; got[0] != filepath.Join(dir, "a.mkv") || got[1] != "/abs/b.avi" {
				t.Errorf("paths = %q", got)
			}
		})
	}
}

func TestEvaluate(t *testing.T) {
	const s = time.Second
	label := Occurrence{Ad: fingerprint.ID{1}, Start: 100 * s, End: 130 * s}
	det := func(ad byte, start time.Duration, score int) finalDet {
		return finalDet{AdID: fingerprint.ID{ad}, Start: start, End: start + 30*s, Score: score, detectAt: start + 5*s, confirmAt: never}
	}
	const ms = time.Millisecond
	tests := []struct {
		name      string
		labels    []Occurrence
		dets      []finalDet
		wantFound []bool
		wantFP    int
	}{
		{"exact", []Occurrence{label}, []finalDet{det(1, 100020*ms, 50)}, []bool{true}, 0},
		{"shifted but overlapping most", []Occurrence{label}, []finalDet{det(1, 110*s, 50)}, []bool{true}, 0},
		{"misaligned is a miss and a false positive", []Occurrence{label}, []finalDet{det(1, 120*s, 50)}, []bool{false}, 1},
		{"wrong ad", []Occurrence{label}, []finalDet{det(2, 100*s, 50)}, []bool{false}, 1},
		{"nothing found", []Occurrence{label}, nil, []bool{false}, 0},
		{"unlabelled detection", nil, []finalDet{det(1, 500*s, 50)}, nil, 1},
		{"best of two takes the label", []Occurrence{label}, []finalDet{det(1, 108*s, 30), det(1, 100*s, 60)}, []bool{true}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits, fps := evaluate(tt.labels, tt.dets)
			for i, h := range hits {
				if h.Found != tt.wantFound[i] {
					t.Errorf("label %d found = %v, want %v", i, h.Found, tt.wantFound[i])
				}
			}
			if len(fps) != tt.wantFP {
				t.Errorf("false positives = %v, want %d", fps, tt.wantFP)
			}
		})
	}

	hits, _ := evaluate([]Occurrence{label}, []finalDet{det(1, 100250*ms, 60)})
	h := hits[0]
	if h.StartErr != 250*ms || h.DetectAt == nil || *h.DetectAt != 5250*ms || h.ConfirmAt != nil {
		t.Fatalf("hit = %+v", h)
	}
}

func TestDegradeScale(t *testing.T) {
	d, ok := DegradeByName("pal")
	if !ok {
		t.Fatal("no pal degrade")
	}
	got := d.scale([]Occurrence{{Start: 104270 * time.Millisecond, End: 134270 * time.Millisecond}})[0]
	if back := time.Duration(float64(got.Start) * d.Speed); (back-104270*time.Millisecond).Abs() > time.Microsecond ||
		(time.Duration(float64(got.End)*d.Speed)-134270*time.Millisecond).Abs() > time.Microsecond {
		t.Fatalf("scaled = %+v", got)
	}
	if none, _ := DegradeByName("none"); len(none.scale([]Occurrence{{Start: 1}})) != 1 {
		t.Fatal("none must keep labels")
	}
}

// TestBench runs the benchmark end to end on a generated file with one ad: offline, offline
// degraded, and through the proxy (with the decoder the proxy would use).
func TestBench(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	const pre, adDur, post = 20 * time.Second, 15 * time.Second, 25 * time.Second
	dir := t.TempDir()
	path := filepath.Join(dir, "episode.mkv")
	testmedia.WithAd(t, path, pre, adDur, post, "-c:a", "ac3")
	st, err := store.Open(t.Context(), filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib := st.Lib
	pcm := testmedia.PCM(t, path, pre, adDur)
	ad, err := lib.Add(t.Context(), "ad", "", pcm, nil)
	if err != nil {
		t.Fatal(err)
	}
	labels := []Occurrence{{Ad: ad.ID, Start: pre, End: pre + adDur, Source: SourceManual, Verified: true}}

	check := func(t *testing.T, r Run, tol time.Duration) {
		t.Helper()
		if r.Err != "" {
			t.Fatal(r.Err)
		}
		if len(r.Hits) != 1 || !r.Hits[0].Found || len(r.FalsePos) != 0 {
			t.Fatalf("hits %+v, false positives %+v", r.Hits, r.FalsePos)
		}
		h := r.Hits[0]
		if !h.Det.Confirmed || h.StartErr.Abs() > tol {
			t.Fatalf("hit %+v", h)
		}
		if r.Analysed < pre+adDur+post-time.Second {
			t.Fatalf("analysed %v of %v", r.Analysed, pre+adDur+post)
		}
	}

	t.Run("offline", func(t *testing.T) {
		r := RunOffline(context.Background(), lib, path, labels, Options{})
		check(t, r, 50*time.Millisecond)
		h := r.Hits[0]
		if h.DetectAt == nil || h.ConfirmAt == nil || *h.DetectAt <= 0 || *h.ConfirmAt < *h.DetectAt {
			t.Fatalf("look-ahead not measured: %+v", h)
		}
		s := Summarize([]Run{r})
		if s.Recall != 1 || s.StartErrN != 1 || s.ConfirmN != 1 || s.WeakestTrue == 0 {
			t.Fatalf("summary %+v", s)
		}
	})
	t.Run("offline-aac48", func(t *testing.T) {
		d, _ := DegradeByName("aac48")
		check(t, RunOffline(context.Background(), lib, path, labels, Options{Degrade: d}), 100*time.Millisecond)
	})
	t.Run("proxy", func(t *testing.T) {
		check(t, RunProxy(context.Background(), runStore(t, st), "bench-0", path, labels, Options{FFmpeg: testmedia.Decoder()}), 50*time.Millisecond)
	})
}

// runStore is the store of a benchmark's proxy runs: a copy of st's ads.
func runStore(t *testing.T, st *store.Store) *store.Store {
	t.Helper()
	cp, err := store.Copy(t.Context(), st.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cp.Close() })
	return cp
}
