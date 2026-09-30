package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srgsf/adsvc/internal/config"
	"github.com/srgsf/adsvc/internal/proxy"
)

func TestEffectiveConfigRedacted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adsvc.yaml")
	writeConfig(t, path, "data_dir: "+dir+"\nauth:\n  users:\n    alice: {tokens: [filetoken, secondtoken], admin: true}\n    tv: {tokens: [tvtoken]}\n"+
		"upstream:\n  routes: [\"http://user:routepw@ts.example=http://127.0.0.1:8090\"]\n"+
		"detect:\n  min_score: 25\n"+
		"sync:\n  peers:\n    - {name: friend, url: \"https://friend.example/adsvc\", token: peerkey}\n")
	pf := proxyFlagsFor(t, "-config", path)
	file, err := pf.load()
	if err != nil {
		t.Fatal(err)
	}
	cfg, listen, err := pf.build(file)
	if err != nil {
		t.Fatal(err)
	}
	n, err := nodeSettings(file)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pf.effective(file, cfg, listen, n.policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"filetoken", "secondtoken", "tvtoken", "routepw", "peerkey"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("secret %q in the effective config:\n%s", secret, b)
		}
	}
	// What is shown is a config file again, with the effective values.
	got, err := config.Parse(b)
	if err != nil {
		t.Fatalf("effective config does not parse: %v\n%s", err, b)
	}
	if got.Detect.MinScore != 25 || len(got.Auth.Users["alice"].Tokens) != 2 || !got.Auth.Users["alice"].Admin || got.Auth.Users["tv"].Admin || *got.Trust.Self != 10 ||
		got.Listen != "localhost:8080" || got.Sync.Peers[0].URL != "https://friend.example/adsvc" {
		t.Errorf("effective config: %+v", got)
	}
}

func TestAdminMounted(t *testing.T) {
	dir := t.TempDir()
	pf := proxyFlagsFor(t, "-data-dir", dir)
	file := &config.Proxy{Auth: config.Auth{Users: map[string]config.User{
		"alice": {Tokens: []string{"adm"}, Admin: true}, "tv": {Tokens: []string{"k"}}}}}
	pf.applied.Store(file)
	cfg, listen, err := pf.build(file)
	if err != nil {
		t.Fatal(err)
	}
	px, err := proxy.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	adm, err := newAdmin(t.Context(), pf, px, &reloader{pf: pf, px: px, listen: listen})
	if err != nil {
		t.Fatal(err)
	}
	front := &frontHandler{px: px, proxyH: px.Handler(), adminH: adm}
	for path, want := range map[string]int{
		"/login":         http.StatusOK,           // the page's own auth, not a user token
		"/ads":           http.StatusSeeOther,     // to the login form
		"/reports":       http.StatusSeeOther,     // to the login form
		"/":              http.StatusSeeOther,     // to the login form
		"/ads/library":   http.StatusUnauthorized, // the proxy's API still wants a token
		"/ads/reports":   http.StatusUnauthorized,
		"/ads/reports/1": http.StatusUnauthorized,
		"/s":             http.StatusUnauthorized,
		"/healthz":       http.StatusOK,
		"/ads/0123abcd":  http.StatusSeeOther, // an ad page, not the proxy's
	} {
		rec := httptest.NewRecorder()
		front.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
}
