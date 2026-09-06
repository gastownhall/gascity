// Package doltorphan implements a symptom-based fallback sweep for
// orphaned dolt store directories: a directory is a removal candidate when
// it is old, contains a .dolt marker, and is not held open by any live
// process. It composes with, but does not replace, process-level
// classification (e.g. cmd/gc's classifyDoltProcess) — this package never
// inspects or kills processes, it only judges directories that are already
// symptomatic of abandonment, which is what lets it catch leaks regardless
// of what created them (a killed test binary, an untracked ad-hoc dolt
// invocation, etc.). Ported from the production-proven heuristic in
// gc-test-dolt-reaper.sh sections 4-5.
package doltorphan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/dolthub/fslock"
	"github.com/gastownhall/gascity/internal/clock"
)

// DefaultMinAge is the age a candidate directory's mtime must clear before
// the sweep will consider it abandoned. Matches acceptance criterion 2 of
// ga-ntbpyb.2.
const DefaultMinAge = 60 * time.Minute

// maxMarkerDepth bounds how deep the .dolt marker search descends below a
// candidate directory, mirroring `find "$d" -maxdepth 3 -type d -name
// '.dolt'` from gc-test-dolt-reaper.sh section 4.
const maxMarkerDepth = 3

// lsofScanTimeout bounds the real `lsof -w` invocation, mirroring the
// shell script's `timeout 30 lsof -w`.
const lsofScanTimeout = 30 * time.Second

// lockFileName is the file Dolt's NBS store holds a real kernel flock on
// for as long as a store is open (github.com/dolthub/dolt/go/store/nbs,
// lockFileName), conventionally at <store>/.dolt/noms/LOCK.
const lockFileName = "LOCK"

// maxLockSearchDepth bounds how deep findLockFiles descends below a
// candidate directory looking for a dolt LOCK file, two levels deeper than
// maxMarkerDepth to comfortably cover <marker>/noms/LOCK wherever the
// .dolt marker itself was found.
const maxLockSearchDepth = maxMarkerDepth + 2

// SweepConfig configures a single Sweep pass. Root is required; every
// other field defaults to production behavior when left zero-valued.
type SweepConfig struct {
	// Root is the directory whose direct children are swept, e.g. os.TempDir().
	Root string
	// MinAge overrides DefaultMinAge when positive.
	MinAge time.Duration
	// Clock supplies "now" for age comparisons. Defaults to clock.Real{}.
	Clock clock.Clock
	// RunLsof runs `lsof -w` (or an equivalent) and returns its raw
	// stdout. Defaults to a real lsof -w invocation. Injectable for tests.
	RunLsof func(ctx context.Context) ([]byte, error)
	// RemoveAll removes a candidate directory. Defaults to os.RemoveAll.
	// Injectable for tests.
	RemoveAll func(path string) error
}

// SweepResult reports what a Sweep pass did.
type SweepResult struct {
	// Removed lists the candidate directories that were removed.
	Removed []string
	// Skipped counts candidates that matched age+marker but were held
	// open per lsof, or were held per fail-closed lsof-error handling.
	Skipped int
	// Errors collects non-fatal problems (a single candidate's removal
	// failing, or the lsof scan itself failing) without aborting the rest
	// of the pass.
	Errors []error
}

// Sweep removes direct children of cfg.Root that look like abandoned dolt
// store directories: mtime older than MinAge, a .dolt marker directory
// within maxMarkerDepth levels, and not currently held open by any live
// process per two independent lsof scans. Candidate selection intentionally
// does not filter on directory name — the three signals above are what
// establish abandonment, not any particular naming convention, so this
// catches leaks "regardless of creation source" (ga-ntbpyb.2 acceptance
// criterion 2) including directories named by Go's t.TempDir() rather than
// the bare-mktemp "tmp.*" pattern the heuristic was first observed against.
//
// A directory is removed only when it is absent from both an initial lsof
// scan and a confirming second scan taken just before removal. A single
// lsof invocation is not atomic: under heavy concurrent process churn on a
// shared host, lsof's process-by-process scan can transiently miss a
// still-open file for one specific PID without lsof itself reporting an
// error (observed in ga-vbyn8v — a live dolt sql-server's data dir was
// reported unheld and removed under a 40-job parallel test run, though the
// identical test passed in isolation). Either scan reporting a directory
// held is enough to protect it, so the confirm scan only ever adds
// caution — it cannot cause a genuinely-held directory to be removed, and
// it is only spent on candidates that are already about to be deleted.
//
// If an lsof scan fails, Sweep fails closed: nothing is removed this pass
// (an unverifiable "is this held open" check is treated the same as "yes,
// it's held").
func Sweep(cfg SweepConfig) SweepResult {
	var result SweepResult

	removeAll := cfg.RemoveAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	minAge := cfg.MinAge
	if minAge <= 0 {
		minAge = DefaultMinAge
	}

	entries, err := os.ReadDir(cfg.Root)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("read %s: %w", cfg.Root, err))
		return result
	}

	now := clk.Now()
	var candidates []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(cfg.Root, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < minAge {
			continue
		}
		if !hasDoltMarker(dir, maxMarkerDepth) {
			continue
		}
		candidates = append(candidates, dir)
	}
	if len(candidates) == 0 {
		return result
	}

	held, err := lsofHeldChildren(cfg.Root, cfg.RunLsof)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("lsof -w: %w", err))
		result.Skipped = len(candidates)
		return result
	}

	var unheld []string
	for _, dir := range candidates {
		if held[dir] {
			result.Skipped++
			continue
		}
		unheld = append(unheld, dir)
	}
	if len(unheld) == 0 {
		return result
	}

	// Re-confirm just before removal: see the Sweep doc comment for why a
	// single scan isn't trusted to delete anything on its own.
	confirmHeld, err := lsofHeldChildren(cfg.Root, cfg.RunLsof)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("lsof -w (confirm): %w", err))
		result.Skipped += len(unheld)
		return result
	}

	for _, dir := range unheld {
		if confirmHeld[dir] {
			result.Skipped++
			continue
		}
		// Belt-and-suspenders beyond the lsof scans: a single lsof
		// invocation parses a point-in-time /proc snapshot and can
		// transiently miss a still-open file for a live process under
		// heavy host load without erroring (observed in ga-vbyn8v even
		// with the two-scan confirm above). Dolt's own NBS store holds a
		// real kernel flock for as long as it has a store open, which is
		// atomic and race-free because the kernel enforces it directly
		// rather than it being reconstructed from a process listing.
		held, err := doltLockHeld(dir)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("dolt lock probe %s: %w", dir, err))
			result.Skipped++
			continue
		}
		if held {
			result.Skipped++
			continue
		}
		if err := removeAll(dir); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("remove %s: %w", dir, err))
			continue
		}
		result.Removed = append(result.Removed, dir)
	}
	return result
}

// hasDoltMarker reports whether a directory literally named ".dolt" exists
// within depth levels of dir (dir's direct children are depth 1).
func hasDoltMarker(dir string, depth int) bool {
	if depth <= 0 {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == ".dolt" {
			return true
		}
		if hasDoltMarker(filepath.Join(dir, e.Name()), depth-1) {
			return true
		}
	}
	return false
}

// findLockFiles returns the full path of every file literally named "LOCK"
// within depth levels of dir (dir's direct children are depth 1), mirroring
// hasDoltMarker's bounded recursive traversal shape. An unreadable
// subdirectory is reported as an error rather than silently treated as
// lock-free: the caller fails closed on it, matching Sweep's contract that
// an unverifiable "is this held open" check counts the same as "held". This
// matters under exactly the load this check exists for — ReadDir can fail
// with EMFILE under heavy parallel churn, and a silent nil there would let
// a live store through. A subdirectory that vanished mid-walk is not an
// error: nothing that no longer exists can hold a lock.
func findLockFiles(dir string, depth int) ([]string, error) {
	if depth <= 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() {
			nested, err := findLockFiles(filepath.Join(dir, e.Name()), depth-1)
			if err != nil {
				return nil, err
			}
			found = append(found, nested...)
			continue
		}
		if e.Name() == lockFileName {
			found = append(found, filepath.Join(dir, e.Name()))
		}
	}
	return found, nil
}

// doltLockHeld reports whether any dolt LOCK file found within dir is
// currently held by a live process. It only ever probes paths already
// discovered by the read-only findLockFiles walk above — fslock.New +
// TryLock against a not-yet-existing path would create an empty file as a
// side effect (the underlying open uses O_CREATE), so this must never be
// called speculatively against a path that hasn't already been confirmed
// to exist.
func doltLockHeld(dir string) (bool, error) {
	paths, err := findLockFiles(dir, maxLockSearchDepth)
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		held, err := probeLockHeld(path)
		if err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

// probeLockHeld reports whether path is currently held by another process,
// via a non-blocking TryLock. A failed TryLock already closes the lock's
// internal file handle, so Unlock below is always a safe no-op in that
// case; Close only releases the directory handle and never a held lock, so
// Unlock must run first regardless of outcome.
func probeLockHeld(path string) (bool, error) {
	lock, err := fslock.New(path)
	if err != nil {
		return false, fmt.Errorf("open lock %s: %w", path, err)
	}
	lockErr := lock.TryLock()
	_ = lock.Unlock()
	_ = lock.Close()
	if lockErr == nil {
		return false, nil
	}
	if errors.Is(lockErr, fslock.ErrLocked) {
		return true, nil
	}
	return false, fmt.Errorf("probe lock %s: %w", path, lockErr)
}

// lsofHeldChildren runs runLsof (defaulting to a real `lsof -w`) and
// returns the set of root's direct children that appear as a path prefix
// of some open file, i.e. directories currently held open by a live
// process anywhere on the system.
func lsofHeldChildren(root string, runLsof func(ctx context.Context) ([]byte, error)) (map[string]bool, error) {
	if runLsof == nil {
		runLsof = runLsofW
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofScanTimeout)
	defer cancel()
	out, err := runLsof(ctx)
	if err != nil {
		return nil, err
	}
	pattern := regexp.MustCompile(regexp.QuoteMeta(filepath.Clean(root)) + `/[^/\s]+`)
	held := make(map[string]bool)
	for _, m := range pattern.FindAllString(string(out), -1) {
		held[m] = true
	}
	return held, nil
}

// runLsofW runs `lsof -w` and returns its stdout. lsof commonly exits
// non-zero when it cannot read some other process's /proc entries
// (permission denied) even though the rest of its output is valid; that
// case is treated as success (mirroring the shell heuristic's `2>/dev/null`,
// which discards the warning but still uses stdout). Only a failure to run
// lsof at all (missing binary, context deadline) is treated as fatal.
func runLsofW(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "lsof", "-w")
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, err
	}
	return out, nil
}
