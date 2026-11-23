package platform

// TaintBelow reports whether floor is weaker than required. An empty floor means "no
// observation", which is not the same as "observed external": the first is ignorance and
// the second is a fact about the justification. Both fail a requirement, but only the
// second is attributable.
func TaintBelow(floor, required TaintClass) bool {
	if required == "" {
		return false
	}
	return floor.Rank() < required.Rank()
}
