package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeProxiedLifecycleScope creates the smallest canonical bd scope needed
// by lifecycle guards. The provider-state fixture is intentionally optional:
// callers that need to prove stale direct-Dolt artifacts are ignored add it
// after this helper returns.
func writeProxiedLifecycleScope(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"proxied-test\"\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "proxied-server",
		"dolt_database": "hq",
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.mode: proxied-server\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	t.Setenv("GC_DOLT", "")
	t.Setenv("GC_DOLT_HOST", "")
	t.Setenv("GC_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_PROXIED_SERVER", "")
	return cityPath
}

func TestWaitForBeadsScopeReadyAfterRecoverySkipsProxiedScope(t *testing.T) {
	cityPath := writeProxiedLifecycleScope(t)
	deadline := time.Now().Add(-time.Second)

	if err := waitForBeadsScopeReadyAfterRecovery(cityPath, cityPath, deadline); err != nil {
		t.Fatalf("waitForBeadsScopeReadyAfterRecovery() = %v, want nil for proxied scope", err)
	}
}

func TestWaitForAllBeadsScopesReadyAfterRecoverySkipsProxiedScopes(t *testing.T) {
	cityPath := writeProxiedLifecycleScope(t)
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(fmt.Sprintf("[workspace]\nname = \"proxied-test\"\n[beads]\nprovider = \"bd\"\n\n[[rigs]]\nname = \"frontend\"\npath = %q\nprefix = \"fe\"\n", rigPath)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"frontend"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForAllBeadsScopesReadyAfterRecovery(cityPath, time.Nanosecond); err != nil {
		t.Fatalf("waitForAllBeadsScopesReadyAfterRecovery() = %v, want nil for proxied scopes", err)
	}
}

func TestVerifyManagedDoltDatabaseExistsAfterInitSkipsProxiedMode(t *testing.T) {
	cityPath := writeProxiedLifecycleScope(t)
	port := writeReachableProviderManagedDoltState(t, cityPath)
	called := false
	oldList := managedDoltListUserDatabasesAfterInit
	managedDoltListUserDatabasesAfterInit = func(gotPort string) ([]string, error) {
		called = true
		return nil, fmt.Errorf("unexpected direct catalog probe on port %s", gotPort)
	}
	t.Cleanup(func() { managedDoltListUserDatabasesAfterInit = oldList })

	if port <= 0 {
		t.Fatal("precondition: provider state port was not created")
	}
	if err := verifyManagedDoltDatabaseExistsAfterInit(cityPath, cityPath, "hq"); err != nil {
		t.Fatalf("verifyManagedDoltDatabaseExistsAfterInit() = %v, want nil", err)
	}
	if called {
		t.Fatal("proxied verification invoked direct Dolt catalog probe")
	}
}

func TestCurrentResolvableManagedDoltPortSkipsProxiedMode(t *testing.T) {
	cityPath := writeProxiedLifecycleScope(t)
	port := writeReachableProviderManagedDoltState(t, cityPath)
	if got := currentResolvableManagedDoltPort(cityPath); got != "" {
		t.Fatalf("currentResolvableManagedDoltPort() = %q (provider state port %d), want empty for proxied mode", got, port)
	}
}

func TestStopCityManagedBeadsProviderSkipsProxiedModeWithStaleRuntime(t *testing.T) {
	cityPath := writeProxiedLifecycleScope(t)
	providerScript := filepath.Join(cityPath, "provider.sh")
	stopMarker := filepath.Join(cityPath, "stop-called")
	if err := os.WriteFile(providerScript, []byte("#!/bin/sh\ntouch "+stopMarker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Keep rawBeadsProvider(cityPath) == bd while making the stale runtime
	// artifact resolvable. stopCityManagedBeadsProvider must classify mode
	// before consulting the port and must not invoke the shutdown seam.
	_ = providerScript
	writeReachableProviderManagedDoltState(t, cityPath)
	oldShutdown := shutdownBeadsProviderForStop
	shutdownBeadsProviderForStop = func(string) error {
		t.Fatal("shutdownBeadsProviderForStop called for proxied scope")
		return nil
	}
	t.Cleanup(func() { shutdownBeadsProviderForStop = oldShutdown })

	stopped, err := stopCityManagedBeadsProvider(cityPath)
	if err != nil {
		t.Fatalf("stopCityManagedBeadsProvider() error = %v", err)
	}
	if stopped {
		t.Fatal("stopCityManagedBeadsProvider() = true, want false for proxied mode")
	}
	if _, err := os.Stat(stopMarker); !os.IsNotExist(err) {
		t.Fatalf("provider stop marker exists, stat err = %v", err)
	}
}

func TestProxiedBridgeEnvironmentLeavesAutoStartEnabled(t *testing.T) {
	cityPath := writeProxiedLifecycleScope(t)
	t.Setenv("BEADS_DOLT_AUTO_START", "0")
	env := bdStoreBridgeEnv(cityPath, "", "", "", "")
	if _, ok := env["BEADS_DOLT_AUTO_START"]; ok {
		t.Fatal("proxied bridge environment must not disable Beads-managed child auto-start")
	}
}
