package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// snapshotTargetName is the `dolt backup` target name the maintenance
	// loop registers. It is opaque to callers — do not rename without
	// planning migration for cities that have already registered it.
	snapshotTargetName = "snapshot-target"

	// snapshotTimestampFormat is RFC3339 with colons replaced by hyphens
	// so the directory segment is valid on all filesystems (Windows/NTFS
	// reject colons in path components). Must sort lexicographically to
	// match creation order — ISO-8601 does.
	snapshotTimestampFormat = "2006-01-02T15-04-05Z"

	// snapshotRetainSuccess is the count of successful snapshots kept
	// after each run; older ones are pruned. Matches ga-d5y design D3.
	snapshotRetainSuccess = 3

	// snapshotRetainFailed is the count of failed snapshots kept. The
	// design retains one so operators always have the most recent
	// failure available for postmortem without letting failed runs
	// accumulate on disk.
	snapshotRetainFailed = 1
)

// DoltBackupRunner wraps the `dolt backup` CLI subcommands that the
// maintenance loop needs. Production implementations shell out to the
// `dolt` binary running inside the managed Dolt database directory;
// tests supply fakes.
type DoltBackupRunner interface {
	// Add registers a named backup target at url. Implementations must
	// be idempotent: repeated calls with the same name + url on a
	// target that already exists return nil so the first maintenance
	// run after a supervisor restart is safe to retry.
	Add(ctx context.Context, name, url string) error

	// Sync pushes the current database state to the named backup
	// target. A non-nil return indicates the sync did not complete.
	Sync(ctx context.Context, name string) error
}

// DoltBackupRunnerFactory opens a DoltBackupRunner for one snapshot
// cycle. The per-cycle shape mirrors DoltOpsFactory so callers can
// apply deadlines and observability consistently across stages.
type DoltBackupRunnerFactory func(ctx context.Context) (DoltBackupRunner, error)

// NewExecDoltBackupRunner returns a factory whose runner shells out to
// the `dolt` binary in PATH with the process working directory set to
// dbDir (the managed Dolt database directory). The returned runner
// inherits the caller's context deadline; no extra timeouts are
// applied — the surrounding runSnapshot supplies them.
func NewExecDoltBackupRunner(dbDir string) DoltBackupRunnerFactory {
	return func(context.Context) (DoltBackupRunner, error) {
		return &execDoltBackupRunner{dbDir: dbDir}, nil
	}
}

// execDoltBackupRunner is the production DoltBackupRunner: it invokes
// `dolt backup …` via exec.CommandContext from dbDir.
type execDoltBackupRunner struct {
	dbDir string
}

func (r *execDoltBackupRunner) Add(ctx context.Context, name, url string) error {
	cmd := exec.CommandContext(ctx, "dolt", "backup", "add", name, url)
	cmd.Dir = r.dbDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// Idempotency: Dolt rejects a second `backup add` with the same
	// name or URL. Treating the duplicate-remote error as success lets
	// the first snapshot after a supervisor restart reuse the existing
	// target without the caller having to list + diff first.
	lower := strings.ToLower(string(out))
	if strings.Contains(lower, "already exists") ||
		strings.Contains(lower, "duplicate") ||
		strings.Contains(lower, "unique to existing") {
		return nil
	}
	return fmt.Errorf("dolt backup add %s %s: %w: %s", name, url, err, strings.TrimSpace(string(out)))
}

func (r *execDoltBackupRunner) Sync(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "dolt", "backup", "sync", name)
	cmd.Dir = r.dbDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("dolt backup sync %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runSnapshot performs one snapshot cycle: ensure the backup target,
// push via `dolt backup sync`, rotate current/ to success/<ts>/, and
// prune older entries per the retention policy.
//
// Returns the absolute path of the immutable snapshot directory on
// success. On backup failure it returns a *MaintenanceError{Stage:
// "backup"}; when current/ was populated at the point of failure it
// is moved to failed/<ts>/ and that path is returned alongside the
// error so the caller can surface it in the failed event.
//
// Retention errors (prune failures) are logged to stderr but do not
// regress a successful run — a failed prune must not roll back a
// successful snapshot+gc cycle.
//
// When openDoltBackup is nil the method is a no-op returning
// ("", nil). This matches runDoltGC's nil-factory convention and
// lets callers wire the dependencies incrementally.
func (m *StoreMaintenanceLoop) runSnapshot(ctx context.Context) (string, error) {
	if m.openDoltBackup == nil {
		return "", nil
	}

	backupsDir := filepath.Join(m.cityPath, ".beads", "dolt-backups")
	currentDir := filepath.Join(backupsDir, "current")
	successDir := filepath.Join(backupsDir, "success")
	failedDir := filepath.Join(backupsDir, "failed")

	for _, dir := range []string{backupsDir, successDir, failedDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", &MaintenanceError{Stage: "backup", Err: fmt.Errorf("ensure %s: %w", dir, err)}
		}
	}

	runner, err := m.openDoltBackup(ctx)
	if err != nil {
		return "", &MaintenanceError{Stage: "backup", Err: fmt.Errorf("open dolt backup runner: %w", err)}
	}

	addURL := "file://" + currentDir
	if err := runner.Add(ctx, snapshotTargetName, addURL); err != nil {
		failedPath := m.recordFailedSnapshot(currentDir, failedDir)
		m.pruneSnapshotsLog(failedDir, snapshotRetainFailed)
		return failedPath, &MaintenanceError{Stage: "backup", Err: fmt.Errorf("add target: %w", err)}
	}

	if err := runner.Sync(ctx, snapshotTargetName); err != nil {
		failedPath := m.recordFailedSnapshot(currentDir, failedDir)
		m.pruneSnapshotsLog(failedDir, snapshotRetainFailed)
		return failedPath, &MaintenanceError{Stage: "backup", Err: fmt.Errorf("sync target: %w", err)}
	}

	ts := m.clock().UTC().Format(snapshotTimestampFormat)
	successPath := filepath.Join(successDir, ts)
	if err := os.Rename(currentDir, successPath); err != nil {
		return "", &MaintenanceError{Stage: "backup", Err: fmt.Errorf("rotate current → %s: %w", successPath, err)}
	}

	m.pruneSnapshotsLog(successDir, snapshotRetainSuccess)
	m.pruneSnapshotsLog(failedDir, snapshotRetainFailed)

	return successPath, nil
}

// recordFailedSnapshot rotates current/ (if present) to
// failed/<ts>/. A missing current/ means the failure happened before
// any bytes were written, so the rotation is silently skipped.
// Rename errors are logged but do not shadow the outer MaintenanceError.
func (m *StoreMaintenanceLoop) recordFailedSnapshot(currentDir, failedDir string) string {
	if _, err := os.Stat(currentDir); os.IsNotExist(err) {
		return ""
	} else if err != nil {
		fmt.Fprintf(m.stderr, "store-maintenance: stat failed snapshot: %v\n", err) //nolint:errcheck // best-effort stderr
		return ""
	}
	ts := m.clock().UTC().Format(snapshotTimestampFormat)
	failedPath := filepath.Join(failedDir, ts)
	if err := os.Rename(currentDir, failedPath); err != nil {
		fmt.Fprintf(m.stderr, "store-maintenance: rotate failed snapshot: %v\n", err) //nolint:errcheck // best-effort stderr
		return ""
	}
	return failedPath
}

// pruneSnapshotsLog removes snapshot directories in dir beyond the
// newest keep. Entries are sorted lexicographically; because the
// snapshot timestamp format is ISO-8601 lexicographic order == time
// order. Non-directory entries are ignored so operator scratch files
// under success/ or failed/ survive. Errors (ReadDir or RemoveAll)
// are logged but never propagated.
func (m *StoreMaintenanceLoop) pruneSnapshotsLog(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(m.stderr, "store-maintenance: prune read %s: %v\n", dir, err) //nolint:errcheck // best-effort stderr
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return
	}
	sort.Strings(names)
	victims := names[:len(names)-keep]
	for _, v := range victims {
		path := filepath.Join(dir, v)
		if err := os.RemoveAll(path); err != nil {
			fmt.Fprintf(m.stderr, "store-maintenance: prune remove %s: %v\n", path, err) //nolint:errcheck // best-effort stderr
		}
	}
}
