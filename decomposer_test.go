package gaap

import (
	"context"
	"testing"
)

func TestDecomposerValidatesEmptyGoal(t *testing.T) {
	t.Parallel()
	d := NewDecomposer(&StaticDecomposition{})
	_, err := d.Decompose(context.Background(), "", "/tmp/repo")
	if err == nil {
		t.Error("expected error for empty goal")
	}
}

func TestDecomposerStaticStrategy(t *testing.T) {
	t.Parallel()
	tasks := []TaskSpec{
		{TaskID: "tsk_1", AgentType: "static_analysis", Status: "ready", Goal: "run lint"},
		{TaskID: "tsk_2", AgentType: "quality_scan", Status: "ready", Goal: "scan quality"},
	}
	strategy := &StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "tsk_1", AgentType: "static_analysis", Status: "ready", Goal: "run lint"},
			{TaskID: "tsk_2", AgentType: "quality_scan", Status: "ready", Goal: "scan quality"},
		},
	}
	d := NewDecomposer(strategy)
	result, err := d.Decompose(context.Background(), "audit", "/tmp/repo")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(result))
	}
	if result[0].AgentType != tasks[0].AgentType {
		t.Errorf("task[0] AgentType = %q, want %q", result[0].AgentType, tasks[0].AgentType)
	}
	if result[1].AgentType != tasks[1].AgentType {
		t.Errorf("task[1] AgentType = %q, want %q", result[1].AgentType, tasks[1].AgentType)
	}
}

func TestDecomposerPrefixesTaskIDs(t *testing.T) {
	t.Parallel()
	strategy := &StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "leaf", AgentType: "worker", Status: "ready", Goal: "do work"},
		},
	}
	d := NewDecomposer(strategy)
	result, err := d.Decompose(context.Background(), "audit", "/tmp/repo")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 task, got %d", len(result))
	}
	// Task ID should be prefixed to avoid collisions across runs.
	if result[0].TaskID == "leaf" {
		t.Error("expected prefixed task ID, got bare 'leaf'")
	}
}

func TestDecomposerEmptyTaskList(t *testing.T) {
	t.Parallel()
	strategy := &StaticDecomposition{
		Tasks: nil, // empty
	}
	d := NewDecomposer(strategy)
	_, err := d.Decompose(context.Background(), "audit", "/tmp/repo")
	if err == nil {
		t.Error("expected error for empty decomposition")
	}
}

func TestDecomposerValidatesDAGNoCycles(t *testing.T) {
	t.Parallel()
	// Self-dependency should be caught.
	strategy := &StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "bad", AgentType: "worker", Status: "ready", Goal: "bad",
				ParentIDs: []string{"bad"}},
		},
	}
	d := NewDecomposer(strategy)
	_, err := d.Decompose(context.Background(), "audit", "/tmp/repo")
	if err == nil {
		t.Error("expected error for self-referencing task")
	}
}

func TestDecomposerFallbackToHardcoded(t *testing.T) {
	t.Parallel()
	// A strategy that always fails should trigger the hardcoded fallback.
	strategy := &FailingDecomposition{}
	d := NewDecomposer(strategy)
	result, err := d.Decompose(context.Background(), "audit codebase", "/tmp/repo")
	if err != nil {
		t.Fatalf("fallback should produce tasks, got error: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 fallback tasks, got %d", len(result))
	}
	// Check DAG structure: first two are leaf (no parents), third has both as parents.
	if len(result[0].ParentIDs) != 0 {
		t.Errorf("task[0] should be leaf, got parents: %v", result[0].ParentIDs)
	}
	if len(result[1].ParentIDs) != 0 {
		t.Errorf("task[1] should be leaf, got parents: %v", result[1].ParentIDs)
	}
	if len(result[2].ParentIDs) != 2 {
		t.Errorf("task[2] should have 2 parents, got %v", result[2].ParentIDs)
	}
}

func TestDecomposerContextPropagation(t *testing.T) {
	t.Parallel()
	strategy := &StaticDecomposition{
		Tasks: []TaskSpec{
			{
				TaskID: "ctx-task", AgentType: "worker", Status: "ready",
				Goal: "test context",
				Context: map[string]any{
					"source_path": "/my/repo",
					"dependents":  []string{"dep-1"},
				},
			},
		},
	}
	d := NewDecomposer(strategy)
	result, err := d.Decompose(context.Background(), "audit", "/tmp/repo")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 task, got %d", len(result))
	}
	task := result[0]
	if task.Context == nil {
		t.Fatal("context should not be nil")
	}
	if task.Context["source_path"] != "/my/repo" {
		t.Errorf("source_path = %v, want /my/repo", task.Context["source_path"])
	}
}
