package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aj-nt/gaap/platform"
)

const allowRead = `{"version": 1, "rules": [
  {"id": "read", "tool": "ReadFile", "action": "allow"},
  {"id": "exec", "tool": "Execute", "action": "allow"}
]}`

const denyRead = `{"version": 1, "rules": [
  {"id": "read", "tool": "ReadFile", "action": "deny", "reason": "the fleet was told to stop reading"}
]}`

func testBroker(t *testing.T, policy string) *broker {
	t.Helper()
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	// Two synthetic agents, plus whatever uid the test itself runs as so the transport test
	// can exercise the peer-credential path without depending on the CI user id.
	principals := []map[string]any{{"uid": 501, "agent": "agent-a"}, {"uid": 502, "agent": "agent-b"}}
	if u := os.Getuid(); u != 501 && u != 502 {
		principals = append(principals, map[string]any{"uid": u, "agent": "agent-self"})
	}
	raw, err := json.Marshal(map[string]any{"principals": principals})
	if err != nil {
		t.Fatal(err)
	}
	grants := string(raw)
	grantsPath := filepath.Join(dir, "memory-grants.json")
	if err := os.WriteFile(grantsPath, []byte(grants), 0o600); err != nil {
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

func decide(t *testing.T, b *broker, uid uint32, claimed, tool string) Response {
	t.Helper()
	return b.serve(Request{Op: "decide", Agent: claimed, Tool: tool, Args: map[string]any{}}, uid)
}

// The whole point of the daemon: one policy, N agents, and an edit that lands on the next
// call rather than at the next restart. Two identities, two allows, one file rewritten, two
// denials — with nothing restarted between them.
func TestOnePolicyFlipStopsEveryAgent(t *testing.T) {
	b := testBroker(t, allowRead)
	for _, c := range []struct {
		uid  uint32
		name string
	}{{501, "agent-a"}, {502, "agent-b"}} {
		resp := decide(t, b, c.uid, c.name, "ReadFile")
		if !resp.OK || resp.Decision == nil || resp.Decision.Action != platform.ActionAllow {
			t.Fatalf("%s: %+v, want an allow", c.name, resp)
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

	for _, c := range []struct {
		uid  uint32
		name string
	}{{501, "agent-a"}, {502, "agent-b"}} {
		resp := decide(t, b, c.uid, c.name, "ReadFile")
		if !resp.OK || resp.Decision == nil {
			t.Fatalf("%s: %+v", c.name, resp)
		}
		if resp.Decision.Action != platform.ActionDeny {
			t.Fatalf("%s still allowed after the flip: %+v", c.name, resp.Decision)
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
}

// Identity comes from the socket, not from the request. An agent that names itself is
// asserting, not identifying — and an assertion is only worth the peer credentials behind it.
func TestClaimedIdentityMustMatchThePeer(t *testing.T) {
	b := testBroker(t, allowRead)
	resp := b.serve(Request{Op: "decide", Agent: "agent-b", Tool: "ReadFile"}, 501)
	if resp.OK {
		t.Fatal("uid mapped to agent-a decided as agent-b")
	}
	if !strings.Contains(resp.Error, "not authorised to act as") {
		t.Fatalf("error = %q, want it to name the claim", resp.Error)
	}
	if n := len(b.ledger.Entries()); n != 0 {
		t.Fatalf("an unauthenticated claim produced %d ledger entries", n)
	}
}

func TestUnmappedPeerIsRefused(t *testing.T) {
	b := testBroker(t, allowRead)
	resp := b.serve(Request{Op: "decide", Agent: "agent-a", Tool: "ReadFile"}, 999)
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
	b := testBroker(t, allowRead)
	first := decide(t, b, 501, "agent-a", "ReadFile")
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

	second := decide(t, b, 501, "agent-a", "ReadFile")
	if !second.OK || second.Decision == nil || second.Decision.Action != platform.ActionAllow {
		t.Fatalf("a broken policy file changed a decision: %+v", second)
	}
	if second.Revision != first.Revision {
		t.Fatalf("revision moved to %q on a policy that failed to parse", second.Revision)
	}
}

// The response has to be usable by a client that was not compiled against these structs.
func TestDecisionSurvivesTheWire(t *testing.T) {
	b := testBroker(t, allowRead)
	resp := decide(t, b, 501, "agent-a", "ReadFile")
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

// The transport, end to end: a real unix socket, a real peer uid, no claim in the request. The
// daemon tests exercise the rule; this exercises the thing the client will actually talk to.
func TestSocketPeerIdentity(t *testing.T) {
	b := testBroker(t, allowRead)
	dir, err := os.MkdirTemp("", "gaapd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "g.sock")
	ln, err := listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		b.handle(conn, false)
	}()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := json.NewEncoder(conn).Encode(Request{Op: "decide", Tool: "ReadFile"}); err != nil {
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
