package gaap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aj-nt/gaap/internal/status"
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
	// Kind says why a human is being asked: a task that failed (dead_letter) or
	// a call the policy would not authorize on its own (review). The answer means
	// different things — a decline of a review is a refusal to run, not an
	// accepted failure — so the request has to carry which question it is.
	Kind      string    `json:"kind,omitempty"`
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

const (
	// ApprovalKindDeadLetter is a question about a task the daemon gave up on.
	ApprovalKindDeadLetter = "dead_letter"
	// ApprovalKindReview is a question the policy asked instead of deciding.
	ApprovalKindReview = "review"
)

// ReviewReason is the question a review asks: the policy's own reason, so a
// human reads the rule that stopped it rather than a summary of the rule.
func ReviewReason(reason string) string {
	if reason == "" {
		return "policy review: no reason given"
	}
	return "policy review: " + reason
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

// loadApprovalDecision reads the answer a human left on the blackboard. The
// result key travels on the approval task, exactly as a worker's result does, so
// the answer arrives without a second channel.
func (o *Orchestrator) loadApprovalDecision(resultKey string) (ApprovalDecision, error) {
	var d ApprovalDecision
	if resultKey == "" {
		return d, fmt.Errorf("the approval task completed with no result key")
	}
	mem, err := o.daemon.GetMemory(o.ctx, resultKey)
	if err != nil {
		return d, fmt.Errorf("reading %q: %w", resultKey, err)
	}
	if mem == nil || mem.Content == "" {
		return d, fmt.Errorf("memory %q is empty", resultKey)
	}
	if err := json.Unmarshal([]byte(mem.Content), &d); err != nil {
		return d, fmt.Errorf("memory %q is not an ApprovalDecision: %w", resultKey, err)
	}
	return d, nil
}

// applyApproval enacts a human's answer.
//
// Three of the four answers replace the failed node with a new one — a
// dead-lettered task is terminal in the daemon, so "try again" cannot mean
// "reopen that row" — and cancel accepts the failure, which is what the run
// would have done without the loop. A decision that cannot be enacted fails the
// run: it is data that arrived over a daemon, and a run that quietly continues
// after a broken instruction is worse than one that says so.
func (o *Orchestrator) applyApproval(ctx context.Context, s *ApprovalState, d ApprovalDecision) OrchestratorState {
	log := slog.With("task_id", s.Request.TaskID, "action", string(d.Action))

	if err := d.Validate(); err != nil {
		log.Error("approval: the decision cannot be enacted", "error", err, "decided_by", d.DecidedBy)
		return &FailedState{}
	}
	node, ok := o.dag.nodes[s.Request.TaskID]
	if !ok {
		log.Error("approval: the task this decision is about is no longer in the graph")
		return &FailedState{}
	}

	log.Info("approval: decision received",
		"decided_by", d.DecidedBy, "agent_type", d.AgentType, "reason", d.Reason)

	if d.Action == ApprovalCancel {
		if s.Request.Kind == ApprovalKindReview {
			// The question was "may this run?", so declining means it does not.
			// It is a refusal, not an accepted failure: nothing was attempted.
			o.denyTask(node, "review declined by "+d.DecidedBy+": "+d.Reason)
			return o.afterDecision()
		}
		// Accepting the failure means exactly that: the branch stays lost. Any
		// work still in flight elsewhere decides the run's outcome as usual —
		// through the verdict, because another branch may be owed a question of
		// its own and deciding here would end the run before asking it.
		return o.afterDecision()
	}

	replacement := &TaskNode{
		ID:        o.nextReplacementID(node.ID),
		AgentType: node.AgentType,
		Goal:      node.Goal,
		Context:   node.Context,
		// A human decided this call. Asking the governor again would turn "yes"
		// into "ask again", which is how a review loop never ends.
		Approved: true,
	}
	switch d.Action {
	case ApprovalReassign:
		replacement.AgentType = d.AgentType
	case ApprovalEdit:
		var amended map[string]any
		if err := json.Unmarshal([]byte(d.Context), &amended); err != nil {
			log.Error("approval: the edited context is not a JSON object", "error", err, "context", d.Context)
			return &FailedState{}
		}
		replacement.Context = amended
	}

	reopened, err := o.dag.Supersede(node.ID, replacement)
	if err != nil {
		log.Error("approval: could not replace the task", "error", err)
		return &FailedState{}
	}
	log.Info("approval: task replaced",
		"replacement", replacement.ID, "agent_type", replacement.AgentType, "reopened", reopened)

	// A replacement whose parents are done is ready now; one whose parents are
	// still running is dispatched by the promotion path when they finish.
	if replacement.Status == status.TaskReady {
		if err := o.dispatchTask(ctx, replacement.ID); err != nil {
			log.Warn("approval: the replacement could not be dispatched", "error", err)
		}
	}
	return o.afterDecision()
}

// withdrawApprovalRequest removes a request the run has given up on. Left in
// place, it would keep inviting an answer to a question nobody is listening for,
// and the person who answered it would believe they had changed something.
func (o *Orchestrator) withdrawApprovalRequest(s *ApprovalState) {
	if s.ApprovalTaskID == "" || o.daemon == nil {
		return
	}
	if _, err := o.daemon.DeleteTask(o.ctx, s.ApprovalTaskID); err != nil {
		slog.Warn("approval: could not withdraw the request", "approval_task", s.ApprovalTaskID, "error", err)
		return
	}
	slog.Info("approval: withdrew the unanswered request", "approval_task", s.ApprovalTaskID)
}

// nextReplacementID returns an unused id for the task that replaces failedID:
// "<id>_retry", then "_retry2", and so on. Deterministic, because the id shows
// up in logs and in the daemon and a UUID would make the history unreadable.
func (o *Orchestrator) nextReplacementID(failedID string) string {
	candidate := failedID + "_retry"
	for n := 2; ; n++ {
		if _, taken := o.dag.nodes[candidate]; !taken {
			return candidate
		}
		candidate = fmt.Sprintf("%s_retry%d", failedID, n)
	}
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
