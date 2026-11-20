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
