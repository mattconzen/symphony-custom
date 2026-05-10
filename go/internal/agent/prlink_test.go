package agent

import (
	"sync"
	"testing"
)

func TestExtractPRLink_Match(t *testing.T) {
	cases := []struct {
		in     string
		number int
		owner  string
		repo   string
	}{
		{"opened https://github.com/foo/bar/pull/42", 42, "foo", "bar"},
		{"see https://github.com/owner-1/repo_2/pull/9999", 9999, "owner-1", "repo_2"},
		{"PR available at https://github.com/a/b/pull/1 and other text", 1, "a", "b"},
	}
	for _, c := range cases {
		got, ok := ExtractPRLink(c.in)
		if !ok {
			t.Fatalf("expected match for %q", c.in)
		}
		if got.Number != c.number || got.Owner != c.owner || got.Repo != c.repo {
			t.Errorf("got %+v want owner=%s repo=%s number=%d", got, c.owner, c.repo, c.number)
		}
	}
}

func TestExtractPRLink_NoMatch(t *testing.T) {
	cases := []string{
		"no link here",
		"https://github.com/foo/bar/issues/42",
		"https://example.com/foo/bar/pull/42",
	}
	for _, c := range cases {
		if _, ok := ExtractPRLink(c); ok {
			t.Errorf("unexpected match for %q", c)
		}
	}
}

func TestPRLinkEmitter_DedupesPerSession(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	cb := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		if pl, ok := ev.Payload.(PRLinkPayload); ok {
			seen = append(seen, pl.URL)
		}
	}
	e := NewPRLinkEmitter(cb)
	e.Scan("sess", "open https://github.com/o/r/pull/1")
	e.Scan("sess", "still https://github.com/o/r/pull/1")
	e.Scan("sess", "and https://github.com/o/r/pull/2")
	if len(seen) != 2 {
		t.Fatalf("seen=%v want 2 unique", seen)
	}
}
