package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDashboardNoticePrintsSupervisorURL(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Chdir(t.TempDir())

	oldAlive := supervisorAliveHook
	oldCityFlag := cityFlag
	oldRigFlag := rigFlag
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	supervisorAliveHook = func() int { return 4242 }
	cityFlag = ""
	rigFlag = ""

	var stdout bytes.Buffer
	if err := runDashboardNotice("gc dashboard", "", &stdout, io.Discard); err != nil {
		t.Fatalf("runDashboardNotice() error: %v", err)
	}

	wantURL, err := supervisorAPIBaseURL()
	if err != nil {
		t.Fatalf("supervisorAPIBaseURL(): %v", err)
	}
	wantURL = strings.TrimRight(wantURL, "/")
	if !strings.Contains(stdout.String(), wantURL) {
		t.Fatalf("notice = %q, want it to include supervisor URL %q", stdout.String(), wantURL)
	}
}

func TestRunDashboardNoticeUsesAPIOverride(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Chdir(t.TempDir())

	oldAlive := supervisorAliveHook
	oldCityFlag := cityFlag
	oldRigFlag := rigFlag
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	supervisorAliveHook = func() int { return 0 }
	cityFlag = ""
	rigFlag = ""

	var stdout bytes.Buffer
	if err := runDashboardNotice("gc dashboard", "http://127.0.0.1:9999/", &stdout, io.Discard); err != nil {
		t.Fatalf("runDashboardNotice() error: %v", err)
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:9999") {
		t.Fatalf("notice = %q, want trimmed override URL", stdout.String())
	}
	if strings.Contains(stdout.String(), "http://127.0.0.1:9999/") {
		t.Fatalf("notice = %q, want trailing slash trimmed", stdout.String())
	}
}

// TestRunDashboardNoticeHintsStartWhenUnresolvable pins that, when neither a
// supervisor nor a standalone-controller API can be resolved, the command
// prints how to start the supervisor and still exits 0 (returns nil).
func TestRunDashboardNoticeHintsStartWhenUnresolvable(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Chdir(t.TempDir())

	oldAlive := supervisorAliveHook
	oldCityFlag := cityFlag
	oldRigFlag := rigFlag
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	supervisorAliveHook = func() int { return 0 }
	cityFlag = ""
	rigFlag = ""

	var stdout bytes.Buffer
	if err := runDashboardNotice("gc dashboard", "", &stdout, io.Discard); err != nil {
		t.Fatalf("runDashboardNotice() error = %v, want nil (informational command exits 0)", err)
	}
	if !strings.Contains(stdout.String(), "gc supervisor start") {
		t.Fatalf("notice = %q, want it to include the start hint %q", stdout.String(), "gc supervisor start")
	}
}

// TestRunDashboardNoticeUsesStandaloneControllerAPI pins that the standalone
// controller's API (cfg.API.Port) is reported as the dashboard URL when no
// machine-wide supervisor is running.
func TestRunDashboardNoticeUsesStandaloneControllerAPI(t *testing.T) {
	configureIsolatedRuntimeEnv(t)

	cityDir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("mkdir city dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "alpha"
provider = "claude"

[providers.claude]
base = "builtin:claude"

[api]
port = 9123
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	t.Chdir(t.TempDir())

	oldAlive := supervisorAliveHook
	oldCityFlag := cityFlag
	oldRigFlag := rigFlag
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	supervisorAliveHook = func() int { return 0 }
	cityFlag = cityDir
	rigFlag = ""

	var stdout bytes.Buffer
	if err := runDashboardNotice("gc dashboard", "", &stdout, io.Discard); err != nil {
		t.Fatalf("runDashboardNotice() error = %v, want nil (standalone-controller API is supported)", err)
	}
	if !strings.Contains(stdout.String(), ":9123") {
		t.Fatalf("notice = %q, want it to include the configured standalone port :9123", stdout.String())
	}
}
