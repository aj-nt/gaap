// Package principal answers one question the same way for every broker: who is this peer,
// given what the grant file says about their uid and what the peer claims about itself?
//
// It exists as a package because the answer was written twice. A second copy of the identity
// rule is a second answer to "who is acting", and the two would disagree the first time one
// of them learned about a new failure mode.
package principal

import (
	"fmt"
	"sort"
	"strings"
)

// Grants maps a peer uid to the agent instances that uid may act as.
//
// A set, not a value, because that is the granularity the kernel actually offers: a unix
// socket attests a *user*, and one user routinely runs several agents — which is what a
// collection is. Mapping a uid to exactly one agent models container-per-agent deployment and
// silently breaks the ordinary case of two agent processes under one login.
type Grants map[int][]string

// ErrUnmapped says the peer's uid appears in no grant. Distinct from an impersonation claim
// because the operator response differs: one is an unconfigured host, the other is an agent
// asserting someone else's name.
type ErrUnmapped struct{ UID uint32 }

func (e ErrUnmapped) Error() string {
	return fmt.Sprintf("uid %d is not mapped to an agent", e.UID)
}

// ErrEmptyGrant is a uid that is configured with no agents at all: a broken grant file, not a
// permission. Kept separate from ErrUnmapped so the operator sees the difference between "I
// never configured you" and "I configured you with nothing".
type ErrEmptyGrant struct{ UID uint32 }

func (e ErrEmptyGrant) Error() string {
	return fmt.Sprintf("uid %d is granted no agents", e.UID)
}

// ErrAmbiguous is a peer that may act as several agents and did not say which. Guessing would
// attribute a decision to a workload that never made it, and the ledger has no way to take
// that back.
type ErrAmbiguous struct {
	UID     uint32
	Allowed []string
}

func (e ErrAmbiguous) Error() string {
	return fmt.Sprintf("uid %d may act as any of %s; the request must name one in its agent field",
		e.UID, strings.Join(sorted(e.Allowed), ", "))
}

// ErrImpersonation is a claim outside the grant — another user's agent, or a name nobody was
// given.
type ErrImpersonation struct {
	UID     uint32
	Claimed string
	Allowed []string
}

func (e ErrImpersonation) Error() string {
	return fmt.Sprintf("uid %d is mapped to %s and not authorised to act as %s",
		e.UID, strings.Join(sorted(e.Allowed), ", "), e.Claimed)
}

// Resolve maps a peer uid to the agent it is authorised to act as.
//
// The claim is an assertion about itself, never an identity: it can only narrow the grant, not
// widen it. With one agent on the uid an absent claim is unambiguous and allowed; with several
// it is refused, because the alternative is choosing the agent for the caller.
func Resolve(grants Grants, uid uint32, claimed string) (string, error) {
	allowed, known := grants[int(uid)]
	// Unknown comes first: a uid nobody configured is unidentified, and reporting its claim as
	// impersonation would name an empty grant and read as if the peer had one.
	if !known {
		return "", ErrUnmapped{UID: uid}
	}
	if claimed != "" {
		for _, a := range allowed {
			if a == claimed {
				return a, nil
			}
		}
		return "", ErrImpersonation{UID: uid, Claimed: claimed, Allowed: allowed}
	}
	switch len(allowed) {
	case 0:
		return "", ErrEmptyGrant{UID: uid}
	case 1:
		return allowed[0], nil
	default:
		return "", ErrAmbiguous{UID: uid, Allowed: allowed}
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
