package dolt_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const syncScript = "commands/sync/run.sh"

func startReachableTCPListener(t *testing.T) (int, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				close(done)
				return
			}
			_ = conn.Close()
		}
	}()
	cleanup := func() {
		_ = listener.Close()
		<-done
	}
	return listener.Addr().(*net.TCPAddr).Port, cleanup
}

func writeSyncFakeDolt(t *testing.T, dir string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes LIMIT 1"*)
    printf 'name,url\norigin,https://example.invalid/repo\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

func writeSyncFakeDoltRemoteLookupFailure(t *testing.T, dir string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes LIMIT 1"*)
    printf 'sql lookup failed\n' >&2
    exit 7
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

func writeSyncFakeBeadsBD(t *testing.T, cityPath string) string {
	t.Helper()
	scriptDir := filepath.Join(cityPath, ".gc", "system", "packs", "bd", "assets", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bd dir: %v", err)
	}
	logPath := filepath.Join(cityPath, "bd.log")
	body := `#!/bin/sh
printf '%s\n' "$1" >> "` + logPath + `"
exit 0
`
	if err := os.WriteFile(filepath.Join(scriptDir, "gc-beads-bd.sh"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake bd script: %v", err)
	}
	return logPath
}

func TestSyncUsesLiveSQLWhenManagedServerReachable(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writeSyncFakeDolt(t, binDir)
	bdLog := writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}

	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("sync called gc-beads-bd while server was reachable: %q", data)
	}

	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"SELECT name, url FROM dolt_remotes LIMIT 1",
		"CALL DOLT_PUSH('origin', 'main')",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("dolt log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	for _, unwanted := range []string{
		"CALL DOLT_ADD",
		"CALL DOLT_COMMIT",
	} {
		if strings.Contains(log, unwanted) {
			t.Fatalf("sync should not auto-commit working changes via SQL; found %q\nlog:\n%s", unwanted, log)
		}
	}
}

func TestSyncForceUsesSetUpstreamWithLiveSQL(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app", "--force")
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc dolt sync --force failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	log := string(data)
	want := "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')"
	if !strings.Contains(log, want) {
		t.Fatalf("force sync should set upstream\nwant %q\nlog:\n%s\noutput:\n%s", want, log, out)
	}
}

func TestSyncSkipsDatabasesWithNoSyncMarker(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	dbDir := filepath.Join(dataDir, "app")
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, ".no-sync"), []byte("skip\n"), 0o644); err != nil {
		t.Fatalf("write no-sync marker: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}

	if data, err := os.ReadFile(doltLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("sync touched database with .no-sync marker: %q\noutput:\n%s", data, out)
	}
	if !strings.Contains(string(out), "app: skipped (.no-sync)") {
		t.Fatalf("output missing .no-sync skip:\n%s", out)
	}
}

func TestSyncReportsLiveSQLRemoteLookupFailure(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writeSyncFakeDoltRemoteLookupFailure(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc dolt sync succeeded despite remote lookup failure:\n%s", out)
	}
	if !strings.Contains(string(out), "app: ERROR: failed to query remotes") {
		t.Fatalf("output missing remote lookup failure:\n%s", out)
	}
	if strings.Contains(string(out), "skipped (no remote)") {
		t.Fatalf("remote lookup failure should not be reported as no remote:\n%s", out)
	}

	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "SELECT name, url FROM dolt_remotes LIMIT 1") {
		t.Fatalf("dolt log missing remote lookup:\n%s", log)
	}
	if strings.Contains(log, "DOLT_PUSH") {
		t.Fatalf("sync should not push after remote lookup failure:\n%s", log)
	}
}

func TestSyncCLIFallbackPushesOriginMain(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	dbDir := filepath.Join(dataDir, "app")
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	remotes := `{"remotes":[{"name":"origin","url":"https://example.invalid/repo"}]}`
	if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "remotes.json"), []byte(remotes), 0o644); err != nil {
		t.Fatalf("write remotes: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_PORT=1",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "push origin main") {
		t.Fatalf("CLI fallback should push explicit origin main\nlog:\n%s\noutput:\n%s", log, out)
	}
}
