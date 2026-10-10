package scripts_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testHooksStamp is the link-time variable that turns gc's test-only hooks
// on (internal/clock.testHooks; today GC_TEST_BACKSTOP_SPEEDUP).
const testHooksStamp = "internal/clock.testHooks"

// TestOnlyTheTestonlyGCLinkCarriesTheTestHooksStamp keeps gc's test hooks out
// of every binary a user can run. The hook is honored only in a binary linked
// with -X .../internal/clock.testHooks=enabled, so this pins that:
//
//   - exactly one Bazel target stamps it, //cmd/gc:gc_testhooks, and that
//     target is testonly, manual and visible to //test/acceptance only;
//   - the release config (.goreleaser.yml) and the Makefile's LDFLAGS never
//     mention it;
//   - every acceptance target that sets GC_TEST_BACKSTOP_SPEEDUP runs that
//     binary (GC_ACCEPTANCE_GC_BIN), since a gc without the stamp ignores it.
func TestOnlyTheTestonlyGCLinkCarriesTheTestHooksStamp(t *testing.T) {
	root := repoRoot(t)
	var stamped []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "bazel-") || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "BUILD.bazel" && d.Name() != "BUILD" && !strings.HasSuffix(d.Name(), ".bzl") {
			return nil
		}
		body, err := os.ReadFile(path) //nolint:gosec // a path this test walked inside the repo
		if err != nil {
			return err
		}
		if strings.Contains(string(body), testHooksStamp+`": "enabled"`) {
			rel, _ := filepath.Rel(root, path)
			stamped = append(stamped, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(stamped) != 1 || stamped[0] != "cmd/gc/BUILD.bazel" {
		t.Fatalf("BUILD files stamping %s: %v; want cmd/gc/BUILD.bazel only", testHooksStamp, stamped)
	}

	build := readRepoFile(t, root, "cmd/gc/BUILD.bazel")
	rule := regexp.MustCompile(`(?ms)^go_binary\(\n    name = "gc_testhooks",\n(.*?)^\)`).FindStringSubmatch(build)
	if rule == nil {
		t.Fatal("cmd/gc/BUILD.bazel has no go_binary gc_testhooks")
	}
	for _, want := range []string{
		"    testonly = True,\n",
		`    tags = ["manual"],` + "\n",
		`    visibility = ["//test/acceptance:__pkg__"],` + "\n",
		`    x_defs = {"github.com/gastownhall/gascity/` + testHooksStamp + `": "enabled"},` + "\n",
	} {
		if !strings.Contains(rule[1], want) {
			t.Errorf("gc_testhooks lacks %q:\n%s", strings.TrimSpace(want), rule[1])
		}
	}
	if n := strings.Count(starlarkCode(build), testHooksStamp); n != 1 {
		t.Errorf("cmd/gc/BUILD.bazel mentions %s %d times; only gc_testhooks may stamp it", testHooksStamp, n)
	}

	for _, rel := range []string{".goreleaser.yml", "Makefile"} {
		if strings.Contains(readRepoFile(t, root, rel), "testHooks") {
			t.Errorf("%s mentions testHooks; release and developer builds must never enable gc's test hooks", rel)
		}
	}

	acc := readRepoFile(t, root, "test/acceptance/BUILD.bazel")
	hooksEnv := regexp.MustCompile(`(?ms)^TESTHOOKS_GC_ENV = \{\n(.*?)^\}`).FindStringSubmatch(acc)
	if hooksEnv == nil {
		t.Fatal("test/acceptance/BUILD.bazel has no TESTHOOKS_GC_ENV dict")
	}
	if !strings.Contains(hooksEnv[1], `"GC_ACCEPTANCE_GC_BIN": "$(rootpath //cmd/gc:gc_testhooks)",`) {
		t.Errorf("TESTHOOKS_GC_ENV does not run //cmd/gc:gc_testhooks:\n%s", hooksEnv[1])
	}
	if n := strings.Count(starlarkCode(acc), "GC_TEST_BACKSTOP_SPEEDUP"); n != strings.Count(hooksEnv[1], "GC_TEST_BACKSTOP_SPEEDUP") {
		t.Errorf("test/acceptance/BUILD.bazel sets GC_TEST_BACKSTOP_SPEEDUP outside TESTHOOKS_GC_ENV (%d mentions); "+
			"a target that sets it must run the test-hooks gc", n)
	}
}

// starlarkCode is a BUILD file without its comment lines.
func starlarkCode(build string) string {
	var b strings.Builder
	for _, line := range strings.Split(build, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
