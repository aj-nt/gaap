package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const demoPolicy = `{
  "version": 1,
  "rules": [
    {"id": "read-only-tools", "tool": "ReadFile", "action": "allow"},
    {"id": "no-destructive-shell", "tool": "Execute", "action": "deny",
     "reason": "destructive shell command", "args_contain": {"command": "rm -rf"}},
    {"id": "allow-tests", "tool": "Execute", "action": "allow",
     "args_contain": {"command": "go test"}}
  ]
}`

func TestParsePolicyBuildsRulesInOrder(t *testing.T) {
	rules, err := ParsePolicy(strings.NewReader(demoPolicy))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(rules))
	}
	if rules[0].ID != "read-only-tools" || rules[0].Tool != "ReadFile" || rules[0].Decision.Action != ActionAllow {
		t.Fatalf("rule 0 = %+v", rules[0])
	}
	if rules[1].ID != "no-destructive-shell" || rules[1].Decision.Action != ActionDeny {
		t.Fatalf("rule 1 = %+v", rules[1])
	}
	if rules[1].Decision.Reason != "destructive shell command" {
		t.Fatalf("rule 1 reason = %q", rules[1].Decision.Reason)
	}
}

func TestParsedPolicyEvaluatesFirstMatchWins(t *testing.T) {
	e := NewPolicyEngine()
	rules, err := ParsePolicy(strings.NewReader(demoPolicy))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	for _, r := range rules {
		e.AddRule(r)
	}

	if d := e.Evaluate("ReadFile", map[string]any{"path": "/etc/hosts"}); d.Action != ActionAllow || d.RuleID != "read-only-tools" {
		t.Fatalf("read decision = %+v", d)
	}
	d := e.Evaluate("Execute", map[string]any{"command": "go test ./..."})
	if d.Action != ActionAllow || d.RuleID != "allow-tests" {
		t.Fatalf("test decision = %+v, want allowed by allow-tests", d)
	}
	if d := e.Evaluate("Execute", map[string]any{"command": "ls"}); d.Action != ActionDeny || d.RuleID != "" {
		t.Fatalf("unmatched Execute = %+v, want deny-by-default with no rule ID", d)
	}
}

func TestArgsContainPredicate(t *testing.T) {
	e := NewPolicyEngine()
	rules, _ := ParsePolicy(strings.NewReader(demoPolicy))
	for _, r := range rules {
		e.AddRule(r)
	}

	d := e.Evaluate("Execute", map[string]any{"command": "rm -rf /tmp/gaap-demo"})
	if d.Action != ActionDeny || d.RuleID != "no-destructive-shell" {
		t.Fatalf("destructive command = %+v, want deny by no-destructive-shell", d)
	}
	// A predicate that does not match must fall through, not deny by accident.
	d = e.Evaluate("Execute", map[string]any{"command": "go vet ./..."})
	if d.RuleID != "" {
		t.Fatalf("go vet matched %q, want no rule match", d.RuleID)
	}
}

func TestParsePolicyRejectsUnknownAction(t *testing.T) {
	_, err := ParsePolicy(strings.NewReader(`{"rules":[{"id":"x","tool":"*","action":"maybe"}]}`))
	if err == nil {
		t.Fatal("unknown action was accepted")
	}
}

func TestParsePolicyRejectsMissingID(t *testing.T) {
	_, err := ParsePolicy(strings.NewReader(`{"rules":[{"tool":"*","action":"allow"}]}`))
	if err == nil {
		t.Fatal("rule without an ID was accepted; it would be unattributable in the ledger")
	}
}

func TestParsePolicyRejectsDuplicateIDs(t *testing.T) {
	_, err := ParsePolicy(strings.NewReader(`{"rules":[
	  {"id":"dup","tool":"A","action":"allow"},
	  {"id":"dup","tool":"B","action":"allow"}]}`))
	if err == nil {
		t.Fatal("duplicate rule IDs were accepted; the audit trail would be ambiguous")
	}
}

func TestParsePolicyRejectsMissingTool(t *testing.T) {
	_, err := ParsePolicy(strings.NewReader(`{"rules":[{"id":"x","action":"allow"}]}`))
	if err == nil {
		t.Fatal("rule without a tool was accepted")
	}
}

func TestParsePolicyRequiresReasonOnNonAllow(t *testing.T) {
	_, err := ParsePolicy(strings.NewReader(`{"rules":[{"id":"x","tool":"*","action":"deny"}]}`))
	if err == nil {
		t.Fatal("deny rule with no reason was accepted; the ledger would record a bare deny")
	}
}

func TestParsePolicyRejectsUnknownVersion(t *testing.T) {
	_, err := ParsePolicy(strings.NewReader(`{"version":7,"rules":[]}`))
	if err == nil {
		t.Fatal("unknown policy version was accepted")
	}
}

func TestParsePolicyAcceptsEmptyRuleList(t *testing.T) {
	rules, err := ParsePolicy(strings.NewReader(`{"version":1,"rules":[]}`))
	if err != nil {
		t.Fatalf("empty rule list rejected: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("rules = %d, want 0", len(rules))
	}
}

func TestParsePolicyRejectsMalformedJSON(t *testing.T) {
	if _, err := ParsePolicy(strings.NewReader(`{nope`)); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
}

func TestLoadPolicyFileReadsFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(demoPolicy), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	e, err := LoadPolicyFile(path)
	if err != nil {
		t.Fatalf("LoadPolicyFile: %v", err)
	}
	if d := e.Evaluate("ReadFile", nil); d.Action != ActionAllow {
		t.Fatalf("loaded policy did not take effect: %+v", d)
	}
}

func TestLoadPolicyFileMissingIsAnError(t *testing.T) {
	if _, err := LoadPolicyFile(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("missing policy file was accepted")
	}
}
