// Command govmemd is the governed-memory broker: the only way an agent reaches the
// store.
//
// The claim it exists to make true is narrow and checkable: an agent cannot reach the
// store except through this socket, and this socket knows who is talking to it. Both
// halves matter. A store the agent can open directly is a store the agent can edit
// around the broker, and a socket that trusts a claimed name is a broker with a form
// instead of an identity check.
//
// Identity is (uid, agent type). The transport attests the uid — SO_PEERCRED on Linux,
// LOCAL_PEERCRED on Darwin — and the grant file says which agent that uid may be. The
// agent type in the request is a claim, checked against that mapping; it is never the
// source of identity.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/aj-nt/gaap/internal/memstore"
	"github.com/aj-nt/gaap/internal/peercred"
	"github.com/aj-nt/gaap/platform"
)

// Request is one broker call. One request per connection, one reply, then close:
// a long-lived session would need its own liveness and revocation story, and the
// socket is local, so connection setup is cheap.
type Request struct {
	Op     string          `json:"op"` // read | write | resolve | tombstone | injectable | search
	Agent  string          `json:"agent"`
	Ticket string          `json:"ticket,omitempty"`
	Entry  *memstore.Entry `json:"entry,omitempty"`
	Query  string          `json:"query,omitempty"`
	ID     string          `json:"id,omitempty"`
	Budget int             `json:"budget,omitempty"`
}

// Response is the reply. Values are omitted when empty so a refusal carries no payload.
type Response struct {
	OK        bool             `json:"ok"`
	Error     string           `json:"error,omitempty"`
	Effective string           `json:"effective,omitempty"`
	Entries   []memstore.Entry `json:"entries,omitempty"`
	Block     string           `json:"block,omitempty"`
	Revision  string           `json:"revision,omitempty"`
}

type broker struct {
	store *memstore.Store
	rules *platform.PolicyEngine
	// principals maps uid -> authorised agent type.
	principals map[int]string
	revision   string
}

func main() {
	var (
		socketPath = flag.String("socket", "/run/govmem.sock", "unix socket to serve on")
		storePath  = flag.String("store", "/var/lib/gaap/mem.db", "sqlite store path (created 0600)")
		grantsPath = flag.String("grants", "/etc/gaap/memory-grants.json", "grant file with principals")
		verbose    = flag.Bool("v", false, "log every decision")
	)
	flag.Parse()

	rules, principals, rev, err := platform.LoadMemoryGrantsFull(*grantsPath)
	if err != nil {
		log.Fatalf("govmemd: grants: %v", err)
	}
	if len(principals) == 0 {
		// No principals means no caller can be identified, so the broker would serve
		// nobody. Starting anyway would look like a working service.
		log.Fatalf("govmemd: %s declares no principals; every caller would be unidentified", *grantsPath)
	}

	if err := os.MkdirAll(filepath.Dir(*storePath), 0o700); err != nil {
		log.Fatalf("govmemd: store dir: %v", err)
	}
	store, err := memstore.Open(*storePath)
	if err != nil {
		log.Fatalf("govmemd: store: %v", err)
	}
	defer func() { _ = store.Close() }()
	// The store is broker-only. 0600 owned by the broker's uid is the second half of
	// "the agent cannot reach the store": the socket is the only door.
	if err := os.Chmod(*storePath, 0o600); err != nil {
		log.Fatalf("govmemd: store perms: %v", err)
	}

	ln, err := listen(*socketPath)
	if err != nil {
		log.Fatalf("govmemd: listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	b := &broker{store: store, rules: platform.GrantsEngine(rules), principals: principals, revision: rev}
	log.Printf("govmemd: serving %s (revision %s, %d principals)", *socketPath, short(rev), len(principals))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		go b.handle(conn, *verbose)
	}
}

// listen binds the socket and widens it to 0666.
//
// bind() creates the socket 0755 under umask 022, owned by the broker. A different-uid
// agent then gets EACCES on connect — which is indistinguishable, from the agent's side,
// from a policy deny. That ambiguity is the problem: an agent that cannot tell "I am not
// allowed" from "I cannot reach it" will retry, reframe, or report a broken system
// instead of stopping. Permissions are the broker's to widen because the broker is what
// decides who is served: the socket is a door with a guard behind it, not a lock.
func listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// A stale socket from a crashed broker would make bind fail with "address already
	// in use" forever; it is a socket file in a directory only the broker controls.
	if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func (b *broker) handle(conn net.Conn, verbose bool) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	// Read the request BEFORE deciding. Replying to a peer that has not finished
	// sending resets the connection, and a client that sees ECONNRESET instead of a
	// refusal reports a broken broker rather than a denied one — the same ambiguity the
	// socket permissions exist to avoid.
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		b.reply(conn, Response{Error: "malformed request: " + err.Error()})
		return
	}

	uid, err := peercred.UID(conn)
	if err != nil {
		b.reply(conn, Response{Error: "cannot identify the peer: " + err.Error()})
		return
	}
	agent, ok := b.principals[int(uid)]
	if !ok {
		// Not "anonymous": unidentified. Refusing is the only safe answer, and the uid
		// goes in the log because that is the only handle an operator has.
		log.Printf("govmemd: refused uid %d: no principal mapping", uid)
		b.reply(conn, Response{Error: "uid " + strconv.Itoa(int(uid)) + " is not mapped to an agent"})
		return
	}
	// The agent field is a CLAIM. It must match what this uid is authorised to be; a
	// mismatch is an attempt to act as someone else, not a typo.
	if req.Agent != "" && req.Agent != agent {
		log.Printf("govmemd: uid %d (agent %s) claimed %s", uid, agent, req.Agent)
		b.reply(conn, Response{Error: "uid " + strconv.Itoa(int(uid)) + " is not authorised to act as " + req.Agent})
		return
	}
	req.Agent = agent

	tool := "mem." + req.Op
	d := b.rules.Evaluate(tool, map[string]any{"identity": agent})
	if d.Action != platform.ActionAllow {
		if verbose {
			log.Printf("govmemd: deny %s uid=%d agent=%s rule=%s", tool, uid, agent, d.RuleID)
		}
		b.reply(conn, Response{Error: fmt.Sprintf("%s refused for %s (rule %s)", tool, agent, d.RuleID)})
		return
	}

	resp := b.serve(req)
	if verbose {
		log.Printf("govmemd: %s uid=%d agent=%s ok=%v", tool, uid, agent, resp.OK)
	}
	b.reply(conn, resp)
}

func (b *broker) serve(req Request) Response {
	switch req.Op {
	case "read":
		if req.ID == "" {
			return Response{Error: "read requires id"}
		}
		e, err := b.store.Get(req.ID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		_, eff, err := b.store.EffectiveTaint(req.ID)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Entries: []memstore.Entry{e}, Effective: eff}

	case "search":
		entries, err := b.store.Search(req.Query)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Entries: entries}

	case "write":
		if req.Entry == nil {
			return Response{Error: "write requires entry"}
		}
		var err error
		if req.Ticket != "" {
			err = b.store.AppendWithTicket(req.Ticket, *req.Entry)
		} else {
			err = b.store.Append(*req.Entry)
		}
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}

	case "tombstone":
		if err := b.store.Tombstone(req.ID); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}

	case "resolve":
		if err := b.store.Resolve(req.ID, "confirmed", req.Agent, "via broker"); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}

	case "injectable":
		entries, err := b.store.Injectable(req.Budget)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, Block: memstore.RenderInjectable(entries)}

	default:
		return Response{Error: "unknown op " + req.Op}
	}
}

func (b *broker) reply(conn net.Conn, r Response) {
	r.Revision = b.revision
	_ = json.NewEncoder(conn).Encode(r)
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
