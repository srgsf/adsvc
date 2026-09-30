package admin

import (
	"context"
	"errors"
	"sync"

	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/library"
)

// qualities caches library.Quality per ad for one version of the index: the Ads list
// shows possible duplicates from it, and it takes a Match per window of each ad.
type qualities struct {
	mu      sync.Mutex
	version uint64
	m       map[fingerprint.ID]library.Quality
}

// maxQualities bounds the cache: the ads of a few dozen pages of the list.
const maxQualities = 4096

// quality is Lib.Quality of id, cached while the index stays the same.
func (a *Admin) quality(ctx context.Context, id fingerprint.ID) (library.Quality, error) {
	v := a.d.Lib.Version()
	c := &a.quals
	c.mu.Lock()
	if c.version != v || c.m == nil {
		c.version, c.m = v, map[fingerprint.ID]library.Quality{}
	}
	q, ok := c.m[id]
	c.mu.Unlock()
	if ok {
		return q, nil
	}
	q, err := a.d.Lib.Quality(ctx, id)
	if err != nil {
		return q, err
	}
	c.mu.Lock()
	if c.version == v {
		if len(c.m) >= maxQualities {
			clear(c.m) // one version held open long enough to see this many ads
		}
		c.m[id] = q
	}
	c.mu.Unlock()
	return q, nil
}

// dupOfView is another tracked ad that matches this one above the detection threshold: the
// same ad enrolled twice, most likely.
type dupOfView struct {
	ID    fingerprint.ID `json:"id"`
	Label string         `json:"label"`
	Score int            `json:"score"`
}

// possibleDup is the ad that collides with v above the threshold, if any. Ads already
// in a duplicate pair are left alone: that pair is known.
func (a *Admin) possibleDup(ctx context.Context, v *adView) error {
	if v.knownDup() {
		return nil
	}
	q, err := a.quality(ctx, v.ID)
	if errors.Is(err, library.ErrNotFound) {
		return nil // no landmarks here: nothing to compare
	}
	if err != nil {
		return err
	}
	if q.Worst.Score > 0 && q.Worst.Score >= a.minScore() {
		d := &dupOfView{ID: q.Worst.AdID, Score: q.Worst.Score}
		if ad := a.d.Lib.Get(q.Worst.AdID); ad != nil {
			d.Label = ad.Label
		}
		v.PossibleDup = d
	}
	return nil
}

// knownDup: v is in a duplicate pair this node marked.
func (v *adView) knownDup() bool { return v.State == "dup" || v.Canonical != nil || len(v.DupedBy) > 0 }

func (a *Admin) minScore() int {
	if a.d.MinScore != nil {
		return a.d.MinScore()
	}
	return detect.DefaultMinScore
}
