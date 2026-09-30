package replica

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/record"
)

// Peer is a node this one syncs with (sync.peers in the config file).
type Peer struct {
	Name     string
	URL      string // adsvc base URL of the peer; /sync/... is appended
	Token    string // a user token on the peer, if its /sync/* needs one (never stored in the catalogue)
	Pull     bool   // pull its log
	Push     bool   // push this node's log to it
	Interval time.Duration
}

const (
	pushBatch       = 500
	defaultInterval = 10 * time.Minute
)

// Syncer runs a loop per peer: pull, then push, every Interval. It lives as long as the
// context it was made with.
type Syncer struct {
	ctx     context.Context
	db      *catalog.DB
	client  *http.Client
	refresh *debouncer

	mu    sync.Mutex
	loops map[string]*loop
	wg    sync.WaitGroup
}

type loop struct {
	peer   Peer
	cancel context.CancelFunc
}

// NewSyncer syncs db with peers (see SetPeers) until ctx ends; changed is called
// (debounced) after a pull brought records.
func NewSyncer(ctx context.Context, db *catalog.DB, client *http.Client, changed func(context.Context) error) *Syncer {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Syncer{ctx: ctx, db: db, client: client, refresh: newDebouncer(ctx, 2*time.Second, changed), loops: map[string]*loop{}}
}

// SetPeers makes peers the ones synced with: loops of peers that are gone or changed stop,
// loops of new or changed ones start (their first round right away).
func (s *Syncer) SetPeers(peers []Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]Peer{}
	for _, p := range peers {
		want[p.Name] = p
	}
	for name, l := range s.loops {
		if p, ok := want[name]; !ok || p != l.peer {
			l.cancel()
			delete(s.loops, name)
		}
	}
	for name, p := range want {
		if _, ok := s.loops[name]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(s.ctx)
		s.loops[name] = &loop{peer: p, cancel: cancel}
		s.wg.Go(func() { s.run(ctx, p) })
	}
	syncPeers.Set(float64(len(s.loops)))
}

// Wait waits for the loops to stop (after the Syncer's context ended).
func (s *Syncer) Wait() { s.wg.Wait() }

func (s *Syncer) run(ctx context.Context, p Peer) {
	every := p.Interval
	if every <= 0 {
		every = defaultInterval
	}
	for {
		if err := s.Round(ctx, p); err != nil && ctx.Err() == nil {
			slog.Warn("sync failed", "peer", p.Name, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Round syncs with p once: it pulls p's log (if p.Pull), then pushes this node's (if
// p.Push). Progress is saved as it goes, so a failed round resumes where it stopped.
func (s *Syncer) Round(ctx context.Context, p Peer) (err error) {
	start := time.Now()
	defer func() {
		if ctx.Err() != nil {
			return // stopped, not failed
		}
		syncRoundSeconds.With(p.Name).Since(start)
		if err != nil {
			syncRounds.With(p.Name, "error").Inc()
			return
		}
		syncRounds.With(p.Name, "ok").Inc()
		syncLastSuccess.With(p.Name).Set(float64(time.Now().UnixMilli()) / 1e3)
	}()
	st, err := s.db.Peer(ctx, p.Name)
	if err != nil {
		return err
	}
	var total catalog.IngestResult
	err = s.round(ctx, p, &st, &total)
	if serr := s.db.SavePeer(ctx, st, err, total); serr != nil && err == nil {
		err = serr
	}
	if total.Accepted > 0 {
		s.refresh.trigger()
	}
	if err == nil {
		slog.Debug("synced", "peer", p.Name, "accepted", total.Accepted, "duplicate", total.Duplicate, "rejected", total.Rejected)
	}
	return err
}

func (s *Syncer) round(ctx context.Context, p Peer, st *catalog.PeerState, total *catalog.IngestResult) error {
	total.Rejected = map[string]int{}
	info, err := s.info(ctx, p)
	if err != nil {
		return err
	}
	if info.NodeID == s.db.Self() {
		return errors.New("the peer is this node")
	}
	if st.NodeID != (record.Origin{}) && st.NodeID != info.NodeID {
		slog.Warn("peer has a new identity; syncing from the start", "peer", p.Name, "was", st.NodeID.Short(), "now", info.NodeID.Short())
		st.PullCursor, st.PushCursor = 0, 0
	}
	if info.Head < st.PullCursor {
		slog.Warn("peer's log is shorter than what was pulled; syncing from the start", "peer", p.Name)
		st.PullCursor = 0
	}
	st.NodeID = info.NodeID
	if p.Pull {
		if err := s.pullAll(ctx, p, info, st, total); err != nil {
			return err
		}
		syncPullLag.With(p.Name).Set(max(0, float64(info.Head)-float64(st.PullCursor)))
	}
	if p.Push {
		return s.pushAll(ctx, p, info, st)
	}
	return nil
}

// pullAll pulls the peer's log up to its head, ingesting page by page and saving the
// cursor after each.
func (s *Syncer) pullAll(ctx context.Context, p Peer, info Info, st *catalog.PeerState, total *catalog.IngestResult) error {
	for st.PullCursor < info.Head {
		es, err := s.pull(ctx, p, st.PullCursor)
		if err != nil || len(es) == 0 {
			return err
		}
		recs := make([]record.Record, len(es))
		for i, e := range es {
			recs[i] = e.Record
		}
		res, err := s.db.Ingest(ctx, recs, info.NodeID)
		if err != nil {
			return err
		}
		add(total, res)
		syncRecords.With(p.Name, "pull").Add(float64(len(recs)))
		st.PullCursor = es[len(es)-1].ID
		if err := s.db.SavePeer(ctx, *st, nil, catalog.IngestResult{}); err != nil {
			return err
		}
	}
	return nil
}

// pushAll pushes this node's log after the push cursor, batch by batch, leaving out what
// came from the peer itself.
func (s *Syncer) pushAll(ctx context.Context, p Peer, info Info, st *catalog.PeerState) error {
	for {
		es, err := s.db.Log(ctx, st.PushCursor, pushBatch)
		if err != nil || len(es) == 0 {
			return err
		}
		var body []byte
		n := 0
		for i := range es {
			if es[i].Via != info.NodeID { // what came from the peer goes back to it no more
				body = record.AppendFrame(body, &es[i].Record)
				n++
			}
		}
		if len(body) > 0 {
			if err := s.push(ctx, p, body); err != nil {
				return err
			}
			syncRecords.With(p.Name, "push").Add(float64(n))
		}
		st.PushCursor = es[len(es)-1].ID
		if err := s.db.SavePeer(ctx, *st, nil, catalog.IngestResult{}); err != nil {
			return err
		}
	}
}

func add(total *catalog.IngestResult, r catalog.IngestResult) {
	total.Accepted += r.Accepted
	total.Duplicate += r.Duplicate
	for k, n := range r.Rejected {
		total.Rejected[k] += n
	}
}

func (s *Syncer) request(ctx context.Context, p Peer, method, path string, q url.Values, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(strings.TrimRight(p.URL, "/") + path)
	if err != nil {
		return nil, err
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	return req, nil
}

func (s *Syncer) do(req *http.Request) (*http.Response, error) {
	resp, err := s.client.Do(req) //nolint:gosec // G704: the peer URLs come from this node's config
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Path, resp.Status, bytes.TrimSpace(b))
	}
	return resp, nil
}

// info asks the peer who it is, and checks that it signs as that node.
func (s *Syncer) info(ctx context.Context, p Peer) (Info, error) {
	nonce := newNonce()
	req, err := s.request(ctx, p, http.MethodGet, "/sync/info", url.Values{"nonce": {hex.EncodeToString(nonce)}}, nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := s.do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	var info Info
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
		return Info{}, fmt.Errorf("sync info: %w", err)
	}
	if !record.VerifyBytes(info.NodeID, infoMessage(nonce, info.NodeID), info.Sig) {
		return Info{}, ErrNotPeer
	}
	return info, nil
}

func (s *Syncer) pull(ctx context.Context, p Peer, after int64) ([]catalog.Entry, error) {
	req, err := s.request(ctx, p, http.MethodGet, "/sync/log",
		url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(maxLogPage)}}, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	es, err := readEntries(resp.Body, maxLogPage)
	if err != nil {
		return nil, err
	}
	if len(es) > 0 && es[0].ID <= after {
		return nil, fmt.Errorf("log entry %d is not after %d", es[0].ID, after)
	}
	return es, nil
}

// push sends records, signed as this node, gzipped.
func (s *Syncer) push(ctx context.Context, p Peer, body []byte) error {
	id := s.db.Identity()
	if id == nil {
		return errors.New("no identity to push with")
	}
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	_, _ = zw.Write(body)
	_ = zw.Close()
	req, err := s.request(ctx, p, http.MethodPost, "/sync/push", nil, &z)
	if err != nil {
		return err
	}
	date := strconv.FormatInt(time.Now().UnixMilli(), 10)
	req.Header.Set("Content-Type", typeRecords)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set(hdrNode, hexID(id.Origin))
	req.Header.Set(hdrDate, date)
	req.Header.Set(hdrSig, base64.StdEncoding.EncodeToString(id.SignBytes(pushMessage(date, body))))
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var res catalog.IngestResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&res); err != nil {
		return fmt.Errorf("push result: %w", err)
	}
	if len(res.Rejected) > 0 {
		slog.Warn("peer rejected records", "peer", p.Name, "rejected", res.Rejected)
	}
	return nil
}
