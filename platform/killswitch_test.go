package platform

import "testing"

func TestKillSwitchTripIsIrreversible(t *testing.T) {
	k := NewKillSwitch()
	k.Trip("agent-7")
	k.Trip("agent-7") // idempotent
	if !k.Tripped("agent-7") {
		t.Fatal("expected tripped after Trip")
	}
	// There is deliberately no Untrip method. Compile-time irreversibility:
	// the only public mutations are Trip and Tripped.
}

func TestKillSwitchGlobalAppliesToAll(t *testing.T) {
	k := NewKillSwitch()
	if k.Tripped("agent-9") {
		t.Fatal("untripped switch must not report tripped")
	}
	k.Trip("global")
	if !k.Tripped("agent-9") {
		t.Fatal("global trip must trip every scope")
	}
}
