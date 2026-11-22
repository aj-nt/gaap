package main

import (
	"errors"
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

// resolveIdentity is the branch that decides who the store is opened for. It was three
// statements inside a unix-socket handler, reachable only by a real peer uid — which is
// why it had no test. Separating the rule from the connection is the whole reason it is a
// function now.
func TestResolveIdentity(t *testing.T) {
	principals := map[int]string{501: "agent-a", 502: "agent-b"}

	// The uid is the identity; an absent claim is not a problem.
	agent, err := resolveIdentity(principals, 501, "")
	if err != nil || agent != "agent-a" {
		t.Fatalf("uid 501 = %q, %v; want agent-a", agent, err)
	}
	// A claim that agrees is allowed and changes nothing.
	if agent, err = resolveIdentity(principals, 501, "agent-a"); err != nil || agent != "agent-a" {
		t.Fatalf("uid 501 claiming agent-a = %q, %v", agent, err)
	}

	// No mapping: unidentified, not anonymous. Refused, and told apart from impersonation
	// because the operator log has to say which happened.
	_, err = resolveIdentity(principals, 999, "")
	if err == nil {
		t.Fatal("an unmapped uid was served")
	}
	var unmapped unmappedUID
	if !errors.As(err, &unmapped) {
		t.Fatalf("unmapped uid produced %T, want unmappedUID", err)
	}
	if !strings.Contains(err.Error(), "not mapped to an agent") {
		t.Fatalf("response text %q does not say what happened", err)
	}

	// A mapped uid claiming to be another agent is impersonation, not a typo.
	_, err = resolveIdentity(principals, 501, "agent-b")
	if err == nil {
		t.Fatal("uid 501 was allowed to act as agent-b")
	}
	if errors.As(err, &unmapped) {
		t.Fatalf("impersonation was reported as an unmapped uid: %v", err)
	}
	if !strings.Contains(err.Error(), "not authorised to act as agent-b") {
		t.Fatalf("response text %q does not name the claim", err)
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
