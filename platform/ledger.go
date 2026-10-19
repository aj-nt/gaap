package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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
	// KeyID identifies the signing key that authenticated this entry. Empty
	// until signing is wired in; the field exists so entries are self-describing
	// across key rotation.
	KeyID string
	// Algo names the hash algorithm used for Hash. Only "sha256" is
	// implemented; Append stamps it so old entries stay verifiable under the
	// algorithm they were written with.
	Algo string
	// Hash is the chain hash of this entry: a hash over the previous entry's
	// hash and this entry's own fields. This is what makes tampering detectable.
	Hash string
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

// hashEntry computes the chain hash for an entry: sha256 over a canonical,
// deterministic serialization of the previous entry's hash and this entry's
// own fields. Append and Verify both use it, so a mismatch means tampering.
func hashEntry(e Entry, prevHash string) (string, error) {
	if e.Algo != "" && e.Algo != "sha256" {
		return "", fmt.Errorf("unsupported hash algorithm %q (only sha256 implemented)", e.Algo)
	}
	content := struct {
		PrevHash      string `json:"prev_hash"`
		KeyID         string `json:"key_id"`
		Algo          string `json:"algo"`
		Seq           int64  `json:"seq"`
		TimeUnixNano  int64  `json:"time_unix_nano"`
		Kind          string `json:"kind"`
		Subject       string `json:"subject"`
		Detail        string `json:"detail"`
		Reversibility string `json:"reversibility"`
	}{
		PrevHash:      prevHash,
		KeyID:         e.KeyID,
		Algo:          e.Algo,
		Seq:           e.Seq,
		TimeUnixNano:  e.Time.UnixNano(),
		Kind:          e.Kind,
		Subject:       e.Subject,
		Detail:        e.Detail,
		Reversibility: string(e.Reversibility),
	}
	b, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Append adds an entry and returns its 1-based sequence number.
func (l *Ledger) Append(e Entry) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq = int64(len(l.entries)) + 1
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Algo == "" {
		e.Algo = "sha256"
	}
	prevHash := ""
	if len(l.entries) > 0 {
		prevHash = l.entries[len(l.entries)-1].Hash
	}
	h, err := hashEntry(e, prevHash)
	if err != nil {
		return 0, err
	}
	e.Hash = h
	l.entries = append(l.entries, e)
	return e.Seq, nil
}

// Entries returns a snapshot (a copy) of all entries in order.
func (l *Ledger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

// Verify recomputes every entry's chain hash and confirms the recorded hashes
// match. It returns an error on the first mismatch, which means an entry was
// tampered with or carries an algorithm this build cannot verify.
func (l *Ledger) Verify() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	prevHash := ""
	for _, e := range l.entries {
		h, err := hashEntry(e, prevHash)
		if err != nil {
			return fmt.Errorf("entry %d: %w", e.Seq, err)
		}
		if e.Hash != h {
			return fmt.Errorf("entry %d: hash mismatch (tampered)", e.Seq)
		}
		prevHash = e.Hash
	}
	return nil
}

// Save writes every entry to path as JSON. It does not verify the chain
// first; call Verify to check integrity before relying on a saved ledger.
func (l *Ledger) Save(path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := json.MarshalIndent(l.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}

// LoadLedger reads a ledger previously written by Save. It reconstructs the
// entries in order but does not verify the chain; call Verify after loading.
func LoadLedger(path string) (*Ledger, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, err
	}
	return &Ledger{entries: entries}, nil
}
