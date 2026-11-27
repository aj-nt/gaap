package platform

import (
	"fmt"
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
	// Reversibility is the rule's own declaration of whether the effect can be
	// undone. Empty means none declared, and the governor falls back to the
	// workload's classifier. A declaration lives on the rule so the instrument
	// that authorised the call is versioned with it, and one channel serves both
	// the CLI and the orchestrator.
	Reversibility Reversibility
	// Artifact declares what durable thing this rule may create. nil means the
	// rule declares none, which is a real answer: the consequence is unrecorded,
	// not absent.
	Artifact *Artifact
	// Predicates are conditions the call must satisfy, in addition to the tool
	// match. A rule should use Predicates or Match, not both: Match is the escape
	// hatch for rules built in code, and predicates are what a linter can inspect.
	Predicates []Predicate
	// MinTaint is the weakest provenance this rule will accept for an allow. A call
	// justified by weaker evidence is reviewed rather than allowed: the standing
	// order permits the effect, not for this reason.
	MinTaint TaintClass
}

func (r Rule) matches(tool string, args map[string]any) bool {
	if r.Tool != "*" && r.Tool != tool {
		return false
	}
	// Predicates are ANDed with each other and with Match, never ORed. Two
	// conjunctive conditions can only narrow a rule; a disjunction is how a rule
	// stops meaning anything specific.
	for _, p := range r.Predicates {
		if !p.matches(tool, args) {
			return false
		}
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
	// revision identifies the exact instrument in force: a sha256 over the bytes
	// of the file it was loaded from. Empty for an engine assembled by hand, which
	// is the honest answer — no file, no revision, and the record says so rather
	// than inventing one.
	revision string
}

// Revision returns the content revision of the policy in force, or "" when the
// engine was built in code.
func (e *PolicyEngine) Revision() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.revision
}

// setRevision records the content revision. Called by the loader, never by a
// caller building rules by hand.
func (e *PolicyEngine) setRevision(rev string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revision = rev
}

// NewPolicyEngine returns an engine that denies everything by default.
func NewPolicyEngine() *PolicyEngine {
	return &PolicyEngine{}
}

// Tools returns every tool name the policy mentions, for checking against a catalogue. The
// wildcard is not a name, so it is not returned: there is nothing to check it against.
func (e *PolicyEngine) Tools() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	seen := make(map[string]struct{}, len(e.rules))
	for _, r := range e.rules {
		if r.Tool == "" || r.Tool == "*" {
			continue
		}
		seen[r.Tool] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// AddRule appends a rule. Rules are evaluated in insertion order; the first
// match wins.
func (e *PolicyEngine) AddRule(r Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = append(e.rules, r)
}

// RequiringPreState names the rules whose declared artifact is a deletion. Such a
// call cannot be honoured without a pre-state capturer, and the honest place to say
// so is when the policy is loaded, not when the call arrives: a rule that can only
// ever be refused is configuration that reads as a permission.
func (e *PolicyEngine) RequiringPreState() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var ids []string
	for _, r := range e.rules {
		if r.Artifact != nil && r.Artifact.Type == ArtifactDelete {
			id := r.ID
			if id == "" {
				id = "(unnamed rule for " + r.Tool + ")"
			}
			ids = append(ids, id)
		}
	}
	return ids
}

// CheckPreState reports whether the policy can be honoured with the capturer that
// is actually configured. It is called at startup rather than per call: a rule
// that declares a deletion and cannot be satisfied is configuration that reads as
// a permission and behaves as a refusal, and the operator should hear about it
// once, by name, before the first call.
func (e *PolicyEngine) CheckPreState(capturerSet bool) error {
	if capturerSet {
		return nil
	}
	if ids := e.RequiringPreState(); len(ids) > 0 {
		return fmt.Errorf("policy declares deletions that need a pre-state capturer, and none is configured: %v", ids)
	}
	return nil
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
			// The declaration travels with the decision, so the recorder has one
			// source for what this call may cost and needs no lookup of its own.
			d.Reversibility = r.Reversibility
			d.Artifact = r.Artifact
			d.MinTaint = r.MinTaint
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
