package doctor

import (
	"fmt"
	"os/exec"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workdir"
)

// PoolWorkDirIsolationCheck warns when the instances of a pool agent would
// share one working directory, or when a slot-isolated pool's rig has an
// unborn HEAD and so cannot mint a worktree. Advisory only: a shared
// explicit pool can be deliberate, so the check names the agent and the
// reason rather than gating the run.
type PoolWorkDirIsolationCheck struct {
	cfg      *config.City
	cityPath string
	cityName string
	// sessionProvider is the already-resolved session provider name; used
	// only to name the runtime in the remote-runtime warning.
	sessionProvider string
	// providerHostLocal reports whether sessionProvider runs sessions on
	// this host. A non-host-local runtime cannot use host-side worktrees,
	// so slot isolation is skipped there and the instances share the rig
	// root exactly as they did before isolation.
	providerHostLocal bool
	// gitPath is injectable for tests.
	gitPath func(name string) (string, error)
}

// NewPoolWorkDirIsolationCheck creates the pool working-directory isolation
// check. sessionProvider must already be resolved through the same
// effectiveProviderName path session start uses; providerHostLocal is that
// provider's host-local verdict, so the probe agrees with what provisioning
// will actually do.
func NewPoolWorkDirIsolationCheck(cfg *config.City, cityPath, cityName, sessionProvider string, providerHostLocal bool) *PoolWorkDirIsolationCheck {
	return &PoolWorkDirIsolationCheck{
		cfg:               cfg,
		cityPath:          cityPath,
		cityName:          cityName,
		sessionProvider:   sessionProvider,
		providerHostLocal: providerHostLocal,
		gitPath:           exec.LookPath,
	}
}

// Name returns the check identifier.
func (c *PoolWorkDirIsolationCheck) Name() string { return "pool-workdir-isolation" }

// Run probes each pool-capable agent's synthetic -1/-2 instance identities
// through the same strict resolution path session start uses and warns when
// both land in one directory, or when a marked agent's rig HEAD is unborn.
func (c *PoolWorkDirIsolationCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no config; nothing to check"
		return r
	}

	var details []string
	for i := range c.cfg.Agents {
		a := c.cfg.Agents[i]
		if !a.Implicit && !workdir.RequiresPoolWorkDirIsolationCheck(a) {
			continue
		}

		probe := a
		if probe.PoolSlotWorktrees && !c.providerHostLocal {
			// At provision time resolveConfiguredWorkDir strips the mark
			// for non-host-local runtimes, so the instances resolve to
			// exactly what the stripped probe reports here.
			probe.PoolSlotWorktrees = false
		}

		firstName := probe.QualifiedInstanceName(probe.Name + "-1")
		secondName := probe.QualifiedInstanceName(probe.Name + "-2")
		first, err1 := workdir.ResolveWorkDirPathStrict(c.cityPath, c.cityName, firstName, probe, c.cfg.Rigs)
		second, err2 := workdir.ResolveWorkDirPathStrict(c.cityPath, c.cityName, secondName, probe, c.cfg.Rigs)
		if err1 != nil || err2 != nil {
			probeErr := err1
			if probeErr == nil {
				probeErr = err2
			}
			details = append(details, fmt.Sprintf(
				"agent %q: resolving pool instance work dirs failed: %v", a.QualifiedName(), probeErr))
			continue
		}
		if first == second {
			details = append(details, fmt.Sprintf(
				"agent %q: instances %q and %q both resolve to %q (%s)",
				a.QualifiedName(), firstName, secondName, first, c.sharingReason(a)))
			continue
		}
		if a.PoolSlotWorktrees && c.providerHostLocal {
			if detail, ok := c.unbornRigHeadDetail(a); ok {
				details = append(details, detail)
			}
		}
	}

	if len(details) == 0 {
		r.Status = StatusOK
		r.Message = "every pool agent's instances run in isolated working directories"
		return r
	}
	r.Status = StatusWarning
	r.Severity = SeverityAdvisory
	r.Message = fmt.Sprintf("%d pool agent(s) share a working directory or cannot start", len(details))
	r.Details = details
	r.FixHint = "give each pool instance its own working directory (a work_dir template with {{.AgentBase}} for explicit pools; a git rig for implicit slots), or opt out explicitly with workspace.implicit_isolation = \"none\""
	return r
}

// sharingReason names why the probed agent's instances share a directory, so
// the warning tells the operator which knob actually applies.
func (c *PoolWorkDirIsolationCheck) sharingReason(a config.Agent) string {
	if a.Implicit {
		if c.cfg.Workspace.EffectiveImplicitIsolation() == config.ImplicitIsolationNone {
			return fmt.Sprintf("workspace.implicit_isolation = %q keeps them on the shared rig root", config.ImplicitIsolationNone)
		}
		if a.PoolSlotWorktrees && !c.providerHostLocal {
			return fmt.Sprintf("session runtime %q is not host-local, so slot worktrees are skipped", c.sessionProvider)
		}
		return fmt.Sprintf("rig %q has no git repository to mint slot worktrees from", a.Dir)
	}
	return "work_dir does not vary per instance; set work_dir to a template that includes {{.AgentBase}}"
}

// unbornRigHeadDetail reports the unborn-HEAD warning line for a marked
// agent's rig: provisioning branches each slot worktree from rig HEAD, so an
// unborn HEAD means slot sessions cannot start. Git being unavailable is
// left to the git-binary check; nothing is claimed here.
func (c *PoolWorkDirIsolationCheck) unbornRigHeadDetail(a config.Agent) (string, bool) {
	gitBin, err := c.gitPath("git")
	if err != nil {
		return "", false
	}
	rigName := workdir.ConfiguredRigName(c.cityPath, a, c.cfg.Rigs)
	rigRoot := workdir.RigRootForName(rigName, c.cfg.Rigs)
	if rigRoot == "" {
		return "", false
	}
	if _, err := runGitCommand(gitBin, rigRoot, "rev-parse", "--verify", "HEAD"); err != nil {
		return fmt.Sprintf(
			"agent %q: rig %q HEAD is unborn; its slot worktrees cannot branch from it until the rig has a commit",
			a.QualifiedName(), rigName), true
	}
	return "", false
}

// WarmupEligible returns false; the check runs on demand via `gc doctor`.
func (c *PoolWorkDirIsolationCheck) WarmupEligible() bool { return false }

// CanFix returns false because isolation is a configuration decision.
func (c *PoolWorkDirIsolationCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *PoolWorkDirIsolationCheck) Fix(_ *CheckContext) error { return nil }
