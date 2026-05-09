// Package web provides the embedded HTTP observability dashboard for the Symphony
// Go runtime. It vends the HTML template, CSS, and vendored htmx assets via
// embed.FS so the binary stays single-file deployable.
package web

import "embed"

//go:embed templates/*.tmpl
var templatesFS embed.FS

//go:embed static/dashboard.css static/htmx.min.js static/htmx-ws.min.js
var staticFS embed.FS

// TemplatesFS is the embedded filesystem rooted at this package containing
// dashboard HTML templates under templates/*.tmpl.
var TemplatesFS embed.FS = templatesFS

// StaticFS is the embedded filesystem rooted at this package containing
// vendored static assets (CSS + htmx) under static/*.
var StaticFS embed.FS = staticFS
