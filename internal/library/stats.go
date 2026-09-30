package library

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

// Stats describes the index.
type Stats struct {
	Ads             int // tracked ads: in the index or the overlay
	IndexAds        int // in the CSR, removed ones included
	Postings        int // of the CSR
	Distinct        int // distinct hashes of the CSR
	DeadAds         int // CSR ads removed since the build
	DeadPostings    int
	OverlayAds      int
	OverlayPostings int
	File            string // the index file; "" when the index is on the heap
	MappedBytes     int64  // the mapped file (page cache, not heap)
	HeapBytes       int64  // estimate: the CSR when it is not mapped, plus the overlay
	Built           time.Time
	BuildTook       time.Duration // 0 when the index was loaded from its file
}

// overlayPostingBytes estimates what a posting of the overlay costs: an occ in a map of
// slices.
const overlayPostingBytes = 24

// Stats describes the index as it is now.
func (l *Library) Stats() Stats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c := l.csr
	s := Stats{
		Ads: len(l.ads), IndexAds: len(c.ids), Postings: len(c.post), Distinct: len(c.low), DeadPostings: l.deadN,
		OverlayAds: len(l.overlayIDs), OverlayPostings: l.overlayN, File: c.file(l.csrPath), Built: c.built, BuildTook: c.took,
		HeapBytes: int64(l.overlayN) * overlayPostingBytes,
	}
	for _, d := range l.dead {
		if d {
			s.DeadAds++
		}
	}
	if c.mapped != nil {
		s.MappedBytes = int64(len(c.mapped))
	} else {
		s.HeapBytes += int64(16*len(c.ids) + 4*(len(c.sizes)+len(c.start)+len(c.top)+len(c.off)+len(c.post)) + len(c.low))
	}
	return s
}

// Quality windows: what detection matches at a time, 10 s of new audio plus 3 s of
// context (detect.Block, detect.Overlap; detect imports this package).
var (
	windowFrames = int32(math.Round(float64(13*time.Second) / float64(fingerprint.FrameDur)))
	stepFrames   = int32(math.Round(float64(10*time.Second) / float64(fingerprint.FrameDur)))
)

// Quality is how well an ad stands out, measured over the windows detection matches.
type Quality struct {
	Windows      int
	PointsPerSec float64
	// SelfMin is the weakest window's score against the ad itself: what clean audio of
	// the ad scores at best.
	SelfMin int
	// Worst is the strongest match of another tracked ad in any window of this one (Score
	// 0 if none), and WorstAt where that window starts in the ad.
	Worst   Match
	WorstAt time.Duration
}

// Quality measures ad id: its landmarks, cut into windows, are matched against the ad
// itself and against the index (with the ad left out), whether or not it is tracked.
func (l *Library) Quality(ctx context.Context, id fingerprint.ID) (Quality, error) {
	b, err := l.db.Points(ctx, id)
	if err != nil {
		return Quality{}, err
	}
	if b == nil {
		return Quality{}, fmt.Errorf("ad %s: %w", id, ErrNotFound)
	}
	pts, err := fingerprint.Decode(b)
	if err != nil {
		return Quality{}, fmt.Errorf("ad %s: %w", id, err)
	}
	var q Quality
	if len(pts) == 0 {
		return q, nil
	}
	own := map[uint32][]occ{}
	last := int32(0)
	for _, p := range pts {
		own[p.H] = append(own[p.H], occ{0, p.T})
		last = max(last, p.T)
	}
	q.PointsPerSec = float64(len(pts)) / (time.Duration(last+1) * fingerprint.FrameDur).Seconds()
	q.SelfMin = math.MaxInt
	for from := int32(0); ; from += stepFrames {
		if err := ctx.Err(); err != nil {
			return Quality{}, err
		}
		var win []fingerprint.Point
		for _, p := range pts {
			if p.T >= from && p.T < from+windowFrames {
				win = append(win, p)
			}
		}
		q.Windows++
		self := 0
		votes := overlayVotes(own, win)
		for _, m := range peaks(votes, []fingerprint.ID{id}, 1, nil) {
			self = max(self, m.Score)
		}
		putVotes(votes)
		q.SelfMin = min(q.SelfMin, self)
		if m := l.best(win, id); m.Score > q.Worst.Score {
			q.Worst, q.WorstAt = m, time.Duration(from)*fingerprint.FrameDur
		}
		if from+windowFrames > last {
			break
		}
	}
	return q, nil
}
