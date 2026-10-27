package platform

import (
	"errors"
	"fmt"
	"sync"
)

// ErrContentNotFound is returned when the content store holds nothing for a ref.
var ErrContentNotFound = errors.New("content not found")

// ContentStore is the erasable half of the record: everything that identifies a
// person or carries what an agent actually saw. The ledger holds commitments
// into this store and never a copy of it.
//
// The contract Delete has to honor is the whole reason this interface exists:
// after Delete returns nil, no copy the store is responsible for may outlive it,
// including backups, snapshots, and replicas. A store that keeps a backup past
// the key's destruction turns a crypto-shred into a promise. Implementing this
// against a backend means answering "what copies exist, and how does Delete
// reach them" before writing a line of it.
type ContentStore interface {
	// Put stores content under ref, replacing anything already there.
	Put(ref string, content []byte) error
	// Get returns the content for ref, or ErrContentNotFound.
	Get(ref string) ([]byte, error)
	// Delete removes all content for ref. Idempotent.
	Delete(ref string) error
}

// MemoryContentStore is a ContentStore that lives in process memory. It is the
// reference implementation and the test double: nothing survives the process,
// so it cannot outlive a key by accident.
type MemoryContentStore struct {
	mu    sync.Mutex
	items map[string][]byte
}

// NewMemoryContentStore returns an empty in-memory content store.
func NewMemoryContentStore() *MemoryContentStore {
	return &MemoryContentStore{items: make(map[string][]byte)}
}

// Put stores content under ref.
func (s *MemoryContentStore) Put(ref string, content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string][]byte)
	}
	cp := make([]byte, len(content))
	copy(cp, content)
	s.items[ref] = cp
	return nil
}

// Get returns the content for ref.
func (s *MemoryContentStore) Get(ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.items[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrContentNotFound, ref)
	}
	cp := make([]byte, len(content))
	copy(cp, content)
	return cp, nil
}

// Delete zeroes and removes the content for ref.
func (s *MemoryContentStore) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if content, ok := s.items[ref]; ok {
		for i := range content {
			content[i] = 0
		}
		delete(s.items, ref)
	}
	return nil
}
