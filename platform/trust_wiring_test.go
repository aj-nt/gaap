package platform

import (
	"strings"
	"testing"
)

func trustGovernor() *Governor {
	g := NewGovernor()
	g.Policy.AddRule(Rule{Tool: "*", Decision: Allow})
	return g
}

// TestAllowRewardsTrustScore: the score was penalized on deny and never moved
// otherwise, so it could only ratchet down — decay plus penalties with no
// counterweight. Reward is the other half of the signal.
func TestAllowRewardsTrustScore(t *testing.T) {
	g := trustGovernor()
	before := g.Trust.Score()

	d := g.Decide("agent-1", "ReadFile", nil)
	if d.Action != ActionAllow {
		t.Fatalf("action = %q, want allow", d.Action)
	}
	if after := g.Trust.Score(); after != before+1 {
		t.Fatalf("score = %.0f, want %.0f (an allow must reward)", after, before+1)
	}
}

// TestDenyPenalizesTrustScore keeps the existing half pinned in the same place,
// so a future change to Decide cannot silently drop it.
func TestDenyPenalizesTrustScore(t *testing.T) {
	g := NewGovernor() // no rules: deny by default
	before := g.Trust.Score()

	if d := g.Decide("agent-1", "Execute", nil); d.Action != ActionDeny {
		t.Fatalf("action = %q, want deny", d.Action)
	}
	if after := g.Trust.Score(); after >= before {
		t.Fatalf("score = %.0f, want below %.0f (a deny must penalize)", after, before)
	}
}

// TestTrustFloorDowngradesAllow is the score being *read*: below the floor, a
// call that policy would allow is escalated for review instead. The floor is
// opt-in (zero disables it) because a governor that starts reviewing everything
// on day one is a governor nobody runs.
func TestTrustFloorDowngradesAllow(t *testing.T) {
	g := trustGovernor()
	g.TrustFloor = 600 // above the 500 start

	d := g.Decide("agent-1", "ReadFile", nil)
	if d.Action != ActionReview {
		t.Fatalf("action = %q, want review below the trust floor", d.Action)
	}
	if !strings.Contains(d.Reason, "trust") {
		t.Fatalf("reason = %q, must explain the trust floor", d.Reason)
	}
}

func TestTrustFloorZeroIsDisabled(t *testing.T) {
	g := trustGovernor()
	g.TrustFloor = 0

	if d := g.Decide("agent-1", "ReadFile", nil); d.Action != ActionAllow {
		t.Fatalf("action = %q, want allow with no floor set", d.Action)
	}
}

// TestTrustFloorNeverDowngradesADeny: the floor escalates allows, it does not
// launder a deny into a review.
func TestTrustFloorNeverDowngradesADeny(t *testing.T) {
	g := NewGovernor()
	g.TrustFloor = 600

	if d := g.Decide("agent-1", "Execute", nil); d.Action != ActionDeny {
		t.Fatalf("action = %q, want deny", d.Action)
	}
}

// TestKillSwitchDenyNamesTheScope: the operator has to be able to tell "this
// agent was stopped" from "everything was stopped".
func TestKillSwitchDenyNamesTheScope(t *testing.T) {
	g := trustGovernor()
	k := NewKillSwitch()
	k.Trip("agent-7")
	g.Kill = k

	d := g.Decide("agent-7", "ReadFile", nil)
	if d.Action != ActionDeny {
		t.Fatalf("action = %q, want deny", d.Action)
	}
	if !strings.Contains(d.Reason, "agent-7") {
		t.Fatalf("reason = %q, must name the tripped scope", d.Reason)
	}

	k.Trip("global")
	d = g.Decide("agent-8", "ReadFile", nil)
	if !strings.Contains(d.Reason, "global") {
		t.Fatalf("reason = %q, must name the global scope", d.Reason)
	}
}

// TestFileKillSwitchGovernsDecisions is the wiring end to end: a trip written by
// one process stops decisions in the next one.
func TestFileKillSwitchGovernsDecisions(t *testing.T) {
	path := t.TempDir() + "/kill.json"

	writer, err := OpenFileKillSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Trip("agent-7"); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenFileKillSwitch(path)
	if err != nil {
		t.Fatal(err)
	}
	g := trustGovernor()
	g.Kill = reader // the read side; the embedded *KillSwitch satisfies KillReader

	if d := g.Decide("agent-7", "ReadFile", nil); d.Action != ActionDeny {
		t.Fatalf("action = %q, want deny from a persisted trip", d.Action)
	}
	if d := g.Decide("agent-8", "ReadFile", nil); d.Action != ActionAllow {
		t.Fatalf("action = %q, want allow for an unrelated agent", d.Action)
	}
}
