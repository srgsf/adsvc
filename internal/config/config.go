// Package config is the YAML configuration file of `adsvc proxy`.
//
// Every key is optional: an absent key keeps the command's default, and a flag given on
// the command line wins over the file. Unknown keys are errors, so that a typo does not
// silently keep a default. The file holds secrets (tokens, passwords): keep it private.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/srgsf/adsvc/internal/record"
)

// Proxy is the config file of `adsvc proxy`.
type Proxy struct {
	Listen string `yaml:"listen"`
	// PublicURL is the base URL clients reach adsvc at (behind a reverse proxy, or by a
	// LAN address), for the play links of the Reports page; unset, the request's host.
	PublicURL string   `yaml:"public_url"`
	DataDir   string   `yaml:"data_dir"` // holds catalogue.db and tracking.csr
	FFmpeg    string   `yaml:"ffmpeg"`
	Log       Log      `yaml:"log"`
	Auth      Auth     `yaml:"auth"`
	Upstream  Upstream `yaml:"upstream"`
	Media     Media    `yaml:"media"`
	Metrics   Metrics  `yaml:"metrics"`
	Detect    Detect   `yaml:"detect"`
	Trust     Trust    `yaml:"trust"`
	Tracking  Tracking `yaml:"tracking"`
	Sync      Sync     `yaml:"sync"`
}

// Trust is this node's opinion of other nodes (docs/database.md §4). Unset keys keep the
// defaults (self 10, unknown 0.2, min_trust 1, file_map_min 1, quota_per_day 5000).
type Trust struct {
	Self        *float64          `yaml:"self"`
	Unknown     *float64          `yaml:"unknown"`
	MinTrust    *float64          `yaml:"min_trust"`
	FileMapMin  *float64          `yaml:"file_map_min"`
	QuotaPerDay int               `yaml:"quota_per_day"`
	Origins     map[string]Origin `yaml:"origins"` // node id (hex) -> opinion
}

// Origin is the opinion of one node.
type Origin struct {
	Name    string   `yaml:"name"`
	Weight  *float64 `yaml:"weight"` // unset = the unknown weight
	Blocked bool     `yaml:"blocked"`
}

// Tracking bounds the set of ads the index holds.
type Tracking struct {
	MaxAds int `yaml:"max_ads"` // 0 = the index's limit
}

// Sync configures federation (package replica).
type Sync struct {
	Name   string `yaml:"name"`   // advertised to peers
	Serve  *bool  `yaml:"serve"`  // serve /sync/* (default true)
	Public bool   `yaml:"public"` // /sync/* without a token (the catalogue holds no secrets)
	Share  Share  `yaml:"share"`
	Peers  []Peer `yaml:"peers"`
}

// Share says what this node publishes to peers; every kind is shared by default.
type Share struct {
	Ads      *bool `yaml:"ads"`
	Labels   *bool `yaml:"labels"`
	Votes    *bool `yaml:"votes"`
	FileMaps *bool `yaml:"file_maps"`
}

// Peer is a node to sync with.
type Peer struct {
	Name     string        `yaml:"name"`
	URL      string        `yaml:"url"`      // its adsvc base URL
	Token    string        `yaml:"token"`    // a user token of this node on the peer, if its /sync/* needs one
	Mode     string        `yaml:"mode"`     // pull, push or both (default)
	Interval time.Duration `yaml:"interval"` // default 10m
}

// PullPush is what Mode asks for.
func (p Peer) PullPush() (pull, push bool) {
	switch p.Mode {
	case "pull":
		return true, false
	case "push":
		return false, true
	}
	return true, true
}

// Log configures the default logger.
type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn or error
	Format string `yaml:"format"` // text or json
}

// Auth is adsvc's own access control (never forwarded upstream).
type Auth struct {
	// Users by name. A request carries `Authorization: Bearer TOKEN`, or NAME:TOKEN as the
	// user info of the adsvc URL. Without a user that has a token, adsvc is open.
	Users map[string]User `yaml:"users"`
}

// User is one of adsvc's own users.
type User struct {
	Tokens []string `yaml:"tokens"`
	Admin  bool     `yaml:"admin"` // sees every page of the web page, not only Reports
}

// Upstream restricts and rewrites the sources adsvc proxies.
type Upstream struct {
	Allow  []string `yaml:"allow"`  // host names; empty = any host, or only routed ones
	Routes []string `yaml:"routes"` // FROM=TO, as the -route flag
}

// Media serves a local folder for the proxy to play (package media).
type Media struct {
	Dir    string `yaml:"dir"`    // empty = off
	Listen string `yaml:"listen"` // loopback only (default 127.0.0.1:8000)
}

// Metrics serves Prometheus metrics on a listener of its own (package metrics).
type Metrics struct {
	Listen string `yaml:"listen"` // empty = off; no host = localhost
}

// Detect tunes detection and sessions.
type Detect struct {
	MinScore     int           `yaml:"min_score"`
	ConfirmScore int           `yaml:"confirm_score"`
	MarkWindow   time.Duration `yaml:"mark_window"`
	MaxStall     time.Duration `yaml:"max_stall"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
}

// Load reads and validates the config file at path.
func Load(path string) (*Proxy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// Parse decodes and validates a config file. An empty file is an empty config.
func Parse(b []byte) (*Proxy, error) {
	c := &Proxy{}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one YAML document")
	}
	return c, c.validate()
}

func (c *Proxy) validate() error {
	var errs []error
	if c.Log.Level != "" {
		var l slog.Level
		if err := l.UnmarshalText([]byte(c.Log.Level)); err != nil {
			errs = append(errs, fmt.Errorf("log.level: %w", err))
		}
	}
	if f := strings.ToLower(c.Log.Format); f != "" && f != "text" && f != "json" {
		errs = append(errs, fmt.Errorf("log.format %q: want text or json", c.Log.Format))
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, fmt.Errorf("public_url %q: want http(s)://host[:port][/path]", c.PublicURL))
		}
	}
	if l := c.Metrics.Listen; l != "" {
		if _, port, err := net.SplitHostPort(l); err != nil || port == "" || port == "0" {
			errs = append(errs, fmt.Errorf("metrics.listen %q: want [host]:port with a fixed port", l))
		}
	}
	errs = append(errs, c.Auth.validate()...)
	errs = append(errs, c.validateNode()...)
	d := c.Detect
	if d.MinScore < 0 || d.ConfirmScore < 0 {
		errs = append(errs, errors.New("detect: scores must not be negative"))
	}
	if d.MarkWindow < 0 || d.MaxStall < 0 || d.IdleTimeout < 0 {
		errs = append(errs, errors.New("detect: durations must not be negative"))
	}
	return errors.Join(errs...)
}

// validate checks that names and tokens work in a URL's user info (NAME:TOKEN@host) as
// they are, and that a token names one user.
func (a Auth) validate() []error {
	var errs []error
	owner := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(a.Users)) {
		if !validName(name) {
			errs = append(errs, fmt.Errorf("auth.users: name %q: use letters, digits, '.', '_' or '-'", name))
		}
		for i, t := range a.Users[name].Tokens {
			switch {
			case t == "" || strings.ContainsAny(t, ":@/?#%") || strings.ContainsFunc(t, notPrintable):
				// they would have to be escaped in NAME:TOKEN@host
				errs = append(errs, fmt.Errorf("auth.users.%s.tokens[%d]: empty, or has a space or one of \":@/?#%%\"", name, i))
			case owner[t] != "":
				errs = append(errs, fmt.Errorf("auth.users.%s.tokens[%d]: also a token of %s", name, i, owner[t]))
			default:
				owner[t] = name
			}
		}
	}
	return errs
}

// validName: ASCII letters, digits, '.', '_' and '-'.
func validName(s string) bool {
	ok := func(r rune) bool {
		return r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) || r == '.' || r == '_' || r == '-'
	}
	for _, r := range s {
		if !ok(r) {
			return false
		}
	}
	return s != ""
}

func notPrintable(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

func (c *Proxy) validateNode() []error {
	var errs []error
	t := c.Trust
	// In a fixed order, like every list here: the joined error (and the SIGHUP warning
	// quoting it) must not change between runs.
	for _, f := range []struct {
		name string
		v    *float64
	}{{"self", t.Self}, {"unknown", t.Unknown}, {"min_trust", t.MinTrust}, {"file_map_min", t.FileMapMin}} {
		if f.v != nil && *f.v < 0 {
			errs = append(errs, fmt.Errorf("trust.%s: negative", f.name))
		}
	}
	if t.QuotaPerDay < 0 || c.Tracking.MaxAds < 0 {
		errs = append(errs, errors.New("trust.quota_per_day and tracking.max_ads must not be negative"))
	}
	for _, id := range slices.Sorted(maps.Keys(t.Origins)) {
		o := t.Origins[id]
		if _, err := record.ParseOrigin(id); err != nil {
			errs = append(errs, fmt.Errorf("trust.origins: %w", err))
		}
		if o.Weight != nil && *o.Weight < 0 {
			errs = append(errs, fmt.Errorf("trust.origins.%s: negative weight", id))
		}
	}
	names := map[string]bool{}
	for i, p := range c.Sync.Peers {
		u, err := url.Parse(p.URL)
		switch {
		case p.Name == "" || names[p.Name]:
			errs = append(errs, fmt.Errorf("sync.peers[%d]: a unique name is required", i))
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil:
			errs = append(errs, fmt.Errorf("sync.peers[%d] %s: url must be an http(s) URL without credentials (use token)", i, p.Name))
		case p.Mode != "" && p.Mode != "pull" && p.Mode != "push" && p.Mode != "both":
			errs = append(errs, fmt.Errorf("sync.peers[%d] %s: mode %q: want pull, push or both", i, p.Name, p.Mode))
		case p.Interval != 0 && p.Interval < time.Minute:
			errs = append(errs, fmt.Errorf("sync.peers[%d] %s: interval under a minute", i, p.Name))
		}
		names[p.Name] = true
	}
	return errs
}
