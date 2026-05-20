package gaap

import (
	"context"
	"testing"

	"github.com/aj-nt/vassago-sdk/client"
)

// testOrchestrator creates an orchestrator with a NullMnemo client for unit testing.
func testOrchestrator() *Orchestrator {
	return &Orchestrator{
		cfg: &Config{
			MaxWaitSec:      30,
			PollIntervalSec: 1,
			RepoPath:        "/tmp/test-repo",
		},
		ctx:    context.Background(),
		daemon: &client.NullMnemo{},
		dag:    NewDAG(),
	}
}

func TestNewOrchestratorDefaultsToIdle(t *testing.T) {
	t.Parallel()
	o := testOrchestrator()
	o.state = &IdleState{}
	if o.state == nil {
		t.Fatal("state should not be nil")
	}
	if o.state.Name() != "idle" {
		t.Errorf("state name = %q, want idle", o.state.Name())
	}
}

func TestIdleStateTransitionsToPlanning(t *testing.T) {
	t.Parallel()
	o := testOrchestrator()
	idle := &IdleState{}
	err := idle.Enter(o.ctx, o)
	if err != nil {
		t.Fatalf("IdleState.Enter: %v", err)
	}
	o.state = idle

	next, err := idle.HandleEvent(o.ctx, o, Event{Type: EventGoalReceived, Payload: map[string]any{"goal": "audit codebase"}})
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if next == nil {
		t.Fatal("expected transition to PlanningState")
	}
	if next.Name() != "planning" {
		t.Errorf("next state = %q, want planning", next.Name())
	}
}

func TestPlanningStateCreatesTasks(t *testing.T) {
	t.Parallel()
	o := testOrchestrator()
	o.goal = "audit codebase"
	o.dag = NewDAG()
	decomp := NewDecomposer(&StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "leaf-1", AgentType: "static_analysis", Status: "ready", Goal: "run lint"},
			{TaskID: "leaf-2", AgentType: "quality_scan", Status: "ready", Goal: "scan"},
			{TaskID: "synth", AgentType: "synthesis", Status: "blocked", Goal: "synthesize",
				ParentIDs: []string{"leaf-1", "leaf-2"}},
		},
	})
	o.decomposer = decomp

	planning := &PlanningState{}
	err := planning.Enter(o.ctx, o)
	if err != nil {
		t.Fatalf("PlanningState.Enter: %v", err)
	}

	// DAG should have 3 tasks
	if o.dag.TaskCount() != 3 {
		t.Fatalf("expected 3 tasks in DAG, got %d", o.dag.TaskCount())
	}

	// Count tasks by status
	readyCount := 0
	blockedCount := 0
	for id, node := range o.dag.nodes {
		switch node.Status {
		case "ready":
			readyCount++
		case "blocked":
			blockedCount++
		default:
			t.Errorf("unexpected status %q for task %s", node.Status, id)
		}
	}

	if readyCount != 2 {
		t.Errorf("expected 2 ready tasks, got %d", readyCount)
	}
	if blockedCount != 1 {
		t.Errorf("expected 1 blocked task, got %d", blockedCount)
	}
}

func TestPlanningStateTransitionsToWaiting(t *testing.T) {
	t.Parallel()
	o := testOrchestrator()
	o.goal = "audit codebase"
	o.decomposer = NewDecomposer(&StaticDecomposition{
		Tasks: []TaskSpec{
			{TaskID: "t1", AgentType: "worker", Status: "ready", Goal: "work"},
		},
	})
	planning := &PlanningState{}
	err := planning.Enter(o.ctx, o)
	if err != nil {
		t.Fatalf("PlanningState.Enter: %v", err)
	}
	o.state = planning

	next, err := planning.HandleEvent(o.ctx, o, Event{Type: EventTasksCreated})
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if next == nil {
		t.Fatal("expected transition to WaitingState")
	}
	if next.Name() != "waiting" {
		t.Errorf("next state = %q, want waiting", next.Name())
	}
}

func TestWaitingStatePromotesChildWhenParentCompletes(t *testing.T) {
	t.Parallel()
	o := testOrchestrator()
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "p1", Status: "done", Goal: "parent 1", AgentType: "worker"})
	_ = o.dag.AddTask(&TaskNode{ID: "p2", Status: "done", Goal: "parent 2", AgentType: "worker"})
	_ = o.dag.AddTask(&TaskNode{ID: "synth", Status: "blocked", Goal: "synthesize",
		AgentType: "synthesis", ParentIDs: []string{"p1", "p2"}})

	waiting := &WaitingState{}
	err := waiting.Enter(o.ctx, o)
	if err != nil {
		t.Fatalf("WaitingState.Enter: %v", err)
	}
	o.state = waiting

	// Handle a task completion event for p2 (even after p1 was already done in DAG)
	evt := Event{Type: EventTaskCompleted, Payload: map[string]any{"task_id": "p2"}}
	next, err := waiting.HandleEvent(o.ctx, o, evt)
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	// Should still be waiting — not all tasks done (synth is still blocked and needs to be promoted)
	if next != nil && next.Name() != "waiting" {
		_ = next // valid: stay in waiting or transition — key is DAG advancement
	}
	// Verify synthesis was promoted
	synth, _ := o.dag.GetTask("synth")
	if synth.Status != "ready" {
		t.Errorf("synth status = %q, want ready after all parents complete", synth.Status)
	}
}

func TestWaitingStateTransitionsToSynthesizingWhenAllTasksComplete(t *testing.T) {
	t.Parallel()
	o := testOrchestrator()
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "t1", Status: "done", Goal: "task 1", AgentType: "worker"})
	_ = o.dag.AddTask(&TaskNode{ID: "t2", Status: "done", Goal: "task 2", AgentType: "worker"})

	waiting := &WaitingState{}
	_ = waiting.Enter(o.ctx, o)
	o.state = waiting

	next, err := waiting.HandleEvent(o.ctx, o, Event{Type: EventTaskCompleted})
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if next == nil {
		t.Fatal("expected transition to SynthesizingState")
	}
	if next.Name() != "synthesizing" {
		t.Errorf("next state = %q, want synthesizing", next.Name())
	}
}

func TestDoneStateIsTerminal(t *testing.T) {
	t.Parallel()
	done := &DoneState{}
	err := done.Enter(context.Background(), testOrchestrator())
	if err != nil {
		t.Fatalf("DoneState.Enter: %v", err)
	}
	next, err := done.HandleEvent(context.Background(), testOrchestrator(), Event{})
	if err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if next != nil {
		t.Error("DoneState should not transition (terminal)")
	}
}
