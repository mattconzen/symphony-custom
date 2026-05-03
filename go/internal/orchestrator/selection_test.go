package orchestrator

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
)

func makeCfg(maxAgents int, byState map[string]int) config.Config {
	cfg := config.Config{}
	cfg.Agent.MaxConcurrentAgents = maxAgents
	cfg.Agent.MaxConcurrentAgentsByState = byState
	cfg.Tracker.TerminalStates = []string{"Done", "Cancelled", "Closed"}
	return cfg
}

func ptr(i int) *int { return &i }

func TestChooseIssues_PrioritySort(t *testing.T) {
	p1 := ptr(1)
	p2 := ptr(2)
	p3 := ptr(3)
	candidates := []domain.Issue{
		{ID: "c", Priority: p3, State: "In Progress"},
		{ID: "a", Priority: p1, State: "In Progress"},
		{ID: "b", Priority: p2, State: "In Progress"},
	}
	cfg := makeCfg(10, nil)
	chosen := chooseIssues(candidates, nil, nil, cfg)
	require.Len(t, chosen, 3)
	assert.Equal(t, "a", chosen[0].ID)
	assert.Equal(t, "b", chosen[1].ID)
	assert.Equal(t, "c", chosen[2].ID)
}

func TestChooseIssues_CreatedAtTieBreak(t *testing.T) {
	earlier := time.Now().Add(-1 * time.Hour)
	later := time.Now()
	p := ptr(1)
	candidates := []domain.Issue{
		{ID: "later", Priority: p, State: "In Progress", CreatedAt: &later},
		{ID: "earlier", Priority: p, State: "In Progress", CreatedAt: &earlier},
	}
	cfg := makeCfg(10, nil)
	chosen := chooseIssues(candidates, nil, nil, cfg)
	require.Len(t, chosen, 2)
	assert.Equal(t, "earlier", chosen[0].ID)
	assert.Equal(t, "later", chosen[1].ID)
}

func TestChooseIssues_SkipsAlreadyClaimed(t *testing.T) {
	candidates := []domain.Issue{
		{ID: "a", State: "In Progress"},
		{ID: "b", State: "In Progress"},
	}
	claimed := map[string]struct{}{"a": {}}
	cfg := makeCfg(10, nil)
	chosen := chooseIssues(candidates, claimed, nil, cfg)
	require.Len(t, chosen, 1)
	assert.Equal(t, "b", chosen[0].ID)
}

func TestChooseIssues_SkipsTerminalStates(t *testing.T) {
	candidates := []domain.Issue{
		{ID: "a", State: "Done"},
		{ID: "b", State: "In Progress"},
	}
	cfg := makeCfg(10, nil)
	chosen := chooseIssues(candidates, nil, nil, cfg)
	require.Len(t, chosen, 1)
	assert.Equal(t, "b", chosen[0].ID)
}

func TestChooseIssues_GlobalConcurrencyLimit(t *testing.T) {
	candidates := []domain.Issue{
		{ID: "a", State: "In Progress"},
		{ID: "b", State: "In Progress"},
		{ID: "c", State: "In Progress"},
	}
	cfg := makeCfg(2, nil) // max 2 agents
	// 1 already running
	runningByState := map[string]int{"in progress": 1}
	chosen := chooseIssues(candidates, nil, runningByState, cfg)
	// globalSlots = 2 - 1 = 1
	assert.Len(t, chosen, 1)
}

func TestChooseIssues_GlobalLimitZeroSlots(t *testing.T) {
	candidates := []domain.Issue{
		{ID: "a", State: "In Progress"},
	}
	cfg := makeCfg(2, nil)
	runningByState := map[string]int{"in progress": 2}
	chosen := chooseIssues(candidates, nil, runningByState, cfg)
	assert.Empty(t, chosen)
}

func TestChooseIssues_PerStateLimit(t *testing.T) {
	candidates := []domain.Issue{
		{ID: "a", State: "In Progress"},
		{ID: "b", State: "In Progress"},
		{ID: "c", State: "Todo"},
	}
	byState := map[string]int{"in progress": 1}
	cfg := makeCfg(10, byState)
	// 1 already running in "in progress"
	runningByState := map[string]int{"in progress": 1}
	chosen := chooseIssues(candidates, nil, runningByState, cfg)

	// "in progress" at limit; only "todo" should be chosen.
	require.Len(t, chosen, 1)
	assert.Equal(t, "c", chosen[0].ID)
}

func TestChooseIssues_NilPriorityLastInSort(t *testing.T) {
	p1 := ptr(1)
	candidates := []domain.Issue{
		{ID: "nil-priority", State: "In Progress"}, // nil priority
		{ID: "prio-1", Priority: p1, State: "In Progress"},
	}
	cfg := makeCfg(10, nil)
	chosen := chooseIssues(candidates, nil, nil, cfg)
	require.Len(t, chosen, 2)
	// prio-1 should come first, nil-priority last.
	assert.Equal(t, "prio-1", chosen[0].ID)
	assert.Equal(t, "nil-priority", chosen[1].ID)
}
