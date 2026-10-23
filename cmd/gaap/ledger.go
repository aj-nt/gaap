package main

import (
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
//	gaap ledger verify <path>
//	gaap ledger tail [--n N] <path>
func ledgerCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: gaap ledger <verify|tail> <path>")
	}
	switch args[0] {
	case "verify":
		return ledgerVerify(args[1:])
	case "tail":
		return ledgerTail(args[1:])
	default:
		return fmt.Errorf("unknown ledger subcommand %q (want verify or tail)", args[0])
	}
}

// ledgerVerify recomputes the whole hash chain and reports the head.
func ledgerVerify(args []string) error {
	fs := flag.NewFlagSet("ledger verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := fs.Arg(0)
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

	fmt.Printf("ok: %d entries, chain verified\n", len(entries))
	if len(entries) > 0 {
		fmt.Printf("genesis: %s\n", shortHash(entries[0].Hash))
		fmt.Printf("head:    %s\n", shortHash(entries[len(entries)-1].Hash))
	}
	return nil
}

// ledgerTail prints the most recent entries. A chain that does not verify is
// still printed — the operator needs to see what is there — but the failure is
// reported first and on stderr.
func ledgerTail(args []string) error {
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
	fmt.Print(formatLedgerEntries(entries))
	return nil
}

// formatLedgerEntries renders entries as an aligned table. Kept pure so the
// output can be tested without touching stdout.
func formatLedgerEntries(entries []platform.Entry) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SEQ\tTIME\tOUTCOME\tTOOL\tRULE\tSUBJECT\tREVERSIBILITY\tREASON")
	for _, e := range entries {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
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
