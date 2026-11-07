package gaap

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/status"

	pb "github.com/aj-nt/vassago-sdk/proto"
)

// openApproval drives a run to the approving phase the way the watcher would,
// over a real bufconn daemon: one dead-lettered task with a dependent below it.
func openApproval(t *testing.T, cfg *Config) (*Orchestrator, *testDaemon, *ApprovalState) {
	t.Helper()

	td := &testDaemon{
		tasks:      map[string]*pb.TaskEntry{},
		deadLetter: map[string]bool{"t1": true},
		memories:   map[string]*pb.MemoryEntry{},
	}
	daemon, cleanup := serveTestDaemon(t, td)
	t.Cleanup(cleanup)

	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "t1", Status: status.TaskReady, AgentType: "quality_scan", Goal: "inventory"})
	_ = o.dag.AddTask(&TaskNode{ID: "leaf", Status: status.TaskBlocked, ParentIDs: []string{"t1"}})
	o.state = &WaitingState{}

	o.handleTaskEvent(taskEvent{ID: "t1", Status: status.TaskDeadLetter, RetryCount: 2})
	if err := o.Transition(o.waitVerdict()); err != nil {
		t.Fatalf("opening the approval: %v", err)
	}
	s, ok := o.state.(*ApprovalState)
	if !ok {
		t.Fatalf("state = %T, want *ApprovalState", o.state)
	}
	return o, td, s
}

// answerApproval does what `gaap approve` does: write the decision to memory,
// then complete the approval task with the memory entry's ID as its result key.
// The decision travels as data, so this is the whole wire protocol.
func answerApproval(t *testing.T, o *Orchestrator, s *ApprovalState, d ApprovalDecision) OrchestratorState {
	t.Helper()

	if d.DecidedAt.IsZero() {
		d.DecidedAt = time.Now()
	}
	if d.DecidedBy == "" {
		d.DecidedBy = "aj"
	}
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := o.daemon.AddMemory(context.Background(), "approval", "approval_decision",
		"approval_"+s.Request.TaskID+"_result", string(body), 5, "gaap-approve")
	if err != nil {
		t.Fatalf("storing the decision: %v", err)
	}
	if _, err := o.daemon.CompleteTask(context.Background(), s.ApprovalTaskID, mem.Id); err != nil {
		t.Fatalf("completing the approval task: %v", err)
	}

	evt := taskEvent{ID: s.ApprovalTaskID, Status: status.TaskDone, ResultKey: mem.Id, Source: "poll"}
	next, err := s.HandleEvent(context.Background(), o, Event{Type: EventTaskUpdate, Task: &evt})
	if err != nil {
		t.Fatalf("handling the answer: %v", err)
	}
	return next
}

// TestApprovalRetryReplacesTheTask: "try again" is a new node standing where the
// failure stood, because a dead-lettered task cannot be reopened.
func TestApprovalRetryReplacesTheTask(t *testing.T) {
	o, td, s := openApproval(t, approvalConfig(time.Minute))

	next := answerApproval(t, o, s, ApprovalDecision{Action: ApprovalRetry, Reason: "the model was rate limited"})
	if _, ok := next.(*WaitingState); !ok {
		t.Fatalf("after a retry the run should wait on the replacement, got %T", next)
	}

	failed, _ := o.dag.GetTask("t1")
	if failed.SupersededBy == "" {
		t.Fatal("failed task should name its replacement")
	}
	replacement, err := o.dag.GetTask(failed.SupersededBy)
	if err != nil {
		t.Fatalf("replacement %q is not in the DAG: %v", failed.SupersededBy, err)
	}
	if replacement.AgentType != "quality_scan" {
		t.Errorf("retry agent type = %q, want the original", replacement.AgentType)
	}
	if replacement.Goal != "inventory" {
		t.Errorf("retry goal = %q, want the original", replacement.Goal)
	}

	dispatched := td.tasksOfType("quality_scan")
	var found *pb.TaskEntry
	for _, e := range dispatched {
		if e.Id == replacement.ID {
			found = e
		}
	}
	if found == nil {
		t.Fatalf("replacement %q was not dispatched to the daemon (have %d quality_scan tasks)", replacement.ID, len(dispatched))
	}
	if found.MaxRetries != 2 {
		t.Errorf("replacement max_retries = %d, want the dispatch default 2", found.MaxRetries)
	}

	leaf, _ := o.dag.GetTask("leaf")
	if leaf.Status != status.TaskBlocked || len(leaf.ParentIDs) != 1 || leaf.ParentIDs[0] != replacement.ID {
		t.Errorf("dependent = %q with parents %v, want blocked on %q", leaf.Status, leaf.ParentIDs, replacement.ID)
	}
}

// TestApprovalReassignUsesTheNewAgentType: a human who knows better than the
// decomposer gets to say so, and the new type is what runs.
func TestApprovalReassignUsesTheNewAgentType(t *testing.T) {
	o, td, s := openApproval(t, approvalConfig(time.Minute))

	next := answerApproval(t, o, s, ApprovalDecision{
		Action:    ApprovalReassign,
		AgentType: "file_analysis",
		Reason:    "the goal is a file inventory; quality_scan has no file tools",
	})
	if _, ok := next.(*WaitingState); !ok {
		t.Fatalf("after a reassign the run should wait on the replacement, got %T", next)
	}

	failed, _ := o.dag.GetTask("t1")
	replacement, err := o.dag.GetTask(failed.SupersededBy)
	if err != nil {
		t.Fatalf("no replacement after reassign: %v", err)
	}
	if replacement.AgentType != "file_analysis" {
		t.Errorf("replacement agent type = %q, want file_analysis", replacement.AgentType)
	}
	if got := td.tasksOfType("file_analysis"); len(got) != 1 || got[0].Id != replacement.ID {
		t.Errorf("daemon file_analysis tasks = %v, want the replacement", got)
	}
}

// TestApprovalEditUsesTheNewContext: amended context has to reach the worker, or
// "edit" is a retry with extra ceremony.
func TestApprovalEditUsesTheNewContext(t *testing.T) {
	o, td, s := openApproval(t, approvalConfig(time.Minute))

	next := answerApproval(t, o, s, ApprovalDecision{
		Action:  ApprovalEdit,
		Context: `{"source_path":"/tmp/x.go","note":"the decomposer named the wrong file"}`,
		Reason:  "wrong input file",
	})
	if _, ok := next.(*WaitingState); !ok {
		t.Fatalf("after an edit the run should wait on the replacement, got %T", next)
	}

	failed, _ := o.dag.GetTask("t1")
	replacement, err := o.dag.GetTask(failed.SupersededBy)
	if err != nil {
		t.Fatalf("no replacement after edit: %v", err)
	}
	if replacement.AgentType != "quality_scan" {
		t.Errorf("edit agent type = %q, want the original", replacement.AgentType)
	}
	if replacement.Context["source_path"] != "/tmp/x.go" {
		t.Errorf("replacement context = %v, want the amended source_path", replacement.Context)
	}

	got := td.tasksOfType("quality_scan")
	var found *pb.TaskEntry
	for _, e := range got {
		if e.Id == replacement.ID {
			found = e
		}
	}
	if found == nil {
		t.Fatal("edited replacement was not dispatched")
	}
	var ctxMap map[string]any
	if err := json.Unmarshal([]byte(found.Context), &ctxMap); err != nil {
		t.Fatalf("dispatched context is not JSON: %v (%q)", err, found.Context)
	}
	if ctxMap["source_path"] != "/tmp/x.go" {
		t.Errorf("dispatched context = %v, want the amended source_path", ctxMap)
	}
}

// TestApprovalEditWithBrokenJSONFailsTheRun: an edit that is not a JSON object
// is a decision the code cannot enact. Failing loudly beats dispatching a worker
// with `null` context and calling it progress.
func TestApprovalEditWithBrokenJSONFailsTheRun(t *testing.T) {
	o, td, s := openApproval(t, approvalConfig(time.Minute))

	next := answerApproval(t, o, s, ApprovalDecision{Action: ApprovalEdit, Context: `not json`, Reason: "oops"})
	if _, ok := next.(*FailedState); !ok {
		t.Fatalf("a broken edit should fail the run, got %T", next)
	}
	if got := td.tasksOfType("quality_scan"); len(got) != 0 {
		t.Errorf("nothing should have been dispatched, found %v", got)
	}
}

// TestApprovalCancelKeepsTheFailure: "cancel" accepts that the branch is lost,
// which is what the run would have done without the loop — no silent repair.
func TestApprovalCancelKeepsTheFailure(t *testing.T) {
	o, td, s := openApproval(t, approvalConfig(time.Minute))

	next := answerApproval(t, o, s, ApprovalDecision{Action: ApprovalCancel, Reason: "not worth the tokens"})
	if _, ok := next.(*FailedState); !ok {
		t.Fatalf("cancel with nothing left to run should fail the run, got %T", next)
	}
	failed, _ := o.dag.GetTask("t1")
	if failed.SupersededBy != "" {
		t.Errorf("cancel must not create a replacement, got %q", failed.SupersededBy)
	}
	if got := td.tasksOfType("quality_scan"); len(got) != 0 {
		t.Errorf("cancel must not dispatch anything, found %v", got)
	}
}

// TestApprovalRejectsUnusableDecision: the decision is data that arrived over a
// daemon, so it is validated where it is enacted.
func TestApprovalRejectsUnusableDecision(t *testing.T) {
	o, td, s := openApproval(t, approvalConfig(time.Minute))

	next := answerApproval(t, o, s, ApprovalDecision{Action: ApprovalRetry}) // no reason
	if _, ok := next.(*FailedState); !ok {
		t.Fatalf("an unusable decision should fail the run, got %T", next)
	}
	if got := td.tasksOfType("quality_scan"); len(got) != 0 {
		t.Errorf("nothing should have been dispatched, found %v", got)
	}
}

// TestApprovalTaskDeadLetterFailsTheRun: the approval task itself cannot be
// retried (max_retries 0), so its dead letter means nobody answered.
func TestApprovalTaskDeadLetterFailsTheRun(t *testing.T) {
	o, _, s := openApproval(t, approvalConfig(time.Minute))

	evt := taskEvent{ID: s.ApprovalTaskID, Status: status.TaskDeadLetter, Source: "poll"}
	next, err := s.HandleEvent(context.Background(), o, Event{Type: EventTaskUpdate, Task: &evt})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next.(*FailedState); !ok {
		t.Fatalf("a dead-lettered approval task should fail the run, got %T", next)
	}
}

// TestApprovalIgnoresOtherTasks: the run is paused, but the daemon keeps
// talking. A worker finishing some other branch must not be read as an answer.
func TestApprovalIgnoresOtherTasks(t *testing.T) {
	o, _, s := openApproval(t, approvalConfig(time.Minute))

	evt := taskEvent{ID: "t1", Status: status.TaskDone, Source: "poll"}
	next, err := s.HandleEvent(context.Background(), o, Event{Type: EventTaskUpdate, Task: &evt})
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("an unrelated task update should be ignored, got %T", next)
	}
}
