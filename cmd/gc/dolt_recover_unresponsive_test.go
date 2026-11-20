package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
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
