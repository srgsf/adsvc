// Package testdb generates a synthetic federation for manual testing: several origins
// (an honest crowd, a curator, a collider and a trash producer) that author signed records
// over made-up fingerprints and files, written as snapshots (what `adsvc import` reads),
// and a set of node configurations to import them into (testbed.go).
//
// The fingerprints are random landmarks, not audio: they exercise the catalogue, trust,
// duplicate detection and the pagers, not matching against real media.
package testdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/filekey"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// Options sizes the world. Zero fields take the defaults of Defaults.
type Options struct {
	Seed       uint64 // the same seed gives the same ads, files and votes (not the keys)
	Honest     int    // origins that enrol real-looking ads and confirm them in files
	Ads        int    // honest ads in all
	Files      int    // files in the shared pool
	Collisions int    // honest ads the collider copies, in three kinds of overlap
	TrashAds   int    // ads the trash origin publishes
	TrashMaps  int    // file maps the trash origin publishes
	Days       int    // how far back the records reach
}

// Defaults fills in what is not set: sized for two pages of the ads and files lists at
// 100 rows, and a trash origin just over the default quota of 5000 records a day.
func (o Options) Defaults() Options {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.Honest, 3)
	def(&o.Ads, 900)
	def(&o.Files, 2500)
	def(&o.Collisions, 60)
	def(&o.TrashAds, 3500)
	def(&o.TrashMaps, 1800)
	def(&o.Days, 45)
	if o.Seed == 0 {
		o.Seed = 1
	}
	return o
}

// Persona is one origin of the world.
type Persona struct {
	Name    string
	Role    string
	ID      *record.Identity
	Records []record.Record
	Ads     int // ad.add records
	Maps    int // file.map records
}

// Origin is the persona's node id, for trust.origins.
func (p *Persona) Origin() record.Origin { return p.ID.Origin }

// World is every persona, in the order honest…, curator, collider, trash.
type World struct {
	Options  Options
	Personas []*Persona
}

// Persona finds one by name.
func (w *World) Persona(name string) *Persona {
	for _, p := range w.Personas {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// ad is a generated ad: its landmarks are kept to derive near-duplicates.
type ad struct {
	id     fingerprint.ID
	pts    []fingerprint.Point
	durMs  int32
	author *Persona
}

// signer builds the records of one persona.
type signer struct {
	p   *Persona
	seq uint64
	ts  int64 // the latest record's time: times only go up
}

func (s *signer) emit(kind string, body any, ts int64) error {
	s.seq++
	ts = max(ts, s.ts+1)
	s.ts = ts
	r, err := record.New(kind, s.seq, ts, body)
	if err != nil {
		return err
	}
	s.p.ID.Sign(&r)
	s.p.Records = append(s.p.Records, r)
	switch kind {
	case record.KindAdAdd:
		s.p.Ads++
	case record.KindFileMap:
		s.p.Maps++
	}
	return nil
}

type gen struct {
	opt   Options
	rng   *rand.Rand
	start time.Time // the oldest record
	span  time.Duration
	files []string
	w     *World
}

// Generate builds the world. It writes nothing.
func Generate(opt Options) (*World, error) {
	opt = opt.Defaults()
	//nolint:gosec // G404: a seeded generator is the point: the same seed gives the same world
	g := &gen{opt: opt, rng: rand.New(rand.NewPCG(opt.Seed, opt.Seed^0x9e3779b97f4a7c15)), w: &World{Options: opt}}
	g.span = time.Duration(opt.Days) * 24 * time.Hour
	g.start = time.Now().Add(-g.span)
	for i := range g.opt.Files {
		g.files = append(g.files, g.fileKey(i))
	}
	signers := map[string]*signer{}
	persona := func(name, role string) *signer {
		p := &Persona{Name: name, Role: role, ID: record.NewIdentity()}
		g.w.Personas = append(g.w.Personas, p)
		signers[name] = &signer{p: p}
		return signers[name]
	}
	var honest []*signer
	for i := range opt.Honest {
		honest = append(honest, persona(fmt.Sprintf("honest-%d", i+1), "enrols ads, confirms them in files"))
	}
	curator := persona("curator", "votes: good for honest ads, not_ad for trash, marks duplicates")
	collider := persona("collider", "publishes near-duplicates and partial overlaps of honest ads")
	trash := persona("trash", "floods ads and file maps that nobody wants")

	var honestAds []*ad
	for i := range opt.Ads {
		s := honest[i%len(honest)]
		a, err := g.newAd(s, g.pick(honestDurations), brandLabel(g.rng, i), g.at(i, opt.Ads))
		if err != nil {
			return nil, err
		}
		honestAds = append(honestAds, a)
	}
	if err := g.confirm(honest, honestAds, 0.55); err != nil {
		return nil, err
	}

	var collisions []collision
	for i := range min(opt.Collisions, len(honestAds)) {
		orig := honestAds[g.rng.IntN(len(honestAds))]
		c, err := g.collide(collider, orig, i)
		if err != nil {
			return nil, err
		}
		collisions = append(collisions, c)
	}

	var trashAds []*ad
	for i := range opt.TrashAds {
		a, err := g.newAd(trash, g.pick(trashDurations), fmt.Sprintf("промо %d", i), g.at(i, opt.TrashAds))
		if err != nil {
			return nil, err
		}
		trashAds = append(trashAds, a)
	}
	if err := g.confirmTrash(trash, trashAds, opt.TrashMaps); err != nil {
		return nil, err
	}

	if err := g.curate(curator, honestAds, collisions, trashAds); err != nil {
		return nil, err
	}
	return g.w, nil
}

var (
	honestDurations = []int32{10_000, 15_000, 20_000, 30_000, 30_000, 45_000, 60_000}
	trashDurations  = []int32{2_000, 3_000, 5_000, 8_000}
)

func (g *gen) pick(from []int32) int32 { return from[g.rng.IntN(len(from))] }

// at is the time of item i of n, spread over the span in order.
func (g *gen) at(i, n int) int64 {
	return g.start.Add(g.span * time.Duration(i) / time.Duration(max(n, 1))).UnixMilli()
}

func (g *gen) fileKey(i int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "adsgen file %d %d", g.opt.Seed, i))
	if i%2 == 0 {
		return filekey.Torrent(hex.EncodeToString(sum[:20]), fmt.Sprint(1+i%12))
	}
	return "c:" + hex.EncodeToString(sum[:16])
}

var brands = []string{"Ozon", "Beeline", "Tinkoff", "Lada", "Mail", "Yandex", "Sber", "MTS", "Magnit", "Dixy", "Kaspersky", "Rostelecom"}

func brandLabel(rng *rand.Rand, i int) string {
	if rng.IntN(10) < 3 {
		return "" // the default: ad-<short id>
	}
	return fmt.Sprintf("%s #%d", brands[rng.IntN(len(brands))], i)
}

// landmarks makes random landmarks at the density of real audio: five per 32 ms frame.
func (g *gen) landmarks(durMs int32) []fingerprint.Point {
	frames := int(durMs / 32)
	pts := make([]fingerprint.Point, 0, frames*5)
	for t := range frames {
		for range 5 {
			pts = append(pts, fingerprint.Point{H: g.rng.Uint32N(1 << fingerprint.HashBits), T: int32(t)})
		}
	}
	return pts
}

func (g *gen) newAd(s *signer, durMs int32, label string, ts int64) (*ad, error) {
	return g.addAd(s, g.landmarks(durMs), durMs, label, ts)
}

func (g *gen) addAd(s *signer, pts []fingerprint.Point, durMs int32, label string, ts int64) (*ad, error) {
	enc, err := fingerprint.Encode(pts)
	if err != nil {
		return nil, err
	}
	id := fingerprint.IDOf(enc)
	body := record.AdAdd{ID: id, FPVersion: fingerprint.Version, DurationMs: durMs, Points: enc, Label: label}
	if err := s.emit(record.KindAdAdd, body, ts); err != nil {
		return nil, err
	}
	return &ad{id: id, pts: pts, durMs: durMs, author: s.p}, nil
}

// confirm has each honest origin confirm ads in files: a file holds one to three ads and
// is confirmed by about `share` of the origins, so several origins vouch for most of them.
func (g *gen) confirm(honest []*signer, ads []*ad, share float64) error {
	for i, key := range g.files {
		var dets []record.Detection
		for range 1 + g.rng.IntN(3) {
			a := ads[g.rng.IntN(len(ads))]
			start := int32(g.rng.IntN(40*60_000)) + 30_000
			dets = append(dets, record.Detection{Ad: a.id, StartMs: start, EndMs: start + a.durMs,
				Score: 60 + g.rng.IntN(400), Confirmed: true})
		}
		dur := int32(20*60_000 + g.rng.IntN(40*60_000))
		confirmed := false
		for _, s := range honest {
			if g.rng.Float64() > share && (confirmed || s != honest[len(honest)-1]) {
				continue
			}
			confirmed = true
			body := record.FileMap{Key: key, FPVersion: fingerprint.Version, Size: int64(dur) * 700, DurationMs: dur, Ads: dets}
			if err := s.emit(record.KindFileMap, body, dayStart(g.at(i, len(g.files)))); err != nil {
				return err
			}
		}
	}
	return nil
}

func dayStart(ms int64) int64 { return time.UnixMilli(ms).UTC().Truncate(24 * time.Hour).UnixMilli() }

// confirmTrash publishes maps that "find" many trash ads in every file, at scores just
// over the threshold.
func (g *gen) confirmTrash(s *signer, ads []*ad, maps int) error {
	for i := range maps {
		key := g.files[g.rng.IntN(len(g.files))]
		var dets []record.Detection
		for range 5 + g.rng.IntN(20) {
			a := ads[g.rng.IntN(len(ads))]
			start := int32(g.rng.IntN(50 * 60_000))
			dets = append(dets, record.Detection{Ad: a.id, StartMs: start, EndMs: start + a.durMs,
				Score: 20 + g.rng.IntN(15), Confirmed: true})
		}
		body := record.FileMap{Key: key, FPVersion: fingerprint.Version, DurationMs: 40 * 60_000, Ads: dets}
		if err := s.emit(record.KindFileMap, body, dayStart(g.at(i, maps))); err != nil {
			return err
		}
	}
	return nil
}

// collision is a collider ad and the honest ad it overlaps.
type collision struct {
	orig, copy *ad
	kind       string
}

// collide publishes a variant of orig. The kinds are the ones Library.Quality tells apart
// by score: a near-duplicate (same ad, other boundaries), half an ad, and a shared music
// bed (a few seconds in common).
func (g *gen) collide(s *signer, orig *ad, i int) (collision, error) {
	var pts []fingerprint.Point
	var kind string
	dur := orig.durMs
	switch i % 3 {
	case 0:
		kind = "near-duplicate"
		shift := int32(g.rng.IntN(4))
		for _, p := range orig.pts {
			if g.rng.IntN(100) < 92 {
				pts = append(pts, fingerprint.Point{H: p.H, T: p.T + shift})
			}
		}
		dur += int32(g.rng.IntN(600)) - 200
	case 1:
		kind = "half"
		cut := orig.durMs / 32 / 2 // frames in the first half
		for _, p := range orig.pts {
			if p.T < cut {
				pts = append(pts, p)
			}
		}
		for _, p := range g.landmarks(dur / 2) {
			pts = append(pts, fingerprint.Point{H: p.H, T: p.T + cut})
		}
	default:
		kind = "music bed"
		bed := min(4_000, dur/3) / 32
		for _, p := range orig.pts {
			if p.T < bed {
				pts = append(pts, p)
			}
		}
		fresh := g.landmarks(dur)
		for _, p := range fresh {
			if p.T >= bed {
				pts = append(pts, p)
			}
		}
	}
	if len(pts) < record.MinAdLandmarks {
		return collision{}, fmt.Errorf("collision %d: %d landmarks", i, len(pts))
	}
	c, err := g.addAd(s, pts, max(dur, 1_000), "", g.at(i, g.opt.Collisions)+1)
	return collision{orig, c, kind}, err
}

// curate votes: +1 (good) on most honest ads, −1 (not_ad) on 60% of the trash, a boundary
// complaint here and there, and a dup mark on two thirds of the collisions (so the Ads
// list has both rose and amber duplicates).
func (g *gen) curate(s *signer, honest []*ad, cols []collision, trash []*ad) error {
	ts := g.start.UnixMilli()
	step := int64(time.Minute / time.Millisecond)
	next := func() int64 { ts += step; return min(ts, time.Now().UnixMilli()-1) }
	for _, a := range honest {
		switch r := g.rng.IntN(100); {
		case r < 70:
			if err := s.emit(record.KindAdVote, record.AdVote{Ad: a.id, Value: 1, Reason: record.ReasonGood}, next()); err != nil {
				return err
			}
		case r < 75:
			if err := s.emit(record.KindAdVote, record.AdVote{Ad: a.id, Value: -1, Reason: record.ReasonBoundary}, next()); err != nil {
				return err
			}
		}
	}
	for _, a := range trash {
		if g.rng.IntN(100) < 60 {
			if err := s.emit(record.KindAdVote, record.AdVote{Ad: a.id, Value: -1, Reason: record.ReasonNotAd}, next()); err != nil {
				return err
			}
		}
	}
	for i, c := range cols {
		if i%3 == 2 {
			continue
		}
		if err := s.emit(record.KindAdDup, record.AdDup{Ad: c.copy.id, Canonical: c.orig.id}, next()); err != nil {
			return err
		}
	}
	return nil
}

// WriteSnapshots writes one snapshot per persona into dir (name.db) and returns their
// paths by persona name. A snapshot is a catalogue holding that persona's records under
// its identity, so `adsvc import` takes it as it takes a peer's export.
func (w *World) WriteSnapshots(ctx context.Context, dir string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range w.Personas {
		path := filepath.Join(dir, p.Name+".db")
		if err := writeSnapshot(ctx, p, path); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		out[p.Name] = path
	}
	return out, nil
}

func writeSnapshot(ctx context.Context, p *Persona, path string) error {
	db, err := catalog.Open(ctx, "")
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.SetIdentity(ctx, p.ID); err != nil {
		return err
	}
	pol := catalog.DefaultPolicy
	pol.QuotaPerDay = 1 << 30 // the receiver's quota is what is being tested
	db.SetPolicy(pol)
	for i := 0; i < len(p.Records); i += 500 {
		res, err := db.Ingest(ctx, p.Records[i:min(i+500, len(p.Records))], p.ID.Origin)
		if err != nil {
			return err
		}
		if n := res.Accepted + res.Duplicate; n == 0 || len(res.Rejected) > 0 {
			return fmt.Errorf("records %d…: %+v", i, res)
		}
	}
	return db.Snapshot(ctx, path)
}
