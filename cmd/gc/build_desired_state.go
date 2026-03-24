package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
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
// ("s-{beadID}") for discovered session beads. Configured singleton agents
// are treated as templates only; the controller no longer auto-creates a
// canonical chat session for them. When store is nil, the legacy
// SessionNameFor function is used for backward compatibility.
//
// Performs idempotent side effects on each tick: hook installation and ACP
// route registration. These are safe to repeat because hooks are installed
// to stable filesystem paths and ACP routing is idempotent.
func buildDesiredState(
	cityName, cityPath string,
	beaconTime time.Time,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	stderr io.Writer,
) map[string]TemplateParams {
	var sessionBeads *sessionBeadSnapshot
	if store != nil {
		var err error
		sessionBeads, err = loadSessionBeadSnapshot(store)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: listing session beads: %v\n", err) //nolint:errcheck
		}
	}
	return buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, cfg, sp, store, sessionBeads, stderr)
}

func buildDesiredStateWithSessionBeads(
	cityName, cityPath string,
	beaconTime time.Time,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	sessionBeads *sessionBeadSnapshot,
	stderr io.Writer,
) map[string]TemplateParams {
	if cfg.Workspace.Suspended {
		return nil
	}

	bp := newAgentBuildParams(cityName, cityPath, cfg, sp, beaconTime, store, stderr)
	bp.sessionBeads = sessionBeads

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
	eligibleTemplates := make(map[string]bool)
	realizedTemplates := make(map[string]bool)
	namedSpecs := make(map[string]namedSessionSpec)

	for i := range cfg.Agents {
		if cfg.Agents[i].Suspended {
			continue
		}

		pool := cfg.Agents[i].EffectivePool()

		if pool.Max == 0 {
			continue
		}

		rigName := configuredRigName(cityPath, &cfg.Agents[i], cfg.Rigs)
		if rigName != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rigName, cfg.Rigs))] {
			continue
		}
		eligibleTemplates[cfg.Agents[i].QualifiedName()] = true

		if pool.Max == 1 && !cfg.Agents[i].IsPool() {
			// Singleton agents are templates only. A session exists only after
			// an explicit create or other on-demand session minting path.
			continue
		}

		// Pool agent: collect for parallel scale_check.
		poolDir := agentCommandDir(cityPath, &cfg.Agents[i], cfg.Rigs)
		pendingPools = append(pendingPools, poolEvalWork{agentIdx: i, pool: pool, poolDir: poolDir})
	}

	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if agentUsesSuspendedRig(cityPath, spec.Agent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		namedSpecs[identity] = spec
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

	poolDesiredCounts := make([]int, len(pendingPools))
	for j, pw := range pendingPools {
		pr := evalResults[j]
		if pr.err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: %v (using min=%d)\n", pr.err, pw.pool.Min) //nolint:errcheck
		}
		poolDesiredCounts[j] = pr.desired
		if pr.desired > 0 {
			realizedTemplates[cfg.Agents[pw.agentIdx].QualifiedName()] = true
		}
	}

	markDiscoveredSessionTemplates(bp.sessionBeads, cityPath, cfg, desired, realizedTemplates, suspendedRigPaths)

	namedWorkReady := make(map[string]bool, len(namedSpecs))
	for identity, spec := range namedSpecs {
		if spec.Mode == "always" {
			realizedTemplates[identity] = true
			continue
		}
		wq := prefixedWorkQueryForProbe(cfg, cityName, store, bp.sessionBeads, spec.Agent)
		if wq == "" {
			continue
		}
		dir := agentCommandDir(cityPath, spec.Agent, cfg.Rigs)
		out, err := shellScaleCheck(wq, dir)
		if err != nil {
			continue
		}
		if workQueryHasReadyWork(strings.TrimSpace(out)) {
			namedWorkReady[identity] = true
			realizedTemplates[identity] = true
		}
	}

	dependencyFloors := dependencyRealizedFloors(cfg, eligibleTemplates, realizedTemplates)

	for j, pw := range pendingPools {
		desiredCount := poolDesiredCounts[j]
		floorOnlyCount := 0
		if floor := dependencyFloors[cfg.Agents[pw.agentIdx].QualifiedName()]; floor > desiredCount {
			floorOnlyCount = floor - desiredCount
			desiredCount = floor
		}
		for slot := 1; slot <= desiredCount; slot++ {
			// If single-instance (max == 1), use bare name (no suffix).
			// If multi-instance (max > 1 or unlimited), use themed name
			// (from namepool) or {name}-{N} suffix.
			name := cfg.Agents[pw.agentIdx].Name
			if pw.pool.IsMultiInstance() {
				name = poolInstanceName(cfg.Agents[pw.agentIdx].Name, slot, pw.pool)
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
			tp.DependencyOnly = slot > desiredCount-floorOnlyCount
			installAgentSideEffects(bp, &instanceAgent, tp, stderr)
			desired[tp.SessionName] = tp
		}
	}

	for identity, spec := range namedSpecs {
		_, hasCanonical := findCanonicalNamedSessionBead(bp.sessionBeads, identity)
		if !hasCanonical {
			if _, conflict := findNamedSessionConflict(bp.sessionBeads, spec); conflict {
				continue
			}
		}
		if spec.Mode != "always" && !hasCanonical && !namedWorkReady[identity] && dependencyFloors[identity] == 0 {
			continue
		}
		fpExtra := buildFingerprintExtra(spec.Agent)
		tp, err := resolveTemplate(bp, spec.Agent, identity, fpExtra)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: named session %q: %v (skipping)\n", identity, err) //nolint:errcheck
			continue
		}
		tp.Alias = identity
		tp.ConfiguredNamedIdentity = identity
		tp.ConfiguredNamedMode = spec.Mode
		installAgentSideEffects(bp, spec.Agent, tp, stderr)
		desired[tp.SessionName] = tp
	}

	// Phase 2: discover session beads created outside config iteration
	// (e.g., by "gc session new"). Include them in desired state if they
	// have a valid template and are not held/closed.
	discoverSessionBeads(bp, cfg, desired, suspendedRigPaths, stderr)

	return desired
}

func dependencyRealizedFloors(cfg *config.City, eligibleTemplates, realizedTemplates map[string]bool) map[string]int {
	floors := make(map[string]int)
	if cfg == nil || len(realizedTemplates) == 0 {
		return floors
	}
	agentsByTemplate := make(map[string]*config.Agent)
	for i := range cfg.Agents {
		agentsByTemplate[cfg.Agents[i].QualifiedName()] = &cfg.Agents[i]
	}
	visited := make(map[string]bool)
	var visit func(template string)
	visit = func(template string) {
		if visited[template] {
			return
		}
		visited[template] = true
		agent := agentsByTemplate[template]
		if agent == nil {
			return
		}
		for _, dep := range agent.DependsOn {
			if dep == "" || !eligibleTemplates[dep] || agentsByTemplate[dep] == nil {
				continue
			}
			floors[dep] = 1
			visit(dep)
		}
	}
	for template := range realizedTemplates {
		visit(template)
	}
	return floors
}

func markDiscoveredSessionTemplates(
	sessionBeads *sessionBeadSnapshot,
	cityPath string,
	cfg *config.City,
	desired map[string]TemplateParams,
	realizedTemplates map[string]bool,
	suspendedRigPaths map[string]bool,
) {
	if sessionBeads == nil || cfg == nil {
		return
	}
	for _, b := range sessionBeads.Open() {
		if b.Status == "closed" {
			continue
		}
		sn := b.Metadata["session_name"]
		if sn == "" {
			continue
		}
		if _, exists := desired[sn]; exists {
			continue
		}
		template := b.Metadata["template"]
		if template == "" {
			template = b.Metadata["common_name"]
		}
		if template == "" {
			continue
		}
		cfgAgent := findAgentByTemplate(cfg, template)
		if cfgAgent == nil {
			continue
		}
		if agentUsesSuspendedRig(cityPath, cfgAgent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		realizedTemplates[template] = true
	}
}

func isManualSessionRoot(b beads.Bead) bool {
	return b.Metadata["manual_session"] == "true" || b.Metadata["state"] == "creating"
}

func agentUsesSuspendedRig(cityPath string, cfgAgent *config.Agent, rigs []config.Rig, suspendedRigPaths map[string]bool) bool {
	if cfgAgent == nil || len(suspendedRigPaths) == 0 {
		return false
	}
	rigName := configuredRigName(cityPath, cfgAgent, rigs)
	if rigName == "" {
		return false
	}
	return suspendedRigPaths[filepath.Clean(rigRootForName(rigName, rigs))]
}

// discoverSessionBeads queries the store for open session beads that are
// not already in the desired state and adds them. This enables "gc session
// new" to create a bead that the reconciler then starts.
func discoverSessionBeads(
	bp *agentBuildParams,
	cfg *config.City,
	desired map[string]TemplateParams,
	suspendedRigPaths map[string]bool,
	stderr io.Writer,
) {
	sessionBeads := bp.sessionBeads
	if sessionBeads == nil && bp.beadStore != nil {
		var err error
		sessionBeads, err = loadSessionBeadSnapshot(bp.beadStore)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: listing session beads: %v\n", err) //nolint:errcheck
			return
		}
	}
	if sessionBeads == nil {
		return
	}
	for _, b := range sessionBeads.Open() {
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
		if agentUsesSuspendedRig(bp.cityPath, cfgAgent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		// Pool agents: respect the pool's scaling decision for config-managed
		// slots, but keep manual session roots discoverable even when the pool
		// currently wants 0 instances. Manual roots come from `gc session new`
		// and intentionally bypass scale checks.
		if cfgAgent.Pool != nil {
			templateHasDesired := false
			for _, existing := range desired {
				if existing.TemplateName == template {
					templateHasDesired = true
					break
				}
			}
			if !templateHasDesired && !isManualSessionRoot(b) {
				continue
			}
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
		tp.ManualSession = isManualSessionRoot(b)
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

// poolInstanceName returns the name for pool slot N.
// If the pool has namepool names and the slot is in range, uses the themed
// name. Otherwise falls back to "{base}-{slot}".
func poolInstanceName(base string, slot int, pool config.PoolConfig) string {
	if slot >= 1 && slot <= len(pool.NamepoolNames) {
		return pool.NamepoolNames[slot-1]
	}
	return fmt.Sprintf("%s-%d", base, slot)
}
