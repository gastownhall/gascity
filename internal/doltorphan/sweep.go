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

// sentinelFileName, when present directly inside a top-level candidate
// directory, exempts that directory from sweeping entirely regardless of
// age or any nested .dolt marker. This is a per-directory opt-out an owner
// creates deliberately (architect ruling ga-txnhdk, Option 4) — not a
// name/prefix filter on the candidate itself, which was rejected because it
// would reintroduce the fragility ga-ntbpyb.2 AC2's name-independent
// detection removed.
const sentinelFileName = ".no-orphan-sweep"

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
	// Removed lists the store directories that were removed.
	Removed []string
	// Skipped counts candidates that matched age+marker but were held
	// open per lsof, or were held per fail-closed lsof-error handling.
	Skipped int
	// Errors collects non-fatal problems (a single candidate's removal
	// failing, or the lsof scan itself failing) without aborting the rest
	// of the pass.
	Errors []error
}

// candidate pairs a top-level swept directory with the Dolt store directory
// nested within it that actually owns the .dolt marker. lsof-held checks
// stay keyed on topLevel — matching lsofHeldChildren's one-segment-past-root
// regex, which cannot see any deeper — while removal targets storeDir, so a
// container's unrelated siblings survive sweeping its abandoned store.
type candidate struct {
	topLevel string
	storeDir string
}

// Sweep considers direct children of cfg.Root that look like abandoned dolt
// containers: mtime older than MinAge, a .dolt marker directory within
// maxMarkerDepth levels, and not currently held open by any live process
// per lsof (checked against the top-level child, since that's as deep as
// lsofHeldChildren can see). Candidate selection intentionally does not
// filter on directory name — the signals above are what establish
// abandonment, not any particular naming convention, so this catches leaks
// "regardless of creation source" (ga-ntbpyb.2 acceptance criterion 2)
// including directories named by Go's t.TempDir() rather than the
// bare-mktemp "tmp.*" pattern the heuristic was first observed against.
//
// A top-level child containing a file literally named sentinelFileName is
// exempted entirely, regardless of age or marker.
//
// Removal targets only the directory that actually owns the .dolt marker,
// which may be nested below the top-level child — never the top-level
// child itself — so that unrelated payload the child legitimately holds
// alongside an abandoned Dolt copy survives the sweep.
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
	var candidates []candidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(cfg.Root, e.Name())
		if hasSentinel(dir) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < minAge {
			continue
		}
		storeDir, ok := findDoltStoreDir(dir, maxMarkerDepth)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{topLevel: dir, storeDir: storeDir})
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

	for _, c := range candidates {
		if held[c.topLevel] {
			result.Skipped++
			continue
		}
		if err := removeAll(c.storeDir); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("remove %s: %w", c.storeDir, err))
			continue
		}
		result.Removed = append(result.Removed, c.storeDir)
	}
	return result
}

// hasSentinel reports whether dir directly contains sentinelFileName.
func hasSentinel(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, sentinelFileName))
	return err == nil
}

// findDoltStoreDir searches for a directory literally named ".dolt" within
// depth levels of dir (dir's direct children are depth 1) and, if found,
// returns the path of the directory that directly contains it — the actual
// Dolt store root. This is the removal target: deleting only this path,
// rather than the top-level candidate dir, is what keeps a container's
// unrelated siblings intact when it legitimately holds other payload
// alongside an abandoned Dolt copy nested deeper within it.
//
// A subtree can contain more than one .dolt marker at different depths: a
// `dolt sql-server --data-dir <dir>` writes its own server-bookkeeping
// marker directly at <dir>/.dolt (holding tmp/ and sql-server.info),
// distinct from a database's own store marker nested inside it (e.g.
// <dir>/<db>/.dolt, created by `dolt init`). Since os.ReadDir returns
// entries in lexical order and ".dolt" sorts before most other names, a
// naive first-match search hits the shallow bookkeeping marker before ever
// looking inside the sibling that holds the real, deeper store — which
// makes Sweep delete the whole top-level container instead of just the
// abandoned store nested within it. To avoid that, this function always
// prefers the deepest match: it fully explores every subdirectory before
// falling back to treating dir itself as the store when dir directly
// contains a marker.
func findDoltStoreDir(dir string, depth int) (string, bool) {
	if depth <= 0 {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	selfIsStore := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == ".dolt" {
			selfIsStore = true
			continue
		}
		if store, ok := findDoltStoreDir(filepath.Join(dir, e.Name()), depth-1); ok {
			return store, true
		}
	}
	if selfIsStore {
		return dir, true
	}
	return "", false
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
