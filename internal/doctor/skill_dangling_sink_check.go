package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/materialize"
)

// SkillDanglingSinkCheck surfaces dangling symlinks in agent skill
// sinks — links whose target no longer exists. The motivating case is
// the .gc/system/packs retirement (#3344): its config-only migration
// stranded every pre-manifest sink link, and the materializer's
// ownership gate then treated those orphans as user-owned forever
// (hq-38je). gc doctor previously reported only skill collisions, so
// the fleet-wide breakage was invisible to the standard health check.
//
// The check walks a static sink list (agent scope-root × vendor sinks,
// resolved by the caller from config) plus a lazily-evaluated live
// session-workdir sink list, and Lstat/Readlinks every entry. Dangling
// links are classified gc-owned (target under a legacy/cache root —
// safe for --fix to remove; the next materialize pass recreates any
// still-desired link) or user-owned (reported only).
type SkillDanglingSinkCheck struct {
	staticSinks []string
	gcRoots     []string
	liveSinksFn func() []string
}

// NewSkillDanglingSinkCheck builds a check that scans the given sink
// directories for dangling symlinks. staticSinks are the config-derived
// agent sinks; gcOwnedRoots are the retired/managed roots (typically
// materialize.LegacyOwnedRootsFor(cityPath)) whose dangling links
// --fix may remove. liveSinksFn, when non-nil, is evaluated inside Run
// so store-backed live-session enumeration does not slow check
// construction or fail a doctor run that never reaches this check.
func NewSkillDanglingSinkCheck(staticSinks []string, gcOwnedRoots []string, liveSinksFn func() []string) *SkillDanglingSinkCheck {
	return &SkillDanglingSinkCheck{staticSinks: staticSinks, gcRoots: gcOwnedRoots, liveSinksFn: liveSinksFn}
}

// Name returns the check identifier.
func (c *SkillDanglingSinkCheck) Name() string { return "skill-dangling-sink" }

// danglingSinkLink records one dangling symlink found in a sink.
type danglingSinkLink struct {
	path    string // absolute path of the symlink
	target  string // raw readlink target
	gcOwned bool   // target under a legacy/cache root — safe to remove
}

// scan walks every sink and returns the dangling links, deduplicated
// and sorted by path. Missing sink directories are skipped silently —
// an agent that never started has no sink and nothing to report.
func (c *SkillDanglingSinkCheck) scan() []danglingSinkLink {
	sinks := append([]string{}, c.staticSinks...)
	if c.liveSinksFn != nil {
		sinks = append(sinks, c.liveSinksFn()...)
	}
	seen := make(map[string]bool)
	var out []danglingSinkLink
	for _, sink := range sinks {
		if sink == "" || seen[sink] {
			continue
		}
		seen[sink] = true
		entries, err := os.ReadDir(sink)
		if err != nil {
			continue
		}
		for _, de := range entries {
			path := filepath.Join(sink, de.Name())
			info, err := os.Lstat(path)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				continue
			}
			target, err := os.Readlink(path)
			if err != nil {
				continue
			}
			// Dangling = following the link fails with not-exist.
			// Other stat errors (permission, I/O) are inconclusive —
			// never classify as dangling, so --fix cannot remove a
			// link whose health we could not establish.
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				continue
			}
			out = append(out, danglingSinkLink{
				path:    path,
				target:  target,
				gcOwned: materialize.TargetUnderManagedRoot(target, c.gcRoots),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// Run reports a warning when any sink entry is a dangling symlink.
func (c *SkillDanglingSinkCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	dangling := c.scan()
	if len(dangling) == 0 {
		r.Status = StatusOK
		r.Message = "no dangling skill-sink symlinks"
		return r
	}
	gcOwned := 0
	details := make([]string, 0, len(dangling))
	for _, d := range dangling {
		class := "user-owned"
		if d.gcOwned {
			class = "gc-owned"
			gcOwned++
		}
		details = append(details, fmt.Sprintf("%s -> %s (%s, dangling)", d.path, d.target, class))
	}
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = fmt.Sprintf("%d dangling skill-sink symlink(s) (%d gc-owned)", len(dangling), gcOwned)
	r.Details = details
	if gcOwned > 0 {
		r.FixHint = "gc doctor --fix removes gc-owned dangling links; the next materialize pass recreates any still-desired link"
	} else {
		r.FixHint = "remove user-owned dangling links manually after confirming the target is truly retired"
	}
	return r
}

// CanFix returns true — gc-owned dangling links are safe to remove.
func (c *SkillDanglingSinkCheck) CanFix() bool { return true }

// WarmupEligible returns false — the scan is cheap but the live-session
// sink enumeration opens the session store, which the `gc start`
// warm-up path should not pay for.
func (c *SkillDanglingSinkCheck) WarmupEligible() bool { return false }

// Fix removes every gc-owned dangling symlink found by a fresh scan.
// User-owned links are never touched. A re-scan (rather than cached Run
// state) keeps the deletion decision current with the filesystem.
func (c *SkillDanglingSinkCheck) Fix(_ *CheckContext) error {
	var failed []string
	for _, d := range c.scan() {
		if !d.gcOwned {
			continue
		}
		if err := os.Remove(d.path); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", d.path, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("removing dangling gc-owned skill-sink links: %s", strings.Join(failed, "; "))
	}
	return nil
}
