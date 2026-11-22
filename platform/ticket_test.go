package platform

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testKey(t *testing.T) *TicketKey {
	t.Helper()
	k, err := LoadOrCreateTicketKey(filepath.Join(t.TempDir(), "ticket.key"))
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return k
}

func TestTicketRoundTrip(t *testing.T) {
	k := testKey(t)
	tok, issued, err := k.Issue("e1", "agent-7", "cluster/prod", time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	got, err := k.Verify(tok, "e1", "agent-7", "cluster/prod")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.ID != issued.ID {
		t.Fatalf("id = %q, want %q", got.ID, issued.ID)
	}
}

// A ticket for one resource presented for another is a valid MAC over the wrong
// assertion. Returning claims without comparing them is how that passes.
func TestTicketIsBoundToItsClaims(t *testing.T) {
	k := testKey(t)
	tok, _, _ := k.Issue("e1", "agent-7", "cluster/prod", time.Minute)
	if _, err := k.Verify(tok, "e1", "agent-7", "cluster/dev"); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("wrong resource = %v, want ErrTicketInvalid", err)
	}
	if _, err := k.Verify(tok, "e2", "agent-7", "cluster/prod"); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("wrong entry = %v", err)
	}
	if _, err := k.Verify(tok, "e1", "agent-8", "cluster/prod"); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("wrong agent = %v", err)
	}
}

func TestTamperedTicketRejected(t *testing.T) {
	k := testKey(t)
	tok, _, _ := k.Issue("e1", "agent-7", "r", time.Minute)
	// Flip a byte in the payload segment.
	b := []byte(tok)
	i := len(ticketPrefix) + 4
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	if _, err := k.Verify(string(b), "e1", "agent-7", "r"); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("tampered token = %v, want ErrTicketInvalid", err)
	}
}

// Expiry is separate from forgery: a fresh ticket is the right response, and a caller
// cannot know that from an undifferentiated "invalid".
func TestExpiredTicketIsDistinguishable(t *testing.T) {
	k := testKey(t)
	tok, _, _ := k.Issue("e1", "agent-7", "r", time.Nanosecond)
	time.Sleep(20 * time.Millisecond)
	if _, err := k.Verify(tok, "e1", "agent-7", "r"); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("expired token = %v, want ErrTicketExpired", err)
	}
}

func TestAnotherKeyCannotVerify(t *testing.T) {
	k := testKey(t)
	other := NewTicketKey([]byte("0123456789abcdef0123456789abcdef"))
	tok, _, _ := k.Issue("e1", "agent-7", "r", time.Minute)
	if _, err := other.Verify(tok, "e1", "agent-7", "r"); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("foreign key verified = %v", err)
	}
}

// The key is stable across restarts: a key that changes invalidates every in-flight
// ticket, which looks exactly like a forgery wave.
func TestKeySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ticket.key")
	k1, err := LoadOrCreateTicketKey(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := k1.Issue("e1", "agent-7", "r", time.Minute)
	k2, err := LoadOrCreateTicketKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Verify(tok, "e1", "agent-7", "r"); err != nil {
		t.Fatalf("a ticket did not survive a restart: %v", err)
	}
}

func TestTicketRequiresEveryClaim(t *testing.T) {
	k := testKey(t)
	if _, _, err := k.Issue("", "agent-7", "r", 0); err == nil {
		t.Fatal("a ticket with no entry id was issued")
	}
	if _, _, err := k.Issue("e1", "", "r", 0); err == nil {
		t.Fatal("a ticket with no agent was issued")
	}
	if _, _, err := k.Issue("e1", "agent-7", "", 0); err == nil {
		t.Fatal("a ticket with no resource was issued")
	}
}

func TestTicketIDsAreUnique(t *testing.T) {
	k := testKey(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		_, tkt, err := k.Issue("e1", "agent-7", "r", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if seen[tkt.ID] {
			t.Fatalf("ticket id %q issued twice", tkt.ID)
		}
		seen[tkt.ID] = true
	}
}
