package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/testmedia"
)

const testHash = "0123456789abcdef0123456789abcdef01234567"

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRoute(t *testing.T) {
	rt, err := ParseRoute("https://ts.example.com/base=http://127.0.0.1:8090")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ in, want string }{
		{"https://ts.example.com/base/play/h/1?x=1", "http://127.0.0.1:8090/play/h/1?x=1"},
		{"https://u:p@TS.example.com/base", "http://u:p@127.0.0.1:8090"},
		{"https://ts.example.com/basement/x", ""},
		{"http://ts.example.com/base/x", ""},
		{"https://other.example/base/x", ""},
	} {
		got := ""
		if u := rt.apply(mustURL(t, c.in)); u != nil {
			got = u.String()
		}
		if got != c.want {
			t.Errorf("route(%s) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := ParseRoute("nonsense"); err == nil {
		t.Error("ParseRoute accepted a route without =")
	}
}

// TestProxyAuth: adsvc's own credentials and the source's are separate. The client's
// Authorization header is adsvc's and never reaches the source; the source's credentials
// travel in X-Upstream-Authorization or in the user info of u.
func TestProxyAuth(t *testing.T) {
	payload := bytes.Repeat([]byte("not a media file, just bytes."), 100)
	var mu sync.Mutex
	var seen []string
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if u, p, ok := r.BasicAuth(); !ok || u != "ts" || p != "tspw" {
			w.Header().Set("WWW-Authenticate", `Basic realm="ts"`)
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		http.ServeContent(w, r, "x.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer src.Close()
	px, err := New(t.Context(), Config{Users: []User{
		{Name: "alice", Tokens: []string{"T0K", "T0K2"}},
		{Name: "admin", Tokens: []string{"ADM"}, Admin: true},
		{Name: "off"}, // no tokens: cannot log in at all
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	fu := mustURL(t, front.URL)
	withUser := func(u, p string) string { c := *fu; c.User = url.UserPassword(u, p); return c.String() }
	srcWithCreds := mustURL(t, src.URL)
	srcWithCreds.User = url.UserPassword("ts", "tspw")

	// /healthz needs no auth, and says what answers.
	if resp, err := http.Get(front.URL + "/healthz"); err != nil {
		t.Fatal(err)
	} else {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "adsvc dev\n" {
			t.Errorf("healthz: %d %q, want 200 \"adsvc dev\"", resp.StatusCode, body)
		}
	}

	for _, c := range []struct {
		name, url string
		hdr       map[string]string
		code      int
	}{
		{"no auth", front.URL + "/s?u=" + urlEscape(src.URL+"/x"), nil, http.StatusUnauthorized},
		{"api needs auth too", front.URL + "/ads/maps", nil, http.StatusUnauthorized},
		{"wrong key", front.URL + "/s?u=" + urlEscape(src.URL+"/x"), map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"bearer + upstream header", front.URL + "/s?u=" + urlEscape(src.URL+"/x"),
			map[string]string{"Authorization": "Bearer T0K", "X-Upstream-Authorization": basic("ts", "tspw")}, http.StatusOK},
		{"an admin's token works too", front.URL + "/s?u=" + urlEscape(src.URL+"/x"),
			map[string]string{"Authorization": "Bearer ADM", "X-Upstream-Authorization": basic("ts", "tspw")}, http.StatusOK},
		{"bearer, source rejects", front.URL + "/s?u=" + urlEscape(src.URL+"/x"),
			map[string]string{"Authorization": "Bearer T0K"}, http.StatusUnauthorized},
		{"name:token in the URL, source credentials in u", withUser("alice", "T0K") + "/s?u=" + urlEscape(srcWithCreds.String()+"/x"), nil, http.StatusOK},
		{"the user's other token", withUser("alice", "T0K2") + "/s?u=" + urlEscape(srcWithCreds.String()+"/x"), nil, http.StatusOK},
		{"another user's token", withUser("admin", "T0K") + "/s?u=" + urlEscape(srcWithCreds.String()+"/x"), nil, http.StatusUnauthorized},
		{"unknown user", withUser("bob", "T0K") + "/s?u=" + urlEscape(srcWithCreds.String()+"/x"), nil, http.StatusUnauthorized},
		{"token without a name", withUser("", "T0K") + "/s?u=" + urlEscape(srcWithCreds.String()+"/x"), nil, http.StatusUnauthorized},
		{"token as the name", withUser("T0K", "") + "/s?u=" + urlEscape(srcWithCreds.String()+"/x"), nil, http.StatusUnauthorized},
		{"a user without tokens", withUser("off", "") + "/s?u=" + urlEscape(src.URL+"/x"), nil, http.StatusUnauthorized},
		{"empty basic", withUser("", "") + "/s?u=" + urlEscape(src.URL+"/x"), nil, http.StatusUnauthorized},
	} {
		req, _ := http.NewRequest(http.MethodGet, c.url, nil)
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("%s: got %d, want %d", c.name, resp.StatusCode, c.code)
			continue
		}
		if c.code == http.StatusOK && !bytes.Equal(body, payload) {
			t.Errorf("%s: forwarded %d of %d bytes", c.name, len(body), len(payload))
		}
		if c.code == http.StatusUnauthorized && !slices.ContainsFunc(resp.Header.Values("WWW-Authenticate"), isBasic) {
			t.Errorf("%s: 401 without a Basic challenge (ffmpeg needs one to send name:token@host): %q",
				c.name, resp.Header.Values("WWW-Authenticate"))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, a := range seen {
		if a != "" && a != basic("ts", "tspw") {
			t.Errorf("source saw adsvc's credentials: %q", a)
		}
	}
}

func basic(u, p string) string {
	r, _ := http.NewRequest(http.MethodGet, "http://x", nil)
	r.SetBasicAuth(u, p)
	return r.Header.Get("Authorization")
}

// TestProxyIdentity: a TorrServer URL is keyed by its torrent file, and a re-signed URL
// (different token) is the same file.
func TestProxyIdentity(t *testing.T) {
	payload := bytes.Repeat([]byte("not a media file, just bytes."), 100)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer src.Close()
	px, err := New(t.Context(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	play := func(up string) {
		resp, err := http.Get(front.URL + "/s?u=" + urlEscape(up))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	play(src.URL + "/play/" + strings.ToUpper(testHash) + "/2")
	if rep := fetchReport(t, front.URL+"/ads/file?ih="+testHash+"&idx=2"); rep.Key != "ih:"+testHash+"/2" {
		t.Fatalf("TorrServer /play URL not keyed by its torrent: %+v", rep)
	}

	play(src.URL + "/v/x.bin?token=first&q=1")
	play(src.URL + "/v/x.bin?q=1&token=second")
	px.mu.Lock()
	n := len(px.sess)
	px.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d sessions, want 2 (the torrent and one re-signed URL)", n)
	}
	if rep := fetchReport(t, front.URL+"/ads/file?u="+urlEscape(src.URL+"/v/x.bin?q=1&token=third")); rep.Key == "" {
		t.Fatal("re-signed URL not found by the ads API")
	}
}

// TestProxyRouteTorrServer is the co-located deployment end to end: the client wraps the
// TorrServer URL it knows (a public name adsvc cannot resolve), adsvc routes it to the
// local TorrServer, and the ad is reported under the torrent key.
func TestProxyRouteTorrServer(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ep.avi")
	const pre, adDur, post = 10 * time.Second, 12 * time.Second, 8 * time.Second
	testmedia.AVIWithAd(t, path, pre, adDur, post)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/play/") {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, path)
	}))
	defer ts.Close()
	rt, err := ParseRoute("https://ts.public.invalid=" + ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	px, err := New(t.Context(), Config{Routes: []Route{rt}, Users: users("T"), DataDir: filepath.Join(dir, "data")})
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
	get := func(u string) (int, int64) {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("Authorization", "Bearer T")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		n, _ := io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, n
	}

	if code, _ := get(front.URL + "/s?u=" + urlEscape("https://elsewhere.invalid/play/"+testHash+"/1")); code != http.StatusForbidden {
		t.Fatalf("unrouted upstream: got %d, want 403", code)
	}
	code, n := get(front.URL + "/s?u=" + urlEscape(fmt.Sprintf("https://ts.public.invalid/play/%s/1", testHash)))
	if code != http.StatusOK || n == 0 {
		t.Fatalf("routed stream: %d, %d bytes", code, n)
	}
	var rep Report
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, front.URL+"/ads/file?ih="+testHash+"&idx=1", nil)
		req.Header.Set("Authorization", "Bearer T")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rep = Report{}
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
				t.Fatal(err)
			}
		}
		resp.Body.Close()
		if len(rep.Ads) > 0 && rep.Ads[0].Confirmed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(rep.Ads) != 1 || !rep.Ads[0].Confirmed || rep.Key != "ih:"+testHash+"/1" {
		t.Fatalf("ad not reported under the torrent key: %+v", rep)
	}
	if got := rep.Ads[0]; (got.Start-pre).Abs() > 500*time.Millisecond || (got.End-(pre+adDur)).Abs() > 500*time.Millisecond {
		t.Fatalf("wrong boundaries: %+v", got)
	}
}

// users is one user with these tokens.
func users(tokens ...string) []User { return []User{{Name: "u", Tokens: tokens}} }

// isBasic reports whether a WWW-Authenticate value offers Basic auth.
func isBasic(challenge string) bool { return strings.HasPrefix(challenge, "Basic ") }
