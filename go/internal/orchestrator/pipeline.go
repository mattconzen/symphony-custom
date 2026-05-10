package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/prompt"
	"github.com/openai/symphony/go/internal/transcript"
)

// dispatchPipeline runs an issue through the configured sequential
// agent.pipeline. Each role gets the existing turn-loop with its own
// runtime + max_turns. Roles complete on ready_artifact existence;
// failure schedules a retry of the failing role (not the whole
// pipeline). on_artifact loopbacks let a reviewer rewind to a prior
// role up to max_loopbacks times.
func (o *Orchestrator) dispatchPipeline(ctx context.Context, issue domain.Issue) {
	log := o.log.WithIssue(issue)
	log.Info("pipeline dispatch started", "roles", roleNames(o.cfg.Agent.Pipeline))

	var tw *transcript.Writer
	if path := o.transcriptPath(issue); path != "" {
		var err error
		tw, err = transcript.NewWriter(path)
		if err != nil {
			log.Warn("transcript writer init failed", "err", err.Error())
			tw = nil
		}
	}

	defer func() {
		if tw != nil {
			_ = tw.Close() //nolint:errcheck
		}
		o.releaseClaim(issue.ID)
		log.Info("pipeline dispatch finished")
	}()

	ws, err := o.ws.EnsureForIssue(ctx, issue)
	if err != nil {
		log.Error("workspace ensure failed", "err", fmt.Sprintf("%v", err))
		o.scheduleRetry(issue, err)
		return
	}

	// Load any durable pipeline progress from a prior dispatch so we can
	// resume at the recorded role with the loopback counters preserved
	// across the releaseClaim → scheduleRetry → re-dispatch cycle (T4).
	loaded := o.loadPipelineProgress(issue.ID)

	o.mu.Lock()
	if entry, ok := o.running[issue.ID]; ok {
		entry.workspacePath = ws.Path
		if entry.pipeline == nil {
			entry.pipeline = &domain.PipelineProgress{
				Loopbacks: make(map[string]int),
				Artifacts: make(map[string]string),
			}
		}
		if loaded != nil {
			// Overlay durable progress onto the freshly-created entry.
			if loaded.CurrentRole != "" {
				entry.pipeline.CurrentRole = loaded.CurrentRole
			}
			if len(loaded.CompletedRoles) > 0 {
				entry.pipeline.CompletedRoles = append([]string(nil), loaded.CompletedRoles...)
			}
			if len(loaded.Loopbacks) > 0 {
				for k, v := range loaded.Loopbacks {
					entry.pipeline.Loopbacks[k] = v
				}
			}
			if len(loaded.Artifacts) > 0 {
				for k, v := range loaded.Artifacts {
					entry.pipeline.Artifacts[k] = v
				}
			}
		}
	}
	o.mu.Unlock()

	roles := o.cfg.Agent.Pipeline
	idx := 0
	if entry, ok := o.runEntryForIssue(issue.ID); ok && entry.pipeline != nil && entry.pipeline.CurrentRole != "" {
		// Resume support: pick up at the recorded current role.
		if i := findRoleIndex(roles, entry.pipeline.CurrentRole); i >= 0 {
			idx = i
		}
	}

	for idx < len(roles) {
		role := roles[idx]
		rt := o.roleRuntimes[role.Role]
		if rt == nil {
			err := fmt.Errorf("pipeline: no runtime for role %q (build failed at startup)", role.Role)
			log.Error("pipeline: missing runtime", "role", role.Role)
			o.scheduleRetry(issue, err)
			return
		}

		o.recordPipelineProgress(issue.ID, role.Role, nil)

		runErr, completed := o.runPipelineRole(ctx, issue, ws, role, rt, log, tw)
		if runErr != nil {
			if ctx.Err() == nil && !o.cancelRequested(issue.ID) {
				o.scheduleRetry(issue, fmt.Errorf("pipeline role %q: %w", role.Role, runErr))
			}
			return
		}
		if !completed {
			err := fmt.Errorf("pipeline role %q: ready_artifact %q missing after max_turns", role.Role, role.ReadyArtifact)
			log.Warn("pipeline: ready_artifact missing, scheduling retry", "role", role.Role)
			if !o.cancelRequested(issue.ID) {
				o.scheduleRetry(issue, err)
			}
			return
		}

		o.recordPipelineProgress(issue.ID, "", &role)

		// Loopback check: did the agent write any of the on_artifact paths?
		if loopback, target := o.detectLoopback(ws, role); loopback {
			loopIdx := findRoleIndex(roles, target.RetryFrom)
			if loopIdx < 0 {
				err := fmt.Errorf("pipeline role %q: on_artifact target %q is not a known role", role.Role, target.RetryFrom)
				log.Error("pipeline: loopback target unknown", "role", role.Role, "target", target.RetryFrom)
				o.scheduleRetry(issue, err)
				return
			}
			if !o.bumpLoopback(issue.ID, target.RetryFrom, target.MaxLoopbacks) {
				err := fmt.Errorf("pipeline: max_loopbacks (%d) reached for role %q", target.MaxLoopbacks, target.RetryFrom)
				log.Warn("pipeline: max_loopbacks reached", "target", target.RetryFrom)
				o.scheduleRetry(issue, err)
				return
			}
			log.Info("pipeline: loopback fired", "from", role.Role, "to", target.RetryFrom)
			idx = loopIdx
			// Honour pause/cancel between roles even on loopback (T3).
			if cancelled := o.observePauseOrCancel(ctx, issue.ID, log, 0); cancelled {
				return
			}
			continue
		}

		idx++
		// Pause/Cancel observation between roles (T3). Pause in pipeline
		// mode is "between roles" by analogy with the single-role "between
		// turns" semantics. Block while paused; return on cancel.
		if idx < len(roles) {
			if cancelled := o.observePauseOrCancel(ctx, issue.ID, log, 0); cancelled {
				return
			}
		}
	}

	log.Info("pipeline: all roles complete")
	// Clear the durable progress record on terminal completion (T4) so a
	// re-dispatch of the same issue starts from role 0.
	o.clearPipelineProgress(issue.ID)
}

// runPipelineRole runs the existing turn-loop for one role. Returns
// (err, completed). When completed=true, the ready_artifact exists; when
// false, max_turns was reached without producing the artifact.
func (o *Orchestrator) runPipelineRole(
	ctx context.Context,
	issue domain.Issue,
	ws domain.Workspace,
	role config.PipelineRole,
	rt agent.Runtime,
	log *observability.Logger,
	tw *transcript.Writer,
) (error, bool) {
	roleCtx := ctx
	if role.TimeoutMs > 0 {
		var cancel context.CancelFunc
		roleCtx, cancel = context.WithTimeout(ctx, time.Duration(role.TimeoutMs)*time.Millisecond)
		defer cancel()
	}

	sess, err := rt.StartSession(roleCtx, ws)
	if err != nil {
		return err, false
	}
	defer rt.StopSession(roleCtx, sess) //nolint:errcheck

	maxTurns := role.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 1
	}

	prevArtifacts := o.collectPriorArtifacts(issue.ID, ws.Path)

	for turn := 1; turn <= maxTurns; turn++ {
		if roleCtx.Err() != nil {
			return roleCtx.Err(), false
		}

		p, perr := renderRolePrompt(role.PromptTemplate, issue, prevArtifacts)
		if perr != nil {
			return perr, false
		}

		currentTurn := turn
		currentRole := role.Role
		cb := func(ev agent.Event) {
			tev := transcript.Event{
				Ts:              ev.Timestamp,
				IssueID:         issue.ID,
				IssueIdentifier: issue.Identifier,
				SessionID:       sess.ID,
				Role:            currentRole,
				Turn:            currentTurn,
				Kind:            string(ev.Kind),
				Payload:         ev.Payload,
			}
			if tw != nil {
				_ = tw.Append(tev) //nolint:errcheck
			}
			if o.transcriptBus != nil {
				o.transcriptBus.Publish(tev)
			}
		}

		result, runErr := rt.RunTurn(roleCtx, sess, p, issue, cb)
		if runErr != nil {
			return runErr, false
		}
		o.recordTurnComplete(issue.ID, turn, result)
		o.notify()

		if result.Status == agent.TurnFailed {
			return fmt.Errorf("turn %d failed: %w", turn, result.Err), false
		}

		// Role completes as soon as the ready_artifact appears, even
		// before max_turns is reached.
		if artifactExists(ws.Path, role.ReadyArtifact) {
			o.recordArtifact(issue.ID, role.Role, role.ReadyArtifact)
			return nil, true
		}

		if result.Status == agent.TurnCancelled {
			return roleCtx.Err(), false
		}
	}

	// Max turns reached without the artifact.
	return nil, false
}

func renderRolePrompt(tmpl string, issue domain.Issue, artifacts map[string]string) (string, error) {
	vars := prompt.Vars{Issue: issue, Artifacts: artifacts}
	return prompt.Render(tmpl, vars)
}

// artifactExists reports whether the path exists relative to workspace.
func artifactExists(workspace, rel string) bool {
	if rel == "" {
		return false
	}
	abs := filepath.Join(workspace, rel)
	_, err := os.Stat(abs)
	return err == nil
}

// detectLoopback walks the role's on_artifact map and returns the first
// matching loopback target.
func (o *Orchestrator) detectLoopback(ws domain.Workspace, role config.PipelineRole) (bool, config.PipelineLoopback) {
	for rel, lb := range role.OnArtifact {
		if artifactExists(ws.Path, rel) {
			return true, lb
		}
	}
	return false, config.PipelineLoopback{}
}

// bumpLoopback increments the loopback counter for target and returns
// false if max_loopbacks would be exceeded.
//
// T21: max_loopbacks=0 now means "no loopbacks allowed" (rejected at
// preflight if negative). Treat any non-positive value as zero here so
// the first attempt is refused — matches user expectation that 0
// disables loopbacks rather than silently coercing to 1.
func (o *Orchestrator) bumpLoopback(issueID, target string, max int) bool {
	o.mu.Lock()
	entry, ok := o.running[issueID]
	if !ok || entry.pipeline == nil {
		o.mu.Unlock()
		return false
	}
	if entry.pipeline.Loopbacks == nil {
		entry.pipeline.Loopbacks = make(map[string]int)
	}
	if max < 0 {
		max = 0
	}
	if entry.pipeline.Loopbacks[target] >= max {
		o.mu.Unlock()
		return false
	}
	entry.pipeline.Loopbacks[target]++
	snapshot := clonePipelineProgress(entry.pipeline)
	o.mu.Unlock()
	o.persistPipelineProgress(issueID, snapshot)
	return true
}

// recordPipelineProgress updates CurrentRole and CompletedRoles. When
// completed is non-nil, the role is appended to CompletedRoles.
func (o *Orchestrator) recordPipelineProgress(issueID, currentRole string, completed *config.PipelineRole) {
	o.mu.Lock()
	entry, ok := o.running[issueID]
	if !ok {
		o.mu.Unlock()
		return
	}
	if entry.pipeline == nil {
		entry.pipeline = &domain.PipelineProgress{
			Loopbacks: make(map[string]int),
			Artifacts: make(map[string]string),
		}
	}
	entry.pipeline.CurrentRole = currentRole
	if completed != nil {
		// Append unless this role is already the last entry (avoids
		// duplicates after a loopback that re-completes the same role).
		if len(entry.pipeline.CompletedRoles) == 0 || entry.pipeline.CompletedRoles[len(entry.pipeline.CompletedRoles)-1] != completed.Role {
			entry.pipeline.CompletedRoles = append(entry.pipeline.CompletedRoles, completed.Role)
		}
	}
	snapshot := clonePipelineProgress(entry.pipeline)
	o.mu.Unlock()
	o.persistPipelineProgress(issueID, snapshot)
	o.notify()
}

// recordArtifact stores the absolute artifact path for the named role.
func (o *Orchestrator) recordArtifact(issueID, role, path string) {
	o.mu.Lock()
	entry, ok := o.running[issueID]
	if !ok || entry.pipeline == nil {
		o.mu.Unlock()
		return
	}
	if entry.pipeline.Artifacts == nil {
		entry.pipeline.Artifacts = make(map[string]string)
	}
	entry.pipeline.Artifacts[role] = path
	snapshot := clonePipelineProgress(entry.pipeline)
	o.mu.Unlock()
	o.persistPipelineProgress(issueID, snapshot)
}

// collectPriorArtifacts reads each previously-completed role's artifact
// content from the workspace and returns a map keyed by role name. Used
// to feed `{{ artifacts.planner }}` etc. into prompt templates.
func (o *Orchestrator) collectPriorArtifacts(issueID, workspace string) map[string]string {
	o.mu.Lock()
	entry, ok := o.running[issueID]
	if !ok || entry.pipeline == nil {
		o.mu.Unlock()
		return nil
	}
	completedCopy := append([]string(nil), entry.pipeline.CompletedRoles...)
	artifactsCopy := make(map[string]string, len(entry.pipeline.Artifacts))
	for k, v := range entry.pipeline.Artifacts {
		artifactsCopy[k] = v
	}
	o.mu.Unlock()

	out := make(map[string]string, len(completedCopy))
	for _, role := range completedCopy {
		rel := artifactsCopy[role]
		if rel == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(workspace, rel)) //nolint:gosec
		if err != nil {
			continue
		}
		out[role] = string(body)
	}
	return out
}

// runEntryForIssue is a small lock-respecting accessor used by
// dispatchPipeline at the top of the function (read-only).
func (o *Orchestrator) runEntryForIssue(issueID string) (*runEntry, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, ok := o.running[issueID]
	return e, ok
}

func roleNames(roles []config.PipelineRole) string {
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, r.Role)
	}
	return strings.Join(names, "→")
}

// findRoleIndex returns the position of name in roles, or -1 if missing.
func findRoleIndex(roles []config.PipelineRole, name string) int {
	for i, r := range roles {
		if r.Role == name {
			return i
		}
	}
	return -1
}

// pipelineProgressKey returns the durable.Store name for one issue's
// pipeline progress record. Slash-delimited so files sit under a
// "pipeline_progress/" subdirectory.
func pipelineProgressKey(issueID string) string {
	return "pipeline_progress/" + issueID
}

// loadPipelineProgress reads the on-disk pipeline progress for issueID
// (T4). Returns nil when durable is disabled, when the file is missing,
// or on read error. Errors are logged at warn level; the caller falls
// back to today's per-dispatch behaviour.
func (o *Orchestrator) loadPipelineProgress(issueID string) *domain.PipelineProgress {
	if o.durable == nil {
		return nil
	}
	var p domain.PipelineProgress
	err := o.durable.Load(pipelineProgressKey(issueID), &p)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			o.log.Warn("pipeline progress load failed", "issue_id", issueID, "err", err.Error())
		}
		return nil
	}
	return &p
}

// persistPipelineProgress writes the snapshot to durable storage (T4).
// When durable is disabled or save fails, the call is best-effort: the
// in-memory entry continues to drive the run. A single info log notes
// the disabled case once per dispatch.
func (o *Orchestrator) persistPipelineProgress(issueID string, p *domain.PipelineProgress) {
	if o.durable == nil {
		// Match the brief documented in T4: log once per call site is
		// noisy; emit at debug level only to keep logs clean.
		o.log.Debug("pipeline progress not durable: durable disabled", "issue_id", issueID)
		return
	}
	if p == nil {
		return
	}
	if err := o.durable.Save(pipelineProgressKey(issueID), p); err != nil {
		o.log.Warn("pipeline progress save failed", "issue_id", issueID, "err", err.Error())
	}
}

// clearPipelineProgress deletes the durable record for issueID (T4).
// Called on terminal pipeline completion. Best-effort.
func (o *Orchestrator) clearPipelineProgress(issueID string) {
	if o.durable == nil {
		return
	}
	if err := o.durable.Delete(pipelineProgressKey(issueID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		o.log.Warn("pipeline progress delete failed", "issue_id", issueID, "err", err.Error())
	}
}

// clonePipelineProgress deep-copies progress so callers can pass it
// outside o.mu without later mutation interfering with persistence.
func clonePipelineProgress(p *domain.PipelineProgress) *domain.PipelineProgress {
	if p == nil {
		return nil
	}
	out := &domain.PipelineProgress{
		CurrentRole:    p.CurrentRole,
		CompletedRoles: append([]string(nil), p.CompletedRoles...),
	}
	if p.Loopbacks != nil {
		out.Loopbacks = make(map[string]int, len(p.Loopbacks))
		for k, v := range p.Loopbacks {
			out.Loopbacks[k] = v
		}
	}
	if p.Artifacts != nil {
		out.Artifacts = make(map[string]string, len(p.Artifacts))
		for k, v := range p.Artifacts {
			out.Artifacts[k] = v
		}
	}
	return out
}
