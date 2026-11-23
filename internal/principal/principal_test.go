package principal

import (
	"errors"
	"strings"
	"testing"
)

// Grants are per *user*, because that is what a unix socket can attest. One user routinely
// runs several agents — that is what a collection is — so a grant is a set, not a value.
func grants() Grants {
	return Grants{
		501: {"vagent-analyst", "vagent-worker"},
		502: {"vagent-admin"},
	}
}

// The single-agent case keeps working without a claim: nothing is ambiguous about it, and
// asking every existing caller to start naming itself would be a migration nobody needs.
func TestSingleAgentPrincipalNeedsNoClaim(t *testing.T) {
	agent, err := Resolve(grants(), 502, "")
	if err != nil || agent != "vagent-admin" {
		t.Fatalf("uid 502 = %q, %v; want vagent-admin", agent, err)
	}
}

// Several agents under one user: the claim is the only thing that can tell them apart, so an
// absent claim is refused rather than guessed. A guess attributes a decision to a workload
// that never made it.
func TestClaimDecidesBetweenAgentsOnOneUid(t *testing.T) {
	for _, want := range []string{"vagent-analyst", "vagent-worker"} {
		agent, err := Resolve(grants(), 501, want)
		if err != nil || agent != want {
			t.Fatalf("uid 501 claiming %s = %q, %v", want, agent, err)
		}
	}

	_, err := Resolve(grants(), 501, "")
	if err == nil {
		t.Fatal("an ambiguous peer was served without a claim")
	}
	var ambiguous ErrAmbiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("no claim from a multi-agent uid produced %T, want ErrAmbiguous", err)
	}
	// The refusal has to say what it could have been, or the operator cannot tell a typo
	// from a missing agent.
	if !strings.Contains(err.Error(), "vagent-analyst") || !strings.Contains(err.Error(), "vagent-worker") {
		t.Fatalf("refusal %q does not name the allowed agents", err)
	}
}

// Claiming an agent the peer may not run is impersonation, whether or not the peer runs other
// agents — and it must not be confused with an unmapped uid.
func TestClaimOutsideTheGrantIsImpersonation(t *testing.T) {
	for _, c := range []struct {
		uid     uint32
		claimed string
	}{
		{501, "vagent-admin"}, // another user's agent
		{502, "vagent-analyst"},
	} {
		_, err := Resolve(grants(), c.uid, c.claimed)
		if err == nil {
			t.Fatalf("uid %d was allowed to act as %s", c.uid, c.claimed)
		}
		var unmapped ErrUnmapped
		if errors.As(err, &unmapped) {
			t.Fatalf("uid %d claiming %s was reported as unmapped", c.uid, c.claimed)
		}
		var ambiguous ErrAmbiguous
		if errors.As(err, &ambiguous) {
			t.Fatalf("uid %d claiming %s was reported as ambiguous", c.uid, c.claimed)
		}
		if !strings.Contains(err.Error(), "not authorised to act as "+c.claimed) {
			t.Fatalf("refusal %q does not name the claim", err)
		}
	}
}

func TestUnmappedPeer(t *testing.T) {
	_, err := Resolve(grants(), 999, "")
	if err == nil {
		t.Fatal("an unmapped uid was served")
	}
	var unmapped ErrUnmapped
	if !errors.As(err, &unmapped) {
		t.Fatalf("unmapped uid produced %T", err)
	}
	if !strings.Contains(err.Error(), "not mapped to") {
		t.Fatalf("text %q does not say what happened", err)
	}
}

// A grant entry with no agents is a configuration error, not a permission: it must not fall
// through to "anyone on this uid may be anyone".
func TestEmptyGrantIsNotAPermission(t *testing.T) {
	empty := Grants{501: nil}
	if _, err := Resolve(empty, 501, "vagent-analyst"); err == nil {
		t.Fatal("a uid granted no agents was allowed to act as one")
	}
	if _, err := Resolve(empty, 501, ""); err == nil {
		t.Fatal("a uid granted no agents was served without a claim")
	}
}
