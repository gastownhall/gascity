//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// buildFakeProc builds a minimal /proc-shaped fixture tree under root for pid
// with parent PID 1 (init). environ is written as NUL-delimited key=value pairs.
func buildFakeProc(t *testing.T, root string, pid int, env map[string]string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	var buf []byte
	for k, v := range env {
		buf = append(buf, []byte(k+"="+v+"\x00")...)
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), buf, 0o644); err != nil {
		t.Fatalf("write environ: %v", err)
	}
	// stat: ppid=1 (init) so this process is classified as a root.
	stat := strconv.Itoa(pid) + " (cmd) S 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
}

func writeFakeProcCmdline(t *testing.T, root string, pid int, argv ...string) {
	t.Helper()
	path := filepath.Join(root, strconv.Itoa(pid), "cmdline")
	data := []byte(strings.Join(argv, "\x00") + "\x00")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}
}

func TestScanWithRootStatVanished(t *testing.T) {
	root := t.TempDir()
	pid := 500
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write environ but no stat file (process died between environ read and stat check).
	env := []byte("GC_SESSION_ID=ga-test\x00")
	if err := os.WriteFile(filepath.Join(dir, "environ"), env, 0o644); err != nil {
		t.Fatalf("write environ: %v", err)
	}
	// stat file absent — simulates TOCTOU race.

	got, err := scanWithRoot(root, "ga-test")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("scanWithRoot returned %d runtimes for a vanished process, want 0", len(got))
	}
}

func TestScanWithRootEmptyReturnsNonNilSlice(t *testing.T) {
	root := t.TempDir()
	got, err := scanWithRoot(root, "")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if got == nil {
		t.Fatal("scanWithRoot returned nil slice, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("scanWithRoot returned %d runtimes, want 0", len(got))
	}
}

func TestScanWithRootFiltersBySessionID(t *testing.T) {
	root := t.TempDir()
	// pid 100: parent 1 (init), session ga-abc
	buildFakeProc(t, root, 100, map[string]string{"GC_SESSION_ID": "ga-abc"})
	// pid 200: parent 1 (init), session ga-xyz
	buildFakeProc(t, root, 200, map[string]string{"GC_SESSION_ID": "ga-xyz"})

	got, err := scanWithRoot(root, "ga-abc")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "ga-abc" || got[0].PID != 100 {
		t.Fatalf("scanWithRoot = %v, want [{ga-abc pid=100}]", got)
	}
}

func TestScanWithRootEmptyIDReturnsAll(t *testing.T) {
	root := t.TempDir()
	buildFakeProc(t, root, 100, map[string]string{"GC_SESSION_ID": "ga-abc"})
	buildFakeProc(t, root, 200, map[string]string{"GC_SESSION_ID": "ga-xyz"})

	got, err := scanWithRoot(root, "")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("scanWithRoot = %d entries, want 2", len(got))
	}
}

func TestScanWithRootParsesEpoch(t *testing.T) {
	root := t.TempDir()
	buildFakeProc(t, root, 300, map[string]string{
		"GC_SESSION_ID":    "ga-epoch",
		"GC_RUNTIME_EPOCH": "42",
	})

	got, err := scanWithRoot(root, "ga-epoch")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].Epoch != 42 {
		t.Fatalf("Epoch = %d, want 42", got[0].Epoch)
	}
}

func TestScanWithRootPopulatesCityFromGCPath(t *testing.T) {
	root := t.TempDir()
	buildFakeProc(t, root, 310, map[string]string{
		"GC_SESSION_ID": "ga-city",
		"GC_CITY_PATH":  "/tmp/primary-city",
		"GC_CITY":       "/tmp/fallback-city",
	})

	got, err := scanWithRoot(root, "ga-city")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].City != "/tmp/primary-city" {
		t.Fatalf("City = %q, want GC_CITY_PATH value", got[0].City)
	}
}

func TestScanWithRootPopulatesCityFromGCCityFallback(t *testing.T) {
	root := t.TempDir()
	buildFakeProc(t, root, 320, map[string]string{
		"GC_SESSION_ID": "ga-city",
		"GC_CITY":       "/tmp/fallback-city",
	})

	got, err := scanWithRoot(root, "ga-city")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].City != "/tmp/fallback-city" {
		t.Fatalf("City = %q, want GC_CITY fallback value", got[0].City)
	}
}

func TestScanWithRootMissingEnvironSkipped(t *testing.T) {
	root := t.TempDir()
	// Directory exists but no environ (ENOENT) — should be skipped without error.
	dir := filepath.Join(root, "400")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, err := scanWithRoot(root, "")
	if err != nil {
		t.Fatalf("scanWithRoot error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries, want 0", len(got))
	}
}

func TestScanWithRootMarksLegacyManagedDoltWatchdogByArgvSentinel(t *testing.T) {
	root := t.TempDir()
	const (
		pid      = 410
		cityPath = "/home/test/gas-city"
	)
	configPath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	buildFakeProc(t, root, pid, map[string]string{
		"GC_SESSION_ID": "gc-closed-session",
		"GC_CITY_PATH":  cityPath,
	})
	writeFakeProcCmdline(t, root, pid,
		"/usr/local/bin/gc",
		"__gc-managed-dolt-scope-watchdog",
		configPath,
		filepath.Join(filepath.Dir(configPath), "dolt.log"),
		cityPath,
	)

	got, err := scanWithRoot(root, "gc-closed-session")
	if err != nil {
		t.Fatalf("scanWithRoot: %v", err)
	}
	if len(got) != 1 || !got[0].ManagedDolt {
		t.Fatalf("scanWithRoot = %+v, want one protected managed-Dolt watchdog", got)
	}
}

func TestScanWithRootRequiresManagedDoltCommandShapeForEnvironmentSentinel(t *testing.T) {
	root := t.TempDir()
	const (
		pid      = 420
		cityPath = "/home/test/gas-city"
	)
	buildFakeProc(t, root, pid, map[string]string{
		"GC_SESSION_ID":           "gc-closed-session",
		"GC_CITY_PATH":            cityPath,
		"GC_MANAGED_DOLT_PROCESS": "1",
	})
	writeFakeProcCmdline(t, root, pid, "/usr/bin/sleep", "300")

	got, err := scanWithRoot(root, "gc-closed-session")
	if err != nil {
		t.Fatalf("scanWithRoot: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("scanWithRoot returned %d runtimes, want 1", len(got))
	}
	if got[0].ManagedDolt {
		t.Fatalf("ordinary process with forged env sentinel was protected: %+v", got[0])
	}
}

func TestScanWithRootRejectsForgedManagedDoltWatchdogPaths(t *testing.T) {
	root := t.TempDir()
	const (
		pid      = 425
		cityPath = "/home/test/gas-city"
	)
	configPath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	buildFakeProc(t, root, pid, map[string]string{
		"GC_SESSION_ID": "gc-closed-session",
		"GC_CITY_PATH":  cityPath,
	})
	writeFakeProcCmdline(t, root, pid,
		"/usr/local/bin/gc",
		"__gc-managed-dolt-scope-watchdog",
		configPath,
		"/tmp/forged-dolt.log",
		cityPath,
	)

	got, err := scanWithRoot(root, "gc-closed-session")
	if err != nil {
		t.Fatalf("scanWithRoot: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("scanWithRoot returned %d runtimes, want 1", len(got))
	}
	if got[0].ManagedDolt {
		t.Fatalf("watchdog with forged log path was protected: %+v", got[0])
	}
}

func TestScanWithRootMarksSentinelManagedDoltServerWithCanonicalConfig(t *testing.T) {
	root := t.TempDir()
	const (
		pid      = 430
		cityPath = "/home/test/gas-city"
	)
	configPath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")
	buildFakeProc(t, root, pid, map[string]string{
		"GC_SESSION_ID":           "gc-closed-session",
		"GC_CITY_PATH":            cityPath,
		"GC_MANAGED_DOLT_PROCESS": "1",
	})
	writeFakeProcCmdline(t, root, pid, "dolt", "sql-server", "--config", configPath)

	got, err := scanWithRoot(root, "gc-closed-session")
	if err != nil {
		t.Fatalf("scanWithRoot: %v", err)
	}
	if len(got) != 1 || !got[0].ManagedDolt {
		t.Fatalf("scanWithRoot = %+v, want one protected managed-Dolt server", got)
	}
}
