package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srgsf/adsvc/internal/admap"
	"github.com/srgsf/adsvc/internal/decode"
	"github.com/srgsf/adsvc/internal/demux"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/filekey"
)

// session is one file being streamed through the proxy.
type session struct {
	p    *Proxy
	id   string
	norm string // filekey.NormalizeURL of the URL the client sent
	kb   filekey.Builder
	ring *decode.PCMRing

	q          chan qchunk
	queued     atomic.Int64 // bytes waiting for the analyser
	dead       atomic.Bool  // analysis given up
	detached   bool         // created during shutdown: forward only (see detached)
	dropping   atomic.Bool  // behind: drop until the analyser has caught up completely
	sendMu     sync.RWMutex
	sendClosed bool

	// worker-only state
	sniff   []byte
	seen    int64
	cont    demux.Container
	sink    *decode.Sink
	ctype   string
	nextOff int64

	mu      sync.Mutex
	key     string
	alias   []string
	size    int64
	covered detect.Intervals
	ads     []detect.Candidate
	// remote are the ads other nodes found in this file, as far as this node trusts them
	// (admap.Store.PeerAds). They are reported, never stored or published as this
	// node's own.
	remote     []detect.Detection
	duration   time.Duration // as far as the container knows it
	analysedTo time.Duration // media time reached by the analyser
	// reported: someone reported an ad in this file that is not marked yet. The decoder
	// then runs over ranges analysed before too, so that /ads/mark finds their audio.
	reported bool
	// marking: the player asked for marking mode (MarkParam in the URL): nothing counts as
	// analysed, so every range is decoded into the ring. Unlike reported it does not end
	// when a mark clears the report.
	marking bool
	used    time.Time
	dirty   bool
	closed  bool
	wg      sync.WaitGroup

	ready   chan struct{} // closed once the stored state has been loaded (init)
	storeMu sync.Mutex    // one store at a time, from snapshot to commit
}

type qchunk struct {
	off  int64
	data []byte
	buf  *[]byte // data's buffer, from chunkPool
}

// The analysis queue: at most queueChunks chunks of at most copyBuf bytes each (the size
// of stream.go's copy buffer, so one forwarded read is one chunk): 8 MiB per session.
const (
	copyBuf     = 128 << 10
	queueChunks = 64
)

// chunkPool holds the buffers chunks are copied into (the copy buffer is reused for the
// next read); the analyser returns each once it has consumed it.
var chunkPool = sync.Pool{New: func() any { b := make([]byte, copyBuf); return &b }}

// chunk copies p into a pooled buffer.
func chunk(off int64, p []byte) qchunk {
	if len(p) > copyBuf {
		return qchunk{off: off, data: slices.Clone(p)}
	}
	buf := chunkPool.Get().(*[]byte)
	return qchunk{off: off, data: (*buf)[:copy(*buf, p)], buf: buf}
}

// release returns the chunk's buffer to the pool.
func (c qchunk) release() {
	if c.buf != nil {
		chunkPool.Put(c.buf)
	}
}

// MarkParam is the query parameter of /s that switches a session to marking mode: the
// decoder runs over ranges analysed before too, so an ad the node does not know can be
// enrolled with /ads/mark from the audio just played.
const MarkParam = "mark"

// fileIdent names the file a request refers to. key is the file key when the request or
// the upstream URL says it (id, ih/idx, a TorrServer URL); otherwise the file is known by
// its normalised upstream URL until the content key has been computed.
func fileIdent(q url.Values) (id, key, norm string) {
	if u, err := url.Parse(param(q, "u", "upstream")); err == nil && u.Host != "" {
		norm = filekey.NormalizeURL(u)
		key = filekey.FromTorrServerURL(u)
	}
	if v := q.Get("id"); v != "" {
		key = filekey.Opaque(v)
	} else if ih := param(q, "ih", "hash"); ih != "" {
		key = filekey.Torrent(ih, param(q, "idx", "index"))
	}
	id = key
	if id == "" {
		id = norm
	}
	return id, key, norm
}

// session returns the session of the file q refers to, creating it when there is none.
// The catalogue is read outside p.mu: a new file waits for its own reads only, and
// requests for it that arrive meanwhile wait for them too (ready).
func (p *Proxy) session(q url.Values, orig *url.URL) *session {
	id, key, norm := fileIdent(q)
	marking := q.Get(MarkParam) != ""
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return detached(p, id)
	}
	if s := p.sess[id]; s != nil {
		p.mu.Unlock()
		s.mu.Lock()
		s.used = time.Now()
		s.marking = s.marking || marking
		s.mu.Unlock()
		<-s.ready
		return s
	}
	s := &session{
		p: p, id: id, norm: norm,
		ring:  decode.NewPCMRing(p.conf().MarkWindow),
		q:     make(chan qchunk, queueChunks),
		used:  time.Now(),
		ready: make(chan struct{}),
		key:   key,

		marking: marking,
	}
	if s.key == "" {
		s.key = "session:" + id // provisional until the content key can be computed
	}
	p.sess[id] = s
	s.wg.Add(1)
	p.mu.Unlock()
	s.init(key, orig)
	go s.worker()
	return s
}

// init reads what the catalogue knows about the new session's file: whether it has an
// open report, and its stored map (when the file key is known already).
func (s *session) init(key string, orig *url.URL) {
	defer close(s.ready)
	reported := s.p.reported(s.id, key, s.norm)
	s.mu.Lock()
	s.reported = reported
	s.mu.Unlock()
	if key != "" {
		s.load(key)
	}
	kind := "normal"
	if s.reported {
		kind = "reported"
	}
	sessionsOpened.With(kind).Inc()
	sessionsActive.Inc()
	slog.Info("new session", "session", s.id, "key", s.key, "upstream", filekey.RedactURL(orig))
}

// existing is the live session of the file q refers to, or nil.
func (p *Proxy) existing(q url.Values) *session {
	id, _, _ := fileIdent(q)
	p.mu.Lock()
	s := p.sess[id]
	p.mu.Unlock()
	if s != nil {
		<-s.ready
	}
	return s
}

// detached is a session for a request that arrives while the proxy shuts down: the bytes
// are forwarded, nothing is analysed or stored.
func detached(p *Proxy, id string) *session {
	s := &session{p: p, id: id, detached: true, ready: closedReady}
	s.dead.Store(true)
	sessionsOpened.With("detached").Inc()
	return s
}

// closedReady is the ready channel of sessions with nothing to load.
var closedReady = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func param(q url.Values, names ...string) string {
	for _, n := range names {
		if v := q.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// lookup finds the session a request refers to (by id, ih/idx, upstream URL or file key).
func (p *Proxy) lookup(r *http.Request) *session {
	q := r.URL.Query()
	id, _, norm := fileIdent(q)
	key := q.Get("key")
	p.mu.Lock()
	s := p.sess[id]
	if id == "" || s == nil {
		s = nil
		for _, c := range p.sess {
			c.mu.Lock()
			hit := (norm != "" && c.norm == norm) || (key != "" && (c.key == key || slices.Contains(c.alias, key)))
			c.mu.Unlock()
			if hit {
				s = c
				break
			}
		}
	}
	p.mu.Unlock()
	if s != nil {
		<-s.ready
	}
	return s
}

func (s *session) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.used)
}

func (s *session) setSize(total int64) {
	if total <= 0 {
		return
	}
	s.kb.SetSize(total)
	s.mu.Lock()
	if s.size != total {
		s.size, s.dirty = total, true // part of the map
	}
	s.mu.Unlock()
	s.keyCheck()
}

// keyCheck adopts the content key as soon as both ends of the file have passed through.
func (s *session) keyCheck() {
	ck, ok := s.kb.Done()
	if !ok {
		return
	}
	s.mu.Lock()
	if slices.Contains(s.alias, ck) || s.key == ck {
		s.mu.Unlock()
		return
	}
	provisional := strings.HasPrefix(s.key, "session:")
	if provisional {
		s.key = ck
	} else {
		s.alias = append(s.alias, ck)
	}
	key := s.key
	s.dirty = true
	s.mu.Unlock()
	slog.Info("content key", "session", s.id, "key", ck)
	if provisional {
		s.load(key)
	} else {
		s.loadRemote(ck) // other nodes may know the file by its content only
	}
	s.store() // re-files the map (and anything already found) under the new key
}

// load merges what the store knows about key into the session.
func (s *session) load(key string) {
	s.loadRemote(key)
	s.loadSources(key)
	m, err := s.p.Maps.Lookup(s.p.ctx, key)
	if err != nil {
		slog.Warn("reading the ad map; analysing the file as new", "session", s.id, "key", key, "err", err)
	}
	if m == nil {
		return
	}
	s.mu.Lock()
	for _, r := range m.Analyzed {
		s.covered = s.covered.Add(r[0], r[1])
	}
	for _, d := range m.Ads {
		var i int
		// A stored detection may merge into a candidate already there (not the last one),
		// or lose to a better alignment of it: only the one it landed on is confirmed.
		if s.ads, i, _ = detect.MergeCandidate(s.ads, d, detect.NoBlock, detect.NoBlock); i >= 0 && d.Confirmed {
			s.ads[i].Sticky = true
		}
	}
	for _, a := range m.Aliases {
		if a != s.key && !slices.Contains(s.alias, a) {
			s.alias = append(s.alias, a)
		}
	}
	s.confirm()
	n, cov := len(s.ads), s.covered
	s.mu.Unlock()
	slog.Info("known file", "session", s.id, "key", key, "ads", n, "analysed_ranges", len(cov))
}

// loadSources adds the ads enrolled from this file as confirmed detections where they
// were marked: that is where they are, whether or not the analysis has seen them since.
func (s *session) loadSources(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.p.sourceAds(append([]string{key, s.key}, s.alias...)...) {
		d.Confirmed = false // the candidate's Sticky says so
		var i int
		if s.ads, i, _ = detect.MergeCandidate(s.ads, d, detect.NoBlock, detect.NoBlock); i >= 0 {
			s.ads[i].Sticky = true
		}
	}
}

// loadRemote loads what other nodes found in the file.
func (s *session) loadRemote(key string) {
	s.mu.Lock()
	keys := append([]string{key, s.key}, s.alias...)
	s.mu.Unlock()
	remote, err := s.p.Maps.PeerAds(s.p.ctx, s.p.Lib.Tracked, keys...)
	if err != nil {
		slog.Warn("reading other nodes' ad maps", "session", s.id, "err", err)
		return
	}
	s.mu.Lock()
	s.remote = remote
	s.mu.Unlock()
	if len(remote) > 0 {
		slog.Info("ads known from other nodes", "session", s.id, "key", key, "ads", len(remote))
	}
}

// store writes the session state back into the ad map store. Stores are serialised, so
// that the map on disk is never older than the last snapshot taken.
func (s *session) store() {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	s.mu.Lock()
	if s.detached || !s.dirty || !filekey.Stored(s.key) {
		s.mu.Unlock()
		return
	}
	m := &admap.FileMap{Key: s.key, Aliases: append([]string(nil), s.alias...), Size: s.size,
		Duration: s.duration, Analyzed: append(detect.Intervals(nil), s.covered...)}
	for i := range s.ads {
		d := s.ads[i].Detection
		d.Confirmed = s.ads[i].Sticky
		m.Ads = append(m.Ads, d)
	}
	s.dirty = false
	s.mu.Unlock()
	// Not cancelled by shutdown: the last store of a session happens during shutdown.
	if err := s.p.Maps.Put(context.WithoutCancel(s.p.ctx), m); err != nil {
		s.mu.Lock()
		s.dirty = true // try again on the next store
		s.mu.Unlock()
		slog.Warn("storing the ad map", "session", s.id, "key", m.Key, "err", err)
	}
}

// feed queues bytes for analysis. It never blocks the player: when the analyser falls
// behind, data is dropped and the demuxer re-syncs on the next offset it does get.
func (s *session) feed(off int64, p []byte) {
	if s.dead.Load() {
		return
	}
	s.sendMu.RLock() // keeps close() from closing the queue under a sender
	defer s.sendMu.RUnlock()
	if s.sendClosed {
		return
	}
	if s.dropping.Load() {
		// stay out of the way until the analyser is idle again, rather than stalling
		// the player once per chunk
		if s.queued.Load() > 0 {
			droppedBytes.Add(float64(len(p)))
			return
		}
		s.dropping.Store(false)
	}
	c := chunk(off, p)
	n := len(c.data)
	s.queued.Add(int64(n))
	queueBytes.Add(float64(n))
	select {
	case s.q <- c:
		return
	default:
	}
	if stall := s.p.conf().MaxStall; stall > 0 {
		start := time.Now()
		t := time.NewTimer(stall)
		defer t.Stop()
		select {
		case s.q <- c:
			stallSeconds.Add(time.Since(start).Seconds())
			return
		case <-t.C:
		}
		stallSeconds.Add(time.Since(start).Seconds())
	}
	c.release()
	s.queued.Add(-int64(n))
	queueBytes.Add(-float64(n))
	skipAhead.Inc()
	droppedBytes.Add(float64(n))
	s.dropping.Store(true)
	slog.Debug("analyser behind, skipping ahead", "session", s.id, "offset", off)
}

func (s *session) worker() {
	defer s.wg.Done()
	for c := range s.q {
		s.consume(c.off, c.data)
		s.queued.Add(-int64(len(c.data)))
		queueBytes.Add(-float64(len(c.data)))
		c.release()
	}
	if s.cont != nil {
		if err := s.cont.Close(); err != nil && !s.dead.Load() { // a demux error disables the session already
			slog.Warn("analysis ended with an error", "session", s.id, "err", err)
		}
	}
}

const (
	sniffHead = 1024     // enough of the file start for demux.Sniff to be conclusive
	sniffMax  = 16 << 20 // ... but give up if the player never reads the start at all
)

func (s *session) consume(off int64, data []byte) {
	if s.dead.Load() {
		return
	}
	// Demuxers parse untrusted bytes. A bug there must cost the analysis of this file,
	// not the process - and with it every stream the proxy is forwarding.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("demuxer panic", "session", s.id, "offset", off, "panic", r, "stack", string(debug.Stack()))
			s.disable("panic", fmt.Errorf("demuxer panic at offset %d: %v", off, r))
		}
	}()
	if s.cont == nil {
		s.seen += int64(len(data))
		if have := int64(len(s.sniff)); off <= have && off+int64(len(data)) > have {
			s.sniff = append(s.sniff, data[have-off:]...)
		}
		typ := demux.Sniff(s.sniff)
		if typ == "" {
			if len(s.sniff) >= sniffHead {
				s.disable("unknown_container", errors.New("container not recognised"))
			} else if s.seen > sniffMax {
				s.disable("no_head", errors.New("the player never read the start of the file"))
			}
			return
		}
		if !s.open(typ) {
			return
		}
		s.nextOff = int64(len(s.sniff))
		s.cont.Range(0)
		if err := s.cont.Write(s.sniff); err != nil {
			s.disable("demux_error", err)
			return
		}
		s.sniff = nil
		if end := off + int64(len(data)); end <= s.nextOff {
			return
		}
		if off < s.nextOff {
			data, off = data[s.nextOff-off:], s.nextOff
		}
	}
	if off != s.nextOff {
		s.cont.Range(off)
	}
	s.nextOff = off + int64(len(data))
	if err := s.cont.Write(data); err != nil {
		s.disable("demux_error", err)
		return
	}
	if d := s.cont.Duration(); d > 0 {
		s.mu.Lock()
		if d > s.duration {
			s.duration, s.dirty = d, true
		}
		s.mu.Unlock()
	}
}

func (s *session) open(typ string) bool {
	cfg := s.p.conf()
	s.sink = decode.NewSink(s.p.ctx, cfg.FFmpeg, s.p.Lib, cfg.MinScore, s.ring, s.onBlock, s.analysed)
	c, err := demux.New(typ, s.sink)
	if err != nil {
		s.disable("open_failed", err)
		return false
	}
	s.cont = c
	s.mu.Lock()
	s.ctype = typ
	s.mu.Unlock()
	containers.With(typ).Inc()
	slog.Info("container recognised", "session", s.id, "container", typ)
	return true
}

// disable stops analysis for this session; forwarding to the player is unaffected.
// reason is the analysisDisabled label.
func (s *session) disable(reason string, err error) {
	if !s.dead.Swap(true) {
		if s.p.ctx.Err() != nil {
			slog.Debug("analysis stopped by shutdown", "session", s.id)
		} else {
			analysisDisabled.With(reason).Inc()
			slog.Warn("analysis off", "session", s.id, "err", err)
		}
		if s.sink != nil {
			s.sink.Close()
		}
	}
}

// analysed reports whether this media time has already been covered, so the decoder can
// stay idle: a file whose ad map is known needs no analysis at all. While the file has an
// open report nothing counts as analysed: the reported ad is about to be marked, and
// /ads/mark needs its audio decoded.
func (s *session) analysed(t time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.reported && !s.marking && s.covered.Contains(t, t+detect.Block)
}

func (s *session) onBlock(b detect.BlockResult) {
	s.mu.Lock()
	s.covered = s.covered.Add(b.From, b.To)
	s.analysedTo = b.To
	s.used = time.Now()
	s.dirty = true
	var found []detect.Detection
	for _, d := range b.Detections {
		if s.addDetection(d, b.From, b.To) {
			found = append(found, d)
		}
	}
	s.confirm()
	s.mu.Unlock()
	// No store here: /ads/file answers from the live session, and the janitor writes the
	// map to disk (so do keyCheck, marks and close).
	for _, d := range found {
		detections.Inc()
		detectionScore.Observe(float64(d.Score))
		slog.Info("ad detected", "session", s.id, "ad", d.AdID, "type", d.Type, "label", d.Label, "start", d.Start, "end", d.End, "score", d.Score)
	}
}

// addDetection merges d into the session and tracks how many consecutive blocks agree on
// the alignment. It reports whether this is a new or moved detection.
func (s *session) addDetection(d detect.Detection, blockFrom, blockTo time.Duration) bool {
	var changed bool
	s.ads, _, changed = detect.MergeCandidate(s.ads, d, blockFrom, blockTo)
	return changed
}

// close ends the session; reason (idle, shutdown) is the sessionsClosed label.
func (s *session) close(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	sessionsClosed.With(reason).Inc()
	sessionsActive.Dec()
	s.sendMu.Lock()
	s.sendClosed = true
	close(s.q)
	s.sendMu.Unlock()
	s.wg.Wait() // the analyser drains the queue and its decoder: their blocks go in too
	s.store()
	s.ring.Release()
}
