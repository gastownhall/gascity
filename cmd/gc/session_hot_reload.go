package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// rebaselineHotReloadDrift adopts hot-reloadable config drift into a running
// session in place: re-materialize workdir content (CopyFiles, overlay dirs,
// and — for stage-2 sessions — skills) so the live agent sees the new content,
// then rebaseline the four fingerprint metadata fields. No process restart, no
// drain, no SessionDraining event. Returns true if the rebaseline (incl.
// restage) was applied; false if the runtime cannot restage in place (caller
// must then DEFER, never kill).
//
// citySessionProvider is the city-level workspace provider selector
// (cfg.Workspace.Provider); cfgAgent is the matched config.Agent for the
// session's template. Both feed canStage1Materialize so pod/in-process runtimes
// (acp/k8s/hybrid) — where re-staging the host workdir would not reach the
// agent — return false and the caller defers rather than cold-kills.
//
// agentCfg carries the resolved WorkDir + staging inputs (CopyFiles,
// OverlayDir, PackOverlayDirs) for the session; StageSessionWorkDirWithWarnings
// re-stages them onto the host filesystem, which is correct for tmux/subprocess
// and is gated off for pod runtimes above.
func rebaselineHotReloadDrift(
	citySessionProvider string,
	cfgAgent *config.Agent,
	session *beads.Bead,
	store beads.Store,
	cfg *config.City,
	cityPath string,
	name string,
	agentCfg runtime.Config,
	stdout, stderr io.Writer,
) bool {
	if !canStage1Materialize(citySessionProvider, cfgAgent) {
		// Pod/in-process runtime (acp/k8s/hybrid): the agent does not read
		// from the host workdir we would restage, so a host restage cannot
		// hot-reload it. Caller defers rather than cold-kills.
		return false
	}
	if err := runtime.StageSessionWorkDirWithWarnings(agentCfg, stderr); err != nil {
		fmt.Fprintf(stderr, "session reconciler: restaging workdir for hot-reload %s: %v\n", name, err) //nolint:errcheck
		return false                                                                                    // staging failed: caller defers, does not kill
	}
	// Stage-2 skill re-materialization: a per-session-worktree session
	// (WorkDir != the agent's scope root) gets its skills materialized into
	// the worktree on Start via a PreStart hook; re-stage them here so the
	// live agent sees updated skill content without a restart. Fail safe: if
	// re-materialization fails we must NOT rebaseline — advancing the stored
	// hash to "current" while the worktree still holds stale skills would mask
	// the staleness, and since no drift is detected next tick it would never
	// self-heal. Defer instead (return false); the unchanged drift retries the
	// hot-reload on the next reconcile tick.
	if shouldReMaterializeStage2Skills(citySessionProvider, cfgAgent, cfg, cityPath, agentCfg.WorkDir) {
		if err := materializeSkillsIntoWorkdir(cfg, cfgAgent, agentCfg.WorkDir, nil, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "session reconciler: re-materializing stage-2 skills for hot-reload %s: %v\n", name, err) //nolint:errcheck
			return false                                                                                                  // skills not restaged: defer, do not rebaseline (would mask staleness)
		}
	}
	if err := silentRebaselineSessionHashes(session, store, agentCfg); err != nil {
		fmt.Fprintf(stderr, "session reconciler: rebaselining hot-reload hashes for %s: %v\n", name, err) //nolint:errcheck
		return false
	}
	return true
}

// shouldReMaterializeStage2Skills reports whether the running session is a
// stage-2 (per-session-worktree) session whose skills must be re-materialized
// into its workdir during a hot-reload. It mirrors the start-time gate: the
// session runtime must be stage-2-eligible AND the session's workdir must
// differ from the agent's scope root (a scope-root session already has its
// skills delivered by stage-1 materialization, re-staged via the overlay/
// CopyFiles restage above).
func shouldReMaterializeStage2Skills(citySessionProvider string, cfgAgent *config.Agent, cfg *config.City, cityPath, workDir string) bool {
	if cfg == nil || cfgAgent == nil || workDir == "" {
		return false
	}
	if !isStage2EligibleSession(citySessionProvider, cfgAgent) {
		return false
	}
	scopeRoot := agentScopeRoot(cfgAgent, cityPath, cfg.Rigs)
	return canonicaliseFilePath(workDir, cityPath) != scopeRoot
}
