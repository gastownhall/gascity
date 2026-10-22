package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// These tests exercise the journal-corruption auto-recovery helpers from
// examples/bd/assets/scripts/gc-beads-bd.sh (#3176) against a stub `dolt`
// binary. The contract under test:
//   - only databases whose offline probe confirms journal corruption are
//     touched;
//   - the corrupt store is preserved (moved aside, never deleted) before any
//     restore;
//   - restore only runs from a non-empty local file:// backup: bd's
//     "default" backup when it names the database, else the legacy
//     <db>-backup;
//   - every recovery line reaches both stderr and $LOG_FILE;
//   - fail-closed: when no usable backup exists or the restore fails, the
//     store ends up back in the data dir and the function fails so the server
//     cannot start silently missing a database.

func journalRecoveryHarness(t *testing.T) string {
	t.Helper()
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	script := string(scriptBytes)
	var harness strings.Builder
	harness.WriteString("#!/usr/bin/env bash\nset -u\n")
	for _, fn := range []string{
		"run_with_timeout",
		"journal_corruption_signature",
		"database_journal_corrupt",
		"repo_state_backups_object",
		"backup_remote_url_for_recovery",
		"backup_restore_source_usable",
		"backup_url_names_database",
		"select_recovery_backup",
		"recovery_notice",
		"attempt_journal_corruption_recovery",
	} {
		harness.WriteString(extractShellFunction(t, script, fn))
		harness.WriteString("\n")
	}
	harness.WriteString("attempt_journal_corruption_recovery\n")
	return harness.String()
}

// writeJournalFakeDolt stubs `dolt status` (corrupt when the database dir
// contains a .corrupt marker) and `dolt backup restore` (recreates the
// database with a restored-marker holding the restore URL, honoring
// FAKE_RESTORE_EXIT).
func writeJournalFakeDolt(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -u
case "${1:-}" in
  status)
    if [ -f "$PWD/.corrupt" ]; then
      echo "failed to load database: possible data loss detected in journal file at offset 961963: corrupted journal" >&2
      exit 1
    fi
    exit 0
    ;;
  backup)
    if [ "${2:-}" = "restore" ]; then
      if [ "${FAKE_RESTORE_EXIT:-0}" != "0" ]; then
        echo "restore failed" >&2
        exit "$FAKE_RESTORE_EXIT"
      fi
      db="$4"
      mkdir -p "$PWD/$db/.dolt"
      printf '%s\n' "$3" > "$PWD/$db/restored-marker"
      exit 0
    fi
    ;;
esac
exit 0
`)
}

type journalRecoveryEnv struct {
	cityDir   string
	dataDir   string
	stateDir  string
	logFile   string
	backupDir string
	binDir    string
}

func setupJournalRecoveryEnv(t *testing.T) journalRecoveryEnv {
	t.Helper()
	cityDir := t.TempDir()
	env := journalRecoveryEnv{
		cityDir:   cityDir,
		dataDir:   filepath.Join(cityDir, "dolt-data"),
		stateDir:  filepath.Join(cityDir, "pack-state"),
		logFile:   filepath.Join(cityDir, "dolt.log"),
		backupDir: filepath.Join(cityDir, "backups", "prod"),
		binDir:    filepath.Join(cityDir, "bin"),
	}
	for _, dir := range []string{env.dataDir, env.stateDir, env.backupDir, env.binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeJournalFakeDolt(t, env.binDir)
	return env
}

func (e journalRecoveryEnv) addDatabase(t *testing.T, name string, corrupt bool, repoState string) {
	t.Helper()
	dbDir := filepath.Join(e.dataDir, name)
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dbDir, err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, "original-marker"), []byte(name), 0o644); err != nil {
		t.Fatalf("write original marker: %v", err)
	}
	if corrupt {
		if err := os.WriteFile(filepath.Join(dbDir, ".corrupt"), []byte("1"), 0o644); err != nil {
			t.Fatalf("write corrupt marker: %v", err)
		}
	}
	if repoState != "" {
		if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "repo_state.json"), []byte(repoState), 0o644); err != nil {
			t.Fatalf("write repo_state.json: %v", err)
		}
	}
}

func (e journalRecoveryEnv) command(harness string, extraEnv ...string) *exec.Cmd {
	cmd := exec.Command("bash", "-c", harness)
	cmd.Env = append(os.Environ(),
		"PATH="+e.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DATA_DIR="+e.dataDir,
		"PACK_STATE_DIR="+e.stateDir,
		"LOG_FILE="+e.logFile,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd
}

func (e journalRecoveryEnv) run(t *testing.T, harness string, extraEnv ...string) (string, error) {
	t.Helper()
	cmd := e.command(harness, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestRunWithTimeoutCapturedOutputDoesNotWaitForWatchdogSleep(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	runWithTimeout := extractShellFunction(t, string(scriptBytes), "run_with_timeout")

	newPipe := func(name string) (*os.File, *os.File) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("create %s pipe: %v", name, err)
		}
		t.Cleanup(func() {
			_ = r.Close()
			_ = w.Close()
		})
		return r, w
	}
	signalR, signalW := newPipe("sleep-signal")
	releaseR, releaseW := newPipe("sleep-release")
	commandGateR, commandGateW := newPipe("command-gate")

	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "sleep"), `#!/bin/sh
set -u
[ "$#" -eq 1 ] && [ "$1" = "120" ] || exit 64
: "${SLEEP_SIGNAL_FD:?}"
: "${SLEEP_RELEASE_FD:?}"
: "${COMMAND_GATE_WRITE_FD:?}"
printf 'started\n' >&"$SLEEP_SIGNAL_FD"
printf 'go\n' >&"$COMMAND_GATE_WRITE_FD"
IFS= read -r release <&"$SLEEP_RELEASE_FD"
[ "$release" = "release" ] || exit 65
printf 'finished\n' >&"$SLEEP_SIGNAL_FD"
`)

	harness := "#!/usr/bin/env bash\nset -u\n" + runWithTimeout + `
result=$(run_with_timeout 120 sh -c 'IFS= read -r gate <&"$COMMAND_GATE_READ_FD"; [ "$gate" = go ]; printf command-output')
printf 'result=%s\n' "$result"
`
	env := journalRecoveryEnv{binDir: binDir}
	cmd := env.command(harness,
		"SLEEP_SIGNAL_FD=3",
		"SLEEP_RELEASE_FD=4",
		"COMMAND_GATE_WRITE_FD=5",
		"COMMAND_GATE_READ_FD=6",
	)
	cmd.ExtraFiles = []*os.File{signalW, releaseR, commandGateW, commandGateR}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start run_with_timeout harness: %v", err)
	}
	_ = signalW.Close()
	_ = releaseR.Close()
	_ = commandGateR.Close()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	controlsReleased := false
	releaseControls := func() {
		if controlsReleased {
			return
		}
		controlsReleased = true
		_, _ = fmt.Fprintln(commandGateW, "go")
		_ = commandGateW.Close()
		_, _ = fmt.Fprintln(releaseW, "release")
		_ = releaseW.Close()
	}
	signalReader := bufio.NewReader(signalR)
	readSignal := func(name, want string) error {
		if err := signalR.SetReadDeadline(time.Now().Add(testutil.ExecRaceTimeout)); err != nil {
			return fmt.Errorf("set %s deadline: %w", name, err)
		}
		var got string
		if _, err := fmt.Fscan(signalReader, &got); err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if got != want {
			return fmt.Errorf("%s = %q, want %q", name, got, want)
		}
		return nil
	}
	var runErr error
	doneConsumed := false
	waitCommand := func() bool {
		if doneConsumed {
			return true
		}
		timer := time.NewTimer(testutil.ExecRaceTimeout)
		defer timer.Stop()
		select {
		case runErr = <-done:
			doneConsumed = true
			return true
		case <-timer.C:
			return false
		}
	}
	defer func() {
		releaseControls()
		if waitCommand() {
			return
		}
		_ = cmd.Process.Kill()
		runErr = <-done
		doneConsumed = true
	}()

	if err := readSignal("watchdog sleep started signal", "started"); err != nil {
		t.Fatal(err)
	}

	completedBeforeRelease := waitCommand()
	releaseControls()
	if err := readSignal("watchdog sleep finished signal", "finished"); err != nil {
		t.Fatal(err)
	}
	blockedByWatchdogPipe := !completedBeforeRelease
	if blockedByWatchdogPipe {
		if !waitCommand() {
			t.Fatal("run_with_timeout harness did not finish after releasing the watchdog sleep")
		}
	}

	if runErr != nil {
		t.Errorf("run_with_timeout harness failed: %v\nstderr: %s", runErr, stderr.String())
	}
	if got, want := stdout.String(), "result=command-output\n"; got != want {
		t.Errorf("harness output = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "" {
		t.Errorf("harness stderr = %q, want empty", got)
	}
	if blockedByWatchdogPipe {
		t.Fatal("run_with_timeout command substitution stayed blocked after the watchdog sleep released the timed command because the sleep retained the substitution output pipe")
	}
}

func prodRepoState(backupDir string) string {
	return `{"head":"refs/heads/main","backups":{"prod-backup":{"name":"prod-backup","url":"file://` + backupDir + `","fetch_specs":[],"params":{}}}}`
}

// repoStateWithBackups renders a repo_state.json whose backups object holds
// one object-form entry per name -> url.
func repoStateWithBackups(backups map[string]string) string {
	names := make([]string, 0, len(backups))
	for name := range backups {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, fmt.Sprintf(`%q:{"name":%q,"url":%q,"fetch_specs":null,"params":{}}`, name, name, backups[name]))
	}
	return `{"head":"refs/heads/main","remotes":{},"backups":{` + strings.Join(entries, ",") + `},"branches":{}}`
}

// seedBackupDir creates a non-empty backup directory and returns its
// file:// URL.
func seedBackupDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir backup dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seed backup content: %v", err)
	}
	return "file://" + dir
}

func readRestoredFrom(t *testing.T, dbDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dbDir, "restored-marker"))
	if err != nil {
		t.Fatalf("database at %s was not restored: %v", dbDir, err)
	}
	return strings.TrimSpace(string(data))
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recovery log %s: %v", path, err)
	}
	return string(data)
}

// mol-dog-backup registers each scope through `bd backup init`, which names
// the backup "default" (#6751); recovery must find it.
func TestJournalRecoveryRestoresFromBdDefaultBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	url := seedBackupDir(t, env.backupDir)
	env.addDatabase(t, "prod", true, repoStateWithBackups(map[string]string{"default": url}))

	out, err := env.run(t, journalRecoveryHarness(t))
	if err != nil {
		t.Fatalf("recovery failed: %v\n%s", err, out)
	}
	if got := readRestoredFrom(t, filepath.Join(env.dataDir, "prod")); got != url {
		t.Fatalf("restored from %q, want %q\n%s", got, url, out)
	}
	asides, err := filepath.Glob(filepath.Join(env.stateDir, "corrupt-aside", "prod.*"))
	if err != nil || len(asides) != 1 {
		t.Fatalf("corrupt store must be preserved aside, got %v (err %v)\n%s", asides, err, out)
	}
	if _, err := os.Stat(filepath.Join(asides[0], "original-marker")); err != nil {
		t.Fatalf("aside copy missing original content: %v", err)
	}
	want := "RESTORED 'prod' from backup default (" + url + ")"
	if !strings.Contains(out, want) {
		t.Fatalf("stderr missing %q:\n%s", want, out)
	}
	if log := readLogFile(t, env.logFile); !strings.Contains(log, want) {
		t.Fatalf("LOG_FILE missing %q:\n%s", want, log)
	}
}

// snapshotTree records every path under root (relative) with its file
// content, so a test can prove a directory was left exactly as it was.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			snap[rel+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snap[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snap
}

func writeNomsManifest(t *testing.T, dbDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt", "noms"), 0o755); err != nil {
		t.Fatalf("mkdir noms: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "noms", "manifest"), []byte("manifest"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// The data dir is shared by every database the managed server hosts.
// Recovering one database must never move, restore or alter another — the
// incident in #1979 lost a 4.8 GB sibling to a whole-dir rebuild.
func TestJournalRecoveryLeavesSiblingDatabasesInPlace(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	tests := []struct {
		name        string
		prodBackup  bool
		wantRestore bool
	}{
		{name: "prod restored from default backup", prodBackup: true, wantRestore: true},
		{name: "prod fails closed without backup", prodBackup: false, wantRestore: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := setupJournalRecoveryEnv(t)
			prodState := `{"head":"refs/heads/main"}`
			if tc.prodBackup {
				prodState = repoStateWithBackups(map[string]string{"default": seedBackupDir(t, env.backupDir)})
			}
			env.addDatabase(t, "prod", true, prodState)
			writeNomsManifest(t, filepath.Join(env.dataDir, "prod"))

			env.addDatabase(t, "pa", false, `{"head":"refs/heads/main"}`)
			writeNomsManifest(t, filepath.Join(env.dataDir, "pa"))
			beadsURL := seedBackupDir(t, filepath.Join(env.cityDir, "backups", "beads"))
			env.addDatabase(t, "beads", false, repoStateWithBackups(map[string]string{"default": beadsURL}))
			writeNomsManifest(t, filepath.Join(env.dataDir, "beads"))
			// Phantom: a .dolt directory with no committed store.
			env.addDatabase(t, "ghost", false, "")

			siblings := []string{"pa", "beads", "ghost"}
			before := map[string]map[string]string{}
			for _, name := range siblings {
				before[name] = snapshotTree(t, filepath.Join(env.dataDir, name))
			}

			out, err := env.run(t, journalRecoveryHarness(t))
			if tc.wantRestore {
				if err != nil {
					t.Fatalf("recovery failed: %v\n%s", err, out)
				}
				if got := readRestoredFrom(t, filepath.Join(env.dataDir, "prod")); got != "file://"+env.backupDir {
					t.Fatalf("prod restored from %q, want file://%s\n%s", got, env.backupDir, out)
				}
			} else if err == nil {
				t.Fatalf("recovery must fail closed without a usable backup\n%s", out)
			}

			for _, name := range siblings {
				after := snapshotTree(t, filepath.Join(env.dataDir, name))
				if fmt.Sprint(after) != fmt.Sprint(before[name]) {
					t.Fatalf("sibling %s changed:\nbefore %v\nafter  %v\n%s", name, before[name], after, out)
				}
				if _, statErr := os.Stat(filepath.Join(env.dataDir, name, "restored-marker")); statErr == nil {
					t.Fatalf("sibling %s must not be restored\n%s", name, out)
				}
			}

			entries, readErr := os.ReadDir(filepath.Join(env.stateDir, "corrupt-aside"))
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatalf("read corrupt-aside: %v", readErr)
			}
			var aside []string
			for _, entry := range entries {
				aside = append(aside, entry.Name())
			}
			if tc.wantRestore {
				if len(aside) != 1 || !strings.HasPrefix(aside[0], "prod.") {
					t.Fatalf("corrupt-aside = %v, want only prod's store\n%s", aside, out)
				}
			} else {
				if len(aside) != 0 {
					t.Fatalf("corrupt-aside = %v, want empty after fail-closed\n%s", aside, out)
				}
				if _, statErr := os.Stat(filepath.Join(env.dataDir, "prod", "original-marker")); statErr != nil {
					t.Fatalf("prod must stay in place when not recoverable: %v", statErr)
				}
			}
		})
	}
}

func TestJournalRecoveryRestoresCorruptDBFromLocalBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seed backup content: %v", err)
	}
	env.addDatabase(t, "prod", true, prodRepoState(env.backupDir))
	env.addDatabase(t, "healthy", false, "")

	out, err := env.run(t, journalRecoveryHarness(t))
	if err != nil {
		t.Fatalf("recovery failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "RESTORED 'prod'") {
		t.Fatalf("output missing loud restore notice:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "prod", "restored-marker")); err != nil {
		t.Fatalf("prod was not restored from backup: %v\n%s", err, out)
	}
	asides, err := filepath.Glob(filepath.Join(env.stateDir, "corrupt-aside", "prod.*"))
	if err != nil || len(asides) != 1 {
		t.Fatalf("corrupt store must be preserved aside, got %v (err %v)\n%s", asides, err, out)
	}
	if _, err := os.Stat(filepath.Join(asides[0], "original-marker")); err != nil {
		t.Fatalf("aside copy missing original content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "healthy", "original-marker")); err != nil {
		t.Fatalf("healthy database must not be touched: %v", err)
	}
	if dirs, _ := filepath.Glob(filepath.Join(env.stateDir, "corrupt-aside", "healthy.*")); len(dirs) != 0 {
		t.Fatalf("healthy database must not be moved aside: %v", dirs)
	}
}

func TestJournalRecoveryFailsClosedWithoutUsableBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	tests := []struct {
		name      string
		repoState func(env journalRecoveryEnv) string
		seed      bool
	}{
		{
			name:      "no backup remote recorded",
			repoState: func(journalRecoveryEnv) string { return `{"head":"refs/heads/main"}` },
		},
		{
			name:      "backup remote dir empty",
			repoState: func(env journalRecoveryEnv) string { return prodRepoState(env.backupDir) },
		},
		{
			name: "non-file backup remote",
			repoState: func(journalRecoveryEnv) string {
				return `{"backups":{"prod-backup":{"url":"aws://bucket/prod"}}}`
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := setupJournalRecoveryEnv(t)
			env.addDatabase(t, "prod", true, tc.repoState(env))

			out, err := env.run(t, journalRecoveryHarness(t))
			if err == nil {
				t.Fatalf("recovery must fail closed without a usable backup\n%s", out)
			}
			if !strings.Contains(out, "NOT auto-recovering 'prod'") {
				t.Fatalf("output missing loud refusal:\n%s", out)
			}
			if _, statErr := os.Stat(filepath.Join(env.dataDir, "prod", "original-marker")); statErr != nil {
				t.Fatalf("prod store must stay in place when not recoverable: %v", statErr)
			}
		})
	}
}

func TestJournalRecoveryMovesStoreBackWhenRestoreFails(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	if err := os.WriteFile(filepath.Join(env.backupDir, "manifest"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seed backup content: %v", err)
	}
	env.addDatabase(t, "prod", true, prodRepoState(env.backupDir))

	out, err := env.run(t, journalRecoveryHarness(t), "FAKE_RESTORE_EXIT=1")
	if err == nil {
		t.Fatalf("recovery must fail when restore fails\n%s", out)
	}
	if !strings.Contains(out, "backup restore failed for 'prod'") {
		t.Fatalf("output missing restore failure notice:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(env.dataDir, "prod", "original-marker")); statErr != nil {
		t.Fatalf("prod store must be moved back after failed restore (fail closed): %v\n%s", statErr, out)
	}
}

func TestJournalRecoveryReportsWhenNoCorruptionConfirmed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	env.addDatabase(t, "healthy", false, "")

	out, err := env.run(t, journalRecoveryHarness(t))
	if err == nil {
		t.Fatalf("recovery must fail when nothing is corrupt (caller dies with original error)\n%s", out)
	}
	if !strings.Contains(out, "no corrupt database confirmed") {
		t.Fatalf("output missing probe-found-nothing notice:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(env.dataDir, "healthy", "original-marker")); statErr != nil {
		t.Fatalf("healthy database must not be touched: %v", statErr)
	}
}

// runBackupRemoteURLShapeTests exercises backup_remote_url_for_recovery
// against both repo_state.json backup shapes. maskJQ forces the sed fallback
// by hiding jq from `command -v`; otherwise the host jq path is exercised.
func runBackupRemoteURLShapeTests(t *testing.T, maskJQ bool) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	root := repoRootForLint(t)
	scriptBytes, err := os.ReadFile(filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	var fns strings.Builder
	for _, name := range []string{"repo_state_backups_object", "backup_remote_url_for_recovery"} {
		fns.WriteString(extractShellFunction(t, string(scriptBytes), name))
		fns.WriteString("\n")
	}

	tests := []struct {
		name       string
		backupName string
		repoState  string
		want       string
	}{
		{
			name:      "object form",
			repoState: `{"backups":{"prod-backup":{"name":"prod-backup","url":"file:///backups/prod","fetch_specs":[]}}}`,
			want:      "file:///backups/prod",
		},
		{
			name:      "legacy string form",
			repoState: `{"backups":{"prod-backup":"file:///backups/prod"}}`,
			want:      "file:///backups/prod",
		},
		{
			name:       "bd default object form",
			backupName: "default",
			repoState:  `{"head":"refs/heads/main","remotes":{},"backups":{"default":{"name":"default","url":"file:///city/.dolt-backup/prod","fetch_specs":null,"params":{}}},"branches":{}}`,
			want:       "file:///city/.dolt-backup/prod",
		},
		{
			name:       "bd default string form",
			backupName: "default",
			repoState:  `{"backups":{"default":"file:///city/.dolt-backup/prod"}}`,
			want:       "file:///city/.dolt-backup/prod",
		},
		{
			name:       "default only in remotes after backups",
			backupName: "default",
			repoState:  `{"backups":{"default":{"name":"default","url":"file:///city/.dolt-backup/prod","params":{}}},"remotes":{"default":{"name":"default","url":"file:///remotes/prod","params":{}}}}`,
			want:       "file:///city/.dolt-backup/prod",
		},
		{
			name:       "default only in remotes before backups",
			backupName: "default",
			repoState:  `{"remotes":{"default":{"name":"default","url":"file:///remotes/prod","params":{}}},"backups":{"default":{"name":"default","url":"file:///city/.dolt-backup/prod","params":{}}},"branches":{}}`,
			want:       "file:///city/.dolt-backup/prod",
		},
		{
			name:       "default registered only as a remote",
			backupName: "default",
			repoState:  `{"remotes":{"default":{"name":"default","url":"file:///remotes/prod","params":{}}},"backups":{"prod-backup":{"name":"prod-backup","url":"file:///backups/prod","params":{}}}}`,
			want:       "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "repo_state.json"), []byte(tc.repoState), 0o644); err != nil {
				t.Fatal(err)
			}
			harness := "#!/usr/bin/env bash\nset -u\n"
			if maskJQ {
				// The `command` override masks jq so the sed fallback is what
				// gets exercised regardless of the host toolchain.
				harness += "command() { case \"$*\" in '-v jq') return 1 ;; esac; builtin command \"$@\"; }\n"
			}
			harness += fns.String() + "\nbackup_remote_url_for_recovery prod \"$1\" $2\n"
			out, err := exec.Command("bash", "-c", harness, "harness", dbDir, tc.backupName).CombinedOutput()
			if tc.want == "" {
				if err == nil {
					t.Fatalf("backup_remote_url_for_recovery = %q, want not found", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("backup_remote_url_for_recovery failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("url = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBackupRemoteURLForRecoveryParsesBothShapesWithoutJQ(t *testing.T) {
	runBackupRemoteURLShapeTests(t, true)
}

// The jq path must handle the legacy string form too: `.url` on a string
// errors in jq, so the expression uses `.url?` — this test pins that (#3176
// review finding).
func TestBackupRemoteURLForRecoveryParsesBothShapesWithJQ(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; skipping jq-path test")
	}
	runBackupRemoteURLShapeTests(t, false)
}
