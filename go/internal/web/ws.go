package web

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// fragmentTemplates is the ordered list of dashboard sub-templates streamed
// over the WebSocket as htmx out-of-band swaps. Order matters: the dashboard
// renders these in this order on first load, so streamed updates follow the
// same convention.
var fragmentTemplates = []struct {
	name      string
	targetID  string
}{
	{"header-status", "header-status"},
	{"metric-grid", "metric-grid"},
	{"rate-limits", "rate-limits"},
	{"running-sessions", "running-sessions"},
	{"retrying-sessions", "retrying-sessions"},
}

// broadcaster fans out a single orchestrator update event to many concurrent
// WebSocket subscribers. Each subscriber is a buffered channel registered
// at handshake time and removed when the connection closes. broadcasts are
// non-blocking: if a subscriber's buffer is full (slow client), the event
// is dropped for that subscriber rather than holding up the orchestrator.
type broadcaster struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: make(map[chan struct{}]struct{})}
}

// subscribe returns a buffered channel that receives a token for every
// broadcast. The unsubscribe func removes it from the active set and
// closes the channel; callers MUST invoke it on disconnect.
func (b *broadcaster) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	once := sync.Once{}
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
}

// broadcast wakes every subscriber. Non-blocking: a subscriber whose buffer
// is already full is skipped — they'll pick up the next event.
func (b *broadcaster) broadcast() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// handleWS upgrades the connection, subscribes to orchestrator updates, and
// streams an initial snapshot followed by an HTML fragment payload on each
// broadcast. Blocks until the client disconnects, the context is cancelled,
// or a write fails.
func (h *Handler) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The dashboard is bound to 127.0.0.1 by default and the client is
		// served from the same origin. InsecureSkipVerify here means "do
		// not enforce the same-origin check"; that's deliberate because
		// operators may put the dashboard behind a reverse proxy that
		// rewrites the Host header. Auth/origin enforcement is the proxy's
		// job, matching the no-auth design decision in the proposal.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return // Accept already wrote the failure response.
	}
	defer conn.CloseNow() //nolint:errcheck

	ctx := r.Context()
	updates, unsubscribe := h.broadcast.subscribe()
	defer unsubscribe()

	// Send an initial frame so freshly-connected clients see current state
	// without waiting for the next orchestrator event.
	if err := h.writeFragments(ctx, conn); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-updates:
			if !ok {
				return
			}
			if err := h.writeFragments(ctx, conn); err != nil {
				return
			}
		}
	}
}

// writeFragments renders each dashboard sub-template wrapped in an htmx
// out-of-band swap marker and pushes the concatenated HTML as a single
// WebSocket text message. One message per update keeps client-side
// bookkeeping minimal and avoids reordering.
func (h *Handler) writeFragments(ctx context.Context, conn *websocket.Conn) error {
	view := dashboardView{Snapshot: h.orch.Snapshot()}
	payload, err := renderFragments(h.tmpl, view)
	if err != nil {
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, payload)
}

func renderFragments(tmpl *template.Template, view dashboardView) ([]byte, error) {
	var out bytes.Buffer
	for _, frag := range fragmentTemplates {
		fmt.Fprintf(&out, `<div hx-swap-oob="innerHTML:#%s">`, frag.targetID)
		if err := tmpl.ExecuteTemplate(&out, frag.name, view); err != nil {
			return nil, fmt.Errorf("render %s: %w", frag.name, err)
		}
		out.WriteString(`</div>`)
	}
	return out.Bytes(), nil
}

