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
)

// supervisorLogArchiveTimestampLayout is the compact UTC-pinned timestamp
// embedded in archive filenames (same layout as the events.jsonl archive
// convention) so directory listings sort chronologically.
const supervisorLogArchiveTimestampLayout = "20060102T150405Z"

// maybeRotateSupervisorLog archives the supervisor log when it has reached
// supervisorLogMaxBytes and prunes archives beyond
// supervisorLogKeepArchives. A missing log is a no-op; all other failures
// return an error with context so the caller can surface them.
func maybeRotateSupervisorLog(path string, now time.Time) error {
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
	if err := rotateSupervisorLog(path, now); err != nil {
		return err
	}
	return pruneSupervisorLogArchives(path, supervisorLogKeepArchives)
}

// rotateSupervisorLog compresses the current log contents into a
// timestamped .gz archive next to the active file and truncates the active
// file in place. Copy-then-truncate (not rename) is deliberate: service
// managers hold O_APPEND fds on the log across the supervisor's lifetime
// (systemd StandardOutput=append:, launchd StandardOutPath), and renaming
// the active file would divert the running instance's own output into the
// archive. Truncation keeps the inode, so O_APPEND writers continue at
// offset 0. The archive is staged under a fixed temp name and moved into
// place with an atomic os.Rename; a crash mid-copy leaves only the temp
// file, which the next rotation overwrites.
func rotateSupervisorLog(path string, now time.Time) error {
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening supervisor log %s for archiving: %w", path, err)
	}
	defer src.Close() //nolint:errcheck // read-only handle

	tmp := path + ".archive.tmp"
	if err := compressSupervisorLog(src, tmp); err != nil {
		return err
	}

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
	return nil
}

// compressSupervisorLog gzip-streams src into a staging file at tmp,
// truncating any leftover from a previously crashed rotation.
func compressSupervisorLog(src io.Reader, tmp string) error {
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("creating supervisor log archive staging file %s: %w", tmp, err)
	}
	gz := gzip.NewWriter(dst)
	if _, err := io.Copy(gz, src); err != nil {
		dst.Close()    //nolint:errcheck // surfacing the copy error instead
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup of staged archive
		return fmt.Errorf("compressing supervisor log into %s: %w", tmp, err)
	}
	if err := gz.Close(); err != nil {
		dst.Close()    //nolint:errcheck // surfacing the gzip error instead
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup of staged archive
		return fmt.Errorf("flushing supervisor log archive %s: %w", tmp, err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup of staged archive
		return fmt.Errorf("closing supervisor log archive staging file %s: %w", tmp, err)
	}
	return nil
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
