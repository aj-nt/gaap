package platform

// Rule matches a tool call and produces a decision when it matches.
type Rule struct {
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
	rules []Rule
}

// NewPolicyEngine returns an engine that denies everything by default.
func NewPolicyEngine() *PolicyEngine {
	return &PolicyEngine{}
}

// AddRule appends a rule. Rules are evaluated in insertion order; the first
// match wins.
func (e *PolicyEngine) AddRule(r Rule) {
	e.rules = append(e.rules, r)
}

// Evaluate returns the decision for a tool call.
func (e *PolicyEngine) Evaluate(tool string, args map[string]any) Decision {
	for _, r := range e.rules {
		if r.matches(tool, args) {
			return r.Decision
		}
	}
	return Deny("no rule allows " + tool)
}
