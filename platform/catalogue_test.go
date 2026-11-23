package platform

import (
	"strings"
	"testing"
)

func cataloguePolicy() *PolicyEngine {
	e := NewPolicyEngine()
	e.AddRule(Rule{ID: "read", Tool: "FileRead", Decision: Allow})
	e.AddRule(Rule{ID: "write", Tool: "FileWrite", Decision: Allow})
	e.AddRule(Rule{ID: "shell", Tool: "Execute", Decision: Deny("the shell is off")})
	return e
}

// A refusal that advertises a tool the agent does not have sends it looking for a name it
// cannot call. The catalogue is the only thing that knows; when it is absent the kernel says
// nothing about alternatives rather than guessing.
func TestPermittedAlternativesAreLimitedToTheAgentsCatalogue(t *testing.T) {
	g := NewGovernor()
	g.Policy = cataloguePolicy()
	g.Catalogue = func(agentID string) []string {
		if agentID == "analyst" {
			return []string{"FileRead"}
		}
		return nil
	}

	d := g.Decide("analyst", "Execute", map[string]any{"command": "uname -a"})
	if d.Denial == nil {
		t.Fatalf("deny carried no denial class: %+v", d)
	}
	if got := strings.Join(d.Denial.Permitted, ","); got != "FileRead" {
		t.Fatalf("permitted = %q, want only FileRead: the agent has no FileWrite", got)
	}

	// No catalogue: the policy's list stands, unfiltered and unimproved.
	d = g.Decide("stranger", "Execute", nil)
	if d.Denial == nil || len(d.Denial.Permitted) != 2 {
		t.Fatalf("unknown catalogue filtered the policy's alternatives: %+v", d.Denial)
	}
}

// The broker originates refusals the policy never saw (a tool the agent does not have). They
// have to reach the ledger like any other, or the record is missing exactly the calls that
// were incoherent.
func TestRefuseRecordsTheRefusal(t *testing.T) {
	g := NewGovernor()
	g.Policy = cataloguePolicy()
	l := NewLedger()
	g.Ledger = l

	d := g.Refuse("analyst", "ReadFile", nil, "broker:unknown-tool",
		"this agent has no tool named ReadFile")
	if d.Action != ActionDeny {
		t.Fatalf("refusal = %+v", d)
	}
	if d.RuleID != "broker:unknown-tool" {
		t.Fatalf("rule = %q", d.RuleID)
	}
	if d.Denial != nil {
		t.Fatal("a broker-originated refusal was given a policy denial class: the agent would read it as a rule it could satisfy")
	}
	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want the refusal", len(entries))
	}
	if entries[0].Outcome != ActionDeny || entries[0].RuleID != "broker:unknown-tool" || entries[0].Tool != "ReadFile" {
		t.Fatalf("entry = %+v", entries[0])
	}
	if !strings.Contains(entries[0].Reason, "no tool named") {
		t.Fatalf("entry reason = %q", entries[0].Reason)
	}
}

func TestPolicyToolsListsEveryNamedTool(t *testing.T) {
	got := cataloguePolicy().Tools()
	want := map[string]bool{"FileRead": true, "FileWrite": true, "Execute": true}
	if len(got) != len(want) {
		t.Fatalf("Tools() = %v, want the three named tools", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("Tools() returned %q", name)
		}
	}

	// A wildcard names no tool, so it contributes no name to check against a catalogue.
	wild := NewPolicyEngine()
	wild.AddRule(Rule{ID: "all", Tool: "*", Decision: Deny("nothing")})
	if got := wild.Tools(); len(got) != 0 {
		t.Fatalf("wildcard produced %v", got)
	}
}
