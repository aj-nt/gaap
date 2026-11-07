package gaap

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/status"

	pb "github.com/aj-nt/vassago-sdk/proto"
)

// TestRunAsksAHumanAndHonoursTheAnswer is the loop end to end: a real run over a
// bufconn daemon dead-letters a task, pauses, is answered the way `gaap approve`
// answers, and finishes the work the answer asked for.
func TestRunAsksAHumanAndHonoursTheAnswer(t *testing.T) {
	td := &testDaemon{
		tasks:        map[string]*pb.TaskEntry{},
		autoComplete: true,
		deadLetter:   map[string]bool{"t1": true},
		memories:     map[string]*pb.MemoryEntry{},
	}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := &Config{
		PollIntervalSec: 1,
		MaxWaitSec:      30,
		RepoPath:        t.TempDir(),
		Approval:        ApprovalPolicy{Enabled: true, Timeout: 20 * time.Second},
	}
	o := NewOrchestrator(context.Background(), cfg, daemon, NewDecomposer(NewStaticDecomposition([]TaskSpec{
		{TaskID: "t1", AgentType: "quality_scan", Status: status.TaskReady, Goal: "inventory"},
	})))

	done := make(chan error, 1)
	go func() { done <- o.Run("needs a human") }()

	// The request appears in the daemon, which is how a person would find it.
	var approvalTask *pb.TaskEntry
	deadline := time.After(10 * time.Second)
	for approvalTask == nil {
		select {
		case <-deadline:
			t.Fatal("the run never asked for approval")
		case <-time.After(50 * time.Millisecond):
			if got := td.tasksOfType(ApprovalAgentType); len(got) > 0 {
				approvalTask = got[0]
			}
		}
	}

	// What `gaap approve --retry` does.
	body, _ := json.Marshal(ApprovalDecision{
		Action:    ApprovalRetry,
		Reason:    "the scanner was rate limited",
		DecidedBy: "aj",
		DecidedAt: time.Now(),
	})
	mem, err := daemon.AddMemory(context.Background(), "approval", "approval_decision",
		"approval_t1_result", string(body), 5, "gaap-approve")
	if err != nil {
		t.Fatalf("storing the decision: %v", err)
	}
	if _, err := daemon.CompleteTask(context.Background(), approvalTask.Id, mem.Id); err != nil {
		t.Fatalf("completing the approval task: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the answered run should have finished: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the run never finished after the answer")
	}

	// The replacement is what ran, and the failed node kept its history. The
	// decomposer namespaces ids with the run, so the node is found by suffix.
	failed := nodeEndingIn(t, o.dag, "t1")
	if failed.Status != status.TaskFailed {
		t.Errorf("original task = %q, want failed", failed.Status)
	}
	if failed.SupersededBy == "" {
		t.Fatal("no replacement after a retry decision")
	}
	replacement, err := o.dag.GetTask(failed.SupersededBy)
	if err != nil {
		t.Fatalf("replacement missing: %v", err)
	}
	if replacement.Status != status.TaskDone {
		t.Errorf("replacement = %q, want done", replacement.Status)
	}
}

// TestRunFailsWhenNobodyAnswers: the deadline is the run's, not the operator's
// memory. A request that expires leaves nothing in the daemon to trip over.
func TestRunFailsWhenNobodyAnswers(t *testing.T) {
	td := &testDaemon{
		tasks:        map[string]*pb.TaskEntry{},
		autoComplete: true,
		deadLetter:   map[string]bool{"t1": true},
		memories:     map[string]*pb.MemoryEntry{},
	}
	daemon, cleanup := serveTestDaemon(t, td)
	defer cleanup()

	cfg := &Config{
		PollIntervalSec: 1,
		MaxWaitSec:      30,
		RepoPath:        t.TempDir(),
		Approval:        ApprovalPolicy{Enabled: true, Timeout: 400 * time.Millisecond},
	}
	o := NewOrchestrator(context.Background(), cfg, daemon, NewDecomposer(NewStaticDecomposition([]TaskSpec{
		{TaskID: "t1", AgentType: "quality_scan", Status: status.TaskReady, Goal: "inventory"},
	})))

	start := time.Now()
	err := o.Run("nobody is coming")
	if err == nil {
		t.Fatal("an unanswered approval should fail the run")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the run waited %v; the approval deadline was 400ms", elapsed)
	}
	if got := td.tasksOfType(ApprovalAgentType); len(got) != 0 {
		t.Errorf("the expired request is still in the daemon: %v", got)
	}
}
