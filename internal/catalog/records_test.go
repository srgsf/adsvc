package catalog

import (
	"context"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// node is an in-memory catalogue with an identity.
func node(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.SetIdentity(t.Context(), record.NewIdentity()); err != nil {
		t.Fatal(err)
	}
	return db
}

func enroll(t *testing.T, db *DB, seed int32, label string) fingerprint.ID {
	t.Helper()
	var pts []fingerprint.Point
	for i := range int32(90) {
		pts = append(pts, fingerprint.Point{H: uint32((seed*7919 + i*104729) & (1<<fingerprint.HashBits - 1)), T: i / 6})
	}
	b, err := fingerprint.Encode(pts)
	if err != nil {
		t.Fatal(err)
	}
	id := fingerprint.IDOf(b)
	if _, err := db.AddAd(t.Context(), Ad{ID: id, FPVersion: fingerprint.Version, Label: label, DurationMs: 30000,
		NPoints: len(pts), Created: time.Now()}, b); err != nil {
		t.Fatal(err)
	}
	return id
}

func records(t *testing.T, db *DB) []record.Record {
	t.Helper()
	es, err := db.Log(t.Context(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]record.Record, len(es))
	for i, e := range es {
		out[i] = e.Record
	}
	return out
}

func ingest(t *testing.T, db *DB, from *DB, recs []record.Record) IngestResult {
	t.Helper()
	res, err := db.Ingest(t.Context(), recs, from.Self())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func label(t *testing.T, db *DB, id fingerprint.ID) string {
	t.Helper()
	all, err := db.Ads(t.Context(), fingerprint.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.ID == id {
			return a.Label
		}
	}
	return "<missing>"
}

func tracked(t *testing.T, db *DB, id fingerprint.ID) bool {
	t.Helper()
	ts, err := db.Tracking(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(ts, func(a Ad) bool { return a.ID == id })
}

func TestSyncBetweenNodes(t *testing.T) {
	a, b := node(t), node(t)
	ad := enroll(t, a, 1, "brand x")
	if err := a.Label(t.Context(), ad, "Brand X (30 s)", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.PublishFileMap(t.Context(), &FileMap{Key: "ih:abc/1", FPVersion: fingerprint.Version, Size: 10,
		Detections: []Detection{{Ad: ad, StartMs: 1000, EndMs: 31000, Score: 90, Confirmed: true}}}); err != nil {
		t.Fatal(err)
	}
	recs := records(t, a)
	if len(recs) != 3 {
		t.Fatalf("%d records on a, want ad.add, ad.label, file.map", len(recs))
	}
	if res := ingest(t, b, a, recs); res.Accepted != 3 || len(res.Rejected) != 0 {
		t.Fatalf("ingest: %+v", res)
	}
	if res := ingest(t, b, a, recs); res.Duplicate != 3 || res.Accepted != 0 {
		t.Fatalf("second ingest: %+v", res)
	}
	if got := label(t, b, ad); got != "Brand X (30 s)" {
		t.Fatalf("label on b: %q", got)
	}
	all, _ := b.Ads(t.Context(), fingerprint.Version)
	if len(all) != 1 || all[0].Author != a.Self() {
		t.Fatalf("ads on b: %+v", all)
	}
	// a is unknown to b (weight 0.2): its ad is not tracked, and its detections are not reported.
	if tracked(t, b, ad) {
		t.Fatal("an unknown origin's ad is tracked")
	}
	if ds, _ := b.PeerAds(t.Context(), "ih:abc/1"); len(ds) != 0 {
		t.Fatalf("detections of an untrusted ad reported: %+v", ds)
	}
	w := 2.0
	if err := b.SetOrigins(t.Context(), []OriginRule{{Origin: a.Self(), Name: "a", Weight: &w}}); err != nil {
		t.Fatal(err)
	}
	if !tracked(t, b, ad) {
		t.Fatal("a trusted origin's ad is not tracked")
	}
	ds, err := b.PeerAds(t.Context(), "ih:abc/1")
	if err != nil || len(ds) != 1 || !ds[0].Confirmed || ds[0].Label != "Brand X (30 s)" || ds[0].StartMs != 1000 {
		t.Fatalf("peer ads: %+v, %v", ds, err)
	}
	// b's own word beats a's: b labels it, and votes it down.
	if err := b.Label(t.Context(), ad, "mine", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Label(t.Context(), ad, "later on a", ""); err != nil {
		t.Fatal(err)
	}
	ingest(t, b, a, records(t, a))
	if got := label(t, b, ad); got != "mine" {
		t.Fatalf("this node's label must win here: %q", got)
	}
	if err := b.Vote(t.Context(), record.AdVote{Ad: ad, Value: -1, Reason: record.ReasonNotAd}); err != nil {
		t.Fatal(err)
	}
	if tracked(t, b, ad) {
		t.Fatal("tracked after this node voted it down")
	}
	if err := b.Pin(t.Context(), ad, true); err != nil {
		t.Fatal(err)
	}
	if !tracked(t, b, ad) {
		t.Fatal("a pinned ad is not tracked")
	}
}

// The same records in any order give the same state.
func TestIngestOrderIndependent(t *testing.T) {
	a := node(t)
	ad := enroll(t, a, 2, "first")
	for i, l := range []string{"second", "third", "fourth"} {
		if err := a.Label(t.Context(), ad, l, ""); err != nil {
			t.Fatal(err)
		}
		if err := a.PublishFileMap(t.Context(), &FileMap{Key: "c:1", FPVersion: 1, Size: int64(i),
			Detections: []Detection{{Ad: ad, StartMs: int32(1000 * i), EndMs: 40000, Confirmed: true}}}); err != nil {
			t.Fatal(err)
		}
	}
	recs := records(t, a)
	w := 5.0
	for round := range 5 {
		shuffled := slices.Clone(recs)
		rand.New(rand.NewPCG(uint64(round), 1)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		b := node(t)
		if err := b.SetOrigins(t.Context(), []OriginRule{{Origin: a.Self(), Weight: &w}}); err != nil {
			t.Fatal(err)
		}
		for _, r := range shuffled {
			ingest(t, b, a, []record.Record{r})
		}
		if got := label(t, b, ad); got != "fourth" {
			t.Fatalf("round %d: label %q", round, got)
		}
		ds, _ := b.PeerAds(t.Context(), "c:1")
		if len(ds) != 1 || ds[0].StartMs != 2000 {
			t.Fatalf("round %d: the newest map must win: %+v", round, ds)
		}
	}
}

func TestIngestRejects(t *testing.T) {
	a, b, evil := node(t), node(t), node(t)
	enroll(t, a, 3, "x")
	good := records(t, a)[0]

	tampered := good
	tampered.Body = append([]byte(nil), good.Body...)
	tampered.Body[len(tampered.Body)-2] ^= 1
	if res := ingest(t, b, a, []record.Record{tampered}); res.Rejected[record.RejectSignature] != 1 {
		t.Fatalf("tampered: %+v", res)
	}

	// The same sequence number with other content (an origin trying to rewrite history).
	fork, _ := record.New(record.KindAdRetract, good.Seq, good.TS, record.AdRetract{Ad: fingerprint.ID{9}})
	a.Identity().Sign(&fork)
	ingest(t, b, a, []record.Record{good})
	if res := ingest(t, b, a, []record.Record{fork}); res.Rejected[record.RejectInvalid] != 1 {
		t.Fatalf("reused seq: %+v", res)
	}

	// A blocked origin.
	enroll(t, evil, 4, "spam")
	if err := b.SetOrigins(t.Context(), []OriginRule{{Origin: evil.Self(), Blocked: true}}); err != nil {
		t.Fatal(err)
	}
	if res := ingest(t, b, evil, records(t, evil)); res.Rejected[record.RejectBlocked] != 1 {
		t.Fatalf("blocked: %+v", res)
	}

	// A quota.
	c := node(t)
	pol := DefaultPolicy
	pol.QuotaPerDay = 2
	c.SetPolicy(pol)
	for i := range 3 {
		enroll(t, evil, int32(10+i), "flood")
	}
	if res := ingest(t, c, evil, records(t, evil)); res.Accepted != 2 || res.Rejected[record.RejectQuota] != 2 {
		t.Fatalf("quota: %+v", res)
	}
	enroll(t, evil, 13, "flood")
	rs := records(t, evil)
	if res := ingest(t, c, evil, rs[len(rs)-1:]); res.Rejected[record.RejectQuota] != 1 { // counted from the table
		t.Fatalf("quota in a later batch: %+v", res)
	}

	// A kind this version does not know: kept (and relayed), not applied.
	u, _ := record.New("ad.rating", 99, time.Now().UnixMilli(), map[string]int{"stars": 5})
	a.Identity().Sign(&u)
	if res := ingest(t, b, a, []record.Record{u}); res.Accepted != 1 {
		t.Fatalf("unknown kind: %+v", res)
	}
}

func TestRetract(t *testing.T) {
	a, b := node(t), node(t)
	w := 3.0
	if err := b.SetOrigins(t.Context(), []OriginRule{{Origin: a.Self(), Weight: &w}}); err != nil {
		t.Fatal(err)
	}
	ad := enroll(t, a, 5, "x")
	ingest(t, b, a, records(t, a))
	if !tracked(t, b, ad) {
		t.Fatal("not tracked before the retraction")
	}
	// b cannot retract a's ad: it votes it down instead.
	if ok, err := b.Retract(t.Context(), ad); err != nil || !ok {
		t.Fatal(err)
	}
	rs := records(t, b)
	if last := rs[len(rs)-1]; last.Kind != record.KindAdVote {
		t.Fatalf("retracting another node's ad wrote %s", last.Kind)
	}
	// a retracts its own ad; that reaches every node.
	c := node(t)
	if err := c.SetOrigins(t.Context(), []OriginRule{{Origin: a.Self(), Weight: &w}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.Retract(t.Context(), ad); err != nil || !ok {
		t.Fatal(err)
	}
	ingest(t, c, a, records(t, a))
	if tracked(t, c, ad) || tracked(t, a, ad) {
		t.Fatal("tracked after its author retracted it")
	}
	// A retraction by someone other than the author does not count.
	d := node(t)
	if err := d.SetOrigins(t.Context(), []OriginRule{{Origin: a.Self(), Weight: &w}}); err != nil {
		t.Fatal(err)
	}
	ingest(t, d, a, records(t, a)[:1]) // only the ad.add
	fake, _ := record.New(record.KindAdRetract, 1, time.Now().UnixMilli(), record.AdRetract{Ad: ad})
	b.Identity().Sign(&fake)
	ingest(t, d, b, []record.Record{fake})
	if !tracked(t, d, ad) {
		t.Fatal("a retraction by a non-author removed the ad")
	}
}

// Peer detections of one ad from several origins are one, reported if their weights add up.
func TestPeerAdsMerge(t *testing.T) {
	a, b, c, me := node(t), node(t), node(t), node(t)
	ad := enroll(t, a, 6, "x")
	for _, n := range []*DB{b, c} {
		ingest(t, n, a, records(t, a)[:1])
	}
	for i, n := range []*DB{a, b, c} {
		if err := n.PublishFileMap(t.Context(), &FileMap{Key: "c:7", FPVersion: 1,
			Detections: []Detection{{Ad: ad, StartMs: int32(5000 + 100*i), EndMs: 35000, Score: 50 + i, Confirmed: true}}}); err != nil {
			t.Fatal(err)
		}
	}
	half := 0.5
	if err := me.SetOrigins(t.Context(), []OriginRule{{Origin: a.Self(), Weight: new(2.0)}, {Origin: b.Self(), Weight: &half}, {Origin: c.Self(), Weight: &half}}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*DB{a, b, c} {
		ingest(t, me, n, records(t, n))
	}
	ds, err := me.PeerAds(t.Context(), "c:7")
	if err != nil || len(ds) != 1 || !ds[0].Confirmed || ds[0].Score != 52 {
		t.Fatalf("merged: %+v, %v", ds, err)
	}
	pol := DefaultPolicy
	pol.FileMapMin = 3.5 // a, b and c together have 3
	me.SetPolicy(pol)
	if ds, _ := me.PeerAds(t.Context(), "c:7"); len(ds) != 0 {
		t.Fatalf("reported below FileMapMin: %+v", ds)
	}
	pol.FileMapMin = 2.5
	me.SetPolicy(pol)
	if ds, _ := me.PeerAds(t.Context(), "c:7"); len(ds) != 1 {
		t.Fatalf("not reported although the weights add up: %+v", ds)
	}
}

func TestPublishFileMapOnlyChanges(t *testing.T) {
	a := node(t)
	ad := enroll(t, a, 7, "x")
	m := &FileMap{Key: "c:9", FPVersion: 1, Analyzed: [][2]int32{{0, 10000}},
		Detections: []Detection{{Ad: ad, StartMs: 1000, EndMs: 31000, Confirmed: true}, {Ad: fingerprint.ID{2}, StartMs: 50000, EndMs: 60000}}}
	before := len(records(t, a))
	for range 3 {
		m.Analyzed = append(m.Analyzed, [2]int32{10000, 20000}) // analysis progresses
		if err := a.PublishFileMap(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	rs := records(t, a)
	if len(rs) != before+1 {
		t.Fatalf("%d file.map records for one unchanged set of confirmed ads", len(rs)-before)
	}
	body, _ := record.Decode(&rs[len(rs)-1])
	fm := body.(*record.FileMap)
	if len(fm.Ads) != 1 || len(fm.Analyzed) != 0 {
		t.Fatalf("published %+v: want only the confirmed ad, no ranges", fm)
	}
	if day := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli(); rs[len(rs)-1].TS != day {
		t.Fatalf("file.map time %d, want the start of the day", rs[len(rs)-1].TS)
	}
	if err := a.PublishFileMap(t.Context(), &FileMap{Key: "c:10", FPVersion: 1,
		Detections: []Detection{{Ad: ad, StartMs: 1, EndMs: 2}}}); err != nil {
		t.Fatal(err)
	}
	if len(records(t, a)) != before+1 {
		t.Fatal("a map with no confirmed ads was published")
	}
}

// A node that gets its own records back (a reinstall with the same identity, from a
// snapshot) never reuses their sequence numbers.
func TestOwnRecordsBack(t *testing.T) {
	a := node(t)
	for i := range 3 {
		enroll(t, a, int32(20+i), "x")
	}
	mine := records(t, a)
	fresh, err := Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.SetIdentity(t.Context(), a.Identity()); err != nil {
		t.Fatal(err)
	}
	if res := ingest(t, fresh, a, mine); res.Accepted != 3 {
		t.Fatalf("own records back: %+v", res)
	}
	enroll(t, fresh, 30, "new")
	rs := records(t, fresh)
	if last := rs[len(rs)-1]; last.Seq != 4 {
		t.Fatalf("new record after own records came back has seq %d, want 4", last.Seq)
	}
}

// Ads stored before a node had an identity become its own, and are published.
func TestSetIdentityPublishesPending(t *testing.T) {
	db, err := Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ps []fingerprint.Point
	for i := range int32(60) {
		ps = append(ps, fingerprint.Point{H: uint32(i * 131), T: i})
	}
	pts, _ := fingerprint.Encode(ps)
	a := Ad{ID: fingerprint.IDOf(pts), FPVersion: fingerprint.Version, Label: "old", DurationMs: 3000, NPoints: len(ps), Created: time.Now()}
	if _, err := db.PutAd(t.Context(), a, pts); err != nil {
		t.Fatal(err)
	}
	tiny, tp := testAd(t, "too small to publish", 1) // stays local, does not stop anything
	if _, err := db.PutAd(t.Context(), tiny, tp); err != nil {
		t.Fatal(err)
	}
	if err := db.SetShare(context.Background(), Share{}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetIdentity(t.Context(), record.NewIdentity()); err != nil {
		t.Fatal(err)
	}
	if n := len(records(t, db)); n != 0 {
		t.Fatalf("%d records published with sharing off", n)
	}
	if err := db.SetShare(t.Context(), ShareAll); err != nil {
		t.Fatal(err)
	}
	rs := records(t, db)
	if len(rs) != 1 || rs[0].Kind != record.KindAdAdd || rs[0].Origin != db.Self() {
		t.Fatalf("records after sharing ads: %+v", rs)
	}
	all, _ := db.Ads(t.Context(), fingerprint.Version)
	for _, x := range all {
		if x.Author != db.Self() {
			t.Fatalf("local ad %s is not this node's", x.Label)
		}
	}
}

// A node restores its own export into a new catalogue: its records are applied (its file
// maps too, unless it has a newer one), and its next record follows the imported ones.
func TestImportOwnSnapshot(t *testing.T) {
	id := record.NewIdentity()
	db, err := Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetIdentity(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	x, y := enroll(t, db, 1, "x"), enroll(t, db, 2, "y")
	if err := db.Label(t.Context(), y, "why", ""); err != nil {
		t.Fatal(err)
	}
	fm := func(key string, alias string, start int32) *FileMap {
		return &FileMap{Key: key, Aliases: []string{alias}, FPVersion: fingerprint.Version, Size: 10, DurationMs: 60000,
			Detections: []Detection{{Ad: x, StartMs: start, EndMs: start + 30000, Score: 50, Confirmed: true}}}
	}
	for _, m := range []*FileMap{fm("c:1", "ih:abc/1", 1000), fm("c:1", "ih:abc/1", 2000), fm("c:2", "ih:def/1", 3000)} {
		if err := db.PublishFileMap(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "snap.db")
	if err := db.Snapshot(t.Context(), path); err != nil {
		t.Fatal(err)
	}

	back, err := Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if err := back.SetIdentity(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	live := fm("c:2", "ih:def/1", 9000) // a map made since: kept
	if err := back.PutFileMap(t.Context(), live); err != nil {
		t.Fatal(err)
	}
	if res, err := back.ImportSnapshot(t.Context(), path); err != nil || res.Accepted != 6 {
		t.Fatalf("import: %+v, %v", res, err)
	}
	for key, start := range map[string]int32{"ih:abc/1": 2000, "c:2": 9000} {
		m, err := back.FileMap(t.Context(), key)
		if err != nil || m == nil || len(m.Detections) != 1 || m.Detections[0].StartMs != start {
			t.Fatalf("map of %s after the import: %+v, %v", key, m, err)
		}
	}
	if err := back.PublishFileMap(t.Context(), fm("c:1", "ih:abc/1", 2000)); err != nil {
		t.Fatal(err)
	}
	ads, err := back.Ads(t.Context(), fingerprint.Version)
	if err != nil {
		t.Fatal(err)
	}
	got := map[fingerprint.ID]string{}
	for _, a := range ads {
		got[a.ID] = a.Label
	}
	if len(got) != 2 || got[x] != "x" || got[y] != "why" {
		t.Fatalf("restored ads %v", got)
	}
	enroll(t, back, 3, "z")
	es, err := back.Log(t.Context(), 0, 10)
	if err != nil || len(es) != 7 || es[6].Seq != 7 { // the restored map was not published again
		t.Fatalf("log after a new ad: %d records, %v", len(es), err)
	}
}

// An author who withdrew an ad adds it again: the retraction is overtaken here and on a
// peer, whatever order the records arrive in.
func TestReAddAfterRetract(t *testing.T) {
	a := node(t)
	id := enroll(t, a, 1, "first")
	if _, err := a.Retract(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if tracked(t, a, id) {
		t.Fatal("retracted ad is tracked")
	}
	if enroll(t, a, 1, "again") != id {
		t.Fatal("the same audio must be the same ad")
	}
	if !tracked(t, a, id) {
		t.Fatal("re-added ad is not tracked")
	}
	recs := records(t, a)
	for name, order := range map[string]func(){"forward": func() {}, "reversed": func() { slices.Reverse(recs) }} {
		order()
		b := node(t)
		w := 2.0
		if err := b.SetOrigins(t.Context(), []OriginRule{{Origin: a.Self(), Name: "a", Weight: &w}}); err != nil {
			t.Fatal(err)
		}
		for _, r := range recs { // one by one: the order is what is tested
			if _, err := b.Ingest(t.Context(), []record.Record{r}, a.Self()); err != nil {
				t.Fatal(name, err)
			}
		}
		if !tracked(t, b, id) {
			t.Fatalf("%s: a peer does not track the re-added ad", name)
		}
	}
}
