package gaap

import (
	"testing"

	"github.com/aj-nt/gaap/internal/status"
)

// TestAllTasksTerminal pins the difference the polling loop depends on:
// "finished one way or another" is not the same question as "everything
// succeeded". Before this, a dead-lettered task left the DAG non-complete and
// the only exit was the 300s poll timeout.
func TestAllTasksTerminal(t *testing.T) {
	d := NewDAG()
	_ = d.AddTask(&TaskNode{ID: "a", Status: status.TaskDone})
	_ = d.AddTask(&TaskNode{ID: "b", Status: status.TaskFailed})
	_ = d.AddTask(&TaskNode{ID: "c", Status: status.TaskSkipped})

	if !d.AllTasksTerminal() {
		t.Fatal("done+failed+skipped must be terminal")
	}
	if d.AllTasksComplete() {
		t.Fatal("a failed task is not complete — the two predicates must differ here")
	}

	_ = d.AddTask(&TaskNode{ID: "d", Status: status.TaskBlocked})
	if d.AllTasksTerminal() {
		t.Fatal("a blocked task is not terminal")
	}
}

func TestFailedIDs(t *testing.T) {
	d := NewDAG()
	_ = d.AddTask(&TaskNode{ID: "b", Status: status.TaskFailed})
	_ = d.AddTask(&TaskNode{ID: "a", Status: status.TaskFailed})
	_ = d.AddTask(&TaskNode{ID: "c", Status: status.TaskDone})

	got := d.FailedIDs()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("FailedIDs = %v, want sorted [a b]", got)
	}
	if !d.HasFailures() {
		t.Fatal("HasFailures must be true")
	}
}

// TestSkipDescendants covers the reason skipped exists at all: a descendant of
// a failed task can never run, and leaving it blocked forces the run to end by
// timeout instead of by fact.
func TestSkipDescendants(t *testing.T) {
	d := NewDAG()
	_ = d.AddTask(&TaskNode{ID: "a", Status: status.TaskFailed})
	_ = d.AddTask(&TaskNode{ID: "b", ParentIDs: []string{"a"}, Status: status.TaskBlocked})
	_ = d.AddTask(&TaskNode{ID: "c", ParentIDs: []string{"b"}, Status: status.TaskBlocked})
	_ = d.AddTask(&TaskNode{ID: "d", Status: status.TaskReady}) // unrelated branch

	skipped := d.SkipDescendants("a")

	if len(skipped) != 2 || skipped[0] != "b" || skipped[1] != "c" {
		t.Fatalf("skipped = %v, want [b c]", skipped)
	}
	if d.nodes["b"].Status != status.TaskSkipped || d.nodes["c"].Status != status.TaskSkipped {
		t.Fatalf("descendants = %s/%s, want skipped/skipped", d.nodes["b"].Status, d.nodes["c"].Status)
	}
	if d.nodes["d"].Status != status.TaskReady {
		t.Fatalf("unrelated branch = %s, want ready (fail-soft lets it run)", d.nodes["d"].Status)
	}
	if d.nodes["a"].Status != status.TaskFailed {
		t.Fatalf("failed task = %s, must keep its own story", d.nodes["a"].Status)
	}
}

// TestSkipDescendantsKeepsCompletedWork guards the partial-report path: a
// descendant that somehow already completed is evidence, not cleanup.
func TestSkipDescendantsKeepsCompletedWork(t *testing.T) {
	d := NewDAG()
	_ = d.AddTask(&TaskNode{ID: "a", Status: status.TaskFailed})
	_ = d.AddTask(&TaskNode{ID: "b", ParentIDs: []string{"a"}, Status: status.TaskDone})

	if skipped := d.SkipDescendants("a"); len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none — b already ran", skipped)
	}
	if d.nodes["b"].Status != status.TaskDone {
		t.Fatalf("b = %s, want done", d.nodes["b"].Status)
	}
}
