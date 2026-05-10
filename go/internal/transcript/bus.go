package transcript

import (
	"sync"
	"sync/atomic"
)

// Bus is a thread-safe per-issue fan-out for live transcript events. The
// dispatch goroutine calls Publish; WebSocket subscribers each receive on
// a buffered channel returned from Subscribe. Slow subscribers drop
// events rather than blocking the publisher — matches the dashboard
// broadcaster's policy.
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[*subscription]struct{}
}

// subscription pairs a subscriber's delivery channel with a per-subscriber
// atomic counter of events dropped due to a full buffer.
type subscription struct {
	ch      chan Event
	id      string
	dropped atomic.Int64
}

// NewBus returns an empty Bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[string]map[*subscription]struct{})}
}

// Subscribe returns a buffered channel that receives events appended for
// identifier. The cancel func unsubscribes (and closes the channel) and
// must always be called; otherwise subscribers leak.
func (b *Bus) Subscribe(identifier string) (<-chan Event, func()) {
	sub := &subscription{
		ch: make(chan Event, 32),
		id: identifier,
	}
	b.mu.Lock()
	if b.subs[identifier] == nil {
		b.subs[identifier] = make(map[*subscription]struct{})
	}
	b.subs[identifier][sub] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			if set, ok := b.subs[identifier]; ok {
				delete(set, sub)
				if len(set) == 0 {
					delete(b.subs, identifier)
				}
			}
			b.mu.Unlock()
			close(sub.ch)
		})
	}
}

// Publish fans ev out to every subscriber registered for ev.IssueIdentifier.
// Non-blocking — full buffers drop the event for that subscriber only and
// increment the subscriber's drop counter.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	set := b.subs[ev.IssueIdentifier]
	subs := make([]*subscription, 0, len(set))
	for s := range set {
		subs = append(subs, s)
	}
	b.mu.Unlock()

	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
			s.dropped.Add(1)
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

// Snapshot returns the current per-subscriber-identifier drop totals,
// summed across all active subscribers for that identifier. Useful for
// the /metrics endpoint.
func (b *Bus) Snapshot() map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int64, len(b.subs))
	for id, set := range b.subs {
		var total int64
		for s := range set {
			total += s.dropped.Load()
		}
		out[id] = total
	}
	return out
}
