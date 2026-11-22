package platform

import (
	"strings"
	"testing"
)

const ticketPolicy = `{
  "version": 1,
  "rules": [
    {"id": "read-own", "tool": "ReadFile", "action": "allow"},
    {"id": "shell", "tool": "Execute", "action": "allow"},
    {"id": "rotate", "tool": "RotateKey", "action": "allow"}
  ]
}`

// governedWithTickets is a governor that can attest: a key, and a classifier that knows
// ReadFile is a pure read while Execute is not. Pure is the only class the kernel assigns,
// and the only one that has an observation to attest.
func governedWithTickets(t *testing.T) *Governor {
	t.Helper()
	g := governorFromPolicy(t, ticketPolicy)
	g.Tickets = NewTicketKey([]byte("0123456789abcdef0123456789abcdef"))
	g.Classify = func(tool string) Reversibility {
		if tool == "ReadFile" {
			return Pure
		}
		return Irreversible
	}
	return g
}

// The ticket is the only route to internal provenance, so the claim has to be checked:
// a token that verifies for a different entry, agent or resource is a token that transfers,
// and a transferable attestation attests nothing.
func TestAllowForAPureReadIssuesAVerifiableTicket(t *testing.T) {
	g := governedWithTickets(t)
	d := g.Decide("agent-7", "ReadFile", map[string]any{"path": "/etc/hosts"},
		WithTicketFor("e1", "/etc/hosts"))
	if d.Action != ActionAllow {
		t.Fatalf("action = %q (%s), want allow", d.Action, d.Reason)
	}
	if d.Ticket == "" {
		t.Fatal("no ticket issued for an allowed pure read")
	}
	tk, err := g.Tickets.Verify(d.Ticket, "e1", "agent-7", "/etc/hosts")
	if err != nil {
		t.Fatalf("issued ticket does not verify for its own claim: %v", err)
	}
	if tk.EntryID != "e1" || tk.Agent != "agent-7" || tk.Resource != "/etc/hosts" {
		t.Fatalf("claims = %+v", tk)
	}
	for _, bad := range []struct{ name, entry, agent, resource string }{
		{"another entry", "e2", "agent-7", "/etc/hosts"},
		{"another agent", "e1", "agent-8", "/etc/hosts"},
		{"another resource", "e1", "agent-7", "/etc/passwd"},
	} {
		if _, err := g.Tickets.Verify(d.Ticket, bad.entry, bad.agent, bad.resource); err == nil {
			t.Errorf("ticket verified for %s", bad.name)
		}
	}
}

// An allow is not enough. A call with effects has an outcome, not an observation: content
// derived from it is "what I did", and the store's internal class means "what the resource
// actually says". Issuing there would launder a mutation into a fact about the world.
func TestTicketIsRefusedForACallWithEffects(t *testing.T) {
	g := governedWithTickets(t)
	d := g.Decide("agent-7", "Execute", nil, WithTicketFor("e1", "host-3"))
	if d.Action != ActionDeny {
		t.Fatalf("action = %q (%s), want deny for a ticketed impure call", d.Action, d.Reason)
	}
	if d.Ticket != "" {
		t.Fatal("a ticket was issued for a call with effects")
	}
	if !strings.Contains(d.Reason, "pure") {
		t.Fatalf("reason = %q, want it to say the call is not a pure read", d.Reason)
	}
	// Attribution survives the refusal: a denial that loses its RuleID is one nobody can
	// trace back to a standing order.
	if d.RuleID != "shell" {
		t.Fatalf("rule id = %q, want shell", d.RuleID)
	}
}

// Asking for attestation a governor cannot provide is a refusal, not an allow without a
// ticket: the caller would record content believing it had provenance it never had.
func TestTicketIsRefusedWithoutAKey(t *testing.T) {
	g := governedWithTickets(t)
	g.Tickets = nil
	d := g.Decide("agent-7", "ReadFile", nil, WithTicketFor("e1", "/etc/hosts"))
	if d.Action != ActionDeny {
		t.Fatalf("action = %q, want deny when no key is configured", d.Action)
	}
	if !strings.Contains(d.Reason, "no ticket key") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

// A ticket is a bearer token. Minting one nobody asked for puts a credential in a decision
// record and in whatever renders it.
func TestNoTicketUnlessAskedFor(t *testing.T) {
	g := governedWithTickets(t)
	d := g.Decide("agent-7", "ReadFile", nil)
	if d.Action != ActionAllow || d.Ticket != "" {
		t.Fatalf("action = %q ticket = %q, want an allow with no ticket", d.Action, d.Ticket)
	}
}

// A decision that ends as a review authorises nothing yet, so it must not mint anything:
// the token would outlive the human's answer either way, and a rejected call would leave a
// valid ticket behind.
func TestReviewDecisionIssuesNoTicket(t *testing.T) {
	g := governedWithTickets(t)
	// Trust starts at 500 and an allow rewards it before the floor is checked, so the
	// floor has to sit above 501 to escalate.
	g.TrustFloor = 600
	d := g.Decide("agent-7", "ReadFile", nil, WithTicketFor("e1", "/etc/hosts"))
	if d.Action != ActionReview {
		t.Fatalf("action = %q, want review", d.Action)
	}
	if d.Ticket != "" {
		t.Fatal("a review decision minted a ticket")
	}
}

// A ticket needs a non-empty resource to bind to. An attestation that names nothing could
// be presented for any observation of the same entry.
func TestTicketRequiresAResource(t *testing.T) {
	g := governedWithTickets(t)
	d := g.Decide("agent-7", "ReadFile", nil, WithTicketFor("e1", ""))
	if d.Action != ActionDeny {
		t.Fatalf("action = %q (%s), want deny for a ticket with no resource", d.Action, d.Reason)
	}
}
