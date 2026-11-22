package platform

import "fmt"

// RevocationStage is how far a revocation has got. The stages are ordered and the
// order matters: only the last one may close a containment entry.
//
// It is a stage, not a status, because "we asked" and "it is gone" are different
// claims and an operator deciding whether to sleep needs to tell them apart.
type RevocationStage string

const (
	// RevocationRequested: the request was recorded. Nothing has changed yet.
	RevocationRequested RevocationStage = "requested"
	// RevocationPropagated: it was pushed to wherever the artifact lives. Still a
	// claim about our own action, not about the world.
	RevocationPropagated RevocationStage = "propagated"
	// RevocationVerified: someone checked, and can produce the evidence.
	RevocationVerified RevocationStage = "verified"
)

func (s RevocationStage) rank() int {
	switch s {
	case RevocationRequested:
		return 1
	case RevocationPropagated:
		return 2
	case RevocationVerified:
		return 3
	}
	return 0
}

// Known reports whether s is a declared stage.
func (s RevocationStage) Known() bool { return s.rank() > 0 }

// RevocationOf derives how far the revocation of ref has got, from the ledger, in
// sequence order.
//
// Derived, never stored on the original entry. Entries are append-only and
// hashed; a revocation field mutated on the artifact's own row would either break
// the chain or live outside it, and either way the record would disagree with
// itself.
func RevocationOf(entries []Entry, ref string) RevocationStage {
	var furthest RevocationStage
	for _, e := range entries {
		if e.Kind != KindRevocation || e.ArtifactRef != ref {
			continue
		}
		s := RevocationStage(e.RevocationStage)
		if s.rank() > furthest.rank() {
			furthest = s
		}
	}
	return furthest
}

// ContainmentClosed reports whether the revocation of ref has been verified.
//
// The limit is deliberate and belongs in the code rather than a document: gaap
// holds no cloud credential and cannot observe a token. A "closed" containment
// therefore means verified-by-evidence-supplied-to-us — it does not mean an
// access token issued before the deletion has stopped working. In Entra ID that
// token generally stays valid until it expires, typically 60 to 90 minutes,
// unless the resource and client support continuous access evaluation.
func ContainmentClosed(entries []Entry, ref string) bool {
	return RevocationOf(entries, ref) == RevocationVerified
}

// RecordRevocation appends a revocation stage for an artifact reference.
//
// Two refusals: an unknown stage (a typo would sit in the ledger looking like
// progress), and a verification with no evidence. The second is the whole
// difference between "we believe it is gone" and "we checked".
func (l *Ledger) RecordRevocation(ref string, stage RevocationStage, evidence string) error {
	if ref == "" {
		return fmt.Errorf("revocation needs an artifact reference")
	}
	if !stage.Known() {
		return fmt.Errorf("unknown revocation stage %q (want requested, propagated, or verified)", stage)
	}
	if stage == RevocationVerified && evidence == "" {
		return fmt.Errorf("revocation of %q cannot be verified without an evidence reference", ref)
	}
	_, err := l.Append(Entry{
		Kind:            KindRevocation,
		ArtifactRef:     ref,
		RevocationStage: string(stage),
		// The evidence reference is the reason this row exists; storing it in
		// Reason keeps the entry type-count down and reads correctly in `ledger
		// tail`, where the column is already labelled REASON.
		Reason: evidence,
	})
	return err
}
