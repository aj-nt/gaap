package gaap

import (
	"testing"
)

func TestNewDAG(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	if dag == nil {
		t.Fatal("NewDAG returned nil")
	}
	if dag.TaskCount() != 0 {
		t.Errorf("expected 0 tasks, got %d", dag.TaskCount())
	}
}

func TestAddTaskLeaf(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	task := &TaskNode{ID: "task-1", Status: "ready"}
	err := dag.AddTask(task)
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if dag.TaskCount() != 1 {
		t.Errorf("expected 1 task, got %d", dag.TaskCount())
	}
	got, err := dag.GetTask("task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ID != "task-1" {
		t.Errorf("ID = %q, want task-1", got.ID)
	}
}

func TestAddTaskDuplicate(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_ = dag.AddTask(&TaskNode{ID: "dup", Status: "ready"})
	err := dag.AddTask(&TaskNode{ID: "dup", Status: "ready"})
	if err == nil {
		t.Error("expected error on duplicate task ID")
	}
}

func TestAddTaskWithParents(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_ = dag.AddTask(&TaskNode{ID: "parent-1", Status: "done"})
	_ = dag.AddTask(&TaskNode{ID: "parent-2", Status: "done"})
	child := &TaskNode{
		ID: "child", Status: "blocked",
		ParentIDs: []string{"parent-1", "parent-2"},
	}
	err := dag.AddTask(child)
	if err != nil {
		t.Fatalf("AddTask with parents: %v", err)
	}
	if dag.TaskCount() != 3 {
		t.Errorf("expected 3 tasks, got %d", dag.TaskCount())
	}
}

func TestChildrenOf(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_ = dag.AddTask(&TaskNode{ID: "leaf-1", Status: "ready"})
	_ = dag.AddTask(&TaskNode{ID: "leaf-2", Status: "ready"})
	_ = dag.AddTask(&TaskNode{
		ID: "synthesis", Status: "blocked",
		ParentIDs: []string{"leaf-1", "leaf-2"},
	})

	// Neither leaf has children directly recorded unless we track them.
	// ChildrenOf returns tasks that list this as a parent.
	children := dag.ChildrenOf("leaf-1")
	if len(children) != 1 {
		t.Fatalf("expected 1 child of leaf-1, got %d", len(children))
	}
	if children[0].ID != "synthesis" {
		t.Errorf("child ID = %q, want synthesis", children[0].ID)
	}
}

func TestChildrenOfNoChildren(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_ = dag.AddTask(&TaskNode{ID: "singleton", Status: "done"})
	children := dag.ChildrenOf("singleton")
	if len(children) != 0 {
		t.Errorf("expected 0 children, got %d", len(children))
	}
}

func TestAllParentsComplete(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_ = dag.AddTask(&TaskNode{ID: "p1", Status: "done"})
	_ = dag.AddTask(&TaskNode{ID: "p2", Status: "done"})
	_ = dag.AddTask(&TaskNode{
		ID: "c1", Status: "blocked",
		ParentIDs: []string{"p1", "p2"},
	})

	ok, err := dag.AllParentsComplete("c1")
	if err != nil {
		t.Fatalf("AllParentsComplete: %v", err)
	}
	if !ok {
		t.Error("expected all parents complete (both done)")
	}
}

func TestAllParentsCompleteNotReady(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_ = dag.AddTask(&TaskNode{ID: "p1", Status: "done"})
	_ = dag.AddTask(&TaskNode{ID: "p2", Status: "ready"}) // not done!
	_ = dag.AddTask(&TaskNode{
		ID: "c1", Status: "blocked",
		ParentIDs: []string{"p1", "p2"},
	})

	ok, err := dag.AllParentsComplete("c1")
	if err != nil {
		t.Fatalf("AllParentsComplete: %v", err)
	}
	if ok {
		t.Error("expected not all parents complete (p2 not done)")
	}
}

func TestAllParentsCompleteUnknownTask(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_, err := dag.AllParentsComplete("no-such-task")
	if err == nil {
		t.Error("expected error for unknown task")
	}
}

func TestPromoteToReady(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_ = dag.AddTask(&TaskNode{ID: "p1", Status: "done"})
	_ = dag.AddTask(&TaskNode{ID: "p2", Status: "done"})
	_ = dag.AddTask(&TaskNode{
		ID: "synthesis", Status: "blocked",
		ParentIDs: []string{"p1", "p2"},
	})

	err := dag.PromoteToReady("synthesis")
	if err != nil {
		t.Fatalf("PromoteToReady: %v", err)
	}
	task, _ := dag.GetTask("synthesis")
	if task.Status != "ready" {
		t.Errorf("status = %q, want ready", task.Status)
	}
}

func TestPromoteToReadyNotAllParentsDone(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_ = dag.AddTask(&TaskNode{ID: "p1", Status: "done"})
	_ = dag.AddTask(&TaskNode{ID: "p2", Status: "claimed"})
	_ = dag.AddTask(&TaskNode{
		ID: "synthesis", Status: "blocked",
		ParentIDs: []string{"p1", "p2"},
	})

	err := dag.PromoteToReady("synthesis")
	if err == nil {
		t.Error("expected error: not all parents done")
	}
}

func TestPromoteToReadyAlreadyReady(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_ = dag.AddTask(&TaskNode{ID: "t1", Status: "ready"})
	err := dag.PromoteToReady("t1")
	if err != nil {
		t.Fatalf("PromoteToReady on already-ready task should be a no-op: %v", err)
	}
}

func TestCircularDependency(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_ = dag.AddTask(&TaskNode{ID: "a", Status: "ready", ParentIDs: []string{"b"}})
	_ = dag.AddTask(&TaskNode{ID: "b", Status: "ready", ParentIDs: []string{"a"}})

	// Cycle detection: AddTask should detect when adding the second edge.
	// But AddTask already added both. We test at the DAG validation level.
	// For now, AllParentsComplete on either should detect the cycle.
	err := dag.Validate()
	if err == nil {
		t.Error("expected cycle detection error")
	}
}

func TestGetTaskUnknown(t *testing.T) {
	t.Parallel()
	dag := NewDAG()
	_, err := dag.GetTask("ghost")
	if err == nil {
		t.Error("expected error for unknown task")
	}
}
