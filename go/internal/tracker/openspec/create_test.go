package openspec

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

func newAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "changes"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Tracker.OpenSpec.Root = root
	cfg.Tracker.ActiveStates = []string{"Todo"}
	cfg.Tracker.TerminalStates = []string{"Done"}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a, root
}

func TestOpenSpec_CreateIssue_WritesScaffold(t *testing.T) {
	a, root := newAdapter(t)
	ctx := context.Background()
	iss, err := a.CreateIssue(ctx, domain.IssueDraft{Title: "My Big Feature", Description: "Make it work."})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if iss.Identifier != "my-big-feature" {
		t.Errorf("identifier: got %q want my-big-feature", iss.Identifier)
	}
	if _, err := os.Stat(filepath.Join(root, "changes", "my-big-feature", "proposal.md")); err != nil {
		t.Errorf("proposal.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "changes", "my-big-feature", "tasks.md")); err != nil {
		t.Errorf("tasks.md missing: %v", err)
	}
	has, _ := a.HasSpec(ctx, iss.Identifier)
	if !has {
		t.Error("HasSpec should be true after CreateIssue")
	}
}

func TestOpenSpec_ReadWriteSpec_Roundtrip(t *testing.T) {
	a, _ := newAdapter(t)
	ctx := context.Background()
	iss, _ := a.CreateIssue(ctx, domain.IssueDraft{Title: "Spec Roundtrip"})

	body, etag, err := a.ReadSpec(ctx, iss.Identifier)
	if err != nil {
		t.Fatalf("ReadSpec: %v", err)
	}
	if body == "" || etag == "" {
		t.Errorf("expected nonempty body/etag, got body=%q etag=%q", body, etag)
	}

	newBody := "# Replaced\n\nNew content."
	newEtag, err := a.WriteSpec(ctx, iss.Identifier, newBody, etag)
	if err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	if newEtag == etag {
		t.Errorf("etag should change after write")
	}

	if _, err := a.WriteSpec(ctx, iss.Identifier, "stale write", etag); err == nil {
		t.Error("expected ErrSpecConflict on stale etag")
	}
}

func TestOpenSpec_FetchAllIssues_ListsChangesAndArchive(t *testing.T) {
	a, root := newAdapter(t)
	ctx := context.Background()
	if _, err := a.CreateIssue(ctx, domain.IssueDraft{Title: "X One"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateIssue(ctx, domain.IssueDraft{Title: "Y Two"}); err != nil {
		t.Fatal(err)
	}
	// Move Y Two to archive by archiving its slug.
	if err := os.MkdirAll(filepath.Join(root, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(
		filepath.Join(root, "changes", "y-two"),
		filepath.Join(root, "archive", "y-two"),
	); err != nil {
		t.Fatal(err)
	}
	all, err := a.FetchAllIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(all))
	}
}
