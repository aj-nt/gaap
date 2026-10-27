package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aj-nt/gaap/platform"
)

// TestDemoFlow is the executable version of examples/demo/run.sh. It uses the
// policy file the demo ships, so the script cannot claim behaviour the code does
// not have: if this test passes and the script fails, the script is stale, and if
// this test fails the demo is a lie.
func TestDemoFlow(t *testing.T) {
	policyPath := filepath.Join("..", "..", "examples", "demo", "rules.json")
	if _, err := os.Stat(policyPath); err != nil {
		t.Fatalf("demo policy missing: %v", err)
	}
	state := filepath.Join(t.TempDir(), "state")

	// 1. A read is allowed, by a named rule.
	out, err := runDecide(t, state, policyPath, "agent-1", "ReadFile", `{"path":"/etc/hosts"}`)
	if err != nil {
		t.Fatalf("read decision: %v", err)
	}
	if !strings.Contains(out, "allow") || !strings.Contains(out, "read-only-tools") {
		t.Fatalf("read decision output = %q", out)
	}

	// 2. A destructive command is denied, with the reason the policy names.
	out, err = runDecide(t, state, policyPath, "agent-1", "Execute", `{"command":"rm -rf /tmp/gaap-demo"}`)
	if err != nil {
		t.Fatalf("destructive decision: %v", err)
	}
	if !strings.Contains(out, "deny") || !strings.Contains(out, "destructive shell command") {
		t.Fatalf("destructive decision output = %q", out)
	}

	// 3. A test run passes through, because the policy allows it.
	out, err = runDecide(t, state, policyPath, "agent-1", "Execute", `{"command":"go test ./..."}`)
	if err != nil {
		t.Fatalf("test decision: %v", err)
	}
	if !strings.Contains(out, "allow") || !strings.Contains(out, "allow-tests") {
		t.Fatalf("test decision output = %q", out)
	}

	// 4. The record holds structure and commitments, not arguments.
	raw, err := os.ReadFile(ledgerPath(state))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	for _, argument := range []string{"rm -rf /tmp/gaap-demo", "go test", "/etc/hosts"} {
		if strings.Contains(string(raw), argument) {
			t.Fatalf("the ledger contains the argument %q; arguments belong in the erasable store", argument)
		}
	}
	if strings.Contains(string(raw), "agent-1") {
		t.Fatal("the ledger contains the subject identity")
	}

	// 5. The chain verifies, and a single changed byte breaks it.
	l, err := platform.LoadLedger(ledgerPath(state))
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("clean chain failed verification: %v", err)
	}
	tamperedPath := filepath.Join(t.TempDir(), "tampered.jsonl")
	tampered := strings.Replace(string(raw), `"outcome":"allow"`, `"outcome":"deny"`, 1)
	if tampered == string(raw) {
		t.Fatal("fixture: nothing to tamper")
	}
	if err := os.WriteFile(tamperedPath, []byte(tampered), 0o600); err != nil {
		t.Fatalf("write tampered ledger: %v", err)
	}
	broken, err := platform.LoadLedger(tamperedPath)
	if err != nil {
		t.Fatalf("LoadLedger(tampered): %v", err)
	}
	if err := broken.Verify(); err == nil {
		t.Fatal("the tampered chain verified")
	}

	// 6. Erasure destroys the content and the key, and the chain still verifies.
	before := readLedger(t, state)
	var eraseOut bytes.Buffer
	if err := governErase([]string{"--state", state, "--subject", "agent-1", "--reason", "gdpr-request"}, &eraseOut); err != nil {
		t.Fatalf("governErase: %v", err)
	}
	content, err := platform.OpenFileContentStore(filepath.Join(state, "content"))
	if err != nil {
		t.Fatalf("open content: %v", err)
	}
	if content.Has(before[0].SubjectRef) {
		t.Fatal("the arguments survived the erasure")
	}
	after, err := platform.LoadLedger(ledgerPath(state))
	if err != nil {
		t.Fatalf("LoadLedger after erasure: %v", err)
	}
	if err := after.Verify(); err != nil {
		t.Fatalf("chain failed verification after erasure: %v", err)
	}
	if got := len(after.Entries()); got != len(before)+1 {
		t.Fatalf("entries = %d, want %d", got, len(before)+1)
	}
	if last := after.Entries()[len(after.Entries())-1]; last.Kind != platform.EraseKind {
		t.Fatalf("last entry kind = %q, want %q", last.Kind, platform.EraseKind)
	}
}
