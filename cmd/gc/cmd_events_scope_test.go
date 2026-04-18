package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveEventsScopeRejectsStandaloneCityAPIOutsideCityDir(t *testing.T) {
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

	_, err := resolveEventsScope("")
	if err == nil {
		t.Fatal("resolveEventsScope() error = nil, want supervisor-only failure")
	}
	if !strings.Contains(err.Error(), "requires the supervisor API") {
		t.Fatalf("resolveEventsScope() error = %q, want supervisor-only failure", err)
	}
}
