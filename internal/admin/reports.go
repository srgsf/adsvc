package admin

import (
	"cmp"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/filekey"
	"github.com/srgsf/adsvc/internal/mediatime"
	"github.com/srgsf/adsvc/internal/proxy"
	"github.com/srgsf/adsvc/mpv"
)

// leadIn is how long before a reported position the play link starts: the ad began
// before the user reported it, and marking needs its start decoded.
const leadIn = 20 * time.Second

type reportView struct {
	ID      int64     `json:"id"`
	User    string    `json:"user"`
	Source  string    `json:"source"` // the URL, credentials and volatile parameters redacted
	PosMs   int32     `json:"pos_ms"`
	Note    string    `json:"note,omitempty"`
	Created time.Time `json:"created"`
	Command string    `json:"command"` // plays the file in mpv, for marking the ad
}

// reportFile is the reports of one file, the one reported last first.
type reportFile struct {
	Source  string       `json:"source"`
	Reports []reportView `json:"reports"`
}

type reportsPage struct {
	Files []reportFile `json:"files"`
	All   bool         `json:"all"` // an admin: everyone's reports
}

// reports lists the viewer's reports (an admin's: everyone's), with an mpv command each.
func (a *Admin) reports(w http.ResponseWriter, r *http.Request) {
	p := a.newPage(r, "reports")
	d, err := a.reportsData(r)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	p.Data = d
	a.show(w, r, p, "reports-table", http.StatusOK)
}

func (a *Admin) reportsData(r *http.Request) (reportsPage, error) {
	v := viewerOf(r)
	user := v.Name
	if v.Admin {
		user = ""
	}
	reps, err := a.d.DB.Reports(r.Context(), user)
	if err != nil {
		return reportsPage{}, err
	}
	base := a.baseURL(r)
	d := reportsPage{All: v.Admin, Files: []reportFile{}}
	at := map[string]int{} // file -> index in d.Files
	for _, rep := range reps {
		file := cmp.Or(rep.FileKey, rep.Norm)
		i, ok := at[file]
		if !ok {
			i = len(d.Files)
			at[file] = i
			d.Files = append(d.Files, reportFile{Source: redact(rep.URL)})
		}
		d.Files[i].Reports = append(d.Files[i].Reports, reportView{ID: rep.ID, User: rep.User, Source: redact(rep.URL),
			PosMs: rep.PosMs, Note: rep.Note, Created: rep.Created, Command: mpvCommand(base, rep)})
	}
	return d, nil
}

func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return filekey.RedactURL(u)
}

// mpvCommand is the command that plays a report's file through adsvc in mpv, from a
// little before the reported position. The URL is
// single-quoted, which POSIX shells and PowerShell read alike: it holds no quote, since
// everything but base is query-escaped.
func mpvCommand(base string, rep catalog.Report) string {
	link := base + "/s?u=" + url.QueryEscape(rep.URL)
	if rep.Sel != "" {
		link += "&" + rep.Sel
	}
	link += "&" + proxy.MarkParam + "=1"
	link = strings.ReplaceAll(link, "'", "%27")
	start := max(0, mediatime.Dur(rep.PosMs)-leadIn)
	return fmt.Sprintf("mpv --start=%d '%s'", int(start.Seconds()), link) // mpv takes seconds
}

// baseURL is where clients reach adsvc: the configured public URL, or the scheme and host
// the request came to.
func (a *Admin) baseURL(r *http.Request) string {
	if a.d.PublicURL != nil {
		if u := a.d.PublicURL(); u != "" {
			return strings.TrimSuffix(u, "/")
		}
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// reportDelete deletes one of the viewer's reports (an admin: anyone's).
func (a *Admin) reportDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, fmt.Errorf("bad report id"))
		return
	}
	v := viewerOf(r)
	user := v.Name
	if v.Admin {
		user = ""
	}
	ok, err := a.d.DB.DeleteReport(r.Context(), id, user)
	if err != nil {
		a.internal(w, r, err)
		return
	}
	p := a.newPage(r, "reports")
	if !ok {
		a.fail(w, r, http.StatusNotFound, fmt.Errorf("%s", p.T("reports.gone")))
		return
	}
	if p.Data, err = a.reportsData(r); err != nil {
		a.internal(w, r, err)
		return
	}
	p.Msg = p.T("reports.deleted")
	a.show(w, r, p, "reports-table", http.StatusOK)
}

// script hands out adskip.lua with the viewer's token in it: one download serves all of
// their reports (and plain playback through adsvc).
func (a *Admin) script(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-lua; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="adskip.lua"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(mpv.Script(viewerOf(r).token))
}
