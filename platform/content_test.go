package platform

import (
	"bytes"
	"errors"
	"testing"
)

func TestMemoryContentStoreRoundTrip(t *testing.T) {
	s := NewMemoryContentStore()
	if err := s.Put("sub-a", []byte("alice@example.com")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get("sub-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "alice@example.com" {
		t.Fatalf("Get = %q", got)
	}
}

func TestMemoryContentStoreDeleteIsIdempotent(t *testing.T) {
	s := NewMemoryContentStore()
	_ = s.Put("sub-a", []byte("x"))
	if err := s.Delete("sub-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete("sub-a"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestMemoryContentStoreGetAfterDeleteFails(t *testing.T) {
	s := NewMemoryContentStore()
	_ = s.Put("sub-a", []byte("x"))
	_ = s.Delete("sub-a")
	if _, err := s.Get("sub-a"); !errors.Is(err, ErrContentNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrContentNotFound", err)
	}
}

func TestMemoryContentStoreCopiesOnPutAndGet(t *testing.T) {
	s := NewMemoryContentStore()
	value := []byte("payload")
	_ = s.Put("sub-a", value)
	value[0] = 'X' // caller mutating its buffer must not rewrite stored content

	got, _ := s.Get("sub-a")
	if !bytes.Equal(got, []byte("payload")) {
		t.Fatalf("stored content = %q, want payload", got)
	}
	got[0] = 'Y'
	again, _ := s.Get("sub-a")
	if !bytes.Equal(again, []byte("payload")) {
		t.Fatal("caller mutating a returned buffer rewrote stored content")
	}
}
