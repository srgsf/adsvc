package decode

import (
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/mediatime"
)

const ms = time.Millisecond

// samples writes d of PCM from t0 whose values are the sample's index on the media
// timeline (mod 2^15), so a slice tells where its samples came from.
func samples(r *PCMRing, t0, d time.Duration) {
	first := mediatime.SamplesAt(t0, fingerprint.SampleRate)
	n := mediatime.SamplesAt(d, fingerprint.SampleRate)
	b := make([]byte, 0, 2*n)
	for i := range n {
		v := int16((first + i) % 32768)
		b = append(b, byte(v), byte(uint16(v)>>8))
	}
	_, _ = r.Write(b)
}

func TestRingResetInsideSpanKeepsEarlierAudio(t *testing.T) {
	r := NewPCMRing(10 * time.Second)
	r.Reset(100 * time.Second)
	samples(r, 100*time.Second, 3*time.Second)
	r.Reset(102500 * ms) // a step back into what was played
	samples(r, 102500*ms, 2*time.Second)
	if from, to := r.Span(); from != 100*time.Second || to != 104500*ms {
		t.Fatalf("span %v..%v", from, to)
	}
	pcm, err := r.Slice(100*time.Second, 104500*ms)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range pcm {
		if want := float32(int16((100*fingerprint.SampleRate+i)%32768)) / 32768; v != want {
			t.Fatalf("sample %d: %v, want %v", i, v, want)
		}
	}

	r.Reset(200 * time.Second) // a real seek: a new run
	if from, to := r.Span(); from != 200*time.Second || to != 200*time.Second {
		t.Fatalf("span after a seek %v..%v", from, to)
	}
}

func TestRingResetAfterWrapKeepsOnlyWhatIsHeld(t *testing.T) {
	r := NewPCMRing(2 * time.Second)
	r.Reset(0)
	samples(r, 0, 5*time.Second) // holds 3..5
	r.Reset(4 * time.Second)
	if from, to := r.Span(); from != 3*time.Second || to != 4*time.Second {
		t.Fatalf("span %v..%v", from, to)
	}
	if _, err := r.Slice(2500*ms, 4*time.Second); err == nil {
		t.Fatal("overwritten audio returned")
	}
	samples(r, 4*time.Second, 500*ms)
	if from, _ := r.Span(); from != 3*time.Second {
		t.Fatalf("held from %v", from)
	}
}
