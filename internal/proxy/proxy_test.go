package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/admap"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/filekey"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/store"
	"github.com/srgsf/adsvc/internal/testmedia"
)

// TestProxyAVI streams an AVI through the proxy exactly the way a player would - header,
// index, then media data from a seek - and checks that the ad is found in the bytes that
// were forwarded, without any extra request to the source.
func TestProxyAVI(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "episode.avi")
	const pre, adDur, post = 20 * time.Second, 15 * time.Second, 25 * time.Second
	testmedia.AVIWithAd(t, path, pre, adDur, post)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(raw))

	var upstreamReqs atomic.Int32
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamReqs.Add(1)
		http.ServeFile(w, r, path)
	}))
	defer src.Close()

	px, err := New(t.Context(), Config{
		FFmpeg:  testmedia.Decoder(),
		DataDir: filepath.Join(dir, "data"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front := httptest.NewServer(px.Handler())
	defer front.Close()

	// the reference ad (a user marked it in some other episode)
	pcm := testmedia.PCM(t, path, pre, adDur)
	first, err := px.Lib.Add(t.Context(), "test ad", "", pcm, nil)
	if err != nil {
		t.Fatal(err)
	}

	base := front.URL + "/s?u=" + urlEscape(src.URL+"/file.avi") + "&ih=TESTHASH&idx=1"
	get := func(rng string) []byte {
		req, _ := http.NewRequest(http.MethodGet, base, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// 1. the player reads the file header ...
	head := get(fmt.Sprintf("bytes=0-%d", filekey.EdgeSize-1))
	if !bytes.Equal(head, raw[:filekey.EdgeSize]) {
		t.Fatalf("header not forwarded byte for byte (%d bytes)", len(head))
	}
	// 2. ... then the end of the file, where idx1 is ...
	tail := get(fmt.Sprintf("bytes=%d-", size-filekey.EdgeSize))
	if !bytes.Equal(tail, raw[size-filekey.EdgeSize:]) {
		t.Fatal("tail not forwarded byte for byte")
	}
	// 3. ... then plays from a seek at 20% (before the ad, which starts at 20s of 60s).
	seek := size / 5
	body := get(fmt.Sprintf("bytes=%d-", seek))
	if !bytes.Equal(body, raw[seek:]) {
		t.Fatalf("media not forwarded byte for byte (%d of %d bytes)", len(body), size-seek)
	}

	if n := upstreamReqs.Load(); n != 3 {
		t.Fatalf("pass-through must not read the source on its own: %d upstream requests for 3 player requests", n)
	}

	var rep Report
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		rep = fetchReport(t, front.URL+"/ads/file?ih=TESTHASH&idx=1")
		if len(rep.Ads) > 0 && rep.Ads[0].Confirmed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(rep.Ads) != 1 || !rep.Ads[0].Confirmed {
		t.Fatalf("ad not detected/confirmed: %+v", rep)
	}
	got := rep.Ads[0]
	t.Logf("detected %+v (analysed %v, container %s)", got, rep.Analyzed, rep.Container)
	if (got.Start-pre).Abs() > 500*time.Millisecond || (got.End-(pre+adDur)).Abs() > 500*time.Millisecond {
		t.Fatalf("wrong boundaries: %+v", got)
	}
	if rep.Container != "avi" {
		t.Fatalf("container = %q", rep.Container)
	}

	// the file key the caller gave, aliased to the content key computed from its own bytes
	wantContent := contentKey(raw)
	if rep.Key != filekey.Torrent("TESTHASH", "1") {
		t.Fatalf("key = %q", rep.Key)
	}
	if !slices.Contains(rep.Aliases, wantContent) {
		t.Fatalf("aliases %v do not contain the content key %s", rep.Aliases, wantContent)
	}
	// the same file, streamed without an infohash, is recognised by its content key
	storeAll(px) // what the janitor does every 15 s
	if m, err := px.Maps.Lookup(t.Context(), wantContent); err != nil || m == nil || len(m.Ads) != 1 {
		t.Fatalf("ad map not stored under the content key: %+v, %v", m, err)
	}

	// enrolling from the proxy uses the audio already decoded: no reading anything again
	before := upstreamReqs.Load()
	resp, err := http.Post(front.URL+"/ads/mark?ih=TESTHASH&idx=1&startMs=21000&endMs=34000&label=second", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mark: %s: %s", resp.Status, b)
	}
	if n := upstreamReqs.Load(); n != before {
		t.Fatalf("mark read the source again (%d -> %d)", before, n)
	}
	if len(px.Lib.List()) != 2 {
		t.Fatalf("library: %+v", px.Lib.List())
	}

	// ad maps survive a restart
	if err := px.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := st.Maps.Lookup(t.Context(), wantContent)
	if err != nil || m == nil {
		t.Fatalf("no ad map under the content key after a restart: %v", err)
	}
	var found *detect.Detection
	for i := range m.Ads { // marking added a second ad, which is found in this file too
		if m.Ads[i].AdID == first.ID {
			found = &m.Ads[i]
		}
	}
	if found == nil || (found.Start-pre).Abs() > 500*time.Millisecond || !found.Confirmed {
		t.Fatalf("reloaded ad map: %+v", m)
	}
}

// TestProxyPassesThroughUnknownContainer: a format we cannot demux must still be forwarded.
func TestProxyPassesThroughUnknownContainer(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("not a media file, just bytes."), 5000)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer src.Close()
	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder(), DataDir: filepath.Join(dir, "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front := httptest.NewServer(px.Handler())
	defer front.Close()

	resp, err := http.Get(front.URL + "/s?u=" + urlEscape(src.URL+"/x.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, payload) {
		t.Fatalf("forwarded %d of %d bytes", len(got), len(payload))
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("upstream headers not forwarded: %v", resp.Header)
	}
}

func TestProxyRejectsBadUpstream(t *testing.T) {
	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder()})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	cfg := px.Config()
	cfg.AllowUpstream = func(u *url.URL) bool { return u.Host == "allowed.example" }
	px.Reload(cfg)
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	for _, c := range []struct {
		url  string
		code int
	}{
		{front.URL + "/s", http.StatusBadRequest},
		{front.URL + "/s?u=" + urlEscape("file:///etc/passwd"), http.StatusBadRequest},
		{front.URL + "/s?u=" + urlEscape("http://denied.example/x"), http.StatusForbidden},
	} {
		resp, err := http.Get(c.url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("%s: got %d, want %d", c.url, resp.StatusCode, c.code)
		}
	}
}

// TestConfirmTwoBlocks covers the skip rule for a short look-ahead: with only part of the
// ad analysed, two consecutive blocks agreeing on the alignment confirm it.
func TestConfirmTwoBlocks(t *testing.T) {
	s := &session{p: withConfig(Config{ConfirmScore: 40}), key: "k"}
	d := detect.Detection{AdID: fingerprint.ID{1}, Label: "ad", Type: "ad", Start: 100 * time.Second, End: 130 * time.Second, Score: 60}
	s.mu.Lock()
	s.addDetection(d, 110*time.Second, 120*time.Second)
	s.covered = s.covered.Add(110*time.Second, 120*time.Second)
	s.confirm()
	s.mu.Unlock()
	if r := s.report(); r.Ads[0].Confirmed {
		t.Fatalf("one block must not confirm: %+v", r.Ads[0])
	}
	s.mu.Lock()
	s.addDetection(d, 120*time.Second, 130*time.Second) // the next block agrees on the same alignment
	s.covered = s.covered.Add(120*time.Second, 130*time.Second)
	s.confirm()
	s.mu.Unlock()
	if r := s.report(); !r.Ads[0].Confirmed {
		t.Fatalf("two consecutive blocks must confirm: %+v", r.Ads[0])
	}
	// the same evidence below the confirm threshold is not enough
	s3 := &session{p: withConfig(Config{ConfirmScore: 200}), key: "k"}
	s3.addDetection(d, 110*time.Second, 120*time.Second)
	s3.addDetection(d, 120*time.Second, 130*time.Second)
	s3.confirm()
	if r := s3.report(); r.Ads[0].Confirmed {
		t.Fatalf("a low score must not confirm: %+v", r.Ads[0])
	}
	// a non-consecutive repeat is not extra evidence
	s2 := &session{p: withConfig(Config{ConfirmScore: 40}), key: "k"}
	s2.addDetection(d, 110*time.Second, 120*time.Second)
	s2.addDetection(d, 200*time.Second, 210*time.Second)
	s2.confirm()
	if r := s2.report(); r.Ads[0].Confirmed {
		t.Fatalf("blocks with a gap must not confirm: %+v", r.Ads[0])
	}
}

// storeAll writes every live session's map, as the janitor does.
func storeAll(px *Proxy) {
	px.mu.Lock()
	all := slices.Collect(maps.Values(px.sess))
	px.mu.Unlock()
	for _, s := range all {
		s.store()
	}
}

// withConfig is a bare Proxy with cfg, for tests of session logic.
func withConfig(cfg Config) *Proxy {
	p := &Proxy{ctx: context.Background()}
	p.cfg.Store(&cfg)
	return p
}

func contentKey(data []byte) string {
	h := sha256.New()
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(len(data)))
	h.Write(sz[:])
	h.Write(data[:filekey.EdgeSize])
	h.Write(data[len(data)-filekey.EdgeSize:])
	return "c:" + hex.EncodeToString(h.Sum(nil))
}

func urlEscape(s string) string { return url.QueryEscape(s) }

func fetchReport(t *testing.T, u string) Report {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rep Report
	if resp.StatusCode == http.StatusNotFound {
		return rep
	}
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// TestProxyAVIFromStart covers a player that just plays from the beginning and never reads
// the index: the demuxer has to build its own timeline while the file streams past.
func TestProxyAVIFromStart(t *testing.T) {
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
	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder(), DataDir: filepath.Join(dir, "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front := httptest.NewServer(px.Handler())
	defer front.Close()

	pcm := testmedia.PCM(t, path, pre, adDur)
	if _, err := px.Lib.Add(t.Context(), "test ad", "", pcm, nil); err != nil {
		t.Fatal(err)
	}

	// one plain GET, no Range: the whole file streams through in order
	resp, err := http.Get(front.URL + "/s?u=" + urlEscape(src.URL+"/clip.avi"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); n != st.Size() {
		t.Fatalf("forwarded %d bytes of %d", n, st.Size())
	}

	var rep Report
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		rep = fetchReport(t, front.URL+"/ads/file?u="+urlEscape(src.URL+"/clip.avi"))
		if len(rep.Ads) > 0 && rep.Ads[0].Confirmed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(rep.Ads) != 1 || !rep.Ads[0].Confirmed {
		t.Fatalf("ad not detected without an index: %+v", rep)
	}
	if got := rep.Ads[0]; (got.Start-pre).Abs() > 500*time.Millisecond || (got.End-(pre+adDur)).Abs() > 500*time.Millisecond {
		t.Fatalf("wrong boundaries: %+v", got)
	}
	// streamed whole, the file is identified by its content alone
	if !strings.HasPrefix(rep.Key, "c:") {
		t.Fatalf("key = %q, want a content key", rep.Key)
	}
	t.Logf("detected %+v as %s (duration %v)", rep.Ads[0], rep.Key, rep.Duration)
}

// TestProxyContainers streams each container through the proxy the way a player reads it
// and expects the ad to be found at the right time, with no extra reads on the source.
func TestProxyContainers(t *testing.T) {
	testmedia.NeedFFmpeg(t)
	const pre, adDur, post = 20 * time.Second, 15 * time.Second, 25 * time.Second
	tests := []struct {
		name string
		ext  string
		args []string
	}{
		{"mkv-aac", ".mkv", []string{"-c:a", "aac"}},
		{"mkv-ac3", ".mkv", []string{"-c:a", "ac3"}},
		{"webm-opus", ".webm", []string{"-c:a", "libopus", "-c:v", "libvpx"}},
		{"mp4-aac-moov-at-end", ".mp4", []string{"-c:a", "aac"}},
		{"mp4-fragmented", ".mp4", []string{"-c:a", "aac", "-movflags", "frag_keyframe+empty_moov"}},
		{"mov-ac3", ".mov", []string{"-c:a", "ac3"}},
		{"ts-aac", ".ts", []string{"-c:a", "aac"}},
		{"ts-mp2", ".ts", []string{"-c:a", "mp2"}},
		{"m2ts-ac3", ".m2ts", []string{"-c:a", "ac3", "-mpegts_m2ts_mode", "1"}},
	}
	dir := t.TempDir()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name+tt.ext)
			testmedia.WithAd(t, path, pre, adDur, post, tt.args...)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			size := int64(len(raw))

			var upstream atomic.Int32
			src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstream.Add(1)
				http.ServeFile(w, r, path)
			}))
			defer src.Close()
			px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder()})
			if err != nil {
				t.Fatal(err)
			}
			defer px.Close()
			front := httptest.NewServer(px.Handler())
			defer front.Close()

			pcm := testmedia.PCM(t, path, pre, adDur)
			if _, err := px.Lib.Add(t.Context(), "ad", "", pcm, nil); err != nil {
				t.Fatal(err)
			}

			base := front.URL + "/s?u=" + urlEscape(src.URL+"/f"+tt.ext)
			get := func(from, to int64) {
				req, _ := http.NewRequest(http.MethodGet, base, nil)
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to-1))
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				got, _ := io.ReadAll(resp.Body)
				if !bytes.Equal(got, raw[from:to]) {
					t.Fatalf("bytes %d-%d not forwarded unchanged", from, to)
				}
			}
			// the player: file header, the index if it is at the end, then play from 20%
			reads := 1
			get(0, min(size, 256<<10))
			if moov, ok := testmedia.TopBoxes(raw)["moov"]; ok && tt.ext != ".mkv" && moov > 256<<10 {
				get(moov, size)
				reads++
			}
			get(size/5, size)
			reads++
			if n := upstream.Load(); int(n) != reads {
				t.Fatalf("%d upstream requests for %d player requests", n, reads)
			}

			var rep Report
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				rep = fetchReport(t, front.URL+"/ads/file?u="+urlEscape(src.URL+"/f"+tt.ext))
				if len(rep.Ads) > 0 && rep.Ads[0].Confirmed {
					break
				}
			}
			if len(rep.Ads) != 1 || !rep.Ads[0].Confirmed {
				t.Fatalf("ad not detected: %+v", rep)
			}
			if got := rep.Ads[0]; (got.Start-pre).Abs() > 300*time.Millisecond || (got.End-(pre+adDur)).Abs() > 300*time.Millisecond {
				t.Fatalf("ad at %v-%v, want %v-%v", got.Start, got.End, pre, pre+adDur)
			}
			t.Logf("%s: ad at %v-%v (score %d), analysed %v", rep.Container, rep.Ads[0].Start, rep.Ads[0].End, rep.Ads[0].Score, rep.Analyzed)
		})
	}
}

// TestConfirmationSurvivesReload: a file that has been analysed before must be skippable
// straight away, even where the stored coverage does not span the whole ad.
func TestConfirmationSurvivesReload(t *testing.T) {
	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	s := &session{p: px, key: "ih:x/1"}
	d := detect.Detection{AdID: fingerprint.ID{1}, Label: "ad", Type: "ad", Start: 100 * time.Second, End: 130 * time.Second, Score: 60}
	s.addDetection(d, 110*time.Second, 120*time.Second)
	s.addDetection(d, 120*time.Second, 130*time.Second) // two consecutive blocks: confirmed
	s.covered = s.covered.Add(110*time.Second, 130*time.Second)
	s.confirm()
	s.dirty = true
	s.store()

	s2 := &session{p: px, key: "ih:x/1"}
	s2.load("ih:x/1")
	r := s2.report()
	if len(r.Ads) != 1 || !r.Ads[0].Confirmed {
		t.Fatalf("stored confirmation lost: %+v", r.Ads)
	}
	if !r.Analyzed.Contains(110*time.Second, 130*time.Second) {
		t.Fatalf("stored coverage lost: %v", r.Analyzed)
	}
}

// A stored confirmation lands on the occurrence it belongs to, also when the session
// already holds several occurrences of the ad.
func TestLoadConfirmsItsOwnOccurrence(t *testing.T) {
	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	first := detect.Detection{AdID: fingerprint.ID{1}, Label: "ad", Type: "ad", Start: 100 * time.Second, End: 130 * time.Second, Score: 60}
	second := first
	second.Start, second.End = 500*time.Second, 530*time.Second
	stored := first
	stored.Confirmed = true
	if err := px.Maps.Put(t.Context(), &admap.FileMap{Key: "ih:x/1", Ads: []detect.Detection{stored}}); err != nil {
		t.Fatal(err)
	}

	s := &session{p: px, key: "ih:x/1"}
	s.addDetection(first, 110*time.Second, 120*time.Second)
	s.addDetection(second, 510*time.Second, 520*time.Second)
	s.load("ih:x/1")
	r := s.report()
	if len(r.Ads) != 2 {
		t.Fatalf("ads after the load: %+v", r.Ads)
	}
	for _, a := range r.Ads {
		if want := a.Start == first.Start; a.Confirmed != want {
			t.Errorf("ad at %v: confirmed %v, want %v", a.Start, a.Confirmed, want)
		}
	}
}

// adsvc's own cookies (the web page's session) never reach a source; the source's own do.
func TestStreamDropsOwnCookies(t *testing.T) {
	got := make(chan string, 1)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Cookie")
	}))
	defer src.Close()
	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder()})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/s?u="+urlEscape(src.URL+"/x"), nil)
	req.Header.Set("Cookie", "adsvc_session=secret-session; source=value")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if c := <-got; c != "source=value" {
		t.Fatalf("upstream got Cookie %q, want only the source's", c)
	}
}

// Every redirect hop passes the allow-list and the routes, not only the URL the client sent.
func TestStreamRedirects(t *testing.T) {
	var forbiddenHits atomic.Int32
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forbiddenHits.Add(1)
	}))
	defer forbidden.Close()
	var allowed *httptest.Server
	allowed = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, forbidden.URL+"/private", http.StatusFound)
		case "/here":
			http.Redirect(w, r, "/file", http.StatusFound)
		case "/self":
			http.Redirect(w, r, allowed.URL+"/file", http.StatusTemporaryRedirect)
		case "/file":
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer allowed.Close()
	au, _ := url.Parse(allowed.URL)

	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder()})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	get := func(u string) (int, string) {
		t.Helper()
		resp, err := http.Get(front.URL + "/s?u=" + urlEscape(u))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	cfg := px.Config()
	cfg.AllowUpstream = func(u *url.URL) bool { return u.Host == au.Host }
	px.Reload(cfg)
	if code, body := get(allowed.URL + "/here"); code != http.StatusOK || body != "ok" {
		t.Errorf("redirect within the allowed host: %d %q", code, body)
	}
	if code, _ := get(allowed.URL + "/away"); code != http.StatusForbidden {
		t.Errorf("redirect to a host not allowed: %d, want 403", code)
	}

	// With a route, the internal target may redirect within itself.
	rt, err := ParseRoute("https://ts.example=" + allowed.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg = px.Config()
	cfg.AllowUpstream, cfg.Routes = nil, []Route{rt}
	px.Reload(cfg)
	if code, body := get("https://ts.example/self"); code != http.StatusOK || body != "ok" {
		t.Errorf("redirect within the route's target: %d %q", code, body)
	}
	if code, _ := get("https://ts.example/away"); code != http.StatusForbidden {
		t.Errorf("redirect off the route's target: %d, want 403", code)
	}
	if n := forbiddenHits.Load(); n != 0 {
		t.Fatalf("the host not allowed was contacted %d times", n)
	}
}

// /ads/file keeps its JSON shape (docs/handoff.md) with the map embedded in Report.
func TestReportJSON(t *testing.T) {
	d := detect.Detection{AdID: fingerprint.ID{1}, Label: "ad", Type: "ad", Start: 100 * time.Second, End: 130 * time.Second, Score: 60, Confirmed: true}
	rep := Report{Key: "ih:x/1", Aliases: []string{"c:1"}, Size: 10, Duration: time.Minute,
		Analyzed: detect.Intervals{{0, 30 * time.Second}}, Ads: []detect.Detection{d}, Container: "matroska", AnalysedTo: 30 * time.Second}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"key", "aliases", "size", "durationMs", "analyzed", "ads", "container", "analysedToMs"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("no %q in %s", k, b)
		}
	}
	if len(keys) != 8 {
		t.Errorf("unexpected fields in %s", b)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Key != rep.Key || back.Duration != rep.Duration || back.AnalysedTo != rep.AnalysedTo || back.Container != rep.Container ||
		len(back.Ads) != 1 || back.Ads[0] != d || !slices.Equal(back.Analyzed, rep.Analyzed) {
		t.Fatalf("round trip: %+v", back)
	}
}

// A HEAD starts no session (and so no analysis); a session holds no decoded audio until
// it decodes some.
func TestHeadStartsNoSession(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		if r.Method == http.MethodGet {
			_, _ = w.Write(make([]byte, 100))
		}
	}))
	defer src.Close()
	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder()})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	u := front.URL + "/s?u=" + urlEscape(src.URL+"/x")
	resp, err := http.Head(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	px.mu.Lock()
	n := len(px.sess)
	px.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d sessions after a HEAD", n)
	}
	resp, err = http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	px.mu.Lock()
	for _, s := range px.sess {
		if b := s.ring.Bytes(); b != 0 {
			t.Errorf("a session that decoded nothing holds %d bytes of audio", b)
		}
	}
	px.mu.Unlock()
}
