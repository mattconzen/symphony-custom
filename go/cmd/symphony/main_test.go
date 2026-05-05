package main

import (
	"path/filepath"
	"testing"
)

// TestDirOf_RegresssionFilepathDir is a regression test for the bug where
// dirOf was implemented manually instead of using filepath.Dir, causing
// incorrect results for edge cases (e.g., dirOf("/") returned "" instead
// of "/").
func TestDirOf_RegressionFilepathDir(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"WORKFLOW.md", "."},
		{"./WORKFLOW.md", "."},
		{"/abs/path/WORKFLOW.md", "/abs/path"},
		{"relative/WORKFLOW.md", "relative"},
		{"/", "/"},
		{".", "."},
		{"/a/b/c.md", "/a/b"},
	}

	for _, tt := range tests {
		got := dirOf(tt.input)
		want := filepath.Dir(tt.input) // ground truth
		if got != want {
			t.Errorf("dirOf(%q) = %q, want %q (filepath.Dir says %q)", tt.input, got, tt.want, want)
		}
	}
}
