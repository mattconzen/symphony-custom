package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openai/symphony/go/internal/config"
)

func TestComputeBackoffMs_Attempts(t *testing.T) {
	cfg := config.Config{}
	cfg.Agent.MaxRetryBackoffMs = 300_000

	tests := []struct {
		attempt    int
		wantMs     int64
		wantCapped bool
	}{
		{1, 5_000, false},
		{2, 10_000, false},
		{3, 20_000, false},
		{4, 40_000, false},
		{5, 80_000, false},
		{6, 160_000, false},
		{7, 300_000, true}, // 320_000 > 300_000 → capped
		{10, 300_000, true},
	}

	for _, tt := range tests {
		got := computeBackoffMs(tt.attempt, cfg)
		if tt.wantCapped {
			assert.Equal(t, int64(300_000), got, "attempt %d should be capped at maxRetryBackoffMs", tt.attempt)
		} else {
			assert.Equal(t, tt.wantMs, got, "attempt %d backoff mismatch", tt.attempt)
		}
	}
}

func TestComputeBackoffMs_ZeroMax(t *testing.T) {
	cfg := config.Config{}
	cfg.Agent.MaxRetryBackoffMs = 0 // uses default 300_000
	got := computeBackoffMs(1, cfg)
	assert.Equal(t, int64(5_000), got)
}

func TestComputeBackoffMs_SmallMax(t *testing.T) {
	cfg := config.Config{}
	cfg.Agent.MaxRetryBackoffMs = 1_000
	got := computeBackoffMs(1, cfg)
	assert.Equal(t, int64(1_000), got) // base 5000 > max 1000 → capped at max
}

func TestComputeBackoffMs_AttemptZero(t *testing.T) {
	cfg := config.Config{}
	cfg.Agent.MaxRetryBackoffMs = 300_000
	// attempt=0 → shift=-1 clamped to 0 → same as attempt=1
	got := computeBackoffMs(0, cfg)
	assert.Equal(t, int64(5_000), got)
}
