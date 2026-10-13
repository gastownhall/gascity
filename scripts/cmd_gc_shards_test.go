package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCmdGCIntegrationSoloTestsRunExactlyOnce guards cmd/gc/BUILD.bazel's
// INTEGRATION_SOLO_TESTS: gc_test's longest process-backed tests, which the
// integration build skips in gc_test and runs each alone in a target of its
// own (integration-packages' gc_test shards ran 110-128 s with two of them in
// one shard, against ~40 s for the rest). Each seam would quietly drop a test
// from the gating lane or run it twice:
//
//   - every entry names a top-level test cmd/gc declares (a stale name runs
//     nothing in its target and skips nothing in gc_test);
//   - gc_test skips exactly the entries, and only in the integration build
//     (the unit build still runs them in gc_test);
//   - the targets run the entries by exact name, are manual (the unit lane's
//     //... must not run them a second time), and the
//     :gc_integration_solo_tests suite names them all;
//   - //test:integration_packages, which bazel.yml's integration-packages
//     lane runs, names the suite.
//
// INTEGRATION_SOLO_SPLIT_TESTS holds the solo tests still too long to run
// whole (their subtests run in series): one target per named first-level
// subtest, and one ("") for the rest. The same seams apply, plus: each split
// test is declared and is not also a whole-test entry, and each named subtest
// is one some cmd/gc test file spells as a string literal (a renamed case
// would leave its target running only the parent).
func TestCmdGCIntegrationSoloTestsRunExactlyOnce(t *testing.T) {
	root := repoRoot(t)
	build := readRepoFile(t, root, "cmd/gc/BUILD.bazel")

	table := regexp.MustCompile(`(?ms)^INTEGRATION_SOLO_TESTS = \{\n(.*?)^\}`).FindStringSubmatch(build)
	if table == nil {
		t.Fatal("cmd/gc/BUILD.bazel has no INTEGRATION_SOLO_TESTS dict")
	}
	entries := regexp.MustCompile(`(?m)^    "([a-z0-9_]+_test)": "(Test[A-Za-z0-9_]+)",`).FindAllStringSubmatch(table[1], -1)
	if len(entries) == 0 {
		t.Fatal("INTEGRATION_SOLO_TESTS has no entries; the scan is broken")
	}

	declared := map[string]bool{}
	var testSources strings.Builder
	files, err := filepath.Glob(filepath.Join(root, "cmd", "gc", "*_test.go"))
	if err != nil {
		t.Fatalf("glob cmd/gc: %v", err)
	}
	for _, path := range files {
		body, err := os.ReadFile(path) //nolint:gosec // a path this test globbed inside the repo
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		testSources.Write(body)
		for _, name := range topLevelTestFunctions(string(body)) {
			declared[name] = true
		}
	}
	seen := map[string]string{}
	for _, entry := range entries {
		if !declared[entry[2]] {
			t.Errorf("INTEGRATION_SOLO_TESTS %s names %s, which no cmd/gc test file declares: the target runs nothing", entry[1], entry[2])
		}
		if prev, ok := seen[entry[2]]; ok {
			t.Errorf("INTEGRATION_SOLO_TESTS runs %s in both %s and %s", entry[2], prev, entry[1])
		}
		seen[entry[2]] = entry[1]
	}

	splitTable := regexp.MustCompile(`(?ms)^INTEGRATION_SOLO_SPLIT_TESTS = \{\n(.*?)^\}`).FindStringSubmatch(build)
	if splitTable == nil {
		t.Fatal("cmd/gc/BUILD.bazel has no INTEGRATION_SOLO_SPLIT_TESTS dict")
	}
	splits := regexp.MustCompile(`(?ms)^    "(Test[A-Za-z0-9_]+)": \{\n(.*?)^    \},`).FindAllStringSubmatch(splitTable[1], -1)
	if len(splits) == 0 {
		t.Fatal("INTEGRATION_SOLO_SPLIT_TESTS has no entries; the scan is broken")
	}
	targets := map[string]bool{}
	for _, entry := range entries {
		targets[entry[1]] = true
	}
	for _, split := range splits {
		test := split[1]
		if !declared[test] {
			t.Errorf("INTEGRATION_SOLO_SPLIT_TESTS names %s, which no cmd/gc test file declares: its targets run nothing", test)
		}
		if prev, ok := seen[test]; ok {
			t.Errorf("INTEGRATION_SOLO_SPLIT_TESTS splits %s, which INTEGRATION_SOLO_TESTS also runs whole in %s", test, prev)
		}
		seen[test] = "INTEGRATION_SOLO_SPLIT_TESTS"
		parts := regexp.MustCompile(`(?m)^        "([a-z0-9_]+_test)": "([A-Za-z0-9_-]*)",`).FindAllStringSubmatch(split[2], -1)
		rest := 0
		for _, part := range parts {
			if targets[part[1]] {
				t.Errorf("INTEGRATION_SOLO_SPLIT_TESTS target %s is defined twice", part[1])
			}
			targets[part[1]] = true
			if part[2] == "" {
				rest++
				continue
			}
			if !strings.Contains(testSources.String(), `"`+part[2]+`/`) && !strings.Contains(testSources.String(), `"`+part[2]+`"`) {
				t.Errorf("INTEGRATION_SOLO_SPLIT_TESTS %s narrows %s to subtest %q, which no cmd/gc test file spells: the target would run the parent alone", part[1], test, part[2])
			}
		}
		if rest != 1 || len(parts) < 2 {
			t.Errorf("INTEGRATION_SOLO_SPLIT_TESTS %s needs at least one named subtest and exactly one \"\" target for the rest; got %d parts, %d rest", test, len(parts), rest)
		}
	}

	rule := goTestRule(t, build, "cmd/gc/BUILD.bazel", "gc_test")
	wantArgs := `    args = select({
        "//:gotags_integration": ["-test.skip=^(%s)$$" % "|".join(INTEGRATION_SOLO_TESTS.values() + INTEGRATION_SOLO_SPLIT_TESTS.keys())],
        "//conditions:default": [],
    }),
`
	if !strings.Contains(rule, wantArgs) {
		t.Errorf("gc_test must skip exactly INTEGRATION_SOLO_TESTS and INTEGRATION_SOLO_SPLIT_TESTS in the integration build, and nothing in the unit build:\nwant\n%s", wantArgs)
	}

	wantVariants := `[go_variant_test(
    name = name,
    size = "large",
    args = ["-test.run=^%s$$" % test],
    tags = ["manual"],
    test = ":gc_test",
) for name, test in INTEGRATION_SOLO_TESTS.items()]

[go_variant_test(
    name = name,
    size = "large",
    args = ["-test.run=^%s$$/^%s$$" % (test, subtest)] if subtest else [
        "-test.run=^%s$$" % test,
        "-test.skip=^%s$$/^(%s)$$" % (
            test,
            "|".join([s for s in parts.values() if s]),
        ),
    ],
    tags = ["manual"],
    test = ":gc_test",
) for test, parts in INTEGRATION_SOLO_SPLIT_TESTS.items() for name, subtest in parts.items()]

test_suite(
    name = "gc_integration_solo_tests",
    tags = ["manual"],
    tests = [":" + name for name in INTEGRATION_SOLO_TESTS] + [
        ":" + name
        for parts in INTEGRATION_SOLO_SPLIT_TESTS.values()
        for name in parts
    ],
)
`
	if !strings.Contains(build, wantVariants) {
		t.Errorf("cmd/gc/BUILD.bazel must run each INTEGRATION_SOLO_TESTS entry by exact name, and each INTEGRATION_SOLO_SPLIT_TESTS part by subtest, in a manual target, all named by :gc_integration_solo_tests:\nwant\n%s", wantVariants)
	}

	suite := readRepoFile(t, root, "test/BUILD.bazel")
	if !strings.Contains(suite, `        "//cmd/gc:gc_integration_solo_tests",`) {
		t.Error("//test:integration_packages does not name //cmd/gc:gc_integration_solo_tests: the integration-packages lane would run none of INTEGRATION_SOLO_TESTS")
	}
	if !strings.Contains(suite, `        "//cmd/gc:gc_test",`) {
		t.Error("//test:integration_packages does not name //cmd/gc:gc_test")
	}
}
