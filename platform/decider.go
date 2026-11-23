package platform

// Decider is the one method the enforcement path needs from the kernel.
//
// An agent that decides its own calls governs itself: the policy it reads, the record it
// writes and the process that runs the tool are all the same process, so anything that can
// run code there can also permit itself. That is the arrangement every framework surveyed on
// 2026-11-22 drifted back to, and one of them deleted its scope check on the way.
//
// The interface exists so the decision can live in a daemon while the agent holds a client:
// same signature, different answer to "can the workload rewrite the rule that just refused
// it". Nothing about Decision changes — the caller still gets a struct, a rule id and a
// correlation id, and the ledger still belongs to whoever made the call.
type Decider interface {
	Decide(agentID, tool string, args map[string]any, opts ...DecideOpt) Decision
}

// *Governor satisfies the interface for free; the assertion is here so a future change to
// Decide's signature breaks the client and the daemon together instead of drifting apart.
var _ Decider = (*Governor)(nil)
