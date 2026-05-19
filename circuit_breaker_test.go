package gaap

import (
	"testing"
	"time"
)

func TestCircuitBreakerTripsAfterThreshold(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker("static_analysis", 3, 1*time.Hour)

	// First two failures — still closed
	for i := 0; i < 2; i++ {
		cb.RecordFailure()
		if cb.State() != StateClosed {
			t.Errorf("failure %d: expected closed, got %s", i+1, cb.State())
		}
	}

	// Third failure trips to open
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Errorf("after threshold: expected open, got %s", cb.State())
	}
}

func TestCircuitBreakerAllowRequest(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker("static_analysis", 2, 100*time.Millisecond)

	// Allow when closed
	if !cb.AllowRequest() {
		t.Error("expected request allowed when closed")
	}

	// Trip to open
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.AllowRequest() {
		t.Error("expected request blocked when open")
	}

	// Wait for cooldown → half-open
	time.Sleep(150 * time.Millisecond)
	if !cb.AllowRequest() {
		t.Error("expected request allowed when half-open")
	}
	// After half-open, the next request should also be allowed (test probe)
	if !cb.AllowRequest() {
		t.Error("expected request allowed for half-open probe")
	}
}

func TestCircuitBreakerHalfOpenRecovery(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker("static_analysis", 1, 50*time.Millisecond)

	// Trip
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Fatalf("expected open, got %s", cb.State())
	}

	// Wait for cooldown
	time.Sleep(100 * time.Millisecond)

	// Half-open — allow a probe request
	if !cb.AllowRequest() {
		t.Fatal("expected probe allowed in half-open")
	}

	// Success resets to closed
	cb.RecordSuccess()
	if cb.State() != StateClosed {
		t.Errorf("expected closed after success, got %s", cb.State())
	}
}

func TestCircuitBreakerHalfOpenReTrip(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker("static_analysis", 1, 50*time.Millisecond)

	// Trip
	cb.RecordFailure()
	time.Sleep(100 * time.Millisecond)

	// Half-open probe
	if !cb.AllowRequest() {
		t.Fatal("expected probe allowed in half-open")
	}

	// Failure in half-open re-trips
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Errorf("expected re-trip to open, got %s", cb.State())
	}
}

func TestCircuitBreakerRecordsFailure(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker("quality_scan", 3, 1*time.Hour)

	if cb.FailureCount() != 0 {
		t.Errorf("expected 0 failures, got %d", cb.FailureCount())
	}

	cb.RecordFailure()
	cb.RecordFailure()
	if cb.FailureCount() != 2 {
		t.Errorf("expected 2 failures, got %d", cb.FailureCount())
	}
}

func TestCircuitBreakerAgentType(t *testing.T) {
	t.Parallel()
	cb := NewCircuitBreaker("quality_scan", 2, 1*time.Hour)

	if cb.AgentType() != "quality_scan" {
		t.Errorf("expected quality_scan, got %s", cb.AgentType())
	}
}

func TestCircuitBreakerWindowReset(t *testing.T) {
	t.Parallel()
	// Very short window — failures expire quickly
	cb := NewCircuitBreaker("static_analysis", 5, 10*time.Millisecond)

	cb.RecordFailure()
	cb.RecordFailure()

	time.Sleep(50 * time.Millisecond)

	// Old failures should have expired — need threshold+1 to trip
	cb.RecordFailure()
	if cb.State() != StateClosed {
		t.Errorf("expected closed after window expiry, got %s (failures: %d)", cb.State(), cb.FailureCount())
	}
}
