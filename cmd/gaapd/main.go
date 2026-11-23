// Command gaapd is the decision broker: one policy, one ledger, one identity source, shared
// by every agent that asks it what it may do.
//
// It exists because the alternative is worse. When each agent builds its own governor, the
// policy it reads and the ledger it writes live in the process running the tool, so N agents
// means N policies, N ledgers that cannot be merged into one order, and a subject the caller
// writes about itself. Moving the decision out of the workload is the whole feature; the
// transport is the least interesting part of it.
//
// The identity comes from the socket (uid, via peer credentials) and the claim in the request
// is only an assertion checked against it. What stays with the caller is what only the caller
// knows: the provenance floor of its own justification.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aj-nt/gaap/internal/peercred"
	"github.com/aj-nt/gaap/internal/principal"
	"github.com/aj-nt/gaap/platform"
)

// Request is one question. Ops are refused by name rather than ignored, because a broker that
// answers an unknown op with silence is indistinguishable from one that crashed.
type Request struct {
	Op string `json:"op"`
	// Agent is an assertion, not an identity: the broker uses it only to check that the peer
	// is not claiming to be someone else.
	Agent string         `json:"agent,omitempty"`
	Tool  string         `json:"tool,omitempty"`
	Args  map[string]any `json:"args,omitempty"`
	// Floor is what the caller knows about its own justification and the broker cannot see.
	Floor string `json:"floor,omitempty"`
}

type Response struct {
	OK       bool               `json:"ok"`
	Error    string             `json:"error,omitempty"`
	Decision *platform.Decision `json:"decision,omitempty"`
	// Revision names the policy that produced this decision. A caller that logs only the
	// verdict cannot tell "the rule I wrote refuses this" from "the rule I replaced an hour
	// ago refuses this".
	Revision string `json:"revision,omitempty"`
}

// grantsFile is the principals half of the memory grant file, so one file configures identity
// for both brokers. A second file would be a second answer to "who is uid 501".
type grantsFile struct {
	Principals []struct {
		UID   int    `json:"uid"`
		Agent string `json:"agent"`
	} `json:"principals"`
}

type config struct {
	Socket     string
	StateDir   string
	PolicyPath string
	GrantsPath string
	TrustFloor float64
}

// policyStamp is what a reload is keyed on: content length plus mtime. Cheap to read once per
// decision, and it needs no watcher thread whose missed event would leave the fleet on a
// policy nobody meant to keep.
type policyStamp struct {
	size int64
	mod  time.Time
}

type broker struct {
	// mu serialises decisions. The kernel's Trust manager and denial counter are per-agent
	// state, and a broker is the wrong place to discover that one of them was not written
	// for concurrent callers; a fleet of a few agents cannot tell the difference, and this
	// is measurable later if it matters.
	mu         sync.Mutex
	gov        *platform.Governor
	ledger     *platform.Ledger
	principals map[int]string
	policyPath string
	stamp      policyStamp
	stateDir   string
}

func newBroker(cfg config) (*broker, error) {
	if cfg.PolicyPath == "" {
		return nil, fmt.Errorf("gaapd needs a policy file")
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("gaapd needs a state directory")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	policy, err := platform.LoadPolicyFile(cfg.PolicyPath)
	if err != nil {
		return nil, err
	}
	ledger, err := platform.NewLedgerAt(filepath.Join(cfg.StateDir, "ledger.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	kill, err := platform.OpenFileKillSwitch(filepath.Join(cfg.StateDir, "kill.json"))
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("open kill switch: %w", err)
	}
	denials, err := platform.OpenDenialCounter(filepath.Join(cfg.StateDir, "denials.json"),
		platform.DefaultDenialWindow, platform.DefaultDenialThreshold)
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("denial counter: %w", err)
	}
	principals, err := loadPrincipals(cfg.GrantsPath)
	if err != nil {
		_ = ledger.Close()
		return nil, err
	}

	gov := platform.NewGovernor()
	gov.Policy = policy
	gov.Ledger = ledger
	gov.Kill = kill
	gov.Denials = denials
	gov.TrustFloor = cfg.TrustFloor

	b := &broker{
		gov:        gov,
		ledger:     ledger,
		principals: principals,
		policyPath: cfg.PolicyPath,
		stateDir:   cfg.StateDir,
	}
	if fi, err := os.Stat(cfg.PolicyPath); err == nil {
		b.stamp = policyStamp{size: fi.Size(), mod: fi.ModTime()}
	}
	return b, nil
}

func (b *broker) Close() error { return b.ledger.Close() }

func loadPrincipals(path string) (map[int]string, error) {
	if path == "" {
		return nil, fmt.Errorf("gaapd needs a grants file: a broker with no principals cannot tell one agent from another")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f grantsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("gaapd: parse %s: %w", path, err)
	}
	out := make(map[int]string, len(f.Principals))
	for _, p := range f.Principals {
		if p.Agent == "" {
			return nil, fmt.Errorf("gaapd: %s maps uid %d to no agent", path, p.UID)
		}
		out[p.UID] = p.Agent
	}
	return out, nil
}

// serve answers one request from a peer whose uid the transport established.
func (b *broker) serve(req Request, uid uint32) Response {
	if req.Op != "decide" {
		return Response{Error: fmt.Sprintf("unknown op %q (want \"decide\")", req.Op)}
	}
	if req.Tool == "" {
		return Response{Error: "decide requires tool"}
	}
	agent, err := principal.Resolve(b.principals, uid, req.Agent)
	if err != nil {
		var unmapped principal.ErrUnmapped
		if isUnmapped(err, &unmapped) {
			log.Printf("gaapd: refused uid %d: %v", uid, err)
		} else {
			// The second kind is an attempt, and the log has to say so: a mapped peer
			// naming another agent is not a misconfiguration.
			log.Printf("gaapd: IMPERSONATION uid %d: %v", uid, err)
		}
		return Response{Error: err.Error()}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.reloadLocked()

	var opts []platform.DecideOpt
	if req.Floor != "" {
		class := platform.TaintClass(req.Floor)
		if !class.Known() {
			return Response{Error: fmt.Sprintf("floor %q is not a provenance class", req.Floor)}
		}
		opts = append(opts, platform.WithFloor(class))
	}
	d := b.gov.Decide(agent, req.Tool, req.Args, opts...)
	return Response{OK: true, Decision: &d, Revision: b.gov.Policy.Revision()}
}

// reloadLocked swaps in a new policy when the file changed, and keeps the last good one when
// it did not parse. An operator typo must not stop the fleet, and it must not silently install
// a policy either: the revision in every response is the receipt either way.
func (b *broker) reloadLocked() {
	fi, err := os.Stat(b.policyPath)
	if err != nil {
		return
	}
	stamp := policyStamp{size: fi.Size(), mod: fi.ModTime()}
	if stamp == b.stamp {
		return
	}
	engine, err := platform.LoadPolicyFile(b.policyPath)
	if err != nil {
		log.Printf("gaapd: %s did not parse (%v); still serving revision %s",
			b.policyPath, err, b.gov.Policy.Revision())
		return
	}
	b.gov.Policy = engine
	b.stamp = stamp
	log.Printf("gaapd: reloaded %s, revision %s", b.policyPath, engine.Revision())
}

func isUnmapped(err error, target *principal.ErrUnmapped) bool {
	if e, ok := err.(principal.ErrUnmapped); ok {
		*target = e
		return true
	}
	return false
}

func listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// A socket left behind by a crashed broker would make bind fail forever; it is a file in
	// a directory only the broker writes to.
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
	resp := b.serve(req, uid)
	if verbose {
		outcome := "refused"
		if resp.OK && resp.Decision != nil {
			outcome = string(resp.Decision.Action)
		}
		log.Printf("gaapd: uid=%d agent=%s tool=%s -> %s", uid, b.principals[int(uid)], req.Tool, outcome)
	}
	b.reply(conn, resp)
}

func (b *broker) reply(conn net.Conn, r Response) {
	if err := json.NewEncoder(conn).Encode(r); err != nil {
		log.Printf("gaapd: reply: %v", err)
	}
}

func main() {
	var (
		socketPath = flag.String("socket", "", "unix socket (default <state>/govern.sock)")
		stateDir   = flag.String("state", "/var/lib/gaap", "state directory: ledger, kill switch, denials")
		policyPath = flag.String("policy", "", "policy file (default <state>/policy.json)")
		grantsPath = flag.String("grants", "", "grant file with principals (default <state>/memory-grants.json)")
		trustFloor = flag.Float64("trust-floor", 0, "escalate allows to review below this trust score")
		verbose    = flag.Bool("v", false, "log one line per decision")
	)
	flag.Parse()

	cfg := config{
		Socket:     *socketPath,
		StateDir:   *stateDir,
		PolicyPath: *policyPath,
		GrantsPath: *grantsPath,
		TrustFloor: *trustFloor,
	}
	if cfg.Socket == "" {
		cfg.Socket = filepath.Join(cfg.StateDir, "govern.sock")
	}
	if cfg.PolicyPath == "" {
		cfg.PolicyPath = filepath.Join(cfg.StateDir, "policy.json")
	}
	if cfg.GrantsPath == "" {
		cfg.GrantsPath = filepath.Join(cfg.StateDir, "memory-grants.json")
	}

	b, err := newBroker(cfg)
	if err != nil {
		log.Fatalf("gaapd: %v", err)
	}
	defer func() { _ = b.Close() }()

	ln, err := listen(cfg.Socket)
	if err != nil {
		log.Fatalf("gaapd: listen %s: %v", cfg.Socket, err)
	}
	defer func() { _ = ln.Close() }()

	log.Printf("gaapd: serving %s (policy %s, revision %s, %d principals)",
		cfg.Socket, cfg.PolicyPath, b.gov.Policy.Revision(), len(b.principals))

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("gaapd: accept: %v", err)
			continue
		}
		go b.handle(conn, *verbose)
	}
}
