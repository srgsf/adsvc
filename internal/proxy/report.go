package proxy

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/srgsf/adsvc/internal/admap"
	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/filekey"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/library"
	"github.com/srgsf/adsvc/internal/mediatime"
)

// Report is what GET /ads/file returns: the file's map, and what the live session knows
// besides. In JSON, times are integer milliseconds: durationMs, analysedToMs, and those of
// Analyzed and Ads.
type Report struct {
	admap.FileMap
	Container  string
	AnalysedTo time.Duration // media time the analysis reached
}

type reportJSON struct {
	Container    string `json:"container,omitempty"`
	AnalysedToMs int32  `json:"analysedToMs"`
}

// MarshalJSON writes the map's fields and the session's next to them.
func (r Report) MarshalJSON() ([]byte, error) {
	m, err := json.Marshal(r.FileMap)
	if err != nil {
		return nil, err
	}
	extra, err := json.Marshal(reportJSON{r.Container, mediatime.Ms(r.AnalysedTo)})
	if err != nil {
		return nil, err
	}
	return append(append(m[:len(m)-1], ','), extra[1:]...), nil // both are objects
}

// UnmarshalJSON reads what MarshalJSON writes.
func (r *Report) UnmarshalJSON(b []byte) error {
	var v reportJSON
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if err := json.Unmarshal(b, &r.FileMap); err != nil {
		return err
	}
	r.Container, r.AnalysedTo = v.Container, mediatime.Dur(v.AnalysedToMs)
	return nil
}

func (p *Proxy) report(r *http.Request) (Report, bool, error) {
	if s := p.lookup(r); s != nil {
		return s.report(), true, nil
	}
	// no live session: the stored map still answers for a file seen before
	q := r.URL.Query()
	_, key, _ := fileIdent(q)
	m, err := p.Maps.Lookup(r.Context(), q.Get("key"), key)
	if err != nil {
		return Report{}, false, err
	}
	if m == nil { // never analysed here: other nodes may know it, or an ad was marked in it
		remote, err := p.Maps.PeerAds(r.Context(), p.Lib.Tracked, q.Get("key"), key)
		if err != nil {
			return Report{}, false, err
		}
		ads := withRemote(p.sourceAds(q.Get("key"), key), remote)
		if len(ads) == 0 {
			return Report{}, false, nil
		}
		return Report{Key: cmp.Or(key, q.Get("key")), Analyzed: detect.Intervals{}, Ads: ads}, true, nil
	}
	fm := *m
	fm.Ads = slices.Clone(m.Ads)
	rep := Report{FileMap: fm}
	for i := range rep.Ads {
		rep.Ads[i].Confirmed = rep.Ads[i].Confirmed || m.Analyzed.Contains(rep.Ads[i].Start, rep.Ads[i].End)
	}
	keys := append([]string{m.Key}, m.Aliases...)
	remote, err := p.Maps.PeerAds(r.Context(), p.Lib.Tracked, keys...)
	if err != nil {
		return Report{}, false, err
	}
	rep.Ads = withRemote(withRemote(rep.Ads, p.sourceAds(keys...)), remote)
	if rep.Analyzed == nil {
		rep.Analyzed = detect.Intervals{}
	}
	return rep, true, nil
}

// sourceAds are the ads enrolled from a file known by any of keys, as confirmed detections
// where they were marked.
func (p *Proxy) sourceAds(keys ...string) []detect.Detection {
	var out []detect.Detection
	for _, ad := range p.Lib.FromSource(keys...) {
		out = append(out, detect.Detection{AdID: ad.ID, Label: ad.Label, Type: ad.Type, Start: mediatime.Dur(ad.Source.StartMs),
			End: mediatime.Dur(ad.Source.EndMs), Score: ad.Hashes, Confirmed: true})
	}
	return out
}

// confirm applies the skip rule to every candidate, once the evidence has changed (a block,
// a stored map): an ad is safe to skip once the whole of it has been analysed, or two
// consecutive blocks agreed on its alignment with a high score (the player may buffer only
// 10-60 s, so waiting for full coverage is often not an option). Readers (report, store)
// only read Sticky. Must be called with s.mu held.
func (s *session) confirm() {
	cs := s.p.conf().ConfirmScore
	for i := range s.ads {
		s.ads[i].Confirm(s.covered, cs)
	}
}

func (s *session) report() Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := Report{Key: s.key, Aliases: append([]string(nil), s.alias...), Size: s.size,
		Duration: s.duration, Analyzed: append(detect.Intervals{}, s.covered...), Container: s.ctype, AnalysedTo: s.analysedTo}
	for i := range s.ads {
		d := s.ads[i].Detection
		d.Confirmed = s.ads[i].Sticky
		rep.Ads = append(rep.Ads, d)
	}
	rep.Ads = detect.DropShadowed(withRemote(rep.Ads, s.remote))
	slices.SortStableFunc(rep.Ads, func(a, b detect.Detection) int { return cmp.Compare(a.Start, b.Start) })
	return rep
}

// withRemote adds to own the ads other nodes found where this node's own analysis has
// no detection of the same ad: this node's own findings come first.
func withRemote(own, remote []detect.Detection) []detect.Detection {
	for _, r := range remote {
		overlaps := false
		for _, d := range own {
			if d.AdID == r.AdID && d.Start < r.End && r.Start < d.End {
				overlaps = true
				break
			}
		}
		if !overlaps {
			own = append(own, r)
		}
	}
	return own
}

// Ad lengths /ads/mark accepts.
const (
	minMark = 3 * time.Second
	maxMark = 3 * time.Minute
)

// Mark enrolls [start,end) of the session's file as a reference ad, using the audio the
// proxy has already decoded - the source is not read again.
func (p *Proxy) Mark(ctx context.Context, s *session, start, end time.Duration, label, typ string) (*library.Ad, error) {
	if l := end - start; l < minMark || l > maxMark {
		return nil, fmt.Errorf("ad length %dms out of range (%d..%dms)", l.Milliseconds(), minMark.Milliseconds(), maxMark.Milliseconds())
	}
	pcm, err := s.ring.Slice(start, end)
	if err != nil {
		from, to := s.ring.Span()
		return nil, fmt.Errorf("%w (decoded audio held: %d..%dms)", err, mediatime.Ms(from), mediatime.Ms(to))
	}
	// Remember where the ad came from (so that it can be enrolled again with new
	// fingerprint parameters), when the file has a key that may be stored.
	s.mu.Lock()
	var src *catalog.Source
	for _, k := range append([]string{s.key}, s.alias...) {
		if filekey.Stored(k) {
			src = &catalog.Source{Key: k, StartMs: mediatime.Ms(start), EndMs: mediatime.Ms(end)}
			break
		}
	}
	s.mu.Unlock()
	ad, err := p.Lib.Add(ctx, label, typ, pcm, src) // an empty label becomes <type>-<short id>
	if err != nil {
		return nil, err
	}
	slog.Info("enrolled ad", "ad", ad.ID, "type", ad.Type, "label", ad.Label, "duration", ad.Duration, "hashes", ad.Hashes, "session", s.id)
	p.rescan(ctx)
	s.marked(ad, start, end, pcm)
	return ad, nil
}

// marked files the ad just enrolled from [start,end) of s as a confirmed detection there:
// it is where the ad is, and the player may never send those bytes again (it seeks back
// within its own cache), so waiting for the analysis to find it could mean never.
func (s *session) marked(ad *library.Ad, start, end time.Duration, pcm []float32) {
	score := 0
	for _, m := range s.p.Lib.Match(fingerprint.Compute(pcm), 1) {
		if m.AdID == ad.ID {
			score = max(score, m.Score)
		}
	}
	d := detect.Detection{AdID: ad.ID, Label: ad.Label, Type: ad.Type, Start: start, End: end, Score: score}
	s.mu.Lock()
	var i int
	if s.ads, i, _ = detect.MergeCandidate(s.ads, d, detect.NoBlock, detect.NoBlock); i >= 0 {
		s.ads[i].Sticky = true
	}
	s.dirty = true
	s.mu.Unlock()
	s.store()
}

// rescan forgets what has been analysed, everywhere, so the new ad is looked for again.
func (p *Proxy) rescan(ctx context.Context) {
	p.mu.Lock()
	all := make([]*session, 0, len(p.sess))
	for _, s := range p.sess {
		all = append(all, s)
	}
	p.mu.Unlock()
	for _, s := range all {
		s.mu.Lock()
		s.covered, s.dirty = nil, true
		s.mu.Unlock()
	}
	if err := p.Maps.ResetAnalyzed(ctx); err != nil {
		slog.Warn("stored ad maps not reset; files seen before are not searched for the new ad", "err", err)
	}
}
