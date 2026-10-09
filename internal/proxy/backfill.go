package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/srgsf/adsvc/internal/demux"
	"github.com/srgsf/adsvc/internal/filekey"
	"github.com/srgsf/adsvc/internal/metrics"
)

// Backfill: the analyser needs the start of the file (to identify the container and the
// audio track) and, for AVI and progressive MP4, the index at its end. A player that starts
// in the middle of a file usually has both cached, or does not need them, and never sends
// them through the proxy. When they have not shown up after a grace period, the session
// reads them itself with one extra request to the source, once per file.
const (
	backfillHeadBytes = 256 << 10
	backfillTailBytes = 16 << 20
	backfillTimeout   = 2 * time.Minute
)

// Grace periods before a backfill; variables so that tests can shorten them.
var (
	headWait = time.Second
	tailWait = 10 * time.Second
)

var backfills = metrics.NewCounterVec("adsvc_backfills_total",
	"Extra reads of a file's head or index by the proxy, because the player did not send them, by kind (head, tail) and result (ok, error).", "kind", "result")

// source is how to reach the file's upstream again: the resolved URL, the config that
// resolved it and the headers of the player's request that carry the source's credentials.
// It lives in memory only.
type source struct {
	cfg *Config
	url *url.URL
	hdr http.Header
}

// setSource remembers how to reach the file's source (the latest request wins, so that
// refreshed credentials are used).
func (s *session) setSource(cfg *Config, up *url.URL, hdr http.Header) {
	h := hdr.Clone()
	for _, k := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since"} {
		h.Del(k)
	}
	s.mu.Lock()
	s.src = &source{cfg: cfg, url: up, hdr: h}
	s.mu.Unlock()
}

// armHead schedules a read of the file head, unless the analyser has found it by then.
func (s *session) armHead() {
	if s.headArmed.CompareAndSwap(false, true) {
		s.later(headWait, "head", func() bool { return s.opened.Load() })
	}
}

// armTail schedules a read of the file's end when the container needs an index from there.
func (s *session) armTail() {
	if s.tailArmed.CompareAndSwap(false, true) {
		s.later(tailWait, "tail", s.indexed.Load)
	}
}

// later runs the backfill of kind after d, unless done reports that it is not needed.
func (s *session) later(d time.Duration, kind string, done func() bool) {
	s.mu.Lock()
	if s.closed || s.detached || s.stop == nil {
		s.mu.Unlock()
		return
	}
	s.bg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.bg.Done()
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-s.stop.Done():
			return
		case <-t.C:
		}
		if done() || s.dead.Load() || !s.wantsAnalysis() {
			return
		}
		if err := s.backfill(kind); err != nil && s.stop.Err() == nil {
			backfills.With(kind, "error").Inc()
			slog.Warn("reading the file ourselves failed", "session", s.id, "what", kind, "err", err)
			return
		}
		backfills.With(kind, "ok").Inc()
	}()
}

// wantsAnalysis is false for a file whose ad map says everything has been analysed and
// nobody is marking: its decoder stays idle, so the head and the index are not needed.
func (s *session) wantsAnalysis() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reported || s.marking || s.duration <= 0 {
		return true
	}
	return !s.covered.Contains(0, s.duration)
}

// backfill reads the head or the tail of the file from the source and feeds it to the
// analyser like the player's own reads.
func (s *session) backfill(kind string) error {
	s.mu.Lock()
	src, size := s.src, s.size
	s.mu.Unlock()
	if src == nil {
		return fmt.Errorf("no source known")
	}
	from, to := int64(0), int64(backfillHeadBytes-1)
	if kind == "tail" {
		if size <= 0 {
			return fmt.Errorf("file size unknown")
		}
		from, to = max(0, size-backfillTailBytes), size-1
	}
	ctx, cancel := context.WithTimeout(s.stop, backfillTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.url.String(), nil) //nolint:gosec // G704: the URL passed the allow-list when the player asked for it
	if err != nil {
		return err
	}
	req.Header = src.hdr.Clone()
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := src.cfg.fetch(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	upstreamRequests.With(statusClass(resp.StatusCode)).Inc()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("source answered %d to a range request", resp.StatusCode) // a 200 would be the whole file
	}
	off, _ := bodyRange(resp)
	slog.Debug("reading the file ourselves", "session", s.id, "what", kind, "offset", off, "upstream", filekey.RedactURL(src.url))
	buf := make([]byte, copyBuf)
	for {
		n, err := io.ReadFull(resp.Body, buf)
		if n > 0 {
			if !s.feedWhenIdle(ctx, off, buf[:n]) {
				return nil
			}
			off += int64(n)
		}
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		if kind == "tail" && s.indexed.Load() {
			return nil // the index is in: no need for the rest
		}
	}
}

// feedWhenIdle queues p once the analyser has caught up, so that these bytes (which the
// player is not waiting for) are not dropped like a burst would be.
func (s *session) feedWhenIdle(ctx context.Context, off int64, p []byte) bool {
	for s.queued.Load() > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	if s.dead.Load() {
		return false
	}
	s.kb.Feed(off, p)
	s.feed(off, p)
	return true
}

// refreshIndexed records whether the container has the index it needs (worker only).
func (s *session) refreshIndexed() {
	if ix, ok := s.cont.(demux.Indexed); ok {
		s.indexed.Store(ix.Indexed())
	}
}
