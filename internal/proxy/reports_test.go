package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/admap"
	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/testmedia"
)

// TestReportThenMark is the reporting workflow: a user reports an ad seen on a TV, then
// plays the file again (in mpv) and marks the ad, which clears the report. The file was
// analysed completely before: only its open report gets its audio decoded again.
func TestReportThenMark(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.avi")
	const pre, adDur, post = 10 * time.Second, 12 * time.Second, 8 * time.Second
	testmedia.AVIWithAd(t, path, pre, adDur, post)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path)
	}))
	defer src.Close()

	cfg := Config{FFmpeg: testmedia.Decoder(), DataDir: filepath.Join(dir, "data"),
		Users: []User{{Name: "alice", Tokens: []string{"ta"}}, {Name: "bob", Tokens: []string{"tb"}}}}
	do := func(front, method, rawURL, token string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, front+rawURL, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	sel := "u=" + urlEscape(src.URL+"/clip.avi") + "&id=ep1"

	// first playback, on the TV: the whole file is analysed (an unrelated ad is known, so
	// that the file gets a map)
	px, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	other := testmedia.PCM(t, path, 0, 5*time.Second)
	if _, err := px.Lib.Add(t.Context(), "intro", "", other, nil); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(px.Handler())
	if code, _ := do(front.URL, http.MethodGet, "/s?"+sel, "ta"); code != http.StatusOK {
		t.Fatalf("stream: %d", code)
	}
	waitFor(t, func() bool {
		rep := fetchReportAuth(t, front.URL+"/ads/file?"+sel, "ta")
		return rep.Analyzed.Contains(0, 20*time.Second) // the ad start included; the tail is analysed on close
	})
	code, b := do(front.URL, http.MethodPost, "/ads/report?"+sel+"&position=14000&note=bank", "ta")
	if code != http.StatusCreated {
		t.Fatalf("report: %d %s", code, b)
	}
	var created struct{ ID int64 }
	if err := json.Unmarshal(b, &created); err != nil {
		t.Fatal(err)
	}
	front.Close()
	if err := px.Close(); err != nil {
		t.Fatal(err)
	}

	// later, in mpv
	px, err = New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front = httptest.NewServer(px.Handler())
	defer front.Close()

	var reps []catalog.Report
	code, b = do(front.URL, http.MethodGet, "/ads/reports", "tb")
	if err := json.Unmarshal(b, &reps); err != nil || code != http.StatusOK || len(reps) != 0 {
		t.Fatalf("bob sees %s (%d)", b, code)
	}
	if code, _ := do(front.URL, http.MethodDelete, fmt.Sprintf("/ads/reports/%d", created.ID), "tb"); code != http.StatusNotFound {
		t.Fatalf("bob deletes alice's report: %d", code)
	}
	code, b = do(front.URL, http.MethodGet, "/ads/reports", "ta")
	if err := json.Unmarshal(b, &reps); err != nil || code != http.StatusOK || len(reps) != 1 || reps[0].PosMs != 14_000 ||
		reps[0].Sel != "id=ep1" || reps[0].Note != "bank" {
		t.Fatalf("alice sees %s (%d)", b, code)
	}

	if code, _ := do(front.URL, http.MethodGet, "/s?"+sel, "ta"); code != http.StatusOK {
		t.Fatalf("stream: %d", code)
	}
	mark := fmt.Sprintf("/ads/mark?%s&startMs=%d&endMs=%d&label=bank&type=intro", sel, pre.Milliseconds(), (pre + adDur).Milliseconds())
	var last []byte
	waitFor(t, func() bool {
		code, last = do(front.URL, http.MethodPost, mark, "ta")
		return code == http.StatusOK
	})
	var marked struct{ Type string }
	if err := json.Unmarshal(last, &marked); err != nil || marked.Type != "intro" {
		t.Fatalf("marked ad %s: type %q, %v", last, marked.Type, err)
	}
	if code, _ := do(front.URL, http.MethodPost, mark+"x", "ta"); code != http.StatusBadRequest {
		t.Fatalf("unknown type: %d", code)
	}
	code, b = do(front.URL, http.MethodGet, "/ads/reports", "ta")
	if err := json.Unmarshal(b, &reps); err != nil || len(reps) != 0 {
		t.Fatalf("report not cleared by the mark: %s (%d; mark: %s)", b, code, last)
	}
	if n := len(px.Lib.List()); n != 2 {
		t.Fatalf("library has %d ads", n)
	}
	// the player will not stream the ad again (it seeks back within its cache): the mark
	// alone must make it skippable there
	rep := fetchReportAuth(t, front.URL+"/ads/file?"+sel, "ta")
	found := false
	for _, d := range rep.Ads {
		found = found || (d.Label == "bank" && d.Confirmed && d.Start == pre && d.End == pre+adDur && d.Score > 0)
	}
	if !found {
		t.Fatalf("the marked ad is not a confirmed detection of its file: %+v", rep.Ads)
	}

	// A file map without it (stored before the mark, say): a new session of the file still
	// knows the ad from where it was enrolled.
	if err := px.Maps.Put(t.Context(), &admap.FileMap{Key: rep.Key}); err != nil {
		t.Fatal(err)
	}
	px.mu.Lock()
	px.sess = map[string]*session{}
	px.mu.Unlock()
	q, _ := url.ParseQuery(sel)
	orig, _ := url.Parse(q.Get("u"))
	s := px.session(q, orig)
	found = false
	for _, d := range s.report().Ads {
		found = found || (d.Label == "bank" && d.Confirmed && d.Start == pre)
	}
	if !found {
		t.Fatalf("a new session does not know the ad enrolled from its file: %+v", s.report().Ads)
	}
	px.mu.Lock()
	for _, s := range px.sess {
		s.mu.Lock()
		if s.reported {
			t.Errorf("session %s still decodes analysed ranges after its report was cleared", s.id)
		}
		s.mu.Unlock()
	}
	px.mu.Unlock()
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

func fetchReportAuth(t *testing.T, u, token string) Report {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rep Report
	if resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&rep)
	}
	return rep
}
