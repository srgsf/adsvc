package admin

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
	"github.com/srgsf/adsvc/web"
)

// pageNames are the templates/<name>.tmpl files; each defines "content" (the page in
// the layout) and the fragments htmx asks for.
var pageNames = []string{"login", "disabled", "error", "reports", "ads", "ad", "files", "file", "origins", "peers", "tracking", "config"}

// languages are the translations in web/i18n; the first is the fallback.
var languages = []string{"en", "ru"}

func (a *Admin) load() error {
	base, err := template.New("").Funcs(funcs).ParseFS(web.FS, "templates/layout.tmpl")
	if err != nil {
		return fmt.Errorf("admin templates: %w", err)
	}
	a.pages = map[string]*template.Template{}
	for _, name := range pageNames {
		t, err := template.Must(base.Clone()).ParseFS(web.FS, "templates/"+name+".tmpl")
		if err != nil {
			return fmt.Errorf("admin templates: %w", err)
		}
		a.pages[name] = t
	}
	a.tr = map[string]map[string]string{}
	for _, l := range languages {
		b, err := fs.ReadFile(web.FS, "i18n/"+l+".json")
		if err != nil {
			return err
		}
		m := map[string]string{}
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("admin translations %s: %w", l, err)
		}
		a.tr[l] = m
	}
	a.assets = map[string]string{}
	return fs.WalkDir(web.FS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(web.FS, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		a.assets[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:6])
		return nil
	})
}

// staticHandler serves web/static under /static/. Asset URLs carry the hash of the
// file (page.Asset), so a response to such a URL can be cached for good; embedded files
// have no modification time to revalidate against.
func (a *Admin) staticHandler() http.Handler {
	sub, _ := fs.Sub(web.FS, "static")
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := a.assets[strings.TrimPrefix(r.URL.Path, "/static/")]
		if !ok {
			http.NotFound(w, r) // nor directory listings
			return
		}
		if r.URL.Query().Get("v") == h {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// page is what every template gets: the data of one page plus the request's language,
// messages and the names this node knows origins by.
type page struct {
	Name     string // the template set, and the nav entry
	Lang     string
	Msg, Err string
	Next     string // login: where to go afterwards
	Data     any
	User     viewer // who is logged in (none on the login page)

	a     *Admin
	ctx   context.Context // the request's: the origin names are read on first use
	tr    map[string]string
	names map[record.Origin]string // nil until Origin needs them
	self  record.Origin
}

func (a *Admin) newPage(r *http.Request, name string) *page {
	lang := detectLang(r.Header.Get("Accept-Language"))
	return &page{Name: name, Lang: lang, User: viewerOf(r), a: a, ctx: r.Context(), tr: a.tr[lang], self: a.d.DB.Self()}
}

// translate translates key for r's language, without building a page (error paths).
func (a *Admin) translate(r *http.Request, key string) string {
	return (&page{a: a, tr: a.tr[detectLang(r.Header.Get("Accept-Language"))]}).T(key)
}

// T translates key; a key missing from the language falls back to English, then to the
// key itself.
func (p *page) T(key string) string {
	if s, ok := p.tr[key]; ok {
		return s
	}
	if s, ok := p.a.tr[languages[0]][key]; ok {
		return s
	}
	slog.Debug("admin: no translation", "key", key)
	return key
}

// Tf translates key and formats args into it.
func (p *page) Tf(key string, args ...any) string { return fmt.Sprintf(p.T(key), args...) }

// Origin is how an origin is shown: this node, its name here, or the start of its id.
func (p *page) Origin(o record.Origin) string {
	switch o {
	case record.Origin{}:
		return "—"
	case p.self:
		return p.T("origin.self")
	}
	if p.names == nil {
		names, err := p.a.d.DB.OriginNames(p.ctx)
		if err != nil {
			slog.Warn("admin: reading origin names", "err", err)
		}
		if names == nil {
			names = map[record.Origin]string{}
		}
		p.names = names
	}
	if n := p.names[o]; n != "" {
		return n
	}
	return o.Short()
}

// Version is adsvc's version ("dev" for a build without one).
func (p *page) Version() string { return cmp.Or(p.a.d.Version, "dev") }

// Asset is the URL of a file under web/static.
func (p *page) Asset(file string) string {
	return "/static/" + file + "?v=" + p.a.assets[file]
}

// show answers with p: JSON of p.Data (or of p.Err) when the client asks for JSON, the
// fragment frag (plus the flash message) for htmx, else the whole page.
func (a *Admin) show(w http.ResponseWriter, r *http.Request, p *page, frag string, status int) {
	if wantsJSON(r) {
		if p.Err != "" {
			writeJSON(w, status, map[string]string{"error": p.Err})
		} else {
			writeJSON(w, status, p.Data)
		}
		return
	}
	t := a.pages[p.Name]
	if t == nil {
		t = a.pages["error"]
	}
	var buf bytes.Buffer
	var err error
	if isHTMX(r) && frag != "" {
		err = t.ExecuteTemplate(&buf, frag, p)
		if err == nil && (p.Msg != "" || p.Err != "") {
			err = t.ExecuteTemplate(&buf, "flash-oob", p)
		}
	} else {
		err = t.ExecuteTemplate(&buf, "layout", p)
	}
	if err != nil {
		slog.Error("admin: rendering a page", "page", p.Name, "fragment", frag, "err", err)
		http.Error(w, "rendering failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, private")
	w.Header().Set("Vary", "HX-Request, Accept") // a fragment must not stand in for the page
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// fail answers with an error: JSON, the flash area for htmx (the element that asked keeps
// its content), or an error page.
func (a *Admin) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	p := a.newPage(r, "error")
	p.Err = err.Error()
	if isHTMX(r) {
		w.Header().Set("HX-Retarget", "#flash")
		w.Header().Set("HX-Reswap", "innerHTML")
		a.show(w, r, p, "flash", status)
		return
	}
	a.show(w, r, p, "", status)
}

// internal logs err and answers 500 without details.
func (a *Admin) internal(w http.ResponseWriter, r *http.Request, err error) {
	slog.Warn("admin: request failed", "path", r.URL.Path, "err", err)
	a.fail(w, r, http.StatusInternalServerError, errors.New(a.translate(r, "error.internal")))
}

// isHTMX: htmx asks for a fragment. Restoring history it wants the whole page.
func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true"
}

// wantsJSON: the /api/ routes, or a client that asks for JSON and not for HTML.
func wantsJSON(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	acc := r.Header.Get("Accept")
	return strings.Contains(acc, "application/json") && !strings.Contains(acc, "text/html")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// detectLang picks the supported language the Accept-Language header prefers most (by
// q-value; a primary tag such as "ru" in "ru-RU" is enough), or English.
func detectLang(header string) string {
	best, bestQ := languages[0], 0.0
	for part := range strings.SplitSeq(header, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			var err error
			if q, err = strconv.ParseFloat(v, 64); err != nil {
				continue
			}
		}
		lang, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tag)), "-")
		if slices.Contains(languages, lang) && q > bestQ {
			best, bestQ = lang, q
		}
	}
	return best
}

// funcs are the template helpers. Times in the catalogue are milliseconds.
var funcs = template.FuncMap{
	// pos formats a position in a file: h:mm:ss.s or m:ss.s.
	"pos": func(ms int32) string {
		d := time.Duration(ms) * time.Millisecond
		sign := ""
		if d < 0 {
			sign, d = "-", -d
		}
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		s := float64(d%time.Minute) / float64(time.Second)
		if h > 0 {
			return fmt.Sprintf("%s%d:%02d:%04.1f", sign, h, m, s)
		}
		return fmt.Sprintf("%s%d:%04.1f", sign, m, s)
	},
	// secs formats a duration in ms as seconds.
	"secs": func(ms int32) string { return strconv.FormatFloat(float64(ms)/1000, 'f', 1, 64) + " s" },
	"when": func(tt time.Time) string {
		if tt.IsZero() {
			return "—"
		}
		return tt.Local().Format("2006-01-02 15:04")
	},
	"f1":    func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
	"short": func(id fingerprint.ID) string { return id.Short() },
	"pct": func(n, of int) string {
		if of == 0 {
			return "—"
		}
		return strconv.Itoa(n*100/of) + "%"
	},
	"mib": func(b int64) string { return strconv.FormatFloat(float64(b)/(1<<20), 'f', 1, 64) + " MiB" },
	// counts formats counts by reason, largest first: "quota 3, invalid 1".
	"counts": func(m map[string]int) string {
		if len(m) == 0 {
			return "—"
		}
		keys := slices.SortedFunc(maps.Keys(m), func(a, b string) int {
			if m[a] != m[b] {
				return m[b] - m[a]
			}
			return strings.Compare(a, b)
		})
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + " " + strconv.Itoa(m[k])
		}
		return strings.Join(parts, ", ")
	},
	"list": func(s ...string) []string { return s },
	// args makes the argument of a {{template}} out of name, value pairs.
	"args": func(kv ...any) (map[string]any, error) {
		if len(kv)%2 != 0 {
			return nil, errors.New("args: want name, value pairs")
		}
		m := make(map[string]any, len(kv)/2)
		for i := 0; i < len(kv); i += 2 {
			k, ok := kv[i].(string)
			if !ok {
				return nil, fmt.Errorf("args: name %v is not a string", kv[i])
			}
			m[k] = kv[i+1]
		}
		return m, nil
	},
}
