package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoTreeManagedBlock is the managed per-package block repo_tree.py writes,
// in the exact form gazelle's formatter leaves it. Emitting anything else
// makes `make bazel-sync` a non-fixed point: gazelle reformats the block on
// the next run and the "BUILD files in sync" check goes red (ga-1kn9f3).
const repoTreeManagedBlock = `# --- bazel_repo_srcs (managed by tools/bazel/repo_tree.py) ---

filegroup(
    name = "bazel_repo_srcs",
    srcs = glob(
        ["**"],
        allow_empty = True,
        exclude = ["BUILD.bazel"],
    ),
    visibility = ["//visibility:public"],
)

filegroup(
    name = "bazel_go_srcs",
    srcs = glob(
        ["**/*.go"],
        allow_empty = True,
        exclude = ["**/*_test.go"],
    ),
    visibility = ["//visibility:public"],
)

filegroup(
    name = "bazel_go_test_srcs",
    srcs = glob(
        ["**/*_test.go"],
        allow_empty = True,
    ),
    visibility = ["//visibility:public"],
)
`

const repoTreeRootBuild = `load("@rules_go//go:def.bzl", "go_library")

go_library(
    name = "gascity",
    srcs = ["schemas_embed.go"],
)
`

// newRepoTreeFixture copies tools/bazel/repo_tree.py into a scratch
// repository holding the given files.
func newRepoTreeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "tools", "bazel", "repo_tree.py"))
	if err != nil {
		t.Fatalf("read repo_tree.py: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "tools", "bazel", "repo_tree.py"), string(script))
	writeTestFile(t, filepath.Join(root, "MODULE.bazel"), "module(name = \"fixture\")\n")
	if _, ok := files["BUILD.bazel"]; !ok {
		writeTestFile(t, filepath.Join(root, "BUILD.bazel"), repoTreeRootBuild)
	}
	for rel, content := range files {
		writeTestFile(t, filepath.Join(root, rel), content)
	}
	return root
}

func runRepoTree(t *testing.T, root string) {
	t.Helper()
	cmd := testCommand("python3", filepath.Join(root, "tools", "bazel", "repo_tree.py"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repo_tree.py: %v\n%s", err, output)
	}
}

func readRepoTreeFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func TestRepoTreeWritesGazelleCanonicalBlockForNewPackage(t *testing.T) {
	rule := "go_library(\n    name = \"fresh\",\n    srcs = [\"fresh.go\"],\n)\n"
	root := newRepoTreeFixture(t, map[string]string{
		"internal/fresh/BUILD.bazel": rule,
		"internal/fresh/fresh.go":    "package fresh\n",
	})

	runRepoTree(t, root)

	want := rule + "\n" + repoTreeManagedBlock
	if got := readRepoTreeFile(t, root, "internal/fresh/BUILD.bazel"); got != want {
		t.Errorf("internal/fresh/BUILD.bazel:\n%s\nwant:\n%s", got, want)
	}
}

func TestRepoTreeIsAFixedPoint(t *testing.T) {
	root := newRepoTreeFixture(t, map[string]string{
		"internal/a/BUILD.bazel": "go_library(\n    name = \"a\",\n)\n",
		"cmd/b/BUILD.bazel":      "go_binary(\n    name = \"b\",\n)\n",
	})
	runRepoTree(t, root)
	first := map[string]string{}
	for _, rel := range []string{"BUILD.bazel", "internal/a/BUILD.bazel", "cmd/b/BUILD.bazel"} {
		first[rel] = readRepoTreeFile(t, root, rel)
	}

	runRepoTree(t, root)

	for rel, want := range first {
		if got := readRepoTreeFile(t, root, rel); got != want {
			t.Errorf("%s changed on the second run:\n%s\nfirst run:\n%s", rel, got, want)
		}
	}
}

func TestRepoTreeRewritesLegacyBlocksInPlace(t *testing.T) {
	legacy := "# --- bazel_repo_srcs (managed by tools/bazel/repo_tree.py) ---\n\n" +
		"filegroup(\n" +
		"    name = \"bazel_repo_srcs\",\n" +
		"    srcs = glob([\"**\"], exclude = [\"BUILD.bazel\"], allow_empty = True),\n" +
		"    visibility = [\"//visibility:public\"],\n" +
		")\n"
	before := "go_library(\n    name = \"old\",\n)\n\n"
	// gazelle appends rules for newly added files after the managed block.
	after := "\ngo_test(\n    name = \"old_test\",\n    srcs = [\"old_test.go\"],\n)\n"
	root := newRepoTreeFixture(t, map[string]string{
		"internal/old/BUILD.bazel": before + legacy + "\n" + legacy + after,
	})

	runRepoTree(t, root)

	want := before + repoTreeManagedBlock + after
	if got := readRepoTreeFile(t, root, "internal/old/BUILD.bazel"); got != want {
		t.Errorf("internal/old/BUILD.bazel:\n%s\nwant:\n%s", got, want)
	}
}

func TestRepoTreeAggregatesEveryPackageAtTheRoot(t *testing.T) {
	root := newRepoTreeFixture(t, map[string]string{
		"internal/a/BUILD.bazel":       "",
		"internal/a/sub/BUILD.bazel":   "",
		"cmd/b/BUILD.bazel":            "",
		"internal/.hidden/BUILD.bazel": "",
		"schemas_embed.go":             "package gascity\n",
	})

	runRepoTree(t, root)
	got := readRepoTreeFile(t, root, "BUILD.bazel")

	if !strings.HasPrefix(got, repoTreeRootBuild) {
		t.Errorf("root BUILD.bazel lost its hand-written prefix:\n%s", got)
	}
	for name, pkgFilegroup := range map[string]string{
		"repo_source_tree":  "bazel_repo_srcs",
		"repo_go_srcs":      "bazel_go_srcs",
		"repo_go_test_srcs": "bazel_go_test_srcs",
	} {
		body := repoTreeRule(t, got, name)
		for _, pkg := range []string{"cmd", "cmd/b", "internal", "internal/a", "internal/a/sub"} {
			label := "\"//" + pkg + ":" + pkgFilegroup + "\""
			if !strings.Contains(body, label) {
				t.Errorf("%s does not aggregate %s:\n%s", name, label, body)
			}
		}
		if strings.Contains(body, ".hidden") {
			t.Errorf("%s aggregates a dot directory:\n%s", name, body)
		}
	}
	if body := repoTreeRule(t, got, "repo_go_srcs"); !strings.Contains(body, "\":schemas_embed.go\"") {
		t.Errorf("repo_go_srcs omits the root package's Go source:\n%s", body)
	}
	if body := repoTreeRule(t, got, "repo_source_tree"); !strings.Contains(body, "\":root_extras\"") {
		t.Errorf("repo_source_tree omits the root-level trees:\n%s", body)
	}
}

// repoTreeRule returns the text of the root filegroup with the given name.
func repoTreeRule(t *testing.T, build, name string) string {
	t.Helper()
	start := strings.Index(build, "    name = \""+name+"\",\n")
	if start < 0 {
		t.Fatalf("root BUILD.bazel has no %s filegroup:\n%s", name, build)
	}
	end := strings.Index(build[start:], "\n)\n")
	if end < 0 {
		t.Fatalf("unterminated %s filegroup:\n%s", name, build)
	}
	return build[start : start+end]
}

// TestRepoTreeRestoresEmbedLabelWhenTestDataNamesTheSameFilegroup pins that
// the gazelle-dropped embedsrcs label is restored even when the package's
// go_test already lists the same filegroup in data: a substring check on the
// label silently skipped the restore and broke the go:embed compile.
func TestRepoTreeRestoresEmbedLabelWhenTestDataNamesTheSameFilegroup(t *testing.T) {
	gazelleOutput := "go_library(\n" +
		"    name = \"bootstrap\",\n" +
		"    visibility = [\"//:__subpackages__\"],\n" +
		"    deps = [\"//internal/config\"],\n" +
		")\n\n" +
		"go_test(\n" +
		"    name = \"bootstrap_test\",\n" +
		"    data = [\"//internal/bootstrap/packs/core:pack_files\"],\n" +
		")\n"
	root := newRepoTreeFixture(t, map[string]string{"internal/bootstrap/BUILD.bazel": gazelleOutput})

	runRepoTree(t, root)

	got := readRepoTreeFile(t, root, "internal/bootstrap/BUILD.bazel")
	want := "    visibility = [\"//:__subpackages__\"],\n" +
		"    embedsrcs = [\"//internal/bootstrap/packs/core:pack_files\"],\n" +
		"    deps = ["
	if !strings.Contains(got, want) {
		t.Errorf("internal/bootstrap/BUILD.bazel lost the go_library embedsrcs:\n%s", got)
	}
}
