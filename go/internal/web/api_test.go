package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openai/symphony/go/internal/observability"
)

func sampleSnapshot() observability.Snapshot {
	started := time.Date(2026, 5, 9, 11, 55, 0, 0, time.UTC)
	lastEvent := time.Date(2026, 5, 9, 11, 59, 30, 0, time.UTC)
	due := time.Date(2026, 5, 9, 12, 5, 0, 0, time.UTC)
	wp1 := "/tmp/work/TEST-1"
	host := "worker-a"
	sid := "sess-1"
	ev := "assistant_message"
	msg := "Refactoring the parser."
	errMsg := "transient agent failure"
	return observability.Snapshot{
		GeneratedAt: time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
		Counts:      observability.Counts{Running: 1, Retrying: 1},
		AgentTotals: observability.TokenTotals{TotalTokens: 1500, InputTokens: 900, OutputTokens: 600, SecondsRunning: 42},
		Running: []observability.RunningEntry{
			{
				IssueID:         "issue-1",
				IssueIdentifier: "TEST-1",
				State:           "In Progress",
				WorkerHost:      &host,
				WorkspacePath:   &wp1,
				SessionID:       &sid,
				TurnCount:       2,
				LastEvent:       &ev,
				LastMessage:     &msg,
				StartedAt:       &started,
				LastEventAt:     &lastEvent,
				Tokens:          observability.EntryTokens{TotalTokens: 800, InputTokens: 500, OutputTokens: 300},
			},
		},
		Retrying: []observability.RetryEntry{
			{
				IssueID:         "issue-3",
				IssueIdentifier: "TEST-3",
				Attempt:         2,
				DueAt:           &due,
				Error:           &errMsg,
			},
		},
	}
}

func TestAPIState_ReturnsSnapshotJSON(t *testing.T) {
	t.Parallel()

	snap := sampleSnapshot()
	h := newTestHandler(snap)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/state")
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: got %q want application/json prefix", ct)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["counts"] == nil {
		t.Errorf("missing counts")
	}
	if got["running"] == nil {
		t.Errorf("missing running")
	}
	if got["retrying"] == nil {
		t.Errorf("missing retrying")
	}
	if got["agent_totals"] == nil {
		t.Errorf("missing agent_totals")
	}
}

func TestAPIState_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	h := newTestHandler(observability.Snapshot{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/state", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST state: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, string(body))
	}
	if env.Error.Code != "method_not_allowed" {
		t.Errorf("code: got %q want method_not_allowed", env.Error.Code)
	}
}

func TestAPIIssue_ReturnsRunningPayload(t *testing.T) {
	t.Parallel()

	h := newTestHandler(sampleSnapshot())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/TEST-1")
	if err != nil {
		t.Fatalf("GET issue: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["issue_identifier"] != "TEST-1" {
		t.Errorf("issue_identifier: got %v want TEST-1", got["issue_identifier"])
	}
	if got["status"] != "running" {
		t.Errorf("status: got %v want running", got["status"])
	}
	if got["running"] == nil {
		t.Errorf("running should be populated")
	}
	if got["retry"] != nil {
		t.Errorf("retry should be null when no retry entry")
	}
	ws, ok := got["workspace"].(map[string]any)
	if !ok {
		t.Fatalf("workspace not an object: %T", got["workspace"])
	}
	if ws["path"] != "/tmp/work/TEST-1" {
		t.Errorf("workspace.path: got %v", ws["path"])
	}
	events, _ := got["recent_events"].([]any)
	if len(events) != 1 {
		t.Errorf("recent_events: got %d want 1", len(events))
	}
}

func TestAPIIssue_ReturnsRetryingPayload(t *testing.T) {
	t.Parallel()

	h := newTestHandler(sampleSnapshot())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/TEST-3")
	if err != nil {
		t.Fatalf("GET issue: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "retrying" {
		t.Errorf("status: got %v want retrying", got["status"])
	}
	if got["running"] != nil {
		t.Errorf("running should be null for retry-only issue")
	}
	if got["last_error"] != "transient agent failure" {
		t.Errorf("last_error: got %v", got["last_error"])
	}
	attempts, _ := got["attempts"].(map[string]any)
	if attempts["current_retry_attempt"] != float64(2) {
		t.Errorf("current_retry_attempt: got %v want 2", attempts["current_retry_attempt"])
	}
	if attempts["restart_count"] != float64(1) {
		t.Errorf("restart_count: got %v want 1", attempts["restart_count"])
	}
}

func TestAPIIssue_SynthesizesWorkspacePathFromRoot(t *testing.T) {
	t.Parallel()

	// Retry-only entry with no WorkspacePath — workspace.path should be
	// synthesized from the configured workspace root and the issue
	// identifier's WorkspaceKey.
	due := time.Date(2026, 5, 9, 12, 5, 0, 0, time.UTC)
	snap := observability.Snapshot{
		Counts: observability.Counts{Retrying: 1},
		Retrying: []observability.RetryEntry{
			{IssueID: "issue-7", IssueIdentifier: "TEST 7", Attempt: 1, DueAt: &due},
		},
	}
	h := newTestHandlerWithRoot(snap, "/srv/symphony/work")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/TEST%207")
	if err != nil {
		t.Fatalf("GET issue: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ws, ok := got["workspace"].(map[string]any)
	if !ok {
		t.Fatalf("workspace not an object: %T", got["workspace"])
	}
	if ws["path"] != "/srv/symphony/work/TEST_7" {
		t.Errorf("workspace.path: got %v want /srv/symphony/work/TEST_7", ws["path"])
	}
}

func TestAPIIssue_NotFound(t *testing.T) {
	t.Parallel()

	h := newTestHandler(sampleSnapshot())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/UNKNOWN-99")
	if err != nil {
		t.Fatalf("GET issue: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, string(body))
	}
	if env.Error.Code != "issue_not_found" {
		t.Errorf("code: got %q want issue_not_found", env.Error.Code)
	}
}

func TestAPIIssue_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	h := newTestHandler(sampleSnapshot())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/TEST-1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE issue: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", resp.StatusCode)
	}
}

func TestAPIRefresh_Accepted_NotCoalesced(t *testing.T) {
	t.Parallel()

	src := &fakeSource{refreshQueued: true}
	h := newHandlerFromSource(src, nil, nil, nil, nil, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/v1/refresh", "", nil)
	if err != nil {
		t.Fatalf("POST refresh: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status: got %d want 202", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["queued"] != true {
		t.Errorf("queued: got %v want true", got["queued"])
	}
	if got["coalesced"] != false {
		t.Errorf("coalesced: got %v want false", got["coalesced"])
	}
	if got["requested_at"] == nil {
		t.Errorf("requested_at should be set")
	}
	ops, _ := got["operations"].([]any)
	if len(ops) != 2 || ops[0] != "poll" || ops[1] != "reconcile" {
		t.Errorf("operations: got %v want [poll, reconcile]", ops)
	}
	if src.refreshCalls != 1 {
		t.Errorf("RequestRefresh calls: got %d want 1", src.refreshCalls)
	}
}

func TestAPIRefresh_Accepted_Coalesced(t *testing.T) {
	t.Parallel()

	// refreshQueued=false simulates a refresh already pending — RequestRefresh
	// returns false so coalesced=true.
	src := &fakeSource{refreshQueued: false}
	h := newHandlerFromSource(src, nil, nil, nil, nil, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/v1/refresh", "", nil)
	if err != nil {
		t.Fatalf("POST refresh: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status: got %d want 202", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["coalesced"] != true {
		t.Errorf("coalesced: got %v want true", got["coalesced"])
	}
	if src.refreshCalls != 1 {
		t.Errorf("RequestRefresh calls: got %d want 1", src.refreshCalls)
	}
}

func TestAPIRefresh_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	h := newTestHandler(observability.Snapshot{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/refresh")
	if err != nil {
		t.Fatalf("GET refresh: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", resp.StatusCode)
	}
}
