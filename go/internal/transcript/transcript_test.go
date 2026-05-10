package transcript_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/transcript"
)

func TestWriter_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	w, err := transcript.NewWriter(path)
	require.NoError(t, err)

	require.NoError(t, w.Append(transcript.Event{IssueIdentifier: "WEB-1", Kind: "session_started"}))
	require.NoError(t, w.Append(transcript.Event{IssueIdentifier: "WEB-1", Kind: "assistant_message", Payload: "hi"}))
	require.NoError(t, w.Close())

	events, _, complete, err := transcript.Read(path, 0, 100)
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, "session_started", events[0].Kind)
	assert.Equal(t, "assistant_message", events[1].Kind)
	assert.True(t, complete)
}

func TestWriter_TruncatesLargePayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	w, err := transcript.NewWriter(path)
	require.NoError(t, err)
	defer w.Close() //nolint:errcheck

	big := strings.Repeat("x", 80*1024)
	require.NoError(t, w.Append(transcript.Event{IssueIdentifier: "WEB-1", Kind: "tool_result", Payload: big}))

	// Open the file directly to verify the JSON line is well-formed.
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 128*1024), 1024*1024)
	require.True(t, sc.Scan())
	line := sc.Text()
	assert.True(t, strings.Contains(line, `"truncated":true`), "expected truncated flag in %q", line)
	assert.NotContains(t, line, big, "large payload should be replaced")
}

func TestRead_Pagination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	w, err := transcript.NewWriter(path)
	require.NoError(t, err)
	for i := 0; i < 25; i++ {
		require.NoError(t, w.Append(transcript.Event{IssueIdentifier: "WEB-1", Kind: "tick"}))
	}
	require.NoError(t, w.Close())

	page1, next, complete, err := transcript.Read(path, 0, 10)
	require.NoError(t, err)
	assert.Len(t, page1, 10)
	assert.Equal(t, 10, next)
	assert.False(t, complete)

	page2, next, complete, err := transcript.Read(path, next, 10)
	require.NoError(t, err)
	assert.Len(t, page2, 10)
	assert.Equal(t, 20, next)
	assert.False(t, complete)

	page3, next, complete, err := transcript.Read(path, next, 10)
	require.NoError(t, err)
	assert.Len(t, page3, 5)
	assert.Equal(t, 25, next)
	assert.True(t, complete)
}

func TestTail_ReturnsLastN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	w, _ := transcript.NewWriter(path)
	for i := 0; i < 100; i++ {
		_ = w.Append(transcript.Event{Kind: "k", Turn: i})
	}
	_ = w.Close()

	tail, err := transcript.Tail(path, 5)
	require.NoError(t, err)
	require.Len(t, tail, 5)
	assert.Equal(t, 99, tail[len(tail)-1].Turn)
}

func TestWriter_OversizedIdentifierFallsBackToMinimal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	w, err := transcript.NewWriter(path)
	require.NoError(t, err)
	defer w.Close() //nolint:errcheck

	// IssueIdentifier itself is huge, so dropping the payload won't help.
	huge := strings.Repeat("y", 100*1024)
	require.NoError(t, w.Append(transcript.Event{
		IssueIdentifier: huge,
		Kind:            "tool_result",
		Payload:         "small",
	}))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 256*1024), 256*1024)
	require.True(t, sc.Scan())
	line := sc.Text()
	// The written line including newline must be bounded.
	assert.LessOrEqual(t, len(line)+1, 64*1024, "line should be <= 64 KiB")
	assert.Contains(t, line, `"error":"event too large"`)
	assert.NotContains(t, line, huge)
}

func TestRead_FromHugeOffsetReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	w, err := transcript.NewWriter(path)
	require.NoError(t, err)
	require.NoError(t, w.Append(transcript.Event{Kind: "k"}))
	require.NoError(t, w.Close())

	events, _, complete, err := transcript.Read(path, 1<<31, 10)
	require.NoError(t, err)
	assert.Len(t, events, 0)
	assert.True(t, complete)
}

func TestWriter_PerFileSizeCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	w, err := transcript.NewWriter(path)
	require.NoError(t, err)
	w.SetMaxFileBytes(4096)

	// Write enough events to exceed 4 KiB. Each event with a short
	// payload is well over 50 bytes, so 200 attempts is plenty.
	for i := 0; i < 200; i++ {
		require.NoError(t, w.Append(transcript.Event{
			IssueIdentifier: "WEB-1",
			Kind:            "tick",
			Turn:            i,
		}))
	}
	require.NoError(t, w.Close())

	// File size is bounded near the cap (allowing the marker line).
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.LessOrEqual(t, info.Size(), int64(4096+512), "file should respect cap")

	// Read every line; the last must be the synthetic marker.
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 256*1024)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	require.NotEmpty(t, lines)
	last := lines[len(lines)-1]
	assert.Contains(t, last, `"kind":"transcript_truncated"`)
	assert.Contains(t, last, `"reason":"size_limit"`)
}

func TestTail_LargeFileReturnsCorrectWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	w, err := transcript.NewWriter(path)
	require.NoError(t, err)
	const total = 1500
	for i := 0; i < total; i++ {
		require.NoError(t, w.Append(transcript.Event{Kind: "k", Turn: i}))
	}
	require.NoError(t, w.Close())

	tail, err := transcript.Tail(path, 5)
	require.NoError(t, err)
	require.Len(t, tail, 5)
	// Expect events at indices 1495..1499 inclusive.
	for i, ev := range tail {
		assert.Equal(t, total-5+i, ev.Turn, "tail[%d] Turn mismatch", i)
	}
}

func TestBus_FanOut(t *testing.T) {
	b := transcript.NewBus()
	ch1, cancel1 := b.Subscribe("WEB-1")
	ch2, cancel2 := b.Subscribe("WEB-1")
	defer cancel1()
	defer cancel2()

	b.Publish(transcript.Event{IssueIdentifier: "WEB-1", Kind: "k"})
	select {
	case ev := <-ch1:
		assert.Equal(t, "k", ev.Kind)
	case <-time.After(time.Second):
		t.Fatal("ch1 never received event")
	}
	select {
	case ev := <-ch2:
		assert.Equal(t, "k", ev.Kind)
	case <-time.After(time.Second):
		t.Fatal("ch2 never received event")
	}
}

func TestBus_UnsubscribeStopsDelivery(t *testing.T) {
	b := transcript.NewBus()
	ch, cancel := b.Subscribe("WEB-1")
	cancel()
	b.Publish(transcript.Event{IssueIdentifier: "WEB-1", Kind: "k"})
	_, ok := <-ch
	assert.False(t, ok, "channel should be closed")
	assert.Equal(t, 0, b.SubscriberCount("WEB-1"))
}

func TestBus_DropCounterIncrementsWhenBufferFull(t *testing.T) {
	b := transcript.NewBus()
	_, cancel := b.Subscribe("WEB-1")
	defer cancel()

	// Buffer is 32; publish 50 without ever receiving, so 18 will drop.
	const published = 50
	for i := 0; i < published; i++ {
		b.Publish(transcript.Event{IssueIdentifier: "WEB-1", Kind: "k"})
	}

	snap := b.Snapshot()
	got, ok := snap["WEB-1"]
	require.True(t, ok, "snapshot should include WEB-1")
	assert.Equal(t, int64(published-32), got)
}

func TestBus_OtherIssueNotReceived(t *testing.T) {
	b := transcript.NewBus()
	ch, cancel := b.Subscribe("WEB-1")
	defer cancel()
	b.Publish(transcript.Event{IssueIdentifier: "WEB-2", Kind: "k"})
	select {
	case <-ch:
		t.Fatal("subscriber should not receive events for other issues")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWriter_ConcurrentAppendsSerialise(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	w, _ := transcript.NewWriter(path)
	defer w.Close() //nolint:errcheck

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, w.Append(transcript.Event{IssueIdentifier: "WEB-1", Kind: "k"}))
		}()
	}
	wg.Wait()

	events, _, _, err := transcript.Read(path, 0, 1000)
	require.NoError(t, err)
	assert.Len(t, events, 100)
}
