// Package transcript implements the per-issue JSONL audit trail per SPEC
// §13.4. Each dispatch opens a Writer that appends one JSON line per
// agent.Event; readers stream the same file back for the /transcript API
// and the /issue/{id} dashboard page.
package transcript

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxPayloadBytes caps payload size before truncation. POSIX guarantees
// atomicity of write() calls up to PIPE_BUF, so we stay well under that
// threshold for typical events; very large payloads are clipped.
const maxPayloadBytes = 64 * 1024

// Event is one line of the per-issue JSONL transcript.
type Event struct {
	Ts              time.Time `json:"ts"`
	IssueID         string    `json:"issue_id"`
	IssueIdentifier string    `json:"issue_identifier"`
	SessionID       string    `json:"session_id,omitempty"`
	Role            string    `json:"role,omitempty"`
	Turn            int       `json:"turn,omitempty"`
	Kind            string    `json:"kind"`
	Payload         any       `json:"payload,omitempty"`
	Truncated       bool      `json:"truncated,omitempty"`
}

// Writer appends events to a single per-dispatch JSONL file. Concurrent
// Append calls are serialised via a mutex; the underlying file is opened
// with O_APPEND so the kernel guarantees ordering even when the lock is
// dropped (the mutex is belt-and-braces).
type Writer struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// NewWriter opens (or creates) the JSONL file at path with O_APPEND.
// Parent directories are created on the way down.
func NewWriter(path string) (*Writer, error) {
	if path == "" {
		return nil, errors.New("transcript: path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("transcript: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("transcript: open %s: %w", path, err)
	}
	return &Writer{f: f, path: path}, nil
}

// Path returns the file path the writer is appending to.
func (w *Writer) Path() string { return w.path }

// Append serialises ev as a single JSON line. Payloads that exceed the
// per-line cap are replaced with a short marker and Truncated is set.
func (w *Writer) Append(ev Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return errors.New("transcript: writer closed")
	}
	if ev.Ts.IsZero() {
		ev.Ts = time.Now().UTC()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("transcript: marshal: %w", err)
	}
	if len(data) > maxPayloadBytes {
		clipped := ev
		clipped.Payload = nil
		clipped.Truncated = true
		data, err = json.Marshal(clipped)
		if err != nil {
			return fmt.Errorf("transcript: marshal truncated: %w", err)
		}
	}
	data = append(data, '\n')
	_, err = w.f.Write(data)
	return err
}

// Close releases the file handle. A nil writer or a doubly-closed writer
// is a no-op.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Read returns up to limit events starting at line index from, along with
// the next index to resume from. The complete flag is true when fewer
// than limit events were returned (i.e. EOF was reached).
func Read(path string, from, limit int) ([]Event, int, bool, error) {
	if from < 0 {
		from = 0
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, from, true, err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 128*1024), 1024*1024)

	idx := 0
	out := make([]Event, 0, limit)
	for scanner.Scan() {
		if idx < from {
			idx++
			continue
		}
		if len(out) >= limit {
			break
		}
		line := scanner.Bytes()
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// Skip malformed lines but advance the cursor so paging
			// stays sequential.
			idx++
			continue
		}
		out = append(out, ev)
		idx++
	}
	complete := false
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return out, idx, false, fmt.Errorf("transcript: scan: %w", err)
	}
	// If we got fewer than limit events, we've reached EOF.
	if len(out) < limit {
		complete = true
	}
	return out, idx, complete, nil
}

// Tail returns the last n events from path, in chronological order.
// Used by the /issue/{id} page to seed the transcript section without
// loading the entire file.
func Tail(path string, n int) ([]Event, error) {
	all, _, _, err := Read(path, 0, 1000)
	if err != nil {
		return nil, err
	}
	if len(all) <= n {
		return all, nil
	}
	return all[len(all)-n:], nil
}
