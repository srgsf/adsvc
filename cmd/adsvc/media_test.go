package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srgsf/adsvc/internal/config"
	"github.com/srgsf/adsvc/internal/proxy"
)

func TestMediaSettings(t *testing.T) {
	for _, c := range []struct {
		args    []string
		file    config.Media
		want    mediaSettings
		invalid bool
	}{
		{args: nil, want: mediaSettings{}},
		{args: []string{"-media-listen", "0.0.0.0:1"}, want: mediaSettings{}}, // off: not checked
		{args: []string{"-media", "/v"}, want: mediaSettings{"/v", defaultMediaListen}},
		{file: config.Media{Dir: "/f", Listen: "127.0.0.1:9"}, want: mediaSettings{"/f", "127.0.0.1:9"}},
		{args: []string{"-media", "/v"}, file: config.Media{Dir: "/f", Listen: "[::1]:9"}, want: mediaSettings{"/v", "[::1]:9"}},
		{args: []string{"-media", "/v", "-media-listen", "0.0.0.0:8000"}, invalid: true}, // no auth: loopback only
		{args: []string{"-media", "/v", "-media-listen", "127.0.0.1:0"}, invalid: true},  // URLs must be stable
		{args: []string{"-media", "/v", "-media-listen", "8000"}, invalid: true},
		{args: []string{"-media", "/v", "-media-listen", ":9"}, want: mediaSettings{"/v", "localhost:9"}}, // no host: localhost
	} {
		pf := proxyFlagsFor(t, c.args...)
		got, err := pf.media(&config.Proxy{Media: c.file})
		if _, usage := errors.AsType[usageError](err); c.invalid != usage || (!c.invalid && err != nil) {
			t.Errorf("%v %+v: err %v", c.args, c.file, err)
			continue
		}
		if !c.invalid && got != c.want {
			t.Errorf("%v %+v: %+v, want %+v", c.args, c.file, got, c.want)
		}
	}
}

func TestAllowMedia(t *testing.T) {
	u := func(s string) *url.URL {
		v, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if allowMedia("127.0.0.1:8000", nil, false) != nil {
		t.Error("an open proxy must stay open")
	}
	only := allowMedia("127.0.0.1:8000", nil, true) // routes: only routed upstreams, and media
	if !only(u("http://127.0.0.1:8000/a.mkv")) || only(u("http://127.0.0.1:8001/a.mkv")) || only(u("https://127.0.0.1:8000/a")) {
		t.Error("with routes only the media server is allowed besides them")
	}
	both := allowMedia("localhost:8000", nil, true) // localhost: either loopback address
	for _, s := range []string{"http://localhost:8000/a", "http://LOCALHOST:8000/a", "http://127.0.0.1:8000/a", "http://[::1]:8000/a"} {
		if !both(u(s)) {
			t.Errorf("%s: not the media server at localhost:8000", s)
		}
	}
	if both(u("http://[::1]:8001/a")) || both(u("http://192.168.1.2:8000/a")) {
		t.Error("another port or host passed as the media server")
	}
	list := allowMedia("127.0.0.1:8000", allowHosts([]string{"ts.lan"}), false)
	if !list(u("http://127.0.0.1:8000/a.mkv")) || !list(u("http://ts.lan/x")) || list(u("http://other/x")) {
		t.Error("the allow-list must keep its hosts and add the media server")
	}
}

// A file of the media folder plays through the proxy, although -allow names another host.
func TestMediaThroughProxy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ep 1.mkv"), []byte("not really a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	pf := proxyFlagsFor(t, "-data-dir", "", "-allow", "ts.lan", "-media", dir, "-media-listen", addr)
	file := &config.Proxy{}
	if pf.mediaRun, err = pf.media(file); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := pf.build(file)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	px, err := proxy.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	wait, err := startMedia(ctx, pf.mediaRun, front.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); wait(); _ = px.Close() }()

	fetch := func(u string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
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
		return resp.StatusCode, string(b)
	}
	code, page := fetch("http://" + addr + "/")
	play := front.URL + "/s?u=" + url.QueryEscape("http://"+addr+"/ep%201.mkv")
	if code != http.StatusOK || !strings.Contains(page, strings.ReplaceAll(play, "&", "&amp;")) {
		t.Fatalf("listing (%d) lacks %s:\n%s", code, play, page)
	}
	if code, body := fetch(play); code != http.StatusOK || body != "not really a video" {
		t.Errorf("through the proxy: %d %q", code, body)
	}
	if code, _ := fetch(front.URL + "/s?u=" + url.QueryEscape("http://other.lan/x")); code != http.StatusForbidden {
		t.Errorf("other hosts: %d, want 403", code)
	}
}
