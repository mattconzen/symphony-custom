package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/openai/symphony/go/internal/domain"
)

// prFetcher is the minimal contract the PR reconciler depends on. It
// matches *github.Client.GetPullRequest so tests can substitute a fake.
type prFetcher interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (domain.PullRequest, error)
}

// runPRReconciler periodically refreshes the PR state for every issue with a
// known PR. Stops when ctx is cancelled. interval==0 disables the loop.
func (o *Orchestrator) runPRReconciler(ctx context.Context, fetcher prFetcher, interval time.Duration) {
	if fetcher == nil || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.reconcilePRs(ctx, fetcher)
		}
	}
}

// reconcilePRs walks the in-memory PR map and re-fetches state for every
// non-merged PR. Merged PRs are not re-fetched; their state is final.
func (o *Orchestrator) reconcilePRs(ctx context.Context, fetcher prFetcher) {
	o.mu.Lock()
	type job struct {
		identifier string
		owner      string
		repo       string
		number     int
	}
	var jobs []job
	for id, pr := range o.pullRequests {
		if pr.State == "merged" {
			continue
		}
		jobs = append(jobs, job{id, pr.Owner, pr.Repo, pr.Number})
	}
	o.mu.Unlock()

	for _, j := range jobs {
		updated, err := fetcher.GetPullRequest(ctx, j.owner, j.repo, j.number)
		if err != nil {
			o.log.Warn("pr_reconciler: fetch failed",
				"identifier", j.identifier,
				"owner", j.owner, "repo", j.repo, "number", j.number,
				"err", fmt.Sprintf("%v", err),
			)
			continue
		}
		o.SetPullRequest(ctx, j.identifier, updated)
	}
}
