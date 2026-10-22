package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workdir"
)

// PoolWorkDirIsolationCheck warns when an agent that may run concurrent
// sessions resolves every instance to the same rig-root working tree, the
// #1181 hazard: two pool sessions sharing one checkout silently clobber
// each other's uncommitted work the moment both run at once.
//
// A shared cwd is a legitimate choice (single-agent cities, tutorial
// setups), so the check is always a Warning, never an Error, and offers no
// fix — the remedies are configuration the operator must choose. It covers
// both implicit provider pools (unlimited by construction) and explicit
// pools carrying a pool signal; every other agent shape is left alone.
type PoolWorkDirIsolationCheck struct {
	cfg *config.City
}

// NewPoolWorkDirIsolationCheck creates the shared-workdir warning check for
// every pool-shaped agent in cfg.
func NewPoolWorkDirIsolationCheck(cfg *config.City) *PoolWorkDirIsolationCheck {
	return &PoolWorkDirIsolationCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *PoolWorkDirIsolationCheck) Name() string { return "pool-workdir-isolation" }

// CanFix returns false — the remedies are configuration choices.
func (c *PoolWorkDirIsolationCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *PoolWorkDirIsolationCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; the warning is informational and must not
// add work to gc start's warm-up scan.
func (c *PoolWorkDirIsolationCheck) WarmupEligible() bool { return false }

// Run probes each concurrent-session-capable rig-scoped agent by resolving
// two synthetic instances ("-1" and "-2") and warns when both land on the
// rig root of a git-backed rig.
func (c *PoolWorkDirIsolationCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no city config loaded"
		return r
	}
	cityPath := ""
	if ctx != nil {
		cityPath = ctx.CityPath
	}
	cityName := workdir.CityName(cityPath, c.cfg)
	var issues []string
	for i := range c.cfg.Agents {
		a := c.cfg.Agents[i]
		if !a.SupportsExpandedSessionIdentities() {
			continue
		}
		// Only pools that can run more than one concurrent instance are a
		// hazard; a max of 1 (agent, rig or workspace) honors that.
		if limit := a.ResolvedMaxActiveSessions(c.cfg); limit != nil && *limit == 1 {
			continue
		}
		rigName := workdir.ConfiguredRigName(cityPath, a, c.cfg.Rigs)
		if rigName == "" {
			continue
		}
		if !a.Implicit && !workdir.RequiresPoolWorkDirIsolationCheck(a) {
			continue
		}
		first, second, err := workdir.PoolInstanceWorkDirs(cityPath, cityName, a, c.cfg.Rigs)
		if err != nil {
			// Template errors are the load path's business (strict
			// resolution fails session start); doctor reports on the
			// resolution that exists.
			continue
		}
		if first != second {
			continue
		}
		rigRoot := workdir.ResolveDirPath(cityPath, workdir.RigRootForName(rigName, c.cfg.Rigs))
		if first != rigRoot {
			continue
		}
		// Lstat only: a .git file (linked worktree) still marks a git repo.
		// No subprocess, no walk upward.
		if _, err := os.Lstat(filepath.Join(first, ".git")); err != nil {
			continue
		}
		issues = append(issues, fmt.Sprintf(
			"agent %q: pool instances %q and %q share the working tree %q; concurrent sessions editing one checkout clobber each other's uncommitted work",
			a.QualifiedName(), a.QualifiedInstanceName(a.Name+"-1"), a.QualifiedInstanceName(a.Name+"-2"), first))
	}
	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "every concurrent-session agent resolves a distinct working directory per instance"
		return r
	}
	sort.Strings(issues)
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d agent pool(s) share one working tree; concurrent sessions can clobber each other's uncommitted work", len(issues))
	r.Details = issues
	r.FixHint = `set [workspace] implicit_agent_isolation = "worktree" (implicit provider agents), cap the pool with max_active_sessions = 1 ([[patches.agent]], rig or workspace), or give the agent a work_dir containing {{.AgentBase}} plus a pre_start that creates the worktree`
	return r
}
