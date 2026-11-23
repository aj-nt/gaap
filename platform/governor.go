package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
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
	// Catalogue reports the tool names an agent actually exposes. nil = unknown, and an
	// unknown catalogue filters nothing: a kernel that guessed would hide a tool the agent
	// has, which is how a refusal teaches the caller to stop trying something it can do.
	//
	// It exists because a refusal that lists "permitted alternatives" from the policy alone
	// told a live agent to call a tool its own registry did not contain.
	Catalogue func(agentID string) []string
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
	// PreState captures a commitment to the state a destructive call replaces,
	// before the call is recorded. nil means no capturer: a rule declaring an
	// artifact of type delete then denies, because a deletion nobody can describe
	// is not recoverable and must not be recorded as though it were.
	//
	// Capturing pre-state needs no inverse. Preserving what was there is a
	// different capability from being able to put it back.
	PreState func(tool string, args map[string]any) (commitment string, err error)
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
	// Tickets mints the attestation a memory write needs before the store will
	// record its content as internal. nil = this governor cannot attest, and a
	// caller that asked for a ticket is refused rather than served without one.
	//
	// The key is the governor's because the governor is where a call is
	// authorised: an attestation is a statement about an authorised observation,
	// and the party that allowed the observation is the only one positioned to
	// say so.
	Tickets *TicketKey
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

// DecideOpts carries context a decision needs but the governor cannot derive:
// which run it belongs to, and which tasks it descends from.
//
// Both are absent on the CLI path, and empty is the honest answer there — no
// run, no parent — rather than a guess. It is an options struct because a second
// Decide signature would leave two ways to pass context to one decision, and the
// call site that forgot one is the one that records a blank.
type DecideOpts struct {
	RunID       string
	ParentTasks string
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

// WithRun attributes the decision to a run.
func WithRun(id string) DecideOpt { return func(o *DecideOpts) { o.RunID = id } }

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

// WithParents attributes the decision to the tasks it descends from. The lineage
// that exists is a list of task ids; an agent type has no parent.
func WithParents(ids ...string) DecideOpt {
	return func(o *DecideOpts) { o.ParentTasks = strings.Join(ids, ",") }
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
func (g *Governor) Decide(agentID, tool string, args map[string]any, opts ...DecideOpt) Decision {
	var do DecideOpts
	for _, opt := range opts {
		opt(&do)
	}
	var d Decision
	if scope := g.Kill.TrippedScope(agentID); scope != "" {
		// Name the scope. "kill switch tripped" reads the same whether one
		// agent was stopped or the whole fleet was.
		d = Deny("kill switch tripped: scope " + scope)
	} else {
		d = g.Policy.Evaluate(tool, args)
	}
	// Pre-state capture for a destructive write, before anything else is decided:
	// a deletion that cannot be described cannot be authorised, and recording the
	// decision after the effect would be a decision about the past.
	if d.Action != ActionDeny && needsPreState(d) {
		// Preserve the rule's attribution across the refusal: a denial that loses
		// its RuleID is a refusal nobody can trace to a standing order.
		ruleID, art, rev := d.RuleID, d.Artifact, d.Reversibility
		if g.PreState == nil {
			d = Deny(fmt.Sprintf("artifact type %q destroys something and requires a pre-state capture, but no capturer is configured", art.Type))
		} else if commitment, err := g.PreState(tool, args); err != nil {
			d = Deny("pre-state capture failed: " + err.Error())
		} else if commitment == "" {
			d = Deny("pre-state capture returned nothing, so the prior state is unknown")
		} else {
			d.PreState = commitment
		}
		d.RuleID, d.Artifact, d.Reversibility = ruleID, art, rev
	}
	if d.Action == ActionAllow {
		g.Trust.Reward(1)
		if g.TrustFloor > 0 {
			if score := g.Trust.Score(); score < g.TrustFloor {
				// Mutate rather than replace. Replacing the struct silently dropped
				// RuleID and the rule's declaration, which is what makes an
				// escalation attributable and its consequence recordable.
				d.Action = ActionReview
				d.Reason = fmt.Sprintf("trust %.0f below floor %.0f", score, g.TrustFloor)
			}
		}
	} else {
		g.Trust.Penalize(1)
	}
	// Over-ceiling promotion, separate from the trust floor because it keys on the
	// declared consequence rather than on who is calling: the standing order
	// permits the call, but not for as long as it asked for.
	if d.Action == ActionAllow && ArtifactClassOf(d.Artifact) == ArtifactOverCeiling {
		d.Action = ActionReview
		d.Reason = fmt.Sprintf("declared artifact lifetime %s exceeds the %s ceiling",
			d.Artifact.Lifetime, d.Artifact.Ceiling)
	}
	// Taint reaches the verdict, not just the row. A rule that requires internal
	// provenance for an allow is saying the effect is permitted but the reason is not:
	// an allow justified by something an agent read off a web page is the exact case
	// memory poisoning aims at, and it is indistinguishable from a well-founded allow
	// once it is in the ledger if this check does not exist.
	//
	// A review, not a deny: a human can see the evidence and decide, and a deny would
	// train the workload to stop citing its sources.
	d.Floor = do.Floor
	if d.Action == ActionAllow && TaintBelow(do.Floor, d.MinTaint) {
		floor := do.Floor
		if floor == "" {
			floor = "unreported"
		}
		d.Action = ActionReview
		d.Reason = fmt.Sprintf("rule %s requires %s provenance, justification floor is %s", d.RuleID, d.MinTaint, floor)
	}
	// A ticket attests an observation, so it is issued only where the design says: the
	// call was allowed, the workload classified it as a pure read, and the caller named
	// the observation. Everything else is refused rather than served without a ticket —
	// a caller that asked for attestation and quietly got an unattested allow would
	// record content believing it had provenance it never had.
	//
	// Placed after the trust floor, the artifact ceiling and the taint floor on purpose:
	// a decision that ended as a review has authorised nothing yet, and minting a token
	// for it would leave a valid credential behind for a call a human then rejected.
	if d.Action == ActionAllow && do.TicketEntryID != "" {
		ruleID, art, rev := d.RuleID, d.Artifact, d.Reversibility
		switch class := g.reversibilityFor(d, tool); {
		case g.Tickets == nil:
			d = Deny("a ticket was requested but no ticket key is configured")
		case do.TicketResource == "":
			d = Deny("a ticket must name the resource it attests; an unattributed attestation is presentable for any observation")
		case class != Pure:
			d = Deny(fmt.Sprintf("a ticket was requested for %s, which is %s: only a pure read leaves an observation to attest", tool, class))
		default:
			token, _, err := g.Tickets.Issue(do.TicketEntryID, agentID, do.TicketResource, 0)
			if err != nil {
				// Cannot attest, so cannot authorise what was asked for. Same rule as
				// recording: a governor that cannot do the thing it promised refuses.
				d = Deny("ticket issuance failed: " + err.Error())
			} else {
				d.Ticket = token
			}
		}
		d.RuleID, d.Artifact, d.Reversibility = ruleID, art, rev
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
			Permitted:     g.permittedFor(agentID, tool),
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
	if err := g.record(agentID, tool, args, d, do); err != nil {
		return Deny("governor could not record the decision: " + err.Error())
	}
	return d
}

// permittedFor is the policy's list of alternatives, narrowed to what the agent can actually
// call when a catalogue says. An unknown catalogue narrows nothing.
func (g *Governor) permittedFor(agentID, tool string) []string {
	permitted := g.Policy.PermittedFor(tool)
	if g.Catalogue == nil {
		return permitted
	}
	names := g.Catalogue(agentID)
	if len(names) == 0 {
		return permitted
	}
	has := make(map[string]struct{}, len(names))
	for _, n := range names {
		has[n] = struct{}{}
	}
	out := permitted[:0]
	for _, p := range permitted {
		if _, ok := has[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Refuse records a refusal the policy never saw — a tool the caller does not have, a broker
// that cannot answer. It exists so those refusals reach the ledger through the same path as a
// policy's, instead of a second implementation of record().
//
// No denial class is attached: the agent must not read this as a rule it could satisfy by
// changing the call, because the problem is not the call's content.
func (g *Governor) Refuse(agentID, tool string, args map[string]any, ruleID, reason string) Decision {
	d := Deny(reason)
	d.RuleID = ruleID
	if g.Ledger == nil {
		return d
	}
	if err := g.record(agentID, tool, args, d, DecideOpts{}); err != nil {
		return Deny("governor could not record the refusal: " + err.Error())
	}
	return d
}

// record appends the structural skeleton of a decision: what tool, which rule,
// what outcome, on which input (hashed), why — and a hash of who, never the
// identity itself.
func (g *Governor) record(agentID, tool string, args map[string]any, d Decision, o DecideOpts) error {
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
		Reversibility: g.reversibilityFor(d, tool),
		DenialClass:   denialClassOf(d),
		CorrelationID: correlationOf(d),

		ArtifactType:      artifactTypeOf(d),
		ArtifactClass:     g.artifactClassFor(d),
		ArtifactTTL:       artifactTTLOf(d),
		ArtifactCeiling:   artifactCeilingOf(d),
		ArtifactScope:     artifactScopeOf(d),
		ArtifactLocatable: d.Artifact != nil && d.Artifact.Locatable,
		ArtifactPreState:  d.PreState,
		PolicyRev:         g.Policy.Revision(),
		RunID:             o.RunID,
		ParentTasks:       o.ParentTasks,
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

// needsPreState reports whether the decision destroys something whose prior
// state must be preserved first.
//
// Delete only. Disable is not included: it is reversible in principle, and
// demanding a snapshot for it would train operators to capture pre-state for
// everything, which is how a control becomes a formality.
func needsPreState(d Decision) bool {
	return d.Artifact != nil && d.Artifact.Type == ArtifactDelete
}

// reversibilityFor resolves what the ledger records: the rule's declaration when
// there is one, the workload's classifier otherwise. Self-extension is always
// irreversible regardless of either, because loading a hook is not something an
// expiry clock reaches.
func (g *Governor) reversibilityFor(d Decision, tool string) Reversibility {
	if d.Artifact != nil && d.Artifact.Type.IsSelfExtension() {
		return Irreversible
	}
	if d.Reversibility != "" {
		return d.Reversibility
	}
	return g.classifyTool(tool)
}

// artifactClassFor characterises the consequence. A deny is not "unrecorded" —
// nothing happened, so there is no consequence and the field stays empty.
// Everything that may run records a class, including "unrecorded", because
// nobody-characterised-this is the state an operator needs to be able to query.
func (g *Governor) artifactClassFor(d Decision) string {
	if d.Action == ActionDeny || d.Action == ActionRateLimit {
		return ""
	}
	return string(ArtifactClassOf(d.Artifact))
}

func artifactTypeOf(d Decision) string {
	if d.Artifact == nil {
		return ""
	}
	return string(d.Artifact.Type)
}

// artifactTTLOf renders the declared lifetime in the form a person reads in
// `ledger tail`. Zero is omitted rather than written as "0s": no declared
// lifetime is the absence of a bound, and "0s" would read as an expiry.
func artifactTTLOf(d Decision) string {
	if d.Artifact == nil || d.Artifact.Lifetime <= 0 {
		return ""
	}
	return d.Artifact.Lifetime.String()
}

func artifactCeilingOf(d Decision) string {
	if d.Artifact == nil || d.Artifact.Ceiling <= 0 {
		return ""
	}
	return d.Artifact.Ceiling.String()
}

func artifactScopeOf(d Decision) string {
	if d.Artifact == nil {
		return ""
	}
	return d.Artifact.Scope
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
