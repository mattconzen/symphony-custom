package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"strings"
	"testing"
)

func TestTemplatesFSContainsDashboard(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(TemplatesFS, "templates")
	if err != nil {
		t.Fatalf("read templates dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !contains(names, "dashboard.html.tmpl") {
		t.Fatalf("templates/ missing dashboard.html.tmpl, got %v", names)
	}
}

func TestStaticFSContainsAssets(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"static/dashboard.css",
		"static/htmx.min.js",
		"static/htmx-ws.min.js",
	} {
		data, err := fs.ReadFile(StaticFS, want)
		if err != nil {
			t.Fatalf("read %s: %v", want, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s is empty", want)
		}
	}
}

func TestDashboardTemplateParses(t *testing.T) {
	t.Parallel()

	tmpl, err := template.New("dashboard").
		Funcs(Funcs()).
		ParseFS(TemplatesFS, "templates/*.tmpl")
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}

	if tmpl.Lookup("dashboard.html.tmpl") == nil {
		t.Fatalf("dashboard.html.tmpl not registered after ParseFS")
	}
}

func TestDashboardTemplateReferencesRequiredIDs(t *testing.T) {
	t.Parallel()

	dashboard, err := fs.ReadFile(TemplatesFS, "templates/dashboard.html.tmpl")
	if err != nil {
		t.Fatalf("read dashboard template: %v", err)
	}
	for _, id := range []string{
		`id="metric-grid"`,
		`id="running-sessions"`,
		`id="retrying-sessions"`,
		`id="rate-limits"`,
		`id="header-status"`,
	} {
		if !bytes.Contains(dashboard, []byte(id)) {
			t.Errorf("dashboard template missing required marker %q", id)
		}
	}
	for _, marker := range []string{
		`hx-ext="ws"`,
		`ws-connect="/ws"`,
	} {
		if !bytes.Contains(dashboard, []byte(marker)) {
			t.Errorf("dashboard template missing required marker %q", marker)
		}
	}

	head, err := fs.ReadFile(TemplatesFS, "templates/_head.html.tmpl")
	if err != nil {
		t.Fatalf("read head partial: %v", err)
	}
	for _, marker := range []string{
		`/static/htmx.min.js`,
		`/static/htmx-ws.min.js`,
		`/static/dashboard.css`,
	} {
		if !bytes.Contains(head, []byte(marker)) {
			t.Errorf("head partial missing required marker %q", marker)
		}
	}
}

func TestDashboardTemplateExecutesWithMinimalData(t *testing.T) {
	t.Parallel()

	tmpl, err := template.New("dashboard").
		Funcs(Funcs()).
		ParseFS(TemplatesFS, "templates/*.tmpl")
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}

	type tokens struct {
		TotalTokens, InputTokens, OutputTokens int64
	}
	type counts struct {
		Running, Retrying int
	}
	type codexTotals struct {
		TotalTokens, InputTokens, OutputTokens, SecondsRunning int64
	}
	type snapshot struct {
		Counts      counts
		CodexTotals codexTotals
		RateLimits  any
		Running     []any
		Retrying    []any
		Error       any
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "dashboard.html.tmpl", snapshot{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Operations Dashboard") {
		t.Errorf("expected hero title in output, got %q", trim(out))
	}
	if !strings.Contains(out, "No active sessions.") {
		t.Errorf("expected empty-running placeholder in output")
	}
	if !strings.Contains(out, "No issues are currently backing off.") {
		t.Errorf("expected empty-retrying placeholder in output")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func trim(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
