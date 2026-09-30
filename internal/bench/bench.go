// Package bench is the labelled benchmark: where the library's ads really occur in real
// files, scored against what the detector finds, either offline (ffmpeg decodes the whole
// file, optionally degraded) or through a real Proxy (the container demuxers and the
// minimal ffmpeg).
package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/srgsf/adsvc/internal/ffmpeg"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/library"
	"github.com/srgsf/adsvc/internal/mediatime"
)

// Set is the ground truth. Recall only concerns ads that are in the library.
type Set struct {
	DataDir string `json:"data_dir,omitempty"` // the data directory whose ads the ids refer to
	Files   []File `json:"files"`
}

// File is one programme, available as one or more files (releases, containers) that
// share its audio timeline, so the same labels apply to each.
type File struct {
	Paths []string     `json:"paths"`
	Ads   []Occurrence `json:"ads"`
	Note  string       `json:"note,omitempty"`
}

// Where a label's boundaries come from. Boundary error is only measured against manual
// labels: the other two are the detector's own alignment.
const (
	SourceManual   = "manual"   // set by hand
	SourceEnrolled = "enrolled" // the segment the ad was enrolled from (a self-match)
	SourceDetected = "detected" // bootstrapped from a scan (bench -draft)
)

// Occurrence is one occurrence of a library ad on the file's media timeline. In the labels
// file its times are integer milliseconds: startMs, endMs.
type Occurrence struct {
	Ad       fingerprint.ID
	Label    string
	Start    time.Duration
	End      time.Duration
	Source   string
	Verified bool // a person has checked that the ad is really there
	Note     string
}

type occurrenceJSON struct {
	Ad       fingerprint.ID `json:"ad"` // UUID form
	Label    string         `json:"label,omitempty"`
	StartMs  int32          `json:"startMs"`
	EndMs    int32          `json:"endMs"`
	Source   string         `json:"source,omitempty"`
	Verified bool           `json:"verified"`
	Note     string         `json:"note,omitempty"`
}

func (o Occurrence) toJSON() occurrenceJSON {
	return occurrenceJSON{o.Ad, o.Label, mediatime.Ms(o.Start), mediatime.Ms(o.End), o.Source, o.Verified, o.Note}
}

// MarshalJSON writes the times as integer milliseconds.
func (o Occurrence) MarshalJSON() ([]byte, error) { return json.Marshal(o.toJSON()) }

// UnmarshalJSON reads what MarshalJSON writes.
func (o *Occurrence) UnmarshalJSON(b []byte) error {
	var j occurrenceJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*o = Occurrence{j.Ad, j.Label, mediatime.Dur(j.StartMs), mediatime.Dur(j.EndMs), j.Source, j.Verified, j.Note}
	return nil
}

// Load reads a labels file. Relative paths in it are relative to the file itself.
func Load(path string) (*Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var set Set
	if err := json.Unmarshal(b, &set); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	rel := func(p string) string {
		if p == "" || filepath.IsAbs(p) || ffmpeg.IsHTTP(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	set.DataDir = rel(set.DataDir)
	for i := range set.Files {
		f := &set.Files[i]
		if len(f.Paths) == 0 {
			return nil, fmt.Errorf("%s: file %d has no paths", path, i+1)
		}
		for j := range f.Paths {
			f.Paths[j] = rel(f.Paths[j])
		}
		for _, a := range f.Ads {
			if a.End <= a.Start {
				return nil, fmt.Errorf("%s: %s: ad %s at %dms ends before it starts", path, f.Paths[0], a.Ad.Short(), mediatime.Ms(a.Start))
			}
		}
	}
	return &set, nil
}

// Check compares the labels with the library: unknown ads, other labels, other durations.
func (s *Set) Check(lib *library.Library) []string {
	var warn []string
	for _, f := range s.Files {
		for _, a := range f.Ads {
			ad := lib.Get(a.Ad)
			switch {
			case ad == nil:
				warn = append(warn, fmt.Sprintf("%s: ad %s is not in the library", filepath.Base(f.Paths[0]), a.Ad.Short()))
			case a.Label != "" && a.Label != ad.Label:
				warn = append(warn, fmt.Sprintf("%s: ad %s is %q in the library, %q in the labels", filepath.Base(f.Paths[0]), a.Ad.Short(), ad.Label, a.Label))
			case ((a.End - a.Start) - ad.Duration).Abs() > 500*time.Millisecond:
				warn = append(warn, fmt.Sprintf("%s: ad %s at %v lasts %v, the library's %v", filepath.Base(f.Paths[0]), a.Ad.Short(), a.Start, a.End-a.Start, ad.Duration))
			}
		}
	}
	return warn
}
