package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/metrics"
	"github.com/srgsf/adsvc/internal/testmedia"
)

// samples parses the metrics text into series (name plus labels, as written) -> value.
func samples(t *testing.T) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(metrics.Text()))
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "#") {
			continue
		}
		i := strings.LastIndexByte(l, ' ')
		v, err := strconv.ParseFloat(l[i+1:], 64)
		if err != nil {
			t.Fatalf("bad sample %q", l)
		}
		out[l[:i]] = v
	}
	return out
}

// TestMetrics streams a file with an ad through the proxy and expects the metrics of
// every stage to move, with no URL or file key in them.
func TestMetrics(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.mkv")
	const pre, adDur, post = 10 * time.Second, 12 * time.Second, 8 * time.Second
	testmedia.WithAd(t, path, pre, adDur, post, "-c:a", "ac3")
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path)
	}))
	defer src.Close()
	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder(), DataDir: filepath.Join(dir, "data")})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	pcm := testmedia.PCM(t, path, pre, adDur)
	if _, err := px.Lib.Add(t.Context(), "test ad", "", pcm, nil); err != nil {
		t.Fatal(err)
	}

	before := samples(t)
	upstream := src.URL + "/clip.mkv"
	resp, err := http.Get(front.URL + "/s?u=" + urlEscape(upstream))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var rep Report
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if rep = fetchReport(t, front.URL+"/ads/file?u="+urlEscape(upstream)); len(rep.Ads) > 0 && rep.Ads[0].Confirmed {
			break
		}
	}
	if len(rep.Ads) == 0 {
		t.Fatalf("ad not detected: %+v", rep)
	}
	if err := px.Close(); err != nil { // ends the decoder runs and the session
		t.Fatal(err)
	}
	after := samples(t)

	for _, s := range []string{
		`adsvc_stream_bytes_total`,
		`adsvc_upstream_requests_total{result="2xx"}`,
		`adsvc_upstream_response_seconds_count`,
		`adsvc_http_requests_total{handler="stream",method="GET",code="200"}`,
		`adsvc_http_requests_total{handler="file",method="GET",code="200"}`,
		`adsvc_http_request_duration_seconds_count{handler="file"}`,
		`adsvc_sessions_opened_total{kind="normal"}`,
		`adsvc_sessions_closed_total{reason="shutdown"}`,
		`adsvc_containers_total{type="matroska"}`,
		`adsvc_ffmpeg_runs_total{result="ok"}`,
		`adsvc_ffmpeg_run_seconds_count`,
		`adsvc_analysed_media_seconds_total`,
		`adsvc_fingerprint_duration_seconds_count`,
		`adsvc_match_duration_seconds_count`,
		`adsvc_detections_total`,
		`adsvc_detection_score_count`,
		`adsvc_confirmations_total`,
		`adsvc_file_map_requests_total{result="hit"}`,
		`adsvc_catalog_write_duration_seconds_count`,
	} {
		if after[s] <= before[s] {
			t.Errorf("%s did not grow: %v -> %v", s, before[s], after[s])
		}
	}
	for _, s := range []string{`adsvc_sessions_active`, `adsvc_analyser_queue_bytes`, `adsvc_ffmpeg_running`, `adsvc_pcm_ring_bytes`} {
		if after[s] != before[s] {
			t.Errorf("%s = %v after the proxy closed, want %v", s, after[s], before[s])
		}
	}
	if after[`adsvc_library_ads`] < 1 {
		t.Errorf("adsvc_library_ads = %v", after[`adsvc_library_ads`])
	}
	text := string(metrics.Text())
	for _, secret := range []string{src.URL, strings.TrimPrefix(src.URL, "http://"), "clip.mkv", rep.Key} {
		if strings.Contains(text, secret) {
			t.Errorf("metrics mention %q", secret)
		}
	}
}
