package proxy

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/filekey"
	"github.com/srgsf/adsvc/internal/mediatime"
)

// reportSlack widens a marked ad when clearing the reports it answers: a user reports an
// ad some seconds after it started, or just after it ended.
const reportSlack = 15 * time.Second

// addReport stores an ad a user saw, to be marked later (POST /ads/report): position is
// where, in milliseconds on the media timeline. u is required: the play link for
// marking is made of it.
func (p *Proxy) addReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u, err := url.Parse(param(q, "u", "upstream"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "u must be the http(s) source URL", http.StatusBadRequest)
		return
	}
	pos, ok := msParam(q, "position")
	if !ok {
		http.Error(w, "position must be in milliseconds", http.StatusBadRequest)
		return
	}
	_, key, norm := fileIdent(q)
	sel := url.Values{}
	if v := q.Get("id"); v != "" {
		sel.Set("id", v)
	} else if ih := param(q, "ih", "hash"); ih != "" {
		sel.Set("ih", ih)
		if idx := param(q, "idx", "index"); idx != "" {
			sel.Set("idx", idx)
		}
	}
	rep := catalog.Report{User: reqUser(r), FileKey: key, Norm: norm, URL: u.String(), Sel: sel.Encode(),
		PosMs: mediatime.Ms(pos), Note: q.Get("note")}
	id, err := p.DB.AddReport(r.Context(), rep)
	if err != nil {
		slog.Warn("storing a report", "err", err)
		http.Error(w, "catalogue unavailable", http.StatusInternalServerError)
		return
	}
	if s := p.lookup(r); s != nil { // being played right now: keep its audio for marking
		s.mu.Lock()
		s.reported = true
		s.mu.Unlock()
	}
	reports.Inc()
	slog.Info("ad reported", "report", id, "user", rep.User, "key", key, "upstream", filekey.RedactURL(u), "position_ms", rep.PosMs)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"id": id, "position": rep.PosMs})
}

// listReports answers GET /ads/reports: the caller's reports.
func (p *Proxy) listReports(w http.ResponseWriter, r *http.Request) {
	reps, err := p.DB.Reports(r.Context(), reqUser(r))
	if err != nil {
		slog.Warn("listing reports", "err", err)
		http.Error(w, "catalogue unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, append([]catalog.Report{}, reps...))
}

// deleteReport answers DELETE /ads/reports/{id}: one of the caller's reports.
func (p *Proxy) deleteReport(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ok, err := p.DB.DeleteReport(r.Context(), id, reqUser(r))
	if err != nil {
		slog.Warn("deleting a report", "err", err)
		http.Error(w, "catalogue unavailable", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no such report", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clearReports deletes the caller's reports an ad just marked in s answers: those of the
// same file within the ad (give or take reportSlack). Without open reports left, the
// session goes back to skipping analysed ranges.
func (p *Proxy) clearReports(r *http.Request, s *session, start, end time.Duration) {
	user, ctx := reqUser(r), r.Context()
	s.mu.Lock()
	files := append([]string{s.id, s.key, s.norm}, s.alias...)
	s.mu.Unlock()
	n, err := p.DB.DeleteReportsIn(ctx, user, files, mediatime.Ms(start-reportSlack), mediatime.Ms(end+reportSlack))
	if err != nil {
		slog.Warn("clearing reports", "session", s.id, "err", err)
	}
	if n > 0 {
		slog.Info("reports cleared", "session", s.id, "user", user, "reports", n)
	}
	reported := p.reported(files...)
	s.mu.Lock()
	s.reported = reported
	s.mu.Unlock()
}

// reported reports whether a file known by any of files has an open report (anyone's).
func (p *Proxy) reported(files ...string) bool {
	ok, err := p.DB.HasReports(p.ctx, files)
	if err != nil {
		slog.Warn("reading reports", "err", err)
	}
	return ok
}
