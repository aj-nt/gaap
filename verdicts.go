package gaap

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/aj-nt/gaap/internal/status"
)

// Where the run goes after an event has been applied.
//
// One place, because two paths advance the DAG — a watcher update and the
// simulated run — and a run that ends by one rule on one path and another rule
// on the other is a run whose outcome depends on which transport answered first.

// waitVerdict decides where a waiting run goes after an event was applied:
// still waiting, failed, or synthesizing. One place, so the two paths that can
// advance the DAG — a watcher update and a simulated run — cannot disagree about
// when the run is over.
func (o *Orchestrator) waitVerdict() OrchestratorState {
	// A failure with a human on the other end is not the end of the run. This
	// is checked before fail_fast: fail_fast stops dispatching to other
	// branches, it does not skip the question the operator opted into.
	if o.openQuestion {
		// A question is open. Its answer decides that failure; the ones already
		// queued are answered afterwards, one at a time.
		return nil
	}
	if st := o.reviewVerdict(); st != nil {
		return st
	}
	if st := o.approvalVerdict(); st != nil {
		return st
	}
	if o.failFast() && o.dag.HasFailures() {
		slog.Warn("fail_fast: ending run at the first failure")
		return &FailedState{}
	}
	if !o.dag.AllTasksTerminal() {
		return nil
	}
	if failed := o.dag.FailedIDs(); len(failed) > 0 {
		slog.Warn("tasks failed, transitioning to failed", "failed", failed)
		return &FailedState{}
	}
	slog.Info("all tasks complete, transitioning to synthesizing")
	return &SynthesizingState{}
}

// afterDecision is where a run goes once a question is answered: back to
// waiting, unless the verdict says the run is over. It must not open the next
// question itself — approving -> approving is not an edge, and pretending it is
// would hide a handler that meant to move. The waiting phase announces a queued
// failure as its own question, which is the one place allowed to ask.
func (o *Orchestrator) afterDecision() OrchestratorState {
	// A queued failure is not decided here: the waiting phase announces it, and
	// that is the only phase allowed to open a question (approving -> approving
	// is not an edge). Deciding it here would consume the question and drop it.
	if len(o.pendingFailures) > 0 {
		return &WaitingState{}
	}
	if st := o.waitVerdict(); st != nil {
		return st
	}
	return &WaitingState{}
}

// reviewVerdict turns the oldest policy review into a question. Reviews come
// before failures: a task held by policy has not run and is not lost, so it is
// the cheaper thing to resolve, and a failure that arrived while the run was
// paused stays queued behind it.
func (o *Orchestrator) reviewVerdict() OrchestratorState {
	if len(o.pendingReviews) == 0 || !o.cfg.Approval.Enabled {
		return nil
	}
	r := o.pendingReviews[0]
	node, ok := o.dag.nodes[r.TaskID]
	if !ok || node.Status != status.TaskBlocked {
		o.pendingReviews = o.pendingReviews[1:]
		return o.reviewVerdict()
	}
	o.pendingReviews = o.pendingReviews[1:]

	req := NewApprovalRequest(node.ID, node.AgentType, 0, r.Reason, time.Now(), o.cfg.Approval.Timeout)
	req.Kind = ApprovalKindReview
	slog.Info("approval: the policy referred a task to a human",
		"task_id", req.TaskID, "agent_type", req.AgentType, "reason", req.Reason,
		"deadline", req.Deadline.Format(time.RFC3339))
	return &ApprovalState{
		Request:        req,
		ApprovalTaskID: req.TaskID + "_approval",
		Deadline:       req.Deadline,
	}
}

// approvalVerdict answers the pending dead letter with a question, when the run
// opted into asking humans. nil means the failure stands as the run's answer.
//
// It consumes the failure: the request owns it now, so a second call cannot
// open a second approval for the same dead letter.
func (o *Orchestrator) approvalVerdict() OrchestratorState {
	if len(o.pendingFailures) == 0 || !o.cfg.Approval.Enabled {
		return nil
	}
	f := &o.pendingFailures[0]
	node, ok := o.dag.nodes[f.TaskID]
	if !ok || node.SupersededBy != "" {
		// Already answered or replaced: drop it and look at the next one.
		o.pendingFailures = o.pendingFailures[1:]
		return o.approvalVerdict()
	}
	o.pendingFailures = o.pendingFailures[1:]

	req := NewApprovalRequest(node.ID, node.AgentType, f.Attempts, node.Summary, time.Now(), o.cfg.Approval.Timeout)
	slog.Info("approval: asking a human",
		"task_id", req.TaskID, "agent_type", req.AgentType,
		"attempts", req.Attempts, "deadline", req.Deadline.Format(time.RFC3339))

	return &ApprovalState{
		Request:        req,
		ApprovalTaskID: node.ID + "_" + ApprovalAgentType,
		Deadline:       req.Deadline,
	}
}

// failTask records a task the daemon gave up on. dead_letter is the daemon's
// word for "retries exhausted"; failed is the DAG's word for what that means.
// Descendants are skipped here because nothing will ever promote them, and
// leaving them blocked would end the run by timeout instead of by fact.
func (o *Orchestrator) failTask(node *TaskNode, attempts int32, logPrefix string) {
	node.Status = status.TaskFailed
	node.Summary = fmt.Sprintf("daemon dead-lettered the task after %d attempt(s)", attempts)
	skipped := o.dag.SkipDescendants(node.ID)
	// The verdict reads this to decide whether the failure ends the run or
	// becomes a question for a human. Recorded here because this is the one
	// place that sees a dead letter from either transport.
	// Queued, not overwritten: another branch can dead-letter while the run is
	// paused on a question, and an event that is reported once cannot be looked
	// up again later.
	o.pendingFailures = append(o.pendingFailures, failureRecord{TaskID: node.ID, Attempts: attempts})
	o.fireTaskFailed(node.ID, logPrefix, skipped)
}

// reviewRecord is a call the policy referred to a human.
type reviewRecord struct {
	TaskID    string
	AgentType string
	Reason    string
}

// failureRecord is the failure a verdict still has to answer for.
type failureRecord struct {
	TaskID   string
	Attempts int32
}

// failFast reports whether the run should stop at the first task failure
// instead of letting independent branches finish.
func (o *Orchestrator) failFast() bool {
	return o.cfg.Mode() == FailFast
}
