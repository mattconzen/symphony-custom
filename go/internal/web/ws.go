package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/openai/symphony/go/internal/transcript"
)

// wsControlMessage is the inbound JSON envelope clients send over /ws.
// Either field is the subscribe/unsubscribe target; the other is empty.
type wsControlMessage struct {
	SubscribeTranscript   string `json:"subscribe_transcript,omitempty"`
	UnsubscribeTranscript string `json:"unsubscribe_transcript,omitempty"`
}

// fragmentTemplates is the ordered list of dashboard sub-templates streamed
// over the WebSocket as htmx out-of-band swaps. Order matters: the dashboard
// renders these in this order on first load, so streamed updates follow the
// same convention.
var fragmentTemplates = []struct {
	name      string
	targetID  string
}{
	{"header-status", "header-status"},
	{"kanban", "kanban"},
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
// broadcast. Also reads inbound JSON control messages from the client so
// individual /issue/{id} pages can subscribe to live transcript events.
// Blocks until the client disconnects, the context is cancelled, or a
// write fails.
func (h *Handler) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return // Accept already wrote the failure response.
	}
	defer conn.CloseNow() //nolint:errcheck

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	updates, unsubscribe := h.broadcast.subscribe()
	defer unsubscribe()

	// Per-connection transcript subscription set + the merged channel that
	// receives all events the client is subscribed to.
	transcriptEvents := make(chan transcript.Event, 64)
	subs := newTranscriptSubManager(h.transcriptBus, transcriptEvents)
	defer subs.closeAll()

	// Read-pump: decode JSON control messages from the client.
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			var msg wsControlMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if msg.SubscribeTranscript != "" {
				subs.add(msg.SubscribeTranscript)
			}
			if msg.UnsubscribeTranscript != "" {
				subs.remove(msg.UnsubscribeTranscript)
			}
		}
	}()

	// Send an initial frame so freshly-connected clients see current state.
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
		case ev, ok := <-transcriptEvents:
			if !ok {
				continue
			}
			if err := writeTranscriptFragment(ctx, conn, ev); err != nil {
				return
			}
		}
	}
}

// transcriptSubManager keeps a per-connection subscription set and fans
// in events from the orchestrator bus to a single merged channel.
type transcriptSubManager struct {
	bus    *transcript.Bus
	out    chan<- transcript.Event
	mu     sync.Mutex
	cancel map[string]func()
}

func newTranscriptSubManager(bus *transcript.Bus, out chan<- transcript.Event) *transcriptSubManager {
	return &transcriptSubManager{bus: bus, out: out, cancel: make(map[string]func())}
}

func (m *transcriptSubManager) add(id string) {
	if m.bus == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.cancel[id]; exists {
		return
	}
	ch, cancel := m.bus.Subscribe(id)
	m.cancel[id] = cancel
	go func() {
		for ev := range ch {
			select {
			case m.out <- ev:
			default:
				// Drop on full buffer; per-issue overload doesn't take
				// down the whole connection.
			}
		}
	}()
}

func (m *transcriptSubManager) remove(id string) {
	m.mu.Lock()
	cancel, ok := m.cancel[id]
	delete(m.cancel, id)
	m.mu.Unlock()
	if ok {
		cancel()
	}
}

func (m *transcriptSubManager) closeAll() {
	m.mu.Lock()
	cancels := make([]func(), 0, len(m.cancel))
	for _, c := range m.cancel {
		cancels = append(cancels, c)
	}
	m.cancel = make(map[string]func())
	m.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// writeTranscriptFragment emits a single htmx OOB swap appending one event
// row to the transcript section for the event's issue identifier.
func writeTranscriptFragment(ctx context.Context, conn *websocket.Conn, ev transcript.Event) error {
	payloadJSON, _ := json.Marshal(ev.Payload) //nolint:errcheck
	var buf bytes.Buffer
	fmt.Fprintf(&buf,
		`<div hx-swap-oob="beforeend:#transcript-%s"><li class="transcript-event" data-kind="%s"><span class="ts mono">%s</span><span class="kind">%s</span>`,
		htmlEscapeAttr(ev.IssueIdentifier),
		htmlEscapeAttr(ev.Kind),
		ev.Ts.UTC().Format(time.RFC3339),
		htmlEscape(ev.Kind),
	)
	if ev.Role != "" {
		fmt.Fprintf(&buf, `<span class="role">%s</span>`, htmlEscape(ev.Role))
	}
	fmt.Fprintf(&buf, `<pre class="payload">%s</pre></li></div>`, htmlEscape(string(payloadJSON)))

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, buf.Bytes())
}

func htmlEscape(s string) string { return template.HTMLEscapeString(s) }
func htmlEscapeAttr(s string) string {
	return template.HTMLEscapeString(s)
}

// writeFragments renders each dashboard sub-template wrapped in an htmx
// out-of-band swap marker and pushes the concatenated HTML as a single
// WebSocket text message. One message per update keeps client-side
// bookkeeping minimal and avoids reordering.
func (h *Handler) writeFragments(ctx context.Context, conn *websocket.Conn) error {
	view := dashboardView{Snapshot: h.orch.Snapshot(), CanCreateIssue: h.canCreate()}
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

