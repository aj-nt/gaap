package gaap

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/ollama"
	"github.com/aj-nt/gaap/internal/worker"
	"github.com/aj-nt/vassago-sdk/client"
)

// TestE2EWithRealDaemonAndWorkers runs the full pipeline against a live Vassago
// daemon with auto-workers. Requires VASSAGO_DAEMON env var (e.g., "localhost:50051")
// and a running Ollama instance with the configured model.
func TestE2EWithRealDaemonAndWorkers(t *testing.T) {
	addr := os.Getenv("VASSAGO_DAEMON")
	if addr == "" {
		t.Skip("VASSAGO_DAEMON not set — skipping E2E test (needs live daemon + Ollama)")
	}

	repoPath := os.Getenv("GAAP_TEST_REPO")
	if repoPath == "" {
		repoPath = os.TempDir()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Connect to the real daemon
	daemonClient, err := client.Connect(ctx, client.Config{Address: addr})
	if err != nil {
		t.Fatalf("connect to daemon at %s: %v", addr, err)
	}
	defer daemonClient.Close()

	if err := daemonClient.HealthCheck(ctx); err != nil {
		t.Fatalf("daemon health check: %v", err)
	}

	// Build LLM decomposer
	ollamaClient := ollama.NewClient(ollama.Config{
		BaseURL:     "http://localhost:11434/v1",
		Model:       envOrDefault("GAAP_MODEL", "glm-5.1:cloud"),
		MaxTokens:   4096,
		Temperature: 0.1,
		TimeoutSec:  120,
	})

	chatFn := func(ctx context.Context, prompt string) (string, error) {
		return ollamaClient.Chat([]ollama.Message{{Role: "user", Content: prompt}})
	}

	cfg := &Config{
		DaemonAddr:      addr,
		RepoPath:        repoPath,
		MaxWaitSec:      120,
		PollIntervalSec: 2,
	}

	decomposer := NewDecomposer(NewLLMDecomposition(chatFn, nil))
	orchestrator := NewOrchestrator(ctx, cfg, daemonClient, decomposer)
	orchestrator.SetSynthesisChatFn(chatFn)

	// Wire up auto-workers
	wpCfg := worker.PoolConfig{
		DaemonAddr:  addr,
		AgentID:     "gaap-test-worker",
		AgentName:   "gaap-test-worker",
		AgentTypes:  []string{"static_analysis", "quality_scan"},
		WorkerCount: 2,
		PollSec:     2,
		MaxTurns:    10,
		RepoPath:    repoPath,
		Ollama: ollama.Config{
			BaseURL:     "http://localhost:11434/v1",
			Model:       envOrDefault("GAAP_MODEL", "glm-5.1:cloud"),
			MaxTokens:   2000,
			Temperature: 0.1,
			TimeoutSec:  120,
		},
	}

	pool, err := worker.NewPool(ctx, wpCfg, daemonClient)
	if err != nil {
		t.Fatalf("create worker pool: %v", err)
	}
	orchestrator.SetWorkerPool(pool)

	// Run a simple goal that any worker can handle
	goal := "count the number of Go source files (.go) in " + repoPath

	t.Logf("Running: %s", goal)
	err = orchestrator.Run(goal)
	if err != nil {
		t.Fatalf("orchestrator run: %v", err)
	}

	// Verify completion
	if !orchestrator.dag.AllTasksComplete() {
		t.Error("not all tasks completed")
	}

	if orchestrator.dag.TaskCount() == 0 {
		t.Error("no tasks in DAG")
	}

	// Verify synthesis produced output
	if orchestrator.result == nil {
		t.Error("synthesis result is nil")
	} else {
		t.Logf("Synthesis: %s", orchestrator.result.ExecutiveSummary)
	}

	comp, fail := pool.Stats()
	t.Logf("Worker stats: completed=%d failed=%d", comp, fail)

	if orchestrator.state == nil || orchestrator.state.Name() != "done" {
		t.Errorf("expected state done, got %q", orchestrator.state.Name())
	}
}

// TestE2EWithAutoWorkersNullMnemo verifies that the orchestrator+worker wiring
// doesn't crash when using NullMnemo. The simulation fallback handles everything
// (no real daemon needed).
func TestE2EWithAutoWorkersNullMnemo(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		DaemonAddr:      "",
		RepoPath:        "/tmp/test-repo",
		MaxWaitSec:      30,
		PollIntervalSec: 1,
	}

	daemon := &client.NullMnemo{}

	decomp := NewDecomposer(&StaticDecomposition{
		Tasks: []TaskSpec{
			{
				TaskID:    "leaf-1",
				AgentType: "static_analysis",
				Status:    "ready",
				Goal:      "Run golangci-lint on /tmp/test-repo",
				Context:   map[string]any{"source_path": "/tmp/test-repo"},
			},
			{
				TaskID:    "leaf-2",
				AgentType: "quality_scan",
				Status:    "ready",
				Goal:      "Scan codebase quality",
				Context:   map[string]any{"source_path": "/tmp/test-repo"},
			},
			{
				TaskID:    "synthesis",
				AgentType: "synthesis",
				Status:    "blocked",
				Goal:      "Synthesize audit report",
				ParentIDs: []string{"leaf-1", "leaf-2"},
				Context: map[string]any{
					"source_path": "/tmp/test-repo",
					"dependents":  []string{"leaf-1", "leaf-2"},
				},
			},
		},
	})

	orchestrator := NewOrchestrator(context.Background(), cfg, daemon, decomp)

	// Worker pool with NullMnemo will fail to register (expected).
	// That's fine — orchestrator's simulation fallback handles it.
	_, err := worker.NewPool(context.Background(), worker.PoolConfig{
		AgentID:     "test-worker",
		AgentName:   "test-worker",
		AgentTypes:  []string{"static_analysis", "quality_scan"},
		WorkerCount: 2,
		PollSec:     1,
		MaxTurns:    10,
		Ollama: ollama.Config{
			BaseURL: "http://localhost:11434/v1",
			Model:   "glm-5.1:cloud",
		},
	}, daemon)
	if err != nil {
		t.Logf("worker pool error (expected with NullMnemo): %v", err)
	}
	// Pool not set — orchestrator uses simulation fallback.

	err = orchestrator.Run("audit the test codebase")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !orchestrator.dag.AllTasksComplete() {
		t.Error("not all tasks completed")
	}
	if orchestrator.dag.TaskCount() != 3 {
		t.Errorf("expected 3 tasks, got %d", orchestrator.dag.TaskCount())
	}
	if orchestrator.state == nil || orchestrator.state.Name() != "done" {
		t.Errorf("expected state done, got %q", orchestrator.state.Name())
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
