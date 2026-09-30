package replica

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

type testNode struct {
	db  *catalog.DB
	srv *httptest.Server
}

func newNode(t *testing.T) *testNode {
	t.Helper()
	db, err := catalog.Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.SetIdentity(t.Context(), record.NewIdentity()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(t.Context(), db, "n", t.TempDir(), nil)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &testNode{db, srv}
}

func (n *testNode) peer(name string, pull, push bool) Peer {
	return Peer{Name: name, URL: n.srv.URL, Pull: pull, Push: push}
}

func enroll(t *testing.T, db *catalog.DB, seed int32) fingerprint.ID {
	t.Helper()
	var pts []fingerprint.Point
	for i := range int32(90) {
		pts = append(pts, fingerprint.Point{H: uint32((seed*7919 + i*104729) & (1<<fingerprint.HashBits - 1)), T: i / 6})
	}
	b, _ := fingerprint.Encode(pts)
	id := fingerprint.IDOf(b)
	if _, err := db.AddAd(t.Context(), catalog.Ad{ID: id, FPVersion: fingerprint.Version, Label: "x", DurationMs: 30000,
		NPoints: len(pts), Created: time.Now()}, b); err != nil {
		t.Fatal(err)
	}
	return id
}

func head(t *testing.T, db *catalog.DB) int64 {
	t.Helper()
	h, err := db.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func hasAd(t *testing.T, db *catalog.DB, id fingerprint.ID) bool {
	t.Helper()
	all, err := db.Ads(t.Context(), fingerprint.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.ID == id {
			return true
		}
	}
	return false
}

func TestPullAndPush(t *testing.T) {
	a, b := newNode(t), newNode(t)
	for i := range 1500 { // more than one page of the log
		enroll(t, a.db, int32(i))
	}
	mine := enroll(t, b.db, 99999)

	changed := make(chan struct{}, 1)
	sy := NewSyncer(t.Context(), b.db, nil, func(context.Context) error {
		select {
		case changed <- struct{}{}:
		default:
		}
		return nil
	})
	if err := sy.Round(t.Context(), a.peer("a", true, true)); err != nil {
		t.Fatal(err)
	}
	if got := head(t, b.db); got != 1501 {
		t.Fatalf("b holds %d records, want 1500 from a and its own", got)
	}
	if !hasAd(t, a.db, mine) {
		t.Fatal("b's ad was not pushed to a")
	}
	st, _ := b.db.Peer(t.Context(), "a")
	if st.NodeID != a.db.Self() || st.PullCursor != 1500 || st.PushCursor != 1501 {
		t.Fatalf("peer state %+v", st)
	}
	// a counts what b pushed, against b.
	if _, pushers, err := a.db.Peers(t.Context()); err != nil || len(pushers) != 1 || pushers[0].NodeID != b.db.Self() ||
		pushers[0].Received != 1 || len(pushers[0].Rejected) != 0 {
		t.Fatalf("pushers on a: %+v, %v", pushers, err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh after records arrived")
	}

	// Nothing new: the next round moves nothing (a's own records are not pushed back).
	before := head(t, a.db)
	if err := sy.Round(t.Context(), a.peer("a", true, true)); err != nil {
		t.Fatal(err)
	}
	if head(t, a.db) != before || head(t, b.db) != 1501 {
		t.Fatalf("a second round moved records: a %d, b %d", head(t, a.db), head(t, b.db))
	}
}

// A node that answers /sync/info with another node's id cannot sign for it.
func TestInfoMustBeSigned(t *testing.T) {
	a, b := newNode(t), newNode(t)
	liar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(a.srv.URL + r.URL.String())
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		var info Info
		_ = json.NewDecoder(resp.Body).Decode(&info)
		info.NodeID = b.db.Self() // claims to be b
		writeJSON(w, info)
	}))
	defer liar.Close()
	sy := NewSyncer(t.Context(), newNode(t).db, nil, nil)
	err := sy.Round(t.Context(), Peer{Name: "liar", URL: liar.URL, Pull: true})
	if !errors.Is(err, ErrNotPeer) {
		t.Fatalf("round with a lying peer: %v", err)
	}
}

func TestPushNeedsASignature(t *testing.T) {
	a, b := newNode(t), newNode(t)
	enroll(t, b.db, 1)
	es, _ := b.db.Log(t.Context(), 0, 10)
	body := record.AppendFrame(nil, &es[0].Record)
	id := b.db.Identity()
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	old := strconv.FormatInt(time.Now().Add(-time.Hour).UnixMilli(), 10)
	for _, c := range []struct {
		name       string
		node, date string
		sig        []byte
		code       int
	}{
		{"signed", id.Origin.String(), now, id.SignBytes(pushMessage(now, body)), http.StatusOK},
		{"unsigned", id.Origin.String(), now, nil, http.StatusUnauthorized},
		{"signed by someone else", id.Origin.String(), now, record.NewIdentity().SignBytes(pushMessage(now, body)), http.StatusUnauthorized},
		{"stale", id.Origin.String(), old, id.SignBytes(pushMessage(old, body)), http.StatusBadRequest},
	} {
		req, _ := http.NewRequest(http.MethodPost, a.srv.URL+"/sync/push", bytes.NewReader(body))
		req.Header.Set(hdrNode, c.node)
		req.Header.Set(hdrDate, c.date)
		req.Header.Set(hdrSig, base64.StdEncoding.EncodeToString(c.sig))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.code)
		}
	}
}

func TestSnapshot(t *testing.T) {
	a := newNode(t)
	ad := enroll(t, a.db, 5)
	if err := a.db.PublishFileMap(t.Context(), &catalog.FileMap{Key: "c:1", FPVersion: 1,
		Detections: []catalog.Detection{{Ad: ad, StartMs: 0, EndMs: 30000, Confirmed: true}}}); err != nil {
		t.Fatal(err)
	}
	// a's local analysis (its watch history) must not travel.
	if err := a.db.PutFileMap(t.Context(), &catalog.FileMap{Key: "ih:secret/1", FPVersion: 1}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(a.srv.URL + "/sync/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snap.db")
	b, _ := readAll(resp)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := catalog.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if maps, _ := snap.FileMaps(t.Context()); len(maps) != 0 {
		t.Fatalf("the snapshot holds this node's file maps: %+v", maps)
	}
	if ads, _ := snap.Ads(t.Context(), fingerprint.Version); len(ads) != 0 {
		t.Fatal("the snapshot holds derived tables")
	}
	if h := head(t, snap); h != 2 {
		t.Fatalf("snapshot has %d records, want 2", h)
	}
	snap.Close()

	c := newNode(t)
	res, err := c.db.ImportSnapshot(t.Context(), path)
	if err != nil || res.Accepted != 2 {
		t.Fatalf("import: %+v, %v", res, err)
	}
	if !hasAd(t, c.db, ad) {
		t.Fatal("imported ad missing")
	}
}

// A peer that starts over (a new catalogue, or a new identity) is synced from the start.
func TestPeerReset(t *testing.T) {
	a, b := newNode(t), newNode(t)
	enroll(t, a.db, 1)
	enroll(t, a.db, 2)
	sy := NewSyncer(t.Context(), b.db, nil, nil)
	if err := sy.Round(t.Context(), a.peer("a", true, false)); err != nil {
		t.Fatal(err)
	}
	a2 := newNode(t) // same name, another node
	fresh := enroll(t, a2.db, 3)
	if err := sy.Round(t.Context(), Peer{Name: "a", URL: a2.srv.URL, Pull: true}); err != nil {
		t.Fatal(err)
	}
	if !hasAd(t, b.db, fresh) {
		t.Fatal("the new node behind the peer name was not synced from the start")
	}
}

// The loops of SetPeers stop when the Syncer's context ends.
func TestSyncerStops(t *testing.T) {
	a, b := newNode(t), newNode(t)
	enroll(t, a.db, 1)
	ctx, cancel := context.WithCancel(t.Context())
	sy := NewSyncer(ctx, b.db, nil, nil)
	sy.SetPeers([]Peer{{Name: "a", URL: a.srv.URL, Pull: true, Interval: 10 * time.Millisecond}})
	deadline := time.Now().Add(5 * time.Second)
	for head(t, b.db) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if head(t, b.db) == 0 {
		t.Fatal("the loop did not pull")
	}
	cancel()
	done := make(chan struct{})
	go func() { sy.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loops outlived their context")
	}
}

func readAll(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	var b bytes.Buffer
	_, err := b.ReadFrom(resp.Body)
	return b.Bytes(), err
}

// A snapshot is written once per state of the log: asked again, the same file is served.
func TestSnapshotCached(t *testing.T) {
	db, err := catalog.Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetIdentity(t.Context(), record.NewIdentity()); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	s := NewServer(t.Context(), db, "n", tmp, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func() []byte {
		t.Helper()
		resp, err := http.Get(srv.URL + "/sync/snapshot")
		if err != nil {
			t.Fatal(err)
		}
		b, err := readAll(resp)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("snapshot: %d, %v", resp.StatusCode, err)
		}
		return b
	}
	dirs := func() int {
		es, _ := os.ReadDir(tmp)
		return len(es)
	}
	enroll(t, db, 1)
	first := get()
	s.snapMu.Lock()
	built := s.snap
	s.snapMu.Unlock()
	if !bytes.Equal(get(), first) || s.snap != built || dirs() != 1 {
		t.Fatal("the same log was snapshotted again")
	}
	enroll(t, db, 2)
	get()
	if s.snap == built || dirs() != 1 {
		t.Fatalf("a new record did not replace the snapshot (%d snapshot dirs)", dirs())
	}
}
