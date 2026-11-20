package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// recoveryGateHarness drives recoverManagedDoltProcessWithOps with injected
// SQL, endpoint and identity observations and records every lifecycle call.
type recoveryGateHarness struct {
	t        *testing.T
	city     string
	port     string
	now      time.Time
	probeErr error
	health   managedDoltSQLHealthReport
	healthEr error
	endpoint managedDoltEndpointObservation
	ident    managedDoltServerIncarnation
	identErr error
	window   time.Duration
	calls    []string
}

func newRecoveryGateHarness(t *testing.T) *recoveryGateHarness {
	t.Helper()
	return &recoveryGateHarness{
		t:        t,
		city:     setupRecoveryTestCity(t),
		port:     "3311",
		now:      time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
		probeErr: fmt.Errorf("query probe: %w", context.DeadlineExceeded),
		health:   managedDoltSQLHealthReport{QueryReady: true, ReadOnly: "false"},
		endpoint: managedDoltEndpointObservation{State: managedDoltEndpointListening},
		ident:    managedDoltServerIncarnation{PID: 4242, StartTime: "777"},
		window:   180 * time.Second,
	}
}

func (h *recoveryGateHarness) ops() managedDoltRecoveryOps {
	return managedDoltRecoveryOps{
		queryProbe: func(string, string, string) error { h.calls = append(h.calls, "query"); return h.probeErr },
		healthCheck: func(string, string, string) (managedDoltSQLHealthReport, error) {
			h.calls = append(h.calls, "health")
			return h.health, h.healthEr
		},
		stop: func(string, string) (managedDoltStopReport, error) {
			h.calls = append(h.calls, "stop")
			return managedDoltStopReport{HadPID: true, PID: h.ident.PID}, nil
		},
		preflightCleanup: func(string) error { h.calls = append(h.calls, "preflight"); return nil },
		start: func(string, string, string, string, string, time.Duration) (managedDoltStartReport, error) {
			h.calls = append(h.calls, "start")
			return managedDoltStartReport{Ready: true, PID: 5151, Port: 3311}, nil
		},
		publish:       func(string) error { h.calls = append(h.calls, "publish"); return nil },
		failedCleanup: func(_ string, _, _ int, cause error) error { h.calls = append(h.calls, "failed-cleanup"); return cause },
		observeEndpoint: func(string, string) managedDoltEndpointObservation {
			h.calls = append(h.calls, "observe")
			return h.endpoint
		},
		identify: func(string, string) (managedDoltServerIncarnation, error) {
			return h.ident, h.identErr
		},
		restartAfter: func(string) (time.Duration, error) { return h.window, nil },
		now:          func() time.Time { return h.now },
	}
}

func (h *recoveryGateHarness) recover() (managedDoltRecoverReport, error) {
	h.t.Helper()
	h.calls = nil
	return recoverManagedDoltProcessWithOps(h.city, "127.0.0.1", h.port, "root", "warning", 2*time.Second, h.ops())
}

func (h *recoveryGateHarness) restarted() bool {
	for _, c := range h.calls {
		if c == "stop" || c == "start" {
			return true
		}
	}
	return false
}

func (h *recoveryGateHarness) logFile() string {
	h.t.Helper()
	layout, err := resolveManagedDoltRuntimeLayout(h.city)
	if err != nil {
		h.t.Fatal(err)
	}
	return layout.LogFile
}

func (h *recoveryGateHarness) logText() string {
	h.t.Helper()
	data, err := os.ReadFile(h.logFile())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		h.t.Fatal(err)
	}
	return string(data)
}

func assertDeferredWithoutRestart(t *testing.T, h *recoveryGateHarness, report managedDoltRecoverReport, err error) {
	t.Helper()
	if h.restarted() {
		t.Fatalf("recovery stopped or started the server: calls=%v report=%+v err=%v", h.calls, report, err)
	}
	if err == nil {
		t.Fatalf("recovery returned nil error for a deferred unhealthy server (report=%+v)", report)
	}
	if !report.Deferred || report.Restarted || report.Healthy {
		t.Fatalf("report = %+v, want Deferred=true Restarted=false Healthy=false", report)
	}
}

// AC-1: the first failed observation of a listening server defers, whether
// the query probe times out or the following health check fails to respond,
// and whether or not runtime metadata names the server.
func TestRecoverManagedDoltFirstFailedObservationOfListenerDefers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *recoveryGateHarness)
	}{
		{name: "query probe timeout", setup: func(*recoveryGateHarness) {}},
		{name: "health check fails to respond", setup: func(h *recoveryGateHarness) {
			h.probeErr = nil
			h.healthEr = fmt.Errorf("health probe: %w", context.DeadlineExceeded)
		}},
		{name: "query probe timeout with runtime metadata", setup: func(h *recoveryGateHarness) {
			port, _ := strconv.Atoi(h.port)
			writeRecoveryRuntimeState(h.t, h.city, h.ident.PID, port)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecoveryGateHarness(t)
			tc.setup(h)
			report, err := h.recover()
			assertDeferredWithoutRestart(t, h, report, err)
			if !strings.Contains(h.logText(), "decision=defer") {
				t.Fatalf("dolt.log has no defer decision:\n%s", h.logText())
			}
		})
	}
}

// AC-1 through the live endpoint observer: a real listener with no runtime
// metadata at all is evidence to defer, not permission to stop.
func TestRecoverManagedDoltLiveListenerWithoutMetadataDefers(t *testing.T) {
	h := newRecoveryGateHarness(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	h.port = strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ops := h.ops()
	ops.observeEndpoint = nil
	ops.identify = nil
	h.calls = nil
	report, err := recoverManagedDoltProcessWithOps(h.city, "127.0.0.1", h.port, "root", "warning", 2*time.Second, ops)
	assertDeferredWithoutRestart(t, h, report, err)
}

// seedRecoveryRecord appends a recovery record for the harness city/endpoint.
func (h *recoveryGateHarness) seedRecoveryRecord(rec managedDoltRecoveryRecord) {
	h.t.Helper()
	if rec.Scope == "" {
		rec.Scope = normalizePathForCompare(h.city)
	}
	if rec.Endpoint == "" {
		rec.Endpoint = net.JoinHostPort("127.0.0.1", h.port)
	}
	if rec.Caller == "" {
		rec.Caller = "seed"
	}
	if rec.Window == 0 {
		rec.Window = h.window
	}
	if err := appendManagedDoltRecoveryRecord(h.logFile(), rec); err != nil {
		h.t.Fatal(err)
	}
}

func (h *recoveryGateHarness) seedDefer(at, since time.Time, ident managedDoltServerIncarnation) {
	h.seedRecoveryRecord(managedDoltRecoveryRecord{
		At: at, Decision: managedDoltRecoveryDecisionDefer, Reason: managedDoltRecoveryReasonFirstObservation,
		PID: ident.PID, StartTime: ident.StartTime, UnresponsiveSince: since,
	})
}

// lastRecoveryRecord parses the newest recovery record in dolt.log.
func (h *recoveryGateHarness) lastRecoveryRecord() managedDoltRecoveryRecord {
	h.t.Helper()
	history, err := readManagedDoltRecoveryHistory(h.logFile(), h.now)
	if err != nil {
		h.t.Fatal(err)
	}
	if len(history.Records) == 0 {
		h.t.Fatalf("dolt.log has no recovery records:\n%s", h.logText())
	}
	return history.Records[len(history.Records)-1]
}

// AC-2: repeated failures of the same verified incarnation keep the original
// streak start while inside the window, and record a stop decision then enter
// the existing recovery path once the window has elapsed.
func TestRecoverManagedDoltSameIncarnationStreakGatesRestart(t *testing.T) {
	t.Run("inside the window preserves the original streak and defers", func(t *testing.T) {
		h := newRecoveryGateHarness(t)
		since := h.now.Add(-100 * time.Second)
		h.seedDefer(since, since, h.ident)
		h.seedDefer(h.now.Add(-40*time.Second), since, h.ident)
		report, err := h.recover()
		assertDeferredWithoutRestart(t, h, report, err)
		last := h.lastRecoveryRecord()
		if last.Decision != managedDoltRecoveryDecisionDefer || !last.UnresponsiveSince.Equal(since) {
			t.Fatalf("last record = %+v, want defer with original since %s", last, since)
		}
		if last.Reason != managedDoltRecoveryReasonWithinWindow {
			t.Fatalf("last reason = %q, want %q", last.Reason, managedDoltRecoveryReasonWithinWindow)
		}
	})
	for _, elapsed := range []time.Duration{180 * time.Second, 400 * time.Second} {
		t.Run("at or after the window records stop and restarts/"+elapsed.String(), func(t *testing.T) {
			h := newRecoveryGateHarness(t)
			since := h.now.Add(-elapsed)
			h.seedDefer(since, since, h.ident)
			h.seedDefer(h.now.Add(-60*time.Second), since, h.ident)
			report, err := h.recover()
			if err != nil {
				t.Fatalf("recover: %v (calls=%v)", err, h.calls)
			}
			if !report.Restarted || report.Deferred {
				t.Fatalf("report = %+v, want Restarted=true Deferred=false", report)
			}
			if strings.Join(h.calls, ",") != "query,observe,observe,stop,preflight,start,health,publish" {
				t.Fatalf("calls = %v, want a re-verified observation before stop/start", h.calls)
			}
			last := h.lastRecoveryRecord()
			if last.Decision != managedDoltRecoveryDecisionStop || !last.UnresponsiveSince.Equal(since) || last.PID != h.ident.PID {
				t.Fatalf("last record = %+v, want stop decision for pid %d since %s", last, h.ident.PID, since)
			}
		})
	}
	t.Run("a changed incarnation starts a fresh window", func(t *testing.T) {
		h := newRecoveryGateHarness(t)
		since := h.now.Add(-400 * time.Second)
		h.seedDefer(since, since, managedDoltServerIncarnation{PID: h.ident.PID, StartTime: "older-boot"})
		report, err := h.recover()
		assertDeferredWithoutRestart(t, h, report, err)
		last := h.lastRecoveryRecord()
		if !last.UnresponsiveSince.Equal(h.now) || last.Reason != managedDoltRecoveryReasonFirstObservation {
			t.Fatalf("last record = %+v, want a fresh streak at %s", last, h.now)
		}
	})
	t.Run("a different endpoint's streak does not count", func(t *testing.T) {
		h := newRecoveryGateHarness(t)
		since := h.now.Add(-400 * time.Second)
		h.seedRecoveryRecord(managedDoltRecoveryRecord{
			At: since, Decision: managedDoltRecoveryDecisionDefer, Reason: managedDoltRecoveryReasonFirstObservation,
			Endpoint: "127.0.0.1:9999", PID: h.ident.PID, StartTime: h.ident.StartTime, UnresponsiveSince: since,
		})
		report, err := h.recover()
		assertDeferredWithoutRestart(t, h, report, err)
	})
	t.Run("an elapsed window whose re-verification changes identity defers", func(t *testing.T) {
		h := newRecoveryGateHarness(t)
		since := h.now.Add(-400 * time.Second)
		h.seedDefer(h.now.Add(-60*time.Second), since, h.ident)
		ops := h.ops()
		identifyCalls := 0
		ops.identify = func(string, string) (managedDoltServerIncarnation, error) {
			identifyCalls++
			if identifyCalls > 1 {
				return managedDoltServerIncarnation{PID: 9191, StartTime: "replacement"}, nil
			}
			return h.ident, nil
		}
		h.calls = nil
		report, err := recoverManagedDoltProcessWithOps(h.city, "127.0.0.1", h.port, "root", "warning", 2*time.Second, ops)
		assertDeferredWithoutRestart(t, h, report, err)
	})
	t.Run("zero window restarts a positively observed listener at once", func(t *testing.T) {
		h := newRecoveryGateHarness(t)
		h.window = 0
		report, err := h.recover()
		if err != nil || !report.Restarted {
			t.Fatalf("report = %+v err = %v, want restart with a zero window", report, err)
		}
		if last := h.lastRecoveryRecord(); last.Decision != managedDoltRecoveryDecisionStop {
			t.Fatalf("last record = %+v, want stop decision", last)
		}
	})
}

// AC-3: a healthy measurement of the same server closes an open streak, so a
// later failure starts a fresh window instead of inheriting the old start.
func TestRecoverManagedDoltHealthyObservationClosesStreak(t *testing.T) {
	t.Run("a healthy recovery closes the streak", func(t *testing.T) {
		h := newRecoveryGateHarness(t)
		since := h.now.Add(-170 * time.Second)
		h.seedDefer(since, since, h.ident)
		h.probeErr = nil
		report, err := h.recover()
		if err != nil || !report.Healthy || h.restarted() {
			t.Fatalf("healthy recovery: report=%+v err=%v calls=%v", report, err, h.calls)
		}
		if last := h.lastRecoveryRecord(); last.Decision != managedDoltRecoveryDecisionHealthy || last.PID != h.ident.PID {
			t.Fatalf("last record = %+v, want healthy record for pid %d", last, h.ident.PID)
		}

		h.now = h.now.Add(20 * time.Second)
		h.probeErr = fmt.Errorf("query probe: %w", context.DeadlineExceeded)
		report, err = h.recover()
		assertDeferredWithoutRestart(t, h, report, err)
		last := h.lastRecoveryRecord()
		if !last.UnresponsiveSince.Equal(h.now) || last.Reason != managedDoltRecoveryReasonFirstObservation {
			t.Fatalf("last record = %+v, want a fresh streak at %s", last, h.now)
		}
	})
	t.Run("a healthy recovery without an open streak records nothing", func(t *testing.T) {
		h := newRecoveryGateHarness(t)
		h.probeErr = nil
		if report, err := h.recover(); err != nil || !report.Healthy {
			t.Fatalf("healthy recovery: report=%+v err=%v", report, err)
		}
		if strings.Contains(h.logText(), managedDoltRecoveryRecordTag) {
			t.Fatalf("dolt.log gained a recovery record without an open streak:\n%s", h.logText())
		}
	})
	t.Run("provider health closes the streak after revalidating under the lock", func(t *testing.T) {
		h := newRecoveryGateHarness(t)
		since := h.now.Add(-170 * time.Second)
		h.seedDefer(since, since, h.ident)
		h.probeErr = nil
		if err := closeManagedDoltStreakAfterProviderHealth(h.city, h.port, "root", h.ops()); err != nil {
			t.Fatalf("closeManagedDoltStreakAfterProviderHealth: %v", err)
		}
		if strings.Join(h.calls, ",") != "query,health" {
			t.Fatalf("calls = %v, want a revalidating query and health check", h.calls)
		}
		if last := h.lastRecoveryRecord(); last.Decision != managedDoltRecoveryDecisionHealthy {
			t.Fatalf("last record = %+v, want healthy record", last)
		}
	})
	t.Run("provider health that fails revalidation leaves the streak open", func(t *testing.T) {
		h := newRecoveryGateHarness(t)
		since := h.now.Add(-170 * time.Second)
		h.seedDefer(since, since, h.ident)
		if err := closeManagedDoltStreakAfterProviderHealth(h.city, h.port, "root", h.ops()); err != nil {
			t.Fatalf("closeManagedDoltStreakAfterProviderHealth: %v", err)
		}
		if last := h.lastRecoveryRecord(); last.Decision != managedDoltRecoveryDecisionDefer {
			t.Fatalf("last record = %+v, want the open defer streak kept", last)
		}
	})
	t.Run("provider health skips the close while another lifecycle holds the lock", func(t *testing.T) {
		h := newRecoveryGateHarness(t)
		since := h.now.Add(-170 * time.Second)
		h.seedDefer(since, since, h.ident)
		h.probeErr = nil
		lockFile, _, err := openManagedDoltLifecycleLock(h.city)
		if err != nil {
			t.Fatal(err)
		}
		if locked, err := tryManagedDoltLifecycleLock(lockFile); err != nil || !locked {
			t.Fatalf("tryManagedDoltLifecycleLock = %v, %v", locked, err)
		}
		defer releaseManagedDoltLifecycleLock(lockFile)
		if err := closeManagedDoltStreakAfterProviderHealth(h.city, h.port, "root", h.ops()); err != nil {
			t.Fatalf("closeManagedDoltStreakAfterProviderHealth: %v", err)
		}
		if len(h.calls) != 0 {
			t.Fatalf("calls = %v, want none while the lifecycle lock is held", h.calls)
		}
		if last := h.lastRecoveryRecord(); last.Decision != managedDoltRecoveryDecisionDefer {
			t.Fatalf("last record = %+v, want the open defer streak kept", last)
		}
	})
}

// AC-3: keeping an unmeasured server after an inconclusive readiness wait is
// not a healthy result and leaves an open streak untouched.
func TestRecoverManagedDoltInconclusiveReuseKeepsStreak(t *testing.T) {
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dir): %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}
	oldPort := reserveRandomTCPPort(t)
	newPort := reserveRandomTCPPort(t)
	for newPort == oldPort {
		newPort = reserveRandomTCPPort(t)
	}
	listener := startTCPListenerProcessInDir(t, newPort, layout.DataDir)
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(listener.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	state := doltRuntimeState{
		Running:   true,
		PID:       listener.Process.Pid,
		Port:      newPort,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, state); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(layout): %v", err)
	}
	if err := writeDoltRuntimeStateFile(managedDoltStatePath(cityPath), state); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile(published): %v", err)
	}
	binDir := t.TempDir()
	writeFakeDoltSQLBinary(t, binDir, filepath.Join(t.TempDir(), "dolt-invocation"), `#!/bin/sh
case "${1:-}" in
  config) exit 0 ;;
  *) exit 1 ;;
esac
`)
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	stubManagedDoltDial(t, func(string, string, time.Duration) (net.Conn, error) {
		return nil, dialErrorFor("connect", syscall.EADDRNOTAVAIL)
	})
	since := time.Now().UTC().Add(-time.Minute)
	if err := appendManagedDoltRecoveryRecord(layout.LogFile, managedDoltRecoveryRecord{
		At: since, Decision: managedDoltRecoveryDecisionDefer, Reason: managedDoltRecoveryReasonFirstObservation,
		Scope: normalizePathForCompare(cityPath), Endpoint: net.JoinHostPort("127.0.0.1", strconv.Itoa(newPort)),
		PID: listener.Process.Pid, StartTime: "1", UnresponsiveSince: since, Window: 180 * time.Second, Caller: "seed",
	}); err != nil {
		t.Fatal(err)
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", strconv.Itoa(oldPort), "root", "warning", 2*time.Second)
	if err != nil {
		t.Fatalf("recoverManagedDoltProcess() error = %v", err)
	}
	if !report.ReadyInconclusive || report.Healthy {
		t.Fatalf("report = %+v, want an inconclusive reuse", report)
	}
	history, err := readManagedDoltRecoveryHistory(layout.LogFile, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Records) != 1 || history.Records[0].Decision != managedDoltRecoveryDecisionDefer {
		t.Fatalf("history = %+v, want only the open defer streak", history.Records)
	}
}
