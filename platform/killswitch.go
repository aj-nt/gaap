package platform

import "sync"

// KillSwitch is a scoped, irreversible stop. Tripping is one-way by design:
// there is no Untrip — a kill switch that the governed can reopen is not a
// kill switch.
type KillSwitch struct {
	mu      sync.RWMutex
	tripped map[string]bool
}

// NewKillSwitch returns an all-open switch.
func NewKillSwitch() *KillSwitch {
	return &KillSwitch{tripped: make(map[string]bool)}
}

// Trip irreversibly opens the switch for the given scope (an agent ID, a
// capability, or "global"). Idempotent.
func (k *KillSwitch) Trip(scope string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.tripped[scope] = true
}

// Tripped reports whether the scope — or the global scope — is tripped.
func (k *KillSwitch) Tripped(scope string) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.tripped["global"] || k.tripped[scope]
}
