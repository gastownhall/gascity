package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const darwinCheckBuild = "tools/darwincheck/BUILD.bazel"

// fakeBazelDarwin stands in for the bazel binary tools/bazel/darwin_check.py
// shells out to. It answers `bazel query --output=label_kind <expr>` from
// targets.tsv (kind, label, visible-to-//tools/darwincheck as 1 or 0) and
// edges.tsv (a direct dep edge per line), and it applies the kind("<re>", ...)
// filter the generator passes, as bazel does, so the test pins the filter the
// generator asks for and not only its parsing.
const fakeBazelDarwin = `#!/usr/bin/env python3
import os, re, sys

here = os.path.dirname(os.path.abspath(__file__))
expr = sys.argv[-1]
if sys.argv[1] != "query":
    sys.exit("fake bazel: only query is supported: %r" % sys.argv[1:])

targets = []
for line in open(os.path.join(here, "targets.tsv")).read().splitlines():
    if line.strip():
        kind, label, visible = line.split("\t")
        targets.append((kind, label, visible == "1"))
edges = {}
for line in open(os.path.join(here, "edges.tsv")).read().splitlines():
    if line.strip():
        src, dst = line.split("\t")
        edges.setdefault(src, set()).add(dst)

m = re.search(r'kind\("([^"]+)"', expr)
pattern = m.group(1) if m else None
picked = [t for t in targets if pattern is None or re.search(pattern, t[0] + " rule")]

def labels_in(text):
    return text.split()

if expr.startswith("visible("):
    picked = [t for t in picked if t[2]]
elif expr.startswith("deps("):
    m = re.match(r"deps\(set\(([^)]*)\)\) intersect set\(([^)]*)\)$", expr)
    if not m:
        sys.exit("fake bazel: unexpected deps query: %r" % expr)
    seen, todo = set(), labels_in(m.group(1))
    while todo:
        cur = todo.pop()
        if cur in seen:
            continue
        seen.add(cur)
        todo.extend(edges.get(cur, ()))
    want = set(labels_in(m.group(2)))
    picked = [t for t in targets if t[1] in seen and t[1] in want]
for kind, label, _ in sorted(picked, key=lambda t: t[1]):
    print("%s rule %s" % (kind, label))
`

func newDarwinCheckTree(t *testing.T, targets, edges []string) string {
	t.Helper()
	root := t.TempDir()
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "tools", "bazel", "darwin_check.py"))
	if err != nil {
		t.Fatalf("read darwin_check.py: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "tools", "bazel", "darwin_check.py"), string(script))
	writeTestFile(t, filepath.Join(root, "fakebin", "bazel"), fakeBazelDarwin)
	if err := os.Chmod(filepath.Join(root, "fakebin", "bazel"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDarwinFixtures(t, root, targets, edges)
	return root
}

func writeDarwinFixtures(t *testing.T, root string, targets, edges []string) {
	t.Helper()
	writeTestFile(t, filepath.Join(root, "fakebin", "targets.tsv"), strings.Join(targets, "\n")+"\n")
	writeTestFile(t, filepath.Join(root, "fakebin", "edges.tsv"), strings.Join(edges, "\n")+"\n")
}

func runDarwinCheck(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	args = append([]string{
		filepath.Join(root, "tools", "bazel", "darwin_check.py"),
		"--bazel", filepath.Join(root, "fakebin", "bazel"),
	}, args...)
	cmd := testCommand("python3", args...)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// depsOf returns the labels inside the generated target's deps = [ ... ].
func depsOf(t *testing.T, build string) []string {
	t.Helper()
	_, after, ok := strings.Cut(build, "    deps = [\n")
	if !ok {
		t.Fatalf("generated file has no deps list:\n%s", build)
	}
	list, _, ok := strings.Cut(after, "    ],\n")
	if !ok {
		t.Fatalf("generated deps list is not closed:\n%s", build)
	}
	var deps []string
	for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
		deps = append(deps, strings.Trim(strings.TrimSpace(line), "\","))
	}
	return deps
}

func darwinTargets() []string {
	row := func(kind, label string, visible int) string {
		v := "0"
		if visible == 1 {
			v = "1"
		}
		return kind + "\t" + label + "\t" + v
	}
	return []string{
		row("go_library", "//pkg/zeta:zeta", 1),
		row("go_test", "//pkg/zeta:zeta_test", 1),
		row("go_library", "//pkg/alpha:alpha", 1),
		row("go_binary", "//cmd/tool:tool", 1),
		// Restricted: reached through the binary that embeds it.
		row("go_library", "//cmd/tool:tool_lib", 0),
		row("go_binary", "//cmd/gc:gc", 1),
		// Restricted: the cmd/gc test variants are covered by //cmd/gc:gc.
		row("go_test", "//cmd/gc:gc_testhooks", 0),
		// Restricted: a nogo internal reached only through a listed test.
		row("go_library", "//tools/nogo/internal/position:position", 0),
		// Not Go compile targets: the kind filter must leave every one out.
		row("go_test_run", "//pkg/alpha:alpha_run", 1),
		row("go_variant_test", "//pkg/alpha:alpha_variant", 1),
		row("filegroup", "//pkg/alpha:files", 1),
		row("gc_sim_corpus", "//cmd/gc:sim_corpus", 1),
	}
}

func darwinEdges() []string {
	return []string{
		"//cmd/tool:tool\t//cmd/tool:tool_lib",
		"//pkg/zeta:zeta_test\t//tools/nogo/internal/position:position",
	}
}

// TestDarwinCheckGeneratesTheDepListAndTracksDrift pins what
// tools/bazel/darwin_check.py writes into //tools/darwincheck: every visible
// go_library, go_binary and go_test, sorted, and nothing of another kind;
// restricted-visibility targets left out and listed in a comment when a listed
// target covers them; and --check reporting drift without writing.
func TestDarwinCheckGeneratesTheDepListAndTracksDrift(t *testing.T) {
	root := newDarwinCheckTree(t, darwinTargets(), darwinEdges())

	output, err := runDarwinCheck(t, root, "--check")
	if err == nil {
		t.Fatalf("--check passed with no generated file:\n%s", output)
	}
	if !strings.Contains(output, darwinCheckBuild) {
		t.Errorf("--check does not name %s:\n%s", darwinCheckBuild, output)
	}
	if _, statErr := os.Stat(filepath.Join(root, darwinCheckBuild)); statErr == nil {
		t.Fatalf("--check wrote %s", darwinCheckBuild)
	}

	if output, err := runDarwinCheck(t, root); err != nil {
		t.Fatalf("darwin_check.py: %v\n%s", err, output)
	}
	got := readBuild(t, root, "tools/darwincheck")

	if !strings.HasPrefix(got, "load(\"//tools/bazel/rules:go_darwin_check.bzl\", \"go_darwin_check_test\")\n") {
		t.Errorf("file must start with the rule's load:\n%s", got)
	}
	if !strings.Contains(got, "go_darwin_check_test(\n    name = \"darwin_check_test\",\n    size = \"small\",\n") {
		t.Errorf("file lacks the darwin_check_test target (size small):\n%s", got)
	}
	want := []string{
		"//cmd/gc:gc",
		"//cmd/tool:tool",
		"//pkg/alpha:alpha",
		"//pkg/zeta:zeta",
		"//pkg/zeta:zeta_test",
	}
	if deps := depsOf(t, got); strings.Join(deps, ",") != strings.Join(want, ",") {
		t.Errorf("deps = %v, want %v (every visible go_library, go_binary and go_test, sorted; no other kind)", deps, want)
	}
	// What was skipped is named in a comment, so the file says what it leaves out.
	head, _, _ := strings.Cut(got, "go_darwin_check_test(\n")
	for _, label := range []string{
		"//cmd/tool:tool_lib",
		"//cmd/gc:gc_testhooks",
		"//tools/nogo/internal/position:position",
	} {
		if !strings.Contains(head, "#   "+label+"\n") {
			t.Errorf("skipped %s is not listed in the comment before the target:\n%s", label, head)
		}
	}

	if output, err := runDarwinCheck(t, root, "--check"); err != nil {
		t.Fatalf("--check after generating: %v\n%s", err, output)
	}
	if output, err := runDarwinCheck(t, root); err != nil {
		t.Fatalf("second run: %v\n%s", err, output)
	}
	if again := readBuild(t, root, "tools/darwincheck"); again != got {
		t.Errorf("second run changed the file:\n%s\nwant:\n%s", again, got)
	}

	// A new Go package without regenerating is drift; regenerating picks it up.
	writeDarwinFixtures(t, root,
		append(darwinTargets(), "go_library\t//pkg/newpkg:newpkg\t1"), darwinEdges())
	output, err = runDarwinCheck(t, root, "--check")
	if err == nil {
		t.Fatalf("--check passed with a new package missing from the file:\n%s", output)
	}
	if !strings.Contains(output, darwinCheckBuild) {
		t.Errorf("--check does not name %s:\n%s", darwinCheckBuild, output)
	}
	if stale := readBuild(t, root, "tools/darwincheck"); stale != got {
		t.Errorf("--check rewrote the file")
	}
	if output, err := runDarwinCheck(t, root); err != nil {
		t.Fatalf("darwin_check.py: %v\n%s", err, output)
	}
	if deps := depsOf(t, readBuild(t, root, "tools/darwincheck")); !strings.Contains(strings.Join(deps, ","), "//pkg/newpkg:newpkg") {
		t.Errorf("regenerated deps lack //pkg/newpkg:newpkg: %v", deps)
	}
}

// TestDarwinCheckRefusesARestrictedTargetNothingCovers pins the other half of
// the skip rule: a restricted-visibility Go target is left out only when a
// listed target reaches it (or the known cmd/gc variants' covering binary is
// listed); otherwise the generator exits 1 naming it and writes nothing, so a
// target never drops out of the darwin check silently.
func TestDarwinCheckRefusesARestrictedTargetNothingCovers(t *testing.T) {
	t.Run("unreached", func(t *testing.T) {
		root := newDarwinCheckTree(t,
			append(darwinTargets(), "go_library\t//internal/secret:secret\t0"), darwinEdges())
		output, err := runDarwinCheck(t, root)
		if err == nil {
			t.Fatalf("generator passed with an uncovered restricted target:\n%s", output)
		}
		if !strings.Contains(output, "//internal/secret:secret") {
			t.Errorf("failure does not name the uncovered target:\n%s", output)
		}
		if strings.Contains(output, "//cmd/tool:tool_lib") {
			t.Errorf("failure also names a covered target:\n%s", output)
		}
		if _, statErr := os.Stat(filepath.Join(root, darwinCheckBuild)); statErr == nil {
			t.Errorf("generator wrote %s despite the failure", darwinCheckBuild)
		}
		if output, err := runDarwinCheck(t, root, "--check"); err == nil {
			t.Errorf("--check passed with an uncovered restricted target:\n%s", output)
		}
	})

	t.Run("cmd/gc variant without its binary", func(t *testing.T) {
		var targets []string
		for _, row := range darwinTargets() {
			if !strings.Contains(row, "\t//cmd/gc:gc\t") {
				targets = append(targets, row)
			}
		}
		root := newDarwinCheckTree(t, targets, darwinEdges())
		output, err := runDarwinCheck(t, root)
		if err == nil {
			t.Fatalf("generator passed with //cmd/gc:gc_testhooks and no //cmd/gc:gc to cover it:\n%s", output)
		}
		if !strings.Contains(output, "//cmd/gc:gc_testhooks") {
			t.Errorf("failure does not name //cmd/gc:gc_testhooks:\n%s", output)
		}
	})
}
