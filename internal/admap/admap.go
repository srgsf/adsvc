// Package admap stores the per-file ad maps: where a file has been analysed and which ads
// were found in it. It is the first (and cheapest) database tier: a known file needs no
// analysis. The maps live in the catalogue; this package converts them to and from the
// detector's types (time.Duration in memory, integer milliseconds in the catalogue).
package admap

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/mediatime"
)

// FileMap is what is known about one file: where it has been analysed and which ads were
// found. In JSON (the API) times are integer milliseconds.
type FileMap struct {
	Key      string             `json:"key"`
	Aliases  []string           `json:"aliases,omitempty"`
	Size     int64              `json:"size,omitempty"`
	Duration time.Duration      `json:"-"` // durationMs in JSON
	Analyzed detect.Intervals   `json:"analyzed"`
	Ads      []detect.Detection `json:"ads"`
	Updated  time.Time          `json:"updated,omitzero"` // zero for a live session's map
}

type fileMapJSON struct {
	DurationMs int32 `json:"durationMs,omitempty"`
}

// MarshalJSON writes the duration as durationMs.
func (m FileMap) MarshalJSON() ([]byte, error) {
	type plain FileMap
	return json.Marshal(struct {
		plain
		fileMapJSON
	}{plain(m), fileMapJSON{mediatime.Ms(m.Duration)}})
}

// UnmarshalJSON reads what MarshalJSON writes.
func (m *FileMap) UnmarshalJSON(b []byte) error {
	type plain FileMap
	var v struct {
		plain
		fileMapJSON
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*m = FileMap(v.plain)
	m.Duration = mediatime.Dur(v.DurationMs)
	return nil
}

// Store is the per-file ad maps of a catalogue, keyed by file key, with aliases between
// the keys a file can be known by (infohash+index from the caller, content hash from its
// own bytes).
type Store struct{ db *catalog.DB }

// New is the store of db's file maps.
func New(db *catalog.DB) *Store { return &Store{db} }

// Lookup returns the map stored under any of keys, or nil. Analysed ranges computed with
// other fingerprint parameters are dropped: those files have to be analysed again.
func (s *Store) Lookup(ctx context.Context, keys ...string) (*FileMap, error) {
	m, err := s.db.FileMap(ctx, keys...)
	if err != nil || m == nil {
		return nil, err
	}
	return fromCatalog(m), nil
}

// Put stores m under m.Key plus its aliases, replacing what was stored under any of them,
// and publishes its confirmed ads when they changed (catalog.SaveFileMap).
func (s *Store) Put(ctx context.Context, m *FileMap) error {
	if m == nil || m.Key == "" {
		return nil
	}
	return s.db.SaveFileMap(ctx, toCatalog(m))
}

// PeerAds is what other origins found in the file known by keys, as far as this node
// trusts them, for the ads tracked says are tracked (catalog.PeerAdsIn).
func (s *Store) PeerAds(ctx context.Context, tracked catalog.Tracked, keys ...string) ([]detect.Detection, error) {
	ds, err := s.db.PeerAdsIn(ctx, tracked, keys...)
	if err != nil {
		return nil, err
	}
	out := make([]detect.Detection, len(ds))
	for i, d := range ds {
		out[i] = detectionOf(d)
	}
	return out, nil
}

// List returns every stored map, newest first.
func (s *Store) List(ctx context.Context) ([]*FileMap, error) {
	ms, err := s.db.FileMaps(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*FileMap, len(ms))
	for i := range ms {
		out[i] = fromCatalog(&ms[i])
	}
	return out, nil
}

// ResetAnalyzed forgets what has been analysed in every file, keeping the ads found: after
// a new ad is enrolled, every file has to be looked at again.
func (s *Store) ResetAnalyzed(ctx context.Context) error { return s.db.ResetAnalyzed(ctx) }

func fromCatalog(m *catalog.FileMap) *FileMap {
	f := &FileMap{Key: m.Key, Aliases: m.Aliases, Size: m.Size, Duration: mediatime.Dur(m.DurationMs), Updated: m.Updated}
	if m.FPVersion == fingerprint.Version {
		f.Analyzed = detect.FromMs(m.Analyzed)
	}
	for _, d := range m.Detections {
		f.Ads = append(f.Ads, detectionOf(d))
	}
	return f
}

func toCatalog(f *FileMap) *catalog.FileMap {
	m := &catalog.FileMap{Key: f.Key, Aliases: slices.Clone(f.Aliases), FPVersion: fingerprint.Version, Size: f.Size,
		DurationMs: mediatime.Ms(f.Duration), Analyzed: f.Analyzed.Ms()}
	for _, d := range f.Ads {
		m.Detections = append(m.Detections, catalogDetection(d))
	}
	return m
}

// detectionOf is a detection of the catalogue (integer milliseconds) as the detector's.
func detectionOf(d catalog.Detection) detect.Detection {
	return detect.Detection{AdID: d.Ad, Label: d.Label, Type: d.Type, Start: mediatime.Dur(d.StartMs), End: mediatime.Dur(d.EndMs),
		Score: d.Score, Confirmed: d.Confirmed}
}

// catalogDetection is detectionOf the other way.
func catalogDetection(d detect.Detection) catalog.Detection {
	return catalog.Detection{Ad: d.AdID, Label: d.Label, Type: d.Type, StartMs: mediatime.Ms(d.Start), EndMs: mediatime.Ms(d.End),
		Score: d.Score, Confirmed: d.Confirmed}
}
