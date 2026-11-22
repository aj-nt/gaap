package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/aj-nt/gaap/platform"
)

// governCommand is the operator's handle on the governor: ask it for a decision,
// or erase a subject. Both work on a state directory that holds the record, the
// key ring, the subject handles, and the erasable content, so a decision made in
// one invocation can be read, verified, and erased in the next.
//
//	gaap govern decide --policy rules.json --subject agent-1 --tool Execute --args '{...}'
//	gaap govern erase  --subject alice@example.com --reason gdpr-request
//	gaap govern kill   --scope global --reason incident-42
//	gaap govern status
func governCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: gaap govern <decide|erase|kill|status> [flags]")
	}
	switch args[0] {
	case "decide":
		return governDecide(args[1:], out)
	case "erase":
		return governErase(args[1:], out)
	case "kill":
		return governKill(args[1:], out)
	case "status":
		return governStatus(args[1:], out)
	case "lint":
		return governLint(args[1:], out)
	default:
		return fmt.Errorf("unknown govern subcommand %q (want decide, erase, kill, status or lint)", args[0])
	}
}

// governLint reports the gaps in a policy that would make the record weaker than
// it reads, and fails only on the error-level ones. The check lives here rather
// than at load because a policy that will not load cannot be inspected at all,
// and an operator migrating an existing policy needs the report before the rule.
func governLint(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("govern lint", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	policyPath := fs.String("policy", "", "policy file to lint (required)")
	strict := fs.Bool("strict", false, "exit non-zero on error-level findings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *policyPath == "" {
		return fmt.Errorf("govern lint needs --policy")
	}
	engine, err := platform.LoadPolicyFile(*policyPath)
	if err != nil {
		return err
	}
	findings := engine.Lint()
	if len(findings) == 0 {
		_, _ = fmt.Fprintf(out, "policy %s: clean (revision %s)\n", *policyPath, shortRevision(engine.Revision()))
		return nil
	}
	for _, f := range findings {
		_, _ = fmt.Fprintln(out, f)
	}
	if *strict && platform.HasErrors(findings) {
		return fmt.Errorf("policy has error-level findings")
	}
	return nil
}

// ledgerPath is where the state directory keeps the record.
func ledgerPath(stateDir string) string {
	return filepath.Join(stateDir, "ledger.jsonl")
}

// governState is everything a single invocation needs to open, in the order it
// must be closed.
type governState struct {
	dir     string
	ledger  *platform.Ledger
	ring    *platform.FileKeyRing
	refs    *platform.FileRefRegistry
	content *platform.FileContentStore
	kill    *platform.FileKillSwitch
	tempo   *platform.Tempo
}

// openGovernState opens the durable halves of the governor from a state directory.
func openGovernState(dir string) (*governState, error) {
	if dir == "" {
		return nil, fmt.Errorf("state directory is required")
	}
	ledger, err := platform.NewLedgerAt(ledgerPath(dir))
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	ring, err := platform.OpenFileKeyRing(filepath.Join(dir, "keys.json"))
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("open key ring: %w", err)
	}
	refs, err := platform.OpenFileRefRegistry(filepath.Join(dir, "refs.json"))
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("open ref registry: %w", err)
	}
	content, err := platform.OpenFileContentStore(filepath.Join(dir, "content"))
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("open content store: %w", err)
	}
	kill, err := platform.OpenFileKillSwitch(filepath.Join(dir, "kill.json"))
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("open kill switch: %w", err)
	}
	return &governState{dir: dir, ledger: ledger, ring: ring, refs: refs, content: content, kill: kill}, nil
}

func (s *governState) Close() error { return s.ledger.Close() }

// buildGovernor opens the durable state and assembles the governor over it, so a
// decision made during a run lands in the same record as one made from the CLI.
// The caller owns the returned state and must close it.
func buildGovernor(policyPath, stateDir string, trustFloor float64, readTools string) (*platform.Governor, *governState, error) {
	if stateDir == "" {
		stateDir = defaultStateDir
	}
	if policyPath == "" {
		return nil, nil, fmt.Errorf("a governor needs --policy")
	}
	policy, err := platform.LoadPolicyFile(policyPath)
	if err != nil {
		return nil, nil, err
	}
	state, err := openGovernState(stateDir)
	if err != nil {
		return nil, nil, err
	}
	committer, err := platform.NewCommitter(state.ring)
	if err != nil {
		_ = state.Close()
		return nil, nil, err
	}
	g := platform.NewGovernor()
	g.Policy = policy
	g.Ledger = state.ledger
	g.Committer = committer
	g.Refs = state.refs
	g.Content = state.content
	// The read side only: an operator trips the switch with `govern kill`, and
	// the trip is durable, so a run cannot forget it.
	g.Kill = state.kill
	g.TrustFloor = trustFloor
	// A refusal pattern outlives the process that saw it: the counter lives
	// beside the kill switch, so a run that dies and resumes still sees itself
	// as the same prober.
	denials, err := platform.OpenDenialCounter(
		filepath.Join(stateDir, "denials.json"),
		platform.DefaultDenialWindow, platform.DefaultDenialThreshold)
	if err != nil {
		_ = state.Close()
		return nil, nil, err
	}
	g.Denials = denials
	// Arrival timing, measured beside the ledger. Observation only — it changes no
	// verdict, and a run with no tempo configured simply records no timing.
	tempo, err := platform.OpenTempo(
		filepath.Join(stateDir, "tempo.json"), platform.DefaultTempoWindow, 256)
	if err != nil {
		_ = state.Close()
		return nil, nil, err
	}
	state.tempo = tempo
	if readTools != "" {
		// The workload knows its own read-only tools; the kernel never guesses.
		pure := make(map[string]struct{})
		for _, name := range strings.Split(readTools, ",") {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				pure[trimmed] = struct{}{}
			}
		}
		g.Classify = func(tool string) platform.Reversibility {
			if _, ok := pure[tool]; ok {
				return platform.Pure
			}
			return platform.Irreversible
		}
	}
	return g, state, nil
}

// defaultStateDir is where a governed run or CLI decision keeps its record. One
// constant because five flags default to it and a run that writes to `.gaap`
// while `ledger verify` reads somewhere else is a record nobody can check.
const defaultStateDir = ".gaap"

// openRunGovernor attaches a governor to a run. Empty policy means ungoverned —
// the common case — and the returned cleanup is never nil, so a caller can
// defer it without asking whether anything was opened.
func openRunGovernor(policy, stateDir string, trustFloor float64) (*platform.Governor, *platform.Tempo, func(), error) {
	if policy == "" {
		return nil, nil, func() {}, nil
	}
	g, state, err := buildGovernor(policy, stateDir, trustFloor, "")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("governed run: %w", err)
	}
	slog.Info("govern: the run is governed", "policy", policy, "state", state.dir)
	return g, state.tempo, func() { _ = state.Close() }, nil
}

// governDecide asks the governor for one decision about one tool call, records
// it, and prints the outcome. A deny is a successful run: the decision is the
// data, not an error.
func governDecide(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("govern decide", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state", defaultStateDir, "state directory (ledger, keys, refs, content)")
	policyPath := fs.String("policy", "", "policy file (required)")
	subject := fs.String("subject", "", "who the call is about: an agent id, or a person")
	tool := fs.String("tool", "", "tool name the policy matches against")
	argsJSON := fs.String("args", "{}", "tool arguments as JSON")
	readTools := fs.String("read-tools", "", "comma-separated tool names to classify as pure reads")
	floor := fs.String("floor", "", "provenance floor of the justification: external, internal or fact (default: unreported)")
	ticketEntry := fs.String("ticket-entry", "", "issue an attestation covering this memory entry id (needs a pure read)")
	ticketResource := fs.String("ticket-resource", "", "the resource the observation is about; must equal the entry's source_ref, which is what the broker verifies the ticket against")
	ticketKeyPath := fs.String("ticket-key", "", "gate-ticket key path (default <state>/ticket.key)")
	trustFloor := fs.Float64("trust-floor", 0, "escalate allows to review while the trust score is below this (0 disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *policyPath == "" {
		return fmt.Errorf("govern decide requires --policy")
	}
	if *subject == "" {
		return fmt.Errorf("govern decide requires --subject")
	}
	if *tool == "" {
		return fmt.Errorf("govern decide requires --tool")
	}
	// The floor is what the caller knows about its own justification — including anything it
	// read out of memory — and the governor refuses to guess it. An unknown word is an error
	// rather than a silent external: a typo that quietly weakens the provenance would look
	// exactly like a policy that never asked.
	if *floor != "" && !platform.TaintClass(*floor).Known() {
		return fmt.Errorf("--floor %q is not a provenance class (want external, internal or fact)", *floor)
	}
	if (*ticketEntry == "") != (*ticketResource == "") {
		return fmt.Errorf("--ticket-entry and --ticket-resource go together: an attestation names the entry it covers and what that entry is about")
	}
	var callArgs map[string]any
	if err := json.Unmarshal([]byte(*argsJSON), &callArgs); err != nil {
		return fmt.Errorf("--args is not a JSON object: %w", err)
	}

	g, state, err := buildGovernor(*policyPath, *stateDir, *trustFloor, *readTools)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()

	before := len(state.ledger.Entries())
	// The CLI is a caller like any other, so it is measured like one. Skipping this
	// would leave tempo with a producer only inside a run, and an operator checking
	// the mechanism from the command line would find no file and conclude it was
	// broken.
	if err := state.tempo.Observe(*subject); err != nil {
		slog.Warn("tempo: observation not recorded", "subject", *subject, "err", err)
	}
	// The attestation path: the one place a decision mints a credential. The key is the
	// issuer's, created beside the kill switch and the ledger if absent — rotation is a
	// deliberate operation, and a key that appeared when a deploy changed an environment
	// variable would invalidate every in-flight ticket.
	var opts []platform.DecideOpt
	if *floor != "" {
		opts = append(opts, platform.WithFloor(platform.TaintClass(*floor)))
	}
	if *ticketEntry != "" {
		keyPath := *ticketKeyPath
		if keyPath == "" {
			keyPath = filepath.Join(state.dir, "ticket.key")
		}
		key, err := platform.LoadOrCreateTicketKey(keyPath)
		if err != nil {
			return fmt.Errorf("ticket key: %w", err)
		}
		g.Tickets = key
		opts = append(opts, platform.WithTicketFor(*ticketEntry, *ticketResource))
	}
	decision := g.Decide(*subject, *tool, callArgs, opts...)
	entries := state.ledger.Entries()
	if len(entries) == before {
		// Decide refuses rather than allows when it cannot record; surface that
		// instead of printing a decision with no record behind it.
		return fmt.Errorf("decision was not recorded: %s", decision.Reason)
	}
	// The decision is the first entry appended, not the last: a burst records a
	// second row after it, and printing that one would report the annotation's
	// seq as if it were the decision's.
	entry := entries[before]

	return renderDecision(out, g, decision, entry)
}

// renderDecision prints one decision: what was decided, on what evidence, and where the
// record of it is. Extracted from governDecide so the decision verb can grow flags without
// turning into a function that also formats, and so this rendering can be tested on a
// Decision without a governor's whole state.
func renderDecision(out io.Writer, g *platform.Governor, decision platform.Decision, entry platform.Entry) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "decision:\t%s\n", decision.Action)
	_, _ = fmt.Fprintf(tw, "rule:\t%s\n", orNone(decision.RuleID))
	_, _ = fmt.Fprintf(tw, "reason:\t%s\n", orNone(decision.Reason))
	// Carried only when there is something to say: a rule that requires nothing and a
	// caller that reported nothing have no provenance question, and two empty rows on every
	// decision would train the reader to skip the section.
	if decision.MinTaint != "" || decision.Floor != "" {
		_, _ = fmt.Fprintf(tw, "min taint:\t%s\n", orNone(string(decision.MinTaint)))
		_, _ = fmt.Fprintf(tw, "justification floor:\t%s\n", orUnreported(string(decision.Floor)))
	}
	_, _ = fmt.Fprintf(tw, "tool:	%s\n", entry.Tool)
	_, _ = fmt.Fprintf(tw, "reversibility:	%s\n", entry.Reversibility)
	// A bearer token, printed to the caller and nowhere else: the ledger records the
	// decision, not the credential. Only present when attestation was asked for and
	// granted — an empty line would read as "issued but lost".
	if decision.Ticket != "" {
		_, _ = fmt.Fprintf(tw, "ticket:\t%s\n", decision.Ticket)
	}
	if entry.DenialClass != "" {
		_, _ = fmt.Fprintf(tw, "denial class:	%s\n", entry.DenialClass)
	}
	if entry.CorrelationID != "" {
		_, _ = fmt.Fprintf(tw, "correlation:	%s\n", entry.CorrelationID)
	}
	if decision.Burst > 0 {
		_, _ = fmt.Fprintf(tw, "burst:	%d denials of this action class in the window\n", decision.Burst)
	}
	if decision.Artifact != nil {
		_, _ = fmt.Fprintf(tw, "artifact:	%s %s ttl=%s ceiling=%s scope=%s\n",
			decision.Artifact.Type, platform.ArtifactClassOf(decision.Artifact),
			orDash(decision.Artifact.Lifetime.String()), orDash(decision.Artifact.Ceiling.String()),
			orDash(decision.Artifact.Scope))
	} else if decision.Action == platform.ActionAllow {
		// Not "absent": nobody characterised what this call leaves behind, which is
		// the state an operator most needs to see.
		_, _ = fmt.Fprintf(tw, "artifact:	unrecorded (no artifact declared on this rule)\n")
	}
	// The content revision of the policy actually in force. A hand-built engine
	// has none, and saying so is better than printing the format version.
	_, _ = fmt.Fprintf(tw, "policy revision:	%s\n", orUnrecorded(shortRevision(g.Policy.Revision())))
	// The text the agent would be shown, distinct from the raw reason above:
	// the reason is for the operator, this is for the refused workload.
	if decision.Denial != nil {
		_, _ = fmt.Fprintf(tw, "reframe:	%s\n", platform.ReframeDenial(*decision.Denial))
	}
	_, _ = fmt.Fprintf(tw, "subject handle:	%s\n", entry.SubjectRef)
	_, _ = fmt.Fprintf(tw, "seq:	%d\n", entry.Seq)
	// The score is process-local: TrustManager decays and updates in memory, and
	// each CLI invocation starts a fresh governor. A long-lived governor (a
	// governed workload) is where the floor accumulates meaning.
	_, _ = fmt.Fprintf(tw, "trust:	%.0f (process-local)\n", g.Trust.Score())
	return tw.Flush()
}

// shortRevision abbreviates a content hash for a terminal line. The full value is
// in the ledger row.
func shortRevision(rev string) string {
	const n = 12
	if len(rev) <= n {
		return rev
	}
	return rev[:n]
}

// orUnrecorded says "unknown" rather than printing an empty column: a hand-built
// engine has no file and therefore no revision, and a blank reads as a bug.
func orUnrecorded(s string) string {
	if s == "" {
		return "unrecorded (engine built in code, no policy file)"
	}
	return s
}

// governErase honors an erasure request for a subject: content deleted, key
// shredded, the erasure recorded. The chain still verifies afterwards, which is
// the whole point.
func governErase(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("govern erase", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state", defaultStateDir, "state directory (ledger, keys, refs, content)")
	subject := fs.String("subject", "", "subject to erase (required)")
	reason := fs.String("reason", "erasure-request", "reason code recorded on the erase entry")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *subject == "" {
		return fmt.Errorf("govern erase requires --subject")
	}

	state, err := openGovernState(*stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()

	before := len(state.ledger.Entries())
	eraser := &platform.Eraser{
		Content: state.content,
		Ring:    state.ring,
		Ledger:  state.ledger,
		Refs:    state.refs,
	}
	if err := eraser.EraseSubject(*subject, *reason); err != nil {
		return err
	}
	after := state.ledger.Entries()

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "erased:\t%s\n", *subject)
	_, _ = fmt.Fprintf(tw, "reason:\t%s\n", *reason)
	if len(after) == before {
		_, _ = fmt.Fprintf(tw, "note:\tno content or key was held for this subject\n")
	} else {
		_, _ = fmt.Fprintf(tw, "entry:\tseq %d kind=%s\n", after[len(after)-1].Seq, after[len(after)-1].Kind)
	}
	_, _ = fmt.Fprintf(tw, "chain:\t%d entries, verified\n", len(after))
	return tw.Flush()
}

// governKill stops a scope, durably, and puts the stop on the record. The order
// matters: the trip is persisted before it is reported, because a stop that
// reports success while still in memory is forgotten by the next invocation.
func governKill(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("govern kill", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state", defaultStateDir, "state directory (ledger, keys, refs, content, kill switch)")
	scope := fs.String("scope", "", "scope to stop: an agent id, a tool, or \"global\" (required)")
	reason := fs.String("reason", "operator-stop", "reason recorded on the kill entry")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scope == "" {
		return fmt.Errorf("govern kill requires --scope")
	}

	state, err := openGovernState(*stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()

	before := state.kill.TrippedScope(*scope)
	if err := state.kill.Trip(*scope); err != nil {
		return err
	}
	seq, err := state.ledger.Append(platform.Entry{
		Kind:    platform.KindKillswitch,
		Tool:    *scope,
		Outcome: platform.ActionDeny,
		Reason:  *reason,
	})
	if err != nil {
		return fmt.Errorf("trip recorded in the switch but not in the ledger: %w", err)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "tripped:\t%s\n", *scope)
	if before != "" {
		_, _ = fmt.Fprintf(tw, "note:\talready tripped by scope %s\n", before)
	}
	_, _ = fmt.Fprintf(tw, "reason:\t%s\n", *reason)
	_, _ = fmt.Fprintf(tw, "durable:\t%s\n", filepath.Join(state.dir, "kill.json"))
	_, _ = fmt.Fprintf(tw, "entry:\tseq %d\n", seq)
	_, _ = fmt.Fprintln(tw, "untrip:\tnone — this is one-way; reopening is an operator action on kill.json")
	return tw.Flush()
}

// governStatus reports what the state directory currently says: which scopes are
// stopped, and how much record is behind them.
func governStatus(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("govern status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state", defaultStateDir, "state directory (ledger, keys, refs, content, kill switch)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	state, err := openGovernState(*stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()

	entries := state.ledger.Entries()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "state:\t%s\n", state.dir)
	_, _ = fmt.Fprintf(tw, "tripped:\t%s\n", orNone(strings.Join(state.kill.Scopes(), ", ")))
	_, _ = fmt.Fprintf(tw, "entries:\t%d\n", len(entries))
	if len(entries) > 0 {
		last := entries[len(entries)-1]
		_, _ = fmt.Fprintf(tw, "head:\tseq %d %s\n", last.Seq, last.Hash)
	}
	return tw.Flush()
}

// orNone renders an empty field as a dash so the output has no blank values.
// orUnreported names the absent floor rather than printing a dash. Absent is not external:
// the first is ignorance about the justification, the second is a fact about it, and only the
// second is attributable — the same distinction the kernel's downgrade reason draws.
func orUnreported(s string) string {
	if s == "" {
		return "unreported (no floor stated)"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
