package gaap

import (
	"fmt"
	"strings"
	"time"
)

// ApprovalAgentType is the reserved agent type an approval travels under.
//
// It must not appear in a worker pool's agent types: an approval is a question
// for a person, and a pool that claimed it would be answering its own question.
// The CLI filters it out of --agent-types rather than trusting the operator to.
const ApprovalAgentType = "approval"

// ApprovalPolicy is a run's decision to involve a human. Off unless asked for:
// an orchestrator that blocks on a person out of the box is one nobody runs
// unattended, so the loop is a policy, not a default.
type ApprovalPolicy struct {
	Enabled bool
	// Timeout is how long a pending request stays open before the run fails.
	// Zero means the request expires immediately — a misconfiguration should
	// not become an unbounded wait.
	Timeout time.Duration
	// Agents is the advisory allowlist of who may answer. Enforcement is in
	// `gaap approve`; vassago has no ACL, so this catches mistakes and drift
	// rather than restraining a determined principal.
	Agents []string
}

// ApprovalAction is what a human can tell the orchestrator to do about a task
// the daemon gave up on. Four answers, no free text: a decision the code cannot
// act on is not a decision.
type ApprovalAction string

const (
	// ApprovalRetry re-dispatches the same agent type with the same context.
	ApprovalRetry ApprovalAction = "retry"
	// ApprovalReassign re-dispatches with a different agent type.
	ApprovalReassign ApprovalAction = "reassign"
	// ApprovalEdit re-dispatches with amended context.
	ApprovalEdit ApprovalAction = "edit"
	// ApprovalCancel accepts the failure and continues without that branch.
	ApprovalCancel ApprovalAction = "cancel"
)

// ApprovalRequest is what the orchestrator asks a human about: a task the daemon
// gave up on, with what a person needs in order to decide.
//
// It is persisted in RunState while a decision is pending, so it has to survive
// JSON: the fields are the whole of what a restarted orchestrator has to know to
// keep waiting for the same answer.
type ApprovalRequest struct {
	TaskID    string    `json:"task_id"`
	AgentType string    `json:"agent_type"`
	Attempts  int32     `json:"attempts"`
	Reason    string    `json:"reason"`
	OpenedAt  time.Time `json:"opened_at"`
	// Deadline is absolute, not a duration. A relative timeout recomputed on
	// restart would silently grant the human more time every time the
	// orchestrator bounced, which is the one thing a deadline must not do.
	Deadline time.Time `json:"deadline"`
}

// NewApprovalRequest builds a request and fixes its deadline on the wall clock.
// A non-positive timeout yields a deadline of "now": no time left is the safe
// reading of a misconfiguration, because an unbounded wait is a hung run
// wearing a question's clothes.
func NewApprovalRequest(taskID, agentType string, attempts int32, reason string, now time.Time, timeout time.Duration) ApprovalRequest {
	if timeout < 0 {
		timeout = 0
	}
	return ApprovalRequest{
		TaskID:    taskID,
		AgentType: agentType,
		Attempts:  attempts,
		Reason:    reason,
		OpenedAt:  now,
		Deadline:  now.Add(timeout),
	}
}

// Expired reports whether the deadline has passed at the given time.
func (r ApprovalRequest) Expired(now time.Time) bool {
	return !now.Before(r.Deadline)
}

// ApprovalDecision is a human's answer to an approval request.
//
// It is written as a memory entry by whoever answers (the CLI, a dashboard) and
// read back by the orchestrator, which is why its JSON tags are the contract.
type ApprovalDecision struct {
	Action    ApprovalAction `json:"action"`
	AgentType string         `json:"agent_type,omitempty"` // required for reassign
	Context   string         `json:"context,omitempty"`    // required for edit
	Reason    string         `json:"reason"`               // required
	DecidedBy string         `json:"decided_by"`
	DecidedAt time.Time      `json:"decided_at"`
}

// Validate reports whether this decision can be enacted. Every rejection names
// the field that is missing, because the caller is a person at a terminal and
// "invalid decision" tells them nothing they can act on.
func (d ApprovalDecision) Validate() error {
	switch d.Action {
	case ApprovalRetry, ApprovalCancel:
		// Self-contained: the orchestrator already knows the task's type and
		// context.
	case ApprovalReassign:
		if strings.TrimSpace(d.AgentType) == "" {
			return fmt.Errorf("action %q requires agent_type", d.Action)
		}
	case ApprovalEdit:
		if strings.TrimSpace(d.Context) == "" {
			return fmt.Errorf("action %q requires context", d.Action)
		}
	default:
		return fmt.Errorf("unknown action %q (want retry, reassign, edit or cancel)", d.Action)
	}

	if strings.TrimSpace(d.Reason) == "" {
		return fmt.Errorf("reason is required: a decision with no stated reason is not auditable")
	}
	return nil
}
