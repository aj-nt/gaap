package platform

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is a single immutable record in the ledger. Only the governor appends;
// the agent never writes here directly.
//
// The entry is a STRUCTURAL SKELETON, not a transcript. A decision references
// its subject, so a decision is personal data too: storing the identity or the
// content here would put personal data in a store that is designed to be
// impossible to erase. The ledger therefore holds commitments (hashes) and
// non-identifying structure (action type, rule, time, outcome). The content and
// the decision text live in an erasable store, keyed by these hashes.
//
// "At time T, under rule R, a deny was made on input H_c, producing outcome H_d."
type Entry struct {
	Seq  int64     `json:"seq"`
	Time time.Time `json:"time"`
	// Kind is the record category: "decision", "grant", "killswitch", "trust",
	// "request".
	Kind string `json:"kind"`
	// SubjectHash commits to the identity the decision was about, without
	// storing it.
	SubjectHash string `json:"subject_hash"`
	// Tool is the structural action type ("Execute", "FileWrite"). Not personal
	// data, so it is stored directly.
	Tool string `json:"tool"`
	// RuleID names the policy rule that produced the decision. Empty means no
	// rule matched (the deny-by-default fallback) or the decision did not come
	// from a rule (e.g. the kill switch).
	RuleID string `json:"rule_id,omitempty"`
	// Outcome is the decision's action, typed. Never parsed back out of text.
	Outcome Action `json:"outcome"`
	// InputHash commits to the arguments the decision was made on, without
	// storing them.
	InputHash string `json:"input_hash"`
	// Reason is the sovereign-authored reason code that accompanies a non-allow
	// outcome. It must not be interpolated with call arguments or subject data;
	// subject-referencing decision text belongs in the erasable store.
	Reason string `json:"reason,omitempty"`
	// Reversibility classifies whether the recorded effect can be undone.
	// Zero value means "not classified" — valid for non-decision entries.
	Reversibility Reversibility `json:"reversibility,omitempty"`
	// KeyID identifies the signing key that authenticated this entry. Empty
	// until signing is wired in; the field exists so entries are self-describing
	// across key rotation.
	KeyID string `json:"key_id,omitempty"`
	// Algo names the hash algorithm used for Hash. Only "sha256" is
	// implemented; Append stamps it so old entries stay verifiable under the
	// algorithm they were written with.
	Algo string `json:"algo"`
	// Hash is the chain hash of this entry: a hash over the previous entry's
	// hash and this entry's own fields. This is what makes tampering detectable.
	Hash string `json:"hash"`
}

// Sink receives every appended entry. Append is called synchronously and its
// error propagates to the caller: a decision that could not be recorded must
// never be reported as governed.
type Sink interface {
	Append(Entry) error
	Close() error
}

// Ledger is an append-only record. There is deliberately no delete, update, or
// clear method — appending is the only mutation, which is what makes it a
// record the governed cannot rewrite.
//
// An in-memory ledger is a working set, not a record. Attach a Sink (see
// NewLedgerAt) and the record survives the process.
type Ledger struct {
	mu      sync.Mutex
	entries []Entry
	sink    Sink
}

// NewLedger returns an empty, memory-only ledger.
func NewLedger() *Ledger {
	return &Ledger{}
}

// SetSink attaches a durable sink to an EMPTY ledger. Attaching one to a ledger
// that already holds entries is refused rather than backfilled: the usual way to
// get a populated in-memory ledger is loading it from a file, and backfilling
// would then append the same entries to that file a second time. Use
// NewLedgerAt to reopen an existing record.
func (l *Ledger) SetSink(s Sink) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sink != nil {
		return errors.New("ledger already has a sink")
	}
	if len(l.entries) > 0 {
		return fmt.Errorf("cannot attach a sink to a ledger holding %d entries; reopen it with NewLedgerAt", len(l.entries))
	}
	l.sink = s
	return nil
}

// Close closes the attached sink, if any. The ledger stays readable.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sink == nil {
		return nil
	}
	err := l.sink.Close()
	l.sink = nil
	return err
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
		SubjectHash   string `json:"subject_hash"`
		Tool          string `json:"tool"`
		RuleID        string `json:"rule_id"`
		Outcome       string `json:"outcome"`
		InputHash     string `json:"input_hash"`
		Reason        string `json:"reason"`
		Reversibility string `json:"reversibility"`
	}{
		PrevHash:      prevHash,
		KeyID:         e.KeyID,
		Algo:          e.Algo,
		Seq:           e.Seq,
		TimeUnixNano:  e.Time.UnixNano(),
		Kind:          e.Kind,
		SubjectHash:   e.SubjectHash,
		Tool:          e.Tool,
		RuleID:        e.RuleID,
		Outcome:       string(e.Outcome),
		InputHash:     e.InputHash,
		Reason:        e.Reason,
		Reversibility: string(e.Reversibility),
	}
	b, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Append adds an entry, writes it through to the sink, and returns its 1-based
// sequence number. If the sink fails, the append is rejected entirely: the entry
// never enters memory and its sequence number is not consumed, so memory and
// disk cannot diverge and a retry reuses the same slot.
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
	if l.sink != nil {
		if err := l.sink.Append(e); err != nil {
			return 0, fmt.Errorf("ledger sink: %w", err)
		}
	}
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

// Verify recomputes every entry's chain hash and confirms the sequence is
// contiguous from 1. It returns an error on the first defect, which means an
// entry was tampered with or entries were removed from the middle of the log.
//
// Note the limit this does NOT close: dropping entries from the TAIL leaves a
// shorter, internally consistent chain. Only an external anchor (a published
// head hash with independent custody) can detect that.
func (l *Ledger) Verify() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	prevHash := ""
	for i, e := range l.entries {
		want := int64(i) + 1
		if e.Seq != want {
			return fmt.Errorf("entry %d: sequence discontinuity (stored seq %d, want %d)", i+1, e.Seq, want)
		}
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

// Save rewrites the whole ledger to path as JSON Lines, atomically: it writes a
// temp file in the same directory, fsyncs it, renames it into place, then
// fsyncs the directory. A crash mid-write therefore leaves the previous ledger
// intact instead of a truncated one.
//
// For a running governor, prefer NewLedgerAt, which appends and fsyncs per
// entry rather than rewriting history.
func (l *Ledger) Save(path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ledger-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename

	w := bufio.NewWriter(tmp)
	for _, e := range l.entries {
		b, err := json.Marshal(e)
		if err != nil {
			_ = tmp.Close()
			return err
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so a rename inside it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// LoadLedger reads a ledger previously written by Save or by a FileSink. It
// reconstructs the entries in order but does not verify the chain; call Verify
// after loading (NewLedgerAt does).
func LoadLedger(path string) (*Ledger, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, fmt.Errorf("line %d: not a ledger entry (expected one JSON entry per line): %w", line, err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return &Ledger{entries: entries}, nil
}

// NewLedgerAt opens (or creates) a durable ledger at path. An existing ledger is
// loaded and its chain verified BEFORE it is reopened for appending, so a
// governor never appends onto a log it cannot vouch for.
func NewLedgerAt(path string) (*Ledger, error) {
	var entries []Entry
	if _, err := os.Stat(path); err == nil {
		loaded, err := LoadLedger(path)
		if err != nil {
			return nil, err
		}
		if err := loaded.Verify(); err != nil {
			return nil, fmt.Errorf("refusing to append to %s: %w", path, err)
		}
		entries = loaded.entries
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	s, err := OpenFileSink(path)
	if err != nil {
		return nil, err
	}
	// Attach the sink directly. The loaded entries are already in the file, so
	// backfilling them (as SetSink would) would duplicate the whole log.
	return &Ledger{entries: entries, sink: s}, nil
}

// FileSink appends entries to a file as JSON Lines, fsyncing every append. The
// format is one JSON object per line so an append is O(1) and never rewrites
// history — the reason a log can be append-only on disk and not just in memory.
type FileSink struct {
	mu sync.Mutex
	f  *os.File
}

// OpenFileSink opens path for appending, creating it (and its directory) if
// needed.
func OpenFileSink(path string) (*FileSink, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileSink{f: f}, nil
}

// Append marshals the entry, appends one line, and fsyncs. The fsync is the
// difference between "the decision was written" and "the decision survives a
// power loss."
func (s *FileSink) Append(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return s.f.Sync()
}

// Close closes the underlying file.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
