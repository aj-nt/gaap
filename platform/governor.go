package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// KillReader is the read side of a kill switch. Trip is deliberately absent:
// only an operator trips a switch, and only through a durable one, so the
// governor cannot be handed the ability to stop itself.
type KillReader interface {
	Tripped(scope string) bool
	TrippedScope(scope string) string
}

// Governor wires the primitives into a single decision authority — the "judge"
// from the design doc. It decides deterministically and records everything.
type Governor struct {
	Policy *PolicyEngine
	Kill   KillReader
	Trust  *TrustManager
	Ledger *Ledger
	// Enforcer applies the decision to the world. nil = noop.
	Enforcer Enforcer
	// TrustFloor escalates an allow to review while the trust score sits below
	// it. Zero disables the check: a governor that starts reviewing everything
	// on the day it is installed is a governor nobody runs.
	TrustFloor float64
	// Classify maps a tool name to its reversibility class. Defaults to the
	// kernel's conservative classifier (everything irreversible). A governed
	// workload injects a classifier that knows its own read-only tools, so the
	// ledger can admit which effects are provably pure. nil = conservative.
	Classify func(tool string) Reversibility
	// ClassOf maps a tool name to its action class when no rule declares one.
	// nil = rule declarations only.
	//
	// A governed workload injects this because it knows what its own tools do;
	// the kernel must not guess semantics it cannot see — the same reason
	// Classify is injectable. Without it, a deny-by-default refusal (no rule
	// names the tool, which is the common case) carries no class, so every
	// unlisted tool lands in one bucket and three unrelated refusals read as a
	// single sustained attempt.
	ClassOf func(tool string) ActionClass
	// Committer makes the subject and input commitments keyed and erasable.
	// nil = plain sha256 (the pre-erasure format): commitments are then
	// brute-forceable by anyone holding the ledger. Prefer a Committer with a
	// KeyRing: erasure of a subject is key destruction, and the chain still
	// verifies afterwards because the chain hash is unkeyed on purpose.
	Committer *Committer
	// Content is the erasable store the commitments point into. When set, every
	// recorded decision stores the arguments it decided on, under the input
	// commitment as the item key, so a record can be resolved to its content
	// while the ledger itself holds none.
	//
	// It is not optional decoration. A commitment that points at nothing makes
	// "what did it see when it decided" unanswerable, and makes an erasure a
	// no-op that still reports success. A decision whose content cannot be
	// stored is refused, on the same rule that governs recording itself.
	Content ContentStore
	// Refs maps a subject to the opaque handle its commitments are keyed under.
	// nil = the subject string is the handle, which is acceptable for agent ids
	// and unacceptable for people: it stores the identifier in the clear. A
	// RefRegistry mints a random handle and can forget it on erasure.
	Refs RefRegistry
	// Hash commits to a value without storing it. nil = sha256. Only used when
	// Committer is nil; it cannot be erased, so it survives as the legacy path.
	Hash func(value string) string
	// Denials counts recent refusals per (agent, action class). nil = no
	// counting, which is the memory-only default: a counter that lives as long
	// as the process cannot see the pattern that spans one.
	//
	// When set, a refusal that reaches the threshold is annotated on the decision
	// and a separate burst entry is recorded. The decision itself does not
	// change: a burst never turns a deny into a review.
	Denials *DenialCounter
}

// NewGovernor returns a governor with defaults: deny-by-default policy, an open
// kill switch, trust starting at 500, a memory-only ledger and content store,
// and a noop enforcer. Attach a durable ledger (NewLedgerAt) and the matching
// file-backed switch, refs and content store to make the record survive the
// process.
func NewGovernor() *Governor {
	return &Governor{
		Policy:   NewPolicyEngine(),
		Kill:     NewKillSwitch(),
		Trust:    NewTrustManager(500),
		Ledger:   NewLedger(),
		Enforcer: &NoopEnforcer{},
		// Content is not decoration: without it a decision is recorded with a
		// commitment that points at nothing, and an erasure is a no-op that
		// still reports success. The default governor stores what it decided on.
		Content: NewMemoryContentStore(),
	}
}

// Decide evaluates a tool call for an agent. Kill switch first (non-negotiable),
// then policy. The decision moves trust in both directions — an allow is
// evidence too — and the trust floor can escalate an allow to review. Every
// decision is recorded.
//
// If the decision cannot be recorded, Decide refuses the call. A governor that
// cannot record cannot govern: what is not on the record cannot be proven, so
// the action is denied with the recording failure as its reason rather than
// allowed blind.
func (g *Governor) Decide(agentID, tool string, args map[string]any) Decision {
	var d Decision
	if scope := g.Kill.TrippedScope(agentID); scope != "" {
		// Name the scope. "kill switch tripped" reads the same whether one
		// agent was stopped or the whole fleet was.
		d = Deny("kill switch tripped: scope " + scope)
	} else {
		d = g.Policy.Evaluate(tool, args)
	}
	if d.Action == ActionAllow {
		g.Trust.Reward(1)
		if g.TrustFloor > 0 {
			if score := g.Trust.Score(); score < g.TrustFloor {
				d = Decision{
					Action: ActionReview,
					Reason: fmt.Sprintf("trust %.0f below floor %.0f", score, g.TrustFloor),
				}
			}
		}
	} else {
		g.Trust.Penalize(1)
	}
	// A refusal is rendered from structure, not from the reason text. The
	// correlation id is minted here so the text the agent sees and the row the
	// operator reads carry the same handle; minting it at the call site would
	// leave the ledger row unsearchable.
	if d.Action != ActionAllow {
		dc := DenialClass{
			Rule:          d.RuleID,
			ActionClass:   g.actionClassFor(tool),
			Forbidden:     tool,
			Permitted:     g.Policy.PermittedFor(tool),
			CorrelationID: NewCorrelationID(),
		}
		d.Denial = &dc
	}
	// A burst is a pattern, not a different verdict: the deny already stands, and
	// the count only makes the sustained attempt visible in the record. It is
	// counted here rather than at the call site so a workload that retries in a
	// new process still lands in the same bucket.
	if d.Action == ActionDeny && g.Denials != nil && d.Denial != nil {
		count, burst, err := g.Denials.Observe(agentID, d.Denial.ActionClass)
		if err == nil && burst {
			d.Burst = count
		}
	}
	if err := g.record(agentID, tool, args, d); err != nil {
		return Deny("governor could not record the decision: " + err.Error())
	}
	return d
}

// record appends the structural skeleton of a decision: what tool, which rule,
// what outcome, on which input (hashed), why — and a hash of who, never the
// identity itself.
func (g *Governor) record(agentID, tool string, args map[string]any, d Decision) error {
	subjectHash, inputHash, ref, algo, err := g.commitments(agentID, args)
	if err != nil {
		return err
	}
	if g.Content != nil {
		if err := g.Content.Put(ref, inputHash, []byte(canonicalArgs(args))); err != nil {
			return fmt.Errorf("store decision content: %w", err)
		}
	}
	if _, err := g.Ledger.Append(Entry{
		Kind:          KindDecision,
		SubjectHash:   subjectHash,
		SubjectRef:    ref,
		CommitAlgo:    algo,
		Tool:          tool,
		RuleID:        d.RuleID,
		Outcome:       d.Action,
		InputHash:     inputHash,
		Reason:        d.Reason,
		Reversibility: g.classifyTool(tool),
		DenialClass:   denialClassOf(d),
		CorrelationID: correlationOf(d),
	}); err != nil {
		return err
	}
	if d.Burst > 0 {
		// A separate row: a burst is a pattern observed about the agent, not a
		// property of this call. Folding it into the decision row would make two
		// identical calls hash differently depending on history, and the chain's
		// job is to say what happened, not why it was suspicious.
		if _, err := g.Ledger.Append(Entry{
			Kind:          KindDenialBurst,
			SubjectHash:   subjectHash,
			SubjectRef:    ref,
			CommitAlgo:    algo,
			Tool:          tool,
			RuleID:        d.RuleID,
			Outcome:       d.Action,
			Reason:        fmt.Sprintf("sustained refusal: %d denials of this action class inside the window", d.Burst),
			Reversibility: g.classifyTool(tool),
			DenialClass:   denialClassOf(d),
			CorrelationID: correlationOf(d),
		}); err != nil {
			return err
		}
	}
	return nil
}

// actionClassFor resolves the class of a refused call: what the rule declared,
// else what the injected classifier says. Rule wins because a policy author
// naming a tool is more specific than a tool's default class.
func (g *Governor) actionClassFor(tool string) ActionClass {
	if c := g.Policy.ActionClassOf(tool); c != "" {
		return c
	}
	if g.ClassOf != nil {
		return g.ClassOf(tool)
	}
	return ""
}

// denialClassOf and correlationOf read the structured refusal, if there is one.
// Kept as free functions so record stays a flat struct literal: a nil Denial must
// produce empty fields, not a panic, because allow decisions have none.
func denialClassOf(d Decision) string {
	if d.Denial == nil {
		return ""
	}
	return string(d.Denial.ActionClass)
}

func correlationOf(d Decision) string {
	if d.Denial == nil {
		return ""
	}
	return d.Denial.CorrelationID
}

// commitments returns the subject and input commitments for a decision, plus
// the handle and algorithm they were made under so the entry is self-describing
// after a key rotation or an erasure. With a Committer the commitments are keyed
// and erasable; without one they are the legacy unkeyed hashes.
func (g *Governor) commitments(agentID string, args map[string]any) (subjectHash, inputHash, ref, algo string, err error) {
	if g.Committer == nil {
		return g.commit(agentID), g.commit(canonicalArgs(args)), "", "", nil
	}
	ref, err = g.subjectRef(agentID)
	if err != nil {
		return "", "", "", "", err
	}
	subjectHash, err = g.Committer.Commit(ref, agentID)
	if err != nil {
		return "", "", "", "", fmt.Errorf("commit subject: %w", err)
	}
	inputHash, err = g.Committer.Commit(ref, canonicalArgs(args))
	if err != nil {
		return "", "", "", "", fmt.Errorf("commit input: %w", err)
	}
	return subjectHash, inputHash, ref, g.Committer.AlgoName(), nil
}

// subjectRef resolves the handle a decision is keyed under.
func (g *Governor) subjectRef(subject string) (string, error) {
	if g.Refs == nil {
		return subject, nil
	}
	ref, err := g.Refs.Ref(subject)
	if err != nil {
		return "", fmt.Errorf("resolve subject ref: %w", err)
	}
	return ref, nil
}

// commit hashes a value so it can be referenced without being stored.
func (g *Governor) commit(value string) string {
	if g.Hash != nil {
		return g.Hash(value)
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// canonicalArgs renders call arguments deterministically so the same call
// always produces the same input hash. encoding/json sorts map keys; the fmt
// fallback is only reachable for values that cannot be marshaled (channels,
// functions), which JSON-decoded tool arguments never contain.
func canonicalArgs(args map[string]any) string {
	if args == nil {
		return "null"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(b)
}

// classifyTool resolves the reversibility class for a tool: the injected
// workload classifier when present, else the kernel's conservative default
// (everything irreversible).
func (g *Governor) classifyTool(tool string) Reversibility {
	if g.Classify != nil {
		return g.Classify(tool)
	}
	return classify(tool)
}
