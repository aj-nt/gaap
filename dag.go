package gaap

import (
	"fmt"
)

// TaskNode represents a single task in the DAG with its status and parent dependencies.
type TaskNode struct {
	ID        string         `json:"task_id"`
	ParentIDs []string       `json:"parent_ids"`
	Status    string         `json:"status"`
	Goal      string         `json:"goal"`
	AgentType string         `json:"agent_type"`
	Context   map[string]any `json:"context"`
	Findings  map[string]any `json:"findings,omitempty"` // structured findings from worker execution
	Summary   string         `json:"summary,omitempty"`  // natural-language summary from DONE: line (maps to worker ExecuteResult.Summary)
}

// DAG is a directed acyclic graph of tasks with dependency tracking.
// Tasks advance from blocked → ready → claimed → done based on parent completion.
type DAG struct {
	nodes    map[string]*TaskNode
	children map[string][]string // parent → list of child IDs
}

// NewDAG creates an empty task DAG.
func NewDAG() *DAG {
	return &DAG{
		nodes:    make(map[string]*TaskNode),
		children: make(map[string][]string),
	}
}

// TaskCount returns the number of tasks in the DAG.
func (d *DAG) TaskCount() int {
	return len(d.nodes)
}

// AddTask adds a task to the DAG. Returns an error if a task with the same ID
// already exists.
func (d *DAG) AddTask(task *TaskNode) error {
	if task.ID == "" {
		return fmt.Errorf("task ID is required")
	}
	if _, exists := d.nodes[task.ID]; exists {
		return fmt.Errorf("duplicate task ID: %s", task.ID)
	}
	d.nodes[task.ID] = task
	for _, parentID := range task.ParentIDs {
		d.children[parentID] = append(d.children[parentID], task.ID)
	}
	return nil
}

// GetTask returns the task with the given ID, or an error if not found.
func (d *DAG) GetTask(id string) (*TaskNode, error) {
	task, ok := d.nodes[id]
	if !ok {
		return nil, fmt.Errorf("task not found: %s", id)
	}
	return task, nil
}

// ChildrenOf returns all tasks that list the given task ID as a parent.
func (d *DAG) ChildrenOf(parentID string) []*TaskNode {
	childIDs := d.children[parentID]
	result := make([]*TaskNode, 0, len(childIDs))
	for _, id := range childIDs {
		if task, ok := d.nodes[id]; ok {
			result = append(result, task)
		}
	}
	return result
}

// AllParentsComplete returns true if all parents of the given task have
// status "done".
func (d *DAG) AllParentsComplete(id string) (bool, error) {
	task, ok := d.nodes[id]
	if !ok {
		return false, fmt.Errorf("task not found: %s", id)
	}
	for _, parentID := range task.ParentIDs {
		parent, ok := d.nodes[parentID]
		if !ok {
			return false, fmt.Errorf("parent task not found: %s", parentID)
		}
		if parent.Status != "done" {
			return false, nil
		}
	}
	return true, nil
}

// PromoteToReady promotes a task from "blocked" to "ready" if all of its
// parents have completed. Returns an error if not all parents are done or
// the task is not in "blocked" status.
func (d *DAG) PromoteToReady(id string) error {
	task, ok := d.nodes[id]
	if !ok {
		return fmt.Errorf("task not found: %s", id)
	}
	if task.Status == "ready" || task.Status == "done" {
		// Already promoted or completed — no-op success.
		return nil
	}
	if task.Status != "blocked" {
		return fmt.Errorf("task %s is %s, not blocked", id, task.Status)
	}
	allDone, err := d.AllParentsComplete(id)
	if err != nil {
		return err
	}
	if !allDone {
		return fmt.Errorf("not all parents complete for %s", id)
	}
	task.Status = "ready"
	return nil
}

// AllTasksComplete returns true when all tasks in the DAG have status "done".
func (d *DAG) AllTasksComplete() bool {
	for _, node := range d.nodes {
		if node.Status != "done" {
			return false
		}
	}
	return true
}

// Validate checks the DAG for structural issues. Currently detects circular
// dependencies via DFS.
func (d *DAG) Validate() error {
	// Detect cycles using DFS with recursion stack tracking.
	visited := make(map[string]bool)
	inStack := make(map[string]bool)

	var dfs func(id string) error
	dfs = func(id string) error {
		if inStack[id] {
			return fmt.Errorf("circular dependency detected at task %s", id)
		}
		if visited[id] {
			return nil
		}
		visited[id] = true
		inStack[id] = true
		for _, childID := range d.children[id] {
			if err := dfs(childID); err != nil {
				return err
			}
		}
		inStack[id] = false
		return nil
	}

	for id := range d.nodes {
		if err := dfs(id); err != nil {
			return err
		}
	}

	return nil
}
