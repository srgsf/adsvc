package decode

import (
	"errors"
	"sync"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/mediatime"
)

// PCMRing keeps the most recent decoded audio of the current run, so an ad can be enrolled
// from bytes the player already streamed instead of reading the source again.
type PCMRing struct {
	mu   sync.Mutex
	size int     // samples held at most
	buf  []int16 // allocated on the first sample: a session that decodes nothing pays nothing
	t0   time.Duration
	n    int64 // samples written since Reset
	lo   int64 // no sample before this one is held (a Reset that continued the run)
	odd  []byte
}

// NewPCMRing holds the last d of audio (5 minutes when d <= 0).
func NewPCMRing(d time.Duration) *PCMRing {
	if d <= 0 {
		d = 5 * time.Minute
	}
	return &PCMRing{size: int(mediatime.SamplesAt(d, fingerprint.SampleRate))}
}

// Bytes is the memory the ring holds.
func (r *PCMRing) Bytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return 2 * len(r.buf)
}

// Release frees the ring's memory; it must not be written afterwards.
func (r *PCMRing) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	ringBytes.Add(-float64(2 * len(r.buf)))
	r.buf, r.n, r.lo = nil, 0, 0
}

// Reset starts a new run whose first sample is at media time t0. A run that starts inside
// the audio held (a frame step back, a short seek back) continues it from t0: what came
// before stays, so an ad whose start was played before the step can still be enrolled.
func (r *PCMRing) Reset(t0 time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.odd = nil
	if k := mediatime.SamplesAt(t0-r.t0, fingerprint.SampleRate); r.n > 0 && k >= r.first() && k <= r.n {
		r.lo, r.n = r.first(), k
		return
	}
	r.t0, r.n, r.lo = t0, 0, 0
}

// Write consumes mono s16le PCM at fingerprint.SampleRate (it is the tap on the decoder's output).
func (r *PCMRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	written := len(p)
	if len(r.odd) == 1 && len(p) > 0 {
		r.put(int16(uint16(r.odd[0]) | uint16(p[0])<<8))
		p, r.odd = p[1:], nil
	}
	for ; len(p) >= 2; p = p[2:] {
		r.put(int16(uint16(p[0]) | uint16(p[1])<<8))
	}
	if len(p) == 1 {
		r.odd = []byte{p[0]}
	}
	return written, nil
}

func (r *PCMRing) put(v int16) {
	if r.buf == nil {
		r.buf = make([]int16, r.size)
		ringBytes.Add(float64(2 * r.size))
	}
	r.buf[r.n%int64(len(r.buf))] = v
	r.n++
}

// Span returns the media time range currently held.
func (r *PCMRing) Span() (from, to time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.t0 + mediatime.Samples(r.first(), fingerprint.SampleRate), r.t0 + mediatime.Samples(r.n, fingerprint.SampleRate)
}

func (r *PCMRing) first() int64 {
	return max(r.n-int64(r.size), r.lo)
}

// Slice returns the PCM of [from,to) of media time, if the ring still holds it.
func (r *PCMRing) Slice(from, to time.Duration) ([]float32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i0 := mediatime.SamplesAt(from-r.t0, fingerprint.SampleRate)
	i1 := mediatime.SamplesAt(to-r.t0, fingerprint.SampleRate)
	if i1 <= i0 {
		return nil, errors.New("empty range")
	}
	if i0 < r.first() || i1 > r.n {
		return nil, errors.New("outside the audio still buffered (play through the ad first)")
	}
	out := make([]float32, i1-i0)
	m := int64(len(r.buf))
	for i := i0; i < i1; i++ {
		out[i-i0] = float32(r.buf[i%m]) / 32768
	}
	return out, nil
}
