// Package config provides typed configuration derived from WORKFLOW.md front
// matter per SPEC §6.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/workflow"
)

// Sentinel errors for preflight validation.
var (
	ErrMissingTrackerKind          = errors.New("tracker.kind is required")
	ErrInvalidAgentRuntime         = errors.New("agent.runtime must be one of: codex, claude, mock")
	ErrBetweenTurnsRequiresTimeout = errors.New("hooks.timeout_ms must be > 0 when hooks.between_turns is set")
	ErrUnsupportedTrackerKind      = errors.New("unsupported tracker.kind")
)

// TrackerMarkdown holds config for tracker.kind == "markdown".
type TrackerMarkdown struct {
	Root string
}

// TrackerOpenSpec holds config for tracker.kind == "openspec".
type TrackerOpenSpec struct {
	Root string
}

// TrackerJIRA holds config for tracker.kind == "jira".
type TrackerJIRA struct {
	Endpoint    string
	Email       string
	APIToken    string
	ProjectKey  string
	JQLOverride string
}

// Tracker holds tracker configuration per SPEC §5.3.1.
type Tracker struct {
	Kind           string
	Endpoint       string
	APIKey         string
	ProjectSlug    string
	ActiveStates   []string
	TerminalStates []string
	Markdown       TrackerMarkdown
	OpenSpec       TrackerOpenSpec
	JIRA           TrackerJIRA
}

// Polling holds polling configuration per SPEC §5.3.2.
type Polling struct {
	IntervalMs int
}

// Workspace holds workspace configuration per SPEC §5.3.3.
type Workspace struct {
	Root string
}

// Hooks holds hook configuration per SPEC §5.3.4.
type Hooks struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	BetweenTurns string
	TimeoutMs    int
}

// Agent holds agent configuration per SPEC §5.3.5.
type Agent struct {
	Runtime                    string
	MaxConcurrentAgents        int
	MaxTurns                   int
	MaxRetryBackoffMs          int
	MaxConcurrentAgentsByState map[string]int
}

// Codex holds codex-specific configuration per SPEC §5.3.6.
type Codex struct {
	Command           string
	ApprovalPolicy    string
	ThreadSandbox     string
	TurnSandboxPolicy string
	TurnTimeoutMs     int
	ReadTimeoutMs     int
	StallTimeoutMs    int
}

// Claude holds claude-specific configuration per SPEC §5.3.7.
type Claude struct {
	Command        string
	Model          string
	PermissionMode string
	TurnTimeoutMs  int
	ReadTimeoutMs  int
	StallTimeoutMs int
}

// Config is the fully resolved typed configuration per SPEC §4.1.3.
type Config struct {
	Tracker   Tracker
	Polling   Polling
	Workspace Workspace
	Hooks     Hooks
	Agent     Agent
	Codex     Codex
	Claude    Claude
}

// Resolve builds a Config from a workflow definition and the directory containing
// the workflow file. It applies defaults, expands env vars on path/secret fields,
// and normalizes paths to absolute per SPEC §6.1.
func Resolve(wf domain.Workflow, workflowDir string) (Config, error) {
	cfg := Config{}

	// ---- Tracker ----
	if t, ok := wf.Config["tracker"]; ok {
		if tm, ok := t.(map[string]any); ok {
			cfg.Tracker.Kind = strVal(tm, "kind")
			cfg.Tracker.Endpoint = strVal(tm, "endpoint")
			cfg.Tracker.APIKey = resolveVar(strVal(tm, "api_key"))
			cfg.Tracker.ProjectSlug = strVal(tm, "project_slug")
			cfg.Tracker.ActiveStates = strSlice(tm, "active_states")
			cfg.Tracker.TerminalStates = strSlice(tm, "terminal_states")

			// Markdown sub-config
			if md, ok := tm["markdown"]; ok {
				if mdm, ok := md.(map[string]any); ok {
					root := strVal(mdm, "root")
					cfg.Tracker.Markdown.Root = resolvePathField(root, workflowDir)
				}
			}
			// OpenSpec sub-config
			if os2, ok := tm["openspec"]; ok {
				if osm, ok := os2.(map[string]any); ok {
					root := strVal(osm, "root")
					cfg.Tracker.OpenSpec.Root = resolvePathField(root, workflowDir)
				}
			}
			// JIRA sub-config
			if jira, ok := tm["jira"]; ok {
				if jm, ok := jira.(map[string]any); ok {
					cfg.Tracker.JIRA.Endpoint = strVal(jm, "endpoint")
					cfg.Tracker.JIRA.Email = resolveVar(strVal(jm, "email"))
					cfg.Tracker.JIRA.APIToken = resolveVar(strVal(jm, "api_token"))
					cfg.Tracker.JIRA.ProjectKey = strVal(jm, "project_key")
					cfg.Tracker.JIRA.JQLOverride = strVal(jm, "jql_override")
				}
			}
		}
	}

	// Tracker defaults
	if len(cfg.Tracker.ActiveStates) == 0 {
		cfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	}
	if len(cfg.Tracker.TerminalStates) == 0 {
		cfg.Tracker.TerminalStates = []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}
	}
	if cfg.Tracker.Endpoint == "" && cfg.Tracker.Kind == "linear" {
		cfg.Tracker.Endpoint = "https://api.linear.app/graphql"
	}
	if cfg.Tracker.OpenSpec.Root == "" {
		cfg.Tracker.OpenSpec.Root = resolvePathField("openspec", workflowDir)
	}

	// ---- Polling ----
	if p, ok := wf.Config["polling"]; ok {
		if pm, ok := p.(map[string]any); ok {
			if v := intVal(pm, "interval_ms"); v > 0 {
				cfg.Polling.IntervalMs = v
			}
		}
	}
	if cfg.Polling.IntervalMs == 0 {
		cfg.Polling.IntervalMs = 30000
	}

	// ---- Workspace ----
	wsRoot := ""
	if ws, ok := wf.Config["workspace"]; ok {
		if wsm, ok := ws.(map[string]any); ok {
			wsRoot = strVal(wsm, "root")
		}
	}
	if wsRoot == "" {
		cfg.Workspace.Root = filepath.Join(os.TempDir(), "symphony_workspaces")
	} else {
		wsRoot = expandHome(resolveVar(wsRoot))
		if !filepath.IsAbs(wsRoot) {
			wsRoot = filepath.Join(workflowDir, wsRoot)
		}
		cfg.Workspace.Root = filepath.Clean(wsRoot)
	}

	// ---- Hooks ----
	hooksTimeoutSet := false
	if h, ok := wf.Config["hooks"]; ok {
		if hm, ok := h.(map[string]any); ok {
			cfg.Hooks.AfterCreate = strVal(hm, "after_create")
			cfg.Hooks.BeforeRun = strVal(hm, "before_run")
			cfg.Hooks.AfterRun = strVal(hm, "after_run")
			cfg.Hooks.BeforeRemove = strVal(hm, "before_remove")
			cfg.Hooks.BetweenTurns = strVal(hm, "between_turns")
			if _, present := hm["timeout_ms"]; present {
				hooksTimeoutSet = true
				cfg.Hooks.TimeoutMs = intVal(hm, "timeout_ms")
			}
		}
	}
	if !hooksTimeoutSet {
		cfg.Hooks.TimeoutMs = 60000
	}

	// ---- Agent ----
	if a, ok := wf.Config["agent"]; ok {
		if am, ok := a.(map[string]any); ok {
			cfg.Agent.Runtime = strVal(am, "runtime")
			if v := intVal(am, "max_concurrent_agents"); v > 0 {
				cfg.Agent.MaxConcurrentAgents = v
			}
			if v := intVal(am, "max_turns"); v > 0 {
				cfg.Agent.MaxTurns = v
			}
			if v := intVal(am, "max_retry_backoff_ms"); v > 0 {
				cfg.Agent.MaxRetryBackoffMs = v
			}
			// max_concurrent_agents_by_state
			if bs, ok := am["max_concurrent_agents_by_state"]; ok {
				if bsm, ok := bs.(map[string]any); ok {
					m := make(map[string]int, len(bsm))
					for k, v := range bsm {
						if n, ok := toInt(v); ok && n > 0 {
							m[strings.ToLower(k)] = n
						}
					}
					cfg.Agent.MaxConcurrentAgentsByState = m
				}
			}
		}
	}
	if cfg.Agent.Runtime == "" {
		cfg.Agent.Runtime = "codex"
	}
	if cfg.Agent.MaxConcurrentAgents == 0 {
		cfg.Agent.MaxConcurrentAgents = 10
	}
	if cfg.Agent.MaxTurns == 0 {
		cfg.Agent.MaxTurns = 20
	}
	if cfg.Agent.MaxRetryBackoffMs == 0 {
		cfg.Agent.MaxRetryBackoffMs = 300000
	}

	// ---- Codex ----
	if c, ok := wf.Config["codex"]; ok {
		if cm, ok := c.(map[string]any); ok {
			cfg.Codex.Command = strVal(cm, "command")
			cfg.Codex.ApprovalPolicy = strVal(cm, "approval_policy")
			cfg.Codex.ThreadSandbox = strVal(cm, "thread_sandbox")
			cfg.Codex.TurnSandboxPolicy = strVal(cm, "turn_sandbox_policy")
			if v := intVal(cm, "turn_timeout_ms"); v > 0 {
				cfg.Codex.TurnTimeoutMs = v
			}
			if v := intVal(cm, "read_timeout_ms"); v > 0 {
				cfg.Codex.ReadTimeoutMs = v
			}
			if v := intVal(cm, "stall_timeout_ms"); v != 0 {
				cfg.Codex.StallTimeoutMs = v
			}
		}
	}
	if cfg.Codex.Command == "" {
		cfg.Codex.Command = "codex app-server"
	}
	if cfg.Codex.TurnTimeoutMs == 0 {
		cfg.Codex.TurnTimeoutMs = 3600000
	}
	if cfg.Codex.ReadTimeoutMs == 0 {
		cfg.Codex.ReadTimeoutMs = 5000
	}
	if cfg.Codex.StallTimeoutMs == 0 {
		cfg.Codex.StallTimeoutMs = 300000
	}

	// ---- Claude ----
	if c, ok := wf.Config["claude"]; ok {
		if cm, ok := c.(map[string]any); ok {
			cfg.Claude.Command = strVal(cm, "command")
			cfg.Claude.Model = strVal(cm, "model")
			cfg.Claude.PermissionMode = strVal(cm, "permission_mode")
			if v := intVal(cm, "turn_timeout_ms"); v > 0 {
				cfg.Claude.TurnTimeoutMs = v
			}
			if v := intVal(cm, "read_timeout_ms"); v > 0 {
				cfg.Claude.ReadTimeoutMs = v
			}
			if v := intVal(cm, "stall_timeout_ms"); v != 0 {
				cfg.Claude.StallTimeoutMs = v
			}
		}
	}
	if cfg.Claude.Command == "" {
		cfg.Claude.Command = "claude --print --output-format stream-json --input-format stream-json --verbose"
	}
	if cfg.Claude.TurnTimeoutMs == 0 {
		cfg.Claude.TurnTimeoutMs = 3600000
	}
	if cfg.Claude.ReadTimeoutMs == 0 {
		cfg.Claude.ReadTimeoutMs = 5000
	}
	if cfg.Claude.StallTimeoutMs == 0 {
		cfg.Claude.StallTimeoutMs = 300000
	}

	return cfg, nil
}

// Preflight validates the config before dispatch per SPEC §6.3.
func Preflight(cfg Config) error {
	if cfg.Tracker.Kind == "" {
		return ErrMissingTrackerKind
	}

	validRuntimes := map[string]bool{
		"codex":  true,
		"claude": true,
		"mock":   true,
	}
	if !validRuntimes[cfg.Agent.Runtime] {
		return fmt.Errorf("%w: %q", ErrInvalidAgentRuntime, cfg.Agent.Runtime)
	}

	// Validate codex command when runtime is codex
	if cfg.Agent.Runtime == "codex" && cfg.Codex.Command == "" {
		return fmt.Errorf("codex.command is required when agent.runtime=codex")
	}

	// between_turns requires positive timeout_ms
	if cfg.Hooks.BetweenTurns != "" && cfg.Hooks.TimeoutMs <= 0 {
		return ErrBetweenTurnsRequiresTimeout
	}

	// Tracker-kind-specific required fields per SPEC §6.3.
	if cfg.Tracker.Kind == "markdown" {
		if cfg.Tracker.Markdown.Root == "" {
			return fmt.Errorf("tracker.markdown.root is required when tracker.kind=markdown")
		}
		info, err := os.Stat(cfg.Tracker.Markdown.Root)
		if err != nil {
			return fmt.Errorf("tracker.markdown.root %q is not accessible: %w", cfg.Tracker.Markdown.Root, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("tracker.markdown.root %q is not a directory", cfg.Tracker.Markdown.Root)
		}
	}

	if cfg.Tracker.Kind == "openspec" {
		if cfg.Tracker.OpenSpec.Root == "" {
			return fmt.Errorf("tracker.openspec.root is required when tracker.kind=openspec")
		}
		info, err := os.Stat(cfg.Tracker.OpenSpec.Root)
		if err != nil {
			return fmt.Errorf("tracker.openspec.root %q is not accessible: %w", cfg.Tracker.OpenSpec.Root, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("tracker.openspec.root %q is not a directory", cfg.Tracker.OpenSpec.Root)
		}
	}

	if cfg.Tracker.Kind == "jira" {
		j := cfg.Tracker.JIRA
		if j.Endpoint == "" {
			return fmt.Errorf("tracker.jira.endpoint is required when tracker.kind=jira")
		}
		if j.Email == "" {
			return fmt.Errorf("tracker.jira.email is required when tracker.kind=jira")
		}
		if j.APIToken == "" {
			return fmt.Errorf("tracker.jira.api_token is required when tracker.kind=jira")
		}
		if j.ProjectKey == "" {
			return fmt.Errorf("tracker.jira.project_key is required when tracker.kind=jira")
		}
	}

	return nil
}

// Watch starts a filesystem watcher on the workflow file at path. It sends
// resolved Config values on the returned channel. The stop function must be
// called to release resources. On parse failure, the last-known-good config is
// kept and no value is sent per SPEC §6.2.
func Watch(ctx context.Context, path string) (<-chan Config, func(), error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, fmt.Errorf("creating fsnotify watcher: %w", err)
	}

	if err := watcher.Add(path); err != nil {
		watcher.Close() //nolint:errcheck
		return nil, nil, fmt.Errorf("watching %s: %w", path, err)
	}

	ch := make(chan Config, 4)
	workflowDir := filepath.Dir(path)

	loadAndSend := func(last *Config) *Config {
		wf, err := workflow.Load(path)
		if err != nil {
			return last
		}
		cfg, err := Resolve(wf, workflowDir)
		if err != nil {
			return last
		}
		select {
		case ch <- cfg:
		default:
		}
		return &cfg
	}

	go func() {
		defer watcher.Close() //nolint:errcheck
		defer close(ch)

		var last *Config

		// Send initial config
		last = loadAndSend(last)

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					// Small debounce to avoid double-fire
					time.Sleep(50 * time.Millisecond)
					last = loadAndSend(last)
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			}
		}
	}()

	stop := func() {
		watcher.Close() //nolint:errcheck
	}

	return ch, stop, nil
}

// ---- helpers ----

func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func intVal(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		if n, ok := toInt(v); ok {
			return n
		}
	}
	return 0
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func strSlice(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// resolveVar resolves $VAR_NAME environment variable references.
func resolveVar(s string) string {
	if strings.HasPrefix(s, "$") {
		varName := s[1:]
		return os.Getenv(varName)
	}
	return s
}

// expandHome expands a leading ~ to the user's home directory.
func expandHome(s string) string {
	if !strings.HasPrefix(s, "~") {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	return home + s[1:]
}

// resolvePathField expands home and env then resolves relative to workflowDir.
func resolvePathField(s, workflowDir string) string {
	if s == "" {
		return ""
	}
	s = expandHome(resolveVar(s))
	if !filepath.IsAbs(s) {
		s = filepath.Join(workflowDir, s)
	}
	return filepath.Clean(s)
}
