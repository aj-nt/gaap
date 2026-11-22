package platform

import (
	"sort"
	"sync"
)

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
	// ActionClass is what the call is FOR, independent of what it is called.
	// A refusal counter keyed on the tool name is defeated by renaming the tool;
	// keyed on the class, a reframe increments the same counter.
	ActionClass ActionClass
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

// ActionClassOf returns the class declared by the first rule that names the tool.
// A deny rule declares the class of what it forbids, which is the case that
// matters: the counter has to fire on the refused call, not only on an allowed
// one. Empty when no rule names the tool.
func (e *PolicyEngine) ActionClassOf(tool string) ActionClass {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, r := range e.rules {
		if r.Tool == tool {
			return r.ActionClass
		}
	}
	return ""
}

// PermittedFor returns the tools these standing orders do allow, excluding the
// one being refused. Sorted and de-duplicated, so the text rendered to an agent
// is stable across runs.
//
// It answers the only question a refused agent can act on: is there another way
// to do this, or is the whole class closed to me. Empty means closed.
func (e *PolicyEngine) PermittedFor(tool string) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	seen := make(map[string]struct{}, len(e.rules))
	out := make([]string, 0, len(e.rules))
	for _, r := range e.rules {
		if r.Decision.Action != ActionAllow {
			continue
		}
		// A wildcard allow says nothing specific, and naming it would invite the
		// agent to look for a tool called "*".
		if r.Tool == "" || r.Tool == "*" || r.Tool == tool {
			continue
		}
		if _, dup := seen[r.Tool]; dup {
			continue
		}
		seen[r.Tool] = struct{}{}
		out = append(out, r.Tool)
	}
	sort.Strings(out)
	return out
}
