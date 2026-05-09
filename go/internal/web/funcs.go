package web

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"
)

// Funcs returns the template.FuncMap required by dashboard.html.tmpl. The
// handler MUST register this map (via template.Funcs) before ParseFS, or
// parsing will fail because the template references these helpers by name.
//
// All helpers tolerate zero values and unexpected types, returning "n/a"
// rather than panicking — the dashboard is observability, not an oracle.
func Funcs() template.FuncMap {
	return template.FuncMap{
		"formatInt":            formatInt,
		"formatRuntime":        formatRuntimeSeconds,
		"prettyValue":          prettyValue,
		"stateBadgeClass":      stateBadgeClass,
		"runtimeAndTurns":      runtimeAndTurns,
		"totalRuntimeSeconds":  totalRuntimeSeconds,
	}
}

func formatInt(v any) string {
	n, ok := toInt64(v)
	if !ok {
		return "n/a"
	}
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	// Insert thousands separators.
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func formatRuntimeSeconds(v any) string {
	secs, ok := toInt64(v)
	if !ok || secs < 0 {
		secs = 0
	}
	return fmt.Sprintf("%dm %ds", secs/60, secs%60)
}

// prettyValue mirrors Elixir's `inspect(value, pretty: true)` for the
// rate-limits panel. nil renders as "n/a"; everything else uses %#v.
func prettyValue(v any) string {
	if v == nil {
		return "n/a"
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return "n/a"
		}
		return s
	}
	return fmt.Sprintf("%#v", v)
}

func stateBadgeClass(state any) string {
	const base = "state-badge"
	s := strings.ToLower(fmt.Sprintf("%v", state))
	switch {
	case containsAny(s, "progress", "running", "active"):
		return base + " state-badge-active"
	case containsAny(s, "blocked", "error", "failed"):
		return base + " state-badge-danger"
	case containsAny(s, "todo", "queued", "pending", "retry"):
		return base + " state-badge-warning"
	default:
		return base
	}
}

func runtimeAndTurns(startedAt any, turnCount any) string {
	rt := runtimeSecondsFrom(startedAt, time.Now().UTC())
	tc, _ := toInt64(turnCount)
	if tc > 0 {
		return fmt.Sprintf("%s / %d", formatRuntimeSeconds(rt), tc)
	}
	return formatRuntimeSeconds(rt)
}

// totalRuntimeSeconds returns the completed-runtime total plus the elapsed
// time of every currently-running session, mirroring the Elixir
// dashboard_live.ex calculation. It accepts the snapshot as `any` and
// reflects across the well-known field names, so it works regardless of
// whether the snapshot package lands its struct as Snapshot or Payload.
func totalRuntimeSeconds(snap any) int64 {
	if snap == nil {
		return 0
	}
	completed, _ := toInt64(fieldByPath(snap, "CodexTotals", "SecondsRunning"))
	now := time.Now().UTC()
	total := completed
	for _, entry := range fieldSlice(snap, "Running") {
		started := fieldByPath(entry, "StartedAt")
		total += runtimeSecondsFrom(started, now)
	}
	return total
}

func runtimeSecondsFrom(startedAt any, now time.Time) int64 {
	switch v := startedAt.(type) {
	case nil:
		return 0
	case time.Time:
		if v.IsZero() {
			return 0
		}
		d := now.Sub(v)
		if d < 0 {
			return 0
		}
		return int64(d.Seconds())
	case string:
		if v == "" {
			return 0
		}
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			t, err = time.Parse(time.RFC3339, v)
		}
		if err != nil {
			return 0
		}
		return runtimeSecondsFrom(t, now)
	default:
		return 0
	}
}
