package platform

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// makeUnwritable removes write permission from dir and restores it when the test
// ends, so a temp dir can still be cleaned up.
func makeUnwritable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod readonly: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// The failure these tests cover is silent and durable: if a key ring mutates
// memory and the write then fails, the tombstone lives in memory but not on disk.
// A retried erasure would see the tombstone, skip the destroy, and append an erase
// entry for an erasure the disk never performed. After a restart the key is back
// and the ledger is lying.

func TestFileKeyRingDestroyRollsBackWhenPersistFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	kr, err := OpenFileKeyRing(path)
	if err != nil {
		t.Fatalf("OpenFileKeyRing: %v", err)
	}
	key, err := kr.Key("sub-a")
	if err != nil {
		t.Fatalf("Key: %v", err)
	}

	makeUnwritable(t, dir)
	err = kr.Destroy("sub-a")
	if err == nil {
		t.Fatal("Destroy reported success while the key ring could not be written")
	}
	if !kr.Has("sub-a") {
		t.Fatal("Destroy kept its in-memory tombstone after a failed write; a retry would record an erasure that never happened")
	}

	got, err := kr.Key("sub-a")
	if err != nil {
		t.Fatalf("Key after a failed Destroy: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("the key changed after a failed Destroy")
	}
}

func TestFileKeyRingKeyRollsBackWhenPersistFails(t *testing.T) {
	dir := t.TempDir()
	kr, err := OpenFileKeyRing(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatalf("OpenFileKeyRing: %v", err)
	}

	makeUnwritable(t, dir)
	if _, err := kr.Key("sub-a"); err == nil {
		t.Fatal("Key reported success while the ring could not be written")
	}
	if kr.Has("sub-a") {
		t.Fatal("a key that was never persisted is live in memory; it would vanish on restart and every commitment made with it would break")
	}
}

func TestFileKeyRingDestroyRollbackSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	kr, _ := OpenFileKeyRing(path)
	_, _ = kr.Key("sub-a")

	makeUnwritable(t, dir)
	_ = kr.Destroy("sub-a")
	_ = os.Chmod(dir, 0o700)

	reopened, err := OpenFileKeyRing(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reopened.Has("sub-a") {
		t.Fatal("memory and disk disagree: the tombstone was never written, so the subject must still have a key")
	}
}

func TestFileRefRegistryForgetRollsBackWhenPersistFails(t *testing.T) {
	dir := t.TempDir()
	refs, err := OpenFileRefRegistry(filepath.Join(dir, "refs.json"))
	if err != nil {
		t.Fatalf("OpenFileRefRegistry: %v", err)
	}
	original, err := refs.Ref("alice@example.com")
	if err != nil {
		t.Fatalf("Ref: %v", err)
	}

	makeUnwritable(t, dir)
	if err := refs.Forget("alice@example.com"); err == nil {
		t.Fatal("Forget reported success while the registry could not be written")
	}

	got, err := refs.Ref("alice@example.com")
	if err != nil {
		t.Fatalf("Ref after a failed Forget: %v", err)
	}
	if got != original {
		t.Fatal("a failed Forget minted a new handle in memory; the disk still maps the subject to the old one, so the two would disagree after a restart")
	}
}

func TestFileRefRegistryRefRollsBackWhenPersistFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "refs.json")
	refs, _ := OpenFileRefRegistry(path)

	makeUnwritable(t, dir)
	if _, err := refs.Ref("alice@example.com"); err == nil {
		t.Fatal("Ref reported success while the registry could not be written")
	}
	_ = os.Chmod(dir, 0o700)

	// After a rollback the next Ref mints fresh and persists. If the failed mint
	// stayed in memory, this call would return it without writing anything, and a
	// reopened registry would disagree.
	ref, err := refs.Ref("alice@example.com")
	if err != nil {
		t.Fatalf("Ref after rollback: %v", err)
	}
	reopened, err := OpenFileRefRegistry(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Ref("alice@example.com")
	if err != nil {
		t.Fatalf("Ref after reopen: %v", err)
	}
	if got != ref {
		t.Fatal("the handle handed out after a rolled-back mint was never persisted; memory and disk disagree")
	}
}

// TestEraseRetryAfterFailedPersistActuallyShreds is the end-to-end version: the
// first erasure cannot write the key ring, and the retry must actually shred
// rather than trust an in-memory tombstone that never reached the disk.
func TestEraseRetryAfterFailedPersistActuallyShreds(t *testing.T) {
	dir := t.TempDir()
	keysPath := filepath.Join(dir, "keys.json")
	ring, err := OpenFileKeyRing(keysPath)
	if err != nil {
		t.Fatalf("OpenFileKeyRing: %v", err)
	}
	content := NewMemoryContentStore()
	c, err := NewCommitter(ring)
	if err != nil {
		t.Fatalf("NewCommitter: %v", err)
	}
	if _, err := c.Commit("sub-a", "x"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = content.Put("sub-a", "item", []byte("alice@example.com"))
	ledger := NewLedger()
	e := &Eraser{Content: content, Ring: ring, Ledger: ledger}

	makeUnwritable(t, dir)
	if err := e.Erase("sub-a", "gdpr-request"); err == nil {
		t.Fatal("Erase reported success while the key ring could not be written")
	}
	if got := len(ledger.Entries()); got != 0 {
		t.Fatalf("ledger entries after a failed erasure = %d, want 0", got)
	}
	_ = os.Chmod(dir, 0o700)

	if err := e.Erase("sub-a", "gdpr-request"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	reopened, err := OpenFileKeyRing(keysPath)
	if err != nil {
		t.Fatalf("reopen key ring: %v", err)
	}
	if reopened.Has("sub-a") {
		t.Fatal("the retried erasure recorded success while the key is still on disk; the record now claims an erasure that did not happen")
	}
	if got := len(ledger.Entries()); got != 1 {
		t.Fatalf("ledger entries = %d, want exactly one erase entry", got)
	}
}
