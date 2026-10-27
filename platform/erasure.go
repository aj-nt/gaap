package platform

import (
	"errors"
	"fmt"
)

// EraseKind is the ledger kind stamped on an erasure record.
const EraseKind = "erase"

// Eraser honors an erasure request: it destroys the content, shreds the key, and
// records that the erasure happened.
//
// The record is the point. A governor that deletes silently cannot prove it
// honored the request, and a governor that keeps the record cannot prove it
// deleted. The erase entry resolves that: it says an erasure occurred, for which
// opaque handle, at what time, under which reason code, and nothing else. The
// entry carries no commitment to the erased data, because a commitment is what
// the erasure was meant to destroy.
type Eraser struct {
	// Content holds what the subject's records are about. May be nil if this
	// governor never stored content.
	Content ContentStore
	// Ring holds the per-subject keys. Required.
	Ring KeyRing
	// Committer is the committer the ledger's commitments were made with.
	// Required, so an erasure is performed by the same keying authority that
	// made the commitments it is destroying.
	Committer *Committer
	// Ledger records the erasure. Optional, but an unrecorded erasure is an
	// erasure nobody can prove.
	Ledger *Ledger
}

// Erase deletes the subject's content, shreds their key, and appends an erase
// entry. It returns an error only if some part of that failed.
//
// Order is deliberate. The content goes first and the key second: if the store
// cannot delete, the key is kept, so nothing has been unlinked from data that
// still exists, and the request can be retried. Shredding first would leave
// orphaned content that no longer has an owner.
//
// Idempotent. A repeat erasure is a no-op, except in one case worth having: if
// the key was shredded out of band and never recorded, Erase writes the missing
// record rather than returning silently, so the audit trail can be repaired.
func (e *Eraser) Erase(ref, reason string) error {
	if e.Ring == nil {
		return errors.New("eraser requires a key ring")
	}
	if e.Committer == nil {
		return errors.New("eraser requires a committer")
	}

	hadKey := e.Ring.Has(ref)
	shredded := e.Ring.Destroyed(ref)
	hadContent, err := e.hasContent(ref)
	if err != nil {
		return err
	}
	if !hadKey && !shredded && !hadContent {
		return nil // never seen: nothing to erase and nothing to record
	}

	if hadKey || hadContent {
		if e.Content != nil {
			if err := e.Content.Delete(ref); err != nil {
				return fmt.Errorf("erase content for %s: %w", ref, err)
			}
		}
	}
	if hadKey {
		if err := e.Ring.Destroy(ref); err != nil {
			return fmt.Errorf("shred key for %s: %w", ref, err)
		}
	}

	// A key shredded out of band left the erasure unrecorded. Write the missing
	// record instead of returning silently: the audit trail is the deliverable.
	if e.recorded(ref) {
		return nil
	}
	if e.Ledger == nil {
		return nil
	}
	if _, err := e.Ledger.Append(Entry{Kind: EraseKind, SubjectRef: ref, Reason: reason}); err != nil {
		return fmt.Errorf("record erasure of %s: %w", ref, err)
	}
	return nil
}

// hasContent reports whether the content store holds anything for ref. A store
// error other than "not found" is propagated: an eraser that cannot see the
// content cannot claim to have erased it.
func (e *Eraser) hasContent(ref string) (bool, error) {
	if e.Content == nil {
		return false, nil
	}
	if _, err := e.Content.Get(ref); err != nil {
		if errors.Is(err, ErrContentNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("inspect content for %s: %w", ref, err)
	}
	return true, nil
}

// recorded reports whether the ledger already holds an erase entry for ref.
func (e *Eraser) recorded(ref string) bool {
	if e.Ledger == nil {
		return false
	}
	for _, entry := range e.Ledger.Entries() {
		if entry.Kind == EraseKind && entry.SubjectRef == ref {
			return true
		}
	}
	return false
}
