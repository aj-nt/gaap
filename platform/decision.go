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
	// RuleID identifies the policy rule that produced this decision. Empty when
	// no rule matched (the deny-by-default fallback) or when the decision did not
	// come from a rule (e.g. the kill switch). The governor records it, which is
	// what makes a decision attributable instead of anonymous.
	RuleID string
	// Denial is the structured refusal, set by the governor for any non-allow
	// decision and nil otherwise. It is what the caller renders to the agent: the
	// raw Reason is written for the operator and encodes thresholds the agent
	// must not learn.
	Denial *DenialClass
	// Burst is how many refusals this agent has accumulated for this action class
	// inside the counter's window, when a counter is attached and the threshold
	// was reached. Zero means no burst. It never changes Action: a sustained
	// attempt is denied exactly like a single one, and a burst must never be
	// downgraded to "ask a human".
	Burst int
}

// Allow is the trivial allow decision.
var Allow = Decision{Action: ActionAllow}

// Deny returns a deny decision with a reason.
func Deny(reason string) Decision {
	return Decision{Action: ActionDeny, Reason: reason}
}
