package protocol

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
//
// It lives here rather than with the governor because it is the contract a remote client is
// written against: a daemon in another process satisfies it over a socket, and the client
// should not have to import the machinery that answers it.
type Decider interface {
	Decide(agentID, tool string, args map[string]any, opts ...DecideOpt) Decision
}

// DecideOpts carries context a decision needs but the governor cannot derive:
// It is an options struct because a second Decide signature would leave two ways
// to pass context to one decision, and the call site that forgot one is the one
// that records a blank.
type DecideOpts struct {
	// Floor is the worst provenance class present in the justification chain,
	// computed by the caller — including anything the caller read out of memory.
	// The governor does not derive it: provenance lives where the evidence is, and a
	// governor that guessed would be certifying its own guess.
	Floor TaintClass
	// TicketEntryID and TicketResource name the observation a ticket would attest:
	// "entry E, written by agent A, about resource R". Both empty means no ticket was
	// asked for, which is the default — a token is never minted for an allow that did
	// not say which observation it authorises, because then it would authorise any.
	TicketEntryID  string
	TicketResource string
}

// DecideOpt adjusts the context of one decision.
type DecideOpt func(*DecideOpts)

// WithFloor supplies the justification's provenance floor. It travels in the option
// set introduced in M5.4 rather than as another positional parameter: two ways to pass
// context to one decision is how a call site ends up silently recording a blank.
func WithFloor(floor TaintClass) DecideOpt { return func(o *DecideOpts) { o.Floor = floor } }

// WithTicketFor asks for an attestation covering one observation. The governor issues it
// only if the call is allowed and pure; otherwise the decision is a refusal, so the caller
// cannot end up holding an allow it believes was attested when it was not.
func WithTicketFor(entryID, resource string) DecideOpt {
	return func(o *DecideOpts) {
		o.TicketEntryID = entryID
		o.TicketResource = resource
	}
}
