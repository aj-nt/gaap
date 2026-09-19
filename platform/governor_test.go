package platform

import "testing"

func TestGovernorKillSwitchWins(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{Tool: "*", Decision: Allow})
	g.Kill.Trip("agent-7")

	d := g.Decide("agent-7", "Execute", map[string]any{"command": "ls"})
	if d.Action != ActionDeny {
		t.Fatalf("kill-switched call = %q, want deny", d.Action)
	}
	if d.Reason == "" {
		t.Fatal("kill-switch deny must carry a reason")
	}
}

func TestGovernorRecordsEveryDecision(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{Tool: "ReadFile", Decision: Allow})
	g.Decide("agent-1", "ReadFile", nil)
	g.Decide("agent-1", "Execute", nil) // denied (no rule)

	if got := len(g.Ledger.Entries()); got != 2 {
		t.Fatalf("ledger len = %d, want 2 (one per decision)", got)
	}
}

func TestGovernorDenyPenalizesTrust(t *testing.T) {
	g := NewGovernor() // trust starts at 500
	before := g.Trust.Score()
	g.Decide("agent-1", "Execute", nil) // denied
	if got := g.Trust.Score(); got >= before {
		t.Fatalf("deny did not penalize trust: %v >= %v", got, before)
	}
}
