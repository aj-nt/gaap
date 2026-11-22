package platform

import (
	"strings"
	"testing"
)

// The taxonomy is the load-bearing claim: what the observed party authors cannot
// carry a boundary.
func TestPredicateClassIsDerivedNotDeclared(t *testing.T) {
	stable := []PredicateKind{PredTool, PredEffect, PredIdentity, PredVolume, PredSequence}
	for _, k := range stable {
		if k.Class() != PredicateStable {
			t.Errorf("%q classed %q, want stable", k, k.Class())
		}
	}
	cosmetic := []PredicateKind{PredUA, PredFilename, PredHash}
	for _, k := range cosmetic {
		if k.Class() != PredicateCosmetic {
			t.Errorf("%q classed %q, want cosmetic", k, k.Class())
		}
	}
	// An unknown kind must never read as stable, or a typo silently becomes a
	// well-founded deny.
	if PredicateKind("toolname").Class() != PredicateCosmetic {
		t.Error("unknown kind classed stable")
	}
}

func TestPredicateMatchesExactValues(t *testing.T) {
	p := Predicate{Kind: PredEffect, Value: "credential_write"}
	if !p.matches("CreateToken", map[string]any{"effect": "credential_write"}) {
		t.Error("exact match failed")
	}
	if p.matches("CreateToken", map[string]any{"effect": "read"}) {
		t.Error("mismatch matched")
	}
	if p.matches("CreateToken", map[string]any{}) {
		t.Error("absent argument matched")
	}
}

// A literal starting with "<" must not be parsed as a comparison.
func TestLiteralAngleBracketIsNotAComparison(t *testing.T) {
	p := Predicate{Kind: PredFilename, Value: "<script>"}
	if !p.matches("T", map[string]any{"filename": "<script>"}) {
		t.Fatal("a literal <script> was treated as an operator")
	}
}

func TestNumericComparisons(t *testing.T) {
	cases := []struct {
		value string
		arg   any
		want  bool
	}{
		{">=100", 150, true},
		{">=100", 50, false},
		{"<10", 3, true},
		{">10", 10, false},
		{"<=10", 10, true},
		{">=100", "120", true}, // numeric strings are accepted
		{">=100", "lots", false},
	}
	for _, c := range cases {
		p := Predicate{Kind: PredVolume, Value: c.value}
		if got := p.matches("T", map[string]any{"volume": c.arg}); got != c.want {
			t.Errorf("%s against %v = %v, want %v", c.value, c.arg, got, c.want)
		}
	}
}

const predicatePolicy = `{
  "version": 1,
  "rules": [
    {"id": "bulk-credential-write", "tool": "CreateToken", "action": "deny",
     "reason": "bulk credential creation",
     "predicates": [{"kind": "effect", "value": "credential_write"}, {"kind": "volume", "value": ">=100"}]},
    {"id": "allow-small", "tool": "CreateToken", "action": "allow"}
  ]
}`

func TestPredicatesGateTheRule(t *testing.T) {
	rules, err := ParsePolicy(strings.NewReader(predicatePolicy))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	e := NewPolicyEngine()
	for _, r := range rules {
		e.AddRule(r)
	}

	// Matches both predicates → the deny fires.
	got := e.Evaluate("CreateToken", map[string]any{"effect": "credential_write", "volume": 500})
	if got.Action != ActionDeny || got.RuleID != "bulk-credential-write" {
		t.Fatalf("decision = %+v, want the predicate deny", got)
	}
	// Right effect, small volume → falls through to the allow.
	got = e.Evaluate("CreateToken", map[string]any{"effect": "credential_write", "volume": 2})
	if got.Action != ActionAllow {
		t.Fatalf("decision = %+v, want allow for a small volume", got)
	}
	// No arguments at all → predicates cannot be satisfied, so the deny must not
	// fire on a call it knows nothing about.
	got = e.Evaluate("CreateToken", nil)
	if got.Action != ActionAllow {
		t.Fatalf("decision = %+v, want allow when predicates cannot be evaluated", got)
	}
}

// A deny resting only on what the caller authors is matched by anyone who reads
// it. Refused at load rather than recorded as a control.
func TestCosmeticOnlyDenyRefused(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"ua","tool":"Browse","action":"deny","reason":"ua","predicates":[{"kind":"user_agent","value":"artex-enrich/1.0"}]}]}`
	_, err := ParsePolicy(strings.NewReader(p))
	if err == nil {
		t.Fatal("a user-agent-only deny was accepted")
	}
	if !strings.Contains(err.Error(), "cosmetic") {
		t.Fatalf("error does not name the problem: %v", err)
	}
}

// One stable predicate is enough to make the rest admissible: the stable one is
// what the caller would have to change their behaviour to evade.
func TestCosmeticPlusStableDenyAccepted(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"mix","tool":"Browse","action":"deny","reason":"mix","predicates":[{"kind":"user_agent","value":"x"},{"kind":"identity","value":"svc-1"}]}]}`
	if _, err := ParsePolicy(strings.NewReader(p)); err != nil {
		t.Fatalf("a deny with one stable predicate was refused: %v", err)
	}
}

// An empty predicate set is not cosmetic-only: naming a tool already names
// something stable, and treating it otherwise would refuse every ordinary deny.
func TestPlainToolDenyStillAccepted(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"no-shell","tool":"Execute","action":"deny","reason":"no"}]}`
	if _, err := ParsePolicy(strings.NewReader(p)); err != nil {
		t.Fatalf("a plain tool deny was refused: %v", err)
	}
}

func TestUnknownPredicateKindRefused(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"x","tool":"T","action":"allow","predicates":[{"kind":"toolname","value":"y"}]}]}`
	if _, err := ParsePolicy(strings.NewReader(p)); err == nil {
		t.Fatal("unknown predicate kind accepted")
	}
}

func TestPredicateWithoutValueRefused(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"x","tool":"T","action":"allow","predicates":[{"kind":"effect"}]}]}`
	if _, err := ParsePolicy(strings.NewReader(p)); err == nil {
		t.Fatal("predicate with no value accepted")
	}
}

// Predicates are ANDed with an existing args_contain match, never ORed: two
// conjunctive conditions can only narrow a rule.
func TestPredicatesAndArgsContainBothApply(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"x","tool":"Execute","action":"deny","reason":"rm",
	  "args_contain": {"command": "rm"},
	  "predicates": [{"kind": "identity", "value": "svc-1"}]}]}`
	rules, err := ParsePolicy(strings.NewReader(p))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	r := rules[0]
	if !r.matches("Execute", map[string]any{"command": "rm -rf /", "identity": "svc-1"}) {
		t.Fatal("both conditions satisfied but the rule did not match")
	}
	if r.matches("Execute", map[string]any{"command": "ls", "identity": "svc-1"}) {
		t.Fatal("args_contain failed but the rule matched")
	}
	if r.matches("Execute", map[string]any{"command": "rm -rf /", "identity": "svc-2"}) {
		t.Fatal("predicate failed but the rule matched")
	}
}
