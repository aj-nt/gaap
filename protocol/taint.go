package protocol

// TaintClass is how far a piece of justification can be trusted, ordered worst to
// best. It is the vocabulary the governor, the memory store and the ledger share: three
// modules that each spelling their own strings is how "external" in one place stops
// meaning "external" in another, and the drift is silent.
type TaintClass string

const (
	// TaintExternal: came from outside the boundary. A tool result, a fetched page, an
	// agent's own summary of either.
	TaintExternal TaintClass = "external"
	// TaintInternal: came from a ticketed call about the agent's own resources — the
	// platform attested that this is what the resource actually says.
	TaintInternal TaintClass = "internal"
	// TaintFact: two or more independent roots agree, and a principal other than the
	// producer said so.
	TaintFact TaintClass = "fact"
)

// Rank orders the classes. Higher is better; an unknown class ranks as external so a
// typo can never make a justification look stronger than it is.
func (t TaintClass) Rank() int {
	switch t {
	case TaintInternal:
		return 1
	case TaintFact:
		return 2
	default:
		return 0
	}
}

// Known reports whether the class is declared.
func (t TaintClass) Known() bool {
	switch t {
	case TaintExternal, TaintInternal, TaintFact:
		return true
	}
	return false
}
