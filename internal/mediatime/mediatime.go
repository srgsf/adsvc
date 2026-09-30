// Package mediatime converts media times. Code keeps positions and durations on the media
// timeline as time.Duration; the HTTP API and the catalogue carry them as int32
// milliseconds (Ms, Dur). Only the web page renders seconds and minutes.
package mediatime

import (
	"math"
	"math/bits"
	"time"
)

// Ticks is t units of num/den seconds (a sample count at a rate, a container timescale),
// rounded to the nearest nanosecond (half away from zero), in exact integer arithmetic;
// it saturates where the result does not fit a Duration.
func Ticks(t, num, den int64) time.Duration {
	if den == 0 {
		return 0
	}
	neg := (t < 0) != (num < 0) != (den < 0)
	a, b, d := abs(t), abs(num), abs(den)
	hi1, b := bits.Mul64(b, uint64(time.Second)) // num seconds in ns: 128 bits
	hi, lo := bits.Mul64(a, b)
	if hi1 != 0 {
		h2, l2 := bits.Mul64(a, hi1)
		if h2 != 0 {
			return saturate(neg)
		}
		var c uint64
		hi, c = bits.Add64(hi, l2, 0)
		if c != 0 {
			return saturate(neg)
		}
	}
	lo, c := bits.Add64(lo, d/2, 0) // round half away from zero
	hi += c
	if hi >= d {
		return saturate(neg)
	}
	q, _ := bits.Div64(hi, lo, d)
	if q > math.MaxInt64 {
		return saturate(neg)
	}
	if neg {
		return -time.Duration(q)
	}
	return time.Duration(q)
}

func abs(v int64) uint64 {
	if v < 0 {
		return uint64(-v) //nolint:gosec // G115: MinInt64 wraps to 1<<63, its magnitude
	}
	return uint64(v)
}

func saturate(neg bool) time.Duration {
	if neg {
		return math.MinInt64
	}
	return math.MaxInt64
}

// Samples is the duration of n samples at rate Hz.
func Samples(n int64, rate int) time.Duration { return Ticks(n, 1, int64(rate)) }

// SamplesAt is how many samples at rate Hz d holds (rounded).
func SamplesAt(d time.Duration, rate int) int64 {
	return int64(math.Round(d.Seconds() * float64(rate)))
}

// Ms is d in whole milliseconds, rounded, clamped to int32 (about ±24.8 days).
func Ms(d time.Duration) int32 {
	ms := math.Round(float64(d) / float64(time.Millisecond))
	return int32(max(min(ms, math.MaxInt32), math.MinInt32))
}

// Dur is ms milliseconds.
func Dur(ms int32) time.Duration { return time.Duration(ms) * time.Millisecond }
