// Package detect finds library ads in a stream of decoded audio: it cuts PCM into
// overlapping blocks, matches each block against the library, and applies the rules that
// decide when a detection is safe to skip.
package detect

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/srgsf/adsvc/internal/adtype"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/library"
	"github.com/srgsf/adsvc/internal/mediatime"
)

// Block geometry of the analysis.
const (
	Block   = 10 * time.Second // new audio per analysis step
	Overlap = 3 * time.Second  // context carried into the next block
)

// Detection is an ad found in a specific file, at times on its media timeline. In JSON
// (the proxy's API) the times are integer milliseconds: startMs, endMs.
type Detection struct {
	AdID  fingerprint.ID
	Label string
	Type  string // adtype.Ad or adtype.Intro
	Start time.Duration
	End   time.Duration
	Score int
	// Confirmed: the whole [Start,End) has been analysed, so the alignment is final.
	// Players should auto-skip only confirmed detections.
	Confirmed bool
}

// detectionJSON is Detection in the API.
type detectionJSON struct {
	AdID      fingerprint.ID `json:"adId"` // UUID form
	Label     string         `json:"label"`
	Type      string         `json:"type"`
	StartMs   int32          `json:"startMs"`
	EndMs     int32          `json:"endMs"`
	Score     int            `json:"score"`
	Confirmed bool           `json:"confirmed"`
}

// MarshalJSON writes the times as integer milliseconds.
func (d Detection) MarshalJSON() ([]byte, error) {
	return json.Marshal(detectionJSON{d.AdID, d.Label, adtype.Of(d.Type), mediatime.Ms(d.Start), mediatime.Ms(d.End), d.Score, d.Confirmed})
}

// UnmarshalJSON reads what MarshalJSON writes.
func (d *Detection) UnmarshalJSON(b []byte) error {
	var j detectionJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*d = Detection{j.AdID, j.Label, adtype.Of(j.Type), mediatime.Dur(j.StartMs), mediatime.Dur(j.EndMs), j.Score, j.Confirmed}
	return nil
}

// BlockResult is reported after each analysed block.
type BlockResult struct {
	From, To   time.Duration // audio range newly covered
	Reached    time.Duration // media time decoded so far (To plus the overlap read ahead)
	Detections []Detection   // scoring at least minScore
	// Weak is every detection scoring at least the floor Scan was given, the strong ones
	// included (nil without a floor): what a benchmark measures the noise with.
	Weak []Detection
}

// Scan reads mono s16le PCM starting at media time t0 and reports detections block by block.
// With floor > 0, each block also reports the detections from floor up (BlockResult.Weak).
// It returns the media time reached.
func Scan(ctx context.Context, r io.Reader, t0 time.Duration, lib *library.Library, minScore, floor int, onBlock func(BlockResult)) (time.Duration, error) {
	return Windows(ctx, r, t0, func(w Window) {
		start := time.Now()
		pts := fingerprint.Compute(w.PCM)
		fingerprintSeconds.Since(start)
		b := BlockResult{From: w.From, To: w.To, Reached: w.T0 + w.Dur()}
		ms := lib.Match(pts, cmp.Or(min(floor, minScore), minScore))
		if floor > 0 {
			b.Weak = Pick(lib, ms, w.T0, w.Dur())
			ms = slices.DeleteFunc(ms, func(m library.Match) bool { return m.Score < minScore })
		}
		analysedSeconds.Add((w.To - w.From).Seconds())
		b.Detections = Pick(lib, ms, w.T0, w.Dur())
		onBlock(b)
	})
}

// Window is one analysis step: PCM starts at media time T0 and newly covers [From,To).
// Except for the last window, PCM runs Overlap past To. PCM is only valid during the
// callback: its buffer is reused.
type Window struct {
	PCM      []float32
	T0       time.Duration
	From, To time.Duration
}

// Dur is the length of the window's PCM.
func (w Window) Dur() time.Duration {
	return mediatime.Samples(int64(len(w.PCM)), fingerprint.SampleRate)
}

// Windows cuts mono s16le PCM starting at media time t0 into Block steps, each with
// Overlap of context carried into the next one, and returns the media time reached.
func Windows(ctx context.Context, r io.Reader, t0 time.Duration, fn func(Window)) (time.Duration, error) {
	blockN := int(mediatime.SamplesAt(Block, fingerprint.SampleRate))
	overlapN := int(mediatime.SamplesAt(Overlap, fingerprint.SampleRate))
	raw := make([]byte, 2*fingerprint.SampleRate) // 1 s reads
	buf := make([]float32, 0, blockN+overlapN+fingerprint.SampleRate)
	bufT0 := t0
	covered := t0

	process := func(n int, final bool) {
		win := buf
		if !final && len(win) > n+overlapN {
			win = win[:n+overlapN]
		}
		from := covered
		if final {
			covered = bufT0 + mediatime.Samples(int64(len(buf)), fingerprint.SampleRate)
		} else {
			covered = bufT0 + mediatime.Samples(int64(n), fingerprint.SampleRate)
		}
		fn(Window{PCM: win, T0: bufT0, From: from, To: covered})
	}

	for {
		if err := ctx.Err(); err != nil {
			return covered, err
		}
		k, err := io.ReadFull(r, raw)
		buf = fingerprint.AppendS16LE(buf, raw[:k&^1])
		for len(buf) >= blockN+overlapN {
			process(blockN, false)
			buf = buf[:copy(buf, buf[blockN:])] // the overlap moves to the front: no reallocation
			bufT0 += Block
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if len(buf) > 0 {
				process(len(buf), true)
			}
			return covered, nil
		}
		if err != nil {
			return covered, err
		}
	}
}

// Pick chooses one alignment per ad. Repetitive ads (looping jingles) produce
// several near-equal offsets; prefer the one with the highest match density, i.e. score
// per second of overlap between the window and the implied ad interval.
func Pick(lib *library.Library, ms []library.Match, winT0, winDur time.Duration) []Detection {
	best := map[fingerprint.ID]library.Match{}
	for _, m := range ms {
		if b, ok := best[m.AdID]; !ok || m.Score > b.Score {
			best[m.AdID] = m
		}
	}
	chosen := map[fingerprint.ID]Detection{}
	density := map[fingerprint.ID]float64{}
	for _, m := range ms {
		ad := lib.Get(m.AdID)
		if ad == nil || float64(m.Score) < 0.8*float64(best[m.AdID].Score) {
			continue
		}
		start := winT0 - time.Duration(m.Offset)*fingerprint.FrameDur
		ov := min(winT0+winDur, start+ad.Duration) - max(winT0, start)
		if ov < time.Second {
			continue
		}
		if d := float64(m.Score) / ov.Seconds(); d > density[m.AdID] {
			density[m.AdID] = d
			chosen[m.AdID] = Detection{AdID: ad.ID, Label: ad.Label, Type: ad.Type, Start: start, End: start + ad.Duration, Score: m.Score}
		}
	}
	out := make([]Detection, 0, len(chosen))
	for _, d := range chosen {
		out = append(out, d)
	}
	return out
}

// Merge adds d to list. Detections of the same ad that overlap in time are one
// occurrence; the higher score wins. Returns the new list and whether d is new/changed.
func Merge(list []Detection, d Detection) ([]Detection, bool) {
	for i, e := range list {
		if e.AdID == d.AdID && d.Start < e.End && e.Start < d.End {
			if d.Score > e.Score {
				changed := (d.Start - e.Start).Abs() > 100*time.Millisecond
				list[i] = d
				return list, changed
			}
			return list, false
		}
	}
	return append(list, d), true
}
