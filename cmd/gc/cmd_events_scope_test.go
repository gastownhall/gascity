package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveEventsScopeUsesStandaloneControllerAPI pins the post-fixup
// behavior: the standalone controller's API serves supervisor-shaped
// /v0/city/{cityName}/events routes via api.NewSupervisorMux, so
// `gc events` resolves to the local controller API instead of
// hard-erroring. The previous revision
// ("TestResolveEventsScopeRejectsStandaloneCityAPIOutsideCityDir")
// asserted the rejection that this fixup intentionally removed.
func TestResolveEventsScopeUsesStandaloneControllerAPI(t *testing.T) {
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

	scope, err := resolveEventsScope("")
	if err != nil {
		t.Fatalf("resolveEventsScope() error = %v, want nil (standalone-controller API is supported)", err)
	}
	if !strings.Contains(scope.apiURL, ":9123") {
		t.Fatalf("standalone events scope apiURL = %q, want configured port :9123", scope.apiURL)
	}
	if scope.cityName != "alpha" {
		t.Fatalf("standalone events scope cityName = %q, want %q", scope.cityName, "alpha")
	}
}
