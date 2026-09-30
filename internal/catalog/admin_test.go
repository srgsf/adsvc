package catalog

import (
	"errors"
	"slices"
	"testing"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

func adRow(t *testing.T, db *DB, id fingerprint.ID) AdRow {
	t.Helper()
	rows, _, err := db.AdPage(t.Context(), AdQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("ad %s not listed", id)
	return AdRow{}
}

func TestAdRowsAndDetail(t *testing.T) {
	a, b := node(t), node(t)
	good := enroll(t, a, 1, "good")
	bad := enroll(t, a, 2, "bad")
	if err := a.PublishFileMap(t.Context(), &FileMap{Key: "ih:abc/1", FPVersion: fingerprint.Version, Size: 10,
		Detections: []Detection{{Ad: good, StartMs: 1000, EndMs: 31000, Score: 90, Confirmed: true}}}); err != nil {
		t.Fatal(err)
	}
	ingest(t, b, a, records(t, a))
	w := 2.0
	if err := b.SetOrigins(t.Context(), []OriginRule{{Origin: a.Self(), Name: "a", Weight: &w}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Vote(t.Context(), record.AdVote{Ad: bad, Value: -1, Reason: record.ReasonNotAd}); err != nil {
		t.Fatal(err)
	}
	if err := b.PutFileMap(t.Context(), &FileMap{Key: "c:1", FPVersion: fingerprint.Version,
		Detections: []Detection{{Ad: good, StartMs: 0, EndMs: 30000, Score: 50, Confirmed: true}}}); err != nil {
		t.Fatal(err)
	}

	g := adRow(t, b, good)
	if !tracked(t, b, good) || g.State != "" || g.Files != 2 || g.Confirmations != 2 || g.LastSeen.IsZero() || g.Author != a.Self() {
		t.Fatalf("good: %+v", g)
	}
	// author 2 + a's confirmation 1 + b's own 5
	if g.Trust != 8 {
		t.Fatalf("good: trust %v, want 8", g.Trust)
	}
	x := adRow(t, b, bad)
	if tracked(t, b, bad) || x.Down != 1 || x.Up != 0 || x.Trust != 2-10 {
		t.Fatalf("bad: %+v", x)
	}

	if err := b.Dup(t.Context(), bad, good); err != nil {
		t.Fatal(err)
	}
	if err := b.Dup(t.Context(), bad, fingerprint.ID{1}); !errors.Is(err, ErrNoAd) {
		t.Fatalf("dup of an unknown ad: %v", err)
	}
	if err := b.Label(t.Context(), bad, "really bad", ""); err != nil {
		t.Fatal(err)
	}
	if r := adRow(t, b, bad); r.State != StateDup || r.Canonical != good || r.Label != "really bad" {
		t.Fatalf("after dup: %+v", r)
	}

	d, err := b.AdDetail(t.Context(), bad)
	if err != nil || d == nil {
		t.Fatalf("detail: %v", err)
	}
	if len(d.Votes) != 1 || d.Votes[0].Origin != b.Self() || d.Votes[0].Weight != 10 || d.Votes[0].Reason != record.ReasonNotAd {
		t.Fatalf("votes: %+v", d.Votes)
	}
	if len(d.Labels) != 1 || d.Labels[0].Label != "really bad" {
		t.Fatalf("labels: %+v", d.Labels)
	}
	if len(d.Dups) != 1 || d.Dups[0].Canonical != good {
		t.Fatalf("dups: %+v", d.Dups)
	}
	var kinds []string
	for _, h := range d.History {
		kinds = append(kinds, h.Kind)
		if h.Kind == record.KindAdAdd && (h.Via != a.Self() || h.Origin != a.Self() || h.Received.IsZero()) {
			t.Fatalf("ad.add: via %s origin %s", h.Via.Short(), h.Origin.Short())
		}
	}
	if want := []string{record.KindAdLabel, record.KindAdDup, record.KindAdVote, record.KindAdAdd}; !equal(kinds, want) {
		t.Fatalf("history %v, want %v", kinds, want)
	}
	// The canonical ad's history has the dup claim too.
	gd, err := b.AdDetail(t.Context(), good)
	if err != nil || len(gd.Sightings) != 2 || len(gd.Dups) != 1 {
		t.Fatalf("good detail: %+v, %v", gd, err)
	}
	if d, err := b.AdDetail(t.Context(), fingerprint.ID{9}); d != nil || err != nil {
		t.Fatalf("unknown ad: %v, %v", d, err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOriginsAndPolicy(t *testing.T) {
	a, b := node(t), node(t)
	good := enroll(t, a, 1, "good")
	bad := enroll(t, a, 2, "bad")
	gone := enroll(t, a, 3, "gone")
	if _, err := a.Retract(t.Context(), gone); err != nil {
		t.Fatal(err)
	}
	ingest(t, b, a, records(t, a))
	if err := b.Vote(t.Context(), record.AdVote{Ad: bad, Value: -1, Reason: record.ReasonBoundary}); err != nil {
		t.Fatal(err)
	}
	if err := b.Vote(t.Context(), record.AdVote{Ad: good, Value: 1, Reason: record.ReasonGood}); err != nil {
		t.Fatal(err)
	}
	os, err := b.Origins(t.Context())
	if err != nil || len(os) != 2 {
		t.Fatalf("origins: %+v, %v", os, err)
	}
	if !os[0].Self || os[0].Votes != 2 || os[0].Weight != 10 {
		t.Fatalf("self: %+v", os[0])
	}
	o := os[1]
	if o.Origin != a.Self() || o.Records != 4 || o.Ads != 3 || o.Trashed != 1 || o.Retracted != 1 || o.Weight != 0.2 || o.Own != nil {
		t.Fatalf("a: %+v", o)
	}

	w := 3.0
	if err := b.SetOriginPolicy(t.Context(), OriginRule{Origin: a.Self(), Name: "alice", Weight: &w}); err != nil {
		t.Fatal(err)
	}
	if os, _ = b.Origins(t.Context()); os[1].Weight != 3 || os[1].Name != "alice" || os[1].Managed {
		t.Fatalf("after set: %+v", os[1])
	}
	if err := b.SetOriginPolicy(t.Context(), OriginRule{Origin: b.Self(), Blocked: true}); !errors.Is(err, ErrSelf) {
		t.Fatalf("policy for self: %v", err)
	}
	if err := b.ClearOriginPolicy(t.Context(), a.Self()); err != nil {
		t.Fatal(err)
	}
	if os, _ = b.Origins(t.Context()); os[1].Weight != 0.2 {
		t.Fatalf("after clear: %+v", os[1])
	}
	if err := b.SetOrigins(t.Context(), []OriginRule{{Origin: a.Self(), Blocked: true}}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetOriginPolicy(t.Context(), OriginRule{Origin: a.Self()}); !errors.Is(err, ErrManaged) {
		t.Fatalf("set a managed origin: %v", err)
	}
	if err := b.ClearOriginPolicy(t.Context(), a.Self()); !errors.Is(err, ErrManaged) {
		t.Fatalf("clear a managed origin: %v", err)
	}
	if r := adRow(t, b, good); r.State != StateBlocked || tracked(t, b, good) {
		t.Fatalf("ad of a blocked origin: %+v", r)
	}
	if r := adRow(t, b, gone); r.State != StateRetracted {
		t.Fatalf("retracted and blocked: %+v", r)
	}
}

func TestPeersAndPushers(t *testing.T) {
	a, b := node(t), node(t)
	bad := enroll(t, a, 1, "bad")
	enroll(t, a, 2, "fine")
	res := ingest(t, b, a, records(t, a))
	if err := b.SavePeer(t.Context(), PeerState{Name: "alice", NodeID: a.Self(), PullCursor: 2}, nil, res); err != nil {
		t.Fatal(err)
	}
	if err := b.Vote(t.Context(), record.AdVote{Ad: bad, Value: -1, Reason: record.ReasonNotAd}); err != nil {
		t.Fatal(err)
	}
	pusher := record.NewIdentity().Origin
	for range 2 {
		if err := b.CountPush(t.Context(), pusher, IngestResult{Accepted: 1, Duplicate: 2, Rejected: map[string]int{"quota": 3}}); err != nil {
			t.Fatal(err)
		}
	}
	peers, pushers, err := b.Peers(t.Context())
	if err != nil || len(peers) != 1 || len(pushers) != 1 {
		t.Fatalf("peers %+v, pushers %+v, %v", peers, pushers, err)
	}
	p := peers[0]
	if p.Name != "alice" || p.NodeID != a.Self() || p.PullCursor != 2 || p.Received != 2 || p.LastOK.IsZero() ||
		p.Relayed != 2 || p.RelayedTrash != 1 {
		t.Fatalf("peer: %+v", p)
	}
	q := pushers[0]
	if q.NodeID != pusher || q.Received != 2 || q.Duplicate != 4 || q.Rejected["quota"] != 6 || q.LastPush.IsZero() {
		t.Fatalf("pusher: %+v", q)
	}
}

func TestFiles(t *testing.T) {
	a, b := node(t), node(t)
	ad := enroll(t, a, 1, "x")
	if err := a.PublishFileMap(t.Context(), &FileMap{Key: "ih:abc/1", FPVersion: fingerprint.Version, Size: 10, DurationMs: 60000,
		Detections: []Detection{{Ad: ad, StartMs: 1000, EndMs: 31000, Score: 90, Confirmed: true}}}); err != nil {
		t.Fatal(err)
	}
	ingest(t, b, a, records(t, a))
	if err := b.PutFileMap(t.Context(), &FileMap{Key: "ih:abc/1", FPVersion: fingerprint.Version, DurationMs: 60000,
		Analyzed: [][2]int32{{0, 20000}, {30000, 40000}}}); err != nil {
		t.Fatal(err)
	}
	if err := b.PutFileMap(t.Context(), &FileMap{Key: "c:2", FPVersion: fingerprint.Version}); err != nil {
		t.Fatal(err)
	}
	fs, total, err := b.Files(t.Context(), "", 0, 0)
	if err != nil || len(fs) != 2 || total != 2 {
		t.Fatalf("files %+v (%d), %v", fs, total, err)
	}
	var f FileRow
	for _, x := range fs {
		if x.Key == "ih:abc/1" {
			f = x
		}
	}
	if !f.Own || f.AnalyzedMs != 30000 || f.Origins != 1 || f.PeerAds != 1 || f.Ads != 0 || f.DurationMs != 60000 {
		t.Fatalf("file: %+v", f)
	}
	if fs, total, _ := b.Files(t.Context(), "abc", 0, 0); len(fs) != 1 || total != 1 {
		t.Fatalf("search: %+v", fs)
	}
	first, _, _ := b.Files(t.Context(), "", 0, 1)
	second, total, _ := b.Files(t.Context(), "", 1, 1)
	if len(first) != 1 || len(second) != 1 || first[0].Key == second[0].Key || total != 2 {
		t.Fatalf("pages: %+v, %+v", first, second)
	}
	dets, err := b.PeerDetections(t.Context(), "ih:abc/1")
	if err != nil || len(dets) != 1 || dets[0].Origin != a.Self() {
		t.Fatalf("peer detections %+v, %v", dets, err)
	}
	ls, err := b.Labels(t.Context(), []fingerprint.ID{ad, {7}})
	if err != nil || len(ls) != 1 || ls[ad] != "x" {
		t.Fatalf("labels %v, %v", ls, err)
	}
}

func TestAdPage(t *testing.T) {
	a, b := node(t), node(t)
	peer := enroll(t, a, 1, "Реклама Банка")
	ingest(t, b, a, records(t, a))
	if err := b.SetOrigins(t.Context(), []OriginRule{{Origin: a.Self(), Name: "Friend"}}); err != nil {
		t.Fatal(err)
	}
	x, y := enroll(t, b, 2, "beta"), enroll(t, b, 3, "Alpha")
	for i, id := range []fingerprint.ID{peer, x, y} { // created in this order, a second apart
		if _, err := b.w.ExecContext(t.Context(), `UPDATE ads SET created = ? WHERE id = ?`, 1_800_000_000_000+i*1000, id[:]); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Pin(t.Context(), y, true); err != nil {
		t.Fatal(err)
	}
	if err := b.Vote(t.Context(), record.AdVote{Ad: x, Value: -1, Reason: record.ReasonNotAd}); err != nil {
		t.Fatal(err)
	}
	if err := b.Dup(t.Context(), x, y); err != nil {
		t.Fatal(err)
	}
	ids := func(rows []AdRow) []fingerprint.ID {
		var out []fingerprint.ID
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	yes, no := true, false
	for _, c := range []struct {
		name  string
		q     AdQuery
		want  []fingerprint.ID
		total int
	}{
		{"newest first", AdQuery{}, []fingerprint.ID{y, x, peer}, 3},
		{"by label", AdQuery{Sort: SortLabel}, []fingerprint.ID{y, x, peer}, 3},
		{"label, any case", AdQuery{Q: "реклама"}, []fingerprint.ID{peer}, 1},
		{"id prefix", AdQuery{Q: x.String()[:9]}, []fingerprint.ID{x}, 1},
		{"author name", AdQuery{Author: "friend"}, []fingerprint.ID{peer}, 1},
		{"author prefix", AdQuery{Author: b.Self().String()[:6]}, []fingerprint.ID{y, x}, 2},
		{"pinned", AdQuery{Pinned: &yes}, []fingerprint.ID{y}, 1},
		{"not pinned", AdQuery{Pinned: &no, Sort: SortLabel}, []fingerprint.ID{x, peer}, 2},
		{"other version", AdQuery{FPVersion: fingerprint.Version + 1}, nil, 0},
		{"second page", AdQuery{Sort: SortLabel, Offset: 1, Limit: 1}, []fingerprint.ID{x}, 3},
	} {
		rows, total, err := b.AdPage(t.Context(), c.q)
		if err != nil || total != c.total || !slices.Equal(ids(rows), c.want) {
			t.Errorf("%s: %v (%d), %v; want %v (%d)", c.name, ids(rows), total, err, c.want, c.total)
		}
	}
	// the standing of a page's rows is theirs
	rows, _, err := b.AdPage(t.Context(), AdQuery{Sort: SortLabel, Offset: 1, Limit: 1})
	if err != nil || rows[0].Down != 1 || rows[0].State != StateDup || rows[0].Canonical != y || rows[0].CanonicalLabel != "Alpha" {
		t.Fatalf("x on its own page: %+v, %v", rows, err)
	}
	rows, _, _ = b.AdPage(t.Context(), AdQuery{Pinned: &yes})
	if !rows[0].Pinned || len(rows[0].DupedBy) != 1 || rows[0].DupedBy[0] != (AdRef{x, "beta"}) || rows[0].Down != 0 {
		t.Fatalf("y: %+v", rows[0])
	}
}
