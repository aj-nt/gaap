package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aj-nt/gaap/platform"
)

// seedLedger writes a small durable ledger and returns its path.
func seedLedger(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := platform.NewLedgerAt(path)
	if err != nil {
		t.Fatalf("NewLedgerAt: %v", err)
	}
	g := platform.NewGovernor()
	g.Ledger = l
	g.Policy.AddRule(platform.Rule{ID: "allow-exec", Tool: "Execute", Decision: platform.Allow})
	g.Decide("agent-7", "Execute", map[string]any{"command": "ls"})
	g.Decide("agent-7", "FileWrite", nil) // deny-by-default
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func TestLedgerVerifyAcceptsCleanFile(t *testing.T) {
	path := seedLedger(t)
	if err := ledgerVerify([]string{path}); err != nil {
		t.Fatalf("verify on a clean ledger: %v", err)
	}
}

func TestLedgerVerifyRejectsTamperedFile(t *testing.T) {
	path := seedLedger(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	tampered := strings.Replace(string(raw), `"outcome":"allow"`, `"outcome":"deny"`, 1)
	if tampered == string(raw) {
		t.Fatal("test setup: outcome marker not found")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err = ledgerVerify([]string{path})
	if err == nil {
		t.Fatal("verify accepted a tampered ledger")
	}
	if !strings.Contains(err.Error(), "VERIFY FAILED") {
		t.Fatalf("verify error = %q, want it to report verification failure", err)
	}
}

func TestLedgerVerifyRequiresPath(t *testing.T) {
	if err := ledgerVerify(nil); err == nil {
		t.Fatal("verify accepted a missing path")
	}
}

func TestLedgerTailRejectsUnknownSubcommand(t *testing.T) {
	if err := ledgerCommand([]string{"nonsense"}); err == nil {
		t.Fatal("unknown ledger subcommand was accepted")
	}
}

func TestLedgerCommandRequiresSubcommand(t *testing.T) {
	if err := ledgerCommand(nil); err == nil {
		t.Fatal("missing ledger subcommand was accepted")
	}
}

func TestFormatLedgerEntriesShowsAttribution(t *testing.T) {
	l, err := platform.LoadLedger(seedLedger(t))
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	out := formatLedgerEntries(l.Entries())

	for _, want := range []string{"SEQ", "OUTCOME", "TOOL", "RULE", "SUBJECT", "allow-exec", "Execute", "deny"} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q:\n%s", want, out)
		}
	}
	// The reader is also a check on the design: no identity in the record.
	if strings.Contains(out, "agent-7") {
		t.Errorf("table leaked the agent identity:\n%s", out)
	}
}

func TestFormatLedgerEntriesEmpty(t *testing.T) {
	out := formatLedgerEntries(nil)
	if !strings.Contains(out, "SEQ") {
		t.Fatalf("empty table lost its header: %q", out)
	}
}
