package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// seedUnresponsiveHistory writes a streak for f.identity plus a provider
// cooldown reservation and returns the cooldown timestamp.
func seedUnresponsiveHistory(t *testing.T, layout managedDoltRuntimeLayout, identity managedDoltServerIdentity, first, last time.Time, failures int) time.Time {
	t.Helper()
	cooldown := last.Add(-5 * time.Second)
	if err := os.MkdirAll(layout.PackStateDir, 0o755); err != nil {
		t.Fatalf("create pack state dir: %v", err)
	}
	if err := writeManagedDoltRecoveryState(managedDoltRecoveryStatePath(layout), managedDoltRecoveryState{
		Streak: &managedDoltUnresponsiveStreak{
			managedDoltServerIdentity: identity,
			FirstFailedAt:             first,
			LastFailedAt:              last,
			Failures:                  failures,
		},
		LastProviderAttemptAt: &cooldown,
	}); err != nil {
		t.Fatalf("seed recovery history: %v", err)
	}
	return cooldown
}

// assertStreakClearedCooldownKept reads the history at layout and fails
// unless the streak is gone and the provider cooldown is unchanged.
func assertStreakClearedCooldownKept(t *testing.T, layout managedDoltRuntimeLayout, cooldown time.Time) {
	t.Helper()
	state, diagnostic, err := readManagedDoltRecoveryState(managedDoltRecoveryStatePath(layout))
	if err != nil || diagnostic != "" {
		t.Fatalf("readManagedDoltRecoveryState: diagnostic=%q err=%v", diagnostic, err)
	}
	if state.Streak != nil {
		t.Errorf("streak = %+v, want cleared by the healthy observation", state.Streak)
	}
	if state.LastProviderAttemptAt == nil || !state.LastProviderAttemptAt.Equal(cooldown) {
		t.Errorf("last_provider_attempt_at = %v, want the cooldown %v preserved", state.LastProviderAttemptAt, cooldown)
	}
}

func TestRecoverManagedDolt_MeasuredHealthyReturnsClearStreakAndKeepCooldown(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(f *unresponsiveRecoveryFixture)
		wantRestart bool
	}{
		{name: "measured direct health", configure: func(f *unresponsiveRecoveryFixture) { f.probeErr = nil }},
		{name: "post-restart health with a zero window", wantRestart: true, configure: func(f *unresponsiveRecoveryFixture) { f.window = 0 }},
		{name: "post-restart health after a dead server", wantRestart: true, configure: func(f *unresponsiveRecoveryFixture) { f.live = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUnresponsiveRecoveryFixture(t)
			cooldown := seedUnresponsiveHistory(t, f.layout, f.identity, f.now.Add(-60*time.Second), f.now.Add(-30*time.Second), 2)
			tt.configure(f)

			report, err := f.recover()
			if err != nil {
				t.Fatalf("recover error = %v, want a healthy return", err)
			}
			if report.Restarted != tt.wantRestart || !report.Healthy {
				t.Fatalf("report = %+v, want healthy with restarted=%v", report, tt.wantRestart)
			}
			assertStreakClearedCooldownKept(t, f.layout, cooldown)
		})
	}
}

// TestRecoverManagedDolt_HealthyResetWaitsForInFlightFailureWrite forces a
// healthy reset to start while a failure observation holds the lifecycle
// lock with an older snapshot of the history. The reset must wait for that
// write and clear the result, so the older snapshot cannot restore the streak.
func TestRecoverManagedDolt_HealthyResetWaitsForInFlightFailureWrite(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	cooldown := seedUnresponsiveHistory(t, f.layout, f.identity, f.now.Add(-60*time.Second), f.now.Add(-30*time.Second), 2)

	resetDone := make(chan error, 1)
	var once sync.Once
	var resetFinishedEarly bool
	ops := f.ops()
	ops.writeHistory = func(path string, state managedDoltRecoveryState) error {
		once.Do(func() {
			go func() {
				resetDone <- clearManagedDoltUnresponsiveStreak(f.layout, writeManagedDoltRecoveryState, io.Discard)
			}()
			select {
			case err := <-resetDone:
				resetFinishedEarly = true
				resetDone <- err
			case <-time.After(300 * time.Millisecond):
			}
		})
		return writeManagedDoltRecoveryState(path, state)
	}

	_, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops)
	var deferred *managedDoltRecoveryDeferredError
	if !errors.As(err, &deferred) {
		t.Fatalf("failure recover error = %v, want a deferral", err)
	}
	if resetFinishedEarly {
		t.Error("healthy reset completed while the failure observation held the lifecycle lock")
	}
	select {
	case err := <-resetDone:
		if err != nil {
			t.Fatalf("healthy reset error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("healthy reset did not complete after the failure released the lifecycle lock")
	}
	assertStreakClearedCooldownKept(t, f.layout, cooldown)

	f.now = f.now.Add(time.Second)
	_, err = f.recover()
	if !errors.As(err, &deferred) || deferred.Elapsed != 0 {
		t.Fatalf("next failure: err = %v, want a fresh deferral", err)
	}
	if n := f.destructiveCalls(); n != 0 {
		t.Fatalf("destructive calls = %d, want 0", n)
	}
}

// TestRecoverManagedDolt_FailureDuringHealthyResetCannotReuseClearedStreak
// forces a failure observation to arrive while a healthy reset holds the
// lifecycle lock over a streak that has already reached the window. The
// failure must wait and then start a fresh streak instead of stopping.
func TestRecoverManagedDolt_FailureDuringHealthyResetCannotReuseClearedStreak(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	cooldown := seedUnresponsiveHistory(t, f.layout, f.identity, f.now.Add(-f.window-10*time.Second), f.now.Add(-30*time.Second), 5)

	type result struct{ err error }
	recoverDone := make(chan result, 1)
	var once sync.Once
	var recoverFinishedEarly bool
	write := func(path string, state managedDoltRecoveryState) error {
		once.Do(func() {
			go func() {
				_, err := f.recover()
				recoverDone <- result{err: err}
			}()
			select {
			case r := <-recoverDone:
				recoverFinishedEarly = true
				recoverDone <- r
			case <-time.After(300 * time.Millisecond):
			}
		})
		return writeManagedDoltRecoveryState(path, state)
	}

	if err := clearManagedDoltUnresponsiveStreak(f.layout, write, io.Discard); err != nil {
		t.Fatalf("healthy reset error = %v", err)
	}
	if recoverFinishedEarly {
		t.Error("failure recovery completed while the healthy reset held the lifecycle lock")
	}
	var r result
	select {
	case r = <-recoverDone:
	case <-time.After(10 * time.Second):
		t.Fatal("failure recovery did not complete after the reset released the lifecycle lock")
	}
	var deferred *managedDoltRecoveryDeferredError
	if !errors.As(r.err, &deferred) || deferred.Elapsed != 0 {
		t.Fatalf("failure after reset: err = %v, want a fresh deferral", r.err)
	}
	if n := f.destructiveCalls(); n != 0 {
		t.Fatalf("destructive calls = %d, want 0 (a cleared streak cannot authorize a stop)", n)
	}
	state := f.history()
	if state.Streak == nil || !state.Streak.FirstFailedAt.Equal(f.now) || state.Streak.Failures != 1 {
		t.Fatalf("streak = %+v, want a fresh streak starting now", state.Streak)
	}
	if state.LastProviderAttemptAt == nil || !state.LastProviderAttemptAt.Equal(cooldown) {
		t.Fatalf("last_provider_attempt_at = %v, want %v preserved", state.LastProviderAttemptAt, cooldown)
	}
}

func TestManagedDoltRecoveryState_HealthyResetBoundsLockWaitAndKeepsStreak(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	holder, _, err := openManagedDoltLifecycleLock(f.cityPath)
	if err != nil {
		t.Fatalf("openManagedDoltLifecycleLock: %v", err)
	}
	if locked, err := tryManagedDoltLifecycleLock(holder); err != nil || !locked {
		_ = holder.Close()
		t.Fatalf("hold lifecycle lock: locked=%v err=%v", locked, err)
	}
	prevTimeout := managedDoltHistoryLockTimeout
	managedDoltHistoryLockTimeout = 150 * time.Millisecond
	t.Cleanup(func() { managedDoltHistoryLockTimeout = prevTimeout })

	// With no visible streak there is nothing to clear, so a held lock does
	// not delay the healthy return.
	if err := clearManagedDoltUnresponsiveStreak(f.layout, writeManagedDoltRecoveryState, io.Discard); err != nil {
		releaseManagedDoltLifecycleLock(holder)
		t.Fatalf("reset with no streak error = %v, want nil", err)
	}

	cooldown := seedUnresponsiveHistory(t, f.layout, f.identity, f.now.Add(-60*time.Second), f.now.Add(-30*time.Second), 2)
	started := time.Now()
	err = clearManagedDoltUnresponsiveStreak(f.layout, writeManagedDoltRecoveryState, io.Discard)
	waited := time.Since(started)
	releaseManagedDoltLifecycleLock(holder)
	if err == nil || !strings.Contains(err.Error(), "synchronize managed dolt recovery history") {
		t.Fatalf("reset error = %v, want a synchronization error", err)
	}
	if waited > 5*time.Second {
		t.Fatalf("reset waited %s, want bounded by the lock timeout", waited)
	}
	if streak := f.history().Streak; streak == nil || streak.Failures != 2 {
		t.Fatalf("streak after a timed-out reset = %+v, want it intact", streak)
	}

	probe, _, err := openManagedDoltLifecycleLock(f.cityPath)
	if err != nil {
		t.Fatalf("openManagedDoltLifecycleLock: %v", err)
	}
	locked, err := tryManagedDoltLifecycleLock(probe)
	releaseManagedDoltLifecycleLock(probe)
	if err != nil || !locked {
		t.Fatalf("lifecycle lock after a timed-out reset: locked=%v err=%v, want it free", locked, err)
	}
	if err := clearManagedDoltUnresponsiveStreak(f.layout, writeManagedDoltRecoveryState, io.Discard); err != nil {
		t.Fatalf("reset after the lock is free error = %v", err)
	}
	assertStreakClearedCooldownKept(t, f.layout, cooldown)
}

func TestManagedDoltRecoveryState_HealthyResetReportsWriteFailure(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	seedUnresponsiveHistory(t, f.layout, f.identity, f.now.Add(-60*time.Second), f.now.Add(-30*time.Second), 2)
	f.probeErr = nil
	ops := f.ops()
	writeErr := errors.New("disk full")
	ops.writeHistory = func(string, managedDoltRecoveryState) error { return writeErr }

	_, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops)
	if !errors.Is(err, writeErr) || !strings.Contains(err.Error(), "unresponsive streak") {
		t.Fatalf("recover error = %v, want the failed streak clear surfaced", err)
	}
	if n := f.destructiveCalls(); n != 0 {
		t.Fatalf("destructive calls = %d, want 0", n)
	}
}

func TestHealthBeadsProviderSuccessClearsUnresponsiveStreak(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	writeBreakerAwarePreflightFakes(t, cityPath, "")
	healthy := "#!/bin/sh\ncase \"$1\" in\n  health) exit 0 ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(gcBeadsBdScriptPath(cityPath), []byte(healthy), 0o755); err != nil {
		t.Fatal(err)
	}
	layout, err := resolveManagedDoltRuntimeLayoutStrict(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayoutStrict: %v", err)
	}
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	identity := managedDoltServerIdentity{PID: 4242, StartTime: "123456", Port: 3306}
	cooldown := seedUnresponsiveHistory(t, layout, identity, now.Add(-60*time.Second), now.Add(-30*time.Second), 2)

	// Downstream publication may fail without a live server; the streak
	// clear follows the measured healthy provider result.
	_ = healthBeadsProvider(cityPath)

	assertStreakClearedCooldownKept(t, layout, cooldown)
}

// stubManagedDoltDirectProbesHealthy makes direct SQL probes report a
// writable, query-ready server for the duration of the test.
func stubManagedDoltDirectProbesHealthy(t *testing.T) {
	t.Helper()
	t.Setenv("GC_DOLT_PASSWORD", "test-password")
	oldQuery := managedDoltQueryProbeDirectFn
	oldReadOnly := managedDoltReadOnlyStateDirectFn
	oldConnections := managedDoltConnectionCountDirectFn
	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltReadOnlyStateDirectFn = func(_, _, _ string) (string, error) { return "false", nil }
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "1", nil }
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn = oldQuery
		managedDoltReadOnlyStateDirectFn = oldReadOnly
		managedDoltConnectionCountDirectFn = oldConnections
	})
}

// writeRecoveryProcessState records pid as the managed dolt on port in both
// the provider and published runtime state.
func writeRecoveryProcessState(t *testing.T, cityPath string, layout managedDoltRuntimeLayout, pid, port int) {
	t.Helper()
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	state := doltRuntimeState{Running: true, PID: pid, Port: port, DataDir: layout.DataDir, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeDoltRuntimeStateFile(layout.StateFile, state); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(layout): %v", err)
	}
	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), state); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(published): %v", err)
	}
}

func newRecoveryProcessCity(t *testing.T) (string, managedDoltRuntimeLayout) {
	t.Helper()
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	for _, dir := range []string{filepath.Dir(layout.PIDFile), layout.DataDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	return cityPath, layout
}

func TestRecoverManagedDoltProcess_ReboundReuseClearsStreakOnlyWhenMeasured(t *testing.T) {
	for _, measured := range []bool{true, false} {
		t.Run(fmt.Sprintf("measured=%v", measured), func(t *testing.T) {
			cityPath, layout := newRecoveryProcessCity(t)
			oldPort := reserveRandomTCPPort(t)
			newPort := reserveRandomTCPPort(t)
			for newPort == oldPort {
				newPort = reserveRandomTCPPort(t)
			}
			listener := startTCPListenerProcessInDir(t, newPort, layout.DataDir)
			writeRecoveryProcessState(t, cityPath, layout, listener.Process.Pid, newPort)
			now := time.Now().UTC()
			identity := managedDoltServerIdentity{PID: listener.Process.Pid, StartTime: "1", Port: newPort}
			cooldown := seedUnresponsiveHistory(t, layout, identity, now.Add(-60*time.Second), now.Add(-30*time.Second), 2)
			if measured {
				stubManagedDoltDirectProbesHealthy(t)
			} else {
				stubManagedDoltDial(t, func(string, string, time.Duration) (net.Conn, error) {
					return nil, dialErrorFor("connect", syscall.EADDRNOTAVAIL)
				})
			}

			report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", strconv.Itoa(oldPort), "root", "warning", 2*time.Second)
			if err != nil {
				t.Fatalf("recoverManagedDoltProcess() error = %v", err)
			}
			if report.Restarted || report.ReadyInconclusive == measured {
				t.Fatalf("report = %+v, want the rebound server kept (inconclusive=%v)", report, !measured)
			}
			if measured {
				assertStreakClearedCooldownKept(t, layout, cooldown)
				return
			}
			state, _, err := readManagedDoltRecoveryState(managedDoltRecoveryStatePath(layout))
			if err != nil || state.Streak == nil || state.Streak.Failures != 2 {
				t.Fatalf("streak after an inconclusive keep = %+v (err %v), want it kept", state.Streak, err)
			}
		})
	}
}

// startLockHoldingListenerProcessInDir starts a process that takes the
// lifecycle lock, listens on port from dir, and releases the lock after hold
// while it keeps listening.
func startLockHoldingListenerProcessInDir(t *testing.T, lockFile string, port int, dir string, hold time.Duration) *exec.Cmd {
	t.Helper()
	skipSlowCmdGCTest(t, "spawns a lock-holding TCP listener process to emulate a concurrent managed dolt lifecycle; run make test-cmd-gc-process for full coverage")
	readyFile := filepath.Join(t.TempDir(), "listening.ready")
	cmd := exec.Command("python3", "-c", `
import fcntl, os, signal, socket, sys, time
lock_file, ready_file, port, data_dir, hold_s = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4], float(sys.argv[5])
lock = open(lock_file, "a+")
fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
os.chdir(data_dir)
sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("127.0.0.1", port))
sock.listen(5)
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
with open(ready_file, "w", encoding="utf-8") as f:
    f.write("ready\n")
time.sleep(hold_s)
fcntl.flock(lock.fileno(), fcntl.LOCK_UN)
while True:
    time.sleep(1)
`, lockFile, readyFile, strconv.Itoa(port), dir, strconv.FormatFloat(hold.Seconds(), 'f', 3, 64))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock-holding listener: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			return cmd
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("lock-holding listener on %d did not become ready", port)
	return nil
}

func TestRecoverManagedDoltProcess_LockWaitKeepClearsStreakAfterHolderReleases(t *testing.T) {
	cityPath, layout := newRecoveryProcessCity(t)
	port := reserveRandomTCPPort(t)
	holder := startLockHoldingListenerProcessInDir(t, layout.LockFile, port, layout.DataDir, 1500*time.Millisecond)
	writeRecoveryProcessState(t, cityPath, layout, holder.Process.Pid, port)
	now := time.Now().UTC()
	identity := managedDoltServerIdentity{PID: holder.Process.Pid, StartTime: "1", Port: port}
	cooldown := seedUnresponsiveHistory(t, layout, identity, now.Add(-60*time.Second), now.Add(-30*time.Second), 2)
	stubManagedDoltDirectProbesHealthy(t)

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", strconv.Itoa(port), "root", "warning", 3*time.Second)
	if err != nil {
		t.Fatalf("recoverManagedDoltProcess() error = %v", err)
	}
	if report.Restarted || !report.Healthy || report.PID != holder.Process.Pid {
		t.Fatalf("report = %+v, want the lock holder's server kept as healthy", report)
	}
	assertStreakClearedCooldownKept(t, layout, cooldown)
	if !managedStopPIDAlive(holder.Process.Pid) {
		t.Fatalf("lock holder pid %d no longer alive", holder.Process.Pid)
	}
}
