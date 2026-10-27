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

// KeyLen is the length in bytes of a per-subject commitment key.
const KeyLen = 32

// ErrKeyDestroyed is returned when a subject's commitment key has been
// shredded. It is the signal that an erasure happened: the record still exists,
// the link between the record and the subject does not.
var ErrKeyDestroyed = errors.New("commitment key destroyed")

// KeyRing holds the per-subject keys that turn content into a commitment.
//
// The key ring is the erasable half of the ledger. An entry references a
// subject through an opaque ref, never through the key itself, so destroying a
// key leaves the entry intact and unlinkable. That is the whole mechanism:
// erasure of personal data is key destruction plus content deletion, not
// deletion of the record.
//
// Keys are minted at random and never derived from the ref or from personal
// data. A derived key is recomputable by anyone who knows the derivation input,
// which rebuilds the brute-force oracle the keyed commitment exists to remove.
type KeyRing interface {
	// Key returns the commitment key for ref, minting one on first use.
	// It returns ErrKeyDestroyed if the ref was erased. A destroyed ref is
	// never reminted: a subject encountered again after an erasure is assigned
	// a fresh ref by the caller, so new records cannot be linked back to the
	// erased history.
	Key(ref string) ([]byte, error)
	// Destroy shreds the key for ref and leaves a tombstone. Idempotent.
	Destroy(ref string) error
	// Has reports whether ref currently holds a usable key.
	Has(ref string) bool
	// Destroyed reports whether ref was erased. It is distinct from Has: a ref
	// that was never seen and a ref that was shredded both have no key, and only
	// the eraser can tell them apart.
	Destroyed(ref string) bool
}

// MemoryKeyRing is a KeyRing that lives in process memory and forgets
// everything on exit. Use it for tests and for short-lived governors; a
// governor that must honor an erasure across restarts needs FileKeyRing.
type MemoryKeyRing struct {
	mu        sync.Mutex
	keys      map[string][]byte
	destroyed map[string]struct{}
}

// NewMemoryKeyRing returns an empty in-memory key ring.
func NewMemoryKeyRing() *MemoryKeyRing {
	return &MemoryKeyRing{
		keys:      make(map[string][]byte),
		destroyed: make(map[string]struct{}),
	}
}

// Key returns the key for ref, minting a random one on first use.
func (k *MemoryKeyRing) Key(ref string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.keyLocked(ref)
}

func (k *MemoryKeyRing) keyLocked(ref string) ([]byte, error) {
	if k.keys == nil {
		k.keys = make(map[string][]byte)
	}
	if k.destroyed == nil {
		k.destroyed = make(map[string]struct{})
	}
	if _, gone := k.destroyed[ref]; gone {
		return nil, fmt.Errorf("%w: %s", ErrKeyDestroyed, ref)
	}
	if key, ok := k.keys[ref]; ok {
		out := make([]byte, len(key))
		copy(out, key)
		return out, nil
	}
	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("mint commitment key: %w", err)
	}
	k.keys[ref] = key
	out := make([]byte, len(key))
	copy(out, key)
	return out, nil
}

// Destroy shreds the key for ref. The key material is zeroed before removal,
// and the ref is tombstoned so it can never be reminted.
func (k *MemoryKeyRing) Destroy(ref string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.keys == nil {
		k.keys = make(map[string][]byte)
	}
	if k.destroyed == nil {
		k.destroyed = make(map[string]struct{})
	}
	if key, ok := k.keys[ref]; ok {
		for i := range key {
			key[i] = 0
		}
		delete(k.keys, ref)
	}
	k.destroyed[ref] = struct{}{}
	return nil
}

// Has reports whether ref holds a usable key.
func (k *MemoryKeyRing) Has(ref string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, gone := k.destroyed[ref]; gone {
		return false
	}
	_, ok := k.keys[ref]
	return ok
}

// Destroyed reports whether ref was erased.
func (k *MemoryKeyRing) Destroyed(ref string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, gone := k.destroyed[ref]
	return gone
}

// snapshot returns copies of the live keys and the tombstones.
func (k *MemoryKeyRing) snapshot() (map[string][]byte, []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	keys := make(map[string][]byte, len(k.keys))
	for ref, key := range k.keys {
		cp := make([]byte, len(key))
		copy(cp, key)
		keys[ref] = cp
	}
	gone := make([]string, 0, len(k.destroyed))
	for ref := range k.destroyed {
		gone = append(gone, ref)
	}
	return keys, gone
}

// fileKeyRingFormat is the on-disk shape of a FileKeyRing. Keys are stored hex
// encoded; the ledger never stores them at all, which is why destroying this
// file's entry for a ref is sufficient to unlink that subject's records.
type fileKeyRingFormat struct {
	Version   int               `json:"version"`
	Keys      map[string]string `json:"keys"`
	Destroyed []string          `json:"destroyed,omitempty"`
}

// FileKeyRing is a KeyRing persisted to a JSON file, rewritten atomically on
// every mint and every destruction.
//
// Destroying a key rewrites the file without it. That removes the material from
// the live file, not from filesystem blocks or from any backup taken earlier.
// Crypto-shredding is only as strong as the copies: the content store must not
// keep a backup that outlives the key, and this file must not be snapshotted
// into one.
type FileKeyRing struct {
	*MemoryKeyRing
	path string
	wmu  sync.Mutex
}

// OpenFileKeyRing opens (or creates) a persistent key ring at path.
func OpenFileKeyRing(path string) (*FileKeyRing, error) {
	f := &FileKeyRing{MemoryKeyRing: NewMemoryKeyRing(), path: path}
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
	var stored fileKeyRingFormat
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("key ring %s is not readable: %w", path, err)
	}
	for ref, hexKey := range stored.Keys {
		key, err := hex.DecodeString(hexKey)
		if err != nil {
			return nil, fmt.Errorf("key ring %s: key for %s is not valid hex", path, ref)
		}
		f.keys[ref] = key
	}
	for _, ref := range stored.Destroyed {
		f.destroyed[ref] = struct{}{}
	}
	return f, nil
}

// Key returns the key for ref, persisting a newly minted one before returning
// it. A key that is not durable cannot honor an erasure later: the commitment
// would outlive the record of how to unlink it.
func (f *FileKeyRing) Key(ref string) ([]byte, error) {
	minted := !f.Has(ref)
	key, err := f.MemoryKeyRing.Key(ref)
	if err != nil {
		return nil, err
	}
	if minted {
		if err := f.persist(); err != nil {
			return nil, err
		}
	}
	return key, nil
}

// Destroy shreds the key for ref and rewrites the file without it.
func (f *FileKeyRing) Destroy(ref string) error {
	if err := f.MemoryKeyRing.Destroy(ref); err != nil {
		return err
	}
	return f.persist()
}

// persist writes the ring atomically: temp file in the same directory, fsync,
// rename, fsync the directory. A crash mid-write leaves the previous ring intact.
func (f *FileKeyRing) persist() error {
	f.wmu.Lock()
	defer f.wmu.Unlock()

	keys, gone := f.snapshot()
	stored := fileKeyRingFormat{Version: 1, Keys: make(map[string]string, len(keys)), Destroyed: gone}
	for ref, key := range keys {
		stored.Keys[ref] = hex.EncodeToString(key)
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return err
	}

	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".keyring-*.tmp")
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
