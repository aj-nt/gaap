package platform

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryKeyRingMintsDistinctKeysPerRef(t *testing.T) {
	kr := NewMemoryKeyRing()
	a, err := kr.Key("sub-a")
	if err != nil {
		t.Fatalf("Key(sub-a): %v", err)
	}
	b, err := kr.Key("sub-b")
	if err != nil {
		t.Fatalf("Key(sub-b): %v", err)
	}
	if len(a) != KeyLen {
		t.Fatalf("key length = %d, want %d", len(a), KeyLen)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two subjects were minted the same key")
	}
}

func TestMemoryKeyRingIsStablePerRef(t *testing.T) {
	kr := NewMemoryKeyRing()
	first, _ := kr.Key("sub-a")
	second, err := kr.Key("sub-a")
	if err != nil {
		t.Fatalf("second Key(sub-a): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("same ref minted two different keys; commitments would not reproduce")
	}
}

func TestMemoryKeyRingDestroyLeavesTombstone(t *testing.T) {
	kr := NewMemoryKeyRing()
	_, _ = kr.Key("sub-a")
	if err := kr.Destroy("sub-a"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if kr.Has("sub-a") {
		t.Fatal("destroyed subject still reports a key")
	}
	if _, err := kr.Key("sub-a"); !errors.Is(err, ErrKeyDestroyed) {
		t.Fatalf("Key after Destroy = %v, want ErrKeyDestroyed", err)
	}
	if err := kr.Destroy("sub-a"); err != nil {
		t.Fatalf("Destroy is not idempotent: %v", err)
	}
}

func TestKeyRingDoesNotDeriveKeysFromRef(t *testing.T) {
	first, _ := NewMemoryKeyRing().Key("sub-a")
	second, _ := NewMemoryKeyRing().Key("sub-a")
	if bytes.Equal(first, second) {
		t.Fatal("the same ref produced the same key in two independent rings: keys are derived from the ref, not random")
	}
}

func TestFileKeyRingPersistsKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	kr, err := OpenFileKeyRing(path)
	if err != nil {
		t.Fatalf("OpenFileKeyRing: %v", err)
	}
	want, _ := kr.Key("sub-a")

	reopened, err := OpenFileKeyRing(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Key("sub-a")
	if err != nil {
		t.Fatalf("Key after reopen: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("key did not survive a reopen; every commitment it made would be unverifiable")
	}
}

func TestFileKeyRingShredsMaterialOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	kr, _ := OpenFileKeyRing(path)
	key, _ := kr.Key("sub-a")
	if err := kr.Destroy("sub-a"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key ring: %v", err)
	}
	if bytes.Contains(raw, key) || bytes.Contains(bytes.ToLower(raw), []byte(hex.EncodeToString(key))) {
		t.Fatal("destroyed key material is still present in the key ring file")
	}
}

func TestFileKeyRingTombstoneSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	kr, _ := OpenFileKeyRing(path)
	_, _ = kr.Key("sub-a")
	if err := kr.Destroy("sub-a"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	reopened, err := OpenFileKeyRing(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Has("sub-a") {
		t.Fatal("erased subject regained a key after reopen")
	}
	if _, err := reopened.Key("sub-a"); !errors.Is(err, ErrKeyDestroyed) {
		t.Fatalf("Key after reopen = %v, want ErrKeyDestroyed", err)
	}
}
