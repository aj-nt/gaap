package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

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
	return k.TrippedScope(scope) != ""
}

// TrippedScope reports which scope trips the check: "global" wins over a
// narrower trip, then the scope itself, then the empty string for all-clear.
//
// The name matters. A denial that says only "kill switch tripped" reads the
// same whether one agent was stopped or the whole fleet was, and the reason
// string is the only thing an operator sees afterwards.
func (k *KillSwitch) TrippedScope(scope string) string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.tripped["global"] {
		return "global"
	}
	if k.tripped[scope] {
		return scope
	}
	return ""
}

// Scopes returns the tripped scopes, sorted.
func (k *KillSwitch) Scopes() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]string, 0, len(k.tripped))
	for scope := range k.tripped {
		out = append(out, scope)
	}
	sort.Strings(out)
	return out
}

// fileKillSwitchFormat is the on-disk shape of a FileKillSwitch. Only tripped
// scopes are stored: there is no field for "untripped", because nothing can
// clear one.
type fileKillSwitchFormat struct {
	Version int      `json:"version"`
	Tripped []string `json:"tripped"`
}

// FileKillSwitch is a KillSwitch persisted to a JSON file, rewritten atomically
// on every trip.
//
// Durability is the whole point. A stop that ends with the process that declared
// it is not a stop: the CLI builds a governor per invocation, so an in-memory
// trip would be forgotten before the next decision was asked for.
type FileKillSwitch struct {
	*KillSwitch
	path string
	wmu  sync.Mutex
}

// OpenFileKillSwitch opens (or creates) a persistent kill switch at path. A file
// that exists but cannot be parsed is an error, never an all-clear: an operator
// who believes a stop is in force must not be told otherwise by a corrupt file.
func OpenFileKillSwitch(path string) (*FileKillSwitch, error) {
	f := &FileKillSwitch{KillSwitch: NewKillSwitch(), path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return f, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return f, nil
	}
	var stored fileKillSwitchFormat
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("kill switch %s is not readable: %w", path, err)
	}
	for _, scope := range stored.Tripped {
		f.KillSwitch.Trip(scope)
	}
	return f, nil
}

// Trip opens the switch and returns only once the trip is on disk. A trip that
// reported success while still in memory would be a stop the next invocation
// does not know about.
func (f *FileKillSwitch) Trip(scope string) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()

	f.KillSwitch.Trip(scope)
	if err := f.persist(); err != nil {
		return fmt.Errorf("kill switch %s: trip %q not durable: %w", f.path, scope, err)
	}
	return nil
}

// persist rewrites the file atomically: temp file in the same directory, fsync,
// rename, fsync the directory.
func (f *FileKillSwitch) persist() error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(fileKillSwitchFormat{Version: 1, Tripped: f.Scopes()}, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".kill-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return err
	}
	return syncDir(dir)
}
