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

// DesiredStateResult bundles the desired session state with the scale_check
// counts that produced it. Callers that need poolDesired for wake decisions
// can pass ScaleCheckCounts to ComputePoolDesiredStates without re-running
// scale_check commands.
type DesiredStateResult struct {
	State            map[string]TemplateParams
	ScaleCheckCounts map[string]int // nil when store is nil or scale_check not run
}

type poolEvalWork struct {
	agentIdx int
	pool     config.PoolConfig
	poolDir  string
}

func evaluatePendingPools(
	cfg *config.City,
	pendingPools []poolEvalWork,
	stderr io.Writer,
) []int {
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

	counts := make([]int, len(pendingPools))
	for j, pw := range pendingPools {
		pr := evalResults[j]
		if pr.err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: %v (using min=%d)\n", pr.err, pw.pool.Min) //nolint:errcheck
		}
		counts[j] = pr.desired
	}
	return counts
}

// evaluatePendingPoolsMap is like evaluatePendingPools but returns a map
// from agent qualified name → desired count. Used to feed scale_check
// results into ComputePoolDesiredStates.
func evaluatePendingPoolsMap(
	cfg *config.City,
	pendingPools []poolEvalWork,
	stderr io.Writer,
) map[string]int {
	counts := evaluatePendingPools(cfg, pendingPools, stderr)
	m := make(map[string]int, len(counts))
	for j, pw := range pendingPools {
		m[cfg.Agents[pw.agentIdx].QualifiedName()] = counts[j]
	}
	return m
}

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
) DesiredStateResult {
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
) DesiredStateResult {
	if cfg.Workspace.Suspended {
		return DesiredStateResult{}
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
			rigName := configuredRigName(cityPath, &cfg.Agents[i], cfg.Rigs)
			if rigName != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rigName, cfg.Rigs))] {
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

		rigName := configuredRigName(cityPath, &cfg.Agents[i], cfg.Rigs)
		if rigName != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rigName, cfg.Rigs))] {
			continue
		}
		// Pool agent: collect scale-check inputs. Legacy no-store mode uses
		// them directly; bead-backed mode falls back to them when work-bead
		// listing fails so transient store errors do not collapse demand to 0.
		poolDir := agentCommandDir(cityPath, &cfg.Agents[i], cfg.Rigs)
		pendingPools = append(pendingPools, poolEvalWork{agentIdx: i, pool: pool, poolDir: poolDir})
	}

	// scale_check runs in parallel for all pool agents — the authoritative
	// demand signal for new sessions. Computed once, returned in result.
	scaleCheckCounts := evaluatePendingPoolsMap(cfg, pendingPools, stderr)

	if store != nil {
		// Cross-reference: for each non-closed session, query its scope's
		// store for work beads assigned to that session (in-progress + open).
		assignedWorkBeads := collectAssignedWorkBeads(cfg, cityPath, store, suspendedRigPaths)
		poolDesiredStates := ComputePoolDesiredStates(cfg, assignedWorkBeads, sessionBeads.Open(), scaleCheckCounts)
		for _, poolState := range poolDesiredStates {
			cfgAgent := findAgentByTemplate(cfg, poolState.Template)
			if cfgAgent == nil {
				continue
			}
			if agentInSuspendedRig(cityPath, cfgAgent, cfg.Rigs, suspendedRigPaths) {
				continue
			}
			realizePoolDesiredSessions(bp, cfgAgent, poolState, desired, stderr)
		}
	} else {
		// No store — use scale_check counts directly.
		for _, pw := range pendingPools {
			desiredCount := scaleCheckCounts[cfg.Agents[pw.agentIdx].QualifiedName()]
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
				installAgentSideEffects(bp, &instanceAgent, tp, stderr)
				desired[tp.SessionName] = tp
			}
		}
	}

	// Phase 2: discover session beads created outside config iteration
	// (e.g., by "gc session new"). Include them in desired state if they
	// have a valid template and are not held/closed.
	realizedRoots := discoverSessionBeadsWithRoots(bp, cfg, desired, suspendedRigPaths, stderr)
	realizeDependencyFloors(bp, cfg, desired, realizedRoots, suspendedRigPaths, stderr)

	return DesiredStateResult{State: desired, ScaleCheckCounts: scaleCheckCounts}
}

// collectAssignedWorkBeads queries each store (city + rigs) for work beads
// that have an assignee. Two queries per store: bd list --status=in_progress
// and bd ready. Results are filtered to only beads with a non-empty assignee.
// The caller cross-references these with session beads to determine which
// sessions have active work and must stay alive.
func collectAssignedWorkBeads(
	cfg *config.City,
	cityPath string,
	cityStore beads.Store,
	suspendedRigPaths map[string]bool,
) []beads.Bead {
	// Gather stores: city + each non-suspended rig.
	stores := []beads.Store{cityStore}
	for _, rig := range cfg.Rigs {
		if suspendedRigPaths[filepath.Clean(rig.Path)] {
			continue
		}
		stores = append(stores, bdStoreForCity(rig.Path, cityPath))
	}

	var result []beads.Bead
	for _, s := range stores {
		// In-progress beads with an assignee (active work).
		if inProgress, err := s.List("in_progress"); err == nil {
			appendAssigned(&result, inProgress)
		}
		// Ready beads with an assignee (claimed but not started).
		if ready, err := s.Ready(); err == nil {
			appendAssigned(&result, ready)
		}
	}
	return result
}

func appendAssigned(dst *[]beads.Bead, beadList []beads.Bead) {
	for _, b := range beadList {
		if strings.TrimSpace(b.Assignee) != "" {
			*dst = append(*dst, b)
		}
	}
}

func workflowControlOnlyConfig(cfg *config.City) *config.City {
	if cfg == nil {
		return nil
	}
	// Only include city-scoped workflow-control. Rig-scoped instances are
	// handled by the main reconcile loop which has per-rig stores.
	agentCfg, ok := resolveAgentIdentity(cfg, config.WorkflowControlAgentName, "")
	if !ok {
		return nil
	}
	cfgCopy := *cfg
	cfgCopy.Agents = []config.Agent{agentCfg}
	return &cfgCopy
}

// discoverSessionBeads queries the store for open session beads that are
// not already in the desired state and adds them. This enables "gc session
// new" to create a bead that the reconciler then starts.
func discoverSessionBeads(
	bp *agentBuildParams,
	cfg *config.City,
	desired map[string]TemplateParams,
	stderr io.Writer,
) {
	discoverSessionBeadsWithRoots(bp, cfg, desired, nil, stderr)
}

func discoverSessionBeadsWithRoots(
	bp *agentBuildParams,
	cfg *config.City,
	desired map[string]TemplateParams,
	suspendedRigPaths map[string]bool,
	stderr io.Writer,
) map[string]bool {
	sessionBeads := bp.sessionBeads
	if sessionBeads == nil && bp.beadStore != nil {
		var err error
		sessionBeads, err = loadSessionBeadSnapshot(bp.beadStore)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: listing session beads: %v\n", err) //nolint:errcheck
			return nil
		}
	}
	if sessionBeads == nil {
		return nil
	}
	roots := make(map[string]bool)
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
		if agentInSuspendedRig(bp.cityPath, cfgAgent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		roots[template] = true
		// Pool agents: respect the pool's scaling decision. If the main
		// config iteration (which ran evaluatePool / scale_check) did not
		// produce any desired entries for this template, the pool wants 0
		// instances. Don't re-add stale session beads — that bypasses
		// scaling and causes infinite wake→drain→stop loops when there's
		// no work.
		if cfgAgent.Pool != nil {
			manualSession := b.Metadata["manual_session"] == "true"
			if !manualSession && !desiredHasTemplate(desired, template) {
				continue
			}
		}
		// Resolve TemplateParams for this bead's session.
		fpExtra := buildFingerprintExtra(cfgAgent)
		tp, err := resolveTemplateForSessionBead(bp, cfgAgent, cfgAgent.QualifiedName(), fpExtra, b)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: bead %s template %q: %v (skipping)\n", b.ID, template, err) //nolint:errcheck
			continue
		}
		tp.ManualSession = b.Metadata["manual_session"] == "true"
		if cfgAgent.Pool != nil {
			tp.Alias = ""
			tp.InstanceName = sn
		}
		installAgentSideEffects(bp, cfgAgent, tp, stderr)
		desired[sn] = tp
	}
	return roots
}

func realizeDependencyFloors(
	bp *agentBuildParams,
	cfg *config.City,
	desired map[string]TemplateParams,
	roots map[string]bool,
	suspendedRigPaths map[string]bool,
	stderr io.Writer,
) {
	if cfg == nil || len(roots) == 0 {
		return
	}
	visited := make(map[string]bool)
	var visit func(string)
	visit = func(template string) {
		if template == "" || visited[template] {
			return
		}
		visited[template] = true
		agent := findAgentByTemplate(cfg, template)
		if agent == nil {
			return
		}
		for _, dep := range agent.DependsOn {
			depAgent := findAgentByTemplate(cfg, dep)
			if depAgent == nil || depAgent.Suspended {
				continue
			}
			if agentInSuspendedRig(bp.cityPath, depAgent, cfg.Rigs, suspendedRigPaths) {
				continue
			}
			ensureDependencyOnlyTemplate(bp, depAgent, desired, stderr)
			visit(dep)
		}
	}
	for template := range roots {
		visit(template)
	}
}

func ensureDependencyOnlyTemplate(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	desired map[string]TemplateParams,
	stderr io.Writer,
) {
	if cfgAgent == nil || cfgAgent.Pool == nil || desiredHasTemplate(desired, cfgAgent.QualifiedName()) {
		return
	}

	if bp.beadStore == nil {
		name := cfgAgent.Name
		if cfgAgent.Pool.IsMultiInstance() {
			name = poolInstanceName(cfgAgent.Name, 1, *cfgAgent.Pool)
		}
		qualifiedInstance := name
		if cfgAgent.Dir != "" {
			qualifiedInstance = cfgAgent.Dir + "/" + name
		}
		instanceAgent := deepCopyAgent(cfgAgent, name, cfgAgent.Dir)
		fpExtra := buildFingerprintExtra(&instanceAgent)
		tp, err := resolveTemplate(bp, &instanceAgent, qualifiedInstance, fpExtra)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedInstance, err) //nolint:errcheck
			return
		}
		tp.DependencyOnly = true
		installAgentSideEffects(bp, &instanceAgent, tp, stderr)
		desired[tp.SessionName] = tp
		return
	}

	qualifiedName := cfgAgent.QualifiedName()
	sessionBead, err := selectOrCreateDependencyPoolSessionBead(bp, qualifiedName)
	if err != nil {
		fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
		return
	}
	fpExtra := buildFingerprintExtra(cfgAgent)
	tp, err := resolveTemplateForSessionBead(bp, cfgAgent, qualifiedName, fpExtra, sessionBead)
	if err != nil {
		fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
		return
	}
	tp.Alias = ""
	tp.InstanceName = sessionBead.Metadata["session_name"]
	tp.DependencyOnly = true
	installAgentSideEffects(bp, cfgAgent, tp, stderr)
	desired[tp.SessionName] = tp
}

func desiredHasTemplate(desired map[string]TemplateParams, template string) bool {
	for _, existing := range desired {
		if existing.TemplateName == template {
			return true
		}
	}
	return false
}

func realizePoolDesiredSessions(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	poolState PoolDesiredState,
	desired map[string]TemplateParams,
	stderr io.Writer,
) {
	qualifiedName := cfgAgent.QualifiedName()
	fpExtra := buildFingerprintExtra(cfgAgent)
	used := make(map[string]bool)
	for _, request := range poolState.Requests {
		var prefer *beads.Bead
		if request.SessionBeadID != "" {
			if bead, ok := findOpenSessionBeadByID(bp.sessionBeads, request.SessionBeadID); ok {
				prefer = &bead
			}
		}
		sessionBead, err := selectOrCreatePoolSessionBead(bp, qualifiedName, prefer, used)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
			continue
		}
		if used[sessionBead.ID] {
			continue
		}
		used[sessionBead.ID] = true
		tp, err := resolveTemplateForSessionBead(bp, cfgAgent, qualifiedName, fpExtra, sessionBead)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: pool %q session %s: %v (skipping)\n", qualifiedName, sessionBead.ID, err) //nolint:errcheck
			continue
		}
		tp.Alias = ""
		tp.InstanceName = sessionBead.Metadata["session_name"]
		installAgentSideEffects(bp, cfgAgent, tp, stderr)
		desired[tp.SessionName] = tp
	}
}

func resolveTemplateForSessionBead(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	qualifiedName string,
	fpExtra map[string]string,
	sessionBead beads.Bead,
) (TemplateParams, error) {
	local := *bp
	local.beadNames = map[string]string{qualifiedName: sessionBead.Metadata["session_name"]}
	return resolveTemplate(&local, cfgAgent, qualifiedName, fpExtra)
}

func findOpenSessionBeadByID(sessionBeads *sessionBeadSnapshot, id string) (beads.Bead, bool) {
	if sessionBeads == nil || id == "" {
		return beads.Bead{}, false
	}
	for _, bead := range sessionBeads.Open() {
		if bead.ID == id {
			return bead, true
		}
	}
	return beads.Bead{}, false
}

func selectOrCreatePoolSessionBead(
	bp *agentBuildParams,
	template string,
	preferred *beads.Bead,
	used map[string]bool,
) (beads.Bead, error) {
	// Resume tier: reuse the session that has in-progress work assigned.
	if preferred != nil && preferred.ID != "" && !used[preferred.ID] {
		return *preferred, nil
	}
	// Reuse an existing active session bead. Skip drained (dormant,
	// only revived via resume tier) and closed (terminal).
	for _, bead := range bp.sessionBeads.Open() {
		if bead.Status == "closed" {
			continue
		}
		state := bead.Metadata["state"]
		if state == "drained" {
			continue
		}
		if bead.Metadata["manual_session"] == boolMetadata(true) {
			continue
		}
		if isNamedSessionBead(bead) {
			continue
		}
		if used[bead.ID] {
			continue
		}
		if normalizedSessionTemplate(bead, &config.City{Agents: bp.agents}) != template {
			continue
		}
		if desiredName := strings.TrimSpace(bead.Metadata["session_name"]); desiredName != "" {
			return bead, nil
		}
	}
	return createPoolSessionBead(bp.beadStore, template, bp.sessionBeads)
}

func selectOrCreateDependencyPoolSessionBead(
	bp *agentBuildParams,
	template string,
) (beads.Bead, error) {
	for _, bead := range bp.sessionBeads.Open() {
		if bead.Status == "closed" || bead.Metadata["manual_session"] == boolMetadata(true) {
			continue
		}
		if isNamedSessionBead(bead) {
			continue
		}
		if bead.Metadata["dependency_only"] != boolMetadata(true) {
			continue
		}
		if normalizedSessionTemplate(bead, &config.City{Agents: bp.agents}) != template {
			continue
		}
		if desiredName := strings.TrimSpace(bead.Metadata["session_name"]); desiredName != "" {
			return bead, nil
		}
	}
	return createPoolSessionBead(bp.beadStore, template, bp.sessionBeads)
}

func agentInSuspendedRig(
	cityPath string,
	cfgAgent *config.Agent,
	rigs []config.Rig,
	suspendedRigPaths map[string]bool,
) bool {
	rigName := configuredRigName(cityPath, cfgAgent, rigs)
	if rigName == "" {
		return false
	}
	return suspendedRigPaths[filepath.Clean(rigRootForName(rigName, rigs))]
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
