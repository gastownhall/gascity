package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// flakyTestAttemptsFlag matches every spelling of Bazel's retry flag:
// --flaky_test_attempts=N, --flaky_test_attempts N, and the
// regex@N / default forms.
var flakyTestAttemptsFlag = regexp.MustCompile(`--flaky_test_attempts(?:=|\s+)(\S+)`)

// checkNoFlakyRetries reports every --flaky_test_attempts setting in content
// that allows more than one attempt. Under remote execution each attempt is
// its own action: the failed attempt is not cached but a pass on retry is, so
// a retried flake is trusted by the action cache forever for that input hash.
// The cache is the phase-skip mechanism (pre-push, PR, main), so no
// cache-writing invocation may retry. Only an explicit single attempt is
// allowed; "default" is rejected because it grants flaky-tagged targets three
// attempts.
func checkNoFlakyRetries(name, content string) []error {
	var errs []error
	for i, line := range strings.Split(content, "\n") {
		code, _, _ := strings.Cut(line, "#")
		for _, m := range flakyTestAttemptsFlag.FindAllStringSubmatch(code, -1) {
			if strings.Trim(m[1], `"'`) != "1" {
				errs = append(errs, fmt.Errorf("%s:%d sets %s; flaky passes must not be retried into the action cache", name, i+1, strings.TrimSpace(m[0])))
			}
		}
	}
	return errs
}

func TestBazelNoFlakyTestRetries(t *testing.T) {
	root := repoRoot(t)
	files := []string{".bazelrc", "Makefile"}
	workflows, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(workflows) == 0 {
		t.Fatal("no workflows found under .github/workflows")
	}
	for _, wf := range workflows {
		rel, err := filepath.Rel(root, wf)
		if err != nil {
			t.Fatalf("rel %s: %v", wf, err)
		}
		files = append(files, rel)
	}
	for _, rel := range files {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		for _, err := range checkNoFlakyRetries(rel, readFile(t, root, rel)) {
			t.Error(err)
		}
	}
}

func TestCheckNoFlakyRetriesFixtures(t *testing.T) {
	for name, content := range map[string]string{
		"no flag":            "test --test_output=errors\n",
		"single attempt":     "bazel test //x --flaky_test_attempts=1\n",
		"commented out":      "# test --flaky_test_attempts=2\n",
		"trailing comment":   "test --test_output=errors # was --flaky_test_attempts=2\n",
		"quoted single":      `bazel test //x --flaky_test_attempts "1"` + "\n",
		"space single value": "bazel test //x --flaky_test_attempts 1\n",
	} {
		if errs := checkNoFlakyRetries(name, content); len(errs) != 0 {
			t.Errorf("%s: unexpected errors: %v", name, errs)
		}
	}
	for name, content := range map[string]string{
		"bazelrc retry":     "test --flaky_test_attempts=2\n",
		"config retry":      "test:ci --flaky_test_attempts=3\n",
		"space form":        "bazel test //x --flaky_test_attempts 2\n",
		"regex form":        "test --flaky_test_attempts=//cmd/.*@3\n",
		"default form":      "test --flaky_test_attempts=default\n",
		"workflow inline":   "          bazel test //... --keep_going --flaky_test_attempts=2 || true\n",
		"after good on row": "bazel test --flaky_test_attempts=1 //x && bazel test --flaky_test_attempts=2 //y\n",
	} {
		if len(checkNoFlakyRetries(name, content)) == 0 {
			t.Errorf("%s: expected an error for:\n%s", name, content)
		}
	}
}

// TestBazelrcPinsSingleTestAttempt requires .bazelrc to pin one attempt for
// every test invocation, not only the :ci lanes. Leaving the flag unset is
// not the same: Bazel's own default ("default") gives targets marked
// flaky = True three attempts, so a local or pre-push run would retry them
// into the action cache.
func TestBazelrcPinsSingleTestAttempt(t *testing.T) {
	root := repoRoot(t)
	for _, line := range strings.Split(readFile(t, root, ".bazelrc"), "\n") {
		code, _, _ := strings.Cut(line, "#")
		if strings.Join(strings.Fields(code), " ") == "test --flaky_test_attempts=1" {
			return
		}
	}
	t.Fatal(".bazelrc must pin `test --flaky_test_attempts=1` for every invocation: unset, Bazel gives flaky-tagged targets three attempts")
}
