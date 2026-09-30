package admin

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/srgsf/adsvc/internal/record"
)

type peerView struct {
	Name         string         `json:"name"`
	NodeID       *record.Origin `json:"node_id,omitempty"`
	Configured   bool           `json:"configured"`
	Synced       bool           `json:"synced"` // it has a row: it was synced with at least once
	PullCursor   int64          `json:"pull_cursor"`
	PushCursor   int64          `json:"push_cursor"`
	LastOK       time.Time      `json:"last_ok,omitzero"`
	LastError    string         `json:"last_error,omitempty"`
	Received     int            `json:"received"`
	Rejected     map[string]int `json:"rejected"`
	Relayed      int            `json:"relayed_ads"`
	RelayedTrash int            `json:"relayed_trash"`
}

type pusherView struct {
	NodeID       record.Origin  `json:"node_id"`
	LastPush     time.Time      `json:"last_push"`
	Received     int            `json:"received"`
	Duplicate    int            `json:"duplicate"`
	Rejected     map[string]int `json:"rejected"`
	Relayed      int            `json:"relayed_ads"`
	RelayedTrash int            `json:"relayed_trash"`
}

type peersPage struct {
	Peers   []peerView   `json:"peers"`
	Pushers []pusherView `json:"pushers"`
}

func (a *Admin) peers(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "peers")
	rows, pushers, err := a.d.DB.Peers(r.Context())
	if err != nil {
		a.internal(w, r, err)
		return
	}
	var configured []string
	if a.d.Peers != nil {
		configured = a.d.Peers()
	}
	var d peersPage
	for _, x := range rows {
		v := peerView{Name: x.Name, Configured: slices.Contains(configured, x.Name), Synced: true, PullCursor: x.PullCursor,
			PushCursor: x.PushCursor, LastError: x.LastError, Received: x.Received, Rejected: x.Rejected,
			Relayed: x.Relayed, RelayedTrash: x.RelayedTrash}
		if x.NodeID != (record.Origin{}) {
			n := x.NodeID
			v.NodeID = &n
		}
		v.LastOK = x.LastOK
		d.Peers = append(d.Peers, v)
	}
	for _, name := range configured {
		if !slices.ContainsFunc(d.Peers, func(v peerView) bool { return v.Name == name }) {
			d.Peers = append(d.Peers, peerView{Name: name, Configured: true, Rejected: map[string]int{}})
		}
	}
	for _, x := range pushers {
		d.Pushers = append(d.Pushers, pusherView{x.NodeID, x.LastPush, x.Received, x.Duplicate, x.Rejected, x.Relayed, x.RelayedTrash})
	}
	p.Data = d
	a.show(w, r, p, "", http.StatusOK)
}

type trackingPage struct {
	Ads             int       `json:"ads"`
	IndexAds        int       `json:"index_ads"`
	Postings        int       `json:"postings"`
	Distinct        int       `json:"distinct_hashes"`
	DeadAds         int       `json:"dead_ads"`
	DeadPostings    int       `json:"dead_postings"`
	OverlayAds      int       `json:"overlay_ads"`
	OverlayPostings int       `json:"overlay_postings"`
	File            string    `json:"file,omitempty"`
	MappedBytes     int64     `json:"mapped_bytes"`
	HeapBytes       int64     `json:"heap_bytes"`
	Built           time.Time `json:"built,omitzero"`
	BuildTookMs     int64     `json:"build_took_ms"`
	MinTrust        float64   `json:"min_trust"`
	MaxAds          int       `json:"max_ads"`
}

func (a *Admin) loadTracking(p *page) {
	s := a.d.Lib.Stats()
	pol := a.d.DB.Policy()
	d := trackingPage{Ads: s.Ads, IndexAds: s.IndexAds, Postings: s.Postings, Distinct: s.Distinct, DeadAds: s.DeadAds,
		DeadPostings: s.DeadPostings, OverlayAds: s.OverlayAds, OverlayPostings: s.OverlayPostings, File: s.File,
		MappedBytes: s.MappedBytes, HeapBytes: s.HeapBytes, BuildTookMs: s.BuildTook.Milliseconds(),
		MinTrust: pol.MinTrust, MaxAds: pol.MaxAds}
	d.Built = s.Built
	p.Data = d
}

func (a *Admin) tracking(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "tracking")
	a.loadTracking(p)
	a.show(w, r, p, "tracking-stats", http.StatusOK)
}

// rebuild reloads the tracking set and rebuilds the index from it.
func (a *Admin) rebuild(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "tracking")
	start := time.Now()
	if err := a.d.Lib.Rebuild(a.ctx); err != nil {
		a.internal(w, r, err)
		return
	}
	p.Msg = p.Tf("tracking.rebuilt", time.Since(start).Round(time.Millisecond))
	a.loadTracking(p)
	a.show(w, r, p, "tracking-stats", http.StatusOK)
}

type configPage struct {
	YAML string `json:"yaml"`
}

func (a *Admin) loadConfig(p *page) error {
	if a.d.Config == nil {
		return errors.New("no configuration to show")
	}
	b, err := a.d.Config()
	if err != nil {
		return err
	}
	p.Data = configPage{YAML: string(b)}
	return nil
}

func (a *Admin) config(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "config")
	if err := a.loadConfig(p); err != nil {
		a.internal(w, r, err)
		return
	}
	a.show(w, r, p, "config-body", http.StatusOK)
}

// reload re-reads the config file, as SIGHUP does.
func (a *Admin) reload(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "config")
	if a.d.Reload == nil {
		a.fail(w, r, http.StatusConflict, errors.New(p.T("config.no_file")))
		return
	}
	restart, err := a.d.Reload()
	status := http.StatusOK
	switch {
	case err != nil:
		p.Err, status = p.T("config.failed")+": "+err.Error(), http.StatusUnprocessableEntity
	case len(restart) > 0:
		p.Msg = p.T("config.restart") + ": " + strings.Join(restart, ", ")
	default:
		p.Msg = p.T("config.reloaded")
	}
	if err := a.loadConfig(p); err != nil {
		a.internal(w, r, err)
		return
	}
	a.show(w, r, p, "config-body", status)
}
