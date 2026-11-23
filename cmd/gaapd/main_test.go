package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/memstore"
	"github.com/aj-nt/gaap/internal/principal"
	"github.com/aj-nt/gaap/platform"
)

const allowRead = `{"version": 1, "rules": [
  {"id": "read", "tool": "ReadFile", "action": "allow"},
  {"id": "exec", "tool": "Execute", "action": "allow"}
]}`

const denyRead = `{"version": 1, "rules": [
  {"id": "read", "tool": "ReadFile", "action": "deny", "reason": "the fleet was told to stop reading"}
]}`

// testBroker writes a policy and a grant file in the shape a host actually has: one login,
// several agents. The synthetic uid keeps the test independent of who runs it; the socket test
// below is the one that has to use the real uid.
func testBroker(t *testing.T, policy string, grants principal.Grants) *broker {
	t.Helper()
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}

	var principals []map[string]any
	for uid, agents := range grants {
		principals = append(principals, map[string]any{"uid": uid, "agents": agents})
	}
	raw, err := json.Marshal(map[string]any{"principals": principals})
	if err != nil {
		t.Fatal(err)
	}
	grantsPath := filepath.Join(dir, "memory-grants.json")
	if err := os.WriteFile(grantsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	b, err := newBroker(config{
		StateDir:   filepath.Join(dir, "state"),
		PolicyPath: policyPath,
		GrantsPath: grantsPath,
	})
	if err != nil {
		t.Fatalf("newBroker: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// twoAgentsOnOneLogin is the deployment shape the daemon exists for.
func twoAgentsOnOneLogin() principal.Grants {
	return principal.Grants{501: {"vagent-analyst", "vagent-worker"}}
}

func decide(t *testing.T, b *broker, uid uint32, claimed, tool string) Response {
	t.Helper()
	return b.serve(Request{Op: "decide", Agent: claimed, Tool: tool, Args: map[string]any{}}, uid)
}

// The whole point of the daemon: one policy, N agents behind one login, and an edit that
// lands on the next call rather than at the next restart.
func TestOnePolicyFlipStopsEveryAgent(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	agents := []string{"vagent-analyst", "vagent-worker"}

	for _, name := range agents {
		resp := decide(t, b, 501, name, "ReadFile")
		if !resp.OK || resp.Decision == nil || resp.Decision.Action != platform.ActionAllow {
			t.Fatalf("%s: %+v, want an allow", name, resp)
		}
	}

	// The operator edits one file. Distinct length and a forced mtime, because a reload
	// keyed on (size, mtime) must not be defeated by a same-second edit.
	if err := os.WriteFile(b.policyPath, []byte(denyRead), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(b.policyPath, future, future); err != nil {
		t.Fatal(err)
	}

	for _, name := range agents {
		resp := decide(t, b, 501, name, "ReadFile")
		if !resp.OK || resp.Decision == nil {
			t.Fatalf("%s: %+v", name, resp)
		}
		if resp.Decision.Action != platform.ActionDeny {
			t.Fatalf("%s still allowed after the flip: %+v", name, resp.Decision)
		}
		if resp.Decision.RuleID != "read" {
			t.Fatalf("refusal does not name the rule that produced it: %q", resp.Decision.RuleID)
		}
	}

	entries := b.ledger.Entries()
	if len(entries) != 4 {
		t.Fatalf("ledger has %d entries, want 4 (one per decision)", len(entries))
	}
	if entries[2].Outcome != platform.ActionDeny || entries[2].RuleID != "read" {
		t.Fatalf("third entry = %+v, want the denial", entries[2])
	}
	// The two halves were decided under different policies, and the record says so.
	if entries[0].PolicyRev == entries[2].PolicyRev {
		t.Fatalf("both halves carry policy revision %q: the flip left no mark", entries[0].PolicyRev)
	}
	// Two agents behind one uid stay two subjects in the record — and the record does not
	// name them: the commitment is keyed, the ref is opaque.
	if entries[0].SubjectHash == entries[1].SubjectHash {
		t.Fatal("both agents share one subject commitment")
	}
	if entries[0].SubjectRef == "" || entries[0].SubjectRef == entries[1].SubjectRef {
		t.Fatalf("subject refs are missing or shared: %q / %q", entries[0].SubjectRef, entries[1].SubjectRef)
	}
	if entries[0].CommitAlgo == "" {
		t.Fatal("the subject commitment is unkeyed: an agent id is low-entropy, so a plain hash is a re-identification oracle")
	}
}

// A ledger shared by a fleet must not be readable as a roster. Two agents, four decisions, and
// neither name appears on disk — while the rows stay groupable by subject.
func TestTheLedgerDoesNotNameItsSubjects(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	for _, name := range []string{"vagent-analyst", "vagent-worker"} {
		decide(t, b, 501, name, "ReadFile")
	}
	raw, err := os.ReadFile(filepath.Join(b.stateDir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vagent-analyst", "vagent-worker"} {
		if strings.Contains(string(raw), name) {
			t.Fatalf("the ledger names %q in plaintext", name)
		}
	}
	entries := b.ledger.Entries()
	if len(entries) != 2 || entries[0].SubjectRef == "" || entries[0].SubjectRef == entries[1].SubjectRef {
		t.Fatalf("rows are not attributable to distinct subjects: %+v", entries)
	}
}

// One login, several agents, no claim: refused. Choosing for the caller would put a decision
// on a workload that never asked for it.
func TestOneLoginWithSeveralAgentsNeedsAClaim(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	resp := b.serve(Request{Op: "decide", Tool: "ReadFile"}, 501)
	if resp.OK {
		t.Fatal("an ambiguous peer was decided without naming an agent")
	}
	if !strings.Contains(resp.Error, "vagent-analyst") || !strings.Contains(resp.Error, "vagent-worker") {
		t.Fatalf("refusal %q does not name the choices", resp.Error)
	}
	if n := len(b.ledger.Entries()); n != 0 {
		t.Fatalf("an ambiguous request produced %d ledger entries", n)
	}
}

// One agent on the login needs no claim: nothing is ambiguous, and demanding a name would be a
// migration for callers that have nothing to disambiguate.
func TestSingleAgentOnALoginNeedsNoClaim(t *testing.T) {
	b := testBroker(t, allowRead, principal.Grants{502: {"vagent-admin"}})
	resp := b.serve(Request{Op: "decide", Tool: "ReadFile"}, 502)
	if !resp.OK || resp.Decision == nil || resp.Decision.Action != platform.ActionAllow {
		t.Fatalf("single-agent login: %+v", resp)
	}
}

// A claim outside the grant is impersonation: it writes nothing, and the error says which claim
// was refused.
func TestClaimedIdentityMustMatchTheGrant(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	resp := b.serve(Request{Op: "decide", Agent: "vagent-admin", Tool: "ReadFile"}, 501)
	if resp.OK {
		t.Fatal("a peer decided as an agent outside its grant")
	}
	if !strings.Contains(resp.Error, "not authorised to act as vagent-admin") {
		t.Fatalf("error = %q, want it to name the claim", resp.Error)
	}
	if n := len(b.ledger.Entries()); n != 0 {
		t.Fatalf("an unauthenticated claim produced %d ledger entries", n)
	}
}

func TestUnmappedPeerIsRefused(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	resp := b.serve(Request{Op: "decide", Agent: "vagent-analyst", Tool: "ReadFile"}, 999)
	if resp.OK {
		t.Fatal("an unmapped uid was allowed to decide")
	}
	if !strings.Contains(resp.Error, "not mapped to an agent") {
		t.Fatalf("error = %q, want the missing mapping named", resp.Error)
	}
}

// A policy file that stops parsing must not stop the fleet, and must not silently replace the
// rules either: the last good policy stays in force and keeps reporting its own revision.
func TestBrokenPolicyKeepsTheLastGoodOne(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	first := decide(t, b, 501, "vagent-analyst", "ReadFile")
	if !first.OK || first.Revision == "" {
		t.Fatalf("baseline: %+v", first)
	}

	if err := os.WriteFile(b.policyPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(b.policyPath, future, future); err != nil {
		t.Fatal(err)
	}

	second := decide(t, b, 501, "vagent-analyst", "ReadFile")
	if !second.OK || second.Decision == nil || second.Decision.Action != platform.ActionAllow {
		t.Fatalf("a broken policy file changed a decision: %+v", second)
	}
	if second.Revision != first.Revision {
		t.Fatalf("revision moved to %q on a policy that failed to parse", second.Revision)
	}
}

// The response has to be usable by a client that was not compiled against these structs.
func TestDecisionSurvivesTheWire(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	resp := decide(t, b, 501, "vagent-analyst", "ReadFile")
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Response
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Decision == nil || back.Decision.Action != platform.ActionAllow || back.Revision == "" {
		t.Fatalf("round trip lost the decision: %s", encoded)
	}
}

// The transport, end to end: a real unix socket, a real peer uid, and the claim that the peer
// credentials cannot supply — two agents share the uid, so the request has to say which.
func TestSocketPeerIdentity(t *testing.T) {
	me := uint32(os.Getuid())
	b := testBroker(t, allowRead, principal.Grants{int(me): {"vagent-analyst", "vagent-worker"}})

	dir, err := os.MkdirTemp("", "gaapd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "g.sock")
	ln, err := listen(sock, 0o600)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		b.handle(conn)
	}()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := json.NewEncoder(conn).Encode(Request{Op: "decide", Agent: "vagent-worker", Tool: "ReadFile"}); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Decision == nil || resp.Decision.Action != platform.ActionAllow {
		t.Fatalf("over the socket: %+v", resp)
	}
	if resp.Revision == "" {
		t.Fatal("response carries no policy revision")
	}
}

// A register teaches the broker what the agent can actually call: an unregistered name is
// refused as incoherent, and the policy's names that the agent lacks come back as a list the
// operator can act on.
func TestRegisterReportsPolicyNamesTheAgentLacks(t *testing.T) {
	b := testBroker(t, `{"version": 1, "rules": [
  {"id": "read", "tool": "FileRead", "action": "allow"},
  {"id": "shell", "tool": "Shell", "action": "deny", "reason": "no shell"}
]}`, twoAgentsOnOneLogin())

	resp := b.serve(Request{Op: "register", Agent: "vagent-analyst", Catalog: map[string]string{"FileRead": "pure", "CodeRead": "pure"}}, 501)
	if !resp.OK {
		t.Fatalf("register: %+v", resp)
	}
	if resp.Tools != 2 {
		t.Fatalf("tools = %d", resp.Tools)
	}
	if got := strings.Join(resp.UnknownPolicyTools, ","); got != "Shell" {
		t.Fatalf("unknown policy tools = %q, want Shell: the policy names a tool no agent has", got)
	}
}

func TestUnregisteredToolIsRefusedBeforeThePolicy(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	if resp := b.serve(Request{Op: "register", Agent: "vagent-analyst", Catalog: map[string]string{"FileRead": "pure"}}, 501); !resp.OK {
		t.Fatalf("register: %+v", resp)
	}

	// The policy allows the tool; the agent said it does not have it. The agent's catalogue
	// wins, because the alternative is a decision about a call that cannot happen.
	resp := decide(t, b, 501, "vagent-analyst", "Execute")
	if !resp.OK || resp.Decision == nil {
		t.Fatalf("decide: %+v", resp)
	}
	if resp.Decision.Action != platform.ActionDeny || resp.Decision.RuleID != "broker:unknown-tool" {
		t.Fatalf("decision = %+v, want broker:unknown-tool", resp.Decision)
	}
	if !strings.Contains(resp.Decision.Reason, "registered no tool named") {
		t.Fatalf("reason = %q", resp.Decision.Reason)
	}
	if resp.Decision.Denial != nil {
		t.Fatal("an unknown-tool refusal carried a policy denial class")
	}
	entries := b.ledger.Entries()
	last := entries[len(entries)-1]
	if last.RuleID != "broker:unknown-tool" || last.Outcome != platform.ActionDeny {
		t.Fatalf("ledger entry = %+v", last)
	}
}

// No catalogue is not an empty catalogue: a client that never registers keeps the policy-only
// behaviour instead of having every call refused.
func TestAnAgentThatNeverRegisteredKeepsThePolicyPath(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	resp := decide(t, b, 501, "vagent-worker", "Execute")
	if !resp.OK || resp.Decision == nil || resp.Decision.Action != platform.ActionAllow {
		t.Fatalf("unregistered agent: %+v", resp)
	}
}

// The refusal text names only tools the agent has, because the policy's list included one it
// does not — the failure a live run produced.
func TestRefusalAlternativesAreFilteredToTheCatalogue(t *testing.T) {
	b := testBroker(t, `{"version": 1, "rules": [
  {"id": "read", "tool": "FileRead", "action": "allow"},
  {"id": "write", "tool": "FileWrite", "action": "allow"},
  {"id": "shell", "tool": "Execute", "action": "deny", "reason": "no shell"}
]}`, twoAgentsOnOneLogin())
	if resp := b.serve(Request{Op: "register", Agent: "vagent-analyst", Catalog: map[string]string{"FileRead": "pure", "Execute": "irreversible"}}, 501); !resp.OK {
		t.Fatalf("register: %+v", resp)
	}
	resp := decide(t, b, 501, "vagent-analyst", "Execute")
	if resp.Decision == nil || resp.Decision.Denial == nil {
		t.Fatalf("no denial class: %+v", resp.Decision)
	}
	if got := strings.Join(resp.Decision.Denial.Permitted, ","); got != "FileRead" {
		t.Fatalf("permitted = %q, want only FileRead: the agent has no FileWrite", got)
	}
}

// The stop path: a listener that closes drains what is running and returns, so a supervisor's
// SIGTERM cannot cut a decision in half after its row is already in the ledger.
func TestServeLoopDrainsOnClose(t *testing.T) {
	// The transport attests the uid the test runs as, so the grant has to name it: a synthetic
	// 501 passes on this machine and fails on any CI runner that is not uid 501.
	me := uint32(os.Getuid())
	b := testBroker(t, allowRead, principal.Grants{int(me): {"vagent-analyst", "vagent-worker"}})
	dir, err := os.MkdirTemp("", "gaapd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "g.sock")
	ln, err := listen(sock, 0o600)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	done := make(chan struct{})
	go func() {
		b.serveLoop(ln)
		close(done)
	}()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := json.NewEncoder(conn).Encode(Request{Op: "register", Agent: "vagent-analyst", Catalog: map[string]string{"FileRead": "pure"}}); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = conn.Close()
	if !resp.OK {
		t.Fatalf("register: %+v", resp)
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveLoop did not return after the listener closed")
	}
	// Go's unix listener unlinks the socket when it closes, so a clean stop leaves no file
	// behind — the stale-socket branch in listen() only covers a process that never got to
	// close anything.
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket still present after a clean close: %v", err)
	}
}

// A broken configuration must be distinguishable from a transient fault, or a supervisor
// restarts it forever and the real message is buried. EX_CONFIG is that distinction.
func TestBrokenConfigExitsWithEXCONFIG(t *testing.T) {
	dir := t.TempDir()
	// A policy file that cannot parse: present, so the failure is not "missing".
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	grants := `{"principals": [{"uid": 501, "agents": ["a"]}]}`
	if err := os.WriteFile(filepath.Join(dir, "memory-grants.json"), []byte(grants), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"-state", dir, "-socket", filepath.Join(dir, "s.sock")}); code != exitConfig {
		t.Fatalf("exit code = %d, want %d (EX_CONFIG)", code, exitConfig)
	}

	// No grant file at all is the same class: the broker cannot tell one agent from another.
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, "policy.json"), []byte(allowRead), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"-state", empty, "-socket", filepath.Join(empty, "s.sock")}); code != exitConfig {
		t.Fatalf("missing grants: exit code = %d, want %d", code, exitConfig)
	}
}

func TestBadFlagIsAUsageError(t *testing.T) {
	if code := run([]string{"-nope"}); code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if code := run([]string{"-h"}); code != exitOK {
		t.Fatalf("-h exit code = %d, want 0", code)
	}
}

// The broker's rules must not live where the workloads it judges can write them. /var/lib/gaap
// is the container runtime's data root, so a state directory inside it is a policy file the
// governed party owns.
func TestStateDirectoryInsideTheRuntimeRootIsRefused(t *testing.T) {
	for _, dir := range []string{"/var/lib/gaap", "/var/lib/gaap/state", "/var/lib/gaap/ws/agent"} {
		if code := run([]string{"-state", dir, "-socket", filepath.Join(os.TempDir(), "x.sock")}); code != exitConfig {
			t.Fatalf("%s: exit code = %d, want EX_CONFIG", dir, code)
		}
	}
	if same, err := isWithin("/var/lib/gaapd", "/var/lib/gaap"); err != nil || same {
		t.Fatalf("a sibling directory was judged inside the runtime root: %v %v", same, err)
	}
}

// The class is the caller's claim about its own tools, and it is what makes the ledger's
// reversibility mean something over the socket: without it every row says "irreversible"
// because the kernel cannot see the workload's tool surface.
func TestTheLedgerRecordsTheClassTheAgentClaimed(t *testing.T) {
	b := testBroker(t, `{"version": 1, "rules": [
  {"id": "read", "tool": "FileRead", "action": "allow"},
  {"id": "exec", "tool": "Execute", "action": "allow"}
]}`, twoAgentsOnOneLogin())
	if r := b.serve(Request{Op: "register", Agent: "vagent-analyst",
		Catalog: map[string]string{"FileRead": "pure", "Execute": "irreversible"}}, 501); !r.OK {
		t.Fatalf("register: %+v", r)
	}

	for _, tc := range []struct {
		tool string
		want platform.Reversibility
	}{
		{"FileRead", platform.Pure},
		{"Execute", platform.Irreversible},
	} {
		decide(t, b, 501, "vagent-analyst", tc.tool)
		entries := b.ledger.Entries()
		last := entries[len(entries)-1]
		if last.Tool != tc.tool || last.Reversibility != tc.want {
			t.Fatalf("%s recorded as %q, want %q", tc.tool, last.Reversibility, tc.want)
		}
	}
}

// One tool name, two classes, is two answers to "can this be undone". Whichever agent
// registered last would otherwise decide what the ledger claims about both.
func TestAClassConflictBetweenAgentsIsRefused(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	if r := b.serve(Request{Op: "register", Agent: "vagent-analyst",
		Catalog: map[string]string{"FileRead": "pure"}}, 501); !r.OK {
		t.Fatalf("first register: %+v", r)
	}
	r := b.serve(Request{Op: "register", Agent: "vagent-worker",
		Catalog: map[string]string{"FileRead": "irreversible"}}, 501)
	if r.OK {
		t.Fatal("the conflicting class was accepted")
	}
	for _, want := range []string{"vagent-analyst", "vagent-worker", "two classes"} {
		if !strings.Contains(r.Error, want) {
			t.Fatalf("error %q does not name %q", r.Error, want)
		}
	}
	// The first claim still stands: a refused second claim must not silently replace it.
	if got := b.classifyFor("FileRead"); got != platform.Pure {
		t.Fatalf("class after the conflict = %s, want the original pure", got)
	}
}

// Reversible and compensable are reserved: no snapshot or compensation mechanism exists, so a
// workload claiming one would be writing an inverse the runtime cannot back into the audit trail.
func TestReservedClassesAreRefused(t *testing.T) {
	b := testBroker(t, allowRead, twoAgentsOnOneLogin())
	for _, class := range []string{"reversible", "compensable"} {
		r := b.serve(Request{Op: "register", Agent: "vagent-analyst",
			Catalog: map[string]string{"FileWrite": class}}, 501)
		if r.OK {
			t.Fatalf("%s was accepted", class)
		}
		if !strings.Contains(r.Error, "reserved") {
			t.Fatalf("error for %s = %q", class, r.Error)
		}
	}
	r := b.serve(Request{Op: "register", Agent: "vagent-analyst",
		Catalog: map[string]string{"FileWrite": "harmless"}}, 501)
	if r.OK {
		t.Fatal("an invented class was accepted")
	}
}

// A gate ticket over the socket: the caller asks, the broker mints from the key beside
// kill.json, and the token only covers the observation and resource it named.
func TestATicketIsMintedOverTheSocket(t *testing.T) {
	b := testBroker(t, `{"version": 1, "rules": [
  {"id": "read", "tool": "FileRead", "action": "allow"},
  {"id": "exec", "tool": "Execute", "action": "allow"}
]}`, twoAgentsOnOneLogin())
	if r := b.serve(Request{Op: "register", Agent: "vagent-analyst",
		Catalog: map[string]string{"FileRead": "pure", "Execute": "irreversible"}}, 501); !r.OK {
		t.Fatalf("register: %+v", r)
	}

	resp := b.serve(Request{Op: "decide", Agent: "vagent-analyst", Tool: "FileRead",
		Args:   map[string]any{"path": "/etc/hosts"},
		Ticket: &TicketAsk{EntryID: "obs-1", Resource: "memory://obs-1"}}, 501)
	if resp.Decision == nil || resp.Decision.Action != platform.ActionAllow {
		t.Fatalf("decide: %+v", resp.Decision)
	}
	if resp.Decision.Ticket == "" {
		t.Fatal("no ticket in the response")
	}
	key, err := platform.LoadTicketKey(b.ticketPath)
	if err != nil {
		t.Fatalf("ticket key: %v", err)
	}
	if _, err := key.Verify(resp.Decision.Ticket, "obs-1", "vagent-analyst", "memory://obs-1"); err != nil {
		t.Fatalf("the minted ticket does not verify: %v", err)
	}
	// Bound to what it named: a different observation is not covered.
	if _, err := key.Verify(resp.Decision.Ticket, "obs-2", "vagent-analyst", "memory://obs-1"); err == nil {
		t.Fatal("the ticket verified for an observation it did not name")
	}

	// Only a pure read leaves an observation to attest.
	exec := b.serve(Request{Op: "decide", Agent: "vagent-analyst", Tool: "Execute",
		Args:   map[string]any{},
		Ticket: &TicketAsk{EntryID: "obs-3", Resource: "memory://obs-3"}}, 501)
	if exec.Decision == nil || exec.Decision.Action != platform.ActionDeny {
		t.Fatalf("a ticket for an irreversible tool was served: %+v", exec.Decision)
	}
	if !strings.Contains(exec.Decision.Reason, "pure") {
		t.Fatalf("reason = %q", exec.Decision.Reason)
	}

	// An unnamed resource is refused rather than attested for anything.
	r := b.serve(Request{Op: "decide", Agent: "vagent-analyst", Tool: "FileRead",
		Args: map[string]any{}, Ticket: &TicketAsk{EntryID: "obs-4"}}, 501)
	if r.OK {
		t.Fatal("a ticket with no resource was served")
	}
}

// testBrokerWithGrants is testBroker with a hand-written grant file, because the memory
// capabilities live in that file and the principal helper only writes principals.
func testBrokerWithGrants(t *testing.T, policy, grantsJSON string) *broker {
	t.Helper()
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	grantsPath := filepath.Join(dir, "memory-grants.json")
	if err := os.WriteFile(grantsPath, []byte(grantsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := newBroker(config{
		StateDir:   filepath.Join(dir, "state"),
		PolicyPath: policyPath,
		GrantsPath: grantsPath,
	})
	if err != nil {
		t.Fatalf("newBroker: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

const memGrants = `{
  "version": 2,
  "principals": [{"uid": 501, "agents": ["vagent-analyst", "vagent-worker"]}],
  "grants": [
    {"id": "read-analyst", "capability": "read", "scope": "vagent-analyst"},
    {"id": "write-analyst", "capability": "write", "scope": "vagent-analyst", "max_ttl_seconds": 600},
    {"id": "inject-analyst", "capability": "injectable", "scope": "vagent-analyst"}
  ]
}`

// The memory store used to be a second daemon with its own socket and its own identity rule.
// It is now an op on this one, and the grant file is still what opens it.
func TestMemoryOpsAreOnTheOneSocket(t *testing.T) {
	b := testBrokerWithGrants(t, allowRead, memGrants)

	entry := memstore.Entry{ID: "m1", Kind: "note", Body: "the analyst saw a thing", SourceRef: "src-1", ExpiresAt: time.Now().Add(10 * time.Minute)}
	if r := b.serve(Request{Op: "write", Agent: "vagent-analyst", Entry: &entry}, 501); !r.OK {
		t.Fatalf("write: %+v", r)
	}
	r := b.serve(Request{Op: "read", Agent: "vagent-analyst", ID: "m1"}, 501)
	if !r.OK || len(r.Entries) != 1 || r.Entries[0].ID != "m1" {
		t.Fatalf("read: %+v", r)
	}
	if r.MemRev == "" {
		t.Fatal("a memory answer did not carry the grant revision it was served under")
	}
}

// A peer holding several agents must still say which, for memory ops too: the same rule that
// keeps a decision from being attributed to the wrong workload keeps a write from being.
func TestMemoryOpsNeedTheSameIdentityClaim(t *testing.T) {
	b := testBrokerWithGrants(t, allowRead, memGrants)
	r := b.serve(Request{Op: "read", ID: "m1"}, 501)
	if r.OK || !strings.Contains(r.Error, "may act as") {
		t.Fatalf("an unclaimed multi-agent peer was served memory: %+v", r)
	}
}

// The grant file is the door: an agent with no write grant cannot write, on this socket or any.
func TestMemoryFollowsTheGrantNotTheOp(t *testing.T) {
	b := testBrokerWithGrants(t, allowRead, memGrants)
	entry := memstore.Entry{ID: "m2", Kind: "note", Body: "worker tries to write", ExpiresAt: time.Now().Add(10 * time.Minute)}
	r := b.serve(Request{Op: "write", Agent: "vagent-worker", Entry: &entry}, 501)
	if r.OK {
		t.Fatal("vagent-worker has no write grant and wrote anyway")
	}
	if !strings.Contains(r.Error, "mem.write") || !strings.Contains(r.Error, "mem-deny-write") {
		t.Fatalf("refusal = %q, want the capability and the rule that refused it", r.Error)
	}
}

// The standalone broker cast the op name to a capability, so "search" asked for a capability
// called mem.search that no grant can name — the op existed and could never be allowed.
func TestSearchIsAReadOfTheStore(t *testing.T) {
	b := testBrokerWithGrants(t, allowRead, memGrants)
	entry := memstore.Entry{ID: "m3", Kind: "note", Body: "tide tables for the harbour", ExpiresAt: time.Now().Add(10 * time.Minute)}
	if r := b.serve(Request{Op: "write", Agent: "vagent-analyst", Entry: &entry}, 501); !r.OK {
		t.Fatalf("write: %+v", r)
	}
	r := b.serve(Request{Op: "search", Agent: "vagent-analyst", Query: "tide"}, 501)
	if !r.OK {
		t.Fatalf("search refused under a read grant: %+v", r)
	}
	if len(r.Entries) != 1 || r.Entries[0].ID != "m3" {
		t.Fatalf("search returned %+v", r.Entries)
	}
}
