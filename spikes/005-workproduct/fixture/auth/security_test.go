package auth

// Security tests: each fails on the vulnerable code shipped in the fixture and
// passes once the corresponding finding is fixed. They are the grader for the
// work-product probe — the model never sees them.

import (
	"strings"
	"testing"
)

// TestQueryParameterized is the [HIGH] finding: SQL string concatenation.
func TestQueryParameterized(t *testing.T) {
	const hostile = "' OR '1'='1"
	q, args := BuildUserQuery(hostile)

	if strings.Contains(q, hostile) {
		t.Fatalf("the username is interpolated into the query: %s", q)
	}
	if !strings.Contains(q, "?") && !strings.Contains(q, "$1") {
		t.Fatalf("the query has no parameter placeholder: %s", q)
	}
	if len(args) != 1 || args[0] != hostile {
		t.Fatalf("the username is not bound as a query argument: %#v", args)
	}
}

// TestTLSVerifies is a [MED] finding: certificate validation skipped.
func TestTLSVerifies(t *testing.T) {
	if NewTLSClientConfig().InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify disables certificate validation")
	}
}

// TestRetryBounded is a [MED] finding: unbounded retry loop.
func TestRetryBounded(t *testing.T) {
	attempts, _ := RetryPolicy()
	if attempts <= 0 {
		t.Fatal("retry policy is unbounded (attempts <= 0)")
	}
	if attempts > 5 {
		t.Fatalf("retry policy retries %d times; want a small bound", attempts)
	}
}
