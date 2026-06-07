package dolt_test

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const syncScript = "commands/sync/run.sh"

// syncFilteredEnv returns os.Environ() with every env var the sync script reads
// stripped, so a test's GC_DOLT_* config is exactly what the test sets and never
// what the developer/CI happens to export. GC_DOLT_SYNC_PUSH_TIMEOUT_SECS is in
// the set because its validator (run.sh) now runs unconditionally on every
// invocation and exits 2 on any invalid value — an ambient invalid value would
// otherwise flip success-path tests red and make the refspec-failure tests pass
// for the wrong reason. Centralizing the key list here keeps every sync test
// hermetic against the same surface and prevents the strip-set from drifting
// per call site.
func syncFilteredEnv() []string {
	return filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
		"GC_DOLT_SYNC_PUSH_TIMEOUT_SECS", "GC_DOLT_SYNC_FETCH_TIMEOUT_SECS",
	)
}

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
  *"CALL DOLT_FETCH("*)
    :
    ;;
  *"..remotes/origin/"*)
    printf 'n\n0\n'
    ;;
  *"dolt_log('remotes/origin/"*)
    printf 'n\n1\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

func writeSyncFakeDoltActiveBranch(t *testing.T, dir, activeBranch string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes LIMIT 1"*)
    printf 'name,url\norigin,https://example.invalid/repo\n'
    ;;
  *"CALL DOLT_FETCH("*)
    :
    ;;
  *"..remotes/origin/"*)
    printf 'n\n0\n'
    ;;
  *"dolt_log('remotes/origin/"*)
    printf 'n\n1\n'
    ;;
  *"SELECT active_branch()"*)
    printf 'active_branch()\n` + activeBranch + `\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

func writeSyncFakeDoltInvalidActiveBranch(t *testing.T, dir string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes LIMIT 1"*)
    printf 'name,url\norigin,https://example.invalid/repo\n'
    ;;
  *"CALL DOLT_FETCH("*)
    :
    ;;
  *"..remotes/origin/"*)
    printf 'n\n0\n'
    ;;
  *"dolt_log('remotes/origin/"*)
    printf 'n\n1\n'
    ;;
  *"SELECT active_branch()"*)
    printf 'active_branch()\n--force\n'
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

// writeSyncFakeDoltPushFails installs a fake dolt that answers the SQL-mode
// remote-lookup and active-branch queries normally but fails the DOLT_PUSH call
// with the given exit code, writing stderr (when non-empty) to its own stderr.
// It is parameterized by (exitCode, stderr) so one helper covers every SQL push
// failure case (timeout exit 124, transport exit 7, stderr replay, etc.) rather
// than N hardcoded-exit functions. The remotes-lookup query must still succeed
// or the push site is never reached.
func writeSyncFakeDoltPushFails(t *testing.T, dir string, exitCode int, stderr string) {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	stderrEmit := ""
	if stderr != "" {
		// Escape single quotes so an arbitrary stderr value cannot break out
		// of the single-quoted shell literal: ' becomes '\''.
		escaped := strings.ReplaceAll(stderr, "'", `'\''`)
		stderrEmit = "printf '%s\\n' '" + escaped + "' >&2"
	}
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes LIMIT 1"*)
    printf 'name,url\norigin,https://example.invalid/repo\n'
    exit 0
    ;;
  *"CALL DOLT_FETCH("*)
    :
    ;;
  *"..remotes/origin/"*)
    printf 'n\n0\n'
    ;;
  *"dolt_log('remotes/origin/"*)
    printf 'n\n1\n'
    ;;
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n'
    exit 0
    ;;
  *DOLT_PUSH*)
    ` + stderrEmit + `
    exit ` + strconv.Itoa(exitCode) + `
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

// writeSyncFakeDoltPushFailsNoTrailingNewline installs a fake dolt whose
// SQL-mode DOLT_PUSH failure writes a two-line stderr whose FINAL line lacks a
// trailing newline (a terse dolt diagnostic or a SIGKILL-truncated write). Every
// other helper emits via `printf '%s\n'`, so only this one exercises the
// replay loop's last-line flush: a `while read` without the `|| [ -n "$line" ]`
// guard would drop the unterminated final line — the swallowed-failure class
// this command set exists to surface.
func writeSyncFakeDoltPushFailsNoTrailingNewline(t *testing.T, dir string, exitCode int) {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes LIMIT 1"*)
    printf 'name,url\norigin,https://example.invalid/repo\n'
    exit 0
    ;;
  *"CALL DOLT_FETCH("*)
    :
    ;;
  *"..remotes/origin/"*)
    printf 'n\n0\n'
    ;;
  *"dolt_log('remotes/origin/"*)
    printf 'n\n1\n'
    ;;
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n'
    exit 0
    ;;
  *DOLT_PUSH*)
    printf 'error: push diagnostics follow\n' >&2
    printf '%s' 'fatal: last line no newline' >&2
    exit ` + strconv.Itoa(exitCode) + `
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

// writeSyncFakeDoltCLIPushFails installs a fake dolt for CLI-mode tests that
// fails the `dolt push` invocation with the given exit code (writing a stderr
// line). CLI mode resolves remotes/refspec from disk, so the push is the only
// dolt call.
func writeSyncFakeDoltCLIPushFails(t *testing.T, dir string, exitCode int) {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"push "*)
    printf 'remote rejected push\n' >&2
    exit ` + strconv.Itoa(exitCode) + `
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

// writeSyncFakeDoltPushEchoesArgs installs a fake dolt that, on the SQL-mode
// DOLT_PUSH call, reports whether DOLT_CLI_PASSWORD was delivered via the
// environment and echoes its own full argv to stderr (mimicking a dolt that
// prints its command line in a connection diagnostic), then fails. This lets a
// test prove the password reaches dolt via the env var (non-vacuous: the
// 'cli-password-was-set' marker only fires when DOLT_CLI_PASSWORD is non-empty)
// AND never as an argv flag — so the replayed argv cannot leak the secret (RB6).
func writeSyncFakeDoltPushEchoesArgs(t *testing.T, dir string) {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes LIMIT 1"*)
    printf 'name,url\norigin,https://example.invalid/repo\n'
    exit 0
    ;;
  *"CALL DOLT_FETCH("*)
    :
    ;;
  *"..remotes/origin/"*)
    printf 'n\n0\n'
    ;;
  *"dolt_log('remotes/origin/"*)
    printf 'n\n1\n'
    ;;
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n'
    exit 0
    ;;
  *DOLT_PUSH*)
    [ -n "$DOLT_CLI_PASSWORD" ] && printf 'cli-password-was-set\n' >&2
    printf 'push failed; dolt invoked with args: %s\n' "$*" >&2
    exit 1
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

// writeSyncFakeFailingMktemp installs a fake `mktemp` on PATH that always fails,
// reproducing a broken/unwritable TMPDIR. sync only calls mktemp at the SQL push
// site, so this exercises the per-db temp-file guard without affecting the
// earlier metadata queries.
func writeSyncFakeFailingMktemp(t *testing.T, dir string) {
	t.Helper()
	body := `#!/bin/sh
echo "mktemp: failed to create file" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "mktemp"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake mktemp: %v", err)
	}
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
	cmd.Env = append(syncFilteredEnv(),
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

	// The success line is a parsed contract — assert it byte-for-byte so a
	// format change is caught here rather than breaking downstream consumers.
	if !strings.Contains(string(out), "  app: pushed main -> origin:main (https://example.invalid/repo)") {
		t.Fatalf("output missing byte-for-byte success line:\n%s", out)
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
	cmd.Env = append(syncFilteredEnv(),
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

func TestSyncForceUsesResolvedActiveBranchWithLiveSQL(t *testing.T) {
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
	doltLog := writeSyncFakeDoltActiveBranch(t, binDir, "gascity-3")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app", "--force")
	cmd.Env = append(syncFilteredEnv(),
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
	want := "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'gascity-3')"
	if !strings.Contains(log, want) {
		t.Fatalf("force sync should use resolved active branch\nwant %q\nlog:\n%s\noutput:\n%s", want, log, out)
	}
}

func TestSyncForceUsesRefspecEnvOverrideWithLiveSQL(t *testing.T) {
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
	doltLog := writeSyncFakeDoltActiveBranch(t, binDir, "main")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app", "--force")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_REFSPEC_APP=main:gascity-3",
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
	want := "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main:gascity-3')"
	if !strings.Contains(log, want) {
		t.Fatalf("force sync should use refspec override\nwant %q\nlog:\n%s\noutput:\n%s", want, log, out)
	}
}

func TestSyncDryRunShowsResolvedActiveBranch(t *testing.T) {
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
	_ = writeSyncFakeDoltActiveBranch(t, binDir, "gascity-3")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app", "--dry-run")
	cmd.Env = append(syncFilteredEnv(),
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
		t.Fatalf("gc dolt sync --dry-run failed: %v\n%s", err, out)
	}
	want := "app: would push gascity-3 -> origin:gascity-3 (https://example.invalid/repo)"
	if !strings.Contains(string(out), want) {
		t.Fatalf("dry run output should show resolved refspec\nwant %q\ngot:\n%s", want, out)
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
	cmd.Env = append(syncFilteredEnv(),
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
	cmd.Env = append(syncFilteredEnv(),
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
	cmd.Env = append(syncFilteredEnv(),
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

// TestSyncPushesActiveBranchWhenSet verifies that when the live SQL server
// reports a non-'main' active branch, gc dolt sync pushes that branch (to a
// same-named remote ref) rather than the hardcoded 'main' fallback.
func TestSyncPushesActiveBranchWhenSet(t *testing.T) {
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
	doltLog := writeSyncFakeDoltActiveBranch(t, binDir, "gascity-3")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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

	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "CALL DOLT_PUSH('origin', 'gascity-3')") {
		t.Fatalf("expected push of active branch gascity-3, got:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("unexpected fallback to main:\n%s", log)
	}
}

// TestSyncRefspecEnvOverride verifies that GC_DOLT_REFSPEC_<DB> overrides the
// active-branch default with a <local>:<remote> mapping.
func TestSyncRefspecEnvOverride(t *testing.T) {
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
	// Active branch from SQL is "main"; the env override should win and map
	// main -> gascity-3 on the remote.
	doltLog := writeSyncFakeDoltActiveBranch(t, binDir, "main")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_REFSPEC_APP=main:gascity-3",
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
	if !strings.Contains(log, "CALL DOLT_PUSH('origin', 'main:gascity-3')") {
		t.Fatalf("expected refspec push main:gascity-3, got:\n%s\noutput:\n%s", log, out)
	}
}

// TestSyncRefspecEnvOverrideHyphenInDBName verifies that DB names containing
// hyphens are correctly translated to env-var keys (hyphens -> underscores,
// lowercase -> uppercase). The DB "my-app" expects GC_DOLT_REFSPEC_MY_APP.
func TestSyncRefspecEnvOverrideHyphenInDBName(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "my-app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writeSyncFakeDoltActiveBranch(t, binDir, "main")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "my-app")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_REFSPEC_MY_APP=feat-x:trunk",
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
	if !strings.Contains(log, "CALL DOLT_PUSH('origin', 'feat-x:trunk')") {
		t.Fatalf("expected refspec push feat-x:trunk for my-app, got:\n%s\noutput:\n%s", log, out)
	}
}

// TestSyncCLIFallbackReadsRepoStateForActiveBranch verifies that when the SQL
// server is unreachable, the CLI fallback reads repo_state.json to determine
// the active branch instead of defaulting to 'main'.
func TestSyncCLIFallbackReadsRepoStateForActiveBranch(t *testing.T) {
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
	repoState := `{"head":"refs/heads/gascity-3"}`
	if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "repo_state.json"), []byte(repoState), 0o644); err != nil {
		t.Fatalf("write repo_state: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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
	if !strings.Contains(log, "push origin gascity-3") {
		t.Fatalf("CLI fallback should push the repo_state head 'gascity-3', got:\n%s\noutput:\n%s", log, out)
	}
}

func TestSyncCLIFallbackIgnoresNestedRepoStateHead(t *testing.T) {
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
	repoState := `{
  "working": {
    "head": "refs/heads/wrong"
  },
  "head": "refs/heads/gascity-3"
}`
	if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "repo_state.json"), []byte(repoState), 0o644); err != nil {
		t.Fatalf("write repo_state: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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
	if !strings.Contains(log, "push origin gascity-3") {
		t.Fatalf("CLI fallback should push top-level repo_state head, got:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, "push origin wrong") {
		t.Fatalf("CLI fallback must ignore nested repo_state head, got:\n%s\noutput:\n%s", log, out)
	}
}

// TestSyncRefspecInvalidOverrideFails ensures that a malformed
// GC_DOLT_REFSPEC_<DB> value (e.g. with shell-unsafe characters) causes sync
// to fail loudly rather than silently fall back.
func TestSyncRefspecInvalidOverrideFails(t *testing.T) {
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
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_REFSPEC_APP=main:bad branch", // space is invalid in branch name
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected sync to fail on invalid refspec override, output:\n%s", out)
	}
	if !strings.Contains(string(out), "invalid refspec override") {
		t.Fatalf("expected error message about invalid refspec override, got:\n%s", out)
	}
}

func TestSyncRefspecOptionShapedOverrideFails(t *testing.T) {
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
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_REFSPEC_APP=--force",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected sync to fail on option-shaped refspec override, output:\n%s", out)
	}
	if !strings.Contains(string(out), "invalid refspec override") {
		t.Fatalf("expected invalid refspec override message, got:\n%s", out)
	}
}

func TestSyncWarnsWhenActiveBranchFallbacksToMain(t *testing.T) {
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
	doltLog := writeSyncFakeDoltInvalidActiveBranch(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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
	if !strings.Contains(string(out), "WARN: active branch unresolved; falling back to main") {
		t.Fatalf("expected fallback warning, got:\n%s", out)
	}
	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("fallback should push main after warning\nlog:\n%s\noutput:\n%s", log, out)
	}
}

// TestSyncSQLPushTimeoutReportsTimeout verifies that an exit-124 push (the
// run_bounded timeout convention) is reported as a TIMEOUT naming the ceiling
// and env var, and is NOT collapsed into the generic non-timeout error (R1).
func TestSyncSQLPushTimeoutReportsTimeout(t *testing.T) {
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
	writeSyncFakeDoltPushFails(t, binDir, 124, "")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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
		t.Fatalf("expected push failure, output:\n%s", out)
	}
	if !strings.Contains(string(out), "TIMEOUT after 1800s") {
		t.Fatalf("expected TIMEOUT message naming the 1800s ceiling, got:\n%s", out)
	}
	if !strings.Contains(string(out), "GC_DOLT_SYNC_PUSH_TIMEOUT_SECS") {
		t.Fatalf("expected TIMEOUT message to name the env var, got:\n%s", out)
	}
	if strings.Contains(string(out), "ERROR: push failed (exit") {
		t.Fatalf("timeout must not be reported as a generic exit-code failure:\n%s", out)
	}
}

// TestSyncSQLPushReportsExitCode verifies a non-124 SQL push failure reports the
// underlying exit code and is distinguishable from a timeout (R3).
func TestSyncSQLPushReportsExitCode(t *testing.T) {
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
	writeSyncFakeDoltPushFails(t, binDir, 7, "")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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
		t.Fatalf("expected push failure, output:\n%s", out)
	}
	if !strings.Contains(string(out), "ERROR: push failed (exit 7)") {
		t.Fatalf("expected exit-code-7 failure message, got:\n%s", out)
	}
	if strings.Contains(string(out), "TIMEOUT") {
		t.Fatalf("non-124 failure must not be reported as a timeout:\n%s", out)
	}
}

// TestSyncSQLPushReplaysStderr verifies the underlying dolt stderr is surfaced
// on push failure (R2). Runs on the empty-password harness so the assertion is
// non-vacuous (RB6 — the credential-safe replay actually emits the line).
func TestSyncSQLPushReplaysStderr(t *testing.T) {
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
	writeSyncFakeDoltPushFails(t, binDir, 1, "fatal: authentication required")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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
		t.Fatalf("expected push failure, output:\n%s", out)
	}
	if !strings.Contains(string(out), "fatal: authentication required") {
		t.Fatalf("expected underlying dolt stderr to be surfaced, got:\n%s", out)
	}
	if !strings.Contains(string(out), "app: fatal: authentication required") {
		t.Fatalf("replayed stderr should be prefixed with the db name, got:\n%s", out)
	}
}

// TestSyncSQLPushReplaysStderrFinalLineWithoutTrailingNewline verifies the
// stderr replay surfaces a final line that lacks a trailing newline — a terse
// dolt diagnostic or a SIGKILL-truncated write. POSIX `read` returns non-zero
// at an unterminated EOF, so without the loop's `|| [ -n "$line" ]` flush the
// last line is captured but never replayed, re-introducing the swallowed
// failure this command set exists to surface.
func TestSyncSQLPushReplaysStderrFinalLineWithoutTrailingNewline(t *testing.T) {
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
	writeSyncFakeDoltPushFailsNoTrailingNewline(t, binDir, 1)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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
		t.Fatalf("expected push failure, output:\n%s", out)
	}
	// The newline-less final line must still be surfaced, db-prefixed.
	if !strings.Contains(string(out), "app: fatal: last line no newline") {
		t.Fatalf("replay dropped the final stderr line lacking a trailing newline, got:\n%s", out)
	}
	// The preceding (newline-terminated) line must survive too.
	if !strings.Contains(string(out), "app: error: push diagnostics follow") {
		t.Fatalf("expected the first stderr line to be surfaced, got:\n%s", out)
	}
}

// TestSyncSQLPushEmptyStderrNoBlankLines verifies a failure with empty stderr
// emits exactly one structured error line and no spurious db-prefixed blank
// lines (AC4 — guards the replay-loop no-op).
func TestSyncSQLPushEmptyStderrNoBlankLines(t *testing.T) {
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
	writeSyncFakeDoltPushFails(t, binDir, 9, "")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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
		t.Fatalf("expected push failure, output:\n%s", out)
	}
	if n := strings.Count(string(out), "ERROR: push failed (exit 9)"); n != 1 {
		t.Fatalf("expected exactly one structured error line, got %d:\n%s", n, out)
	}
	if strings.Contains(string(out), "  app: \n") {
		t.Fatalf("empty stderr must not produce a db-prefixed blank line:\n%s", out)
	}
}

// TestSyncSQLPushTimeoutHonorsConfiguredCeiling verifies the TIMEOUT message
// names the configured ceiling, not a hardcoded default (R1/R5, AC2).
func TestSyncSQLPushTimeoutHonorsConfiguredCeiling(t *testing.T) {
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
	writeSyncFakeDoltPushFails(t, binDir, 124, "")
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_SYNC_PUSH_TIMEOUT_SECS=3600",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected push failure, output:\n%s", out)
	}
	if !strings.Contains(string(out), "TIMEOUT after 3600s") {
		t.Fatalf("expected TIMEOUT message to name the configured 3600s ceiling, got:\n%s", out)
	}
	if strings.Contains(string(out), "1800s") {
		t.Fatalf("TIMEOUT message must not name the hardcoded default when overridden:\n%s", out)
	}
}

// TestSyncSQLPushReplayDoesNotLeakPassword exercises credential non-exposure
// (RB6) with a NON-EMPTY password. The fake dolt confirms it received the
// password via DOLT_CLI_PASSWORD (the 'cli-password-was-set' marker makes the
// test non-vacuous: it fires only when the env var is non-empty) and echoes its
// own full argv to stderr; yet the secret value must never appear in any
// replayed line because sync passes the password through the environment, never
// as an argv flag. Converts the by-construction RB6 safety into exercised
// safety on the security regression boundary.
func TestSyncSQLPushReplayDoesNotLeakPassword(t *testing.T) {
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
	writeSyncFakeDoltPushEchoesArgs(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	const secret = "s3cr3t-push-token"
	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD="+secret,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected push failure, output:\n%s", out)
	}
	// Non-vacuity guard: the password must actually have reached dolt via the
	// env var, otherwise the no-leak assertion below proves nothing.
	if !strings.Contains(string(out), "cli-password-was-set") {
		t.Fatalf("expected the password to be delivered to dolt via DOLT_CLI_PASSWORD (test is vacuous otherwise), got:\n%s", out)
	}
	// The push diagnostic (with dolt's echoed argv) must be surfaced, but the
	// secret value must not appear anywhere in the replayed output.
	if !strings.Contains(string(out), "push failed; dolt invoked with args") {
		t.Fatalf("expected the dolt push diagnostic to be replayed, got:\n%s", out)
	}
	if strings.Contains(string(out), secret) {
		t.Fatalf("password leaked into sync output — it must reach dolt via DOLT_CLI_PASSWORD, never argv:\n%s", out)
	}
}

// TestSyncSQLPushTimeoutReplaysNoMechanismMarker verifies the exit-124 branch
// replays a non-empty captured stderr, surfacing the run_bounded "cannot run
// bounded command" no-mechanism marker that is otherwise reported under the
// (deliberately overloaded) TIMEOUT headline. Exercises the S3-on-124 mitigation
// that disambiguates a real wall-clock timeout from the no-mechanism
// fall-through.
func TestSyncSQLPushTimeoutReplaysNoMechanismMarker(t *testing.T) {
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
	const marker = "dolt runtime: timeout/gtimeout/python3 not found; cannot run bounded command"
	writeSyncFakeDoltPushFails(t, binDir, 124, marker)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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
		t.Fatalf("expected push failure, output:\n%s", out)
	}
	if !strings.Contains(string(out), "TIMEOUT after 1800s") {
		t.Fatalf("expected the TIMEOUT headline on exit 124, got:\n%s", out)
	}
	if !strings.Contains(string(out), "app: "+marker) {
		t.Fatalf("expected the captured no-mechanism marker to be replayed (db-prefixed) on the 124 branch, got:\n%s", out)
	}
}

// TestSyncSQLPushTempFileFailureDegradesPerDb verifies that a failure to create
// the stderr-capture temp file degrades to a per-database error instead of an
// opaque whole-run abort under `set -e` — the swallowed-failure class this bead
// targets. A fake `mktemp` on PATH always fails; sync calls mktemp only at the
// SQL push site, so the metadata queries still succeed and the guard is what is
// exercised.
func TestSyncSQLPushTempFileFailureDegradesPerDb(t *testing.T) {
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
	_ = writeSyncFakeDolt(t, binDir)
	writeSyncFakeFailingMktemp(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
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
		t.Fatalf("expected non-zero exit when the temp file cannot be created, output:\n%s", out)
	}
	if !strings.Contains(string(out), "app: ERROR: cannot create temp file") {
		t.Fatalf("expected a per-db temp-file error instead of an opaque abort, got:\n%s", out)
	}
}

// assertSyncRejectsInvalidPushTimeout drives one invalid-timeout scenario: it
// runs sync with GC_DOLT_SYNC_PUSH_TIMEOUT_SECS=bad and asserts the script
// aborts with exit 2 and a stderr diagnostic before any database is touched —
// dolt is never invoked (R5 input validation). Parameterized by the bad value
// so each scenario keeps its own standalone test func (no table-driven block).
func assertSyncRejectsInvalidPushTimeout(t *testing.T, bad string) {
	t.Helper()
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

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_SYNC_PUSH_TIMEOUT_SECS="+bad,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected exit 2 for invalid timeout %q, output:\n%s", bad, out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("expected exit code 2 for invalid timeout %q, got %v:\n%s", bad, err, out)
	}
	if !strings.Contains(string(out), "invalid GC_DOLT_SYNC_PUSH_TIMEOUT_SECS") {
		t.Fatalf("expected validation diagnostic for %q, got:\n%s", bad, out)
	}
	if data, rerr := os.ReadFile(doltLog); rerr == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("dolt must not be invoked when the timeout is invalid (%q), log:\n%s", bad, data)
	}
}

// TestSyncRejectsNonNumericPushTimeout verifies a non-numeric
// GC_DOLT_SYNC_PUSH_TIMEOUT_SECS is rejected with exit 2 (R5 input validation).
func TestSyncRejectsNonNumericPushTimeout(t *testing.T) {
	assertSyncRejectsInvalidPushTimeout(t, "abc")
}

// TestSyncRejectsZeroPushTimeout verifies a zero GC_DOLT_SYNC_PUSH_TIMEOUT_SECS
// is rejected with exit 2 — a 0s ceiling would SIGKILL the push immediately and
// emit a misleading TIMEOUT message (R5 input validation).
func TestSyncRejectsZeroPushTimeout(t *testing.T) {
	assertSyncRejectsInvalidPushTimeout(t, "0")
}

// TestSyncRejectsEmptyPushTimeout verifies an empty GC_DOLT_SYNC_PUSH_TIMEOUT_SECS
// is rejected with exit 2 (R5 input validation).
func TestSyncRejectsEmptyPushTimeout(t *testing.T) {
	assertSyncRejectsInvalidPushTimeout(t, "")
}

// TestSyncRejectsLeadingZeroPushTimeout verifies the leading-zero numeric-zero
// form "00" is rejected with exit 2 before any dolt invocation. A guard matching
// only the literal "0" lets "00" through, and GNU `timeout` treats a 0 duration
// as "disable the timeout" — running the push UNBOUNDED and re-opening the
// anti-hang hole RB2 exists to close (R5 input validation).
func TestSyncRejectsLeadingZeroPushTimeout(t *testing.T) {
	assertSyncRejectsInvalidPushTimeout(t, "00")
}

// TestSyncRejectsTripleZeroPushTimeout verifies a longer all-zeros form "000" is
// also rejected with exit 2 — the zero-guard must reject every numeric-zero
// spelling, not just the literal "0" (RB2; R5 input validation).
func TestSyncRejectsTripleZeroPushTimeout(t *testing.T) {
	assertSyncRejectsInvalidPushTimeout(t, "000")
}

// TestSyncCLIPushReportsExitCode verifies the CLI-mode plain push surfaces the
// underlying exit code instead of a generic message (R4).
func TestSyncCLIPushReportsExitCode(t *testing.T) {
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
	writeSyncFakeDoltCLIPushFails(t, binDir, 3)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_PORT=1",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected CLI push failure, output:\n%s", out)
	}
	if !strings.Contains(string(out), "ERROR: push failed (exit 3)") {
		t.Fatalf("expected CLI exit-code-3 failure message, got:\n%s", out)
	}
}

// TestSyncCLIForcePushReportsExitCode verifies the CLI-mode --force branch
// independently captures and reports its exit code — it could otherwise ship
// still swallowing $? (R4; observability force-branch coverage).
func TestSyncCLIForcePushReportsExitCode(t *testing.T) {
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
	writeSyncFakeDoltCLIPushFails(t, binDir, 5)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app", "--force")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_PORT=1",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected CLI --force push failure, output:\n%s", out)
	}
	if !strings.Contains(string(out), "ERROR: push failed (exit 5)") {
		t.Fatalf("expected CLI --force exit-code-5 failure message, got:\n%s", out)
	}
}
