package proxy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Route maps upstream URLs under From to the same path under To.
type Route struct{ From, To *url.URL }

// ParseRoute parses "https://ts.example.com=http://127.0.0.1:8090".
func ParseRoute(s string) (Route, error) {
	from, to, ok := strings.Cut(s, "=")
	if !ok {
		return Route{}, fmt.Errorf("route %q: want FROM=TO", s)
	}
	f, err1 := url.Parse(from)
	t, err2 := url.Parse(to)
	if err1 != nil || err2 != nil || f.Host == "" || t.Host == "" {
		return Route{}, fmt.Errorf("route %q: FROM and TO must be absolute URLs", s)
	}
	return Route{From: f, To: t}, nil
}

// apply returns u rewritten by r, or nil when r does not match it.
func (r Route) apply(u *url.URL) *url.URL {
	prefix := strings.TrimSuffix(r.From.Path, "/")
	if u.Scheme != r.From.Scheme || !strings.EqualFold(u.Host, r.From.Host) {
		return nil
	}
	if u.Path != prefix && !strings.HasPrefix(u.Path, prefix+"/") {
		return nil
	}
	c := *u
	c.Scheme, c.Host = r.To.Scheme, r.To.Host
	c.Path = strings.TrimSuffix(r.To.Path, "/") + strings.TrimPrefix(u.Path, prefix)
	c.RawPath = ""
	return &c
}

// resolve applies the routes and the allow-list to the upstream URL the client sent.
func (cfg *Config) resolve(up *url.URL) (*url.URL, bool) {
	for _, rt := range cfg.Routes {
		if u := rt.apply(up); u != nil {
			return u, true
		}
	}
	if cfg.AllowUpstream != nil {
		return up, cfg.AllowUpstream(up)
	}
	return up, len(cfg.Routes) == 0
}

// errNotAllowed is a redirect to a URL the allow-list refuses.
var errNotAllowed = errors.New("upstream not allowed")

// redirect applies the routes and the allow-list to a URL an upstream redirected to: a
// URL under a route's target stays with that (internal) source, anything else is checked
// like the URL the client sent.
func (cfg *Config) redirect(u *url.URL) (*url.URL, bool) {
	for _, rt := range cfg.Routes {
		back := Route{From: rt.To, To: rt.To}
		if back.apply(u) != nil {
			return u, true
		}
	}
	return cfg.resolve(u)
}
