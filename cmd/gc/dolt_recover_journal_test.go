package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (f *unresponsiveRecoveryFixture) journal() []string {
	f.t.Helper()
	data, err := os.ReadFile(f.layout.LogFile)
	if err != nil {
		f.t.Fatalf("read recovery journal: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.Contains(line, "gc dolt recovery:") {
			lines = append(lines, line)
		}
	}
	return lines
}

func assertJournalLine(t *testing.T, line string, at time.Time, want ...string) {
	t.Helper()
	if !strings.HasPrefix(line, at.UTC().Format(time.RFC3339Nano)+" ") {
		t.Errorf("journal line %q does not start with timestamp %s", line, at.UTC().Format(time.RFC3339Nano))
	}
	for _, field := range want {
		if !strings.Contains(line, field) {
			t.Errorf("journal line %q is missing %q", line, field)
		}
	}
}

func TestRecoverManagedDolt_JournalRecordsEachDeferralWithCaller(t *testing.T) {
	t.Setenv(managedDoltRecoverCallerEnv, "provider-health")
	f := newUnresponsiveRecoveryFixture(t)
	f.probeErr = errors.New("dolt query probe: timed out\nreading handshake")
	first := f.now

	if _, err := f.recover(); err == nil {
		t.Fatal("first failure: want deferral, got nil")
	}
	f.now = first.Add(90 * time.Second)
	if _, err := f.recover(); err == nil {
		t.Fatal("second failure: want deferral, got nil")
	}

	lines := f.journal()
	if len(lines) != 2 {
		t.Fatalf("journal lines = %d, want one per deferral:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	identity := `pid=4242 start_time="123456" port=3306`
	assertJournalLine(t, lines[0], first, "action=defer", `reason="dolt query probe: timed out\nreading handshake"`, "elapsed=0s", "window=3m0s", identity, `caller="provider-health"`)
	assertJournalLine(t, lines[1], f.now, "action=defer", "elapsed=1m30s", "window=3m0s", identity, `caller="provider-health"`)
}

func TestRecoverManagedDolt_JournalRecordsStopAfterWindowWithDefaultCaller(t *testing.T) {
	t.Setenv(managedDoltRecoverCallerEnv, "")
	f := newUnresponsiveRecoveryFixture(t)
	first := f.now
	if _, err := f.recover(); err == nil {
		t.Fatal("first failure: want deferral, got nil")
	}
	f.now = first.Add(f.window)
	if _, err := f.recover(); err != nil {
		t.Fatalf("failure at the window: recover error = %v, want restart", err)
	}

	lines := f.journal()
	if len(lines) != 2 {
		t.Fatalf("journal lines = %d, want a deferral then a stop:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	assertJournalLine(t, lines[1], f.now, "action=stop", `reason="dolt query probe: context deadline exceeded after 5s"`, "elapsed=3m0s", "window=3m0s",
		`pid=4242 start_time="123456" port=3306`, `caller="direct-helper"`)
}

// firstHealthThenReady reports first on the pre-stop health check and a
// ready, writable server after the restart.
func firstHealthThenReady(first managedDoltSQLHealthReport) func(_, _, _ string) (managedDoltSQLHealthReport, error) {
	calls := 0
	return func(_, _, _ string) (managedDoltSQLHealthReport, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false"}, nil
	}
}

func TestRecoverManagedDolt_JournalRecordsImmediateStops(t *testing.T) {
	tests := []struct {
		name      string
		configure func(f *unresponsiveRecoveryFixture, ops *managedDoltRecoveryOps)
		want      []string
	}{
		{
			name:      "zero window",
			configure: func(f *unresponsiveRecoveryFixture, _ *managedDoltRecoveryOps) { f.window = 0 },
			want:      []string{`reason="dolt query probe: context deadline exceeded after 5s"`, "elapsed=n/a", "window=0s", "pid=n/a", "port=3306"},
		},
		{
			name:      "dead or refusing server",
			configure: func(f *unresponsiveRecoveryFixture, _ *managedDoltRecoveryOps) { f.live = false },
			want:      []string{`reason="dolt query probe: context deadline exceeded after 5s"`, "elapsed=n/a", "window=3m0s", "pid=n/a", "port=3306"},
		},
		{
			name: "read-only server",
			configure: func(f *unresponsiveRecoveryFixture, ops *managedDoltRecoveryOps) {
				f.probeErr = nil
				ops.healthCheck = firstHealthThenReady(managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "true"})
			},
			want: []string{`reason="server is read-only"`, "elapsed=n/a", "window=n/a", "pid=n/a", "port=3306"},
		},
		{
			name: "not query-ready server",
			configure: func(f *unresponsiveRecoveryFixture, ops *managedDoltRecoveryOps) {
				f.probeErr = nil
				ops.healthCheck = firstHealthThenReady(managedDoltSQLHealthReport{QueryReady: false, ReadOnly: "unknown"})
			},
			want: []string{`reason="server is not ready (query_ready=false read_only=unknown)"`, "elapsed=n/a", "window=n/a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(managedDoltRecoverCallerEnv, "bd-retry")
			f := newUnresponsiveRecoveryFixture(t)
			ops := f.ops()
			tt.configure(f, &ops)

			if _, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops); err != nil {
				t.Fatalf("recover error = %v, want the existing restart", err)
			}
			lines := f.journal()
			if len(lines) != 1 {
				t.Fatalf("journal lines = %d, want one stop:\n%s", len(lines), strings.Join(lines, "\n"))
			}
			assertJournalLine(t, lines[0], f.now, append([]string{"action=stop", `caller="bd-retry"`}, tt.want...)...)
		})
	}
}

func TestRecoverManagedDolt_JournalFailureIsExposedWithoutHidingTheOutcome(t *testing.T) {
	t.Run("deferral", func(t *testing.T) {
		f := newUnresponsiveRecoveryFixture(t)
		if err := os.MkdirAll(f.layout.LogFile, 0o755); err != nil {
			t.Fatalf("make journal unwritable: %v", err)
		}

		_, err := f.recover()

		var deferred *managedDoltRecoveryDeferredError
		if !errors.As(err, &deferred) || !errors.Is(err, f.probeErr) {
			t.Fatalf("recover error = %v, want the deferral wrapping the probe failure", err)
		}
		if !strings.Contains(err.Error(), "journal") || !strings.Contains(err.Error(), f.layout.LogFile) {
			t.Fatalf("recover error = %q, want the journal failure for %s", err, f.layout.LogFile)
		}
		if n := f.destructiveCalls(); n != 0 {
			t.Fatalf("destructive calls = %d, want 0", n)
		}
	})
	t.Run("stop", func(t *testing.T) {
		f := newUnresponsiveRecoveryFixture(t)
		f.window = 0
		if err := os.MkdirAll(f.layout.LogFile, 0o755); err != nil {
			t.Fatalf("make journal unwritable: %v", err)
		}
		ops := f.ops()
		var diag bytes.Buffer
		ops.diag = &diag

		report, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops)
		if err != nil {
			t.Fatalf("recover error = %v, want the restart to proceed", err)
		}
		if f.stopCalls != 1 || !report.Restarted {
			t.Fatalf("stop=%d restarted=%v, want one restart", f.stopCalls, report.Restarted)
		}
		if !strings.Contains(diag.String(), "journal") || !strings.Contains(diag.String(), f.layout.LogFile) {
			t.Fatalf("diagnostics = %q, want the journal failure for %s", diag.String(), f.layout.LogFile)
		}
	})
	t.Run("reset", func(t *testing.T) {
		f := newUnresponsiveRecoveryFixture(t)
		cooldown := seedUnresponsiveHistory(t, f.layout, f.identity, f.now.Add(-60*time.Second), f.now.Add(-30*time.Second), 2)
		f.probeErr = nil
		if err := os.MkdirAll(f.layout.LogFile, 0o755); err != nil {
			t.Fatalf("make journal unwritable: %v", err)
		}
		ops := f.ops()
		var diag bytes.Buffer
		ops.diag = &diag

		report, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops)
		if err != nil || !report.Healthy {
			t.Fatalf("recover = %+v, %v, want the healthy return to stand", report, err)
		}
		assertStreakClearedCooldownKept(t, f.layout, cooldown)
		if !strings.Contains(diag.String(), "record managed dolt recovery reset") || !strings.Contains(diag.String(), f.layout.LogFile) {
			t.Fatalf("diagnostics = %q, want the reset journal failure for %s", diag.String(), f.layout.LogFile)
		}
	})
}

func TestRecoverManagedDolt_JournalRecordsHealthyReset(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	cooldown := seedUnresponsiveHistory(t, f.layout, f.identity, f.now.Add(-60*time.Second), f.now.Add(-30*time.Second), 2)
	f.probeErr = nil

	if _, err := f.recover(); err != nil {
		t.Fatalf("recover error = %v, want a healthy return", err)
	}

	assertStreakClearedCooldownKept(t, f.layout, cooldown)
	lines := f.journal()
	if len(lines) != 1 {
		t.Fatalf("journal lines = %d, want one reset:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, field := range []string{"action=reset", `reason="healthy observation; provider cooldown preserved"`, `caller="direct-helper"`} {
		if !strings.Contains(lines[0], field) {
			t.Errorf("journal line %q is missing %q", lines[0], field)
		}
	}
}

func TestRecoverManagedDolt_HealthyCheckWithoutStreakJournalsNothing(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	f.probeErr = nil

	if _, err := f.recover(); err != nil {
		t.Fatalf("recover error = %v, want a healthy return", err)
	}

	if data, err := os.ReadFile(f.layout.LogFile); err == nil && strings.Contains(string(data), "gc dolt recovery:") {
		t.Fatalf("journal = %q, want no reset recorded when no streak was pending", data)
	}
}

func TestRecoverManagedDolt_JournalRecordsConfigDiagnostic(t *testing.T) {
	f := newUnresponsiveRecoveryFixture(t)
	f.windowDiagnostics = []string{`daemon.dolt_unresponsive_restart_after "soon": invalid duration; using default 3m0s`}
	ops := f.ops()
	var diag bytes.Buffer
	ops.diag = &diag

	_, err := recoverManagedDoltProcessWithOps(f.cityPath, "127.0.0.1", "3306", "root", "warning", 2*time.Second, ops)

	var deferred *managedDoltRecoveryDeferredError
	if !errors.As(err, &deferred) {
		t.Fatalf("recover error = %v, want a deferral", err)
	}
	if !strings.Contains(diag.String(), "using default 3m0s") {
		t.Errorf("diagnostics = %q, want the config fallback reported", diag.String())
	}
	lines := f.journal()
	if len(lines) != 2 {
		t.Fatalf("journal lines = %d, want the config diagnostic then the deferral:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	assertJournalLine(t, lines[0], f.now, "action=config-diagnostic", `dolt_unresponsive_restart_after \"soon\"`, "using default 3m0s", `caller="direct-helper"`)
	assertJournalLine(t, lines[1], f.now, "action=defer", "window=3m0s")
}

func writeCallerRecordingProviderScript(t *testing.T, cityPath string) string {
	t.Helper()
	callerFile := filepath.Join(t.TempDir(), "recover-caller")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  health) echo unhealthy >&2; exit 1 ;;\n" +
		"  recover) printf '%s\\n' \"${" + managedDoltRecoverCallerEnv + ":-unset}\" >> \"" + callerFile + "\"; exit 0 ;;\n" +
		"  *) exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return callerFile
}

func TestHealthBeadsProviderRecoverTagsTheCaller(t *testing.T) {
	t.Setenv(managedDoltRecoverCallerEnv, "")
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	writeBreakerAwarePreflightFakes(t, cityPath, "unhealthy")
	callerFile := writeCallerRecordingProviderScript(t, cityPath)

	_ = healthBeadsProvider(cityPath)

	data, err := os.ReadFile(callerFile)
	if err != nil {
		t.Fatalf("provider recover did not run: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "provider-health" {
		t.Fatalf("%s seen by the provider recover op = %q, want provider-health", managedDoltRecoverCallerEnv, got)
	}
}

func TestRecoverManagedBDCommandTagsTheCaller(t *testing.T) {
	t.Setenv(managedDoltRecoverCallerEnv, "")
	cityPath := t.TempDir()
	callerFile := writeCallerRecordingProviderScript(t, cityPath)

	if err := recoverManagedBDCommand(cityPath); err != nil {
		t.Fatalf("recoverManagedBDCommand() error = %v", err)
	}
	data, err := os.ReadFile(callerFile)
	if err != nil {
		t.Fatalf("bd retry recover did not run: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "bd-retry" {
		t.Fatalf("%s seen by the bd retry recover op = %q, want bd-retry", managedDoltRecoverCallerEnv, got)
	}
}
