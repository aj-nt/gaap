package platform

import "testing"

func TestLedgerAppendsInOrder(t *testing.T) {
	l := NewLedger()
	l.Append(Entry{Kind: "decision", Subject: "a", Detail: "allow"})
	l.Append(Entry{Kind: "decision", Subject: "b", Detail: "deny"})

	entries := l.Entries()
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Seq != 1 || entries[1].Seq != 2 {
		t.Fatalf("seqs = %d,%d, want 1,2", entries[0].Seq, entries[1].Seq)
	}
	if entries[1].Subject != "b" {
		t.Fatalf("second entry subject = %q, want b", entries[1].Subject)
	}
}

func TestLedgerSnapshotIsACopy(t *testing.T) {
	l := NewLedger()
	l.Append(Entry{Kind: "decision"})
	snap := l.Entries()
	snap[0].Detail = "mutated"
	if got := l.Entries()[0].Detail; got != "" {
		t.Fatalf("mutating the snapshot leaked into the ledger: %q", got)
	}
}
