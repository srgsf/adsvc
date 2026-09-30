// Package media serves a local folder over HTTP, so local files can be played through the
// pass-through proxy like any other source: the proxy reads them from this server, and
// its directory pages link every file to the proxy URL that plays it.
//
// The server is read-only (GET and HEAD, Range requests via http.ServeContent) and never
// leaves the folder: paths go through an os.Root, so neither ".." nor a symlink reaches
// outside it. Names starting with "." are hidden and not served. It has no auth of its
// own and is meant to listen on loopback, where only the proxy reads it.
package media

import (
	"bytes"
	"cmp"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Server serves one folder.
type Server struct {
	root *os.Root
	fsys fs.FS
	// Play returns the proxy URL that plays the file at the given URL of this server.
	Play func(fileURL string) string
	// Base is this server's own URL, as the proxy reaches it (http://127.0.0.1:8000).
	Base string
}

// Open opens dir for serving.
func Open(dir string) (*Server, error) {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Server{root: root, fsys: root.FS()}, nil
}

// Close releases the folder.
func (s *Server) Close() error { return s.root.Close() }

// Dir is the folder being served.
func (s *Server) Dir() string { return s.root.Name() }

// URL is the URL of this server for the file at rel (slash-separated, relative to the
// folder).
func (s *Server) URL(rel string) string {
	return strings.TrimSuffix(s.Base, "/") + (&url.URL{Path: "/" + strings.TrimPrefix(rel, "/")}).EscapedPath()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET or HEAD only", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "."
	}
	if hidden(name) || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	fi, err := fs.Stat(s.fsys, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if fi.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") { // relative links in the listing need it
			// Relative, so that a path like //host cannot redirect to another site.
			http.Redirect(w, r, (&url.URL{Path: path.Base(name) + "/"}).EscapedPath(), http.StatusMovedPermanently)
			return
		}
		s.list(w, r, name)
		return
	}
	if !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	f, err := s.fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "not seekable", http.StatusInternalServerError)
		return
	}
	slog.Debug("media file", "file", name, "range", r.Header.Get("Range"))
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), rs)
}

// hidden reports whether a path has an element starting with ".".
func hidden(name string) bool {
	if name == "." {
		return false
	}
	for el := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(el, ".") {
			return true
		}
	}
	return false
}

type entry struct {
	Name, Href, Play string
	Dir              bool
	Size             int64
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, name string) {
	des, err := fs.ReadDir(s.fsys, name)
	if err != nil {
		http.Error(w, "cannot read the folder", http.StatusInternalServerError)
		return
	}
	rel := func(n string) string {
		if name == "." {
			return n
		}
		return name + "/" + n
	}
	var entries []entry
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		e := entry{Name: de.Name(), Href: (&url.URL{Path: de.Name()}).EscapedPath(), Dir: fi.IsDir()}
		switch {
		case e.Dir:
			e.Href += "/"
		case fi.Mode().IsRegular():
			e.Size = fi.Size()
			if s.Play != nil {
				e.Play = s.Play(s.URL(rel(de.Name())))
			}
		default:
			continue // symlinks to outside, devices, sockets
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	title := "/"
	if name != "." {
		title = "/" + name + "/"
	}
	// Rendered first: a template error is a 500, not a page cut short with a 200.
	var buf bytes.Buffer
	if err := page.Execute(&buf, map[string]any{"Title": title, "Dir": s.Dir(), "Up": name != ".", "Entries": entries}); err != nil {
		slog.Warn("media listing", "err", err)
		http.Error(w, "cannot list the folder", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Cache-Control", "no-cache") // the folder changes
	_, _ = w.Write(buf.Bytes())
}

var page = template.Must(template.New("list").Funcs(template.FuncMap{"size": size}).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>adsvc media {{.Title}}</title>
<style>
:root{color-scheme:light dark;font-family:system-ui,sans-serif}
body{margin:16px;max-width:1100px}
table{border-collapse:collapse;width:100%}
td{padding:4px 8px;border-bottom:1px solid #8884;vertical-align:top}
td.n{text-align:right;white-space:nowrap;font-variant-numeric:tabular-nums}
code{font-size:.85em;word-break:break-all;user-select:all}
p{color:#888}
</style></head><body>
<h1>{{.Title}}</h1>
<p>{{.Dir}}. Give the player the “play” URL of a file; it streams through adsvc, which analyses it on the way. With users configured, put <code>NAME:TOKEN@</code> after <code>http://</code>.</p>
<table>
{{if .Up}}<tr><td><a href="../">..</a></td><td></td><td></td></tr>{{end}}
{{range .Entries}}<tr>
<td><a href="{{.Href}}">{{.Name}}{{if .Dir}}/{{end}}</a></td>
<td class="n">{{if not .Dir}}{{size .Size}}{{end}}</td>
<td>{{if .Play}}<a href="{{.Play}}">play</a> <code>{{.Play}}</code>{{end}}</td>
</tr>{{end}}
</table></body></html>
`))

func size(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	v, i := float64(n), -1
	for v >= unit && i < 3 {
		v /= unit
		i++
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + "KMGT"[i:i+1] + "iB"
}
