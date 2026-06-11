package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFakeDelegatedSystemctl writes an executable `systemctl` shim into a fresh
// temp dir, prepends that dir to PATH, and returns the path of the file
// the shim appends its argv into (one line per invocation). The shim
// prints stderrMsg to stderr (when non-empty) and exits with exitCode, so
// tests can model both healthy and failing systemctl runs without a real
// systemd anywhere near the test.
func installFakeDelegatedSystemctl(t *testing.T, exitCode int, stderrMsg string) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "systemctl-args")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\n", argsFile)
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
		name     string
		scope    string
		wantArgs string
	}{
		{name: "system scope", scope: "", wantArgs: "stop gascity-prod.service"},
		{name: "user scope", scope: "user", wantArgs: "--user stop gascity-prod.service"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh GC_HOME with no supervisor socket: the legacy stop path
			// would fail with "supervisor is not running"; the delegated
			// path must not touch the control socket at all.
			t.Setenv("GC_HOME", t.TempDir())
			t.Setenv(supervisorSystemdUnitEnv, "gascity-prod.service")
			t.Setenv(supervisorSystemdScopeEnv, tc.scope)
			argsFile := installFakeDelegatedSystemctl(t, 0, "")

			var stdout, stderr bytes.Buffer
			if code := stopSupervisorWithWait(&stdout, &stderr, false, 0); code != 0 {
				t.Fatalf("stopSupervisorWithWait code = %d, want 0; stderr=%q", code, stderr.String())
			}
			lines := readRecordedSystemctlArgs(t, argsFile)
			if len(lines) != 1 || lines[0] != tc.wantArgs {
				t.Fatalf("systemctl invocations = %v, want exactly [%q]", lines, tc.wantArgs)
			}
			if !strings.Contains(stdout.String(), "Supervisor stopped.") {
				t.Errorf("stdout = %q, want %q", stdout.String(), "Supervisor stopped.")
			}
		})
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

// TestRunStartDriftCheck_DelegatedRestartUsesTryRestart pins the drift
// auto-restart path under GC_SUPERVISOR_SYSTEMD_UNIT: the restart is a
// single `systemctl try-restart <unit>` and none of gc's own restart
// machinery (user-unit systemctl, launchctl, SIGTERM+respawn) fires.
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
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0; stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())
	}
	if !cont {
		t.Fatalf("cont = false after delegated restart; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	lines := readRecordedSystemctlArgs(t, argsFile)
	if len(lines) != 1 || lines[0] != "try-restart gascity-prod.service" {
		t.Fatalf("systemctl invocations = %v, want exactly [%q]", lines, "try-restart gascity-prod.service")
	}
	if !strings.Contains(stdout.String(), "Restarting supervisor (systemd-delegated)") {
		t.Errorf("stdout = %q, want systemd-delegated restart mode line", stdout.String())
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
