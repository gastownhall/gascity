package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// fakeDoltBackupRunner is a scripted DoltBackupRunner for runSnapshot
// tests. Call counters verify invocation ordering; addErr / syncErr
// drive failure-path coverage. When writeOnSync is true, a successful
// Sync creates the file:// directory implied by the most recent Add
// call so the surrounding rename-to-success path has something to
// move.
type fakeDoltBackupRunner struct {
	addErr      error
	syncErr     error
	writeOnSync bool

	addCalls   int
	syncCalls  int
	addedName  string
	addedURL   string
	syncedName string
	calls      *[]string
}

func tempDirWithRetryCleanup(t *testing.T, pattern string) string {
	t.Helper()

	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		t.Fatalf("MkdirTemp(%q): %v", pattern, err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := os.RemoveAll(dir)
			if err == nil || os.IsNotExist(err) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("cleanup temp dir %s: %v", dir, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return dir
}

func (f *fakeDoltBackupRunner) Add(_ context.Context, name, url string) error {
	f.addCalls++
	if f.calls != nil {
		*f.calls = append(*f.calls, "backup.add")
	}
	f.addedName = name
	f.addedURL = url
	return f.addErr
}

func (f *fakeDoltBackupRunner) Sync(_ context.Context, name string) error {
	f.syncCalls++
	if f.calls != nil {
		*f.calls = append(*f.calls, "backup.sync")
	}
	f.syncedName = name
	if f.syncErr != nil {
		return f.syncErr
	}
	if f.writeOnSync && strings.HasPrefix(f.addedURL, "file://") {
		target := strings.TrimPrefix(f.addedURL, "file://")
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("fake sync: writeOnSync mkdir: %w", err)
		}
		if err := os.WriteFile(filepath.Join(target, "manifest"), []byte("fake-backup"), 0o644); err != nil {
			return fmt.Errorf("fake sync: writeOnSync writefile: %w", err)
		}
	}
	return nil
}

// newSnapshotTestLoop builds a loop with a deterministic clock so the
// timestamp-format assertions are stable. Caller injects the fake.
func newSnapshotTestLoop(t *testing.T, runner *fakeDoltBackupRunner, clockNow time.Time) *StoreMaintenanceLoop {
	t.Helper()
	var stderr bytes.Buffer
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true},
		CityPath: t.TempDir(),
		Clock:    func() time.Time { return clockNow },
		Stderr:   &stderr,
		OpenDoltBackup: func(context.Context) (DoltBackupRunner, error) {
			if runner == nil {
				return nil, errors.New("no fake runner configured")
			}
			return runner, nil
		},
	})
	return loop
}

func TestRunSnapshot_NilFactory_NoOp(t *testing.T) {
	t.Parallel()
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true},
		CityPath: t.TempDir(),
	})
	path, err := loop.runSnapshot(context.Background())
	if err != nil {
		t.Fatalf("runSnapshot with nil factory = %v; want nil", err)
	}
	if path != "" {
		t.Errorf("path = %q; want empty with nil factory", path)
	}
}

func TestRunSnapshot_HappyPath_RotatesToSuccessWithExpectedTimestamp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 23, 12, 30, 45, 0, time.UTC)
	runner := &fakeDoltBackupRunner{writeOnSync: true}
	loop := newSnapshotTestLoop(t, runner, now)

	path, err := loop.runSnapshot(context.Background())
	if err != nil {
		t.Fatalf("runSnapshot = %v; want nil", err)
	}

	if runner.addCalls != 1 || runner.syncCalls != 1 {
		t.Errorf("add=%d sync=%d; want 1 each", runner.addCalls, runner.syncCalls)
	}
	wantName := "snapshot-target"
	if runner.addedName != wantName || runner.syncedName != wantName {
		t.Errorf("addedName=%q syncedName=%q; want %q", runner.addedName, runner.syncedName, wantName)
	}
	wantURL := "file://" + filepath.Join(loop.cityPath, ".beads", "dolt-backups", "current")
	if runner.addedURL != wantURL {
		t.Errorf("addedURL=%q; want %q", runner.addedURL, wantURL)
	}

	wantTS := "2026-04-23T12-30-45Z"
	wantPath := filepath.Join(loop.cityPath, ".beads", "dolt-backups", "success", wantTS)
	if path != wantPath {
		t.Errorf("path = %q; want %q", path, wantPath)
	}
	// Timestamp must not contain colons (filesystem-safe invariant).
	if strings.Contains(filepath.Base(path), ":") {
		t.Errorf("snapshot dir name %q contains colon; must be filesystem-safe", path)
	}
	// Success dir exists with the fake's manifest.
	if _, err := os.Stat(filepath.Join(path, "manifest")); err != nil {
		t.Errorf("success manifest not moved into place: %v", err)
	}
	// current/ is gone after rotation.
	if _, err := os.Stat(filepath.Join(loop.cityPath, ".beads", "dolt-backups", "current")); !os.IsNotExist(err) {
		t.Errorf("current/ still present after successful snapshot: err=%v", err)
	}
}

func TestRunSnapshot_AddFailure_ReturnsStageBackupAndLeavesNoSuccessDir(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 23, 12, 30, 45, 0, time.UTC)
	runner := &fakeDoltBackupRunner{addErr: errors.New("target URL duplicate with remote X")}
	loop := newSnapshotTestLoop(t, runner, now)

	path, err := loop.runSnapshot(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v; want *MaintenanceError", err)
	}
	if me.Stage != "backup" {
		t.Errorf("stage = %q; want %q", me.Stage, "backup")
	}
	if runner.syncCalls != 0 {
		t.Errorf("Sync called %d times after Add failed; want 0", runner.syncCalls)
	}
	// Add failed before Sync created current/, so nothing to rotate.
	// Path must be empty, failed/ must be empty.
	if path != "" {
		t.Errorf("path = %q; want empty when nothing was written", path)
	}
	failedEntries, _ := os.ReadDir(filepath.Join(loop.cityPath, ".beads", "dolt-backups", "failed"))
	if len(failedEntries) != 0 {
		t.Errorf("failed/ contains %d entries; want 0 (current/ never existed)", len(failedEntries))
	}
}

func TestRunSnapshot_SyncFailure_RotatesCurrentToFailedAndReturnsStageBackup(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 23, 12, 30, 45, 0, time.UTC)
	runner := &fakeDoltBackupRunner{syncErr: errors.New("network unreachable")}
	loop := newSnapshotTestLoop(t, runner, now)

	// Pre-populate current/ to simulate a partial sync write.
	currentDir := filepath.Join(loop.cityPath, ".beads", "dolt-backups", "current")
	if err := os.MkdirAll(currentDir, 0o755); err != nil {
		t.Fatalf("seed current: %v", err)
	}
	if err := os.WriteFile(filepath.Join(currentDir, "partial"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}

	path, err := loop.runSnapshot(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v; want *MaintenanceError", err)
	}
	if me.Stage != "backup" {
		t.Errorf("stage = %q; want %q", me.Stage, "backup")
	}

	wantTS := "2026-04-23T12-30-45Z"
	wantPath := filepath.Join(loop.cityPath, ".beads", "dolt-backups", "failed", wantTS)
	if path != wantPath {
		t.Errorf("path = %q; want %q", path, wantPath)
	}
	if _, err := os.Stat(filepath.Join(wantPath, "partial")); err != nil {
		t.Errorf("partial content not moved to failed: %v", err)
	}
	// current/ is gone.
	if _, err := os.Stat(currentDir); !os.IsNotExist(err) {
		t.Errorf("current/ still present after failed sync: err=%v", err)
	}
}

func TestRunSnapshot_SuccessRetention_KeepsNewest3(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 23, 12, 30, 45, 0, time.UTC)
	runner := &fakeDoltBackupRunner{writeOnSync: true}
	loop := newSnapshotTestLoop(t, runner, now)

	// Seed 5 older successful snapshots. Names sort ascending by date.
	successDir := filepath.Join(loop.cityPath, ".beads", "dolt-backups", "success")
	if err := os.MkdirAll(successDir, 0o755); err != nil {
		t.Fatalf("mkdir success: %v", err)
	}
	oldNames := []string{
		"2026-04-01T00-00-00Z",
		"2026-04-05T00-00-00Z",
		"2026-04-10T00-00-00Z",
		"2026-04-15T00-00-00Z",
		"2026-04-20T00-00-00Z",
	}
	for _, n := range oldNames {
		if err := os.MkdirAll(filepath.Join(successDir, n), 0o755); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	if _, err := loop.runSnapshot(context.Background()); err != nil {
		t.Fatalf("runSnapshot = %v; want nil", err)
	}

	entries, err := os.ReadDir(successDir)
	if err != nil {
		t.Fatalf("readdir success: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	want := []string{
		"2026-04-15T00-00-00Z",
		"2026-04-20T00-00-00Z",
		"2026-04-23T12-30-45Z",
	}
	if len(names) != len(want) {
		t.Fatalf("success count = %d (%v); want %d (%v)", len(names), names, len(want), want)
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("success[%d] = %q; want %q (full: %v)", i, names[i], w, names)
		}
	}
}

func TestRunSnapshot_FailedRetention_KeepsNewest1(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 23, 12, 30, 45, 0, time.UTC)
	runner := &fakeDoltBackupRunner{syncErr: errors.New("network error")}
	loop := newSnapshotTestLoop(t, runner, now)

	// Pre-populate current/ so the failed rotation has content to move.
	currentDir := filepath.Join(loop.cityPath, ".beads", "dolt-backups", "current")
	if err := os.MkdirAll(currentDir, 0o755); err != nil {
		t.Fatalf("seed current: %v", err)
	}

	// Seed two older failed snapshots.
	failedDir := filepath.Join(loop.cityPath, ".beads", "dolt-backups", "failed")
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	for _, n := range []string{"2026-04-01T00-00-00Z", "2026-04-10T00-00-00Z"} {
		if err := os.MkdirAll(filepath.Join(failedDir, n), 0o755); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	if _, err := loop.runSnapshot(context.Background()); err == nil {
		t.Fatalf("runSnapshot = nil; want failure")
	}

	entries, err := os.ReadDir(failedDir)
	if err != nil {
		t.Fatalf("readdir failed: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	want := []string{"2026-04-23T12-30-45Z"}
	if len(names) != len(want) {
		t.Fatalf("failed count = %d (%v); want %d (%v)", len(names), names, len(want), want)
	}
	if names[0] != want[0] {
		t.Errorf("failed[0] = %q; want %q", names[0], want[0])
	}
}

func TestRunSnapshot_PruneReadError_LoggedNotPropagated(t *testing.T) {
	t.Parallel()
	// Make successDir exist as a FILE instead of a directory so
	// ReadDir fails during prune. The run should still succeed —
	// prune errors do not propagate.
	now := time.Date(2026, 4, 23, 12, 30, 45, 0, time.UTC)

	var stderr bytes.Buffer
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true},
		CityPath: t.TempDir(),
		Clock:    func() time.Time { return now },
		Stderr:   &stderr,
		OpenDoltBackup: func(context.Context) (DoltBackupRunner, error) {
			return &fakeDoltBackupRunner{writeOnSync: true}, nil
		},
	})

	// Pre-create dolt-backups/ and plant a FILE at success/ so the
	// initial MkdirAll succeeds for backupsDir but fails for successDir.
	// To avoid that, we let runSnapshot create success/ normally, then
	// poison the failed/ directory post-rotation — but since the
	// happy-path only prunes failed/ after the rotation already
	// succeeded, the prune error on failed/ will be exercised.
	backupsDir := filepath.Join(loop.cityPath, ".beads", "dolt-backups")
	if err := os.MkdirAll(backupsDir, 0o755); err != nil {
		t.Fatalf("mkdir backups: %v", err)
	}
	failedFile := filepath.Join(backupsDir, "failed")
	// Plant failed/ as a file so ReadDir errors during prune.
	// runSnapshot's MkdirAll on failed/ will itself fail — so we need
	// a different approach. Instead, we test the prune helper directly.
	if err := os.WriteFile(failedFile, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("plant file at failed/: %v", err)
	}

	// Direct test of pruneSnapshotsLog against a non-directory path.
	loop.pruneSnapshotsLog(failedFile, 1)
	if got := stderr.String(); !strings.Contains(got, "prune read") {
		t.Errorf("stderr did not log prune read error; got %q", got)
	}
}

func TestRunSnapshot_MkdirFailure_ReturnsStageBackup(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 23, 12, 30, 45, 0, time.UTC)
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true},
		CityPath: t.TempDir(),
		Clock:    func() time.Time { return now },
		OpenDoltBackup: func(context.Context) (DoltBackupRunner, error) {
			return &fakeDoltBackupRunner{writeOnSync: true}, nil
		},
	})

	// Plant .beads/ as a file so MkdirAll on dolt-backups/ fails.
	beadsPath := filepath.Join(loop.cityPath, ".beads")
	if err := os.WriteFile(beadsPath, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("plant: %v", err)
	}

	_, err := loop.runSnapshot(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v; want *MaintenanceError", err)
	}
	if me.Stage != "backup" {
		t.Errorf("stage = %q; want %q", me.Stage, "backup")
	}
}

func TestRunSnapshot_OpenRunnerError_ReturnsStageBackup(t *testing.T) {
	t.Parallel()
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      config.DoltMaintenance{Enabled: true},
		CityPath: t.TempDir(),
		OpenDoltBackup: func(context.Context) (DoltBackupRunner, error) {
			return nil, errors.New("binary not found")
		},
	})

	_, err := loop.runSnapshot(context.Background())
	var me *MaintenanceError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v; want *MaintenanceError", err)
	}
	if me.Stage != "backup" {
		t.Errorf("stage = %q; want %q", me.Stage, "backup")
	}
}

// TestRunSnapshot_Integration_RealDoltRoundTrip exercises the exec-based
// runner against a throwaway Dolt database, then restores the snapshot
// to a second throwaway directory to confirm the backup is usable.
// Skipped when `dolt` is not on PATH (ga-d5y design D3 "known risk"
// callout).
func TestRunSnapshot_Integration_RealDoltRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode; skipping dolt round-trip integration test")
	}
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed; skipping")
	}

	cityPath := tempDirWithRetryCleanup(t, "gc-snapshot-city-*")
	dbDir := filepath.Join(cityPath, ".beads", "dolt")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir dbDir: %v", err)
	}

	// Isolate dolt's global config under a temp DOLT_ROOT_PATH so the
	// test does not depend on or pollute the developer's identity.
	doltRoot := tempDirWithRetryCleanup(t, "gc-snapshot-dolt-root-*")
	if err := os.MkdirAll(filepath.Join(doltRoot, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir dolt root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(doltRoot, ".dolt", "config_global.json"),
		[]byte(`{"user.name":"gc-test","user.email":"gc-test@test.local"}`), 0o644); err != nil {
		t.Fatalf("seed dolt identity: %v", err)
	}
	// t.Setenv so exec.CommandContext inside runSnapshot inherits the
	// isolated root without needing a separate env-plumbing API.
	t.Setenv("DOLT_ROOT_PATH", doltRoot)
	doltEnv := os.Environ()

	// Build a minimal dolt DB with one committed row so the restore
	// round-trip has content to verify.
	initCtx, cancelInit := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelInit()
	runDolt := func(ctx context.Context, dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, doltPath, args...)
		cmd.Dir = dir
		cmd.Env = doltEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("dolt %s (dir=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
		}
	}
	runDolt(initCtx, dbDir, "init")
	runDolt(initCtx, dbDir, "sql", "-q", "CREATE TABLE t (k INT PRIMARY KEY, v TEXT);")
	runDolt(initCtx, dbDir, "sql", "-q", "INSERT INTO t VALUES (1, 'hello');")
	runDolt(initCtx, dbDir, "add", ".")
	runDolt(initCtx, dbDir, "commit", "-m", "initial")

	// Invoke runSnapshot against the real dbDir.
	now := time.Date(2026, 4, 23, 12, 30, 45, 0, time.UTC)
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:            config.DoltMaintenance{Enabled: true},
		CityPath:       cityPath,
		Clock:          func() time.Time { return now },
		OpenDoltBackup: NewExecDoltBackupRunner(dbDir),
	})
	runCtx, cancelRun := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelRun()
	snapshotPath, err := loop.runSnapshot(runCtx)
	if err != nil {
		t.Fatalf("runSnapshot = %v; want nil", err)
	}
	if snapshotPath == "" {
		t.Fatalf("snapshotPath empty")
	}

	// Restore the snapshot to a second throwaway directory and verify
	// the table survived the round trip.
	restoreRoot := tempDirWithRetryCleanup(t, "gc-snapshot-restore-*")
	restoredDB := filepath.Join(restoreRoot, "restored")
	restoreCtx, cancelRestore := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelRestore()
	restoreCmd := exec.CommandContext(restoreCtx, doltPath, "backup", "restore", "file://"+snapshotPath, "restored")
	restoreCmd.Dir = restoreRoot
	restoreCmd.Env = doltEnv
	if out, err := restoreCmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt backup restore: %v\n%s", err, out)
	}

	// Read the row back.
	readCtx, cancelRead := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelRead()
	readCmd := exec.CommandContext(readCtx, doltPath, "sql", "-r", "csv", "-q", "SELECT v FROM t WHERE k = 1;")
	readCmd.Dir = restoredDB
	readCmd.Env = doltEnv
	out, err := readCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt sql after restore: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("restored db did not contain row 'hello'; got %q", string(out))
	}
}
