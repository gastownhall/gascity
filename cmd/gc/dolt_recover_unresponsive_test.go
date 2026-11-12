package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeUnresponsiveWindowCityConfig(t *testing.T, cityPath, daemonLine string) {
	t.Helper()
	data := "[workspace]\nname = \"recover-window\"\n\n[daemon]\n" + daemonLine + "\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(data), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
}

func TestManagedDoltRecoveryState_ResolvesUnresponsiveWindowFromCityConfig(t *testing.T) {
	tests := []struct {
		name       string
		daemonLine string
		noConfig   bool
		want       time.Duration
		wantDiag   string
	}{
		{name: "absent", daemonLine: "", want: 180 * time.Second},
		{name: "zero", daemonLine: `dolt_unresponsive_restart_after = "0s"`, want: 0},
		{name: "positive", daemonLine: `dolt_unresponsive_restart_after = "45s"`, want: 45 * time.Second},
		{name: "negative", daemonLine: `dolt_unresponsive_restart_after = "-5s"`, want: 180 * time.Second, wantDiag: "dolt_unresponsive_restart_after"},
		{name: "malformed", daemonLine: `dolt_unresponsive_restart_after = "soon"`, want: 180 * time.Second, wantDiag: "dolt_unresponsive_restart_after"},
		{name: "config unreadable", noConfig: true, want: 180 * time.Second, wantDiag: "using default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if !tt.noConfig {
				writeUnresponsiveWindowCityConfig(t, cityPath, tt.daemonLine)
			}
			got, diagnostics := resolveManagedDoltUnresponsiveRestartAfter(cityPath)
			if got != tt.want {
				t.Errorf("resolveManagedDoltUnresponsiveRestartAfter() = %v, want %v", got, tt.want)
			}
			joined := strings.Join(diagnostics, "\n")
			if tt.wantDiag == "" && len(diagnostics) != 0 {
				t.Errorf("diagnostics = %q, want none", joined)
			}
			if tt.wantDiag != "" && !strings.Contains(joined, tt.wantDiag) {
				t.Errorf("diagnostics = %q, want mention of %q", joined, tt.wantDiag)
			}
		})
	}
}

// unresponsiveRecoveryFixture drives recoverManagedDoltProcessWithOps with an
// injected clock, window and live-listener inspection so the unresponsive gate
// can be exercised without a real dolt server.
type unresponsiveRecoveryFixture struct {
	t        *testing.T
	cityPath string
	layout   managedDoltRuntimeLayout
	now      time.Time
	window   time.Duration
	// windowDiagnostics are returned alongside window by the injected
	// window resolver.
	windowDiagnostics []string
	identity          managedDoltServerIdentity
	live              bool
	probeErr          error
	// observations, when set, is consumed one entry per liveListener call
	// before falling back to identity.
	observations []managedDoltServerIdentity

	stopCalls    int
	startCalls   int
	cleanupCalls int
}

func newUnresponsiveRecoveryFixture(t *testing.T) *unresponsiveRecoveryFixture {
	t.Helper()
	cityPath := setupRecoveryTestCity(t)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	return &unresponsiveRecoveryFixture{
		t:        t,
		cityPath: cityPath,
		layout:   layout,
		now:      time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC),
		window:   180 * time.Second,
		identity: managedDoltServerIdentity{PID: 4242, StartTime: "123456", Port: 3306},
		live:     true,
		probeErr: errors.New("dolt query probe: context deadline exceeded after 5s"),
	}
}

func (f *unresponsiveRecoveryFixture) ops() managedDoltRecoveryOps {
	return managedDoltRecoveryOps{
		queryProbe: func(_, _, _ string) error { return f.probeErr },
		healthCheck: func(_, _, _ string) (managedDoltSQLHealthReport, error) {
			return managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false"}, nil
		},
		stop: func(_, _ string) (managedDoltStopReport, error) {
			f.stopCalls++
			return managedDoltStopReport{HadPID: true, PID: f.identity.PID}, nil
		},
		preflightCleanup: func(_ string) error { return nil },
		start: func(_, _, _, _, _ string, _ time.Duration) (managedDoltStartReport, error) {
			f.startCalls++
			return managedDoltStartReport{Ready: true, PID: f.identity.PID + 1, Port: f.identity.Port}, nil
		},
		publish: func(_ string) error { return nil },
		failedCleanup: func(_ string, _, _ int, cause error) error {
			f.cleanupCalls++
			return cause
		},
		now:                func() time.Time { return f.now },
		unresponsiveWindow: func(_ string) (time.Duration, []string) { return f.window, f.windowDiagnostics },
		liveListener: func(_ string, _ managedDoltRuntimeLayout, _ string) (managedDoltServerIdentity, bool) {
			if len(f.observations) > 0 {
				next := f.observations[0]
				f.observations = f.observations[1:]
				return next, true
			}
			return f.identity, f.live
		},
		diag: io.Discard,
	}
}

func (f *unresponsiveRecoveryFixture) recover() (managedDoltRecoverReport, error) {
	f.t.Helper()
	return recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", strconv.Itoa(f.identity.Port), "root", "warning", 2*time.Second, f.ops())
}

func (f *unresponsiveRecoveryFixture) history() managedDoltRecoveryState {
	f.t.Helper()
	state, diagnostic, err := readManagedDoltRecoveryState(managedDoltRecoveryStatePath(f.layout))
	if err != nil || diagnostic != "" {
		f.t.Fatalf("readManagedDoltRecoveryState: diagnostic=%q err=%v", diagnostic, err)
	}
	return state
}

func (f *unresponsiveRecoveryFixture) destructiveCalls() int {
	return f.stopCalls + f.startCalls + f.cleanupCalls
}

func TestRecoverManagedDolt_DefersLiveListenerBelowWindowAndPersistsIdentity(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)

	report, err := f.recover()

	var deferred *managedDoltRecoveryDeferredError
	if !errors.As(err, &deferred) {
		t.Fatalf("recover error = %v, want *managedDoltRecoveryDeferredError", err)
	}
	if !errors.Is(err, f.probeErr) {
		t.Errorf("deferral error does not wrap the probe failure: %v", err)
	}
	if deferred.Identity != f.identity || deferred.Window != f.window || deferred.Elapsed != 0 {
		t.Errorf("deferral = %+v, want identity %+v window %v elapsed 0", deferred, f.identity, f.window)
	}
	if !strings.Contains(err.Error(), "deferring restart") {
		t.Errorf("deferral error %q does not state the deferral reason", err)
	}
	if n := f.destructiveCalls(); n != 0 {
		t.Fatalf("destructive calls = %d (stop=%d start=%d cleanup=%d), want 0", n, f.stopCalls, f.startCalls, f.cleanupCalls)
	}
	if report.Restarted || report.Ready || report.Healthy {
		t.Errorf("report = %+v, want no restart and no readiness", report)
	}
	if report.PID != f.identity.PID || report.Port != f.identity.Port || !report.HadPID {
		t.Errorf("report = %+v, want the live server's pid %d and port %d", report, f.identity.PID, f.identity.Port)
	}

	streak := f.history().Streak
	if streak == nil {
		t.Fatal("history has no unresponsive streak after a deferral")
	}
	if streak.managedDoltServerIdentity != f.identity {
		t.Errorf("streak identity = %+v, want %+v", streak.managedDoltServerIdentity, f.identity)
	}
	if !streak.FirstFailedAt.Equal(f.now) || !streak.LastFailedAt.Equal(f.now) || streak.Failures != 1 {
		t.Errorf("streak = %+v, want first=last=%v failures=1", streak, f.now)
	}

	f.now = f.now.Add(90 * time.Second)
	_, err = f.recover()
	if !errors.As(err, &deferred) || deferred.Elapsed != 90*time.Second {
		t.Fatalf("second failure at t+90s: err = %v, want deferral with elapsed 90s", err)
	}
	if n := f.destructiveCalls(); n != 0 {
		t.Fatalf("destructive calls after second failure = %d, want 0", n)
	}
	if streak := f.history().Streak; streak == nil || streak.Failures != 2 || !streak.LastFailedAt.Equal(f.now) {
		t.Errorf("streak after second failure = %+v, want failures=2 last=%v", streak, f.now)
	}
}

func TestRecoverManagedDolt_HealthCheckErrorOnLiveListenerDefers(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	f.probeErr = nil
	ops := f.ops()
	healthErr := errors.New("health: context deadline exceeded")
	ops.healthCheck = func(_, _, _ string) (managedDoltSQLHealthReport, error) {
		return managedDoltSQLHealthReport{}, healthErr
	}

	_, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops)

	var deferred *managedDoltRecoveryDeferredError
	if !errors.As(err, &deferred) || !errors.Is(err, healthErr) {
		t.Fatalf("recover error = %v, want a deferral wrapping the health-check error", err)
	}
	if n := f.destructiveCalls(); n != 0 {
		t.Fatalf("destructive calls = %d, want 0", n)
	}
}

func TestRecoverManagedDolt_StreakReachingWindowIsConsumedByOneRecovery(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	start := f.now
	if _, err := f.recover(); err == nil {
		t.Fatal("first failure: want deferral, got nil")
	}

	f.now = start.Add(f.window)
	report, err := f.recover()
	if err != nil {
		t.Fatalf("failure at the window: recover error = %v, want restart", err)
	}
	if f.stopCalls != 1 || f.startCalls != 1 || !report.Restarted {
		t.Fatalf("failure at the window: stop=%d start=%d restarted=%v, want one stop/start", f.stopCalls, f.startCalls, report.Restarted)
	}
	if streak := f.history().Streak; streak != nil {
		t.Fatalf("streak after restart = %+v, want consumed", streak)
	}

	// A second failure right after the restart cannot reuse the consumed
	// streak: it starts a fresh one and defers.
	f.now = f.now.Add(time.Second)
	_, err = f.recover()
	var deferred *managedDoltRecoveryDeferredError
	if !errors.As(err, &deferred) || deferred.Elapsed != 0 {
		t.Fatalf("failure after consumption: err = %v, want a fresh deferral", err)
	}
	if f.stopCalls != 1 || f.startCalls != 1 {
		t.Fatalf("failure after consumption: stop=%d start=%d, want still one", f.stopCalls, f.startCalls)
	}
}

func TestRecoverManagedDolt_ReplacementInstanceDoesNotInheritStreak(t *testing.T) {
	tests := []struct {
		name        string
		replacement managedDoltServerIdentity
	}{
		{name: "pid reused with new start time", replacement: managedDoltServerIdentity{PID: 4242, StartTime: "999999", Port: 3306}},
		{name: "new pid", replacement: managedDoltServerIdentity{PID: 5151, StartTime: "123999", Port: 3306}},
		{name: "rebound port", replacement: managedDoltServerIdentity{PID: 4242, StartTime: "123456", Port: 3307}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUnresponsiveRecoveryFixture(t)
			start := f.now
			if _, err := f.recover(); err == nil {
				t.Fatal("first failure: want deferral, got nil")
			}
			f.identity = tt.replacement
			f.now = start.Add(f.window + time.Second)
			_, err := f.recover()
			var deferred *managedDoltRecoveryDeferredError
			if !errors.As(err, &deferred) || deferred.Identity != tt.replacement || deferred.Elapsed != 0 {
				t.Fatalf("replacement failure: err = %v, want fresh deferral for %+v", err, tt.replacement)
			}
			if n := f.destructiveCalls(); n != 0 {
				t.Fatalf("destructive calls = %d, want 0", n)
			}
			if streak := f.history().Streak; streak == nil || streak.managedDoltServerIdentity != tt.replacement || streak.Failures != 1 {
				t.Fatalf("streak = %+v, want a fresh streak for the replacement", streak)
			}
		})
	}
}

func TestRecoverManagedDolt_ReplacedBeforeStopRestartsTheStreak(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	start := f.now
	if _, err := f.recover(); err == nil {
		t.Fatal("first failure: want deferral, got nil")
	}
	replacement := managedDoltServerIdentity{PID: 6161, StartTime: "777", Port: 3306}
	// The window has elapsed for the original instance, but the recheck just
	// before the stop sees a different instance holding the listener.
	f.observations = []managedDoltServerIdentity{f.identity, replacement}
	f.now = start.Add(f.window)
	_, err := f.recover()
	var deferred *managedDoltRecoveryDeferredError
	if !errors.As(err, &deferred) || deferred.Identity != replacement {
		t.Fatalf("recover error = %v, want deferral for the replacement %+v", err, replacement)
	}
	if n := f.destructiveCalls(); n != 0 {
		t.Fatalf("destructive calls = %d, want 0 (replacement cannot inherit eligibility)", n)
	}
	if streak := f.history().Streak; streak == nil || streak.managedDoltServerIdentity != replacement || !streak.FirstFailedAt.Equal(f.now) {
		t.Fatalf("streak = %+v, want a fresh streak for the replacement starting now", streak)
	}
}

func TestManagedDoltRecoveryState_InvalidHistoryNeverAuthorizesRestart(t *testing.T) {
	base := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	identity := managedDoltServerIdentity{PID: 4242, StartTime: "123456", Port: 3306}
	streakJSON := func(first, last time.Time, failures int) string {
		return fmt.Sprintf(`{"version":1,"streak":{"pid":%d,"start_time":%q,"port":%d,"first_failed_at":%q,"last_failed_at":%q,"failures":%d}}`,
			identity.PID, identity.StartTime, identity.Port, first.Format(time.RFC3339Nano), last.Format(time.RFC3339Nano), failures)
	}
	tests := []struct {
		name     string
		history  string
		wantDiag string
	}{
		{name: "corrupt", history: "{not json", wantDiag: "discarding"},
		{name: "unsupported version", history: strings.Replace(streakJSON(base.Add(-time.Hour), base.Add(-time.Minute), 9), `"version":1`, `"version":99`, 1), wantDiag: "version"},
		{name: "stale gap beyond max(window, 5m)", history: streakJSON(base.Add(-400*time.Second), base.Add(-390*time.Second), 2)},
		{name: "last failure in the future", history: streakJSON(base.Add(-200*time.Second), base.Add(10*time.Second), 3), wantDiag: "clock"},
		{name: "first after last", history: streakJSON(base.Add(-250*time.Second), base.Add(-260*time.Second), 2), wantDiag: "clock"},
		{name: "no failures recorded", history: streakJSON(base.Add(-250*time.Second), base.Add(-60*time.Second), 0), wantDiag: "discarding"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUnresponsiveRecoveryFixture(t)
			f.now = base
			f.identity = identity
			path := managedDoltRecoveryStatePath(f.layout)
			if err := os.WriteFile(path, []byte(tt.history), 0o644); err != nil {
				t.Fatalf("seed history: %v", err)
			}
			var diag bytes.Buffer
			ops := f.ops()
			ops.diag = &diag

			_, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops)

			var deferred *managedDoltRecoveryDeferredError
			if !errors.As(err, &deferred) || deferred.Elapsed != 0 {
				t.Fatalf("recover error = %v, want a fresh deferral", err)
			}
			if n := f.destructiveCalls(); n != 0 {
				t.Fatalf("destructive calls = %d, want 0", n)
			}
			streak := f.history().Streak
			if streak == nil || !streak.FirstFailedAt.Equal(base) || streak.Failures != 1 {
				t.Fatalf("streak = %+v, want a fresh streak starting now", streak)
			}
			if tt.wantDiag != "" && !strings.Contains(diag.String(), tt.wantDiag) {
				t.Errorf("diagnostics = %q, want mention of %q", diag.String(), tt.wantDiag)
			}
			if tt.wantDiag != "" {
				lines := f.journal()
				if len(lines) != 2 {
					t.Fatalf("journal lines = %d, want the history diagnostic then the deferral:\n%s", len(lines), strings.Join(lines, "\n"))
				}
				assertJournalLine(t, lines[0], base, "action=history-diagnostic", tt.wantDiag, `caller="direct-helper"`)
				assertJournalLine(t, lines[1], base, "action=defer", "elapsed=0s")
			}
		})
	}

	t.Run("valid streak within the gap still authorizes", func(t *testing.T) {
		f := newUnresponsiveRecoveryFixture(t)
		f.now = base
		f.identity = identity
		if err := os.WriteFile(managedDoltRecoveryStatePath(f.layout), []byte(streakJSON(base.Add(-200*time.Second), base.Add(-100*time.Second), 4)), 0o644); err != nil {
			t.Fatalf("seed history: %v", err)
		}
		if _, err := f.recover(); err != nil {
			t.Fatalf("recover error = %v, want restart", err)
		}
		if f.stopCalls != 1 || f.startCalls != 1 {
			t.Fatalf("stop=%d start=%d, want one restart", f.stopCalls, f.startCalls)
		}
	})
}

func TestManagedDoltRecoveryState_InaccessibleHistoryReturnsErrorWithoutStopping(t *testing.T) {
	writeErr := errors.New("disk full")
	tests := []struct {
		name  string
		setup func(f *unresponsiveRecoveryFixture, ops *managedDoltRecoveryOps)
	}{
		{name: "unreadable history", setup: func(f *unresponsiveRecoveryFixture, _ *managedDoltRecoveryOps) {
			if err := os.MkdirAll(managedDoltRecoveryStatePath(f.layout), 0o755); err != nil {
				f.t.Fatalf("make history a directory: %v", err)
			}
		}},
		{name: "unwritable history below the window", setup: func(_ *unresponsiveRecoveryFixture, ops *managedDoltRecoveryOps) {
			ops.writeHistory = func(string, managedDoltRecoveryState) error { return writeErr }
		}},
		{name: "unwritable history at the window", setup: func(f *unresponsiveRecoveryFixture, ops *managedDoltRecoveryOps) {
			if err := writeManagedDoltRecoveryState(managedDoltRecoveryStatePath(f.layout), managedDoltRecoveryState{Streak: &managedDoltUnresponsiveStreak{
				managedDoltServerIdentity: f.identity,
				FirstFailedAt:             f.now.Add(-f.window),
				LastFailedAt:              f.now.Add(-time.Minute),
				Failures:                  3,
			}}); err != nil {
				f.t.Fatalf("seed history: %v", err)
			}
			ops.writeHistory = func(string, managedDoltRecoveryState) error { return writeErr }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUnresponsiveRecoveryFixture(t)
			ops := f.ops()
			tt.setup(f, &ops)

			_, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops)

			var deferred *managedDoltRecoveryDeferredError
			if err == nil || errors.As(err, &deferred) {
				t.Fatalf("recover error = %v, want a history error (not nil, not a deferral)", err)
			}
			if !strings.Contains(err.Error(), "recovery history") {
				t.Errorf("recover error = %q, want it to name the recovery history", err)
			}
			if n := f.destructiveCalls(); n != 0 {
				t.Fatalf("destructive calls = %d, want 0", n)
			}
		})
	}
}

func TestRecoverManagedDolt_UnresponsiveWindowPreservesExistingRecoveryPaths(t *testing.T) {
	tests := []struct {
		name         string
		configure    func(f *unresponsiveRecoveryFixture, ops *managedDoltRecoveryOps)
		wantReadOnly bool
	}{
		{name: "zero window restarts a live listener immediately", configure: func(f *unresponsiveRecoveryFixture, _ *managedDoltRecoveryOps) {
			f.window = 0
		}},
		{name: "dead or refusing server restarts immediately", configure: func(f *unresponsiveRecoveryFixture, _ *managedDoltRecoveryOps) {
			f.live = false
		}},
		{name: "read-only server restarts immediately", wantReadOnly: true, configure: func(f *unresponsiveRecoveryFixture, ops *managedDoltRecoveryOps) {
			f.probeErr = nil
			calls := 0
			ops.healthCheck = func(_, _, _ string) (managedDoltSQLHealthReport, error) {
				calls++
				if calls == 1 {
					return managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "true"}, nil
				}
				return managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false"}, nil
			}
		}},
		{name: "read-only unknown server restarts immediately", configure: func(f *unresponsiveRecoveryFixture, ops *managedDoltRecoveryOps) {
			f.probeErr = nil
			calls := 0
			ops.healthCheck = func(_, _, _ string) (managedDoltSQLHealthReport, error) {
				calls++
				if calls == 1 {
					return managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "unknown"}, nil
				}
				return managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false"}, nil
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUnresponsiveRecoveryFixture(t)
			// History that cannot be read proves these paths never consult it.
			historyPath := managedDoltRecoveryStatePath(f.layout)
			if err := os.MkdirAll(historyPath, 0o755); err != nil {
				t.Fatalf("make history unreadable: %v", err)
			}
			ops := f.ops()
			tt.configure(f, &ops)

			report, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops)
			if err != nil {
				t.Fatalf("recover error = %v, want the existing restart", err)
			}
			if f.stopCalls != 1 || f.startCalls != 1 || !report.Restarted {
				t.Fatalf("stop=%d start=%d restarted=%v, want one immediate restart", f.stopCalls, f.startCalls, report.Restarted)
			}
			if report.DiagnosedReadOnly != tt.wantReadOnly {
				t.Errorf("DiagnosedReadOnly = %v, want %v", report.DiagnosedReadOnly, tt.wantReadOnly)
			}
		})
	}
}

func TestRecoverManagedDolt_ProviderOwnedScopeRefusedBeforeUnresponsiveGate(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	if err := persistProviderScopeOwnership(f.cityPath, f.cityPath, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(f.cityPath, f.cityPath); err != nil {
		t.Fatal(err)
	}
	gateConsulted := false
	ops := f.ops()
	ops.liveListener = func(string, managedDoltRuntimeLayout, string) (managedDoltServerIdentity, bool) {
		gateConsulted = true
		return f.identity, true
	}

	_, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops)

	var deferred *managedDoltRecoveryDeferredError
	if err == nil || errors.As(err, &deferred) {
		t.Fatalf("recover error = %v, want the existing provider-owned refusal", err)
	}
	if gateConsulted || f.destructiveCalls() != 0 {
		t.Fatalf("gate consulted=%v destructive calls=%d, want neither for a provider-owned scope", gateConsulted, f.destructiveCalls())
	}
	if _, statErr := os.Stat(managedDoltRecoveryStatePath(f.layout)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("recovery history stat = %v, want no history written for a provider-owned scope", statErr)
	}
}
