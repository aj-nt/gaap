package gaap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aj-nt/gaap/internal/status"
	pb "github.com/aj-nt/vassago-sdk/proto"
)

// RunState and its round trip through the daemon.
//
// A run that survives a crash is a run whose shape was written down before it
// moved: the DAG, the policy it started under, and the question it was waiting
// on. Everything here is about that record — writing it after each transition,
// finding it again by key, and rebuilding a DAG from it.

// RunState is a serializable snapshot of an orchestrator run that can be
// stored to and loaded from the daemon. It captures just enough to rebuild
// the DAG and resume from the Waiting state after an orchestrator crash.
type RunState struct {
	Goal     string `json:"goal"`
	RepoPath string `json:"repo_path"`
	// RunID is the task-id namespace, so a resumed run keeps polling for the
	// tasks it left behind instead of re-namespacing a new set.
	RunID       string      `json:"run_id,omitempty"`
	Tasks       []TaskSpec  `json:"tasks"`
	FailureMode FailureMode `json:"failure_mode,omitempty"`
	// ApprovalPolicy is the loop the run started with. Persisted for the same
	// reason as FailureMode: a run that asked humans on Monday must not stop
	// asking because it was resumed on Tuesday without the flag.
	ApprovalPolicy ApprovalPolicy   `json:"approval_policy,omitempty"`
	Pending        *PendingApproval `json:"pending_approval,omitempty"`
}

// PendingApproval is the question a run was waiting on when it died. Persisted
// with the run so a resume adopts the request already in the daemon rather than
// asking it again — and keeps its original deadline, because a restart is not a
// reason to give the human more time.
type PendingApproval struct {
	Request        ApprovalRequest `json:"request"`
	ApprovalTaskID string          `json:"approval_task_id"`
	Deadline       time.Time       `json:"deadline"`
}

// resumeFromState rebuilds the DAG from a saved RunState, translating any
// persisted status into the DAG's vocabulary. A RunState written by an older
// build can carry the daemon's dead_letter, which the DAG does not speak.
func (o *Orchestrator) resumeFromState(rs *RunState) error {
	// Restore the failure policy the run started with. A RunState saved before
	// this field existed has none, which normalizes to fail_soft.
	if rs.FailureMode != "" {
		o.cfg.FailureMode = rs.FailureMode
	}

	o.dag = NewDAG()
	for _, t := range rs.Tasks {
		st := t.Status
		if st == status.TaskDeadLetter {
			st = status.TaskFailed
		}
		err := o.dag.AddTask(&TaskNode{
			ID:        t.TaskID,
			ParentIDs: t.ParentIDs,
			Status:    st,
			Goal:      t.Goal,
			AgentType: t.AgentType,
			Context:   t.Context,
		})
		if err != nil {
			return fmt.Errorf("resume: add task %s: %w", t.TaskID, err)
		}
	}
	return nil
}

// RunStateCategory is the blackboard category a run is saved under. Named
// because two places need it to agree: the writer, and the lookup a resume does
// when all it has is the key.
const RunStateCategory = "orchestration_run"

// SaveRunState persists the orchestrator's current DAG as a RunState to the
// daemon. This allows crash recovery via Resume(). The returned key is the
// daemon memory key that can be passed to gaap resume.
func (o *Orchestrator) SaveRunState(ctx context.Context, runKey string) error {
	rs := &RunState{
		Goal:           o.goal,
		RepoPath:       o.cfg.RepoPath,
		RunID:          o.runID,
		FailureMode:    o.cfg.Mode(),
		ApprovalPolicy: o.cfg.Approval,
		Tasks:          make([]TaskSpec, 0, len(o.dag.nodes)),
	}
	if approval, ok := o.state.(*ApprovalState); ok {
		rs.Pending = &PendingApproval{
			Request:        approval.Request,
			ApprovalTaskID: approval.ApprovalTaskID,
			Deadline:       approval.Deadline,
		}
	}
	for _, node := range o.dag.nodes {
		rs.Tasks = append(rs.Tasks, TaskSpec{
			TaskID:    node.ID,
			ParentIDs: node.ParentIDs,
			Status:    node.Status,
			Goal:      node.Goal,
			AgentType: node.AgentType,
			Context:   node.Context,
		})
	}

	payload, err := json.Marshal(rs)
	if err != nil {
		return fmt.Errorf("marshal run state: %w", err)
	}

	_, err = o.daemon.AddMemory(ctx, "memory", RunStateCategory, runKey,
		string(payload), 5, "gaap-orchestrator")
	if err != nil {
		return fmt.Errorf("save run state to daemon: %w", err)
	}

	o.runKey = runKey
	slog.Info("run state saved to daemon", "key", runKey, "task_count", len(rs.Tasks))
	return nil
}

// saveRunState re-persists the run under the key it was saved with. Planning
// names the run; every later phase edits it. A run that changed shape without
// saving is a run that resumes into the shape it had at planning — no pending
// approval, so the restart cannot find the question it is supposed to answer.
//
// Failure is logged, not fatal: losing the ability to resume a live run is a
// worse outcome than finishing it un-resumably.
func (o *Orchestrator) saveRunState(ctx context.Context) {
	if o.runKey == "" {
		return
	}
	if err := o.SaveRunState(ctx, o.runKey); err != nil {
		slog.Warn("state: could not re-save the run", "key", o.runKey, "error", err)
	}
}

// RunKey returns the saved daemon key for this run state.
func (o *Orchestrator) RunKey() string {
	return o.runKey
}

// findRunStateByKey lists the saved runs and matches the memory key.
func findRunStateByKey(ctx context.Context, daemon MnemoClient, runKey string) (*pb.MemoryEntry, error) {
	category := RunStateCategory
	entries, err := daemon.ListMemories(ctx, "memory", &category, nil)
	if err != nil {
		return nil, fmt.Errorf("look up run state %q: %w", runKey, err)
	}
	for _, e := range entries {
		if e.Key == runKey {
			return e, nil
		}
	}
	return nil, fmt.Errorf("no run state saved under key %q", runKey)
}

// LoadRunState fetches a RunState from the daemon by key.
func LoadRunState(ctx context.Context, daemon MnemoClient, runKey string) (*RunState, error) {
	entry, err := daemon.GetMemory(ctx, runKey)
	if err != nil || entry == nil {
		// The daemon addresses memories by ID, and the run key is the memory's
		// key: a resume that only has the key printed in the log has to look it
		// up in the category it was written to. Reporting "not found" for a run
		// that is sitting right there is the worst possible answer.
		entry, err = findRunStateByKey(ctx, daemon, runKey)
		if err != nil {
			return nil, err
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("run state not found: %s", runKey)
	}

	var rs RunState
	if err := json.Unmarshal([]byte(entry.Content), &rs); err != nil {
		return nil, fmt.Errorf("parse run state JSON: %w", err)
	}
	return &rs, nil
}
