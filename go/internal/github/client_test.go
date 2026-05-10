package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetPullRequest_OpenAndMerged(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
	}{
		{
			"open",
			`{"number": 42, "state": "open", "merged": false, "html_url": "https://github.com/foo/bar/pull/42"}`,
			"open",
		},
		{
			"merged",
			`{"number": 42, "state": "closed", "merged": true, "merged_at": "2026-05-09T11:00:00Z", "html_url": "https://github.com/foo/bar/pull/42"}`,
			"merged",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/foo/bar/pulls/42" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			cli := New("dummy-token")
			cli.BaseURL = srv.URL
			pr, err := cli.GetPullRequest(context.Background(), "foo", "bar", 42)
			if err != nil {
				t.Fatalf("GetPullRequest: %v", err)
			}
			if pr.State != c.want {
				t.Errorf("state: got %q want %q", pr.State, c.want)
			}
			if pr.Source != "github_poll" {
				t.Errorf("source: got %q want github_poll", pr.Source)
			}
		})
	}
}

func TestGetPullRequest_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	cli := New("")
	cli.BaseURL = srv.URL
	if _, err := cli.GetPullRequest(context.Background(), "foo", "bar", 99); err == nil {
		t.Fatal("expected error on 404")
	}
}
