package platform

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestCommitter(t *testing.T, kr KeyRing) *Committer {
	t.Helper()
	c, err := NewCommitter(kr)
	if err != nil {
		t.Fatalf("NewCommitter: %v", err)
	}
	return c
}

func TestCommitIsStableForRefAndValue(t *testing.T) {
	c := newTestCommitter(t, NewMemoryKeyRing())
	first, err := c.Commit("sub-a", `{"path":"/tmp/x"}`)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	second, err := c.Commit("sub-a", `{"path":"/tmp/x"}`)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if first != second {
		t.Fatal("the same subject and value produced different commitments")
	}
	if first == "" {
		t.Fatal("empty commitment")
	}
}

func TestCommitDiffersPerSubjectForSameValue(t *testing.T) {
	c := newTestCommitter(t, NewMemoryKeyRing())
	a, _ := c.Commit("sub-a", "alice@example.com")
	b, _ := c.Commit("sub-b", "alice@example.com")
	if a == b {
		t.Fatal("two subjects committed the same value identically: the ledger is a re-identification oracle again")
	}
}

func TestCommitIsNotPlainSHA256(t *testing.T) {
	c := newTestCommitter(t, NewMemoryKeyRing())
	value := "agent-1"
	got, _ := c.Commit("sub-a", value)
	sum := sha256.Sum256([]byte(value))
	if got == hex.EncodeToString(sum[:]) {
		t.Fatal("commitment is an unkeyed sha256 of the value; anyone with a candidate list can confirm a match")
	}
}

func TestCommitRejectsUnknownAlgo(t *testing.T) {
	c := newTestCommitter(t, NewMemoryKeyRing())
	c.Algo = "md5"
	if _, err := c.Commit("sub-a", "x"); err == nil {
		t.Fatal("unsupported commitment algorithm was accepted")
	}
}

func TestMatchAcceptsItsOwnValue(t *testing.T) {
	c := newTestCommitter(t, NewMemoryKeyRing())
	commitment, _ := c.Commit("sub-a", "alice@example.com")

	ok, err := c.Match("sub-a", "alice@example.com", commitment)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !ok {
		t.Fatal("Match rejected the value it committed to")
	}
}

func TestMatchRejectsAnotherValue(t *testing.T) {
	c := newTestCommitter(t, NewMemoryKeyRing())
	commitment, _ := c.Commit("sub-a", "alice@example.com")

	ok, err := c.Match("sub-a", "bob@example.com", commitment)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if ok {
		t.Fatal("Match accepted a value that was never committed")
	}
}

func TestMatchFailsClosedAfterDestroy(t *testing.T) {
	kr := NewMemoryKeyRing()
	c := newTestCommitter(t, kr)
	commitment, _ := c.Commit("sub-a", "alice@example.com")
	if err := kr.Destroy("sub-a"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	ok, err := c.Match("sub-a", "alice@example.com", commitment)
	if ok {
		t.Fatal("Match confirmed an erased subject; erasure did nothing")
	}
	if !errors.Is(err, ErrKeyDestroyed) {
		t.Fatalf("Match after erasure = %v, want ErrKeyDestroyed", err)
	}
}

func TestCommitAfterDestroyFailsClosed(t *testing.T) {
	kr := NewMemoryKeyRing()
	c := newTestCommitter(t, kr)
	_, _ = c.Commit("sub-a", "x")
	_ = kr.Destroy("sub-a")

	if _, err := c.Commit("sub-a", "y"); !errors.Is(err, ErrKeyDestroyed) {
		t.Fatalf("Commit for an erased subject = %v, want ErrKeyDestroyed (a new key would silently unlink the old records)", err)
	}
}

func TestChainVerifiesAfterShred(t *testing.T) {
	dir := t.TempDir()
	kr, err := OpenFileKeyRing(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatalf("key ring: %v", err)
	}
	c := newTestCommitter(t, kr)
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	l, err := NewLedgerAt(ledgerPath)
	if err != nil {
		t.Fatalf("NewLedgerAt: %v", err)
	}
	defer func() { _ = l.Close() }()

	subject, _ := c.Commit("sub-a", "alice@example.com")
	input, _ := c.Commit("sub-a", `{"path":"/tmp/x"}`)
	if _, err := l.Append(Entry{Kind: "decision", SubjectRef: "sub-a", SubjectHash: subject, InputHash: input, Tool: "FileWrite"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := kr.Destroy("sub-a"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if err := l.Verify(); err != nil {
		t.Fatalf("chain failed verification after a crypto-shred: %v", err)
	}
	if ok, _ := c.Match("sub-a", "alice@example.com", subject); ok {
		t.Fatal("the record is still linked to the erased subject")
	}
	entries := l.Entries()
	if entries[0].SubjectRef != "sub-a" {
		t.Fatalf("SubjectRef = %q, want the opaque handle to survive erasure", entries[0].SubjectRef)
	}
	if entries[0].Kind != "decision" || entries[0].Tool != "FileWrite" {
		t.Fatal("structural skeleton did not survive erasure")
	}
}

func TestLedgerFileHoldsNoKeyMaterial(t *testing.T) {
	dir := t.TempDir()
	kr, _ := OpenFileKeyRing(filepath.Join(dir, "keys.json"))
	c := newTestCommitter(t, kr)
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	l, err := NewLedgerAt(ledgerPath)
	if err != nil {
		t.Fatalf("NewLedgerAt: %v", err)
	}
	defer func() { _ = l.Close() }()

	key, _ := kr.Key("sub-a")
	subject, _ := c.Commit("sub-a", "alice@example.com")
	if _, err := l.Append(Entry{Kind: "decision", SubjectRef: "sub-a", SubjectHash: subject}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if bytes.Contains(raw, key) || bytes.Contains(bytes.ToLower(raw), []byte(hex.EncodeToString(key))) {
		t.Fatal("the ledger file contains commitment key material; erasing the key ring would not erase the content link")
	}
}

func TestEntryIncludesCommitmentFieldsInChainHash(t *testing.T) {
	fixed := time.Unix(1700000000, 0)
	plain := NewLedger()
	_, _ = plain.Append(Entry{Kind: "decision", SubjectHash: "same", Time: fixed})
	committed := NewLedger()
	_, _ = committed.Append(Entry{Kind: "decision", SubjectHash: "same", Time: fixed, CommitAlgo: "hmac-sha256"})

	if plain.Entries()[0].Hash == committed.Entries()[0].Hash {
		t.Fatal("chain hash ignores CommitAlgo; a commitment record can be rewritten into a plain-hash record undetected")
	}
}
