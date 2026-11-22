package platform

import (
	"strings"
	"testing"
)

const floorPolicy = `{
  "version": 1,
  "rules": [
    {"id": "rotate-key", "tool": "RotateKey", "action": "allow", "min_taint": "internal"},
    {"id": "read-docs", "tool": "ReadDocs", "action": "allow"},
    {"id": "no-shell", "tool": "Execute", "action": "deny", "reason": "no", "min_taint": "internal"}
  ]
}`

// R5: taint reaches the verdict. A rule requiring internal provenance is saying the
// effect is permitted but the reason is not, and a well-written summary of a web page
// is exactly the justification that must not carry it.
func TestWeakJustificationDowngradesAllow(t *testing.T) {
	g := governorFromPolicy(t, floorPolicy)

	d := g.Decide("agent-7", "RotateKey", nil, WithFloor(TaintExternal))
	if d.Action != ActionReview {
		t.Fatalf("action = %q, want review", d.Action)
	}
	if !strings.Contains(d.Reason, "requires internal") {
		t.Fatalf("reason does not explain the downgrade: %q", d.Reason)
	}
	if d.Floor != TaintExternal {
		t.Fatalf("floor not recorded on the decision: %q", d.Floor)
	}

	// Strong enough: the allow stands.
	d = g.Decide("agent-7", "RotateKey", nil, WithFloor(TaintInternal))
	if d.Action != ActionAllow {
		t.Fatalf("internal justification = %q, want allow", d.Action)
	}
	d = g.Decide("agent-7", "RotateKey", nil, WithFloor(TaintFact))
	if d.Action != ActionAllow {
		t.Fatalf("fact justification = %q, want allow", d.Action)
	}
}

// No floor supplied is not the same as external, but it fails the requirement all the
// same — and the reason says which it was.
func TestUnreportedFloorFailsTheRequirement(t *testing.T) {
	g := governorFromPolicy(t, floorPolicy)
	d := g.Decide("agent-7", "RotateKey", nil)
	if d.Action != ActionReview {
		t.Fatalf("action = %q, want review when no floor is supplied", d.Action)
	}
	if !strings.Contains(d.Reason, "unreported") {
		t.Fatalf("reason = %q, want it to say the floor was unreported", d.Reason)
	}
}

// A rule asking for nothing is unaffected by the floor: requiring provenance
// everywhere would review every call and train operators to ignore reviews.
func TestNoRequirementIgnoresFloor(t *testing.T) {
	g := governorFromPolicy(t, floorPolicy)
	d := g.Decide("agent-7", "ReadDocs", nil, WithFloor(TaintExternal))
	if d.Action != ActionAllow {
		t.Fatalf("action = %q, want allow for a rule with no min_taint", d.Action)
	}
}

// A deny must never become an allow because the justification looks good, and must not
// become a review either: refusals are terminal.
func TestFloorDoesNotWeakenDeny(t *testing.T) {
	g := governorFromPolicy(t, floorPolicy)
	for _, floor := range []TaintClass{TaintExternal, TaintInternal, TaintFact, ""} {
		d := g.Decide("agent-7", "Execute", nil, WithFloor(floor))
		if d.Action != ActionDeny {
			t.Fatalf("floor %q turned a deny into %q", floor, d.Action)
		}
	}
}

func TestUnknownMinTaintRefusedAtLoad(t *testing.T) {
	const bad = `{"version":1,"rules":[{"id":"x","tool":"T","action":"allow","min_taint":"trusted"}]}`
	if _, err := ParsePolicy(strings.NewReader(bad)); err == nil {
		t.Fatal("an unknown min_taint was accepted")
	}
}

func TestTaintRankOrdering(t *testing.T) {
	if TaintExternal.Rank() >= TaintInternal.Rank() || TaintInternal.Rank() >= TaintFact.Rank() {
		t.Fatal("taint ranks do not increase external < internal < fact")
	}
	// A typo must never rank high enough to satisfy a requirement.
	if TaintClass("fcat").Rank() >= TaintFact.Rank() {
		t.Fatal("an unknown class ranks at or above fact")
	}
	if TaintBelow("", "") {
		t.Fatal("no requirement compared as failing")
	}
}
