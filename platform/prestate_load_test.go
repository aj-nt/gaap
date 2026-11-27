package platform

import (
	"strings"
	"testing"
)

const deleteArtifactPolicy = `{
  "version": 1,
  "rules": [
    {"id": "erase-account", "tool": "EraseAccount", "action": "allow",
     "reversibility": "irreversible",
     "artifact": {"type": "delete"}},
    {"id": "make-token", "tool": "CreateToken", "action": "allow",
     "reversibility": "irreversible",
     "artifact": {"type": "credential", "max_lifetime_seconds": 3600}},
    {"id": "no-shell", "tool": "Execute", "action": "deny", "reason": "no shell"}
  ]
}`

// A rule that declares a deletion can only be honoured with a capturer for the
// state being replaced. Nothing wires one yet, so the rule is a permission that
// every call turns into a refusal -- configuration that reads as an allow and
// behaves as a deny. Naming the rules when the policy loads is the honest fix;
// finding out per call is not.
func TestDeletionRulesAreNamedForThePreStateCheck(t *testing.T) {
	rules, err := ParsePolicy(strings.NewReader(deleteArtifactPolicy))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	e := NewPolicyEngine()
	for _, r := range rules {
		e.AddRule(r)
	}

	got := e.RequiringPreState()
	if len(got) != 1 || got[0] != "erase-account" {
		t.Fatalf("RequiringPreState = %v, want [erase-account] (a credential artifact does not need one)", got)
	}

	// An engine with no deletion rules must not be refused at startup.
	const plain = `{"version":1,"rules":[{"id":"no-shell","tool":"Execute","action":"deny","reason":"no"}]}`
	rules, err = ParsePolicy(strings.NewReader(plain))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	e2 := NewPolicyEngine()
	for _, r := range rules {
		e2.AddRule(r)
	}
	if got := e2.RequiringPreState(); len(got) != 0 {
		t.Fatalf("RequiringPreState = %v, want none", got)
	}
}

// The refusal has to name the rules, or the operator gets "invalid policy" and
// no way to know which line to change.
func TestPreStateRefusalNamesTheRules(t *testing.T) {
	rules, err := ParsePolicy(strings.NewReader(deleteArtifactPolicy))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	e := NewPolicyEngine()
	for _, r := range rules {
		e.AddRule(r)
	}
	err = e.CheckPreState(false)
	if err == nil {
		t.Fatal("a policy with deletions was accepted with no capturer")
	}
	if !strings.Contains(err.Error(), "erase-account") || !strings.Contains(err.Error(), "pre-state") {
		t.Fatalf("refusal does not name the rule and the missing piece: %v", err)
	}
	if err := e.CheckPreState(true); err != nil {
		t.Fatalf("a configured capturer still refused the policy: %v", err)
	}
}
