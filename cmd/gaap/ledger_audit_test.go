package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verifyState runs the audit and returns its output.
func verifyState(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := ledgerVerify(args, &out)
	return out.String(), err
}

// contentFile returns the single stored content file under the state's content
// store, so a test can tamper with the plaintext the commitment points at.
func contentFile(t *testing.T, stateDir string) string {
	t.Helper()
	var found string
	err := filepath.Walk(filepath.Join(stateDir, "content"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if found != "" {
			t.Fatalf("expected one content file, found another at %s", path)
		}
		found = path
		return nil
	})
	if err != nil {
		t.Fatalf("walk content store: %v", err)
	}
	if found == "" {
		t.Fatal("no content was stored for the decision")
	}
	return found
}

// TestLedgerVerifyChecksContentAgainstCommitments is the half of the record that
// was missing: the chain proved the ledger had not been edited, and nothing
// proved the content it points at was still the content it decided on.
func TestLedgerVerifyChecksContentAgainstCommitments(t *testing.T) {
	state, policy := governFixture(t)
	if _, err := runDecide(t, state, policy, "agent-1", "ReadFile", `{"path":"/etc/hosts"}`); err != nil {
		t.Fatalf("governDecide: %v", err)
	}

	out, err := verifyState(t, "--state", state)
	if err != nil {
		t.Fatalf("verify on an intact record: %v", err)
	}
	if !strings.Contains(out, "1 verified") {
		t.Fatalf("output = %q, want the commitment to verify", out)
	}
}

// TestLedgerVerifyCatchesTamperedContent: the ledger is not edited here — the
// content it committed to is. Nothing else in the toolchain would notice.
func TestLedgerVerifyCatchesTamperedContent(t *testing.T) {
	state, policy := governFixture(t)
	if _, err := runDecide(t, state, policy, "agent-1", "ReadFile", `{"path":"/etc/hosts"}`); err != nil {
		t.Fatalf("governDecide: %v", err)
	}

	path := contentFile(t, state)
	if err := os.WriteFile(path, []byte(`{"path":"/etc/shadow"}`), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	_, err := verifyState(t, "--state", state)
	if err == nil {
		t.Fatal("verify accepted content that no longer matches its commitment")
	}
	if !strings.Contains(err.Error(), "1 mismatched") {
		t.Fatalf("error = %v, want a mismatch count", err)
	}
}

// TestLedgerVerifyAcceptsErasedSubjects: after an erasure the content is gone and
// the key is destroyed on purpose, so an unverifiable entry is the expected
// outcome, not a failure. The chain must still verify.
func TestLedgerVerifyAcceptsErasedSubjects(t *testing.T) {
	state, policy := governFixture(t)
	if _, err := runDecide(t, state, policy, "alice@example.com", "ReadFile", `{"path":"/etc/hosts"}`); err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if err := governErase([]string{"--state", state, "--subject", "alice@example.com"}, io.Discard); err != nil {
		t.Fatalf("governErase: %v", err)
	}

	out, err := verifyState(t, "--state", state)
	if err != nil {
		t.Fatalf("verify after erasure: %v", err)
	}
	if !strings.Contains(out, "1 erased") {
		t.Fatalf("output = %q, want the erased entry accounted for", out)
	}
}

// TestLedgerExportWritesAVerifyingCopy pins Ledger.Save to a real caller: an
// operator taking the record off the box.
func TestLedgerExportWritesAVerifyingCopy(t *testing.T) {
	state, policy := governFixture(t)
	for i := 0; i < 3; i++ {
		if _, err := runDecide(t, state, policy, "agent-1", "ReadFile", `{}`); err != nil {
			t.Fatalf("governDecide: %v", err)
		}
	}
	out := filepath.Join(t.TempDir(), "exported.jsonl")

	// Flags must precede the positional path: Go's flag package stops at the
	// first non-flag argument.
	if err := ledgerExport([]string{"--out", out, ledgerPath(state)}, io.Discard); err != nil {
		t.Fatalf("ledgerExport: %v", err)
	}
	verifyOut, err := verifyState(t, out)
	if err != nil {
		t.Fatalf("verify exported copy: %v", err)
	}
	if !strings.Contains(verifyOut, "3 entries") {
		t.Fatalf("output = %q, want all three entries", verifyOut)
	}
}
