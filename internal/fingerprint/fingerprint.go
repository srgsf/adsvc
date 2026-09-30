// Package fingerprint computes Shazam-style landmark hashes from mono PCM: peaks of a
// log-magnitude spectrogram, paired into f1|f2|Δt hashes packed in a uint32.
package fingerprint

import (
	"cmp"
	"encoding/binary"
	"math"
	"math/cmplx"
	"slices"
	"sync"
	"time"
)

// Version is the version of the fingerprint parameters below. Results stored with another
// version are discarded, because every ad in a library has to be re-enrolled when the
// front-end changes.
const Version = 1

// HashBits is the width of a landmark hash: f1 (fBits) | f2 (fBits) | dt (dtBits).
const HashBits = 2*fBits + dtBits

// Widths of the fields of a hash; maxBin and maxDT must fit them.
const (
	fBits  = 9
	dtBits = 6
)

// Compile-time checks that the parameters fit the hash fields (a negative array length
// does not compile).
var (
	_ [1<<fBits - maxBin]struct{}
	_ [1<<dtBits - 1 - maxDT]struct{}
)

// Audio front-end parameters. PCM is expected as mono int16 at SampleRate.
const (
	SampleRate = 8000
	FFTSize    = 1024
	Hop        = 256                            // 32 ms per frame
	FrameDur   = Hop * time.Second / SampleRate // one frame: 32 ms

	nBins        = FFTSize / 2
	minBin       = 4   // skip DC / rumble (<31 Hz)
	maxBin       = 500 // ~3.9 kHz
	peakNbT      = 3   // peak neighbourhood, frames
	peakNbF      = 10  // peak neighbourhood, bins
	peaksPerSec  = 30
	fanOut       = 6
	maxDT        = 1<<dtBits - 1
	peakFloorLog = 0.5
)

// Point is one landmark hash anchored at frame T (relative to the analysed PCM start).
type Point struct {
	H uint32
	T int32
}

type peak struct {
	t, f int32
	v    float32
}

var (
	hann    [FFTSize]float64
	twiddle [FFTSize / 2]complex128
)

func init() {
	for i := range hann {
		hann[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(FFTSize-1))
	}
	for i := range twiddle {
		twiddle[i] = cmplx.Exp(complex(0, -2*math.Pi*float64(i)/float64(FFTSize)))
	}
}

// fft is an in-place iterative radix-2 FFT for len == FFTSize.
func fft(a []complex128) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half, step := size/2, n/size
		for start := 0; start < n; start += size {
			for k := range half {
				w := twiddle[k*step]
				u, v := a[start+k], a[start+k+half]*w
				a[start+k], a[start+k+half] = u+v, u-v
			}
		}
	}
}

// scratch holds the buffers of one Compute, reused through scratches: at 13 s per block
// they are about 400 KB, allocated per block otherwise.
type scratch struct {
	buf        []complex128
	spec, fmax []float32 // [frame*nBins + bin]
	cands, out []peak
}

var scratches = sync.Pool{New: func() any { return &scratch{buf: make([]complex128, FFTSize)} }}

// spectrogram fills sc.spec with log-magnitude frames and returns their count.
func (sc *scratch) spectrogram(pcm []float32) int {
	if len(pcm) < FFTSize {
		return 0
	}
	nFrames := (len(pcm)-FFTSize)/Hop + 1
	sc.spec = slices.Grow(sc.spec[:0], nFrames*nBins)[:nFrames*nBins]
	buf := sc.buf
	for fi := range nFrames {
		off := fi * Hop
		for i := range FFTSize {
			buf[i] = complex(float64(pcm[off+i])*hann[i], 0)
		}
		fft(buf)
		row := sc.spec[fi*nBins : (fi+1)*nBins]
		for b := range nBins {
			row[b] = float32(math.Log1p(cmplx.Abs(buf[b])))
		}
	}
	return nFrames
}

// findPeaks picks local maxima (separable max filter) of the nT frames of sc.spec and
// keeps the strongest per second, ordered by time then frequency (in sc.out).
func (sc *scratch) findPeaks(nT int) []peak {
	if nT == 0 {
		return nil
	}
	spec := sc.spec
	// max over frequency neighbourhood
	sc.fmax = slices.Grow(sc.fmax[:0], nT*nBins)[:nT*nBins]
	fmax := sc.fmax
	for t := range nT {
		row, out := spec[t*nBins:(t+1)*nBins], fmax[t*nBins:(t+1)*nBins]
		for f := minBin; f < maxBin; f++ {
			m := float32(0)
			for _, v := range row[max(0, f-peakNbF):min(nBins, f+peakNbF+1)] {
				m = max(m, v)
			}
			out[f] = m
		}
	}
	cands := sc.cands[:0]
	for t := range nT {
		for f := minBin; f < maxBin; f++ {
			v := spec[t*nBins+f]
			if v < peakFloorLog || v < fmax[t*nBins+f] {
				continue
			}
			isMax := true
			for k := max(0, t-peakNbT); k <= min(nT-1, t+peakNbT) && isMax; k++ {
				if k != t && fmax[k*nBins+f] > v {
					isMax = false
				}
			}
			if isMax {
				cands = append(cands, peak{int32(t), int32(f), v})
			}
		}
	}
	sc.cands = cands
	// keep top-N per ~1 s bucket: candidates come in time order, so a bucket is a run
	framesPerSec := int32(math.Round(float64(time.Second) / float64(FrameDur)))
	out := sc.out[:0]
	for len(cands) > 0 {
		n := 1
		for n < len(cands) && cands[n].t/framesPerSec == cands[0].t/framesPerSec {
			n++
		}
		ps := cands[:n]
		slices.SortFunc(ps, func(a, b peak) int { return cmp.Compare(b.v, a.v) })
		out = append(out, ps[:min(len(ps), peaksPerSec)]...)
		cands = cands[n:]
	}
	slices.SortFunc(out, func(a, b peak) int { return cmp.Or(cmp.Compare(a.t, b.t), cmp.Compare(a.f, b.f)) })
	sc.out = out
	return out
}

// Compute converts mono PCM (float32 in [-1,1] at SampleRate) into landmark hashes.
func Compute(pcm []float32) []Point {
	sc := scratches.Get().(*scratch)
	defer scratches.Put(sc)
	peaks := sc.findPeaks(sc.spectrogram(pcm))
	pts := make([]Point, 0, fanOut*len(peaks))
	for i, a := range peaks {
		n := 0
		for j := i + 1; j < len(peaks) && n < fanOut; j++ {
			b := peaks[j]
			dt := b.t - a.t
			if dt == 0 {
				continue
			}
			if dt > maxDT {
				break
			}
			h := uint32(a.f)<<(fBits+dtBits) | uint32(b.f)<<dtBits | uint32(dt)
			pts = append(pts, Point{H: h, T: a.t})
			n++
		}
	}
	return pts
}

// FromS16LE converts little-endian signed 16-bit PCM into float32 samples in [-1,1).
// A trailing odd byte is ignored.
func FromS16LE(b []byte) []float32 { return AppendS16LE(make([]float32, 0, len(b)/2), b) }

// AppendS16LE appends the samples of b (as FromS16LE) to dst.
func AppendS16LE(dst []float32, b []byte) []float32 {
	for i := 0; i+1 < len(b); i += 2 {
		dst = append(dst, float32(int16(binary.LittleEndian.Uint16(b[i:])))/32768)
	}
	return dst
}
