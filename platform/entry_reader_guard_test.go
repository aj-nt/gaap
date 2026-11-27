package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// inertEntryFields reports the Entry fields that no reader outside the hash path
// touches. It is the Phase 0 guard: a field is either read by something that acts
// on it, or it is decoration.
//
// Two deliberate exclusions, both of which would make every field look alive:
//
//   - <root>/platform/ledger.go, where hashEntry and the JSON round-trip read every
//     field by construction. A reader there proves the field is serialised, not
//     that anything acts on it.
//   - _test.go files. The plan's rule is that a unit-test-only writer is a defect,
//     so a test-only reader is not a consumer either.
//
// It is a name-based pass, not a type-checked one: it can over-count (another
// struct with a field of the same name looks like a reader), so a green result
// means "not obviously inert", never "proven live". It cannot under-count in the
// direction that matters — a field read only by the hash path is reported.
func inertEntryFields(root string) ([]string, error) {
	fields, err := entryFieldNames(filepath.Join(root, "platform"))
	if err != nil {
		return nil, err
	}

	counts := map[string]int{}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".hermes", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		slash := filepath.ToSlash(path)
		if strings.HasSuffix(slash, "/platform/ledger.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, want := fields[sel.Sel.Name]; want {
				counts[sel.Sel.Name]++
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	var inert []string
	for name := range fields {
		if counts[name] == 0 {
			inert = append(inert, name)
		}
	}
	sort.Strings(inert)
	return inert, nil
}

// entryFieldNames parses the Entry struct and returns its exported field names.
// Read from the source rather than hand-listed: a hand-list is a second
// declaration that can disagree with the struct, which is the failure this whole
// guard exists to make impossible.
func entryFieldNames(dir string) (map[string]struct{}, error) {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, "ledger.go"), nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name == nil || ts.Name.Name != "Entry" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			for _, name := range field.Names {
				if name.IsExported() {
					out[name.Name] = struct{}{}
				}
			}
		}
		return false
	})
	return out, nil
}

// The guard itself, over this module.
func TestEveryEntryFieldHasAReader(t *testing.T) {
	fields, err := entryFieldNames(".")
	if err != nil {
		t.Fatalf("parse ledger.go: %v", err)
	}
	if len(fields) < 10 {
		t.Fatalf("parsed only %d Entry fields (%v); the struct changed shape and this guard is reading the wrong thing", len(fields), fields)
	}

	inert, err := inertEntryFields("..")
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(inert) > 0 {
		t.Fatalf("%d Entry field(s) have no reader outside the hash path: %v\n"+
			"Either give the field a reader in the change that adds it, or delete it — "+
			"a field read only by hashEntry is decoration, and the plan's rule is that "+
			"no field ships without a consumer.", len(inert), inert)
	}
}

// A guard nobody has seen fail is a guard nobody should trust. This drives it
// over a fixture module and checks both directions:
//
//   - a field read only by the hash path is reported, which is the whole point;
//   - removing a real reader turns a live field inert, so the check cannot pass
//     by accident;
//   - a test-only reader does not count.
func TestGuardFlagsFieldsThatOnlyTheHashPathReads(t *testing.T) {
	dir := t.TempDir()
	plat := filepath.Join(dir, "platform")
	if err := os.MkdirAll(plat, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("platform/ledger.go", `package platform

type Entry struct {
	Live       string
	Dead       string
	Hasher     string
	AlsoHasher string
}

func (e Entry) Marshal() map[string]string {
	return map[string]string{
		"live": e.Live, "dead": e.Dead, "hasher": e.Hasher, "also": e.AlsoHasher,
	}
}
`)
	// consumer.go reads Live only.
	write("consumer.go", `package fixture

func Use(e Entry) string { return e.Live }
`)
	// A file that reads a field from outside ledger.go counts as a reader even
	// though it is hash-shaped: the exclusion is about the file, not the name.
	write("parser.go", `package fixture

func Parse(e Entry) string { return e.AlsoHasher }
`)
	// a test-only reader must not count
	write("only_test.go", `package fixture

func readInTest(e Entry) string { return e.Dead }
`)

	got, err := inertEntryFields(dir)
	if err != nil {
		t.Fatalf("inertEntryFields: %v", err)
	}
	want := []string{"Dead", "Hasher"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("inert = %v, want %v (Live and AlsoHasher have readers; Hasher is read only from ledger.go; Dead is read only from a test)", got, want)
	}

	// Now remove the one real reader: Live must start being reported.
	write("consumer.go", `package fixture

const unrelated = 1
`)
	got, err = inertEntryFields(dir)
	if err != nil {
		t.Fatalf("inertEntryFields: %v", err)
	}
	if !contains(got, "Live") {
		t.Fatalf("removing the only reader of Live did not flag it: %v", got)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
