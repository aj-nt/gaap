package platform

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
}

// NewGovernor returns a governor with defaults: deny-by-default policy, an
// open kill switch, trust starting at 500, a fresh ledger, and a noop enforcer
// (enforce nothing, like today — swap in a DockerEnforcer to make it coercive).
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
func (g *Governor) Decide(agentID, tool string, args map[string]any) Decision {
	var d Decision
	if g.Kill.Tripped(agentID) {
		d = Deny("kill switch tripped for " + agentID)
	} else {
		d = g.Policy.Evaluate(tool, args)
	}
	if d.Action != ActionAllow {
		g.Trust.Penalize(1)
	}
	g.record(agentID, tool, d)
	return d
}

func (g *Governor) record(agentID, tool string, d Decision) {
	g.Ledger.Append(Entry{
		Kind:          "decision",
		Subject:       agentID + "/" + tool,
		Detail:        string(d.Action) + " " + d.Reason,
		Reversibility: g.classifyTool(tool),
	})
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
