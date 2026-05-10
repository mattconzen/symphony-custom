package memory

import (
	"context"
	"testing"

	"github.com/openai/symphony/go/internal/domain"
)

func TestMemoryTracker_CreateIssue_AppendsAndAssignsID(t *testing.T) {
	tr := New(nil)
	ctx := context.Background()
	got, err := tr.CreateIssue(ctx, domain.IssueDraft{Title: "Hello", Description: "World"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if got.Identifier == "" || got.Identifier != got.ID {
		t.Errorf("expected nonempty identifier == ID, got id=%q ident=%q", got.ID, got.Identifier)
	}
	if got.Title != "Hello" {
		t.Errorf("title: got %q want Hello", got.Title)
	}

	all, err := tr.FetchAllIssues(ctx)
	if err != nil {
		t.Fatalf("FetchAllIssues: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(all))
	}
}

func TestMemoryTracker_HasSpec_AfterWrite(t *testing.T) {
	tr := New(nil)
	ctx := context.Background()
	got, _ := tr.CreateIssue(ctx, domain.IssueDraft{Title: "X"})

	has, err := tr.HasSpec(ctx, got.Identifier)
	if err != nil {
		t.Fatalf("HasSpec: %v", err)
	}
	if has {
		t.Fatal("expected HasSpec=false initially")
	}
	if _, err := tr.WriteSpec(ctx, got.Identifier, "# spec body", ""); err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	has, _ = tr.HasSpec(ctx, got.Identifier)
	if !has {
		t.Fatal("expected HasSpec=true after WriteSpec")
	}
	body, etag, err := tr.ReadSpec(ctx, got.Identifier)
	if err != nil {
		t.Fatalf("ReadSpec: %v", err)
	}
	if body != "# spec body" {
		t.Errorf("body: got %q", body)
	}
	if etag == "" {
		t.Errorf("expected nonempty etag")
	}
}

func TestMemoryTracker_SetPullRequest(t *testing.T) {
	tr := New(nil)
	ctx := context.Background()
	got, _ := tr.CreateIssue(ctx, domain.IssueDraft{Title: "A"})
	pr := domain.PullRequest{URL: "https://github.com/o/r/pull/1", Number: 1, State: "open"}
	if err := tr.SetPullRequest(ctx, got.Identifier, pr); err != nil {
		t.Fatalf("SetPullRequest: %v", err)
	}
	all, _ := tr.FetchAllIssues(ctx)
	if len(all) != 1 || all[0].PR == nil || all[0].PR.Number != 1 {
		t.Errorf("expected PR set on issue, got %+v", all[0].PR)
	}
}
