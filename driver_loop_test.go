package gaap

import (
	"context"
	"reflect"
	"testing"

	"github.com/aj-nt/gaap/internal/status"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// TestPromotedChildIsDispatched is the regression test for a run that could
// never finish: planning dispatches roots, and promotion used to change a
// child's status to ready without telling the daemon. Every multi-level DAG
// therefore sat at ready until the wait timed out and the run reported failure,
// with the daemon logging "GetTask: task not found" for a task it had never been
// given.
func TestPromotedChildIsDispatched(t *testing.T) {
	td := &testDaemon{tasks: make(map[string]*pb.TaskEntry), autoComplete: true}
	client, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := &Config{PollIntervalSec: 1, MaxWaitSec: 5, RepoPath: t.TempDir()}
	o := NewOrchestrator(context.Background(), cfg, client, NewDecomposer(NewStaticDecomposition([]TaskSpec{
		{TaskID: "t1", AgentType: "coder", Status: status.TaskReady, Goal: "first"},
		{TaskID: "t2", ParentIDs: []string{"t1"}, AgentType: "coder", Status: status.TaskBlocked, Goal: "second"},
	})))

	if err := o.Run("two-level"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	td.mu.Lock()
	names := make([]string, 0, len(td.tasks))
	for id := range td.tasks {
		names = append(names, id)
	}
	td.mu.Unlock()

	if len(names) != 2 {
		t.Fatalf("daemon has %d tasks, want 2 (the root and the promoted child): %v", len(names), names)
	}
	if got := nodeEndingIn(t, o.dag, "t2").Status; got != status.TaskDone {
		t.Fatalf("child = %s, want done", got)
	}
}

// TestRunFollowsThePhaseGraph: a completed run entered exactly the phases the
// transition table allows, in order. This is what "driven by events" means in
// practice — no phase is entered by the driver inspecting the previous one.
func TestRunFollowsThePhaseGraph(t *testing.T) {
	td := &testDaemon{tasks: make(map[string]*pb.TaskEntry), autoComplete: true}
	client, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := &Config{PollIntervalSec: 1, MaxWaitSec: 5, RepoPath: t.TempDir()}
	o := NewOrchestrator(context.Background(), cfg, client, NewDecomposer(NewStaticDecomposition([]TaskSpec{
		{TaskID: "t1", AgentType: "coder", Status: status.TaskReady, Goal: "only"},
	})))

	if err := o.Run("single task"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []Phase{PhasePlanning, PhaseWaiting, PhaseSynthesizing, PhaseDone}
	if got := o.PhaseHistory(); !reflect.DeepEqual(got, want) {
		t.Fatalf("phase history = %v, want %v", got, want)
	}
}

// TestRunIsSingleWriter is the driver's half of the race fix: the watcher
// goroutines only post, so under -race a run with many concurrent task updates
// must not report a DAG write from anywhere but the loop.
func TestRunIsSingleWriter(t *testing.T) {
	td := &testDaemon{tasks: make(map[string]*pb.TaskEntry), autoComplete: true}
	client, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	specs := make([]TaskSpec, 0, 12)
	for i := 0; i < 12; i++ {
		specs = append(specs, TaskSpec{
			TaskID:    "leaf-" + string(rune('a'+i)),
			AgentType: "coder",
			Status:    status.TaskReady,
			Goal:      "work",
		})
	}

	cfg := &Config{PollIntervalSec: 1, MaxWaitSec: 10, RepoPath: t.TempDir()}
	o := NewOrchestrator(context.Background(), cfg, client, NewDecomposer(NewStaticDecomposition(specs)))
	if err := o.Run("wide"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := o.dag.TaskCount(); got != 12 {
		t.Fatalf("task count = %d, want 12", got)
	}
	if !o.dag.AllTasksComplete() {
		t.Fatalf("not all tasks completed: %v", o.dag.FailedIDs())
	}
}
