package genshell

import (
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// fencedImportPrefix is the tree this guard keeps out of non-Windows builds:
// internal/genshell and everything under it, genshelltest included.
const fencedImportPrefix = "github.com/gastownhall/gascity/internal/genshell"

// TestGenshellStaysOutOfNonWindowsBuilds is the maintainer's condition on
// gastownhall/gascity#8890. genshell carries an embedded shell interpreter
// (mvdan.cc/sh/v3) and a jq engine (gojq) for the Windows port, and neither may
// link into the Linux or macOS gc. So a Go file outside internal/genshell that
// imports the tree must be Windows-only: a _windows.go (or _windows_<arch>.go)
// name, or a //go:build constraint that is false for both linux and darwin.
// Test files are exempt, because they never link into the gc binary.
//
// This is a check rather than a //go:build windows tag on the package, so the
// package's own tests keep running on Linux and its Bazel targets keep their
// sources. The mechanism is the one internal/beads/splittest/import_boundary_test.go
// uses: parse imports, name the offending file, fail.
func TestGenshellStaysOutOfNonWindowsBuilds(t *testing.T) {
	t.Parallel()
	root := fenceRepoRoot(t)
	offenders, scanned, err := nonWindowsGenshellImporters(root)
	if err != nil {
		t.Fatalf("scanning the module for genshell imports: %v", err)
	}
	if scanned == 0 {
		t.Fatal("genshell import fence scanned zero non-test Go files; it is not evaluating anything")
	}
	if len(offenders) > 0 {
		t.Errorf("files that can build for Linux or macOS import %s:\n  %s\n\n"+
			"internal/genshell is Windows-only: it carries a shell interpreter and a jq engine that must "+
			"never link into the Linux or macOS gc. Give the importing file a _windows.go name or a "+
			"//go:build windows constraint.",
			fencedImportPrefix, strings.Join(offenders, "\n  "))
	}
}

// TestGenshellFenceCatchesNonWindowsImporters is the fence's negative self-test:
// a guard that cannot fail pins nothing.
func TestGenshellFenceCatchesNonWindowsImporters(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	importLib := "import _ \"github.com/gastownhall/gascity/internal/genshell\"\n"
	importKit := "import _ \"github.com/gastownhall/gascity/internal/genshell/genshelltest\"\n"

	write("go.mod", "module example.com/fence\n")
	// Must be caught: each can build for Linux or macOS.
	write("plain/plain.go", "package plain\n\n"+importLib)
	write("kit/kit.go", "package kit\n\n"+importKit)
	write("nottagged/n.go", "//go:build !windows\n\npackage nottagged\n\n"+importLib)
	write("linuxonly/l_linux.go", "package linuxonly\n\n"+importLib)
	write("darwintag/d.go", "//go:build darwin || windows\n\npackage darwintag\n\n"+importLib)
	write("customtag/c.go", "//go:build integration\n\npackage customtag\n\n"+importLib)
	write("negcustom/n.go", "//go:build !integration\n\npackage negcustom\n\n"+importLib)
	write("negarch/a.go", "//go:build !arm64\n\npackage negarch\n\n"+importLib)
	write("negcgo/c.go", "//go:build !cgo\n\npackage negcgo\n\n"+importLib)
	write("mixed/m.go", "//go:build windows || !integration\n\npackage mixed\n\n"+importLib)
	// Must pass: Windows-only, test-only, the fenced tree itself, or not importing it.
	write("win/w_windows.go", "package win\n\n"+importLib)
	write("win/w_windows_amd64.go", "package win\n\n"+importLib)
	write("wintag/w.go", "//go:build windows\n\npackage wintag\n\n"+importLib)
	write("winarch/w.go", "//go:build windows && amd64\n\npackage winarch\n\n"+importLib)
	write("winneg/w.go", "//go:build windows && !integration\n\npackage winneg\n\n"+importLib)
	write("winarchneg/w.go", "//go:build windows && !arm64\n\npackage winarchneg\n\n"+importLib)
	write("ignored/i.go", "//go:build ignore\n\npackage main\n\n"+importLib)
	write("tests/t_test.go", "package tests\n\n"+importLib)
	write("internal/genshell/genshelltest/kit.go", "package genshelltest\n\n"+importLib)
	write("unrelated/u.go", "package unrelated\n\nimport _ \"fmt\"\n")

	got, scanned, err := nonWindowsGenshellImporters(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := []string{
		"customtag/c.go",
		"darwintag/d.go",
		"kit/kit.go",
		"linuxonly/l_linux.go",
		"mixed/m.go",
		"negarch/a.go",
		"negcgo/c.go",
		"negcustom/n.go",
		"nottagged/n.go",
		"plain/plain.go",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("offenders = %q, want %q", got, want)
	}
	if scanned != 18 {
		t.Errorf("scanned %d non-test files outside the fenced tree, want 18", scanned)
	}
}

// nonWindowsGenshellImporters returns, sorted and slash-separated relative to
// root, every non-test Go file outside internal/genshell that imports the
// fenced tree and can build for linux or darwin. scanned counts the non-test
// files it parsed, so a caller can tell an empty scan from a clean one.
func nonWindowsGenshellImporters(root string) (offenders []string, scanned int, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if skipFenceScanDir(path, root, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/genshell/") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if parseErr != nil {
			// An unparseable file cannot be cleared, and a guard that passes
			// when it cannot evaluate manufactures false confidence.
			return parseErr
		}
		scanned++
		imports := false
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if p == fencedImportPrefix || strings.HasPrefix(p, fencedImportPrefix+"/") {
				imports = true
				break
			}
		}
		if imports && !windowsOnly(filepath.Base(path), src) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	sort.Strings(offenders)
	return offenders, scanned, err
}

// windowsOnly reports whether a Go file can build only for GOOS=windows: its
// name carries a _windows or _windows_<arch> suffix, or its //go:build line is
// false for both linux and darwin.
func windowsOnly(base string, src []byte) bool {
	parts := strings.Split(strings.TrimSuffix(base, ".go"), "_")
	if n := len(parts); n >= 2 && parts[n-1] == "windows" ||
		n >= 3 && parts[n-2] == "windows" && fenceKnownArch[parts[n-1]] {
		return true
	}
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			// A constraint that cannot be read cannot clear the file.
			return false
		}
		return constraintExcludesLinuxAndDarwin(expr)
	}
	return false
}

// fenceMaxFreeTags bounds the free tags constraintExcludesLinuxAndDarwin
// enumerates. A constraint with more cannot clear its file.
const fenceMaxFreeTags = 12

// constraintExcludesLinuxAndDarwin reports whether expr is false in every
// GOOS=linux and GOOS=darwin build. In such a build the GOOS tag and "unix"
// are true, and "ignore" and every other operating system are false. Every
// other tag in expr (arch, cgo, goN.N, custom tags) is free: the fence cannot
// know how a build sets it, so it tries every true/false assignment of the
// free tags, and expr excludes linux and darwin only if no assignment
// satisfies it on either. A negated free tag such as !integration therefore
// cannot clear a file.
func constraintExcludesLinuxAndDarwin(expr constraint.Expr) bool {
	var free []string
	seen := map[string]bool{}
	var collect func(constraint.Expr)
	collect = func(e constraint.Expr) {
		switch e := e.(type) {
		case *constraint.TagExpr:
			if seen[e.Tag] {
				return
			}
			seen[e.Tag] = true
			if e.Tag != "unix" && e.Tag != "ignore" && !fenceKnownOS[e.Tag] {
				free = append(free, e.Tag)
			}
		case *constraint.NotExpr:
			collect(e.X)
		case *constraint.AndExpr:
			collect(e.X)
			collect(e.Y)
		case *constraint.OrExpr:
			collect(e.X)
			collect(e.Y)
		}
	}
	collect(expr)
	if len(free) > fenceMaxFreeTags {
		return false
	}
	freeIndex := make(map[string]int, len(free))
	for i, tag := range free {
		freeIndex[tag] = i
	}
	for _, goos := range []string{"linux", "darwin"} {
		for assignment := 0; assignment < 1<<len(free); assignment++ {
			ok := func(tag string) bool {
				if i, isFree := freeIndex[tag]; isFree {
					return assignment&(1<<i) != 0
				}
				return tag == goos || tag == "unix"
			}
			if expr.Eval(ok) {
				return false
			}
		}
	}
	return true
}

var fenceKnownOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
	"hurd": true, "illumos": true, "ios": true, "js": true, "linux": true, "nacl": true,
	"netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true,
	"windows": true, "zos": true,
}

var fenceKnownArch = map[string]bool{
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true, "arm64": true,
	"arm64be": true, "loong64": true, "mips": true, "mipsle": true, "mips64": true,
	"mips64le": true, "mips64p32": true, "mips64p32le": true, "ppc": true, "ppc64": true,
	"ppc64le": true, "riscv": true, "riscv64": true, "s390": true, "s390x": true,
	"sparc": true, "sparc64": true, "wasm": true,
}

// skipFenceScanDir prunes directories the scan must not descend into: vendored
// and generated trees, dot/underscore directories, and linked git worktrees
// checked out inside the repo. Mirrors skipImportScanDir in
// internal/beads/splittest/import_boundary_test.go, including the rule that the
// walk root itself is always in scope (ga-xoioq).
func skipFenceScanDir(path, root, name string) bool {
	if path == root {
		return false
	}
	if name == "vendor" || name == "node_modules" || name == "testdata" {
		return true
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	if name == "worktrees" || strings.HasPrefix(name, "worktree-") {
		return true
	}
	info, err := os.Lstat(filepath.Join(path, ".git"))
	return err == nil && !info.IsDir()
}

// fenceRepoRoot walks up from the working directory to the module root, or
// takes bazel's override. It reads the filesystem rather than shelling out to
// git, which the resource census would count as a subprocess.
func fenceRepoRoot(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return root
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root above the working directory")
		}
		dir = parent
	}
}
