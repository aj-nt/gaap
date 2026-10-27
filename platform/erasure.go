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
	// Ledger records the erasure. Optional, but an unrecorded erasure is an
	// erasure nobody can prove.
	Ledger *Ledger
	// Refs is the subject-to-handle registry. Required by EraseSubject, and
	// ignored by Erase, which is already given a handle.
	Refs RefRegistry
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

	hadKey := e.Ring.Has(ref)
	shredded := e.Ring.Destroyed(ref)
	hadContent := e.Content != nil && e.Content.Has(ref)
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

// EraseSubject erases a subject by identity: resolve the handle, erase the
// records behind it, then forget the mapping.
//
// The order matters in the same way as Erase. The mapping is forgotten last, so
// a failure part-way leaves a handle the next attempt can still resolve. Forget
// is what makes the subject resolve to a fresh handle afterwards, which is what
// keeps a post-erasure decision from relinking to the erased history.
func (e *Eraser) EraseSubject(subject, reason string) error {
	if e.Refs == nil {
		return errors.New("EraseSubject requires a ref registry; Erase takes a handle directly")
	}
	ref, err := e.Refs.Ref(subject)
	if err != nil {
		return fmt.Errorf("resolve subject %q: %w", subject, err)
	}
	if err := e.Erase(ref, reason); err != nil {
		return err
	}
	if err := e.Refs.Forget(subject); err != nil {
		return fmt.Errorf("forget subject mapping: %w", err)
	}
	return nil
}
