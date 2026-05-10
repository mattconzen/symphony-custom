package web

import (
	"testing"

	"github.com/openai/symphony/go/internal/observability"
)

func TestFormatNextPoll(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   observability.Polling
		want string
	}{
		{
			name: "checking",
			in:   observability.Polling{Checking: true},
			want: "Checking…",
		},
		{
			name: "checking_overrides_next",
			in:   observability.Polling{Checking: true, NextPollInMs: 1500},
			want: "Checking…",
		},
		{
			name: "any_moment_zero",
			in:   observability.Polling{NextPollInMs: 0},
			want: "any moment",
		},
		{
			name: "any_moment_negative",
			in:   observability.Polling{NextPollInMs: -200},
			want: "any moment",
		},
		{
			name: "sub_minute",
			in:   observability.Polling{NextPollInMs: 1500},
			want: "1.5s",
		},
		{
			name: "exactly_one_minute",
			in:   observability.Polling{NextPollInMs: 60_000},
			want: "1m",
		},
		{
			name: "minutes_and_seconds",
			in:   observability.Polling{NextPollInMs: 65_000},
			want: "1m 5s",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := formatNextPoll(tc.in)
			if got != tc.want {
				t.Errorf("formatNextPoll(%+v): got %q want %q", tc.in, got, tc.want)
			}
		})
	}
}
