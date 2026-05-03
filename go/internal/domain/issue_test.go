package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWorkspaceKey_Sanitization(t *testing.T) {
	tests := []struct {
		identifier string
		want       string
	}{
		{"ABC-123", "ABC-123"},
		{"ABC_123", "ABC_123"},
		{"ABC.123", "ABC.123"},
		{"ABC 123", "ABC_123"},
		{"ABC/123", "ABC_123"},
		{"ABC@#123", "ABC__123"},
		{"hello world", "hello_world"},
		{"", ""},
		{"ALL-CAPS-123", "ALL-CAPS-123"},
		{"mixedCASE.test-123", "mixedCASE.test-123"},
	}

	for _, tt := range tests {
		t.Run(tt.identifier, func(t *testing.T) {
			issue := Issue{Identifier: tt.identifier}
			got := issue.WorkspaceKey()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizedState_Lowercase(t *testing.T) {
	tests := []struct {
		state string
		want  string
	}{
		{"Todo", "todo"},
		{"In Progress", "in progress"},
		{"DONE", "done"},
		{"Closed", "closed"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			issue := Issue{State: tt.state}
			got := issue.NormalizedState()
			assert.Equal(t, tt.want, got)
		})
	}
}
