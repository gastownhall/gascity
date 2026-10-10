package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// worker-env tiers (rbe-reexecution-design.md (c)). rbe-west's pool workers
// also advertise worker-env-base: the pinned manifest without its go and dolt
// lines. An action that sends it without worker-env (beads) runs with the
// host's go and dolt hidden by the action launcher, which reads both
// properties from NativeLink as RBE_X_WORKER_ENV and RBE_X_WORKER_ENV_BASE.
// An action's own environment wins over those, so only a deliberate RBE_X_*
// in a Bazel file could steer the launcher; none may exist (invariant I-f).
// gascity's tests run the host go and dolt (repository lint, drift binaries,
// managed Dolt), so its platform keeps the full worker-env until they are
// hermetic (the design's F8).

// TestRBEXControlValuesNeverInBazelFiles: no .bazelrc, BUILD, .bzl or
// MODULE.bazel names an RBE_X_ variable. Under Bazel the walk sees the
// files scripts_test declares (.bazelrc, where --test_env and --action_env
// live, MODULE.bazel and a few BUILD files); `go test` sees them all.
func TestRBEXControlValuesNeverInBazelFiles(t *testing.T) {
	root := repoRoot(t)
	bazelFile := regexp.MustCompile(`(^|/)(\.bazelrc[^/]*|BUILD|BUILD\.bazel|[^/]+\.bzl|MODULE\.bazel)$`)
	checked := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") && name != ".github" || strings.HasPrefix(name, "bazel-") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !bazelFile.MatchString(filepath.ToSlash(rel)) {
			return nil
		}
		checked[filepath.ToSlash(rel)] = true
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "RBE_X_") {
			t.Errorf("%s names an RBE_X_ variable: those are the rbe-west action launcher's control values (worker-env tiers, network, timeouts) and an action must never set them", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".bazelrc", "MODULE.bazel", "platforms/BUILD.bazel"} {
		if !checked[want] {
			t.Errorf("%s was not checked (%d files were); the walk is broken", want, len(checked))
		}
	}
}

// TestRBEWorkerPlatformKeepsTheFullWorkerEnv: //platforms:rbe_worker sends
// worker-env (the pin), so gascity's actions keep the host's go and dolt; a
// worker-env-base beside it changes nothing, alone it would hide them.
func TestRBEWorkerPlatformKeepsTheFullWorkerEnv(t *testing.T) {
	build := readFile(t, repoRoot(t), "platforms/BUILD.bazel")
	if !regexp.MustCompile(`(?m)^\s*"worker-env": "sha256:[0-9a-f]{64}",$`).MatchString(build) {
		t.Errorf("platforms/BUILD.bazel: //platforms:rbe_worker must send the full worker-env pin while gascity's tests run the host go and dolt")
	}
}
