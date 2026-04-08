package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// ── Dolt config wiring tests (issue 011) ──────────────────────────────

func TestBdRuntimeEnvIncludesDoltHost(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "mini2.hippo-tilapia.ts.net")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "agent")
	t.Setenv("GC_DOLT_PASSWORD", "s3cret")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := bdRuntimeEnv(cityPath)

	if got := env["GC_DOLT_HOST"]; got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("GC_DOLT_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "mini2.hippo-tilapia.ts.net" {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "agent" {
		t.Errorf("BEADS_DOLT_SERVER_USER = %q, want %q", got, "agent")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "s3cret" {
		t.Errorf("BEADS_DOLT_PASSWORD = %q, want %q", got, "s3cret")
	}
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
	}
}

func TestBdRuntimeEnvExternalHostSkipsLocalState(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "remote.example.com")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := bdRuntimeEnv(cityPath)

	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Errorf("GC_DOLT_PORT = %q, want %q (should use env, not local state)", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q (should mirror external env)", got, "3307")
	}
}

func TestCityRuntimeProcessEnvIncludesDoltHost(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_HOST", "mini2.hippo-tilapia.ts.net")
	t.Setenv("GC_DOLT_PORT", "3307")
	t.Setenv("GC_DOLT_USER", "agent")
	t.Setenv("GC_DOLT_PASSWORD", "s3cret")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	env := cityRuntimeProcessEnv(cityPath)

	var foundHost, foundPort, foundBeadsHost, foundBeadsPort, foundBeadsUser, foundBeadsPass bool
	for _, entry := range env {
		if strings.HasPrefix(entry, "GC_DOLT_HOST=") {
			foundHost = true
			if got := strings.TrimPrefix(entry, "GC_DOLT_HOST="); got != "mini2.hippo-tilapia.ts.net" {
				t.Errorf("GC_DOLT_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
			}
		}
		if strings.HasPrefix(entry, "GC_DOLT_PORT=") {
			foundPort = true
			if got := strings.TrimPrefix(entry, "GC_DOLT_PORT="); got != "3307" {
				t.Errorf("GC_DOLT_PORT = %q, want %q", got, "3307")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_HOST=") {
			foundBeadsHost = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_HOST="); got != "mini2.hippo-tilapia.ts.net" {
				t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "mini2.hippo-tilapia.ts.net")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_PORT=") {
			foundBeadsPort = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_PORT="); got != "3307" {
				t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_USER=") {
			foundBeadsUser = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_USER="); got != "agent" {
				t.Errorf("BEADS_DOLT_SERVER_USER = %q, want %q", got, "agent")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_PASSWORD=") {
			foundBeadsPass = true
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_PASSWORD="); got != "s3cret" {
				t.Errorf("BEADS_DOLT_PASSWORD = %q, want %q", got, "s3cret")
			}
		}
	}
	if !foundHost {
		t.Error("GC_DOLT_HOST not found in cityRuntimeProcessEnv output")
	}
	if !foundPort {
		t.Error("GC_DOLT_PORT not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsHost {
		t.Error("BEADS_DOLT_SERVER_HOST not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsPort {
		t.Error("BEADS_DOLT_SERVER_PORT not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsUser {
		t.Error("BEADS_DOLT_SERVER_USER not found in cityRuntimeProcessEnv output")
	}
	if !foundBeadsPass {
		t.Error("BEADS_DOLT_PASSWORD not found in cityRuntimeProcessEnv output")
	}
}

func TestMergeRuntimeEnvIncludesDoltHost(t *testing.T) {
	parent := []string{
		"BEADS_DOLT_SERVER_HOST=old-beads-host",
		"BEADS_DOLT_SERVER_PORT=9999",
		"PATH=/usr/bin",
		"GC_DOLT_HOST=old-host",
	}
	overrides := map[string]string{
		"BEADS_DOLT_SERVER_HOST": "new-host.example.com",
		"BEADS_DOLT_SERVER_PORT": "3307",
		"GC_DOLT_HOST":           "new-host.example.com",
	}
	result := mergeRuntimeEnv(parent, overrides)

	var count, beadsCount, beadsPortCount int
	for _, entry := range result {
		if strings.HasPrefix(entry, "GC_DOLT_HOST=") {
			count++
			if got := strings.TrimPrefix(entry, "GC_DOLT_HOST="); got != "new-host.example.com" {
				t.Errorf("GC_DOLT_HOST = %q, want %q", got, "new-host.example.com")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_HOST=") {
			beadsCount++
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_HOST="); got != "new-host.example.com" {
				t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "new-host.example.com")
			}
		}
		if strings.HasPrefix(entry, "BEADS_DOLT_SERVER_PORT=") {
			beadsPortCount++
			if got := strings.TrimPrefix(entry, "BEADS_DOLT_SERVER_PORT="); got != "3307" {
				t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 GC_DOLT_HOST entry, got %d", count)
	}
	if beadsCount != 1 {
		t.Errorf("expected exactly 1 BEADS_DOLT_SERVER_HOST entry, got %d", beadsCount)
	}
	if beadsPortCount != 1 {
		t.Errorf("expected exactly 1 BEADS_DOLT_SERVER_PORT entry, got %d", beadsPortCount)
	}
}

func TestBdRuntimeEnvLocalHostNoHostKey(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")

	cityPath := t.TempDir()
	env := bdRuntimeEnv(cityPath)

	if _, ok := env["GC_DOLT_HOST"]; ok {
		t.Error("GC_DOLT_HOST should not be present when not configured")
	}
	if _, ok := env["BEADS_DOLT_SERVER_HOST"]; ok {
		t.Error("BEADS_DOLT_SERVER_HOST should not be present when not configured")
	}
}

func TestOpenStoreAtForCityUsesExplicitCityForExternalRig(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	externalRig := filepath.Join(t.TempDir(), "test-external")
	if err := os.MkdirAll(externalRig, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "file")

	store, err := openStoreAtForCity(externalRig, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "external rig bead", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cityStore, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if _, err := cityStore.Get(created.ID); err != nil {
		t.Fatalf("city store should see created bead %s: %v", created.ID, err)
	}
}

func TestMergeRuntimeEnvReplacesInheritedRuntimeKeys(t *testing.T) {
	env := mergeRuntimeEnv([]string{
		"BEADS_DIR=/rig/.beads",
		"BEADS_DOLT_SERVER_PORT=9999",
		"PATH=/bin",
		"GC_CITY_PATH=/wrong",
		"GC_DOLT_PORT=9999",
		"GC_PACK_STATE_DIR=/wrong/.gc/runtime/packs/dolt",
		"GC_RIG=demo",
		"GC_RIG_ROOT=/rig",
	}, map[string]string{
		"BEADS_DOLT_SERVER_PORT": "31364",
		"GC_CITY_PATH":           "/city",
		"GC_DOLT_PORT":           "31364",
	})

	got := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}

	if got["GC_CITY_PATH"] != "/city" {
		t.Fatalf("GC_CITY_PATH = %q, want %q", got["GC_CITY_PATH"], "/city")
	}
	if got["GC_DOLT_PORT"] != "31364" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got["GC_DOLT_PORT"], "31364")
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "31364" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got["BEADS_DOLT_SERVER_PORT"], "31364")
	}
	if _, ok := got["BEADS_DIR"]; ok {
		t.Fatalf("BEADS_DIR should be removed, env = %#v", got)
	}
	if _, ok := got["GC_PACK_STATE_DIR"]; ok {
		t.Fatalf("GC_PACK_STATE_DIR should be removed, env = %#v", got)
	}
	if _, ok := got["GC_RIG"]; ok {
		t.Fatalf("GC_RIG should be removed, env = %#v", got)
	}
	if _, ok := got["GC_RIG_ROOT"]; ok {
		t.Fatalf("GC_RIG_ROOT should be removed, env = %#v", got)
	}
}

func TestBdCommandRunnerForCityPinsCityStoreEnv(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("BEADS_DIR", "/rig/.beads")
	t.Setenv("GC_RIG", "demo-rig")
	t.Setenv("GC_RIG_ROOT", "/rig")

	runner := bdCommandRunnerForCity(cityDir)
	out, err := runner(cityDir, "sh", "-c", `printf '%s\n%s\n%s\n%s\n' "$GC_CITY_PATH" "$BEADS_DIR" "$GC_RIG" "$GC_RIG_ROOT"`)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(string(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("lines = %q, want 5 lines including trailing newline", string(out))
	}
	lines = lines[:4]
	if len(lines) != 4 {
		t.Fatalf("lines = %q, want 4 lines", string(out))
	}
	if lines[0] != cityDir {
		t.Fatalf("GC_CITY_PATH = %q, want %q", lines[0], cityDir)
	}
	if lines[1] != filepath.Join(cityDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", lines[1], filepath.Join(cityDir, ".beads"))
	}
	if lines[2] != "" {
		t.Fatalf("GC_RIG = %q, want empty", lines[2])
	}
	if lines[3] != "" {
		t.Fatalf("GC_RIG_ROOT = %q, want empty", lines[3])
	}
}

// BUG: PR #201 — bdStoreForRig() does not exist. All bd operations use
// bdStoreForCity() which returns a store rooted at the city level, not the
// rig level. For rig-scoped bead IDs, the city-level store cannot resolve
// them because it looks in the city's .beads directory, not the rig's.
//
// This test demonstrates that:
// 1. bdStoreForRig is needed but does not exist (only bdStoreForCity exists)
// 2. bdRuntimeEnv sets BEADS_DIR to the city's .beads, not a rig's
// 3. bdCommandRunnerForCity always pins BEADS_DIR to cityDir/.beads
func TestBdStoreForRig_DoesNotExist(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a rig directory — a separate repository outside the city.
	rigDir := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// bdRuntimeEnv always sets BEADS_DIR to cityDir/.beads.
	// A rig-scoped agent needs BEADS_DIR=rigDir/.beads, but no
	// bdStoreForRig() exists to produce that.
	env := bdRuntimeEnv(cityDir)
	beadsDir := env["BEADS_DIR"]
	wantCityBeads := filepath.Join(cityDir, ".beads")
	if beadsDir != wantCityBeads {
		t.Errorf("BEADS_DIR = %q, want %q (city-level)", beadsDir, wantCityBeads)
	}
	rigBeadsDir := filepath.Join(rigDir, ".beads")
	if beadsDir == rigBeadsDir {
		t.Error("BEADS_DIR unexpectedly points to rig — bdStoreForRig may have been added")
	}

	// bdCommandRunnerForCity pins BEADS_DIR to the RUNNER's dir arg (not the
	// rig). This is the command runner used by bdStoreForCity. It always
	// constructs env with cityDir context, never rig-specific context.
	runner := bdCommandRunnerForCity(cityDir)

	// Run a command in the rig directory to see what BEADS_DIR is set to.
	out, err := runner(rigDir, "sh", "-c", `printf '%s' "$BEADS_DIR"`)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	// BEADS_DIR is pinned to rigDir/.beads (the runner overrides per-call dir),
	// but GC_RIG and GC_RIG_ROOT are always empty — no rig context is injected.
	gotBeadsDir := string(out)
	wantRunnerBeads := filepath.Join(rigDir, ".beads")
	if gotBeadsDir != wantRunnerBeads {
		t.Errorf("runner BEADS_DIR = %q, want %q", gotBeadsDir, wantRunnerBeads)
	}

	// Verify GC_RIG is empty — the runner does not know which rig it serves.
	rigOut, err := runner(rigDir, "sh", "-c", `printf '%s' "$GC_RIG"`)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if string(rigOut) != "" {
		t.Errorf("GC_RIG = %q, want empty (no rig context in bdCommandRunnerForCity)", string(rigOut))
	}

	// PR #201 adds bdStoreForRig which opens a store at the rig directory
	// with rig-level Dolt config. Verify it returns a store pointed at the
	// rig path, not the city path. Also verify bdRuntimeEnvForRig injects
	// rig-level Dolt host/port when configured.
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:     "myrig",
			Path:     rigDir,
			DoltHost: "rig-host",
			DoltPort: "3307",
		}},
	}

	// bdRuntimeEnvForRig should inject rig-level Dolt config.
	rigEnv := bdRuntimeEnvForRig(cityDir, cfg, rigDir)
	if rigEnv["BEADS_DOLT_SERVER_HOST"] != "rig-host" {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", rigEnv["BEADS_DOLT_SERVER_HOST"], "rig-host")
	}
	if rigEnv["BEADS_DOLT_SERVER_PORT"] != "3307" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", rigEnv["BEADS_DOLT_SERVER_PORT"], "3307")
	}
	if got := rigEnv["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Errorf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
	if got := rigEnv["GC_RIG"]; got != "myrig" {
		t.Errorf("GC_RIG = %q, want %q", got, "myrig")
	}
	if got := rigEnv["GC_RIG_ROOT"]; got != rigDir {
		t.Errorf("GC_RIG_ROOT = %q, want %q", got, rigDir)
	}
}

func TestBdRuntimeEnvForRigUsesManagedRigPort(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	portFile := filepath.Join(rigDir, ".beads", "dolt-server.port")
	if err := os.WriteFile(portFile, []byte("31364"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Make the advertised port reachable so currentDoltPort accepts it.
	ln, err := net.Listen("tcp", "127.0.0.1:31364")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup

	env := bdRuntimeEnvForRig(cityDir, &config.City{}, rigDir)
	if got := env["GC_DOLT_PORT"]; got != "31364" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "31364")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "31364" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "31364")
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
}

func TestBdRuntimeEnvForRigFallsBackToManagedCityPort(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnvForRig(cityDir, &config.City{}, rigDir)
	want := strings.TrimSpace(strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:"))
	if got := env["GC_DOLT_PORT"]; got != want {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, want)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != want {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, want)
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
}

func TestBdRuntimeEnvForRigPrefersExplicitRigDoltConfigOverManagedCity(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup

	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:     "repo",
			Path:     rigDir,
			DoltHost: "rig-db.example.com",
			DoltPort: "3307",
		}},
	}

	env := bdRuntimeEnvForRig(cityDir, cfg, rigDir)
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "3307" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3307")
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
	if got := env["GC_RIG"]; got != "repo" {
		t.Fatalf("GC_RIG = %q, want %q", got, "repo")
	}
	if got := env["GC_RIG_ROOT"]; got != rigDir {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", got, rigDir)
	}
}

func TestDoltAutoStartSuppressedInAllEnvPaths(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()

	t.Run("bdRuntimeEnv", func(t *testing.T) {
		env := bdRuntimeEnv(cityPath)
		if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
			t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
		}
	})

	t.Run("bdRuntimeEnvForRig", func(t *testing.T) {
		rigDir := filepath.Join(t.TempDir(), "rig")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		env := bdRuntimeEnvForRig(cityPath, &config.City{}, rigDir)
		if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
			t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
		}
	})

	t.Run("sessionDoltEnv", func(t *testing.T) {
		env := sessionDoltEnv(cityPath, "", nil)
		if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
			t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q", got, "0")
		}
	})
}

// ── cityForStoreDir boundary tests ──────────────────────────────────

func TestCityForStoreDirHonoursGCCity(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	cityDir := filepath.Join(homeDir, "mycity")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"mine\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// GC_CITY points to the exact city root — should resolve via
	// validateCityPath without walk-up, even when the store dir is
	// outside bounded discovery range.
	t.Setenv("GC_CITY", cityDir)
	outsideDir := t.TempDir()
	got := cityForStoreDir(outsideDir)
	if got != cityDir {
		t.Errorf("cityForStoreDir(%q) = %q, want %q (from GC_CITY)", outsideDir, got, cityDir)
	}
}

func TestCityForStoreDirFallsBackToFindCity(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

	// Unset GC_CITY so cityForStoreDir falls back to findCity.
	t.Setenv("GC_CITY", "")

	cityDir := filepath.Join(homeDir, "projects", "alpha")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"alpha\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Store dir is inside the city — findCity should discover it.
	storeDir := filepath.Join(cityDir, "rigs", "repo")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := cityForStoreDir(storeDir)
	if got != cityDir {
		t.Errorf("cityForStoreDir(%q) = %q, want %q (from findCity)", storeDir, got, cityDir)
	}
}

func TestCityForStoreDirFallsBackToDirWhenNoCityFound(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("GC_CITY", "")

	// No city.toml anywhere — cityForStoreDir should return dir as fallback.
	noCity := filepath.Join(homeDir, "nocity", "deep")
	if err := os.MkdirAll(noCity, 0o755); err != nil {
		t.Fatal(err)
	}

	got := cityForStoreDir(noCity)
	if got != noCity {
		t.Errorf("cityForStoreDir(%q) = %q, want same dir as fallback", noCity, got)
	}
}
