package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aj-nt/gaap/platform"
)

const governTestPolicy = `{
  "version": 1,
  "rules": [
    {"id": "read-only-tools", "tool": "ReadFile", "action": "allow"},
    {"id": "no-destructive-shell", "tool": "Execute", "action": "deny",
     "reason": "destructive shell command", "args_contain": {"command": "rm -rf"}}
  ]
}`

func governFixture(t *testing.T) (stateDir, policyPath string) {
	t.Helper()
	dir := t.TempDir()
	policyPath = filepath.Join(dir, "rules.json")
	if err := os.WriteFile(policyPath, []byte(governTestPolicy), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return filepath.Join(dir, "state"), policyPath
}

func runDecide(t *testing.T, stateDir, policyPath, subject, tool, args string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := governDecide([]string{
		"--state", stateDir,
		"--policy", policyPath,
		"--subject", subject,
		"--tool", tool,
		"--args", args,
	}, &out)
	return out.String(), err
}

func TestGovernDecideDeniesDestructiveCommand(t *testing.T) {
	state, policy := governFixture(t)
	out, err := runDecide(t, state, policy, "agent-1", "Execute", `{"command":"rm -rf /tmp/gaap-demo"}`)
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if !strings.Contains(out, "deny") {
		t.Fatalf("output = %q, want a deny", out)
	}
	if !strings.Contains(out, "no-destructive-shell") {
		t.Fatalf("output = %q, want the rule ID for attribution", out)
	}
	if !strings.Contains(out, "destructive shell command") {
		t.Fatalf("output = %q, want the reason", out)
	}
}

func TestGovernDecideAllowsReadAndRecordsIt(t *testing.T) {
	state, policy := governFixture(t)
	out, err := runDecide(t, state, policy, "agent-1", "ReadFile", `{"path":"/etc/hosts"}`)
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if !strings.Contains(out, "allow") {
		t.Fatalf("output = %q, want an allow", out)
	}
	entries := readLedger(t, state)
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	if entries[0].Outcome != platform.ActionAllow {
		t.Fatalf("recorded outcome = %q", entries[0].Outcome)
	}
	if entries[0].InputHash == "" || entries[0].SubjectRef == "" {
		t.Fatal("entry is missing its commitment or subject handle")
	}
}

func TestGovernDecideStoresTheArguments(t *testing.T) {
	state, policy := governFixture(t)
	if _, err := runDecide(t, state, policy, "agent-1", "ReadFile", `{"path":"/etc/hosts"}`); err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	entries := readLedger(t, state)
	content, err := platform.OpenFileContentStore(filepath.Join(state, "content"))
	if err != nil {
		t.Fatalf("open content: %v", err)
	}
	got, err := content.Get(entries[0].SubjectRef, entries[0].InputHash)
	if err != nil {
		t.Fatalf("content was not stored under the commitment: %v", err)
	}
	if string(got) != `{"path":"/etc/hosts"}` {
		t.Fatalf("stored content = %q", got)
	}
}

func TestGovernDecideRequiresPolicy(t *testing.T) {
	state, _ := governFixture(t)
	if _, err := runDecide(t, state, "", "agent-1", "ReadFile", "{}"); err == nil {
		t.Fatal("decide without a policy file reported success")
	}
}

func TestGovernDecideRejectsBadArgsJSON(t *testing.T) {
	state, policy := governFixture(t)
	if _, err := runDecide(t, state, policy, "agent-1", "ReadFile", `{nope`); err == nil {
		t.Fatal("malformed --args was accepted")
	}
}

func TestGovernEraseRemovesContentAndKeepsTheChain(t *testing.T) {
	state, policy := governFixture(t)
	if _, err := runDecide(t, state, policy, "agent-1", "ReadFile", `{"path":"/a"}`); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if _, err := runDecide(t, state, policy, "agent-1", "ReadFile", `{"path":"/b"}`); err != nil {
		t.Fatalf("decide: %v", err)
	}
	before := readLedger(t, state)

	var out bytes.Buffer
	if err := governErase([]string{
		"--state", state,
		"--subject", "agent-1",
		"--reason", "gdpr-request",
	}, &out); err != nil {
		t.Fatalf("governErase: %v", err)
	}
	if !strings.Contains(out.String(), "erased") {
		t.Fatalf("output = %q, want a confirmation", out.String())
	}

	content, err := platform.OpenFileContentStore(filepath.Join(state, "content"))
	if err != nil {
		t.Fatalf("open content: %v", err)
	}
	if content.Has(before[0].SubjectRef) {
		t.Fatal("arguments survived the erasure")
	}

	after := readLedger(t, state)
	if len(after) != 3 {
		t.Fatalf("ledger entries = %d, want 2 decisions plus the erase entry", len(after))
	}
	if after[2].Kind != platform.EraseKind {
		t.Fatalf("last entry kind = %q, want %q", after[2].Kind, platform.EraseKind)
	}
	reloaded, err := platform.LoadLedger(ledgerPath(state))
	if err != nil {
		t.Fatalf("LoadLedger after erasure: %v", err)
	}
	if err := reloaded.Verify(); err != nil {
		t.Fatalf("chain failed verification after the erasure: %v", err)
	}
}

func TestGovernEraseUnknownSubjectIsANoOp(t *testing.T) {
	state, _ := governFixture(t)
	var out bytes.Buffer
	if err := governErase([]string{"--state", state, "--subject", "ghost", "--reason", "test"}, &out); err != nil {
		t.Fatalf("governErase: %v", err)
	}
	if len(readLedger(t, state)) != 0 {
		t.Fatal("erasing a subject that was never recorded wrote a ledger entry")
	}
}

// readLedger loads the state ledger, tolerating its absence.
func readLedger(t *testing.T, stateDir string) []platform.Entry {
	t.Helper()
	path := ledgerPath(stateDir)
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	l, err := platform.LoadLedger(path)
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	return l.Entries()
}
