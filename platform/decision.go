package platform

// Action is the deterministic outcome of a policy evaluation.
type Action string

const (
	ActionAllow     Action = "allow"
	ActionDeny      Action = "deny"
	ActionReview    Action = "review"
	ActionRateLimit Action = "rate_limit"
)

// Decision is the structured result of evaluating a tool call against policy.
// Reason is non-empty whenever Action != allow.
type Decision struct {
	Action Action
	Reason string
}

// Allow is the trivial allow decision.
var Allow = Decision{Action: ActionAllow}

// Deny returns a deny decision with a reason.
func Deny(reason string) Decision {
	return Decision{Action: ActionDeny, Reason: reason}
}
