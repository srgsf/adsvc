package main

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/srgsf/adsvc/internal/admin"
	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/config"
	"github.com/srgsf/adsvc/internal/proxy"
)

// reloader re-reads the config file for SIGHUP and the admin page's button, one at a
// time.
type reloader struct {
	mu     sync.Mutex
	pf     *proxyFlags
	px     *proxy.Proxy
	listen string
	onNode func(node) error
}

func (rl *reloader) reload() ([]string, error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	restart, err := rl.pf.reload(rl.px, rl.listen, rl.onNode)
	result := "ok"
	if err != nil {
		result = "invalid"
	}
	configReloads.With(result).Inc()
	return restart, err
}

// newAdmin is the web page of the proxy (package admin), at the root.
func newAdmin(ctx context.Context, pf *proxyFlags, px *proxy.Proxy, rl *reloader) (*admin.Admin, error) {
	d := admin.Deps{
		DB:  px.DB,
		Lib: px.Lib,
		Users: func() []admin.User {
			var users []admin.User
			for _, u := range px.Config().Users {
				users = append(users, admin.User{Name: u.Name, Tokens: u.Tokens, Admin: u.Admin})
			}
			return users
		},
		PublicURL: func() string {
			if f := pf.applied.Load(); f != nil {
				return f.PublicURL
			}
			return ""
		},
		MinScore: func() int { return px.Config().MinScore },
		Version:  version,
		Config: func() ([]byte, error) {
			return pf.effective(pf.applied.Load(), px.Config(), rl.listen, px.DB.Policy())
		},
		Peers: func() []string {
			var names []string
			for _, p := range pf.applied.Load().Sync.Peers {
				names = append(names, p.Name)
			}
			return names
		},
	}
	d.Reload = rl.reload // without a config file it says so
	return admin.New(ctx, d)
}

// redacted stands for a secret in the effective config.
const redacted = "<redacted>"

// effective is the configuration in effect, as a config file: the file merged with flags,
// environment and defaults. Secrets (keys, credentials in URLs) are redacted.
func (pf *proxyFlags) effective(file *config.Proxy, cfg proxy.Config, listen string, pol catalog.Policy) ([]byte, error) {
	if file == nil {
		file = &config.Proxy{}
	}
	c := *file
	c.Listen, c.DataDir, c.FFmpeg = listen, cfg.DataDir, cfg.FFmpeg.FFmpegPath
	c.Media = config.Media{Dir: pf.mediaRun.dir, Listen: pf.mediaRun.listen}
	c.Log = config.Log{Level: strings.ToLower(logLevel.Level().String()), Format: cmp.Or(file.Log.Format, "text")}
	c.Auth = config.Auth{}
	n := 0
	for _, u := range cfg.Users {
		if c.Auth.Users == nil {
			c.Auth.Users = map[string]config.User{}
		}
		tokens := make([]string, len(u.Tokens))
		for i := range tokens {
			n++ // numbered: a token may belong to one user only
			tokens[i] = fmt.Sprintf("<redacted-%d>", n)
		}
		c.Auth.Users[u.Name] = config.User{Tokens: tokens, Admin: u.Admin}
	}
	routes := file.Upstream.Routes
	if len(pf.routes) > 0 {
		routes = pf.routes
	}
	c.Upstream.Routes = nil
	for _, r := range routes {
		from, to, _ := strings.Cut(r, "=")
		c.Upstream.Routes = append(c.Upstream.Routes, redactURL(from)+"="+redactURL(to))
	}
	if pf.set("allow") {
		c.Upstream.Allow = strings.Split(*pf.allow, ",")
	}
	c.Detect = config.Detect{MinScore: cfg.MinScore, ConfirmScore: cfg.ConfirmScore,
		MarkWindow: cfg.MarkWindow, MaxStall: cfg.MaxStall, IdleTimeout: cfg.IdleTimeout}
	c.Trust = config.Trust{Self: &pol.SelfWeight, Unknown: &pol.UnknownWeight, MinTrust: &pol.MinTrust,
		FileMapMin: &pol.FileMapMin, QuotaPerDay: pol.QuotaPerDay, Origins: file.Trust.Origins}
	c.Tracking = config.Tracking{MaxAds: pol.MaxAds}
	c.Sync.Peers = nil
	for _, p := range file.Sync.Peers {
		if p.Token != "" {
			p.Token = redacted
		}
		p.URL = redactURL(p.URL)
		c.Sync.Peers = append(c.Sync.Peers, p)
	}
	var buf bytes.Buffer
	buf.WriteString("# in effect: flags, environment, the config file, defaults; secrets redacted\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&c); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// redactURL hides the user info of a URL.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = url.User(redacted)
	return u.String()
}
