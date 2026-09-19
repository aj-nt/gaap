package platform

import (
	"context"
	"testing"
)

func TestGovernorWithNoopEnforcer(t *testing.T) {
	g := NewGovernor()
	g.Enforcer = &NoopEnforcer{}
	g.Policy.AddRule(Rule{Tool: "Execute", Decision: Allow})

	if err := g.Enforcer.Ensure(context.Background()); err != nil {
		t.Fatalf("noop Ensure errored: %v", err)
	}
	if _, err := g.Enforcer.Run(context.Background(), NewNamespaceSpec(10001, "/w"), "alpine", "true"); err != nil {
		t.Fatalf("noop Run errored: %v", err)
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
