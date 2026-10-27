package platform

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingContentStore deletes nothing and reports an error, standing in for a
// store whose backend is unavailable.
type failingContentStore struct{}

func (failingContentStore) Put(string, []byte) error { return nil }
func (failingContentStore) Get(string) ([]byte, error) {
	return nil, ErrContentNotFound
}
func (failingContentStore) Delete(string) error { return errors.New("store unavailable") }

func newTestEraser(t *testing.T) (*Eraser, *MemoryKeyRing, *MemoryContentStore, *Committer) {
	t.Helper()
	ring := NewMemoryKeyRing()
	content := NewMemoryContentStore()
	c, err := NewCommitter(ring)
	if err != nil {
		t.Fatalf("NewCommitter: %v", err)
	}
	e := &Eraser{Content: content, Ring: ring, Ledger: NewLedger()}
	return e, ring, content, c
}

func TestEraseDestroysContentAndKey(t *testing.T) {
	e, _, content, c := newTestEraser(t)
	commitment, _ := c.Commit("sub-a", "alice@example.com")
	if err := content.Put("sub-a", []byte("alice@example.com")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := e.Erase("sub-a", "gdpr-request"); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	if _, err := content.Get("sub-a"); !errors.Is(err, ErrContentNotFound) {
		t.Fatalf("content survived the erasure: %v", err)
	}
	if ok, err := c.Match("sub-a", "alice@example.com", commitment); ok || !errors.Is(err, ErrKeyDestroyed) {
		t.Fatalf("Match after Erase = (%v, %v), want (false, ErrKeyDestroyed)", ok, err)
	}
}

func TestEraseRecordsAuditableEntry(t *testing.T) {
	e, _, content, _ := newTestEraser(t)
	_ = content.Put("sub-a", []byte("alice@example.com"))
	if err := e.Erase("sub-a", "gdpr-request"); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	entries := e.Ledger.Entries()
	if len(entries) != 1 {
		t.Fatalf("ledger len = %d, want 1 erase entry", len(entries))
	}
	got := entries[0]
	if got.Kind != "erase" {
		t.Fatalf("Kind = %q, want erase", got.Kind)
	}
	if got.SubjectRef != "sub-a" {
		t.Fatalf("SubjectRef = %q, want the opaque handle", got.SubjectRef)
	}
	if got.Reason != "gdpr-request" {
		t.Fatalf("Reason = %q, want the reason code", got.Reason)
	}
	if got.SubjectHash != "" || got.InputHash != "" {
		t.Fatal("erase entry carries commitments to erased data")
	}
	if got.Tool != "" {
		t.Fatal("erase entry carries action data it should not have")
	}
	if err := e.Ledger.Verify(); err != nil {
		t.Fatalf("chain failed verification after the erase entry: %v", err)
	}
}

func TestEraseEntryHoldsNoPersonalData(t *testing.T) {
	dir := t.TempDir()
	ring, err := OpenFileKeyRing(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatalf("key ring: %v", err)
	}
	c, _ := NewCommitter(ring)
	content := NewMemoryContentStore()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	l, err := NewLedgerAt(ledgerPath)
	if err != nil {
		t.Fatalf("NewLedgerAt: %v", err)
	}
	defer func() { _ = l.Close() }()

	subject := "alice@example.com"
	_ = content.Put("ref-7f3a", []byte(subject))
	commitment, _ := c.Commit("ref-7f3a", subject)
	_, _ = l.Append(Entry{Kind: "decision", SubjectRef: "ref-7f3a", SubjectHash: commitment, CommitAlgo: CommitAlgoHMACSHA256})

	e := &Eraser{Content: content, Ring: ring, Ledger: l}
	if err := e.Erase("ref-7f3a", "gdpr-request"); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if strings.Contains(string(raw), subject) {
		t.Fatal("the ledger contains the erased personal data")
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("chain failed verification: %v", err)
	}
}

func TestEraseIsIdempotent(t *testing.T) {
	e, _, content, _ := newTestEraser(t)
	_ = content.Put("sub-a", []byte("x"))
	if err := e.Erase("sub-a", "gdpr-request"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if err := e.Erase("sub-a", "gdpr-request"); err != nil {
		t.Fatalf("second Erase: %v", err)
	}
	if got := len(e.Ledger.Entries()); got != 1 {
		t.Fatalf("ledger len = %d, want 1 entry after two identical erasures", got)
	}
}

func TestEraseOfUnknownRefRecordsNothing(t *testing.T) {
	e, _, _, _ := newTestEraser(t)
	if err := e.Erase("never-seen", "gdpr-request"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if got := len(e.Ledger.Entries()); got != 0 {
		t.Fatalf("ledger len = %d, want 0: erasing a subject that was never recorded is not an event", got)
	}
}

func TestEraseKeepsKeyWhenContentDeleteFails(t *testing.T) {
	ring := NewMemoryKeyRing()
	c, _ := NewCommitter(ring)
	_, _ = c.Commit("sub-a", "x")
	e := &Eraser{Content: failingContentStore{}, Ring: ring, Ledger: NewLedger()}

	err := e.Erase("sub-a", "gdpr-request")
	if err == nil {
		t.Fatal("Erase reported success while the content store failed")
	}
	if !ring.Has("sub-a") {
		t.Fatal("key was shredded before the content was gone; the remaining content can no longer be attributed or found")
	}
	if got := len(e.Ledger.Entries()); got != 0 {
		t.Fatalf("ledger len = %d, want 0: a failed erasure must not be recorded as done", got)
	}
	if _, matchErr := c.Commit("sub-a", "x"); matchErr != nil {
		t.Fatalf("commitment broken by a failed erasure: %v", matchErr)
	}
}

func TestEraseRecordsWhenTheKeyIsAlreadyGone(t *testing.T) {
	ring := NewMemoryKeyRing()
	c, _ := NewCommitter(ring)
	_, _ = c.Commit("sub-a", "x")
	_ = ring.Destroy("sub-a") // shredded out of band, never recorded

	e := &Eraser{Content: NewMemoryContentStore(), Ring: ring, Ledger: NewLedger()}
	if err := e.Erase("sub-a", "gdpr-request"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	entries := e.Ledger.Entries()
	if len(entries) != 1 || entries[0].Kind != "erase" {
		t.Fatal("the erasure went unrecorded because the key was already gone; the record must be recoverable")
	}
}

func TestEraseRequiresARing(t *testing.T) {
	if err := (&Eraser{Ledger: NewLedger()}).Erase("sub-a", "x"); err == nil {
		t.Fatal("Eraser without a key ring reported success")
	}
}

func TestEraseSubjectForgetsTheHandle(t *testing.T) {
	ring := NewMemoryKeyRing()
	content := NewMemoryContentStore()
	refs := NewMemoryRefRegistry()
	c, _ := NewCommitter(ring)

	subject := "alice@example.com"
	ref, _ := refs.Ref(subject)
	commitment, _ := c.Commit(ref, subject)
	_ = content.Put(ref, []byte(subject))

	e := &Eraser{Content: content, Ring: ring, Ledger: NewLedger(), Refs: refs}
	if err := e.EraseSubject(subject, "gdpr-request"); err != nil {
		t.Fatalf("EraseSubject: %v", err)
	}

	entries := e.Ledger.Entries()
	if len(entries) != 1 || entries[0].SubjectRef != ref {
		t.Fatalf("ledger entries = %+v, want one erase entry for the old handle", entries)
	}
	if _, err := content.Get(ref); !errors.Is(err, ErrContentNotFound) {
		t.Fatalf("content survived: %v", err)
	}
	if ok, err := c.Match(ref, subject, commitment); ok || !errors.Is(err, ErrKeyDestroyed) {
		t.Fatalf("Match = (%v, %v), want (false, ErrKeyDestroyed)", ok, err)
	}
	fresh, err := refs.Ref(subject)
	if err != nil {
		t.Fatalf("Ref after erase: %v", err)
	}
	if fresh == ref {
		t.Fatal("the erased subject still resolves to its old handle; a later record would relink to the erased history")
	}
}

func TestEraseSubjectRequiresARegistry(t *testing.T) {
	e := &Eraser{Ring: NewMemoryKeyRing(), Ledger: NewLedger()}
	if err := e.EraseSubject("alice@example.com", "gdpr-request"); err == nil {
		t.Fatal("EraseSubject without a ref registry reported success")
	}
}

func TestEraseOnlyNeedsARing(t *testing.T) {
	ring := NewMemoryKeyRing()
	content := NewMemoryContentStore()
	c, _ := NewCommitter(ring)
	_, _ = c.Commit("sub-a", "x")
	_ = content.Put("sub-a", []byte("x"))

	// No Committer: the eraser destroys content and keys, and never needs to
	// recompute a commitment, so requiring one would be a dependency it does
	// not use.
	e := &Eraser{Content: content, Ring: ring, Ledger: NewLedger()}
	if err := e.Erase("sub-a", "gdpr-request"); err != nil {
		t.Fatalf("Erase without a committer: %v", err)
	}
	if got := len(e.Ledger.Entries()); got != 1 {
		t.Fatalf("ledger len = %d, want 1", got)
	}
}
