package platform

import "testing"

func TestPolicyEngineDenyByDefault(t *testing.T) {
	e := NewPolicyEngine()
	if got := e.Evaluate("Execute", map[string]any{"command": "ls"}); got.Action != ActionDeny {
		t.Fatalf("empty engine = %q, want deny-by-default", got.Action)
	}
}

func TestPolicyEngineFirstMatchWins(t *testing.T) {
	e := NewPolicyEngine()
	e.AddRule(Rule{Tool: "ReadFile", Decision: Allow})
	e.AddRule(Rule{Tool: "*", Decision: Deny("not read")})

	if got := e.Evaluate("ReadFile", nil); got.Action != ActionAllow {
		t.Errorf("ReadFile = %q, want allow", got.Action)
	}
	if got := e.Evaluate("Execute", nil); got.Action != ActionDeny {
		t.Errorf("Execute = %q, want deny", got.Action)
	}
}

func TestPolicyEngineRulePredicate(t *testing.T) {
	e := NewPolicyEngine()
	e.AddRule(Rule{
		Tool: "Execute",
		Match: func(args map[string]any) bool {
			return args["command"] == "ls"
		},
		Decision: Allow,
	})

	if got := e.Evaluate("Execute", map[string]any{"command": "ls"}); got.Action != ActionAllow {
		t.Errorf("ls = %q, want allow", got.Action)
	}
	if got := e.Evaluate("Execute", map[string]any{"command": "rm -rf /"}); got.Action != ActionDeny {
		t.Errorf("rm = %q, want deny", got.Action)
	}
}
