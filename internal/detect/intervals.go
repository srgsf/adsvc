package detect

import (
	"cmp"
	"encoding/json"
	"slices"
	"time"

	"github.com/srgsf/adsvc/internal/mediatime"
)

// Intervals is a sorted set of non-overlapping [From, To) ranges of media time. In JSON
// (the proxy's API) it is [[fromMs, toMs], ...] in integer milliseconds.
type Intervals [][2]time.Duration

// Tolerances of the set.
const (
	joinGap = 50 * time.Millisecond  // ranges this close are merged, and count as covering
	minGap  = 500 * time.Millisecond // shorter holes are not worth analysing
)

// Add returns the set with [from,to) added. Ranges closer than 50 ms are merged.
func (iv Intervals) Add(from, to time.Duration) Intervals {
	if to <= from {
		return iv
	}
	out := append(Intervals{}, iv...)
	out = append(out, [2]time.Duration{from, to})
	slices.SortFunc(out, func(a, b [2]time.Duration) int { return cmp.Compare(a[0], b[0]) })
	merged := Intervals{out[0]}
	for _, r := range out[1:] {
		last := &merged[len(merged)-1]
		if r[0] <= last[1]+joinGap {
			if r[1] > last[1] {
				last[1] = r[1]
			}
		} else {
			merged = append(merged, r)
		}
	}
	return merged
}

// FirstGap returns the first uncovered sub-range of [from, to).
func (iv Intervals) FirstGap(from, to time.Duration) (time.Duration, time.Duration, bool) {
	p := from
	for _, r := range iv {
		if r[1] <= p {
			continue
		}
		if r[0] > p {
			end := min(r[0], to)
			if end-p > minGap {
				return p, end, true
			}
		}
		if r[1] > p {
			p = r[1]
		}
		if p >= to {
			return 0, 0, false
		}
	}
	if to-p > minGap {
		return p, to, true
	}
	return 0, 0, false
}

// Contains reports whether [from,to) is fully covered.
func (iv Intervals) Contains(from, to time.Duration) bool {
	for _, r := range iv {
		if r[0] <= from+joinGap && r[1] >= to-joinGap {
			return true
		}
	}
	return false
}

// Ms is the set in integer milliseconds, as the API and the catalogue carry it.
func (iv Intervals) Ms() [][2]int32 {
	out := make([][2]int32, len(iv))
	for i, r := range iv {
		out[i] = [2]int32{mediatime.Ms(r[0]), mediatime.Ms(r[1])}
	}
	return out
}

// FromMs is the set of ms ranges (as Ms writes them).
func FromMs(ms [][2]int32) Intervals {
	out := make(Intervals, len(ms))
	for i, r := range ms {
		out[i] = [2]time.Duration{mediatime.Dur(r[0]), mediatime.Dur(r[1])}
	}
	return out
}

// MarshalJSON writes the ranges in integer milliseconds.
func (iv Intervals) MarshalJSON() ([]byte, error) { return json.Marshal(iv.Ms()) }

// UnmarshalJSON reads what MarshalJSON writes.
func (iv *Intervals) UnmarshalJSON(b []byte) error {
	var ms [][2]int32
	if err := json.Unmarshal(b, &ms); err != nil {
		return err
	}
	*iv = FromMs(ms)
	return nil
}
