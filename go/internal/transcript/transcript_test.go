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
