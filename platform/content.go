package platform

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrContentNotFound is returned when the content store holds nothing for a ref.
var ErrContentNotFound = errors.New("content not found")

// ContentStore is the erasable half of the record: everything that identifies a
// person or carries what an agent actually saw. The ledger holds commitments
// into this store and never a copy of it.
//
// Items are scoped to a subject ref, and a subject may hold many: one per
// decision. The item key is the commitment that points at it, which is why the
// ledger can resolve a record to its content while the content stays out of the
// ledger.
//
// This store holds plaintext by definition, because it is the data itself. Its
// confidentiality is at-rest custody (encryption, access control), not the
// keyed commitment: the commitment stops the *ledger alone* from being a
// re-identification oracle, and this store's own protection is a separate
// problem the operator owns.
//
// The contract Delete has to honor is the whole reason this interface exists:
// after Delete returns nil, no copy the store is responsible for may outlive it,
// including backups, snapshots, and replicas. A store that keeps a backup past
// the key's destruction turns a crypto-shred into a promise. Implementing this
// against a backend means answering "what copies exist, and how does Delete
// reach them" before writing a line of it.
type ContentStore interface {
	// Put stores content for a subject ref under item, replacing that item only.
	Put(ref, item string, content []byte) error
	// Get returns the content for one item, or ErrContentNotFound.
	Get(ref, item string) ([]byte, error)
	// Has reports whether ref holds any content at all.
	Has(ref string) bool
	// Delete removes every item for ref, whatever their keys. Idempotent.
	Delete(ref string) error
}

// MemoryContentStore is a ContentStore that lives in process memory. It is the
// reference implementation and the test double: nothing survives the process,
// so it cannot outlive a key by accident.
type MemoryContentStore struct {
	mu    sync.Mutex
	items map[string]map[string][]byte
}

// NewMemoryContentStore returns an empty in-memory content store.
func NewMemoryContentStore() *MemoryContentStore {
	return &MemoryContentStore{items: make(map[string]map[string][]byte)}
}

// Put stores content under item for ref, replacing that item only.
func (s *MemoryContentStore) Put(ref, item string, content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]map[string][]byte)
	}
	subject, ok := s.items[ref]
	if !ok {
		subject = make(map[string][]byte)
		s.items[ref] = subject
	}
	cp := make([]byte, len(content))
	copy(cp, content)
	subject[item] = cp
	return nil
}

// Get returns the content for one item.
func (s *MemoryContentStore) Get(ref, item string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subject, ok := s.items[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrContentNotFound, ref)
	}
	content, ok := subject[item]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrContentNotFound, ref)
	}
	cp := make([]byte, len(content))
	copy(cp, content)
	return cp, nil
}

// Has reports whether ref holds any content.
func (s *MemoryContentStore) Has(ref string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items[ref]) > 0
}

// Items returns the item keys held for ref, sorted.
func (s *MemoryContentStore) Items(ref string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	subject := s.items[ref]
	items := make([]string, 0, len(subject))
	for item := range subject {
		items = append(items, item)
	}
	sort.Strings(items)
	return items
}

// Delete zeroes and removes every item held for ref.
func (s *MemoryContentStore) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	subject, ok := s.items[ref]
	if !ok {
		return nil
	}
	for _, content := range subject {
		for i := range content {
			content[i] = 0
		}
	}
	delete(s.items, ref)
	return nil
}
