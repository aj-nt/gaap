package gaap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/status"
	vclient "github.com/aj-nt/vassago-sdk/client"
)

// deadLetterMnemo answers GetTask from a table. Tasks the table does not know
// about report as claimed, so a poll does not mistake them for completed work.
type deadLetterMnemo struct {
	vclient.NullMnemo
	entries map[string]*vclient.TaskEntry
}

func (m *deadLetterMnemo) GetTask(_ context.Context, id string) (*vclient.TaskEntry, error) {
	if e, ok := m.entries[id]; ok {
		return e, nil
	}
	return &vclient.TaskEntry{Id: id, Status: string(status.TaskClaimed)}, nil
}

func deadLetterOrchestrator(t *testing.T, m *deadLetterMnemo) *Orchestrator {
	t.Helper()
	o := NewOrchestrator(context.Background(), &Config{RepoPath: "/tmp/x", MaxWaitSec: 5, PollIntervalSec: 1}, m, nil)
	o.goal = "audit package"
	o.state = &WaitingState{}
	return o
}

// TestPollOnceMarksDeadLetterFailed is the regression test for the whole
// change: the daemon's dead_letter means "gave up after N retries", and it was
// being written into the DAG as done.
func TestPollOnceMarksDeadLetterFailed(t *testing.T) {
	m := &deadLetterMnemo{entries: map[string]*vclient.TaskEntry{
		"a": {Id: "a", Status: string(status.TaskDeadLetter), RetryCount: 2},
	}}
	o := deadLetterOrchestrator(t, m)
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskClaimed, AgentType: "static_analysis"})
	_ = o.dag.AddTask(&TaskNode{ID: "b", ParentIDs: []string{"a"}, Status: status.TaskBlocked, AgentType: "static_analysis"})

	o.pollOnce()

	if got := o.dag.nodes["a"].Status; got != status.TaskFailed {
		t.Fatalf("a = %s, want failed", got)
	}
	if got := o.dag.nodes["b"].Status; got != status.TaskSkipped {
		t.Fatalf("b = %s, want skipped", got)
	}
	if !o.dag.AllTasksTerminal() {
		t.Fatal("DAG must be terminal once the only branch failed")
	}
	if o.dag.AllTasksComplete() {
		t.Fatal("a failed task must never satisfy AllTasksComplete")
	}
}

// TestPollOnceKeepsSuccessfulNeighbour checks fail-soft at the DAG level: a
// failure in one branch does not disturb a branch that is still running.
func TestPollOnceKeepsSuccessfulNeighbour(t *testing.T) {
	m := &deadLetterMnemo{entries: map[string]*vclient.TaskEntry{
		"a": {Id: "a", Status: string(status.TaskDeadLetter)},
	}}
	o := deadLetterOrchestrator(t, m)
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskClaimed})
	_ = o.dag.AddTask(&TaskNode{ID: "b", Status: status.TaskClaimed})

	o.pollOnce()

	if got := o.dag.nodes["b"].Status; got != status.TaskClaimed {
		t.Fatalf("b = %s, want claimed (independent branch keeps running)", got)
	}
	if o.dag.AllTasksTerminal() {
		t.Fatal("DAG is not terminal while b is still in flight")
	}
}

// TestSubscriptionMarksDeadLetterFailed covers the push path, which carried the
// same mapping and would otherwise have been fixed in only one of two places.
func TestSubscriptionMarksDeadLetterFailed(t *testing.T) {
	o := deadLetterOrchestrator(t, &deadLetterMnemo{})
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskClaimed, AgentType: "static_analysis"})
	_ = o.dag.AddTask(&TaskNode{ID: "b", ParentIDs: []string{"a"}, Status: status.TaskBlocked})

	o.handleTaskEvent(taskEvent{ID: "a", Status: status.TaskDeadLetter, RetryCount: 1, Source: "sub"})

	if got := o.dag.nodes["a"].Status; got != status.TaskFailed {
		t.Fatalf("a = %s, want failed", got)
	}
	if got := o.dag.nodes["b"].Status; got != status.TaskSkipped {
		t.Fatalf("b = %s, want skipped", got)
	}
}

// TestSubscriptionIgnoresUnknownTask keeps the extracted handler honest about
// events for tasks this orchestrator does not own.
func TestSubscriptionIgnoresUnknownTask(t *testing.T) {
	o := deadLetterOrchestrator(t, &deadLetterMnemo{})
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskClaimed})

	o.handleTaskEvent(taskEvent{ID: "zzz", Status: status.TaskDeadLetter, Source: "sub"})

	if got := o.dag.nodes["a"].Status; got != status.TaskClaimed {
		t.Fatalf("a = %s, want untouched", got)
	}
}

// TestPollDaemonReturnsOnFailure pins the timeout question: a terminal DAG must
// end the poll loop, not leave it sleeping until MaxWaitSec.
func TestPollDaemonReturnsOnFailure(t *testing.T) {
	m := &deadLetterMnemo{entries: map[string]*vclient.TaskEntry{
		"a": {Id: "a", Status: string(status.TaskDeadLetter)},
	}}
	o := deadLetterOrchestrator(t, m)
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskClaimed})

	done := make(chan struct{})
	go func() {
		o.pollDaemon()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pollDaemon waited on a terminal DAG — it is still gated on AllTasksComplete")
	}
}

// TestRunReturnsErrorWhenOrchestrationFails makes a failed run visible to a
// script. Before this, a run whose only task dead-lettered exited 0.
func TestRunReturnsErrorWhenOrchestrationFails(t *testing.T) {
	m := &deadLetterMnemo{entries: map[string]*vclient.TaskEntry{
		"a": {Id: "a", Status: string(status.TaskDeadLetter)},
	}}
	o := deadLetterOrchestrator(t, m)
	_ = o.dag.AddTask(&TaskNode{ID: "a", Status: status.TaskClaimed})

	err := o.runWaitingAndBeyond()
	if !errors.Is(err, ErrOrchestrationFailed) {
		t.Fatalf("err = %v, want ErrOrchestrationFailed", err)
	}
}
