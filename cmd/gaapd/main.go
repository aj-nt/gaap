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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aj-nt/gaap/internal/peercred"
	"github.com/aj-nt/gaap/internal/principal"
	"github.com/aj-nt/gaap/platform"
)

// Request is one question. Ops are refused by name rather than ignored, because a broker that
// answers an unknown op with silence is indistinguishable from one that crashed.
// TicketAsk names the observation a caller wants attested and the resource it attests.
type TicketAsk struct {
	EntryID  string `json:"entry_id"`
	Resource string `json:"resource"`
}

type Request struct {
	Op string `json:"op"`
	// Agent is an assertion, not an identity: the broker uses it only to check that the peer
	// is not claiming to be someone else.
	Agent string         `json:"agent,omitempty"`
	Tool  string         `json:"tool,omitempty"`
	Args  map[string]any `json:"args,omitempty"`
	// Floor is what the caller knows about its own justification and the broker cannot see.
	Floor string `json:"floor,omitempty"`
	// Catalog is what a register request reports: the tool names this agent exposes and, for
	// each, its reversibility class. The class is the caller's claim about its own tools; the
	// broker cannot derive it, and the kernel must not guess it.
	Catalog map[string]string `json:"catalog,omitempty"`
	// RunID and Parents are the caller's story about where this call came from. They are
	// attribution, not identity, and the ledger records them as the caller's claim.
	RunID   string `json:"run_id,omitempty"`
	Parents string `json:"parents,omitempty"`
	// Ticket asks for an attestation covering one observation. Only a pure read can be
	// attested, so asking for one on anything else is refused rather than served.
	Ticket *TicketAsk `json:"ticket,omitempty"`
}

type Response struct {
	OK       bool               `json:"ok"`
	Error    string             `json:"error,omitempty"`
	Decision *platform.Decision `json:"decision,omitempty"`
	// Status answers a liveness question without recording anything: a client that wants to
	// know whether it can rely on this broker must be able to ask without leaving a decision
	// in the ledger it is asking about.
	Status *Status `json:"status,omitempty"`
	// Tools and UnknownPolicyTools answer a register: how many names the agent reported, and
	// which the policy uses that this agent does not have. That list is the typo detector —
	// without it the mistake surfaces as an agent flailing through refusals.
	Tools              int      `json:"tools,omitempty"`
	UnknownPolicyTools []string `json:"unknown_policy_tools,omitempty"`
	// Revision names the policy that produced this decision. A caller that logs only the
	// verdict cannot tell "the rule I wrote refuses this" from "the rule I replaced an hour
	// ago refuses this".
	Revision string `json:"revision,omitempty"`
}

// Status is what the broker says about itself. Deliberately small: everything here is a fact
// the operator cannot otherwise see without reading the state directory.
type Status struct {
	PolicyPath string   `json:"policy_path"`
	Principals int      `json:"principals"`
	Agents     []string `json:"agents,omitempty"`
	Started    string   `json:"started"`
}

// grantsFile is the principals half of the memory grant file, so one file configures identity
// for both brokers. A second file would be a second answer to "who is uid 501".
//
// `agent` is the single form the memory broker uses, where a store is scoped to one agent;
// `agents` is what a login that runs several agents needs. Both are read, and a principal that
// names neither is a configuration error rather than an empty permission.
type grantsFile struct {
	Principals []struct {
		UID    int      `json:"uid"`
		Agent  string   `json:"agent"`
		Agents []string `json:"agents"`
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
	grants     principal.Grants
	policyPath string
	stamp      policyStamp
	stateDir   string
	started    time.Time
	// catalogues is what each agent said about its own tools, by agent id. An absent entry
	// means "not known", which must behave like no catalogue at all rather than like an
	// empty one — an empty catalogue would refuse every call.
	catalogues map[string]map[string]platform.Reversibility
	// classes is the fleet-wide tool -> reversibility index built from every catalogue, and
	// owners says who supplied each entry so a disagreement can name both agents. A tool with
	// two classes is two answers to "can this be undone", and guessing one would put a claim
	// in the ledger that half the fleet contradicts.
	classes    map[string]platform.Reversibility
	owners     map[string]string
	tickets    *platform.TicketKey
	ticketPath string
	verbose    bool
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
	// Keyed commitments, not plain hashes: a fleet ledger is shared by every agent, so an
	// unkeyed hash of an agent id would be a re-identification oracle over low-entropy names.
	// The key also gives each subject a ref, which is how a row is grouped with the rest of
	// that agent's rows without the ledger ever naming it.
	ring, err := platform.OpenFileKeyRing(filepath.Join(cfg.StateDir, "keys.json"))
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("open key ring: %w", err)
	}
	refs, err := platform.OpenFileRefRegistry(filepath.Join(cfg.StateDir, "refs.json"))
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("open ref registry: %w", err)
	}
	committer, err := platform.NewCommitter(ring)
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("committer: %w", err)
	}
	denials, err := platform.OpenDenialCounter(filepath.Join(cfg.StateDir, "denials.json"),
		platform.DefaultDenialWindow, platform.DefaultDenialThreshold)
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("denial counter: %w", err)
	}
	grants, err := loadGrants(cfg.GrantsPath)
	if err != nil {
		_ = ledger.Close()
		return nil, err
	}

	gov := platform.NewGovernor()
	gov.Policy = policy
	gov.Ledger = ledger
	gov.Committer = committer
	gov.Refs = refs
	gov.Kill = kill
	gov.Denials = denials
	gov.TrustFloor = cfg.TrustFloor

	b := &broker{
		gov:        gov,
		ledger:     ledger,
		grants:     grants,
		policyPath: cfg.PolicyPath,
		stateDir:   cfg.StateDir,
		started:    time.Now().UTC(),
		catalogues: make(map[string]map[string]platform.Reversibility),
		classes:    make(map[string]platform.Reversibility),
		owners:     make(map[string]string),
	}
	gov.Catalogue = b.catalogueFor
	gov.Classify = b.classifyFor
	b.ticketPath = filepath.Join(cfg.StateDir, "ticket.key")
	if fi, err := os.Stat(cfg.PolicyPath); err == nil {
		b.stamp = policyStamp{size: fi.Size(), mod: fi.ModTime()}
	}
	return b, nil
}

func (b *broker) Close() error { return b.ledger.Close() }

func loadGrants(path string) (principal.Grants, error) {
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
	out := make(principal.Grants, len(f.Principals))
	for _, p := range f.Principals {
		agents := p.Agents
		if len(agents) == 0 && p.Agent != "" {
			agents = []string{p.Agent}
		}
		if len(agents) == 0 {
			return nil, fmt.Errorf("gaapd: %s grants uid %d no agents", path, p.UID)
		}
		out[p.UID] = agents
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("gaapd: %s names no principals: a broker with no principals cannot tell one agent from another", path)
	}
	return out, nil
}

// serve answers one request from a peer whose uid the transport established.
func (b *broker) serve(req Request, uid uint32) Response {
	if req.Op != "decide" && req.Op != "status" && req.Op != "register" {
		return Response{Error: fmt.Sprintf("unknown op %q (want \"decide\", \"status\" or \"register\")", req.Op)}
	}
	if req.Op == "decide" && req.Tool == "" {
		return Response{Error: "decide requires tool"}
	}
	agent, err := principal.Resolve(b.grants, uid, req.Agent)
	if err != nil {
		var unmapped principal.ErrUnmapped
		var ambiguous principal.ErrAmbiguous
		switch {
		case errors.As(err, &unmapped):
			log.Printf("gaapd: refused uid %d: %v", uid, err)
		case errors.As(err, &ambiguous):
			// Not a misconfiguration and not an attempt: the peer holds several agents and
			// did not say which, so the request is answerable only by the caller.
			log.Printf("gaapd: ambiguous peer uid %d: %v", uid, err)
		default:
			// A mapped peer naming an agent it does not hold is an attempt, and the log
			// has to say so.
			log.Printf("gaapd: IMPERSONATION uid %d: %v", uid, err)
		}
		return Response{Error: err.Error()}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.reloadLocked()

	switch req.Op {
	case "status":
		if b.verbose {
			log.Printf("gaapd: uid=%d agent=%s status", uid, agent)
		}
		return Response{OK: true, Revision: b.gov.Policy.Revision(), Status: &Status{
			PolicyPath: b.policyPath,
			Principals: len(b.grants),
			Agents:     b.agentNamesLocked(),
			Started:    b.started.Format(time.RFC3339),
		}}
	case "register":
		if len(req.Catalog) == 0 {
			return Response{Error: "register requires the tool names this agent exposes, with their classes"}
		}
		catalog := make(map[string]platform.Reversibility, len(req.Catalog))
		for tool, class := range req.Catalog {
			rev, err := parseReversibility(class)
			if err != nil {
				return Response{Error: fmt.Sprintf("%s: %v", tool, err)}
			}
			// A tool with two classes in one fleet is two answers to "can this be undone".
			// The second one is refused rather than overwriting: whichever agent registered
			// last would otherwise decide what the ledger claims about both.
			if owner, ok := b.owners[tool]; ok && b.classes[tool] != rev && owner != agent {
				return Response{Error: fmt.Sprintf("%s is %s for %s and %s for %s; one tool cannot have two classes — rename one of them",
					tool, b.classes[tool], owner, rev, agent)}
			}
			catalog[tool] = rev
			b.classes[tool] = rev
			b.owners[tool] = agent
		}
		b.catalogues[agent] = catalog
		tools := sortedCopy(keysOf(catalog))
		unknown := b.policyToolsNotInLocked(tools)
		var pure int
		for _, rev := range catalog {
			if rev == platform.Pure {
				pure++
			}
		}
		if b.verbose || len(unknown) > 0 {
			log.Printf("gaapd: %s registered %d tools (%d pure); policy names %d it does not have %v",
				agent, len(catalog), pure, len(unknown), unknown)
		}
		return Response{OK: true, Revision: b.gov.Policy.Revision(),
			Tools: len(catalog), UnknownPolicyTools: unknown}
	}

	// A tool the agent did not register is incoherent before it is policy: refusing here means
	// the record says "asked for a tool it does not have" instead of a rule that would have
	// allowed it if only the name had been right.
	if cat, known := b.catalogues[agent]; known {
		if _, has := cat[req.Tool]; !has {
			d := b.gov.Refuse(agent, req.Tool, req.Args, "broker:unknown-tool",
				fmt.Sprintf("this agent registered no tool named %q, so the policy was not consulted; its tools are: %s",
					req.Tool, strings.Join(sortedCopy(keysOf(cat)), ", ")))
			if b.verbose {
				log.Printf("gaapd: uid=%d agent=%s tool=%s -> deny (unknown tool)", uid, agent, req.Tool)
			}
			return Response{OK: true, Decision: &d, Revision: b.gov.Policy.Revision()}
		}
	}

	var opts []platform.DecideOpt
	if req.Floor != "" {
		class := platform.TaintClass(req.Floor)
		if !class.Known() {
			return Response{Error: fmt.Sprintf("floor %q is not a provenance class", req.Floor)}
		}
		opts = append(opts, platform.WithFloor(class))
	}
	if req.RunID != "" {
		opts = append(opts, platform.WithRun(req.RunID))
	}
	if req.Parents != "" {
		opts = append(opts, platform.WithParents(strings.Split(req.Parents, ",")...))
	}
	// A caller that asked for attestation gets a token or a refusal, never a quiet allow: the
	// broker mints tickets from the key beside kill.json, creating it on first use so a ticket
	// exists for the whole deployment rather than per process start.
	if req.Ticket != nil {
		if req.Ticket.EntryID == "" || req.Ticket.Resource == "" {
			return Response{Error: "a ticket must name the observation and the resource it attests"}
		}
		if b.gov.Tickets == nil {
			key, err := platform.LoadOrCreateTicketKey(b.ticketPath)
			if err != nil {
				return Response{Error: "ticket key: " + err.Error()}
			}
			b.gov.Tickets = key
		}
		opts = append(opts, platform.WithTicketFor(req.Ticket.EntryID, req.Ticket.Resource))
	}

	d := b.gov.Decide(agent, req.Tool, req.Args, opts...)
	if b.verbose {
		log.Printf("gaapd: uid=%d agent=%s tool=%s -> %s", uid, agent, req.Tool, d.Action)
	}
	return Response{OK: true, Decision: &d, Revision: b.gov.Policy.Revision()}
}

// catalogueFor reports an agent's registered tools to the governor, so a refusal can narrow
// its "permitted alternatives" to names the agent can actually call. Called under b.mu.
func (b *broker) catalogueFor(agentID string) []string {
	return sortedCopy(keysOf(b.catalogues[agentID]))
}

// classifyFor is the fleet's classifier: what the agents said their tools do. Absent means the
// kernel's conservative answer (irreversible), never a guess that a name looks like a read.
func (b *broker) classifyFor(tool string) platform.Reversibility {
	if rev, ok := b.classes[tool]; ok {
		return rev
	}
	return platform.Irreversible
}

// parseReversibility accepts only the classes this runtime can back. Reversible and compensable
// are reserved: claiming an inverse the runtime does not hold is the unchecked-inverse error,
// reproduced in an audit trail.
func parseReversibility(class string) (platform.Reversibility, error) {
	switch platform.Reversibility(class) {
	case platform.Pure:
		return platform.Pure, nil
	case platform.Irreversible:
		return platform.Irreversible, nil
	case platform.Reversible, platform.Compensable:
		return "", fmt.Errorf("class %q is reserved: no snapshot or compensation mechanism backs it yet, so recording it would claim an inverse that does not exist", class)
	default:
		return "", fmt.Errorf("class %q is not one of pure, irreversible", class)
	}
}

func keysOf(m map[string]platform.Reversibility) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (b *broker) policyToolsNotInLocked(tools []string) []string {
	has := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		has[t] = struct{}{}
	}
	var out []string
	for _, name := range b.gov.Policy.Tools() {
		if _, ok := has[name]; !ok {
			out = append(out, name)
		}
	}
	return sortedCopy(out)
}

func (b *broker) agentNamesLocked() []string {
	out := make([]string, 0, len(b.catalogues))
	for name := range b.catalogues {
		out = append(out, name)
	}
	return sortedCopy(out)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
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

// listen binds the socket. The mode is a parameter because the default (0666) means "any
// local user may ask", which is only defensible because the grant file decides who may act;
// an install that puts the socket in a group should say 0660 and let the filesystem say no
// before the broker has to.
func listen(path string, mode os.FileMode) (net.Listener, error) {
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
	if err := os.Chmod(path, mode); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// serveLoop accepts until the listener closes, then waits for the handlers already running.
// Draining matters more here than in a normal server: a decision cut off mid-response has
// already been written to the ledger, so stopping abruptly leaves a record of a call the
// caller was told nothing about.
func (b *broker) serveLoop(ln net.Listener) {
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				wg.Wait()
				return
			}
			log.Printf("gaapd: accept: %v", err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.handle(conn)
		}()
	}
}

func (b *broker) handle(conn net.Conn) {
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
	b.reply(conn, b.serve(req, uid))
}

func (b *broker) reply(conn net.Conn, r Response) {
	if err := json.NewEncoder(conn).Encode(r); err != nil {
		log.Printf("gaapd: reply: %v", err)
	}
}

// Exit codes a supervisor can act on. A broken policy or grant file is a configuration
// error, not a transient fault: restarting it just buries the message and burns the start
// limit, so the unit is told (RestartPreventExitStatus=78) to stop rather than loop.
const (
	exitOK     = 0
	exitUsage  = 2
	exitConfig = 78 // sysexits.h EX_CONFIG
)

func main() { os.Exit(run(os.Args[1:])) }

// isWithin reports whether path is root or sits under it.
func isWithin(path, root string) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false, err
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))), nil
}

func run(args []string) int {
	fs := flag.NewFlagSet("gaapd", flag.ContinueOnError)
	var (
		socketPath = fs.String("socket", "", "unix socket (default <state>/govern.sock)")
		stateDir   = fs.String("state", "/var/lib/gaapd", "state directory: ledger, keys, policy, grants (never the workload runtime's data root)")
		policyPath = fs.String("policy", "", "policy file (default <state>/policy.json)")
		grantsPath = fs.String("grants", "", "grant file with principals (default <state>/memory-grants.json)")
		socketMode = fs.Uint("socket-mode", 0o666, "permissions on the unix socket (an install that uses a group wants 0660)")
		trustFloor = fs.Float64("trust-floor", 0, "escalate allows to review below this trust score")
		verbose    = fs.Bool("v", false, "log one line per decision")
	)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "gaapd: %v\n", err)
		return exitUsage
	}

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

	// The policy file this daemon hot-reloads must not live where the governed workloads can
	// write. /var/lib/gaap is the dedicated dockerd data root (and /var/lib/gaap/ws/<agent> is
	// an agent's workspace): a broker whose rules sit there can have its rules rewritten by the
	// thing it is judging. Found by deploying next to a real worker, not by reasoning.
	if same, err := isWithin(cfg.StateDir, "/var/lib/gaap"); err == nil && same {
		log.Printf("gaapd: %s is the workload runtime's data root; the broker needs its own state directory (e.g. /var/lib/gaapd)", cfg.StateDir)
		return exitConfig
	}

	b, err := newBroker(cfg)
	if err != nil {
		log.Printf("gaapd: %v", err)
		return exitConfig
	}
	b.verbose = *verbose
	defer func() { _ = b.Close() }()

	ln, err := listen(cfg.Socket, os.FileMode(*socketMode).Perm())
	if err != nil {
		log.Printf("gaapd: listen %s: %v", cfg.Socket, err)
		return exitConfig
	}
	defer func() { _ = ln.Close() }()

	log.Printf("gaapd: serving %s (policy %s, revision %s, %d principals)",
		cfg.Socket, cfg.PolicyPath, b.gov.Policy.Revision(), len(b.grants))

	// SIGTERM is how a supervisor stops this. There is no graceful state to save — every
	// decision is fsynced as it is made — but in-flight requests are drained, and the socket
	// is unlinked so a restart does not have to reason about a file left behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("gaapd: stopping")
		_ = ln.Close()
	}()

	b.serveLoop(ln)
	_ = os.Remove(cfg.Socket)
	log.Printf("gaapd: stopped")
	return exitOK
}
