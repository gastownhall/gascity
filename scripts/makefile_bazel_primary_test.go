package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The primary make targets run the bazel commands CI gates on
// (.github/workflows/bazel.yml's lanes, minus the lane-only --config=ci)
// everywhere, GitHub Actions included, and each keeps a plain-go twin under
// an explicit -go name. A workflow job that runs a Go-native suite calls the
// -go name, so the engine it uses is written in the workflow.

const fakeMakeBazel = "/fake/bazel"

func makeDryRun(t *testing.T, target string, env ...string) string {
	t.Helper()
	cmd := makeCommand("--no-print-directory", "-n",
		"-f", filepath.Join(repoRoot(t), "Makefile"),
		"BAZEL="+fakeMakeBazel,
		"SYS_USR_CGO_FALLBACK=0",
		target)
	cmd.Dir = repoRoot(t)
	for _, entry := range filteredMakefileCGOTestEnv() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "GITHUB_ACTIONS" || name == "BAZEL_FLAGS" {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n %s: %v\n%s", target, err, out)
	}
	return string(out)
}

func TestMakePrimaryTargetsRunBazel(t *testing.T) {
	for target, want := range map[string]string{
		"test":             fakeMakeBazel + " test  --keep_going //...",
		"check":            fakeMakeBazel + " test  --keep_going //...",
		"check-docs":       fakeMakeBazel + " test  --keep_going //test/docsync:docsync_test",
		"test-acceptance":  fakeMakeBazel + " test  --keep_going --config=acceptance //test/acceptance:acceptance_test",
		"test-integration": fakeMakeBazel + " test  --keep_going --config=integration //test:integration_packages //test/integration:integration_test",
	} {
		for _, env := range [][]string{nil, {"GITHUB_ACTIONS=true"}} {
			t.Run(target+strings.Join(env, ","), func(t *testing.T) {
				out := makeDryRun(t, target, env...)
				if !strings.Contains(out, want) {
					t.Errorf("make %s (env %v) does not run %q:\n%s", target, env, want, out)
				}
				if strings.Contains(out, "go test") || strings.Contains(out, "go-test-observable") {
					t.Errorf("make %s (env %v) runs go test instead of bazel:\n%s", target, env, out)
				}
			})
		}
	}
}

func TestMakeCheckKeepsShellGuardsBesideBazel(t *testing.T) {
	out := makeDryRun(t, "check")
	for _, want := range []string{
		"./scripts/check-routed-test-rows.sh",
		"./scripts/check-split-topology-rows.sh",
		"./scripts/check-residency-boundary.sh",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("make check does not run %s:\n%s", want, out)
		}
	}
}

func TestMakeGoTwinsRunGoTest(t *testing.T) {
	for target, want := range map[string]string{
		"test-go":             "scripts/go-test-observable test",
		"check-docs-go":       "go test ./test/docsync",
		"test-acceptance-go":  "go test -tags acceptance_a",
		"test-integration-go": "go test -tags integration",
	} {
		t.Run(target, func(t *testing.T) {
			out := makeDryRun(t, target)
			if !strings.Contains(out, want) {
				t.Errorf("make %s does not run %q:\n%s", target, want, out)
			}
			if strings.Contains(out, fakeMakeBazel) {
				t.Errorf("make %s runs bazel:\n%s", target, out)
			}
		})
	}
}

// primaryMakeCallRE matches a call of a bazel-backed primary make target (not
// its -go twin or any other target sharing the prefix).
var primaryMakeCallRE = regexp.MustCompile(`\bmake (test|check|check-all|check-docs|test-acceptance|test-integration)(\s|$|["'])`)

// TestWorkflowsNameTheirTestEngine: a workflow runs either bazel (directly) or
// a Go-native suite through an explicit -go make target, never a primary name
// whose engine a reader would have to look up in the Makefile.
func TestWorkflowsNameTheirTestEngine(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.y*ml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob workflows: %v (%d files)", err, len(files))
	}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
			if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "name:") {
				continue
			}
			if m := primaryMakeCallRE.FindStringSubmatch(line); m != nil {
				t.Errorf("%s:%d calls `make %s`, which runs bazel; call `make %s-go` for the Go-native suite or bazel directly",
					filepath.Base(file), i+1, m[1], m[1])
			}
		}
	}
}

func nativeDepContractMakeRecipe(t *testing.T) []string {
	t.Helper()
	_, rest, ok := strings.Cut(nativeDepContractRead(t, "Makefile"), "\ncheck-native-dependency-surface:")
	if !ok {
		t.Fatal("missing native dependency Make target")
	}
	lines := strings.Split(rest, "\n")
	var recipe []string
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, "\t") {
			break
		}
		recipe = append(recipe, strings.TrimPrefix(line, "\t"))
	}
	if len(recipe) == 0 {
		t.Fatal("native dependency Make target has no recipe")
	}
	return recipe
}

func TestNativeDepContractMakeDelegatesToCILabel(t *testing.T) {
	label := nativeDepContractCILabel(t)
	for _, flags := range []string{"", "--config=fork-cache"} {
		t.Run("flags_"+flags, func(t *testing.T) {
			out := makeDryRun(t, "check-native-dependency-surface", "BAZEL_FLAGS="+flags)
			want := "mkdir -p /tmp/bt\n" + fakeMakeBazel + " test " + flags + " --keep_going --test_output=all " + label + "\n"
			if out != want {
				t.Errorf("local guard must run the CI target with identical policy\ngot:\n%swant:\n%s", out, want)
			}
			for _, forbidden := range []string{"go build", "CGO_ENABLED", "check-native-dependency-surface.sh", "GC_NATIVE_DEP", "--test_env", "--action_env"} {
				if strings.Contains(out, forbidden) {
					t.Errorf("local guard introduces independent build/policy %q:\n%s", forbidden, out)
				}
			}
		})
	}
}

func TestNativeDepContractMakeRecipeDoesNotRunScript(t *testing.T) {
	for _, line := range nativeDepContractMakeRecipe(t) {
		if strings.Contains(line, "check-native-dependency-surface.sh") {
			t.Errorf("Make guard runs the script instead of the CI target: %s", line)
		}
	}
}

func TestNativeDepContractMakeRecipeDoesNotMaskFailure(t *testing.T) {
	for _, line := range nativeDepContractMakeRecipe(t) {
		trimmed := strings.TrimLeft(line, "@ ")
		if strings.HasPrefix(trimmed, "-") || regexp.MustCompile(`\|\|\s*(true|:)(\s|$)`).MatchString(line) {
			t.Errorf("guard failure is masked by recipe %q", line)
		}
	}
}

func TestNativeDepContractMakeStaysOutOfCheck(t *testing.T) {
	out := makeDryRun(t, "check")
	for _, forbidden := range []string{nativeDepContractCILabel(t), "check-native-dependency-surface.sh"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("check duplicates the native guard through %q:\n%s", forbidden, out)
		}
	}
	if strings.Count(out, fakeMakeBazel+" test ") != 1 || !strings.Contains(out, fakeMakeBazel+" test  --keep_going //...") {
		t.Errorf("check must retain its one existing Bazel invocation:\n%s", out)
	}
}

func TestNativeDepContractMakeHasNoGoTwin(t *testing.T) {
	makefile := nativeDepContractRead(t, "Makefile")
	if regexp.MustCompile(`(?m)^check-native-dependency-surface-go\s*:`).MatchString(makefile) {
		t.Error("host Go twin would measure a different artifact")
	}
	source := nativeDepContractRead(t, "scripts/makefile_bazel_primary_test.go")
	_, primary, ok := strings.Cut(source, "func TestMakePrimaryTargetsRunBazel(")
	if !ok {
		t.Fatal("missing primary-target map test")
	}
	primary, _, _ = strings.Cut(primary, "\nfunc ")
	if strings.Contains(primary, "check-native-dependency-surface") {
		t.Error("native guard has no Go twin and cannot join the primary-target map")
	}
}

func TestNativeDepContractMakePropagatesBazelFailure(t *testing.T) {
	bin := t.TempDir()
	stub := filepath.Join(bin, "bazel")
	writeExecutable(t, stub, "#!/bin/sh\necho 'fixture Bazel build failed' >&2\nexit 23\n")
	// The legacy recipe must fail without ever building the real gc.
	writeExecutable(t, filepath.Join(bin, "go"), "#!/bin/sh\necho 'unexpected host go invocation' >&2\nexit 1\n")
	cmd := makeCommand("--no-print-directory", "-f", filepath.Join(repoRoot(t), "Makefile"),
		"BAZEL="+stub, "BAZEL_FLAGS=", "SYS_USR_CGO_FALLBACK=0", "check-native-dependency-surface")
	cmd.Dir = repoRoot(t)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "SHELL=/bin/sh"}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "fixture Bazel build failed") {
		t.Fatalf("make must propagate its Bazel build failure: %v\n%s", err, out)
	}
}
