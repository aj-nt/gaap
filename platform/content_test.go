package platform

import (
	"bytes"
	"errors"
	"sort"
	"testing"
)

func TestMemoryContentStoreRoundTrip(t *testing.T) {
	s := NewMemoryContentStore()
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

func TestMemoryContentStoreHoldsManyItemsPerSubject(t *testing.T) {
	s := NewMemoryContentStore()
	_ = s.Put("ref-a", "item-1", []byte("first"))
	_ = s.Put("ref-a", "item-2", []byte("second"))

	first, err := s.Get("ref-a", "item-1")
	if err != nil {
		t.Fatalf("Get item-1: %v", err)
	}
	if string(first) != "first" {
		t.Fatal("a second decision for the same subject overwrote the first: earlier commitments would point at the wrong content")
	}
	if items := s.Items("ref-a"); len(items) != 2 {
		t.Fatalf("items = %v, want 2", items)
	}
}

func TestMemoryContentStoreHas(t *testing.T) {
	s := NewMemoryContentStore()
	if s.Has("ref-a") {
		t.Fatal("empty store reports content")
	}
	_ = s.Put("ref-a", "item-1", []byte("x"))
	if !s.Has("ref-a") {
		t.Fatal("store does not report content it holds")
	}
}

func TestMemoryContentStoreDeleteRemovesEveryItem(t *testing.T) {
	s := NewMemoryContentStore()
	_ = s.Put("ref-a", "item-1", []byte("first"))
	_ = s.Put("ref-a", "item-2", []byte("second"))
	_ = s.Put("ref-b", "item-3", []byte("keep me"))
	if err := s.Delete("ref-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Has("ref-a") {
		t.Fatal("Delete left items behind")
	}
	if _, err := s.Get("ref-a", "item-1"); !errors.Is(err, ErrContentNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrContentNotFound", err)
	}
	if !s.Has("ref-b") {
		t.Fatal("Delete removed another subject's content")
	}
}

func TestMemoryContentStoreDeleteIsIdempotent(t *testing.T) {
	s := NewMemoryContentStore()
	_ = s.Put("ref-a", "item-1", []byte("x"))
	if err := s.Delete("ref-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete("ref-a"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestMemoryContentStoreCopiesOnPutAndGet(t *testing.T) {
	s := NewMemoryContentStore()
	value := []byte("payload")
	_ = s.Put("ref-a", "item-1", value)
	value[0] = 'X'

	got, _ := s.Get("ref-a", "item-1")
	if !bytes.Equal(got, []byte("payload")) {
		t.Fatalf("stored content = %q, want payload", got)
	}
	got[0] = 'Y'
	again, _ := s.Get("ref-a", "item-1")
	if !bytes.Equal(again, []byte("payload")) {
		t.Fatal("caller mutating a returned buffer rewrote stored content")
	}
}

func TestMemoryContentStoreItemsAreSorted(t *testing.T) {
	s := NewMemoryContentStore()
	_ = s.Put("ref-a", "b", []byte("x"))
	_ = s.Put("ref-a", "a", []byte("x"))
	items := s.Items("ref-a")
	if !sort.StringsAreSorted(items) {
		t.Fatalf("items = %v, want sorted for deterministic output", items)
	}
}
