package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
)

// buildDesiredState computes the desired session state from config,
// returning sessionName → TemplateParams. This is the canonical path
// for constructing the desired agent set — both reconcilers use it.
//
// When store is non-nil, session names are derived from bead IDs
// ("s-{beadID}") and session beads are auto-created for configured agents
// that don't have them yet. When store is nil, the legacy SessionNameFor
// function is used for backward compatibility.
//
// Performs idempotent side effects on each tick: hook installation,
// ACP route registration, and session bead auto-creation. These are safe
// to repeat because hooks are installed to stable filesystem paths,
// ACP routing is idempotent, and bead creation is deduplicated by template.
func buildDesiredState(
	cityName, cityPath string,
	beaconTime time.Time,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	stderr io.Writer,
) map[string]TemplateParams {
	if cfg.Workspace.Suspended {
		return nil
	}

	bp := newAgentBuildParams(cityName, cityPath, cfg, sp, beaconTime, store, stderr)

	// Pre-compute suspended rig paths.
	suspendedRigPaths := make(map[string]bool)
	for _, r := range cfg.Rigs {
		if r.Suspended {
			suspendedRigPaths[filepath.Clean(r.Path)] = true
		}
	}

	type poolEvalWork struct {
		agentIdx int
		pool     config.PoolConfig
		poolDir  string
	}

	desired := make(map[string]TemplateParams)
	var pendingPools []poolEvalWork

	for i := range cfg.Agents {
		if cfg.Agents[i].Suspended {
			continue
		}

		pool := cfg.Agents[i].EffectivePool()

		if pool.Max == 0 {
			continue
		}

		if pool.Max == 1 && !cfg.Agents[i].IsPool() {
			// Fixed agent.
			expandedDir := expandDirTemplate(cfg.Agents[i].Dir, SessionSetupContext{
				Agent:    cfg.Agents[i].QualifiedName(),
				Rig:      cfg.Agents[i].Dir,
				CityRoot: cityPath,
				CityName: cityName,
			})
			workDir, err := resolveAgentDir(cityPath, expandedDir)
			if err != nil {
				fmt.Fprintf(stderr, "buildDesiredState: agent %q: %v (skipping)\n", cfg.Agents[i].QualifiedName(), err) //nolint:errcheck
				continue
			}
			if suspendedRigPaths[filepath.Clean(workDir)] {
				continue
			}

			fpExtra := buildFingerprintExtra(&cfg.Agents[i])
			tp, err := resolveTemplate(bp, &cfg.Agents[i], cfg.Agents[i].QualifiedName(), fpExtra)
			if err != nil {
				fmt.Fprintf(stderr, "buildDesiredState: %v (skipping)\n", err) //nolint:errcheck
				continue
			}
			installAgentSideEffects(bp, &cfg.Agents[i], tp, stderr)
			desired[tp.SessionName] = tp
			continue
		}

		// Pool agent: collect for parallel scale_check.
		if cfg.Agents[i].Dir != "" {
			poolDir, pdErr := resolveAgentDir(cityPath, cfg.Agents[i].Dir)
			if pdErr == nil && suspendedRigPaths[filepath.Clean(poolDir)] {
				continue
			}
		}
		poolDir := cityPath
		if cfg.Agents[i].Dir != "" {
			if pd, pdErr := resolveAgentDir(cityPath, cfg.Agents[i].Dir); pdErr == nil {
				poolDir = pd
			}
		}
		pendingPools = append(pendingPools, poolEvalWork{agentIdx: i, pool: pool, poolDir: poolDir})
	}

	// Parallel scale_check evaluation for pools.
	type poolEvalResult struct {
		desired int
		err     error
	}
	evalResults := make([]poolEvalResult, len(pendingPools))
	var wg sync.WaitGroup
	for j, pw := range pendingPools {
		wg.Add(1)
		go func(idx int, name string, pool config.PoolConfig, dir string) {
			defer wg.Done()
			d, err := evaluatePool(name, pool, dir, shellScaleCheck)
			evalResults[idx] = poolEvalResult{desired: d, err: err}
		}(j, cfg.Agents[pw.agentIdx].Name, pw.pool, pw.poolDir)
	}
	wg.Wait()

	for j, pw := range pendingPools {
		pr := evalResults[j]
		if pr.err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: %v (using min=%d)\n", pr.err, pw.pool.Min) //nolint:errcheck
		}
		for slot := 1; slot <= pr.desired; slot++ {
			// If single-instance (max == 1), use bare name (no suffix).
			// If multi-instance (max > 1 or unlimited), use {name}-{N} suffix.
			// This matches the old poolAgents behavior and preserves session
			// name continuity for singleton pools.
			name := cfg.Agents[pw.agentIdx].Name
			if pw.pool.IsMultiInstance() {
				name = fmt.Sprintf("%s-%d", cfg.Agents[pw.agentIdx].Name, slot)
			}
			qualifiedInstance := name
			if cfg.Agents[pw.agentIdx].Dir != "" {
				qualifiedInstance = cfg.Agents[pw.agentIdx].Dir + "/" + name
			}
			instanceAgent := deepCopyAgent(&cfg.Agents[pw.agentIdx], name, cfg.Agents[pw.agentIdx].Dir)
			fpExtra := buildFingerprintExtra(&instanceAgent)
			tp, err := resolveTemplate(bp, &instanceAgent, qualifiedInstance, fpExtra)
			if err != nil {
				fmt.Fprintf(stderr, "buildDesiredState: pool instance %q: %v (skipping)\n", qualifiedInstance, err) //nolint:errcheck
				continue
			}
			installAgentSideEffects(bp, &instanceAgent, tp, stderr)
			desired[tp.SessionName] = tp
		}
	}

	// Phase 2: discover session beads created outside config iteration
	// (e.g., by "gc session new"). Include them in desired state if they
	// have a valid template and are not held/closed.
	discoverSessionBeads(bp, cfg, store, desired, stderr)

	return desired
}

// discoverSessionBeads queries the store for open session beads that are
// not already in the desired state and adds them. This enables "gc session
// new" to create a bead that the reconciler then starts.
func discoverSessionBeads(
	bp *agentBuildParams,
	cfg *config.City,
	store beads.Store,
	desired map[string]TemplateParams,
	stderr io.Writer,
) {
	if store == nil {
		return
	}
	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		fmt.Fprintf(stderr, "buildDesiredState: listing session beads: %v\n", err) //nolint:errcheck
		return
	}
	for _, b := range all {
		if b.Status == "closed" {
			continue
		}
		sn := b.Metadata["session_name"]
		if sn == "" {
			continue
		}
		// Skip beads already in desired state (from config iteration).
		if _, exists := desired[sn]; exists {
			continue
		}
		// Skip held beads — the reconciler's wakeReasons handles held_until,
		// but we still need the bead in desired state so the reconciler
		// doesn't classify it as orphaned. Only skip if we can't resolve
		// the template.
		template := b.Metadata["template"]
		if template == "" {
			template = b.Metadata["common_name"]
		}
		if template == "" {
			continue
		}
		// Find the config agent for this template.
		cfgAgent := findAgentByTemplate(cfg, template)
		if cfgAgent == nil {
			continue
		}
		// Resolve TemplateParams for this bead's session.
		fpExtra := buildFingerprintExtra(cfgAgent)
		tp, err := resolveTemplate(bp, cfgAgent, cfgAgent.QualifiedName(), fpExtra)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: bead %s template %q: %v (skipping)\n", b.ID, template, err) //nolint:errcheck
			continue
		}
		// Override the session name with the bead-derived name.
		// Also update GC_SESSION_NAME in the env so each fork gets its
		// own session identity in the config fingerprint. Without this,
		// forks inherit the primary session's name from resolveSessionName
		// cache, causing spurious config-drift when the cache changes.
		tp.SessionName = sn
		if tp.Env == nil {
			tp.Env = make(map[string]string)
		}
		tp.Env["GC_SESSION_NAME"] = sn
		installAgentSideEffects(bp, cfgAgent, tp, stderr)
		desired[sn] = tp
	}
}

// installAgentSideEffects performs idempotent side effects for a resolved
// agent: hook installation and ACP route registration. Called from
// buildDesiredState on every tick; safe to repeat.
func installAgentSideEffects(bp *agentBuildParams, cfgAgent *config.Agent, tp TemplateParams, stderr io.Writer) {
	// Install provider hooks (idempotent filesystem side effect).
	if ih := config.ResolveInstallHooks(cfgAgent, bp.workspace); len(ih) > 0 {
		if hErr := hooks.Install(bp.fs, bp.cityPath, tp.WorkDir, ih); hErr != nil {
			fmt.Fprintf(stderr, "agent %q: hooks: %v\n", tp.DisplayName(), hErr) //nolint:errcheck
		}
	}
	// Register ACP route on the auto provider for dynamic sessions.
	if tp.IsACP {
		if autoSP, ok := bp.sp.(*sessionauto.Provider); ok {
			autoSP.RouteACP(tp.SessionName)
		}
	}
}
