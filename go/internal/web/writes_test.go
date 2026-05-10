package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
)

// fakeTracker is a minimal trackerSource + SpecReadWriter for write-endpoint
// tests.
type fakeTracker struct {
	createErr   error
	createdAs   string
	specBody    string
	specEtag    string
	writtenBody string
}

func (f *fakeTracker) CreateIssue(_ context.Context, draft domain.IssueDraft) (domain.Issue, error) {
	if f.createErr != nil {
		return domain.Issue{}, f.createErr
	}
	f.createdAs = draft.Title
	return domain.Issue{ID: "MEM-1", Identifier: "MEM-1", Title: draft.Title}, nil
}

func (f *fakeTracker) HasSpec(_ context.Context, _ string) (bool, error) { return f.specBody != "", nil }

func (f *fakeTracker) ReadSpec(_ context.Context, _ string) (string, string, error) {
	if f.specBody == "" {
		return "", "", domain.ErrSpecNotFound
	}
	return f.specBody, f.specEtag, nil
}

func (f *fakeTracker) WriteSpec(_ context.Context, _, body, ifMatchEtag string) (string, error) {
	if ifMatchEtag != "" && ifMatchEtag != f.specEtag {
		return "", domain.ErrSpecConflict
	}
	f.specBody = body
	f.specEtag = "etag-new"
	f.writtenBody = body
	return f.specEtag, nil
}

func newWriteHandler(trk trackerSource, gen SpecGenerator) *Handler {
	return newHandlerFromSource(&fakeSource{snap: observability.Snapshot{}, refreshQueued: true}, nil, nil, nil, trk, gen)
}

func TestCreateIssue_Created(t *testing.T) {
	t.Parallel()
	trk := &fakeTracker{}
	h := newWriteHandler(trk, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"title":"Hello world"}`)
	resp, err := http.Post(srv.URL+"/api/v1/issues", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d want 201", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["issue_identifier"] != "MEM-1" {
		t.Errorf("identifier: got %v", out["issue_identifier"])
	}
	if trk.createdAs != "Hello world" {
		t.Errorf("createdAs: got %q", trk.createdAs)
	}
}

func TestCreateIssue_Unsupported(t *testing.T) {
	t.Parallel()
	trk := &fakeTracker{createErr: domain.ErrCreateUnsupported}
	h := newWriteHandler(trk, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"title":"x"}`)
	resp, err := http.Post(srv.URL+"/api/v1/issues", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", resp.StatusCode)
	}
}

func TestCreateIssue_BadRequest(t *testing.T) {
	t.Parallel()
	h := newWriteHandler(&fakeTracker{}, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/v1/issues", "application/json", strings.NewReader(`{"title":""}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
}

func TestSpecReadWrite_Roundtrip(t *testing.T) {
	t.Parallel()
	trk := &fakeTracker{specBody: "old", specEtag: "etag-old"}
	h := newWriteHandler(trk, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/issues/MEM-1/spec")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read status: got %d", resp.StatusCode)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["body"] != "old" {
		t.Errorf("body: got %v", got["body"])
	}

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/issues/MEM-1/spec",
		bytes.NewBufferString(`{"body":"new","if_match_etag":"etag-old"}`))
	req.Header.Set("Content-Type", "application/json")
	pr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer pr.Body.Close() //nolint:errcheck
	if pr.StatusCode != http.StatusOK {
		t.Fatalf("write status: got %d", pr.StatusCode)
	}
	if trk.writtenBody != "new" {
		t.Errorf("written body: got %q", trk.writtenBody)
	}

	// Stale etag → conflict.
	req2, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/issues/MEM-1/spec",
		bytes.NewBufferString(`{"body":"newer","if_match_etag":"etag-old"}`))
	req2.Header.Set("Content-Type", "application/json")
	pr2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("PUT2: %v", err)
	}
	defer pr2.Body.Close() //nolint:errcheck
	if pr2.StatusCode != http.StatusConflict {
		t.Fatalf("stale write status: got %d want 409", pr2.StatusCode)
	}
}
