package gaap

import (
	"context"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/status"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// TestBreakersDisabledByDefault: a breaker stops dispatch mid-run, which is not
// a thing that should happen because nobody configured it.
func TestBreakersDisabledByDefault(t *testing.T) {
	o := NewOrchestrator(context.Background(), &Config{}, nil, nil)
	if o.breakerRegistry != nil {
		t.Fatalf("breakerRegistry = %v, want nil with no threshold configured", o.breakerRegistry)
	}
	if cb := o.breakerFor("coder"); cb != nil {
		t.Fatalf("breakerFor = %v, want nil when breakers are off", cb)
	}
}

func TestBreakersBuiltWhenConfigured(t *testing.T) {
	o := NewOrchestrator(context.Background(), &Config{BreakerThreshold: 2}, nil, nil)
	cb := o.breakerFor("coder")
	if cb == nil {
		t.Fatal("breakerFor returned nil with a threshold configured")
	}
	if cb.State() != StateClosed {
		t.Fatalf("state = %q, want closed", cb.State())
	}
	if o.cfg.BreakerCooldownSec != 60 {
		t.Fatalf("cooldown = %d, want the 60s default", o.cfg.BreakerCooldownSec)
	}
	if again := o.breakerFor("coder"); again != cb {
		t.Fatal("breakerFor created a second breaker for the same agent type")
	}
}

// TestBreakerOpensOnDeadLetters is the wiring from the failure path: before this,
// nothing ever called RecordFailure, so the breaker could not open however many
// tasks the daemon gave up on.
func TestBreakerOpensOnDeadLetters(t *testing.T) {
	o := NewOrchestrator(context.Background(), &Config{BreakerThreshold: 2, BreakerCooldownSec: 60}, nil, nil)
	o.dag.nodes["t1"] = &TaskNode{ID: "t1", AgentType: "coder", Status: status.TaskClaimed}

	o.handleTaskEvent(taskEvent{ID: "t1", Status: status.TaskDeadLetter, Source: "test"})
	if got := o.breakerFor("coder").State(); got != StateClosed {
		t.Fatalf("state after 1 failure = %q, want closed (threshold is 2)", got)
	}

	o.dag.nodes["t2"] = &TaskNode{ID: "t2", AgentType: "coder", Status: status.TaskClaimed}
	o.handleTaskEvent(taskEvent{ID: "t2", Status: status.TaskDeadLetter, Source: "test"})
	if got := o.breakerFor("coder").State(); got != StateOpen {
		t.Fatalf("state after 2 failures = %q, want open", got)
	}
	if o.breakerFor("coder").AllowRequest() {
		t.Fatal("an open breaker allowed a request inside the cooldown")
	}
}

// TestBreakerClosesAfterASuccessfulProbe: the breaker has to be able to come
// back, or a transient outage stops the agent type for the rest of the run.
func TestBreakerClosesAfterASuccessfulProbe(t *testing.T) {
	o := NewOrchestrator(context.Background(), &Config{BreakerThreshold: 1, BreakerCooldownSec: 1}, nil, nil)
	// A short-cooldown breaker directly, so the test does not sleep for a second.
	cb := NewCircuitBreaker("coder", 1, 10*time.Millisecond)
	o.breakerRegistry["coder"] = cb
	o.dag.nodes["t1"] = &TaskNode{ID: "t1", AgentType: "coder", Status: status.TaskClaimed}
	o.handleTaskEvent(taskEvent{ID: "t1", Status: status.TaskDeadLetter, Source: "test"})

	if cb.State() != StateOpen {
		t.Fatalf("state = %q, want open", cb.State())
	}
	time.Sleep(20 * time.Millisecond) // let the cooldown elapse
	if !cb.AllowRequest() {
		t.Fatal("breaker did not reach half-open after its cooldown")
	}
	o.dag.nodes["t2"] = &TaskNode{ID: "t2", AgentType: "coder", Status: status.TaskClaimed}
	o.handleTaskEvent(taskEvent{ID: "t2", Status: status.TaskDone, Source: "test"})
	if cb.State() != StateClosed {
		t.Fatalf("state = %q, want closed after a successful probe", cb.State())
	}
}

// TestPlanningSkipsDispatchWhenBreakerOpen drives the guard that was unreachable
// in production: the planning phase must not hand work to an agent type whose
// breaker is open.
func TestPlanningSkipsDispatchWhenBreakerOpen(t *testing.T) {
	td := &testDaemon{tasks: make(map[string]*pb.TaskEntry)}
	client, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := &Config{BreakerThreshold: 1, BreakerCooldownSec: 60, RepoPath: t.TempDir()}
	o := NewOrchestrator(context.Background(), cfg, client, NewDecomposer(NewStaticDecomposition([]TaskSpec{
		{TaskID: "t1", AgentType: "coder", Status: status.TaskReady, Goal: "do the thing"},
	})))
	o.goal = "do the thing"
	o.breakerFor("coder").RecordFailure() // threshold 1 → open

	if err := (&PlanningState{}).Enter(context.Background(), o); err != nil {
		t.Fatalf("planning: %v", err)
	}

	td.mu.Lock()
	dispatched := len(td.tasks)
	td.mu.Unlock()
	if dispatched != 0 {
		t.Fatalf("dispatched %d tasks to a tripped agent type, want 0", dispatched)
	}
}
