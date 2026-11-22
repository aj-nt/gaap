package platform

import (
	"strings"
	"testing"
)

const classedPolicy = `{
  "version": 1,
  "rules": [
    {"id": "read-files", "tool": "ReadFile", "action": "allow", "class": "file_write"},
    {"id": "write-files", "tool": "WriteFile", "action": "allow", "class": "file_write"},
    {"id": "no-shell", "tool": "Execute", "action": "deny", "reason": "no shell here", "class": "shell"},
    {"id": "any-tool", "tool": "*", "action": "allow", "class": "network"}
  ]
}`

func mustParse(t *testing.T, src string) []Rule {
	t.Helper()
	rules, err := ParsePolicy(strings.NewReader(src))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	return rules
}

func engineFrom(t *testing.T, src string) *PolicyEngine {
	t.Helper()
	e := NewPolicyEngine()
	for _, r := range mustParse(t, src) {
		e.AddRule(r)
	}
	return e
}

func TestPolicyFileCarriesActionClass(t *testing.T) {
	rules := mustParse(t, classedPolicy)
	if rules[2].ActionClass != ClassShell {
		t.Fatalf("deny rule class = %q, want %q", rules[2].ActionClass, ClassShell)
	}
}

// A misspelled class must fail the load. Accepted silently, it would leave the
// refusal counter keyed on "" and every denial would land in one bucket.
func TestUnknownClassRejectedAtLoad(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"x","tool":"Execute","action":"deny","reason":"no","class":"shells"}]}`
	_, err := ParsePolicy(strings.NewReader(p))
	if err == nil {
		t.Fatal("ParsePolicy accepted an undeclared class")
	}
	if !strings.Contains(err.Error(), "class") {
		t.Fatalf("error does not name the class: %v", err)
	}
}

func TestAbsentClassLoads(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"x","tool":"ReadFile","action":"allow"}]}`
	rules := mustParse(t, p)
	if rules[0].ActionClass != "" {
		t.Fatalf("absent class = %q, want empty", rules[0].ActionClass)
	}
}

func TestActionClassOfUsesFirstRuleNamingTheTool(t *testing.T) {
	e := engineFrom(t, classedPolicy)
	if got := e.ActionClassOf("Execute"); got != ClassShell {
		t.Fatalf("ActionClassOf(Execute) = %q, want shell", got)
	}
	if got := e.ActionClassOf("ReadFile"); got != ClassFileWrite {
		t.Fatalf("ActionClassOf(ReadFile) = %q, want file_write", got)
	}
	if got := e.ActionClassOf("Unknown"); got != "" {
		t.Fatalf("ActionClassOf(Unknown) = %q, want empty", got)
	}
}

// The permitted list is the only actionable thing in a refusal: either another
// way exists or the class is closed.
func TestPermittedForExcludesTheRefusedToolAndNonAllows(t *testing.T) {
	e := engineFrom(t, classedPolicy)
	got := e.PermittedFor("Execute")
	want := []string{"ReadFile", "WriteFile"}
	if len(got) != len(want) {
		t.Fatalf("PermittedFor(Execute) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PermittedFor(Execute) = %v, want %v (sorted, and no wildcard)", got, want)
		}
	}
}

func TestPermittedForEmptyWhenNothingAllowed(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"no","tool":"Execute","action":"deny","reason":"no","class":"shell"}]}`
	e := engineFrom(t, p)
	if got := e.PermittedFor("Execute"); len(got) != 0 {
		t.Fatalf("PermittedFor = %v, want empty", got)
	}
}

func TestPermittedForIgnoresWildcardAllow(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"all","tool":"*","action":"allow"}]}`
	e := engineFrom(t, p)
	if got := e.PermittedFor("Execute"); len(got) != 0 {
		t.Fatalf("PermittedFor = %v, want empty: a wildcard is not a named alternative", got)
	}
}

// A misspelled key is a rule the operator believes they wrote and did not. It
// used to load as a rule with no action and produce a message naming nothing.
func TestUnknownRuleKeyRejected(t *testing.T) {
	const p = `{"version":1,"rules":[{"id":"x","tool":"Execute","action":"allow","reson":"because"}]}`
	_, err := ParsePolicy(strings.NewReader(p))
	if err == nil {
		t.Fatal("ParsePolicy accepted a misspelled rule key")
	}
	if !strings.Contains(err.Error(), "reson") {
		t.Fatalf("error does not name the offending key: %v", err)
	}
}

func TestUnknownTopLevelKeyRejected(t *testing.T) {
	const p = `{"versoin":1,"rules":[{"id":"x","tool":"ReadFile","action":"allow"}]}`
	_, err := ParsePolicy(strings.NewReader(p))
	if err == nil {
		t.Fatal("ParsePolicy accepted a misspelled top-level key")
	}
	if !strings.Contains(err.Error(), "versoin") {
		t.Fatalf("error does not name the offending key: %v", err)
	}
}
