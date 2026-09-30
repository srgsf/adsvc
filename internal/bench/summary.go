package bench

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/mediatime"
)

// Summary aggregates runs. Zero counts mean the matching figures were not measured.
type Summary struct {
	Runs       int           `json:"runs"`
	Failed     int           `json:"failed"`
	Labels     int           `json:"labels"`
	Unverified int           `json:"unverified"`
	Found      int           `json:"found"`
	Confirmed  int           `json:"confirmed"` // found and confirmed by the end of the file
	Recall     float64       `json:"recall"`
	FalsePos   int           `json:"falsePos"`
	Analysed   time.Duration `json:"-"` // audio analysed

	StartErrN       int           `json:"startErrN"` // found occurrences with manual labels
	StartErrMeanAbs time.Duration `json:"-"`
	StartErrMaxAbs  time.Duration `json:"-"`

	WeakestTrue    int               `json:"weakestTrue"` // lowest score of a found occurrence
	StrongestNoise *detect.Detection `json:"strongestNoise,omitempty"`
	NoisePath      string            `json:"noisePath,omitempty"`

	DetectN       int           `json:"detectN"`
	DetectMedian  time.Duration `json:"-"`
	DetectMax     time.Duration `json:"-"`
	ConfirmN      int           `json:"confirmN"`
	ConfirmMedian time.Duration `json:"-"`
	ConfirmMax    time.Duration `json:"-"`

	SpeedMedian float64 `json:"speedMedian"` // x realtime, per file
}

// MarshalJSON writes the times as integer milliseconds (analysed audio in ms too, as an
// int64: a long benchmark exceeds what int32 ms holds).
func (s Summary) MarshalJSON() ([]byte, error) {
	type plain Summary
	ms := mediatime.Ms
	return json.Marshal(struct {
		plain
		AnalysedMs        int64 `json:"analysedMs"`
		StartErrMeanAbsMs int32 `json:"startErrMeanAbsMs"`
		StartErrMaxAbsMs  int32 `json:"startErrMaxAbsMs"`
		DetectMedianMs    int32 `json:"detectMedianMs"`
		DetectMaxMs       int32 `json:"detectMaxMs"`
		ConfirmMedianMs   int32 `json:"confirmMedianMs"`
		ConfirmMaxMs      int32 `json:"confirmMaxMs"`
	}{plain(s), s.Analysed.Milliseconds(), ms(s.StartErrMeanAbs), ms(s.StartErrMaxAbs), ms(s.DetectMedian), ms(s.DetectMax),
		ms(s.ConfirmMedian), ms(s.ConfirmMax)})
}

// Summarize aggregates runs.
func Summarize(runs []Run) Summary {
	var s Summary
	var errs, detect, confirm []time.Duration
	var speed []float64
	for _, r := range runs {
		s.Runs++
		if r.Err != "" {
			s.Failed++
			continue
		}
		s.Analysed += r.Analysed
		s.FalsePos += len(r.FalsePos)
		speed = append(speed, r.Speed())
		if r.Noise != nil && (s.StrongestNoise == nil || r.Noise.Score > s.StrongestNoise.Score) {
			s.StrongestNoise, s.NoisePath = r.Noise, r.Path
		}
		for _, h := range r.Hits {
			s.Labels++
			if !h.Verified {
				s.Unverified++
			}
			if !h.Found {
				continue
			}
			s.Found++
			if h.Det.Confirmed {
				s.Confirmed++
			}
			if s.WeakestTrue == 0 || h.Det.Score < s.WeakestTrue {
				s.WeakestTrue = h.Det.Score
			}
			if h.Source == SourceManual {
				errs = append(errs, h.StartErr.Abs())
			}
			if h.DetectAt != nil {
				detect = append(detect, *h.DetectAt)
			}
			if h.ConfirmAt != nil {
				confirm = append(confirm, *h.ConfirmAt)
			}
		}
	}
	if s.Labels > 0 {
		s.Recall = float64(s.Found) / float64(s.Labels)
	}
	s.StartErrN = len(errs)
	var sum time.Duration
	for _, e := range errs {
		sum += e
		s.StartErrMaxAbs = max(s.StartErrMaxAbs, e)
	}
	if len(errs) > 0 {
		s.StartErrMeanAbs = sum / time.Duration(len(errs))
	}
	s.DetectN, s.DetectMedian, s.DetectMax = len(detect), median(detect), maxOf(detect)
	s.ConfirmN, s.ConfirmMedian, s.ConfirmMax = len(confirm), median(confirm), maxOf(confirm)
	s.SpeedMedian = median(speed)
	return s
}

func median[T time.Duration | float64](v []T) T {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func maxOf(v []time.Duration) time.Duration {
	var m time.Duration
	for i, x := range v {
		if i == 0 || x > m {
			m = x
		}
	}
	return m
}
