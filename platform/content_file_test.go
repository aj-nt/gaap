package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileContentStoreRoundTrip(t *testing.T) {
	s, err := OpenFileContentStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenFileContentStore: %v", err)
	}
	if err := s.Put("ref-a", "item-1", []byte("alice@example.com")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get("ref-a", "item-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "alice@example.com" {
		t.Fatalf("Get = %q", got)
	}
}

func TestFileContentStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenFileContentStore(dir)
	if err := s.Put("ref-a", "item-1", []byte("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reopened, err := OpenFileContentStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Get("ref-a", "item-1")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("Get after reopen = %q", got)
	}
}

func TestFileContentStoreDeleteRemovesDirectory(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenFileContentStore(dir)
	_ = s.Put("ref-a", "item-1", []byte("first"))
	_ = s.Put("ref-a", "item-2", []byte("second"))
	_ = s.Put("ref-b", "item-3", []byte("keep"))

	if err := s.Delete("ref-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Has("ref-a") {
		t.Fatal("Delete left items behind")
	}
	if _, err := os.Stat(filepath.Join(dir, "ref-a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subject directory still on disk after Delete: %v", err)
	}
	if !s.Has("ref-b") {
		t.Fatal("Delete removed another subject's content")
	}
}

func TestFileContentStoreDeleteIsIdempotent(t *testing.T) {
	s, _ := OpenFileContentStore(t.TempDir())
	if err := s.Delete("never-seen"); err != nil {
		t.Fatalf("Delete of an unseen subject: %v", err)
	}
}

func TestFileContentStoreGetMissing(t *testing.T) {
	s, _ := OpenFileContentStore(t.TempDir())
	if _, err := s.Get("ref-a", "item-1"); !errors.Is(err, ErrContentNotFound) {
		t.Fatalf("Get on empty store = %v, want ErrContentNotFound", err)
	}
	_ = s.Put("ref-a", "item-1", []byte("x"))
	if _, err := s.Get("ref-a", "other"); !errors.Is(err, ErrContentNotFound) {
		t.Fatalf("Get on missing item = %v, want ErrContentNotFound", err)
	}
}

func TestFileContentStoreOverwritesOnlyThatItem(t *testing.T) {
	s, _ := OpenFileContentStore(t.TempDir())
	_ = s.Put("ref-a", "item-1", []byte("one"))
	_ = s.Put("ref-a", "item-2", []byte("two"))
	_ = s.Put("ref-a", "item-1", []byte("one-updated"))

	first, _ := s.Get("ref-a", "item-1")
	second, _ := s.Get("ref-a", "item-2")
	if string(first) != "one-updated" || string(second) != "two" {
		t.Fatalf("content = %q / %q", first, second)
	}
}

func TestFileContentStoreRejectsPathTraversal(t *testing.T) {
	s, _ := OpenFileContentStore(t.TempDir())
	if err := s.Put("..", "item", []byte("x")); err == nil {
		t.Fatal("a ref of '..' was accepted; it would write outside the store")
	}
	if err := s.Put("ref-a", "../../etc/passwd", []byte("x")); err == nil {
		t.Fatal("an item containing a path separator was accepted")
	}
	if err := s.Put("sub/dir", "item", []byte("x")); err == nil {
		t.Fatal("a ref containing a path separator was accepted")
	}
	if err := s.Delete("../escape"); err == nil {
		t.Fatal("Delete accepted a traversal ref")
	}
}

func TestFileContentStoreHasIgnoresEmptySubjectDir(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenFileContentStore(dir)
	_ = os.MkdirAll(filepath.Join(dir, "ref-a"), 0o700)
	if s.Has("ref-a") {
		t.Fatal("an empty subject directory reports content")
	}
}
