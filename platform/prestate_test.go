package platform

import (
	"errors"
	"strings"
	"testing"
)

const deletePolicy = `{
  "version": 1,
  "rules": [
    {"id": "rm-account", "tool": "DeleteAccount", "action": "allow",
     "reversibility": "irreversible", "artifact": {"type": "delete"}},
    {"id": "disable-account", "tool": "DisableAccount", "action": "allow",
     "artifact": {"type": "disable"}}
  ]
}`

// A deletion nobody can describe is not recoverable, so it must not be recorded
// as though it were. Deny is the only honest verdict.
func TestDeleteWithoutPreStateDenied(t *testing.T) {
	g := governorFromPolicy(t, deletePolicy)
	d := g.Decide("agent-1", "DeleteAccount", map[string]any{"id": "u1"})
	if d.Action != ActionDeny {
		t.Fatalf("action = %q, want deny", d.Action)
	}
	if !strings.Contains(d.Reason, "pre-state") {
		t.Fatalf("reason = %q, want it to name the pre-state requirement", d.Reason)
	}
	if d.RuleID != "rm-account" {
		t.Fatalf("rule id = %q, want it preserved through the refusal", d.RuleID)
	}
	// The refusal is still a record.
	if len(g.Ledger.Entries()) != 1 {
		t.Fatalf("entries = %d, want 1", len(g.Ledger.Entries()))
	}
}

func TestDeleteWithPreStateAllowed(t *testing.T) {
	g := governorFromPolicy(t, deletePolicy)
	g.PreState = func(tool string, args map[string]any) (string, error) {
		return "commitment-abc123", nil
	}
	d := g.Decide("agent-1", "DeleteAccount", map[string]any{"id": "u1"})
	if d.Action != ActionAllow {
		t.Fatalf("action = %q, want allow", d.Action)
	}
	e := g.Ledger.Entries()[0]
	if e.ArtifactType != "delete" {
		t.Fatalf("artifact type = %q, want delete", e.ArtifactType)
	}
}

func TestCaptureFailureDenies(t *testing.T) {
	g := governorFromPolicy(t, deletePolicy)
	g.PreState = func(string, map[string]any) (string, error) {
		return "", errors.New("disk unreadable")
	}
	d := g.Decide("agent-1", "DeleteAccount", nil)
	if d.Action != ActionDeny {
		t.Fatalf("action = %q, want deny", d.Action)
	}
	if !strings.Contains(d.Reason, "disk unreadable") {
		t.Fatalf("reason = %q, want it to carry the capture error", d.Reason)
	}
}

// An empty commitment is not a capture: it would record "we have the prior state"
// with nothing behind it.
func TestEmptyCaptureDenies(t *testing.T) {
	g := governorFromPolicy(t, deletePolicy)
	g.PreState = func(string, map[string]any) (string, error) { return "", nil }
	if d := g.Decide("agent-1", "DeleteAccount", nil); d.Action != ActionDeny {
		t.Fatalf("action = %q, want deny", d.Action)
	}
}

// Disable is not held to the same bar, or the control becomes a formality
// everybody works around.
func TestDisableNeedsNoPreState(t *testing.T) {
	g := governorFromPolicy(t, deletePolicy)
	d := g.Decide("agent-1", "DisableAccount", nil)
	if d.Action != ActionAllow {
		t.Fatalf("action = %q, want allow", d.Action)
	}
	// The entry no longer carries a pre-state field: it was written and never read,
	// so the capture is returned on the decision and the ledger stops pretending to
	// keep it. What this test still holds is the bar itself — a disable is allowed
	// with no capturer configured.
	if d.PreState != "" {
		t.Fatal("a disable took a pre-state it never needed")
	}
}
