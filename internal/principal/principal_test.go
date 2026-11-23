package principal

import (
	"errors"
	"strings"
	"testing"
)

// The rule decides who the store is opened for and who the ledger names. It used to be three
// statements inside a unix-socket handler, reachable only by a real peer uid, which is why it
// had no test; it is one function now, and this is the test that travelled with it.
func TestResolve(t *testing.T) {
	principals := map[int]string{501: "agent-a", 502: "agent-b"}

	// The uid is the identity; an absent claim is not a problem.
	agent, err := Resolve(principals, 501, "")
	if err != nil || agent != "agent-a" {
		t.Fatalf("uid 501 = %q, %v; want agent-a", agent, err)
	}
	// A claim that agrees is allowed and changes nothing.
	if agent, err = Resolve(principals, 501, "agent-a"); err != nil || agent != "agent-a" {
		t.Fatalf("uid 501 claiming agent-a = %q, %v", agent, err)
	}

	// No mapping: unidentified, not anonymous. Refused, and told apart from impersonation
	// because the operator log has to say which happened.
	_, err = Resolve(principals, 999, "")
	if err == nil {
		t.Fatal("an unmapped uid was served")
	}
	var unmapped ErrUnmapped
	if !errors.As(err, &unmapped) {
		t.Fatalf("unmapped uid produced %T, want ErrUnmapped", err)
	}
	if !strings.Contains(err.Error(), "not mapped to an agent") {
		t.Fatalf("response text %q does not say what happened", err)
	}

	// A mapped uid claiming to be another agent is impersonation, not a typo.
	_, err = Resolve(principals, 501, "agent-b")
	if err == nil {
		t.Fatal("uid 501 was allowed to act as agent-b")
	}
	if errors.As(err, &unmapped) {
		t.Fatalf("impersonation was reported as an unmapped uid: %v", err)
	}
	if !strings.Contains(err.Error(), "not authorised to act as agent-b") {
		t.Fatalf("response text %q does not name the claim", err)
	}
}
