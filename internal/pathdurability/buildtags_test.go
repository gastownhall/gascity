package pathdurability

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"testing"
)

// TestRealFilesystemProbeIsLinuxGated ensures the tests that exercise the real,
// un-faked syscall probes (as opposed to the fakeMounts double installed by
// fakeMounts.install) only build on platforms where those probes have a real
// implementation. probe_other.go is a deliberate stub returning Unknown for
// every non-linux platform (see its doc comment), so an assertion against the
// real probe's result — such as "same device as the city root" resolving to
// CityDevice — is only ever true on linux. Compiling that assertion
// unconditionally makes it fail on every other platform.
//
// This test runs on every platform (it carries no build tag itself): it asks
// go/build which test files a darwin build would include, rather than
// executing on darwin, since only a linux host is available here. It parses
// each candidate file's declarations rather than grepping raw source, so it
// does not trip over its own name appearing in a comment or string literal.
func TestRealFilesystemProbeIsLinuxGated(t *testing.T) {
	const wantGated = "TestClassifyOnRealFilesystem"

	ctx := build.Default
	ctx.GOOS = "darwin"
	ctx.GOARCH = "amd64"
	ctx.CgoEnabled = false

	pkg, err := ctx.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("ImportDir(GOOS=darwin): %v", err)
	}

	fset := token.NewFileSet()
	for _, f := range pkg.TestGoFiles {
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != wantGated {
				continue
			}
			t.Fatalf("%s declares func %s and is included in a GOOS=darwin build.\n"+
				"Its \"same device as the city root\" subtest asserts CityDevice "+
				"unconditionally, which is only true where the real probe is "+
				"implemented (linux) — probe_other.go deliberately stubs every "+
				"other platform to Unknown. Move %s into a file with a "+
				"'//go:build linux' constraint.", f, wantGated, wantGated)
		}
	}
}
