package platform

import (
	"strings"
	"testing"
)

func TestMutatingRuleWithoutArtifactFlagged(t *testing.T) {
	const p = `{"version":1,"rules":[
	  {"id":"needs-artifact","tool":"CreateUser","action":"allow","reversibility":"irreversible","class":"network"},
	  {"id":"has-artifact","tool":"CreateToken","action":"allow","reversibility":"irreversible","class":"self_extension","artifact":{"type":"credential","max_lifetime_seconds":60}},
	  {"id":"pure-read","tool":"ReadFile","action":"allow","reversibility":"pure","class":"file_write"}
	]}`
	rules, err := ParsePolicy(strings.NewReader(p))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	e := NewPolicyEngine()
	for _, r := range rules {
		e.AddRule(r)
	}
	findings := e.Lint()

	var flagged []string
	for _, f := range findings {
		if f.Level == LintError {
			flagged = append(flagged, f.RuleID)
		}
	}
	if len(flagged) != 1 || flagged[0] != "needs-artifact" {
		t.Fatalf("errors on %v, want only needs-artifact", flagged)
	}
	if !HasErrors(findings) {
		t.Fatal("a missing artifact declaration should count as an error")
	}
}

// A missing action class is a warning: refusals still work, they are just
// uncountable by class. Making it an error would fail every policy written before
// the field existed.
func TestMissingActionClassIsAWarning(t *testing.T) {
	e := NewPolicyEngine()
	e.AddRule(Rule{ID: "no-class", Tool: "T", Decision: Allow})
	findings := e.Lint()
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want one warning", findings)
	}
	if findings[0].Level != LintWarning {
		t.Fatalf("level = %q, want warning", findings[0].Level)
	}
	if HasErrors(findings) {
		t.Fatal("a warning must not fail a strict run")
	}
}

func TestCleanPolicyLintsClean(t *testing.T) {
	e := NewPolicyEngine()
	e.AddRule(Rule{
		ID: "ok", Tool: "T", Decision: Allow,
		Reversibility: Pure, ActionClass: ClassFileWrite,
	})
	if got := e.Lint(); len(got) != 0 {
		t.Fatalf("findings = %v, want none", got)
	}
}

// A deny rule needs no artifact: it permits nothing.
func TestDenyNeedsNoArtifact(t *testing.T) {
	e := NewPolicyEngine()
	e.AddRule(Rule{
		ID: "no-shell", Tool: "Execute", Decision: Deny("no"),
		Reversibility: Irreversible, ActionClass: ClassShell,
	})
	if got := e.Lint(); len(got) != 0 {
		t.Fatalf("findings = %v, want none for a deny", got)
	}
}
