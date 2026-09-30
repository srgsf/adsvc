// Package library is the reference ad index: the ads of the catalogue as an inverted
// index of fingerprints, with offset voting.
//
// The index is an immutable CSR (csr.go) of the tracking set, kept in a file that is
// mapped into memory (csrfile.go) and reused while the set is unchanged. Ads added since
// the build live in a small map index (the overlay), removed ones get a tombstone, and the
// CSR is rebuilt once either grows past a fraction of it.
package library

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/srgsf/adsvc/internal/adtype"
	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/mediatime"
)

// Errors of Add, Remove and Find.
var (
	ErrTooFewLandmarks = errors.New("too few landmarks: segment is too short or silent")
	ErrTooLong         = fmt.Errorf("ad is longer than %v", MaxDuration.Round(time.Second))
	ErrFull            = fmt.Errorf("library holds %d ads", MaxAds)
	ErrNotFound        = errors.New("no such ad")
	ErrAmbiguous       = errors.New("ad id prefix matches several ads")
	ErrBadType         = errors.New("unknown ad type")
)

// Ad is a reference ad. Its landmarks live in the catalogue and the index.
type Ad struct {
	ID       fingerprint.ID
	Label    string
	Type     string // adtype.Ad or adtype.Intro
	Duration time.Duration
	Created  time.Time
	Hashes   int             // landmarks
	Source   *catalog.Source // where it was enrolled from, when known
}

func adOf(a catalog.Ad) *Ad {
	return &Ad{ID: a.ID, Label: a.Label, Type: adtype.Of(a.Type), Duration: mediatime.Dur(a.DurationMs), Created: a.Created, Hashes: a.NPoints, Source: a.Source}
}

// occ is a posting of the overlay: an index into Library.overlayIDs and a frame.
type occ struct {
	ad uint32
	t  int32
}

// Match is the best alignment of a query window against one reference ad.
type Match struct {
	AdID   fingerprint.ID
	Offset int32 // adT - queryT, in frames
	Score  int
}

// Library is the index of the ads of a catalogue.
type Library struct {
	db      *catalog.DB
	csrPath string // index file; "" = heap

	mu    sync.RWMutex
	ads   map[fingerprint.ID]*Ad
	csr   *csr
	dead  []bool // per CSR ad: removed since the build; nil when none is
	deadN int    // postings of dead ads
	// Ads added since the last build: their landmarks, and the overlay index over them.
	fresh      map[fingerprint.ID]freshAd
	addSeq     uint64 // counts Adds; freshAd.seq
	overlay    map[uint32][]occ
	overlayIDs []fingerprint.ID
	overlayN   int    // postings in the overlay
	version    uint64 // changes whenever what Match can return may change

	buildMu sync.Mutex // one rebuild at a time

	// Rebuilds after Add and Remove run in the background (rebuildSoon), one at a time,
	// under ctx (the one Open was given).
	ctx       context.Context
	bgMu      sync.Mutex
	bgRunning bool
	bgClosed  bool
	bgWG      sync.WaitGroup
}

// freshAd is an ad added since the last build: its landmarks, and the addSeq it got.
type freshAd struct {
	pts []fingerprint.Point
	seq uint64
}

// Version changes whenever the tracked ads or the index change: results of Match (and
// Quality) computed at one version hold as long as it stays.
func (l *Library) Version() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.version
}

// Open indexes the ads of db. The index is kept in the file csrPath (reused when it still
// matches the ads, rebuilt otherwise); an empty csrPath keeps it on the heap.
func Open(ctx context.Context, db *catalog.DB, csrPath string) (*Library, error) {
	if csrPath != "" && !littleEndian {
		slog.Warn("ad index kept in memory: index files need a little-endian host")
		csrPath = ""
	}
	l := &Library{db: db, csrPath: csrPath, ads: map[fingerprint.ID]*Ad{}, ctx: ctx}
	metas, err := db.Tracking(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range metas {
		l.ads[a.ID] = adOf(a)
	}
	c, loaded, err := l.load(ctx, metas)
	if err != nil {
		return nil, fmt.Errorf("ad index: %w", err)
	}
	l.install(c)
	if loaded {
		l.checkPostings(c)
	}
	return l, nil
}

// checkPostings checks the postings of c, an index loaded from its file, in the
// background (reading them all is reading the whole file) and rebuilds the index if one
// is damaged. Match skips postings out of range meanwhile.
func (l *Library) checkPostings(c *csr) {
	l.bgMu.Lock()
	defer l.bgMu.Unlock()
	if l.bgClosed {
		return
	}
	l.bgWG.Go(func() {
		l.mu.RLock() // c stays mapped: installing another index waits for this
		var err error
		if l.csr == c {
			err = c.validatePostings()
		}
		l.mu.RUnlock()
		if err == nil {
			return
		}
		slog.Warn("ad index file is corrupt, rebuilding", "file", l.csrPath, "err", err)
		if err := l.rebuildIf(l.ctx, true); err != nil {
			slog.Warn("ad index not rebuilt", "err", err)
		}
	})
}

// load reuses the index file when it holds exactly the tracking set (loaded), else
// builds one.
func (l *Library) load(ctx context.Context, metas []catalog.Ad) (c *csr, loaded bool, err error) {
	if l.csrPath != "" {
		c, h, err := openCSR(l.csrPath)
		switch {
		case err == nil && h.digest == setDigest(idsOf(metas)):
			if err = c.validate(); err == nil {
				c.digest, c.built = h.digest, time.UnixMilli(h.builtAt)
				slog.Info("ad index loaded", "file", l.csrPath, "ads", len(c.ids), "postings", len(c.post))
				return c, true, nil
			}
			c.release()
			slog.Warn("ad index file is corrupt, rebuilding", "file", l.csrPath, "err", err)
		case err == nil:
			c.release()
			slog.Info("ads changed since the index was built, rebuilding", "file", l.csrPath)
		case !errors.Is(err, os.ErrNotExist):
			slog.Warn("ad index file unusable, rebuilding", "file", l.csrPath, "err", err)
		}
	}
	c, err = l.build(ctx)
	return c, false, err
}

// idsOf is the ids of the tracking set (catalog.Tracking), ascending. The catalogue caps
// the set at its policy's MaxAds; the index holds at most MaxAds.
func idsOf(metas []catalog.Ad) []fingerprint.ID {
	if len(metas) > MaxAds {
		slog.Warn("more ads than the index holds; indexing the most recent", "ads", len(metas), "max", MaxAds)
		metas = slices.SortedFunc(slices.Values(metas), func(a, b catalog.Ad) int { return b.Created.Compare(a.Created) })[:MaxAds]
	}
	ids := make([]fingerprint.ID, len(metas))
	for i, a := range metas {
		ids[i] = a.ID
	}
	slices.SortFunc(ids, compareID)
	return ids
}

// build indexes the tracking set (chosen by trust, see catalog.Tracking) from one
// snapshot of the catalogue, so that both passes of the build see the same ads.
func (l *Library) build(ctx context.Context) (*csr, error) {
	var c *csr
	var dropped int
	start := time.Now()
	err := l.db.View(ctx, func(v catalog.View) error {
		metas, err := v.Tracking(ctx)
		if err != nil {
			return err
		}
		ids := idsOf(metas)
		bad := map[fingerprint.ID]bool{}
		var buf []fingerprint.Point // one buffer for every ad of both passes
		checked := false
		src := func(fn func(d int, pts []fingerprint.Point) error) error {
			defer func() { checked = true }()
			return v.EachPointsOf(ctx, ids, func(id fingerprint.ID, b []byte) error { // the tracked ads only
				d, ok := slices.BinarySearchFunc(ids, id, compareID)
				if !ok || bad[id] {
					return nil
				}
				var err error
				buf, err = fingerprint.AppendDecode(buf[:0], b)
				if err == nil && !checked && fingerprint.IDOf(b) != id {
					err = errors.New("landmarks do not match the ad id")
				}
				if err != nil {
					bad[id] = true // left out of both passes
					slog.Warn("ad left out of the index", "ad", id, "err", err)
					return nil
				}
				return fn(d, buf)
			})
		}
		heap := l.csrPath == ""
		if !heap {
			c, dropped, err = writeCSR(l.csrPath, ids, src)
			if err != nil && ctx.Err() == nil {
				// A read-only data directory, a filesystem without shared writable
				// mappings, a full disk: the same ads serve from the heap until the next
				// build.
				slog.Warn("ad index file not written, keeping the index in memory", "file", l.csrPath, "err", err)
				clear(bad)
				checked, heap = false, true
			}
		}
		if heap {
			c, dropped, err = buildCSR(ids, src, nil)
		}
		if err == nil {
			c.digest, c.built, c.took = setDigest(ids), time.Now(), time.Since(start)
		}
		return err
	})
	if err != nil {
		builds.With("error").Inc()
		return nil, err
	}
	builds.With("ok").Inc()
	buildSeconds.Since(start)
	if dropped > 0 {
		slog.Warn("landmarks beyond the index limits left out", "landmarks", dropped, "max_duration", MaxDuration)
	}
	slog.Info("ad index built", "ads", len(c.ids), "postings", len(c.post), "file", l.csrPath, "took", time.Since(start))
	return c, nil
}

// install makes c the index and releases the previous one: nothing can still use it, as
// Match holds mu for reading. Ads added since the build that c lacks stay in the
// overlay, and ads of c that have been removed get a tombstone. Callers hold mu for
// writing (or own l).
func (l *Library) install(c *csr) {
	old := l.csr
	l.csr, l.dead, l.deadN = c, nil, 0
	l.overlay, l.overlayIDs, l.overlayN = map[uint32][]occ{}, nil, 0
	for id, f := range l.fresh {
		if _, ok := c.dense(id); ok || l.ads[id] == nil {
			delete(l.fresh, id)
		} else {
			l.addOverlay(id, f.pts)
		}
	}
	for d, id := range c.ids {
		if l.ads[id] == nil {
			l.kill(uint32(d))
		}
	}
	old.release()
}

func (l *Library) addOverlay(id fingerprint.ID, pts []fingerprint.Point) {
	i := uint32(len(l.overlayIDs))
	l.overlayIDs = append(l.overlayIDs, id)
	for _, p := range pts {
		l.overlay[p.H] = append(l.overlay[p.H], occ{i, p.T})
	}
	l.overlayN += len(pts)
}

func (l *Library) removeOverlay(id fingerprint.ID, pts []fingerprint.Point) {
	i := uint32(slices.Index(l.overlayIDs, id))
	for _, p := range pts {
		occs := l.overlay[p.H][:0]
		for _, o := range l.overlay[p.H] {
			if o.ad != i {
				occs = append(occs, o)
			}
		}
		if len(occs) == 0 {
			delete(l.overlay, p.H)
		} else {
			l.overlay[p.H] = occs
		}
	}
	l.overlayN -= len(pts)
}

func (l *Library) kill(d uint32) {
	if l.dead == nil {
		l.dead = make([]bool, len(l.csr.ids))
	}
	l.dead[d] = true
	l.deadN += int(l.csr.sizes[d])
}

// stale reports whether the overlay or the tombstones have grown enough to rebuild the
// CSR: the overlay costs several times as much memory per posting, and dead postings
// still cost lookups.
func (l *Library) stale() bool {
	n := len(l.csr.post)
	return l.overlayN > max(1<<16, n/16) || l.deadN > max(1<<16, n/8)
}

// rebuildSoon starts a rebuild in the background when the index is stale, unless one is
// running: the caller (a mark, a delete) does not wait for it. The overlay and the
// tombstones serve meanwhile.
func (l *Library) rebuildSoon() {
	l.mu.RLock()
	stale := l.stale()
	l.mu.RUnlock()
	if !stale {
		return
	}
	l.bgMu.Lock()
	defer l.bgMu.Unlock()
	if l.bgRunning || l.bgClosed {
		return
	}
	l.bgRunning = true
	l.bgWG.Go(func() {
		for {
			err := l.rebuild(l.ctx)
			if err != nil {
				// The next Add or Remove retries.
				slog.Warn("ad index not rebuilt", "err", err)
			}
			l.bgMu.Lock()
			l.mu.RLock()
			again := err == nil && !l.bgClosed && l.stale() // it went stale again meanwhile
			l.mu.RUnlock()
			if !again {
				l.bgRunning = false
				l.bgMu.Unlock()
				return
			}
			l.bgMu.Unlock()
		}
	})
}

// rebuild builds a CSR of the current ads and installs it, if the current one is stale.
// Match keeps running on the old index meanwhile; ads added or removed during the build
// are reconciled by install.
func (l *Library) rebuild(ctx context.Context) error { return l.rebuildIf(ctx, false) }

func (l *Library) rebuildIf(ctx context.Context, force bool) error {
	l.buildMu.Lock()
	defer l.buildMu.Unlock()
	l.mu.RLock()
	stale := force || l.stale()
	l.mu.RUnlock()
	if !stale {
		return nil
	}
	c, err := l.build(ctx)
	if err != nil {
		return fmt.Errorf("rebuilding the ad index: %w", err)
	}
	l.mu.Lock()
	l.install(c)
	l.version++
	l.mu.Unlock()
	return nil
}

// Add fingerprints the PCM of a whole ad and stores it; src tells where it comes from
// (nil if unknown). The same landmarks are the same ad: adding them again returns the ad
// already stored. An empty label becomes "ad-<short id>" (or "intro-…"), an empty type an ad.
func (l *Library) Add(ctx context.Context, label, typ string, pcm []float32, src *catalog.Source) (*Ad, error) {
	dur := mediatime.Samples(int64(len(pcm)), fingerprint.SampleRate)
	if dur > MaxDuration {
		return nil, ErrTooLong
	}
	pts := fingerprint.Compute(pcm)
	if len(pts) < 50 {
		return nil, ErrTooFewLandmarks
	}
	return l.addPoints(ctx, label, typ, pts, dur, src)
}

// addPoints stores an ad of duration dur with landmarks pts (see Add).
func (l *Library) addPoints(ctx context.Context, label, typ string, pts []fingerprint.Point, dur time.Duration, src *catalog.Source) (*Ad, error) {
	enc, err := fingerprint.Encode(pts)
	if err != nil {
		return nil, err
	}
	id := fingerprint.IDOf(enc)
	typ = adtype.Of(typ)
	if !adtype.Valid(typ) {
		return nil, fmt.Errorf("%w %q", ErrBadType, typ)
	}
	if label == "" {
		label = typ + "-" + id.Short()
	}
	meta := catalog.Ad{ID: id, FPVersion: fingerprint.Version, Label: label, Type: typ, DurationMs: mediatime.Ms(dur),
		NPoints: len(pts), Created: time.Now(), Source: src}

	l.mu.RLock()
	a, n := l.ads[id], len(l.ads)
	l.mu.RUnlock()
	if a != nil {
		return a, nil
	}
	if n >= MaxAds {
		return nil, ErrFull
	}
	if _, err := l.db.AddAd(ctx, meta, enc); err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.version++
	if a = l.ads[id]; a != nil { // added concurrently
		l.mu.Unlock()
		return a, nil
	}
	a = adOf(meta)
	l.ads[id] = a
	if l.fresh == nil {
		l.fresh = map[fingerprint.ID]freshAd{}
	}
	l.addSeq++
	l.fresh[id] = freshAd{pts: pts, seq: l.addSeq}
	l.addOverlay(id, pts)
	l.mu.Unlock()
	l.rebuildSoon() // meanwhile the ad is matched through the overlay
	return a, nil
}

// Remove takes an ad out of this node's library and index (catalog.Retract: its author
// withdraws it, anyone else votes it down), with its detections.
func (l *Library) Remove(ctx context.Context, id fingerprint.ID) error {
	ok, err := l.db.Retract(ctx, id)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.version++
	if l.ads[id] == nil && !ok {
		l.mu.Unlock()
		return fmt.Errorf("ad %s: %w", id, ErrNotFound)
	}
	delete(l.ads, id)
	if f, isFresh := l.fresh[id]; isFresh {
		l.removeOverlay(id, f.pts)
		delete(l.fresh, id)
	} else if d, inCSR := l.csr.dense(id); inCSR && (l.dead == nil || !l.dead[d]) {
		l.kill(d)
	}
	l.mu.Unlock()
	l.rebuildSoon() // meanwhile the ad is a tombstone
	return nil
}

// Refresh reloads the tracking set from the catalogue (after records arrived, or the
// trust policy changed): labels are updated, and the index is rebuilt when the set of ads
// is not the one it holds.
func (l *Library) Refresh(ctx context.Context) error { return l.refresh(ctx, false) }

// Rebuild is Refresh, and then builds the index even when the set is unchanged, which
// folds the overlay and the tombstones into it.
func (l *Library) Rebuild(ctx context.Context) error { return l.refresh(ctx, true) }

func (l *Library) refresh(ctx context.Context, force bool) error {
	// Ads added up to seen were stored before the tracking set is read, so the set says
	// whether they stay; later ones may be missing from it only because they are new.
	l.mu.RLock()
	seen := l.addSeq
	l.mu.RUnlock()
	metas, err := l.db.Tracking(ctx)
	if err != nil {
		return err
	}
	ids := idsOf(metas)
	l.mu.Lock()
	ads := make(map[fingerprint.ID]*Ad, len(metas)+len(l.fresh))
	for _, a := range metas {
		ads[a.ID] = adOf(a)
	}
	for id, f := range l.fresh { // added here and not in the index yet
		switch a := l.ads[id]; {
		case ads[id] != nil:
		case a != nil && f.seq > seen:
			ads[id] = a
		default: // the tracking set no longer holds it
			l.removeOverlay(id, f.pts)
			delete(l.fresh, id)
		}
	}
	if !sameKeys(ads, l.ads) {
		l.version++ // the tracked set changed (a rebuild, if one follows, bumps it again)
	}
	l.ads = ads
	changed := force || setDigest(ids) != l.csr.digest
	l.mu.Unlock()
	if !changed {
		return nil
	}
	return l.rebuildIf(ctx, true)
}

// Tracked reports whether the ad is tracked, and its label and type (a catalog.Tracked).
func (l *Library) Tracked(id fingerprint.ID) (label, typ string, ok bool) {
	if a := l.Get(id); a != nil {
		return a.Label, a.Type, true
	}
	return "", "", false
}

func sameKeys(a, b map[fingerprint.ID]*Ad) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}

// Close releases the index. The library must not be used afterwards; the catalogue stays
// open (it belongs to the caller).
func (l *Library) Close() error {
	l.bgMu.Lock()
	l.bgClosed = true
	l.bgMu.Unlock()
	l.bgWG.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.csr.release()
	l.csr = &csr{}
	return nil
}

// Get returns the ad with this id, or nil.
func (l *Library) Get(id fingerprint.ID) *Ad {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.ads[id]
}

// FromSource returns the tracked ads that were enrolled from a file known by any of keys.
func (l *Library) FromSource(keys ...string) []*Ad {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []*Ad
	for _, a := range l.ads {
		if a.Source != nil && slices.Contains(keys, a.Source.Key) {
			out = append(out, a)
		}
	}
	return out
}

// Find returns the ad whose id is id or starts with the hex digits of id (dashes are
// ignored), for command lines.
func (l *Library) Find(id string) (*Ad, error) {
	prefix := strings.ToLower(strings.ReplaceAll(id, "-", ""))
	if _, err := hex.DecodeString(prefix + prefix[:len(prefix)%2]); err != nil || prefix == "" {
		return nil, fmt.Errorf("ad id %q: want hex digits", id)
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	var found *Ad
	for aid, a := range l.ads {
		if strings.HasPrefix(hex.EncodeToString(aid[:]), prefix) {
			if found != nil {
				return nil, fmt.Errorf("%q: %w", id, ErrAmbiguous)
			}
			found = a
		}
	}
	if found == nil {
		return nil, fmt.Errorf("ad %s: %w", id, ErrNotFound)
	}
	return found, nil
}

// AdInfo is the public (hash-less) view of an ad, as the API returns it.
type AdInfo struct {
	ID         fingerprint.ID `json:"id"`
	Label      string         `json:"label"`
	Type       string         `json:"type"` // "ad" or "intro"
	DurationMs int32          `json:"durationMs"`
	Hashes     int            `json:"hashes"`
	Created    time.Time      `json:"created"`
}

// Info is the public view of a.
func (a *Ad) Info() AdInfo {
	return AdInfo{ID: a.ID, Label: a.Label, Type: a.Type, DurationMs: mediatime.Ms(a.Duration), Hashes: a.Hashes, Created: a.Created}
}

// List returns every ad, oldest first.
func (l *Library) List() []AdInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]AdInfo, 0, len(l.ads))
	for _, a := range l.ads {
		out = append(out, a.Info())
	}
	slices.SortFunc(out, func(a, b AdInfo) int {
		return cmp.Or(a.Created.Compare(b.Created), compareID(a.ID, b.ID))
	})
	return out
}

// best is the strongest match of q (score 0 if none) of any tracked ad but except: what
// Quality asks of each window, without collecting and sorting every match.
func (l *Library) best(q []fingerprint.Point, except fingerprint.ID) Match {
	var b Match
	keep := func(m Match) {
		if m.AdID != except && m.Score > b.Score {
			b = m
		}
	}
	l.mu.RLock()
	l.csr.each(q, l.dead, 1, keep)
	if l.overlayN > 0 {
		votes := overlayVotes(l.overlay, q)
		eachPeak(votes, l.overlayIDs, 1, keep)
		putVotes(votes)
	}
	l.mu.RUnlock()
	return b
}

// Match votes query hashes against the index and returns candidate alignments
// (ad, offset) whose score (votes at offset±1 frame) reaches minScore, best first.
func (l *Library) Match(q []fingerprint.Point, minScore int) []Match {
	defer matchSeconds.Since(time.Now())
	l.mu.RLock()
	out := l.csr.match(q, l.dead, minScore, nil)
	if l.overlayN > 0 {
		votes := overlayVotes(l.overlay, q)
		out = peaks(votes, l.overlayIDs, minScore, out)
		putVotes(votes)
	}
	l.mu.RUnlock()
	slices.SortFunc(out, func(a, b Match) int { return cmp.Compare(b.Score, a.Score) })
	return out
}
