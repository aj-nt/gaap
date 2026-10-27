package platform

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileContentStore is a ContentStore on disk: one directory per subject ref, one
// file per item. Deleting a subject removes the directory, which is the cheapest
// form of "every copy this store is responsible for" that a filesystem offers.
//
// It is plaintext. Encrypting it is the operator's custody problem, and it has to
// be solved alongside the backups: a copy of this directory that outlives the key
// ring defeats the erasure the key destruction was supposed to perform.
type FileContentStore struct {
	root string
}

// OpenFileContentStore opens (or creates) a content store rooted at dir.
func OpenFileContentStore(dir string) (*FileContentStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create content store %s: %w", dir, err)
	}
	return &FileContentStore{root: dir}, nil
}

// safeSegment rejects names that could escape the store root. Refs and items
// reach this store from a registry and from commitments, and a caller-supplied
// subject string must never be able to steer a write.
func safeSegment(name string) error {
	switch {
	case name == "":
		return errors.New("empty name")
	case name == "." || name == "..":
		return fmt.Errorf("invalid name %q", name)
	case strings.ContainsAny(name, "/\\"):
		return fmt.Errorf("name %q contains a path separator", name)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("name %q starts with a dot", name)
	}
	return nil
}

// Put stores content for one item atomically: temp file, fsync, rename.
func (s *FileContentStore) Put(ref, item string, content []byte) error {
	if err := safeSegment(ref); err != nil {
		return fmt.Errorf("ref: %w", err)
	}
	if err := safeSegment(item); err != nil {
		return fmt.Errorf("item: %w", err)
	}
	dir := filepath.Join(s.root, ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".item-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(content); err != nil {
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
	return os.Rename(tmpName, filepath.Join(dir, item))
}

// Get returns the content for one item.
func (s *FileContentStore) Get(ref, item string) ([]byte, error) {
	if err := safeSegment(ref); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrContentNotFound, err)
	}
	if err := safeSegment(item); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrContentNotFound, err)
	}
	b, err := os.ReadFile(filepath.Join(s.root, ref, item))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrContentNotFound, ref)
		}
		return nil, err
	}
	return b, nil
}

// Has reports whether ref holds any content.
func (s *FileContentStore) Has(ref string) bool {
	if err := safeSegment(ref); err != nil {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(s.root, ref))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".item-") {
			return true
		}
	}
	return false
}

// Items returns the item keys held for ref, sorted.
func (s *FileContentStore) Items(ref string) []string {
	if err := safeSegment(ref); err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(s.root, ref))
	if err != nil {
		return nil
	}
	items := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".item-") {
			continue
		}
		items = append(items, e.Name())
	}
	sort.Strings(items)
	return items
}

// Delete removes every item held for ref by removing the subject's directory.
func (s *FileContentStore) Delete(ref string) error {
	if err := safeSegment(ref); err != nil {
		return fmt.Errorf("ref: %w", err)
	}
	err := os.RemoveAll(filepath.Join(s.root, ref))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
