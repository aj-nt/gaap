package platform

import "time"

// ArtifactCeiling is the longest lifetime a single decision may grant without a
// human looking at it. 90 days: short enough that a forgotten grant expires
// inside a quarter, long enough that ordinary service accounts are not
// interrupted.
//
// It is a constant rather than a field on each rule because the point is a
// ceiling that exists. A per-rule ceiling that defaults to zero is a ceiling
// nobody is ever over.
const ArtifactCeiling = 90 * 24 * time.Hour

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
