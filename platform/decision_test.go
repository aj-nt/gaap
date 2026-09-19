package platform

import "testing"

func TestDecisionConstructors(t *testing.T) {
	if Allow.Action != ActionAllow {
		t.Errorf("Allow.Action = %q, want allow", Allow.Action)
	}
	d := Deny("no rule")
	if d.Action != ActionDeny || d.Reason != "no rule" {
		t.Errorf("Deny = %+v, want deny with reason", d)
	}
	if d.Reason == "" {
		t.Error("deny decision must carry a reason")
	}
}
