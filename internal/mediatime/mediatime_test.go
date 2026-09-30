package mediatime

import (
	"math"
	"math/big"
	"math/rand/v2"
	"testing"
	"time"
)

func TestConversions(t *testing.T) {
	if got := Ticks(1152, 1, 48000); got != 24*time.Millisecond {
		t.Errorf("Ticks: %v", got)
	}
	if got := Samples(3*3600*44100+1, 44100); got != 3*time.Hour+22676*time.Nanosecond {
		t.Errorf("Samples: %v", got)
	}
	if got := SamplesAt(1500*time.Millisecond, 8000); got != 12000 {
		t.Errorf("SamplesAt: %d", got)
	}
	for d, want := range map[time.Duration]int32{1499 * time.Microsecond: 1, 1500 * time.Microsecond: 2, -2 * time.Millisecond: -2,
		1000 * time.Hour: math.MaxInt32} {
		if got := Ms(d); got != want {
			t.Errorf("Ms(%v) = %d, want %d", d, got, want)
		}
	}
	if Dur(1500) != 1500*time.Millisecond {
		t.Error("Dur")
	}
}

// Ticks is exact, and agrees with the float form wherever that one is.
func TestTicksExact(t *testing.T) {
	for _, c := range []struct {
		t, num, den int64
		want        time.Duration
	}{
		{1, 1, 3, 333333333},
		{2, 1, 3, 666666667},
		{-2, 1, 3, -666666667},
		{1, 1, 2_000_000_000, 1}, // 0.5 ns rounds away from zero
		{-1, 1, 2_000_000_000, -1},
		{1<<33 - 1, 1, 90000, 95443717677778}, // a 33-bit PTS
		{3, 1001, 30000, 100100000},           // NTSC frames
		{math.MaxInt64, 1, 1, math.MaxInt64},  // saturates
		{math.MinInt64, 1, 1, math.MinInt64},  // saturates
		{5, 1, 0, 0},
	} {
		if got := Ticks(c.t, c.num, c.den); got != c.want {
			t.Errorf("Ticks(%d, %d, %d) = %d, want %d", c.t, c.num, c.den, got, c.want)
		}
	}
	// against math/big, rounding half away from zero
	exact := func(tk, num, den int64) time.Duration {
		n := new(big.Int).Mul(big.NewInt(tk), big.NewInt(num))
		n.Mul(n, big.NewInt(int64(time.Second)))
		d := big.NewInt(den)
		q, m := new(big.Int).QuoRem(n, d, new(big.Int))
		if m.Mul(m.Abs(m), big.NewInt(2)).Cmp(new(big.Int).Abs(d)) >= 0 {
			if (n.Sign() < 0) != (d.Sign() < 0) {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
		}
		if !q.IsInt64() { // Ticks saturates
			if q.Sign() < 0 {
				return math.MinInt64
			}
			return math.MaxInt64
		}
		return time.Duration(q.Int64())
	}
	r := rand.New(rand.NewPCG(3, 4))
	for range 100000 {
		tk, num, den := r.Int64N(1<<40)-1<<39, r.Int64N(2000)+1, r.Int64N(200000)+1
		if got, want := Ticks(tk, num, den), exact(tk, num, den); got != want {
			t.Fatalf("Ticks(%d, %d, %d) = %d, want %d", tk, num, den, got, want)
		}
	}
}
