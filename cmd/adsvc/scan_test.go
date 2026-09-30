package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/ffmpeg"
	"github.com/srgsf/adsvc/internal/store"
	"github.com/srgsf/adsvc/internal/testmedia"
)

// scan demuxes an AVI itself (ffmpeg only decodes its audio frames), from a file or a URL,
// and a file it does not know goes to ffmpeg whole.
func TestScan(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	dir := t.TempDir()
	avi := filepath.Join(dir, "clip.avi")
	const pre, adDur, post = 10 * time.Second, 12 * time.Second, 8 * time.Second
	testmedia.AVIWithAd(t, avi, pre, adDur, post)
	wav := filepath.Join(dir, "clip.wav") // no demuxer of adsvc's own
	if err := testmedia.Run("-i", avi, "-vn", "-c:a", "pcm_s16le", wav); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Enrollment reads the AVI whole: the full ffmpeg on PATH, not the decode path's.
	pcm := testmedia.PCM(t, avi, pre, adDur)
	if _, err := st.Lib.Add(t.Context(), "test ad", "", pcm, nil); err != nil {
		t.Fatal(err)
	}
	src := httptest.NewServer(http.FileServerFS(os.DirFS(dir)))
	defer src.Close()

	want, _, err := fileContentKey(avi)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, in string
		demuxed  bool
	}{
		{"file", avi, true},
		{"url", src.URL + "/clip.avi", true},
		{"ffmpeg", wav, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := &scanner{confirmScore: 40}
			err := sc.demuxed(t.Context(), testmedia.Decoder(), st.Lib, 20, c.in)
			if !c.demuxed {
				if err == nil {
					t.Fatal("a WAV must not be demuxed by adsvc")
				}
				// a file adsvc does not demux needs a full ffmpeg (the minimal one has no WAV)
				if err = sc.whole(t.Context(), ffmpeg.Tools{}, st.Lib, 20, c.in); err == nil {
					sc.key, sc.size, err = fileContentKey(c.in)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(sc.ads) != 1 || (sc.ads[0].Start-pre).Abs() > 100*time.Millisecond || (sc.ads[0].End-(pre+adDur)).Abs() > 100*time.Millisecond {
				t.Fatalf("found %+v, want one ad at %v-%v", sc.ads, pre, pre+adDur)
			}
			if sc.reached < pre+adDur+post-3*time.Second {
				t.Errorf("scanned up to %v of %v", sc.reached, pre+adDur+post)
			}
			if c.demuxed && sc.key != want {
				t.Errorf("content key %q, want %q (what the proxy computes from the same bytes)", sc.key, want)
			}
			if err := sc.save(t.Context(), st.Maps); err != nil {
				t.Fatal(err)
			}
			m, err := st.Maps.Lookup(t.Context(), sc.key)
			if err != nil || m == nil {
				t.Fatalf("no ad map stored: %v", err)
			}
			if len(m.Ads) != 1 || !m.Ads[0].Confirmed || !m.Analyzed.Contains(0, pre+adDur+post-3*time.Second) {
				t.Errorf("stored map: ads %+v, analysed %v", m.Ads, m.Analyzed)
			}
		})
	}

	// A second scan merges with the stored map instead of losing what it had.
	sc := &scanner{confirmScore: 40, key: want}
	sc.covered = sc.covered.Add(0, 5*time.Second)
	if err := sc.save(t.Context(), st.Maps); err != nil {
		t.Fatal(err)
	}
	if m, err := st.Maps.Lookup(t.Context(), want); err != nil || m == nil || len(m.Ads) != 1 || !m.Ads[0].Confirmed {
		t.Errorf("after a partial scan: %+v, %v", m, err)
	}
}
