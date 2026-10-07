package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const hermeticTagsLedger = `[[target]]
package = "leaky"
tags = ["external", "requires-network"]
reason = "fixture"
`

// newHermeticTagsTree copies tools/bazel/hermetic_tags.py into a scratch
// repository holding the fixture ledger and the given BUILD.bazel files.
func newHermeticTagsTree(t *testing.T, builds map[string]string) string {
	t.Helper()
	root := t.TempDir()
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "tools", "bazel", "hermetic_tags.py"))
	if err != nil {
		t.Fatalf("read hermetic_tags.py: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "tools", "bazel", "hermetic_tags.py"), string(script))
	writeTestFile(t, filepath.Join(root, "test", "bazel-hermeticity.toml"), hermeticTagsLedger)
	for pkg, content := range builds {
		writeTestFile(t, filepath.Join(root, pkg, "BUILD.bazel"), content)
	}
	return root
}

func runHermeticTags(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	cmd := testCommand("python3", append([]string{filepath.Join(root, "tools", "bazel", "hermetic_tags.py")}, args...)...)
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func readBuild(t *testing.T, root, pkg string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, pkg, "BUILD.bazel"))
	if err != nil {
		t.Fatalf("read %s/BUILD.bazel: %v", pkg, err)
	}
	return string(data)
}

func TestHermeticTagsSyncsLedgerTagsIntoGoTests(t *testing.T) {
	builds := map[string]string{
		"leaky": "go_test(\n    name = \"a_test\",\n    srcs = [\"a_test.go\"],\n)\n\n" +
			"go_test(\n    name = \"b_test\",\n    srcs = [\"b_test.go\"],\n    tags = [\"manual\"],\n)\n",
		"tidy": "go_test(\n    name = \"tidy_test\",\n    tags = [\n        \"external\",\n        \"manual\",\n    ],\n)\n",
	}
	root := newHermeticTagsTree(t, builds)

	if output, err := runHermeticTags(t, root, "--check"); err == nil {
		t.Fatalf("--check passed on drifted BUILD files:\n%s", output)
	}
	if output, err := runHermeticTags(t, root); err != nil {
		t.Fatalf("hermetic_tags.py: %v\n%s", err, output)
	}
	wantLeaky := "go_test(\n    name = \"a_test\",\n    srcs = [\"a_test.go\"],\n    tags = [\n        \"external\",\n        \"requires-network\",\n    ],\n)\n\n" +
		"go_test(\n    name = \"b_test\",\n    srcs = [\"b_test.go\"],\n    tags = [\n        \"external\",\n        \"manual\",\n        \"requires-network\",\n    ],\n)\n"
	if got := readBuild(t, root, "leaky"); got != wantLeaky {
		t.Errorf("leaky/BUILD.bazel:\n%s\nwant:\n%s", got, wantLeaky)
	}
	wantTidy := "go_test(\n    name = \"tidy_test\",\n    tags = [\"manual\"],\n)\n"
	if got := readBuild(t, root, "tidy"); got != wantTidy {
		t.Errorf("tidy/BUILD.bazel:\n%s\nwant:\n%s", got, wantTidy)
	}
	if output, err := runHermeticTags(t, root, "--check"); err != nil {
		t.Fatalf("--check after sync: %v\n%s", err, output)
	}
}

func TestHermeticTagsRejectsTagsItCannotRewrite(t *testing.T) {
	for name, tags := range map[string]string{
		"trailing keep comment": "    tags = [\"external\"],  # keep\n",
		"list expression":       "    tags = [\"manual\"] + select({\"//conditions:default\": []}),\n",
		"comment in list":       "    tags = [\n        \"external\",  # why\n        \"manual\",\n    ],\n",
	} {
		t.Run(name, func(t *testing.T) {
			for _, pkg := range []string{"leaky", "tidy"} {
				build := "go_test(\n    name = \"x_test\",\n" + tags + ")\n"
				builds := map[string]string{"leaky": "go_test(\n    name = \"l_test\",\n    tags = [\n        \"external\",\n        \"requires-network\",\n    ],\n)\n"}
				builds[pkg] = build
				root := newHermeticTagsTree(t, builds)
				for _, args := range [][]string{{"--check"}, nil} {
					output, err := runHermeticTags(t, root, args...)
					if err == nil {
						t.Fatalf("%s %v: accepted an unsupported tags attribute:\n%s", pkg, args, output)
					}
					if !strings.Contains(output, filepath.Join(pkg, "BUILD.bazel")) || !strings.Contains(output, "x_test") {
						t.Errorf("%s %v: error does not name the file and rule:\n%s", pkg, args, output)
					}
					if got := readBuild(t, root, pkg); got != build {
						t.Errorf("%s %v: BUILD.bazel rewritten despite the error:\n%s", pkg, args, got)
					}
				}
			}
		})
	}
}
