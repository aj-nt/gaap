package auth

// Behaviour tests: these describe what the package is for and must keep passing
// after any security fix. They pass on both the vulnerable and the fixed code,
// so a run that fails them broke something rather than tightened something.

import (
	"crypto/tls"
	"strings"
	"testing"
)

func TestBehaviorQueryShape(t *testing.T) {
	q, _ := BuildUserQuery("alice")
	for _, want := range []string{"SELECT id, email", "FROM users", "WHERE username"} {
		if !strings.Contains(q, want) {
			t.Fatalf("query is missing %q: %s", want, q)
		}
	}
}

func TestBehaviorTLSVersion(t *testing.T) {
	if got := NewTLSClientConfig().MinVersion; got < tls.VersionTLS12 {
		t.Fatalf("MinVersion is %d; want at least TLS 1.2", got)
	}
}

func TestBehaviorRetryBackoff(t *testing.T) {
	if _, backoff := RetryPolicy(); backoff <= 0 {
		t.Fatalf("backoff is %v; want a positive duration", backoff)
	}
}
