// Package web holds the admin page's templates, translations and static files
// (package admin), embedded into the binary. The list is explicit, so that stray files
// (.DS_Store, sources of generated files) are not embedded.
//
// static/js/admin.js is served as written (no build step). static/js/htmx.min.js is
// htmx, vendored so that the page works offline.
package web

import "embed"

// FS is the embedded files.
//
//go:embed static/css/admin.css static/js/htmx.min.js static/js/admin.js static/img/*.png templates/*.tmpl i18n/*.json
var FS embed.FS
