package admin

import (
	"cmp"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// filesPerPage is how many files a page of the list shows.
const filesPerPage = 100

type fileView struct {
	Key         string    `json:"key"`
	Own         bool      `json:"own"`
	Updated     time.Time `json:"updated"`
	DurationMs  int32     `json:"duration_ms"`
	AnalyzedMs  int32     `json:"analyzed_ms"`
	CoveragePct int       `json:"coverage_pct"`
	Ads         int       `json:"ads"`
	Origins     int       `json:"origins"`
	PeerAds     int       `json:"peer_ads"`
}

type filesPage struct {
	Q     string     `json:"-"`
	Rows  []fileView `json:"files"`
	Total int        `json:"total"`
	Page  int        `json:"page"`
	Pages int        `json:"pages"`
	Prev  string     `json:"-"` // links to the neighbouring pages ("" if none)
	Next  string     `json:"-"`
}

func coverage(analyzed, duration int32) int {
	if duration <= 0 {
		return 0
	}
	return int(min(100, int64(analyzed)*100/int64(duration)))
}

func (a *Admin) files(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "files")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	n = max(n, 1)
	rows, total, err := a.d.DB.Files(r.Context(), q, (n-1)*filesPerPage, filesPerPage)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	d := filesPage{Q: q, Total: total, Page: n, Pages: max(1, (total+filesPerPage-1)/filesPerPage)}
	link := func(n int) string {
		v := url.Values{"page": {strconv.Itoa(n)}}
		if q != "" {
			v.Set("q", q)
		}
		return "/files?" + v.Encode()
	}
	if n > 1 {
		d.Prev = link(min(n, d.Pages+1) - 1)
	}
	if n < d.Pages {
		d.Next = link(n + 1)
	}
	for _, f := range rows {
		d.Rows = append(d.Rows, fileView{Key: f.Key, Own: f.Own, Updated: f.Updated, DurationMs: f.DurationMs,
			AnalyzedMs: f.AnalyzedMs, CoveragePct: coverage(f.AnalyzedMs, f.DurationMs), Ads: f.Ads, Origins: f.Origins, PeerAds: f.PeerAds})
	}
	p.Data = d
	a.show(w, r, p, "files-table", http.StatusOK)
}

// fileDetail is one file: every origin's detections side by side, grouped by ad and start
// (within 250 ms, as the proxy merges them), and what the proxy reports for it.
type fileDetail struct {
	Key         string          `json:"key"`
	Aliases     []string        `json:"aliases"`
	Own         bool            `json:"own"`
	DurationMs  int32           `json:"duration_ms"`
	Analyzed    [][2]int32      `json:"analyzed"`
	CoveragePct int             `json:"coverage_pct"`
	Updated     time.Time       `json:"updated,omitzero"`
	Origins     []record.Origin `json:"origins"` // the columns of Rows; this node first
	Rows        []clusterView   `json:"rows"`
	Reported    []reportedView  `json:"reported"`
}

type clusterView struct {
	Ad      fingerprint.ID `json:"ad"`
	Label   string         `json:"label"`
	StartMs int32          `json:"start_ms"`
	EndMs   int32          `json:"end_ms"`
	Cells   []cellView     `json:"cells"` // one per origin, in fileDetail.Origins order
	// Disagree: some origin does not report it, or they differ on confirming it.
	Disagree bool `json:"disagree"`
	// Contradicted: this node analysed the whole range and did not find it.
	Contradicted bool `json:"contradicted"`
}

type cellView struct {
	Found     bool `json:"found"`
	Score     int  `json:"score,omitempty"`
	Confirmed bool `json:"confirmed,omitempty"`
}

// reportedView is a detection /ads/file reports: this node's own, or a peer's that
// passed the trust policy.
type reportedView struct {
	Ad        fingerprint.ID `json:"ad"`
	Label     string         `json:"label"`
	StartMs   int32          `json:"start_ms"`
	EndMs     int32          `json:"end_ms"`
	Score     int            `json:"score"`
	Confirmed bool           `json:"confirmed"`
	Own       bool           `json:"own"`
}

func covered(iv [][2]int32, from, to int32) bool {
	for _, x := range iv {
		if x[0] <= from && to <= x[1] {
			return true
		}
	}
	return false
}

func (a *Admin) file(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "file")
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		a.fail(w, r, http.StatusBadRequest, errors.New("key is required"))
		return
	}
	ctx := r.Context()
	own, err := a.d.DB.FileMap(ctx, key)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	d := fileDetail{Key: key, Origins: []record.Origin{p.self}}
	keys := []string{key}
	var dets []catalog.PeerDetection
	if own != nil {
		d.Key, d.Aliases, d.Own, d.DurationMs, d.Analyzed = own.Key, own.Aliases, true, own.DurationMs, own.Analyzed
		d.Updated = own.Updated
		keys = append([]string{own.Key}, own.Aliases...)
		for _, x := range own.Detections {
			dets = append(dets, catalog.PeerDetection{Origin: p.self, Detection: x})
			d.Reported = append(d.Reported, reportedView{x.Ad, x.Label, x.StartMs, x.EndMs, x.Score, x.Confirmed, true})
		}
	}
	peer, err := a.d.DB.PeerDetections(ctx, keys...)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	if own == nil && len(peer) == 0 {
		a.fail(w, r, http.StatusNotFound, errors.New(p.T("file.unknown")))
		return
	}
	dets = append(dets, peer...)
	var analyzed int32
	for _, x := range d.Analyzed {
		analyzed += x[1] - x[0]
	}
	d.CoveragePct = coverage(analyzed, d.DurationMs)
	col := map[record.Origin]int{p.self: 0}
	var ids []fingerprint.ID
	for _, x := range dets {
		if _, ok := col[x.Origin]; !ok {
			col[x.Origin] = len(d.Origins)
			d.Origins = append(d.Origins, x.Origin)
		}
		ids = append(ids, x.Ad)
	}
	labels, err := a.d.DB.Labels(ctx, ids)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	slices.SortFunc(dets, func(x, y catalog.PeerDetection) int {
		return cmp.Or(strings.Compare(x.Ad.String(), y.Ad.String()), cmp.Compare(x.StartMs, y.StartMs))
	})
	for i := 0; i < len(dets); {
		c := clusterView{Ad: dets[i].Ad, Label: labels[dets[i].Ad], StartMs: dets[i].StartMs, EndMs: dets[i].EndMs,
			Cells: make([]cellView, len(d.Origins))}
		j := i
		for ; j < len(dets) && dets[j].Ad == dets[i].Ad && dets[j].StartMs-dets[i].StartMs <= 250; j++ {
			cell := &c.Cells[col[dets[j].Origin]]
			if !cell.Found || dets[j].Score > cell.Score {
				*cell = cellView{Found: true, Score: dets[j].Score, Confirmed: dets[j].Confirmed}
			}
			c.EndMs = max(c.EndMs, dets[j].EndMs)
		}
		for _, cell := range c.Cells {
			c.Disagree = c.Disagree || !cell.Found || cell.Confirmed != c.Cells[0].Confirmed
		}
		c.Contradicted = !c.Cells[0].Found && covered(d.Analyzed, c.StartMs, c.EndMs)
		d.Rows = append(d.Rows, c)
		i = j
	}
	slices.SortFunc(d.Rows, func(x, y clusterView) int { return cmp.Compare(x.StartMs, y.StartMs) })
	merged, err := a.d.DB.PeerAdsIn(ctx, a.d.Lib.Tracked, keys...)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	for _, x := range merged {
		if !slices.ContainsFunc(d.Reported, func(o reportedView) bool {
			return o.Ad == x.Ad && max(o.StartMs-x.StartMs, x.StartMs-o.StartMs) <= 250
		}) {
			d.Reported = append(d.Reported, reportedView{x.Ad, x.Label, x.StartMs, x.EndMs, x.Score, x.Confirmed, false})
		}
	}
	slices.SortFunc(d.Reported, func(x, y reportedView) int { return cmp.Compare(x.StartMs, y.StartMs) })
	p.Data = d
	a.show(w, r, p, "", http.StatusOK)
}
