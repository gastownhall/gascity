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
	State             map[string]TemplateParams
	ScaleCheckCounts  map[string]int // nil when store is nil or scale_check not run
	AssignedWorkBeads []beads.Bead   // work beads with non-empty Assignee
}

type poolEvalWork struct {
	agentIdx int
	sp       scaleParams
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
		// For rig-scoped agents with an external Dolt server, prefix
		// the pool check command with BEADS_DOLT_PORT so bd connects
		// to the correct server (matching computeWorkSet behaviour).
		sp := pw.sp
		if dir := cfg.Agents[pw.agentIdx].Dir; dir != "" {
			for _, r := range cfg.Rigs {
				if r.Name == dir && r.DoltPort != "" {
					sp.Check = "BEADS_DOLT_PORT=" + r.DoltPort + " " + sp.Check
					break
				}
			}
		}
		go func(idx int, name string, sp scaleParams, dir string) {
			defer wg.Done()
			d, err := evaluatePool(name, sp, dir, shellScaleCheck)
			evalResults[idx] = poolEvalResult{desired: d, err: err}
		}(j, cfg.Agents[pw.agentIdx].Name, sp, pw.poolDir)
	}
	wg.Wait()

	counts := make([]int, len(pendingPools))
	for j, pw := range pendingPools {
		pr := evalResults[j]
		if pr.err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: %v (using min=%d)\n", pr.err, pw.sp.Min) //nolint:errcheck
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
	return buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, cfg, sp, store, nil, sessionBeads, stderr)
}

func buildDesiredStateWithSessionBeads(
	cityName, cityPath string,
	beaconTime time.Time,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	rigStores map[string]beads.Store,
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

		sp := scaleParamsFor(&cfg.Agents[i])

		if sp.Max == 0 {
			continue
		}

		isExplicitMultiSession := cfg.Agents[i].MaxActiveSessions != nil && *cfg.Agents[i].MaxActiveSessions != 1
		if sp.Max == 1 && !isExplicitMultiSession {
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
		pendingPools = append(pendingPools, poolEvalWork{agentIdx: i, sp: sp, poolDir: poolDir})
	}

	// scale_check runs in parallel for all pool agents — the authoritative
	// demand signal for new sessions. Computed once, returned in result.
	scaleCheckCounts := evaluatePendingPoolsMap(cfg, pendingPools, stderr)

	// Collect work beads with assignees — used for both pool demand and
	// named session on_demand wake. Hoisted out of the store block so
	// the named session section can also use it.
	var assignedWorkBeads []beads.Bead
	if store != nil {
		assignedWorkBeads = collectAssignedWorkBeads(cfg, store, rigStores, suspendedRigPaths)
		if len(assignedWorkBeads) > 0 {
			fmt.Fprintf(stderr, "assignedWorkBeads: %d beads found\n", len(assignedWorkBeads)) //nolint:errcheck
			for _, wb := range assignedWorkBeads {
				fmt.Fprintf(stderr, "  %s assignee=%s routed=%s status=%s\n", wb.ID, wb.Assignee, wb.Metadata["gc.routed_to"], wb.Status) //nolint:errcheck
			}
		} else {
			fmt.Fprintf(stderr, "assignedWorkBeads: 0 beads (rigStores=%d)\n", len(rigStores)) //nolint:errcheck
		}
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
				isMultiInstance := pw.sp.Max != 1
				if isMultiInstance {
					name = poolInstanceName(cfg.Agents[pw.agentIdx].Name, slot, &cfg.Agents[pw.agentIdx])
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

	// Named sessions: materialize session beads for configured [[named_session]]
	// entries. "always" mode sessions are unconditionally materialized; "on_demand"
	// sessions are materialized only when they already have a canonical bead or
	// when their work query returns ready work.
	namedSpecs := make(map[string]namedSessionSpec)
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if agentInSuspendedRig(cityPath, spec.Agent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		namedSpecs[identity] = spec
	}
	namedWorkReady := make(map[string]bool, len(namedSpecs))
	// Check assigned work beads: if any work bead's Assignee matches a
	// named session's identity (alias), that session has demand.
	// This handles cross-agent handoff (e.g., polecat assigns work to
	// refinery by setting Assignee to the refinery's alias).
	for identity := range namedSpecs {
		for _, wb := range assignedWorkBeads {
			if strings.TrimSpace(wb.Assignee) == identity && (wb.Status == "open" || wb.Status == "in_progress") {
				fmt.Fprintf(stderr, "namedWorkReady: %s matched by bead %s (assignee=%s status=%s)\n", identity, wb.ID, wb.Assignee, wb.Status) //nolint:errcheck
				namedWorkReady[identity] = true
				break
			}
		}
	}
	if len(assignedWorkBeads) > 0 {
		fmt.Fprintf(stderr, "namedWorkReady: %d assigned beads, %d named specs, ready=%v\n", len(assignedWorkBeads), len(namedSpecs), namedWorkReady) //nolint:errcheck
	}
	for identity, spec := range namedSpecs {
		if spec.Mode == "always" || namedWorkReady[identity] {
			continue
		}
		wq := spec.Agent.EffectiveWorkQuery()
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
		}
	}
	for identity, spec := range namedSpecs {
		canonicalBead, hasCanonical := findCanonicalNamedSessionBead(bp.sessionBeads, identity)
		if !hasCanonical {
			if _, conflict := findNamedSessionConflict(bp.sessionBeads, spec); conflict {
				continue
			}
		}
		if spec.Mode != "always" && !hasCanonical && !namedWorkReady[identity] {
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
		// When a canonical bead exists, use ITS session_name as the
		// desiredState key so syncSessionBeads finds it in bySessionName
		// and takes the UPDATE path. Without this, resolveSessionName
		// might find a different (leaked) bead and produce a mismatched
		// key, sending the canonical bead through the CREATE path where
		// the alias check fails against itself.
		if hasCanonical {
			if sn := strings.TrimSpace(canonicalBead.Metadata["session_name"]); sn != "" {
				tp.SessionName = sn
			}
		}
		installAgentSideEffects(bp, spec.Agent, tp, stderr)
		desired[tp.SessionName] = tp
	}

	// Phase 2: discover session beads created outside config iteration
	// (e.g., by "gc session new"). Include them in desired state if they
	// have a valid template and are not held/closed.
	realizedRoots := discoverSessionBeadsWithRoots(bp, cfg, desired, suspendedRigPaths, stderr)
	realizeDependencyFloors(bp, cfg, desired, realizedRoots, suspendedRigPaths, stderr)

	return DesiredStateResult{State: desired, ScaleCheckCounts: scaleCheckCounts, AssignedWorkBeads: assignedWorkBeads}
}

// collectAssignedWorkBeads queries each store (city + rigs) for work beads
// that have an assignee. Two queries per store: bd list --status=in_progress
// and bd ready. Results are filtered to only beads with a non-empty assignee.
// The caller cross-references these with session beads to determine which
// sessions have active work and must stay alive.
func collectAssignedWorkBeads(
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
) []beads.Bead {
	// Use CachingStore-wrapped stores. Creating raw bdStoreForCity per rig
	// spawns bd subprocesses on every tick, saturating dolt.
	stores := []beads.Store{cityStore}
	for _, rig := range cfg.Rigs {
		if suspendedRigPaths[filepath.Clean(rig.Path)] {
			continue
		}
		if s, ok := rigStores[rig.Name]; ok {
			stores = append(stores, s)
		}
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

func controlDispatcherOnlyConfig(cfg *config.City) *config.City {
	if cfg == nil {
		return nil
	}
	// Only include city-scoped control-dispatcher. Rig-scoped instances are
	// handled by the main reconcile loop which has per-rig stores.
	agentCfg, ok := resolveAgentIdentity(cfg, config.ControlDispatcherAgentName, "")
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
		if isMultiSessionCfgAgent(cfgAgent) {
			manualSession := b.Metadata["manual_session"] == "true"
			creating := b.Metadata["state"] == "creating"
			if isPoolManagedSessionBead(b) && !manualSession && !isNamedSessionBead(b) && !creating {
				continue
			}
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
		if isMultiSessionCfgAgent(cfgAgent) {
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
	if cfgAgent == nil || !isMultiSessionCfgAgent(cfgAgent) || desiredHasTemplate(desired, cfgAgent.QualifiedName()) {
		return
	}

	if bp.beadStore == nil {
		name := cfgAgent.Name
		sp := scaleParamsFor(cfgAgent)
		isMultiInstance := sp.Max != 1
		if isMultiInstance {
			name = poolInstanceName(cfgAgent.Name, 1, cfgAgent)
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
	sessionBead, err := selectOrCreateDependencyPoolSessionBead(bp, cfgAgent, qualifiedName)
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
	// Reuse an existing active/creating session bead. Skip drained, closed,
	// and asleep — asleep ephemerals are not restarted; a fresh session is
	// created instead. The reconciler closes orphaned asleep beads.
	for _, bead := range bp.sessionBeads.Open() {
		if bead.Status == "closed" {
			continue
		}
		if isDrainedSessionBead(bead) {
			continue
		}
		if bead.Metadata["state"] == "asleep" {
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
	_ *config.Agent,
	template string,
) (beads.Bead, error) {
	for _, bead := range bp.sessionBeads.Open() {
		if bead.Status == "closed" || bead.Metadata["manual_session"] == boolMetadata(true) {
			continue
		}
		if isDrainedSessionBead(bead) {
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

// isMultiSessionCfgAgent reports whether a config agent supports multiple
// concurrent sessions. This replaces the removed IsPool() / Pool != nil checks.
func isMultiSessionCfgAgent(a *config.Agent) bool {
	if a == nil {
		return false
	}
	maxSess := a.EffectiveMaxActiveSessions()
	return maxSess == nil || *maxSess != 1
}

// poolInstanceName returns the name for pool slot N.
// If the agent has namepool names and the slot is in range, uses the themed
// name. Otherwise falls back to "{base}-{slot}".
func poolInstanceName(base string, slot int, a *config.Agent) string {
	if a != nil && slot >= 1 && slot <= len(a.NamepoolNames) {
		return a.NamepoolNames[slot-1]
	}
	return fmt.Sprintf("%s-%d", base, slot)
}
