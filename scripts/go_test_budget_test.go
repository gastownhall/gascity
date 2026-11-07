package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// sweepTargets are the unsharded `go test ./...` sweeps the Makefile runs
// through scripts/go-test-observable: the Linux unit sweep behind
// `make check-go` and its Mac twin.
var sweepTargets = []struct {
	target         string
	observableName string
}{
	{target: "test-go", observableName: "test"},
	{target: "test-mac", observableName: "test-mac"},
}

// sweepTimeoutFlag dry-runs a sweep target with GO_TEST_TIMEOUT set to
// override and returns the -timeout its go-test-observable line carries.
func sweepTimeoutFlag(t *testing.T, target, observableName, override string) string {
	t.Helper()
	cmd := makeCommand("-n", target)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "GO_TEST_TIMEOUT="+override)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n %s failed: %v\n%s", target, err, out)
	}
	marker := "scripts/go-test-observable " + observableName + " -- "
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, marker) {
			return goTestFlagValue(t, line, "timeout")
		}
	}
	t.Fatalf("make -n %s runs no %q line:\n%s", target, marker, out)
	return ""
}

// runnerTimeoutDefault returns the per-package budget scripts/test-local-parallel
// applies when GO_TEST_TIMEOUT is unset.
func runnerTimeoutDefault(t *testing.T) string {
	t.Helper()
	want := resolveShellVars(t, localParallelScript(t), "${go_test_timeout}")
	if want == "" || strings.Contains(want, "$") {
		t.Fatalf("scripts/test-local-parallel resolves no literal go_test_timeout default: %q", want)
	}
	return want
}

// TestSweepTargetsShareTheFanOutTimeoutBudget: `make test-go`
// and `make test-mac` sweep every package in one `go test`, the same shape as
// test-fast-parallel's unit-core job, but carried their own 15m while the
// fan-out moved to 20m (ga-9au). cmd/gc crossed 15m on a loaded host with
// every test green, and a CI gate could only report a red with no
// failing test to name. The sweeps must resolve the budget the runner applies.
func TestSweepTargetsShareTheFanOutTimeoutBudget(t *testing.T) {
	want := runnerTimeoutDefault(t)
	wantDuration, err := time.ParseDuration(want)
	if err != nil {
		t.Fatalf("runner default %q is not a Go duration: %v", want, err)
	}
	if wantDuration <= goDefaultTestTimeout {
		t.Fatalf("runner default %s is not above the %s go default it exists to replace", wantDuration, goDefaultTestTimeout)
	}
	for _, sweep := range sweepTargets {
		got := sweepTimeoutFlag(t, sweep.target, sweep.observableName, "")
		if got != want {
			t.Fatalf("make %s passes -timeout %q, but scripts/test-local-parallel budgets %q per package; the sweeps must share the fan-out's number",
				sweep.target, got, want)
		}
	}
}

// TestSweepTargetsForwardTimeoutOverride proves `GO_TEST_TIMEOUT=30m make
// test-go` reaches go test. TEST_ENV's env -i would drop the variable, so the
// Makefile resolves the budget before the scrub instead of forwarding it.
func TestSweepTargetsForwardTimeoutOverride(t *testing.T) {
	const override = "37m"
	for _, sweep := range sweepTargets {
		if got := sweepTimeoutFlag(t, sweep.target, sweep.observableName, override); got != override {
			t.Fatalf("make %s with GO_TEST_TIMEOUT=%s passes -timeout %q", sweep.target, override, got)
		}
	}
}

// TestMakefileLeavesSweepTimeoutDefaultToTheRunner keeps the number in one
// place: no sweep recipe (a go-test-observable line over ./... or the Mac
// package list) may spell its own -timeout literal again. Opt-in targets that
// run one package, such as test-herdr-live, keep their own budgets.
func TestMakefileLeavesSweepTimeoutDefaultToTheRunner(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	literal := regexp.MustCompile(`scripts/go-test-observable (test|test-mac) -- [^\n]*-timeout [0-9][^\n]*(\./\.\.\.|\$\(MAC_UNIT_PKGS\))`)
	if match := literal.Find(data); match != nil {
		t.Fatalf("a sweep recipe hardcodes its -timeout; use $(GO_TEST_BUDGET):\n%s", match)
	}
}

// runGoTestBudget runs a go-test-budget script with GO_TEST_TIMEOUT set to
// override and returns its trimmed combined output.
func runGoTestBudget(t *testing.T, script, override string) (string, error) {
	t.Helper()
	cmd := exec.Command(script)
	cmd.Dir = filepath.Dir(script)
	cmd.Env = append(os.Environ(), "GO_TEST_TIMEOUT="+override)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestGoTestBudgetHelper pins scripts/go-test-budget itself: the default is
// the runner's, an override wins verbatim, and a malformed override is
// refused rather than passed on to go test.
func TestGoTestBudgetHelper(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "go-test-budget")
	want := runnerTimeoutDefault(t)

	if got, err := runGoTestBudget(t, script, ""); err != nil || got != want {
		t.Fatalf("go-test-budget default = %q, %v; want the runner's %q", got, err, want)
	}
	if got, err := runGoTestBudget(t, script, "37m"); err != nil || got != "37m" {
		t.Fatalf("go-test-budget override = %q, %v; want 37m", got, err)
	}
	if got, err := runGoTestBudget(t, script, "soon"); err == nil {
		t.Fatalf("go-test-budget accepted GO_TEST_TIMEOUT=soon: %q", got)
	}
}

// TestGoTestBudgetFailsClosedWithoutRunnerDefault: when the runner no longer
// declares the assignment the helper reads, the helper exits non-zero and
// names it, which the Makefile turns into a stopped target rather than an
// empty -timeout.
func TestGoTestBudgetFailsClosedWithoutRunnerDefault(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	helper, err := os.ReadFile(filepath.Join(root, "scripts", "go-test-budget"))
	if err != nil {
		t.Fatalf("read scripts/go-test-budget: %v", err)
	}
	script := filepath.Join(dir, "go-test-budget")
	if err := os.WriteFile(script, helper, 0o755); err != nil {
		t.Fatalf("copy helper: %v", err)
	}
	runner := "#!/usr/bin/env bash\ngo_test_timeout=20m\n"
	if err := os.WriteFile(filepath.Join(dir, "test-local-parallel"), []byte(runner), 0o755); err != nil {
		t.Fatalf("write runner stub: %v", err)
	}

	got, err := runGoTestBudget(t, script, "")
	if err == nil {
		t.Fatalf("go-test-budget printed %q with no runner default to read", got)
	}
	if !strings.Contains(got, "go_test_timeout") {
		t.Fatalf("go-test-budget failure does not name the missing runner assignment:\n%s", got)
	}
}
