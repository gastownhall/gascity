package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"
)

// A single timed-out SQL probe against an owned server that is still
// listening must report the store unhealthy without stopping the server.
func TestRecoverManagedDolt_ProbeTimeoutOnLiveListenerDoesNotStop(t *testing.T) {
	cityPath := setupRecoveryTestCity(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	var stopCalls, startCalls int
	ops := managedDoltRecoveryOps{
		queryProbe: func(_, _, _ string) error {
			return fmt.Errorf("query probe: %w", context.DeadlineExceeded)
		},
		healthCheck: func(_, _, _ string) (managedDoltSQLHealthReport, error) {
			return managedDoltSQLHealthReport{}, fmt.Errorf("health probe: %w", context.DeadlineExceeded)
		},
		stop: func(_, _ string) (managedDoltStopReport, error) {
			stopCalls++
			return managedDoltStopReport{HadPID: true, PID: 111}, nil
		},
		preflightCleanup: func(string) error { return nil },
		start: func(_, _, _, _, _ string, _ time.Duration) (managedDoltStartReport, error) {
			startCalls++
			return managedDoltStartReport{Ready: true, PID: 222}, nil
		},
		publish:       func(string) error { return nil },
		failedCleanup: func(_ string, _, _ int, cause error) error { return cause },
	}

	report, err := recoverManagedDoltProcessWithOps(cityPath, "127.0.0.1", port, "root", "warning", 2*time.Second, ops)
	if stopCalls != 0 || startCalls != 0 {
		t.Fatalf("one timed-out probe on a live listener restarted the server: stop=%d start=%d report=%+v err=%v", stopCalls, startCalls, report, err)
	}
	if err == nil {
		t.Fatalf("recover returned nil error; want the probe timeout reported as unhealthy (report=%+v)", report)
	}
	if report.Restarted {
		t.Fatalf("report.Restarted = true; want false")
	}
}
