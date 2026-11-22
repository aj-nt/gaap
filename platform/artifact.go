package platform

import "time"

// ArtifactType names the kind of durable thing a call creates or changes.
//
// The distinction that matters is not "did this write a file" but "what will
// still exist tomorrow that the operator did not ask for". A credential, a
// grant, a hooked shell and a loaded skill are the same hazard at different
// layers: each outlives the run that made it.
type ArtifactType string

const (
	ArtifactCredential ArtifactType = "credential"
	ArtifactConsent    ArtifactType = "consent"
	ArtifactRole       ArtifactType = "role"
	ArtifactAccount    ArtifactType = "account"
	ArtifactMemory     ArtifactType = "memory"
	ArtifactSchedule   ArtifactType = "schedule"
	ArtifactWebhook    ArtifactType = "webhook"
	ArtifactPolicy     ArtifactType = "policy"
	ArtifactFile       ArtifactType = "file"
	// Self-extension: the agent growing its own capabilities. These are never
	// bounded and never pure — see IsSelfExtension.
	ArtifactSkill   ArtifactType = "skill"
	ArtifactHook    ArtifactType = "hook"
	ArtifactMCP     ArtifactType = "mcp"
	ArtifactToolDef ArtifactType = "tool_definition"
	ArtifactDelete  ArtifactType = "delete"  // removes a pre-existing artifact
	ArtifactDisable ArtifactType = "disable" // disables one, reversibly in principle
)

// ArtifactCeiling is the longest lifetime a single decision may grant without a
// human looking at it. 90 days: short enough that a forgotten grant expires
// inside a quarter, long enough that ordinary service accounts are not
// interrupted.
//
// It is a constant rather than a field on each rule because the point is a
// ceiling that exists. A per-rule ceiling that defaults to zero is a ceiling
// nobody is ever over.
const ArtifactCeiling = 90 * 24 * time.Hour

// artifactTypes is the closed set the policy file may declare. A load-time check
// needs the list, and a typo ("credentail") must fail rather than create a
// bucket nothing else shares.
var artifactTypes = map[ArtifactType]bool{
	ArtifactCredential: true, ArtifactConsent: true, ArtifactRole: true,
	ArtifactAccount: true, ArtifactMemory: true, ArtifactSchedule: true,
	ArtifactWebhook: true, ArtifactPolicy: true, ArtifactFile: true,
	ArtifactSkill: true, ArtifactHook: true, ArtifactMCP: true,
	ArtifactToolDef: true, ArtifactDelete: true, ArtifactDisable: true,
}

// Known reports whether t is a declared artifact type.
func (t ArtifactType) Known() bool { return artifactTypes[t] }

// IsSelfExtension reports whether the artifact extends the agent's own
// capabilities — a skill, a hook, an MCP server, a tool definition.
//
// These are treated as permanent regardless of any declared lifetime: a loaded
// hook or skill is not something an expiry clock reaches, and the whole point of
// an agent writing one is that it survives the run that wrote it.
func (t ArtifactType) IsSelfExtension() bool {
	switch t {
	case ArtifactSkill, ArtifactHook, ArtifactMCP, ArtifactToolDef:
		return true
	}
	return false
}

// Artifact is a durable consequence, described without holding it.
//
// It carries commitments and handles, never values: the ledger must not become
// the place credentials leak from.
type Artifact struct {
	// Type is what kind of thing this is.
	Type ArtifactType
	// Lifetime is what the rule declared; zero means none was declared, which
	// means the thing does not expire on its own.
	Lifetime time.Duration
	// Ceiling is the ceiling in force for this decision; zero means none.
	Ceiling time.Duration
	// Scope is where the consequence lands: tenant|run|host|repo.
	Scope string
	// Ref is a keyed commitment to the artifact, never its value.
	Ref string
	// Handle points into the ContentStore for anything that must be revocable.
	// Never written to the ledger.
	Handle string
	// Locatable says whether the operator can actually find the thing again. An
	// artifact nobody can enumerate is not a containment surface, and saying so
	// is the difference between a plan and a claim.
	Locatable bool
}

// ArtifactClass is the coarse judgement about a consequence, and the four values
// are the whole vocabulary because they answer one question: what does the
// operator have to do about this later?
type ArtifactClass string

const (
	// ArtifactUnrecorded: nobody characterised this consequence. The dangerous
	// state, and therefore a value rather than an absence — it has to be
	// queryable.
	ArtifactUnrecorded ArtifactClass = "unrecorded"
	// ArtifactBounded: it expires, on its own, within the ceiling.
	ArtifactBounded ArtifactClass = "bounded"
	// ArtifactOverCeiling: it expires, but not before the ceiling says it should
	// have. This is what promotes an allow to a review.
	ArtifactOverCeiling ArtifactClass = "over_ceiling"
	// ArtifactPermanent: it does not expire, either because nothing was declared
	// or because it is self-extension.
	ArtifactPermanent ArtifactClass = "permanent"
)

// ArtifactClassOf classifies a consequence.
//
// A free function, not a method: Unrecorded is the nil case, and a method on a
// value can never observe its own absence. A method would make one of the four
// values unreachable while looking total.
func ArtifactClassOf(a *Artifact) ArtifactClass {
	if a == nil {
		return ArtifactUnrecorded
	}
	if a.Type.IsSelfExtension() {
		// Declared lifetimes on a hook are theatre: nothing expires it.
		return ArtifactPermanent
	}
	if a.Lifetime <= 0 {
		return ArtifactPermanent
	}
	if a.Ceiling > 0 && a.Lifetime > a.Ceiling {
		return ArtifactOverCeiling
	}
	return ArtifactBounded
}
