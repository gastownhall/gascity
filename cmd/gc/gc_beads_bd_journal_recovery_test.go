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
//   - restore only runs from a non-empty local file:// <db>-backup remote;
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
// database with a restored-marker holding the restored-from URL, honoring
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
      url="$3"
      db="$4"
      mkdir -p "$PWD/$db/.dolt"
      printf '%s\n' "$url" > "$PWD/$db/restored-marker"
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

// repoStateWithBackups renders a repo_state.json whose "backups" object maps
// each backup name to a file:// URL, in the object form dolt writes.
func repoStateWithBackups(backups map[string]string) string {
	names := make([]string, 0, len(backups))
	for name := range backups {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(`{"head":"refs/heads/main","backups":{`)
	for i, name := range names {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q:"+`{"name":%q,"url":%q,"fetch_specs":[],"params":{}}`, name, name, backups[name])
	}
	b.WriteString(`}}`)
	return b.String()
}

func prodRepoState(backupDir string) string {
	return repoStateWithBackups(map[string]string{"prod-backup": "file://" + backupDir})
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

// seedBackupDir makes a backup source directory non-empty so it counts as a
// usable file:// restore source.
func seedBackupDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seed backup content: %v", err)
	}
}

// TestJournalRecoveryRestoresFromBdDefaultBackup pins AC-1: mol-dog-backup
// registers each scope's backup destination as bd's "default" remote, so a
// database whose repo_state.json carries only that name must still
// auto-recover, and the restore notice must reach the managed log — stderr is
// discarded by the provider runner when the start script exits 0.
func TestJournalRecoveryRestoresFromBdDefaultBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	env := setupJournalRecoveryEnv(t)
	seedBackupDir(t, env.backupDir)
	env.addDatabase(t, "prod", true, repoStateWithBackups(map[string]string{
		"default": "file://" + env.backupDir,
	}))

	out, err := env.run(t, journalRecoveryHarness(t))
	if err != nil {
		t.Fatalf("recovery failed: %v\n%s", err, out)
	}
	wantNotice := "RESTORED 'prod' from backup default (file://" + env.backupDir + ")"
	if !strings.Contains(out, wantNotice) {
		t.Fatalf("stderr missing restore notice naming the default backup:\n%s", out)
	}
	logBytes, err := os.ReadFile(env.logFile)
	if err != nil {
		t.Fatalf("read managed log: %v", err)
	}
	if !strings.Contains(string(logBytes), wantNotice) {
		t.Fatalf("managed log missing restore notice:\n%s", logBytes)
	}
	marker := filepath.Join(env.dataDir, "prod", "restored-marker")
	markerBytes, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("prod was not restored from the default backup: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(markerBytes)); got != "file://"+env.backupDir {
		t.Fatalf("restored from %q, want the default backup URL %q", got, "file://"+env.backupDir)
	}
	asides, err := filepath.Glob(filepath.Join(env.stateDir, "corrupt-aside", "prod.*"))
	if err != nil || len(asides) != 1 {
		t.Fatalf("corrupt store must be preserved aside once, got %v (err %v)\n%s", asides, err, out)
	}
	if _, err := os.Stat(filepath.Join(asides[0], "original-marker")); err != nil {
		t.Fatalf("aside copy missing original content: %v", err)
	}
}

// TestJournalRecoveryLeavesSiblingDatabasesInPlace pins AC-2: the managed
// data dir is shared by every database in the city (#1979), so recovery of
// one corrupt database must neither move, restore, nor touch any sibling —
// including a phantom directory (a .dolt without a committed noms store).
func TestJournalRecoveryLeavesSiblingDatabasesInPlace(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	tests := []struct {
		name      string
		repoState func(env journalRecoveryEnv) string
		seedProd  bool
		wantError bool
	}{
		{
			name: "restores from default backup",
			repoState: func(env journalRecoveryEnv) string {
				return repoStateWithBackups(map[string]string{"default": "file://" + env.backupDir})
			},
			seedProd:  true,
			wantError: false,
		},
		{
			name: "fails closed without usable backup",
			repoState: func(journalRecoveryEnv) string {
				return `{"head":"refs/heads/main"}`
			},
			seedProd:  false,
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := setupJournalRecoveryEnv(t)
			if tc.seedProd {
				seedBackupDir(t, env.backupDir)
			}
			env.addDatabase(t, "prod", true, tc.repoState(env))
			// Siblings: pa has no backup registered, beads has a usable
			// backup, ghost is a phantom (.dolt without a noms store).
			env.addDatabase(t, "pa", false, "")
			beadsBackupDir := filepath.Join(filepath.Dir(env.backupDir), "beads")
			seedBackupDir(t, beadsBackupDir)
			env.addDatabase(t, "beads", false, repoStateWithBackups(map[string]string{"default": "file://" + beadsBackupDir}))
			env.addDatabase(t, "ghost", false, "")
			if err := os.Remove(filepath.Join(env.dataDir, "ghost", ".dolt", "repo_state.json")); err != nil && !os.IsNotExist(err) {
				t.Fatalf("remove ghost repo_state: %v", err)
			}

			out, err := env.run(t, journalRecoveryHarness(t))
			if (err == nil) == tc.wantError {
				t.Fatalf("recovery exit error = %v, wantError %v\n%s", err, tc.wantError, out)
			}
			for _, sibling := range []string{"pa", "beads", "ghost"} {
				markerBytes, statErr := os.ReadFile(filepath.Join(env.dataDir, sibling, "original-marker"))
				if statErr != nil {
					t.Fatalf("sibling %q must stay at its original path: %v\n%s", sibling, statErr, out)
				}
				if string(markerBytes) != sibling {
					t.Fatalf("sibling %q contents changed: %q", sibling, markerBytes)
				}
				if _, statErr := os.Stat(filepath.Join(env.dataDir, sibling, "restored-marker")); statErr == nil {
					t.Fatalf("sibling %q must not be restored\n%s", sibling, out)
				}
			}
			asides, globErr := filepath.Glob(filepath.Join(env.stateDir, "corrupt-aside", "*"))
			if globErr != nil {
				t.Fatalf("glob corrupt-aside: %v", globErr)
			}
			if tc.wantError {
				if len(asides) != 0 {
					t.Fatalf("corrupt-aside must hold nothing when recovery fails closed, got %v\n%s", asides, out)
				}
			} else {
				if len(asides) != 1 || !strings.HasPrefix(filepath.Base(asides[0]), "prod.") {
					t.Fatalf("corrupt-aside must hold only prod's store, got %v\n%s", asides, out)
				}
			}
		})
	}
}

// TestJournalRecoveryPrefersBdDefaultBackup pins AC-3: bd's "default"
// destination is the one mol-dog-backup keeps current, so when both it and
// the legacy <db>-backup name are usable recovery must restore from
// "default" and say so. A "default" that is not a usable local file://
// source naming the database falls back to the legacy name.
func TestJournalRecoveryPrefersBdDefaultBackup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}
	defaultDir := filepath.Join(t.TempDir(), "prod")
	seedBackupDir(t, defaultDir)

	t.Run("restores from default when both are usable", func(t *testing.T) {
		env := setupJournalRecoveryEnv(t)
		legacyDir := filepath.Join(filepath.Dir(env.backupDir), "legacy-prod")
		seedBackupDir(t, legacyDir)
		env.addDatabase(t, "prod", true, repoStateWithBackups(map[string]string{
			"default":     "file://" + defaultDir,
			"prod-backup": "file://" + legacyDir,
		}))

		out, err := env.run(t, journalRecoveryHarness(t))
		if err != nil {
			t.Fatalf("recovery failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "RESTORED 'prod' from backup default (file://"+defaultDir+")") {
			t.Fatalf("output must name the default backup and its URL:\n%s", out)
		}
		markerBytes, err := os.ReadFile(filepath.Join(env.dataDir, "prod", "restored-marker"))
		if err != nil {
			t.Fatalf("prod was not restored: %v\n%s", err, out)
		}
		if got := strings.TrimSpace(string(markerBytes)); got != "file://"+defaultDir {
			t.Fatalf("restored from %q, want the default backup URL %q", got, "file://"+defaultDir)
		}
	})

	t.Run("falls back to legacy name when default is not a local file source", func(t *testing.T) {
		env := setupJournalRecoveryEnv(t)
		legacyDir := env.backupDir
		seedBackupDir(t, legacyDir)
		env.addDatabase(t, "prod", true, repoStateWithBackups(map[string]string{
			"default":     "aws://bucket/prod",
			"prod-backup": "file://" + legacyDir,
		}))

		out, err := env.run(t, journalRecoveryHarness(t))
		if err != nil {
			t.Fatalf("recovery failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "RESTORED 'prod' from backup prod-backup (file://"+legacyDir+")") {
			t.Fatalf("output must name the legacy backup and its URL:\n%s", out)
		}
		markerBytes, err := os.ReadFile(filepath.Join(env.dataDir, "prod", "restored-marker"))
		if err != nil {
			t.Fatalf("prod was not restored: %v\n%s", err, out)
		}
		if got := strings.TrimSpace(string(markerBytes)); got != "file://"+legacyDir {
			t.Fatalf("restored from %q, want the legacy backup URL %q", got, "file://"+legacyDir)
		}
	})
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
	fn := extractShellFunction(t, string(scriptBytes), "backup_remote_url_for_recovery")

	tests := []struct {
		name      string
		repoState string
		want      string
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
			harness += fn + "\nbackup_remote_url_for_recovery prod \"$1\"\n"
			out, err := exec.Command("bash", "-c", harness, "harness", dbDir).CombinedOutput()
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
