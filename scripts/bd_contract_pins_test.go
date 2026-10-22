package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The bd contract cells run under Bazel against prebuilt bd release
// archives (MODULE.bazel http_archive), not against a bd installed by a
// go-test job. These pins keep each cell testing the bd deps.env names:
//
//   - every bd_bin_v* archive downloads that version's linux_amd64 release
//     with the sha256 .github/scripts/install-bd-archive.sh pins for it;
//   - //test/acceptance:bd_cli_contract_prev_test runs BD_PREV_VERSION's bd;
//   - //test/acceptance:bd_cli_contract_current_test and
//     //internal/beads:bd_conditional_release_contract_test (bd first on
//     PATH through GC_TEST_TOOL_PATHS) run BD_CURRENT_VERSION's bd. BD_CURRENT_REF is the commit that release was
//     cut from (its tag); moving the ref off a released tag needs a
//     source-built bd here instead, and this test then has no archive to
//     point at.
//   - the conditional-release target selects the Makefile's
//     BD_CONDITIONAL_RELEASE_TEST, which must exist in internal/beads under
//     the integration tag: a -run selector that matches nothing passes.
func TestBdContractCellsRunTheDepsEnvPins(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(data)
	}
	depsEnv := read("deps.env")
	module := read("MODULE.bazel")
	installer := read(".github/scripts/install-bd-archive.sh")
	acceptance := read("test/acceptance/BUILD.bazel")
	beadsBuild := read("internal/beads/BUILD.bazel")
	makefile := read("Makefile")

	pin := func(key string) string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^` + key + `=(\S+)$`).FindStringSubmatch(depsEnv)
		if m == nil {
			t.Fatalf("deps.env has no %s", key)
		}
		return m[1]
	}
	repoFor := func(version string) string {
		return "bd_bin_" + strings.NewReplacer(".", "_", "-", "_").Replace(version)
	}
	checkArchive := func(version string) {
		t.Helper()
		repo := repoFor(version)
		block := regexp.MustCompile(`(?ms)^http_archive\(\n    name = "` + repo + `",\n.*?^\)`).FindString(module)
		if block == "" {
			t.Fatalf("MODULE.bazel has no http_archive %s for bd %s", repo, version)
		}
		bare := strings.TrimPrefix(version, "v")
		url := fmt.Sprintf("https://github.com/gastownhall/beads/releases/download/%s/beads_%s_linux_amd64.tar.gz", version, bare)
		if !strings.Contains(block, `"`+url+`"`) {
			t.Errorf("%s does not download %s:\n%s", repo, url, block)
		}
		sha := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(version) + `:linux_amd64\) expected_sha="([0-9a-f]{64})"`).FindStringSubmatch(installer)
		if sha == nil {
			t.Fatalf("install-bd-archive.sh pins no linux_amd64 sha256 for %s", version)
		}
		if !strings.Contains(block, `sha256 = "`+sha[1]+`"`) {
			t.Errorf("%s sha256 differs from install-bd-archive.sh's %s:\n%s", repo, sha[1], block)
		}
	}
	rule := func(build, name string) string {
		t.Helper()
		r := regexp.MustCompile(`(?ms)^go_variant_test\(\n    name = "` + name + `",\n.*?^\)`).FindString(build)
		if r == "" {
			t.Fatalf("no go_variant_test %s", name)
		}
		if strings.Contains(r, "manual") {
			t.Errorf("%s must run in //... (the unit lane), not be tagged manual", name)
		}
		return r
	}

	prev, current := pin("BD_PREV_VERSION"), pin("BD_CURRENT_VERSION")
	checkArchive(prev)
	checkArchive(current)

	for name, version := range map[string]string{
		"bd_cli_contract_prev_test":    prev,
		"bd_cli_contract_current_test": current,
	} {
		r := rule(acceptance, name)
		label := "@" + repoFor(version) + "//:bd"
		for _, want := range []string{
			// The cell's bd is the only one: named for helpers.FindBD and
			// first on PATH, over the one :acceptance_test's env puts there.
			`"GC_ACCEPTANCE_BD_BIN": "$(rootpath ` + label + `)",`,
			`"GC_TEST_TOOL_PATHS": "$(rootpath ` + label + `)",`,
			`gotags = ["acceptance_bd_contract"],`,
			`test = ":acceptance_test",`,
		} {
			if !strings.Contains(r, want) {
				t.Errorf("%s lacks %s:\n%s", name, want, r)
			}
		}
	}

	r := rule(beadsBuild, "bd_conditional_release_contract_test")
	testName := regexp.MustCompile(`(?m)^BD_CONDITIONAL_RELEASE_TEST = (\S+)$`).FindStringSubmatch(makefile)
	if testName == nil {
		t.Fatal("Makefile has no BD_CONDITIONAL_RELEASE_TEST")
	}
	for _, want := range []string{
		`args = ["-test.run=^` + testName[1] + `$$"],`,
		`"GC_REQUIRE_BD_CONDITIONAL_RELEASE": "1",`,
		`gotags = ["integration"],`,
		`"GC_TEST_TOOL_PATHS": "$(rootpath @` + repoFor(current) + `//:bd)",`,
		`data = ["@` + repoFor(current) + `//:bd"],`,
	} {
		if !strings.Contains(r, want) {
			t.Errorf("bd_conditional_release_contract_test lacks %s:\n%s", want, r)
		}
	}
	files, err := filepath.Glob(filepath.Join(root, "internal", "beads", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		if strings.HasPrefix(src, "//go:build integration\n") && strings.Contains(src, "\nfunc "+testName[1]+"(t *testing.T) {") {
			found = true
		}
	}
	if !found {
		t.Errorf("no //go:build integration test file in internal/beads defines %s; the contract target would select nothing", testName[1])
	}
}
