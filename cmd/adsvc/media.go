package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"strings"

	"github.com/srgsf/adsvc/internal/config"
	"github.com/srgsf/adsvc/internal/media"
)

// defaultMediaListen is where the media folder is served when -media is given.
const defaultMediaListen = "localhost:8000"

// mediaSettings is the local folder the proxy serves to itself (-media, media.dir) and
// where (-media-listen, media.listen). Both need a restart.
type mediaSettings struct{ dir, listen string }

func (m mediaSettings) on() bool { return m.dir != "" }

// media merges the media flags with the file. The address must be loopback with a port:
// the server has no auth (only the proxy reads it), and its URLs are part of the file
// keys of what is played from it, so they must not change between runs.
func (pf *proxyFlags) media(file *config.Proxy) (mediaSettings, error) {
	m := mediaSettings{dir: file.Media.Dir, listen: file.Media.Listen}
	if pf.set("media") {
		m.dir = *pf.mediaDir
	}
	if pf.set("media-listen") {
		m.listen = *pf.mediaListen
	}
	if !m.on() {
		return mediaSettings{}, nil
	}
	m.listen = cmp.Or(m.listen, defaultMediaListen)
	if host, port, err := net.SplitHostPort(m.listen); err == nil && host == "" {
		m.listen = net.JoinHostPort("localhost", port) // loopback only, whatever the family
	}
	if _, port, err := net.SplitHostPort(m.listen); err != nil || port == "0" || port == "" || !loopback(m.listen) {
		return m, usagef("media listen %q: want a loopback address with a fixed port, e.g. %s", m.listen, defaultMediaListen)
	}
	return m, nil
}

// allowMedia lets the proxy read the media server even when an allow-list or routes
// restrict upstreams. Without either the proxy is open anyway, and allow stays nil.
func allowMedia(host string, allow func(*url.URL) bool, routed bool) func(*url.URL) bool {
	if allow == nil && !routed {
		return nil
	}
	return func(u *url.URL) bool {
		if u.Scheme == "http" && sameLoopback(u.Host, host) {
			return true
		}
		return allow != nil && allow(u)
	}
}

// sameLoopback reports whether the URL host hp names the media server at listen: the
// same port, and the same host, where localhost also matches the loopback addresses it
// may be bound to.
func sameLoopback(hp, listen string) bool {
	h, p, err1 := net.SplitHostPort(hp)
	lh, lp, err2 := net.SplitHostPort(listen)
	if err1 != nil || err2 != nil || p != lp {
		return false
	}
	if strings.EqualFold(lh, "localhost") {
		return strings.EqualFold(h, "localhost") || h == "127.0.0.1" || h == "::1"
	}
	return strings.EqualFold(h, lh)
}

// startMedia serves m.dir on m.listen until ctx ends; its directory pages link each file
// to proxyBase/s?u=<file URL>. The returned func waits for the server to stop.
func startMedia(ctx context.Context, m mediaSettings, proxyBase string) (wait func(), err error) {
	srv, err := media.Open(m.dir)
	if err != nil {
		return nil, err
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", m.listen)
	if err != nil {
		return nil, errors.Join(err, srv.Close())
	}
	srv.Base = "http://" + m.listen
	play := func(fileURL string) string { return proxyBase + "/s?u=" + url.QueryEscape(fileURL) }
	srv.Play = play
	// Unescaped, to be readable: players take it as it is. The pages give escaped links,
	// which also work for names with "+", "&" or "#".
	slog.Info("media folder served", "dir", srv.Dir(), "browse", srv.Base+"/",
		"play", proxyBase+"/s?u="+srv.Base+"/<file>")
	return serveUntil(ctx, "media", ln, srv, func() {
		if err := srv.Close(); err != nil {
			slog.Warn("closing the media folder", "dir", m.dir, "err", err)
		}
	}), nil
}
