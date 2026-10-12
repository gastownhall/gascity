package beadmeta

// beadmeta is a leaf, and that is what makes it a safe home for the tables
// every other layer has to consult.
//
// The reserved-prefix table moved here from internal/config precisely because
// config's cone reaches internal/git, internal/remotesource and
// internal/worker/builtin -> internal/runtime: asking "which namespace does
// this class mint under" was linking a process-spawning cone into every
// package that asked. Nothing keeps it out except this test — one convenience
// import of a gascity package would restore the cone silently, because the
// relocation's only visible symptom is a build graph nobody reads.
//
// Test files are exempt on purpose: the generated testenv import is one, and a
// test's dependencies are not the shipped package's.

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestPackageImportsNothingFromGasCity(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}
	fset := token.NewFileSet()
	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		parsed++
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquoting import %s: %v", name, spec.Path.Value, err)
			}
			if strings.HasPrefix(path, "github.com/gastownhall/gascity/") {
				t.Errorf("%s imports %s; beadmeta must stay a leaf so a residency decision does not link a process-spawning cone", name, path)
			}
		}
	}
	// A walk that reached no file would pass vacuously, and the one thing this
	// test asserts is the absence of something.
	if parsed == 0 {
		t.Fatal("parsed no non-test files; the leaf check inspected nothing")
	}
}
