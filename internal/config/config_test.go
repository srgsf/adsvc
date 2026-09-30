package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExampleConfig(t *testing.T) {
	c, err := Load("../../config.yml.example")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "localhost:8080" || c.DataDir != "/data/adsvc" || c.Log.Format != "text" {
		t.Errorf("top level: %+v", c)
	}
	if len(c.Auth.Users) != 2 || !c.Auth.Users["alice"].Admin || len(c.Auth.Users["tv-box"].Tokens) != 1 {
		t.Errorf("auth: %+v", c.Auth)
	}
	if len(c.Upstream.Allow) != 1 || len(c.Upstream.Routes) != 1 {
		t.Errorf("upstream: %+v", c.Upstream)
	}
	if c.Metrics.Listen != "localhost:9464" {
		t.Errorf("metrics: %+v", c.Metrics)
	}
	want := Detect{MinScore: 20, ConfirmScore: 40, MarkWindow: 5 * time.Minute, MaxStall: 2 * time.Second, IdleTimeout: 10 * time.Minute}
	if c.Detect != want {
		t.Errorf("detect: %+v, want %+v", c.Detect, want)
	}
	if *c.Trust.Unknown != 0.2 || len(c.Trust.Origins) != 2 || c.Tracking.MaxAds != 10000 {
		t.Errorf("trust/tracking: %+v %+v", c.Trust, c.Tracking)
	}
	if len(c.Sync.Peers) != 1 || c.Sync.Peers[0].Token == "" || c.Sync.Peers[0].Interval != 10*time.Minute {
		t.Errorf("sync: %+v", c.Sync)
	}
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		name, yaml, err string // err: substring of the error, "" = valid
	}{
		{"empty", "", ""},
		{"comments only", "# nothing\n", ""},
		{"unknown key", "listen: x\nlisten_addr: y\n", "listen_addr"},
		{"unknown nested key", "detect:\n  minscore: 3\n", "minscore"},
		{"bad duration", "detect:\n  max_stall: soon\n", "line 2"},
		{"bad type", "detect:\n  min_score: many\n", "line 2"},
		{"negative score", "detect:\n  min_score: -1\n", "negative"},
		{"negative duration", "detect:\n  idle_timeout: -1s\n", "negative"},
		{"bad level", "log:\n  level: loud\n", "log.level"},
		{"bad format", "log:\n  format: xml\n", "log.format"},
		{"users", "auth:\n  users:\n    alice: {tokens: [t1, t2], admin: true}\n    tv.box-2_: {tokens: [t3]}\n    off: {}\n", ""},
		{"empty token", "auth:\n  users: {a: {tokens: ['']}}\n", "users.a.tokens[0]"},
		{"token with a colon", "auth:\n  users: {a: {tokens: ['x:y']}}\n", "users.a.tokens[0]"},
		{"token with a space", "auth:\n  users: {a: {tokens: ['x y']}}\n", "users.a.tokens[0]"},
		{"token with an at", "auth:\n  users: {a: {tokens: ['x@y']}}\n", "users.a.tokens[0]"},
		{"shared token", "auth:\n  users: {a: {tokens: [t]}, b: {tokens: [t]}}\n", "also a token of a"},
		{"name with a colon", "auth:\n  users: {'a:b': {tokens: [t]}}\n", "name \"a:b\""},
		{"name with a space", "auth:\n  users: {'a b': {tokens: [t]}}\n", "name"},
		{"unknown user key", "auth:\n  users: {a: {token: t}}\n", "token"},
		{"two documents", "listen: a\n---\nlisten: b\n", "more than one"},
		{"public URL", "public_url: https://ads.example.com/adsvc\n", ""},
		{"public URL without a scheme", "public_url: ads.example.com\n", "public_url"},
		{"public URL with credentials", "public_url: http://u:p@ads.lan\n", "public_url"},
		{"metrics", "metrics:\n  listen: :9464\n", ""},
		{"metrics without a port", "metrics:\n  listen: localhost\n", "metrics.listen"},
		{"metrics on port 0", "metrics:\n  listen: localhost:0\n", "metrics.listen"},
		{"bad origin", "trust:\n  origins:\n    abc: {weight: 1}\n", "trust.origins"},
		{"negative weight", "trust:\n  unknown: -1\n", "trust.unknown"},
		{"peer URL with credentials", "sync:\n  peers: [{name: p, url: 'https://u:p@x'}]\n", "without credentials"},
		{"peer without a name", "sync:\n  peers: [{url: 'https://x'}]\n", "unique name"},
		{"peer mode", "sync:\n  peers: [{name: p, url: 'https://x', mode: sideways}]\n", "mode"},
		{"peer interval", "sync:\n  peers: [{name: p, url: 'https://x', interval: 1s}]\n", "interval"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			switch {
			case c.err == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
				t.Fatalf("error %v, want one mentioning %q", err, c.err)
			}
		})
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load of a missing file: %v", err)
	}
}

func TestLoadNamesTheFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "adsvc.yaml")
	if err := os.WriteFile(p, []byte("bogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("error %v does not name %s", err, p)
	}
}
