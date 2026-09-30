// Package admin is the web page of adsvc (docs/database.md §9), served at the root next to
// the proxy's routes. Every user reports ads there: the Reports page lists the user's own
// reports with a play link each for marking them in mpv. Admins also get ads, files,
// origins, peers, the tracking index and the config. Pages are rendered on the server
// from catalogue queries with html/template; htmx swaps the fragments that forms and
// filters ask for. The same handlers serve JSON under /api/.
//
// Any user's token (auth.users) gets in: the login form trades a token for a session
// cookie. Requests that change something also need the X-Adsvc-Admin header (htmx sends
// it) and must come from the page's own origin.
package admin

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"sync"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/library"
)

// Deps is what the admin page works on.
type Deps struct {
	DB  *catalog.DB
	Lib *library.Library
	// Users returns the users in effect; with no token among them the page is off.
	Users func() []User
	// Config returns the effective configuration as YAML, secrets redacted.
	Config func() ([]byte, error)
	// Reload re-reads the config file as SIGHUP does, and returns what changed but
	// needs a restart.
	Reload func() (restart []string, err error)
	// Peers returns the names of the configured peers.
	Peers func() []string
	// MinScore returns the detection threshold (detect.min_score), to judge ad quality by.
	MinScore func() int
	// Version is adsvc's version, shown in the header.
	Version string
	// PublicURL is the base URL clients reach adsvc at, for the play links of the Reports
	// page; "" takes the scheme and host of the request.
	PublicURL func() string
}

// User is one of adsvc's users (auth.users).
type User struct {
	Name   string
	Tokens []string
	Admin  bool // sees every page and everyone's reports
}

// Admin serves the admin page.
type Admin struct {
	ctx     context.Context // index rebuilds run in it, not in a request's
	d       Deps
	sess    *sessions
	pages   map[string]*template.Template
	assets  map[string]string // file under static/ -> hash of its contents
	tr      map[string]map[string]string
	mux     *http.ServeMux
	h       http.Handler // mux behind net/http's cross-origin protection, which every route passes
	loginMu sync.Mutex   // failed logins wait their turn
	quals   qualities    // Lib.Quality per ad, for possible duplicates
}

// New builds the admin page over d. Work it starts (index rebuilds) runs in ctx.
func New(ctx context.Context, d Deps) (*Admin, error) {
	if d.DB == nil || d.Lib == nil || d.Users == nil {
		return nil, errors.New("admin: DB, Lib and Users are required")
	}
	a := &Admin{ctx: ctx, d: d, sess: newSessions(), mux: http.NewServeMux()}
	a.h = http.NewCrossOriginProtection().Handler(a.mux)
	if err := a.load(); err != nil {
		return nil, err
	}
	a.routes()
	return a, nil
}

func (a *Admin) routes() {
	m := a.mux
	m.Handle("GET /static/", a.staticHandler())
	m.HandleFunc("GET /login", a.loginForm)
	m.HandleFunc("POST /login", a.login)
	m.Handle("POST /logout", a.guard(a.logout, false))
	m.Handle("GET /{$}", a.guard(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, home(viewerOf(r)), http.StatusSeeOther)
	}, false))
	// Every page and action, also as JSON under /api.
	for _, rt := range []struct {
		method, path string
		h            http.HandlerFunc
		admin        bool
	}{
		{"GET", "/reports", a.reports, false},
		{"GET", "/reports/adskip.lua", a.script, false},
		{"POST", "/reports/{id}/delete", a.reportDelete, false},
		{"GET", "/ads", a.ads, true},
		{"POST", "/ads/bulk", a.adsBulk, true},
		{"GET", "/ads/dups", a.adsDups, true},
		{"GET", "/ads/{id}", a.ad, true},
		{"GET", "/ads/{id}/quality", a.adQuality, true},
		{"POST", "/ads/{id}/label", a.adLabel, true},
		{"POST", "/ads/{id}/vote", a.adVote, true},
		{"POST", "/ads/{id}/pin", a.adPin, true},
		{"POST", "/ads/{id}/dup", a.adDup, true},
		{"POST", "/ads/{id}/delete", a.adDelete, true},
		{"GET", "/files", a.files, true},
		{"GET", "/file", a.file, true},
		{"GET", "/origins", a.origins, true},
		{"POST", "/origins/{origin}", a.originSet, true},
		{"GET", "/peers", a.peers, true},
		{"GET", "/tracking", a.tracking, true},
		{"POST", "/tracking/rebuild", a.rebuild, true},
		{"GET", "/config", a.config, true},
		{"POST", "/config/reload", a.reload, true},
	} {
		h := a.guard(rt.h, rt.admin)
		m.Handle(rt.method+" "+rt.path, h)
		m.Handle(rt.method+" /api"+rt.path, h)
	}
}

// home is where a user starts: admins at the ads, everyone else at their reports.
func home(v viewer) string {
	if v.Admin {
		return "/ads"
	}
	return "/reports"
}

// ServeHTTP serves the page: everything the proxy does not (proxy.APIPath).
func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
		"frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
	a.h.ServeHTTP(w, r)
}

// refresh brings the ad index up to date with a change of trust. It runs in the admin's
// context: a request that goes away must not leave the index behind.
func (a *Admin) refresh() error { return a.d.Lib.Refresh(a.ctx) }
