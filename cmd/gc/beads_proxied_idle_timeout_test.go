package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// D3 pins every GC-owned proxied scope to bd's IdleTimeoutNever. Two
// initializers reach bd's proxied path without going through the
// provider-owned front door — the default rig-store initializer here, and the
// legacy script's run_bd_init_proxied — and both omitted the flag, so a scope
// created through either got bd's 30s idle timeout (a proxy-plus-Dolt cold
// start on every later command after a quiet period) and no client-info
// sidecar for the lifecycle to read the proxy root from.
func TestDefaultRigBdStoreInitPinsIdleNeverOnProxiedScopes(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "bd-args")
	fakeBd := filepath.Join(city, "bd")
	if err := os.WriteFile(fakeBd, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+logPath+"\"\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("BD_BIN", fakeBd)
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"r1"}`)

	// The init itself does more than call bd; only the invocation matters here.
	_ = initDefaultRigBdStore(city, rig, "r1", "r1")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the initializer did not invoke bd: %v", err)
	}
	invocation := string(data)
	if !strings.Contains(invocation, "--proxied-server") {
		t.Fatalf("bd was not asked for a proxied store: %s", invocation)
	}
	if !strings.Contains(invocation, "--proxied-server-idle-timeout 0") {
		t.Fatalf("proxied init omitted the idle-never pin: %s", invocation)
	}
}

// The legacy script's proxied initializer carries the same pin. It is the one
// an ambient BEADS_DOLT_PROXIED_SERVER can still reach.
func TestLegacyScriptProxiedInitPinsIdleNever(t *testing.T) {
	data, err := os.ReadFile("../../examples/bd/assets/scripts/gc-beads-bd.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	marker := "set -- init --quiet --proxied-server"
	idx := strings.Index(script, marker)
	if idx < 0 {
		t.Fatalf("run_bd_init_proxied no longer builds its argv with %q; move this assertion with it", marker)
	}
	line := script[idx:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	if !strings.Contains(line, "--proxied-server-idle-timeout 0") {
		t.Fatalf("run_bd_init_proxied omits the idle-never pin: %s", line)
	}
}

// A bare non-bd provider (`provider = "doltlite"`) is not a bd-contract city:
// gc runs no provider-owned front door for it, and the only provider shape that
// door accepts is `exec:`. The default rig-store initializer nevertheless took
// bd's proxied arm for such a city, which made the fresh rig provider-owned by
// binding — after which every `gc start`, health tick and `gc stop` refused the
// city with "requires an exec beads provider", and bd's idle-never proxy plus
// its Dolt child stayed resident with no gc verb left to retire them.
func TestDefaultRigBdStoreKeepsDirectServerForNonBdContractCity(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "api")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	cityTOML := "[workspace]\nname = \"tincan-city\"\n\n[beads]\nprovider = \"doltlite\"\n\n" +
		"[[rigs]]\nname = \"api\"\npath = " + strconv.Quote(rig) + "\nprefix = \"api\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityTOML), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "bd-args")
	fakeBd := filepath.Join(city, "bd")
	if err := os.WriteFile(fakeBd, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+logPath+"\"\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("BD_BIN", fakeBd)

	if err := initDefaultRigBdStore(city, rig, "api", "api"); err != nil {
		t.Fatalf("initDefaultRigBdStore: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the initializer did not invoke bd: %v", err)
	}
	invocation := string(data)
	if strings.Contains(invocation, "--proxied-server") {
		t.Fatalf("bd was asked for a proxied store on a non-bd-contract city: %s", invocation)
	}
	if !strings.Contains(invocation, "--server") {
		t.Fatalf("bd was not asked for a direct server store: %s", invocation)
	}

	// The point of keeping --server: the rig must not classify as
	// provider-owned, because nothing here can run its lifecycle.
	if owned, err := scopeProviderOwned(city, rig); err != nil {
		t.Fatalf("scopeProviderOwned: %v", err)
	} else if owned {
		t.Fatalf("a rig store on a non-bd-contract city classified as provider-owned")
	}
	if err := initAndHookDir(city, rig, "api"); err != nil {
		t.Fatalf("second boot door on the same rig: %v", err)
	}
	if err := shutdownBeadsProvider(city); err != nil {
		t.Fatalf("gc stop after the rig add: %v", err)
	}
}
