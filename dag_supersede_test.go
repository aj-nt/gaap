package gaap

import (
	"testing"

	"github.com/aj-nt/gaap/internal/status"
)

// supersedeFixture is a three-level DAG: root -> mid -> leaf.
func supersedeFixture() *DAG {
	d := NewDAG()
	_ = d.AddTask(&TaskNode{ID: "root", Status: status.TaskDone})
	_ = d.AddTask(&TaskNode{ID: "mid", Status: status.TaskFailed, ParentIDs: []string{"root"}})
	_ = d.AddTask(&TaskNode{ID: "leaf", Status: status.TaskSkipped, ParentIDs: []string{"mid"}})
	return d
}

// TestSupersedeInheritsParents: the replacement stands where the failure stood,
// so the rest of the graph does not have to be rewritten.
func TestSupersedeInheritsParents(t *testing.T) {
	t.Parallel()

	d := supersedeFixture()
	replacement := &TaskNode{ID: "mid_retry", AgentType: "quality_scan", Goal: "again"}

	reopened, err := d.Supersede("mid", replacement)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	got, _ := d.GetTask("mid_retry")
	if len(got.ParentIDs) != 1 || got.ParentIDs[0] != "root" {
		t.Errorf("replacement parents = %v, want [root]", got.ParentIDs)
	}
	if got.Status != status.TaskReady {
		t.Errorf("replacement status = %q, want ready (its parent is done)", got.Status)
	}
	if len(reopened) != 1 || reopened[0] != "leaf" {
		t.Errorf("reopened = %v, want [leaf]", reopened)
	}
}

// TestSupersedeKeepsTheFailure: the failed node did fail. That is evidence, and
// superseding it must not rewrite the record of what happened.
func TestSupersedeKeepsTheFailure(t *testing.T) {
	t.Parallel()

	d := supersedeFixture()
	if _, err := d.Supersede("mid", &TaskNode{ID: "mid_retry"}); err != nil {
		t.Fatal(err)
	}

	failed, _ := d.GetTask("mid")
	if failed.Status != status.TaskFailed {
		t.Errorf("failed node status = %q, want failed", failed.Status)
	}
	if failed.SupersededBy != "mid_retry" {
		t.Errorf("SupersededBy = %q, want mid_retry", failed.SupersededBy)
	}
}

// TestSupersedeReparentsDescendants: a skipped descendant has to hang off the
// replacement now, or it can never be promoted and the run ends by timeout.
func TestSupersedeReparentsDescendants(t *testing.T) {
	t.Parallel()

	d := supersedeFixture()
	if _, err := d.Supersede("mid", &TaskNode{ID: "mid_retry"}); err != nil {
		t.Fatal(err)
	}

	leaf, _ := d.GetTask("leaf")
	if leaf.Status != status.TaskBlocked {
		t.Errorf("leaf status = %q, want blocked (re-opened)", leaf.Status)
	}
	if len(leaf.ParentIDs) != 1 || leaf.ParentIDs[0] != "mid_retry" {
		t.Errorf("leaf parents = %v, want [mid_retry]", leaf.ParentIDs)
	}
	if kids := d.ChildrenOf("mid_retry"); len(kids) != 1 || kids[0].ID != "leaf" {
		t.Errorf("replacement children = %v, want [leaf]", ids(kids))
	}

	// The replacement completing is what promotes the re-opened descendant.
	mid, _ := d.GetTask("mid_retry")
	mid.Status = status.TaskDone
	if err := d.PromoteToReady("leaf"); err != nil {
		t.Fatalf("re-opened descendant should be promotable after the replacement: %v", err)
	}
}

// TestSupersedeDoesNotReopenDoneWork: a descendant that had already completed
// before the failure is evidence, not cleanup.
func TestSupersedeDoesNotReopenDoneWork(t *testing.T) {
	t.Parallel()

	d := NewDAG()
	_ = d.AddTask(&TaskNode{ID: "root", Status: status.TaskDone})
	_ = d.AddTask(&TaskNode{ID: "mid", Status: status.TaskFailed, ParentIDs: []string{"root"}})
	_ = d.AddTask(&TaskNode{ID: "sibling", Status: status.TaskDone, ParentIDs: []string{"mid"}})

	reopened, err := d.Supersede("mid", &TaskNode{ID: "mid_retry"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened) != 0 {
		t.Errorf("reopened = %v, want none", reopened)
	}
	sib, _ := d.GetTask("sibling")
	if sib.Status != status.TaskDone {
		t.Errorf("completed descendant status = %q, want done", sib.Status)
	}
}

// TestSupersedeErrors: the three ways a supersede is wrong.
func TestSupersedeErrors(t *testing.T) {
	t.Parallel()

	d := supersedeFixture()
	if _, err := d.Supersede("nope", &TaskNode{ID: "x"}); err == nil {
		t.Error("superseding an unknown task should error")
	}
	if _, err := d.Supersede("mid", nil); err == nil {
		t.Error("a nil replacement should error")
	}
	if _, err := d.Supersede("mid", &TaskNode{ID: "leaf"}); err == nil {
		t.Error("a replacement that collides with an existing ID should error")
	}
	if _, err := d.Supersede("mid", &TaskNode{ID: "mid_retry"}); err != nil {
		t.Fatalf("first supersede should work: %v", err)
	}
	if _, err := d.Supersede("mid", &TaskNode{ID: "mid_retry_2"}); err == nil {
		t.Error("superseding the same node twice should error")
	}
}

// TestSupersedeOnRunningParentsLeavesReplacementBlocked: when the failed node's
// parents are not finished, the replacement waits like any other blocked task.
func TestSupersedeOnRunningParentsLeavesReplacementBlocked(t *testing.T) {
	t.Parallel()

	d := NewDAG()
	_ = d.AddTask(&TaskNode{ID: "root", Status: status.TaskReady})
	_ = d.AddTask(&TaskNode{ID: "mid", Status: status.TaskFailed, ParentIDs: []string{"root"}})

	if _, err := d.Supersede("mid", &TaskNode{ID: "mid_retry"}); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetTask("mid_retry")
	if got.Status != status.TaskBlocked {
		t.Errorf("replacement status = %q, want blocked while its parent runs", got.Status)
	}
}

func ids(nodes []*TaskNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}
