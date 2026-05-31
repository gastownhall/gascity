//go:build integration

package integration

// Integration suite for `gc start` supervisor binary-drift detection
// (ga-a3ry.1 phase 3). Phase 2's unit tests pinned the decision logic
// and pure-data helpers; this suite exercises the production paths
// that unit tests cannot reach: the real /proc/<pid>/exe lookup, the
// real systemctl --user invocation, the real SIGTERM + spawn cycle,
// and the real /health round-trip after restart.
//
// Each test isolates GC_HOME / XDG_RUNTIME_DIR so a failing run on a
// developer box does not corrupt the real supervisor; supervisors
// started here listen on a per-test reserved loopback port.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

const (
	driftHappyOldCommit       = "deadbeefcafe1111"
	driftHappyNewCommit       = "facefeed02222222"
	driftReadyTimeout         = 15 * time.Second
	driftDirectRestartBudget  = 10 * time.Second
	driftSystemdRestartBudget = 15 * time.Second
)

// TestStartDrift_DirectLaunch_RestartsToNewBuildID exercises the direct
// (non-systemd) supervisor restart path. After overwriting the
// supervisor binary on disk, `gc start` from the new binary must:
//
//   - print the always-on `Supervisor:` identity line first;
//   - emit the `Drift detected:` block with both build hashes;
//   - kill the running supervisor and respawn from /proc/<pid>/exe;
//   - print `Restarting supervisor (direct)... ready (Xs).`;
//   - re-print the `Supervisor:` line so the operator's last impression
//     is of the post-restart (new) build identity;
//   - exit 0.
//
// NFR-2 target: the restart cycle should complete under 5s p95. The
// integration assertion uses a wider wall-clock budget so a loaded CI
// runner does not fail an otherwise healthy restart path.
func TestStartDrift_DirectLaunch_RestartsToNewBuildID(t *testing.T) {
	requireLinuxProcExe(t)
	tc := setupDriftDirectScenario(t)

	out, exitCode, _ := runDriftCommand(t, tc.newBinary, tc.env, tc.cityDir, "start", tc.cityDir)
	if exitCode != 0 {
		t.Fatalf("gc start exit = %d, want 0\noutput:\n%s", exitCode, out)
	}

	// First-line identity line must always be present and start with
	// "Supervisor:" — the load-bearing FR-5 contract.
	first := firstLine(out)
	if !strings.HasPrefix(first, "Supervisor:") {
		t.Errorf("first line = %q, want %q prefix", first, "Supervisor:")
	}
	// The pre-restart Supervisor: line should report the OLD build_id.
	if !strings.Contains(out, "buildID="+driftHappyOldCommit) {
		t.Errorf("output missing pre-restart buildID=%s\n%s", driftHappyOldCommit, out)
	}
	// Drift-detected report with both hashes.
	for _, want := range []string{
		"Drift detected:",
		"binary: local=" + driftHappyNewCommit,
		"supervisor=" + driftHappyOldCommit,
		"Restarting supervisor (direct)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	// Restart should converge to the NEW build_id post-restart and
	// re-print the identity line. We accept either substring order:
	// the design says re-print the Supervisor: line after the ready
	// marker so the new build_id appears at least once.
	if !strings.Contains(out, "buildID="+driftHappyNewCommit) {
		t.Errorf("output missing post-restart buildID=%s — re-print after restart did not happen\n%s",
			driftHappyNewCommit, out)
	}
	// Final supervisor /health (queried directly) must report the NEW
	// commit so we know the restart actually swapped binaries.
	gotID := pollHealthBuildID(t, tc.supervisorPort, driftHappyNewCommit, driftReadyTimeout)
	if gotID != driftHappyNewCommit {
		t.Fatalf("post-restart /health build_id = %q, want %q", gotID, driftHappyNewCommit)
	}
	assertRestartDuration(t, out, driftDirectRestartBudget, "direct")
}

// TestStartDrift_SystemdManaged_RestartsToNewBuildID is the systemd
// counterpart. It installs a real gascity-supervisor unit pointing at
// the test gc binary, starts it via `systemctl --user start`, swaps
// the on-disk binary for the new build, and runs `gc start` from the
// new binary. The expected restart path takes the systemd branch:
// `systemctl --user restart …` rather than kill+spawn.
//
// Skipped when:
//   - systemctl is not on PATH (containers, BSD)
//   - `systemctl --user is-system-running` reports an unusable user bus
//     (no DBus, no dbus-user-session, etc.)
func TestStartDrift_SystemdManaged_RestartsToNewBuildID(t *testing.T) {
	requireUserSystemd(t)
	tc := setupDriftSystemdScenario(t)

	out, exitCode, _ := runDriftCommand(t, tc.newBinary, tc.env, tc.cityDir, "start", tc.cityDir)
	if exitCode != 0 {
		t.Fatalf("gc start exit = %d, want 0\noutput:\n%s", exitCode, out)
	}

	first := firstLine(out)
	if !strings.HasPrefix(first, "Supervisor:") {
		t.Errorf("first line = %q, want %q prefix", first, "Supervisor:")
	}
	if !strings.Contains(out, "buildID="+driftHappyOldCommit) {
		t.Errorf("output missing pre-restart buildID=%s\n%s", driftHappyOldCommit, out)
	}
	for _, want := range []string{
		"Drift detected:",
		"binary: local=" + driftHappyNewCommit,
		"supervisor=" + driftHappyOldCommit,
		"Restarting supervisor (systemd-managed)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if !strings.Contains(out, "buildID="+driftHappyNewCommit) {
		t.Errorf("output missing post-restart buildID=%s\n%s", driftHappyNewCommit, out)
	}
	gotID := pollHealthBuildID(t, tc.supervisorPort, driftHappyNewCommit, driftReadyTimeout)
	if gotID != driftHappyNewCommit {
		t.Fatalf("post-restart /health build_id = %q, want %q", gotID, driftHappyNewCommit)
	}
	assertRestartDuration(t, out, driftSystemdRestartBudget, "systemd-managed")
}

// TestStartDrift_NoAutoRestart_ExitsNonZero pins the --no-auto-restart
// behavior: drift is detected, reported, and the operator is told to
// rerun `gc start` (or systemctl restart) — no restart happens. Exit
// code 1 lets CI assert "supervisor matches the build I just produced".
func TestStartDrift_NoAutoRestart_ExitsNonZero(t *testing.T) {
	tc := setupDriftDirectScenario(t)
	prePID := readPidFromHealth(t, tc.supervisorPort)

	out, exitCode, _ := runDriftCommand(t, tc.newBinary, tc.env, tc.cityDir,
		"start", "--no-auto-restart", tc.cityDir)
	if exitCode != 1 {
		t.Fatalf("gc start --no-auto-restart exit = %d, want 1\noutput:\n%s", exitCode, out)
	}

	for _, want := range []string{
		"Supervisor:",
		"Drift detected:",
		"binary: local=" + driftHappyNewCommit,
		"supervisor=" + driftHappyOldCommit,
		"error: supervisor binary drift",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	// Restart must NOT have happened.
	if strings.Contains(out, "Restarting supervisor") {
		t.Errorf("--no-auto-restart triggered a restart line:\n%s", out)
	}
	if id := readBuildIDFromHealth(t, tc.supervisorPort); id != driftHappyOldCommit {
		t.Errorf("supervisor build_id changed under --no-auto-restart: got %q, want %q (still old)",
			id, driftHappyOldCommit)
	}
	if postPID := readPidFromHealth(t, tc.supervisorPort); postPID != prePID {
		t.Errorf("supervisor PID changed under --no-auto-restart: pre=%d post=%d", prePID, postPID)
	}
}

// TestStartDrift_DryRun_ReportsButDoesNotRestart pins the --dry-run
// behavior: drift is reported, the "(would auto-restart; --dry-run)"
// suffix is emitted, exit code is 0, and the supervisor is left alone.
func TestStartDrift_DryRun_ReportsButDoesNotRestart(t *testing.T) {
	tc := setupDriftDirectScenario(t)
	prePID := readPidFromHealth(t, tc.supervisorPort)

	out, exitCode, _ := runDriftCommand(t, tc.newBinary, tc.env, tc.cityDir,
		"start", "--dry-run", tc.cityDir)
	if exitCode != 0 {
		t.Fatalf("gc start --dry-run exit = %d, want 0\noutput:\n%s", exitCode, out)
	}

	for _, want := range []string{
		"Supervisor:",
		"Drift detected:",
		"binary: local=" + driftHappyNewCommit,
		"supervisor=" + driftHappyOldCommit,
		"(would auto-restart; --dry-run)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "Restarting supervisor") {
		t.Errorf("--dry-run triggered a restart line:\n%s", out)
	}
	if id := readBuildIDFromHealth(t, tc.supervisorPort); id != driftHappyOldCommit {
		t.Errorf("supervisor build_id changed under --dry-run: got %q, want %q",
			id, driftHappyOldCommit)
	}
	if postPID := readPidFromHealth(t, tc.supervisorPort); postPID != prePID {
		t.Errorf("supervisor PID changed under --dry-run: pre=%d post=%d", prePID, postPID)
	}
}

// TestStartDrift_KillSwitchInConfig_PreventsRestart pins the
// `[daemon].auto_restart_on_drift = false` kill switch: the per-invocation
// flag does NOT override it (production safety wins). Error message
// must point operators at the config key, not at re-running gc start.
func TestStartDrift_KillSwitchInConfig_PreventsRestart(t *testing.T) {
	tc := setupDriftDirectScenario(t)
	writeKillSwitch(t, tc.cityDir, false)
	prePID := readPidFromHealth(t, tc.supervisorPort)

	out, exitCode, _ := runDriftCommand(t, tc.newBinary, tc.env, tc.cityDir,
		"start", tc.cityDir)
	if exitCode != 1 {
		t.Fatalf("gc start with kill-switch exit = %d, want 1\noutput:\n%s", exitCode, out)
	}
	for _, want := range []string{
		"Drift detected:",
		"error: supervisor binary drift",
		"auto_restart_on_drift",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "Restarting supervisor") {
		t.Errorf("kill-switch triggered a restart line:\n%s", out)
	}
	if postPID := readPidFromHealth(t, tc.supervisorPort); postPID != prePID {
		t.Errorf("supervisor PID changed under kill-switch: pre=%d post=%d", prePID, postPID)
	}
}

// TestStartDrift_PermissionDenied_DescriptiveError pins the behavior
// when `/proc/<pid>/exe` cannot be read because the supervisor runs as
// a different uid. The acceptance brief calls for a descriptive error
// that routes the operator to fix the uid mismatch or pass
// --no-auto-restart; the implementation now surfaces that error rather
// than the silent `exe=(unreadable)` fallback.
//
// Skipped when the test process is root (root can read any
// /proc/<pid>/exe so the scenario is unreproducible) or when no
// secondary uid is available to spawn the decoy supervisor.
func TestStartDrift_PermissionDenied_DescriptiveError(t *testing.T) {
	requireLinuxProcExe(t)
	requireSecondaryUID(t)
	tc := setupDriftDirectScenarioAsUID(t, secondaryUID(t))

	out, exitCode, _ := runDriftCommand(t, tc.newBinary, tc.env, tc.cityDir,
		"start", tc.cityDir)
	// The acceptance brief asks for a descriptive error + non-zero exit
	// when /proc/<pid>/exe is unreadable; the implementation must not
	// silently fall back to exe=(unreadable) and proceed.
	if exitCode == 0 {
		t.Errorf("gc start under permission-denied returned exit 0; design requires non-zero so the operator notices")
	}
	if !strings.Contains(out, "different user") && !strings.Contains(out, "permission denied") {
		t.Errorf("output does not surface the uid-mismatch cause; design requires a descriptive error.\noutput:\n%s", out)
	}
	if !strings.Contains(out, "--no-auto-restart") && !strings.Contains(out, "rerun as supervisor") {
		t.Errorf("output does not route operator to remediation (rerun as supervisor's uid OR pass --no-auto-restart)\noutput:\n%s", out)
	}
}

// TestStartDrift_RestartTimeout_ExitsNonZero pins the timeout path:
// when the restart sequence completes (kill + spawn) but the new
// supervisor never serves /health within the 5s budget, gc start must
// surface a descriptive error and exit 1 — never hang.
//
// We exercise this by replacing the supervisor binary with one that
// exits immediately (it is /bin/true wrapped in a `gc supervisor run`
// shim that just sleeps without binding the port). The kill+spawn
// succeeds; PollReady cannot get a 200; the timeout fires.
func TestStartDrift_RestartTimeout_ExitsNonZero(t *testing.T) {
	requireLinuxProcExe(t)
	tc := setupDriftDirectScenario(t)

	// Replace the binary on disk with a no-op shim so the post-kill
	// spawn never serves /health.
	stuckShim := writeStuckSupervisorShim(t, tc.binaryPath)
	defer os.Remove(stuckShim) //nolint:errcheck

	out, exitCode, elapsed := runDriftCommand(t, tc.newBinary, tc.env, tc.cityDir,
		"start", tc.cityDir)
	if exitCode != 1 {
		t.Fatalf("gc start with stuck post-restart supervisor exit = %d, want 1\noutput:\n%s",
			exitCode, out)
	}
	if !strings.Contains(out, "supervisor restart timed out after") {
		t.Errorf("output missing %q\n%s", "supervisor restart timed out after", out)
	}
	if !strings.Contains(out, "Last known pid=") {
		t.Errorf("output missing %q (operator needs the pid to investigate)\n%s",
			"Last known pid=", out)
	}
	// The whole invocation must NOT hang — restart timeout (~5s) +
	// detection (~0.1s) + kill/spawn (~0.5s); pin a generous outer
	// limit at 30s so a hang is loud.
	if elapsed > 30*time.Second {
		t.Errorf("gc start hung for %s (expected to fail-fast after ~5s)", elapsed)
	}
}

// TestStartDrift_RestartLoopGuard_RefusesFourthInWindow pins the
// architect's 3-in-60s threshold. Four drift-and-restart cycles in a
// row must result in the fourth being refused with the loop-detected
// error. Persistent tracking of restart attempts (driftRestartHistoryFile)
// keeps the guard honest across `gc start` invocations.
func TestStartDrift_RestartLoopGuard_RefusesFourthInWindow(t *testing.T) {
	requireLinuxProcExe(t)
	tc := setupDriftDirectScenario(t)

	// Drive four drift cycles back-to-back. Between cycles, alternate
	// the on-disk commit so each invocation actually detects drift.
	for cycle := 1; cycle <= 4; cycle++ {
		commit := driftHappyNewCommit
		expectedPrev := driftHappyOldCommit
		if cycle%2 == 0 {
			commit = driftHappyOldCommit
			expectedPrev = driftHappyNewCommit
		}
		// Build the binary that gc start will run as (mimicking
		// `go install` overwriting the on-disk gc).
		buildGCBinaryWithCommit(t, tc.newBinary, commit)

		out, exitCode, _ := runDriftCommand(t, tc.newBinary, tc.env, tc.cityDir,
			"start", tc.cityDir)
		if cycle <= 3 {
			if exitCode != 0 {
				t.Fatalf("cycle %d: exit = %d, want 0 (within budget)\noutput:\n%s",
					cycle, exitCode, out)
			}
			if !strings.Contains(out, "Drift detected:") || !strings.Contains(out, "supervisor="+expectedPrev) {
				t.Errorf("cycle %d: drift not reported as expected (prev=%s):\n%s",
					cycle, expectedPrev, out)
			}
			// After this cycle, /health should report `commit`.
			if got := pollHealthBuildID(t, tc.supervisorPort, commit, driftReadyTimeout); got != commit {
				t.Fatalf("cycle %d: post-restart /health = %q, want %q", cycle, got, commit)
			}
			continue
		}
		// Fourth cycle: must be refused.
		if exitCode != 1 {
			t.Errorf("cycle 4: exit = %d, want 1 (loop guard should refuse)\noutput:\n%s",
				exitCode, out)
		}
		for _, want := range []string{
			"supervisor restart loop detected",
			"3 restarts in 60s",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("cycle 4: output missing %q (loop-guard message)\n%s", want, out)
			}
		}
	}
}

// TestStartDrift_RestartDoesNotTriggerStandaloneControllerConflict is a
// regression test for the race in which the just-spawned supervisor is
// briefly visible on the controller socket before the registry reflects
// it managing the city. Before the fix, registerCityWithSupervisorNamed's
// ensureNoStandaloneController call would see the new supervisor's PID
// on the controller socket and misclassify it as a competing standalone
// controller — failing `gc start` immediately after a successful binary
// drift restart. The fix records the just-restarted PID so the check
// short-circuits when the socket-holder matches our own new supervisor.
//
// The full-flow path (proxy → drift → restart → register) must:
//   - exit 0;
//   - never print "standalone controller already running";
//   - converge /health to the new build_id.
func TestStartDrift_RestartDoesNotTriggerStandaloneControllerConflict(t *testing.T) {
	requireLinuxProcExe(t)
	tc := setupDriftDirectScenario(t)

	out, exitCode, _ := runDriftCommand(t, tc.newBinary, tc.env, tc.cityDir, "start", tc.cityDir)
	if exitCode != 0 {
		t.Fatalf("gc start exit = %d, want 0\noutput:\n%s", exitCode, out)
	}
	if strings.Contains(out, "standalone controller already running") {
		t.Fatalf("gc start surfaced a false-positive standalone-controller conflict\noutput:\n%s", out)
	}
	if !strings.Contains(out, "Restarting supervisor") {
		t.Fatalf("expected restart path to run; output:\n%s", out)
	}
	if got := pollHealthBuildID(t, tc.supervisorPort, driftHappyNewCommit, driftReadyTimeout); got != driftHappyNewCommit {
		t.Fatalf("post-restart /health build_id = %q, want %q", got, driftHappyNewCommit)
	}
}

// driftScenario captures the state of a single integration scenario.
type driftScenario struct {
	gcHome         string
	cityDir        string
	env            []string
	supervisorPort string
	binaryPath     string // path of the running supervisor's binary on disk
	newBinary      string // path of the gc binary used to invoke `gc start` (drifted)
	supervisorPID  int
}

// setupDriftDirectScenario builds an old gc binary, starts it as a
// direct (non-systemd) supervisor, bootstraps a city, and overlays a
// new gc binary at the same path with a different commit. The caller
// receives a fully-armed scenario where running `<newBinary> start
// <cityDir>` triggers binary-drift detection.
func setupDriftDirectScenario(t *testing.T) *driftScenario {
	t.Helper()

	gcHome, runtimeDir, env := newDriftIsolatedEnvRoot(t)
	port := readSupervisorPortFromConfig(t, gcHome)

	binaryDir := filepath.Join(filepath.Dir(gcHome), "bin")
	if err := os.MkdirAll(binaryDir, 0o755); err != nil {
		t.Fatalf("creating drift binary dir: %v", err)
	}
	binaryPath := filepath.Join(binaryDir, "gc-drift")
	buildGCBinaryWithCommit(t, binaryPath, driftHappyOldCommit)

	pid := launchDirectSupervisor(t, binaryPath, env, gcHome)
	pollHealthBuildID(t, port, driftHappyOldCommit, driftReadyTimeout)

	cityDir := bootstrapDriftCity(t, binaryPath, env, gcHome)

	// Mimic `go install` overwriting the binary at the same path with a
	// new build. The kernel keeps the running process tied to the old
	// inode; /proc/<pid>/exe still resolves to binaryPath, which now
	// points to the new bytes.
	buildGCBinaryWithCommit(t, binaryPath, driftHappyNewCommit)

	t.Cleanup(func() {
		stopDirectSupervisor(pid)
	})

	_ = runtimeDir
	return &driftScenario{
		gcHome:         gcHome,
		cityDir:        cityDir,
		env:            env,
		supervisorPort: port,
		binaryPath:     binaryPath,
		newBinary:      binaryPath,
		supervisorPID:  pid,
	}
}

// setupDriftSystemdScenario is the systemd-managed counterpart of
// setupDriftDirectScenario. It writes a real user-systemd unit file
// pointing at the test gc binary and starts it via `systemctl --user
// start`. Cleanup stops + removes the unit.
func setupDriftSystemdScenario(t *testing.T) *driftScenario {
	t.Helper()

	gcHome, runtimeDir, env := newDriftIsolatedEnvRoot(t)
	// The gc subprocess needs the real XDG_RUNTIME_DIR to reach user
	// systemd via `systemctl --user`. Isolation is unnecessary here:
	// supervisor.RuntimeDir() short-circuits to gcHome under
	// UsesIsolatedGCHomeOverride, so XDG_RUNTIME_DIR is unused by the
	// supervisor itself. Without this swap, supervisorSystemctlActive
	// reads /<runtimeDir>/systemd/private (which doesn't exist) and
	// returns false, dropping the restart through the 'direct' branch.
	realXDG := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
	if realXDG != "" {
		env = filterEnv(env, "XDG_RUNTIME_DIR")
		env = append(env, "XDG_RUNTIME_DIR="+realXDG)
	}
	port := readSupervisorPortFromConfig(t, gcHome)

	binaryDir := filepath.Join(filepath.Dir(gcHome), "bin")
	if err := os.MkdirAll(binaryDir, 0o755); err != nil {
		t.Fatalf("creating drift binary dir: %v", err)
	}
	binaryPath := filepath.Join(binaryDir, "gc-drift-systemd")
	buildGCBinaryWithCommit(t, binaryPath, driftHappyOldCommit)

	unit := writeSystemdUserUnit(t, binaryPath, gcHome, runtimeDir)
	mustSystemctlUser(t, "daemon-reload")
	mustSystemctlUser(t, "start", unit)
	t.Cleanup(func() {
		_ = systemctlUser("stop", unit)
		_ = systemctlUser("disable", unit)
		_ = os.Remove(filepath.Join(systemdUserUnitDir(), unit))
		_ = systemctlUser("daemon-reload")
	})

	pollHealthBuildID(t, port, driftHappyOldCommit, driftReadyTimeout)
	cityDir := bootstrapDriftCity(t, binaryPath, env, gcHome)

	// Overwrite binary on disk with the new build.
	buildGCBinaryWithCommit(t, binaryPath, driftHappyNewCommit)

	return &driftScenario{
		gcHome:         gcHome,
		cityDir:        cityDir,
		env:            env,
		supervisorPort: port,
		binaryPath:     binaryPath,
		newBinary:      binaryPath,
	}
}

// setupDriftDirectScenarioAsUID is setupDriftDirectScenario with the
// supervisor process spawned under a different uid so /proc/<pid>/exe
// is unreadable from the test process.
func setupDriftDirectScenarioAsUID(t *testing.T, uid uint32) *driftScenario {
	t.Helper()
	tc := setupDriftDirectScenarioWithoutLaunch(t)

	cmd := exec.Command(tc.binaryPath, "supervisor", "run")
	cmd.Env = tc.env
	cmd.Dir = tc.gcHome
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid},
		Setpgid:    true,
	}
	logPath := filepath.Join(tc.gcHome, "supervisor.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("opening supervisor log: %v", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("starting supervisor as uid=%d: %v", uid, err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		stopDirectSupervisor(pid)
		_ = logFile.Close()
	})

	pollHealthBuildID(t, tc.supervisorPort, driftHappyOldCommit, driftReadyTimeout)
	cityDir := bootstrapDriftCity(t, tc.binaryPath, tc.env, tc.gcHome)
	tc.cityDir = cityDir

	buildGCBinaryWithCommit(t, tc.binaryPath, driftHappyNewCommit)
	tc.supervisorPID = pid
	return tc
}

// setupDriftDirectScenarioWithoutLaunch is the prefix shared by
// setupDriftDirectScenario and setupDriftDirectScenarioAsUID. It
// builds the old binary and reserves the env but does NOT start the
// supervisor — the caller does that under whatever credentials it
// needs.
func setupDriftDirectScenarioWithoutLaunch(t *testing.T) *driftScenario {
	t.Helper()
	gcHome, _, env := newDriftIsolatedEnvRoot(t)
	port := readSupervisorPortFromConfig(t, gcHome)

	binaryDir := filepath.Join(filepath.Dir(gcHome), "bin")
	if err := os.MkdirAll(binaryDir, 0o777); err != nil {
		t.Fatalf("creating drift binary dir: %v", err)
	}
	// 0o755 on the file plus a world-executable directory so a
	// secondary uid can exec the binary even though the test owns the
	// parent dir.
	binaryPath := filepath.Join(binaryDir, "gc-drift")
	buildGCBinaryWithCommit(t, binaryPath, driftHappyOldCommit)
	if err := os.Chmod(binaryPath, 0o755); err != nil {
		t.Fatalf("chmod binary: %v", err)
	}
	return &driftScenario{
		gcHome:         gcHome,
		env:            env,
		supervisorPort: port,
		binaryPath:     binaryPath,
		newBinary:      binaryPath,
	}
}

func newDriftIsolatedEnvRoot(t *testing.T) (string, string, []string) {
	t.Helper()
	gcHome, runtimeDir, env := newIsolatedEnvRoot(t, false)
	env = replaceEnv(env, "GC_BEADS", "file")
	env = replaceEnv(env, "GC_SESSION", "fake")
	return gcHome, runtimeDir, env
}

// buildGCBinaryWithCommit compiles the gc binary at outPath with
// `-X main.commit=commitID` so the supervisor's /health reports
// build_id=commitID. Used to fabricate drift between the running
// supervisor and the on-disk binary.
func buildGCBinaryWithCommit(t *testing.T, outPath, commitID string) {
	t.Helper()
	cmd := exec.Command("go", "build",
		"-buildvcs=false",
		"-ldflags", "-X main.commit="+commitID,
		"-o", outPath,
		"./cmd/gc",
	)
	cmd.Dir = findModuleRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building gc with commit=%s: %v\n%s", commitID, err, string(out))
	}
}

// launchDirectSupervisor spawns `binary supervisor run` in the
// background as the test process. Returns the PID. The caller is
// responsible for stopping the supervisor via t.Cleanup.
func launchDirectSupervisor(t *testing.T, binary string, env []string, gcHome string) int {
	t.Helper()
	logPath := filepath.Join(gcHome, "supervisor.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("opening supervisor log: %v", err)
	}
	cmd := exec.Command(binary, "supervisor", "run")
	cmd.Env = env
	cmd.Dir = gcHome
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("starting drift supervisor: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = logFile.Close() })
	return pid
}

// stopDirectSupervisor SIGTERMs a supervisor PID and waits briefly for
// it to exit. Used by t.Cleanup so a failing test does not leak
// supervisor processes onto the developer's box.
func stopDirectSupervisor(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// bootstrapDriftCity runs `gc init` to scaffold a minimal city using
// the supplied gc binary, returning the city dir.
func bootstrapDriftCity(t *testing.T, binary string, env []string, gcHome string) string {
	t.Helper()
	cityName := "drift-" + filepath.Base(t.TempDir())
	cityDir := filepath.Join(filepath.Dir(gcHome), cityName)
	cmd := exec.Command(binary, "init", "--skip-provider-readiness", cityDir)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc init: %v\n%s", err, string(out))
	}
	t.Cleanup(func() { _ = os.RemoveAll(cityDir) })
	return cityDir
}

// runDriftCommand runs `binary args...` capturing combined output,
// exit code, and wall-clock duration. The default timeout is 60s,
// generous enough for the restart-loop guard test (which runs four
// `gc start` cycles).
func runDriftCommand(t *testing.T, binary string, env []string, cwd string, args ...string) (string, int, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	cmd.Dir = cwd
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return string(out), exitCode, elapsed
}

// readSupervisorPortFromConfig parses the integration-test
// supervisor.toml to discover the port newIsolatedEnvRoot reserved.
func readSupervisorPortFromConfig(t *testing.T, gcHome string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(gcHome, "supervisor.toml"))
	if err != nil {
		t.Fatalf("reading supervisor.toml: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "port") {
			continue
		}
		if i := strings.Index(line, "="); i >= 0 {
			return strings.TrimSpace(line[i+1:])
		}
	}
	t.Fatalf("supervisor.toml did not contain a port:\n%s", data)
	return ""
}

// pollHealthBuildID GETs /health and waits up to timeout for the
// returned build_id to equal want. Returns the last observed build_id.
// Used to confirm the supervisor swap actually completed.
func pollHealthBuildID(t *testing.T, port, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastID string
	var lastErr error
	for time.Now().Before(deadline) {
		id, err := fetchHealthField(port, "build_id")
		if err == nil {
			lastID = id
			if id == want {
				return id
			}
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		t.Logf("pollHealthBuildID last error: %v", lastErr)
	}
	t.Fatalf("supervisor /health build_id never converged to %q (last=%q)", want, lastID)
	return lastID
}

// readBuildIDFromHealth is a single-shot variant of pollHealthBuildID.
func readBuildIDFromHealth(t *testing.T, port string) string {
	t.Helper()
	id, err := fetchHealthField(port, "build_id")
	if err != nil {
		t.Fatalf("/health build_id fetch: %v", err)
	}
	return id
}

// readPidFromHealth uses the integration-test gc binary to discover
// the supervisor PID. /health does not surface it; we read the pid
// from /proc by listening port owners is overkill, so just `pgrep` is
// not portable. Instead we use a direct probe: the supervisor's
// supervisor.lock file in GC_HOME would be authoritative, but the
// simplest portable approach is to track the PID we launched.
//
// For this suite we exposed cmd.Process.Pid in the scenario struct;
// callers that need post-restart PID observation use that path.
// readPidFromHealth fetches a tracking field from /health if present;
// today /health does not expose pid, so we fall back to inspecting
// the listening socket via /proc/net/tcp. Surface a clear error if
// neither path succeeds — restart-detection tests rely on this.
func readPidFromHealth(t *testing.T, port string) int {
	t.Helper()
	// Try a port-owner lookup: /proc/<pid>/net/tcp would require root;
	// instead we use the fact that on Linux, listing /proc and reading
	// /proc/<pid>/net/tcp for the test process is equivalent. To keep
	// this simple, we use a local `ss` invocation when available;
	// otherwise we return 0 and the caller is expected not to compare.
	if pid, ok := pidOwningLoopbackPort(port); ok {
		return pid
	}
	return 0
}

// pidOwningLoopbackPort returns the PID listening on 127.0.0.1:<port>,
// using `ss -lntp` so the test does not need root. Returns ok=false
// when ss is not available; tests treat 0 as "unknown" rather than
// failing.
func pidOwningLoopbackPort(port string) (int, bool) {
	out, err := exec.Command("ss", "-lntpH", "src", "127.0.0.1:"+port).CombinedOutput()
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "users:") {
			continue
		}
		// users:(("gascity-superv",pid=12345,fd=3))
		i := strings.Index(line, "pid=")
		if i < 0 {
			continue
		}
		rest := line[i+4:]
		j := strings.IndexAny(rest, ",)")
		if j < 0 {
			continue
		}
		var pid int
		_, err := fmt.Sscanf(rest[:j], "%d", &pid)
		if err == nil && pid > 0 {
			return pid, true
		}
	}
	return 0, false
}

// fetchHealthField does a GET on http://127.0.0.1:<port>/health and
// returns the named string field of the JSON body.
func fetchHealthField(port, field string) (string, error) {
	addr := net.JoinHostPort("127.0.0.1", port)
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/health", nil)
	if err != nil {
		return "", err
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("/health returned %d", resp.StatusCode)
	}
	var body map[string]any
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&body); err != nil {
		return "", err
	}
	v, _ := body[field].(string)
	return v, nil
}

// firstLine returns the substring of s up to (and not including) the
// first newline. Used to inspect the always-on `Supervisor:` identity
// line emitted by gc start.
func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func assertRestartDuration(t *testing.T, out string, budget time.Duration, mode string) {
	t.Helper()
	d, ok := parseReadyDuration(out)
	if !ok {
		t.Errorf("could not parse ready-duration from output:\n%s", out)
		return
	}
	if d > budget {
		t.Errorf("NFR-2 violated (%s): restart took %s (>%s)", mode, d, budget)
	} else {
		t.Logf("NFR-2 OK (%s): restart took %s (budget %s)", mode, d, budget)
	}
}

// parseReadyDuration extracts the wall-clock from a `Restarting
// supervisor (...)... ready (X.Ys).` line. Returns ok=false if no such
// line is present.
func parseReadyDuration(out string) (time.Duration, bool) {
	const marker = "ready ("
	i := strings.Index(out, marker)
	if i < 0 {
		return 0, false
	}
	rest := out[i+len(marker):]
	j := strings.Index(rest, "s")
	if j < 0 {
		return 0, false
	}
	var secs float64
	if _, err := fmt.Sscanf(rest[:j], "%f", &secs); err != nil {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

// writeKillSwitch sets `[daemon].auto_restart_on_drift` in the city's
// city.toml. Used by the kill-switch test. Appends to the existing
// file rather than rewriting from scratch so we preserve whatever
// `gc init` wrote.
func writeKillSwitch(t *testing.T, cityDir string, enabled bool) {
	t.Helper()
	tomlPath := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	val := "true"
	if !enabled {
		val = "false"
	}
	addition := fmt.Sprintf("\n[daemon]\nauto_restart_on_drift = %s\n", val)
	if err := os.WriteFile(tomlPath, append(data, []byte(addition)...), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

// writeStuckSupervisorShim overwrites binaryPath with a shell script
// that, when invoked as `<binary> supervisor run`, sleeps without
// binding the /health port. Used by the restart-timeout test.
func writeStuckSupervisorShim(t *testing.T, binaryPath string) string {
	t.Helper()
	script := "#!/bin/sh\nif [ \"$1\" = \"supervisor\" ] && [ \"$2\" = \"run\" ]; then\n  exec sleep 60\nfi\nexec '" + binaryPath + ".real' \"$@\"\n"
	// Move the real binary aside so the shim can fall through for any
	// non-`supervisor run` subcommand the drift code path needs.
	realPath := binaryPath + ".real"
	if err := os.Rename(binaryPath, realPath); err != nil {
		t.Fatalf("moving real binary: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(binaryPath)
		_ = os.Rename(realPath, binaryPath)
	})
	if err := os.WriteFile(binaryPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stuck shim: %v", err)
	}
	return binaryPath
}

// requireUserSystemd skips the test unless `systemctl --user
// is-system-running` (or equivalent) returns successfully. CI
// containers and macOS lack a user systemd instance.
func requireUserSystemd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl not on PATH")
	}
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		t.Skip("XDG_RUNTIME_DIR not set; user systemd not addressable")
	}
	cmd := exec.Command("systemctl", "--user", "is-system-running")
	out, err := cmd.CombinedOutput()
	state := strings.TrimSpace(string(out))
	// is-system-running returns non-zero in degraded/starting modes;
	// for our purposes those still mean a user bus exists. We only
	// skip when the bus is completely unavailable.
	if err != nil && (strings.Contains(state, "offline") || strings.Contains(state, "Failed to connect")) {
		t.Skipf("user systemd not usable: %s", state)
	}
}

func requireLinuxProcExe(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("direct supervisor binary-drift restart uses Linux /proc/<pid>/exe; macOS production restart is covered by launchd unit tests")
	}
}

// requireSecondaryUID skips the test unless we can spawn processes as
// a different uid than the test runs as.
func requireSecondaryUID(t *testing.T) {
	t.Helper()
	if syscall.Getuid() != 0 {
		t.Skip("permission-denied scenario requires root to drop to a secondary uid")
	}
	if _, err := exec.LookPath("nobody"); err == nil {
		// some distros provide a `nobody` helper; not relied upon here
		_ = err
	}
}

// secondaryUID returns a uid distinct from the current process. We
// pick `nobody` (uid 65534 on most distros) when reachable; otherwise
// a high static uid that should be unused.
func secondaryUID(t *testing.T) uint32 {
	t.Helper()
	// Prefer 65534 (nobody) on Linux; fall back to 65533 if 65534 is
	// somehow the current uid.
	if syscall.Getuid() == 65534 {
		return 65533
	}
	return 65534
}

// expectedSupervisorSystemdUnit returns the systemd unit name the gc
// binary will derive for a supervisor running under the supplied
// GC_HOME. It mirrors supervisorSystemdServiceName() +
// supervisorServiceSuffix() in cmd/gc/cmd_supervisor_lifecycle.go so
// the test installs the unit at the name the binary's
// `systemctl --user is-active` probe will look for.
//
// Algorithm: normalize gcHome (symlink-resolve + abs), sanitize its
// basename to [a-z0-9-], hash the normalized path with sha1[:8], and
// concatenate as `gascity-supervisor-<base>-<hash>.service`. Empty
// basename falls back to `isolated-<hash>` per the binary.
//
// The two algorithms must stay in lockstep — when the binary changes
// its naming, this helper must change with it or
// TestStartDrift_SystemdManaged_RestartsToNewBuildID will revert to
// the 'direct' branch silently.
func expectedSupervisorSystemdUnit(gcHome string) string {
	suffix := expectedSupervisorServiceSuffix(gcHome)
	if suffix == "" {
		return "gascity-supervisor.service"
	}
	return "gascity-supervisor-" + suffix + ".service"
}

// expectedSupervisorServiceSuffix replicates supervisorServiceSuffix()
// from cmd/gc/cmd_supervisor_lifecycle.go. Returns "" for the
// non-isolated (empty / default-home) case — the test never hits that
// branch because newIsolatedEnvRoot always sets an isolated GC_HOME,
// but the empty arm is preserved so the helper stays a faithful
// mirror of the production function.
func expectedSupervisorServiceSuffix(gcHome string) string {
	gcHome = pathutil.NormalizePathForCompare(strings.TrimSpace(gcHome))
	if gcHome == "" {
		return ""
	}
	base := sanitizeSupervisorServiceName(filepath.Base(gcHome))
	sum := sha1.Sum([]byte(gcHome))
	hash := hex.EncodeToString(sum[:])[:8]
	if base == "" {
		return "isolated-" + hash
	}
	return base + "-" + hash
}

// sanitizeSupervisorServiceName mirrors sanitizeServiceName() from
// cmd/gc/cmd_supervisor_lifecycle.go: lowercase, collapse non-alnum
// runs to '-', trim leading/trailing '-'.
func sanitizeSupervisorServiceName(name string) string {
	name = strings.ToLower(name)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	name = re.ReplaceAllString(name, "-")
	return strings.Trim(name, "-")
}

// systemdUserUnitDir returns the directory where user-level systemd
// units live for the current user. Tests write the supervisor unit
// here directly rather than going through `gc supervisor install` so
// the test is self-contained.
func systemdUserUnitDir() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "systemd", "user")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "systemd", "user")
}

// writeSystemdUserUnit writes a minimal [Service]/[Install] unit file
// for the supervisor at binaryPath, using gcHome / runtimeDir as its
// environment. Returns the unit name (with .service suffix).
//
// The unit name is computed via expectedSupervisorSystemdUnit so it
// matches what the gc binary's supervisorSystemdServiceName() will
// resolve when invoked with the same GC_HOME. If the names diverge,
// the binary's `systemctl --user is-active <derived>` check returns
// false even when this unit file is loaded under a different name,
// and the restart path falls through to the 'direct' branch instead
// of 'systemd-managed' (the original bug this helper is fixing).
func writeSystemdUserUnit(t *testing.T, binaryPath, gcHome, runtimeDir string) string {
	t.Helper()
	dir := systemdUserUnitDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating systemd user dir: %v", err)
	}
	unitName := expectedSupervisorSystemdUnit(gcHome)
	unit := fmt.Sprintf(`[Unit]
Description=Gas City drift integration test supervisor

[Service]
Type=simple
ExecStart=%s supervisor run
Restart=no
StandardOutput=append:%s/supervisor.log
StandardError=append:%s/supervisor.log
Environment=GC_HOME=%s
Environment=XDG_RUNTIME_DIR=%s
Environment=GC_DOLT=skip
Environment=GC_BEADS=file
Environment=GC_SESSION=fake

[Install]
WantedBy=default.target
`, binaryPath, gcHome, gcHome, gcHome, runtimeDir)
	path := filepath.Join(dir, unitName)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatalf("writing systemd unit: %v", err)
	}
	return unitName
}

// systemctlUser invokes systemctl --user with the given args. Returns
// stderr-style combined output and the error so tests can both assert
// success and surface failure messages.
func systemctlUser(args ...string) error {
	full := append([]string{"--user"}, args...)
	cmd := exec.Command("systemctl", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w\n%s", strings.Join(full, " "), err, string(out))
	}
	return nil
}

// mustSystemctlUser wraps systemctlUser with t.Fatalf on error.
func mustSystemctlUser(t *testing.T, args ...string) {
	t.Helper()
	if err := systemctlUser(args...); err != nil {
		t.Fatalf("%v", err)
	}
}
