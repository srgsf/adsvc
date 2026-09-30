package proxy

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/srgsf/adsvc/internal/metrics"
)

var (
	httpRequests = metrics.NewCounterVec("adsvc_http_requests_total",
		"HTTP requests by handler, method and status code.", "handler", "method", "code")
	httpDuration = metrics.NewHistogramVec("adsvc_http_request_duration_seconds",
		"Time to serve an HTTP request, by handler (streams are left out: they last as long as playback).",
		metrics.ExpBuckets(0.001, 4, 8), "handler") // 1 ms … 16 s
	httpInFlight = metrics.NewGaugeVec("adsvc_http_requests_in_flight",
		"HTTP requests being served, by handler (for stream: open player connections).", "handler")
)

// logRequests logs every request at debug level (method, path, status, bytes and
// duration) and counts it in the HTTP metrics under its route. The query is left out,
// because it carries upstream URLs and their credentials.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observe(routeName(r.URL.Path), next, w, r)
	})
}

// Instrument counts the requests of a handler that is not the proxy's (the web page,
// /sync/*) in the HTTP metrics as name, and logs them as logRequests does.
func Instrument(name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { observe(name, next, w, r) })
}

func observe(name string, next http.Handler, w http.ResponseWriter, r *http.Request) {
	inFlight := httpInFlight.With(name)
	inFlight.Inc()
	defer inFlight.Dec()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	next.ServeHTTP(rec, r)
	took := time.Since(start)
	httpRequests.With(name, methodName(r.Method), strconv.Itoa(rec.status)).Inc()
	if name != "stream" {
		httpDuration.With(name).ObserveDuration(took)
	}
	slog.DebugContext(r.Context(), "http request", "method", r.Method, "path", r.URL.Path,
		"status", rec.status, "bytes", rec.bytes, "duration", took, "remote", r.RemoteAddr)
}

// routeName is the metrics label of a path Handler serves: a fixed set, never the path.
func routeName(path string) string {
	if name, ok := apiRoute(path); ok {
		return name
	}
	return "other"
}

// methodName bounds the method label: clients can send any token.
func methodName(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodPatch, http.MethodOptions:
		return m
	}
	return "other"
}

// statusRecorder remembers the status and size of a response. Unwrap keeps
// http.ResponseController (Flush) working through it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
