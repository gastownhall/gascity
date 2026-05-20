package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestResolveInitFormulas_PreservesPackImportSymlinks is a regression for
// ga-kd2 / pe-mcpb. Earlier, cmd_init passed only the city-local formulas dir
// to ResolveFormulas, so pack-imported formula symlinks placed in
// .beads/formulas/ by a prior `gc start` were treated as stale and removed by
// cleanStaleFormulaSymlinks. resolveInitFormulas must load the full pack
// layer set and keep those symlinks.
func TestResolveInitFormulas_PreservesPackImportSymlinks(t *testing.T) {
	rootDir := t.TempDir()
	cityPath := filepath.Join(rootDir, "city")
	packDir := filepath.Join(rootDir, "packs", "mypack")

	if err := os.MkdirAll(filepath.Join(packDir, "formulas"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "mypack"
schema = 2
`)
	formulaSrc := filepath.Join(packDir, "formulas", "mol-pack-formula.toml")
	writeFile(t, formulaSrc, `formula = "mol-pack-formula"
`)

	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityPath, "city.toml"), `[workspace]
name = "testcity"
`)
	writeFile(t, filepath.Join(cityPath, "pack.toml"), `[pack]
name = "testcity"
schema = 2

[imports.mypack]
source = "../packs/mypack"
`)

	symlinkDir := filepath.Join(cityPath, ".beads", "formulas")
	if err := os.MkdirAll(symlinkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalLink := filepath.Join(symlinkDir, "mol-pack-formula.toml")
	legacyLink := filepath.Join(symlinkDir, "mol-pack-formula.formula.toml")
	for _, link := range []string{canonicalLink, legacyLink} {
		if err := os.Symlink(formulaSrc, link); err != nil {
			t.Fatalf("creating symlink %q: %v", link, err)
		}
	}

	var stderr bytes.Buffer
	resolveInitFormulas(cityPath, &stderr)
	if stderr.Len() > 0 {
		t.Fatalf("resolveInitFormulas stderr: %s", stderr.String())
	}

	for _, link := range []string{canonicalLink, legacyLink} {
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("expected symlink %q to exist after resolveInitFormulas, got: %v", link, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %q to be a symlink, got mode %v", link, fi.Mode())
		}
		target, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("Readlink(%q): %v", link, err)
		}
		if target != formulaSrc {
			t.Fatalf("symlink %q points at %q, want %q", link, target, formulaSrc)
		}
	}
}

// TestResolveInitFormulas_CreatesPackImportSymlinks verifies the positive
// case: a fresh city that imports a pack with formulas gets the pack's
// formula symlinks materialized in .beads/formulas/ by init. With the bug,
// only city-local formulas were resolved, so pack-imported formulas were
// invisible to bd until the next `gc start`.
func TestResolveInitFormulas_CreatesPackImportSymlinks(t *testing.T) {
	rootDir := t.TempDir()
	cityPath := filepath.Join(rootDir, "city")
	packDir := filepath.Join(rootDir, "packs", "mypack")

	if err := os.MkdirAll(filepath.Join(packDir, "formulas"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "mypack"
schema = 2
`)
	formulaSrc := filepath.Join(packDir, "formulas", "mol-pack-formula.toml")
	writeFile(t, formulaSrc, `formula = "mol-pack-formula"
`)

	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityPath, "city.toml"), `[workspace]
name = "testcity"
`)
	writeFile(t, filepath.Join(cityPath, "pack.toml"), `[pack]
name = "testcity"
schema = 2

[imports.mypack]
source = "../packs/mypack"
`)

	var stderr bytes.Buffer
	resolveInitFormulas(cityPath, &stderr)
	if stderr.Len() > 0 {
		t.Fatalf("resolveInitFormulas stderr: %s", stderr.String())
	}

	for _, name := range []string{"mol-pack-formula.toml", "mol-pack-formula.formula.toml"} {
		link := filepath.Join(cityPath, ".beads", "formulas", name)
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("expected symlink %q to be created by resolveInitFormulas, got: %v", link, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %q to be a symlink, got mode %v", link, fi.Mode())
		}
		target, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("Readlink(%q): %v", link, err)
		}
		if target != formulaSrc {
			t.Fatalf("symlink %q points at %q, want %q", link, target, formulaSrc)
		}
	}
}

// TestResolveInitFormulas_NoConfigFileIsSilentNoop verifies that
// resolveInitFormulas does not panic or write to stderr when there is no
// city.toml yet. This matters because the init paths that previously called
// the buggy ResolveFormulas directly were tolerant of partial scaffolds.
func TestResolveInitFormulas_NoConfigFileIsSilentNoop(t *testing.T) {
	cityPath := t.TempDir()
	var stderr bytes.Buffer
	resolveInitFormulas(cityPath, &stderr)
	if stderr.Len() > 0 {
		t.Fatalf("expected silent no-op when city.toml is missing; stderr: %s", stderr.String())
	}
}

// TestCmdInitFromTOMLFileWithOptions_PreservesPackFormulaSymlinks exercises
// the cmd_init.go:cmdInitFromTOMLFileWithOptions call site end-to-end and
// asserts that pre-existing pack formula symlinks survive an init re-run
// with --preserve-existing. Regression for ga-kd2.
func TestCmdInitFromTOMLFileWithOptions_PreservesPackFormulaSymlinks(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	disableBootstrapForTests(t)

	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })
	stubInitDependencyChecks(t)
	stubInitDoltAuthorIdentity(t, map[string]string{
		"user.name":  "Tester",
		"user.email": "tester@example.com",
	})

	rootDir := t.TempDir()
	cityPath := filepath.Join(rootDir, "city")
	packDir := filepath.Join(rootDir, "packs", "mypack")

	if err := os.MkdirAll(filepath.Join(packDir, "formulas"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "mypack"
schema = 2
`)
	formulaSrc := filepath.Join(packDir, "formulas", "mol-pack-formula.toml")
	writeFile(t, formulaSrc, `formula = "mol-pack-formula"
`)

	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityPath, "city.toml"), `[workspace]
name = "testcity"
`)
	writeFile(t, filepath.Join(cityPath, "pack.toml"), `[pack]
name = "testcity"
schema = 2

[imports.mypack]
source = "../packs/mypack"
`)
	symlinkDir := filepath.Join(cityPath, ".beads", "formulas")
	if err := os.MkdirAll(symlinkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalLink := filepath.Join(symlinkDir, "mol-pack-formula.toml")
	legacyLink := filepath.Join(symlinkDir, "mol-pack-formula.formula.toml")
	for _, link := range []string{canonicalLink, legacyLink} {
		if err := os.Symlink(formulaSrc, link); err != nil {
			t.Fatalf("creating symlink %q: %v", link, err)
		}
	}

	srcToml := filepath.Join(rootDir, "source.toml")
	writeFile(t, srcToml, `[workspace]
name = "testcity"
`)

	var stdout, stderr bytes.Buffer
	code := cmdInitFromTOMLFileWithOptions(fsys.OSFS{}, srcToml, cityPath, "", &stdout, &stderr, true, true)
	if code != 0 {
		t.Fatalf("cmdInitFromTOMLFileWithOptions = %d; stderr: %s", code, stderr.String())
	}

	for _, link := range []string{canonicalLink, legacyLink} {
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("expected symlink %q to survive init, got: %v", link, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %q to be a symlink, got mode %v", link, fi.Mode())
		}
	}
}

// TestDoInit_CreatesPackFormulaSymlinksFromExistingPackToml exercises the
// cmd_init.go:doInit call site end-to-end. Pre-creates a pack.toml with an
// import so the wizard path preserves it under --preserve-existing, then
// asserts pack formula symlinks are materialized after doInit. Regression
// for ga-kd2.
func TestDoInit_CreatesPackFormulaSymlinksFromExistingPackToml(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	disableBootstrapForTests(t)

	rootDir := t.TempDir()
	cityPath := filepath.Join(rootDir, "city")
	packDir := filepath.Join(rootDir, "packs", "mypack")

	if err := os.MkdirAll(filepath.Join(packDir, "formulas"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "mypack"
schema = 2
`)
	formulaSrc := filepath.Join(packDir, "formulas", "mol-pack-formula.toml")
	writeFile(t, formulaSrc, `formula = "mol-pack-formula"
`)

	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cityPath, "pack.toml"), `[pack]
name = "testcity"
schema = 2

[imports.mypack]
source = "../packs/mypack"
`)

	var stdout, stderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, defaultWizardConfig(), "", &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("doInit = %d; stderr: %s", code, stderr.String())
	}

	for _, name := range []string{"mol-pack-formula.toml", "mol-pack-formula.formula.toml"} {
		link := filepath.Join(cityPath, ".beads", "formulas", name)
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("expected symlink %q after doInit, got: %v", link, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected %q to be a symlink, got mode %v", link, fi.Mode())
		}
	}
}
