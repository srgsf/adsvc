package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/srgsf/adsvc/internal/config"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/metrics"
)

var configReloads = metrics.NewCounterVec("adsvc_config_reloads_total",
	"Config reloads on SIGHUP, by result (ok, invalid).", "result")

func init() {
	metrics.NewGaugeFunc("adsvc_build_info", "The adsvc build: always 1.", func(e metrics.Emit) {
		e(1, version, runtime.Version(), strconv.Itoa(fingerprint.Version))
	}, "version", "go_version", "fingerprint_version")
}

// metricsListen is where the metrics are served (-metrics-listen, metrics.listen): ""
// for off. No host means localhost: the listener has no auth, so reaching it from
// elsewhere takes an explicit host such as 0.0.0.0. Needs a restart.
func (pf *proxyFlags) metricsListen(file *config.Proxy) (string, error) {
	l := file.Metrics.Listen
	if pf.set("metrics-listen") {
		l = *pf.metricsAddr
	}
	if l == "" {
		return "", nil
	}
	host, port, err := net.SplitHostPort(l)
	if err != nil || port == "" || port == "0" {
		return "", usagef("metrics listen %q: want [host]:port with a fixed port, e.g. localhost:9464", l)
	}
	if host == "" {
		l = net.JoinHostPort("localhost", port)
	}
	return l, nil
}

// startMetrics serves /metrics on listen until ctx ends. The returned func waits for the
// server to stop.
func startMetrics(ctx context.Context, listen string) (wait func(), err error) {
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", listen)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	slog.Info("metrics served", "url", "http://"+listen+"/metrics")
	return serveUntil(ctx, "metrics", ln, mux, nil), nil
}

// serveUntil serves h on ln until ctx ends, then shuts the server down (as the proxy's
// own); after is called once it has stopped. The returned func waits for that, stopping
// the server first if ctx has not ended (the proxy could not listen).
func serveUntil(ctx context.Context, name string, ln net.Listener, h http.Handler, after func()) (wait func()) {
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn(name+" server stopped", "listen", ln.Addr().String(), "err", err)
		}
		if after != nil {
			after()
		}
	}()
	stopServer := func() {
		if err := shutdown(ctx, hs); err != nil {
			slog.Warn("stopping the "+name+" server", "err", err)
		}
	}
	stop := context.AfterFunc(ctx, stopServer)
	return func() {
		if stop() {
			stopServer()
		}
		<-done
	}
}
