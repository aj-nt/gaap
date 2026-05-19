package gaap

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLLMDecompositionProducesValidDAG(t *testing.T) {
	t.Parallel()

	// Mock chatFn returns a valid 3-task DAG
	chatFn := func(ctx context.Context, prompt string) (string, error) {
		tasks := []TaskSpec{
			{
				TaskID:    "task_static",
				ParentIDs: []string{},
				Status:    "ready",
				Goal:      "Run static analysis",
				AgentType: "static_analysis",
				Context:   map[string]any{"repo_path": "/tmp/test"},
			},
			{
				TaskID:    "task_quality",
				ParentIDs: []string{},
				Status:    "ready",
				Goal:      "Run quality scan",
				AgentType: "quality_scan",
				Context:   map[string]any{"repo_path": "/tmp/test"},
			},
			{
				TaskID:    "task_synthesis",
				ParentIDs: []string{"task_static", "task_quality"},
				Status:    "blocked",
				Goal:      "Synthesize findings",
				AgentType: "synthesis",
				Context: map[string]any{
					"repo_path":  "/tmp/test",
					"dependents": []any{"task_static", "task_quality"},
				},
			},
		}
		b, _ := json.Marshal(tasks)
		return string(b), nil
	}

	strategy := NewLLMDecomposition(chatFn)
	tasks, err := strategy.Decompose(context.Background(), "audit the codebase", "/tmp/test")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}

	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}

	// Verify leaf tasks
	if tasks[0].Status != "ready" || len(tasks[0].ParentIDs) != 0 {
		t.Errorf("task 0 should be ready with no parents, got status=%s parents=%v", tasks[0].Status, tasks[0].ParentIDs)
	}

	// Verify synthesis task is blocked with correct parents
	synth := tasks[2]
	if synth.Status != "blocked" {
		t.Errorf("synthesis status = %q, want blocked", synth.Status)
	}
	if len(synth.ParentIDs) != 2 {
		t.Errorf("synthesis parent count = %d, want 2", len(synth.ParentIDs))
	}
}

func TestLLMDecompositionFallsBackOnLLMFailure(t *testing.T) {
	t.Parallel()

	// chatFn always errors
	chatFn := func(ctx context.Context, prompt string) (string, error) {
		return "", context.DeadlineExceeded
	}

	strategy := NewLLMDecomposition(chatFn)
	tasks, err := strategy.Decompose(context.Background(), "audit codebase", "/tmp/test")
	// The strategy itself should not return an error — the Decomposer wrapper
	// handles fallback. But the LLMDecomposition strategy should report the failure.
	if err == nil {
		t.Fatal("expected an error from LLM failure")
	}
	_ = tasks // may be nil or hardcoded fallback
}

func TestLLMDecompositionFallsBackOnBadJSON(t *testing.T) {
	t.Parallel()

	chatFn := func(ctx context.Context, prompt string) (string, error) {
		return "not json at all", nil
	}

	strategy := NewLLMDecomposition(chatFn)
	_, err := strategy.Decompose(context.Background(), "audit", "/tmp/test")
	if err == nil {
		t.Fatal("expected error from invalid JSON")
	}
}

func TestLLMDecompositionRejectsEmptyTaskList(t *testing.T) {
	t.Parallel()

	chatFn := func(ctx context.Context, prompt string) (string, error) {
		return "[]", nil
	}

	strategy := NewLLMDecomposition(chatFn)
	_, err := strategy.Decompose(context.Background(), "audit", "/tmp/test")
	if err == nil {
		t.Fatal("expected error for empty task list")
	}
}

func TestLLMDecompositionRejectsUnknownAgentType(t *testing.T) {
	t.Parallel()

	chatFn := func(ctx context.Context, prompt string) (string, error) {
		tasks := []TaskSpec{
			{
				TaskID:    "task_wizard",
				ParentIDs: []string{},
				Status:    "ready",
				Goal:      "Cast spells",
				AgentType: "wizard", // not in the catalog
			},
		}
		b, _ := json.Marshal(tasks)
		return string(b), nil
	}

	strategy := NewLLMDecomposition(chatFn)
	_, err := strategy.Decompose(context.Background(), "audit", "/tmp/test")
	if err == nil {
		t.Fatal("expected error for unknown agent type")
	}
}

func TestLLMDecompositionCleansFencedJSON(t *testing.T) {
	t.Parallel()

	chatFn := func(ctx context.Context, prompt string) (string, error) {
		tasks := []TaskSpec{
			{
				TaskID:    "task_static",
				ParentIDs: []string{},
				Status:    "ready",
				Goal:      "Run static analysis",
				AgentType: "static_analysis",
			},
		}
		b, _ := json.Marshal(tasks)
		return "```json\n" + string(b) + "\n```", nil
	}

	strategy := NewLLMDecomposition(chatFn)
	tasks, err := strategy.Decompose(context.Background(), "audit", "/tmp/test")
	if err != nil {
		t.Fatalf("Decompose (fenced JSON): %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task from fenced JSON, got %d", len(tasks))
	}
	if tasks[0].TaskID != "task_static" {
		t.Errorf("task ID = %q, want task_static", tasks[0].TaskID)
	}
}
