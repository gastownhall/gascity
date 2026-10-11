package dolt_test

// Remote-selection determinism: `gc dolt pull` must not guess which remote
// to pull FROM when a database has more than one configured — an unordered
// pick risks silently reconciling from a public/network remote (or a stale
// mirror) instead of the fleet-private source of truth. This is pull's own
// policy: refuses outright unless GC_DOLT_REMOTE_<DB> names one of the
// candidates; a non-file:// pick additionally requires
// GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1. The same locality/allow-flag check also
// applies to a database with exactly one configured remote (ga-nht26j) — a
// sole non-local remote is not exempt just because there was nothing to
// disambiguate. It is not currently identical to sync's remote-selection
// fix (ga-2w96wd, not yet on main) — consolidating the two into a shared
// helper is a future intent, not current behavior. These tests cover both
// the SQL-mode (dolt_remotes table) and CLI-mode (.dolt/remotes.json)
// candidate sources.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writePullFakeDoltMultiRemote installs a fake dolt reporting three configured
// remotes for the SQL-mode remote-lookup query: "alpha" (a decoy file://
// remote, listed first, so a test selecting "internal" or "public" proves
// selection went through the override rather than an accidental first-row
// pick), "internal" (a file:// remote), and "public" (a non-file:// remote).
// CALL DOLT_PULL falls through to the default success path.
func writePullFakeDoltMultiRemote(t *testing.T, dir string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes"*)
    printf 'name,url\nalpha,file:///data/remotes/alpha\ninternal,file:///data/remotes/internal\npublic,https://public.example.invalid/repo\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

func TestPullSQLMultipleRemotesRefusesWithoutOverride(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writePullFakeDoltMultiRemote(t, binDir)
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
		"GC_DOLT_REMOTE_APP=",
		"GC_DOLT_PULL_ALLOW_REMOTE_APP=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc dolt pull should refuse an ambiguous multi-remote database:\n%s", out)
	}

	output := string(out)
	if !strings.Contains(output, "multiple remotes configured") {
		t.Fatalf("expected a 'multiple remotes configured' refusal:\n%s", output)
	}
	if !strings.Contains(output, "internal") || !strings.Contains(output, "public") {
		t.Fatalf("expected the refusal to list both candidate remote names:\n%s", output)
	}
	if strings.Contains(output, "failed to query remotes") {
		t.Fatalf("a policy refusal must not be reported as a query failure:\n%s", output)
	}

	log := readLog(t, doltLog)
	if strings.Contains(log, "DOLT_PULL") {
		t.Fatalf("an ambiguous database must NOT be pulled:\nlog:\n%s", log)
	}
}

func TestPullSQLMultipleRemotesOverrideSelectsFileRemote(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writePullFakeDoltMultiRemote(t, binDir)
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
		"GC_DOLT_REMOTE_APP=internal",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc dolt pull with a valid override should succeed:\n%s", out)
	}

	output := string(out)
	if strings.Contains(output, "multiple remotes configured") {
		t.Fatalf("a valid override must not be refused as ambiguous:\n%s", output)
	}

	log := readLog(t, doltLog)
	want := "CALL DOLT_PULL('internal', 'main')"
	if !strings.Contains(log, want) {
		t.Fatalf("expected pull from the overridden 'internal' remote.\nwant %q\nlog:\n%s\noutput:\n%s", want, log, out)
	}
}

func TestPullSQLMultipleRemotesNonFileOverrideRequiresAllowFlag(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writePullFakeDoltMultiRemote(t, binDir)
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
		"GC_DOLT_REMOTE_APP=public",
		"GC_DOLT_PULL_ALLOW_REMOTE_APP=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a non-file:// override without the allow flag should be refused:\n%s", out)
	}

	output := string(out)
	if !strings.Contains(output, "non-local remote 'public'") {
		t.Fatalf("expected a non-local-remote refusal naming 'public':\n%s", output)
	}
	if !strings.Contains(output, "GC_DOLT_PULL_ALLOW_REMOTE") {
		t.Fatalf("expected the refusal to name the allow-flag escape hatch:\n%s", output)
	}

	log := readLog(t, doltLog)
	if strings.Contains(log, "DOLT_PULL") {
		t.Fatalf("a gated non-local override must NOT be pulled:\nlog:\n%s", log)
	}
}

func TestPullSQLMultipleRemotesNonFileOverrideWithAllowFlagProceeds(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writePullFakeDoltMultiRemote(t, binDir)
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
		"GC_DOLT_REMOTE_APP=public",
		"GC_DOLT_PULL_ALLOW_REMOTE_APP=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a non-file:// override with the allow flag set should succeed:\n%s", out)
	}

	log := readLog(t, doltLog)
	want := "CALL DOLT_PULL('public', 'main')"
	if !strings.Contains(log, want) {
		t.Fatalf("expected pull from the overridden 'public' remote.\nwant %q\nlog:\n%s\noutput:\n%s", want, log, out)
	}
}

func TestPullCLIMultipleRemotesRefusesWithoutOverride(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	dbDir := filepath.Join(dataDir, "app")
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	// "alpha" is a decoy listed first: proves any override test below
	// selects "internal" via the override, not by accidentally matching a
	// naive first-candidate default.
	remotes := `{"remotes":[{"name":"alpha","url":"file:///data/remotes/alpha"},{"name":"internal","url":"file:///data/remotes/internal"},{"name":"public","url":"https://public.example.invalid/repo"}]}`
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
		"GC_DOLT_REMOTE_APP=",
		"GC_DOLT_PULL_ALLOW_REMOTE_APP=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("CLI-mode pull should refuse an ambiguous multi-remote database:\n%s", out)
	}

	output := string(out)
	if !strings.Contains(output, "multiple remotes configured") {
		t.Fatalf("expected a 'multiple remotes configured' refusal:\n%s", output)
	}

	log := readLog(t, doltLog)
	if strings.Contains(log, "pull") {
		t.Fatalf("an ambiguous CLI-mode database must NOT be pulled:\nlog:\n%s", log)
	}
}

func TestPullSQLMultipleRemotesInvalidOverrideSyntaxRefuses(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writePullFakeDoltMultiRemote(t, binDir)
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
		"GC_DOLT_REMOTE_APP=has spaces",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an override with characters outside [A-Za-z0-9_.-] should be refused:\n%s", out)
	}

	output := string(out)
	if !strings.Contains(output, "invalid GC_DOLT_REMOTE override") {
		t.Fatalf("expected an 'invalid GC_DOLT_REMOTE override' refusal:\n%s", output)
	}
	if !strings.Contains(output, "has spaces") {
		t.Fatalf("expected the refusal to echo back the bad override value:\n%s", output)
	}

	log := readLog(t, doltLog)
	if strings.Contains(log, "DOLT_PULL") {
		t.Fatalf("a syntactically invalid override must NOT be pulled:\nlog:\n%s", log)
	}
}

func TestPullSQLMultipleRemotesUnknownOverrideRefuses(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writePullFakeDoltMultiRemote(t, binDir)
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
		"GC_DOLT_REMOTE_APP=missing",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an override naming a remote absent from the candidate list should be refused:\n%s", out)
	}

	output := string(out)
	if !strings.Contains(output, "GC_DOLT_REMOTE names unknown remote 'missing'") {
		t.Fatalf("expected an 'unknown remote' refusal naming 'missing':\n%s", output)
	}
	if !strings.Contains(output, "alpha") || !strings.Contains(output, "internal") || !strings.Contains(output, "public") {
		t.Fatalf("expected the refusal to list all available candidate remotes:\n%s", output)
	}

	log := readLog(t, doltLog)
	if strings.Contains(log, "DOLT_PULL") {
		t.Fatalf("an override naming an unknown remote must NOT be pulled:\nlog:\n%s", log)
	}
}

func TestPullCLIMultipleRemotesOverrideSelectsNamed(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	dbDir := filepath.Join(dataDir, "app")
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	// "alpha" is a decoy listed first: see TestPullCLIMultipleRemotesRefusesWithoutOverride.
	remotes := `{"remotes":[{"name":"alpha","url":"file:///data/remotes/alpha"},{"name":"internal","url":"file:///data/remotes/internal"},{"name":"public","url":"https://public.example.invalid/repo"}]}`
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
		"GC_DOLT_REMOTE_APP=internal",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CLI-mode pull with a valid override should succeed:\n%s", out)
	}

	log := readLog(t, doltLog)
	if !strings.Contains(log, "pull internal main") {
		t.Fatalf("expected CLI pull from the overridden 'internal' remote.\nlog:\n%s\noutput:\n%s", log, out)
	}
}

// writePullFakeDoltSoleRemote installs a fake dolt reporting exactly one
// configured remote, named "origin", at the given URL, for the SQL-mode
// remote-lookup query. CALL DOLT_PULL falls through to the default success
// path.
func writePullFakeDoltSoleRemote(t *testing.T, dir string, url string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes"*)
    printf 'name,url\norigin,` + url + `\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

// A sole configured remote is not exempt from the locality policy: the
// count<=1 shortcut used to return it unconditionally, so a database with
// exactly one non-local remote would pull from it with no override, no
// allow flag, and no refusal. This must require the same
// GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1 opt-in that a multi-remote override
// already demands (ga-nht26j — mirrors the equivalent sync-side fix,
// ga-2w96wd).
func TestPullSQLSoleNonLocalRemoteRequiresAllowFlag(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writePullFakeDoltSoleRemote(t, binDir, "https://public.example.invalid/repo")
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
		"GC_DOLT_PULL_ALLOW_REMOTE_APP=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc dolt pull should refuse a sole non-local remote without the allow flag:\n%s", out)
	}

	output := string(out)
	if !strings.Contains(output, "non-local remote") {
		t.Fatalf("expected a 'non-local remote' refusal:\n%s", output)
	}
	if !strings.Contains(output, "origin") {
		t.Fatalf("expected the refusal to name the sole remote 'origin':\n%s", output)
	}
	if !strings.Contains(output, "GC_DOLT_PULL_ALLOW_REMOTE") {
		t.Fatalf("expected the refusal to name the allow-flag escape hatch:\n%s", output)
	}

	log := readLog(t, doltLog)
	if strings.Contains(log, "DOLT_PULL") {
		t.Fatalf("a gated sole non-local remote must NOT be pulled:\nlog:\n%s", log)
	}
}

func TestPullSQLSoleNonLocalRemoteWithAllowFlagProceeds(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writePullFakeDoltSoleRemote(t, binDir, "https://public.example.invalid/repo")
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
		"GC_DOLT_PULL_ALLOW_REMOTE_APP=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a sole non-local remote with the allow flag set should succeed:\n%s", out)
	}

	log := readLog(t, doltLog)
	want := "CALL DOLT_PULL('origin', 'main')"
	if !strings.Contains(log, want) {
		t.Fatalf("expected pull from the sole 'origin' remote.\nwant %q\nlog:\n%s\noutput:\n%s", want, log, out)
	}
}

// A sole local (file://) remote is the common case (a freshly cloned or
// single-writer database) and must keep proceeding with zero ceremony —
// this is a regression guard for that default, not new behavior.
func TestPullSQLSoleFileRemoteProceedsWithoutOverride(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writePullFakeDoltSoleRemote(t, binDir, "file:///data/remotes/origin")
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
		t.Fatalf("a sole file:// remote should proceed without any override:\n%s", out)
	}

	log := readLog(t, doltLog)
	want := "CALL DOLT_PULL('origin', 'main')"
	if !strings.Contains(log, want) {
		t.Fatalf("expected pull from the sole 'origin' remote.\nwant %q\nlog:\n%s\noutput:\n%s", want, log, out)
	}
}
