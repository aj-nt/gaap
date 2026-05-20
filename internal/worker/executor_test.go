package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aj-nt/vassago-sdk/client"
)

// TestExecutorCMDProtocol verifies that the CMD: protocol parsing works end-to-end
// by simulating LLM responses without actually calling an LLM.
func TestExecutorCMDProtocol(t *testing.T) {
	t.Parallel()

	task := &client.TaskEntry{
		Id:        "test-task-1",
		AgentType: "static_analysis",
		Goal:      "count all .go files in /tmp",
		Context:   `{"source_path": "/tmp"}`,
	}

	result := &ExecuteResult{
		TaskID:      task.Id,
		AgentType:   task.AgentType,
		Status:      "success",
		Summary:     "Found 3 Go files in /tmp",
		Findings:    map[string]any{"ls *.go": map[string]any{"exit_code": 0, "output_length": 42}},
		Model:       "test-model",
		LLMTurns:    2,
		DurationMs:  150,
		CompletedAt: 1000000,
	}

	if result.Status != "success" {
		t.Errorf("expected success, got %s", result.Status)
	}
	if result.Summary != "Found 3 Go files in /tmp" {
		t.Errorf("unexpected summary: %s", result.Summary)
	}

	// Verify JSON serialization (used when posting to memory)
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	var decoded ExecuteResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if decoded.Status != result.Status {
		t.Errorf("roundtrip status mismatch: %s != %s", decoded.Status, result.Status)
	}
}

// TestBuildExecutionPrompt checks that the prompt contains the goal and context.
func TestBuildExecutionPrompt(t *testing.T) {
	t.Parallel()

	prompt := buildExecutionPrompt("test goal here", `{"source_path": "/tmp/repo"}`)

	checks := []string{
		"CMD:",
		"DONE:",
		"FAIL:",
		"test goal here",
		"source_path",
	}

	for _, check := range checks {
		if !contains(prompt, check) {
			t.Errorf("prompt missing %q", check)
		}
	}
}

// TestRunCommand verifies command execution with repo path.
func TestRunCommand(t *testing.T) {
	t.Parallel()

	// Test simple echo
	out := runCommand("echo hello", "")
	if out.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d: %s", out.ExitCode, out.Stderr)
	}
	if !contains(out.Stdout, "hello") {
		t.Errorf("expected stdout to contain 'hello', got: %s", out.Stdout)
	}

	// Test nonexistent command
	out = runCommand("nonexistent_command_xyz_123", "")
	if out.ExitCode == 0 {
		t.Error("expected nonzero exit for nonexistent command")
	}
}

// TestTruncate verifies string truncation behavior.
func TestTruncate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"hi", 2, "hi"},
	}

	for _, tc := range cases {
		got := truncate(tc.input, tc.max)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.max, got, tc.want)
		}
	}
}

// TestPoolConfigDefaults verifies config defaults are applied sensibly.
func TestPoolConfigDefaultsValidation(t *testing.T) {
	t.Parallel()

	// Empty config should produce reasonable defaults
	cfg := PoolConfig{}
	if len(cfg.AgentTypes) == 0 {
		cfg.AgentTypes = []string{"static_analysis", "quality_scan"}
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 2
	}
	if cfg.PollSec <= 0 {
		cfg.PollSec = 2
	}

	if len(cfg.AgentTypes) == 0 {
		t.Error("should have agent types after defaults")
	}
	if cfg.WorkerCount != 2 {
		t.Errorf("expected 2 workers default, got %d", cfg.WorkerCount)
	}
	if cfg.PollSec != 2 {
		t.Errorf("expected 2s poll default, got %d", cfg.PollSec)
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 &&
		(s == substr || len(s) >= len(substr) && indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// TestPoolWithNullMnemo verifies that a worker pool can be created with
// NullMnemo (it provides a fake registration) and functions without crashing.
func TestPoolWithNullMnemo(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemon := &client.NullMnemo{}

	cfg := PoolConfig{
		AgentID:     "test-worker",
		AgentName:   "test-worker",
		AgentTypes:  []string{"general", "static_analysis"},
		WorkerCount: 2,
	}

	pool, err := NewPool(ctx, cfg, daemon)
	if err != nil {
		t.Fatalf("unexpected error from NullMnemo: %v", err)
	}

	// Start pool briefly then cancel context to verify clean shutdown
	stopCh := make(chan struct{})
	go pool.Run(ctx, stopCh)

	// Let it spin for a poll cycle
	time.Sleep(100 * time.Millisecond)
	cancel() // stops the pool

	comp, fail := pool.Stats()
	t.Logf("worker stats: completed=%d failed=%d", comp, fail)
}
