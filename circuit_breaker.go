package gaap

import (
	"sync"
	"time"
)

// CBState represents the state of a circuit breaker.
type CBState string

const (
	StateClosed   CBState = "closed"
	StateOpen     CBState = "open"
	StateHalfOpen CBState = "half_open"
)

// CircuitBreaker tracks agent-type failures and prevents dispatching
// tasks to agent types that are repeatedly failing. Implements the
// standard closed → open → half-open → closed/open pattern with a
// sliding failure window.
type CircuitBreaker struct {
	agentType string
	threshold int
	cooldown  time.Duration
	window    time.Duration

	mu             sync.Mutex
	state          CBState
	failures       []time.Time
	openedAt       time.Time
	halfOpenCount  int
	halfOpenFailed bool
}

// NewCircuitBreaker creates a circuit breaker for an agent type.
// threshold: number of failures within window to trip
// cooldown: time to wait in open state before allowing probe requests
func NewCircuitBreaker(agentType string, threshold int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		agentType: agentType,
		threshold: threshold,
		cooldown:  cooldown,
		window:    cooldown, // same duration used for failure window
		state:     StateClosed,
	}
}

// State returns the current circuit breaker state.
func (cb *CircuitBreaker) State() CBState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// AgentType returns the agent type this breaker monitors.
func (cb *CircuitBreaker) AgentType() string {
	return cb.agentType
}

// FailureCount returns the number of failures in the current window.
func (cb *CircuitBreaker) FailureCount() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.trimFailures()
	return len(cb.failures)
}

// AllowRequest returns true if a request to this agent type should be
// allowed through. Returns false when the circuit is open.
func (cb *CircuitBreaker) AllowRequest() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(cb.openedAt) >= cb.cooldown {
			cb.state = StateHalfOpen
			cb.halfOpenCount = 0
			cb.halfOpenFailed = false
			return true
		}
		return false
	case StateHalfOpen:
		cb.halfOpenCount++
		return true
	default:
		return true
	}
}

// RecordFailure records a failure for this agent type. If the failure
// count reaches the threshold, the circuit trips to open. If in
// half-open state, re-trips immediately.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	cb.failures = append(cb.failures, now)
	cb.trimFailures()

	switch cb.state {
	case StateHalfOpen:
		// Any failure in half-open re-trips
		cb.state = StateOpen
		cb.openedAt = now
		cb.halfOpenFailed = true
	case StateClosed:
		if len(cb.failures) >= cb.threshold {
			cb.state = StateOpen
			cb.openedAt = now
		}
	}
}

// RecordSuccess records a successful task completion. In half-open
// state, this resets the breaker to closed.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == StateHalfOpen {
		cb.state = StateClosed
		cb.failures = nil
		cb.halfOpenCount = 0
		cb.halfOpenFailed = false
	}
}

// trimFailures removes failures older than the window.
// Must be called with mu held.
func (cb *CircuitBreaker) trimFailures() {
	cutoff := time.Now().Add(-cb.window)
	kept := 0
	for _, t := range cb.failures {
		if t.After(cutoff) {
			cb.failures[kept] = t
			kept++
		}
	}
	cb.failures = cb.failures[:kept]
}
