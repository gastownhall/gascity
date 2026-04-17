package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDashboardServeAllowsNoCityWithSupervisor(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Chdir(t.TempDir())

	oldAlive := supervisorAliveHook
	oldServe := dashboardServeHook
	oldCityFlag := cityFlag
	oldRigFlag := rigFlag
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		dashboardServeHook = oldServe
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	supervisorAliveHook = func() int { return 4242 }
	cityFlag = ""
	rigFlag = ""

	var gotPort int
	var gotURL string
	dashboardServeHook = func(port int, apiURL string) error {
		gotPort = port
		gotURL = apiURL
		return nil
	}

	if err := runDashboardServe("gc dashboard", 9090, "", io.Discard); err != nil {
		t.Fatalf("runDashboardServe() error: %v", err)
	}

	wantURL, err := supervisorAPIBaseURL()
	if err != nil {
		t.Fatalf("supervisorAPIBaseURL(): %v", err)
	}
	if gotPort != 9090 {
		t.Fatalf("dashboard port = %d, want 9090", gotPort)
	}
	if gotURL != strings.TrimRight(wantURL, "/") {
		t.Fatalf("dashboard api URL = %q, want %q", gotURL, strings.TrimRight(wantURL, "/"))
	}
}

func TestRunDashboardServeAllowsNoCityWithAPIOverride(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Chdir(t.TempDir())

	oldAlive := supervisorAliveHook
	oldServe := dashboardServeHook
	oldCityFlag := cityFlag
	oldRigFlag := rigFlag
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		dashboardServeHook = oldServe
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	supervisorAliveHook = func() int { return 0 }
	cityFlag = ""
	rigFlag = ""

	var gotURL string
	dashboardServeHook = func(_ int, apiURL string) error {
		gotURL = apiURL
		return nil
	}

	if err := runDashboardServe("gc dashboard", 9090, "http://127.0.0.1:9999/", io.Discard); err != nil {
		t.Fatalf("runDashboardServe() error: %v", err)
	}
	if gotURL != "http://127.0.0.1:9999" {
		t.Fatalf("dashboard api URL = %q, want trimmed override", gotURL)
	}
}

func TestRunDashboardServeHonorsExplicitCityOutsideCityDir(t *testing.T) {
	configureIsolatedRuntimeEnv(t)

	cityDir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("mkdir city dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "alpha"
provider = "claude"

[api]
port = 9123
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	t.Chdir(t.TempDir())

	oldAlive := supervisorAliveHook
	oldServe := dashboardServeHook
	oldCityFlag := cityFlag
	oldRigFlag := rigFlag
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		dashboardServeHook = oldServe
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	supervisorAliveHook = func() int { return 0 }
	cityFlag = cityDir
	rigFlag = ""

	var gotURL string
	dashboardServeHook = func(_ int, apiURL string) error {
		gotURL = apiURL
		return nil
	}

	if err := runDashboardServe("gc dashboard", 9090, "", io.Discard); err != nil {
		t.Fatalf("runDashboardServe() error: %v", err)
	}
	if gotURL != "http://127.0.0.1:9123" {
		t.Fatalf("dashboard api URL = %q, want standalone city API", gotURL)
	}
}
