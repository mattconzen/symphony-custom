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

// defaultMaxFileBytes caps each per-dispatch transcript at 100 MiB. Once
// this size is hit, the writer emits a single synthetic
// "transcript_truncated" event and refuses further appends.
const defaultMaxFileBytes int64 = 100 * 1024 * 1024

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
	mu           sync.Mutex
	f            *os.File
	path         string
	size         int64 // current on-disk size in bytes
	maxFileBytes int64
	sealed       bool // true once the synthetic truncation marker has been written
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
	// Seed size from current file length so the per-file cap is
	// preserved across restarts.
	var size int64
	if st, statErr := f.Stat(); statErr == nil {
		size = st.Size()
	}
	return &Writer{f: f, path: path, size: size, maxFileBytes: defaultMaxFileBytes}, nil
}

// SetMaxFileBytes overrides the per-file size cap. Intended for tests.
func (w *Writer) SetMaxFileBytes(n int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.maxFileBytes = n
}

// Path returns the file path the writer is appending to.
func (w *Writer) Path() string { return w.path }

// Append serialises ev as a single JSON line. Payloads that exceed the
// per-line cap are replaced with a short marker and Truncated is set.
// Once the file reaches the per-dispatch size cap, a single synthetic
// "transcript_truncated" line is appended and further appends are
// silently dropped.
func (w *Writer) Append(ev Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return errors.New("transcript: writer closed")
	}
	if w.sealed {
		return nil
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
		// Even after dropping the payload, a pathological field
		// (e.g. a huge IssueIdentifier) may keep the line over the
		// cap. Fall back to a minimal envelope that records only
		// the bookkeeping fields plus an error marker.
		if len(data) > maxPayloadBytes {
			minimal := Event{
				Ts:        ev.Ts,
				Kind:      ev.Kind,
				Truncated: true,
			}
			// Carry the dispatch identity only via SessionID
			// when it itself is small enough to be safe. We
			// shoehorn the explicit error marker through a
			// dedicated minimal-payload struct.
			type minimalEnvelope struct {
				Ts        time.Time `json:"ts"`
				Dispatch  string    `json:"dispatch,omitempty"`
				Kind      string    `json:"kind"`
				Truncated bool      `json:"truncated"`
				Error     string    `json:"error"`
			}
			env := minimalEnvelope{
				Ts:        minimal.Ts,
				Dispatch:  ev.SessionID,
				Kind:      minimal.Kind,
				Truncated: true,
				Error:     "event too large",
			}
			data, err = json.Marshal(env)
			if err != nil {
				return fmt.Errorf("transcript: marshal minimal: %w", err)
			}
		}
	}
	data = append(data, '\n')

	// Per-file size cap: if writing this line would push the file
	// past maxFileBytes, write a single synthetic truncation marker
	// instead and seal the writer.
	if w.maxFileBytes > 0 && w.size+int64(len(data)) > w.maxFileBytes {
		w.sealed = true
		marker := struct {
			Ts        time.Time `json:"ts"`
			Kind      string    `json:"kind"`
			Reason    string    `json:"reason"`
			Truncated bool      `json:"truncated"`
		}{
			Ts:        time.Now().UTC(),
			Kind:      "transcript_truncated",
			Reason:    "size_limit",
			Truncated: true,
		}
		mdata, merr := json.Marshal(marker)
		if merr != nil {
			return fmt.Errorf("transcript: marshal marker: %w", merr)
		}
		mdata = append(mdata, '\n')
		n, werr := w.f.Write(mdata)
		w.size += int64(n)
		return werr
	}

	n, werr := w.f.Write(data)
	w.size += int64(n)
	return werr
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
// maxReadFrom is a sanity bound on the `from` index. A pathological
// value (e.g. 2_147_483_647) would otherwise force an O(n) scan of every
// line in the file just to skip them. Anything past the bound returns
// an empty result, matching the existing "from past end returns empty"
// semantics.
const maxReadFrom = 10_000_000

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
	if from > maxReadFrom {
		return []Event{}, from, true, nil
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
// loading the entire file. Implemented as a two-pass scan so it remains
// correct for files with more than 1000 events.
func Tail(path string, n int) ([]Event, error) {
	if n <= 0 {
		return []Event{}, nil
	}
	// First pass: count total events (lines) in the file.
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 128*1024), 1024*1024)
	total := 0
	for scanner.Scan() {
		total++
	}
	if scanErr := scanner.Err(); scanErr != nil && !errors.Is(scanErr, io.EOF) {
		_ = f.Close()
		return nil, fmt.Errorf("transcript: tail scan: %w", scanErr)
	}
	_ = f.Close()

	from := total - n
	if from < 0 {
		from = 0
	}
	// Second pass: read [from, from+n).
	events, _, _, err := Read(path, from, n)
	if err != nil {
		return nil, err
	}
	return events, nil
}
