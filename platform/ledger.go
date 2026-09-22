package platform

import (
	"sync"
	"time"
)

// Entry is a single immutable record in the ledger. Only the governor appends;
// the agent never writes here directly.
type Entry struct {
	Seq     int64
	Time    time.Time
	Kind    string // "decision", "grant", "killswitch", "trust", "request"
	Subject string // agent ID and/or tool name
	Detail  string
	// Reversibility classifies whether the recorded effect can be undone. Zero
	// value (empty string) means "not classified" — valid for non-decision
	// entries (grant, trust, request). Decision entries should always carry a
	// class (see classify).
	Reversibility Reversibility
}

// Ledger is an append-only record. There is deliberately no delete, update, or
// clear method — appending is the only mutation, which is what makes it a
// record the governed cannot rewrite.
type Ledger struct {
	mu      sync.Mutex
	entries []Entry
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{}
}

// Append adds an entry and returns its 1-based sequence number.
func (l *Ledger) Append(e Entry) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq = int64(len(l.entries)) + 1
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	l.entries = append(l.entries, e)
	return e.Seq
}

// Entries returns a snapshot (a copy) of all entries in order.
func (l *Ledger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}
