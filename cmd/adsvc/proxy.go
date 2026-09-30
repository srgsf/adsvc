package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/srgsf/adsvc/internal/config"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/ffmpeg"
	"github.com/srgsf/adsvc/internal/proxy"
	"github.com/srgsf/adsvc/internal/replica"
)

// runProxy runs the pass-through proxy: the player streams through it and the same bytes are
// analysed on the way, so the source sees exactly the reads the player makes.
//
// Settings come from flags, then the environment, then the config file (-config,
// $ADSVC_CONFIG, or config.yml in the current directory when it exists), then defaults. SIGHUP re-reads the file and applies what can change
// while running; see proxyFlags.reload. SIGINT/SIGTERM end the root context, which every
// part of the proxy derives from: each of them shuts down on its own.
func runProxy(args []string) error {
	pf := newProxyFlags()
	if err := parse(pf.fl, args, pf.setupLog, false); err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	if *pf.verbose {
		logLevel.Set(slog.LevelDebug)
	}
	file, err := pf.load()
	if err != nil {
		return err
	}
	pf.applied.Store(file)
	if err := pf.applyLog(file); err != nil {
		return err
	}
	if pf.mediaRun, err = pf.media(file); err != nil {
		return err
	}
	if pf.metricsRun, err = pf.metricsListen(file); err != nil {
		return err
	}
	cfg, listen, err := pf.build(file)
	if err != nil {
		return err
	}
	settings, err := nodeSettings(file)
	if err != nil {
		return err
	}
	px, err := proxy.New(ctx, cfg)
	if err != nil {
		return err
	}
	if pf.mediaRun.on() {
		waitMedia, err := startMedia(ctx, pf.mediaRun, listenURLs(listen)[0])
		if err != nil {
			return errors.Join(err, px.Close())
		}
		defer waitMedia()
	}
	if pf.metricsRun != "" {
		waitMetrics, err := startMetrics(ctx, pf.metricsRun)
		if err != nil {
			return errors.Join(err, px.Close())
		}
		defer waitMetrics()
	}
	// Federation: /sync/* next to the proxy, and a loop per configured peer; both end
	// with ctx.
	syncer := replica.NewSyncer(ctx, px.DB, nil, px.Lib.Refresh)
	front := newFrontHandler(px, replica.NewServer(ctx, px.DB, settings.name, cfg.DataDir, px.Lib.Refresh).Handler())
	if err := settings.apply(ctx, px, syncer, front); err != nil {
		return errors.Join(err, px.Close())
	}
	applyNode := func(n node) error { return n.apply(ctx, px, syncer, front) }
	rl := &reloader{pf: pf, px: px, listen: listen, onNode: applyNode}
	adm, err := newAdmin(ctx, pf, px, rl)
	if err != nil {
		return errors.Join(err, px.Close())
	}
	front.adminH = proxy.Instrument("admin", adm)
	// No write timeout: responses are media streams that last as long as playback. Requests
	// derive from ctx, so a signal also ends the streams and their upstream requests.
	srv := &http.Server{Addr: listen, Handler: front, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	slog.Info("proxy listening", "ads", px.Lib.Stats().Ads, "config", pf.path(), "node", px.DB.Self(),
		"peers", len(settings.peers), "urls", listenURLs(listen), "stream", "/s?u=<upstream>[&ih=<infohash>&idx=<n>]")

loop:
	for {
		select {
		case err = <-served: // could not listen
			break loop
		case <-hup:
			switch restart, err := rl.reload(); {
			case err != nil:
				slog.Warn("config reload failed, keeping the current config", "config", pf.path(), "err", err)
			case len(restart) > 0:
				slog.Warn("config reloaded; some changes need a restart", "config", pf.path(), "restart_required", restart)
			default:
				slog.Info("config reloaded", "config", pf.path())
			}
		case <-ctx.Done(): // the proxy is already shutting down on its own
			err = shutdown(ctx, srv)
			break loop
		}
	}
	// Waits for the proxy's shutdown (or does it, if the server could not listen).
	stop() // ends the sync loops too, if the server could not listen
	syncer.Wait()
	if cerr := px.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("closing the proxy: %w", cerr))
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// frontHandler is what adsvc proxy serves: the proxy's routes (proxy.APIPath), /sync/*
// (package replica), and the web page (package admin, which checks its own sessions) for
// everything else.
type frontHandler struct {
	px     *proxy.Proxy
	proxyH http.Handler
	syncH  http.Handler
	adminH http.Handler
	syncI  http.Handler // serveSync, instrumented (newFrontHandler)
	serve  atomic.Bool
	public atomic.Bool // /sync/* without a token
}

func newFrontHandler(px *proxy.Proxy, syncH http.Handler) *frontHandler {
	f := &frontHandler{px: px, proxyH: px.Handler(), syncH: syncH}
	f.syncI = proxy.Instrument("sync", http.HandlerFunc(f.serveSync))
	return f
}

func (f *frontHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/sync/") {
		if f.adminH != nil && !proxy.APIPath(r.URL.Path) {
			f.adminH.ServeHTTP(w, r)
		} else {
			f.proxyH.ServeHTTP(w, r)
		}
		return
	}
	f.syncI.ServeHTTP(w, r)
}

func (f *frontHandler) serveSync(w http.ResponseWriter, r *http.Request) {
	switch {
	case !f.serve.Load():
		http.NotFound(w, r)
	case !f.public.Load() && !f.px.Authorized(r):
		proxy.Unauthorized(w)
	default:
		f.syncH.ServeHTTP(w, r)
	}
}

// proxyFlags are the flags of `adsvc proxy`. The config file fills in what they leave unset.
type proxyFlags struct {
	fl       *flag.FlagSet
	setupLog func() error

	config, listen, dataDir, allow, ffmpeg *string
	mediaDir, mediaListen                  *string
	mediaRun                               mediaSettings // the media server started, if any
	metricsAddr                            *string
	metricsRun                             string                       // where the metrics are served, if anywhere
	applied                                atomic.Pointer[config.Proxy] // the file in effect
	minScore, confirm                      *int
	markWin                                *time.Duration
	verbose                                *bool
	routes                                 multiFlag
}

func newProxyFlags() *proxyFlags {
	fl := flag.NewFlagSet("proxy", flag.ExitOnError)
	pf := &proxyFlags{
		fl:       fl,
		config:   fl.String("config", os.Getenv("ADSVC_CONFIG"), "YAML config file (also $ADSVC_CONFIG; default "+defaultConfig+" if it exists); flags override it, SIGHUP reloads it"),
		listen:   fl.String("listen", "", "listen address (no host: every interface, IPv4 and IPv6; default "+defaultListen+")"),
		minScore: fl.Int("minscore", 0, fmt.Sprintf("match threshold (default %d)", detect.DefaultMinScore)),
		confirm:  fl.Int("confirm", 0, "score that confirms an ad from two blocks (default 2x minscore)"),
		markWin:  fl.Duration("markwindow", 0, "decoded audio kept for /ads/mark (default "+proxy.DefaultMarkWindow.String()+")"),
		allow:    fl.String("allow", "", "comma-separated upstream host allowlist (default: any host, or only routed ones with -route)"),
		verbose:  fl.Bool("v", false, "shorthand for -log-level debug: log every request, range and decoder run"),
		ffmpeg:   fl.String("ffmpeg", "", "ffmpeg binary, the minimal build is enough (default: the one next to adsvc, else PATH)"),

		mediaDir:    fl.String("media", "", "local folder to play through the proxy: served read-only on -media-listen, whose pages give each file's play URL"),
		mediaListen: fl.String("media-listen", defaultMediaListen, "loopback address the -media folder is served on (no host: localhost)"),
		metricsAddr: fl.String("metrics-listen", "", "serve Prometheus metrics at /metrics on this address, without auth (no host: localhost; default off)"),
	}
	fl.Var(&pf.routes, "route", "FROM=TO upstream rewrite, e.g. https://ts.example.com=http://127.0.0.1:8090 (repeatable)")
	pf.dataDir = dataDirFlag(fl, "")
	pf.setupLog = logFlags(fl, slog.LevelInfo)
	return pf
}

// defaultListen is the proxy's listen address without a flag or a file.
const defaultListen = "localhost:8080"

// defaultConfig is the config file used when -config and $ADSVC_CONFIG name none: in the
// current directory, and only if it exists (without it adsvc runs on flags and defaults).
const defaultConfig = "config.yml"

// path is the config file: the one named (which must exist), else defaultConfig if it
// exists, else "" (none). It is looked up each time, so a config.yml created later is
// read by the next reload.
func (pf *proxyFlags) path() string {
	if *pf.config != "" {
		return *pf.config
	}
	if fi, err := os.Stat(defaultConfig); err == nil && fi.Mode().IsRegular() {
		return defaultConfig
	}
	return ""
}

// load reads the config file, or returns an empty one when there is none.
func (pf *proxyFlags) load() (*config.Proxy, error) {
	if pf.path() == "" {
		return &config.Proxy{}, nil
	}
	return config.Load(pf.path())
}

// applyLog applies the file's log settings where neither a flag nor the environment set
// them. A setting missing from the file goes back to its default, so that removing it and
// reloading has an effect.
func (pf *proxyFlags) applyLog(file *config.Proxy) error {
	if !pf.set("log-level") && !*pf.verbose && os.Getenv("ADSVC_LOG_LEVEL") == "" {
		l := slog.LevelInfo
		if file.Log.Level != "" {
			if err := l.UnmarshalText([]byte(file.Log.Level)); err != nil {
				return err
			}
		}
		logLevel.Set(l)
	}
	if !pf.set("log-format") && os.Getenv("ADSVC_LOG_FORMAT") == "" {
		return installLog(cmp.Or(file.Log.Format, "text"))
	}
	return nil
}

func (pf *proxyFlags) set(name string) bool { return isSet(pf.fl, name) }

// build merges flags, environment and file into the proxy's configuration and listen
// address. Scalars: a flag wins, then the file; what neither sets (the zero value) is the
// default (proxy.Config's own, or the ones here). Users come from the file only. -route
// and -allow replace the file's lists.
func (pf *proxyFlags) build(file *config.Proxy) (proxy.Config, string, error) {
	d := file.Detect
	cfg := proxy.Config{
		DataDir:      cmp.Or(file.DataDir, defaultDataDir),
		MinScore:     cmp.Or(*pf.minScore, d.MinScore),
		ConfirmScore: cmp.Or(*pf.confirm, d.ConfirmScore),
		MarkWindow:   cmp.Or(*pf.markWin, d.MarkWindow),
		MaxStall:     d.MaxStall,
		IdleTimeout:  d.IdleTimeout,
		FFmpeg:       ffmpeg.Tools{FFmpegPath: cmp.Or(*pf.ffmpeg, file.FFmpeg, bundledFFmpeg())},
		Version:      version,
	}
	if pf.set("data-dir") { // even when empty: -data-dir "" keeps everything in memory
		cfg.DataDir = *pf.dataDir
	}
	listen := cmp.Or(*pf.listen, file.Listen, defaultListen)

	for _, name := range slices.Sorted(maps.Keys(file.Auth.Users)) { // checked by config
		u := file.Auth.Users[name]
		cfg.Users = append(cfg.Users, proxy.User{Name: name, Tokens: slices.Clone(u.Tokens), Admin: u.Admin})
	}

	routes := file.Upstream.Routes
	if len(pf.routes) > 0 {
		routes = pf.routes
	}
	for _, r := range routes {
		rt, err := proxy.ParseRoute(r)
		if err != nil {
			return proxy.Config{}, "", usageError{err.Error()}
		}
		cfg.Routes = append(cfg.Routes, rt)
	}
	hosts := file.Upstream.Allow
	if pf.set("allow") {
		hosts = strings.Split(*pf.allow, ",")
	}
	if len(hosts) > 0 {
		cfg.AllowUpstream = allowHosts(hosts)
	}
	if pf.mediaRun.on() {
		cfg.AllowUpstream = allowMedia(pf.mediaRun.listen, cfg.AllowUpstream, len(cfg.Routes) > 0)
	}
	if !slices.ContainsFunc(cfg.Users, func(u proxy.User) bool { return len(u.Tokens) > 0 }) && !loopback(listen) {
		slog.Warn("no users with tokens (auth.users): anyone who can reach the listen address can use adsvc", "listen", listen)
	}
	return cfg, listen, nil
}

func allowHosts(hosts []string) func(*url.URL) bool {
	hosts = slices.Clone(hosts)
	return func(u *url.URL) bool {
		for _, h := range hosts {
			if strings.EqualFold(u.Hostname(), strings.TrimSpace(h)) {
				return true
			}
		}
		return false
	}
}

// reload re-reads the config file (on SIGHUP) and applies it; onNode (if not nil) applies
// the federation settings. An invalid file changes nothing. It returns what changed but
// needs a restart: the listen address, the data directory.
func (pf *proxyFlags) reload(px *proxy.Proxy, listen string, onNode func(node) error) (restart []string, err error) {
	if pf.path() == "" {
		return nil, errors.New("no config file to reload (-config, $ADSVC_CONFIG or ./" + defaultConfig + ")")
	}
	file, err := pf.load()
	if err != nil {
		return nil, err
	}
	cfg, newListen, err := pf.build(file)
	if err != nil {
		return nil, err
	}
	settings, err := nodeSettings(file)
	if err != nil {
		return nil, err
	}
	// The node's settings first: they are the ones that can still fail (they write to the
	// catalogue), and then nothing else has changed. The log settings were checked by
	// config.Load.
	if onNode != nil {
		if err := onNode(settings); err != nil {
			return nil, err
		}
	}
	if err := pf.applyLog(file); err != nil {
		return nil, err
	}
	restart = px.Reload(cfg)
	pf.applied.Store(file)
	if newListen != listen {
		restart = append(restart, "listen")
	}
	if m, err := pf.media(file); err != nil || m != pf.mediaRun {
		restart = append(restart, "media")
	}
	if l, err := pf.metricsListen(file); err != nil || l != pf.metricsRun {
		restart = append(restart, "metrics")
	}
	return restart, nil
}

// shutdown stops accepting connections and gives open ones a moment to finish. Players
// hold a stream open for as long as they play, so the rest are then closed.
func shutdown(parent context.Context, srv *http.Server) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); errors.Is(err, context.DeadlineExceeded) {
		return srv.Close()
	} else if err != nil {
		return err
	}
	return nil
}

// bundledFFmpeg is the default ffmpeg of the proxy: the one shipped next to the executable
// (release archives, Docker image), otherwise whatever is on PATH.
func bundledFFmpeg() string {
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return "ffmpeg"
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// listenURLs are the base URLs the proxy can be reached at: the listen address itself, or,
// when it listens on every interface (":8080", "0.0.0.0:8080", "[::]:8080"), localhost
// (IPv4 or IPv6, whichever the client resolves it to) and this host's other IPv4
// addresses.
func listenURLs(listen string) []string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return []string{"http://" + listen}
	}
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return []string{"http://" + net.JoinHostPort(host, port)}
	}
	urls := []string{"http://" + net.JoinHostPort("localhost", port)}
	addrs, _ := net.InterfaceAddrs() // without them, loopback still works
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			urls = append(urls, "http://"+net.JoinHostPort(n.IP.String(), port))
		}
	}
	return urls
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
