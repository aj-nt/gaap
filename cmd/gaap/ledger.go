package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aj-nt/gaap/platform"
)

// ledgerCommand is the operator's view of the governor's record. Without a
// reader the ledger is write-only, and a write-only record is not evidence.
//
//	gaap ledger verify <path> [--state DIR]
//	gaap ledger tail [--n N] <path>
//	gaap ledger export <path> --out FILE
func ledgerCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: gaap ledger <verify|tail|export> [flags] <path>")
	}
	switch args[0] {
	case "verify":
		return ledgerVerify(args[1:], out)
	case "tail":
		return ledgerTail(args[1:], out)
	case "export":
		return ledgerExport(args[1:], out)
	default:
		return fmt.Errorf("unknown ledger subcommand %q (want verify, tail or export)", args[0])
	}
}

// ledgerVerify recomputes the whole hash chain, reports the head, and — when a
// state directory is given — checks every stored commitment against the content
// it points at.
//
// The two checks answer different questions and neither implies the other. The
// chain proves nobody edited the record; the commitments prove the record still
// describes what the governor actually decided on.
func ledgerVerify(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("ledger verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state", "", "state directory: also verify content against its commitments")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := fs.Arg(0)
	if path == "" && *stateDir != "" {
		path = ledgerPath(*stateDir)
	}
	if path == "" {
		return fmt.Errorf("ledger path is required")
	}

	l, err := platform.LoadLedger(path)
	if err != nil {
		return err
	}
	entries := l.Entries()
	if err := l.Verify(); err != nil {
		return fmt.Errorf("VERIFY FAILED: %w", err)
	}

	_, _ = fmt.Fprintf(out, "ok: %d entries, chain verified\n", len(entries))
	if len(entries) > 0 {
		_, _ = fmt.Fprintf(out, "genesis: %s\n", shortHash(entries[0].Hash))
		_, _ = fmt.Fprintf(out, "head:    %s\n", shortHash(entries[len(entries)-1].Hash))
	}
	if *stateDir != "" {
		return verifyCommitments(*stateDir, entries, out)
	}
	return nil
}

// verifyCommitments resolves each recorded commitment back to the content it
// points at and asks the committer whether it still matches.
//
// An unverifiable entry is not automatically a failure: after an erasure the
// content is deleted and the key destroyed on purpose, so "erased" is the
// expected end state and the chain is still supposed to verify. "Missing while
// the key is intact" and "does not match" are failures — they mean the record
// describes something other than what was decided.
func verifyCommitments(stateDir string, entries []platform.Entry, out io.Writer) error {
	state, err := openGovernState(stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()

	committer, err := platform.NewCommitter(state.ring)
	if err != nil {
		return err
	}

	var verified, erased, missing, mismatched int
	var problems []string
	for _, e := range entries {
		if e.CommitAlgo == "" || e.InputHash == "" || e.SubjectRef == "" {
			continue // legacy unkeyed entry: nothing to resolve
		}
		content, err := state.content.Get(e.SubjectRef, e.InputHash)
		if err != nil {
			if !errors.Is(err, platform.ErrContentNotFound) {
				return fmt.Errorf("seq %d: %w", e.Seq, err)
			}
			if _, kerr := state.ring.Key(e.SubjectRef); errors.Is(kerr, platform.ErrKeyDestroyed) {
				erased++
				continue
			}
			missing++
			problems = append(problems, fmt.Sprintf("seq %d: content is missing and the subject key is intact", e.Seq))
			continue
		}
		ok, err := committer.Match(e.SubjectRef, string(content), e.InputHash)
		if err != nil {
			if errors.Is(err, platform.ErrKeyDestroyed) {
				erased++
				continue
			}
			return fmt.Errorf("seq %d: %w", e.Seq, err)
		}
		if !ok {
			mismatched++
			problems = append(problems, fmt.Sprintf("seq %d: content does not match its commitment", e.Seq))
			continue
		}
		verified++
	}

	_, _ = fmt.Fprintf(out, "content: %d verified, %d erased, %d missing, %d mismatched\n", verified, erased, missing, mismatched)
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "FAIL: %s\n", p)
	}
	if missing+mismatched > 0 {
		return fmt.Errorf("content verification failed: %d mismatched, %d missing", mismatched, missing)
	}
	return nil
}

// ledgerExport writes a verified copy of a record. Ledger.Save is the atomic
// rewrite path: temp file, fsync, rename, fsync the directory.
func ledgerExport(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("ledger export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dest := fs.String("out", "", "destination path (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	from := fs.Arg(0)
	if from == "" {
		return fmt.Errorf("ledger path is required")
	}
	if *dest == "" {
		return fmt.Errorf("ledger export requires --out")
	}

	l, err := platform.LoadLedger(from)
	if err != nil {
		return err
	}
	// Refuse to launder a corrupt record into a fresh file.
	if err := l.Verify(); err != nil {
		return fmt.Errorf("refusing to export a record that does not verify: %w", err)
	}
	if err := l.Save(*dest); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "exported: %d entries\nfrom:     %s\nto:       %s\n", len(l.Entries()), from, *dest)
	return nil
}

// ledgerTail prints the most recent entries. A chain that does not verify is
// still printed — the operator needs to see what is there — but the failure is
// reported first and on stderr.
func ledgerTail(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("ledger tail", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	n := fs.Int("n", 20, "number of entries to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := fs.Arg(0)
	if path == "" {
		return fmt.Errorf("ledger path is required")
	}
	if *n <= 0 {
		return fmt.Errorf("--n must be greater than 0")
	}

	l, err := platform.LoadLedger(path)
	if err != nil {
		return err
	}
	entries := l.Entries()
	verifyErr := l.Verify()
	if verifyErr != nil {
		fmt.Fprintf(os.Stderr, "warning: chain does not verify: %v\n", verifyErr)
	}
	if len(entries) > *n {
		entries = entries[len(entries)-*n:]
	}
	_, _ = fmt.Fprint(out, formatLedgerEntries(entries))
	return nil
}

// formatLedgerEntries renders entries as an aligned table. Kept pure so the
// output can be tested without touching stdout.
func formatLedgerEntries(entries []platform.Entry) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SEQ	TIME	OUTCOME	TOOL	RULE	SUBJECT	REVERSIBILITY	REASON")
	for _, e := range entries {
		_, _ = fmt.Fprintf(w, "%d	%s	%s	%s	%s	%s	%s	%s\n",
			e.Seq,
			e.Time.UTC().Format(time.RFC3339),
			e.Outcome,
			orDash(e.Tool),
			orDash(e.RuleID),
			orDash(shortHash(e.SubjectHash)),
			orDash(string(e.Reversibility)),
			orDash(e.Reason),
		)
	}
	_ = w.Flush()
	return b.String()
}

// shortHash abbreviates a hash for display. The full hash is in the ledger.
func shortHash(h string) string {
	const n = 8
	if len(h) <= n {
		return h
	}
	return h[:n]
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
