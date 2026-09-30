package library

import (
	"cmp"
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/mediatime"
)

// testAd is a synthetic ad: its landmarks and the ID they hash to.
type testAd struct {
	ID     fingerprint.ID
	Points []fingerprint.Point
	Dur    time.Duration
}

func newTestAd(pts []fingerprint.Point, dur time.Duration) testAd {
	b, err := fingerprint.Encode(pts)
	if err != nil {
		panic(err)
	}
	pts, _ = fingerprint.Decode(b) // canonical order, as the index sees them
	return testAd{fingerprint.IDOf(b), pts, dur}
}

// synthAds returns n ads of 20–45 s with synthetic landmarks shaped like real ones
// (the ads of the Babylon bench): one anchor per frame with fanOut pairs, dt roughly
// geometric with mean ~4, and a density falling towards high bins. Real bucket sizes are
// skewed the same way, which is what matters for lookup cost.
func synthAds(n int, seed uint64) []testAd {
	r := rand.New(rand.NewPCG(seed, 0))
	ads := make([]testAd, n)
	for i := range ads {
		frames := int(time.Duration((20+25*r.Float64())*float64(time.Second)) / fingerprint.FrameDur)
		ads[i] = newTestAd(synthPoints(r, 0, frames), time.Duration(frames)*fingerprint.FrameDur)
	}
	return ads
}

func synthPoints(r *rand.Rand, t0, frames int) []fingerprint.Point {
	pts := make([]fingerprint.Point, 0, frames*6)
	for t := t0; t < t0+frames; t++ {
		f1 := synthBin(r)
		for range 6 {
			dt := min(1+int(r.ExpFloat64()*3.5), 63)
			h := uint32(f1)<<15 | uint32(synthBin(r))<<6 | uint32(dt)
			pts = append(pts, fingerprint.Point{H: h, T: int32(t)})
		}
	}
	return pts
}

// synthBin draws a spectral bin in [4, 500) with a density falling towards high bins.
func synthBin(r *rand.Rand) int {
	return 4 + int(496*(1-math.Pow(1-r.Float64(), 1/2.5)))
}

// query is a 10 s block: frames [from, from+312) of ad a when a is not nil, re-anchored at
// frame 0 and mixed with as many noise landmarks, or noise only.
func query(r *rand.Rand, a *testAd, from int32) []fingerprint.Point {
	const frames = 312
	q := synthPoints(r, 0, frames)
	if a == nil {
		return q
	}
	for _, p := range a.Points {
		if p.T >= from && p.T < from+frames {
			q = append(q, fingerprint.Point{H: p.H, T: p.T - from})
		}
	}
	return q
}

// refMatch is the original map index and vote, kept as the oracle for Match.
func refMatch(ads []testAd, q []fingerprint.Point, minScore int) []Match {
	type k struct {
		ad  fingerprint.ID
		off int32
	}
	idx := map[uint32][]k{}
	for _, a := range ads {
		for _, p := range a.Points {
			idx[p.H] = append(idx[p.H], k{a.ID, p.T})
		}
	}
	votes := map[k]int{}
	for _, p := range q {
		for _, o := range idx[p.H] {
			votes[k{o.ad, o.off - p.T}]++
		}
	}
	var out []Match
	for key, v := range votes {
		prev, next := votes[k{key.ad, key.off - 1}], votes[k{key.ad, key.off + 1}]
		if prev >= v || next > v {
			continue
		}
		if s := v + prev + next; s >= minScore {
			out = append(out, Match{AdID: key.ad, Offset: key.off, Score: s})
		}
	}
	sortMatches(out)
	return out
}

func sortMatches(ms []Match) {
	slices.SortFunc(ms, func(a, b Match) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), compareID(a.AdID, b.AdID), cmp.Compare(a.Offset, b.Offset))
	})
}

func checkMatch(t *testing.T, l *Library, live []testAd, r *rand.Rand) {
	t.Helper()
	for i := range 20 {
		var a *testAd
		from := int32(0)
		if i%4 != 0 && len(live) > 0 {
			a = &live[r.IntN(len(live))]
			from = int32(r.IntN(len(a.Points)/6 + 1))
		}
		q := query(r, a, from)
		got := l.Match(q, 3)
		sortMatches(got)
		want := refMatch(live, q, 3)
		if !slices.Equal(got, want) {
			t.Fatalf("query %d: Match differs from the map index:\n got %d matches %v\nwant %d matches %v",
				i, len(got), head(got), len(want), head(want))
		}
	}
}

func head(ms []Match) []Match { return ms[:min(len(ms), 5)] }

// store puts ads into db as enrolled ads.
func store(t testing.TB, db *catalog.DB, ads []testAd) {
	t.Helper()
	for i, a := range ads {
		b, _ := fingerprint.Encode(a.Points)
		meta := catalog.Ad{ID: a.ID, FPVersion: fingerprint.Version, Label: a.ID.Short(), DurationMs: mediatime.Ms(a.Dur),
			NPoints: len(a.Points), Created: time.UnixMilli(int64(1_700_000_000_000 + i))}
		if _, err := db.PutAd(context.Background(), meta, b); err != nil {
			t.Fatal(err)
		}
	}
}

func openCatalog(t testing.TB, path string) *catalog.DB {
	t.Helper()
	db, err := catalog.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// newLib is a library of ads over a new catalogue; csrPath "" keeps the index on the heap.
func newLib(t testing.TB, ads []testAd, csrPath string) *Library {
	t.Helper()
	dbPath := ""
	if csrPath != "" {
		dbPath = filepath.Join(filepath.Dir(csrPath), "catalogue.db")
	}
	db := openCatalog(t, dbPath)
	store(t, db, ads)
	l, err := Open(context.Background(), db, csrPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// add stores a like Add does, without fingerprinting PCM.
func add(t *testing.T, l *Library, a testAd) {
	t.Helper()
	if _, err := l.addPoints(context.Background(), "", "", a.Points, a.Dur, nil); err != nil {
		t.Fatal(err)
	}
	l.bgWG.Wait() // a rebuild it started
}

func forceRebuild(t *testing.T, l *Library) {
	t.Helper()
	c, err := l.build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.install(c)
	l.mu.Unlock()
}

// live is the ads of all that are still in l.
func live(l *Library, all []testAd) []testAd {
	var out []testAd
	for _, a := range all {
		if l.Get(a.ID) != nil {
			out = append(out, a)
		}
	}
	return out
}

func index(t *testing.T) string { return filepath.Join(t.TempDir(), "tracking.csr") }

func TestMatchAgreesWithMapIndex(t *testing.T) {
	ads := synthAds(40, 7)
	for _, path := range []string{"", index(t)} {
		checkMatch(t, newLib(t, ads, path), ads, rand.New(rand.NewPCG(1, 2)))
	}
}

func TestCSRHoldsEveryLandmark(t *testing.T) {
	ads := synthAds(25, 3)
	slices.SortFunc(ads, func(a, b testAd) int { return compareID(a.ID, b.ID) })
	ids, src := heapSource(ads)
	c, dropped, err := buildCSR(ids, src, nil)
	if err != nil || dropped != 0 {
		t.Fatalf("buildCSR: dropped %d, %v", dropped, err)
	}
	if err := errors.Join(c.validate(), c.validatePostings()); err != nil {
		t.Fatalf("a fresh index does not validate: %v", err)
	}
	want := map[uint32][]uint32{}
	n := 0
	for d, a := range ads {
		for _, p := range a.Points {
			want[p.H] = append(want[p.H], uint32(d)<<tBits|uint32(p.T))
			n++
		}
	}
	if len(c.post) != n || len(c.low) != len(want) || len(c.off) != len(want)+1 {
		t.Fatalf("sizes: %d postings, %d/%d distinct hashes; want %d, %d", len(c.post), len(c.low), len(c.off), n, len(want))
	}
	for h, ps := range want {
		if got := c.bucket(h); !slices.Equal(got, ps) {
			t.Fatalf("hash %#x: postings %v, want %v (in ad, frame order)", h, got, ps)
		}
	}
	for _, h := range []uint32{0, 1<<fingerprint.HashBits - 1, 1 << fingerprint.HashBits, 1<<32 - 1} {
		if _, ok := want[h]; !ok && c.bucket(h) != nil {
			t.Errorf("hash %#x: postings for a hash no ad has", h)
		}
	}
}

func TestEmptyLibrary(t *testing.T) {
	for _, path := range []string{"", index(t)} {
		l := newLib(t, nil, path)
		if ms := l.Match(query(rand.New(rand.NewPCG(1, 1)), nil, 0), 1); len(ms) != 0 {
			t.Fatalf("empty library matched: %v", ms)
		}
		if err := l.Remove(t.Context(), fingerprint.ID{1}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Remove on an empty library: %v", err)
		}
	}
}

func TestOverlayAndTombstones(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	base := synthAds(30, 11)
	l := newLib(t, base, index(t))
	for _, i := range []int{3, 17, 29} { // tombstones in the CSR
		if err := l.Remove(t.Context(), base[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	extra := synthAds(8, 12)
	for _, a := range extra { // overlay
		add(t, l, a)
	}
	if err := l.Remove(t.Context(), extra[2].ID); err != nil { // an overlay ad
		t.Fatal(err)
	}
	if l.deadN == 0 || l.overlayN == 0 {
		t.Fatalf("want both tombstones and an overlay: dead %d, overlay %d postings", l.deadN, l.overlayN)
	}
	all := append(slices.Clone(base), extra...)
	checkMatch(t, l, live(l, all), r)

	forceRebuild(t, l)
	if l.dead != nil || l.overlayN != 0 || len(l.csr.ids) != len(l.ads) || len(l.fresh) != 0 {
		t.Fatalf("after a rebuild: dead %v, overlay %d postings, %d/%d ads in the CSR, %d fresh",
			l.dead != nil, l.overlayN, len(l.csr.ids), len(l.ads), len(l.fresh))
	}
	checkMatch(t, l, live(l, all), r)
}

func TestRebuildWhenStale(t *testing.T) {
	base := synthAds(4, 13)
	l := newLib(t, base, index(t))
	extra := synthAds(12, 14) // about 70k postings: over the 64k minimum
	for _, a := range extra {
		add(t, l, a)
	}
	if len(l.csr.ids) <= 4 {
		t.Fatalf("stale overlay not rebuilt: %d postings, %d ads in the CSR", l.overlayN, len(l.csr.ids))
	}
	all := append(slices.Clone(base), extra...)
	for _, a := range all[:14] {
		if err := l.Remove(t.Context(), a.ID); err != nil {
			t.Fatal(err)
		}
		l.bgWG.Wait()
	}
	// The rebuild happens once dead postings pass the minimum; later removes are tombstones.
	if len(l.csr.ids) >= 16 || l.deadN > 1<<16 {
		t.Fatalf("stale tombstones not rebuilt: %d ads in the CSR, %d dead postings", len(l.csr.ids), l.deadN)
	}
	checkMatch(t, l, live(l, all), rand.New(rand.NewPCG(1, 1)))
}

// Match runs while ads are added and removed and the CSR is rebuilt (run with -race).
func TestMatchDuringRebuild(t *testing.T) {
	base := synthAds(20, 15)
	l := newLib(t, base, index(t))
	extra := synthAds(40, 16)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := range 4 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(g), 0))
			for {
				select {
				case <-stop:
					return
				default:
				}
				l.Match(query(r, &extra[r.IntN(len(extra))], 0), 5)
			}
		})
	}
	for i, a := range extra {
		add(t, l, a)
		if i%3 == 0 {
			if err := l.Remove(t.Context(), base[i/3].ID); err != nil {
				t.Error(err)
			}
		}
		if i%10 == 9 {
			forceRebuild(t, l)
		}
	}
	close(stop)
	wg.Wait()
	checkMatch(t, l, live(l, append(slices.Clone(base), extra...)), rand.New(rand.NewPCG(2, 2)))
}

func TestAddSameLandmarksTwice(t *testing.T) {
	l := newLib(t, nil, "")
	a := synthAds(1, 30)[0]
	first, err := l.addPoints(t.Context(), "first", "", a.Points, a.Dur, &catalog.Source{Key: "ih:ab/1", StartMs: 5000, EndMs: 35000})
	if err != nil {
		t.Fatal(err)
	}
	again, err := l.addPoints(t.Context(), "again", "", a.Points, a.Dur, nil)
	if err != nil || again != first || len(l.List()) != 1 || first.Label != "first" {
		t.Fatalf("second add: %+v, %v; %d ads", again, err, len(l.List()))
	}
	if first.ID != a.ID || first.Source == nil || first.Source.StartMs != 5000 {
		t.Fatalf("stored ad %+v", first)
	}
	introPts := synthAds(1, 33)[0]
	intro, err := l.addPoints(t.Context(), "", "intro", introPts.Points, 30*time.Second, nil)
	if err != nil || intro.Type != "intro" || intro.Label != "intro-"+intro.ID.Short() || intro.Info().Type != "intro" {
		t.Fatalf("intro %+v, %v", intro, err)
	}
	if _, err := l.addPoints(t.Context(), "", "credits", synthAds(1, 34)[0].Points, 30*time.Second, nil); !errors.Is(err, ErrBadType) {
		t.Fatalf("unknown type: %v", err)
	}
	unnamed, err := l.addPoints(t.Context(), "", "", synthAds(1, 31)[0].Points, 30*time.Second, nil)
	if err != nil || unnamed.Label != "ad-"+unnamed.ID.Short() {
		t.Fatalf("default label %q, %v", unnamed.Label, err)
	}
}

func TestFind(t *testing.T) {
	ads := synthAds(3, 32)
	l := newLib(t, ads, "")
	for _, a := range ads {
		for _, q := range []string{a.ID.String(), a.ID.Short(), a.ID.String()[:5]} {
			got, err := l.Find(q)
			if err != nil || got.ID != a.ID {
				t.Fatalf("Find(%q) = %v, %v", q, got, err)
			}
		}
	}
	if _, err := l.Find("zz"); err == nil {
		t.Error("Find of a non-hex id succeeded")
	}
	if _, err := l.Find(""); err == nil {
		t.Error("Find of an empty id succeeded")
	}
}

func TestLimits(t *testing.T) {
	l := newLib(t, nil, "")
	long := int(mediatime.SamplesAt(MaxDuration, fingerprint.SampleRate)) + 1
	if _, err := l.Add(t.Context(), "long", "", make([]float32, long), nil); !errors.Is(err, ErrTooLong) {
		t.Fatalf("Add of a %d-sample ad: %v, want ErrTooLong", long, err)
	}
	// Landmarks outside the posting layout are left out of the index.
	a := newTestAd([]fingerprint.Point{{H: 7, T: 0}, {H: 7, T: tMask}, {H: 7, T: tMask + 1}}, 600*time.Second)
	c, dropped, err := buildCSR([]fingerprint.ID{a.ID}, func(fn func(int, []fingerprint.Point) error) error {
		return fn(0, a.Points)
	}, nil)
	if err != nil || dropped != 1 || len(c.bucket(7)) != 2 {
		t.Fatalf("buildCSR: dropped %d, %d postings for hash 7, %v; want 1 dropped, 2 postings", dropped, len(c.bucket(7)), err)
	}
}

// The map fallback for indexes too big for a dense tally votes the same.
func TestMatchMapFallback(t *testing.T) {
	defer func(n uint64) { maxCells = n }(maxCells)
	maxCells = 0
	ads := synthAds(30, 8)
	l := newLib(t, ads, "")
	if err := l.Remove(t.Context(), ads[4].ID); err != nil {
		t.Fatal(err)
	}
	checkMatch(t, l, live(l, ads), rand.New(rand.NewPCG(9, 9)))
}

// Offsets at both ends of a tally run, and queries whose frames start above zero.
func TestMatchAtAdEdges(t *testing.T) {
	ads := synthAds(3, 21)
	l := newLib(t, ads, "")
	r := rand.New(rand.NewPCG(4, 4))
	for _, a := range ads {
		last := a.Points[len(a.Points)-1].T
		for _, from := range []int32{-300, -1, 0, last - 5, last} {
			var q []fingerprint.Point
			for _, p := range a.Points {
				if p.T >= from && p.T < from+312 {
					q = append(q, fingerprint.Point{H: p.H, T: p.T - from + 1000})
				}
			}
			q = append(q, synthPoints(r, 1000, 312)...)
			got := l.Match(q, 1)
			sortMatches(got)
			if want := refMatch(ads, q, 1); !slices.Equal(got, want) {
				t.Fatalf("ad %s from %d: got %v, want %v", a.ID.Short(), from, head(got), head(want))
			}
		}
	}
}

// An index file is reused while the ads are unchanged, and rebuilt when they change.
func TestIndexFileReuse(t *testing.T) {
	path := index(t)
	ads := synthAds(10, 40)
	db := openCatalog(t, filepath.Join(filepath.Dir(path), "catalogue.db"))
	store(t, db, ads)
	open := func() *Library {
		l, err := Open(t.Context(), db, path)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	built := func() int64 {
		c, h, err := openCSR(path)
		if err != nil {
			t.Fatal(err)
		}
		c.release()
		return h.builtAt
	}
	l := open()
	first := built()
	l.Close()
	time.Sleep(2 * time.Millisecond)
	l = open()
	if built() != first {
		t.Fatal("index rebuilt although the ads did not change")
	}
	checkMatch(t, l, ads, rand.New(rand.NewPCG(3, 3)))
	l.Close()

	store(t, db, synthAds(1, 41))
	l = open()
	defer l.Close()
	if built() == first || len(l.csr.ids) != 11 {
		t.Fatalf("index not rebuilt for a new ad: %d ads", len(l.csr.ids))
	}
}

// An odd number of postings (the postings section is padded in the file) round-trips.
func TestIndexFileOddPostings(t *testing.T) {
	ads := synthAds(3, 60)
	ads[0] = newTestAd(ads[0].Points[:len(ads[0].Points)-1], ads[0].Dur) // one landmark less: an odd total
	path := index(t)
	l := newLib(t, ads, path)
	if n := len(l.csr.post); n%2 == 0 {
		t.Fatalf("%d postings: the test needs an odd number", n)
	}
	c, _, err := openCSR(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.release()
	if err := errors.Join(c.validate(), c.validatePostings()); err != nil || len(c.post) != len(l.csr.post) {
		t.Fatalf("reopened: %d postings (built %d), %v", len(c.post), len(l.csr.post), err)
	}
	checkMatch(t, l, ads, rand.New(rand.NewPCG(8, 8)))
}

// A damaged index file is detected and rebuilt, never used.
func TestIndexFileCorrupt(t *testing.T) {
	path := index(t)
	ads := synthAds(6, 42)
	db := openCatalog(t, filepath.Join(filepath.Dir(path), "catalogue.db"))
	store(t, db, ads)
	l, err := Open(t.Context(), db, path)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(6, 6))
	for i := range 40 {
		b := slices.Clone(good)
		switch i % 3 {
		case 0: // flip bytes anywhere past the header
			for range 1 + r.IntN(8) {
				b[headerSize+r.IntN(len(b)-headerSize)] ^= byte(1 + r.IntN(255))
			}
		case 1: // truncate
			b = b[:r.IntN(len(b))]
		case 2: // a posting pointing past its ad
			l := layoutFor(uint64(len(ads)), 0, 0)
			b[l.post+uint64(r.IntN(100))*4+1] = 0xff
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		l, err := Open(t.Context(), db, path)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		l.bgWG.Wait() // the postings are checked (and a damaged index rebuilt) in the background
		checkMatch(t, l, ads, r)
		l.Close()
	}
}

// Whatever bytes the index file holds, opening and using it must not crash: it is
// rejected, or it validates and Match stays in bounds.
func FuzzIndexFile(f *testing.F) {
	dir := f.TempDir()
	path := filepath.Join(dir, "tracking.csr")
	ads := synthAds(2, 50)
	for i := range ads {
		ads[i] = newTestAd(ads[i].Points[:120], 1)
	}
	db := openCatalog(f, filepath.Join(dir, "catalogue.db"))
	store(f, db, ads)
	l, err := Open(context.Background(), db, path)
	if err != nil {
		f.Fatal(err)
	}
	l.Close()
	good, _ := os.ReadFile(path)
	f.Add(good)
	q := query(rand.New(rand.NewPCG(1, 1)), &ads[0], 0)
	f.Fuzz(func(t *testing.T, b []byte) {
		p := filepath.Join(t.TempDir(), "x.csr")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		c, _, err := openCSR(p)
		if err != nil {
			return
		}
		defer c.release()
		if c.validate() != nil {
			return
		}
		c.match(q, nil, 1, nil)
	})
}

// A policy change that drops an ad still in the overlay takes it out of Get and Match; one
// that brings it back puts it in again.
func TestRefreshDropsExcludedOverlayAd(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	l := newLib(t, synthAds(5, 21), "")
	a := synthAds(1, 22)[0]
	add(t, l, a)
	if l.overlayN == 0 {
		t.Fatal("the ad is not in the overlay")
	}
	matched := func() bool {
		for _, m := range l.Match(query(r, &a, 0), 20) {
			if m.AdID == a.ID {
				return true
			}
		}
		return false
	}
	if l.Get(a.ID) == nil || !matched() {
		t.Fatal("the added ad is not tracked")
	}

	pol := catalog.DefaultPolicy
	pol.MinTrust = 1e9
	l.db.SetPolicy(pol)
	if err := l.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if l.Get(a.ID) != nil || matched() || l.overlayN != 0 {
		t.Fatalf("an ad below min_trust is still tracked (overlay %d postings)", l.overlayN)
	}

	l.db.SetPolicy(catalog.DefaultPolicy)
	if err := l.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if l.Get(a.ID) == nil || !matched() {
		t.Fatal("the ad is not tracked again once the policy allows it")
	}
}

// An index file that cannot be written leaves the index on the heap, not the library
// unopened.
func TestIndexFileNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) //nolint:errcheck // cleanup
	ads := synthAds(5, 70)
	db := openCatalog(t, "")
	store(t, db, ads)
	l, err := Open(t.Context(), db, filepath.Join(dir, "tracking.csr"))
	if err != nil {
		t.Fatalf("open with an unwritable index file: %v", err)
	}
	defer l.Close()
	if l.csr.mapped != nil || l.Stats().File != "" {
		t.Fatal("the index is not on the heap")
	}
	checkMatch(t, l, ads, rand.New(rand.NewPCG(9, 9)))
}

// preallocate sizes the file (with its blocks, where the filesystem can).
func TestPreallocate(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const size = 3<<20 + 5
	if err := preallocate(f, size); err != nil {
		t.Fatal(err)
	}
	if fi, err := f.Stat(); err != nil || fi.Size() != size {
		t.Fatalf("size after preallocate: %v, %v", fi.Size(), err)
	}
}
