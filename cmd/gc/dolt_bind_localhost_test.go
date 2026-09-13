package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestNormalizeManagedDoltBindHost pins the managed-server bind default:
// a blank host means "no explicit choice" and must resolve to loopback —
// the production work ledger must never listen on a wildcard interface
// unless the operator explicitly opts in. Explicit values (including the
// 0.0.0.0 wildcard opt-out for multi-host deployments) pass through.
func TestNormalizeManagedDoltBindHost(t *testing.T) {
	cases := []struct {
		name string
		host string
		want string
	}{
		{"blank defaults to loopback", "", "127.0.0.1"},
		{"whitespace defaults to loopback", "   ", "127.0.0.1"},
		{"explicit loopback preserved", "127.0.0.1", "127.0.0.1"},
		{"explicit wildcard opt-out preserved", "0.0.0.0", "0.0.0.0"},
		{"explicit interface preserved", "192.168.1.5", "192.168.1.5"},
		{"surrounding whitespace trimmed", " 10.0.0.7 ", "10.0.0.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeManagedDoltBindHost(tc.host); got != tc.want {
				t.Fatalf("normalizeManagedDoltBindHost(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

// TestStartManagedDoltProcessWithOptions_BlankHostBindsLoopbackByDefault
// drives the production start path with NO host configured and asserts the
// rendered dolt-config.yaml binds the listener to 127.0.0.1 — not the
// wildcard interface (the pre-P0.5 default left the work ledger
// LAN-reachable with permissive auth).
func TestStartManagedDoltProcessWithOptions_BlankHostBindsLoopbackByDefault(t *testing.T) {
	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, _, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			return managedDoltStartedProcess{CityPath: cityPath, PID: 0}, nil
		},
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			return managedDoltWaitReadyReport{Ready: true, PIDAlive: true}, nil
		},
		logSuffixFn: func(_ string, _ int64) (string, error) {
			return "", nil
		},
		portAvailableFn: func(_ string, _ int) bool { return true },
		retryWindow:     time.Second,
	})

	report, err := startManagedDoltProcessWithOptions(cityPath, "", "17790", "root", "warning", -1, time.Second, false)
	if err != nil {
		t.Fatalf("startManagedDoltProcessWithOptions: %v", err)
	}
	if !report.Ready {
		t.Fatalf("report.Ready = false, want true")
	}

	text := readManagedDoltConfigForBindTest(t, cityPath)
	if !strings.Contains(text, "host: 127.0.0.1") {
		t.Errorf("default bind must be loopback; dolt-config.yaml missing %q:\n%s", "host: 127.0.0.1", text)
	}
	if strings.Contains(text, "host: 0.0.0.0") {
		t.Errorf("default bind leaked the wildcard interface:\n%s", text)
	}
}

// TestStartManagedDoltProcessWithOptions_ExplicitWildcardBindPreserved pins
// the multi-host opt-out: an operator who explicitly sets the wildcard host
// keeps the old LAN-reachable bind.
func TestStartManagedDoltProcessWithOptions_ExplicitWildcardBindPreserved(t *testing.T) {
	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, _, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			return managedDoltStartedProcess{CityPath: cityPath, PID: 0}, nil
		},
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			return managedDoltWaitReadyReport{Ready: true, PIDAlive: true}, nil
		},
		logSuffixFn: func(_ string, _ int64) (string, error) {
			return "", nil
		},
		portAvailableFn: func(_ string, _ int) bool { return true },
		retryWindow:     time.Second,
	})

	report, err := startManagedDoltProcessWithOptions(cityPath, "0.0.0.0", "17791", "root", "warning", -1, time.Second, false)
	if err != nil {
		t.Fatalf("startManagedDoltProcessWithOptions: %v", err)
	}
	if !report.Ready {
		t.Fatalf("report.Ready = false, want true")
	}

	text := readManagedDoltConfigForBindTest(t, cityPath)
	if !strings.Contains(text, "host: 0.0.0.0") {
		t.Errorf("explicit wildcard opt-out must be preserved; dolt-config.yaml missing %q:\n%s", "host: 0.0.0.0", text)
	}
}

func readManagedDoltConfigForBindTest(t *testing.T, cityPath string) string {
	t.Helper()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	data, err := os.ReadFile(layout.ConfigFile)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", layout.ConfigFile, err)
	}
	return string(data)
}
