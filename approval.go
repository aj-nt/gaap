package gaap

import "time"

// ApprovalAgentType is the reserved agent type an approval travels under.
//
// It must not appear in a worker pool's agent types: an approval is a question
// for a person, and a pool that claimed it would be answering its own question.
// The CLI filters it out of --agent-types rather than trusting the operator to.
const ApprovalAgentType = "approval"

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
