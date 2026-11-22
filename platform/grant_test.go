package platform

import (
	"strings"
	"testing"
	"time"
)

func TestPathGrantCarriesChown(t *testing.T) {
	g := PathGrant{Path: "/data/foo", Mode: "rw", UID: 10001, ExpiresAt: time.Now().Add(time.Hour)}
	if g.ChownTarget() != "10001:10001" {
		t.Fatalf("ChownTarget = %q, want 10001:10001", g.ChownTarget())
	}
	if g.ChownTarget() == "" {
		t.Fatal("path grant must specify its chown target")
	}
}

// A grant is standing authority to write to a host path, created by one transient
// decision. Without an end it outlives the decision that made it and nothing in
// the system can tell that it should have stopped.
func TestPathGrantWithoutExpiryRefused(t *testing.T) {
	g := PathGrant{Path: "/data/foo", Mode: "rw", UID: 10001}
	err := g.Validate(time.Now())
	if err == nil {
		t.Fatal("Validate accepted a grant with no expiry")
	}
	if !strings.Contains(err.Error(), "expiry") {
		t.Fatalf("error = %q, want it to name the missing expiry", err)
	}
}

func TestPathGrantExpiredRefused(t *testing.T) {
	now := time.Unix(1700000000, 0)
	g := PathGrant{Path: "/data/foo", Mode: "rw", UID: 10001, ExpiresAt: now.Add(-time.Second)}
	err := g.Validate(now)
	if err == nil {
		t.Fatal("Validate accepted an expired grant")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error = %q, want it to name the expiry", err)
	}
}

func TestPathGrantValidBeforeExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	g := PathGrant{Path: "/data/foo", Mode: "rw", UID: 10001, ExpiresAt: now.Add(time.Minute)}
	if err := g.Validate(now); err != nil {
		t.Fatalf("Validate refused a live grant: %v", err)
	}
}
