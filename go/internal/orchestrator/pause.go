package orchestrator

import (
	"errors"
)

// Sentinel errors returned by the pause/resume/cancel control methods.
var (
	// ErrNotRunning is returned when no in-flight dispatch matches the
	// supplied identifier. Maps to HTTP 404.
	ErrNotRunning = errors.New("orchestrator: issue is not currently running")

	// ErrNotPaused is returned by Resume when the entry exists but is not
	// in the paused state. Maps to HTTP 409.
	ErrNotPaused = errors.New("orchestrator: issue is not paused")
)

// findRunningByIdentifier looks up a run entry by issue identifier (NOT id).
// The lookup walks the running map because the dashboard/UX uses
// identifiers, not internal IDs. Caller must hold o.mu.
func (o *Orchestrator) findRunningByIdentifier(identifier string) *runEntry {
	for _, e := range o.running {
		if e.issue.Identifier == identifier {
			return e
		}
	}
	return nil
}

// RequestPause asks the named dispatch to pause after its current turn.
// The transition is idempotent: a second call while already in
// pause_requested or paused state is a no-op.
func (o *Orchestrator) RequestPause(identifier string) error {
	o.mu.Lock()
	entry := o.findRunningByIdentifier(identifier)
	if entry == nil {
		o.mu.Unlock()
		return ErrNotRunning
	}
	if entry.state == runStateRunning {
		entry.state = runStatePauseRequested
		// Re-create the pause channel for each pause cycle so a fresh
		// Resume close is always observable.
		entry.pauseCh = make(chan struct{})
	}
	o.mu.Unlock()
	o.notify()
	return nil
}

// Resume releases a paused dispatch back to running. Returns ErrNotPaused
// if the entry exists but is not paused (callers map this to 409). The
// dispatch goroutine receives from pauseCh; closing it unblocks the loop.
func (o *Orchestrator) Resume(identifier string) error {
	o.mu.Lock()
	entry := o.findRunningByIdentifier(identifier)
	if entry == nil {
		o.mu.Unlock()
		return ErrNotRunning
	}
	if entry.state != runStatePaused && entry.state != runStatePauseRequested {
		o.mu.Unlock()
		return ErrNotPaused
	}
	ch := entry.pauseCh
	entry.state = runStateRunning
	entry.pauseCh = nil
	o.mu.Unlock()

	if ch != nil {
		// Close outside the lock so the receiver can re-enter the
		// orchestrator without deadlock.
		safeClose(ch)
	}
	o.notify()
	return nil
}

// RequestCancel cancels the named dispatch. The agent's RunTurn observes
// ctx.Done() within ~read_timeout_ms; the retry path is suppressed.
func (o *Orchestrator) RequestCancel(identifier string) error {
	o.mu.Lock()
	entry := o.findRunningByIdentifier(identifier)
	if entry == nil {
		o.mu.Unlock()
		return ErrNotRunning
	}
	entry.state = runStateCancelRequested
	cancel := entry.cancel
	// If the entry is currently sitting in a pause, wake it so the
	// turn-loop returns and observes cancel state.
	pauseCh := entry.pauseCh
	entry.pauseCh = nil
	o.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if pauseCh != nil {
		safeClose(pauseCh)
	}
	o.notify()
	return nil
}

// RunStateOf returns the wire-form state for the named identifier. The
// boolean is false when the identifier is not currently running.
func (o *Orchestrator) RunStateOf(identifier string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry := o.findRunningByIdentifier(identifier)
	if entry == nil {
		return "", false
	}
	return entry.state.String(), true
}

// safeClose closes the channel exactly once. A second close would panic,
// so we recover defensively in case the dispatch loop and an
// orchestrator-side Resume race for the close.
func safeClose(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}
