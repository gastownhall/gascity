package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const integrationSuiteRepoTreeBlock = "# --- bazel_repo_srcs (managed by tools/bazel/repo_tree.py) ---\n\nfilegroup(name = \"bazel_repo_srcs\")\n"

// newIntegrationSuiteTree copies tools/bazel/integration_suite.py into a
// scratch repository holding test/BUILD.bazel and the given files.
func newIntegrationSuiteTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "tools", "bazel", "integration_suite.py"))
	if err != nil {
		t.Fatalf("read integration_suite.py: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "tools", "bazel", "integration_suite.py"), string(script))
	writeTestFile(t, filepath.Join(root, "test", "BUILD.bazel"), "filegroup(name = \"agents\")\n\n"+integrationSuiteRepoTreeBlock)
	for path, content := range files {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(path)), content)
	}
	return root
}

func runIntegrationSuite(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	cmd := testCommand("python3", append([]string{filepath.Join(root, "tools", "bazel", "integration_suite.py")}, args...)...)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func goTestBuild(names ...string) string {
	var b strings.Builder
	for _, name := range names {
		b.WriteString("go_test(\n    name = \"" + name + "\",\n    srcs = [\"x_test.go\"],\n)\n\n")
	}
	return b.String()
}

// TestIntegrationSuiteListsEveryPackageTheIntegrationBuildChanges pins which
// packages tools/bazel/integration_suite.py puts in //test:integration_packages:
// those with an integration-tagged .go file (test or not) or a GC_FAST_UNIT=0
// gate, minus the tiers with lanes of their own; and that a re-run (and
// --check) is a fixed point.
func TestIntegrationSuiteListsEveryPackageTheIntegrationBuildChanges(t *testing.T) {
	root := newIntegrationSuiteTree(t, map[string]string{
		"tagged/BUILD.bazel": goTestBuild("tagged_test", "tagged_more_test") +
			"test_suite(\n    name = \"tagged_integration_solo_tests\",\n    tests = [\":tagged_test\"],\n)\n\n" +
			"test_suite(\n    name = \"tagged_other_tests\",\n    tests = [\":tagged_test\"],\n)\n\n",
		"tagged/a_test.go":                   "//go:build integration\n\npackage tagged\n",
		"either/BUILD.bazel":                 goTestBuild("either_test"),
		"either/a_test.go":                   "//go:build integration || dolt_integration\n\npackage either\n",
		"library/BUILD.bazel":                goTestBuild("library_test"),
		"library/seam.go":                    "// Copyright notice.\n\n//go:build integration && linux\n\npackage library\n",
		"gated/BUILD.bazel":                  goTestBuild("gated_test"),
		"gated/a_test.go":                    "package gated\n\nfunc TestX(t *testing.T) {\n\tprocessgrouptest.RequireRealProcessSignals(t)\n}\n",
		"herdr/BUILD.bazel":                  goTestBuild("herdr_test"),
		"herdr/a_test.go":                    "package herdr\n\nfunc TestX(t *testing.T) {\n\therdrtest.RequireLive(t)\n}\n",
		"plain/BUILD.bazel":                  goTestBuild("plain_test"),
		"plain/a_test.go":                    "package plain\n\nconst s = \"processgrouptest.RequireRealProcessSignals(t)\"\n",
		"othertag/BUILD.bazel":               goTestBuild("othertag_test"),
		"othertag/a_test.go":                 "//go:build dolt_integration\n\npackage othertag\n",
		"fixture/BUILD.bazel":                goTestBuild("fixture_test"),
		"fixture/a_test.go":                  "package fixture\n\nconst src = `\n//go:build integration\n`\n",
		"nobuild/a_test.go":                  "//go:build integration\n\npackage nobuild\n",
		"own/testdata/a_test.go":             "//go:build integration\n\npackage testdata\n",
		"own/BUILD.bazel":                    goTestBuild("own_test"),
		"test/integration/BUILD.bazel":       goTestBuild("integration_test"),
		"test/integration/a_test.go":         "//go:build integration\n\npackage integration\n",
		"test/acceptance/tier_b/BUILD.bazel": goTestBuild("tier_b_test"),
		"test/acceptance/tier_b/a_test.go":   "//go:build integration\n\npackage tierb\n",
	})

	if output, err := runIntegrationSuite(t, root, "--check"); err == nil {
		t.Fatalf("--check passed with no suite written:\n%s", output)
	}
	if output, err := runIntegrationSuite(t, root); err != nil {
		t.Fatalf("integration_suite.py: %v\n%s", err, output)
	}
	got := readBuild(t, root, "test")
	for _, label := range []string{
		"//either:either_test",
		"//gated:gated_test",
		"//herdr:herdr_test",
		"//library:library_test",
		"//tagged:tagged_integration_solo_tests",
		"//tagged:tagged_more_test",
		"//tagged:tagged_test",
	} {
		if !strings.Contains(got, "        \""+label+"\",\n") {
			t.Errorf("suite lacks %s:\n%s", label, got)
		}
	}
	if strings.Contains(got, "//tagged:tagged_other_tests") {
		t.Errorf("suite lists a test_suite not named *_integration_solo_tests:\n%s", got)
	}
	for _, pkg := range []string{"plain", "othertag", "fixture", "own", "nobuild", "test/integration", "test/acceptance"} {
		if strings.Contains(got, "\"//"+pkg) {
			t.Errorf("suite lists %s:\n%s", pkg, got)
		}
	}
	if !strings.Contains(got, "    name = \"integration_packages\",\n    tags = [\"manual\"],\n") {
		t.Errorf("suite is not a manual test_suite named integration_packages:\n%s", got)
	}
	if !strings.HasPrefix(got, "filegroup(name = \"agents\")\n\n") || !strings.HasSuffix(got, integrationSuiteRepoTreeBlock) {
		t.Errorf("suite block must sit between the existing rules and repo_tree.py's block, which stays last:\n%s", got)
	}

	if output, err := runIntegrationSuite(t, root, "--check"); err != nil {
		t.Fatalf("--check after sync: %v\n%s", err, output)
	}
	if output, err := runIntegrationSuite(t, root); err != nil {
		t.Fatalf("second run: %v\n%s", err, output)
	}
	if again := readBuild(t, root, "test"); again != got {
		t.Errorf("second run changed test/BUILD.bazel:\n%s\nwant:\n%s", again, got)
	}

	// A package that drops its last integration file leaves the suite.
	if err := os.Remove(filepath.Join(root, "either", "a_test.go")); err != nil {
		t.Fatal(err)
	}
	if output, err := runIntegrationSuite(t, root, "--check"); err == nil {
		t.Fatalf("--check passed on a stale suite:\n%s", output)
	}
	if output, err := runIntegrationSuite(t, root); err != nil {
		t.Fatalf("integration_suite.py: %v\n%s", err, output)
	}
	if got := readBuild(t, root, "test"); strings.Contains(got, "//either") {
		t.Errorf("suite still lists //either after its integration file was removed:\n%s", got)
	}
}
