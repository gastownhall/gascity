package main

import (
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strconv"
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
	State              map[string]TemplateParams
	BaseState          map[string]TemplateParams
	ScaleCheckCounts   map[string]int // nil when store is nil or scale_check not run
	AssignedWorkBeads  []beads.Bead   // actionable assigned work: in_progress or ready+assigned
	OwnershipWorkBeads []beads.Bead   // all assigned work that preserves session ownership: open or in_progress
	// NamedSessionDemand records which named-session identities have active
	// demand — either direct assignee demand (Assignee == identity) or
	// work_query-detected ready work. The reconciler merges this into
	// poolDesired so that on-demand named sessions remain config-eligible
	// even when no gc.routed_to metadata exists for the template.
	NamedSessionDemand map[string]bool
	// StoreQueryPartial is true when one or more bead store queries failed
	// during assigned-work snapshot collection. When set, the reconciler must NOT
	// drain sessions based on the (incomplete) desired state — a transient
	// store failure would cause running sessions to be falsely orphaned
	// and interrupted via Ctrl-C.
	StoreQueryPartial bool
	BeaconTime        time.Time
}

type poolEvalWork struct {
	agentIdx int
	sp       scaleParams
	poolDir  string
}

func evaluatePendingPools(
	cityPath string,
	cfg *config.City,
	pendingPools []poolEvalWork,
	stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
) []int {
	type poolEvalResult struct {
		desired int
		err     error
	}
	evalResults := make([]poolEvalResult, len(pendingPools))
	// Bound per-pool scale_check concurrency so bd subprocess probes
	// don't stampede the shared dolt sql-server. Without this, ~40+
	// pool agents launching goroutines in parallel causes per-call
	// contention that pushes individual probes past their timeout.
	sem := make(chan struct{}, cfg.Daemon.ProbeConcurrencyOrDefault())
	var wg sync.WaitGroup
	for j, pw := range pendingPools {
		wg.Add(1)
		sp := pw.sp
		sp.Check = prefixControllerQueryEnv(cityPath, cfg, &cfg.Agents[pw.agentIdx], sp.Check)
		template := cfg.Agents[pw.agentIdx].QualifiedName()
		agentName := cfg.Agents[pw.agentIdx].Name
		agentIndex := pw.agentIdx
		go func(idx int, template, agentName string, agentIndex int, sp scaleParams, dir string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			started := time.Now()
			d, err := evaluatePool(agentName, sp, dir, shellScaleCheck)
			evalResults[idx] = poolEvalResult{desired: d, err: err}
			if trace != nil {
				outcome := "success"
				if err != nil {
					outcome = "failed"
				}
				trace.recordOperation("trace.scale_check_exec", template, "", "", "scale_check", outcome, traceRecordPayload{
					"pool_dir":       dir,
					"command":        sp.Check,
					"desired":        d,
					"error":          fmt.Sprint(err),
					"duration_ms":    time.Since(started).Milliseconds(),
					"agent_template": template,
					"agent_index":    agentIndex,
				}, "")
			}
		}(j, template, agentName, agentIndex, sp, pw.poolDir)
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
	cityPath string,
	cfg *config.City,
	pendingPools []poolEvalWork,
	stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
) map[string]int {
	counts := evaluatePendingPools(cityPath, cfg, pendingPools, stderr, trace)
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
	return buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, cfg, sp, store, nil, sessionBeads, nil, stderr)
}

func buildDesiredStateWithSessionBeads(
	cityName, cityPath string,
	beaconTime time.Time,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	rigStores map[string]beads.Store,
	sessionBeads *sessionBeadSnapshot,
	trace *sessionReconcilerTraceCycle,
	stderr io.Writer,
) DesiredStateResult {
	if cfg.Workspace.Suspended {
		return DesiredStateResult{}
	}

	bp := newAgentBuildParams(cityName, cityPath, cfg, sp, beaconTime, store, stderr)
	bp.sessionBeads = sessionBeads

	// Pre-compute suspended rig paths.
	suspendedRigPaths := buildSuspendedRigPaths(cfg)

	desired := make(map[string]TemplateParams)
	var pendingPools []poolEvalWork

	for i := range cfg.Agents {
		if cfg.Agents[i].Suspended {
			continue
		}
		backsNamedSession := false
		for j := range cfg.NamedSessions {
			if cfg.NamedSessions[j].TemplateQualifiedName() == cfg.Agents[i].QualifiedName() {
				backsNamedSession = true
				break
			}
		}

		sp := scaleParamsFor(&cfg.Agents[i])

		if !cfg.Agents[i].SupportsGenericEphemeralSessions() {
			continue
		}
		if backsNamedSession {
			rigName := configuredRigName(cityPath, &cfg.Agents[i], cfg.Rigs)
			if rigName != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rigName, cfg.Rigs))] {
				continue
			}
			// Named-session materialization is handled in the named-session pass,
			// but generic scale_check/min demand for the backing template still
			// creates ephemeral capacity through the pool pipeline.
			poolDir := agentCommandDir(cityPath, &cfg.Agents[i], cfg.Rigs)
			pendingPools = append(pendingPools, poolEvalWork{agentIdx: i, sp: sp, poolDir: poolDir})
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
	scaleCheckCounts := evaluatePendingPoolsMap(cityPath, cfg, pendingPools, stderr, trace)

	// Collect work beads with assignees — used for both pool demand and
	// named session on_demand wake. Hoisted out of the store block so
	// the named session section can also use it.
	var assignedWorkBeads []beads.Bead
	var ownershipWorkBeads []beads.Bead
	var storePartial bool
	if store != nil {
		assignedWorkBeads, storePartial = collectAssignedWorkBeads(cfg, store, rigStores, suspendedRigPaths)
		var ownershipPartial bool
		ownershipWorkBeads, ownershipPartial = collectOwnershipWorkBeads(cfg, store, rigStores, suspendedRigPaths)
		storePartial = storePartial || ownershipPartial
		if storePartial {
			fmt.Fprintf(stderr, "assignedWorkBeads: PARTIAL — store query failed, drain decisions suppressed\n") //nolint:errcheck
		}
		if len(assignedWorkBeads) > 0 {
			fmt.Fprintf(stderr, "assignedWorkBeads: %d beads found\n", len(assignedWorkBeads)) //nolint:errcheck
			for _, wb := range assignedWorkBeads {
				fmt.Fprintf(stderr, "  %s assignee=%s routed=%s status=%s\n", wb.ID, wb.Assignee, wb.Metadata["gc.routed_to"], wb.Status) //nolint:errcheck
			}
		} else {
			fmt.Fprintf(stderr, "assignedWorkBeads: 0 beads (rigStores=%d)\n", len(rigStores)) //nolint:errcheck
		}
		poolDesiredStates := ComputePoolDesiredStatesTraced(cfg, assignedWorkBeads, sessionBeads.Open(), scaleCheckCounts, trace)
		for _, poolState := range poolDesiredStates {
			cfgAgent := findAgentByTemplate(cfg, poolState.Template)
			if cfgAgent == nil {
				fmt.Fprintf(stderr, "buildDesiredState: pool %q has demand but no matching agent in config (skipping)\n", poolState.Template) //nolint:errcheck
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
				name := cfg.Agents[pw.agentIdx].Name
				if cfg.Agents[pw.agentIdx].SupportsInstanceExpansion() {
					name = poolInstanceName(cfg.Agents[pw.agentIdx].Name, slot, &cfg.Agents[pw.agentIdx])
				}
				qualifiedInstance := cfg.Agents[pw.agentIdx].QualifiedInstanceName(name)
				instanceAgent := deepCopyAgent(&cfg.Agents[pw.agentIdx], name, cfg.Agents[pw.agentIdx].Dir)
				fpExtra := buildFingerprintExtra(&instanceAgent)
				tp, err := resolveTemplate(bp, &instanceAgent, qualifiedInstance, fpExtra)
				if err != nil {
					fmt.Fprintf(stderr, "buildDesiredState: pool instance %q: %v (skipping)\n", qualifiedInstance, err) //nolint:errcheck
					continue
				}
				tp.PoolSlot = slot
				setTemplateEnvIdentity(&tp, qualifiedInstance)
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
		if spec.Agent.Suspended || agentInSuspendedRig(cityPath, spec.Agent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		namedSpecs[identity] = spec
	}
	namedWorkReady := make(map[string]bool, len(namedSpecs))
	// Check assigned work beads: if any work bead's Assignee matches a named
	// session's identity, that session has direct demand.
	//
	// Raw gc.routed_to metadata is intentionally NOT treated as direct named
	// demand here. Routed metadata feeds the named agent's work_query, and the
	// on-demand session only materializes from that path once the work is
	// actually actionable. This keeps blocked or merely routed work from
	// waking/materializing the named session prematurely.
	for identity := range namedSpecs {
		for _, wb := range assignedWorkBeads {
			if wb.Status != "open" && wb.Status != "in_progress" {
				continue
			}
			assignee := strings.TrimSpace(wb.Assignee)
			if assignee == identity {
				fmt.Fprintf(stderr, "namedWorkReady: %s matched by bead %s (assignee=%s status=%s)\n", identity, wb.ID, assignee, wb.Status) //nolint:errcheck
				namedWorkReady[identity] = true
				break
			}
		}
	}
	if len(assignedWorkBeads) > 0 {
		fmt.Fprintf(stderr, "namedWorkReady: %d assigned beads, %d named specs, ready=%v\n", len(assignedWorkBeads), len(namedSpecs), namedWorkReady) //nolint:errcheck
	}
	for identity, spec := range namedSpecs {
		canonicalBead, hasCanonical := findCanonicalNamedSessionBead(bp.sessionBeads, spec)
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
		tp.TemplateName = namedSessionBackingTemplate(spec)
		tp.InstanceName = identity
		tp.ConfiguredNamedIdentity = identity
		tp.ConfiguredNamedMode = spec.Mode
		if tp.Env == nil {
			tp.Env = make(map[string]string)
		}
		tp.Env["GC_TEMPLATE"] = namedSessionBackingTemplate(spec)
		tp.Env["GC_ALIAS"] = identity
		tp.Env["GC_AGENT"] = identity
		tp.Env["GC_SESSION_ORIGIN"] = "named"
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

	baseDesired := cloneDesiredState(desired)

	// Phase 2: discover session beads created outside config iteration
	// (e.g., by "gc session new"). Include them in desired state if they
	// have a valid template and are not held/closed.
	applySessionBeadDesiredOverlay(bp, cfg, desired, suspendedRigPaths, stderr)

	return DesiredStateResult{
		State:              desired,
		BaseState:          baseDesired,
		ScaleCheckCounts:   scaleCheckCounts,
		AssignedWorkBeads:  assignedWorkBeads,
		OwnershipWorkBeads: ownershipWorkBeads,
		NamedSessionDemand: namedWorkReady,
		StoreQueryPartial:  storePartial,
		BeaconTime:         beaconTime,
	}
}

func buildSuspendedRigPaths(cfg *config.City) map[string]bool {
	if cfg == nil || len(cfg.Rigs) == 0 {
		return nil
	}
	suspendedRigPaths := make(map[string]bool)
	for _, r := range cfg.Rigs {
		if r.Suspended {
			suspendedRigPaths[filepath.Clean(r.Path)] = true
		}
	}
	return suspendedRigPaths
}

func cloneDesiredState(src map[string]TemplateParams) map[string]TemplateParams {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]TemplateParams, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func applySessionBeadDesiredOverlay(
	bp *agentBuildParams,
	cfg *config.City,
	desired map[string]TemplateParams,
	suspendedRigPaths map[string]bool,
	stderr io.Writer,
) {
	realizedRoots := discoverSessionBeadsWithRoots(bp, cfg, desired, suspendedRigPaths, stderr)
	realizeDependencyFloors(bp, cfg, desired, realizedRoots, suspendedRigPaths, stderr)
}

func refreshDesiredStateWithSessionBeads(
	result DesiredStateResult,
	cityName, cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	sessionBeads *sessionBeadSnapshot,
	stderr io.Writer,
) DesiredStateResult {
	if cfg == nil || sessionBeads == nil {
		return result
	}

	base := result.BaseState
	if len(base) == 0 {
		base = result.State
	}
	refreshed := result
	refreshed.State = cloneDesiredState(base)
	if refreshed.State == nil {
		refreshed.State = make(map[string]TemplateParams)
	}

	bp := newAgentBuildParams(cityName, cityPath, cfg, sp, result.BeaconTime, store, stderr)
	bp.sessionBeads = sessionBeads
	applySessionBeadDesiredOverlay(bp, cfg, refreshed.State, buildSuspendedRigPaths(cfg), stderr)
	return refreshed
}

// collectAssignedWorkBeads queries each store (city + rigs) for actionable
// assigned work. It includes in-progress assigned work plus open assigned
// work that is actually ready. Routed-but-unassigned pool queue work is
// intentionally excluded here; new session demand comes from scale_check
// (and work_query as a defense-in-depth wake signal), while this helper is
// only for preserving sessions that already own actionable work.
func collectAssignedWorkBeads(
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
) ([]beads.Bead, bool) {
	return collectAssignedWorkSnapshot(cfg, cityStore, rigStores, suspendedRigPaths, false)
}

// collectOwnershipWorkBeads queries each store (city + rigs) for all assigned
// work that should preserve session ownership. Unlike collectAssignedWorkBeads,
// this includes blocked open work so lifecycle close/sweep paths never retire
// a session bead while future assigned work still points at it.
func collectOwnershipWorkBeads(
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
) ([]beads.Bead, bool) {
	return collectAssignedWorkSnapshot(cfg, cityStore, rigStores, suspendedRigPaths, true)
}

func collectAssignedWorkSnapshot(
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	includeBlockedOpen bool,
) ([]beads.Bead, bool) {
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
	var partial bool
	seen := make(map[string]struct{})
	for _, s := range stores {
		// In-progress beads with an assignee (active work).
		if inProgress, err := s.List(beads.ListQuery{Status: "in_progress"}); err == nil {
			appendAssignedUnique(&result, inProgress, seen)
		} else {
			log.Printf("collectAssignedWorkBeads: List(in_progress) failed: %v", err)
			partial = true
		}
		if includeBlockedOpen {
			// Open assigned beads preserve ownership even when blocked.
			if open, err := s.List(beads.ListQuery{Status: "open"}); err == nil {
				appendAssignedUnique(&result, open, seen)
			} else {
				log.Printf("collectOwnershipWorkBeads: List(open) failed: %v", err)
				partial = true
			}
		} else {
			// Ready beads with an assignee (queued direct handoff work that is
			// actually runnable, not merely open).
			if ready, err := s.Ready(); err == nil {
				appendAssignedUnique(&result, ready, seen)
			} else {
				log.Printf("collectAssignedWorkBeads: Ready() failed: %v", err)
				partial = true
			}
		}
	}
	return result, partial
}

// mergeNamedSessionDemand ensures that named-session assignee demand is
// reflected in poolDesired so downstream consumers (sessionWithinDesiredConfig,
// WakeConfig decisions) recognize the session as config-eligible. Without this,
// a bead with Assignee=identity but no gc.routed_to would materialize the
// session (via namedWorkReady) but leave poolDesired at 0, causing the
// reconciler to treat it as having no config demand.
func mergeNamedSessionDemand(poolDesired map[string]int, namedDemand map[string]bool, cfg *config.City) {
	for identity, ready := range namedDemand {
		if !ready {
			continue
		}
		// Resolve the identity to its backing agent template. cityName is
		// intentionally empty — we only need spec.Agent.QualifiedName(),
		// not spec.SessionName.
		spec, ok := findNamedSessionSpec(cfg, "", identity)
		if !ok {
			continue
		}
		template := spec.Agent.QualifiedName()
		if poolDesired[template] < 1 {
			poolDesired[template] = 1
		}
	}
}

func appendAssignedUnique(dst *[]beads.Bead, beadList []beads.Bead, seen map[string]struct{}) {
	for _, b := range beadList {
		if strings.TrimSpace(b.Assignee) == "" {
			continue
		}
		// Session beads are not actionable work — filter them at the source
		// so all consumers see only real tasks. Message beads are NOT filtered
		// here because they represent mail that should wake/materialize sessions;
		// idle nudge filters messages locally since mail nudging is handled
		// separately by the mail system.
		if b.Type == sessionBeadType {
			continue
		}
		if _, ok := seen[b.ID]; ok {
			continue
		}
		seen[b.ID] = struct{}{}
		*dst = append(*dst, b)
	}
}

func controlDispatcherOnlyConfig(cfg *config.City) *config.City {
	if cfg == nil {
		return nil
	}
	// Include every configured control-dispatcher so standalone mode can
	// recover rig-scoped dispatcher instances as well as the city one.
	var agents []config.Agent
	for _, agentCfg := range cfg.Agents {
		if agentCfg.Name == config.ControlDispatcherAgentName {
			agents = append(agents, agentCfg)
		}
	}
	if len(agents) == 0 {
		return nil
	}
	cfgCopy := *cfg
	cfgCopy.Agents = agents
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
		if isEphemeralSessionBeadForAgent(b, cfgAgent) {
			manualSession := isManualSessionBead(b)
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
		tp.ManualSession = isManualSessionBead(b)
		if tp.ManualSession {
			if manualAlias := strings.TrimSpace(b.Metadata["alias"]); manualAlias != "" {
				// Explicit aliases from `gc session new --alias ...` are
				// user-chosen command targets and must survive controller sync.
				tp.Alias = manualAlias
			}
		}
		if isEphemeralSessionBeadForAgent(b, cfgAgent) {
			if !tp.ManualSession || strings.TrimSpace(b.Metadata["alias"]) == "" {
				tp.Alias = ""
			}
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
	if cfgAgent == nil || !cfgAgent.SupportsGenericEphemeralSessions() || desiredHasTemplate(desired, cfgAgent.QualifiedName()) {
		return
	}

	if bp.beadStore == nil {
		name := cfgAgent.Name
		if cfgAgent.SupportsInstanceExpansion() {
			name = poolInstanceName(cfgAgent.Name, 1, cfgAgent)
		}
		qualifiedInstance := cfgAgent.QualifiedInstanceName(name)
		instanceAgent := deepCopyAgent(cfgAgent, name, cfgAgent.Dir)
		fpExtra := buildFingerprintExtra(&instanceAgent)
		tp, err := resolveTemplate(bp, &instanceAgent, qualifiedInstance, fpExtra)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedInstance, err) //nolint:errcheck
			return
		}
		tp.DependencyOnly = true
		setTemplateEnvIdentity(&tp, qualifiedInstance)
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
	used := make(map[string]bool)
	usedSlots := make(map[int]bool)
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
		slot := claimPoolSlot(cfgAgent, sessionBead, usedSlots)
		instanceName := poolInstanceName(cfgAgent.Name, slot, cfgAgent)
		qualifiedInstance := cfgAgent.QualifiedInstanceName(instanceName)
		instanceAgent := deepCopyAgent(cfgAgent, instanceName, cfgAgent.Dir)
		fpExtra := buildFingerprintExtra(&instanceAgent)
		tp, err := resolveTemplateForSessionBead(bp, &instanceAgent, qualifiedInstance, fpExtra, sessionBead)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: pool %q session %s: %v (skipping)\n", qualifiedName, sessionBead.ID, err) //nolint:errcheck
			continue
		}
		tp.Alias = qualifiedInstance
		tp.InstanceName = qualifiedInstance
		tp.PoolSlot = slot
		setTemplateEnvIdentity(&tp, qualifiedInstance)
		installAgentSideEffects(bp, &instanceAgent, tp, stderr)
		desired[tp.SessionName] = tp
	}
}

func setTemplateEnvIdentity(tp *TemplateParams, identity string) {
	if tp == nil || identity == "" {
		return
	}
	if tp.Env == nil {
		tp.Env = make(map[string]string)
	}
	tp.Env["GC_AGENT"] = identity
	tp.Env["GC_ALIAS"] = identity
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

func claimPoolSlot(cfgAgent *config.Agent, sessionBead beads.Bead, used map[int]bool) int {
	if slot := existingPoolSlot(cfgAgent, sessionBead); slot > 0 && !used[slot] {
		used[slot] = true
		return slot
	}
	for slot := 1; ; slot++ {
		if used[slot] {
			continue
		}
		used[slot] = true
		return slot
	}
}

func existingPoolSlot(cfgAgent *config.Agent, sessionBead beads.Bead) int {
	if sessionBead.Metadata["pool_slot"] != "" {
		if slot, err := strconv.Atoi(strings.TrimSpace(sessionBead.Metadata["pool_slot"])); err == nil && slot > 0 {
			return slot
		}
	}
	agentName := strings.TrimSpace(sessionBeadAgentName(sessionBead))
	if agentName == "" || cfgAgent == nil {
		return 0
	}
	if slot := resolvePoolSlot(agentName, cfgAgent.QualifiedName()); slot > 0 {
		return slot
	}
	if slot := resolvePoolSlot(agentName, cfgAgent.Name); slot > 0 {
		return slot
	}
	for idx, themed := range cfgAgent.NamepoolNames {
		if strings.TrimSpace(themed) == agentName {
			return idx + 1
		}
		if cfgAgent.Dir != "" && strings.TrimSpace(cfgAgent.QualifiedInstanceName(themed)) == agentName {
			return idx + 1
		}
	}
	return 0
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
		if isManualSessionBead(bead) {
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
		if bead.Status == "closed" || isManualSessionBead(bead) {
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

// poolInstanceName returns the name for pool slot N.
// If the agent has namepool names and the slot is in range, uses the themed
// name. Otherwise falls back to "{base}-{slot}".
func poolInstanceName(base string, slot int, a *config.Agent) string {
	if a != nil && slot >= 1 && slot <= len(a.NamepoolNames) {
		return a.NamepoolNames[slot-1]
	}
	return fmt.Sprintf("%s-%d", base, slot)
}
