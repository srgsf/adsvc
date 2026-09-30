package proxy

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/srgsf/adsvc/internal/adtype"
	"github.com/srgsf/adsvc/internal/mediatime"
)

// Handler serves the proxy and its API:
//
//	GET  /s?u=<upstream>[&ih=<infohash>&idx=<n>]   media, forwarded byte for byte
//	GET  /ads/file?u=...|ih=..&idx=..|key=..       what is known about that file
//	POST /ads/mark?...&startMs=S&endMs=E[&label=L]  enroll an ad from the audio just played
//	POST /ads/report?u=...&position=MS[&note=N]    report an ad seen at position, to mark it later
//	GET  /ads/reports                              the caller's reports
//	DELETE /ads/reports/{id}                       delete one of them
//	GET  /ads/library                              reference ads
//	GET  /ads/maps                                 stored per-file ad maps
//	GET  /healthz                                  "adsvc <version>", needs no auth (lets a client decide to use adsvc)
//
// Everything but /healthz requires a user's token once a user has one (Config.Users).
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	// Method patterns: GET also serves HEAD, and any other method is a 405 with Allow.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = fmt.Fprintf(w, "adsvc %s\n", cmp.Or(p.reqConf(r).Version, "dev"))
	})
	mux.HandleFunc("GET /s", p.serveStream)
	mux.HandleFunc("GET /ads/file", func(w http.ResponseWriter, r *http.Request) {
		rep, ok, err := p.report(r)
		if err != nil {
			slog.Warn("reading the ad map", "err", err)
			http.Error(w, "catalogue unavailable", http.StatusInternalServerError)
			return
		}
		if !ok {
			fileMaps.With("miss").Inc()
			http.Error(w, "unknown file", http.StatusNotFound)
			return
		}
		fileMaps.With("hit").Inc()
		writeJSON(w, rep)
	})
	mux.HandleFunc("POST /ads/mark", func(w http.ResponseWriter, r *http.Request) {
		s := p.lookup(r)
		if s == nil {
			http.Error(w, "unknown file (no active session)", http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		start, ok1 := msParam(q, "startMs")
		end, ok2 := msParam(q, "endMs")
		if !ok1 || !ok2 {
			http.Error(w, "startMs and endMs must be positions in milliseconds", http.StatusBadRequest)
			return
		}
		typ := adtype.Of(q.Get("type"))
		if !adtype.Valid(typ) {
			http.Error(w, "type must be ad or intro", http.StatusBadRequest)
			return
		}
		ad, err := p.Mark(r.Context(), s, start, end, q.Get("label"), typ)
		if err != nil {
			marks.With("error").Inc()
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		marks.With("ok").Inc()
		p.clearReports(r, s, start, end)
		writeJSON(w, ad.Info())
	})
	mux.HandleFunc("POST /ads/report", p.addReport)
	mux.HandleFunc("GET /ads/reports", p.listReports)
	mux.HandleFunc("DELETE /ads/reports/{id}", p.deleteReport)
	mux.HandleFunc("GET /ads/library", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, p.Lib.List())
	})
	mux.HandleFunc("GET /ads/maps", func(w http.ResponseWriter, r *http.Request) {
		maps, err := p.Maps.List(r.Context())
		if err != nil {
			slog.Warn("listing ad maps", "err", err)
			http.Error(w, "catalogue unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, maps)
	})
	return logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One config per request: a reload in the middle must not mix the auth decision
		// of one config with the forwarding rules of another.
		cfg := p.conf()
		user, ok := authorized(cfg, r)
		if r.URL.Path != "/healthz" && !ok {
			Unauthorized(w)
			return
		}
		ctx := context.WithValue(r.Context(), confKey{}, cfg)
		mux.ServeHTTP(w, r.WithContext(context.WithValue(ctx, userKey{}, user)))
	}))
}

// apiRoutes are the names under /ads/ that Handler serves.
var apiRoutes = []string{"file", "mark", "library", "maps", "report", "reports"}

// apiRoute names the route of path that Handler serves (the metrics label: a fixed set),
// if it serves path.
func apiRoute(path string) (name string, ok bool) {
	switch path {
	case "/s":
		return "stream", true
	case "/healthz":
		return "healthz", true
	}
	rest, ok := strings.CutPrefix(path, "/ads/")
	name, _, _ = strings.Cut(rest, "/")
	if ok && slices.Contains(apiRoutes, name) {
		return name, true
	}
	return "", false
}

// APIPath reports whether Handler serves path: a front end may hand the other paths to
// something else (the web page).
func APIPath(path string) bool {
	_, ok := apiRoute(path)
	return ok
}

type (
	confKey struct{}
	userKey struct{}
)

// msParam reads a query parameter that is a media position in integer milliseconds.
func msParam(q url.Values, name string) (time.Duration, bool) {
	v, err := strconv.ParseInt(q.Get(name), 10, 32)
	if err != nil || v < 0 {
		return 0, false
	}
	return mediatime.Dur(int32(v)), true
}

// reqUser is the user the request was authorized as: "" when adsvc is open (no user has
// a token).
func reqUser(r *http.Request) string {
	u, _ := r.Context().Value(userKey{}).(string)
	return u
}

// reqConf is the configuration the request was authorized with.
func (p *Proxy) reqConf(r *http.Request) *Config {
	if cfg, ok := r.Context().Value(confKey{}).(*Config); ok {
		return cfg
	}
	return p.conf()
}

// Unauthorized answers 401 with a Basic challenge next to the Bearer one: ffmpeg (and so
// mpv) sends the user info of a URL only after a 401 that offers Basic.
func Unauthorized(w http.ResponseWriter) {
	w.Header().Add("WWW-Authenticate", `Basic realm="adsvc"`)
	w.Header().Add("WWW-Authenticate", `Bearer realm="adsvc"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// authOn reports whether any user has a token: without one adsvc is open.
func authOn(cfg *Config) bool {
	for _, u := range cfg.Users {
		if len(u.Tokens) > 0 {
			return true
		}
	}
	return false
}

// Authorized reports whether r carries a user's token (or no user has one), for handlers
// mounted next to the proxy's (/sync/*).
func (p *Proxy) Authorized(r *http.Request) bool {
	_, ok := authorized(p.conf(), r)
	return ok
}

// authorized checks adsvc's own credentials: a user's token as `Authorization: Bearer
// TOKEN`, or, from players that can only be given a URL, NAME:TOKEN as the user info of
// the adsvc URL, which they send as Basic auth; the token must then be that user's. It
// returns the user's name ("" when no user has a token and adsvc is open).
func authorized(cfg *Config, r *http.Request) (string, bool) {
	if !authOn(cfg) {
		return "", true
	}
	if token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found {
		return cfg.userOf(strings.TrimSpace(token), "")
	}
	if name, token, ok := r.BasicAuth(); ok && name != "" {
		return cfg.userOf(token, name)
	}
	return "", false
}

// userOf returns the user a token belongs to; with a name, only that user's tokens count.
// Every token is compared, in constant time, so that timing tells nothing about which
// user or token came close.
func (cfg *Config) userOf(token, name string) (string, bool) {
	found := ""
	for _, u := range cfg.Users {
		for _, t := range u.Tokens {
			match := subtle.ConstantTimeCompare([]byte(token), []byte(t)) == 1
			if match && found == "" && (name == "" || name == u.Name) {
				found = u.Name
			}
		}
	}
	return found, found != "" && token != ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
