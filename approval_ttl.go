package gaap

import "time"

const (
	// DefaultApprovalTimeoutSec is how long a run waits for a human answer when
	// the operator names no value. It is the CLI default and nothing else, so
	// the flag and the approval task derive from one number.
	DefaultApprovalTimeoutSec = 900

	// approvalTaskGrace keeps the daemon task alive slightly past the run's own
	// deadline. The task is where an answer is written; letting it die first
	// means a late-but-valid answer has nowhere to land.
	approvalTaskGrace = 60 * time.Second
)

// ApprovalTaskTTL derives the daemon task lifetime for an approval request from
// the deadline the run itself observes.
//
// The two were declared separately before — a hardcoded 600s task against a 900s
// run deadline — so for the last five minutes the run held a deadline for an
// answer whose arrival slot had already expired, and reported silence.
//
// A zero or negative deadline means the request was configured to expire
// immediately; that is honored rather than silently widened, and the grace still
// applies so the answer has somewhere to be written.
func ApprovalTaskTTL(deadline time.Duration) int32 {
	if deadline < 0 {
		deadline = 0
	}
	return int32((deadline + approvalTaskGrace) / time.Second)
}
