package gaap

import (
	"context"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/status"
)

// TestApprovalDeadlineFailsTheRun: a question nobody answered is not a run that
// waits forever. The deadline is the whole point of asking with one.
func TestApprovalDeadlineFailsTheRun(t *testing.T) {
	o, td, s := openApproval(t, approvalConfig(100*time.Millisecond))

	next, err := s.HandleEvent(context.Background(), o, Event{
		Type:    EventApprovalTimeout,
		Payload: map[string]any{"approval_task": s.ApprovalTaskID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next.(*FailedState); !ok {
		t.Fatalf("an expired approval should fail the run, got %T", next)
	}
	if got := td.tasksOfType(ApprovalAgentType); len(got) != 0 {
		t.Errorf("the unanswered request should be withdrawn, %d left in the daemon", len(got))
	}
}

// TestApprovalDeadlineForAnotherRequestIsIgnored: a timer from an earlier
// approval must not answer for this one.
func TestApprovalDeadlineForAnotherRequestIsIgnored(t *testing.T) {
	o, _, s := openApproval(t, approvalConfig(time.Minute))

	next, err := s.HandleEvent(context.Background(), o, Event{
		Type:    EventApprovalTimeout,
		Payload: map[string]any{"approval_task": "some_other_task_approval"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("a stale deadline should be ignored, got %T", next)
	}
}

// TestStaleApprovalDeadlineIsNotARunTimeout: the timer fires on the loop after
// the run has moved on. Waiting must not read it as MaxWaitSec expiring, or an
// answered approval whose timer outlives it would kill a healthy run.
func TestStaleApprovalDeadlineIsNotARunTimeout(t *testing.T) {
	o, _, _ := openApproval(t, approvalConfig(time.Minute))
	o.dag.nodes["t1"].Status = status.TaskDone

	next, err := (&WaitingState{}).HandleEvent(context.Background(), o, Event{Type: EventApprovalTimeout})
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("waiting should ignore an approval timer, got %T", next)
	}
}

// TestApprovalDeadlineRunsTheClock: the timer itself, not just the handler. A
// pending approval fails the run at its own deadline, well inside MaxWaitSec.
func TestApprovalDeadlineRunsTheClock(t *testing.T) {
	o, _, s := openApproval(t, approvalConfig(150*time.Millisecond))

	start := time.Now()
	o.startApprovalClock(s)

	// The setup already posted a task-failure event; the clock's own event is
	// the one this test is about.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-o.events:
			if evt.Type != EventApprovalTimeout {
				continue
			}
			if got, _ := evt.Payload["approval_task"].(string); got != s.ApprovalTaskID {
				t.Errorf("event names %q, want %q", got, s.ApprovalTaskID)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("clock took %v, want about 150ms", elapsed)
			}
			return
		case <-deadline:
			t.Fatal("the approval clock never fired")
		}
	}
}

// TestApprovalRequestIsWithdrawnOnExpiry keeps the daemon honest: a request the
// run has given up on must not sit there inviting an answer nobody will read.
func TestApprovalRequestIsWithdrawnOnExpiry(t *testing.T) {
	o, td, s := openApproval(t, approvalConfig(time.Minute))
	if got := td.tasksOfType(ApprovalAgentType); len(got) != 1 {
		t.Fatalf("setup: expected one pending request, got %d", len(got))
	}

	if _, err := s.HandleEvent(context.Background(), o, Event{
		Type:    EventApprovalTimeout,
		Payload: map[string]any{"approval_task": s.ApprovalTaskID},
	}); err != nil {
		t.Fatal(err)
	}

	if got := td.tasksOfType(ApprovalAgentType); len(got) != 0 {
		t.Errorf("daemon still holds %d approval tasks after expiry", len(got))
	}
}
