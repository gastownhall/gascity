package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// installFakeDelegatedSystemctl writes an executable `systemctl` shim into a fresh
// temp dir, prepends that dir to PATH, and returns the path of the file
// the shim appends its argv into (one line per invocation). The shim
// prints stderrMsg to stderr (when non-empty) and exits with exitCode for
// mutating verbs, so tests can model both healthy and failing systemctl
// runs without a real systemd anywhere near the test. `is-active` probes
// are special-cased to exit 0 (unit active) without printing stderrMsg,
// keeping unit state independent of the mutating verb's outcome.
func installFakeDelegatedSystemctl(t *testing.T, exitCode int, stderrMsg string) string {
	t.Helper()
	return installFakeDelegatedSystemctlWithUnitState(t, exitCode, stderrMsg, 0)
}

// installFakeDelegatedSystemctlWithUnitState is installFakeDelegatedSystemctl
// with an explicit exit code for `is-active` probes (0 = active, non-zero
// = inactive).
func installFakeDelegatedSystemctlWithUnitState(t *testing.T, exitCode int, stderrMsg string, isActiveExit int) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "systemctl-args")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\n", argsFile)
	script += fmt.Sprintf("case \" $* \" in *\" is-active \"*) exit %d ;; esac\n", isActiveExit)
	if stderrMsg != "" {
		script += fmt.Sprintf("echo %q >&2\n", stderrMsg)
	}
	script += fmt.Sprintf("exit %d\n", exitCode)
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake systemctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

// installFakeDelegatedSystemctlHangingVerb installs a shim whose
// invocation of verb hangs (exec sleep) so tests can prove the CLI
// bounds the systemctl wait. is-active probes report active; other
// verbs succeed. Returns the path the shim records its argv into.
func installFakeDelegatedSystemctlHangingVerb(t *testing.T, verb string) string {
	t.Helper()
	return installFakeDelegatedSystemctlHangingVerbWithUnitState(t, verb, 0)
}

// installFakeDelegatedSystemctlHangingVerbWithUnitState is
// installFakeDelegatedSystemctlHangingVerb with an explicit exit code for
// `is-active` probes (0 = active, non-zero = inactive), so timeout tests
// can model whether the post-timeout liveness fallback observes a late
// start.
func installFakeDelegatedSystemctlHangingVerbWithUnitState(t *testing.T, verb string, isActiveExit int) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "systemctl-args")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\ncase \" $* \" in *\" is-active \"*) exit %d ;; *\" %s \"*) exec sleep 5 ;; esac\nexit 0\n", argsFile, isActiveExit, verb)
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake systemctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

// installFakeDelegatedSystemctlHangingStartAndIsActive installs a shim
// whose `start` AND `is-active` invocations both hang (exec sleep), while
// other verbs succeed. It models a wedged manager / D-Bus path: the bounded
// `systemctl start` times out, and the post-timeout is-active liveness
// probe would also hang without a CLI-side bound.
func installFakeDelegatedSystemctlHangingStartAndIsActive(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "systemctl-args")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\ncase \" $* \" in *\" is-active \"*) exec sleep 5 ;; *\" start \"*) exec sleep 5 ;; esac\nexit 0\n", argsFile)
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake systemctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// setDelegationEnvForTest configures the systemd delegation env for the
// test and pins supervisorRuntimeGOOS to linux so delegation tests
// exercise the linux/systemd contract on every development platform —
// supervisorSystemdDelegation rejects the env elsewhere.
func setDelegationEnvForTest(t *testing.T, unit, scope string) {
	t.Helper()
	old := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "linux"
	t.Cleanup(func() { supervisorRuntimeGOOS = old })
	t.Setenv(supervisorSystemdUnitEnv, unit)
	t.Setenv(supervisorSystemdScopeEnv, scope)
}

// readRecordedSystemctlArgs returns the argv lines the fake systemctl
// recorded, one invocation per element.
func readRecordedSystemctlArgs(t *testing.T, argsFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading recorded systemctl args: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

// stubSupervisorAliveAfterSystemctl makes supervisorAliveHook report a
// running supervisor only once the fake systemctl has been invoked
// (i.e., once argsFile exists), modeling a delegated unit that brings
// the supervisor up in response to `systemctl start`.
func stubSupervisorAliveAfterSystemctl(t *testing.T, argsFile string, pid int) {
	t.Helper()
	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int {
		if _, err := os.Stat(argsFile); err == nil {
			return pid
		}
		return 0
	}
}

// decodeLifecycleJSONLine parses the single JSONL summary line a
// delegated --json lifecycle action emits.
func decodeLifecycleJSONLine(t *testing.T, out string) map[string]any {
	t.Helper()
	line := strings.TrimSpace(out)
	if line == "" || strings.ContainsRune(line, '\n') {
		t.Fatalf("expected exactly one JSON line, got %q", out)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("unmarshaling %q: %v", line, err)
	}
	return payload
}

func TestSupervisorSystemdDelegationFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		unit    string
		scope   string
		wantOK  bool
		wantErr bool
		want    systemdDelegation
	}{
		{name: "unset env yields no delegation"},
		{name: "blank unit yields no delegation", unit: "   "},
		{
			name:   "default scope is system",
			unit:   "gascity-prod.service",
			wantOK: true,
			want:   systemdDelegation{Unit: "gascity-prod.service", Scope: "system"},
		},
		{
			name:   "explicit system scope",
			unit:   "gascity-prod.service",
			scope:  "system",
			wantOK: true,
			want:   systemdDelegation{Unit: "gascity-prod.service", Scope: "system"},
		},
		{
			name:   "explicit user scope",
			unit:   "gascity-prod.service",
			scope:  "user",
			wantOK: true,
			want:   systemdDelegation{Unit: "gascity-prod.service", Scope: "user"},
		},
		{
			name:    "invalid scope is an error not a silent system fallback",
			unit:    "gascity-prod.service",
			scope:   "remote",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDelegationEnvForTest(t, tc.unit, tc.scope)
			got, ok, err := supervisorSystemdDelegation()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("supervisorSystemdDelegation() err = nil, want scope error")
				}
				return
			}
			if err != nil {
				t.Fatalf("supervisorSystemdDelegation() err = %v, want nil", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("supervisorSystemdDelegation() ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("supervisorSystemdDelegation() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSupervisorSystemdDelegationRequiresLinux pins the platform
// contract: delegation env on a non-systemd platform is an explicit
// configuration error at parse time — surfaced by every lifecycle
// command — instead of a low-level "systemctl: executable file not
// found" once a delegated path execs.
func TestSupervisorSystemdDelegationRequiresLinux(t *testing.T) {
	old := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "darwin"
	t.Cleanup(func() { supervisorRuntimeGOOS = old })
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
	t.Setenv(supervisorSystemdScopeEnv, "")
	t.Setenv("GC_HOME", t.TempDir())

	_, ok, err := supervisorSystemdDelegation()
	if ok || err == nil {
		t.Fatalf("supervisorSystemdDelegation() = (ok=%v, err=%v), want platform error", ok, err)
	}
	for _, want := range []string{"requires linux", "darwin", supervisorSystemdUnitEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want %q named", err, want)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1; stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "requires linux") {
		t.Errorf("stderr = %q, want platform error surfaced", stderr.String())
	}
}

// TestGuardDelegatedSystemctlPath pins the test-binary backstop on the
// delegated exec seam: delegated paths bypass the supervisorSystemctlRun
// hook, so a systemctl resolving into a host system directory inside a
// test binary means a test is about to drive the operator's real
// systemd unit and must panic instead.
func TestGuardDelegatedSystemctlPath(t *testing.T) {
	t.Run("host system dir panics in test binaries", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal(`guardDelegatedSystemctlPath("/usr/bin/systemctl") did not panic inside a test binary`)
			}
		}()
		guardDelegatedSystemctlPath("/usr/bin/systemctl")
	})
	t.Run("temp shim path passes", func(t *testing.T) {
		guardDelegatedSystemctlPath(filepath.Join(t.TempDir(), "systemctl"))
	})
}

// TestDelegatedExecPathsRefuseHostSystemctl pins the guard wiring end to
// end: with no PATH shim installed, both delegated exec helpers must
// panic before spawning the host's real systemctl. Skips when the host
// resolves systemctl outside the guarded system directories (the guard
// is best-effort there); the unit name is deliberately one no host
// should define so a guard regression stays harmless.
func TestDelegatedExecPathsRefuseHostSystemctl(t *testing.T) {
	resolved, err := exec.LookPath("systemctl")
	if err != nil {
		t.Skip("no systemctl on PATH")
	}
	dir := filepath.Dir(resolved) + string(filepath.Separator)
	guarded := false
	for _, sys := range hostSystemctlDirs {
		if dir == sys {
			guarded = true
		}
	}
	if !guarded {
		t.Skipf("host systemctl %q resolves outside the guarded system dirs", resolved)
	}
	d := systemdDelegation{Unit: "gc-test-guard-nonexistent.service", Scope: "system"}
	t.Run("delegatedUnitActive", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("delegatedUnitActive reached host systemctl without panicking")
			}
		}()
		delegatedUnitActive(d)
	})
	t.Run("runDelegatedSystemctlTimeout", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("runDelegatedSystemctlTimeout reached host systemctl without panicking")
			}
		}()
		_ = runDelegatedSystemctlTimeout(d, "start", time.Second)
	})
}

func TestSystemdDelegationCommandShapes(t *testing.T) {
	sys := systemdDelegation{Unit: "u.service", Scope: "system"}
	usr := systemdDelegation{Unit: "u.service", Scope: "user"}
	if got := strings.Join(sys.systemctlArgs("start"), " "); got != "start u.service" {
		t.Errorf("system scope start args = %q, want %q", got, "start u.service")
	}
	if got := strings.Join(usr.systemctlArgs("stop"), " "); got != "--user stop u.service" {
		t.Errorf("user scope stop args = %q, want %q", got, "--user stop u.service")
	}
	if got := sys.commandHint("restart"); got != "systemctl restart u.service" {
		t.Errorf("system scope hint = %q, want %q", got, "systemctl restart u.service")
	}
	if got := usr.commandHint("restart"); got != "systemctl --user restart u.service" {
		t.Errorf("user scope hint = %q, want %q", got, "systemctl --user restart u.service")
	}
	if got := strings.Join(sys.systemctlIsActiveArgs(), " "); got != "is-active --quiet u.service" {
		t.Errorf("system scope is-active args = %q, want %q", got, "is-active --quiet u.service")
	}
	if got := strings.Join(usr.systemctlIsActiveArgs(), " "); got != "--user is-active --quiet u.service" {
		t.Errorf("user scope is-active args = %q, want %q", got, "--user is-active --quiet u.service")
	}
}

func TestSupervisorStartDelegatesToSystemctl(t *testing.T) {
	cases := []struct {
		name     string
		scope    string
		wantArgs string
	}{
		{name: "system scope", scope: "", wantArgs: "start gascity-prod.service"},
		{name: "user scope", scope: "user", wantArgs: "--user start gascity-prod.service"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_HOME", t.TempDir())
			setDelegationEnvForTest(t, "gascity-prod.service", tc.scope)
			argsFile := installFakeDelegatedSystemctl(t, 0, "")
			stubSupervisorAliveAfterSystemctl(t, argsFile, 4242)

			var stdout, stderr bytes.Buffer
			if code := doSupervisorStart(&stdout, &stderr); code != 0 {
				t.Fatalf("doSupervisorStart code = %d, want 0; stderr=%q", code, stderr.String())
			}
			lines := readRecordedSystemctlArgs(t, argsFile)
			if len(lines) != 1 || lines[0] != tc.wantArgs {
				t.Fatalf("systemctl invocations = %v, want exactly [%q]", lines, tc.wantArgs)
			}
			if !strings.Contains(stdout.String(), "Supervisor started (PID 4242)") {
				t.Errorf("stdout = %q, want ready line with PID 4242", stdout.String())
			}
		})
	}
}

func TestSupervisorStartDelegatedJSON(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")
	stubSupervisorAliveAfterSystemctl(t, argsFile, 4242)

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStartJSON(&stdout, &stderr, true); code != 0 {
		t.Fatalf("doSupervisorStartJSON code = %d, want 0; stderr=%q", code, stderr.String())
	}
	payload := decodeLifecycleJSONLine(t, stdout.String())
	if payload["ok"] != true || payload["command"] != "supervisor start" || payload["action"] != "start" {
		t.Errorf("payload = %v, want ok=true command=%q action=%q", payload, "supervisor start", "start")
	}
	if pid, _ := payload["supervisor_pid"].(float64); int(pid) != 4242 {
		t.Errorf("payload supervisor_pid = %v, want 4242", payload["supervisor_pid"])
	}
}

func TestSupervisorStartDelegatedSystemctlFailureSurfacesError(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctl(t, 5, "Unit gascity-prod.service not found.")

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 0 }

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "systemctl start gascity-prod.service") {
		t.Errorf("stderr = %q, want failing systemctl command named", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Unit gascity-prod.service not found.") {
		t.Errorf("stderr = %q, want systemctl output included", stderr.String())
	}
}

// delegatedStartFallbackEnv prepares a delegated start whose control
// socket never answers: delegation env set, supervisor never alive via
// the socket, the readiness poll shrunk, and the API hook pinned to
// apiReachable so each test controls the full fallback evidence chain.
func delegatedStartFallbackEnv(t *testing.T, isActiveExit int, apiReachable bool) string {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctlWithUnitState(t, 0, "", isActiveExit)

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 0 }
	oldTimeout := supervisorReadyTimeout
	supervisorReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorReadyTimeout = oldTimeout })
	oldAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return apiReachable }
	t.Cleanup(func() { supervisorAPIReachable = oldAPI })
	return argsFile
}

// TestSupervisorStartDelegatedSocketUnreachableTrustsServiceManager pins
// the start half of the gascity#2984 fallback under delegation: in the
// documented system-scope/other-uid topology the control socket is
// unreachable from the operator's shell, so an active unit after the
// readiness poll must report success the way status already does — not
// a false "did not become ready".
func TestSupervisorStartDelegatedSocketUnreachableTrustsServiceManager(t *testing.T) {
	argsFile := delegatedStartFallbackEnv(t, 0, false)

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 0 {
		t.Fatalf("doSupervisorStart code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pid unavailable: control socket unreachable; liveness confirmed via service_manager") {
		t.Errorf("stdout = %q, want service-manager liveness framing", stdout.String())
	}
	lines := readRecordedSystemctlArgs(t, argsFile)
	want := []string{"start gascity-prod.service", "is-active --quiet gascity-prod.service"}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("systemctl invocations = %v, want %v", lines, want)
	}
}

// TestSupervisorStartDelegatedSocketUnreachableJSONNamesPIDSource pins
// the --json contract for the fallback success: ok with pid_source
// naming the evidence and no supervisor_pid (the socket never answered),
// mirroring `gc supervisor status --json`.
func TestSupervisorStartDelegatedSocketUnreachableJSONNamesPIDSource(t *testing.T) {
	delegatedStartFallbackEnv(t, 0, false)

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStartJSON(&stdout, &stderr, true); code != 0 {
		t.Fatalf("doSupervisorStartJSON code = %d, want 0; stderr=%q", code, stderr.String())
	}
	payload := decodeLifecycleJSONLine(t, stdout.String())
	if payload["ok"] != true || payload["pid_source"] != "service_manager" {
		t.Errorf("payload = %v, want ok=true pid_source=%q", payload, "service_manager")
	}
	if pid, present := payload["supervisor_pid"]; present {
		t.Errorf("payload supervisor_pid = %v, want omitted when the socket never answered", pid)
	}
}

// TestSupervisorStartDelegatedSocketUnreachableFallsBackToAPI pins the
// second link of the evidence chain: an inactive-reading unit with a
// reachable supervisor API still reports success, attributed to the API.
func TestSupervisorStartDelegatedSocketUnreachableFallsBackToAPI(t *testing.T) {
	delegatedStartFallbackEnv(t, 3, true)

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 0 {
		t.Fatalf("doSupervisorStart code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "liveness confirmed via api") {
		t.Errorf("stdout = %q, want api liveness framing", stdout.String())
	}
}

// TestSupervisorStartDelegatedNotReadyWithoutLivenessEvidenceFails pins
// the genuine failure: socket silent, unit inactive, API unreachable —
// only then is "did not become ready" the truth.
func TestSupervisorStartDelegatedNotReadyWithoutLivenessEvidenceFails(t *testing.T) {
	delegatedStartFallbackEnv(t, 3, false)

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1; stdout=%q", code, stdout.String())
	}
	for _, want := range []string{"did not become ready", "check 'systemctl status gascity-prod.service'"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

// TestRunDelegatedSystemctlTimeoutClassifiesTimeout pins the contract the
// start/ensure/try-restart fall-through depends on: a bounded systemctl
// invocation that exceeds its budget returns a delegatedSystemctlTimeoutError
// (recognized by isDelegatedSystemctlTimeout), while an ordinary systemctl
// failure does not — so only a true timeout is treated as a bounded wait.
func TestRunDelegatedSystemctlTimeoutClassifiesTimeout(t *testing.T) {
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	d := systemdDelegation{Unit: "gascity-prod.service", Scope: "system"}

	t.Run("timeout is classified and bounded", func(t *testing.T) {
		installFakeDelegatedSystemctlHangingVerb(t, "start")
		start := time.Now()
		err := runDelegatedSystemctlTimeout(d, "start", 200*time.Millisecond)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("runDelegatedSystemctlTimeout err = nil, want timeout")
		}
		if !isDelegatedSystemctlTimeout(err) {
			t.Errorf("isDelegatedSystemctlTimeout(%v) = false, want true", err)
		}
		if !strings.Contains(err.Error(), "timed out after") {
			t.Errorf("err = %q, want 'timed out after'", err.Error())
		}
		if elapsed > 3*time.Second {
			t.Fatalf("runDelegatedSystemctlTimeout took %s; the bound did not apply", elapsed)
		}
	})

	t.Run("ordinary failure is not a timeout", func(t *testing.T) {
		installFakeDelegatedSystemctl(t, 5, "Unit gascity-prod.service not found.")
		err := runDelegatedSystemctlTimeout(d, "start", 2*time.Second)
		if err == nil {
			t.Fatal("runDelegatedSystemctlTimeout err = nil, want failure")
		}
		if isDelegatedSystemctlTimeout(err) {
			t.Errorf("isDelegatedSystemctlTimeout(%v) = true, want false for an ordinary systemctl failure", err)
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("err = %q, want systemctl diagnostic folded in", err.Error())
		}
	})
}

// TestSupervisorStartDelegatedBoundsSystemctl pins the CLI-side budget on
// delegated `systemctl start`: a wedged manager or a unit with
// TimeoutStartSec=infinity must not hold `gc supervisor start` past the
// job timeout, matching the bounded delegated stop. The bounded timeout is
// not terminal — it falls through to the same readiness/is-active/API
// fallback a normal start uses — so with the unit still inactive and the
// API unreachable the command reports the standard "did not become ready"
// failure instead of hanging or falsely succeeding.
func TestSupervisorStartDelegatedBoundsSystemctl(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctlHangingVerbWithUnitState(t, "start", 3)
	oldJob := delegatedSystemctlJobTimeout
	delegatedSystemctlJobTimeout = 300 * time.Millisecond
	t.Cleanup(func() { delegatedSystemctlJobTimeout = oldJob })

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 0 }
	oldTimeout := supervisorReadyTimeout
	supervisorReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorReadyTimeout = oldTimeout })
	oldAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() { supervisorAPIReachable = oldAPI })

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := doSupervisorStart(&stdout, &stderr)
	elapsed := time.Since(start)
	if code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("delegated start took %s; the job timeout did not bound the systemctl invocation", elapsed)
	}
	if !strings.Contains(stderr.String(), "did not become ready") {
		t.Errorf("stderr = %q, want 'did not become ready' after the bounded timeout fell through to liveness verification", stderr.String())
	}
}

// TestSupervisorStartDelegatedTimeoutThenLateStartSucceeds pins the
// bounded-wait fix: a delegated `systemctl start` that exceeds the CLI
// budget but whose unit still comes up inside systemd must report success
// via the is-active fallback, not a false failure. The hanging shim leaves
// the unit is-active, modeling a start that completes late.
func TestSupervisorStartDelegatedTimeoutThenLateStartSucceeds(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctlHangingVerbWithUnitState(t, "start", 0)
	oldJob := delegatedSystemctlJobTimeout
	delegatedSystemctlJobTimeout = 300 * time.Millisecond
	t.Cleanup(func() { delegatedSystemctlJobTimeout = oldJob })

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 0 }
	oldTimeout := supervisorReadyTimeout
	supervisorReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorReadyTimeout = oldTimeout })
	oldAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() { supervisorAPIReachable = oldAPI })

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := doSupervisorStart(&stdout, &stderr)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("doSupervisorStart code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("delegated start took %s; the job timeout did not bound the systemctl invocation", elapsed)
	}
	if !strings.Contains(stdout.String(), "liveness confirmed via service_manager") {
		t.Errorf("stdout = %q, want service-manager liveness after a late start", stdout.String())
	}
}

// TestDelegatedUnitActiveBoundsHangingProbe pins the is-active bound
// directly: a `systemctl is-active` against a wedged manager must read as
// inactive within delegatedIsActiveTimeout instead of blocking the caller.
func TestDelegatedUnitActiveBoundsHangingProbe(t *testing.T) {
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctlHangingStartAndIsActive(t)
	oldIsActive := delegatedIsActiveTimeout
	delegatedIsActiveTimeout = 300 * time.Millisecond
	t.Cleanup(func() { delegatedIsActiveTimeout = oldIsActive })

	d := systemdDelegation{Unit: "gascity-prod.service", Scope: "system"}
	start := time.Now()
	active := delegatedUnitActive(d)
	elapsed := time.Since(start)
	if active {
		t.Errorf("delegatedUnitActive = true, want false when the is-active probe hangs past its bound")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("delegatedUnitActive took %s; the is-active probe was not bounded", elapsed)
	}
}

// TestSupervisorStartDelegatedTimeoutThenHangingIsActiveStaysBounded pins
// the is-active bound on the start fall-through: a wedged manager whose
// `systemctl start` times out and whose `is-active` probe also hangs must
// not hold `gc supervisor start` forever in delegatedLivenessWithoutSocket.
// The bounded is-active probe reads inactive, so start falls through to the
// reachable supervisor API and reports success via "api" — instead of
// blocking on the unbounded is-active call and never reaching the API.
func TestSupervisorStartDelegatedTimeoutThenHangingIsActiveStaysBounded(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctlHangingStartAndIsActive(t)

	oldJob := delegatedSystemctlJobTimeout
	delegatedSystemctlJobTimeout = 300 * time.Millisecond
	t.Cleanup(func() { delegatedSystemctlJobTimeout = oldJob })
	oldIsActive := delegatedIsActiveTimeout
	delegatedIsActiveTimeout = 300 * time.Millisecond
	t.Cleanup(func() { delegatedIsActiveTimeout = oldIsActive })

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 0 }
	oldTimeout := supervisorReadyTimeout
	supervisorReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorReadyTimeout = oldTimeout })
	oldAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return true }
	t.Cleanup(func() { supervisorAPIReachable = oldAPI })

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := doSupervisorStart(&stdout, &stderr)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("doSupervisorStart code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("delegated start took %s; the is-active probe was not bounded after the start timeout", elapsed)
	}
	if !strings.Contains(stdout.String(), "liveness confirmed via api") {
		t.Errorf("stdout = %q, want api liveness after the bounded is-active probe read inactive", stdout.String())
	}
}

// TestEnsureSupervisorRunningDelegatedBoundsSystemctl is the gc-start
// ensure twin of TestSupervisorStartDelegatedBoundsSystemctl: the bounded
// timeout falls through to the socket-then-fallback liveness check and,
// with the unit still inactive and the API unreachable, reports "did not
// become ready" instead of hanging or falsely succeeding.
func TestEnsureSupervisorRunningDelegatedBoundsSystemctl(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctlHangingVerbWithUnitState(t, "start", 3)
	oldJob := delegatedSystemctlJobTimeout
	delegatedSystemctlJobTimeout = 300 * time.Millisecond
	t.Cleanup(func() { delegatedSystemctlJobTimeout = oldJob })

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 0 }
	oldTimeout := supervisorReadyTimeout
	supervisorReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorReadyTimeout = oldTimeout })
	oldAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() { supervisorAPIReachable = oldAPI })

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := ensureSupervisorRunning(&stdout, &stderr)
	elapsed := time.Since(start)
	if code != 1 {
		t.Fatalf("ensureSupervisorRunning code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("delegated ensure-start took %s; the job timeout did not bound the systemctl invocation", elapsed)
	}
	if !strings.Contains(stderr.String(), "did not become ready") {
		t.Errorf("stderr = %q, want 'did not become ready' after the bounded timeout fell through to liveness verification", stderr.String())
	}
}

// TestEnsureSupervisorRunningDelegatedTimeoutThenLateStartSucceeds is the
// gc-start ensure twin of
// TestSupervisorStartDelegatedTimeoutThenLateStartSucceeds: a bounded
// systemctl-start timeout whose unit still comes up late must succeed via
// the is-active fallback rather than fail the whole `gc start`.
func TestEnsureSupervisorRunningDelegatedTimeoutThenLateStartSucceeds(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctlHangingVerbWithUnitState(t, "start", 0)
	oldJob := delegatedSystemctlJobTimeout
	delegatedSystemctlJobTimeout = 300 * time.Millisecond
	t.Cleanup(func() { delegatedSystemctlJobTimeout = oldJob })

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 0 }
	oldTimeout := supervisorReadyTimeout
	supervisorReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorReadyTimeout = oldTimeout })
	oldAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() { supervisorAPIReachable = oldAPI })

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := ensureSupervisorRunning(&stdout, &stderr)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("ensureSupervisorRunning code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("delegated ensure-start took %s; the job timeout did not bound the systemctl invocation", elapsed)
	}
	if strings.Contains(stderr.String(), "did not become ready") {
		t.Errorf("stderr = %q, want no readiness failure after a late start", stderr.String())
	}
}

func TestSupervisorStartDelegationInvalidScopeFails(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "remote")

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), supervisorSystemdScopeEnv) {
		t.Errorf("stderr = %q, want %s named", stderr.String(), supervisorSystemdScopeEnv)
	}
}

func TestSupervisorStopDelegatesToSystemctl(t *testing.T) {
	cases := []struct {
		name      string
		scope     string
		wantProbe string
		wantStop  string
	}{
		{
			name:      "system scope",
			scope:     "",
			wantProbe: "is-active --quiet gascity-prod.service",
			wantStop:  "stop gascity-prod.service",
		},
		{
			name:      "user scope",
			scope:     "user",
			wantProbe: "--user is-active --quiet gascity-prod.service",
			wantStop:  "--user stop gascity-prod.service",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh GC_HOME with no supervisor socket: the legacy stop path
			// would fail with "supervisor is not running"; the delegated
			// path trusts the active unit instead and never drives the
			// destructive control-socket stop protocol.
			t.Setenv("GC_HOME", t.TempDir())
			setDelegationEnvForTest(t, "gascity-prod.service", tc.scope)
			argsFile := installFakeDelegatedSystemctl(t, 0, "")

			var stdout, stderr bytes.Buffer
			if code := stopSupervisorWithWait(&stdout, &stderr, false, 0); code != 0 {
				t.Fatalf("stopSupervisorWithWait code = %d, want 0; stderr=%q", code, stderr.String())
			}
			lines := readRecordedSystemctlArgs(t, argsFile)
			if len(lines) != 2 || lines[0] != tc.wantProbe || lines[1] != tc.wantStop {
				t.Fatalf("systemctl invocations = %v, want exactly [%q %q]", lines, tc.wantProbe, tc.wantStop)
			}
			if !strings.Contains(stdout.String(), "Supervisor stopped.") {
				t.Errorf("stdout = %q, want %q", stdout.String(), "Supervisor stopped.")
			}
		})
	}
}

func TestSupervisorStopDelegatedJSON(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctl(t, 0, "")

	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWaitJSON(&stdout, &stderr, false, 0, true); code != 0 {
		t.Fatalf("stopSupervisorWithWaitJSON code = %d, want 0; stderr=%q", code, stderr.String())
	}
	payload := decodeLifecycleJSONLine(t, stdout.String())
	if payload["ok"] != true || payload["command"] != "supervisor stop" || payload["action"] != "stop" {
		t.Errorf("payload = %v, want ok=true command=%q action=%q", payload, "supervisor stop", "stop")
	}
	if payload["message"] != "Supervisor stopped." {
		t.Errorf("payload message = %v, want %q", payload["message"], "Supervisor stopped.")
	}
	if payload["wait"] != false {
		t.Errorf("payload wait = %v, want false", payload["wait"])
	}
}

// TestSupervisorStopDelegatedVerifiesSupervisorExit pins the managed
// happy path: a supervisor that was alive before the delegated stop and
// disappears once `systemctl stop` ran reports success.
func TestSupervisorStopDelegatedVerifiesSupervisorExit(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int {
		data, err := os.ReadFile(argsFile)
		if err == nil && strings.Contains(string(data), "stop gascity-prod.service") {
			return 0
		}
		return 4242
	}

	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, false, 0); code != 0 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Supervisor stopped.") {
		t.Errorf("stdout = %q, want %q", stdout.String(), "Supervisor stopped.")
	}
}

// TestSupervisorStopDelegatedUnmanagedSupervisorFails pins the false-success
// fix: `systemctl stop` no-ops when the live supervisor is not managed by
// the delegated unit, so the stop must fail naming the surviving PID
// instead of printing "Supervisor stopped.".
func TestSupervisorStopDelegatedUnmanagedSupervisorFails(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 4343 }
	oldVerify := delegatedStopVerifyTimeout
	delegatedStopVerifyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { delegatedStopVerifyTimeout = oldVerify })

	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, false, 0); code != 1 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 1; stdout=%q", code, stdout.String())
	}
	if strings.Contains(stdout.String(), "Supervisor stopped.") {
		t.Errorf("stdout = %q, must not report success for an unmanaged supervisor", stdout.String())
	}
	for _, want := range []string{"still running (PID 4343)", "gascity-prod.service", supervisorSystemdUnitEnv} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	lines := readRecordedSystemctlArgs(t, argsFile)
	if len(lines) != 1 || lines[0] != "stop gascity-prod.service" {
		t.Fatalf("systemctl invocations = %v, want exactly the stop (a live supervisor needs no is-active probe)", lines)
	}
}

// TestSupervisorStopDelegatedNothingRunningKeepsExit1 pins the legacy
// scriptable contract: stop with no live supervisor and an inactive unit
// still exits 1 with "supervisor is not running" instead of a false
// "Supervisor stopped.".
func TestSupervisorStopDelegatedNothingRunningKeepsExit1(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctlWithUnitState(t, 0, "", 3)

	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, false, 0); code != 1 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 1; stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "supervisor is not running") {
		t.Errorf("stderr = %q, want legacy not-running message", stderr.String())
	}
	lines := readRecordedSystemctlArgs(t, argsFile)
	if len(lines) != 1 || lines[0] != "is-active --quiet gascity-prod.service" {
		t.Fatalf("systemctl invocations = %v, want only the is-active probe (no stop of an inactive unit)", lines)
	}
}

// TestSupervisorStopDelegatedWaitTimeoutBoundsSystemctl pins the CLI
// contract: --wait-timeout bounds the synchronous systemctl stop instead
// of letting a wedged unit hold the command for systemd's own timeout.
func TestSupervisorStopDelegatedWaitTimeoutBoundsSystemctl(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctlHangingVerb(t, "stop")

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := stopSupervisorWithWait(&stdout, &stderr, true, 300*time.Millisecond)
	elapsed := time.Since(start)
	if code != 1 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("delegated stop took %s; --wait-timeout=300ms did not bound the systemctl invocation", elapsed)
	}
	if !strings.Contains(stderr.String(), "timed out after") {
		t.Errorf("stderr = %q, want systemctl timeout named", stderr.String())
	}
}

func TestSupervisorStopDelegatedSystemctlFailureSurfacesError(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctl(t, 4, "Interactive authentication required.")

	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, false, 0); code != 1 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "systemctl stop gascity-prod.service") {
		t.Errorf("stderr = %q, want failing systemctl command named", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Interactive authentication required.") {
		t.Errorf("stderr = %q, want systemctl output included", stderr.String())
	}
}

func TestEnsureSupervisorRunningDelegatesToSystemctl(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")
	stubSupervisorAliveAfterSystemctl(t, argsFile, 4242)

	var stdout, stderr bytes.Buffer
	if code := ensureSupervisorRunning(&stdout, &stderr); code != 0 {
		t.Fatalf("ensureSupervisorRunning code = %d, want 0; stderr=%q", code, stderr.String())
	}
	// Exactly one `systemctl start <unit>` call: install (daemon-reload,
	// enable, ...) must never run in delegated mode, and the fake records
	// every systemctl invocation, so extra lines would expose it.
	lines := readRecordedSystemctlArgs(t, argsFile)
	if len(lines) != 1 || lines[0] != "start gascity-prod.service" {
		t.Fatalf("systemctl invocations = %v, want exactly [%q]", lines, "start gascity-prod.service")
	}
}

func TestEnsureSupervisorRunningDelegatedAlreadyRunningSkipsSystemctl(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 99 }

	var stdout, stderr bytes.Buffer
	if code := ensureSupervisorRunning(&stdout, &stderr); code != 0 {
		t.Fatalf("ensureSupervisorRunning code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("systemctl was invoked for an already-running supervisor; recorded args: %v",
			readRecordedSystemctlArgs(t, argsFile))
	}
}

// TestEnsureSupervisorRunningDelegatedTimeoutPointsAtUnit pins the
// readiness-timeout diagnostic in delegated mode: a delegated supervisor
// logs to the journal, so the message must point at the unit's systemctl
// status command, not gc's fork-mode log file. The unit must read as
// inactive and the API as unreachable — with liveness evidence the
// silent socket is a success, not a timeout (see
// TestEnsureSupervisorRunningDelegatedSocketUnreachableTrustsServiceManager).
func TestEnsureSupervisorRunningDelegatedTimeoutPointsAtUnit(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctlWithUnitState(t, 0, "", 3)

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 0 }
	oldTimeout := supervisorReadyTimeout
	supervisorReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorReadyTimeout = oldTimeout })
	oldAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() { supervisorAPIReachable = oldAPI })

	var stdout, stderr bytes.Buffer
	if code := ensureSupervisorRunning(&stdout, &stderr); code != 1 {
		t.Fatalf("ensureSupervisorRunning code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "check 'systemctl status gascity-prod.service'") {
		t.Errorf("stderr = %q, want systemctl status guidance", stderr.String())
	}
	if strings.Contains(stderr.String(), "supervisor.log") {
		t.Errorf("stderr = %q, must not point at the fork-mode log file in delegated mode", stderr.String())
	}
}

// TestEnsureSupervisorRunningDelegatedSocketUnreachableTrustsServiceManager
// pins the gc-start ensure path in the documented system-scope/other-uid
// topology: `systemctl start` succeeds, the control socket never answers
// from this shell, and the active unit must read as success — not the
// false "did not become ready" that aborts `gc start` while status
// reports the same supervisor as running.
func TestEnsureSupervisorRunningDelegatedSocketUnreachableTrustsServiceManager(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 0 }
	oldTimeout := supervisorReadyTimeout
	supervisorReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorReadyTimeout = oldTimeout })
	oldAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() { supervisorAPIReachable = oldAPI })

	var stdout, stderr bytes.Buffer
	if code := ensureSupervisorRunning(&stdout, &stderr); code != 0 {
		t.Fatalf("ensureSupervisorRunning code = %d, want 0; stderr=%q", code, stderr.String())
	}
	lines := readRecordedSystemctlArgs(t, argsFile)
	want := []string{"start gascity-prod.service", "is-active --quiet gascity-prod.service"}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("systemctl invocations = %v, want %v", lines, want)
	}
}

// TestSupervisorStatusDelegatedUnitFallback pins the control-socket
// fallback under delegation: when the socket is unreachable (the common
// case for a system-scope unit running under another uid), status must
// probe the delegated unit at its configured scope, not gc's own user
// unit.
func TestSupervisorStatusDelegatedUnitFallback(t *testing.T) {
	cases := []struct {
		name         string
		scope        string
		isActiveExit int
		wantProbe    string
		wantRunning  bool
	}{
		{
			name:        "system scope active unit reports running",
			scope:       "",
			wantProbe:   "is-active --quiet gascity-prod.service",
			wantRunning: true,
		},
		{
			name:        "user scope active unit reports running",
			scope:       "user",
			wantProbe:   "--user is-active --quiet gascity-prod.service",
			wantRunning: true,
		},
		{
			name:         "inactive unit reports not running",
			scope:        "",
			isActiveExit: 3,
			wantProbe:    "is-active --quiet gascity-prod.service",
			wantRunning:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_HOME", t.TempDir())
			setDelegationEnvForTest(t, "gascity-prod.service", tc.scope)
			argsFile := installFakeDelegatedSystemctlWithUnitState(t, 0, "", tc.isActiveExit)

			oldAPI := supervisorAPIReachable
			supervisorAPIReachable = func() bool { return false }
			t.Cleanup(func() { supervisorAPIReachable = oldAPI })

			var stdout bytes.Buffer
			code := supervisorStatusWithOptions(&stdout, io.Discard, false)
			if tc.wantRunning {
				if code != 0 {
					t.Fatalf("supervisorStatusWithOptions code = %d, want 0; stdout=%q", code, stdout.String())
				}
				if !strings.Contains(stdout.String(), "service_manager") {
					t.Errorf("stdout = %q, want liveness confirmed via service_manager", stdout.String())
				}
			} else {
				if code != 1 {
					t.Fatalf("supervisorStatusWithOptions code = %d, want 1; stdout=%q", code, stdout.String())
				}
				if !strings.Contains(stdout.String(), "Supervisor is not running") {
					t.Errorf("stdout = %q, want not-running report", stdout.String())
				}
			}
			lines := readRecordedSystemctlArgs(t, argsFile)
			if len(lines) != 1 || lines[0] != tc.wantProbe {
				t.Fatalf("systemctl invocations = %v, want exactly [%q]", lines, tc.wantProbe)
			}
		})
	}
}

// TestSupervisorStatusInvalidDelegationScope pins that the one read-only
// lifecycle command surfaces a broken delegation env instead of
// swallowing it: with an invalid GC_SUPERVISOR_SYSTEMD_SCOPE, status must
// name the configuration error in both output modes (a stderr line, plus
// a config_error field in --json) rather than letting an unreachable
// control socket read as a bare "Supervisor is not running". Every
// mutating sibling hard-errors on the same typo; status is the command
// operators and monitoring run first when debugging that migration. The
// unit probe must not run — an unparseable scope leaves no trustworthy
// unit to ask.
func TestSupervisorStatusInvalidDelegationScope(t *testing.T) {
	setup := func(t *testing.T, apiReachable bool) string {
		t.Helper()
		t.Setenv("GC_HOME", t.TempDir())
		setDelegationEnvForTest(t, "gascity-prod.service", "systme")
		argsFile := installFakeDelegatedSystemctl(t, 0, "")
		oldAPI := supervisorAPIReachable
		supervisorAPIReachable = func() bool { return apiReachable }
		t.Cleanup(func() { supervisorAPIReachable = oldAPI })
		return argsFile
	}
	const wantErrText = `invalid GC_SUPERVISOR_SYSTEMD_SCOPE="systme"`

	t.Run("text mode names the config error and keeps the not-running exit", func(t *testing.T) {
		argsFile := setup(t, false)
		var stdout, stderr bytes.Buffer
		code := supervisorStatusWithOptions(&stdout, &stderr, false)
		if code != 1 {
			t.Fatalf("supervisorStatusWithOptions code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "Supervisor is not running") {
			t.Errorf("stdout = %q, want not-running report", stdout.String())
		}
		if !strings.Contains(stderr.String(), wantErrText) {
			t.Errorf("stderr = %q, want config error naming %s", stderr.String(), wantErrText)
		}
		if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
			t.Errorf("systemctl probe ran despite unparseable scope: %v", readRecordedSystemctlArgs(t, argsFile))
		}
	})

	t.Run("json mode embeds config_error", func(t *testing.T) {
		setup(t, false)
		var stdout, stderr bytes.Buffer
		code := supervisorStatusWithOptions(&stdout, &stderr, true)
		if code != 0 {
			t.Fatalf("supervisorStatusWithOptions code = %d, want 0 (JSON encodes run-state in the payload); stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		var payload struct {
			Running     bool   `json:"running"`
			ConfigError string `json:"config_error"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
		}
		if payload.Running {
			t.Errorf("running = true, want false; payload stdout=%q", stdout.String())
		}
		if !strings.Contains(payload.ConfigError, wantErrText) {
			t.Errorf("config_error = %q, want it to name %s", payload.ConfigError, wantErrText)
		}
	})

	t.Run("config error still surfaces when liveness comes from the api", func(t *testing.T) {
		setup(t, true)
		var stdout, stderr bytes.Buffer
		code := supervisorStatusWithOptions(&stdout, &stderr, false)
		if code != 0 {
			t.Fatalf("supervisorStatusWithOptions code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "liveness confirmed via api") {
			t.Errorf("stdout = %q, want running-via-api report", stdout.String())
		}
		if !strings.Contains(stderr.String(), wantErrText) {
			t.Errorf("stderr = %q, want config error naming %s", stderr.String(), wantErrText)
		}
	})
}

// TestSupervisorInstallRefusesDelegation pins the install guard: gc must
// not write or load its own service files while the supervisor lifecycle
// is delegated to an operator-managed unit.
func TestSupervisorInstallRefusesDelegation(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")

	var stdout, stderr bytes.Buffer
	if code := doSupervisorInstall(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorInstall code = %d, want 1; stdout=%q", code, stdout.String())
	}
	for _, want := range []string{supervisorSystemdUnitEnv, "gascity-prod.service", "delegated"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestSupervisorInstallInvalidDelegationScopeFails(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "remote")

	var stdout, stderr bytes.Buffer
	if code := doSupervisorInstall(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorInstall code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), supervisorSystemdScopeEnv) {
		t.Errorf("stderr = %q, want %s named", stderr.String(), supervisorSystemdScopeEnv)
	}
}

// TestSupervisorUninstallWarnsAndSkipsDelegatedUnit pins the uninstall
// guard: with delegation configured, uninstall warns, removes only
// gc-owned service state, and never issues a systemctl command against
// the delegated unit.
func TestSupervisorUninstallWarnsAndSkipsDelegatedUnit(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(string) bool { return false }
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := doSupervisorUninstall(&stdout, &stderr); code != 0 {
		t.Fatalf("doSupervisorUninstall code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not touch the delegated unit") {
		t.Errorf("stderr = %q, want delegation warning", stderr.String())
	}
	for _, call := range calls {
		if strings.Contains(call, "gascity-prod.service") {
			t.Fatalf("systemctl call %q targets the delegated unit during uninstall", call)
		}
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("uninstall invoked PATH systemctl against the delegated unit; recorded args: %v",
			readRecordedSystemctlArgs(t, argsFile))
	}
}

// TestUninstallSupervisorSystemdUnderDelegationStopsOwnUnitViaSocket pins
// the blast-radius fix: uninstalling gc's own active unit while
// delegation is configured must drive the graceful control-socket stop of
// gc's own supervisor — not `systemctl stop <delegated-unit>`, which
// would take down the operator's production supervisor as a side effect.
func TestUninstallSupervisorSystemdUnderDelegationStopsOwnUnitViaSocket(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")

	currentPath := filepath.Join(homeDir, ".local", "share", "systemd", "user", supervisorSystemdServiceName())
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(currentPath), err)
	}
	if err := os.WriteFile(currentPath, []byte("current unit\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", currentPath, err)
	}

	var (
		mu             sync.Mutex
		socketStopSeen bool
		stopped        bool
	)
	sockPath := supervisorSocketPath()
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		mu.Lock()
		defer mu.Unlock()
		switch cmd {
		case "ping":
			if stopped {
				return ""
			}
			return "4242\n"
		case "stop":
			socketStopSeen = true
			stopped = true
			return "ok\ndone:ok\n"
		}
		return ""
	})

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	supervisorSystemctlRun = func(...string) error { return nil }
	supervisorSystemctlActive = func(service string) bool {
		return service == supervisorSystemdServiceName()
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := uninstallSupervisorSystemd(&supervisorServiceData{}, &stdout, &stderr); code != 0 {
		t.Fatalf("uninstallSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !socketStopSeen {
		t.Fatal("uninstall did not stop gc's own supervisor through the control socket")
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("uninstall invoked PATH systemctl (delegated redirect leaked into uninstall); recorded args: %v",
			readRecordedSystemctlArgs(t, argsFile))
	}
}

// TestRunStartDriftCheck_DelegatedRestartUsesTryRestart pins the drift
// auto-restart path under GC_SUPERVISOR_SYSTEMD_UNIT: the restart is a
// single `systemctl try-restart <unit>` and none of gc's own restart
// machinery (user-unit systemctl, launchctl, SIGTERM+respawn) fires. The
// fake systemctl restarts nothing and /health keeps serving the old
// build, so the check must FAIL: declaring "ready" here would leave every
// subsequent `gc start` in a detect → no-op → "ready" treadmill against a
// supervisor the unit does not manage.
func TestRunStartDriftCheck_DelegatedRestartUsesTryRestart(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")
	// The supervisor is never replaced, so the verification poll runs to
	// driftReadyTimeout before reporting "was not replaced"; keep it short.
	shrinkDriftReadyTimeout(t)

	oldHelpers := restartHelpersHook
	t.Cleanup(func() { restartHelpersHook = oldHelpers })
	restartHelpersHook = func() restartHelpers {
		return restartHelpers{
			Systemctl: func(...string) error {
				t.Error("delegated drift restart must not use gc's systemd-managed branch")
				return nil
			},
			Launchctl: func(...string) error {
				t.Error("delegated drift restart must not use launchctl")
				return nil
			},
			Kill: func(int) error {
				t.Error("delegated drift restart must not SIGTERM the supervisor")
				return nil
			},
			WaitExit: func(int) error { return nil },
			Spawn: func(string, ...string) error {
				t.Error("delegated drift restart must not respawn the supervisor directly")
				return nil
			},
		}
	}

	var stdout, stderr bytes.Buffer
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("exitCode = %d, want 1 (supervisor was not replaced); stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if cont {
		t.Fatalf("cont = true after a no-op delegated restart; drift is unresolved and must be terminal")
	}
	lines := readRecordedSystemctlArgs(t, argsFile)
	if len(lines) != 1 || lines[0] != "try-restart gascity-prod.service" {
		t.Fatalf("systemctl invocations = %v, want exactly [%q]", lines, "try-restart gascity-prod.service")
	}
	if !strings.Contains(stdout.String(), "Restarting supervisor (systemd-delegated)") {
		t.Errorf("stdout = %q, want systemd-delegated restart mode line", stdout.String())
	}
	for _, want := range []string{"was not replaced", "gascity-prod.service", "old-build-id"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

// TestRunStartDriftCheck_DelegatedRestartReplacementSucceeds pins the
// delegated happy path: when try-restart actually replaces the
// supervisor (served build identity flips), the drift check reports
// ready and continues into normal registration.
func TestRunStartDriftCheck_DelegatedRestartReplacementSucceeds(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")

	// Serve the old build until the fake systemctl has run (argsFile
	// exists), then the new build — modeling a unit restart that swaps
	// the supervisor binary.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		build := "old-build-id"
		if _, err := os.Stat(argsFile); err == nil {
			build = "new-build-id"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":"v0","build_id":%q,"uptime_sec":1,"cities_total":0,"cities_running":0}`, build)
	}))
	t.Cleanup(srv.Close)
	oldURL := supervisorAPIBaseURLHook
	supervisorAPIBaseURLHook = func() (string, error) { return srv.URL, nil }
	t.Cleanup(func() { supervisorAPIBaseURLHook = oldURL })

	var stdout, stderr bytes.Buffer
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0; stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if !cont {
		t.Fatalf("cont = false after a successful delegated restart; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	lines := readRecordedSystemctlArgs(t, argsFile)
	if len(lines) != 1 || lines[0] != "try-restart gascity-prod.service" {
		t.Fatalf("systemctl invocations = %v, want exactly [%q]", lines, "try-restart gascity-prod.service")
	}
	if !strings.Contains(stdout.String(), " ready (") {
		t.Errorf("stdout = %q, want ready line after verified replacement", stdout.String())
	}
}

// TestRunStartDriftCheck_DelegatedRestartNewPIDStillDriftedFails pins the
// replaced-but-still-stale arm: the delegated unit genuinely restarts the
// supervisor (new PID) but its ExecStart still launches the drifted
// binary, so /health keeps serving the old build. Verification must fail
// — a PID change is replacement evidence, not drift resolution — or
// every later `gc start` re-detects the same drift and bounces the whole
// supervisor again.
func TestRunStartDriftCheck_DelegatedRestartNewPIDStillDriftedFails(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")
	// The replaced supervisor stays drifted, so the verification poll runs to
	// driftReadyTimeout before reporting the stale build; keep it short.
	shrinkDriftReadyTimeout(t)

	// Once the fake systemctl has run (argsFile exists), liveness reports
	// a new PID while driftCheckEnv's /health keeps serving old-build-id —
	// a real restart whose ExecStart points at a stale binary.
	basePID := os.Getpid()
	oldAlive := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = oldAlive })
	supervisorAliveHook = func() int {
		if _, err := os.Stat(argsFile); err == nil {
			return basePID + 1
		}
		return basePID
	}

	var stdout, stderr bytes.Buffer
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("exitCode = %d, want 1 (drift unresolved after replacement); stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if cont {
		t.Fatalf("cont = true after a still-drifted delegated restart; drift is unresolved and must be terminal")
	}
	lines := readRecordedSystemctlArgs(t, argsFile)
	if len(lines) != 1 || lines[0] != "try-restart gascity-prod.service" {
		t.Fatalf("systemctl invocations = %v, want exactly [%q]", lines, "try-restart gascity-prod.service")
	}
	for _, want := range []string{"still serves drifted build", "old-build-id", "new-build-id", "gascity-prod.service", "ExecStart"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

// TestRunStartDriftCheck_DelegatedRestartVerifyProbeFailureFails pins
// fail-closed verification: when the post-restart /health response cannot
// be decoded, the drift check must fail with a diagnostic instead of
// skipping verification and declaring "ready" — the fail-open would
// reproduce exactly the false success the verification exists to prevent.
func TestRunStartDriftCheck_DelegatedRestartVerifyProbeFailureFails(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")
	// The probe never decodes, so the verification poll runs to
	// driftReadyTimeout before reporting "cannot verify"; keep it short.
	shrinkDriftReadyTimeout(t)

	// Serve valid /health JSON until the fake systemctl has run, then a
	// 200 with an undecodable body: PollReady's Ping (status-code only)
	// still passes, while the verification Status decode fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := os.Stat(argsFile); err == nil {
			_, _ = io.WriteString(w, "not json")
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":"v0","build_id":"old-build-id","uptime_sec":1,"cities_total":0,"cities_running":0}`)
	}))
	t.Cleanup(srv.Close)
	oldURL := supervisorAPIBaseURLHook
	supervisorAPIBaseURLHook = func() (string, error) { return srv.URL, nil }
	t.Cleanup(func() { supervisorAPIBaseURLHook = oldURL })

	var stdout, stderr bytes.Buffer
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("exitCode = %d, want 1 (unverifiable restart); stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if cont {
		t.Fatalf("cont = true after an unverifiable delegated restart; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	for _, want := range []string{"cannot verify supervisor", "try-restart gascity-prod.service"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

// TestRunStartDriftCheck_KillSwitchGuidance pins the operator remediation
// text on the kill-switch arm: the default text references gc's own user
// unit via supervisorSystemdServiceName (suffixed under an isolated
// GC_HOME, as driftCheckEnv configures), and a configured
// GC_SUPERVISOR_SYSTEMD_UNIT/_SCOPE replaces it with the delegated
// unit's systemctl command.
func TestRunStartDriftCheck_KillSwitchGuidance(t *testing.T) {
	cases := []struct {
		name  string
		unit  string
		scope string
		want  string
	}{
		{
			name: "default guidance names gc's user unit",
			// want computed after env setup: the unit name depends on GC_HOME.
		},
		{
			name: "delegated system unit",
			unit: "gascity-prod.service",
			want: "Restart manually with 'systemctl restart gascity-prod.service'.",
		},
		{
			name:  "delegated user unit",
			unit:  "gascity-prod.service",
			scope: "user",
			want:  "Restart manually with 'systemctl --user restart gascity-prod.service'.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, setCommit := driftCheckEnv(t, "old-build-id")
			setCommit("new-build-id")

			oldDry, oldNoAR := dryRunMode, noAutoRestartMode
			dryRunMode, noAutoRestartMode = false, false
			t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

			setDelegationEnvForTest(t, tc.unit, tc.scope)

			want := tc.want
			if want == "" {
				want = fmt.Sprintf("Restart manually with 'systemctl --user restart %s'.", supervisorSystemdServiceName())
			}

			cityToml := "[workspace]\nname = \"drift-guidance\"\n\n[daemon]\nauto_restart_on_drift = false\n"
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
				t.Fatalf("writing city.toml: %v", err)
			}

			var stdout, stderr bytes.Buffer
			exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
			if exitCode != 1 {
				t.Fatalf("exitCode = %d, want 1 (kill switch); stderr=%q", exitCode, stderr.String())
			}
			if cont {
				t.Fatalf("cont = true on kill-switch drift; should be terminal")
			}
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q, want guidance %q", stderr.String(), want)
			}
		})
	}
}

// TestSupervisorGuidanceUsesServiceNameHelper pins that the
// non-delegated guidance fallback is built from
// supervisorSystemdServiceName, so hosts running the GC_HOME-suffixed
// gc-owned unit are pointed at their actual unit instead of the
// hardcoded default name.
func TestSupervisorGuidanceUsesServiceNameHelper(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(supervisorSystemdUnitEnv, "")
	t.Setenv(supervisorSystemdScopeEnv, "")

	service := supervisorSystemdServiceName()
	if service == defaultSupervisorSystemdUnit {
		t.Fatalf("isolated GC_HOME did not produce a suffixed unit name; got %q", service)
	}
	if got, want := supervisorRestartGuidance(), "systemctl --user restart "+service; got != want {
		t.Errorf("supervisorRestartGuidance() = %q, want %q", got, want)
	}
	if got, want := supervisorStatusGuidance(), "systemctl --user status "+service; got != want {
		t.Errorf("supervisorStatusGuidance() = %q, want %q", got, want)
	}
}

// TestRunStartDriftCheck_DelegatedTryRestartBoundsSystemctl pins the
// CLI-side budget on delegated `systemctl try-restart`: a wedged unit must
// not hold drift remediation (and with it `gc start`) past the job
// timeout, matching the bounded delegated stop and start. The bounded
// timeout is not terminal — it falls through to the post-restart
// verification poll — so a try-restart that never replaced the supervisor
// still fails with the "was not replaced" diagnostic instead of hanging.
func TestRunStartDriftCheck_DelegatedTryRestartBoundsSystemctl(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	setDelegationEnvForTest(t, "gascity-prod.service", "")
	installFakeDelegatedSystemctlHangingVerb(t, "try-restart")
	oldJob := delegatedSystemctlJobTimeout
	delegatedSystemctlJobTimeout = 300 * time.Millisecond
	t.Cleanup(func() { delegatedSystemctlJobTimeout = oldJob })
	// The bounded try-restart never replaces the supervisor, so the
	// verification poll runs to driftReadyTimeout before reporting "was not
	// replaced"; shrink it so this stays well under the elapsed bound below.
	shrinkDriftReadyTimeout(t)

	var stdout, stderr bytes.Buffer
	start := time.Now()
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	elapsed := time.Since(start)
	if exitCode != 1 || cont {
		t.Fatalf("(exitCode, cont) = (%d, %v), want (1, false); stderr=%q", exitCode, cont, stderr.String())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("delegated try-restart took %s; the job timeout did not bound the systemctl invocation", elapsed)
	}
	if !strings.Contains(stderr.String(), "was not replaced") {
		t.Errorf("stderr = %q, want 'was not replaced' after the bounded timeout fell through to drift verification", stderr.String())
	}
}

// TestRunStartDriftCheck_DelegatedTryRestartTimeoutThenReplacementSucceeds
// pins the bounded-wait fix for drift remediation: a delegated
// `systemctl try-restart` that exceeds the CLI budget but whose unit
// replaces the supervisor only *after* the bounded wait must be observed by
// the verification poll, not a single early probe. The fake keeps serving
// the old build for several post-timeout probes before flipping to the new
// build — the late replacement point — so a verify-once implementation
// would sample an early old-build probe and misreport "was not replaced",
// while the poll retries past them, observes the replacement, and reports
// ready.
func TestRunStartDriftCheck_DelegatedTryRestartTimeoutThenReplacementSucceeds(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	setDelegationEnvForTest(t, "gascity-prod.service", "")
	argsFile := installFakeDelegatedSystemctlHangingVerb(t, "try-restart")
	oldJob := delegatedSystemctlJobTimeout
	delegatedSystemctlJobTimeout = 300 * time.Millisecond
	t.Cleanup(func() { delegatedSystemctlJobTimeout = oldJob })

	// Model a unit that replaces the supervisor binary only after the CLI's
	// bounded try-restart wait has elapsed: once the fake systemctl has run
	// (argsFile exists), keep serving the OLD build for the first few
	// verification probes, then flip to the new build at the late
	// replacement point. A verify-once implementation would sample an early
	// old-build probe and misreport "was not replaced"; the poll must retry
	// past them to the replacement.
	const oldBuildProbesBeforeReplace = 3
	var postTimeoutProbes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		build := "old-build-id"
		if _, err := os.Stat(argsFile); err == nil {
			if postTimeoutProbes.Add(1) > oldBuildProbesBeforeReplace {
				build = "new-build-id"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":"v0","build_id":%q,"uptime_sec":1,"cities_total":0,"cities_running":0}`, build)
	}))
	t.Cleanup(srv.Close)
	oldURL := supervisorAPIBaseURLHook
	supervisorAPIBaseURLHook = func() (string, error) { return srv.URL, nil }
	t.Cleanup(func() { supervisorAPIBaseURLHook = oldURL })

	var stdout, stderr bytes.Buffer
	start := time.Now()
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	elapsed := time.Since(start)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0; stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if !cont {
		t.Fatalf("cont = false after a verified late replacement; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if postTimeoutProbes.Load() <= oldBuildProbesBeforeReplace {
		t.Fatalf("verification made %d post-timeout probes; want > %d (the poll must retry past the early old-build probes to the late replacement)", postTimeoutProbes.Load(), oldBuildProbesBeforeReplace)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("delegated try-restart took %s; the job timeout did not bound the systemctl invocation", elapsed)
	}
	if !strings.Contains(stdout.String(), " ready (") {
		t.Errorf("stdout = %q, want ready line after verified late replacement", stdout.String())
	}
}

// TestRunStartDriftCheck_KillSwitchGuidanceNamesInvalidScope pins the
// no-silent-fallback invariant on the kill-switch arm: a scope typo must
// surface the bad env value in the guidance instead of silently naming
// gc's default user unit.
func TestRunStartDriftCheck_KillSwitchGuidanceNamesInvalidScope(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	setDelegationEnvForTest(t, "gascity-prod.service", "systme")

	cityToml := "[workspace]\nname = \"drift-guidance\"\n\n[daemon]\nauto_restart_on_drift = false\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	if exitCode != 1 || cont {
		t.Fatalf("(exitCode, cont) = (%d, %v), want (1, false); stderr=%q", exitCode, cont, stderr.String())
	}
	if !strings.Contains(stderr.String(), `invalid GC_SUPERVISOR_SYSTEMD_SCOPE="systme"`) {
		t.Errorf("stderr = %q, want invalid scope value named in guidance", stderr.String())
	}
	if strings.Contains(stderr.String(), "gascity-supervisor") {
		t.Errorf("stderr = %q, must not silently fall back to the default unit text", stderr.String())
	}
}

// TestRunStartDriftCheck_NoAutoRestartGuidanceUsesDelegatedUnit pins the
// --no-auto-restart remediation text under delegation.
func TestRunStartDriftCheck_NoAutoRestartGuidanceUsesDelegatedUnit(t *testing.T) {
	cityPath, setCommit := driftCheckEnv(t, "old-build-id")
	setCommit("new-build-id")

	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, true
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	setDelegationEnvForTest(t, "gascity-prod.service", "")

	var stdout, stderr bytes.Buffer
	exitCode, cont := runStartDriftCheck(cityPath, &stdout, &stderr)
	if exitCode != 1 || cont {
		t.Fatalf("(exitCode, cont) = (%d, %v), want (1, false); stderr=%q", exitCode, cont, stderr.String())
	}
	want := "rerun 'gc start' (or 'systemctl restart gascity-prod.service') to apply changes."
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want guidance %q", stderr.String(), want)
	}
}
