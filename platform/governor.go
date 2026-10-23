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
	// Hash commits to a value without storing it. nil = sha256. A sovereign that
	// holds a key can inject an HMAC so the commitment cannot be brute-forced by
	// an outside party; that key then becomes a single point of failure for
	// every commitment it made.
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
	_, err := g.Ledger.Append(Entry{
		Kind:          "decision",
		SubjectHash:   g.commit(agentID),
		Tool:          tool,
		RuleID:        d.RuleID,
		Outcome:       d.Action,
		InputHash:     g.commit(canonicalArgs(args)),
		Reason:        d.Reason,
		Reversibility: g.classifyTool(tool),
	})
	return err
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
