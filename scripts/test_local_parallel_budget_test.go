package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// goDefaultTestTimeout is the per-package budget `go test` applies when the
// caller passes no -timeout. Every test entrypoint in this repo overrides it:
// the aggregate runtime of a slow package under a parallel fan-out routinely
// exceeds it, so inheriting the default turns load into a "test timed out
// after 10m0s" panic that reads like a hang but is only contention (ga-9au).
const goDefaultTestTimeout = 10 * time.Minute

// TestLocalParallelUnitCoreNeverInheritsGoDefaultTimeout guards ga-9au: the
// unit-core job sweeps every non-cmd/gc package in one `go test` invocation
// while LOCAL_TEST_JOBS-1 sibling shards compete for the same box. It must
// carry an explicit per-package budget rather than falling back to Go's 10m
// default, which fired on six packages (examples/bd/dolt, examples/gastown,
// internal/api, internal/docgen, internal/mail/exec, scripts) and made
// `make test-fast-parallel` unusable as the merge baseline.
func TestLocalParallelUnitCoreNeverInheritsGoDefaultTimeout(t *testing.T) {
	command := unitCoreJobCommand(t)

	raw := goTestFlagValue(t, command, "timeout")
	if raw == "" {
		t.Fatalf("unit-core job passes no -timeout to go test, so it inherits the %s default:\n%s",
			goDefaultTestTimeout, command)
	}
	got, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("unit-core -timeout %q is not a Go duration: %v", raw, err)
	}
	if got <= goDefaultTestTimeout {
		t.Fatalf("unit-core -timeout = %s, want more than the %s go default it exists to replace",
			got, goDefaultTestTimeout)
	}
}

// TestLocalParallelSharesOneTimeoutBudgetAcrossJobs pins the unit sweep and
// the cmd/gc shards to a single budget. They run concurrently in the same
// fan-out and contend for the same CPU, memory, and I/O, so a package that is
// slow enough to fail should fail for the same reason in either job. Two
// independently maintained numbers drift, and the drift is what produced
// ga-9au: the shards were tuned to 20m while unit-core silently kept Go's 10m.
func TestLocalParallelSharesOneTimeoutBudgetAcrossJobs(t *testing.T) {
	script := localParallelScript(t)

	unitCore := goTestFlagValue(t, unitCoreJobCommand(t), "timeout")
	shard := resolveShellVars(t, script, shardTimeoutAssignment(t, script))
	if unitCore != shard {
		t.Fatalf("unit-core -timeout = %q but cmd/gc shards use GO_TEST_TIMEOUT=%q; the fan-out must share one budget",
			unitCore, shard)
	}
}

// TestLocalParallelUnitCoreDisablesTestResultCaching mirrors the Makefile's
// `test` target: without -count=1 a job that has already passed reports a
// cached result and stalls while Go hashes cache inputs over the working tree,
// which is neither the fast feedback the runner promises nor a real run of the
// code under review.
func TestLocalParallelUnitCoreDisablesTestResultCaching(t *testing.T) {
	command := unitCoreJobCommand(t)
	if got := goTestFlagValue(t, command, "count"); got != "1" {
		t.Fatalf("unit-core -count = %q, want \"1\":\n%s", got, command)
	}
}

// TestFastParallelForwardsTimeoutOverridePastEnvScrub proves the documented
// `GO_TEST_TIMEOUT=30m make test-fast-parallel` escape hatch actually reaches
// the runner. The target wraps the script in TEST_ENV's `env -i`, which drops
// every variable not named explicitly, so an override that is not forwarded is
// silently ignored — the operator raises the budget on a slow host, watches
// the same 10m-class timeouts, and has no way to tell the knob did nothing.
func TestFastParallelForwardsTimeoutOverridePastEnvScrub(t *testing.T) {
	const override = "37m"
	cmd := makeCommand("-n", "test-fast-parallel")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "GO_TEST_TIMEOUT="+override)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n test-fast-parallel failed: %v\n%s", err, out)
	}
	recipe := string(out)
	if !strings.Contains(recipe, "GO_TEST_TIMEOUT="+override+" ") {
		t.Fatalf("test-fast-parallel drops GO_TEST_TIMEOUT=%s before invoking the runner:\n%s", override, recipe)
	}
}

// TestFastParallelLeavesTimeoutDefaultToTheRunner keeps the default in exactly
// one place. A number repeated in the Makefile would be free to drift from the
// script's, which is the failure this whole budget contract exists to prevent.
func TestFastParallelLeavesTimeoutDefaultToTheRunner(t *testing.T) {
	cmd := makeCommand("-n", "test-fast-parallel")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "GO_TEST_TIMEOUT=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n test-fast-parallel failed: %v\n%s", err, out)
	}
	recipe := regexp.MustCompile(`GO_TEST_TIMEOUT=(\S*)`).FindStringSubmatch(string(out))
	if recipe == nil {
		t.Fatalf("test-fast-parallel no longer forwards GO_TEST_TIMEOUT at all:\n%s", out)
	}
	if recipe[1] != "" {
		t.Fatalf("Makefile hardcodes GO_TEST_TIMEOUT=%q; the default belongs to scripts/test-local-parallel alone", recipe[1])
	}
}

// TestLocalParallelWithholdsTimeoutFromIntegrationJobs pins the one-way half of
// the budget contract. scripts/test-integration-shard reads the same
// GO_TEST_TIMEOUT with its own 30m default, so forwarding the fan-out's 20m to
// the integration jobs would silently cut that budget by a third — a change
// that looks like tidying up an inconsistency and reads as flakiness later.
// The runner documents this in a comment; this test is what defends it.
func TestLocalParallelWithholdsTimeoutFromIntegrationJobs(t *testing.T) {
	script := localParallelScript(t)

	if body := shellFunctionBody(t, script, "add_integration_jobs"); strings.Contains(body, "GO_TEST_TIMEOUT") {
		t.Fatalf("integration jobspecs must not carry GO_TEST_TIMEOUT; scripts/test-integration-shard owns its own 30m default:\n%s", body)
	}

	match := perJobEnvAllowlist.FindStringSubmatch(script)
	if match == nil {
		t.Fatal("scripts/test-local-parallel no longer wraps each job in an `env -i` allowlist")
	}
	if strings.Contains(match[1], "GO_TEST_TIMEOUT") {
		t.Fatalf("the per-job `env -i` allowlist names GO_TEST_TIMEOUT, so the fan-out's budget would reach the integration shards and cut 30m to 20m:\n%s", match[1])
	}
}

// perJobEnvAllowlist captures the variable allowlist the worker shell passes
// through `env -i` before invoking each jobspec.
var perJobEnvAllowlist = regexp.MustCompile(`(?s)env -i \\(.*?)bash -lc`)

// unitCoreJobCommand returns the `go test` command line the unit-core jobspec
// hands to each worker shell, with the script's own shell variables resolved.
func unitCoreJobCommand(t *testing.T) string {
	t.Helper()
	script := localParallelScript(t)
	body := shellFunctionBody(t, script, "add_unit_core_job")
	if !strings.Contains(body, `add_job "unit-core"`) {
		t.Fatalf("add_unit_core_job no longer registers a \"unit-core\" jobspec:\n%s", body)
	}
	return resolveShellVars(t, script, body)
}

// shardTimeoutAssignment returns the GO_TEST_TIMEOUT assignment the cmd/gc
// shard jobspecs prepend to their command line.
func shardTimeoutAssignment(t *testing.T, script string) string {
	t.Helper()
	body := shellFunctionBody(t, script, "add_cmd_gc_shards")
	match := regexp.MustCompile(`GO_TEST_TIMEOUT=([^\s"]+)`).FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("add_cmd_gc_shards no longer sets GO_TEST_TIMEOUT:\n%s", body)
	}
	return match[1]
}

// shellFunctionBody extracts the body of a top-level shell function defined as
// `name() {` and closed by a `}` in column zero.
func shellFunctionBody(t *testing.T, script, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\) \{\n(.*?)^\}`)
	match := pattern.FindStringSubmatch(script)
	if match == nil {
		t.Fatalf("scripts/test-local-parallel defines no %s() function", name)
	}
	return match[1]
}

// shellLiteralAssignments matches a top-level `name="value"` assignment whose
// value is a plain literal.
var shellLiteralAssignments = regexp.MustCompile(`(?m)^(?:readonly )?([a-z_][a-z0-9_]*)="([^"$]*)"$`)

// shellDefaultedAssignments matches a top-level `name="${ENV:-value}"`
// assignment. The default is what every unattended caller — pre-push, CI, the
// refinery gate — actually runs with, so that is the value under test.
var shellDefaultedAssignments = regexp.MustCompile(`(?m)^(?:readonly )?([a-z_][a-z0-9_]*)="\$\{[A-Za-z_][A-Za-z0-9_]*:-([^"$}]*)\}"$`)

// resolveShellVars substitutes the script's own top-level assignments into
// text, so an assertion reads the effective value rather than the `${name}`
// placeholder. Literal and env-defaulted assignments are resolved; anything
// else is left verbatim.
func resolveShellVars(t *testing.T, script, text string) string {
	t.Helper()
	for _, assignment := range []*regexp.Regexp{shellLiteralAssignments, shellDefaultedAssignments} {
		for _, match := range assignment.FindAllStringSubmatch(script, -1) {
			text = strings.ReplaceAll(text, "${"+match[1]+"}", match[2])
		}
	}
	return text
}

// goTestFlagValue reads the value of a `go test` flag from a command line,
// accepting both `-flag value` and `-flag=value` spellings. It returns an
// empty string when the flag is absent.
func goTestFlagValue(t *testing.T, command, flag string) string {
	t.Helper()
	pattern := regexp.MustCompile(`-` + regexp.QuoteMeta(flag) + `[= ]([^\s'"]+)`)
	match := pattern.FindStringSubmatch(command)
	if match == nil {
		return ""
	}
	return match[1]
}

// localParallelScript reads the local parallel test runner.
func localParallelScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "test-local-parallel"))
	if err != nil {
		t.Fatalf("read scripts/test-local-parallel: %v", err)
	}
	return string(data)
}
