package doctor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
)

// HooksCheck verifies that installed hook files are up to date.
// Checks all rig workdirs for stale managed hook files that need upgrading.
type HooksCheck struct {
	CityDir  string
	WorkDirs []string
	FS       fsys.FS
	// FixFn reinstalls hooks in all workdirs. Called by Fix().
	FixFn func(stale map[string][]string) error
}

// NewHooksCheck creates a HooksCheck for the given city and rig workdirs.
func NewHooksCheck(cityDir string, workDirs []string, fs fsys.FS, fixFn func(map[string][]string) error) *HooksCheck {
	return &HooksCheck{
		CityDir:  cityDir,
		WorkDirs: workDirs,
		FS:       fs,
		FixFn:    fixFn,
	}
}

// Name returns the check identifier.
func (c *HooksCheck) Name() string { return "hooks" }

// Run checks all rig workdirs for stale hook files.
func (c *HooksCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	allStale := c.findStale()
	if len(allStale) == 0 {
		r.Status = StatusOK
		r.Message = "all hook files up to date"
		return r
	}

	// Sort keys for deterministic output.
	dirs := make([]string, 0, len(allStale))
	for wd := range allStale {
		dirs = append(dirs, wd)
	}
	sort.Strings(dirs)

	var details []string
	total := 0
	for _, workDir := range dirs {
		providers := allStale[workDir]
		total += len(providers)
		details = append(details, fmt.Sprintf("%s: %s", workDir, strings.Join(providers, ", ")))
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d stale hook file(s) found", total)
	r.Details = details
	r.FixHint = "run gc doctor --fix or gc start to upgrade"
	return r
}

// CanFix returns true — stale hooks can be reinstalled.
func (c *HooksCheck) CanFix() bool { return c.FixFn != nil }

// Fix reinstalls stale hook files.
func (c *HooksCheck) Fix(_ *CheckContext) error {
	if c.FixFn == nil {
		return fmt.Errorf("no fix function provided")
	}
	return c.FixFn(c.findStale())
}

// findStale returns a map of directory → stale provider names.
// City-level providers (Claude) are checked once under cityDir.
// WorkDir-level providers (codex, gemini, cursor) are checked per rig.
func (c *HooksCheck) findStale() map[string][]string {
	result := make(map[string][]string)
	// City-level hooks (Claude) — check once, not per workDir.
	if cityStale := hooks.StaleCityHooks(c.FS, c.CityDir); len(cityStale) > 0 {
		result[c.CityDir] = cityStale
	}
	// WorkDir-level hooks (codex, gemini, cursor) — check per rig.
	for _, wd := range c.WorkDirs {
		if wdStale := hooks.StaleWorkDirHooks(c.FS, wd); len(wdStale) > 0 {
			result[wd] = wdStale
		}
	}
	return result
}
