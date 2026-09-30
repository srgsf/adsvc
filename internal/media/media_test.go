package media

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.txt"), "outside")
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a b.mkv"), "0123456789")
	write(t, filepath.Join(dir, "sub", "x.ts"), "ts")
	write(t, filepath.Join(dir, ".hidden"), "no")
	write(t, filepath.Join(dir, ".git", "config"), "no")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "out.mkv")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "outdir")); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.Play = func(u string) string { return "http://proxy/s?u=" + url.QueryEscape(u) }
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	s.Base = hs.URL
	return hs
}

func get(t *testing.T, hs *httptest.Server, method, path string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, hs.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func TestServeFileRange(t *testing.T) {
	hs := newServer(t)
	resp, body := get(t, hs, http.MethodGet, "/a%20b.mkv", "Range", "bytes=2-5")
	if resp.StatusCode != http.StatusPartialContent || body != "2345" {
		t.Errorf("range: %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 2-5/10" {
		t.Errorf("Content-Range %q", got)
	}
	if resp, body = get(t, hs, http.MethodGet, "/sub/x.ts"); resp.StatusCode != http.StatusOK || body != "ts" {
		t.Errorf("subfolder file: %d %q", resp.StatusCode, body)
	}
	if resp, _ = get(t, hs, http.MethodHead, "/a%20b.mkv"); resp.StatusCode != http.StatusOK || resp.ContentLength != 10 {
		t.Errorf("HEAD: %d, length %d", resp.StatusCode, resp.ContentLength)
	}
}

func TestServeRefuses(t *testing.T) {
	hs := newServer(t)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/.hidden", http.StatusNotFound},
		{http.MethodGet, "/.git/config", http.StatusNotFound},
		{http.MethodGet, "/out.mkv", http.StatusNotFound},           // symlink out of the folder
		{http.MethodGet, "/outdir/secret.txt", http.StatusNotFound}, // through a symlinked folder
		{http.MethodGet, "/%2e%2e/secret.txt", http.StatusNotFound},
		{http.MethodGet, "/missing.mkv", http.StatusNotFound},
		{http.MethodPost, "/a%20b.mkv", http.StatusMethodNotAllowed},
		{http.MethodGet, "/sub", http.StatusMovedPermanently},
		{http.MethodGet, "//sub", http.StatusMovedPermanently},
	} {
		resp, body := get(t, hs, c.method, c.path)
		if resp.StatusCode != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
		if loc := resp.Header.Get("Location"); loc != "" && loc != "/sub/" { // never //host
			t.Errorf("%s %s: redirects to %q, want /sub/", c.method, c.path, loc)
		}
		if strings.Contains(body, "outside") {
			t.Errorf("%s %s: served a file outside the folder", c.method, c.path)
		}
	}
}

func TestListing(t *testing.T) {
	hs := newServer(t)
	resp, body := get(t, hs, http.MethodGet, "/")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("listing: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	play := "http://proxy/s?u=" + url.QueryEscape(hs.URL+"/a%20b.mkv")
	for _, want := range []string{`href="sub/"`, `href="a%20b.mkv"`, strings.ReplaceAll(play, "&", "&amp;")} {
		if !strings.Contains(body, want) {
			t.Errorf("listing lacks %s:\n%s", want, body)
		}
	}
	for _, not := range []string{".hidden", ".git", "out.mkv", "outdir"} {
		if strings.Contains(body, not) {
			t.Errorf("listing shows %s", not)
		}
	}
	if _, body = get(t, hs, http.MethodGet, "/sub/"); !strings.Contains(body, url.QueryEscape(hs.URL+"/sub/x.ts")) ||
		!strings.Contains(body, `href="../"`) {
		t.Errorf("subfolder listing:\n%s", body)
	}
}
