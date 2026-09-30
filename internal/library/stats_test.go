package library

import (
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

func TestStatsQualityRebuild(t *testing.T) {
	ads := synthAds(3, 7)
	// near is ads[0]'s first 15 s followed by other audio: a near-duplicate.
	r := rand.New(rand.NewPCG(7, 1))
	var pts []fingerprint.Point
	cut := int32(468) // 15 s
	for _, p := range ads[0].Points {
		if p.T < cut {
			pts = append(pts, p)
		}
	}
	pts = append(pts, synthPoints(r, int(cut), int(cut))...)
	near := newTestAd(pts, 30)

	path := index(t)
	l := newLib(t, ads, path)
	add(t, l, near)
	s := l.Stats()
	if s.Ads != 4 || s.IndexAds != 3 || s.OverlayAds != 1 || s.OverlayPostings != len(near.Points) || s.Built.IsZero() ||
		s.File != path || s.Postings == 0 || s.Distinct == 0 {
		t.Fatalf("stats: %+v", s)
	}
	if littleEndian && s.MappedBytes == 0 {
		t.Fatalf("index file not mapped: %+v", s)
	}

	q, err := l.Quality(t.Context(), near.ID)
	if err != nil {
		t.Fatal(err)
	}
	if q.Windows != 3 || q.SelfMin < 100 || q.Worst.AdID != ads[0].ID || q.Worst.Score < 100 || q.WorstAt != 0 || q.PointsPerSec < 100 {
		t.Fatalf("near-duplicate: %+v", q)
	}
	q, err = l.Quality(t.Context(), ads[1].ID)
	if err != nil || q.Worst.Score >= 20 || q.SelfMin < 100 {
		t.Fatalf("distinct ad: %+v, %v", q, err)
	}
	if _, err := l.Quality(t.Context(), fingerprint.ID{1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ad: %v", err)
	}

	if err := l.Rebuild(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := l.Stats(); s.IndexAds != 4 || s.OverlayAds != 0 || s.BuildTook == 0 {
		t.Fatalf("after rebuild: %+v", s)
	}
}
