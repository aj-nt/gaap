package platform

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefRegistryMintsOpaqueRandomRefs(t *testing.T) {
	r := NewMemoryRefRegistry()
	ref, err := r.Ref("alice@example.com")
	if err != nil {
		t.Fatalf("Ref: %v", err)
	}
	if ref == "" {
		t.Fatal("empty ref")
	}
	if strings.Contains(ref, "alice") {
		t.Fatalf("ref %q is derived from the subject; the ledger would store the identifier in the clear", ref)
	}
	if other, _ := NewMemoryRefRegistry().Ref("alice@example.com"); other == ref {
		t.Fatal("two independent registries minted the same ref for the same subject: refs are derived, not random")
	}
}

func TestRefRegistryIsStablePerSubject(t *testing.T) {
	r := NewMemoryRefRegistry()
	first, _ := r.Ref("alice@example.com")
	second, err := r.Ref("alice@example.com")
	if err != nil {
		t.Fatalf("second Ref: %v", err)
	}
	if first != second {
		t.Fatal("same subject resolved to two refs; its records would not group")
	}
}

func TestRefRegistryForgetForbidsRelinking(t *testing.T) {
	r := NewMemoryRefRegistry()
	old, _ := r.Ref("alice@example.com")
	if err := r.Forget("alice@example.com"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	fresh, err := r.Ref("alice@example.com")
	if err != nil {
		t.Fatalf("Ref after Forget: %v", err)
	}
	if fresh == old {
		t.Fatal("an erased subject got its old ref back; new records would relink to the erased history")
	}
}

func TestFileRefRegistryPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refs.json")
	r, err := OpenFileRefRegistry(path)
	if err != nil {
		t.Fatalf("OpenFileRefRegistry: %v", err)
	}
	want, _ := r.Ref("alice@example.com")

	reopened, err := OpenFileRefRegistry(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Ref("alice@example.com")
	if err != nil {
		t.Fatalf("Ref after reopen: %v", err)
	}
	if got != want {
		t.Fatal("ref did not survive a reopen; the subject's records would split across two handles")
	}
}

func TestFileRefRegistryForgetErasesTheMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refs.json")
	r, _ := OpenFileRefRegistry(path)
	_, _ = r.Ref("alice@example.com")
	_, _ = r.Ref("bob@example.com")
	if err := r.Forget("alice@example.com"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if bytes.Contains(raw, []byte("alice@example.com")) {
		t.Fatal("the registry still maps the erased subject; the mapping table is personal data and must be deleted with it")
	}
	if !bytes.Contains(raw, []byte("bob@example.com")) {
		t.Fatal("Forget removed the wrong subject")
	}
}

func TestFileRefRegistrySurvivesRepeatedWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refs.json")
	r, _ := OpenFileRefRegistry(path)
	seen := make(map[string]string)
	for _, subject := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		ref, err := r.Ref(subject)
		if err != nil {
			t.Fatalf("Ref(%s): %v", subject, err)
		}
		seen[subject] = ref
	}
	reopened, _ := OpenFileRefRegistry(path)
	for subject, want := range seen {
		got, err := reopened.Ref(subject)
		if err != nil {
			t.Fatalf("Ref(%s) after reopen: %v", subject, err)
		}
		if got != want {
			t.Fatalf("ref for %s changed across reopen", subject)
		}
	}
}
