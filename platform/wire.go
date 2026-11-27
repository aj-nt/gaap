package platform

import "github.com/aj-nt/gaap/protocol"

// The wire vocabulary lives in github.com/aj-nt/gaap/protocol so a client can name a
// Decision, a Reversibility or a TaintClass without importing the policy engine, the
// ledger and the enforcer that produce them. While the two lived together, a client's
// pin meant "a kernel release" and moved whenever the kernel did.
//
// They are aliases, not copies: a platform.Decision IS a protocol.Decision, so a value
// crosses between the kernel and a remote client with no conversion and no chance of two
// definitions drifting apart. Everything below exists so existing call sites keep
// compiling unchanged; new code may import either name.

type (
	Action        = protocol.Action
	Decision      = protocol.Decision
	ActionClass   = protocol.ActionClass
	DenialClass   = protocol.DenialClass
	Reversibility = protocol.Reversibility
	ArtifactType  = protocol.ArtifactType
	Artifact      = protocol.Artifact
	ArtifactClass = protocol.ArtifactClass
	TaintClass    = protocol.TaintClass
	Decider       = protocol.Decider
	DecideOpt     = protocol.DecideOpt
	DecideOpts    = protocol.DecideOpts
)

const (
	ActionAllow     = protocol.ActionAllow
	ActionDeny      = protocol.ActionDeny
	ActionReview    = protocol.ActionReview
	ActionRateLimit = protocol.ActionRateLimit

	ClassShell         = protocol.ClassShell
	ClassFileWrite     = protocol.ClassFileWrite
	ClassNetwork       = protocol.ClassNetwork
	ClassMemory        = protocol.ClassMemory
	ClassSelfExtension = protocol.ClassSelfExtension

	Pure         = protocol.Pure
	Reversible   = protocol.Reversible
	Compensable  = protocol.Compensable
	Irreversible = protocol.Irreversible

	ArtifactCredential = protocol.ArtifactCredential
	ArtifactConsent    = protocol.ArtifactConsent
	ArtifactRole       = protocol.ArtifactRole
	ArtifactAccount    = protocol.ArtifactAccount
	ArtifactMemory     = protocol.ArtifactMemory
	ArtifactSchedule   = protocol.ArtifactSchedule
	ArtifactWebhook    = protocol.ArtifactWebhook
	ArtifactPolicy     = protocol.ArtifactPolicy
	ArtifactFile       = protocol.ArtifactFile
	ArtifactSkill      = protocol.ArtifactSkill
	ArtifactHook       = protocol.ArtifactHook
	ArtifactMCP        = protocol.ArtifactMCP
	ArtifactToolDef    = protocol.ArtifactToolDef
	ArtifactDelete     = protocol.ArtifactDelete
	ArtifactDisable    = protocol.ArtifactDisable

	ArtifactUnrecorded  = protocol.ArtifactUnrecorded
	ArtifactBounded     = protocol.ArtifactBounded
	ArtifactOverCeiling = protocol.ArtifactOverCeiling
	ArtifactPermanent   = protocol.ArtifactPermanent

	TaintExternal = protocol.TaintExternal
	TaintInternal = protocol.TaintInternal
	TaintFact     = protocol.TaintFact
)

// Allow is the trivial allow decision.
var Allow = protocol.Allow

// Deny returns a deny decision with a reason.
func Deny(reason string) Decision { return protocol.Deny(reason) }

// WithFloor supplies the justification's provenance floor.
func WithFloor(floor TaintClass) DecideOpt { return protocol.WithFloor(floor) }

// WithTicketFor asks for an attestation covering one observation.
func WithTicketFor(entryID, resource string) DecideOpt {
	return protocol.WithTicketFor(entryID, resource)
}
