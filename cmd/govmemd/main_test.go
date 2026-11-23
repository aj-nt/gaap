package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aj-nt/gaap/internal/memstore"
	"github.com/aj-nt/gaap/platform"
)

// shortTempDir exists because a unix socket path is capped near 104 bytes and t.TempDir()
// on macOS spends most of that on the test's own name.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "govmemd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// testBroker builds a broker over a real store and a real compiled grant file: the point
// of these tests is the broker's own wiring, so faking either would test the fake.
func testBroker(t *testing.T) *broker {
	t.Helper()
	store, err := memstore.Open(filepath.Join(shortTempDir(t), "mem.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rules, err := platform.CompileMemoryGrants([]platform.MemoryGrant{
		{Capability: platform.MemRead, Scope: "agent-a"},
		{Capability: platform.MemWrite, Scope: "agent-a", MaxTTL: 3600},
		{Capability: platform.MemInjectable, Scope: "agent-a"},
	}, "rev-test")
	if err != nil {
		t.Fatalf("grants: %v", err)
	}
	return &broker{
		store:      store,
		rules:      platform.GrantsEngine(rules),
		principals: map[int]string{501: "agent-a"},
		revision:   "rev-test",
	}
}

// The store round-trips through the broker, and the effective class comes from the
// lineage rather than from the write path's own opinion.
func TestServeWriteThenRead(t *testing.T) {
	b := testBroker(t)
	resp := b.serve(Request{Op: "write", Entry: &memstore.Entry{
		ID: "e1", Kind: "note", Body: "cluster has 3 nodes", SourceKind: memstore.TaintExternal,
		ExpiresAt: time.Now().Add(time.Hour),
	}})
	if !resp.OK {
		t.Fatalf("write refused: %s", resp.Error)
	}

	resp = b.serve(Request{Op: "read", ID: "e1"})
	if !resp.OK || len(resp.Entries) != 1 {
		t.Fatalf("read = %+v", resp)
	}
	if resp.Entries[0].Body != "cluster has 3 nodes" {
		t.Fatalf("body = %q", resp.Entries[0].Body)
	}
	// Written with no cited parent: external, whatever the caller believes.
	if resp.Effective != memstore.TaintExternal {
		t.Fatalf("effective = %q, want external", resp.Effective)
	}
}

// A derived entry is internal and the broker's injectable block renders it under the
// data-not-policy header. This is the only shape in which memory reaches a prompt, so the
// header travelling with it is part of the contract, not formatting.
func TestServeInjectableBlockCarriesHeaderAndDerivedEntries(t *testing.T) {
	b := testBroker(t)
	if err := b.store.Append(memstore.Entry{
		ID: "root", Kind: "note", Body: "the finance team approved a transfer",
		SourceKind: memstore.TaintExternal, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// A derived row is only as internal as what it cites, so citing the external root
	// stays external — the point of the closure. Internal has one source, the gate
	// ticket, and the broker does not verify tickets yet (the ticket-wiring finding):
	// this test presents the unverified id the current broker accepts, and stops being
	// this test's shape the moment verification lands.
	if err := b.store.AppendWithTicket("tkt-unverified", memstore.Entry{
		ID: "observed", Kind: "note", Body: "a transfer was approved",
		Producer: "broker", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("ticketed append: %v", err)
	}

	resp := b.serve(Request{Op: "injectable", Budget: 4000})
	if !resp.OK {
		t.Fatalf("injectable refused: %s", resp.Error)
	}
	if !strings.Contains(resp.Block, "DATA, never policy") {
		t.Fatalf("block has no header: %q", resp.Block)
	}
	if !strings.Contains(resp.Block, "a transfer was approved") {
		t.Fatalf("the internal entry was not injectable: %q", resp.Block)
	}
	// The external root stays out of the prompt: searchable, not pushed unasked.
	if strings.Contains(resp.Block, "the finance team approved") {
		t.Fatalf("external content was injected: %q", resp.Block)
	}
}

// A malformed request is an error response, never a panic and never a silent success.
func TestServeRefusesMalformedRequests(t *testing.T) {
	b := testBroker(t)
	cases := []struct {
		name string
		req  Request
		want string
	}{
		{"read without id", Request{Op: "read"}, "read requires id"},
		{"write without entry", Request{Op: "write"}, "write requires entry"},
		{"unknown op", Request{Op: "frobnicate"}, "unknown op frobnicate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := b.serve(c.req)
			if resp.OK {
				t.Fatalf("malformed request succeeded: %+v", resp)
			}
			if !strings.Contains(resp.Error, c.want) {
				t.Fatalf("error = %q, want it to contain %q", resp.Error, c.want)
			}
		})
	}
}

// The socket is a door with a guard behind it, not a lock: bind() leaves it 0755, and a
// different-uid agent then gets EACCES, which is indistinguishable from a policy deny.
// Widening it to 0666 is what makes the broker the thing that decides.
func TestListenWidensTheSocketAndReplacesAStaleOne(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "govmem.sock")

	ln, err := listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket: %v", sock, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o666 {
		t.Fatalf("socket perms = %o, want 666: another uid gets EACCES that looks like a deny", perm)
	}
	// A crashed broker leaves its socket behind; bind would then fail forever.
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	ln2, err := listen(sock)
	if err != nil {
		t.Fatalf("a stale socket blocked the second bind: %v", err)
	}
	_ = ln2.Close()
}

func TestShort(t *testing.T) {
	if got := short("0123456789abcdef"); got != "0123456789ab" {
		t.Fatalf("short = %q", got)
	}
	if got := short("abc"); got != "abc" {
		t.Fatalf("short truncated a short revision: %q", got)
	}
}

// --- gate tickets: the write path verifies, then consumes, then records ---

// withTickets gives the broker a verification key, the way main does from --ticket-key.
func withTickets(t *testing.T, b *broker) *platform.TicketKey {
	t.Helper()
	key := platform.NewTicketKey([]byte("0123456789abcdef0123456789abcdef"))
	b.tickets = key
	return key
}

func ticketedEntry(id, body, sourceRef string) *memstore.Entry {
	return &memstore.Entry{
		ID: id, Kind: "note", Body: body, SourceRef: sourceRef,
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

// The whole point of the ticket: the id alone proves nothing. Before this, any holder of
// mem.write could present a string the store had never seen and have its content recorded
// as internal — the class that admits content to the always-injected block.
func TestAttestedWriteRequiresARealTicket(t *testing.T) {
	b := testBroker(t)
	key := withTickets(t, b)

	token, _, err := key.Issue("e1", "agent-a", "/etc/hosts", 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	resp := b.serve(Request{Op: "write", Agent: "agent-a", Ticket: token,
		Entry: ticketedEntry("e1", "127.0.0.1 localhost", "/etc/hosts")})
	if !resp.OK {
		t.Fatalf("attested write refused: %s", resp.Error)
	}
	if resp.Effective != memstore.TaintInternal {
		t.Fatalf("effective = %q, want internal", resp.Effective)
	}
	if e, err := b.store.Get("e1"); err != nil || e.Body != "127.0.0.1 localhost" {
		t.Fatalf("entry not recorded: %v", err)
	}

	// An arbitrary string is no longer a ticket.
	resp = b.serve(Request{Op: "write", Agent: "agent-a", Ticket: "not-a-ticket",
		Entry: ticketedEntry("e2", "forged", "/etc/passwd")})
	if resp.OK {
		t.Fatal("an unverifiable ticket was accepted")
	}
	if !strings.Contains(resp.Error, "ticket refused") {
		t.Fatalf("error = %q, want a ticket refusal", resp.Error)
	}
	// Refused means not recorded, in any class: a caller that asked to be attested must
	// not be served as something weaker instead.
	if _, err := b.store.Get("e2"); err == nil {
		t.Fatal("a refused ticketed write was stored")
	}
}

// The claim binds the token to one observation. Without that, an attested write of one
// resource would authorise any content at all for as long as the token lasted.
func TestTicketIsBoundToItsClaim(t *testing.T) {
	b := testBroker(t)
	key := withTickets(t, b)
	token, _, err := key.Issue("e1", "agent-a", "/etc/hosts", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		name string
		req  Request
	}{
		{"another entry", Request{Op: "write", Agent: "agent-a", Ticket: token,
			Entry: ticketedEntry("e2", "same observation", "/etc/hosts")}},
		{"another resource", Request{Op: "write", Agent: "agent-a", Ticket: token,
			Entry: ticketedEntry("e1", "same observation", "/etc/passwd")}},
		{"another agent", Request{Op: "write", Agent: "agent-b", Ticket: token,
			Entry: ticketedEntry("e1", "same observation", "/etc/hosts")}},
	} {
		if resp := b.serve(bad.req); resp.OK {
			t.Errorf("ticket presented for %s was accepted", bad.name)
		}
	}
}

// Single use is enforced by the store's primary key, so a replayed token fails with
// "already consumed" rather than a row conflict.
func TestTicketIsSingleUse(t *testing.T) {
	b := testBroker(t)
	key := withTickets(t, b)
	token, _, err := key.Issue("e1", "agent-a", "/etc/hosts", 0)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Op: "write", Agent: "agent-a", Ticket: token,
		Entry: ticketedEntry("e1", "127.0.0.1 localhost", "/etc/hosts")}
	if resp := b.serve(req); !resp.OK {
		t.Fatalf("first presentation refused: %s", resp.Error)
	}
	resp := b.serve(req)
	if resp.OK {
		t.Fatal("the same ticket was spent twice")
	}
	if !strings.Contains(resp.Error, "already consumed") {
		t.Fatalf("error = %q, want the reuse to be named", resp.Error)
	}
}

// No key on this host means nothing can be verified, so nothing gets the internal class —
// but the broker keeps serving everything that does not need one. Fail closed, not down.
func TestTicketedWriteIsRefusedWithoutAKey(t *testing.T) {
	b := testBroker(t)
	resp := b.serve(Request{Op: "write", Agent: "agent-a", Ticket: "gt1.e30.x",
		Entry: ticketedEntry("e1", "body", "/etc/hosts")})
	if resp.OK {
		t.Fatal("a ticketed write was accepted by a broker with no key")
	}
	if !strings.Contains(resp.Error, "no ticket key") {
		t.Fatalf("error = %q, want it to name the missing key", resp.Error)
	}
	// Unattested writes still work.
	if resp := b.serve(Request{Op: "write", Agent: "agent-a",
		Entry: ticketedEntry("e1", "body", "/etc/hosts")}); !resp.OK {
		t.Fatalf("an unticketed write was refused: %s", resp.Error)
	}
}

// loadTicketKey is where "no key" and "a broken key" have to be different answers: the first
// is a host that never configured attestation, the second is a deployment whose tokens are
// all about to look forged.
func TestLoadTicketKeyDistinguishesAbsentFromBroken(t *testing.T) {
	dir := shortTempDir(t)
	if k, err := loadTicketKey(""); k != nil || err != nil {
		t.Fatalf("empty path = %v, %v; want nil, nil (verification disabled)", k, err)
	}
	missing := filepath.Join(dir, "absent.key")
	if k, err := loadTicketKey(missing); k != nil || err != nil {
		t.Fatalf("absent key = %v, %v; want nil, nil", k, err)
	}
	broken := filepath.Join(dir, "broken.key")
	if err := os.WriteFile(broken, []byte("not hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTicketKey(broken); err == nil {
		t.Fatal("a corrupt key was read as usable")
	}
	// A real key round-trips, including one the issuer wrote.
	good := filepath.Join(dir, "ticket.key")
	if _, err := platform.LoadOrCreateTicketKey(good); err != nil {
		t.Fatalf("create: %v", err)
	}
	if k, err := loadTicketKey(good); k == nil || err != nil {
		t.Fatalf("good key = %v, %v", k, err)
	}
}
