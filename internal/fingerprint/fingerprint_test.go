package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"math/rand/v2"
	"testing"
)

// goldenPCM is 20 s of deterministic audio: stepped tones over noise.
func goldenPCM() []float32 {
	r := rand.New(rand.NewPCG(1, 2))
	pcm := make([]float32, 20*SampleRate)
	for i := range pcm {
		t := float64(i) / SampleRate
		f := 300 * math.Pow(2, math.Floor(math.Mod(t*1.7, 7))/6)
		pcm[i] = float32(0.3*math.Sin(2*math.Pi*f*t) + 0.2*math.Sin(2*math.Pi*1800*t*(1+0.1*math.Floor(t))) + 0.05*(r.Float64()*2-1))
	}
	return pcm
}

// Compute's output is the content of an ad (its id is the hash of it): any change to it
// is a new fingerprint Version. This pins it.
func TestComputeGolden(t *testing.T) {
	pts := Compute(goldenPCM())
	h := sha256.New()
	for _, p := range pts {
		h.Write([]byte{byte(p.H >> 24), byte(p.H >> 16), byte(p.H >> 8), byte(p.H), byte(p.T >> 24), byte(p.T >> 16), byte(p.T >> 8), byte(p.T)})
	}
	const want = "9733a7c626c626c2f391034281d32a43bbbbeb342f743131666c655978128a31"
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		t.Fatalf("Compute changed: %d landmarks, digest %s (bump fingerprint.Version if this is intended)", len(pts), got)
	}
}

func BenchmarkCompute(b *testing.B) {
	pcm := goldenPCM()[:13*SampleRate] // one analysis window
	b.ReportAllocs()
	for b.Loop() {
		Compute(pcm)
	}
}
