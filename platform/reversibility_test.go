package platform

import "testing"

func TestDefaultClassifyIsIrreversible(t *testing.T) {
	// The kernel does not know the workload's tool surface, so it never guesses
	// a read is pure — everything defaults to irreversible. This is the honesty
	// rule: never stamp a class whose inverse the runtime does not actually hold.
	for _, tool := range []string{"", "Execute", "ExecuteCode", "WriteFile", "FileWrite", "FileRead", "FileSearch", "ReadFile", "SearchMemories", "UnknownTool"} {
		if got := classify(tool); got != Irreversible {
			t.Fatalf("classify(%q) = %q, want irreversible (conservative default)", tool, got)
		}
	}
}

func TestGovernorDefaultsToConservativeClassifier(t *testing.T) {
	g := NewGovernor()
	g.Policy.AddRule(Rule{Tool: "FileRead", Decision: Allow})
	g.Decide("agent-1", "FileRead", nil) // allowed read, but kernel can't know it's a read

	entries := g.Ledger.Entries()
	if len(entries) != 1 {
		t.Fatalf("ledger len = %d, want 1", len(entries))
	}
	if entries[0].Reversibility != Irreversible {
		t.Fatalf("FileRead = %q, want irreversible (no injected classifier)", entries[0].Reversibility)
	}
}

func TestGovernorUsesInjectedClassifier(t *testing.T) {
	g := NewGovernor()
	g.Classify = func(tool string) Reversibility {
		if tool == "SearchMemories" {
			return Pure
		}
		return Irreversible
	}
	g.Policy.AddRule(Rule{Tool: "*", Decision: Allow})
	g.Decide("agent-1", "SearchMemories", nil)
	g.Decide("agent-1", "FileWrite", nil)

	entries := g.Ledger.Entries()
	if len(entries) != 2 {
		t.Fatalf("ledger len = %d, want 2", len(entries))
	}
	if entries[0].Reversibility != Pure {
		t.Fatalf("SearchMemories = %q, want pure (injected classifier)", entries[0].Reversibility)
	}
	if entries[1].Reversibility != Irreversible {
		t.Fatalf("FileWrite = %q, want irreversible", entries[1].Reversibility)
	}
}
