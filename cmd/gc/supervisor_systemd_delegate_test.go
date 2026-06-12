package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
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

// installFakeDelegatedSystemctlHangingStop installs a shim whose `stop`
// invocation hangs (exec sleep) so tests can prove the CLI bounds the
// systemctl wait. is-active probes report active; other verbs succeed.
func installFakeDelegatedSystemctlHangingStop(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "systemctl-args")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\ncase \" $* \" in *\" is-active \"*) exit 0 ;; *\" stop \"*) exec sleep 5 ;; esac\nexit 0\n", argsFile)
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake systemctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
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
			t.Setenv(supervisorSystemdUnitEnv, tc.unit)
			t.Setenv(supervisorSystemdScopeEnv, tc.scope)
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
			t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
			t.Setenv(supervisorSystemdScopeEnv, tc.scope)
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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

func TestSupervisorStartDelegationInvalidScopeFails(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
	t.Setenv(supervisorSystemdScopeEnv, "remote")

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
			t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
			t.Setenv(supervisorSystemdScopeEnv, tc.scope)
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
	installFakeDelegatedSystemctlHangingStop(t)

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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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
// status command, not gc's fork-mode log file.
func TestEnsureSupervisorRunningDelegatedTimeoutPointsAtUnit(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
	installFakeDelegatedSystemctl(t, 0, "")

	old := supervisorAliveHook
	t.Cleanup(func() { supervisorAliveHook = old })
	supervisorAliveHook = func() int { return 0 }
	oldTimeout := supervisorReadyTimeout
	supervisorReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { supervisorReadyTimeout = oldTimeout })

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
			t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
			t.Setenv(supervisorSystemdScopeEnv, tc.scope)
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
		t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
		t.Setenv(supervisorSystemdScopeEnv, "systme")
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")

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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
	t.Setenv(supervisorSystemdScopeEnv, "remote")

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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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
	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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

	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")

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

	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
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

	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")

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

	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
	argsFile := installFakeDelegatedSystemctl(t, 0, "")

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
// unit, and a configured GC_SUPERVISOR_SYSTEMD_UNIT/_SCOPE replaces it
// with the delegated unit's systemctl command.
func TestRunStartDriftCheck_KillSwitchGuidance(t *testing.T) {
	cases := []struct {
		name  string
		unit  string
		scope string
		want  string
	}{
		{
			name: "default guidance names gc's user unit",
			want: "Restart manually with 'systemctl --user restart gascity-supervisor'.",
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

			t.Setenv(supervisorSystemdUnitEnv, tc.unit)
			t.Setenv(supervisorSystemdScopeEnv, tc.scope)

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
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want guidance %q", stderr.String(), tc.want)
			}
		})
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

	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
	t.Setenv(supervisorSystemdScopeEnv, "systme")

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

	t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")

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
