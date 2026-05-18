package gaap

import (
	"context"
	"testing"

	"github.com/aj-nt/vassago-sdk/client"
)

// TestE2EStaticDecomposition runs the full pipeline with static decomposition
// and a NullMnemo client. No real daemon or LLM needed.
func TestE2EStaticDecomposition(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		DaemonAddr:      "",
		RepoPath:        "/tmp/test-repo",
		MaxWaitSec:      30,
		PollIntervalSec: 1,
	}

	// A 3-task DAG: two leaves + one synthesis that depends on both.
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

	o := NewOrchestrator(context.Background(), cfg, &client.NullMnemo{}, decomp)

	err := o.Run("audit the test codebase")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify all tasks completed
	if !o.dag.AllTasksComplete() {
		t.Error("not all tasks completed")
	}

	// Verify DAG structure
	if o.dag.TaskCount() != 3 {
		t.Errorf("expected 3 tasks, got %d", o.dag.TaskCount())
	}

	// Verify state is Done
	if o.state == nil || o.state.Name() != "done" {
		t.Errorf("expected state done, got %q", o.state.Name())
	}
}

// TestE2EDAGPromotion verifies that a blocked task is promoted when its parents complete.
func TestE2EDAGPromotion(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		RepoPath:        "/tmp/test-repo",
		MaxWaitSec:      30,
		PollIntervalSec: 1,
	}

	decomp := NewDecomposer(&StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "t1", AgentType: "worker", Status: "ready", Goal: "task 1"},
			{TaskID: "t2", AgentType: "worker", Status: "ready", Goal: "task 2"},
			{TaskID: "t3", AgentType: "worker", Status: "blocked", Goal: "task 3",
				ParentIDs: []string{"t1", "t2"}},
		},
	})

	o := NewOrchestrator(context.Background(), cfg, &client.NullMnemo{}, decomp)

	err := o.Run("test dag promotion")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// t3 should have been promoted from blocked → ready → done.
	// Walk nodes by goal since IDs are prefixed by decomposer.
	var t3 *TaskNode
	for _, node := range o.dag.nodes {
		if node.Goal == "task 3" {
			t3 = node
			break
		}
	}
	if t3 == nil {
		t.Fatal("t3 not found in DAG")
	}
	if t3.Status != "done" {
		t.Errorf("t3 status = %q, want done", t3.Status)
	}
}

// TestE2ESingleLeafTask exercises the simplest DAG: one task, no dependencies.
func TestE2ESingleLeafTask(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		RepoPath:        "/tmp/test-repo",
		MaxWaitSec:      30,
		PollIntervalSec: 1,
	}

	decomp := NewDecomposer(&StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "solo", AgentType: "worker", Status: "ready", Goal: "solo work"},
		},
	})

	o := NewOrchestrator(context.Background(), cfg, &client.NullMnemo{}, decomp)

	err := o.Run("solo task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !o.dag.AllTasksComplete() {
		t.Error("solo task should be complete")
	}
	if o.dag.TaskCount() != 1 {
		t.Errorf("expected 1 task, got %d", o.dag.TaskCount())
	}
}
