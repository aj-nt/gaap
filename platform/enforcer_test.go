package platform

import "testing"

func TestGovernorWithNoopEnforcer(t *testing.T) {
	g := NewGovernor()
	g.Enforcer = &NoopEnforcer{}
	g.Policy.AddRule(Rule{Tool: "Execute", Decision: Allow})

	if _, err := g.Enforcer.Spawn(nil, NewNamespaceSpec(10001, "/w"), "alpine", "true"); err != nil {
		t.Fatalf("noop Spawn errored: %v", err)
	}
	if g.Enforcer == nil {
		t.Fatal("governor should hold an enforcer")
	}
}

func TestGovernorDefaultsToNoopEnforcer(t *testing.T) {
	g := NewGovernor()
	if g.Enforcer == nil {
		t.Fatal("NewGovernor must default to a NoopEnforcer (enforce nothing, like today)")
	}
}
