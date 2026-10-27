package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Governor wires the primitives into a single decision authority — the "judge"
// from the design doc. It decides deterministically and records everything.
type Governor struct {
	Policy   *PolicyEngine
	Kill     *KillSwitch
	Trust    *TrustManager
	Ledger   *Ledger
	Enforcer Enforcer
	// Classify maps a tool name to its reversibility class. Defaults to the
	// kernel's conservative classifier (everything irreversible). A governed
	// workload injects a classifier that knows its own read-only tools, so the
	// ledger can admit which effects are provably pure. nil = conservative.
	Classify func(tool string) Reversibility
	// Committer makes the subject and input commitments keyed and erasable.
	// nil = plain sha256 (the pre-erasure format): commitments are then
	// brute-forceable by anyone holding the ledger. Prefer a Committer with a
	// KeyRing: erasure of a subject is key destruction, and the chain still
	// verifies afterwards because the chain hash is unkeyed on purpose.
	Committer *Committer
	// SubjectRef maps a subject to the opaque handle its commitments are keyed
	// under. nil = the subject string is the handle. Inject this when the
	// subject is a person rather than an agent id, so the handle is random and
	// never derived from personal data.
	SubjectRef func(subject string) string
	// Hash commits to a value without storing it. nil = sha256. Only used when
	// Committer is nil; it cannot be erased, so it survives as the legacy path.
	Hash func(value string) string
}

// NewGovernor returns a governor with defaults: deny-by-default policy, an
// open kill switch, trust starting at 500, a memory-only ledger, and a noop
// enforcer. Attach a durable ledger (NewLedgerAt) to make the record survive
// the process.
func NewGovernor() *Governor {
	return &Governor{
		Policy:   NewPolicyEngine(),
		Kill:     NewKillSwitch(),
		Trust:    NewTrustManager(500),
		Ledger:   NewLedger(),
		Enforcer: &NoopEnforcer{},
	}
}

// Decide evaluates a tool call for an agent. Kill switch first (non-negotiable),
// then policy. A non-allow decision penalizes trust. Every decision is recorded.
//
// If the decision cannot be recorded, Decide refuses the call. A governor that
// cannot record cannot govern: what is not on the record cannot be proven, so
// the action is denied with the recording failure as its reason rather than
// allowed blind.
func (g *Governor) Decide(agentID, tool string, args map[string]any) Decision {
	var d Decision
	if g.Kill.Tripped(agentID) {
		d = Deny("kill switch tripped")
	} else {
		d = g.Policy.Evaluate(tool, args)
	}
	if d.Action != ActionAllow {
		g.Trust.Penalize(1)
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
	_, err = g.Ledger.Append(Entry{
		Kind:          "decision",
		SubjectHash:   subjectHash,
		SubjectRef:    ref,
		CommitAlgo:    algo,
		Tool:          tool,
		RuleID:        d.RuleID,
		Outcome:       d.Action,
		InputHash:     inputHash,
		Reason:        d.Reason,
		Reversibility: g.classifyTool(tool),
	})
	return err
}

// commitments returns the subject and input commitments for a decision, plus
// the handle and algorithm they were made under so the entry is self-describing
// after a key rotation or an erasure. With a Committer the commitments are keyed
// and erasable; without one they are the legacy unkeyed hashes.
func (g *Governor) commitments(agentID string, args map[string]any) (subjectHash, inputHash, ref, algo string, err error) {
	if g.Committer == nil {
		return g.commit(agentID), g.commit(canonicalArgs(args)), "", "", nil
	}
	ref = g.subjectRef(agentID)
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
func (g *Governor) subjectRef(subject string) string {
	if g.SubjectRef != nil {
		return g.SubjectRef(subject)
	}
	return subject
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
