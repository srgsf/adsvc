package admin

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/srgsf/adsvc/internal/adtype"
	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/library"
	"github.com/srgsf/adsvc/internal/mediatime"
	"github.com/srgsf/adsvc/internal/record"
)

// adsPerPage is how many ads a page of the list shows.
const adsPerPage = 100

// adView is an ad in the list and on its page.
type adView struct {
	ID            fingerprint.ID  `json:"id"`
	Label         string          `json:"label"`
	Type          string          `json:"type"` // ad or intro
	Author        record.Origin   `json:"author"`
	FPVersion     int             `json:"fp_version"`
	DurationMs    int32           `json:"duration_ms"`
	Points        int             `json:"points"`
	Created       time.Time       `json:"created"`
	Trust         float64         `json:"trust"`
	State         string          `json:"state"` // active, retracted, blocked or dup
	Canonical     *fingerprint.ID `json:"canonical,omitempty"`
	CanonLabel    string          `json:"canonical_label,omitempty"`
	Tracked       bool            `json:"tracked"`
	Pinned        bool            `json:"pinned"`
	Up            int             `json:"votes_up"`
	Down          int             `json:"votes_down"`
	Files         int             `json:"files"`
	Confirmations int             `json:"confirmations"`
	LastSeen      time.Time       `json:"last_seen,omitzero"`
	Source        *sourceView     `json:"source,omitempty"`
	PossibleDup   *dupOfView      `json:"possible_duplicate,omitempty"` // another ad matches this one
	DupedBy       []dupOfView     `json:"duplicated_by,omitempty"`      // ads this node calls duplicates of this one
}

type sourceView struct {
	Key     string `json:"key"`
	StartMs int32  `json:"start_ms"`
	EndMs   int32  `json:"end_ms"`
}

// viewAd is r as the page shows it. Tracked is whether the index holds it now.
func (a *Admin) viewAd(r catalog.AdRow) adView {
	v := adView{ID: r.ID, Label: r.Label, Type: adtype.Of(r.Type), Author: r.Author, FPVersion: r.FPVersion, DurationMs: r.DurationMs,
		Points: r.NPoints, Created: r.Created, Trust: r.Trust, State: cmp.Or(r.State, "active"), Tracked: a.d.Lib.Get(r.ID) != nil,
		Pinned: r.Pinned, Up: r.Up, Down: r.Down, Files: r.Files, Confirmations: r.Confirmations, LastSeen: r.LastSeen}
	if !r.Canonical.IsZero() {
		c := r.Canonical
		v.Canonical, v.CanonLabel = &c, r.CanonicalLabel
	}
	for _, d := range r.DupedBy {
		v.DupedBy = append(v.DupedBy, dupOfView{ID: d.ID, Label: d.Label})
	}
	if r.Source != nil {
		v.Source = &sourceView{Key: r.Source.Key, StartMs: r.Source.StartMs, EndMs: r.Source.EndMs}
	}
	return v
}

// adsFilter is the list's query: the form's values as given, and parsed. It filters and
// sorts on stored columns only (catalog.AdQuery).
type adsFilter struct {
	Q, Origin, Pinned, FP, Sort, Type string
	Page                              int

	fp int
}

func parseAdsFilter(v url.Values) (adsFilter, error) {
	f := adsFilter{Q: strings.TrimSpace(v.Get("q")), Origin: strings.TrimSpace(v.Get("origin")), Pinned: v.Get("pinned"),
		FP: strings.TrimSpace(v.Get("fp")), Sort: cmp.Or(v.Get("sort"), catalog.SortCreated), Type: v.Get("type")}
	f.Page, _ = strconv.Atoi(v.Get("page"))
	f.Page = max(f.Page, 1)
	switch f.Sort {
	case catalog.SortCreated, catalog.SortLabel:
	default:
		return f, fmt.Errorf("sort %q: want %s or %s", f.Sort, catalog.SortCreated, catalog.SortLabel)
	}
	if f.Type != "" && !adtype.Valid(f.Type) {
		return f, fmt.Errorf("type %q: want %s or %s", f.Type, adtype.Ad, adtype.Intro)
	}
	switch f.Pinned {
	case "", "yes", "no":
	default:
		return f, fmt.Errorf("pinned %q: want yes or no", f.Pinned)
	}
	if f.FP != "" {
		var err error
		if f.fp, err = strconv.Atoi(f.FP); err != nil {
			return f, fmt.Errorf("fp_version %q: not a number", f.FP)
		}
	}
	return f, nil
}

// query is the filter as URL parameters (for page links), without the page.
func (f adsFilter) query() url.Values {
	v := url.Values{}
	for k, s := range map[string]string{"q": f.Q, "origin": f.Origin, "pinned": f.Pinned, "fp": f.FP, "sort": f.Sort, "type": f.Type} {
		if s != "" {
			v.Set(k, s)
		}
	}
	return v
}

// adQuery is the catalogue query of page n (from 1) of the list.
func (f adsFilter) adQuery(n int) catalog.AdQuery {
	q := catalog.AdQuery{Q: f.Q, Author: f.Origin, FPVersion: f.fp, Type: f.Type, Sort: f.Sort, Offset: (n - 1) * adsPerPage, Limit: adsPerPage}
	if f.Pinned != "" {
		yes := f.Pinned == "yes"
		q.Pinned = &yes
	}
	return q
}

type adsPage struct {
	Filter   adsFilter `json:"-"`
	Rows     []adView  `json:"ads"`
	Total    int       `json:"total"`
	Page     int       `json:"page"`
	Pages    int       `json:"pages"`
	Versions []int     `json:"-"`
	Prev     string    `json:"-"` // links to the neighbouring pages ("" if none)
	Next     string    `json:"-"`
	// DupsURL loads the possible duplicates of the rows after the table (adsDups): each
	// takes a Match per window of the ad, too slow to wait for.
	DupsURL string `json:"-"`
}

// loadAds fills p with the ads the query in v asks for.
func (a *Admin) loadAds(r *http.Request, p *page, v url.Values) error {
	f, err := parseAdsFilter(v)
	if err != nil {
		return err
	}
	rows, total, err := a.d.DB.AdPage(r.Context(), f.adQuery(f.Page))
	if err != nil {
		return err
	}
	d := adsPage{Filter: f, Total: total, Pages: max(1, (total+adsPerPage-1)/adsPerPage)}
	d.Page = min(f.Page, d.Pages)
	if d.Page != f.Page { // past the last page: show the last one
		if rows, _, err = a.d.DB.AdPage(r.Context(), f.adQuery(d.Page)); err != nil {
			return err
		}
	}
	dups := url.Values{}
	for _, row := range rows {
		v := a.viewAd(row)
		if !v.knownDup() {
			dups.Add("id", v.ID.String())
		}
		d.Rows = append(d.Rows, v)
	}
	if len(dups) > 0 {
		d.DupsURL = "/ads/dups?" + dups.Encode()
	}
	if d.Versions, err = a.d.DB.FPVersions(r.Context()); err != nil {
		return err
	}
	link := func(n int) string {
		q := f.query()
		q.Set("page", strconv.Itoa(n))
		return "/ads?" + q.Encode()
	}
	if d.Page > 1 {
		d.Prev = link(d.Page - 1)
	}
	if d.Page < d.Pages {
		d.Next = link(d.Page + 1)
	}
	p.Data = d
	return nil
}

func (a *Admin) ads(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "ads")
	if err := a.loadAds(r, p, r.URL.Query()); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	a.show(w, r, p, "ads-table", http.StatusOK)
}

// adsDups answers which of the ads id (a page of the list) another tracked ad matches
// above the detection threshold: the amber dots of the list, filled in by htmx.
func (a *Admin) adsDups(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) && !wantsJSON(r) { // a fragment of the list
		http.Redirect(w, r, "/ads", http.StatusSeeOther)
		return
	}
	p := a.newPage(r, "ads")
	ids := r.URL.Query()["id"]
	if len(ids) > adsPerPage {
		a.fail(w, r, http.StatusBadRequest, fmt.Errorf("%w: at most %d ads", errInput, adsPerPage))
		return
	}
	out := []adView{}
	for _, s := range ids {
		id, err := fingerprint.ParseID(s)
		if err != nil {
			a.fail(w, r, http.StatusBadRequest, fmt.Errorf("%w: id %q", errInput, s))
			return
		}
		v := adView{ID: id}
		if err := a.possibleDup(r.Context(), &v); err != nil {
			a.internal(w, r, err)
			return
		}
		if v.PossibleDup != nil {
			out = append(out, v)
		}
	}
	p.Data = out
	a.show(w, r, p, "ads-dups", http.StatusOK)
}

// adsBulk votes on, pins or unpins the ads ticked in the list (form values id), then
// shows the list again with the filter the form carries.
func (a *Admin) adsBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	p := a.newPage(r, "ads")
	var ids []fingerprint.ID
	for _, s := range r.PostForm["id"] {
		id, err := fingerprint.ParseID(s)
		if err != nil {
			a.fail(w, r, http.StatusBadRequest, err)
			return
		}
		ids = append(ids, id)
	}
	action := r.PostFormValue("action")
	var do func(fingerprint.ID) error
	switch action {
	case "up", "down":
		v, err := parseVote(r, action)
		if err != nil {
			a.fail(w, r, http.StatusBadRequest, err)
			return
		}
		do = func(id fingerprint.ID) error { v.Ad = id; return a.d.DB.Vote(a.ctx, v) }
	case "pin", "unpin":
		do = func(id fingerprint.ID) error { return a.d.DB.Pin(a.ctx, id, action == "pin") }
	case "delete":
		do = a.deleteAd
	default:
		a.fail(w, r, http.StatusBadRequest, fmt.Errorf("action %q: want up, down, pin, unpin or delete", action))
		return
	}
	var errs []error
	done := 0
	for _, id := range ids {
		if err := do(id); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id.Short(), err))
			continue
		}
		done++
	}
	if done > 0 {
		errs = append(errs, a.refresh())
	}
	p.Msg = p.Tf("ads.bulk_done", done, len(ids))
	status := http.StatusOK
	if err := errors.Join(errs...); err != nil {
		p.Err, status = err.Error(), http.StatusUnprocessableEntity
	}
	if err := a.loadAds(r, p, r.PostForm); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	a.show(w, r, p, "ads-table", status)
}

// parseVote reads a vote from the form: value (+1 or -1; action up or down stands for it
// when given) and reason (good for +1 by default, not_ad for -1).
func parseVote(r *http.Request, action string) (record.AdVote, error) {
	var value int
	switch cmp.Or(action, r.PostFormValue("value")) {
	case "up", "+1", "1":
		value = 1
	case "down", "-1":
		value = -1
	default:
		return record.AdVote{}, errors.New("value: want +1 or -1")
	}
	reason := r.PostFormValue("reason")
	if reason == "" {
		reason = record.ReasonGood
		if value < 0 {
			reason = record.ReasonNotAd
		}
	}
	switch reason {
	case record.ReasonGood, record.ReasonNotAd, record.ReasonBoundary, record.ReasonDup:
	default:
		return record.AdVote{}, fmt.Errorf("reason %q: want good, not_ad, boundary or dup", reason)
	}
	return record.AdVote{Value: value, Reason: reason}, nil
}

// adDetailView is an ad's page.
type adDetailView struct {
	adView
	Labels        []labelView    `json:"labels"`
	Votes         []voteView     `json:"votes"`
	Dups          []dupView      `json:"dups"`
	History       []historyView  `json:"history"`
	Sightings     []sightingView `json:"sightings"`
	ConfirmedHere bool           `json:"confirmed_here"`
	MyVote        *voteView      `json:"-"`
	Reasons       []string       `json:"-"`
}

type labelView struct {
	Origin record.Origin `json:"origin"`
	Label  string        `json:"label"`
	TS     time.Time     `json:"ts"`
}

type voteView struct {
	Origin  record.Origin `json:"origin"`
	Value   int           `json:"value"`
	Reason  string        `json:"reason"`
	Key     string        `json:"key,omitempty"`
	StartMs *int32        `json:"start_ms,omitempty"`
	TS      time.Time     `json:"ts"`
	Weight  float64       `json:"weight"`
}

type dupView struct {
	Origin    record.Origin  `json:"origin"`
	Ad        fingerprint.ID `json:"ad"`
	Canonical fingerprint.ID `json:"canonical"`
	TS        time.Time      `json:"ts"`
}

type historyView struct {
	ID       int64          `json:"id"`
	Kind     string         `json:"kind"`
	Origin   record.Origin  `json:"origin"`
	Via      *record.Origin `json:"via,omitempty"` // nil: written here
	TS       time.Time      `json:"ts"`
	Received time.Time      `json:"received"`
	Summary  string         `json:"summary"`
}

type sightingView struct {
	Key       string        `json:"key"`
	Origin    record.Origin `json:"origin"`
	StartMs   int32         `json:"start_ms"`
	EndMs     int32         `json:"end_ms"`
	Score     int           `json:"score"`
	Confirmed bool          `json:"confirmed"`
}

// summary describes what a record about an ad says.
func summary(e catalog.HistoryEntry, id fingerprint.ID) string {
	body, err := record.Decode(&e.Record)
	if err != nil {
		return err.Error()
	}
	switch b := body.(type) {
	case *record.AdAdd:
		s := fmt.Sprintf("%.1f s, fp v%d", float64(b.DurationMs)/1000, b.FPVersion)
		if b.Label != "" {
			s += ", " + strconv.Quote(b.Label)
		}
		return s
	case *record.AdLabel:
		return strconv.Quote(b.Label)
	case *record.AdVote:
		s := fmt.Sprintf("%+d %s", b.Value, b.Reason)
		if b.Key != "" {
			s += " @ " + b.Key
		}
		return s
	case *record.AdDup:
		if b.Ad == id {
			return "→ " + b.Canonical.String()
		}
		return "← " + b.Ad.String()
	}
	return ""
}

func (a *Admin) loadAd(r *http.Request, p *page, id fingerprint.ID) (bool, error) {
	d, err := a.d.DB.AdDetail(r.Context(), id)
	if err != nil || d == nil {
		return false, err
	}
	v := adDetailView{adView: a.viewAd(d.AdRow), Reasons: []string{record.ReasonGood, record.ReasonNotAd, record.ReasonBoundary, record.ReasonDup}}
	for _, l := range d.Labels {
		v.Labels = append(v.Labels, labelView{l.Origin, l.Label, time.UnixMilli(l.TS)})
	}
	for _, x := range d.Votes {
		vv := voteView{x.Origin, x.Value, x.Reason, x.Key, x.StartMs, time.UnixMilli(x.TS), x.Weight}
		v.Votes = append(v.Votes, vv)
		if x.Origin == p.self {
			v.MyVote = &vv
		}
	}
	for _, x := range d.Dups {
		v.Dups = append(v.Dups, dupView{x.Origin, x.Ad, x.Canonical, time.UnixMilli(x.TS)})
	}
	for _, h := range d.History {
		hv := historyView{ID: h.ID, Kind: h.Kind, Origin: h.Origin, TS: time.UnixMilli(h.TS), Received: h.Received, Summary: summary(h, id)}
		if h.Via != (record.Origin{}) {
			via := h.Via
			hv.Via = &via
		}
		v.History = append(v.History, hv)
	}
	for _, s := range d.Sightings {
		v.Sightings = append(v.Sightings, sightingView{s.Key, s.Origin, s.StartMs, s.EndMs, s.Score, s.Confirmed})
		v.ConfirmedHere = v.ConfirmedHere || (s.Origin == p.self && s.Confirmed)
	}
	if err := a.possibleDup(r.Context(), &v.adView); err != nil {
		return false, err
	}
	p.Data = v
	return true, nil
}

// adID is the ad of the request's {id}: its full id, or a prefix of one ad's id.
func (a *Admin) adID(r *http.Request, s string) (fingerprint.ID, error) {
	if id, err := fingerprint.ParseID(s); err == nil {
		return id, nil
	}
	ids, err := a.d.DB.FindAds(r.Context(), s, 2)
	switch {
	case err != nil:
		return fingerprint.ID{}, err
	case len(ids) == 0:
		return fingerprint.ID{}, catalog.ErrNoAd
	case len(ids) > 1:
		return fingerprint.ID{}, fmt.Errorf("%q: %w", s, library.ErrAmbiguous)
	}
	return ids[0], nil
}

// showAd shows the ad of the request's {id}; frag is what htmx gets.
func (a *Admin) showAd(w http.ResponseWriter, r *http.Request, p *page, frag string, status int) {
	id, err := a.adID(r, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, http.StatusNotFound, err)
		return
	}
	found, err := a.loadAd(r, p, id)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	if !found {
		a.fail(w, r, http.StatusNotFound, catalog.ErrNoAd)
		return
	}
	a.show(w, r, p, frag, status)
}

func (a *Admin) ad(w http.ResponseWriter, r *http.Request) {
	a.showAd(w, r, a.newPage(r, "ad"), "ad-main", http.StatusOK)
}

// qualityView is library.Quality with the detection threshold to compare it with.
type qualityView struct {
	Windows      int             `json:"windows"`
	PointsPerSec float64         `json:"points_per_sec"`
	SelfMin      int             `json:"self_min"`
	Worst        *fingerprint.ID `json:"worst_ad,omitempty"`
	WorstLabel   string          `json:"worst_label,omitempty"`
	WorstScore   int             `json:"worst_score"`
	WorstAtMs    int32           `json:"worst_at_ms"`
	MinScore     int             `json:"min_score"`
}

// adQuality measures the ad against itself and the index (it runs Match, so the ad page
// loads it separately).
func (a *Admin) adQuality(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "ad")
	id, err := a.adID(r, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, http.StatusNotFound, err)
		return
	}
	if !isHTMX(r) && !wantsJSON(r) { // a fragment of the ad's page
		http.Redirect(w, r, "/ads/"+id.String(), http.StatusSeeOther)
		return
	}
	q, err := a.quality(r.Context(), id)
	if errors.Is(err, library.ErrNotFound) {
		a.fail(w, r, http.StatusNotFound, err)
		return
	}
	if err != nil {
		a.internal(w, r, err)
		return
	}
	v := qualityView{Windows: q.Windows, PointsPerSec: q.PointsPerSec, SelfMin: q.SelfMin, WorstScore: q.Worst.Score,
		WorstAtMs: mediatime.Ms(q.WorstAt), MinScore: a.minScore()}
	if q.Worst.Score > 0 {
		v.Worst = &q.Worst.AdID
		if ad := a.d.Lib.Get(q.Worst.AdID); ad != nil {
			v.WorstLabel = ad.Label
		}
	}
	p.Data = v
	a.show(w, r, p, "ad-quality", http.StatusOK)
}

// adAction runs an action on the ad of the request's {id}, refreshes the index, and shows
// the ad again with msg, or with the error.
func (a *Admin) adAction(w http.ResponseWriter, r *http.Request, msg string, act func(id fingerprint.ID) error) {
	p := a.newPage(r, "ad")
	id, err := a.adID(r, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, http.StatusNotFound, err)
		return
	}
	if err := act(id); err != nil {
		var rej *record.Rejection
		if errors.As(err, &rej) || errors.Is(err, catalog.ErrNoAd) || errors.Is(err, library.ErrNotFound) || errors.Is(err, errInput) {
			a.fail(w, r, http.StatusUnprocessableEntity, err)
			return
		}
		a.internal(w, r, err)
		return
	}
	if err := a.refresh(); err != nil {
		a.internal(w, r, err)
		return
	}
	p.Msg = p.T(msg)
	a.showAd(w, r, p, "ad-main", http.StatusOK)
}

// errInput marks errors in what the form asked for.
var errInput = errors.New("invalid input")

func inputErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInput, fmt.Sprintf(format, args...))
}

func (a *Admin) adLabel(w http.ResponseWriter, r *http.Request) {
	a.adAction(w, r, "ad.labeled", func(id fingerprint.ID) error {
		l := strings.TrimSpace(r.PostFormValue("label"))
		switch {
		case l == "":
			return inputErr("empty label")
		case utf8.RuneCountInString(l) > record.MaxLabel:
			return inputErr("label longer than %d characters", record.MaxLabel)
		case strings.ContainsFunc(l, unicode.IsControl):
			return inputErr("label has control characters")
		case strings.Contains(l, "://"):
			return inputErr("labels are shared with peers: no URLs") // the catalogue holds no URLs or file names
		}
		typ := r.PostFormValue("type")
		if typ != "" && !adtype.Valid(typ) {
			return inputErr("type %q: want %s or %s", typ, adtype.Ad, adtype.Intro)
		}
		return a.d.DB.Label(a.ctx, id, l, typ)
	})
}

func (a *Admin) adVote(w http.ResponseWriter, r *http.Request) {
	a.adAction(w, r, "ad.voted", func(id fingerprint.ID) error {
		v, err := parseVote(r, "")
		if err != nil {
			return inputErr("%v", err)
		}
		v.Ad = id
		return a.d.DB.Vote(a.ctx, v)
	})
}

func (a *Admin) adPin(w http.ResponseWriter, r *http.Request) {
	on := r.PostFormValue("on") == "1"
	a.adAction(w, r, map[bool]string{true: "ad.pinned", false: "ad.unpinned"}[on], func(id fingerprint.ID) error {
		return a.d.DB.Pin(a.ctx, id, on)
	})
}

// adDelete takes the ad out of this node's library (see deleteAd).
func (a *Admin) adDelete(w http.ResponseWriter, r *http.Request) {
	a.adAction(w, r, "ad.deleted", a.deleteAd)
}

// deleteAd unpins the ad and removes it (Library.Remove): this node's own ad is withdrawn
// (ad.retract, which peers get too), another origin's is voted down as not an ad. The
// catalogue keeps the records, so an own ad stays listed, as retracted.
func (a *Admin) deleteAd(id fingerprint.ID) error {
	if err := a.d.DB.Pin(a.ctx, id, false); err != nil {
		return err
	}
	return a.d.Lib.Remove(a.ctx, id)
}

func (a *Admin) adDup(w http.ResponseWriter, r *http.Request) {
	a.adAction(w, r, "ad.duped", func(id fingerprint.ID) error {
		s := strings.TrimSpace(r.PostFormValue("canonical"))
		if s == "" {
			return inputErr("the canonical ad is missing")
		}
		canon, err := a.adID(r, s)
		if err != nil {
			return inputErr("canonical ad: %v", err)
		}
		if canon == id {
			return inputErr("an ad cannot duplicate itself")
		}
		return a.d.DB.Dup(a.ctx, id, canon)
	})
}
