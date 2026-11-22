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

// --- the attestation path, end to end through the CLI ---

// ticketFrom pulls the token out of the rendered decision. The output is a tabwriter table,
// so the token is the second field of the "ticket:" row.
func ticketFrom(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "ticket:" {
			return f[1]
		}
	}
	t.Fatalf("no ticket in output:\n%s", out)
	return ""
}

// An allowed pure read, asked for with an entry and a resource, mints a token that verifies
// against the key the CLI wrote — and that token is what the broker will accept.
func TestGovernDecideIssuesATicketForAPureRead(t *testing.T) {
	state, policy := governFixture(t)
	var out bytes.Buffer
	err := governDecide([]string{
		"--state", state,
		"--policy", policy,
		"--subject", "agent-a",
		"--tool", "ReadFile",
		"--args", `{"path":"/etc/hosts"}`,
		// The workload declares which of its tools are reads; the kernel never guesses.
		"--read-tools", "ReadFile",
		"--ticket-entry", "e1",
		"--ticket-resource", "/etc/hosts",
	}, &out)
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if !strings.Contains(out.String(), "allow") {
		t.Fatalf("output = %q, want an allow", out.String())
	}
	token := ticketFrom(t, out.String())

	key, err := platform.LoadTicketKey(filepath.Join(state, "ticket.key"))
	if err != nil {
		t.Fatalf("the CLI did not leave a key both sides can read: %v", err)
	}
	tk, err := key.Verify(token, "e1", "agent-a", "/etc/hosts")
	if err != nil {
		t.Fatalf("issued ticket does not verify: %v", err)
	}
	if tk.EntryID != "e1" || tk.Resource != "/etc/hosts" {
		t.Fatalf("claims = %+v", tk)
	}
}

// Without the workload saying which tool is a read, the kernel classifies it irreversible and
// the ticket is refused: the CLI cannot talk itself into an attestation.
func TestGovernDecideRefusesATicketForAnUnclassifiedCall(t *testing.T) {
	state, policy := governFixture(t)
	var out bytes.Buffer
	err := governDecide([]string{
		"--state", state,
		"--policy", policy,
		"--subject", "agent-a",
		"--tool", "ReadFile",
		"--ticket-entry", "e1",
		"--ticket-resource", "/etc/hosts",
	}, &out)
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "deny") || !strings.Contains(s, "only a pure read") {
		t.Fatalf("output = %q, want a deny that says why", s)
	}
	if strings.Contains(s, "ticket:\t") {
		t.Fatalf("a ticket was printed for a refused call:\n%s", s)
	}
}

func TestGovernDecideRequiresBothTicketFlags(t *testing.T) {
	state, policy := governFixture(t)
	_, err := runDecide(t, state, policy, "agent-a", "ReadFile", `{}`)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	var out bytes.Buffer
	err = governDecide([]string{
		"--state", state, "--policy", policy, "--subject", "agent-a", "--tool", "ReadFile",
		"--ticket-entry", "e1",
	}, &out)
	if err == nil || !strings.Contains(err.Error(), "go together") {
		t.Fatalf("err = %v, want a complaint about the pair", err)
	}
}

// --- the provenance floor, through the decision verb ---

// taintPolicy requires internal provenance before an allow stands. That requirement was
// implementable and unreachable before this: the kernel could escalate since M7b, and no
// production caller ever passed a floor.
const taintPolicy = `{
  "version": 1,
  "rules": [
    {"id": "read-own", "tool": "ReadFile", "action": "allow", "min_taint": "internal"}
  ]
}`

func policyFixture(t *testing.T, body string) (stateDir, policyPath string) {
	t.Helper()
	dir := t.TempDir()
	policyPath = filepath.Join(dir, "rules.json")
	if err := os.WriteFile(policyPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return filepath.Join(dir, "state"), policyPath
}

func runDecideExtra(t *testing.T, stateDir, policyPath, subject, tool string, extra ...string) (string, error) {
	t.Helper()
	args := []string{"--state", stateDir, "--policy", policyPath, "--subject", subject, "--tool", tool}
	args = append(args, extra...)
	var out bytes.Buffer
	err := governDecide(args, &out)
	return out.String(), err
}

// A weak justification does not carry an allow the rule reserved for a strong one.
func TestGovernDecideEscalatesOnAWeakFloor(t *testing.T) {
	state, policy := policyFixture(t, taintPolicy)
	out, err := runDecideExtra(t, state, policy, "agent-a", "ReadFile", "--floor", "external")
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if !strings.Contains(out, "review") {
		t.Fatalf("output = %q, want a review", out)
	}
	if !strings.Contains(out, "requires internal") || !strings.Contains(out, "external") {
		t.Fatalf("output = %q, want the requirement and the supplied floor", out)
	}
	if !strings.Contains(out, "min taint") || !strings.Contains(out, "justification floor") {
		t.Fatalf("output = %q, want both provenance rows", out)
	}
}

// Strong enough: the allow stands, and the floor is on the record instead of being implied.
func TestGovernDecideAllowsAtTheRequiredFloor(t *testing.T) {
	state, policy := policyFixture(t, taintPolicy)
	out, err := runDecideExtra(t, state, policy, "agent-a", "ReadFile", "--floor", "internal")
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if !strings.Contains(out, "allow") || strings.Contains(out, "review") {
		t.Fatalf("output = %q, want an allow", out)
	}
}

// No floor is not the same as external, and the difference is visible: unreported fails the
// requirement too, but it says so.
func TestGovernDecideTreatsAnUnstatedFloorAsUnreported(t *testing.T) {
	state, policy := policyFixture(t, taintPolicy)
	out, err := runDecideExtra(t, state, policy, "agent-a", "ReadFile")
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if !strings.Contains(out, "review") || !strings.Contains(out, "unreported") {
		t.Fatalf("output = %q, want a review naming the unreported floor", out)
	}
}

// A typo must not quietly weaken the justification: it must not read as "no floor supplied"
// either, which is a legitimate state the engine already handles.
func TestGovernDecideRefusesAnUnknownFloor(t *testing.T) {
	state, policy := policyFixture(t, taintPolicy)
	_, err := runDecideExtra(t, state, policy, "agent-a", "ReadFile", "--floor", "externl")
	if err == nil {
		t.Fatal("a misspelled provenance class was accepted")
	}
	if !strings.Contains(err.Error(), "not a provenance class") {
		t.Fatalf("err = %v, want it to name the vocabulary", err)
	}
}

// The two mechanisms in one call: a review has authorised nothing, so it must not leave a
// credential behind for a call a human then rejects.
func TestGovernDecideMintsNoTicketForAnEscalatedAllow(t *testing.T) {
	state, policy := policyFixture(t, taintPolicy)
	out, err := runDecideExtra(t, state, policy, "agent-a", "ReadFile",
		"--read-tools", "ReadFile",
		"--floor", "external",
		"--ticket-entry", "e1", "--ticket-resource", "/etc/hosts")
	if err != nil {
		t.Fatalf("governDecide: %v", err)
	}
	if !strings.Contains(out, "review") {
		t.Fatalf("output = %q, want a review", out)
	}
	if strings.Contains(out, "ticket:") {
		t.Fatalf("an escalated allow minted a ticket:\n%s", out)
	}
}
