package platform

import (
	"sync"
	"time"
)

// TrustManager tracks a single scalar trust score in [0, 1000]. It is signal,
// not coercion — the one load-bearing consumer is the host-access request path
// (built in a later milestone), where it is the only guard on the agent's
// testimony.
type TrustManager struct {
	mu                sync.Mutex
	score             float64
	min               float64
	max               float64
	lastDecay         time.Time
	decayRate         float64 // points lost per hour of inactivity
	penaltyMultiplier float64 // penalties bite harder than rewards (asymmetric)
}

// NewTrustManager returns a manager starting at initial, clamped to [0, 1000].
func NewTrustManager(initial float64) *TrustManager {
	return &TrustManager{
		score:             initial,
		min:               0,
		max:               1000,
		lastDecay:         time.Now(),
		decayRate:         10,
		penaltyMultiplier: 1.5,
	}
}

// Reward increases the score by amount.
func (t *TrustManager) Reward(amount float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.decayLocked()
	t.score += amount
	t.clampLocked()
}

// Penalize decreases the score by amount * penaltyMultiplier.
func (t *TrustManager) Penalize(amount float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.decayLocked()
	t.score -= amount * t.penaltyMultiplier
	t.clampLocked()
}

// Score returns the current score after applying time-based decay.
func (t *TrustManager) Score() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.decayLocked()
	return t.score
}

func (t *TrustManager) decayLocked() {
	now := time.Now()
	// Decay is granular to whole hours of inactivity — sub-hour drift is
	// meaningless noise for a trust signal and makes the score unstable on
	// every read.
	hours := now.Sub(t.lastDecay).Hours()
	if hours < 1 {
		return
	}
	t.score -= hours * t.decayRate
	t.lastDecay = now
	t.clampLocked()
}

func (t *TrustManager) clampLocked() {
	if t.score < t.min {
		t.score = t.min
	}
	if t.score > t.max {
		t.score = t.max
	}
}
