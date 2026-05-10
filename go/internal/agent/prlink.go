package agent

import (
	"regexp"
	"strconv"
	"sync"
	"time"
)

// prURLPattern matches https://github.com/<owner>/<repo>/pull/<number> URLs.
// The owner and repo capture groups exclude '/' and whitespace.
var prURLPattern = regexp.MustCompile(`https://github\.com/([^/\s]+)/([^/\s]+)/pull/(\d+)`)

// ExtractPRLink scans s for a GitHub pull request URL. Returns the first match
// it finds; ok is false when no match.
func ExtractPRLink(s string) (PRLinkPayload, bool) {
	m := prURLPattern.FindStringSubmatch(s)
	if m == nil {
		return PRLinkPayload{}, false
	}
	num, err := strconv.Atoi(m[3])
	if err != nil {
		return PRLinkPayload{}, false
	}
	return PRLinkPayload{
		URL:    m[0],
		Owner:  m[1],
		Repo:   m[2],
		Number: num,
	}, true
}

// PRLinkEmitter wraps an EventCallback so successive runtimes can scan
// streamed text for GitHub PR URLs and emit a single EventPRLink the first
// time one is seen for a session.
type PRLinkEmitter struct {
	cb EventCallback

	mu   sync.Mutex
	seen map[string]struct{}
}

// NewPRLinkEmitter wraps cb. Pass cb=nil to disable.
func NewPRLinkEmitter(cb EventCallback) *PRLinkEmitter {
	return &PRLinkEmitter{cb: cb, seen: map[string]struct{}{}}
}

// Scan inspects s for a PR URL. If one is found and not already emitted in
// this emitter, it fires the EventPRLink callback.
func (p *PRLinkEmitter) Scan(sessionID, s string) {
	if p == nil || p.cb == nil {
		return
	}
	link, ok := ExtractPRLink(s)
	if !ok {
		return
	}
	p.mu.Lock()
	if _, dup := p.seen[link.URL]; dup {
		p.mu.Unlock()
		return
	}
	p.seen[link.URL] = struct{}{}
	p.mu.Unlock()

	p.cb(Event{
		Kind:      EventPRLink,
		Timestamp: time.Now().UTC(),
		SessionID: sessionID,
		Payload:   link,
	})
}
