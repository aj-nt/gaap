package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	default:
		return fmt.Errorf("unknown govern subcommand %q (want decide, erase, kill or status)", args[0])
	}
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

// governDecide asks the governor for one decision about one tool call, records
// it, and prints the outcome. A deny is a successful run: the decision is the
// data, not an error.
func governDecide(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("govern decide", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state", ".gaap", "state directory (ledger, keys, refs, content)")
	policyPath := fs.String("policy", "", "policy file (required)")
	subject := fs.String("subject", "", "who the call is about: an agent id, or a person")
	tool := fs.String("tool", "", "tool name the policy matches against")
	argsJSON := fs.String("args", "{}", "tool arguments as JSON")
	readTools := fs.String("read-tools", "", "comma-separated tool names to classify as pure reads")
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
	var callArgs map[string]any
	if err := json.Unmarshal([]byte(*argsJSON), &callArgs); err != nil {
		return fmt.Errorf("--args is not a JSON object: %w", err)
	}

	policy, err := platform.LoadPolicyFile(*policyPath)
	if err != nil {
		return err
	}
	state, err := openGovernState(*stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()

	committer, err := platform.NewCommitter(state.ring)
	if err != nil {
		return err
	}
	g := platform.NewGovernor()
	g.Policy = policy
	g.Ledger = state.ledger
	g.Committer = committer
	g.Refs = state.refs
	g.Content = state.content
	// The read side only: an operator trips the switch with `govern kill`, and
	// the trip is durable, so this invocation cannot forget it.
	g.Kill = state.kill
	g.TrustFloor = *trustFloor
	if *readTools != "" {
		pure := make(map[string]struct{})
		for _, name := range strings.Split(*readTools, ",") {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				pure[trimmed] = struct{}{}
			}
		}
		// The workload knows its own read-only tools; the kernel never guesses.
		g.Classify = func(tool string) platform.Reversibility {
			if _, ok := pure[tool]; ok {
				return platform.Pure
			}
			return platform.Irreversible
		}
	}

	before := len(state.ledger.Entries())
	decision := g.Decide(*subject, *tool, callArgs)
	entries := state.ledger.Entries()
	if len(entries) == before {
		// Decide refuses rather than allows when it cannot record; surface that
		// instead of printing a decision with no record behind it.
		return fmt.Errorf("decision was not recorded: %s", decision.Reason)
	}
	entry := entries[len(entries)-1]

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "decision:\t%s\n", decision.Action)
	_, _ = fmt.Fprintf(tw, "rule:\t%s\n", orNone(decision.RuleID))
	_, _ = fmt.Fprintf(tw, "reason:\t%s\n", orNone(decision.Reason))
	_, _ = fmt.Fprintf(tw, "tool:\t%s\n", entry.Tool)
	_, _ = fmt.Fprintf(tw, "reversibility:\t%s\n", entry.Reversibility)
	_, _ = fmt.Fprintf(tw, "subject handle:\t%s\n", entry.SubjectRef)
	_, _ = fmt.Fprintf(tw, "seq:	%d\n", entry.Seq)
	// The score is process-local: TrustManager decays and updates in memory, and
	// each CLI invocation starts a fresh governor. A long-lived governor (a
	// governed workload) is where the floor accumulates meaning.
	_, _ = fmt.Fprintf(tw, "trust:	%.0f (process-local)\n", g.Trust.Score())
	return tw.Flush()
}

// governErase honors an erasure request for a subject: content deleted, key
// shredded, the erasure recorded. The chain still verifies afterwards, which is
// the whole point.
func governErase(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("govern erase", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state", ".gaap", "state directory (ledger, keys, refs, content)")
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
	stateDir := fs.String("state", ".gaap", "state directory (ledger, keys, refs, content, kill switch)")
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
		Kind:    "killswitch",
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
	stateDir := fs.String("state", ".gaap", "state directory (ledger, keys, refs, content, kill switch)")
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
func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
