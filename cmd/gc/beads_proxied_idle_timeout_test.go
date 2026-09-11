package main

import (
	"os"
	"path/filepath"
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
