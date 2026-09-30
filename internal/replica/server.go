package replica

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// Limits of the sync endpoints.
const (
	maxLogPage  = 1000
	maxPushSize = 32 << 20
	maxPushRecs = 5000
)

// Server serves this node's log to peers.
type Server struct {
	db      *catalog.DB
	name    string
	tempDir string
	refresh *debouncer

	snapMu sync.Mutex
	snap   *snapshotFile // the last snapshot written, reused while the log has not moved
}

// snapshotFile is a snapshot on disk, of the log up to head.
type snapshotFile struct {
	head int64
	dir  string // removed when the snapshot is replaced (served copies keep their open file)
	path string
}

// NewServer serves db's log; name is advertised in /sync/info, snapshots are written in
// tempDir ("" = the system's) before they are sent, and changed is called (debounced,
// with ctx) after a push brought records. It lives as long as ctx.
func NewServer(ctx context.Context, db *catalog.DB, name, tempDir string, changed func(context.Context) error) *Server {
	s := &Server{db: db, name: name, tempDir: tempDir, refresh: newDebouncer(ctx, 5*time.Second, changed)}
	context.AfterFunc(ctx, func() {
		s.snapMu.Lock()
		defer s.snapMu.Unlock()
		if s.snap != nil {
			os.RemoveAll(s.snap.dir)
			s.snap = nil
		}
	})
	return s
}

// Handler serves:
//
//	GET  /sync/info?nonce=HEX        Info, signed over the nonce
//	GET  /sync/log?after=N&limit=L   log entries after N (application/x-adsvc-log)
//	GET  /sync/ad/{id}               the ad.add record of one ad (application/x-adsvc-records)
//	GET  /sync/snapshot              every record, as a catalogue file (catalog.Snapshot)
//	POST /sync/push                  records from a peer (signed: X-Adsvc-Node/-Date/-Signature)
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sync/info", s.info)
	mux.HandleFunc("GET /sync/log", s.log)
	mux.HandleFunc("GET /sync/ad/{id}", s.ad)
	mux.HandleFunc("GET /sync/snapshot", s.snapshot)
	mux.HandleFunc("POST /sync/push", s.push)
	return mux
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
	if err != nil || len(nonce) < 8 || len(nonce) > 64 {
		http.Error(w, "nonce: 8 to 64 bytes in hex", http.StatusBadRequest)
		return
	}
	id := s.db.Identity()
	head, err := s.db.Head(r.Context())
	if err != nil || id == nil {
		http.Error(w, "catalogue unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, Info{NodeID: id.Origin, Name: s.name, FPVersion: fingerprint.Version, Schema: catalog.SchemaVersion(),
		Head: head, Sig: id.SignBytes(infoMessage(nonce, id.Origin))})
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, err1 := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, err2 := strconv.Atoi(q.Get("limit"))
	if q.Get("after") == "" {
		after, err1 = 0, nil
	}
	if q.Get("limit") == "" {
		limit, err2 = maxLogPage, nil
	}
	if err1 != nil || err2 != nil || after < 0 || limit <= 0 {
		http.Error(w, "after and limit: non-negative integers", http.StatusBadRequest)
		return
	}
	// Each entry is written (and gzipped) as it is read; an error halfway can only cut
	// the stream short, which the peer reads as a truncated frame.
	bw := newBodyWriter(w, r, typeLog)
	var frame []byte
	err := s.db.EachLog(r.Context(), after, min(limit, maxLogPage), func(e *catalog.Entry) error {
		frame = appendEntry(frame[:0], e)
		_, err := bw.Write(frame)
		return err
	})
	if err == nil {
		err = bw.Close()
	}
	if err != nil && r.Context().Err() == nil {
		slog.Warn("sync: serving the log", "err", err)
	}
}

func (s *Server) ad(w http.ResponseWriter, r *http.Request) {
	id, err := fingerprint.ParseID(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec, err := s.db.AdRecord(r.Context(), id)
	if err != nil {
		http.Error(w, "catalogue unavailable", http.StatusServiceUnavailable)
		return
	}
	if rec == nil {
		http.Error(w, "unknown ad", http.StatusNotFound)
		return
	}
	writeBody(w, r, typeRecords, record.AppendFrame(nil, rec))
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	f, err := s.snapshotFile(r.Context())
	if err != nil {
		slog.Warn("sync: writing a snapshot", "err", err)
		http.Error(w, "snapshot failed", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "snapshot failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="adsvc-snapshot.db"`)
	http.ServeContent(w, r, "adsvc-snapshot.db", fi.ModTime(), f)
}

// snapshotFile opens a snapshot of the log as it is: the last one written if the log has
// not moved since, a new one otherwise. The file is opened before a newer snapshot can
// replace (and remove) it.
func (s *Server) snapshotFile(ctx context.Context) (*os.File, error) {
	head, err := s.db.Head(ctx)
	if err != nil {
		return nil, err
	}
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if s.snap == nil || s.snap.head != head {
		dir, err := os.MkdirTemp(s.tempDir, "adsvc-snapshot-")
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, "snapshot.db")
		if err := s.db.Snapshot(ctx, path); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
		if s.snap != nil {
			os.RemoveAll(s.snap.dir)
		}
		s.snap = &snapshotFile{head: head, dir: dir, path: path}
	}
	return os.Open(s.snap.path)
}

func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	node, err := record.ParseOrigin(r.Header.Get(hdrNode))
	if err != nil {
		http.Error(w, "push: "+hdrNode+": "+err.Error(), http.StatusBadRequest)
		return
	}
	date := r.Header.Get(hdrDate)
	if err := parseDate(date, time.Now()); err != nil {
		http.Error(w, "push: "+err.Error(), http.StatusBadRequest)
		return
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(hdrSig))
	var in io.Reader = http.MaxBytesReader(w, r.Body, maxPushSize)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, zerr := gzip.NewReader(in)
		if zerr != nil {
			http.Error(w, "push: bad gzip", http.StatusBadRequest)
			return
		}
		in = io.LimitReader(zr, maxPushSize+1) // the limit also holds for what it inflates to
	}
	body, rerr := io.ReadAll(in)
	if len(body) > maxPushSize {
		http.Error(w, "push: too big", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil || rerr != nil || !record.VerifyBytes(node, pushMessage(date, body), sig) {
		http.Error(w, "push: bad signature or body", http.StatusUnauthorized)
		return
	}
	recs, err := readRecords(bytes.NewReader(body), maxPushRecs)
	if err != nil {
		http.Error(w, "push: "+err.Error(), http.StatusBadRequest)
		return
	}
	res, err := s.db.Ingest(r.Context(), recs, node)
	if err != nil {
		slog.Warn("sync: storing pushed records", "node", node.Short(), "err", err)
		http.Error(w, "catalogue unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := s.db.CountPush(r.Context(), node, res); err != nil {
		slog.Warn("sync: counting pushed records", "node", node.Short(), "err", err)
	}
	slog.Info("sync: records pushed", "node", node.Short(), "accepted", res.Accepted, "duplicate", res.Duplicate, "rejected", res.Rejected)
	if res.Accepted > 0 {
		s.refresh.trigger()
	}
	writeJSON(w, res)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeBody writes b, gzipped when the client takes it.
func writeBody(w http.ResponseWriter, r *http.Request, contentType string, b []byte) {
	bw := newBodyWriter(w, r, contentType)
	if _, err := bw.Write(b); err == nil {
		_ = bw.Close() // an error is the client hanging up
	}
}

// bodyWriter writes a response body of record frames, gzipped when the client takes it.
type bodyWriter struct {
	bw *bufio.Writer
	zw *gzip.Writer // nil: plain
}

func newBodyWriter(w http.ResponseWriter, r *http.Request, contentType string) *bodyWriter {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		return &bodyWriter{bw: bufio.NewWriterSize(w, 64<<10)}
	}
	w.Header().Set("Content-Encoding", "gzip")
	zw := gzip.NewWriter(w)
	return &bodyWriter{bw: bufio.NewWriterSize(zw, 64<<10), zw: zw}
}

func (b *bodyWriter) Write(p []byte) (int, error) { return b.bw.Write(p) }

// Close flushes what is buffered and ends the gzip stream.
func (b *bodyWriter) Close() error {
	err := b.bw.Flush()
	if b.zw != nil {
		err = errors.Join(err, b.zw.Close())
	}
	return err
}

// debouncer calls fn at most once per wait, some time after a trigger, while ctx lasts.
type debouncer struct {
	ctx  context.Context
	wait time.Duration
	fn   func(context.Context) error
	mu   sync.Mutex
	t    *time.Timer
}

func newDebouncer(ctx context.Context, wait time.Duration, fn func(context.Context) error) *debouncer {
	return &debouncer{ctx: ctx, wait: wait, fn: fn}
}

func (d *debouncer) trigger() {
	if d == nil || d.fn == nil || d.ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.t != nil {
		return // one is pending
	}
	d.t = time.AfterFunc(d.wait, func() {
		d.mu.Lock()
		d.t = nil
		d.mu.Unlock()
		if err := d.fn(d.ctx); err != nil && d.ctx.Err() == nil {
			slog.Warn("sync: refreshing the ad index", "err", err)
		}
	})
}
