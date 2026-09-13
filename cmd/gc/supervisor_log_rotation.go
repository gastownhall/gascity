package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Supervisor log rotation tunables. Nothing bounded ~/.gc/supervisor.log:
// a supervisor that fails the same way on every start crash-loops under
// its service manager (systemd Restart=always, launchd KeepAlive) and
// appends identical failure lines through every restart — 645MB in one
// two-day bind-conflict incident (gastownhall/gascity#3897). Rotation is
// size-gated at supervisor startup, before the new instance writes
// anything, which bounds exactly that shape: every relaunch re-runs the
// check, so the file never exceeds the cap by more than one instance's
// output. Vars (not consts) so tests can lower the thresholds.
var (
	// supervisorLogMaxBytes is the size at or above which a supervisor
	// start archives the log before appending. Non-positive disables
	// rotation.
	supervisorLogMaxBytes int64 = 64 * 1024 * 1024 // 64 MiB

	// supervisorLogKeepArchives is how many compressed archives to keep
	// next to the active log; the oldest are pruned on rotation.
	supervisorLogKeepArchives = 3

	// supervisorLogArchiveMaxBytes bounds how much of an oversized log a
	// single rotation reads into an archive. A pathological log (far past
	// the rotation cap because rotation was disabled or failing) is
	// archived only up to this bound; the tail beyond it is dropped with a
	// warning and the active file is still truncated, because bounding the
	// log is the point of rotating at all.
	supervisorLogArchiveMaxBytes int64 = 1 << 30 // 1 GiB
)

// supervisorLogArchiveTimestampLayout is the compact UTC-pinned timestamp
// embedded in archive filenames (same layout as the events.jsonl archive
// convention) so directory listings sort chronologically.
const supervisorLogArchiveTimestampLayout = "20060102T150405Z"

// acquireSupervisorLockAndRotateLog acquires the supervisor single-instance
// lock and, while holding it, size-gates the supervisor log. Rotation must
// only ever run under this lock: two supervisor processes rotating the same
// log concurrently can interleave the compress/truncate sequence and
// destroy log content, so the losing process has to give up before touching
// the log. Rotation failures are warned to warn and never block startup — a
// supervisor with an oversized log beats no supervisor. Only lock
// acquisition failure is returned; the caller owns closing the lock.
func acquireSupervisorLockAndRotateLog(logPath string, now time.Time, warn io.Writer) (*os.File, error) {
	lock, err := acquireSupervisorLock()
	if err != nil {
		return nil, err
	}
	if err := maybeRotateSupervisorLog(logPath, now, warn); err != nil {
		fmt.Fprintf(warn, "gc supervisor: rotating supervisor log: %v\n", err) //nolint:errcheck // best-effort warning
	}
	return lock, nil
}

// maybeRotateSupervisorLog archives the supervisor log when it has reached
// supervisorLogMaxBytes, prunes archives beyond supervisorLogKeepArchives,
// and sweeps staging leftovers from crashed rotations. Non-fatal notices
// (an over-bound tail dropped from the archive) go to warn. A missing log
// is a no-op; all other failures return an error with context so the
// caller can surface them.
//
// Callers must hold the supervisor single-instance lock (see
// acquireSupervisorLockAndRotateLog): the compress/truncate sequence is
// not safe against a concurrent rotation of the same file.
func maybeRotateSupervisorLog(path string, now time.Time, warn io.Writer) error {
	if supervisorLogMaxBytes <= 0 {
		return nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking supervisor log %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("supervisor log path %s is a directory", path)
	}
	if info.Size() < supervisorLogMaxBytes {
		return nil
	}
	if err := rotateSupervisorLog(path, now, warn); err != nil {
		return err
	}
	if err := pruneSupervisorLogArchives(path, supervisorLogKeepArchives); err != nil {
		return err
	}
	return sweepSupervisorLogStagingFiles(path)
}

// rotateSupervisorLog compresses the current log contents into a
// timestamped .gz archive next to the active file and truncates the active
// file in place. Copy-then-truncate (not rename) is deliberate: service
// managers hold O_APPEND fds on the log across the supervisor's lifetime
// (systemd StandardOutput=append:, launchd StandardOutPath), and renaming
// the active file would divert the running instance's own output into the
// archive. Truncation keeps the inode, so O_APPEND writers continue at
// offset 0. The archive is staged under a per-rotation unique name
// (os.CreateTemp in the log's directory) and moved into place with an
// atomic os.Rename, so no partially written archive is ever visible under
// the archive name and a crash mid-copy leaves only a staging file for the
// next successful rotation to sweep. The copy is bounded at
// supervisorLogArchiveMaxBytes; a tail beyond the bound is dropped from
// the archive and reported to warn after the truncate so the notice lands
// in the fresh log.
func rotateSupervisorLog(path string, now time.Time, warn io.Writer) error {
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening supervisor log %s for archiving: %w", path, err)
	}
	defer src.Close() //nolint:errcheck // read-only handle

	tmp, err := stageCompressedSupervisorLog(src, path)
	if err != nil {
		return err
	}
	// Probe one byte past the bounded copy to learn whether a tail was
	// dropped. A probe error is treated as end-of-input: the archive
	// already holds everything the bounded copy could read, and the worst
	// outcome of a misread here is a missing warning line.
	var probe [1]byte
	n, _ := src.Read(probe[:])
	tailDropped := n > 0

	archive, err := supervisorLogArchivePath(path, now)
	if err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup of staged archive
		return err
	}
	if err := os.Rename(tmp, archive); err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup of staged archive
		return fmt.Errorf("finalizing supervisor log archive %s: %w", archive, err)
	}
	if err := os.Truncate(path, 0); err != nil {
		return fmt.Errorf("truncating supervisor log %s after archiving to %s: %w", path, archive, err)
	}
	if tailDropped {
		fmt.Fprintf(warn, "gc supervisor: supervisor log %s exceeded the %d-byte archive bound; archived the first %d bytes to %s and dropped the rest\n", //nolint:errcheck // best-effort warning
			path, supervisorLogArchiveMaxBytes, supervisorLogArchiveMaxBytes, archive)
	}
	return nil
}

// stageCompressedSupervisorLog gzip-streams at most
// supervisorLogArchiveMaxBytes of src into a uniquely named staging file
// created with os.CreateTemp next to the log, and returns the staging
// path. The per-rotation staging name is load-bearing: a fixed name lets a
// concurrent rotation O_TRUNC this one's staged bytes mid-write, which is
// exactly how racing supervisor starts used to corrupt the archive and
// then truncate away the only intact copy. The bounded copy keeps a
// pathological log from tying the rotation to unbounded input.
func stageCompressedSupervisorLog(src io.Reader, path string) (string, error) {
	dst, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".archive-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating supervisor log archive staging file next to %s: %w", path, err)
	}
	tmp := dst.Name()
	gz := gzip.NewWriter(dst)
	if _, err := io.Copy(gz, io.LimitReader(src, supervisorLogArchiveMaxBytes)); err != nil {
		dst.Close()    //nolint:errcheck // surfacing the copy error instead
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup of staged archive
		return "", fmt.Errorf("compressing supervisor log into %s: %w", tmp, err)
	}
	if err := gz.Close(); err != nil {
		dst.Close()    //nolint:errcheck // surfacing the gzip error instead
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup of staged archive
		return "", fmt.Errorf("flushing supervisor log archive %s: %w", tmp, err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup of staged archive
		return "", fmt.Errorf("closing supervisor log archive staging file %s: %w", tmp, err)
	}
	return tmp, nil
}

// supervisorLogArchivePath returns a not-yet-existing archive path of the
// form <path>.archive-<UTC timestamp>.gz, appending a numeric
// disambiguator when a rotation already landed in the same second.
func supervisorLogArchivePath(path string, now time.Time) (string, error) {
	base := fmt.Sprintf("%s.archive-%s", path, now.UTC().Format(supervisorLogArchiveTimestampLayout))
	candidate := base + ".gz"
	for i := 1; ; i++ {
		_, err := os.Stat(candidate)
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("probing supervisor log archive name %s: %w", candidate, err)
		}
		candidate = fmt.Sprintf("%s-%d.gz", base, i)
	}
}

// pruneSupervisorLogArchives deletes the oldest supervisor log archives so
// at most keep remain, ordered by modification time with filename as the
// tiebreaker.
func pruneSupervisorLogArchives(path string, keep int) error {
	if keep < 1 {
		keep = 1
	}
	dir := filepath.Dir(path)
	prefix := filepath.Base(path) + ".archive-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("listing supervisor log archives in %s: %w", dir, err)
	}
	type archive struct {
		name string
		mod  time.Time
	}
	var archives []archive
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".gz") {
			continue
		}
		info, err := e.Info()
		if os.IsNotExist(err) {
			continue // removed concurrently; nothing to prune
		}
		if err != nil {
			return fmt.Errorf("inspecting supervisor log archive %s: %w", name, err)
		}
		archives = append(archives, archive{name: name, mod: info.ModTime()})
	}
	if len(archives) <= keep {
		return nil
	}
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].mod.Equal(archives[j].mod) {
			return archives[i].name < archives[j].name
		}
		return archives[i].mod.Before(archives[j].mod)
	})
	for _, a := range archives[:len(archives)-keep] {
		stale := filepath.Join(dir, a.name)
		if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("pruning supervisor log archive %s: %w", stale, err)
		}
	}
	return nil
}

// sweepSupervisorLogStagingFiles removes <log>.archive-*.tmp leftovers
// from rotations that crashed between staging and rename. Staging names
// are unique per rotation (os.CreateTemp), so without a sweep the
// leftovers would accumulate one file per crashed rotation forever. Runs
// after a successful rotation, under the supervisor single-instance lock,
// so no concurrent rotation can be mid-staging while the sweep deletes.
func sweepSupervisorLogStagingFiles(path string) error {
	stale, err := filepath.Glob(path + ".archive-*.tmp")
	if err != nil {
		return fmt.Errorf("globbing supervisor log staging leftovers for %s: %w", path, err)
	}
	for _, p := range stale {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale supervisor log staging file %s: %w", p, err)
		}
	}
	return nil
}
