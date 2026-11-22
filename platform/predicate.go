package platform

import (
	"fmt"
	"strconv"
	"strings"
)

// PredicateClass separates what the operator CONTROLS from what the observed
// party chooses.
//
// This is the whole point of the taxonomy. A user agent string, a filename and a
// payload hash are authored by whoever is being matched, so a rule resting on
// them is matched by anyone who reads the rule — and a deny-list of them catches
// nothing but the honest. A stable predicate names something the caller would
// have to change their behaviour to change: the tool, the effect, the identity,
// the volume, the sequence.
type PredicateClass string

const (
	PredicateStable   PredicateClass = "stable"
	PredicateCosmetic PredicateClass = "cosmetic"
)

// PredicateKind is the argument a predicate reads. The class is derived from the
// kind and cannot be declared: an author-declared class that disagrees with the
// kind is a lie with a plausible shape, and there is no case where the author
// knows better than the table.
type PredicateKind string

const (
	// Stable.
	PredTool     PredicateKind = "tool"     // the tool being called
	PredEffect   PredicateKind = "effect"   // what the call does, in the caller's own vocabulary
	PredIdentity PredicateKind = "identity" // which principal
	PredVolume   PredicateKind = "volume"   // how much, as a numeric comparison
	PredSequence PredicateKind = "sequence" // position in an ordered traversal

	// Cosmetic.
	PredUA       PredicateKind = "user_agent"
	PredFilename PredicateKind = "filename"
	PredHash     PredicateKind = "hash"
)

var predicateKinds = map[PredicateKind]PredicateClass{
	PredTool:     PredicateStable,
	PredEffect:   PredicateStable,
	PredIdentity: PredicateStable,
	PredVolume:   PredicateStable,
	PredSequence: PredicateStable,
	PredUA:       PredicateCosmetic,
	PredFilename: PredicateCosmetic,
	PredHash:     PredicateCosmetic,
}

// Known reports whether k is a declared predicate kind.
func (k PredicateKind) Known() bool {
	_, ok := predicateKinds[k]
	return ok
}

// Class returns stable or cosmetic. An unknown kind is cosmetic, which is the
// safe direction: it can never make a deny look well-founded.
func (k PredicateKind) Class() PredicateClass {
	if c, ok := predicateKinds[k]; ok {
		return c
	}
	return PredicateCosmetic
}

// Predicate is one condition a rule requires.
type Predicate struct {
	Kind  PredicateKind
	Value string
}

func (p Predicate) String() string {
	return string(p.Kind) + "=" + p.Value
}

// matches evaluates the predicate against a call. All predicates on a rule must
// match for the rule to fire; there is no OR because a disjunction of predicates
// is how a rule stops meaning anything specific.
//
// value syntax: exact string match, or one of >=, <=, >, < followed by a number
// for numeric arguments. Exact match only would make `volume` useless, since the
// interesting volumes are thresholds rather than one number.
func (p Predicate) matches(tool string, args map[string]any) bool {
	var raw any
	if p.Kind == PredTool {
		raw = tool
	} else {
		raw = args[string(p.Kind)]
	}
	if raw == nil {
		return false
	}

	op, threshold := splitComparison(p.Value)
	if op == "" {
		return scalarString(raw) == p.Value
	}
	n, ok := scalarNumber(raw)
	if !ok {
		return false
	}
	switch op {
	case ">=":
		return n >= threshold
	case "<=":
		return n <= threshold
	case ">":
		return n > threshold
	case "<":
		return n < threshold
	}
	return false
}

// splitComparison pulls a leading operator off a value. Only a recognised
// operator counts, so a literal value like "<script>" is not silently parsed as a
// comparison.
func splitComparison(v string) (string, float64) {
	for _, op := range []string{">=", "<=", ">", "<"} {
		if strings.HasPrefix(v, op) {
			n, err := strconv.ParseFloat(strings.TrimSpace(v[len(op):]), 64)
			if err != nil {
				return "", 0
			}
			return op, n
		}
	}
	return "", 0
}

func scalarString(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		return strconv.FormatBool(typed)
	}
	return fmt.Sprint(v)
}

func scalarNumber(v any) (float64, bool) {
	switch typed := v.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case string:
		n, err := strconv.ParseFloat(typed, 64)
		return n, err == nil
	}
	return 0, false
}

// PredicatesAreCosmeticOnly reports whether a set of predicates rests entirely on
// what the observed party authors.
//
// An empty set is NOT cosmetic-only: a rule that names a tool already has a stable
// predicate — the tool itself — and treating "no predicates" as cosmetic would
// refuse every ordinary deny rule in existence.
func PredicatesAreCosmeticOnly(ps []Predicate) bool {
	if len(ps) == 0 {
		return false
	}
	for _, p := range ps {
		if p.Kind.Class() == PredicateStable {
			return false
		}
	}
	return true
}
