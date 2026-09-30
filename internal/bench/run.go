package bench

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/ffmpeg"
	"github.com/srgsf/adsvc/internal/filekey"
	"github.com/srgsf/adsvc/internal/library"
	"github.com/srgsf/adsvc/internal/mediatime"
	"github.com/srgsf/adsvc/internal/proxy"
	"github.com/srgsf/adsvc/internal/store"
)

// Options configure a benchmark run.
type Options struct {
	// FFmpeg decodes: offline it reads the whole file (and encodes, to degrade), so it has
	// to be a full build; the proxy only needs the minimal one.
	FFmpeg       ffmpeg.Tools
	MinScore     int     // match threshold (default detect.DefaultMinScore)
	ConfirmScore int     // score that confirms from two blocks (default detect.DefaultConfirmScore)
	Floor        int     // lowest candidate score kept for the noise measure (default 5)
	Degrade      Degrade // offline only
}

func (o *Options) defaults() {
	o.MinScore = cmp.Or(o.MinScore, detect.DefaultMinScore)
	o.ConfirmScore = cmp.Or(o.ConfirmScore, detect.DefaultConfirmScore(o.MinScore))
	if o.Floor == 0 {
		o.Floor = 5
	}
	if o.Degrade.Name == "" {
		o.Degrade.Name = "none"
	}
}

// Hit is one labelled occurrence and what the detector made of it. In JSON its times are
// integer milliseconds.
type Hit struct {
	Occurrence
	Found    bool
	Det      detect.Detection // the detection, when found
	StartErr time.Duration    // Det.Start - Start
	// Audio decoded past the labelled start when the detection first appeared with its final
	// alignment, and when it was confirmed: the look-ahead a player needs to skip the ad from
	// its first frame. Measured offline only (playback from the start of the file).
	DetectAt  *time.Duration
	ConfirmAt *time.Duration
}

// MarshalJSON writes the occurrence's fields and the result, times in milliseconds.
func (h Hit) MarshalJSON() ([]byte, error) {
	msp := func(d *time.Duration) *int32 {
		if d == nil {
			return nil
		}
		v := mediatime.Ms(*d)
		return &v
	}
	return json.Marshal(struct {
		occurrenceJSON
		Found       bool             `json:"found"`
		Det         detect.Detection `json:"det"`
		StartErrMs  int32            `json:"startErrMs"`
		DetectAtMs  *int32           `json:"detectAtMs,omitempty"`
		ConfirmAtMs *int32           `json:"confirmAtMs,omitempty"`
	}{h.toJSON(), h.Found, h.Det, mediatime.Ms(h.StartErr), msp(h.DetectAt), msp(h.ConfirmAt)})
}

// Run is the result for one file. In JSON its times are integer milliseconds.
type Run struct {
	Path     string             `json:"path"`
	Mode     string             `json:"mode"`
	Degrade  string             `json:"degrade,omitempty"`
	Duration time.Duration      `json:"-"` // media duration
	Analysed time.Duration      `json:"-"` // media time analysed
	Elapsed  time.Duration      `json:"-"` // wall time
	Hits     []Hit              `json:"hits"`
	FalsePos []detect.Detection `json:"falsePos"`
	// Noise is the strongest candidate, above the threshold or not, that is not a labelled
	// occurrence: the margin to the weakest true match is what the threshold has to fit in.
	// A high one is either a false positive in waiting or an occurrence missing from the
	// labels. Offline only.
	Noise *detect.Detection `json:"noise,omitempty"`
	Err   string            `json:"err,omitempty"`
}

// MarshalJSON writes the times as integer milliseconds.
func (r Run) MarshalJSON() ([]byte, error) {
	type plain Run
	return json.Marshal(struct {
		plain
		DurationMs int32 `json:"durationMs"`
		AnalysedMs int32 `json:"analysedMs"`
		ElapsedMs  int32 `json:"elapsedMs"`
	}{plain(r), mediatime.Ms(r.Duration), mediatime.Ms(r.Analysed), mediatime.Ms(r.Elapsed)})
}

// Speed is analysed media time per second of wall time.
func (r Run) Speed() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return r.Analysed.Seconds() / r.Elapsed.Seconds()
}

// never marks a finalDet time that is unknown, or never came.
const never = time.Duration(math.MinInt64)

// finalDet is a final detection with the decoded media time at which it appeared and was
// confirmed (never: unknown or never).
type finalDet struct {
	detect.Detection
	detectAt, confirmAt time.Duration
}

// sameOccurrence reports whether d is the labelled occurrence l: the same ad, covering at
// least half of the shorter of the two intervals.
func sameOccurrence(l Occurrence, d detect.Detection) bool {
	if l.Ad != d.AdID {
		return false
	}
	ov := min(l.End, d.End) - max(l.Start, d.Start)
	return 2*ov >= min(l.End-l.Start, d.End-d.Start)
}

// evaluate pairs labels with detections (the best-scoring one per label). Detections left
// over are false positives: an ad where none is labelled, the wrong ad, or a misalignment.
func evaluate(labels []Occurrence, dets []finalDet) ([]Hit, []detect.Detection) {
	used := make([]bool, len(dets))
	hits := make([]Hit, len(labels))
	for i, l := range labels {
		h := Hit{Occurrence: l}
		best := -1
		for j, d := range dets {
			if !used[j] && sameOccurrence(l, d.Detection) && (best < 0 || d.Score > dets[best].Score) {
				best = j
			}
		}
		if best >= 0 {
			used[best] = true
			d := dets[best]
			h.Found, h.Det, h.StartErr = true, d.Detection, d.Start-l.Start
			h.DetectAt = sinceStart(d.detectAt, l.Start)
			h.ConfirmAt = sinceStart(d.confirmAt, l.Start)
		}
		hits[i] = h
	}
	fps := []detect.Detection{}
	for j, d := range dets {
		if !used[j] {
			fps = append(fps, d.Detection)
		}
	}
	return hits, fps
}

func sinceStart(t, start time.Duration) *time.Duration {
	if t == never {
		return nil
	}
	v := t - start
	return &v
}

// RunOffline decodes the whole file with ffmpeg (degraded as o says) and scans it from the
// start, the way playback from the beginning is analysed, with the proxy's skip rule.
func RunOffline(ctx context.Context, lib *library.Library, path string, labels []Occurrence, o Options) Run {
	o.defaults()
	run := Run{Path: path, Mode: "offline", Degrade: o.Degrade.Name}
	labels = o.Degrade.scale(labels)
	pcm, err := decodeDegraded(ctx, o.FFmpeg, path, o.Degrade)
	if err != nil {
		run.Err = err.Error()
		return run
	}
	var (
		ads     []detect.Candidate
		covered detect.Intervals
		noise   *detect.Detection
	)
	start := time.Now()
	// The proxy's scan, with a floor: the weak matches measure the noise.
	reached, err := detect.Scan(ctx, pcm, 0, lib, o.MinScore, o.Floor, func(b detect.BlockResult) {
		for _, c := range b.Weak {
			if !labelled(labels, c) && (noise == nil || c.Score > noise.Score) {
				noise = &c
			}
		}
		covered = covered.Add(b.From, b.To)
		for _, d := range b.Detections {
			var i int
			var changed bool
			if ads, i, changed = detect.MergeCandidate(ads, d, b.From, b.To); changed {
				ads[i].FirstSeen, ads[i].ConfirmedAt = b.Reached, never
			}
		}
		for i := range ads {
			if ads[i].ConfirmedAt == never && ads[i].Confirm(covered, o.ConfirmScore) {
				ads[i].ConfirmedAt = b.Reached
			}
		}
	})
	if werr := pcm.Wait(); werr != nil && err == nil {
		err = werr
	}
	run.Elapsed = time.Since(start)
	run.Duration, run.Analysed = reached, reached
	if err != nil {
		run.Err = err.Error()
		return run
	}
	dets := make([]finalDet, len(ads))
	for i := range ads {
		d := ads[i].Detection
		d.Confirmed = ads[i].Sticky
		dets[i] = finalDet{Detection: d, detectAt: ads[i].FirstSeen, confirmAt: ads[i].ConfirmedAt}
	}
	run.Hits, run.FalsePos = evaluate(labels, dets)
	run.Noise = noise
	return run
}

func labelled(labels []Occurrence, d detect.Detection) bool {
	for _, l := range labels {
		if sameOccurrence(l, d) {
			return true
		}
	}
	return false
}

// RunProxy streams the file through a Proxy the way a player reading it from start to end
// would - one GET, as fast as the analyser allows (MaxStall is set high, so nothing is
// skipped) - and scores the ad map the proxy stored. st is a store for benchmark runs
// (store.Copy, shared by every run of one benchmark): each run files its map under its own
// id, so that no run finds another's map and skips the analysis.
func RunProxy(ctx context.Context, st *store.Store, id, path string, labels []Occurrence, o Options) Run {
	o.defaults()
	run := Run{Path: path, Mode: "proxy"}
	fail := func(err error) Run {
		run.Err = err.Error()
		return run
	}
	src, err := serveLocal(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path)
	}))
	if err != nil {
		return fail(err)
	}
	defer src.Close()
	px := proxy.NewWith(ctx, proxy.Config{FFmpeg: o.FFmpeg, MinScore: o.MinScore,
		ConfirmScore: o.ConfirmScore, MaxStall: time.Hour}, st)
	front, err := serveLocal(ctx, px.Handler())
	if err != nil {
		px.Close()
		return fail(err)
	}
	defer front.Close()

	u := front.URL + "/s?id=" + id + "&u=" + url.QueryEscape(src.URL+"/"+url.PathEscape(filepath.Base(path)))
	start := time.Now()
	err = func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("proxy: %s", resp.Status)
		}
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}()
	// The player has all the bytes, but the handler may still be handing the last chunk to
	// the analyser: wait for it, or Close would drop that tail.
	front.Drain(ctx)
	px.Close() // drains the analysis queue and the decoder, then stores the ad map
	run.Elapsed = time.Since(start)
	if err != nil {
		return fail(err)
	}
	m, err := st.Maps.Lookup(ctx, filekey.Opaque(id))
	if err != nil {
		return fail(err)
	}
	if m == nil {
		return fail(errors.New("no ad map stored: container not recognised or analysis off"))
	}
	run.Duration = m.Duration
	for _, r := range m.Analyzed {
		run.Analysed += r[1] - r[0]
	}
	dets := make([]finalDet, len(m.Ads))
	for i, d := range m.Ads {
		dets[i] = finalDet{Detection: d, detectAt: never, confirmAt: never}
	}
	run.Hits, run.FalsePos = evaluate(labels, dets)
	return run
}

// localServer is an HTTP server on a free loopback port.
type localServer struct {
	URL string
	srv *http.Server
}

func serveLocal(ctx context.Context, h http.Handler) (*localServer, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go s.Serve(ln) //nolint:errcheck // ends with ErrServerClosed
	return &localServer{URL: "http://" + ln.Addr().String(), srv: s}, nil
}

func (s *localServer) Close() { s.srv.Close() }

// Drain waits for the requests in flight to return.
func (s *localServer) Drain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
}
