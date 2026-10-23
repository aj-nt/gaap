package platform

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLedgerAppendChainsHashes(t *testing.T) {
	fixed := time.Unix(1700000000, 0)
	l := NewLedger()
	l.Append(Entry{Kind: "decision", SubjectHash: "a", Reason: "allow", Time: fixed})
	l.Append(Entry{Kind: "decision", SubjectHash: "b", Reason: "deny", Time: fixed})

	other := NewLedger()
	other.Append(Entry{Kind: "decision", SubjectHash: "a", Reason: "different", Time: fixed})
	other.Append(Entry{Kind: "decision", SubjectHash: "b", Reason: "deny", Time: fixed})

	a, b := l.Entries(), other.Entries()
	if a[0].Hash == "" || a[1].Hash == "" {
		t.Fatal("entries missing chain hashes")
	}
	if a[1].Hash == b[1].Hash {
		t.Fatal("second entry hash does not depend on the previous entry")
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("clean chain failed verification: %v", err)
	}
}

func TestLedgerVerifyDetectsTamper(t *testing.T) {
	l := NewLedger()
	l.Append(Entry{Kind: "decision", Reason: "allow"})
	l.Append(Entry{Kind: "decision", Reason: "deny"})
	if err := l.Verify(); err != nil {
		t.Fatalf("clean ledger failed verification: %v", err)
	}
	l.entries[0].Reason = "tampered"
	if err := l.Verify(); err == nil {
		t.Fatal("tampered ledger verified clean")
	}
}

func TestLedgerStampsKeyIDAndAlgo(t *testing.T) {
	l := NewLedger()
	l.Append(Entry{Kind: "decision", KeyID: "k1"})
	e := l.Entries()[0]
	if e.KeyID != "k1" {
		t.Fatalf("KeyID = %q, want k1", e.KeyID)
	}
	if e.Algo != "sha256" {
		t.Fatalf("Algo = %q, want sha256", e.Algo)
	}
}

func TestLedgerSaveLoadRoundTrip(t *testing.T) {
	l := NewLedger()
	l.Append(Entry{Kind: "decision", SubjectHash: "a", Reason: "allow", KeyID: "k1"})
	l.Append(Entry{Kind: "decision", SubjectHash: "b", Reason: "deny", KeyID: "k1"})

	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := l.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadLedger(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want, gotEntries := l.Entries(), got.Entries()
	if len(gotEntries) != len(want) {
		t.Fatalf("loaded %d entries, want %d", len(gotEntries), len(want))
	}
	for i := range want {
		if gotEntries[i].Hash != want[i].Hash {
			t.Fatalf("entry %d hash changed across save/load", i)
		}
	}
	if err := got.Verify(); err != nil {
		t.Fatalf("loaded ledger failed verification: %v", err)
	}
}

func TestLedgerAppendRejectsUnsupportedAlgorithm(t *testing.T) {
	l := NewLedger()
	_, err := l.Append(Entry{Kind: "decision", Reason: "x", Algo: "sha512"})
	if err == nil {
		t.Fatal("Append accepted an unsupported hash algorithm")
	}
	if len(l.Entries()) != 0 {
		t.Fatalf("ledger has %d entries after rejected append, want 0", len(l.Entries()))
	}
}

func TestLedgerVerifyRejectsUnsupportedAlgorithm(t *testing.T) {
	// Build a ledger whose stored entry claims an algorithm this build cannot
	// hash, bypassing Append (which now rejects it) to simulate a ledger written
	// by a future version.
	l := &Ledger{entries: []Entry{{Seq: 1, Kind: "decision", Algo: "sha512", Hash: "deadbeef"}}}
	err := l.Verify()
	if err == nil {
		t.Fatal("Verify accepted an unsupported hash algorithm")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Verify error = %q, want it to name the unsupported algorithm", err)
	}
}
