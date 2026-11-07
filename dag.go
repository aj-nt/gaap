package gaap

import (
	"fmt"
	"sort"

	"github.com/aj-nt/gaap/internal/status"
)

// TaskNode represents a single task in the DAG with its status and parent dependencies.
type TaskNode struct {
	ID        string         `json:"task_id"`
	ParentIDs []string       `json:"parent_ids"`
	Status    status.Task    `json:"status"`
	Goal      string         `json:"goal"`
	AgentType string         `json:"agent_type"`
	Context   map[string]any `json:"context"`
	Findings  map[string]any `json:"findings,omitempty"` // structured findings from worker execution
	Summary   string         `json:"summary,omitempty"`  // natural-language summary from DONE: line (maps to worker ExecuteResult.Summary)

	// SupersededBy names the node that replaced this one after a human said
	// "try again". The status stays as it was: the task did fail, and that is
	// evidence about the agent type, not clutter to be cleaned up.
	SupersededBy string `json:"superseded_by,omitempty"`
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
		if parent.Status != status.TaskDone {
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
	if task.Status == status.TaskReady || task.Status == status.TaskDone {
		// Already promoted or completed — no-op success.
		return nil
	}
	if task.Status != status.TaskBlocked {
		return fmt.Errorf("task %s is %s, not blocked", id, task.Status)
	}
	allDone, err := d.AllParentsComplete(id)
	if err != nil {
		return err
	}
	if !allDone {
		return fmt.Errorf("not all parents complete for %s", id)
	}
	task.Status = status.TaskReady
	return nil
}

// AllTasksComplete returns true when all tasks in the DAG have status "done".
func (d *DAG) AllTasksComplete() bool {
	for _, node := range d.nodes {
		if node.Status != status.TaskDone {
			return false
		}
	}
	return true
}

// AllTasksTerminal returns true when no task can make further progress: every
// node is done, failed, or skipped (a descendant of a failed task that was
// never dispatched). Deliberately distinct from AllTasksComplete, which means
// "all succeeded" and gates the synthesizer.
func (d *DAG) AllTasksTerminal() bool {
	for _, node := range d.nodes {
		switch node.Status {
		case status.TaskDone, status.TaskFailed, status.TaskSkipped:
		default:
			return false
		}
	}
	return true
}

// HasFailures reports whether any task failed.
func (d *DAG) HasFailures() bool {
	for _, node := range d.nodes {
		if node.Status == status.TaskFailed {
			return true
		}
	}
	return false
}

// FailedIDs returns the IDs of failed tasks, sorted for stable output.
func (d *DAG) FailedIDs() []string {
	var out []string
	for id, node := range d.nodes {
		if node.Status == status.TaskFailed {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// SkipDescendants marks every task reachable from id as skipped and returns
// the IDs it changed, sorted. A descendant of a failed task will never run, and
// leaving it blocked would force the run to end by poll timeout rather than by
// fact. Done tasks keep their status: completed work is evidence, not cleanup.
// Skips are DAG-local and are never dispatched to the daemon.
func (d *DAG) SkipDescendants(id string) []string {
	var out []string
	seen := map[string]bool{id: true}
	queue := append([]string(nil), d.children[id]...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if node, ok := d.nodes[cur]; ok {
			switch node.Status {
			case status.TaskDone, status.TaskFailed:
				// Keep the story it already earned.
			default:
				node.Status = status.TaskSkipped
				out = append(out, cur)
			}
		}
		queue = append(queue, d.children[cur]...)
	}
	sort.Strings(out)
	return out
}

// Supersede records that failedID was replaced by replacement.
//
// A dead-lettered task is terminal in the daemon — vassago has no "reopen" — so
// a human's "try again" is a new node standing where the old one stood. The
// replacement inherits the failed node's parents, the failed node keeps its
// status (it did fail, and that is evidence about the agent type), and the
// descendants that were skipped are re-opened as blocked so they can run when
// the replacement completes. Re-opened descendants are returned, sorted.
//
// This is the DAG-local half of the decision: it moves nodes, it does not talk
// to the daemon. Dispatching the replacement is the caller's job.
func (d *DAG) Supersede(failedID string, replacement *TaskNode) ([]string, error) {
	failed, ok := d.nodes[failedID]
	if !ok {
		return nil, fmt.Errorf("supersede: unknown task %q", failedID)
	}
	if failed.SupersededBy != "" {
		return nil, fmt.Errorf("supersede: task %q was already replaced by %q", failedID, failed.SupersededBy)
	}
	if replacement == nil || replacement.ID == "" {
		return nil, fmt.Errorf("supersede: the replacement needs an ID")
	}
	if _, exists := d.nodes[replacement.ID]; exists {
		return nil, fmt.Errorf("supersede: task %q already exists", replacement.ID)
	}

	// The replacement stands where the failure stood. Copying rather than
	// aliasing: the failed node's parent slice must not be shared, or a later
	// edit to one would silently rewrite the other.
	replacement.ParentIDs = append([]string(nil), failed.ParentIDs...)
	replacement.Status = status.TaskBlocked
	d.nodes[replacement.ID] = replacement
	for _, parentID := range replacement.ParentIDs {
		d.children[parentID] = append(d.children[parentID], replacement.ID)
	}

	// A descendant of the failed node is a descendant of its replacement now.
	// Without the reparent, a re-opened task waits for a parent that will never
	// complete, and the run ends by timeout instead of by the replacement's work.
	for _, child := range d.children[failedID] {
		if node, ok := d.nodes[child]; ok {
			for i, parentID := range node.ParentIDs {
				if parentID == failedID {
					node.ParentIDs[i] = replacement.ID
				}
			}
		}
	}
	d.children[replacement.ID] = append(d.children[replacement.ID], d.children[failedID]...)
	delete(d.children, failedID)

	failed.SupersededBy = replacement.ID

	// Re-open what the failure took out of play. A task that completed before
	// the failure keeps its status: it is evidence, not cleanup.
	var reopened []string
	seen := map[string]bool{failedID: true}
	queue := append([]string(nil), d.children[replacement.ID]...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if node, ok := d.nodes[cur]; ok && node.Status == status.TaskSkipped {
			node.Status = status.TaskBlocked
			reopened = append(reopened, cur)
		}
		queue = append(queue, d.children[cur]...)
	}
	sort.Strings(reopened)

	// If the parents are already finished, the replacement is ready now. A
	// replacement left blocked with all parents done would never run.
	if allDone, err := d.AllParentsComplete(replacement.ID); err == nil && allDone {
		replacement.Status = status.TaskReady
	}

	return reopened, nil
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
