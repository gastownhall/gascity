package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// The scale_check source of the external-reads lane (I5, POOL-005): runs
// every custom scale_check pool's command on the lane's cadence, off the
// allocator pass, and publishes the counts. The allocator reads the latest
// result in memory; a command never runs inside an allocator pass.
//
// It runs the same work legacy's demand pass runs (evaluatePendingPools under
// the probe_concurrency semaphore) over the same pools: non-suspended,
// generic-ephemeral agents with a custom scale_check outside suspended rigs,
// and none while the city is suspended (the lane skips the pass). A
// city-scoped pool's count sums every active store, as the demand pass does
// (customScaleCheckFanOutProbes).
// One difference is deliberate (BEHAVIORS #38): a pool whose probe env cannot
// be built is marked partial rather than silently skipped, so its count reads
// as untrusted (retain, block create) instead of zero.

// scaleCheckResult is one scale_check run: each custom scale_check pool's
// count (additive new demand) and the pools whose count cannot be trusted.
// Immutable once published; the lane stamps when the run ended and ages it
// (externalReadsRecording.scaleCheck).
type scaleCheckResult struct {
	Counts  map[string]int
	Partial map[string]bool
}

// partial reports whether template's count cannot be trusted: no fresh run
// has published, the run had no check for template, or its check or env
// build failed. It is meaningful only for templates with a custom
// scale_check: every other template reads partial.
func (r *scaleCheckResult) partial(template string) bool {
	if r == nil || r.Partial[template] {
		return true
	}
	_, ok := r.Counts[template]
	return !ok
}

// runCustomScaleChecks runs every custom scale_check once over env.
func runCustomScaleChecks(env externalReadsEnv, runner ScaleCheckRunner, queryEnv probeEnvFunc, stderr io.Writer) *scaleCheckResult {
	work, envFailed := customScaleCheckWork(env.CityName, env.CityPath, env.Cfg, env.SuspendedRigPaths, queryEnv, stderr)
	counts, partials := evaluatePendingPoolsMapWith(env.Cfg, work, runner, stderr, nil)
	for _, template := range envFailed {
		counts[template] = 0
		partials = markScaleCheckPartialTemplate(partials, template)
	}
	return &scaleCheckResult{Counts: counts, Partial: partials}
}

// customScaleCheckWork builds the pools whose custom scale_check legacy's
// demand pass runs with a store, and names the pools whose probe env could
// not be built. A pool backing a named session runs with no probe env, as in
// legacy, and a city-scoped pool carries the probes that fan its check out
// across the active stores.
func customScaleCheckWork(
	cityName, cityPath string,
	cfg *config.City,
	suspendedRigPaths map[string]bool,
	queryEnv probeEnvFunc,
	stderr io.Writer,
) (work []poolEvalWork, envFailed []string) {
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() || strings.TrimSpace(agent.ScaleCheck) == "" {
			continue
		}
		rig := configuredRigName(cityPath, agent, cfg.Rigs)
		if rig != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rig, cfg.Rigs))] {
			continue
		}
		sp := scaleParamsForTopology(agent, config.QueryTopology{Beads: cfg.Beads})
		sp.Check = expandAgentCommandTemplate(cityPath, cityName, agent, cfg.Rigs, "scale_check", sp.Check, stderr)
		w := poolEvalWork{agentIdx: i, sp: sp, poolDir: agentCommandDir(cityPath, agent, cfg.Rigs), newDemand: true}
		if !agentBacksNamedSession(cfg, agent) {
			env, err := queryEnv(cityPath, cfg, agent)
			if err != nil {
				fmt.Fprintf(stderr, "scaleCheck: building env for %s: %v (marking partial)\n", agent.QualifiedName(), err) //nolint:errcheck
				envFailed = append(envFailed, agent.QualifiedName())
				continue
			}
			w.env = env
		}
		w.probes = customScaleCheckFanOutProbes(cityPath, cfg, agent, rig, w.poolDir, w.env, suspendedRigPaths)
		work = append(work, w)
	}
	return work, envFailed
}

// agentBacksNamedSession reports whether a named session uses agent as its
// template.
func agentBacksNamedSession(cfg *config.City, agent *config.Agent) bool {
	for j := range cfg.NamedSessions {
		if cfg.NamedSessions[j].TemplateQualifiedName() == agent.QualifiedName() {
			return true
		}
	}
	return false
}
