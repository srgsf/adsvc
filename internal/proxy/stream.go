package proxy

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/srgsf/adsvc/internal/filekey"
)

var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

func (p *Proxy) serveStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	raw := q.Get("u")
	if raw == "" {
		http.Error(w, "u (upstream URL) is required", http.StatusBadRequest)
		return
	}
	orig, err := url.Parse(raw)
	if err != nil || (orig.Scheme != "http" && orig.Scheme != "https") || orig.Host == "" {
		http.Error(w, "u must be an http(s) URL", http.StatusBadRequest)
		return
	}
	cfg := p.reqConf(r)
	up, ok := cfg.resolve(orig)
	if !ok {
		http.Error(w, "upstream not allowed", http.StatusForbidden)
		return
	}
	if up.Scheme != "http" && up.Scheme != "https" {
		http.Error(w, "route target must be http(s)", http.StatusBadRequest)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, up.String(), nil) //nolint:gosec // G704, see Client.Do
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fwd := []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "User-Agent", "Referer"}
	if !authOn(cfg) {
		fwd = append(fwd, "Authorization") // no auth of our own: the player's is the source's
	}
	for _, h := range fwd {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if v := sourceCookies(r.Header); v != "" {
		req.Header.Set("Cookie", v)
	}
	if v := r.Header.Get("X-Upstream-Authorization"); v != "" {
		req.Header.Set("Authorization", v)
	}
	req.Header.Set("Accept-Encoding", "identity") // we must see the real bytes

	start := time.Now()
	resp, err := cfg.fetch(req)
	upstreamResponse.Since(start)
	if errors.Is(err, errNotAllowed) {
		upstreamRequests.With("error").Inc()
		slog.Warn("upstream redirect refused", "upstream", filekey.RedactURL(up), "err", err)
		http.Error(w, "upstream redirect not allowed", http.StatusForbidden)
		return
	}
	if err != nil {
		upstreamRequests.With("error").Inc()
		slog.Warn("upstream request failed", "upstream", filekey.RedactURL(up), "err", err)
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	upstreamRequests.With(statusClass(resp.StatusCode)).Inc()

	for k, vs := range resp.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead || resp.StatusCode >= 300 || resp.ContentLength == 0 {
		// No media: a session that exists learns the size, but none is started for a
		// HEAD (link checkers, players probing URLs); the first GET gives the size anyway.
		if s := p.existing(q); s != nil && resp.StatusCode < 300 {
			s.setSize(bodyTotal(resp))
		}
		n, _ := io.Copy(w, resp.Body) // an error here is the player hanging up
		streamBytes.Add(float64(n))
		return
	}

	off, total := bodyRange(resp)
	s := p.session(q, orig)
	s.setSize(total)
	slog.Debug("serving", "session", s.id, "offset", off, "status", resp.StatusCode)

	t := &tap{w: w, rc: http.NewResponseController(w), s: s, off: off}
	buf := make([]byte, copyBuf)
	if _, err := io.CopyBuffer(t, resp.Body, buf); err != nil {
		streamCopyErrors.Inc()
		slog.Debug("stream ended", "session", s.id, "offset", t.off, "err", err)
	}
	t.rc.Flush()
	s.keyCheck()
}

// ownCookie is the prefix of adsvc's own cookies (the web page's session): they are
// credentials for adsvc and never reach a source.
const ownCookie = "adsvc_"

// sourceCookies is the player's Cookie header without adsvc's own cookies.
func sourceCookies(h http.Header) string {
	var keep []string
	for _, line := range h.Values("Cookie") {
		for c := range strings.SplitSeq(line, ";") {
			if c = strings.TrimSpace(c); c != "" && !strings.HasPrefix(c, ownCookie) {
				keep = append(keep, c)
			}
		}
	}
	return strings.Join(keep, "; ")
}

// maxRedirects is how many redirects fetch follows, as net/http does.
const maxRedirects = 10

// fetch sends req upstream and follows redirects itself, so that every hop passes the
// routes and the allow-list (redirect), not just the URL the client sent. Like net/http
// it drops the credentials when a hop leaves the host.
func (cfg *Config) fetch(req *http.Request) (*http.Response, error) {
	client := *cfg.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for hops := 0; ; hops++ {
		// Forwarding to a client-chosen URL is the proxy's job; resolve (or redirect,
		// for the hops after the first) applied the routes and the allow-list.
		resp, err := client.Do(req) //nolint:gosec // G704, see above
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		default:
			return resp, nil
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			return resp, nil
		}
		resp.Body.Close()
		if hops == maxRedirects {
			return nil, fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		next, err := req.URL.Parse(loc)
		if err != nil {
			return nil, fmt.Errorf("redirect: %w", err)
		}
		to, ok := cfg.redirect(next)
		if !ok {
			return nil, fmt.Errorf("redirect to %s: %w", filekey.RedactURL(next), errNotAllowed)
		}
		nreq, err := http.NewRequestWithContext(req.Context(), req.Method, to.String(), nil) //nolint:gosec // G704, see above
		if err != nil {
			return nil, err
		}
		nreq.Header = req.Header.Clone()
		if !strings.EqualFold(nreq.URL.Host, req.URL.Host) {
			nreq.Header.Del("Authorization")
			nreq.Header.Del("Cookie")
		}
		req = nreq
	}
}

// tap forwards bytes to the player and hands the same bytes to the analyser.
// The player is never made to wait for analysis: feed drops data when the queue is full.
type tap struct {
	w   io.Writer
	rc  *http.ResponseController
	s   *session
	off int64
}

func (t *tap) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		streamBytes.Add(float64(n))
		t.s.kb.Feed(t.off, p[:n])
		t.s.feed(t.off, p[:n])
		t.off += int64(n)
	}
	if err == nil {
		t.rc.Flush()
	}
	return n, err
}

// bodyRange returns the absolute offset of the response body and the full file size.
func bodyRange(resp *http.Response) (off, total int64) {
	if cr := resp.Header.Get("Content-Range"); strings.HasPrefix(cr, "bytes ") {
		spec := strings.TrimSpace(strings.TrimPrefix(cr, "bytes "))
		if i := strings.Index(spec, "/"); i >= 0 {
			if n, err := strconv.ParseInt(spec[i+1:], 10, 64); err == nil {
				total = n
			}
			spec = spec[:i]
		}
		if i := strings.Index(spec, "-"); i > 0 {
			if n, err := strconv.ParseInt(spec[:i], 10, 64); err == nil {
				off = n
			}
		}
		return off, total
	}
	return 0, resp.ContentLength
}

func bodyTotal(resp *http.Response) int64 {
	_, total := bodyRange(resp)
	return total
}
