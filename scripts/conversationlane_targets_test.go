package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestConversationLaneTargetsRunEachTestOnce guards the split of
// //test/conversationlane's suite in the integration build
// (test/conversationlane/BUILD.bazel): INTEGRATION_SOLO_TESTS maps a target
// to the top-level tests it runs (a regular expression over their names, so
// every X12 test shares one process and the off baselines it learns), and
// INTEGRATION_SOLO_SPLIT_TESTS splits a top-level test by first-level
// subtest, one target per named subtest and one ("") for the rest.
// conversationlane_test skips all of them and keeps everything else (X6,
// the CLI inline pass, and any test added later). Each seam would quietly
// drop a test from the gating lane or run it twice:
//
//   - every declared top-level test runs in exactly one target;
//   - every entry matches a test the package declares (a stale entry runs
//     nothing in its target and skips nothing in conversationlane_test);
//   - every named subtest is one some test file spells as a string literal
//     (a renamed arm would leave its target running only the parent);
//   - conversationlane_test skips exactly the entries, and only in the
//     integration build;
//   - the targets run their entries, are manual (the unit lane's //... must
//     not run them), and :conversationlane_integration_solo_tests names them
//     all, which //test:integration_packages names.
func TestConversationLaneTargetsRunEachTestOnce(t *testing.T) {
	root := repoRoot(t)
	const pkg = "test/conversationlane"
	build := readRepoFile(t, root, pkg+"/BUILD.bazel")

	soloTable := regexp.MustCompile(`(?ms)^INTEGRATION_SOLO_TESTS = \{\n(.*?)^\}`).FindStringSubmatch(build)
	if soloTable == nil {
		t.Fatalf("%s/BUILD.bazel has no INTEGRATION_SOLO_TESTS dict", pkg)
	}
	solos := conversationLaneTableEntries(t, "INTEGRATION_SOLO_TESTS", soloTable[1], "    ")
	if len(solos) == 0 {
		t.Fatal("INTEGRATION_SOLO_TESTS has no entries; the scan is broken")
	}
	splitTable := regexp.MustCompile(`(?ms)^INTEGRATION_SOLO_SPLIT_TESTS = \{\n(.*?)^\}`).FindStringSubmatch(build)
	if splitTable == nil {
		t.Fatalf("%s/BUILD.bazel has no INTEGRATION_SOLO_SPLIT_TESTS dict", pkg)
	}
	splits := regexp.MustCompile(`(?ms)^    "(Test[A-Za-z0-9_]+)": \{\n(.*?)^    \},`).FindAllStringSubmatch(splitTable[1], -1)
	if len(splits) == 0 {
		t.Fatal("INTEGRATION_SOLO_SPLIT_TESTS has no entries; the scan is broken")
	}

	var declared []string
	var sources strings.Builder
	files, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pkg), "*_test.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", pkg, err)
	}
	for _, path := range files {
		body, err := os.ReadFile(path) //nolint:gosec // a path this test globbed inside the repo
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sources.Write(body)
		declared = append(declared, topLevelTestFunctions(string(body))...)
	}
	if !slices.Contains(declared, "TestX6FanIn") {
		t.Fatalf("%s declares no TestX6FanIn; the scan is broken (found %v)", pkg, declared)
	}

	// Which entries take each declared test out of conversationlane_test.
	takers := map[string][]string{}
	targets := map[string]bool{}
	for _, solo := range solos {
		name, pattern := solo[1], solo[2]
		if targets[name] {
			t.Errorf("target %s is defined twice", name)
		}
		targets[name] = true
		re, err := regexp.Compile("^(" + pattern + ")$")
		if err != nil {
			t.Errorf("INTEGRATION_SOLO_TESTS %s: %q is not a regular expression: %v", name, pattern, err)
			continue
		}
		matched := 0
		for _, test := range declared {
			if re.MatchString(test) {
				takers[test] = append(takers[test], name)
				matched++
			}
		}
		if matched == 0 {
			t.Errorf("INTEGRATION_SOLO_TESTS %s runs %q, which matches no test %s declares: the target runs nothing", name, pattern, pkg)
		}
	}
	for _, split := range splits {
		test := split[1]
		if !slices.Contains(declared, test) {
			t.Errorf("INTEGRATION_SOLO_SPLIT_TESTS names %s, which %s does not declare: its targets run nothing", test, pkg)
		}
		takers[test] = append(takers[test], "INTEGRATION_SOLO_SPLIT_TESTS")
		parts := conversationLaneTableEntries(t, "INTEGRATION_SOLO_SPLIT_TESTS "+test, split[2], "        ")
		rest := 0
		for _, part := range parts {
			if targets[part[1]] {
				t.Errorf("target %s is defined twice", part[1])
			}
			targets[part[1]] = true
			if part[2] == "" {
				rest++
				continue
			}
			if !strings.Contains(sources.String(), `"`+part[2]+`"`) {
				t.Errorf("INTEGRATION_SOLO_SPLIT_TESTS %s narrows %s to subtest %q, which no %s test file spells: the target would run the parent alone", part[1], test, part[2], pkg)
			}
		}
		if rest != 1 || len(parts) < 2 {
			t.Errorf("INTEGRATION_SOLO_SPLIT_TESTS %s needs at least one named subtest and exactly one \"\" target for the rest; got %d parts, %d rest", test, len(parts), rest)
		}
	}
	for _, test := range declared {
		if len(takers[test]) > 1 {
			t.Errorf("%s runs in more than one target: %v", test, takers[test])
		}
	}

	rule := goTestRule(t, build, pkg+"/BUILD.bazel", "conversationlane_test")
	wantArgs := `    args = select({
        "//:gotags_integration": ["-test.skip=^(%s)$$" % "|".join(INTEGRATION_SOLO_TESTS.values() + INTEGRATION_SOLO_SPLIT_TESTS.keys())],
        "//conditions:default": [],
    }),
`
	if !strings.Contains(rule, wantArgs) {
		t.Errorf("conversationlane_test must skip exactly INTEGRATION_SOLO_TESTS and INTEGRATION_SOLO_SPLIT_TESTS in the integration build, and nothing in the unit build:\nwant\n%s", wantArgs)
	}

	wantVariants := `[go_variant_test(
    name = name,
    size = "large",
    args = ["-test.run=^(%s)$$" % tests],
    tags = ["manual"],
    test = ":conversationlane_test",
) for name, tests in INTEGRATION_SOLO_TESTS.items()]

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
    test = ":conversationlane_test",
) for test, parts in INTEGRATION_SOLO_SPLIT_TESTS.items() for name, subtest in parts.items()]

test_suite(
    name = "conversationlane_integration_solo_tests",
    tags = ["manual"],
    tests = [":" + name for name in INTEGRATION_SOLO_TESTS] + [
        ":" + name
        for parts in INTEGRATION_SOLO_SPLIT_TESTS.values()
        for name in parts
    ],
)
`
	if !strings.Contains(build, wantVariants) {
		t.Errorf("%s/BUILD.bazel must run each INTEGRATION_SOLO_TESTS entry and each INTEGRATION_SOLO_SPLIT_TESTS part in a manual target, all named by :conversationlane_integration_solo_tests:\nwant\n%s", pkg, wantVariants)
	}

	suite := readRepoFile(t, root, "test/BUILD.bazel")
	for _, label := range []string{"//test/conversationlane:conversationlane_integration_solo_tests", "//test/conversationlane:conversationlane_test"} {
		if !strings.Contains(suite, `        "`+label+`",`) {
			t.Errorf("//test:integration_packages does not name %s: the integration-packages lane would not run it", label)
		}
	}
}

// conversationLaneTableEntries parses a BUILD dict body whose entries are
// `<indent>"<target>": "<value>",` lines (each may end in a comment): every
// other line must be blank or a comment, so an entry the scan cannot read
// fails the test instead of escaping its checks. Each match is
// {line, target, value}, as regexp submatches are.
func conversationLaneTableEntries(t *testing.T, table, body, indent string) [][]string {
	t.Helper()
	entry := regexp.MustCompile(`^` + indent + `"(conversationlane_[a-z0-9_]+_test)": "([^"]*)",(?:\s+#.*)?$`)
	var out [][]string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := entry.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("%s: cannot read %q as `\"conversationlane_<name>_test\": \"<value>\",`", table, trimmed)
			continue
		}
		out = append(out, m)
	}
	return out
}
