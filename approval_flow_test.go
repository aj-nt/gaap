package gaap

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/status"

	pb "github.com/aj-nt/vassago-sdk/proto"
)

// approvalConfig is a run that will ask a human, with a deadline short enough
// for a test to observe but long enough not to race the setup.
func approvalConfig(timeout time.Duration) *Config {
	return &Config{
		MaxWaitSec:      10,
		PollIntervalSec: 1,
		Approval: ApprovalPolicy{
			Enabled: true,
			Timeout: timeout,
		},
	}
}

// TestApprovalOpensOnDeadLetter: a dead-lettered task with approval enabled asks
// a human instead of ending the run.
func TestApprovalOpensOnDeadLetter(t *testing.T) {
	daemon, cleanup := setupDaemonDeadLettering(t, "t1")
	defer cleanup()

	cfg := approvalConfig(time.Minute)
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "t1", Status: status.TaskReady, AgentType: "quality_scan", Goal: "inventory"})
	_ = o.dag.AddTask(&TaskNode{ID: "leaf", Status: status.TaskBlocked, ParentIDs: []string{"t1"}})
	o.state = &WaitingState{}

	// What the watcher does on a dead letter.
	o.handleTaskEvent(taskEvent{ID: "t1", Status: status.TaskDeadLetter, RetryCount: 2})

	next := o.waitVerdict()
	approval, ok := next.(*ApprovalState)
	if !ok {
		t.Fatalf("waitVerdict = %T, want *ApprovalState", next)
	}

	if approval.Request.TaskID != "t1" {
		t.Errorf("request task = %q, want t1", approval.Request.TaskID)
	}
	if approval.Request.AgentType != "quality_scan" {
		t.Errorf("request agent type = %q, want quality_scan", approval.Request.AgentType)
	}
	if approval.Request.Attempts != 2 {
		t.Errorf("request attempts = %d, want 2", approval.Request.Attempts)
	}
	if approval.Request.Reason == "" {
		t.Error("request should say why it is being asked")
	}
	if approval.Deadline.Sub(approval.Request.OpenedAt) != time.Minute {
		t.Errorf("deadline is %v after opening, want 1m", approval.Deadline.Sub(approval.Request.OpenedAt))
	}

	// Entering the phase is what puts the question in the daemon.
	if err := o.Transition(approval); err != nil {
		t.Fatalf("Transition to approving: %v", err)
	}
	if got := o.state.Phase(); got != PhaseApproving {
		t.Errorf("phase = %q, want approving", got)
	}
}

// TestApprovalTaskIsInTheDaemon: the question travels as a task, so a human
// answers with the same claim/complete machinery every other actor uses.
func TestApprovalTaskIsInTheDaemon(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, deadLetter: map[string]bool{"t1": true}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := approvalConfig(time.Minute)
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "t1", Status: status.TaskReady, AgentType: "quality_scan", Goal: "inventory"})
	o.state = &WaitingState{}

	o.handleTaskEvent(taskEvent{ID: "t1", Status: status.TaskDeadLetter, RetryCount: 2})
	if err := o.Transition(o.waitVerdict()); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	approvals := td.tasksOfType(ApprovalAgentType)
	if len(approvals) != 1 {
		t.Fatalf("daemon holds %d approval tasks, want exactly 1", len(approvals))
	}
	at := approvals[0]
	if at.MaxRetries != 0 {
		t.Errorf("approval task max_retries = %d, want 0: a human task must not be auto-retried", at.MaxRetries)
	}

	var req ApprovalRequest
	if err := json.Unmarshal([]byte(at.Context), &req); err != nil {
		t.Fatalf("approval task context is not an ApprovalRequest: %v (%q)", err, at.Context)
	}
	if req.TaskID != "t1" || req.Attempts != 2 {
		t.Errorf("request in daemon = %+v, want t1 with 2 attempts", req)
	}
}

// TestApprovalTaskIsWatched: the answer arrives as a task update, and the
// watcher only polls the ids it was handed. A request the watcher never heard of
// is a question whose answer nobody reads.
func TestApprovalTaskIsWatched(t *testing.T) {
	td := &testDaemon{tasks: map[string]*pb.TaskEntry{}, deadLetter: map[string]bool{"t1": true}}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := approvalConfig(time.Minute)
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "t1", Status: status.TaskReady, AgentType: "quality_scan", Goal: "inventory"})
	o.state = &WaitingState{}

	o.handleTaskEvent(taskEvent{ID: "t1", Status: status.TaskDeadLetter, RetryCount: 2})
	if err := o.Transition(o.waitVerdict()); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	select {
	case id := <-o.dispatchFeed:
		if id != o.state.(*ApprovalState).ApprovalTaskID {
			t.Errorf("watcher was handed %q, want the approval task", id)
		}
	case <-time.After(time.Second):
		t.Fatal("the approval task was never handed to the watcher")
	}
}

// TestAPausedRunStillRecordsOtherBranches: a run paused on a question is paused
// for dispatch, not for observation. The watcher reports a terminal state
// exactly once, so an update dropped by the paused phase is an update lost — and
// the run resumes believing a branch is still running that died twenty minutes
// ago.
func TestAPausedRunStillRecordsOtherBranches(t *testing.T) {
	o, _, s := openApproval(t, approvalConfig(10*time.Minute))
	_ = o.dag.AddTask(&TaskNode{ID: "other", Status: status.TaskReady, AgentType: "quality_scan", Goal: "other branch"})

	evt := taskEvent{ID: "other", Status: status.TaskDeadLetter, RetryCount: 2}
	next, err := s.HandleEvent(context.Background(), o, Event{Type: EventTaskUpdate, Task: &evt})
	if err != nil {
		t.Fatalf("handling another branch's update: %v", err)
	}
	if next != nil {
		t.Errorf("state = %T, want the run to stay paused on the open question", next)
	}
	if node, _ := o.dag.GetTask("other"); node.Status != status.TaskFailed {
		t.Errorf("other branch status = %q, want failed: the update was dropped", node.Status)
	}
}

// TestAFailureRecordedWhilePausedIsAskedAfterTheVerdict: the question was still
// owed an answer after the run moved on. A single-slot record of pending
// failures loses it, and nothing reports it a second time.
func TestAFailureRecordedWhilePausedIsAskedAfterTheVerdict(t *testing.T) {
	o, _, s := openApproval(t, approvalConfig(10*time.Minute))
	_ = o.dag.AddTask(&TaskNode{ID: "other", Status: status.TaskReady, AgentType: "quality_scan", Goal: "other branch"})
	evt := taskEvent{ID: "other", Status: status.TaskDeadLetter, RetryCount: 2}
	if _, err := s.HandleEvent(context.Background(), o, Event{Type: EventTaskUpdate, Task: &evt}); err != nil {
		t.Fatal(err)
	}

	// Answer the first question.
	next := answerApproval(t, o, s, ApprovalDecision{
		Action: ApprovalCancel,
		Reason: "the first one; the second is a different decision",
	})
	if err := o.Transition(next); err != nil {
		t.Fatalf("leaving the approval: %v", err)
	}

	// The waiting phase announces the failure it inherited, and the loop decides
	// it the same way it decides any other.
	select {
	case got := <-o.events:
		if err := o.step(got); err != nil {
			t.Fatalf("stepping the queued verdict: %v", err)
		}
	default:
		t.Fatal("the failure recorded while paused was never announced")
	}
	second, ok := o.state.(*ApprovalState)
	if !ok {
		t.Fatalf("state = %T, want a second question for the other branch", o.state)
	}
	if second.Request.TaskID != "other" {
		t.Errorf("second question is about %q, want the other branch", second.Request.TaskID)
	}
}

// TestApprovalOffKeepsTheOldBehaviour: without --approval a dead letter still
// ends the run. The loop is off unless asked for.
func TestApprovalOffKeepsTheOldBehaviour(t *testing.T) {
	daemon, cleanup := setupDaemonDeadLettering(t, "t1")
	defer cleanup()

	cfg := &Config{MaxWaitSec: 10, PollIntervalSec: 1} // approval disabled
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "t1", Status: status.TaskReady, AgentType: "quality_scan", Goal: "inventory"})
	o.state = &WaitingState{}

	o.handleTaskEvent(taskEvent{ID: "t1", Status: status.TaskDeadLetter, RetryCount: 2})
	next := o.waitVerdict()
	if _, ok := next.(*FailedState); !ok {
		t.Fatalf("waitVerdict = %T, want *FailedState with approval off", next)
	}
}

// TestFailFastStillAsksWhenApprovalIsOn: fail_fast stops dispatching, it does
// not skip the question. Otherwise enabling approval and fail_fast together
// would silently mean "never ask".
func TestFailFastStillAsksWhenApprovalIsOn(t *testing.T) {
	daemon, cleanup := setupDaemonDeadLettering(t, "t1")
	defer cleanup()

	cfg := approvalConfig(time.Minute)
	cfg.FailureMode = FailFast
	o := NewOrchestrator(context.Background(), cfg, daemon, nil)
	o.dag = NewDAG()
	_ = o.dag.AddTask(&TaskNode{ID: "t1", Status: status.TaskReady, AgentType: "quality_scan", Goal: "inventory"})
	o.state = &WaitingState{}

	o.handleTaskEvent(taskEvent{ID: "t1", Status: status.TaskDeadLetter, RetryCount: 2})
	if _, ok := o.waitVerdict().(*ApprovalState); !ok {
		t.Fatal("fail_fast should still open the approval")
	}
}
