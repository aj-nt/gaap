package platform

import "sync"

// Rule matches a tool call and produces a decision when it matches.
type Rule struct {
	// ID names the rule. The ledger records it on every decision the rule
	// produces, so an allow or a deny is attributable to a specific standing
	// order rather than to "policy, somehow". Optional, but a rule without an ID
	// is invisible in the audit trail.
	ID       string
	Tool     string                         // exact tool name, or "*" for any
	Match    func(args map[string]any) bool // optional extra predicate; nil = always
	Decision Decision
}

func (r Rule) matches(tool string, args map[string]any) bool {
	if r.Tool != "*" && r.Tool != tool {
		return false
	}
	if r.Match == nil {
		return true
	}
	return r.Match(args)
}

// PolicyEngine evaluates tool calls against an ordered rule list.
// Deny-by-default: with no matching rule, it denies.
type PolicyEngine struct {
	mu    sync.RWMutex
	rules []Rule
}

// NewPolicyEngine returns an engine that denies everything by default.
func NewPolicyEngine() *PolicyEngine {
	return &PolicyEngine{}
}

// AddRule appends a rule. Rules are evaluated in insertion order; the first
// match wins.
func (e *PolicyEngine) AddRule(r Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = append(e.rules, r)
}

// Evaluate returns the decision for a tool call. The returned decision carries
// the matched rule's ID so the ledger can attribute it.
func (e *PolicyEngine) Evaluate(tool string, args map[string]any) Decision {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, r := range e.rules {
		if r.matches(tool, args) {
			d := r.Decision
			d.RuleID = r.ID
			return d
		}
	}
	return Deny("no rule allows " + tool)
}
