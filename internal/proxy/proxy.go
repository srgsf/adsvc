// Package proxy is the pass-through proxy: the player streams through adsvc, and adsvc
// analyses exactly the bytes it forwards. No second connection to the source is opened, so
// P2P/debrid sources and TorrServer's cache see the same access pattern as without ad
// detection.
//
//	player ──GET /s?u=<upstream>&ih=<infohash>&idx=<n>──► adsvc ──GET <upstream>──► source
//	                                                        │ same bytes
//	                                                        ▼
//	                       container demuxer ─frames─► ffmpeg ─PCM─► fingerprint ─► matcher
//
// The client composes that URL from the stream URL it would play anyway: only the client
// knows how it reaches both the source and adsvc (see docs/handoff.md). Sources need no
// changes and never learn about adsvc.
package proxy

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srgsf/adsvc/internal/admap"
	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/ffmpeg"
	"github.com/srgsf/adsvc/internal/library"
	"github.com/srgsf/adsvc/internal/store"
)

// Config configures the pass-through proxy.
type Config struct {
	FFmpeg ffmpeg.Tools
	// DataDir holds the catalogue (catalogue.db) and the ad index (tracking.csr). Empty
	// keeps everything in memory.
	DataDir string

	MinScore     int           // match threshold (default detect.DefaultMinScore)
	ConfirmScore int           // score needed to confirm from two blocks (default detect.DefaultConfirmScore)
	MarkWindow   time.Duration // decoded audio kept for /ads/mark (default DefaultMarkWindow)
	// MaxStall is how long a forwarded chunk may wait for the analyser before it is
	// dropped instead (default DefaultMaxStall). Analysis runs far faster than playback,
	// so waiting only happens when a player downloads in a burst.
	MaxStall    time.Duration
	IdleTimeout time.Duration // a session unused this long is closed (default DefaultIdleTimeout)
	Client      *http.Client
	// AllowUpstream may reject upstream URLs that no route matches. Without it the proxy
	// is open to any host it can reach unless Routes are set, which then act as the
	// allow-list.
	AllowUpstream func(*url.URL) bool
	// Routes rewrite the upstream URL as the client knows it into the one adsvc should use,
	// e.g. a public TorrServer address into its loopback address when both run on one host.
	Routes []Route
	// Users protect every route but /healthz once one of them has a token: a request
	// carries `Authorization: Bearer TOKEN`, or, from players that can only be given a URL,
	// NAME:TOKEN as the user info of the adsvc URL (sent as Basic auth). The client's
	// Authorization header is then adsvc's own and is never forwarded upstream; upstream
	// credentials go in X-Upstream-Authorization, in the user info of u, or in u itself.
	Users []User
	// Version is adsvc's version, which /healthz answers with.
	Version string
}

// User is one of adsvc's own users.
type User struct {
	Name   string
	Tokens []string // any of them authenticates the user
	Admin  bool     // its tokens also log in to the admin page (package admin)
}

// Defaults of Config (the zero value of a field means its default).
const (
	DefaultMarkWindow  = 5 * time.Minute
	DefaultMaxStall    = 2 * time.Second
	DefaultIdleTimeout = 10 * time.Minute
)

func (c *Config) defaults() {
	c.MinScore = cmp.Or(c.MinScore, detect.DefaultMinScore)
	c.ConfirmScore = cmp.Or(c.ConfirmScore, detect.DefaultConfirmScore(c.MinScore))
	c.MarkWindow = cmp.Or(c.MarkWindow, DefaultMarkWindow)
	c.MaxStall = cmp.Or(c.MaxStall, DefaultMaxStall)
	c.IdleTimeout = cmp.Or(c.IdleTimeout, DefaultIdleTimeout)
	if c.Client == nil {
		c.Client = &http.Client{Timeout: 0} // streaming: no overall deadline
	}
}

// Proxy forwards media to the player and analyses it on the way through.
type Proxy struct {
	cfg   atomic.Pointer[Config] // replaced whole by Reload, never modified
	st    *store.Store
	owned bool // st was opened by New, and is closed by Close
	DB    *catalog.DB
	Lib   *library.Library
	Maps  *admap.Store

	// ctx is the lifetime of the proxy (the process's signal context): sessions and
	// their decoders derive from it, and its end shuts the proxy down.
	ctx       context.Context
	stopWatch func() bool // stops the shutdown-on-ctx watch
	closeOnce sync.Once
	closeErr  error

	mu          sync.Mutex
	sess        map[string]*session
	closing     bool          // no new sessions
	stop        chan struct{} // ends the janitor
	janitorDone chan struct{}
}

// New creates a proxy that lives as long as ctx: it opens the catalogue and the ad index,
// and starts the janitor that closes idle sessions and stores the ad maps of open ones
// every 15 s. When ctx ends, the proxy shuts itself down (see Close).
func New(ctx context.Context, cfg Config) (*Proxy, error) {
	cfg.defaults()
	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	p := NewWith(ctx, cfg, st)
	p.owned = true
	return p, nil
}

// NewWith creates a proxy on an open store (cfg.DataDir is not used), living as long as
// ctx. Close leaves the store open.
func NewWith(ctx context.Context, cfg Config, st *store.Store) *Proxy {
	cfg.defaults()
	st.Lib.ExportMetrics()
	st.DB.ExportMetrics()
	p := &Proxy{st: st, DB: st.DB, Lib: st.Lib, Maps: st.Maps, ctx: ctx, sess: map[string]*session{},
		stop: make(chan struct{}), janitorDone: make(chan struct{})}
	p.cfg.Store(&cfg)
	go p.janitor()
	p.stopWatch = context.AfterFunc(ctx, func() {
		slog.Info("shutting down, storing ad maps")
		if err := p.Close(); err != nil {
			slog.Error("shutting down the proxy", "err", err)
		}
	})
	return p
}

// conf is the current configuration. Callers must not modify it.
func (p *Proxy) conf() *Config { return p.cfg.Load() }

// Config returns the current configuration, with defaults applied.
func (p *Proxy) Config() Config { return *p.conf() }

// Reload replaces the configuration. Requests and sessions pick up the new values as they
// read them: auth, routes and the allow-list on the next request, MaxStall, IdleTimeout
// and ConfirmScore right away, MinScore, MarkWindow and FFmpeg for new sessions. DataDir
// keeps its current value; the names of fields that did change and need a restart are
// returned. A nil Client keeps the current one (and its connections).
func (p *Proxy) Reload(cfg Config) (restart []string) {
	cur := p.conf()
	if cfg.DataDir != cur.DataDir {
		restart = append(restart, "DataDir")
		cfg.DataDir = cur.DataDir
	}
	if cfg.Client == nil {
		cfg.Client = cur.Client
	}
	cfg.defaults()
	p.cfg.Store(&cfg)
	return restart
}

// Close shuts the proxy down: no new sessions, open ones end (their ad maps are stored),
// and the store opened by New is closed. It runs by itself when the proxy's context ends;
// calling it again, or concurrently, waits for that shutdown and returns its result.
// Requests still in flight are forwarded without analysis.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() { p.closeErr = p.shutdown() })
	return p.closeErr
}

func (p *Proxy) shutdown() error {
	p.stopWatch()
	p.mu.Lock()
	p.closing = true
	all := make([]*session, 0, len(p.sess))
	for _, s := range p.sess {
		all = append(all, s)
	}
	p.sess = map[string]*session{}
	p.mu.Unlock()
	close(p.stop)
	<-p.janitorDone // it may be storing a map right now
	for _, s := range all {
		s.close("shutdown")
	}
	if !p.owned {
		return nil
	}
	return p.st.Close()
}

func (p *Proxy) janitor() {
	defer close(p.janitorDone)
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.mu.Lock()
			var idle, live []*session
			for id, s := range p.sess {
				if s.idleFor() > p.conf().IdleTimeout {
					idle = append(idle, s)
					delete(p.sess, id)
				} else {
					live = append(live, s)
				}
			}
			p.mu.Unlock()
			for _, s := range live { // writes: not under p.mu, which every request takes
				s.store()
			}
			for _, s := range idle {
				slog.Info("session idle, closing", "session", s.id)
				s.close("idle")
			}
		}
	}
}
