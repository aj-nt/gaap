package platform

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RefLen is the number of random bytes in a subject ref, rendered as hex.
const RefLen = 16

// RefRegistry maps a subject to the opaque handle its commitments are keyed
// under. Without one, the ledger keys commitments under the subject string
// itself, which stores the identifier in the clear and makes the whole scheme
// decoration.
//
// Two properties matter and both are tested. A ref is random, never derived from
// the subject, so an unrelated party cannot recompute it. And Forget makes the
// subject resolve to a *different* ref next time, so a person who was erased and
// then appears again cannot be linked back to the erased history.
//
// This registry is itself personal data: it is the table that maps people to
// handles. It must be erased with the subject (Forget), and an operator running
// it on disk should encrypt it at rest. That is the sovereign's key custody
// problem, not the governor's.
type RefRegistry interface {
	// Ref returns the handle for subject, minting one on first use.
	Ref(subject string) (string, error)
	// Forget drops the mapping so the subject resolves to a fresh handle.
	Forget(subject string) error
}

// MemoryRefRegistry is a RefRegistry that lives in process memory.
type MemoryRefRegistry struct {
	mu   sync.Mutex
	refs map[string]string
}

// NewMemoryRefRegistry returns an empty in-memory ref registry.
func NewMemoryRefRegistry() *MemoryRefRegistry {
	return &MemoryRefRegistry{refs: make(map[string]string)}
}

// Ref returns the handle for subject, minting a random one on first use.
func (r *MemoryRefRegistry) Ref(subject string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refLocked(subject)
}

func (r *MemoryRefRegistry) refLocked(subject string) (string, error) {
	if r.refs == nil {
		r.refs = make(map[string]string)
	}
	if ref, ok := r.refs[subject]; ok {
		return ref, nil
	}
	buf := make([]byte, RefLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint subject ref: %w", err)
	}
	ref := "ref-" + hex.EncodeToString(buf)
	r.refs[subject] = ref
	return ref, nil
}

// Forget drops the mapping for subject, so it resolves to a fresh handle.
func (r *MemoryRefRegistry) Forget(subject string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.refs, subject)
	return nil
}

func (r *MemoryRefRegistry) snapshot() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.refs))
	for subject, ref := range r.refs {
		out[subject] = ref
	}
	return out
}

func (r *MemoryRefRegistry) restore(refs map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = refs
}

// fileRefRegistryFormat is the on-disk shape. The subject keys are personal data.
type fileRefRegistryFormat struct {
	Version int               `json:"version"`
	Refs    map[string]string `json:"refs"`
}

// FileRefRegistry is a RefRegistry persisted to a JSON file, rewritten atomically
// on every mint and every Forget.
//
// The file holds subject identifiers in the clear. Treat it as a personal-data
// store: it is erased by Forget, and encrypting it at rest is the operator's job.
type FileRefRegistry struct {
	*MemoryRefRegistry
	path string
	wmu  sync.Mutex
}

// OpenFileRefRegistry opens (or creates) a persistent ref registry at path.
func OpenFileRefRegistry(path string) (*FileRefRegistry, error) {
	f := &FileRefRegistry{MemoryRefRegistry: NewMemoryRefRegistry(), path: path}
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
	var stored fileRefRegistryFormat
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("ref registry %s is not readable: %w", path, err)
	}
	if stored.Refs != nil {
		f.MemoryRefRegistry.restore(stored.Refs)
	}
	return f, nil
}

// Ref returns the handle for subject, persisting a newly minted one before
// returning it: a handle that is not durable splits a subject's records across
// two identities after a restart.
func (f *FileRefRegistry) Ref(subject string) (string, error) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	existing := f.MemoryRefRegistry.snapshot()[subject]
	ref, err := f.MemoryRefRegistry.Ref(subject)
	if err != nil {
		return "", err
	}
	if existing == "" {
		if err := f.persist(); err != nil {
			return "", err
		}
	}
	return ref, nil
}

// Forget drops the mapping for subject and rewrites the file without it.
func (f *FileRefRegistry) Forget(subject string) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if err := f.MemoryRefRegistry.Forget(subject); err != nil {
		return err
	}
	return f.persist()
}

// persist writes the registry atomically: temp file, fsync, rename, fsync dir.
func (f *FileRefRegistry) persist() error {
	stored := fileRefRegistryFormat{Version: 1, Refs: f.MemoryRefRegistry.snapshot()}
	b, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".refs-*.tmp")
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
