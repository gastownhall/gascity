package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
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
	oldControllerAlive := eventsControllerAliveHook
	oldCityFlag := cityFlag
	oldRigFlag := rigFlag
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		eventsControllerAliveHook = oldControllerAlive
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	supervisorAliveHook = func() int { return 0 }
	eventsControllerAliveHook = func(string) int { return 1234 }
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

func TestResolveEventsScopeUsesLocalFallbackWhenStandaloneControllerStopped(t *testing.T) {
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
	oldControllerAlive := eventsControllerAliveHook
	oldCityFlag := cityFlag
	oldRigFlag := rigFlag
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		eventsControllerAliveHook = oldControllerAlive
		cityFlag = oldCityFlag
		rigFlag = oldRigFlag
	})

	supervisorAliveHook = func() int { return 0 }
	eventsControllerAliveHook = func(string) int { return 0 }
	cityFlag = cityDir
	rigFlag = ""

	scope, err := resolveEventsScope("")
	if err != nil {
		t.Fatalf("resolveEventsScope() error = %v, want nil (stopped standalone city should use local fallback)", err)
	}
	if !scope.localOnly {
		t.Fatalf("standalone stopped city localOnly = %v, want true", scope.localOnly)
	}
	if scope.apiURL != "" {
		t.Fatalf("standalone stopped city apiURL = %q, want empty", scope.apiURL)
	}
	if scope.cityName != "alpha" {
		t.Fatalf("standalone stopped city cityName = %q, want %q", scope.cityName, "alpha")
	}
}

func TestResolveEventsScopeUsesRegisteredSupervisorCityName(t *testing.T) {
	configureIsolatedRuntimeEnv(t)

	cityDir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("mkdir city dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "renamed-alpha"
provider = "claude"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(supervisor.ConfigPath()), 0o755); err != nil {
		t.Fatalf("mkdir supervisor config dir: %v", err)
	}
	if err := os.WriteFile(supervisor.ConfigPath(), []byte("[supervisor]\nport = 9124\n"), 0o644); err != nil {
		t.Fatalf("write supervisor config: %v", err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityDir, "alpha"); err != nil {
		t.Fatalf("register city: %v", err)
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

	supervisorAliveHook = func() int { return 1234 }
	cityFlag = cityDir
	rigFlag = ""

	scope, err := resolveEventsScope("")
	if err != nil {
		t.Fatalf("resolveEventsScope() error = %v, want nil", err)
	}
	if !strings.Contains(scope.apiURL, ":9124") {
		t.Fatalf("events scope apiURL = %q, want configured supervisor port :9124", scope.apiURL)
	}
	if scope.cityName != "alpha" {
		t.Fatalf("events scope cityName = %q, want registered supervisor name %q", scope.cityName, "alpha")
	}
}

func TestResolveEventsScopeExplicitAPIUsesRegisteredSupervisorCityName(t *testing.T) {
	configureIsolatedRuntimeEnv(t)

	cityDir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("mkdir city dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "renamed-alpha"
provider = "claude"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityDir, "alpha"); err != nil {
		t.Fatalf("register city: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(supervisor.ConfigPath()), 0o755); err != nil {
		t.Fatalf("mkdir supervisor config dir: %v", err)
	}
	if err := os.WriteFile(supervisor.ConfigPath(), []byte("[supervisor]\nport = 8372\n"), 0o644); err != nil {
		t.Fatalf("write supervisor config: %v", err)
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

	supervisorAliveHook = func() int { return 1234 }
	cityFlag = cityDir
	rigFlag = ""

	scope, err := resolveEventsScope("http://localhost:8372")
	if err != nil {
		t.Fatalf("resolveEventsScope() error = %v, want nil", err)
	}
	if scope.apiURL != "http://localhost:8372" {
		t.Fatalf("events scope apiURL = %q, want explicit override", scope.apiURL)
	}
	if !scope.explicitAPI {
		t.Fatal("events scope explicitAPI = false, want true")
	}
	if scope.cityName != "alpha" {
		t.Fatalf("events scope cityName = %q, want registered supervisor name %q", scope.cityName, "alpha")
	}
}

func TestResolveEventsScopeExplicitAPIPreservesLocalCityNameForForeignServer(t *testing.T) {
	configureIsolatedRuntimeEnv(t)

	cityDir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("mkdir city dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "renamed-alpha"
provider = "claude"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityDir, "alpha"); err != nil {
		t.Fatalf("register city: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(supervisor.ConfigPath()), 0o755); err != nil {
		t.Fatalf("mkdir supervisor config dir: %v", err)
	}
	if err := os.WriteFile(supervisor.ConfigPath(), []byte("[supervisor]\nport = 8372\n"), 0o644); err != nil {
		t.Fatalf("write supervisor config: %v", err)
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

	supervisorAliveHook = func() int { return 1234 }
	cityFlag = cityDir
	rigFlag = ""

	scope, err := resolveEventsScope("http://127.0.0.1:9123")
	if err != nil {
		t.Fatalf("resolveEventsScope() error = %v, want nil", err)
	}
	if scope.cityName != "renamed-alpha" {
		t.Fatalf("events scope cityName = %q, want local configured name %q for foreign explicit API", scope.cityName, "renamed-alpha")
	}
}

func TestResolveEventsScopeExplicitLocalSupervisorUsesRegisteredNameWhenSupervisorStopped(t *testing.T) {
	configureIsolatedRuntimeEnv(t)

	cityDir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatalf("mkdir city dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "renamed-alpha"
provider = "claude"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityDir, "alpha"); err != nil {
		t.Fatalf("register city: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(supervisor.ConfigPath()), 0o755); err != nil {
		t.Fatalf("mkdir supervisor config dir: %v", err)
	}
	if err := os.WriteFile(supervisor.ConfigPath(), []byte("[supervisor]\nport = 8372\n"), 0o644); err != nil {
		t.Fatalf("write supervisor config: %v", err)
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

	scope, err := resolveEventsScope("http://localhost:8372")
	if err != nil {
		t.Fatalf("resolveEventsScope() error = %v, want nil", err)
	}
	if !scope.localSupervisorAPI {
		t.Fatal("events scope localSupervisorAPI = false, want true")
	}
	if scope.cityName != "alpha" {
		t.Fatalf("events scope cityName = %q, want registered supervisor name %q", scope.cityName, "alpha")
	}
}
