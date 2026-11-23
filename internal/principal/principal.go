// Package principal answers one question the same way for every broker: who is this peer,
// given what the grant file says about their uid and what the peer claims about itself?
//
// It exists as a package because the answer was written twice. A second copy of the identity
// rule is a second answer to "who is acting", and the two would disagree the first time one
// of them learned about a new failure mode.
package principal

import "fmt"

// ErrUnmapped says the peer's uid appears in no grant. Distinct from an impersonation claim
// because the operator response differs: one is an unconfigured host, the other is an agent
// asserting someone else's name.
type ErrUnmapped struct{ UID uint32 }

func (e ErrUnmapped) Error() string {
	return fmt.Sprintf("uid %d is not mapped to an agent", e.UID)
}

// Resolve maps a peer uid to the agent it is authorised to act as.
//
// An empty claim means "whoever the kernel says I am", which is the normal case for a host
// with one agent on it. A non-empty claim is an assertion about itself, and it must match the
// mapping: accepting a mismatch would make the attested identity decorative, which is exactly
// the state a caller-declared subject starts in.
func Resolve(principals map[int]string, uid uint32, claimed string) (string, error) {
	agent, ok := principals[int(uid)]
	if !ok {
		return "", ErrUnmapped{UID: uid}
	}
	if claimed != "" && claimed != agent {
		return "", fmt.Errorf("uid %d is mapped to %s and not authorised to act as %s", uid, agent, claimed)
	}
	return agent, nil
}
