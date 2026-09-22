package platform

import "testing"

func TestClassifyDefaultsToIrreversible(t *testing.T) {
	for _, tool := range []string{"", "Execute", "ExecuteCode", "WriteFile", "BackgroundProcess", "FileEdit", "SendMessage", "UnknownTool"} {
		if got := classify(tool); got != Irreversible {
			t.Fatalf("classify(%q) = %q, want irreversible (conservative default)", tool, got)
		}
	}
}

func TestClassifyReadsArePure(t *testing.T) {
	for _, tool := range []string{"ReadFile", "FileRead", "FileSearch"} {
		if got := classify(tool); got != Pure {
			t.Fatalf("classify(%q) = %q, want pure", tool, got)
		}
	}
}

func TestGovernorStampsReversibilityOnDecision(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{Tool: "ReadFile", Decision: Allow})
	g.Decide("agent-1", "ReadFile", nil) // allowed read
	g.Decide("agent-1", "Execute", nil)  // denied (no rule)

	entries := g.Ledger.Entries()
	if len(entries) != 2 {
		t.Fatalf("ledger len = %d, want 2", len(entries))
	}
	if entries[0].Reversibility != Pure {
		t.Fatalf("ReadFile entry reversibility = %q, want pure", entries[0].Reversibility)
	}
	if entries[1].Reversibility != Irreversible {
		t.Fatalf("Execute entry reversibility = %q, want irreversible", entries[1].Reversibility)
	}
}
