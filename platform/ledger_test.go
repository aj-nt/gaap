package platform

import "testing"

func TestLedgerAppendsInOrder(t *testing.T) {
	l := NewLedger()
	_, _ = l.Append(Entry{Kind: "decision", SubjectHash: "a", Reason: "allow"})
	_, _ = l.Append(Entry{Kind: "decision", SubjectHash: "b", Reason: "deny"})

	entries := l.Entries()
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Seq != 1 || entries[1].Seq != 2 {
		t.Fatalf("seqs = %d,%d, want 1,2", entries[0].Seq, entries[1].Seq)
	}
	if entries[1].SubjectHash != "b" {
		t.Fatalf("second entry subject hash = %q, want b", entries[1].SubjectHash)
	}
}

func TestLedgerSnapshotIsACopy(t *testing.T) {
	l := NewLedger()
	_, _ = l.Append(Entry{Kind: "decision"})
	snap := l.Entries()
	snap[0].Reason = "mutated"
	if got := l.Entries()[0].Reason; got != "" {
		t.Fatalf("mutating the snapshot leaked into the ledger: %q", got)
	}
}
