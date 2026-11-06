package gaap

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/aj-nt/gaap/internal/status"
	vclient "github.com/aj-nt/vassago-sdk/client"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
// FailedState reports to a human on stdout, so the only way to test the report
// is to read what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

func failureFixture(t *testing.T, mode FailureMode) *Orchestrator {
	t.Helper()
	o := NewOrchestrator(context.Background(), &Config{RepoPath: "/tmp/x", FailureMode: mode}, &vclient.NullMnemo{}, nil)
	o.goal = "audit package"
	return o
}

// TestFailedStateCountsFailuresAndSkips is the report telling the truth: two
// tasks finished, one of them badly, one never ran at all.
func TestFailedStateCountsFailuresAndSkips(t *testing.T) {
	o := failureFixture(t, FailSoft)
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskDone, Summary: "found 2 issues"})
	_ = o.dag.AddTask(&TaskNode{ID: "b", Status: status.TaskFailed, Summary: "dead-lettered after 2 attempt(s)"})
	_ = o.dag.AddTask(&TaskNode{ID: "c", Status: status.TaskSkipped})

	out := captureStdout(t, func() {
		if err := (&FailedState{}).Enter(context.Background(), o); err != nil {
			t.Fatalf("Enter: %v", err)
		}
	})

	for _, want := range []string{"ORCHESTRATION FAILED", "Completed: 1", "Failed: 1", "Skipped: 1", "dead-lettered after 2 attempt(s)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Dead-lettered:") {
		t.Fatalf("report still speaks daemon vocabulary:\n%s", out)
	}
}

// TestSynthesisInputExcludesNonDoneTasks is the other half of the lie: a failed
// task must not appear in the synthesizer's input, and its goal must never be
// passed off as its result.
func TestSynthesisInputExcludesNonDoneTasks(t *testing.T) {
	o := failureFixture(t, FailSoft)
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskDone, Summary: "found 2 issues", AgentType: "static_analysis"})
	_ = o.dag.AddTask(&TaskNode{ID: "b", Status: status.TaskFailed, Goal: "run semgrep", AgentType: "static_analysis"})
	_ = o.dag.AddTask(&TaskNode{ID: "c", Status: status.TaskSkipped})

	results := o.collectSynthesisInput()

	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (only the completed task)", len(results))
	}
	if r, ok := results["a"]; !ok || r.Summary != "found 2 issues" {
		t.Fatalf("completed task wrong: %+v", results["a"])
	}
}

// TestSynthesisInputNeverPassesGoalAsSummary covers the completed-but-silent
// worker case: the report may say there is no summary, it may not invent one
// from the goal.
func TestSynthesisInputNeverPassesGoalAsSummary(t *testing.T) {
	o := failureFixture(t, FailSoft)
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskDone, Goal: "run semgrep", AgentType: "static_analysis"})

	r := o.collectSynthesisInput()["a"]
	if r == nil {
		t.Fatal("completed task missing")
	}
	if r.Summary == "run semgrep" {
		t.Fatal("goal was passed off as the summary")
	}
	if !strings.Contains(r.Summary, "no summary reported") || !strings.Contains(r.Summary, "run semgrep") {
		t.Fatalf("summary = %q, want an explicit no-summary marker carrying the goal as context", r.Summary)
	}
}

// TestWaitingStateTransitionsToFailedOnTerminalFailure is the state machine
// making the call, instead of the poll timeout discovering it later.
func TestWaitingStateTransitionsToFailedOnTerminalFailure(t *testing.T) {
	o := failureFixture(t, FailSoft)
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskFailed})
	_ = o.dag.AddTask(&TaskNode{ID: "b", ParentIDs: []string{"a"}, Status: status.TaskSkipped})

	next, err := (&WaitingState{}).HandleEvent(context.Background(), o, Event{Type: EventTaskFailed,
		Payload: map[string]any{"task_id": "a", "skipped": []string{"b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next.(*FailedState); !ok {
		t.Fatalf("next = %T, want *FailedState", next)
	}
}

// TestFailSoftStaysWaitingWhileWorkRemains pins the default: a failure in one
// branch does not end a run that still has work in flight.
func TestFailSoftStaysWaitingWhileWorkRemains(t *testing.T) {
	o := failureFixture(t, FailSoft)
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskFailed})
	_ = o.dag.AddTask(&TaskNode{ID: "b", Status: status.TaskClaimed})

	next, err := (&WaitingState{}).HandleEvent(context.Background(), o, Event{Type: EventTaskFailed,
		Payload: map[string]any{"task_id": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("next = %T, want nil (stay waiting)", next)
	}
}

// TestFailFastEndsAtFirstFailure is the opt-in: with fail_fast the same DAG
// ends immediately.
func TestFailFastEndsAtFirstFailure(t *testing.T) {
	o := failureFixture(t, FailFast)
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskFailed})
	_ = o.dag.AddTask(&TaskNode{ID: "b", Status: status.TaskClaimed})

	next, err := (&WaitingState{}).HandleEvent(context.Background(), o, Event{Type: EventTaskFailed,
		Payload: map[string]any{"task_id": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next.(*FailedState); !ok {
		t.Fatalf("next = %T, want *FailedState under fail_fast", next)
	}
}

// TestFailureModeDefaultsToFailSoft keeps the zero value safe: a config that
// never heard of failure modes finishes work already in flight.
func TestFailureModeDefaultsToFailSoft(t *testing.T) {
	for _, mode := range []FailureMode{"", "nonsense", FailSoft} {
		c := &Config{FailureMode: mode}
		if got := c.Mode(); got != FailSoft {
			t.Fatalf("Mode(%q) = %s, want fail_soft", mode, got)
		}
	}
	if got := (&Config{FailureMode: FailFast}).Mode(); got != FailFast {
		t.Fatalf("Mode(fail_fast) = %s, want fail_fast", got)
	}
}
