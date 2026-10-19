package platform

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLedgerAppendChainsHashes(t *testing.T) {
	fixed := time.Unix(1700000000, 0)
	l := NewLedger()
	l.Append(Entry{Kind: "decision", Subject: "a", Detail: "allow", Time: fixed})
	l.Append(Entry{Kind: "decision", Subject: "b", Detail: "deny", Time: fixed})

	other := NewLedger()
	other.Append(Entry{Kind: "decision", Subject: "a", Detail: "different", Time: fixed})
	other.Append(Entry{Kind: "decision", Subject: "b", Detail: "deny", Time: fixed})

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
	l.Append(Entry{Kind: "decision", Detail: "allow"})
	l.Append(Entry{Kind: "decision", Detail: "deny"})
	if err := l.Verify(); err != nil {
		t.Fatalf("clean ledger failed verification: %v", err)
	}
	l.entries[0].Detail = "tampered"
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
	l.Append(Entry{Kind: "decision", Subject: "a", Detail: "allow", KeyID: "k1"})
	l.Append(Entry{Kind: "decision", Subject: "b", Detail: "deny", KeyID: "k1"})

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
