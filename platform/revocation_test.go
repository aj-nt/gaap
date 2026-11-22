package platform

import "testing"

func TestRevocationIsDerivedFromTheSequence(t *testing.T) {
	l := NewLedger()
	if err := l.RecordRevocation("artifact-ref-1", RevocationRequested, ""); err != nil {
		t.Fatalf("request: %v", err)
	}
	if got := RevocationOf(l.Entries(), "artifact-ref-1"); got != RevocationRequested {
		t.Fatalf("stage = %q, want requested", got)
	}
	if ContainmentClosed(l.Entries(), "artifact-ref-1") {
		t.Fatal("a request is not a closed containment")
	}
	if err := l.RecordRevocation("artifact-ref-1", RevocationPropagated, "pushed to idp"); err != nil {
		t.Fatalf("propagate: %v", err)
	}
	if ContainmentClosed(l.Entries(), "artifact-ref-1") {
		t.Fatal("propagation is not verification")
	}
	if err := l.RecordRevocation("artifact-ref-1", RevocationVerified, "idp-audit-9931"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ContainmentClosed(l.Entries(), "artifact-ref-1") {
		t.Fatal("verified revocation did not close containment")
	}
}

// Verification without evidence is the claim this API exists to refuse: "we
// checked" with nothing to check is the same sentence as "we believe".
func TestVerifiedRequiresEvidence(t *testing.T) {
	l := NewLedger()
	if err := l.RecordRevocation("ref", RevocationVerified, ""); err == nil {
		t.Fatal("verified revocation with no evidence was accepted")
	}
}

func TestUnknownStageRefused(t *testing.T) {
	l := NewLedger()
	if err := l.RecordRevocation("ref", RevocationStage("done"), "x"); err == nil {
		t.Fatal("unknown stage accepted")
	}
	if err := l.RecordRevocation("", RevocationRequested, ""); err == nil {
		t.Fatal("revocation with no artifact reference accepted")
	}
}

// The original entry is never touched: entries are immutable and hashed, so a
// later stage can only ever be a new row.
func TestRevocationDoesNotMutateOriginalEntry(t *testing.T) {
	l := NewLedger()
	if err := l.RecordRevocation("ref-1", RevocationRequested, ""); err != nil {
		t.Fatal(err)
	}
	before := l.Entries()[0]
	if err := l.RecordRevocation("ref-1", RevocationVerified, "audit-1"); err != nil {
		t.Fatal(err)
	}
	after := l.Entries()[0]
	if before.Hash != after.Hash {
		t.Fatalf("the first entry's hash changed: %s -> %s", before.Hash, after.Hash)
	}
	if after.RevocationStage != string(RevocationRequested) {
		t.Fatalf("the original entry's stage was mutated to %q", after.RevocationStage)
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("chain no longer verifies: %v", err)
	}
}

// Stages for different artifacts must not bleed into each other.
func TestRevocationIsPerArtifact(t *testing.T) {
	l := NewLedger()
	_ = l.RecordRevocation("a", RevocationVerified, "audit-a")
	_ = l.RecordRevocation("b", RevocationRequested, "")
	if !ContainmentClosed(l.Entries(), "a") {
		t.Fatal("a should be closed")
	}
	if ContainmentClosed(l.Entries(), "b") {
		t.Fatal("b inherited a's verification")
	}
}

// Order in the log, not order of arrival: the furthest stage wins regardless.
func TestFurthestStageWins(t *testing.T) {
	l := NewLedger()
	_ = l.RecordRevocation("ref", RevocationVerified, "audit")
	_ = l.RecordRevocation("ref", RevocationRequested, "")
	if got := RevocationOf(l.Entries(), "ref"); got != RevocationVerified {
		t.Fatalf("stage = %q, want verified: a later request must not un-verify", got)
	}
}
