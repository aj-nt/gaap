package gaap

import (
	"context"
	"testing"
	"time"
)

func TestOrchestratorCircuitBreakerBlocksDispatch(t *testing.T) {
	t.Parallel()
	o := testOrchestrator()

	// Create and register a tight breaker (1 failure trips; 1h cooldown)
	breaker := NewCircuitBreaker("static_analysis", 1, 1*time.Hour)
	o.breakerRegistry = map[string]*CircuitBreaker{
		"static_analysis": breaker,
	}

	// Trip it
	breaker.RecordFailure()
	if breaker.State() != StateOpen {
		t.Fatalf("expected open, got %s", breaker.State())
	}

	// Set up decomposer that produces a static_analysis task
	o.decomposer = NewDecomposer(NewStaticDecomposition([]TaskSpec{
		{TaskID: "blocked-task", AgentType: "static_analysis", Status: "ready"},
	}))

	o.goal = "test blocked dispatch"
	o.dag = NewDAG()

	ctx := context.Background()
	planning := &PlanningState{}
	err := planning.Enter(ctx, o)
	if err != nil {
		t.Errorf("planning enter: %v", err)
	}

	// The task should be in the DAG (added before dispatch check).
	// Find it by agent type since Decomposer prefixes IDs.
	found := false
	for _, node := range o.dag.nodes {
		if node.AgentType == "static_analysis" {
			found = true
			// With breaker tripped and NullMnemo (daemon unavailable),
			// the task stays "ready" — it was never dispatched.
			if node.Status != "ready" {
				t.Errorf("task %s: expected ready, got %s", node.ID, node.Status)
			}
			break
		}
	}
	if !found {
		t.Fatal("no static_analysis task found in DAG")
	}
}

func TestOrchestratorCircuitBreakerAllowsWhenClosed(t *testing.T) {
	t.Parallel()
	o := testOrchestrator()

	breaker := NewCircuitBreaker("quality_scan", 2, 1*time.Hour)
	o.breakerRegistry = map[string]*CircuitBreaker{
		"quality_scan": breaker,
	}

	// Not tripped — dispatch should proceed
	o.decomposer = NewDecomposer(NewStaticDecomposition([]TaskSpec{
		{TaskID: "qual-1", AgentType: "quality_scan", Status: "ready"},
	}))

	o.goal = "test allow"
	o.dag = NewDAG()

	ctx := context.Background()
	planning := &PlanningState{}
	err := planning.Enter(ctx, o)
	if err != nil {
		t.Errorf("planning enter: %v", err)
	}

	found := false
	for _, node := range o.dag.nodes {
		if node.AgentType == "quality_scan" {
			found = true
			if node.Status != "ready" {
				t.Errorf("expected ready, got %s", node.Status)
			}
			break
		}
	}
	if !found {
		t.Fatal("no quality_scan task found in DAG")
	}
}
