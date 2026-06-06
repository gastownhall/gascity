package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSupervisorStatusJSON(t *testing.T) {
	clearGCEnv(t)
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

func TestSupervisorStatusJSON_RunningViaLaunchdWhenSocketDown(t *testing.T) {
	clearGCEnv(t)
	origGOOS := supervisorRuntimeGOOS
	supervisorRuntimeGOOS = "darwin"
	t.Cleanup(func() { supervisorRuntimeGOOS = origGOOS })
	origActive := supervisorLaunchdActive
	supervisorLaunchdActive = func(string) bool { return true }
	t.Cleanup(func() { supervisorLaunchdActive = origActive })

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
		t.Fatalf("want pid_source=service_manager socket_status=unreachable, got %+v", p)
	}
	if code != 0 {
		t.Fatalf("exit=%d, want 0", code)
	}
}

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
		t.Fatalf("pid_source = %q, want api", p.PidSource)
	}
	if p.SocketStatus != "unreachable" {
		t.Fatalf("socket_status = %q, want unreachable", p.SocketStatus)
	}
	if code != 0 {
		t.Fatalf("exit=%d, want 0", code)
	}
}

func TestSupervisorStatusJSON_NotRunningWhenAllSignalsDown(t *testing.T) {
	clearGCEnv(t)
	origSM := supervisorServiceManagerActive
	supervisorServiceManagerActive = func() bool { return false }
	t.Cleanup(func() { supervisorServiceManagerActive = origSM })
	origAPI := supervisorAPIReachable
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() { supervisorAPIReachable = origAPI })

	var stdout, stderr bytes.Buffer
	code := run([]string{"supervisor", "status", "--json"}, &stdout, &stderr)
	var p struct {
		Running bool `json:"running"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		t.Fatalf("stdout not JSON: %v\n%s", err, stdout.String())
	}
	if p.Running {
		t.Fatalf("all signals down but status running=true: %s", stdout.String())
	}
	if code != 0 {
		t.Fatalf("json path exit=%d, want 0", code)
	}

	// Text path: verify exit 1 and "Supervisor is not running"
	var textStdout, textStderr bytes.Buffer
	textCode := run([]string{"supervisor", "status"}, &textStdout, &textStderr)
	if textCode != 1 {
		t.Fatalf("text path exit=%d, want 1", textCode)
	}
	if textStdout.String() != "Supervisor is not running\n" {
		t.Fatalf("text = %q, want \"Supervisor is not running\\n\"", textStdout.String())
	}
}
