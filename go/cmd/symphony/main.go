// Command symphony is the CLI entry point for the Symphony orchestrator.
// It loads WORKFLOW.md, resolves configuration, builds the tracker and agent
// runtime, and runs the poll-loop until SIGINT or SIGTERM is received.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	_ "github.com/openai/symphony/go/internal/agent/claude" // register claude runtime
	_ "github.com/openai/symphony/go/internal/agent/codex"  // register codex runtime
	_ "github.com/openai/symphony/go/internal/agent/mock"   // register mock runtime

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/domain"
	"github.com/openai/symphony/go/internal/observability"
	"github.com/openai/symphony/go/internal/orchestrator"
	"github.com/openai/symphony/go/internal/tracker"
	"github.com/openai/symphony/go/internal/web"
	"github.com/openai/symphony/go/internal/workflow"
	"github.com/openai/symphony/go/internal/workspace"
)

func main() {
	var (
		workflowPath string
		port         int
		listen       string
	)
	flag.StringVar(&workflowPath, "workflow", "WORKFLOW.md", "Path to WORKFLOW.md")
	flag.IntVar(&port, "port", 0, "Web dashboard port (0 = disabled)")
	flag.StringVar(&listen, "listen", "127.0.0.1", "Web dashboard bind interface")
	flag.Parse()

	log := observability.New(os.Stdout)

	// 1. Load workflow.
	wf, err := workflow.Load(workflowPath)
	if err != nil {
		log.Error("failed to load workflow", "path", workflowPath, "err", fmt.Sprintf("%v", err))
		os.Exit(1)
	}

	// 2. Resolve config.
	cfg, err := config.Resolve(wf, dirOf(workflowPath))
	if err != nil {
		log.Error("failed to resolve config", "err", fmt.Sprintf("%v", err))
		os.Exit(1)
	}

	// 3. Preflight.
	if err := config.Preflight(cfg); err != nil {
		log.Error("preflight failed", "err", fmt.Sprintf("%v", err))
		os.Exit(1)
	}

	// 4. Build tracker. Honour SYMPHONY_SEED_ISSUES env (JSON array of domain.Issue)
	//    when running with memory tracker.
	var t tracker.Tracker
	if seedJSON := os.Getenv("SYMPHONY_SEED_ISSUES"); seedJSON != "" && cfg.Tracker.Kind == "memory" {
		var seed []domain.Issue
		if jsonErr := json.Unmarshal([]byte(seedJSON), &seed); jsonErr != nil {
			log.Error("SYMPHONY_SEED_ISSUES is not valid JSON", "err", fmt.Sprintf("%v", jsonErr))
			os.Exit(1)
		}
		t, err = tracker.NewWithSeed(cfg, seed)
	} else {
		t, err = tracker.New(cfg)
	}
	if err != nil {
		log.Error("failed to build tracker", "err", fmt.Sprintf("%v", err))
		os.Exit(1)
	}

	// 5. Build agent runtime.
	rt, err := agent.New(cfg)
	if err != nil {
		log.Error("failed to build agent runtime", "err", fmt.Sprintf("%v", err))
		os.Exit(1)
	}

	// 6. Build workspace manager.
	ws := workspace.NewManager(cfg)

	// 7. Build orchestrator.
	orch := orchestrator.New(cfg, t, rt, ws, log).
		WithPromptTemplate(wf.PromptTemplate)

	// 8. Set up signal handling.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Info("symphony started",
		"workflow", workflowPath,
		"tracker_kind", cfg.Tracker.Kind,
		"agent_runtime", cfg.Agent.Runtime,
		"poll_interval_ms", cfg.Polling.IntervalMs,
	)

	// 9. Optional web dashboard. Starts the HTTP server in a goroutine
	//    alongside the orchestrator; both share the same SIGINT/SIGTERM
	//    handler and must drain before main returns. -port=0 keeps the
	//    web layer disabled (and the existing orchestrator-only code path
	//    intact).
	var (
		server     *http.Server
		serverDone <-chan error
	)
	if port > 0 {
		addr := net.JoinHostPort(listen, strconv.Itoa(port))
		server = &http.Server{
			Addr:              addr,
			Handler:           web.NewHandler(orch),
			ReadHeaderTimeout: 10 * time.Second,
		}
		log.Info("web dashboard available at http://" + addr + "/")
		done := make(chan error, 1)
		go func() {
			err := server.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			done <- err
		}()
		serverDone = done
	}

	// 10. Run the orchestrator (blocks until ctx is cancelled).
	orchErr := orch.Run(ctx)

	// 11. Drain the HTTP server. The orchestrator has already returned, so
	//     ctx is cancelled — give Shutdown its own bounded deadline.
	if server != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Warn("web server shutdown error", "err", fmt.Sprintf("%v", err))
		}
		shutdownCancel()
		if err := <-serverDone; err != nil {
			log.Warn("web server exited with error", "err", fmt.Sprintf("%v", err))
		}
	}

	if orchErr != nil {
		log.Error("orchestrator error", "err", fmt.Sprintf("%v", orchErr))
		os.Exit(1)
	}

	log.Info("symphony stopped")
}

// dirOf returns the directory of the given file path.
func dirOf(path string) string {
	return filepath.Dir(path)
}
