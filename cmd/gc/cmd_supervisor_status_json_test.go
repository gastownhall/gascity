package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestSupervisorStatusJSON(t *testing.T) {
	clearGCEnv(t)

	// Stub fallback hooks so a live service manager or API on the test host
	// cannot flip running=true and mask the socket-path-not-found path.
	origSM := supervisorServiceManagerActive
	supervisorServiceManagerActive = func() bool { return false }
	t.Cleanup(func() { supervisorServiceManagerActive = origSM })
	origAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() { supervisorAPIReachable = origAPI })

	var stdout, stderr bytes.Buffer
	code := run([]string{"supervisor", "status", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(supervisor status --json) = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var payload struct {
		SchemaVersion string   `json:"schema_version"`
		Running       bool     `json:"running"`
		PID           int      `json:"pid"`
		SocketPath    string   `json:"socket_path"`
		CheckedPaths  []string `json:"checked_paths"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.SchemaVersion != "1" {
		t.Fatalf("schema_version = %q, want 1", payload.SchemaVersion)
	}
	if len(payload.CheckedPaths) == 0 {
		t.Fatalf("checked_paths empty: %+v", payload)
	}
	if !payload.Running && payload.PID != 0 {
		t.Fatalf("not running with pid = %d", payload.PID)
	}
}

// TestSupervisorStatusJSON_RunningViaLaunchdWhenSocketDown verifies that when
// the launchd service manager reports active but the control socket is
// unreachable, status reports running=true with distinct diagnostic fields.
// FAILS on HEAD (before the gascity#2984 fix); PASSES after.
func TestSupervisorStatusJSON_RunningViaLaunchdWhenSocketDown(t *testing.T) {
	clearGCEnv(t)

	origGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "darwin"
	t.Cleanup(func() { supervisorRuntimeGOOS = origGOOS })

	origActive := supervisorLaunchdActive
	supervisorLaunchdActive = func(string) bool { return true }
	t.Cleanup(func() { supervisorLaunchdActive = origActive })

	// supervisorServiceManagerActive calls supervisorLaunchdActive on darwin,
	// so the real implementation works here — no need to stub it.

	var stdout, stderr bytes.Buffer
	code := run([]string{"supervisor", "status", "--json"}, &stdout, &stderr)
	var p struct {
		Running      bool   `json:"running"`
		PID          int    `json:"pid"`
		PidSource    string `json:"pid_source"`
		SocketStatus string `json:"socket_status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, stdout.String())
	}
	if !p.Running {
		t.Fatalf("launchd active but status running=false (pid=%d exit=%d): %s", p.PID, code, stdout.String())
	}
	if p.PID == 0 && (p.PidSource != "service_manager" || p.SocketStatus != "unreachable") {
		t.Fatalf("want distinct diagnostic state pid_source=service_manager socket_status=unreachable, got %+v", p)
	}
	if code != 0 {
		t.Fatalf("exit=%d, want 0", code)
	}
}

// TestSupervisorStatusJSON_RunningViaAPIWhenSocketAndServiceDown verifies that
// when both the socket and service manager are down but the HTTP API answers,
// status reports running=true via the api fallback.
func TestSupervisorStatusJSON_RunningViaAPIWhenSocketAndServiceDown(t *testing.T) {
	clearGCEnv(t)

	origSM := supervisorServiceManagerActive
	supervisorServiceManagerActive = func() bool { return false }
	t.Cleanup(func() { supervisorServiceManagerActive = origSM })

	origAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return true }
	t.Cleanup(func() { supervisorAPIReachable = origAPI })

	var stdout, stderr bytes.Buffer
	code := run([]string{"supervisor", "status", "--json"}, &stdout, &stderr)
	var p struct {
		Running      bool   `json:"running"`
		PID          int    `json:"pid"`
		PidSource    string `json:"pid_source"`
		SocketStatus string `json:"socket_status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, stdout.String())
	}
	if !p.Running {
		t.Fatalf("API reachable but status running=false (exit=%d): %s", code, stdout.String())
	}
	if p.PidSource != "api" {
		t.Fatalf("want pid_source=api, got %q", p.PidSource)
	}
	if p.SocketStatus != "unreachable" {
		t.Fatalf("want socket_status=unreachable, got %q", p.SocketStatus)
	}
	if code != 0 {
		t.Fatalf("exit=%d, want 0", code)
	}
}

// TestSupervisorStatusJSON_NotRunningWhenAllSignalsDown verifies that when
// all three liveness signals (socket, service manager, API) are down,
// the text path prints "Supervisor is not running" and exits 1.
func TestSupervisorStatusJSON_NotRunningWhenAllSignalsDown(t *testing.T) {
	clearGCEnv(t)

	origSM := supervisorServiceManagerActive
	supervisorServiceManagerActive = func() bool { return false }
	t.Cleanup(func() { supervisorServiceManagerActive = origSM })

	origAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() { supervisorAPIReachable = origAPI })

	var stdout, stderr bytes.Buffer
	code := run([]string{"supervisor", "status"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stdout=%q", code, stdout.String())
	}
	if got := stdout.String(); got != "Supervisor is not running\n" {
		t.Fatalf("stdout=%q, want %q", got, "Supervisor is not running\n")
	}
}

// TestSupervisorAPIReachable_StatusFiltering drives the real
// supervisorAPIReachable probe (not the boolean stub) against an httptest
// server, asserting that only a 2xx on /v0/cities counts as reachable. A 404
// (e.g. an unrelated local service on the configured port) must report false.
func TestSupervisorAPIReachable_StatusFiltering(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want bool
	}{
		{"ok_200", http.StatusOK, true},
		{"notfound_404", http.StatusNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v0/cities" {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(tc.code)
			}))
			t.Cleanup(srv.Close)

			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("parse server URL: %v", err)
			}
			port, err := strconv.Atoi(u.Port())
			if err != nil {
				t.Fatalf("parse server port: %v", err)
			}

			origLoad := supervisorLoadConfig
			supervisorLoadConfig = func(string) (supervisor.Config, error) {
				return supervisor.Config{
					Supervisor: supervisor.Section{Bind: u.Hostname(), Port: port},
				}, nil
			}
			t.Cleanup(func() { supervisorLoadConfig = origLoad })

			if got := supervisorAPIReachable(); got != tc.want {
				t.Fatalf("supervisorAPIReachable() = %v, want %v (status %d)", got, tc.want, tc.code)
			}
		})
	}
}
