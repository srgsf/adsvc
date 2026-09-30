package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/config"
	"github.com/srgsf/adsvc/internal/proxy"
)

func proxyFlagsFor(t *testing.T, args ...string) *proxyFlags {
	t.Helper()
	pf := newProxyFlags()
	if err := pf.fl.Parse(args); err != nil {
		t.Fatal(err)
	}
	return pf
}

func writeConfig(t *testing.T, path, yaml string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func routeStrings(rs []proxy.Route) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.From.String()+"="+r.To.String())
	}
	return out
}

func TestProxyConfigPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adsvc.yaml")
	writeConfig(t, path, `
listen: 0.0.0.0:9000
data_dir: /var/lib/adsvc
ffmpeg: /opt/ffmpeg
auth:
  users:
    alice: {tokens: [file-key]}
upstream:
  allow: [file.example]
  routes: [http://pub.example=http://127.0.0.1:1]
detect:
  min_score: 30
  confirm_score: 70
  mark_window: 1m
`)
	pf := proxyFlagsFor(t, "-config", path, "-minscore", "25",
		"-route", "http://a.example=http://127.0.0.1:2")
	file, err := pf.load()
	if err != nil {
		t.Fatal(err)
	}
	cfg, listen, err := pf.build(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what      string
		got, want any
	}{
		{"listen (file)", listen, "0.0.0.0:9000"},
		{"data dir (file)", cfg.DataDir, "/var/lib/adsvc"},
		{"ffmpeg (file)", cfg.FFmpeg.FFmpegPath, "/opt/ffmpeg"},
		{"min score (flag)", cfg.MinScore, 25},
		{"confirm score (file)", cfg.ConfirmScore, 70},
		{"mark window (file)", cfg.MarkWindow, time.Minute},
		{"users (file)", fmt.Sprint(cfg.Users), "[{alice [file-key] false}]"},
		{"routes (flag replaces)", strings.Join(routeStrings(cfg.Routes), ","), "http://a.example=http://127.0.0.1:2"},
	} {
		if c.got != c.want {
			t.Errorf("%s: %v, want %v", c.what, c.got, c.want)
		}
	}
	allowed := func(raw string) bool {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.AllowUpstream(u)
	}
	if cfg.AllowUpstream == nil || !allowed("http://FILE.example/x") || allowed("http://other.example/x") {
		t.Errorf("allow-list from the file not applied")
	}
}

func TestProxyConfigWithoutFile(t *testing.T) {
	t.Setenv("ADSVC_CONFIG", "")
	t.Chdir(t.TempDir()) // no config.yml here
	pf := proxyFlagsFor(t)
	file, err := pf.load()
	if err != nil {
		t.Fatal(err)
	}
	cfg, listen, err := pf.build(file)
	if err != nil {
		t.Fatal(err)
	}
	// Detection settings stay zero: proxy.Config's defaults fill them in.
	if listen != defaultListen || cfg.DataDir != defaultDataDir || cfg.FFmpeg.FFmpegPath == "" ||
		cfg.MinScore != 0 || cfg.MarkWindow != 0 || cfg.Users != nil || cfg.AllowUpstream != nil {
		t.Errorf("defaults changed: listen %s, %+v", listen, cfg)
	}
	if _, err := pf.reload(nil, listen, nil); err == nil {
		t.Error("reload without a config file must fail")
	}
	// An empty -data-dir given on the command line keeps everything in memory.
	if cfg, _, err := proxyFlagsFor(t, "-data-dir", "").build(file); err != nil || cfg.DataDir != "" {
		t.Errorf("-data-dir \"\": data dir %q, %v", cfg.DataDir, err)
	}
}

// config.yml in the current directory is the config file when none is named.
func TestProxyDefaultConfig(t *testing.T) {
	t.Setenv("ADSVC_CONFIG", "")
	dir := t.TempDir()
	t.Chdir(dir)
	writeConfig(t, filepath.Join(dir, "config.yml"), "detect:\n  min_score: 33\n")
	pf := proxyFlagsFor(t)
	if pf.path() != "config.yml" {
		t.Fatalf("path %q, want config.yml", pf.path())
	}
	file, err := pf.load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _, err := pf.build(file); err != nil || cfg.MinScore != 33 {
		t.Fatalf("config.yml not read: min score %d, %v", cfg.MinScore, err)
	}
	// A file named explicitly wins, and must exist.
	pf = proxyFlagsFor(t, "-config", "missing.yml")
	if _, err := pf.load(); err == nil {
		t.Fatal("a missing -config file must be an error")
	}
	t.Setenv("ADSVC_CONFIG", "other.yml")
	if pf := proxyFlagsFor(t); pf.path() != "other.yml" {
		t.Fatalf("$ADSVC_CONFIG ignored: %q", pf.path())
	}
}

func TestProxyReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adsvc.yaml")
	base := "data_dir: " + dir + "\nlisten: 127.0.0.1:8080\n"
	writeConfig(t, path, base+"auth:\n  users: {a: {tokens: [one]}}\n")
	pf := proxyFlagsFor(t, "-config", path)
	file, err := pf.load()
	if err != nil {
		t.Fatal(err)
	}
	cfg, listen, err := pf.build(file)
	if err != nil {
		t.Fatal(err)
	}
	px, err := proxy.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	writeConfig(t, path, base+"auth:\n  users: {a: {tokens: [two]}, b: {tokens: [adm], admin: true}}\ndetect:\n  confirm_score: 55\n")
	restart, err := pf.reload(px, listen, nil)
	if err != nil || len(restart) != 0 {
		t.Fatalf("reload: restart %v, %v", restart, err)
	}
	if got := px.Config(); fmt.Sprint(got.Users) != "[{a [two] false} {b [adm] true}]" || got.ConfirmScore != 55 {
		t.Fatalf("after reload: users %v, confirm %d", got.Users, got.ConfirmScore)
	}

	writeConfig(t, path, base+"auth:\n  users: {a: {tokens: [three]}}\nbogus: 1\n")
	if _, err := pf.reload(px, listen, nil); err == nil {
		t.Fatal("an invalid file must not reload")
	}
	if got := px.Config(); fmt.Sprint(got.Users) != "[{a [two] false} {b [adm] true}]" {
		t.Fatalf("an invalid file changed the users to %v", got.Users)
	}

	writeConfig(t, path, "listen: 127.0.0.1:9999\ndata_dir: /elsewhere\n")
	restart, err = pf.reload(px, listen, nil)
	slices.Sort(restart)
	if err != nil || !slices.Equal(restart, []string{"DataDir", "listen"}) {
		t.Fatalf("restart-only changes: %v, %v", restart, err)
	}
	if got := px.Config(); got.DataDir != dir || got.Users != nil {
		t.Fatalf("after reload: DataDir %s (want unchanged), users %v (want none)", got.DataDir, got.Users)
	}
}

func TestProxyDataDirFlag(t *testing.T) {
	pf := proxyFlagsFor(t, "-data-dir", "/srv/adsvc")
	file := &config.Proxy{DataDir: "/from/file"}
	cfg, _, err := pf.build(file)
	if err != nil || cfg.DataDir != "/srv/adsvc" {
		t.Fatalf("-data-dir over the file: %q, %v", cfg.DataDir, err)
	}
}

func TestListenURLs(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080", "localhost:8080": "http://localhost:8080",
		"192.168.1.5:80": "http://192.168.1.5:80", "[::1]:8080": "http://[::1]:8080",
		":8080": "http://localhost:8080", "0.0.0.0:8080": "http://localhost:8080", "[::]:8080": "http://localhost:8080",
	} {
		urls := listenURLs(listen)
		if len(urls) == 0 || urls[0] != want {
			t.Errorf("%s: %v, want %s first", listen, urls, want)
		}
		wildcard := strings.HasPrefix(listen, ":") || strings.HasPrefix(listen, "0.0.0.0") || strings.HasPrefix(listen, "[::]")
		if !wildcard && len(urls) != 1 {
			t.Errorf("%s: %v, want only the listen address", listen, urls)
		}
		for _, u := range urls {
			if strings.Contains(u, "//:") || strings.Contains(u, "0.0.0.0") {
				t.Errorf("%s: unusable URL %s", listen, u)
			}
		}
	}
}

// A new token is valid config, and a user who gets it by a reload can log in with it.
func TestTokenByReload(t *testing.T) {
	tok := newToken()
	if len(tok) < 40 || newToken() == tok {
		t.Fatalf("token %q: too short or not random", tok)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "adsvc.yaml")
	base := "data_dir: " + dir + "\nauth:\n  users:\n    alice: {tokens: [old], admin: true}\n"
	writeConfig(t, path, base)
	pf := proxyFlagsFor(t, "-config", path)
	file, err := pf.load()
	if err != nil {
		t.Fatal(err)
	}
	cfg, listen, err := pf.build(file)
	if err != nil {
		t.Fatal(err)
	}
	px, err := proxy.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	valid := func() bool {
		r := httptest.NewRequest(http.MethodGet, "/ads/library", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return px.Authorized(r)
	}
	if valid() {
		t.Fatal("a token not in the config is valid")
	}
	writeConfig(t, path, base+"    tv: {tokens: ["+tok+"]}\n    bob: {tokens: [b], admin: true}\n")
	if _, err := pf.reload(px, listen, nil); err != nil {
		t.Fatal(err)
	}
	if !valid() {
		t.Error("after reload: the token is not valid")
	}
}
