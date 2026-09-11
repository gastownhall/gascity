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
// process per lsof. Candidate selection intentionally does not filter on
// directory name — the three signals above are what establish
// abandonment, not any particular naming convention, so this catches
// leaks "regardless of creation source" (ga-ntbpyb.2 acceptance criterion
// 2) including directories named by Go's t.TempDir() rather than the
// bare-mktemp "tmp.*" pattern the heuristic was first observed against.
//
// If the lsof scan itself fails, Sweep fails closed: nothing is removed
// this pass (an unverifiable "is this held open" check is treated the
// same as "yes, it's held").
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

	for _, dir := range candidates {
		if held[dir] {
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
