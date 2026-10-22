package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// PoolWorkDirIsolationCheck warns when a rig-scoped agent that may run more
// than one concurrent session resolves every instance to the rig root while
// that root is a git checkout. Concurrent instances then share one HEAD,
// index and working tree, so one instance's branch switch or reset clobbers
// another's uncommitted work.
//
// The check probes two synthetic instances through the same work_dir
// resolution session startup uses (workdirutil.PoolInstanceWorkDirs). It
// covers implicit provider agents, whose pool shape comes from their unset
// max_active_sessions, and explicit agents carrying a pool signal
// (workdirutil.RequiresPoolWorkDirIsolationCheck).
type PoolWorkDirIsolationCheck struct {
	cfg *config.City
}

// NewPoolWorkDirIsolationCheck creates a check that reports rig-scoped
// pooled agents whose concurrent instances share the rig's git checkout.
func NewPoolWorkDirIsolationCheck(cfg *config.City) *PoolWorkDirIsolationCheck {
	return &PoolWorkDirIsolationCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *PoolWorkDirIsolationCheck) Name() string { return "pool-workdir-isolation" }

// CanFix returns false — choosing an isolation remedy is the operator's call.
func (c *PoolWorkDirIsolationCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *PoolWorkDirIsolationCheck) Fix(_ *CheckContext) error { return nil }

// Run probes each pooled rig-scoped agent and warns when its instances share
// a git rig root.
func (c *PoolWorkDirIsolationCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Status: StatusOK}
	if c.cfg == nil {
		r.Message = "no city config loaded"
		return r
	}
	cityPath := ""
	if ctx != nil {
		cityPath = ctx.CityPath
	}
	cityName := workdirutil.CityName(cityPath, c.cfg)
	var shared []string
	for i := range c.cfg.Agents {
		a := c.cfg.Agents[i]
		if !c.mayRunConcurrently(a) {
			continue
		}
		rigName := workdirutil.ConfiguredRigName(cityPath, a, c.cfg.Rigs)
		if rigName == "" {
			continue
		}
		rigRoot := workdirutil.ResolveDirPath(cityPath, workdirutil.RigRootForName(rigName, c.cfg.Rigs))
		first, second, err := workdirutil.PoolInstanceWorkDirs(cityPath, cityName, a, c.cfg.Rigs)
		if err != nil {
			r.Details = append(r.Details, err.Error())
			continue
		}
		if first != second || filepath.Clean(first) != filepath.Clean(rigRoot) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(rigRoot, ".git")); err != nil {
			continue
		}
		q := a.QualifiedName()
		shared = append(shared, q)
		r.Details = append(r.Details, fmt.Sprintf("%q: instances %q and %q both work in git checkout %s",
			q, a.QualifiedInstanceName(a.Name+"-1"), a.QualifiedInstanceName(a.Name+"-2"), rigRoot))
	}
	if len(r.Details) == 0 {
		r.Message = "pooled rig agents have isolated working directories"
		return r
	}
	r.Status = StatusWarning
	if len(shared) == 0 {
		r.Message = "cannot resolve pooled rig agent working directories"
		return r
	}
	r.Message = fmt.Sprintf("%d pooled rig agent(s) share the rig git checkout across concurrent sessions: %s",
		len(shared), strings.Join(shared, ", "))
	r.FixHint = `isolate concurrent sessions with one of: ` +
		`[workspace] implicit_agent_isolation = "worktree" (implicit provider agents only); ` +
		`a [[patches.agent]] for the agent with max_active_sessions = 1; ` +
		`or an explicit work_dir containing {{.AgentBase}} plus a pre_start that creates that git worktree`
	return r
}

// mayRunConcurrently reports whether a may run more than one concurrent
// session: it has expanded session identities, its resolved
// max_active_sessions (agent, then rig, then workspace) is unset, negative or
// above 1, and it is either an implicit provider agent or carries an explicit
// pool signal.
func (c *PoolWorkDirIsolationCheck) mayRunConcurrently(a config.Agent) bool {
	if !a.SupportsExpandedSessionIdentities() {
		return false
	}
	if m := a.ResolvedMaxActiveSessions(c.cfg); m != nil && *m >= 0 && *m <= 1 {
		return false
	}
	return a.Implicit || workdirutil.RequiresPoolWorkDirIsolationCheck(a)
}
