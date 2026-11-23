package protocol

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The whole point of this module is that a client can depend on it without the kernel. That
// property is invisible to the compiler: importing platform here would build, pass every
// other test, and quietly restore the coupling the module exists to break — the client would
// be back to pulling the policy engine, the ledger and the enforcer to learn the shape of a
// Decision.
//
// So the rule is enforced as an import test. An import whose first path element contains a
// dot is a domain name, which means it came from outside the standard library.
func TestTheWireImportsNothingButTheStandardLibrary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var checked int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", name, imp.Path.Value, err)
			}
			first := strings.SplitN(path, "/", 2)[0]
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %q: the wire must not depend on anything outside the standard library", name, path)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no source files found; this test would pass over an empty package")
	}
}
