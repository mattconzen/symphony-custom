package transcript

import "sync"

// Bus is a thread-safe per-issue fan-out for live transcript events. The
// dispatch goroutine calls Publish; WebSocket subscribers each receive on
// a buffered channel returned from Subscribe. Slow subscribers drop
// events rather than blocking the publisher — matches the dashboard
// broadcaster's policy.
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
}

// NewBus returns an empty Bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[string]map[chan Event]struct{})}
}

// Subscribe returns a buffered channel that receives events appended for
// identifier. The cancel func unsubscribes (and closes the channel) and
// must always be called; otherwise subscribers leak.
func (b *Bus) Subscribe(identifier string) (<-chan Event, func()) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	if b.subs[identifier] == nil {
		b.subs[identifier] = make(map[chan Event]struct{})
	}
	b.subs[identifier][ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			if set, ok := b.subs[identifier]; ok {
				delete(set, ch)
				if len(set) == 0 {
					delete(b.subs, identifier)
				}
			}
			b.mu.Unlock()
			close(ch)
		})
	}
}

// Publish fans ev out to every subscriber registered for ev.IssueIdentifier.
// Non-blocking — full buffers drop the event for that subscriber only.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	set := b.subs[ev.IssueIdentifier]
	subs := make([]chan Event, 0, len(set))
	for ch := range set {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// SubscriberCount returns the number of active subscribers for identifier.
// Test-only helper.
func (b *Bus) SubscriberCount(identifier string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[identifier])
}
