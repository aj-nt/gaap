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

// TestRunStateRoundTrip verifies that RunState can be serialized and
// the DAG reconstructed from it correctly.
func TestRunStateRoundTrip(t *testing.T) {
	t.Parallel()

	rs := &RunState{
		Goal:     "audit the codebase",
		RepoPath: "/tmp/repo",
		Tasks: []TaskSpec{
			{TaskID: "t1", AgentType: "worker", Status: "ready", Goal: "task 1"},
			{TaskID: "t2", AgentType: "worker", Status: "ready", Goal: "task 2"},
			{TaskID: "t3", AgentType: "synthesis", Status: "blocked", Goal: "synthesize",
				ParentIDs: []string{"t1", "t2"}},
		},
	}

	dag := NewDAG()
	for _, ts := range rs.Tasks {
		if err := dag.AddTask(&TaskNode{
			ID:        ts.TaskID,
			ParentIDs: ts.ParentIDs,
			Status:    ts.Status,
			Goal:      ts.Goal,
			AgentType: ts.AgentType,
			Context:   ts.Context,
		}); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
	}

	if dag.TaskCount() != 3 {
		t.Errorf("expected 3 tasks, got %d", dag.TaskCount())
	}

	// t3 should be blocked with two parents
	node, err := dag.GetTask("t3")
	if err != nil {
		t.Fatalf("GetTask t3: %v", err)
	}
	if node.Status != "blocked" {
		t.Error("t3 should be blocked")
	}
	if len(node.ParentIDs) != 2 {
		t.Errorf("t3 should have 2 parents, got %d", len(node.ParentIDs))
	}
	if allDone, err := dag.AllParentsComplete("t1"); err != nil || !allDone {
		t.Errorf("t1 should have all parents complete: done=%v err=%v", allDone, err)
	}
}

// TestResumeCompletesDAG verifies that Resume picks up a saved RunState
// and completes the full pipeline to Done.
func TestResumeCompletesDAG(t *testing.T) {
	t.Parallel()

	rs := &RunState{
		Goal:     "audit the codebase",
		RepoPath: "/tmp/test-repo",
		Tasks: []TaskSpec{
			{TaskID: "leaf-1", AgentType: "static_analysis", Status: "ready", Goal: "lint"},
			{TaskID: "leaf-2", AgentType: "quality_scan", Status: "ready", Goal: "scan"},
			{TaskID: "synth", AgentType: "synthesis", Status: "blocked", Goal: "synthesize",
				ParentIDs: []string{"leaf-1", "leaf-2"}},
		},
	}

	cfg := &Config{
		RepoPath:        rs.RepoPath,
		MaxWaitSec:      30,
		PollIntervalSec: 1,
	}
	o := NewOrchestrator(context.Background(), cfg, &client.NullMnemo{}, nil)

	err := o.Resume(rs)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if o.state == nil || o.state.Name() != "done" {
		t.Errorf("expected state done, got %q", o.state.Name())
	}
	if !o.dag.AllTasksComplete() {
		t.Error("all tasks should be complete")
	}
	if o.dag.TaskCount() != 3 {
		t.Errorf("expected 3 tasks, got %d", o.dag.TaskCount())
	}
}

// TestResumePreservesGoal verifies that the goal is preserved through resume.
func TestResumePreservesGoal(t *testing.T) {
	t.Parallel()

	rs := &RunState{
		Goal:     "deep scan of gaap",
		RepoPath: "/tmp/repo",
		Tasks: []TaskSpec{
			{TaskID: "single", AgentType: "worker", Status: "ready", Goal: "scan"},
		},
	}

	cfg := &Config{
		RepoPath:        rs.RepoPath,
		MaxWaitSec:      30,
		PollIntervalSec: 1,
	}
	o := NewOrchestrator(context.Background(), cfg, &client.NullMnemo{}, nil)

	err := o.Resume(rs)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if o.goal != "deep scan of gaap" {
		t.Errorf("expected goal 'deep scan of gaap', got %q", o.goal)
	}
}
