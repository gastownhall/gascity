package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/poolplan"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worktree"
)

// storeScopedBeadKey identifies an assigned-work bead by its store ref and ID.
// AssignedWorkBeads can carry the same bead ID from independent city and rig
// stores (see AssignedWorkStores/AssignedWorkStoreRefs), so wake-demand
// readiness must be scoped by store ref. A plain ID key would let a ready bead
// in one store mark a blocked open bead with the same ID in another store as
// ready and reintroduce the awake-demand hang this readiness fix prevents.
type storeScopedBeadKey struct {
	StoreRef string
	ID       string
}

// ContinuationClaimCandidate is a ready graph-v2 successor that may need a
// bounded claim nudge after its current pool session completed the preceding
// step but did not start another turn. All fields are exact provenance:
// StoreRef is the canonical ref of the scope that OWNS the row (city:<name> or
// rig:<name>), which for a class binding is not the leg the row was read from;
// RootBeadID was verified through Store, and Assignee is the bead's persisted
// preassignment. Store is retained for immediate pre-delivery revalidation;
// session identity resolution happens later against the post-reconcile session
// snapshot.
type ContinuationClaimCandidate struct {
	WorkBeadID string
	RootBeadID string
	StoreRef   string
	Assignee   string
	Store      beads.Store
}

// DesiredStateResult bundles the desired session state with the scale_check
// counts that produced it. Callers that need poolDesired for wake decisions
// can pass ScaleCheckCounts to ComputePoolDesiredStates without re-running
// scale_check commands.
type DesiredStateResult struct {
	State            map[string]TemplateParams
	BaseState        map[string]TemplateParams
	ScaleCheckCounts map[string]int // nil when store is nil or scale_check not run
	// ScaleCheckPartialTemplates records all templates whose bead-backed demand
	// probe failed. PoolScaleCheckPartialTemplates blocks fresh pool creates;
	// PoolPartialRetentionTemplates preserves existing pool capacity and may also
	// contain retention-only failures where another store proved positive demand.
	// NamedScaleCheckPartialTemplates only protects configured named sessions.
	ScaleCheckPartialTemplates      map[string]bool
	PoolScaleCheckPartialTemplates  map[string]bool
	PoolPartialRetentionTemplates   map[string]bool
	NamedScaleCheckPartialTemplates map[string]bool
	PoolDesiredCounts               map[string]int // runtime-owned demand snapshot; reused on stable patrol ticks when still fresh
	WorkSet                         map[string]bool
	AssignedWorkBeads               []beads.Bead // actionable assigned work, plus stranded pool work that needs release
	// AssignedWorkStores is aligned by index with AssignedWorkBeads, so later
	// mutation paths update rig-owned work in the right store even when
	// independent stores produce overlapping bead IDs.
	AssignedWorkStores []beads.Store
	// AssignedWorkStoreRefs is aligned by index with AssignedWorkBeads.
	// The empty string means city store; non-empty values are rig names.
	// Consumers that decide whether a specific agent should run must use
	// this scope before treating a bead as reachable work for that agent.
	AssignedWorkStoreRefs []string
	// ReadyUnassignedRoutedWorkBeads contains the ready, routed, unassigned
	// work selected as concrete default pool demand for this tick. It remains
	// separate from AssignedWorkBeads so assignment/wake semantics stay
	// assignee-only; the idle-claim backstop uses it to re-nudge an already
	// running pool slot after that slot is rebound to newly routed work.
	ReadyUnassignedRoutedWorkBeads []beads.Bead
	// ReadyUnassignedRoutedWorkStoreRefs is index-aligned with
	// ReadyUnassignedRoutedWorkBeads and uses canonical city:/rig: refs.
	ReadyUnassignedRoutedWorkStoreRefs []string
	// NamedSessionDemand records which named-session identities have active
	// direct assignee demand (Assignee == identity). The reconciler merges this
	// into poolDesired so that on-demand named sessions remain config-eligible.
	NamedSessionDemand map[string]bool
	// NamedSessionRoutedDemand records, per named-session identity, whether
	// there is routed-but-unassigned demand on the identity's backing template
	// (ScaleCheckCounts[backingTemplate] > 0), computed BEFORE canonical-alias
	// pool suppression runs. Unlike NamedSessionDemand this is not
	// assignee-direct and must never be merged into poolDesired — it exists
	// solely to give ComputeAwakeSet a wake-only signal for an asleep named
	// holder whose alias correctly suppresses the redundant pool standby
	// (ga-jl73y2): routed-but-unclaimed demand that should wake the holder,
	// without affecting pool sizing.
	//
	// It IS sleep-suppressing while the routed demand remains live. The
	// resulting "routed-demand" wake reason is exempt from ComputeAwakeSet's
	// idle-sleep pass and overrides non-interactive sleep suppression in
	// wakeDemandOverridesSleepSuppression — otherwise a long-lived holder
	// carrying a non-zero idle reference is re-slept on the same tick and the
	// wake is silently undone. Suppression ends when demand clears: the holder
	// then drains via the non-exempt "on-demand:running" reason. Scoped to
	// canonical singleton backing pools, so only the one session that can
	// serve the demand is kept awake.
	NamedSessionRoutedDemand map[string]bool
	// ReadyAssigned is the set of AssignedWorkBeads that carry real wake-demand
	// readiness, keyed by store ref + bead ID: in-progress work, assigned
	// molecule roots, and store-Ready()/deps-gated open work. Beads admitted
	// only by the open-routed orphan-release pass are absent, so the awake
	// bridge does not treat a blocked open bead as live wake demand. The key is
	// store-scoped because the same bead ID can appear in independent city and
	// rig stores; see storeScopedBeadKey. The reconciler resolves this into a
	// per-bead readiness slice for buildAwakeInputFromReconciler's
	// AwakeWorkBead.Ready flag.
	ReadyAssigned map[storeScopedBeadKey]bool
	// ContinuationClaimCandidates is the fail-closed projection of
	// ReadyAssigned used by the post-reconcile continuation-claim backstop.
	// It is empty on any assigned-work partial read.
	ContinuationClaimCandidates []ContinuationClaimCandidate
	// ContinuationClaimQueryPartial preserves existing pacing markers when an
	// exact candidate/root read was incomplete or internally contradictory.
	ContinuationClaimQueryPartial bool
	// StoreQueryPartial is true when one or more bead store work queries
	// failed. When set, the reconciler must NOT drain sessions based on the
	// incomplete desired state — a transient failure would cause running
	// sessions to be falsely orphaned and interrupted via Ctrl-C.
	StoreQueryPartial bool
	// SessionQueryPartial is true when the session view for this build is
	// incomplete: either primary session-bead snapshot loading failed, or the
	// cross-store session census (sessions binding + city work store + rig work
	// stores) returned a partial result. Orphan-release and drain decisions must
	// treat this like an incomplete work snapshot because missing live session
	// beads make assigned work look orphaned; it also gates the stalled-pool
	// continuation and execution backstops in city_runtime.go.
	SessionQueryPartial bool
	// SessionSnapshotComplete is the positive proof used by fresh pool planning;
	// it is false for a nil/degraded primary snapshot or any partial cross-store
	// session census. SessionOccupancyInfos preserves that exact complete census
	// for refreshDesiredStateWithSessionBeads, whose primary snapshot alone may
	// omit a live holder from another store.
	SessionSnapshotComplete bool
	SessionOccupancyInfos   []session.Info
	BeaconTime              time.Time
}

func (r DesiredStateResult) snapshotQueryPartial() bool {
	return r.StoreQueryPartial || r.SessionQueryPartial
}

type poolEvalWork struct {
	agentIdx  int
	sp        scaleParams
	poolDir   string
	env       map[string]string
	newDemand bool
}

type defaultScaleCheckTarget struct {
	template string
	storeKey string
	store    beads.Store
	err      error
}

type scaleCheckDemand struct {
	Count          int
	WorkBeadIDs    []string
	Titles         map[string]string
	Packs          map[string]string
	Workspaces     map[string]string
	StoreRefs      map[string]string
	WorktreeSpecs  map[string]*worktree.Spec
	WorktreeErrors map[string]string
	// ParentSIDs maps work-bead id → gc.brain_parent_sid, carrying the fork
	// parent through to the new pool session bead so the launch path can fork
	// the warm arm off its pre-built brain.
	ParentSIDs map[string]string
}

var (
	errPoolSessionCreateBudgetExhausted = errors.New("pool session create budget exhausted")
	errPoolSessionCreatePartial         = errors.New("pool session create skipped: demand read partial")
	errPoolSessionCreateProviderRed     = errors.New("pool session create skipped: provider red")
)

// poolSessionCreateFairShareCounter rotates scarce create tokens across
// contending pools so stable template sort order does not always win.
var poolSessionCreateFairShareCounter atomic.Uint64

func (bp *agentBuildParams) configurePoolSessionCreateFairShare(states []PoolDesiredState) {
	if bp == nil || bp.poolSessionCreateBudget == nil {
		return
	}
	demands := make([]poolplan.Demand, 0, len(states))
	for _, state := range states {
		demand := poolplan.Demand{Template: state.Template}
		for _, request := range state.Requests {
			// Requests with a session bead ID represent in-flight capacity and
			// must not reserve fresh-create budget for this template.
			if request.Tier != "new" || request.SessionBeadID != "" {
				continue
			}
			demand.FreshCreates++
			demand.HasFloor = demand.HasFloor || request.FloorGuarantee
		}
		if demand.FreshCreates > 0 {
			demands = append(demands, demand)
		}
	}
	seed := poolSessionCreateFairShareCounter.Add(1) - 1
	bp.poolSessionCreateBudget.ConfigureFairShare(demands, seed)
}

func (bp *agentBuildParams) tryClaimPoolSessionCreate(template string) bool {
	if bp == nil || bp.poolSessionCreateBudget == nil {
		return true
	}
	return bp.poolSessionCreateBudget.TryClaim(template)
}

func (bp *agentBuildParams) releasePoolSessionCreate() {
	if bp == nil || bp.poolSessionCreateBudget == nil {
		return
	}
	bp.poolSessionCreateBudget.Release()
}

func evaluatePendingPools(
	cfg *config.City,
	pendingPools []poolEvalWork,
	stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
) ([]int, []bool) {
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
		probeEnv := pw.env
		sp.Check = prefixShellEnv(controllerQueryPrefixEnv(probeEnv), sp.Check)
		template := cfg.Agents[pw.agentIdx].QualifiedName()
		agentName := cfg.Agents[pw.agentIdx].Name
		agentIndex := pw.agentIdx
		newDemand := pw.newDemand
		go func(idx int, template, agentName string, agentIndex int, sp scaleParams, dir string, newDemand bool) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			started := time.Now()
			var d int
			var err error
			if newDemand {
				d, err = evaluatePoolNewDemand(agentName, sp, dir, probeEnv, shellScaleCheck)
			} else {
				d, err = evaluatePool(agentName, sp, dir, probeEnv, shellScaleCheck)
			}
			evalResults[idx] = poolEvalResult{desired: d, err: err}
			if trace != nil {
				outcome := TraceOutcomeSuccess
				if err != nil {
					outcome = TraceOutcomeFailed
				}
				trace.RecordOperation(TraceSiteScaleCheckExec, TraceReasonScaleCheck, outcome, "", template, "", time.Since(started), traceRecordPayload{
					"pool_dir":       dir,
					"command":        sp.Check,
					"desired":        d,
					"error":          fmt.Sprint(err),
					"duration_ms":    time.Since(started).Milliseconds(),
					"agent_template": template,
					"agent_index":    agentIndex,
				})
			}
		}(j, template, agentName, agentIndex, sp, pw.poolDir, newDemand)
	}
	wg.Wait()

	counts := make([]int, len(pendingPools))
	partials := make([]bool, len(pendingPools))
	for j, pw := range pendingPools {
		pr := evalResults[j]
		if pr.err != nil {
			partials[j] = true
			if pw.newDemand {
				fmt.Fprintf(stderr, "buildDesiredState: %v (using new demand=0)\n", pr.err) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "buildDesiredState: %v (using min=%d)\n", pr.err, pw.sp.Min) //nolint:errcheck
			}
		}
		counts[j] = pr.desired
	}
	return counts, partials
}

// evaluatePendingPoolsMap is like evaluatePendingPools but returns a map from
// agent qualified name to scale_check count. In bead-backed reconciliation the
// count is additive new demand; legacy no-store callers still use desired
// counts.
func evaluatePendingPoolsMap(
	cfg *config.City,
	pendingPools []poolEvalWork,
	stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
) (map[string]int, map[string]bool) {
	counts, partials := evaluatePendingPools(cfg, pendingPools, stderr, trace)
	m := make(map[string]int, len(counts))
	var partialTemplates map[string]bool
	for j, pw := range pendingPools {
		template := cfg.Agents[pw.agentIdx].QualifiedName()
		m[template] = counts[j]
		if partials[j] {
			partialTemplates = markScaleCheckPartialTemplate(partialTemplates, template)
		}
	}
	return m, partialTemplates
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
// Rig-scoped agents with an implicit default scale_check require rigStores;
// when rigStores is missing, they report zero new demand plus a diagnostic
// rather than counting work from the wrong store.
func buildDesiredState(
	cityName, cityPath string,
	beaconTime time.Time,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	stderr io.Writer,
) DesiredStateResult {
	var sessionBeads *sessionBeadSnapshot
	var sessionQueryPartial bool
	if store != nil {
		var err error
		sessionBeads, err = loadSessionBeadSnapshot(store)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: listing session beads: %v\n", err) //nolint:errcheck
			sessionQueryPartial = true
		}
	}
	poolDecisionTime := time.Now()
	result := buildDesiredStateWithSessionBeadsAt(
		cityName,
		cityPath,
		beaconTime,
		poolDecisionTime,
		cfg,
		sp,
		store,
		nil,
		sessionBeads,
		nil,
		stderr,
	)
	result.SessionQueryPartial = result.SessionQueryPartial || sessionQueryPartial
	return result
}

// recordDemandSubPhase emits one operation record under the demand-snapshot
// trace site for a sub-phase of buildDesiredStateWithSessionBeads. The parent
// `load_demand_snapshot` phase regularly dominates the controller tick
// (measured avg 6.4s / max 40.8s across 190 storm-window cycles on a small
// idle city, gastownhall/gascity#2463) but was previously opaque: a single
// aggregate duration with no split between the cross-store collection reads,
// the per-demand-group Ready probes, the scale_check subprocess execs, and
// pure computation. These records make that split first-class trace data so
// store-contention regressions can be attributed without ad-hoc rebuilds.
// RecordControllerOperation is nil-receiver-safe, so callers without an
// active trace (e.g. buildDesiredState outside the tick) cost one branch.
func recordDemandSubPhase(trace *sessionReconcilerTraceCycle, name string, start time.Time, fields map[string]any) {
	trace.RecordControllerOperation(TraceSiteDemandSnapshot, TraceReasonRetained, TraceOutcomeComplete, name, time.Since(start), fields)
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
	return buildDesiredStateWithSessionBeadsAt(
		cityName,
		cityPath,
		beaconTime,
		time.Now(),
		cfg,
		sp,
		store,
		rigStores,
		sessionBeads,
		trace,
		stderr,
	)
}

func buildDesiredStateWithSessionBeadsAt(
	cityName, cityPath string,
	beaconTime, poolDecisionTime time.Time,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	rigStores map[string]beads.Store,
	sessionBeads *sessionBeadSnapshot,
	trace *sessionReconcilerTraceCycle,
	stderr io.Writer,
) DesiredStateResult {
	citySt, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	if effectiveCitySuspended(cfg, citySt) {
		return DesiredStateResult{}
	}

	bp := newAgentBuildParams(cityName, cityPath, cfg, sp, beaconTime, store, stderr)
	bp.sessionBeads = sessionBeads
	bp.sessionSnapshotCompletenessKnown = true
	bp.sessionSnapshotComplete = store == nil || (sessionBeads != nil && sessionBeads.LoadError() == nil)

	// Pre-compute suspended rig paths (config + runtime state).
	suspendedRigPaths := buildSuspendedRigPathsForCity(cfg, cityPath)
	bp.sessionCensusRigStores = cloneSessionCensusRigStores(rigStores)
	bp.sessionCensusSuspendedRigPaths = cloneSessionCensusSuspendedRigPaths(suspendedRigPaths)

	// Collect all open session Infos from all stores to correctly count
	// running sessions for each pool. A partial/failed collection is logged,
	// not swallowed: undercounting running sessions can misclassify a pool as
	// cold and trigger a spurious scale-from-zero probe.
	subPhaseStart := time.Now()
	allOpenSessionInfos, openSessionBeadsErr := collectAllOpenSessionInfos(cityPath, cfg, store, rigStores, suspendedRigPaths)
	bp.sessionOccupancyInfos = allOpenSessionInfos
	recordDemandSubPhase(trace, "demand_snapshot.collect_open_session_beads", subPhaseStart, map[string]any{
		"beads":   len(allOpenSessionInfos),
		"partial": openSessionBeadsErr != nil,
	})
	if openSessionBeadsErr != nil {
		bp.sessionSnapshotComplete = false
		fmt.Fprintf(stderr, "collectAllOpenSessionInfos: PARTIAL — %v (cold-pool detection may undercount running sessions)\n", openSessionBeadsErr) //nolint:errcheck
	}

	desired := make(map[string]TemplateParams)
	var pendingPools []poolEvalWork
	var defaultScaleTargets []defaultScaleCheckTarget
	var defaultNamedScaleTargets []defaultScaleCheckTarget
	// coldWakeTemplates marks pool templates that received a cold-pool wake
	// probe (FR-S0.1). Their default-probe demand is clamped to 1 in the merge
	// below so the probe wakes a cold pool from zero without overriding the
	// pool's custom scale_check count.
	coldWakeTemplates := map[string]bool{}
	// namedOnDemandTemplates marks templates where namedSessionMode != "always"
	// and a defaultScaleTargets entry was appended (on_demand named-backing).
	// Their pool demand is clamped to 1 at the merge so one pool slot wakes the
	// session without over-spawning {name}-N phantoms when N routed beads arrive.
	namedOnDemandTemplates := map[string]bool{}
	// activeStores is the set of stores a cold custom-scale_check pool is probed
	// against (city + every non-suspended rig store), so routed demand a sleeping
	// rig pool can't see locally — e.g. work queued in the city store — still
	// wakes it. Only consulted for the clamped cold-wake probe.
	type activeStore struct {
		store beads.Store
		ref   string
	}
	activeStores := []activeStore{{store: store, ref: "city"}}
	for _, rig := range cfg.Rigs {
		if suspendedRigPaths[filepath.Clean(rig.Path)] {
			continue
		}
		if s, ok := rigStores[rig.Name]; ok {
			activeStores = append(activeStores, activeStore{store: s, ref: rig.Name})
		}
	}

	for i := range cfg.Agents {
		if cfg.Agents[i].Suspended {
			continue
		}
		namedSessionMode := ""
		for j := range cfg.NamedSessions {
			if cfg.NamedSessions[j].TemplateQualifiedName() == cfg.Agents[i].QualifiedName() {
				namedSessionMode = cfg.NamedSessions[j].ModeOrDefault()
				break
			}
		}
		backsNamedSession := namedSessionMode != ""

		// Controller-owned: config.QueryTopology{Beads: ...} and NOT
		// cityQueryTopology. Resolving the federation fact means asking the
		// storage routes, and the one-shot funnel that answers for a CLI command
		// (cliStorageRoutes) is explicitly for the half of the program that
		// "never builds a CityRuntime" — a controller reaching it would open the
		// city's binding a second time in a process that already holds it open.
		// The controller's own routes are the right source; threading them into
		// this plumbing is a change to controller wiring, not part of swapping
		// the reader, so it stays with the claim-routing slice (ga-601v2).
		//
		// Nothing executes this string on the controller path either: a
		// default-probe pool counts in-process below (defaultScaleTargets), and a
		// custom-scale_check pool runs the operator's own command, so the
		// generated count-form is reached only by the legacy nil-store caller.
		sp := scaleParamsForTopology(&cfg.Agents[i], config.QueryTopology{Beads: cfg.Beads})
		// Expand {{.Rig}}/{{.AgentBase}} before the scale_check enters the
		// controller probe pool so rig-scoped agents query their own rig.
		sp.Check = expandAgentCommandTemplate(cityPath, cityName, &cfg.Agents[i], cfg.Rigs, "scale_check", sp.Check, stderr)

		if !cfg.Agents[i].SupportsGenericEphemeralSessions() {
			continue
		}

		hasCustomScaleCheck := strings.TrimSpace(cfg.Agents[i].ScaleCheck) != ""
		template := cfg.Agents[i].QualifiedName()
		storeScopedControlDispatcher := cfg.Agents[i].Name == config.ControlDispatcherAgentName
		runningSessions := 0
		for _, si := range allOpenSessionInfos {
			if isPoolManagedSessionInfo(si) && poolSessionIsLiveInfo(si) {
				// Match the qualified template by identity equivalence.
				// allOpenSessionInfos is aggregated across the city + every rig
				// store, and pool session beads store the qualified name
				// (agent.QualifiedName(), see session_sleep.go); adopted beads may
				// still carry a legacy bound form of the same identity, which must
				// count here or the cold-wake probe over-wakes a pool that already
				// has a live session. Equivalence preserves the cross-rig guarantee:
				// an unqualified base name never normalizes to a dir-scoped agent,
				// and a same-base-name pool in another rig (e.g. rigB/planner)
				// normalizes to itself, so neither inflates this rig's count.
				if agentTemplateIdentitiesEquivalent(cfg, si.Template, template) {
					runningSessions++
				}
			}
		}

		// Cold-pool wake probe (FR-S0.1): a pool with a custom scale_check that
		// returns 0 while it has zero running sessions and min=0 would never wake
		// to discover routed demand it can't see while asleep. For that case we
		// probe every active store and clamp the result to 1 in the merge below,
		// so the pool wakes from zero without the probe overriding the custom
		// check's count. Pools without a custom scale_check use their own-store
		// default probe (authoritative count; a missing rig store reports
		// zero/partial), so they need no cold-specific handling.
		isCold := runningSessions == 0 && cfg.Agents[i].EffectiveMinActiveSessions() == 0

		if backsNamedSession {
			rigName := configuredRigName(cityPath, &cfg.Agents[i], cfg.Rigs)
			if rigName != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rigName, cfg.Rigs))] {
				continue
			}
			// Named-session materialization is handled in the named-session pass,
			// but explicit scale_check/min demand for the backing template still
			// creates ephemeral capacity through the pool pipeline. The default
			// routed-work probes treat gc.routed_to=<template> as generic pool
			// demand. Named sessions wake only from direct Assignee=<identity>
			// work below; defaultNamedScaleTargets only preserves partial-query
			// retention for configured named-session beads.
			poolDir := agentCommandDir(cityPath, &cfg.Agents[i], cfg.Rigs)
			if store != nil && !hasCustomScaleCheck {
				ownTarget := defaultScaleCheckTargetForAgent(cityPath, cfg, &cfg.Agents[i], store, rigStores)
				// mode='always': named session is unconditionally desired by the named
				// pass; pool demand is redundant and creates {name}-N phantoms when N
				// routed beads arrive. mode='on_demand': pool demand wakes the sleeping
				// singleton (namedWorkReady covers only direct Assignee beads, not
				// gc.routed_to). Leave defaultNamedScaleTargets unchanged for both modes
				// (partial-query retention).
				if namedSessionMode != "always" {
					defaultScaleTargets = append(defaultScaleTargets, ownTarget)
					namedOnDemandTemplates[template] = true
				}
				defaultNamedScaleTargets = append(defaultNamedScaleTargets, ownTarget)
				// Cross-store demand for named-backing pools (vp-cl4): mirror the
				// generic-pool guard (vp-s37 / #3078 below). A rig pool that backs
				// a named session and has no custom scale_check must also probe
				// the city store so that routed demand delivered there (vp-kvp)
				// counts, warm or cold — like the generic-pool probe below, this
				// is NOT gated on isCold: a warm named-backing pool that only
				// probed its rig store would drop to zero demand between city
				// beads and be orphan-drained, then re-glimpse city demand on the
				// next cold tick and respawn (the same spawn/drain treadmill,
				// amplitude clamped to 1 by the namedOnDemandTemplates clamp).
				// Same guard conditions apply: healthy own rig store, not
				// city-aliased, not city-scoped. The named-session target list
				// mirrors these probes only for partial-query retention bookkeeping.
				if !storeScopedControlDispatcher && ownTarget.storeKey != "city" && ownTarget.store != nil && ownTarget.err == nil && ownTarget.store != store {
					cityTarget := defaultScaleCheckTarget{template: template, store: store, storeKey: "city"}
					if namedSessionMode != "always" {
						defaultScaleTargets = append(defaultScaleTargets, cityTarget)
					}
					defaultNamedScaleTargets = append(defaultNamedScaleTargets, cityTarget)
				}
				continue
			}
			if store != nil && isCold && !storeScopedControlDispatcher {
				for _, source := range activeStores {
					target := defaultScaleCheckTarget{template: template, store: source.store, storeKey: source.ref}
					// Mirror the generic-pool cold-wake probe below (vp-s37 /
					// #3078): a custom scale_check that is cold and asleep
					// cannot see routed demand, so probe every active store
					// and feed defaultScaleTargets too, not just
					// defaultNamedScaleTargets (which only preserves
					// partial-query retention for defaultNamedSessionDemand
					// and never itself produces demand — see its doc
					// comment). Gate on mode != "always": an always-on named
					// session is already unconditionally desired by the
					// named pass, so adding pool demand for the same
					// template would spawn a redundant {name}-N phantom
					// alongside it, mirroring the identical guard on the
					// !hasCustomScaleCheck branch above.
					if namedSessionMode != "always" {
						defaultScaleTargets = append(defaultScaleTargets, target)
					}
					defaultNamedScaleTargets = append(defaultNamedScaleTargets, target)
				}
				if namedSessionMode != "always" {
					// Clamp to 1 in the merge below (coldWakeTemplates), same
					// as the generic-pool branch: this probe only wakes the
					// pool from zero and must never override the custom
					// check's own authoritative warm count.
					coldWakeTemplates[template] = true
				}
			}
			pendingPools = append(pendingPools, poolEvalWork{agentIdx: i, sp: sp, poolDir: poolDir, newDemand: store != nil})
			continue
		}

		rigName := configuredRigName(cityPath, &cfg.Agents[i], cfg.Rigs)
		if rigName != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rigName, cfg.Rigs))] {
			continue
		}
		// Pool agent: collect scale_check inputs. Legacy no-store mode uses
		// them as desired counts; bead-backed mode uses them as authoritative
		// new unassigned demand while assigned work drives resume requests.
		poolDir := agentCommandDir(cityPath, &cfg.Agents[i], cfg.Rigs)
		if store != nil && !hasCustomScaleCheck {
			ownTarget := defaultScaleCheckTargetForAgent(cityPath, cfg, &cfg.Agents[i], store, rigStores)
			defaultScaleTargets = append(defaultScaleTargets, ownTarget)
			// Cross-store demand (FR-S0.1 / vp-s37): a rig pool's routed demand
			// may live in the city store (vp-kvp cross-store delivery), which
			// the own-rig probe above cannot see. Add a city-store probe so the
			// pool's demand reflects routed work in either store — matching the
			// claim path, where a rig agent's work_query already federates
			// across stores and claims city-delivered work.
			//
			// NOT gated on isCold. This probe began as a cold-wake assist (a
			// sleeping pool can't discover city-store demand), but gating it on
			// isCold left a WARM rig pool structurally blind to the same
			// demand: its count stayed pinned at the rig-store total while
			// routed beads sat unclaimed in the city store, and a pool at the
			// warm/cold boundary oscillated pool_desired N↔0 (cold ticks
			// glimpsed city demand and spawned; warm ticks went blind and the
			// reconciler orphan-drained the sessions it had just started).
			// Observed in production: a warm worker pool pinned at
			// poolDesired=1 against 1 rig-store + 9 city-store routed beads,
			// serializing every live workflow behind one session and starving
			// all dep-downstream control beads. Demand a pool can claim is
			// demand it must be able to count, warm or cold.
			//
			// No clamp: unlike a custom-scale_check pool — where the probe is
			// clamped so it cannot override the custom count (see
			// coldWakeTemplates below) — the default probe IS the
			// authoritative count, so it scales to total routed demand (bounded
			// by max_active and the daemon's max_wakes_per_tick), matching the
			// retired cold-pool-spawner's scale-to-want. Counts sum across
			// store groups and the beads are distinct per store, so probing
			// both yields the correct union demand. A city-scoped pool's
			// own target is already the city store, so it needs no extra probe.
			//
			// Gated on a healthy own rig store: when the rig store is missing or
			// errored we stay partial and do NOT wake on cross-store demand —
			// a rig executor cannot do its work while its rig store is
			// unreachable, and the partial flag must keep suppressing drain
			// decisions rather than be overridden by a spurious city-store wake.
			//
			// ownTarget.store != store is a same-pointer optimization: it skips
			// appending a "city" probe when the rig store IS the identical Store
			// object as the city store (which would re-probe one store, not form
			// a real cross-store union). It is NOT the alias-safety guard — a rig
			// store that aliases the city store as a DISTINCT Store value (an
			// unbound rig falling back to the city scope) passes this inequality,
			// so the "city" group can still surface the same beads. countedBeads
			// dedups those per template ACROSS store groups by bead ID (see its
			// definition below), and is the load-bearing defense now that the
			// city probe is no longer cold-gated. Current store-map builders skip
			// such rigs, so today this is defense-in-depth against future callers.
			// Control dispatchers are deliberately store-scoped: a rig copy cannot
			// claim a route from the city store. Keep their cold-wake probe on the
			// owning store instead of applying generic cross-store pool delivery.
			if !storeScopedControlDispatcher && ownTarget.storeKey != "city" && ownTarget.store != nil && ownTarget.err == nil && ownTarget.store != store {
				defaultScaleTargets = append(defaultScaleTargets, defaultScaleCheckTarget{template: template, store: store, storeKey: "city"})
			}
			continue
		}
		// Custom-scale_check pools deliberately KEEP the cold-only probe (unlike
		// the default-probe branches above, which count cross-store demand warm
		// or cold): the custom check is the authoritative count while the pool
		// is awake, and this probe is clamped to 1 (coldWakeTemplates) so it can
		// only wake a sleeping pool, never override the custom count. A custom
		// scale_check that should scale on cross-store routed demand must count
		// it itself, or the pool will churn at the warm/cold boundary.
		if store != nil && isCold && !storeScopedControlDispatcher {
			for _, source := range activeStores {
				defaultScaleTargets = append(defaultScaleTargets, defaultScaleCheckTarget{template: template, store: source.store, storeKey: source.ref})
			}
			coldWakeTemplates[template] = true
		}
		env, err := controllerQueryRuntimeEnv(cityPath, cfg, &cfg.Agents[i])
		if err != nil {
			fmt.Fprintf(stderr, "scaleCheck: building env for %s: %v\n", cfg.Agents[i].QualifiedName(), err) //nolint:errcheck
			continue
		}
		pendingPools = append(pendingPools, poolEvalWork{agentIdx: i, sp: sp, poolDir: poolDir, env: env, newDemand: store != nil})
	}

	// Collect work beads with assignees — used for both pool demand and
	// named session on_demand wake. Hoisted out of the store block so
	// the named session section can also use it.
	var assignedWorkBeads []beads.Bead
	var assignedWorkStores []beads.Store
	var assignedWorkStoreRefs []string
	var unassignedRoutedBeads []beads.Bead
	var unassignedRoutedStores []beads.Store
	var unassignedRoutedStoreRefs []string
	var readyUnassignedRoutedWorkBeads []beads.Bead
	var readyUnassignedRoutedWorkStoreRefs []string
	var readyAssigned map[storeScopedBeadKey]bool
	var storePartial bool
	var scaleCheckCounts map[string]int
	var scaleCheckDemandByTemplate map[string]scaleCheckDemand
	var poolScaleCheckPartialTemplates map[string]bool
	var poolPartialRetentionTemplates map[string]bool
	var namedScaleCheckPartialTemplates map[string]bool
	var scaleCheckPartialTemplates map[string]bool
	var namedDefaultDemand map[string]bool
	// Per-store ready snapshots for the demand phase: each probe filters one
	// shared in-memory read per store instead of issuing its own /beads/ready
	// fetch per store per assignee. A snapshot must not span a demand-phase
	// write. The assigned-work pass reads before canonicalizeLegacyBound*
	// rewrites gc.routed_to on open ready work, so it uses its own cache; the
	// scale-check and named-session probes read after those writes and share a
	// second cache created below. See readyDemandCache.
	assignedReadyCache := newReadyDemandCache()
	if store != nil {
		subPhaseStart = time.Now()
		assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs, readyAssigned, storePartial = collectAssignedWorkBeadsWithStores(cityPath, cfg, store, rigStores, suspendedRigPaths, sessionBeads, assignedReadyCache)
		recordDemandSubPhase(trace, "demand_snapshot.collect_assigned_work", subPhaseStart, map[string]any{
			"beads":   len(assignedWorkBeads),
			"partial": storePartial,
		})
		if storePartial {
			fmt.Fprintf(stderr, "assignedWorkBeads: PARTIAL — store query failed, drain decisions suppressed\n") //nolint:errcheck
		}
		if len(assignedWorkBeads) > 0 {
			fmt.Fprintf(stderr, "assignedWorkBeads: %d beads found\n", len(assignedWorkBeads)) //nolint:errcheck
			for _, wb := range assignedWorkBeads {
				fmt.Fprintf(stderr, "  %s assignee=%s routed=%s status=%s\n", wb.ID, wb.Assignee, wb.Metadata[beadmeta.RoutedToMetadataKey], wb.Status) //nolint:errcheck
			}
		} else {
			fmt.Fprintf(stderr, "assignedWorkBeads: 0 beads (rigStores=%d)\n", len(rigStores)) //nolint:errcheck
		}
		// Durably record which session is executing each in-progress work
		// bead. The Assignee link is transient (cleared on close), so without
		// this a completed run carries no session/worktree reference. See
		// stampRunSessionIdentity. Unlike drain decisions, this is not gated on
		// storePartial: stamping the beads that WERE collected is always
		// correct, and any bead missed by a partial query simply gets stamped
		// on a later tick.
		stampRunSessionIdentity(assignedWorkBeads, assignedWorkStores, sessionBeads, stderr)
		// Re-home work pre-assigned to a legacy template identity onto the
		// configured canonical identity, so the canonical session the awake/scale
		// accounting wakes for it can actually surface and claim it (the
		// agent-side work_query/claim path matches identities by raw string).
		canonicalizeLegacyBoundAssignedWork(cfg, assignedWorkBeads, assignedWorkStores, sessionBeads, stderr)
		// Re-home open, unassigned work still routed to a legacy template identity.
		// This is the demand/claim half of the migration: empty-assignee open work
		// never enters the assigned-work collection above, and the canonical
		// pool-demand probe below (defaultScaleCheckCounts) plus the worker
		// work_query/claim path match gc.routed_to canonically by raw string, so
		// the route must be canonicalized before demand is counted or the cold
		// pool never wakes for it.
		subPhaseStart = time.Now()
		var unassignedRoutedPartial bool
		unassignedRoutedBeads, unassignedRoutedStores, unassignedRoutedStoreRefs, unassignedRoutedPartial = collectOpenUnassignedRoutedWork(cityPath, cfg, store, rigStores, suspendedRigPaths, stderr)
		canonicalizeLegacyBoundUnassignedRoutedWork(cfg, unassignedRoutedBeads, unassignedRoutedStores, stderr)
		// Same pass, same reason, different legacy form: a route stamped at a
		// live slot ("<base>-N") is a load-balancing HINT that every raw reader
		// — the generated query's --metadata-field, the claim's string compare —
		// treats as a different target entirely. Collapsing it here, before
		// demand is counted below, is what makes the row both countable and
		// claimable in the same tick instead of neither.
		collapseSlotSuffixedRoutedWork(cfg, unassignedRoutedBeads, unassignedRoutedStores, stderr)
		repairControlDispatcherRoutesForStoreScope(cityPath, cfg, unassignedRoutedBeads, unassignedRoutedStores, unassignedRoutedStoreRefs, stderr)
		// canonicalizeLegacyBound* above rewrote gc.routed_to on open ready
		// work, so the assigned-work snapshot is now stale for demand
		// bucketing. Read the post-rewrite state from a fresh per-store
		// snapshot: reusing assignedReadyCache would bucket demand from the
		// pre-rewrite legacy routes and miss the canonical cold pool, because
		// an explicit-handle CachingStore returns its memoized pre-write live
		// snapshot as the authoritative demand read.
		demandReadyCache := newReadyDemandCache()
		controlDispatcherOpenDemand := openControlDispatcherDemand(cfg, unassignedRoutedBeads)
		recordDemandSubPhase(trace, "demand_snapshot.collect_unassigned_routed", subPhaseStart, map[string]any{
			"beads": len(unassignedRoutedBeads),
		})
		subPhaseStart = time.Now()
		scaleCheckCounts, poolScaleCheckPartialTemplates = evaluatePendingPoolsMap(cfg, pendingPools, stderr, trace)
		recordDemandSubPhase(trace, "demand_snapshot.evaluate_pending_pools", subPhaseStart, map[string]any{
			"pools": len(pendingPools),
		})
		if len(defaultScaleTargets) > 0 {
			subPhaseStart = time.Now()
			defaultCounts, defaultDemand, partialTemplates, errs := defaultScaleCheckCountsAndDemand(cfg, defaultScaleTargets, demandReadyCache)
			recordDemandSubPhase(trace, "demand_snapshot.default_scale_demand", subPhaseStart, map[string]any{
				"targets": len(defaultScaleTargets),
			})
			for _, err := range errs {
				// defaultScaleCheckCounts wraps Ready() failures with
				// enough context to keep this generic outer log honest
				// about partial demand rather than claiming the demand is
				// necessarily zero.
				fmt.Fprintf(stderr, "buildDesiredState: %v (counts above may be a partial of one demand source)\n", err) //nolint:errcheck
			}
			poolScaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(poolScaleCheckPartialTemplates, partialTemplates)
			if scaleCheckCounts == nil {
				scaleCheckCounts = make(map[string]int)
			}
			if scaleCheckDemandByTemplate == nil {
				scaleCheckDemandByTemplate = make(map[string]scaleCheckDemand)
			}
			for template, count := range defaultCounts {
				// A cold-pool wake probe only wakes the pool from zero; clamp its
				// contribution to 1 so it never overrides a custom scale_check's
				// authoritative count for the same template.
				if coldWakeTemplates[template] && count > 1 {
					count = 1
				}
				// An on_demand named-session backing template is a singleton: one pool
				// slot is enough to drain N queued routed tasks sequentially. Clamp to
				// 1 so N unassigned gc.routed_to beads do not spawn {name}-N phantoms.
				if namedOnDemandTemplates[template] && count > 1 {
					count = 1
				}
				if count > scaleCheckCounts[template] {
					scaleCheckCounts[template] = count
				}
				scaleCheckDemandByTemplate[template] = mergeScaleCheckDemand(scaleCheckDemandByTemplate[template], defaultDemand[template], count)
			}
		}
		poolPartialRetentionTemplates = mergeScaleCheckPartialTemplates(poolPartialRetentionTemplates, poolScaleCheckPartialTemplates)
		if len(controlDispatcherOpenDemand) > 0 {
			if scaleCheckCounts == nil {
				scaleCheckCounts = make(map[string]int)
			}
			for template, hasDemand := range controlDispatcherOpenDemand {
				if hasDemand && scaleCheckCounts[template] < 1 {
					scaleCheckCounts[template] = 1
				}
			}
		}
		if unassignedRoutedPartial {
			// The unassigned-routed live read failed, so controlDispatcherOpenDemand
			// above is a partial (possibly empty) view — not proof of zero demand.
			// Mark every deterministic control-dispatcher template for retention so
			// a running dispatcher survives this tick. This is intentionally not a
			// create-suppression marker: another healthy store may have proved real
			// control demand that justifies starting a cold dispatcher (gc-ft31x.2).
			poolPartialRetentionTemplates = markControlDispatcherTemplatesPartial(cfg, poolPartialRetentionTemplates)
		}
		readyUnassignedRoutedWorkBeads, readyUnassignedRoutedWorkStoreRefs = selectReadyUnassignedRoutedWork(
			unassignedRoutedBeads,
			unassignedRoutedStoreRefs,
			scaleCheckDemandByTemplate,
		)
		if len(defaultNamedScaleTargets) > 0 {
			var namedErrs []error
			var partialTemplates map[string]bool
			subPhaseStart = time.Now()
			namedDefaultDemand, partialTemplates, namedErrs = defaultNamedSessionDemand(defaultNamedScaleTargets, cfg, cityName, demandReadyCache)
			recordDemandSubPhase(trace, "demand_snapshot.named_session_demand", subPhaseStart, map[string]any{
				"targets": len(defaultNamedScaleTargets),
			})
			for _, err := range namedErrs {
				fmt.Fprintf(stderr, "buildDesiredState: %v (using named demand=false)\n", err) //nolint:errcheck
			}
			namedScaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(namedScaleCheckPartialTemplates, partialTemplates)
		}
		scaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(scaleCheckPartialTemplates, poolPartialRetentionTemplates)
		scaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(scaleCheckPartialTemplates, namedScaleCheckPartialTemplates)
		if len(scaleCheckPartialTemplates) > 0 {
			fmt.Fprintf(stderr, "scaleCheck: PARTIAL — scale_check failed for %s, retaining affected sessions\n", strings.Join(sortedBoolMapKeys(scaleCheckPartialTemplates), ",")) //nolint:errcheck
		}
		poolWorkBeads := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, store, sessionBeads.OpenInfos(), assignedWorkBeads, assignedWorkStoreRefs)
		bp.assignedWorkBeads = poolWorkBeads
		bp.poolScaleCheckPartialTemplates = poolScaleCheckPartialTemplates
		bp.providerHealthSnapshot = loadProviderHealthSnapshot(cityPath)
		poolDesiredStates := ComputePoolDesiredStatesWithDemandTracedAt(
			cfg,
			poolWorkBeads,
			sessionBeads.OpenInfos(),
			scaleCheckCounts,
			scaleCheckDemandByTemplate,
			poolDecisionTime,
			trace,
		)
		bp.configurePoolSessionCreateFairShare(poolDesiredStates)
		for _, poolState := range poolDesiredStates {
			cfgAgent := findAgentByTemplate(cfg, poolState.Template)
			if cfgAgent == nil {
				fmt.Fprintf(stderr, "buildDesiredState: pool %q has demand but no matching agent in config (skipping)\n", poolState.Template) //nolint:errcheck
				continue
			}
			if agentInSuspendedRig(cityPath, cfgAgent, cfg.Rigs, suspendedRigPaths) {
				continue
			}
			realizePoolDesiredSessionsAt(bp, cfgAgent, poolState, poolDecisionTime, desired, stderr)
		}
	} else {
		// No store — use scale_check counts directly.
		scaleCheckCounts, _ = evaluatePendingPoolsMap(cfg, pendingPools, stderr, trace)
		bp.providerHealthSnapshot = loadProviderHealthSnapshot(cityPath)
		for _, pw := range pendingPools {
			cfgAgent := &cfg.Agents[pw.agentIdx]
			desiredCount := scaleCheckCounts[cfgAgent.QualifiedName()]
			for slot := 1; slot <= desiredCount; slot++ {
				instanceAgent, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(cfgAgent, slot)
				fpExtra := buildFingerprintExtra(instanceAgent)
				tp, err := resolveTemplatePrepared(bp, instanceAgent, qualifiedInstance, fpExtra)
				if err != nil {
					fmt.Fprintf(stderr, "buildDesiredState: pool instance %q: %v (skipping)\n", qualifiedInstance, err) //nolint:errcheck
					continue
				}
				tp.PoolSlot = poolSlot
				if usesTransientPoolSlotIdentity(cfgAgent) {
					clearPoolTemplateRuntimeIdentity(&tp)
				} else {
					setTemplateEnvIdentity(&tp, qualifiedInstance)
				}
				installAgentSideEffects(bp, instanceAgent, tp, stderr)
				desired[tp.SessionName] = tp
			}
		}
	}

	// Named sessions: materialize session beads for configured [[named_session]]
	// entries. "always" mode sessions are unconditionally materialized;
	// "on_demand" sessions are materialized only when they already have a
	// canonical bead or direct assigned work.
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
	// namedWorkBeadID carries the concrete assigned-work-bead ID matched below,
	// for identities that have one. It is intentionally left unpopulated for
	// identities that only satisfy namedDefaultDemand (no bead-specific signal
	// available there) — see TemplateParams.BoundStepID.
	namedWorkBeadID := make(map[string]string, len(namedSpecs))
	for identity := range namedDefaultDemand {
		if _, ok := namedSpecs[identity]; ok {
			namedWorkReady[identity] = true
		}
	}
	// Check assigned work beads: if any work bead's Assignee matches a named
	// session's identity, that session has direct demand.
	//
	// Raw gc.routed_to metadata is intentionally NOT treated as direct named
	// demand here. The controller only uses assignment/readiness state; routed
	// metadata is consumed by the agent-side gc hook path.
	//
	// namedClaimRefs widens the reachability test below the same way the pool
	// demand tier is widened at :862 and the wake filter already is: a claim
	// recorded under a relocated "class:*" binding ref matches no rig name, so a
	// rig-scoped named session that dies holding one is otherwise never resumed
	// even though the wake side still retains the holder (ga-whzrt, one tier
	// over). It is a city-level property, so resolve it once — and only when
	// there is both work to match and a named spec to match it — mirroring the
	// pool filter's guards so an idle city does no topology work.
	// assignedWorkRelocatedClaimRefs answers nil on a single-store city (nothing
	// relocated) and degrades a nil store to nil refs, so this never widens the
	// rig gate where the base rig-equality test is all that is correct.
	var namedClaimRefs []string
	if len(assignedWorkBeads) > 0 && len(namedSpecs) > 0 {
		namedClaimRefs = assignedWorkRelocatedClaimRefs(cityPath, cfg, store)
	}
	for identity, spec := range namedSpecs {
		for i, wb := range assignedWorkBeads {
			// in_progress work is always actionable; open work is direct named
			// demand only when it passed the store's readiness/deps gate. Without
			// the readiness check, an open assigned bead that entered the snapshot
			// via the open-routed orphan-release pass (no deps gate) would keep an
			// on-demand named session awake forever even while blocked.
			switch wb.Status {
			case "in_progress":
			case "open":
				ref := ""
				if i < len(assignedWorkStoreRefs) {
					ref = assignedWorkStoreRefs[i]
				}
				if !readyAssigned[storeScopedBeadKey{StoreRef: ref, ID: wb.ID}] {
					continue
				}
			default:
				continue
			}
			assignee := strings.TrimSpace(wb.Assignee)
			if !namedSessionAssigneeMatchesSpec(spec, identity, assignee) {
				continue
			}
			// ga-i1d0tr Candidate B: a bare-template Assignee used to be
			// distrusted for templates supporting expanded per-instance
			// identities (a multi-slot pool or namepool coexisting with this
			// named session), because pool's wake-known-identity tier had no
			// awareness of cfg.NamedSessions and could independently wake a
			// competing pool worker for the same bare identity. That read-side
			// ambiguity is now resolved structurally at the source
			// (isConfiguredNamedSessionIdentity, pool_desired_state.go): pool
			// can no longer generate wake-known-identity demand for a
			// configured named session's own bare identity, so this bare match
			// is trustworthy unconditionally — no per-template-shape guard
			// needed here anymore (ga-p0u752).
			if !assignedWorkIndexReachableFromAgentOnClaimRefs(cityPath, cfg, spec.Agent, assignedWorkStoreRefs, i, namedClaimRefs) {
				continue
			}
			fmt.Fprintf(stderr, "namedWorkReady: %s matched by bead %s (assignee=%s status=%s)\n", identity, wb.ID, assignee, wb.Status) //nolint:errcheck
			namedWorkReady[identity] = true
			namedWorkBeadID[identity] = wb.ID
			break
		}
	}
	if len(assignedWorkBeads) > 0 {
		fmt.Fprintf(stderr, "namedWorkReady: %d assigned beads, %d named specs, ready=%v\n", len(assignedWorkBeads), len(namedSpecs), namedWorkReady) //nolint:errcheck
	}
	// NamedSessionRoutedDemand: routed (unassigned) scale-check demand on the
	// backing template, independent of direct assignee demand above. See the
	// field doc on DesiredStateResult.NamedSessionRoutedDemand.
	// Canonical singleton backing pools only. This signal exists solely to
	// compensate for alias suppression, and alias suppression applies exactly to
	// canonical singleton identities (see canonicalSingletonAliasHeldTemplates).
	// A multi-instance backing pool can serve routed demand with an ordinary
	// standby, so waking the named holder there would wake it AND mint the
	// standby — the overprovisioning this signal is meant to prevent.
	namedRoutedDemand := make(map[string]bool, len(namedSpecs))
	for identity, spec := range namedSpecs {
		if !spec.Agent.UsesCanonicalSingletonPoolIdentity() {
			continue
		}
		if scaleCheckCounts[namedSessionBackingTemplate(spec)] > 0 {
			namedRoutedDemand[identity] = true
		}
	}
	for identity, spec := range namedSpecs {
		canonicalInfo, hasCanonical := findCanonicalNamedSessionInfo(bp.sessionBeads, spec)
		if !hasCanonical {
			if _, conflict := findNamedSessionConflictInfo(bp.sessionBeads, spec); conflict {
				continue
			}
		}
		if spec.Mode != "always" && !hasCanonical && !namedWorkReady[identity] {
			continue
		}
		fpExtra := buildFingerprintExtra(spec.Agent)
		tp, err := resolveTemplatePrepared(bp, spec.Agent, identity, fpExtra)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: named session %q: %v (skipping)\n", identity, err) //nolint:errcheck
			continue
		}
		tp.Alias = identity
		tp.TemplateName = namedSessionBackingTemplate(spec)
		tp.InstanceName = identity
		tp.ConfiguredNamedIdentity = identity
		tp.ConfiguredNamedMode = spec.Mode
		tp.BoundStepID = namedWorkBeadID[identity]
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
			if sn := strings.TrimSpace(canonicalInfo.SessionNameMetadata); sn != "" {
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
	applySessionBeadDesiredOverlay(bp, cfg, desired, suspendedRigPaths, poolPartialRetentionTemplates, namedScaleCheckPartialTemplates, stderr)

	var continuationClaimCandidates []ContinuationClaimCandidate
	continuationClaimQueryPartial := storePartial
	if !storePartial {
		continuationClaimCandidates, continuationClaimQueryPartial = selectReadyContinuationClaimCandidates(
			cityName,
			assignedWorkBeads,
			assignedWorkStores,
			assignedWorkStoreRefs,
			readyAssigned,
		)
	}

	sessionSnapshotComplete := bp.hasCompleteSessionSnapshot()
	sessionOccupancyInfos := make([]session.Info, len(allOpenSessionInfos))
	copy(sessionOccupancyInfos, allOpenSessionInfos)
	return DesiredStateResult{
		State:                              desired,
		BaseState:                          baseDesired,
		ScaleCheckCounts:                   scaleCheckCounts,
		ScaleCheckPartialTemplates:         scaleCheckPartialTemplates,
		PoolScaleCheckPartialTemplates:     poolScaleCheckPartialTemplates,
		PoolPartialRetentionTemplates:      poolPartialRetentionTemplates,
		NamedScaleCheckPartialTemplates:    namedScaleCheckPartialTemplates,
		AssignedWorkBeads:                  assignedWorkBeads,
		AssignedWorkStores:                 assignedWorkStores,
		AssignedWorkStoreRefs:              assignedWorkStoreRefs,
		ReadyUnassignedRoutedWorkBeads:     readyUnassignedRoutedWorkBeads,
		ReadyUnassignedRoutedWorkStoreRefs: readyUnassignedRoutedWorkStoreRefs,
		ReadyAssigned:                      readyAssigned,
		ContinuationClaimCandidates:        continuationClaimCandidates,
		ContinuationClaimQueryPartial:      continuationClaimQueryPartial,
		NamedSessionDemand:                 namedWorkReady,
		NamedSessionRoutedDemand:           namedRoutedDemand,
		StoreQueryPartial:                  storePartial,
		SessionQueryPartial:                store != nil && !sessionSnapshotComplete,
		SessionSnapshotComplete:            sessionSnapshotComplete,
		SessionOccupancyInfos:              sessionOccupancyInfos,
		BeaconTime:                         beaconTime,
	}
}

func buildSuspendedRigPathsForCity(cfg *config.City, cityPath string) map[string]bool {
	if cfg == nil || len(cfg.Rigs) == 0 {
		return nil
	}
	var suspState suspensionstate.State
	if cityPath != "" {
		suspState, _ = loadSuspensionState(fsys.OSFS{}, cityPath)
	}
	suspNames := buildEffectiveSuspendedRigNames(cfg, suspState)
	if len(suspNames) == 0 {
		return nil
	}
	suspendedRigPaths := make(map[string]bool)
	for _, r := range cfg.Rigs {
		if suspNames[r.Name] {
			suspendedRigPaths[filepath.Clean(r.Path)] = true
		}
	}
	return suspendedRigPaths
}

// collectAllOpenSessionInfos gathers every open session bead across the session
// census legs and projects each onto session.Info at the collection edge, so no
// raw *beads.Bead escapes into the running-session counting loop. Closed beads
// are dropped (equivalently: projected Info with .Closed true). Partial-result
// errors still contribute their partial slice and join into the returned error;
// any hard error is returned with an empty slice for that store.
//
// The fold is FIRST-LEG-WINS by session id, which the pre-resolver arm did not
// need: it read one store per scope, so no id could appear twice. The session
// plan leads with the sessions binding and keeps the work ledger behind it, and
// a migrated city legitimately holds the same session bead in both — the
// migration preserves ids and never deletes back. Counting that session twice
// would make a pool read as over-scaled and reap a live worker.
func collectAllOpenSessionInfos(
	cityPath string,
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
) ([]session.Info, error) {
	return collectOpenSessionInfos(cityPath, cfg, cityStore, rigStores, suspendedRigPaths, true, false)
}

// collectAllOpenSessionAvailabilityInfos preserves distinct identifier
// projections of the same stable bead ID across census legs. The ordinary
// reconciler census is first-leg-wins because duplicated migrated rows must be
// counted once; an availability proof is stricter because an old alias/name in
// either copy must remain reserved until every copy agrees.
func collectAllOpenSessionAvailabilityInfos(
	cityPath string,
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
) ([]session.Info, error) {
	// This read runs only inside the complete identifier lock set. Live bypasses
	// production CachingStore snapshots so an external store mutation committed
	// after the planning census participates in the lock-time proof.
	return collectOpenSessionInfos(cityPath, cfg, cityStore, rigStores, suspendedRigPaths, false, true)
}

func collectOpenSessionInfos(
	cityPath string,
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	firstLegWins bool,
	live bool,
) ([]session.Info, error) {
	// Sessions arm of the reconciler frame, over the Plan(Session) leg set: the
	// sessions binding, the city work store, then the serving rigs.
	// CachingStore-wrapped stores are used when available.
	stores, legErr := sessionCensusStoreCandidates(cityPath, cfg, cityStore, rigStores, suspendedRigPaths)
	if legErr != nil {
		// A refused city. Undercounting running sessions misclassifies a pool as
		// cold, so this is reported rather than answered with zero.
		return nil, legErr
	}

	type storeResult struct {
		infos []session.Info
		err   error
		ref   string
	}
	results := make([]storeResult, len(stores))
	var wg sync.WaitGroup
	for idx, source := range stores {
		idx, source := idx, source
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Per-leg default direct union (session front door over the candidate's
			// store, CachingStore-wrapped when available) — same tier as the prior
			// raw ListAllSessionBeads, projected to Info. Partial-result rows are
			// still returned alongside the error, so the fold below preserves them.
			infos, err := sessionFrontDoor(source.store).ListAll(session.ListAllOptions{Live: live})
			results[idx] = storeResult{infos: infos, err: err, ref: source.ref}
		}()
	}
	wg.Wait()

	var allInfos []session.Info
	var errs []error
	seen := make(map[string]struct{})
	keep := func(infos []session.Info) {
		for _, info := range infos {
			if info.Closed {
				continue
			}
			if id := strings.TrimSpace(info.ID); firstLegWins && id != "" {
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
			}
			allInfos = append(allInfos, info)
		}
	}
	for _, r := range results {
		if r.err != nil {
			ref := strings.TrimSpace(r.ref)
			if ref == "" {
				ref = "<unscoped>"
			}
			errs = append(errs, fmt.Errorf("session census leg %q: %w", ref, r.err))
			if beads.IsPartialResult(r.err) {
				keep(r.infos)
			}
			continue
		}
		keep(r.infos)
	}
	if len(errs) > 0 {
		return allInfos, errors.Join(errs...)
	}
	return allInfos, nil
}

// cloneSessionCensusRigStores freezes a constructor INPUT for a later
// lock-time re-read. residency:allow — this is immutable snapshot plumbing,
// not residency discovery: it preserves the caller-provided name/store pairs
// byte-for-byte; it neither resolves ownership nor discovers or reads stores,
// consults no config, binding, namespace, or leg order, and its result goes
// only back through sessionCensusStoreCandidates/storeref.Plan under the full
// identifier-lock set.
func cloneSessionCensusRigStores(src map[string]beads.Store) map[string]beads.Store {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]beads.Store, len(src))
	for name, store := range src {
		dst[name] = store
	}
	return dst
}

func cloneSessionCensusSuspendedRigPaths(src map[string]bool) map[string]bool {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]bool, len(src))
	for path, suspended := range src {
		dst[path] = suspended
	}
	return dst
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
	poolScaleCheckPartialTemplates map[string]bool,
	namedScaleCheckPartialTemplates map[string]bool,
	stderr io.Writer,
) {
	realizedRoots := discoverSessionBeadsWithRoots(bp, cfg, desired, suspendedRigPaths, poolScaleCheckPartialTemplates, namedScaleCheckPartialTemplates, stderr)
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
	bp.sessionSnapshotCompletenessKnown = true
	bp.sessionSnapshotComplete = false
	bp.sessionOccupancyInfos = sessionOccupancyInfosForRefresh(result, sessionBeads)
	applySessionBeadDesiredOverlay(bp, cfg, refreshed.State, buildSuspendedRigPathsForCity(cfg, cityPath), effectivePoolPartialRetentionTemplates(result), result.NamedScaleCheckPartialTemplates, stderr)
	return refreshed
}

// sessionOccupancyInfosForRefresh carries the original cross-store census through
// a post-tick desired-state refresh as conservative reuse/occupancy evidence.
// A refresh never marks that evidence complete for fresh creation: it reloads
// only the primary sessions store, while Session residency is the union of the
// sessions binding, city work store, and rig work stores. A foreign holder may
// have appeared after the full build census. Fresh pool and dependency rows are
// therefore deferred until the next full build; proven rows may still be reused.
func sessionOccupancyInfosForRefresh(result DesiredStateResult, sessionBeads *sessionBeadSnapshot) []session.Info {
	if result.SessionOccupancyInfos != nil || result.SessionSnapshotComplete {
		infos := make([]session.Info, len(result.SessionOccupancyInfos))
		copy(infos, result.SessionOccupancyInfos)
		return infos
	}
	if result.SessionQueryPartial || sessionBeads == nil || sessionBeads.LoadError() != nil {
		return nil
	}
	return sessionBeads.OpenInfos()
}

// collectAssignedWorkBeads queries ONE store for actionable assigned work. It
// includes in-progress assigned work plus open assigned work that is actually
// ready. Routed-but-unassigned pool queue work is intentionally excluded here,
// except stranded in-progress pool work with no assignee is included so
// reconciliation can reopen it for normal claiming.
//
// The city path is empty by construction, which makes this the SINGLE-STORE
// form: no city path means no registered runtime and no funnel, so the census
// resolves to the one store handed in. Callers that need the city's real leg
// set — every production caller — use collectAssignedWorkBeadsWithStores.
func collectAssignedWorkBeads(
	cfg *config.City,
	cityStore beads.Store,
) ([]beads.Bead, bool) {
	result, _, _, _, partial := collectAssignedWorkBeadsWithStores("", cfg, cityStore, nil, nil, nil)
	return result, partial
}

// collectAssignedWorkBeadsWithStores returns the actionable assigned-work
// snapshot plus index-aligned stores/storeRefs, the store-scoped set of beads
// that carry real wake-demand readiness (readyAssigned), and a partial flag.
// readyAssigned holds only beads admitted by the in-progress pass, the
// assigned-molecule pass, and the store Ready()/deps pass — never the
// open-routed orphan-release pass, whose beads have not passed a readiness
// gate and must not, by status alone, hold a session awake. It is keyed by
// store ref + bead ID so a ready bead in one store cannot mark a blocked open
// bead with the same ID in another store as ready (storeScopedBeadKey).
func collectAssignedWorkBeadsWithStores(
	cityPath string,
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	sessionBeads *sessionBeadSnapshot,
	caches ...*readyDemandCache,
) ([]beads.Bead, []beads.Store, []string, map[storeScopedBeadKey]bool, bool) {
	cache := optionalReadyDemandCache(caches)
	// Work arm of the reconciler frame, over the Plan(Census) leg set: the city
	// work store under the empty store-ref, the serving rigs under their names,
	// then every relocated class binding under its own "class:*" ref. The refs
	// keep the index-aligned workBeads/workStores slices per-bead aligned for the
	// canonicalize/stamp writers, and naming the binding distinctly is what lets
	// a dead claim on a city-scope WORK bead be captured at all (D6).
	// CachingStore-wrapped stores are used; creating raw bdStoreForCity per rig
	// spawns bd subprocesses on every tick, saturating dolt.
	stores, legErr := censusStoreCandidates(cityPath, cfg, cityStore, rigStores, suspendedRigPaths, censusRefBare)
	if legErr != nil {
		// A refused city has told this scan nothing, and an empty assigned-work
		// snapshot presented as complete is the drain-a-live-holder shape. Report
		// it partial so every consumer retains rather than reaps.
		return nil, nil, nil, nil, true
	}

	type storeAssignedWorkResult struct {
		ref       string // store ref these beads came from (empty = city store)
		beads     []beads.Bead
		stores    []beads.Store
		storeRefs []string
		readyIDs  map[string]bool
		errs      []error
	}
	results := make([]storeAssignedWorkResult, len(stores))
	var wg sync.WaitGroup
	for idx, source := range stores {
		idx, source := idx, source
		wg.Add(1)
		go func() {
			defer wg.Done()
			var result []beads.Bead
			var resultStores []beads.Store
			var resultStoreRefs []string
			var errs []error
			readyIDs := make(map[string]bool)
			seen := make(map[string]struct{})
			// In-progress beads with an assignee (active work), plus stranded
			// unassigned pool work that needs to be reopened. This pass runs
			// across every store before any ready handoff probes, so already
			// active work never waits behind unrelated ready scans.
			if inProgress, err := listBothTiersForControllerDemand(source.store, beads.ListQuery{Status: "in_progress"}); err == nil {
				appendInProgressWorkUnique(cfg, &result, &resultStores, &resultStoreRefs, readyIDs, inProgress, seen, source.store, source.ref)
			} else {
				errs = append(errs, fmt.Errorf("List(in_progress): %w", err))
				if beads.IsPartialResult(err) && len(inProgress) > 0 {
					appendInProgressWorkUnique(cfg, &result, &resultStores, &resultStoreRefs, readyIDs, inProgress, seen, source.store, source.ref)
				}
			}
			// Open assigned molecule roots that count as wake demand. Whether an
			// open assigned root is demand must be decided from the bead's RAW
			// status, not the collapsed Bead.Status: mapBdStatus folds bd's
			// blocked/deferred/review/testing into "open", so a blocked assigned
			// root reads as "open" through the cache and would wrongly re-enter
			// demand (EB-42o8/gc-nz5i; extends gc-4zb/#4395). A Live read reaches
			// the backing store's raw --status=open filter, which excludes it —
			// see listOpenForControllerDemandLive.
			if openDemand, err := listOpenForControllerDemandLive(source.store); err == nil {
				appendOpenAssignedMoleculeWorkUnique(&result, &resultStores, &resultStoreRefs, readyIDs, openDemand, seen, source.store, source.ref)
			} else {
				errs = append(errs, fmt.Errorf("List(open, live demand): %w", err))
				if beads.IsPartialResult(err) && len(openDemand) > 0 {
					appendOpenAssignedMoleculeWorkUnique(&result, &resultStores, &resultStoreRefs, readyIDs, openDemand, seen, source.store, source.ref)
				}
			}
			// Open pool-routed beads that still carry an assignee. These are
			// invisible to the in-progress pass (status is "open") and to the
			// ready-by-assignee pass (the assignee is a dead session's
			// long-form id, not enumerated by readyAssignedWorkAssignees).
			// Without this pass, graph.v2 step beads orphaned by a session
			// drain stay assigned forever and releaseOrphanedPoolAssignments
			// never sees them — pool demand stays at 0 and the workflow stalls
			// (issue #2793). The release loop further gates each bead on
			// openSessionOwnsWork / liveOpenSessionAssignmentExists, so
			// live-session step beads in the same range are skipped untouched.
			//
			// This read stays on the collapsed-status cache tier ON PURPOSE. The
			// gc-ft31x fix narrows only the DEMAND read above to live and leaves
			// the blocked-routed reaper's input (appendOpenRoutedWorkUnique ->
			// releaseOrphanedPoolAssignments) exactly as it was, so nothing the
			// reaper relied on is removed — "do not remove the blocked-routed
			// reaper until a reviewed binary is deployed" (gc-ft31x). A blocked
			// bead captured here is not counted as demand regardless:
			// appendOpenRoutedWorkUnique never markReadyAssigned (see the
			// skipReadyAssignees note below), and releaseOrphanedPoolAssignments'
			// own live re-read (liveWorkAssignmentStillReleasable) skips it.
			if openRouted, err := listBothTiersForControllerDemand(source.store, beads.ListQuery{Status: "open"}); err == nil {
				appendOpenRoutedWorkUnique(&result, &resultStores, &resultStoreRefs, openRouted, seen, source.store, source.ref)
			} else {
				errs = append(errs, fmt.Errorf("List(open): %w", err))
				if beads.IsPartialResult(err) && len(openRouted) > 0 {
					appendOpenRoutedWorkUnique(&result, &resultStores, &resultStoreRefs, openRouted, seen, source.store, source.ref)
				}
			}
			results[idx] = storeAssignedWorkResult{ref: source.ref, beads: result, stores: resultStores, storeRefs: resultStoreRefs, readyIDs: readyIDs, errs: errs}
		}()
	}
	wg.Wait()

	var result []beads.Bead
	var resultStores []beads.Store
	var resultStoreRefs []string
	// readyAssigned is the store-scoped wake-demand verdict so neither the awake
	// bridge nor the assignee-probe-skip set below can mistake a blocked open
	// rig bead for a ready city bead with the same ID. It is keyed by store ref
	// + bead ID (storeScopedBeadKey).
	readyAssigned := make(map[storeScopedBeadKey]bool)
	var partial bool
	for _, r := range results {
		result = append(result, r.beads...)
		resultStores = append(resultStores, r.stores...)
		resultStoreRefs = append(resultStoreRefs, r.storeRefs...)
		for id := range r.readyIDs {
			readyAssigned[storeScopedBeadKey{StoreRef: r.ref, ID: id}] = true
		}
		for _, err := range r.errs {
			log.Printf("collectAssignedWorkBeads: %v", err)
			partial = true
		}
	}
	// Skip the Ready handoff probe only for assignees that already have a
	// GENUINELY-ready captured bead in that store (in-progress, molecule root,
	// or a prior Ready() match) — never for an assignee whose only captured
	// bead came from the open-routed orphan-release pass. Those beads carry no
	// readiness verdict; skipping their assignee would silently treat blocked
	// work as live demand. The lookup is store-scoped (result and
	// resultStoreRefs are index-aligned), so a ready bead in one store cannot
	// suppress the Ready probe for a same-ID assignee in another store.
	skipReadyAssignees := readyCapturedAssigneeSet(result, resultStoreRefs, readyAssigned)
	expandSkipAssigneesWithSessionIdentities(skipReadyAssignees, sessionBeads)
	assignees := readyAssignedWorkAssignees(cfg, sessionBeads, skipReadyAssignees)
	if len(skipReadyAssignees) > 0 && len(assignees) == 0 {
		return result, resultStores, resultStoreRefs, readyAssigned, partial
	}

	readyResults := make([]storeAssignedWorkResult, len(stores))
	for idx, source := range stores {
		idx, source := idx, source
		wg.Add(1)
		go func() {
			defer wg.Done()
			var ready []beads.Bead
			var err error
			var errs []error
			if len(assignees) == 0 {
				ready, err = cache.liveReady(source.store, beads.ReadyQuery{Limit: assignedWorkReadyLimit(cfg)})
				if err != nil {
					errs = append(errs, fmt.Errorf("Ready(): %w", err))
				}
			} else {
				for _, assignee := range assignees {
					part, partErr := cache.liveReady(source.store, beads.ReadyQuery{Assignee: assignee, Limit: assignedWorkReadyLimit(cfg)})
					if partErr != nil {
						errs = append(errs, fmt.Errorf("Ready(assignee=%q): %w", assignee, partErr))
					}
					ready = append(ready, part...)
				}
			}
			var readyBeads []beads.Bead
			var readyStores []beads.Store
			var readyStoreRefs []string
			readyIDs := make(map[string]bool)
			seen := make(map[string]struct{})
			appendAssignedUnique(&readyBeads, &readyStores, &readyStoreRefs, readyIDs, ready, seen, source.store, source.ref)
			readyResults[idx] = storeAssignedWorkResult{ref: source.ref, beads: readyBeads, stores: readyStores, storeRefs: readyStoreRefs, readyIDs: readyIDs, errs: errs}
		}()
	}
	wg.Wait()
	for _, r := range readyResults {
		result = append(result, r.beads...)
		resultStores = append(resultStores, r.stores...)
		resultStoreRefs = append(resultStoreRefs, r.storeRefs...)
		for id := range r.readyIDs {
			readyAssigned[storeScopedBeadKey{StoreRef: r.ref, ID: id}] = true
		}
		for _, err := range r.errs {
			log.Printf("collectAssignedWorkBeads: %v", err)
			partial = true
		}
	}
	return result, resultStores, resultStoreRefs, readyAssigned, partial
}

func assignedWorkReadyLimit(cfg *config.City) int {
	if cfg == nil {
		return config.DefaultMaxWakesPerTick
	}
	return cfg.Daemon.MaxWakesPerTickOrDefault()
}

// readyCapturedAssigneeSet returns the assignees of work beads that have
// already been captured WITH a readiness verdict in their own store (their
// store-scoped key is in readyAssigned): in-progress work, assigned molecule
// roots, and any prior Ready() match. Beads captured only by the open-routed
// orphan-release pass are excluded, so their assignee is still probed by the
// Ready handoff pass rather than assumed ready by virtue of having been
// collected. The readiness lookup is store-scoped: a ready bead in one store
// never marks a same-ID blocked bead's assignee in another store as
// already-ready (storeScopedBeadKey). work and storeRefs are index-aligned.
func readyCapturedAssigneeSet(work []beads.Bead, storeRefs []string, readyAssigned map[storeScopedBeadKey]bool) map[string]struct{} {
	if len(work) == 0 {
		return nil
	}
	result := make(map[string]struct{})
	for i, bead := range work {
		assignee := strings.TrimSpace(bead.Assignee)
		if assignee == "" {
			continue
		}
		if bead.Status != "open" && bead.Status != "in_progress" {
			continue
		}
		// Fail safe (probe rather than skip) when the index-aligned store ref
		// is missing, so a short slice never suppresses a real Ready probe.
		if i >= len(storeRefs) {
			continue
		}
		if !readyAssigned[storeScopedBeadKey{StoreRef: storeRefs[i], ID: bead.ID}] {
			continue
		}
		result[assignee] = struct{}{}
	}
	return result
}

func expandSkipAssigneesWithSessionIdentities(skip map[string]struct{}, sessionBeads *sessionBeadSnapshot) {
	if len(skip) == 0 || sessionBeads == nil {
		return
	}
	for _, info := range sessionBeads.OpenInfos() {
		ids := sessionBeadAssigneeIdentitiesInfo(info)
		matched := false
		for _, id := range ids {
			if _, ok := skip[id]; ok {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		for _, id := range ids {
			skip[id] = struct{}{}
		}
	}
}

func readyAssignedWorkAssignees(cfg *config.City, sessionBeads *sessionBeadSnapshot, skip map[string]struct{}) []string {
	seen := make(map[string]struct{})
	var result []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := skip[value]; ok {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if sessionBeads != nil {
		for _, info := range sessionBeads.OpenInfos() {
			if info.Closed {
				continue
			}
			for _, id := range sessionBeadAssigneeIdentitiesInfo(info) {
				add(id)
			}
		}
	}
	if cfg != nil {
		for i := range cfg.NamedSessions {
			if cfg.NamedSessions[i].Mode != "on_demand" {
				continue
			}
			identity := cfg.NamedSessions[i].QualifiedName()
			add(identity)
		}
	}
	return result
}

func defaultScaleCheckTargetForAgent(
	cityPath string,
	cfg *config.City,
	agentCfg *config.Agent,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
) defaultScaleCheckTarget {
	target := defaultScaleCheckTarget{
		template: agentCfg.QualifiedName(),
		storeKey: "city",
		store:    cityStore,
	}
	rigName := configuredRigName(cityPath, agentCfg, cfg.Rigs)
	if rigName == "" {
		return target
	}
	target.storeKey = "rig:" + rigName
	if rigStores != nil {
		if rigStore := rigStores[rigName]; rigStore != nil {
			target.store = rigStore
			return target
		}
	}
	target.store = nil
	target.err = fmt.Errorf("default scale_check %s: rig store %q unavailable", target.template, rigName)
	return target
}

// defaultScaleCheckCounts reports ready, unassigned, routed work as fresh
// generic pool demand. Assigned beads are handled by assigned-work collection
// and named-session demand so they are intentionally excluded here. It has no
// production caller that needs gc.routed_to instance-suffix normalization, so
// it passes a nil cfg through to defaultScaleCheckCountsAndDemand; callers
// that need normalization should call defaultScaleCheckCountsAndDemand
// directly with a real *config.City.
func defaultScaleCheckCounts(targets []defaultScaleCheckTarget) (map[string]int, map[string]bool, []error) {
	counts, _, partialTemplates, errs := defaultScaleCheckCountsAndDemand(nil, targets)
	return counts, partialTemplates, errs
}

func defaultScaleCheckCountsAndDemand(cfg *config.City, targets []defaultScaleCheckTarget, caches ...*readyDemandCache) (map[string]int, map[string]scaleCheckDemand, map[string]bool, []error) {
	cache := optionalReadyDemandCache(caches)
	counts := make(map[string]int, len(targets))
	demand := make(map[string]scaleCheckDemand, len(targets))
	if len(targets) == 0 {
		return counts, demand, nil, nil
	}

	type scaleStoreGroup struct {
		store     beads.Store
		storeKey  string
		templates map[string]struct{}
	}
	groups := make(map[string]*scaleStoreGroup)
	var errs []error
	var partialTemplates map[string]bool
	for _, target := range targets {
		template := strings.TrimSpace(target.template)
		if template == "" {
			continue
		}
		counts[template] = 0
		if target.err != nil {
			errs = append(errs, target.err)
			partialTemplates = markScaleCheckPartialTemplate(partialTemplates, template)
		}
		if target.store == nil {
			if target.err == nil {
				errs = append(errs, fmt.Errorf("default scale_check %s: store unavailable", template))
			}
			partialTemplates = markScaleCheckPartialTemplate(partialTemplates, template)
			continue
		}
		key := strings.TrimSpace(target.storeKey)
		if key == "" {
			key = fmt.Sprintf("%p", target.store)
		}
		group := groups[key]
		if group == nil {
			group = &scaleStoreGroup{store: target.store, storeKey: key, templates: make(map[string]struct{})}
			groups[key] = group
		}
		group.templates[template] = struct{}{}
	}

	// countedBeads dedups counted bead IDs per template ACROSS store groups.
	// Bead IDs are unique within a deployment, so a legitimate cross-store
	// union never collides — but when a rig store aliases the city store as a
	// distinct Store object (pointer inequality passes: a legacy unscoped
	// file-store layout, or a rig dir whose missing .beads resolves bd's
	// walk-up to the city DB), the same beads appear in both the "rig:<name>"
	// and "city" groups and would double the template's demand. With the city
	// probe no longer cold-gated, that double-count would be a persistent
	// warm condition rather than a one-tick wake overshoot, so dedup by ID.
	countedBeads := make(map[string]map[string]struct{})
	for key, group := range groups {
		// Ready()/CachedReady() iteration surfaces actionable work
		// matched against gc.routed_to/gc.run_target. Formula orders that
		// should wake pools must create an actionable root, such as a
		// vapor/root-only wisp. Molecule containers and formula step
		// beads remain hidden by readyExcludeTypes.
		ready, readyErr := cache.controllerDemandReady(group.store)
		if readyErr != nil {
			errs = append(errs, fmt.Errorf("default scale_check %s templates=%s: Ready(): %w", key, strings.Join(sortedStringSet(group.templates), ","), readyErr))
			partialTemplates = markScaleCheckPartialSet(partialTemplates, group.templates)
			if !beads.IsPartialResult(readyErr) {
				ready = nil
			}
		}
		for _, b := range ready {
			// AGREEMENT: count only rows a T-worker's own query would serve it.
			// A routed epic, a bead on a dispatch hold, or a slot-suffixed route
			// is not capacity demand — it is a seat that spawns, reads empty and
			// drains, every tick, forever. See demand_serve_predicate.go.
			template, servable := demandServableForTemplates(cfg, b, group.templates)
			if !servable {
				continue
			}
			seen := countedBeads[template]
			if seen == nil {
				seen = make(map[string]struct{})
				countedBeads[template] = seen
			}
			if _, dup := seen[b.ID]; dup {
				continue
			}
			seen[b.ID] = struct{}{}
			counts[template]++
			entry := demand[template]
			entry.Count++
			entry.WorkBeadIDs = append(entry.WorkBeadIDs, b.ID)
			if entry.Titles == nil {
				entry.Titles = make(map[string]string)
			}
			entry.Titles[b.ID] = b.Title
			if pack := strings.TrimSpace(b.Metadata[beadmeta.PackMetadataKey]); pack != "" {
				if entry.Packs == nil {
					entry.Packs = make(map[string]string)
				}
				entry.Packs[b.ID] = pack
			}
			if workspace := strings.TrimSpace(b.Metadata[beadmeta.PackWorkspaceMetadataKey]); workspace != "" {
				if entry.Workspaces == nil {
					entry.Workspaces = make(map[string]string)
				}
				entry.Workspaces[b.ID] = workspace
			}
			if entry.StoreRefs == nil {
				entry.StoreRefs = make(map[string]string)
			}
			entry.StoreRefs[b.ID] = group.storeKey
			spec, specErr := worktreeSpecForBead(b, group.storeKey)
			if specErr != nil {
				if entry.WorktreeErrors == nil {
					entry.WorktreeErrors = make(map[string]string)
				}
				entry.WorktreeErrors[b.ID] = specErr.Error()
			} else if spec != nil {
				if entry.WorktreeSpecs == nil {
					entry.WorktreeSpecs = make(map[string]*worktree.Spec)
				}
				entry.WorktreeSpecs[b.ID] = spec
			}
			if parentSID := strings.TrimSpace(b.Metadata[beadmeta.BrainParentSIDMetadataKey]); parentSID != "" {
				if entry.ParentSIDs == nil {
					entry.ParentSIDs = make(map[string]string)
				}
				entry.ParentSIDs[b.ID] = parentSID
			}
			demand[template] = entry
		}
	}
	return counts, demand, partialTemplates, errs
}

func mergeScaleCheckDemand(existing, incoming scaleCheckDemand, count int) scaleCheckDemand {
	if count <= 0 || len(incoming.WorkBeadIDs) == 0 {
		return existing
	}
	limit := count
	if limit > len(incoming.WorkBeadIDs) {
		limit = len(incoming.WorkBeadIDs)
	}
	if existing.StoreRefs == nil && len(incoming.StoreRefs) > 0 {
		existing.StoreRefs = make(map[string]string, len(incoming.StoreRefs))
	}
	if existing.Titles == nil && len(incoming.Titles) > 0 {
		existing.Titles = make(map[string]string, len(incoming.Titles))
	}
	if existing.Packs == nil && len(incoming.Packs) > 0 {
		existing.Packs = make(map[string]string, len(incoming.Packs))
	}
	if existing.Workspaces == nil && len(incoming.Workspaces) > 0 {
		existing.Workspaces = make(map[string]string, len(incoming.Workspaces))
	}
	if existing.ParentSIDs == nil && len(incoming.ParentSIDs) > 0 {
		existing.ParentSIDs = make(map[string]string, len(incoming.ParentSIDs))
	}
	if existing.WorktreeSpecs == nil && len(incoming.WorktreeSpecs) > 0 {
		existing.WorktreeSpecs = make(map[string]*worktree.Spec, len(incoming.WorktreeSpecs))
	}
	if existing.WorktreeErrors == nil && len(incoming.WorktreeErrors) > 0 {
		existing.WorktreeErrors = make(map[string]string, len(incoming.WorktreeErrors))
	}
	for _, id := range incoming.WorkBeadIDs[:limit] {
		if strings.TrimSpace(id) == "" {
			continue
		}
		existing.WorkBeadIDs = append(existing.WorkBeadIDs, id)
		if incoming.Titles != nil {
			existing.Titles[id] = incoming.Titles[id]
		}
		if incoming.Packs != nil {
			existing.Packs[id] = incoming.Packs[id]
		}
		if incoming.Workspaces != nil {
			existing.Workspaces[id] = incoming.Workspaces[id]
		}
		if incoming.StoreRefs != nil {
			existing.StoreRefs[id] = incoming.StoreRefs[id]
		}
		if incoming.ParentSIDs != nil {
			if sid := incoming.ParentSIDs[id]; sid != "" {
				existing.ParentSIDs[id] = sid
			}
		}
		if incoming.WorktreeSpecs != nil {
			if spec := incoming.WorktreeSpecs[id]; spec != nil {
				existing.WorktreeSpecs[id] = spec
			}
		}
		if incoming.WorktreeErrors != nil {
			if worktreeErr := incoming.WorktreeErrors[id]; worktreeErr != "" {
				existing.WorktreeErrors[id] = worktreeErr
			}
		}
	}
	existing.Count = len(existing.WorkBeadIDs)
	if existing.Count < count {
		existing.Count = count
	}
	return existing
}

func defaultNamedSessionDemand(targets []defaultScaleCheckTarget, _ *config.City, _ string, caches ...*readyDemandCache) (map[string]bool, map[string]bool, []error) {
	cache := optionalReadyDemandCache(caches)
	demand := make(map[string]bool)
	if len(targets) == 0 {
		return demand, nil, nil
	}

	type scaleStoreGroup struct {
		store     beads.Store
		templates map[string]struct{}
	}
	groups := make(map[string]*scaleStoreGroup)
	var errs []error
	var partialTemplates map[string]bool
	for _, target := range targets {
		template := strings.TrimSpace(target.template)
		if template == "" {
			continue
		}
		if target.err != nil {
			errs = append(errs, target.err)
			partialTemplates = markScaleCheckPartialTemplate(partialTemplates, template)
		}
		if target.store == nil {
			if target.err == nil {
				errs = append(errs, fmt.Errorf("default scale_check %s: store unavailable", template))
			}
			partialTemplates = markScaleCheckPartialTemplate(partialTemplates, template)
			continue
		}
		key := strings.TrimSpace(target.storeKey)
		if key == "" {
			key = fmt.Sprintf("%p", target.store)
		}
		group := groups[key]
		if group == nil {
			group = &scaleStoreGroup{store: target.store, templates: make(map[string]struct{})}
			groups[key] = group
		}
		group.templates[template] = struct{}{}
	}

	// Named sessions are not inferred from gc.routed_to/gc.run_target.
	// A work item targets a named session by Assignee=<session id/name/alias>.
	// This probe remains only to mark named-session backing templates partial
	// when a default demand query is inconclusive, so existing named-session
	// beads are retained instead of swept on a store/query failure.
	for key, group := range groups {
		_, err := cache.controllerDemandReady(group.store)
		if err != nil {
			errs = append(errs, fmt.Errorf("default scale_check %s templates=%s: Ready(): %w", key, strings.Join(sortedStringSet(group.templates), ","), err))
			partialTemplates = markScaleCheckPartialSet(partialTemplates, group.templates)
			continue
		}
	}
	return demand, partialTemplates, errs
}

// controllerDemandRouteCandidates keeps controller-side readers compatible
// with pre-ga-eld2x workflow roots. It matches the shell claim/count shape:
// canonical gc.routed_to first, then gc.run_target only for workflow roots
// stamped before root routing switched to gc.routed_to.
func controllerDemandRouteCandidates(b beads.Bead) []string {
	return routedToAndLegacyWorkflowCandidates(b)
}

func openControlDispatcherDemand(cfg *config.City, workBeads []beads.Bead) map[string]bool {
	demand := make(map[string]bool)
	if cfg == nil || len(workBeads) == 0 {
		return demand
	}
	// Map every route a deterministic control dispatcher answers to — its
	// qualified name plus the pre-1.3 binding-stripped bare alias — back to its
	// canonical qualified template key. Pre-1.3 builds routed control beads to
	// the bare name; honoring it keeps in-flight work persisted across an
	// upgrade scaling the qualified dispatcher (keyed by the template name the
	// scaler matches).
	aliasToCanonical := make(map[string]string)
	for i := range cfg.Agents {
		if !config.IsDeterministicControlDispatcher(&cfg.Agents[i]) {
			continue
		}
		qualified := cfg.Agents[i].QualifiedName()
		aliasToCanonical[qualified] = qualified
		if bare := controlDispatcherBareRoute(qualified); bare != "" {
			if _, taken := aliasToCanonical[bare]; !taken {
				aliasToCanonical[bare] = qualified
			}
		}
	}
	if len(aliasToCanonical) == 0 {
		return demand
	}
	for _, wb := range workBeads {
		if wb.Status != "open" || strings.TrimSpace(wb.Assignee) != "" {
			continue
		}
		for _, candidate := range controllerDemandRouteCandidates(wb) {
			if canonical, ok := aliasToCanonical[candidate]; ok {
				demand[canonical] = true
				break
			}
		}
	}
	return demand
}

func markScaleCheckPartialTemplate(partials map[string]bool, template string) map[string]bool {
	template = strings.TrimSpace(template)
	if template == "" {
		return partials
	}
	if partials == nil {
		partials = make(map[string]bool)
	}
	partials[template] = true
	return partials
}

func markScaleCheckPartialSet(partials map[string]bool, templates map[string]struct{}) map[string]bool {
	for template := range templates {
		partials = markScaleCheckPartialTemplate(partials, template)
	}
	return partials
}

func mergeScaleCheckPartialTemplates(dst, src map[string]bool) map[string]bool {
	for template, partial := range src {
		if partial {
			dst = markScaleCheckPartialTemplate(dst, template)
		}
	}
	return dst
}

func effectivePoolPartialRetentionTemplates(result DesiredStateResult) map[string]bool {
	return mergeScaleCheckPartialTemplates(
		mergeScaleCheckPartialTemplates(nil, result.PoolScaleCheckPartialTemplates),
		result.PoolPartialRetentionTemplates,
	)
}

func sortedBoolMapKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value, include := range values {
		if include {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func retainScaleCheckPartialPoolDesired(cfg *config.City, counts map[string]int, sessionBeads *sessionBeadSnapshot, partialTemplates map[string]bool) map[string]int {
	if len(partialTemplates) == 0 || sessionBeads == nil {
		return counts
	}
	retained := make(map[string]int)
	for _, info := range sessionBeads.OpenInfos() {
		// Adopted session beads can persist a legacy bound template identity;
		// normalize to the current canonical name before the membership check,
		// because partialTemplates is keyed canonically. Without this a transient
		// scale_check partial failure would drop legacy-bound pool sessions.
		template := normalizeAgentTemplateIdentity(cfg, strings.TrimSpace(info.Template))
		if !partialTemplates[template] || !isPoolManagedSessionInfo(info) || !scaleCheckPartialSessionRetainableInfo(info) {
			continue
		}
		retained[template]++
	}
	if len(retained) == 0 {
		return counts
	}
	if counts == nil {
		counts = make(map[string]int)
	}
	for template, count := range retained {
		if counts[template] < count {
			counts[template] = count
		}
	}
	return counts
}

// scaleCheckPartialSessionPreservableInfo preserves dormant affected-template
// beads during transient scale_check failures, but does not count them as awake
// demand. Sessions that are already mid-drain or past-drain
// (draining/drained/archived) are not preserved so a partial read cannot
// interrupt an in-progress drain lifecycle. It reads the raw state metadata
// (Info.MetadataState) and delegates the in-flight-create default case to
// isPendingPoolCreateInfo.
func scaleCheckPartialSessionPreservableInfo(i session.Info) bool {
	switch strings.TrimSpace(i.MetadataState) {
	case "", "active", "awake", "start-pending", "creating", "asleep", "stopped", "suspended", "quarantined":
		return true
	default:
		return isPendingPoolCreateInfo(i)
	}
}

// scaleCheckPartialSessionRetainableInfo counts active/awake affected-template
// beads as retained demand during transient scale_check failures. A fresh
// in-flight create that still holds an active pending_create_claim lease also
// counts as retained capacity; stale creates (lease expired/cleared) do not, so
// they stop inflating the desired count. It reads the raw state metadata
// (Info.MetadataState) and delegates the in-flight-create case to
// isPendingPoolCreateInfo.
func scaleCheckPartialSessionRetainableInfo(i session.Info) bool {
	switch strings.TrimSpace(i.MetadataState) {
	case "active", "awake":
		return true
	default:
		return isPendingPoolCreateInfo(i)
	}
}

func sortedStringSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func listBothTiersForControllerDemand(store beads.Store, query beads.ListQuery) ([]beads.Bead, error) {
	handles := beads.HandlesFor(store)
	rows, err := handles.Cached.List(query)
	if errors.Is(err, beads.ErrCacheUnavailable) {
		return handles.Live.List(query)
	}
	return rows, err
}

// listOpenForControllerDemandLive reads open work for the controller-demand and
// spawn-capacity paths on the LIVE tier so the backing store's raw-status filter
// runs. listBothTiersForControllerDemand serves a Status:"open" query from the
// cache (handles.Cached forces Live=false), which filters against the collapsed
// Bead.Status: mapBdStatus folds bd's blocked/deferred/review/testing into Gas
// City's "open", so a blocked bead is indistinguishable from ready work and
// would count as controller demand, re-entering dispatch (EB-42o8/gc-nz5i). Only
// a Live read reaches the backing store's server-side --status=open filter
// (BdStore passes it to bd; DoltliteReadStore matches WHERE status=?), which
// excludes the raw blocked status. This extends the fix gc-4zb/#4395 applied to
// restoreCarriedWorkRoutes and the workflow projection to the controller-demand
// List reads. AllowScan opts into the intentional open-status population read;
// handles.Live unions the wisp step-bead tier (TierBoth). Correctness outranks
// latency on the demand path (see readyDemandCache): this pays one live
// backing-store read rather than over-counting blocked work as demand.
//
// Known gap: NativeDoltStore maps Status:"open" to
// ExcludeStatus=[closed,in_progress] (see nativeIssueFilterFromListQuery), so it
// still returns raw blocked/deferred rows regardless of Live — this gate is
// inert on that backend, tracked separately.
func listOpenForControllerDemandLive(store beads.Store) ([]beads.Bead, error) {
	return beads.HandlesFor(store).Live.List(beads.ListQuery{Status: "open", AllowScan: true})
}

func readyForControllerDemand(store beads.Store) ([]beads.Bead, error) {
	return readyForControllerDemandQuery(store, beads.ReadyQuery{})
}

func readyForControllerDemandQuery(store beads.Store, query beads.ReadyQuery) ([]beads.Bead, error) {
	query.TierMode = beads.TierBoth
	handles := beads.HandlesFor(store)
	rows, err := handles.Cached.Ready(query)
	if errors.Is(err, beads.ErrCacheUnavailable) {
		return handles.Live.Ready(query)
	}
	if _, hasExplicitHandles := store.(interface {
		Handles() beads.StoreHandles
	}); !hasExplicitHandles {
		return rows, err
	}
	if err != nil && !beads.IsPartialResult(err) {
		rows = nil
	}
	// The live Ready read is the cross-process freshness replacement for the
	// retired gc.pool_demand sentinel. Each controller demand group pays at
	// most one live backing-store Ready query per reconciliation pass and shares
	// that result across every template backed by the same store.
	liveRows, liveErr := handles.Live.Ready(query)
	if liveErr == nil {
		// A complete live read is authoritative; cached rows only preserve
		// demand when the live freshness read is partial or unavailable.
		return liveRows, nil
	}
	if liveErr != nil && !beads.IsPartialResult(liveErr) {
		liveRows = nil
	}
	rows = mergeReadyRowsByID(rows, liveRows)
	if joined := errors.Join(err, liveErr); joined != nil && len(rows) > 0 && !beads.IsPartialResult(joined) {
		return rows, &beads.PartialResultError{Op: "controller ready demand", Err: joined}
	} else if joined != nil {
		return rows, joined
	}
	return rows, nil
}

func liveReadyForControllerDemandQuery(store beads.Store, query beads.ReadyQuery) ([]beads.Bead, error) {
	query.TierMode = beads.TierBoth
	handles := beads.HandlesFor(store)
	return handles.Live.Ready(query)
}

// readyDemandCache memoizes the unfiltered ready reads for a single reconcile
// pass so every pool and demand probe filters one shared in-memory snapshot
// instead of issuing its own /beads/ready fetch. Each backing store is read at
// most once on the live tier and at most once on the cached tier; consumers
// then apply their Assignee/Limit selectors in memory. This is exact for the
// stores that filter client-side over a stable, filter-independent result order
// (MemStore.Ready, BdStore.Ready): the assignee-matching prefix of the
// unfiltered set is exactly the assignee-filtered set, and taking the first
// Limit of it matches a per-assignee fetch. NativeDoltStore.Ready filters the
// assignee server-side, and its wisp sub-query is assignee-aware too: the
// pinned beads@v1.1.0 readyWorkWispIssueFilter carries filter.Assignee into the
// wisp filter (internal/storage/issueops/ready_work.go), which emits
// `assignee = ?` for the wisp table (internal/storage/sqlbuild/filter.go),
// exactly as the issues leg does. Both legs order by a total order independent
// of the assignee predicate with Limit applied client-side, so filtering the
// unfiltered snapshot by assignee returns exactly the assignee-scoped Ready
// set. The transformation is therefore exact for all three production stores,
// not merely demand-safe.
//
// Before this cache a single demand phase fanned out ~60 sequential Ready reads
// on a live city — the assigned-work pass alone issued one live read per store
// per live-session assignee (build_desired_state.go collectAssignedWorkBeads*),
// and the scale-check and named-session probes each re-read the full ready set
// per store group. On the live maintainer-city those reads measured 3.6s avg /
// 7.4s max, so one pass took ~3.5 min against a configured 15s patrol interval.
//
// Keyed by store identity so two templates backed by the same store share one
// fetch. A nil *readyDemandCache falls back to the direct free functions, so
// tests and non-tick callers keep the pre-cache behavior unchanged.
//
// Read-only contract: every read here (liveReady, controllerDemandReady,
// filterReadySnapshot) may return a slice that aliases the shared per-pass
// memo, so consumers must treat results as read-only and never mutate or
// append to them. The current demand consumers only read.
//
// This collapses the read *count* (the dominant cost). The per-read cost is a
// separate defect: the sqlite/embedded infra store's ready-projection cache is
// broken (bd sql unsupported), so HandlesFor(store).Cached.Ready fails with
// ErrCacheUnavailable and the live full hydration serves every read (~2.5s of
// API-route + hydration overhead vs ~0.97s bd-native). Repairing that projection
// is a beads-library change (internal/beads) and is deliberately out of scope
// here. NB: the store_health.go timeout+cache pattern must NOT be copied onto
// these reads — an empty/partial result on timeout would under-count demand and
// starve spawns; correctness outranks latency on the demand path, so the fix is
// fewer full reads, never a bounded-but-lossy read.
type readyDemandCache struct {
	mu     sync.Mutex
	live   map[beads.Store]*readyDemandEntry
	cached map[beads.Store]*readyDemandEntry
}

type readyDemandEntry struct {
	once sync.Once
	rows []beads.Bead
	err  error
}

func newReadyDemandCache() *readyDemandCache {
	return &readyDemandCache{
		live:   make(map[beads.Store]*readyDemandEntry),
		cached: make(map[beads.Store]*readyDemandEntry),
	}
}

// entry returns the per-store memo slot for m, creating it under the cache lock.
// The lock is held only long enough to get-or-create the slot, so reads for
// different stores still fetch concurrently (the assigned-work pass fans out one
// goroutine per store); the slot's sync.Once serializes only same-store readers.
func (c *readyDemandCache) entry(m map[beads.Store]*readyDemandEntry, store beads.Store) *readyDemandEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := m[store]
	if e == nil {
		e = &readyDemandEntry{}
		m[store] = e
	}
	return e
}

// liveSnapshot returns the memoized full live ready set (TierBoth, unfiltered)
// for store, fetching it once. Mirrors the read liveReadyForControllerDemandQuery
// performs before its own assignee/limit filtering.
func (c *readyDemandCache) liveSnapshot(store beads.Store) ([]beads.Bead, error) {
	e := c.entry(c.live, store)
	e.once.Do(func() {
		e.rows, e.err = beads.HandlesFor(store).Live.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	})
	return e.rows, e.err
}

// cachedSnapshot returns the memoized full cached ready set (TierBoth,
// unfiltered) for store, fetching it once. Mirrors the read
// readyForControllerDemandQuery performs against the cached tier.
func (c *readyDemandCache) cachedSnapshot(store beads.Store) ([]beads.Bead, error) {
	e := c.entry(c.cached, store)
	e.once.Do(func() {
		e.rows, e.err = beads.HandlesFor(store).Cached.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	})
	return e.rows, e.err
}

// liveReady reproduces liveReadyForControllerDemandQuery from the shared live
// snapshot. A nil cache reads directly (pre-cache behavior).
func (c *readyDemandCache) liveReady(store beads.Store, query beads.ReadyQuery) ([]beads.Bead, error) {
	if c == nil {
		return liveReadyForControllerDemandQuery(store, query)
	}
	rows, err := c.liveSnapshot(store)
	return filterReadySnapshot(rows, query), err
}

// controllerDemandReady reproduces readyForControllerDemand (the full ready set
// with no assignee/limit selector) from the shared cached and live snapshots,
// preserving its tier-merge precedence exactly (a complete live read is
// authoritative; cached rows only backfill a failed or partial live read). A nil
// cache reads directly (pre-cache behavior). The returned slice may alias the
// shared per-pass snapshot (the live path returns the memo directly), so callers
// must treat it as read-only.
func (c *readyDemandCache) controllerDemandReady(store beads.Store) ([]beads.Bead, error) {
	if c == nil {
		return readyForControllerDemand(store)
	}
	rows, err := c.cachedSnapshot(store)
	if errors.Is(err, beads.ErrCacheUnavailable) {
		return c.liveSnapshot(store)
	}
	if _, hasExplicitHandles := store.(interface {
		Handles() beads.StoreHandles
	}); !hasExplicitHandles {
		return rows, err
	}
	if err != nil && !beads.IsPartialResult(err) {
		rows = nil
	}
	liveRows, liveErr := c.liveSnapshot(store)
	if liveErr == nil {
		return liveRows, nil
	}
	if liveErr != nil && !beads.IsPartialResult(liveErr) {
		liveRows = nil
	}
	// Backfill: a failed or partial live read is topped up from the cached
	// snapshot rather than reported as an absence of work.
	//
	// This is a DELIBERATE asymmetry with the worker, and naming it here is the
	// point. The worker's own read is live-only and fails whole (`gc ready`
	// aborts on any bad leg), so a row merged in from cache can be up to one tick
	// stale relative to what that worker will see: the controller can count
	// demand for a row a fresh read no longer serves. Accepted, in this direction
	// only — the alternative is reporting zero demand from a store hiccup and
	// draining a live pool, which is the correctness-over-latency contract this
	// cache exists to honor. It self-heals on the next tick, and a seat spawned
	// against a stale row now drains cleanly (no_work, ack, reaped) rather than
	// being re-counted forever, because the permanent divergence classes are
	// closed at their source (demand_serve_predicate.go).
	rows = mergeReadyRowsByID(rows, liveRows)
	if joined := errors.Join(err, liveErr); joined != nil && len(rows) > 0 && !beads.IsPartialResult(joined) {
		return rows, &beads.PartialResultError{Op: "controller ready demand", Err: joined}
	} else if joined != nil {
		return rows, joined
	}
	return rows, nil
}

// filterReadySnapshot applies a ReadyQuery's Assignee and Limit selectors to an
// unfiltered snapshot in memory, matching the client-side filtering the store
// backends apply for the same selectors (order-preserving, limit truncates).
// The filtered result is a fresh slice; an empty selector returns the shared
// snapshot by reference. Either way the result is a read-only view that callers
// must not mutate or append to (see readyDemandCache).
func filterReadySnapshot(rows []beads.Bead, query beads.ReadyQuery) []beads.Bead {
	if query.Assignee == "" && query.Limit <= 0 {
		// No selector: the whole snapshot is the result. Return the shared
		// per-pass memo by reference, matching controllerDemandReady's live path,
		// so demand reads stay allocation-free. The result is read-only per the
		// readyDemandCache contract; no production caller passes an empty query
		// (every liveReady call carries an Assignee or a Limit).
		return rows
	}
	out := make([]beads.Bead, 0, len(rows))
	for _, b := range rows {
		if query.Assignee != "" && b.Assignee != query.Assignee {
			continue
		}
		out = append(out, b)
		if query.Limit > 0 && len(out) >= query.Limit {
			break
		}
	}
	return out
}

// optionalReadyDemandCache extracts the optional per-pass ready cache threaded
// through the demand probes. Absent (test/non-tick callers) yields nil, whose
// cache methods read directly and preserve pre-cache behavior.
func optionalReadyDemandCache(caches []*readyDemandCache) *readyDemandCache {
	if len(caches) > 0 {
		return caches[0]
	}
	return nil
}

func mergeReadyRowsByID(primary, secondary []beads.Bead) []beads.Bead {
	if len(primary) == 0 {
		return secondary
	}
	if len(secondary) == 0 {
		return primary
	}
	seen := make(map[string]struct{}, len(primary)+len(secondary))
	out := make([]beads.Bead, 0, len(primary)+len(secondary))
	for _, row := range secondary {
		if row.ID == "" {
			continue
		}
		seen[row.ID] = struct{}{}
		out = append(out, row)
	}
	for _, row := range primary {
		if row.ID == "" {
			continue
		}
		if _, ok := seen[row.ID]; ok {
			continue
		}
		seen[row.ID] = struct{}{}
		out = append(out, row)
	}
	return out
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

func appendInProgressWorkUnique(cfg *config.City, dst *[]beads.Bead, stores *[]beads.Store, storeRefs *[]string, readyIDs map[string]bool, beadList []beads.Bead, seen map[string]struct{}, store beads.Store, storeRef string) {
	for _, b := range beadList {
		if strings.TrimSpace(b.Assignee) == "" && !isRecoverableUnassignedInProgressPoolWork(cfg, b) {
			continue
		}
		if appendWorkUnique(dst, stores, storeRefs, b, seen, store, storeRef) {
			markReadyAssigned(readyIDs, b)
		}
	}
}

func appendAssignedUnique(dst *[]beads.Bead, stores *[]beads.Store, storeRefs *[]string, readyIDs map[string]bool, beadList []beads.Bead, seen map[string]struct{}, store beads.Store, storeRef string) {
	for _, b := range beadList {
		if strings.TrimSpace(b.Assignee) == "" {
			continue
		}
		if appendWorkUnique(dst, stores, storeRefs, b, seen, store, storeRef) {
			markReadyAssigned(readyIDs, b)
		}
	}
}

// appendOpenAssignedMoleculeWorkUnique includes root-only molecule wisps that
// are direct assignments. Ready() intentionally hides molecule roots from
// generic work queues, but an assigned root-only wisp is the executable turn
// for on-demand named sessions such as the Gas Town refinery patrol — so these
// beads contribute to readyIDs (their assigned turn is genuinely actionable),
// unlike the open-routed orphan-release pass.
func appendOpenAssignedMoleculeWorkUnique(dst *[]beads.Bead, stores *[]beads.Store, storeRefs *[]string, readyIDs map[string]bool, beadList []beads.Bead, seen map[string]struct{}, store beads.Store, storeRef string) {
	for _, b := range beadList {
		if !isOpenAssignedMoleculeWork(b) {
			continue
		}
		if appendWorkUnique(dst, stores, storeRefs, b, seen, store, storeRef) {
			markReadyAssigned(readyIDs, b)
		}
	}
}

// markReadyAssigned records a bead ID as wake-demand-ready. It is called only
// by the assigned-work passes that establish real readiness (in-progress,
// store-Ready()/deps, and assigned molecule roots) — never by the open-routed
// orphan-release pass, whose beads have not passed any readiness gate.
func markReadyAssigned(readyIDs map[string]bool, b beads.Bead) {
	if readyIDs == nil {
		return
	}
	readyIDs[b.ID] = true
}

func isOpenAssignedMoleculeWork(b beads.Bead) bool {
	if b.Status != "open" || strings.TrimSpace(b.Assignee) == "" {
		return false
	}
	if !beads.IsMoleculeType(b.Type) {
		return false
	}
	return b.Ephemeral || b.NoHistory || strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]) == "workflow"
}

// appendOpenRoutedWorkUnique includes open beads that are still releasably
// pool-routed AND still carry an assignee. This is the narrow input
// releaseOrphanedPoolAssignments needs to clear step beads abandoned by a
// dead session (graph.v2 wisps where the root depends on the finalize
// step, so the root never enters ready and the step assignee remains a
// long-form dead-session identity invisible to readyAssignedWorkAssignees).
func appendOpenRoutedWorkUnique(dst *[]beads.Bead, stores *[]beads.Store, storeRefs *[]string, beadList []beads.Bead, seen map[string]struct{}, store beads.Store, storeRef string) {
	for _, b := range beadList {
		if strings.TrimSpace(b.Assignee) == "" {
			continue
		}
		if routedToOrLegacyWorkflowTarget(b) == "" {
			continue
		}
		appendWorkUnique(dst, stores, storeRefs, b, seen, store, storeRef)
	}
}

// appendWorkUnique appends b to the aligned dst/stores/storeRefs slices unless
// it is a session bead or already seen. It reports whether the bead was
// actually appended, so ready-pass callers can record readiness only for beads
// this call admitted (and not for beads a prior pass already claimed via seen).
func appendWorkUnique(dst *[]beads.Bead, stores *[]beads.Store, storeRefs *[]string, b beads.Bead, seen map[string]struct{}, store beads.Store, storeRef string) bool {
	// Invariant: dst, stores, and storeRefs are kept index-aligned by this
	// shared growth path and the shared seen guard.
	// Session beads are not actionable work — filter them at the source
	// so all consumers see only real tasks. Message beads are NOT filtered
	// here because they represent mail that should wake/materialize sessions;
	// idle nudge filters messages locally since mail nudging is handled
	// separately by the mail system.
	if b.Type == sessionBeadType {
		return false
	}
	if _, ok := seen[b.ID]; ok {
		return false
	}
	seen[b.ID] = struct{}{}
	*dst = append(*dst, b)
	if stores != nil {
		*stores = append(*stores, store)
	}
	if storeRefs != nil {
		*storeRefs = append(*storeRefs, storeRef)
	}
	return true
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
	discoverSessionBeadsWithRoots(bp, cfg, desired, nil, nil, nil, stderr)
}

func discoverSessionBeadsWithRoots(
	bp *agentBuildParams,
	cfg *config.City,
	desired map[string]TemplateParams,
	suspendedRigPaths map[string]bool,
	poolScaleCheckPartialTemplates map[string]bool,
	namedScaleCheckPartialTemplates map[string]bool,
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
	for _, info := range sessionBeads.OpenInfos() {
		if info.Closed {
			continue
		}
		sn := info.SessionNameMetadata
		if sn == "" {
			continue
		}
		if isFailedCreateSessionInfo(info) {
			continue
		}
		// Remember whether the main config/pool pass already selected this
		// exact session bead. Pool-managed capacity not selected there should
		// not be recovered merely because it is pending or creating.
		_, sessionAlreadyDesired := desired[sn]
		// Skip held beads — the reconciler's wakeReasons handles held_until,
		// but we still need the bead in desired state so the reconciler
		// doesn't classify it as orphaned. Only skip if we can't resolve
		// the template.
		template := resolvedSessionTemplateInfo(info, cfg)
		if template == "" {
			continue
		}
		poolScaleCheckPartial := poolScaleCheckPartialTemplates[template]
		namedScaleCheckPartial := namedScaleCheckPartialTemplates[template] && isNamedSessionInfo(info)
		scaleCheckPartial := scaleCheckPartialSessionPreservableInfo(info) && (poolScaleCheckPartial || namedScaleCheckPartial)
		// Find the config agent for this template.
		cfgAgent := findAgentByTemplate(cfg, template)
		if cfgAgent == nil {
			continue
		}
		if agentInSuspendedRig(bp.cityPath, cfgAgent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		roots[template] = true
		if !sessionAlreadyDesired && !isManualSessionInfoForAgent(info, cfgAgent) && !isNamedSessionInfo(info) &&
			desiredHasCanonicalNonExpandingPoolSession(desired, template, cfgAgent) && staleNonExpandingPoolSessionBeadInfo(cfgAgent, info) {
			continue
		}
		if !isManualSessionInfo(info) && !isNamedSessionInfo(info) && !isPoolManagedSessionInfo(info) && desiredHasConfiguredNamedTemplate(desired, template) {
			// A configured named session already owns this backing template in
			// desired state. Treat any extra plain open bead as leaked state so
			// the reconciler can close it as orphaned instead of reviving it.
			continue
		}
		// Pool agents: respect the pool's scaling decision. If the main
		// config iteration (which ran evaluatePool / scale_check) did not
		// produce any desired entries for this template, the pool wants 0
		// instances. Don't re-add stale session beads — that bypasses
		// scaling and causes infinite wake→drain→stop loops when there's
		// no work.
		if isEphemeralSessionInfoForAgent(info, cfgAgent) {
			manualSession := isManualSessionInfoForAgent(info, cfgAgent)
			creating := info.MetadataState == "creating" || info.MetadataState == string(session.StateStartPending)
			pendingCreate := isPendingPoolCreateInfo(info)
			templateDesired := desiredHasTemplate(desired, template)
			// Pool-managed beads are controller-created capacity. A pending
			// or creating bead that the pool pass did not select is stale
			// capacity, not a reason to spawn a worker with an empty hook.
			controllerManagedPool := info.PoolManaged ||
				strings.TrimSpace(info.PoolSlot) != "" || pendingCreate
			if controllerManagedPool && isDrainedSessionInfo(info) {
				continue
			}
			if controllerManagedPool && !manualSession && !isNamedSessionInfo(info) &&
				!sessionAlreadyDesired && cfgAgent.UsesCanonicalSingletonPoolIdentity() &&
				desiredHasCanonicalNonExpandingPoolSession(desired, template, cfgAgent) {
				continue
			}
			// Use a narrower partial-alive guard than scaleCheckPartial here: for
			// creating/start-pending beads, only protect in-flight creates with an
			// active pending_create_claim lease; stale creates (lease cleared/expired)
			// roll back even during a partial tick. For all other states (active, awake,
			// asleep, stopped, …) the broad preservable rule applies unchanged.
			poolPartialAlive := (poolScaleCheckPartial || namedScaleCheckPartial) &&
				(isPendingPoolCreateInfo(info) || (!creating && scaleCheckPartialSessionPreservableInfo(info)))
			if controllerManagedPool && !manualSession && !isNamedSessionInfo(info) &&
				!sessionAlreadyDesired && !templateDesired && !poolPartialAlive {
				continue
			}
			if !manualSession && (!creating || isStaleCreatingInfo(info)) && !templateDesired && !pendingCreate && !scaleCheckPartial {
				continue
			}
		}
		// Skip beads already in desired state (from config iteration).
		if sessionAlreadyDesired {
			continue
		}
		// Resolve TemplateParams for this bead's session.
		//
		// Pool-managed beads and manual pooled sessions recover identity from
		// different sources:
		//   - Pool-managed rediscovery must canonicalize stamped pool slots to
		//     the same instance identity realizePoolDesiredSessions uses, or
		//     GC_ALIAS / FingerprintExtra will oscillate across ticks.
		//   - Manual sessions must preserve the concrete identity persisted on
		//     the bead (agent_name / explicit session_name / alias), even when
		//     that identity is not a numbered pool slot.
		//
		// The identity-resolution chain below (sessionBeadQualifiedNameInfo,
		// canonicalSessionIdentityWithConfigInfo, resolveTemplateForSessionBeadInfo)
		// reads the session through session.Info. Recover this info's snapshot entry
		// by ID (index-stable: FindInfoByID(info.ID) returns exactly this info), so a
		// bead absent from the snapshot is skipped exactly as the raw path did.
		bInfo, ok := sessionBeads.FindInfoByID(info.ID)
		if !ok {
			continue
		}
		var (
			resolveAgent         *config.Agent
			sessionQualifiedName string
		)
		if isManualSessionInfoForAgent(info, cfgAgent) {
			sessionQualifiedName = sessionBeadQualifiedNameInfo(bp.cityPath, cfgAgent, bp.rigs, bInfo)
			resolveAgent = sessionBeadConfigAgent(cfgAgent, sessionQualifiedName)
		} else {
			// Canonicalize agent identity before calling resolveTemplate so a
			// pool-managed bead with pool_slot stamped resolves as the
			// pool-instance form here — the same shape realizePoolDesiredSessions
			// uses. Before GC_ALIAS was excluded from CoreFingerprint, this
			// identity mismatch caused config-drift drains; the canonical shape
			// still keeps routing/display identity and remaining fingerprint
			// inputs aligned across buildDesiredState paths. Named beads
			// resolve to their own stored alias so this seam reproduces the
			// identity the named-session loop assigned (see
			// canonicalSessionIdentity).
			resolveAgent, sessionQualifiedName = canonicalSessionIdentityWithConfigInfo(cfg, cfgAgent, bInfo)
		}
		fpExtra := buildFingerprintExtra(resolveAgent)
		tp, err := resolveTemplateForSessionBeadInfo(bp, resolveAgent, sessionQualifiedName, fpExtra, bInfo)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: bead %s template %q: %v (skipping)\n", info.ID, template, err) //nolint:errcheck
			continue
		}
		tp.ManualSession = isManualSessionInfoForAgent(info, cfgAgent)
		if tp.ManualSession {
			if manualAlias := strings.TrimSpace(info.Alias); manualAlias != "" {
				// Explicit aliases from `gc session new --alias ...` are
				// user-chosen command targets and must survive controller sync.
				tp.Alias = manualAlias
			}
		}
		if isEphemeralSessionInfoForAgent(info, cfgAgent) {
			if !tp.ManualSession || strings.TrimSpace(info.Alias) == "" {
				tp.Alias = ""
			}
			if tp.ManualSession && sessionQualifiedName != "" {
				tp.InstanceName = sessionQualifiedName
			} else {
				tp.InstanceName = sn
			}
		}
		installAgentSideEffects(bp, cfgAgent, tp, stderr)
		desired[sn] = tp
	}
	return roots
}

// isPendingPoolCreateInfo reports whether a pool-managed session is an in-flight
// create still holding an active pending_create_claim lease.
func isPendingPoolCreateInfo(i session.Info) bool {
	return isPoolManagedSessionInfo(i) && i.PendingCreateClaim
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
	qualifiedName := cfgAgent.QualifiedName()
	if err := validateAgentSessionTransportForBuild(bp, cfgAgent, qualifiedName); err != nil {
		fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
		return
	}

	if bp.beadStore == nil {
		slot, err := claimFreshPoolSlotInfo(bp, cfgAgent, make(map[int]bool))
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
			return
		}
		resolveAgent, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(cfgAgent, slot)
		fpExtra := buildFingerprintExtra(resolveAgent)
		tp, err := resolveTemplatePrepared(bp, resolveAgent, qualifiedInstance, fpExtra)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedInstance, err) //nolint:errcheck
			return
		}
		tp.DependencyOnly = true
		tp.PoolSlot = poolSlot
		if usesTransientPoolSlotIdentity(cfgAgent) {
			clearPoolTemplateRuntimeIdentity(&tp)
		} else {
			setTemplateEnvIdentity(&tp, qualifiedInstance)
		}
		installAgentSideEffects(bp, resolveAgent, tp, stderr)
		desired[tp.SessionName] = tp
		return
	}

	// Bead selection keys off the configured base template, not the pool-
	// instance form, because normalizedSessionTemplate reads the bead's
	// "template" metadata which is always the base.
	sbInfo, slot, err := selectOrCreateDependencyPoolSessionBeadWithSlot(bp, cfgAgent, qualifiedName)
	if err != nil {
		fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
		return
	}
	// selectOrCreateDependencyPoolSessionBead returns the typed session.Info of the
	// selected-or-created dependency-floor session directly (W-pool), so the identity
	// chain below reads through Info with no raw pool-loop projection.
	// Env/fingerprint resolution, on the other hand, must use the same
	// canonical-or-instance identity as both the no-store dependency-floor
	// path above and realizePoolDesiredSessions. Otherwise GC_ALIAS can
	// oscillate across ticks and trigger the reconciler's config-drift drain
	// on the live dependency-floor session.
	resolveAgent, resolveQN, _ := poolDesiredRequestIdentity(cfgAgent, slot)
	fpExtra := buildFingerprintExtra(resolveAgent)
	tp, err := resolveTemplateForSessionBeadInfo(bp, resolveAgent, resolveQN, fpExtra, sbInfo)
	if err != nil {
		fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
		return
	}
	tp.Alias = ""
	tp.InstanceName = sbInfo.SessionNameMetadata
	tp.DependencyOnly = true
	installAgentSideEffects(bp, resolveAgent, tp, stderr)
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

func desiredHasConfiguredNamedTemplate(desired map[string]TemplateParams, template string) bool {
	for _, existing := range desired {
		if existing.TemplateName == template && strings.TrimSpace(existing.ConfiguredNamedIdentity) != "" {
			return true
		}
	}
	return false
}

func desiredHasCanonicalNonExpandingPoolSession(desired map[string]TemplateParams, template string, cfgAgent *config.Agent) bool {
	if !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return false
	}
	canonical := cfgAgent.QualifiedName()
	for _, existing := range desired {
		if existing.TemplateName != template {
			continue
		}
		if existing.DependencyOnly || existing.InstanceName == canonical || existing.Alias == canonical {
			return true
		}
	}
	return false
}

// poolRealizeParallelism caps the number of concurrent pool session bead
// creates inside realizePoolDesiredSessions. Each create acquires per-identity
// session locks + commits to dolt; with N>cap pending creates the work pool
// drains in O(ceil(N/cap) × commit-latency) wall time instead of the prior
// O(N × commit-latency). The cap is intentionally modest: dolt commit
// contention and per-city identity-lock churn put a ceiling on useful
// parallelism even when many distinct identities are pending. See
// gastownhall/gascity#2319.
const poolRealizeParallelism = 8

// poolRealizeWorkItem holds the per-request state threaded across the
// three-phase realizePoolDesiredSessions pipeline. Phase A (serial) populates
// either sessionInfo+slot (reuse path) or plan+slot (create path); Phase B
// (parallel-bounded) materializes plans into sessionInfo/createErr; Phase C
// (serial) resolves the template and installs side effects. sessionInfo is the
// typed session.Info the create/reuse path now returns directly (W-pool), so the
// realize loop carries Info end to end with no raw pool-loop projection.
type poolRealizeWorkItem struct {
	request     SessionRequest
	skip        bool
	plan        *poolSessionCreatePlan
	sessionInfo session.Info
	slot        int
	createErr   error
}

func realizePoolDesiredSessions(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	poolState PoolDesiredState,
	desired map[string]TemplateParams,
	stderr io.Writer,
) {
	realizePoolDesiredSessionsAt(bp, cfgAgent, poolState, time.Now(), desired, stderr)
}

func realizePoolDesiredSessionsAt(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	poolState PoolDesiredState,
	poolDecisionTime time.Time,
	desired map[string]TemplateParams,
	stderr io.Writer,
) {
	qualifiedName := cfgAgent.QualifiedName()
	if err := validateAgentSessionTransportForBuild(bp, cfgAgent, qualifiedName); err != nil {
		fmt.Fprintf(stderr, "buildDesiredState: pool %q: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
		return
	}
	used := make(map[string]bool)
	usedSlots := make(map[int]bool)

	// Phase A (serial, fast): select an existing session bead to reuse OR
	// reserve an (alias, slot) for a fresh create. Mutates used/usedSlots
	// under serial control so dedup and slot allocation remain deterministic.
	items := make([]poolRealizeWorkItem, 0, len(poolState.Requests))
	for _, request := range poolState.Requests {
		// planItem runs the per-request selection and returns the work item;
		// any early-out (skip path) sets item.skip and returns. The single
		// append below keeps slice growth in one place.
		planItem := func() poolRealizeWorkItem {
			item := poolRealizeWorkItem{request: request}
			var prefer *session.Info
			if request.SessionBeadID != "" {
				if candidate, ok := bp.sessionBeads.FindInfoByID(request.SessionBeadID); ok {
					// Defense in depth: ComputePoolDesiredStates filters out
					// named-session beads from pool resume requests. If one
					// slipped through, materializing it here would create a
					// phantom "{name}-N" sibling to the canonical named session.
					if isNamedSessionInfo(candidate) {
						fmt.Fprintf(stderr, "buildDesiredState: pool %q: refusing to materialize named-session bead %s as pool instance (would create phantom %q-N sibling)\n", qualifiedName, candidate.ID, cfgAgent.Name) //nolint:errcheck
						item.skip = true
						return item
					}
					prefer = &candidate
				}
			}
			sessionInfo, slot, plan, err := selectOrPlanPoolSessionBead(bp, cfgAgent, qualifiedName, prefer, request, poolDecisionTime, used, usedSlots)
			if err != nil {
				switch {
				case errors.Is(err, errPoolSessionCreateBudgetExhausted):
					fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (fresh create deferred)\n", qualifiedName, err) //nolint:errcheck
				case errors.Is(err, errPoolSessionCreatePartial):
					fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (partial demand read, fresh create blocked)\n", qualifiedName, err) //nolint:errcheck
				case errors.Is(err, errPoolSessionCreateProviderRed):
					// debug-level: fires every tick during a red episode; not operator noise
					fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (provider red, fresh create blocked)\n", qualifiedName, err) //nolint:errcheck
				case errors.Is(err, errPoolSessionNameUnavailable):
					// The slot's runtime name is a function of its identity, so
					// it cannot be worked around by minting another one — that
					// is exactly the leak this replaced. The holder is named in
					// the error because a stalled slot is only diagnosable if
					// the operator can see who is sitting on the name.
					fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (slot stalled on its own runtime name; retrying next tick)\n", qualifiedName, err) //nolint:errcheck
				default:
					fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
				}
				item.skip = true
				return item
			}
			if plan != nil {
				item.plan = plan
				item.slot = plan.poolSlot
				return item
			}
			if used[sessionInfo.ID] {
				item.skip = true
				return item
			}
			used[sessionInfo.ID] = true
			item.sessionInfo = sessionInfo
			item.slot = slot
			return item
		}
		items = append(items, planItem())
	}

	// Phase B (parallel, bounded): materialize planned creates. Per-identity
	// session locks serialize calls that share either the public alias or the
	// resolved tmux_alias session name; distinct identities proceed in parallel
	// up to poolRealizeParallelism workers. The store write and alias-conflict
	// bookkeeping happen here.
	pending := make([]int, 0, len(items))
	for idx := range items {
		if items[idx].plan != nil {
			pending = append(pending, idx)
		}
	}
	if len(pending) > 0 {
		workerCount := poolRealizeParallelism
		if workerCount > len(pending) {
			workerCount = len(pending)
		}
		jobs := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < workerCount; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for idx := range jobs {
					plan := *items[idx].plan
					info, err := executePlannedPoolSessionBeadCreate(bp, cfgAgent, qualifiedName, plan)
					if err != nil {
						items[idx].createErr = err
						continue
					}
					items[idx].sessionInfo = info
				}
			}()
		}
		for _, idx := range pending {
			jobs <- idx
		}
		close(jobs)
		wg.Wait()
	}

	// Phase C (serial, fast): finalize results in original request order.
	// Failed creates release their reserved slot here, at end-of-cycle —
	// unlike the original serial loop, which freed a failed slot before the
	// next request was planned, letting a same-tick later request reclaim it.
	// With Phase A planning all requests up front, that intra-tick reuse no
	// longer happens: a failed create leaves a slot gap for this cycle and the
	// slot is reclaimed on the next build tick. The pool's active-session
	// count converges identically; only the transient slot numbering differs.
	// Template resolution + installAgentSideEffects (hooks.InstallWithResolver
	// + autoSP.RouteACP) remain serial pending an audit of their thread-safety.
	for i := range items {
		item := &items[i]
		if item.skip {
			continue
		}
		if item.plan != nil {
			if item.createErr != nil {
				fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (skipping)\n", qualifiedName, item.createErr) //nolint:errcheck
				delete(usedSlots, item.plan.slot)
				continue
			}
			if used[item.sessionInfo.ID] {
				continue
			}
			used[item.sessionInfo.ID] = true
		}
		// item.sessionInfo is the typed session.Info the create/reuse path returns
		// directly (W-pool), so the former raw pool-loop projection is gone; the
		// bind fold and every downstream identity read flow through Info.
		sbInfo := item.sessionInfo
		slot := item.slot
		manualSession := isManualSessionInfoForAgent(sbInfo, cfgAgent)
		var (
			resolveAgent      *config.Agent
			qualifiedInstance string
			poolSlot          int
		)
		if manualSession {
			qualifiedInstance = sessionBeadQualifiedNameInfo(bp.cityPath, cfgAgent, bp.rigs, sbInfo)
			resolveAgent = sessionBeadConfigAgent(cfgAgent, qualifiedInstance)
		} else {
			resolveAgent, qualifiedInstance, poolSlot = poolDesiredRequestIdentity(cfgAgent, slot)
		}
		// Bind using this item's own per-slot qualifiedInstance, not the base
		// qualifiedName resolved once above the Phase A/B/C pipeline: the work
		// dir template expands per-request identity (e.g. {{.AgentBase}}), and
		// qualifiedName is loop-invariant across every item in this pipeline.
		// cfgAgent (not resolveAgent) matches the Phase A create-time call
		// (poolTriggerMetadata via selectOrPlanPoolSessionBead), which also
		// resolves the work dir off the base agent plus the per-slot name.
		if bound, err := bindPoolSessionTriggerBead(bp, cfgAgent, qualifiedInstance, sbInfo, item.request); err != nil {
			if errors.Is(err, errPoolTriggerWorktreeEvidence) {
				// Unusable ownership evidence is not a partial bind: a reused
				// session left in desired would restart against its previous
				// binding's work dir.
				fmt.Fprintf(stderr, "buildDesiredState: pool %q session %s trigger bead %s: %v (skipping)\n", qualifiedName, sbInfo.ID, item.request.WorkBeadID, err) //nolint:errcheck
				continue
			}
			fmt.Fprintf(stderr, "buildDesiredState: pool %q session %s trigger bead %s: %v (continuing without trigger env)\n", qualifiedName, sbInfo.ID, item.request.WorkBeadID, err) //nolint:errcheck
		} else {
			sbInfo = bound
		}
		fpExtra := buildFingerprintExtra(resolveAgent)
		tp, err := resolveTemplateForSessionBeadInfo(bp, resolveAgent, qualifiedInstance, fpExtra, sbInfo)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: pool %q session %s: %v (skipping)\n", qualifiedName, sbInfo.ID, err) //nolint:errcheck
			continue
		}
		if manualSession {
			tp.ManualSession = true
			if manualAlias := strings.TrimSpace(sbInfo.Alias); manualAlias != "" {
				tp.Alias = manualAlias
			}
			if qualifiedInstance != "" {
				tp.InstanceName = qualifiedInstance
			} else {
				tp.InstanceName = tp.SessionName
			}
			// Manual sessions are user-owned, even when they still carry legacy
			// pool_slot metadata from before singleton normalization.
			tp.PoolSlot = 0
		} else {
			tp.InstanceName = qualifiedInstance
			tp.PoolSlot = poolSlot
			if usesTransientPoolSlotIdentity(cfgAgent) {
				clearPoolTemplateRuntimeIdentity(&tp)
			} else {
				tp.Alias = qualifiedInstance
				setPoolTemplateRuntimeIdentityInfo(&tp, qualifiedInstance, sbInfo)
			}
		}
		installAgentSideEffects(bp, resolveAgent, tp, stderr)
		desired[tp.SessionName] = tp
	}
}

// computePoolTriggerBindingPatch is the pure key-diff at the heart of
// bindPoolSessionTriggerBead: given the session's current typed Info, the
// dispatch request, and the already-resolved trigger work dir, it returns the
// session-metadata patch that reconciles the trigger/pack/workspace/work-dir
// cluster to the request. Byte-identical to the raw inline diff the function
// used to compute against sessionBead.Metadata; a dedicated oracle
// (TestComputePoolTriggerBindingPatchMatchesRaw) pins it across the clear,
// reassign, store-ref, pack, workspace, and workdir request shapes. An empty
// patch means no change.
func computePoolTriggerBindingPatch(info session.Info, request SessionRequest, workDir string) session.MetadataPatch {
	workBeadID := strings.TrimSpace(request.WorkBeadID)
	metadata := session.MetadataPatch{}
	if workBeadID == "" {
		// Clear: a re-pointed session drops its prior trigger/store-ref and, so it
		// does not inherit the prior fork's "warm" provenance, its parent sid.
		if strings.TrimSpace(info.TriggerBeadID) != "" {
			metadata[beadmeta.TriggerBeadIDMetadataKey] = ""
		}
		if strings.TrimSpace(info.TriggerBeadStoreRef) != "" {
			metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = ""
		}
		if strings.TrimSpace(info.BrainParentSID) != "" {
			metadata[beadmeta.BrainParentSIDMetadataKey] = ""
		}
		return metadata
	}
	oldWorkBeadID := strings.TrimSpace(info.TriggerBeadID)
	if oldWorkBeadID != workBeadID {
		metadata[beadmeta.TriggerBeadIDMetadataKey] = workBeadID
		// On a genuine reassign to a different work bead, reconcile the fork parent
		// to the new work's value (set when the new bead carries one, clear
		// otherwise) so a re-pointed session never inherits the old fork.
		newParentSID := strings.TrimSpace(request.BrainParentSID)
		if strings.TrimSpace(info.BrainParentSID) != newParentSID {
			metadata[beadmeta.BrainParentSIDMetadataKey] = newParentSID
		}
	}
	workStoreRef := strings.TrimSpace(request.WorkStoreRef)
	if workStoreRef != "" && strings.TrimSpace(info.TriggerBeadStoreRef) != workStoreRef {
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = workStoreRef
	} else if workStoreRef == "" && oldWorkBeadID != workBeadID && strings.TrimSpace(info.TriggerBeadStoreRef) != "" {
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = ""
	}
	if pack := strings.TrimSpace(request.WorkPack); strings.TrimSpace(info.Pack) != pack {
		metadata[beadmeta.PackMetadataKey] = pack
	}
	if workspace := packWorkspaceSlug(request); strings.TrimSpace(info.PackWorkspace) != workspace {
		metadata[beadmeta.PackWorkspaceMetadataKey] = workspace
	}
	// Same-trigger reconciles may omit the title suffix the launcher used, so
	// preserve the recorded path. A live resume-mode session can also claim a
	// retry bead without restarting; in that case the process remains in its
	// existing cwd even though the trigger changes. The concrete session id and
	// active lifecycle state distinguish that continuation from an asleep,
	// fresh, or otherwise reusable session that will start in a newly derived
	// worktree. currently_processing_bead_id is deliberately not required here:
	// that secondary marker can lag the live process and must not authorize a cwd
	// metadata rewrite while the process is still running.
	if workDir != "" {
		targetWorkDir := workDir
		existingWorkDir := strings.TrimSpace(info.WorkDirCanonical)
		if existingWorkDir == "" {
			existingWorkDir = strings.TrimSpace(info.WorkDir)
		}
		liveResumeContinuation := oldWorkBeadID != workBeadID &&
			request.Tier == "resume" &&
			request.SessionBeadID == info.ID &&
			info.State == session.StateActive &&
			info.WakeMode != "fresh"
		if existingWorkDir != "" && (oldWorkBeadID == workBeadID || liveResumeContinuation) {
			targetWorkDir = existingWorkDir
		}
		if strings.TrimSpace(info.WorkDirCanonical) != targetWorkDir {
			metadata[beadmeta.WorkDirMetadataKey] = targetWorkDir
		}
		if strings.TrimSpace(info.WorkDir) != targetWorkDir {
			metadata[beadmeta.LegacyWorkDirMetadataKey] = targetWorkDir
		}
	}
	return metadata
}

// bindPoolSessionTriggerBead reconciles a pool session bead's trigger/pack/
// workspace/work-dir cluster to its dispatch request. The SESSION side is typed
// session.Info (WI-5 W3): it computes the byte-identical key diff via
// computePoolTriggerBindingPatch and persists it through the session front
// door's ONE-Update chokepoint (Store.UpdateMetadataInfo → single
// Store.Update(UpdateOpts{Metadata})). The write is byte-identical to the
// beadStore.Update this path used before the typed migration (same sorted
// --set-metadata args in one backend op), which the SetMetadataBatch route it
// briefly took could not guarantee: on an exec: or partial-write backend a
// per-key decomposition can commit an arbitrary subset of the trigger/store-ref/
// brain-parent/pack/workspace/workdir cluster, leaving a mixed provenance row.
// UpdateMetadataInfo commits the whole cluster all-or-nothing and folds the patch
// onto the returned Info only on success; on failure it returns info UNCHANGED
// with the error. It returns the bound Info; the caller folds the returned
// boundInfo into the Info-taking resolveTemplateForSessionBeadInfo chain (WI-5 W4
// dropped the former raw-bead mirror and retired the raw wrapper). A dry-run
// build with no store folds locally without a write.
func bindPoolSessionTriggerBead(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, info session.Info, request SessionRequest) (session.Info, error) {
	if info.ID == "" {
		return info, nil
	}
	workDir, err := verifiedPoolTriggerWorkDir(bp, cfgAgent, qualifiedName, request)
	if err != nil {
		return info, err
	}
	patch := computePoolTriggerBindingPatch(info, request, workDir)
	if len(patch) == 0 {
		return info, nil
	}
	if bp == nil || bp.beadStore == nil {
		return info.ApplyPatch(patch), nil
	}
	boundInfo, err := sessionFrontDoor(bp.beadStore).UpdateMetadataInfo(info, patch)
	if err != nil {
		return info, bindWriteFailure(request, err)
	}
	return boundInfo, nil
}

// bindWriteFailure marks a failed binding write on a managed-worktree
// request. The verified workspace never reached the session bead, so the
// stamped binding still names the previous bead's work dir and the caller
// must skip the item rather than reuse it.
func bindWriteFailure(request SessionRequest, err error) error {
	if request.WorktreeSpec == nil {
		return err
	}
	return fmt.Errorf("%w: binding write failed: %w", errPoolTriggerWorktreeEvidence, err)
}

// namedTriggerRefIsSameStore reports whether a named session's stamped
// gc.trigger_bead_store_ref names the city store that bindNamedSessionTriggerBead
// was handed. Empty is the unstamped same-store default; "city" and
// "city:<cityName>" are the two same-store spellings normalizeDemandStoreRef
// already collapses onto the city. Everything else -- "rig:*", class refs,
// another city's name, anything unrecognized -- is treated as a different
// store. The match is exact by design: a ref this function cannot confidently
// place is one whose target it cannot read, and a stale stamp is cheaper than
// a wrong clear.
func namedTriggerRefIsSameStore(storeRef, cityName string) bool {
	storeRef = strings.TrimSpace(storeRef)
	cityName = strings.TrimSpace(cityName)
	switch {
	case storeRef == "", storeRef == "city":
		return true
	case cityName != "" && storeRef == "city:"+cityName:
		return true
	default:
		return false
	}
}

// bindNamedSessionTriggerBead clears a named session's trigger stamp when its
// stamped target is no longer workable (absent, closed, or blocked -- the last
// read off bd's IsBlocked ready-work projection, since production stores fold
// the raw `blocked` status into "open"), mirroring bindPoolSessionTriggerBead's
// clear semantics for the pool path.
// Unlike pool dispatch, the named path has no per-tick SessionRequest that
// already reflects "no ready work" -- resolvePreservedConfiguredNamedSessionTemplate
// only replays whatever is currently stamped -- so without this check a
// parked target re-aims every subsequent seat indefinitely (gascity#4373).
// A target in a *different* store (info.TriggerBeadStoreRef naming a store
// other than `store`, per namedTriggerRefIsSameStore) is left untouched: this
// function can only see `store`, and misjudging a bead it cannot reach is
// worse than leaving a stale stamp for one more tick. The same-store city
// spellings do reach the status check -- the reported #4373 shape stamps one
// ("city"/"city:<name>"), so bailing on every non-empty ref would no-op on
// exactly the case this exists for.
func bindNamedSessionTriggerBead(store beads.Store, info session.Info, cityName string) (session.Info, error) {
	triggerID := strings.TrimSpace(info.TriggerBeadID)
	if triggerID == "" || store == nil {
		return info, nil
	}
	if !namedTriggerRefIsSameStore(info.TriggerBeadStoreRef, cityName) {
		return info, nil
	}
	target, err := store.Get(triggerID)
	stale := errors.Is(err, beads.ErrNotFound)
	if err != nil && !stale {
		// Lookup failed for a reason other than "gone" (e.g. a transient
		// backend error): don't misjudge a bead we couldn't actually read.
		return info, nil
	}
	if !stale {
		// mapBdStatus folds bd's raw `blocked` into "open" (gc-4zb/#4395), so a
		// literal status check never fires through BdStore/DoltLite/NativeDolt.
		// IsBlocked is bd's denormalized ready-work projection and is the signal
		// the wake side already uses for #4726. A nil projection (native DoltLite
		// snapshots, pre-1.0.5 bd) fails open: the stamp survives one more tick
		// rather than risking a wrong clear. The literal check stays for stores
		// that do surface the raw status.
		stale = target.Status == "closed" ||
			target.Status == "blocked" ||
			(target.IsBlocked != nil && *target.IsBlocked)
	}
	if !stale {
		return info, nil
	}
	// A same-store city ref reaches here stamped, so clear the whole cluster
	// rather than leaving a store-ref key pointing at a trigger id that no
	// longer exists -- the same keys the pool path clears in
	// computePoolTriggerBindingPatch.
	patch := session.MetadataPatch{beadmeta.TriggerBeadIDMetadataKey: ""}
	if strings.TrimSpace(info.TriggerBeadStoreRef) != "" {
		patch[beadmeta.TriggerBeadStoreRefMetadataKey] = ""
	}
	if strings.TrimSpace(info.BrainParentSID) != "" {
		patch[beadmeta.BrainParentSIDMetadataKey] = ""
	}
	return sessionFrontDoor(store).UpdateMetadataInfo(info, patch)
}

// errPoolTriggerWorktreeEvidence marks a bind failure caused by unusable
// worktree ownership evidence. The caller must skip such an item rather than
// continue without trigger env: continuing would restart a reused session
// against whatever work dir its previous binding left behind.
var errPoolTriggerWorktreeEvidence = errors.New("pool trigger worktree evidence")

func verifiedPoolTriggerWorkDir(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, request SessionRequest) (string, error) {
	if strings.TrimSpace(request.WorktreeError) != "" {
		return "", fmt.Errorf("%w invalid: %s", errPoolTriggerWorktreeEvidence, request.WorktreeError)
	}
	if request.WorktreeSpec == nil {
		return poolTriggerWorkDir(bp, cfgAgent, qualifiedName, request), nil
	}
	spec := *request.WorktreeSpec
	workID := strings.TrimSpace(request.WorkBeadID)
	if workID == "" || strings.TrimSpace(spec.BeadID) != workID {
		return "", fmt.Errorf("%w: bead %q does not match request bead %q", errPoolTriggerWorktreeEvidence, spec.BeadID, workID)
	}
	storeRef := strings.TrimSpace(request.WorkStoreRef)
	if storeRef != "" && canonicalEvidenceStoreRef(spec.StoreRef) != canonicalEvidenceStoreRef(storeRef) {
		return "", fmt.Errorf("%w: store %q does not match request store %q", errPoolTriggerWorktreeEvidence, spec.StoreRef, storeRef)
	}
	report, err := worktree.Verify(spec)
	if err != nil {
		return "", fmt.Errorf("%w: verification failed: %w", errPoolTriggerWorktreeEvidence, err)
	}
	return report.Path, nil
}

// canonicalEvidenceStoreRef collapses the spellings one store answers to, so
// the evidence guard can compare a request against workspace provenance. The
// two sides speak different vocabularies: demand records a probe shorthand
// ("city", or a bare rig name from the cold-wake probe's activeStores) while
// the worktree spec carries the bead's canonical gc.root_store_ref
// ("city:<name>", "rig:<name>"). Comparing those literally refuses a workspace
// that is in fact ours. Collapsing them keeps the cross-store guard closed:
// two different rigs still never compare equal.
func canonicalEvidenceStoreRef(storeRef string) string {
	storeRef = strings.TrimSpace(storeRef)
	switch {
	case storeRef == "":
		return ""
	case storeRef == "city", strings.HasPrefix(storeRef, "city:"), storeref.IsClassRef(storeRef):
		return "city"
	case strings.HasPrefix(storeRef, "rig:"):
		return "rig:" + strings.TrimSpace(strings.TrimPrefix(storeRef, "rig:"))
	default:
		// The only bare names in the demand vocabulary are rig names.
		//
		// A rig literally named "city" is ambiguous here, because the city
		// store's own probe ref is the bare string "city". That ambiguity is
		// older and wider than this guard: the same collision already exists
		// in every demand key built from the probe vocabulary. Nothing is
		// reserved at config validation. This guard resolves it fail-closed,
		// refusing the workspace rather than binding a session to a store it
		// cannot prove it owns.
		return "rig:" + storeRef
	}
}

func poolTriggerWorkDir(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, request SessionRequest) string {
	if bp == nil || cfgAgent == nil || strings.TrimSpace(request.WorkBeadID) == "" {
		return ""
	}
	// Pure path computation: this feeds a metadata patch, so it must never
	// create directories (gc-r9fx dry-run purity).
	base, err := resolveConfiguredWorkDirPath(bp.cityPath, bp.cityName, qualifiedName, cfgAgent, bp.rigs)
	if err != nil || strings.TrimSpace(base) == "" {
		return ""
	}
	if pack := strings.TrimSpace(request.WorkPack); pack != "" {
		packDir := filepath.Join(filepath.Dir(base), pack)
		if workspace := packWorkspaceSlug(request); workspace != "" {
			return filepath.Join(packDir, workspace)
		}
		return packDir
	}
	if workspace := packWorkspaceSlug(request); workspace != "" {
		return filepath.Join(base, workspace)
	}
	return base
}

func packWorkspaceSlug(request SessionRequest) string {
	if explicit := safeWorkspaceName(request.WorkWorkspace, 96); explicit != "" {
		return explicit
	}
	return ""
}

func safeWorkspaceName(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			return ""
		}
		if maxLen > 0 && b.Len() >= maxLen {
			break
		}
	}
	return strings.Trim(b.String(), ".-_")
}

// usesTransientPoolSlotIdentity reports whether cfgAgent's pool members are
// identified by a rebinding numeric slot ("rig/claude-1") rather than a stable
// name. Such a slot is bookkeeping, not identity: the controller hands it to a
// fresh session whenever the previous holder dies, so a work bead assigned to
// it names a chair, not an occupant.
//
// That distinction decides whether a session may claim under the name.
// `gc hook --claim` writes GC_ALIAS as the assignee (#4981), which is correct
// for the stable identities — a configured named session, a canonical singleton
// pool member, a namepool name like "nux" — because those double as the
// session's mail address and stay put across restarts. A numeric slot does not,
// and claiming under it produced ownership no reconciler guard could resolve to
// a live session: the drain guard saw no assigned work and drained live
// claim-holders. Transient slots therefore stay out of every identity channel
// (bead alias, GC_ALIAS, GC_AGENT/BEADS_ACTOR) and the session claims under its
// own session name, restoring the original unaliased-pool design (bc2ee15ac4).
func usesTransientPoolSlotIdentity(cfgAgent *config.Agent) bool {
	if cfgAgent == nil {
		return false
	}
	if strings.TrimSpace(cfgAgent.Namepool) != "" || len(cfgAgent.NamepoolNames) > 0 {
		return false
	}
	return !cfgAgent.UsesCanonicalSingletonPoolIdentity()
}

func poolDesiredRequestIdentity(cfgAgent *config.Agent, slot int) (*config.Agent, string, int) {
	qualifiedName := cfgAgent.QualifiedName()
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return cfgAgent, qualifiedName, 0
	}
	instanceName := poolInstanceName(cfgAgent.Name, slot, cfgAgent)
	qualifiedInstance := cfgAgent.QualifiedInstanceName(instanceName)
	instanceAgent := deepCopyAgent(cfgAgent, instanceName, cfgAgent.Dir)
	return &instanceAgent, qualifiedInstance, slot
}

// staleNonExpandingPoolSessionBeadInfo is the session.Info mirror of
// staleNonExpandingPoolSessionBead: it resolves the same non-expanding
// singleton-pool identity from typed Info fields (agent_name/label fallback via
// sessionBeadAgentNameInfo, Alias, Title, agent:<name> labels, PoolSlot) instead
// of raw bead metadata.
func staleNonExpandingPoolSessionBeadInfo(cfgAgent *config.Agent, info session.Info) bool {
	if !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return false
	}
	if isManualSessionInfoForAgent(info, cfgAgent) {
		return false
	}
	if nonExpandingPoolIdentitySlot(cfgAgent, sessionBeadAgentNameInfo(info)) > 0 {
		return true
	}
	if nonExpandingPoolIdentitySlot(cfgAgent, info.Alias) > 0 {
		return true
	}
	if nonExpandingPoolIdentitySlot(cfgAgent, info.Title) > 0 {
		return true
	}
	for _, label := range info.Labels {
		label = strings.TrimSpace(label)
		if strings.HasPrefix(label, "agent:") && nonExpandingPoolIdentitySlot(cfgAgent, strings.TrimPrefix(label, "agent:")) > 0 {
			return true
		}
	}
	return strings.TrimSpace(info.PoolSlot) != ""
}

func nonExpandingPoolIdentitySlot(cfgAgent *config.Agent, identity string) int {
	if !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return 0
	}
	// Accept any numeric -N suffix, not only configured pool bounds: these
	// beads are stale singleton artifacts and may have been written externally.
	return resolvePersistedPoolIdentitySlot(cfgAgent, true, identity)
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
	tp.EnvIdentityStamped = true
}

// resolveTemplateForSessionBeadInfo resolves the TemplateParams for a session bead
// from its session.Info. It reads only session_name, the two trigger keys, and
// pack — all verbatim raw mirrors on session.Info. Its callers hold the Info
// directly (from the typed snapshot via FindInfoByID, or a single boundary
// projection at the raw dependency-floor / pool-create seams); the former raw
// resolveTemplateForSessionBead wrapper was retired in WI-5 W4.
func resolveTemplateForSessionBeadInfo(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	qualifiedName string,
	fpExtra map[string]string,
	info session.Info,
) (TemplateParams, error) {
	local := *bp
	local.beadNames = map[string]string{qualifiedName: info.SessionNameMetadata}
	tp, err := resolveTemplatePrepared(&local, cfgAgent, qualifiedName, fpExtra)
	if err != nil {
		return tp, err
	}
	if triggerID := strings.TrimSpace(info.TriggerBeadID); triggerID != "" {
		if tp.Env == nil {
			tp.Env = make(map[string]string)
		}
		// A POOL-LEVEL marker, presence only: this seat exists because the
		// controller counted demand. Nothing on the claim path may read the
		// trigger id to decide what to claim — the pool is pull, and the
		// controller does not choose the bead — but a seat that drains no_work
		// is worth telling apart from one started for any other reason, which is
		// what the divergence diagnostics key on (demand_divergence.go).
		tp.Env["GC_SPAWN_ORIGIN"] = "demand"
		tp.Env["GC_TRIGGER_BEAD_ID"] = triggerID
		tp.Env["GC_TRIGGER_WORK_BEAD_ID"] = triggerID
		if storeRef := strings.TrimSpace(info.TriggerBeadStoreRef); storeRef != "" {
			tp.Env["GC_TRIGGER_BEAD_STORE_REF"] = storeRef
			tp.Env["GC_TRIGGER_WORK_STORE_REF"] = storeRef
		}
		if pack := strings.TrimSpace(info.Pack); pack != "" {
			tp.Env["GC_PACKER_PACK"] = pack
		}
	}
	return tp, nil
}

// canonicalSessionIdentity returns the agent and qualified name to use when
// resolving a pool-managed session bead through resolveTemplate /
// resolveTemplateForSessionBeadInfo. Scoped to the pool case on purpose:
// realizePoolDesiredSessions uses a deep-copied instance agent +
// qualifiedInstance, and this helper is what makes the other pool-backed
// paths (rediscovery, store-backed dependency-floor) agree. GC_ALIAS and
// FingerprintExtra are part of CoreFingerprint, so divergent shapes across
// ticks trip the reconciler's config-drift drain.
//
// Named beads resolve to their own stored alias (configured_named_identity),
// NOT the shared backing template's qualified name. The main named-session
// loop builds the session with spec.Identity; this rediscovery seam must
// return the same identity or ParseQualifiedName binds AgentBase to the
// template's base name, and every named session backed by that template
// resolves work_dir to one shared directory instead of its own. A named bead
// with no stored identity (legacy, pre-stamp) falls back to the template QN,
// unchanged from before. NOTE: this aligns the (agent, qualifiedName) pair and
// therefore WorkDir/AgentBase; the rest of the named TemplateParams Env shape
// (GC_SESSION_ORIGIN=named, ConfiguredNamedIdentity/Mode) is still authored
// only by the named-session loop, so Env-side drift across rediscovery vs.
// that loop remains a separate follow-up.
//
// Rules:
//   - Named bead with stored identity → (cfgAgent, configured_named_identity).
//   - Named bead without stored identity → (cfgAgent, cfgAgent.QualifiedName()).
//   - Non-expanding agent → (cfgAgent, cfgAgent.QualifiedName()).
//   - Instance-expanding agent with a stamped pool_slot → (deepCopyAgent
//     at that slot, qualifiedInstance). Matches realizePoolDesiredSessions.
//   - Instance-expanding agent without a slot stamp → (cfgAgent,
//     cfgAgent.QualifiedName()); realize will claim and stamp later.
func canonicalSessionIdentity(cfgAgent *config.Agent, bead beads.Bead) (*config.Agent, string) {
	return canonicalSessionIdentityWithConfig(nil, cfgAgent, bead)
}

func canonicalSessionIdentityWithConfig(cfg *config.City, cfgAgent *config.Agent, bead beads.Bead) (*config.Agent, string) {
	if cfgAgent == nil {
		return nil, ""
	}
	if isNamedSessionBead(bead) {
		// A named-session bead's work_dir/AgentBase must resolve from its own
		// stored alias, not from the shared backing template. The named-session
		// loop builds the session with spec.Identity; this rediscovery seam must
		// return the same identity or ParseQualifiedName yields the template's
		// base name, collapsing every named session on that template onto one
		// work_dir. In practice this bites on_demand sessions, which can fall
		// through the named-session loop's mode gate and get their identity
		// recomputed here.
		if identity := namedSessionIdentity(bead); identity != "" {
			return cfgAgent, identity
		}
		return cfgAgent, cfgAgent.QualifiedName()
	}
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return cfgAgent, cfgAgent.QualifiedName()
	}
	slot := existingPoolSlotWithConfig(cfg, cfgAgent, bead)
	if slot <= 0 {
		return cfgAgent, cfgAgent.QualifiedName()
	}
	instanceAgent, qualifiedInstance, _ := poolDesiredRequestIdentity(cfgAgent, slot)
	return instanceAgent, qualifiedInstance
}

// canonicalSessionIdentityWithConfigInfo is the session.Info form of
// canonicalSessionIdentityWithConfig: it resolves the (agent, qualifiedName) pair
// from the typed projection, deferring the pool-slot lookup to
// existingPoolSlotWithConfigInfo. Byte-identical to the raw form, oracle-pinned by
// TestCanonicalSessionIdentityWithConfigInfoMatchesRaw.
func canonicalSessionIdentityWithConfigInfo(cfg *config.City, cfgAgent *config.Agent, info session.Info) (*config.Agent, string) {
	if cfgAgent == nil {
		return nil, ""
	}
	if isNamedSessionInfo(info) {
		// Mirror of the raw form's named-bead branch: resolve from the
		// session's own stored alias so rediscovery reproduces the identity
		// the named-session loop assigned, and work_dir does not collapse
		// onto the backing template's path. Both forms must change together —
		// TestCanonicalSessionIdentityWithConfigInfoMatchesRaw pins them to
		// identical behavior. See canonicalSessionIdentityWithConfig.
		if identity := namedSessionIdentityInfo(info); identity != "" {
			return cfgAgent, identity
		}
		return cfgAgent, cfgAgent.QualifiedName()
	}
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return cfgAgent, cfgAgent.QualifiedName()
	}
	slot := existingPoolSlotWithConfigInfo(cfg, cfgAgent, info)
	if slot <= 0 {
		return cfgAgent, cfgAgent.QualifiedName()
	}
	instanceAgent, qualifiedInstance, _ := poolDesiredRequestIdentity(cfgAgent, slot)
	return instanceAgent, qualifiedInstance
}

func sessionBeadQualifiedName(cityPath string, cfgAgent *config.Agent, rigs []config.Rig, sessionBead beads.Bead) string {
	if cfgAgent == nil {
		return ""
	}
	persistedAgentName := normalizeSessionBeadQualifiedName(cfgAgent, sessionBeadAgentName(sessionBead))
	if persistedAgentName != "" {
		if !cfgAgent.SupportsMultipleSessions() || persistedAgentName != cfgAgent.QualifiedName() {
			return persistedAgentName
		}
	}
	explicitName := ""
	if strings.TrimSpace(sessionBead.Metadata["session_name_explicit"]) == boolMetadata(true) {
		explicitName = strings.TrimSpace(sessionBead.Metadata["session_name"])
	}
	// Legacy aliasless pooled beads predate agent_name/session_name_explicit
	// backfills. Their persisted session_name is the only stable concrete
	// identity we can recover during rediscovery, even when it used the
	// historical s-<id> form.
	if explicitName == "" && strings.TrimSpace(sessionBead.Metadata["alias"]) == "" && persistedAgentName == cfgAgent.QualifiedName() && cfgAgent.SupportsMultipleSessions() {
		explicitName = strings.TrimSpace(sessionBead.Metadata["session_name"])
	}
	if explicitName == "" && strings.TrimSpace(sessionBead.Metadata["alias"]) == "" && persistedAgentName == "" && cfgAgent.SupportsMultipleSessions() {
		explicitName = strings.TrimSpace(sessionBead.Metadata["session_name"])
	}
	qualifiedName := workdirutil.SessionQualifiedName(
		cityPath,
		*cfgAgent,
		rigs,
		strings.TrimSpace(sessionBead.Metadata["alias"]),
		explicitName,
	)
	if qualifiedName != "" {
		return qualifiedName
	}
	return cfgAgent.QualifiedName()
}

// sessionBeadQualifiedNameInfo is the session.Info form of
// sessionBeadQualifiedName: it recovers a manual/pooled session's persisted
// qualified identity from the typed projection (agent_name via
// sessionBeadAgentNameInfo, session_name_explicit, alias, raw session_name)
// instead of cracking the raw bead. Byte-identical to the raw form, oracle-pinned
// by TestSessionBeadQualifiedNameInfoMatchesRaw.
func sessionBeadQualifiedNameInfo(cityPath string, cfgAgent *config.Agent, rigs []config.Rig, info session.Info) string {
	if cfgAgent == nil {
		return ""
	}
	persistedAgentName := normalizeSessionBeadQualifiedName(cfgAgent, sessionBeadAgentNameInfo(info))
	if persistedAgentName != "" {
		if !cfgAgent.SupportsMultipleSessions() || persistedAgentName != cfgAgent.QualifiedName() {
			return persistedAgentName
		}
	}
	explicitName := ""
	if strings.TrimSpace(info.SessionNameExplicit) == boolMetadata(true) {
		explicitName = strings.TrimSpace(info.SessionNameMetadata)
	}
	// Legacy aliasless pooled beads predate agent_name/session_name_explicit
	// backfills. Their persisted session_name is the only stable concrete
	// identity we can recover during rediscovery, even when it used the
	// historical s-<id> form.
	if explicitName == "" && strings.TrimSpace(info.Alias) == "" && persistedAgentName == cfgAgent.QualifiedName() && cfgAgent.SupportsMultipleSessions() {
		explicitName = strings.TrimSpace(info.SessionNameMetadata)
	}
	if explicitName == "" && strings.TrimSpace(info.Alias) == "" && persistedAgentName == "" && cfgAgent.SupportsMultipleSessions() {
		explicitName = strings.TrimSpace(info.SessionNameMetadata)
	}
	qualifiedName := workdirutil.SessionQualifiedName(
		cityPath,
		*cfgAgent,
		rigs,
		strings.TrimSpace(info.Alias),
		explicitName,
	)
	if qualifiedName != "" {
		return qualifiedName
	}
	return cfgAgent.QualifiedName()
}

func normalizeSessionBeadQualifiedName(cfgAgent *config.Agent, identity string) string {
	if cfgAgent == nil {
		return strings.TrimSpace(identity)
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return ""
	}
	if identity == cfgAgent.QualifiedName() || strings.Contains(identity, "/") {
		return identity
	}
	if cfgAgent.BindingName != "" && strings.HasPrefix(identity, cfgAgent.BindingName+".") {
		return identity
	}
	return cfgAgent.QualifiedInstanceName(identity)
}

func sessionBeadConfigAgent(cfgAgent *config.Agent, qualifiedName string) *config.Agent {
	if cfgAgent == nil || !cfgAgent.SupportsMultipleSessions() || strings.TrimSpace(qualifiedName) == "" || qualifiedName == cfgAgent.QualifiedName() {
		return cfgAgent
	}
	localName := strings.TrimSpace(qualifiedName)
	if cfgAgent.Dir != "" {
		localName = strings.TrimPrefix(localName, cfgAgent.Dir+"/")
	}
	if cfgAgent.BindingName != "" {
		localName = strings.TrimPrefix(localName, cfgAgent.BindingName+".")
	}
	instanceAgent := deepCopyAgent(cfgAgent, localName, cfgAgent.Dir)
	return &instanceAgent
}

func existingPoolSlot(cfgAgent *config.Agent, sessionBead beads.Bead) int {
	if cfgAgent == nil {
		return 0
	}
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return 0
	}
	if sessionBead.Metadata["pool_slot"] != "" {
		if slot, err := strconv.Atoi(strings.TrimSpace(sessionBead.Metadata["pool_slot"])); err == nil && slot > 0 {
			return slot
		}
	}
	if slot := resolvePersistedPoolIdentitySlot(cfgAgent, true, sessionBeadAgentName(sessionBead), sessionBead.Metadata["alias"]); slot > 0 {
		return slot
	}
	if strings.TrimSpace(sessionBead.Metadata["alias"]) == "" && !beadOwnsPoolSessionName(sessionBead) {
		if slot := resolvePersistedPoolIdentitySlot(cfgAgent, true, sessionBead.Metadata["session_name"]); slot > 0 {
			return slot
		}
	}
	return 0
}

// existingPoolSlotInfo is the session.Info sibling of existingPoolSlot, reading
// typed Info fields (PoolSlot / Alias / SessionNameMetadata) and the Info
// classifiers instead of raw bead metadata. Equivalence-proven.
func existingPoolSlotInfo(cfgAgent *config.Agent, info session.Info) int {
	if cfgAgent == nil {
		return 0
	}
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return 0
	}
	if info.PoolSlot != "" {
		if slot, err := strconv.Atoi(strings.TrimSpace(info.PoolSlot)); err == nil && slot > 0 {
			return slot
		}
	}
	if slot := resolvePersistedPoolIdentitySlot(cfgAgent, true, sessionBeadAgentNameInfo(info), info.Alias); slot > 0 {
		return slot
	}
	if strings.TrimSpace(info.Alias) == "" && !infoOwnsPoolSessionName(info) {
		if slot := resolvePersistedPoolIdentitySlot(cfgAgent, true, info.SessionNameMetadata); slot > 0 {
			return slot
		}
	}
	return 0
}

func resolvePersistedPoolIdentitySlot(cfgAgent *config.Agent, allowLocalIdentity bool, candidates ...string) int {
	if cfgAgent == nil {
		return 0
	}
	for _, name := range candidates {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if slot := resolvePoolSlot(name, cfgAgent.QualifiedName()); slot > 0 {
			return slot
		}
		if cfgAgent.BindingName != "" {
			if slot := resolvePoolSlot(name, cfgAgent.BindingQualifiedName()); slot > 0 {
				return slot
			}
		}
		if cfgAgent.BindingName == "" && allowLocalIdentity {
			if slot := resolvePoolSlot(name, cfgAgent.Name); slot > 0 {
				return slot
			}
		}
		for idx, themed := range cfgAgent.NamepoolNames {
			themed = strings.TrimSpace(themed)
			if themed == "" {
				continue
			}
			if themed == name {
				return idx + 1
			}
			if strings.TrimSpace(cfgAgent.QualifiedInstanceName(themed)) == name {
				return idx + 1
			}
		}
	}
	return 0
}

func poolSlotHasConfiguredBound(cfgAgent *config.Agent) bool {
	if cfgAgent == nil {
		return false
	}
	if len(cfgAgent.NamepoolNames) > 0 {
		return true
	}
	if maxSessions := cfgAgent.EffectiveMaxActiveSessions(); maxSessions != nil {
		return true
	}
	return false
}

func inBoundsPoolSlot(cfgAgent *config.Agent, slot int) bool {
	if cfgAgent == nil || slot <= 0 || !poolSlotHasConfiguredBound(cfgAgent) {
		return false
	}
	if len(cfgAgent.NamepoolNames) > 0 && slot > len(cfgAgent.NamepoolNames) {
		return false
	}
	if maxSessions := cfgAgent.EffectiveMaxActiveSessions(); maxSessions != nil && *maxSessions > 0 && slot > *maxSessions {
		return false
	}
	return true
}

func usablePoolIdentitySlot(cfgAgent *config.Agent, slot int) bool {
	if slot <= 0 {
		return false
	}
	if !poolSlotHasConfiguredBound(cfgAgent) {
		return true
	}
	return inBoundsPoolSlot(cfgAgent, slot)
}

func existingPoolSlotWithConfig(cfg *config.City, cfgAgent *config.Agent, sessionBead beads.Bead) int {
	if cfgAgent == nil {
		return 0
	}
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return 0
	}
	storedTemplateMatches := cfg == nil || storedTemplateMatchesPoolTemplate(sessionBeadStoredTemplate(sessionBead), cfgAgent.QualifiedName(), cfg)
	agentSlot := resolvePersistedPoolIdentitySlot(cfgAgent, storedTemplateMatches, sessionBeadAgentName(sessionBead))
	aliasSlot := resolvePersistedPoolIdentitySlot(cfgAgent, storedTemplateMatches, sessionBead.Metadata["alias"])
	sessionNameSlot := 0
	if storedTemplateMatches && strings.TrimSpace(sessionBead.Metadata["alias"]) == "" && !beadOwnsPoolSessionName(sessionBead) {
		sessionNameSlot = resolvePersistedPoolIdentitySlot(cfgAgent, true, sessionBead.Metadata["session_name"])
	}
	if sessionBead.Metadata["pool_slot"] != "" {
		if slot, err := strconv.Atoi(strings.TrimSpace(sessionBead.Metadata["pool_slot"])); err == nil && slot > 0 {
			if agentSlot > 0 && agentSlot != slot && usablePoolIdentitySlot(cfgAgent, agentSlot) {
				return agentSlot
			}
			if !storedTemplateMatches && agentSlot == 0 && aliasSlot == 0 {
				return 0
			}
			if !inBoundsPoolSlot(cfgAgent, slot) {
				if usablePoolIdentitySlot(cfgAgent, agentSlot) {
					return agentSlot
				}
				if usablePoolIdentitySlot(cfgAgent, aliasSlot) {
					return aliasSlot
				}
				if usablePoolIdentitySlot(cfgAgent, sessionNameSlot) {
					return sessionNameSlot
				}
				if poolSlotHasConfiguredBound(cfgAgent) {
					return 0
				}
			}
			return slot
		}
	}
	if poolSlotHasConfiguredBound(cfgAgent) {
		if !usablePoolIdentitySlot(cfgAgent, agentSlot) {
			agentSlot = 0
		}
		if !usablePoolIdentitySlot(cfgAgent, aliasSlot) {
			aliasSlot = 0
		}
		if !usablePoolIdentitySlot(cfgAgent, sessionNameSlot) {
			sessionNameSlot = 0
		}
	}
	if agentSlot > 0 {
		return agentSlot
	}
	if aliasSlot > 0 {
		return aliasSlot
	}
	if sessionNameSlot > 0 {
		return sessionNameSlot
	}
	return 0
}

// existingPoolSlotWithConfigInfo is the session.Info form of
// existingPoolSlotWithConfig: it resolves a pool bead's persisted slot from the
// typed projection (stored template via sessionBeadStoredTemplateInfo, agent_name
// via sessionBeadAgentNameInfo, alias, session_name, pool_slot) rather than the
// raw bead. Byte-identical to the raw form, oracle-pinned by
// TestExistingPoolSlotWithConfigInfoMatchesRaw.
func existingPoolSlotWithConfigInfo(cfg *config.City, cfgAgent *config.Agent, info session.Info) int {
	if cfgAgent == nil {
		return 0
	}
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return 0
	}
	storedTemplateMatches := cfg == nil || storedTemplateMatchesPoolTemplate(sessionBeadStoredTemplateInfo(info), cfgAgent.QualifiedName(), cfg)
	agentSlot := resolvePersistedPoolIdentitySlot(cfgAgent, storedTemplateMatches, sessionBeadAgentNameInfo(info))
	aliasSlot := resolvePersistedPoolIdentitySlot(cfgAgent, storedTemplateMatches, info.Alias)
	sessionNameSlot := 0
	if storedTemplateMatches && strings.TrimSpace(info.Alias) == "" && !infoOwnsPoolSessionName(info) {
		sessionNameSlot = resolvePersistedPoolIdentitySlot(cfgAgent, true, info.SessionNameMetadata)
	}
	if info.PoolSlot != "" {
		if slot, err := strconv.Atoi(strings.TrimSpace(info.PoolSlot)); err == nil && slot > 0 {
			if agentSlot > 0 && agentSlot != slot && usablePoolIdentitySlot(cfgAgent, agentSlot) {
				return agentSlot
			}
			if !storedTemplateMatches && agentSlot == 0 && aliasSlot == 0 {
				return 0
			}
			if !inBoundsPoolSlot(cfgAgent, slot) {
				if usablePoolIdentitySlot(cfgAgent, agentSlot) {
					return agentSlot
				}
				if usablePoolIdentitySlot(cfgAgent, aliasSlot) {
					return aliasSlot
				}
				if usablePoolIdentitySlot(cfgAgent, sessionNameSlot) {
					return sessionNameSlot
				}
				if poolSlotHasConfiguredBound(cfgAgent) {
					return 0
				}
			}
			return slot
		}
	}
	if poolSlotHasConfiguredBound(cfgAgent) {
		if !usablePoolIdentitySlot(cfgAgent, agentSlot) {
			agentSlot = 0
		}
		if !usablePoolIdentitySlot(cfgAgent, aliasSlot) {
			aliasSlot = 0
		}
		if !usablePoolIdentitySlot(cfgAgent, sessionNameSlot) {
			sessionNameSlot = 0
		}
	}
	if agentSlot > 0 {
		return agentSlot
	}
	if aliasSlot > 0 {
		return aliasSlot
	}
	if sessionNameSlot > 0 {
		return sessionNameSlot
	}
	return 0
}

// poolSessionCreatePlan describes a fresh pool session bead that has been
// selected for creation by the planning phase. Materializing the plan via
// executePlannedPoolSessionBeadCreate performs the slow per-alias-locked
// dolt write and is safe to call concurrently across distinct
// qualifiedInstance values.
type poolSessionCreatePlan struct {
	qualifiedInstance string
	slot              int
	poolSlot          int
	metadata          map[string]string
}

func selectOrCreatePoolSessionBead(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	preferred *session.Info,
	used map[string]bool,
	usedSlots map[int]bool,
) (session.Info, int, error) {
	info, slot, plan, err := selectOrPlanPoolSessionBead(bp, cfgAgent, template, preferred, SessionRequest{}, time.Now(), used, usedSlots)
	if err != nil {
		return session.Info{}, 0, err
	}
	if plan == nil {
		return info, slot, nil
	}
	info, err = executePlannedPoolSessionBeadCreate(bp, cfgAgent, template, *plan)
	if err != nil {
		delete(usedSlots, plan.slot)
		return info, 0, err
	}
	return info, plan.poolSlot, nil
}

// selectOrPlanPoolSessionBead performs the in-memory selection phase of pool
// session provisioning. It returns one of:
//   - reuse: (bead, slot, nil, nil) where bead is an existing session bead to
//     reuse for this request.
//   - plan:  (zero bead, 0, *plan, nil) where plan describes a fresh bead to
//     be materialized by executePlannedPoolSessionBeadCreate.
//   - error: (zero bead, 0, nil, err) when selection fails (e.g., concrete
//     slot already claimed).
//
// Callers MUST serialize calls that share the same used / usedSlots maps; the
// function mutates both. The plan path defers the slow per-alias-locked dolt
// write to a subsequent (possibly parallel) step so realizePoolDesiredSessions
// can drive distinct aliases concurrently.
func selectOrPlanPoolSessionBead(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	preferred *session.Info,
	request SessionRequest,
	decisionTime time.Time,
	used map[string]bool,
	usedSlots map[int]bool,
) (session.Info, int, *poolSessionCreatePlan, error) {
	if cfgAgent == nil {
		cfgAgent = findAgentByTemplate(&config.City{Agents: bp.agents}, template)
	}
	if cfgAgent == nil {
		return session.Info{}, 0, nil, fmt.Errorf("pool template %q has no configured agent", template)
	}
	// Resume tier: reuse the session that has in-progress work assigned.
	if preferred != nil && preferred.ID != "" && !used[preferred.ID] && !isFailedCreateSessionInfo(*preferred) {
		preserveAboveCapacity := poolRequestResumesAssignedWorkInfo(
			request,
			bp.assignedWorkBeads,
			*preferred,
		)
		slot := claimPreferredPoolSlotWithConfigInfo(
			bp.city,
			cfgAgent,
			*preferred,
			preserveAboveCapacity,
			usedSlots,
		)
		if slot == 0 && !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
			return session.Info{}, 0, nil, fmt.Errorf("pool session %s concrete slot already claimed", preferred.ID)
		}
		if isManualSessionInfoForAgent(*preferred, cfgAgent) {
			return *preferred, slot, nil, nil
		}
		info, err := normalizeNonExpandingPoolSessionInfoForSelection(bp, cfgAgent, *preferred)
		return info, slot, nil, err
	}
	if canonical, ok := findReusableCanonicalNonExpandingPoolSessionInfoForRequest(bp, cfgAgent, template, request, decisionTime, used); ok {
		slot := claimDesiredPoolSlotInfo(bp.city, cfgAgent, canonical, usedSlots)
		info, err := normalizeNonExpandingPoolSessionInfoForSelection(bp, cfgAgent, canonical)
		return info, slot, nil, err
	}
	// Reuse an existing active/creating session bead. Skip drained, closed,
	// and asleep — asleep ephemerals are not restarted; a fresh session is
	// created instead. The reconciler closes orphaned asleep beads.
	for _, candidate := range reusablePoolSessionInfosForRequest(bp, cfgAgent, template, request, decisionTime, used) {
		if desiredName := strings.TrimSpace(candidate.SessionNameMetadata); desiredName != "" {
			slot := claimDesiredPoolSlotInfo(bp.city, cfgAgent, candidate, usedSlots)
			if slot == 0 && !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
				continue
			}
			info, err := normalizeNonExpandingPoolSessionInfoForSelection(bp, cfgAgent, candidate)
			return info, slot, nil, err
		}
	}
	if !bp.hasCompleteSessionSnapshot() {
		return session.Info{}, 0, nil, errPoolSessionCreatePartial
	}
	slot, err := claimFreshPoolSlotInfo(bp, cfgAgent, usedSlots)
	if err != nil {
		return session.Info{}, 0, nil, err
	}
	_, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(cfgAgent, slot)
	metadata, err := poolTriggerMetadata(bp, cfgAgent, qualifiedInstance, request)
	if err != nil {
		delete(usedSlots, slot)
		return session.Info{}, 0, nil, err
	}

	if bp.poolScaleCheckPartialTemplates[template] {
		delete(usedSlots, slot)
		return session.Info{}, 0, nil, errPoolSessionCreatePartial
	}

	// Provider-health gate: refuse new creates when the registry reports this
	// agent's provider as red. Reuse paths above are unaffected (they return
	// before reaching this point). loadProviderHealthSnapshot always returns a
	// non-nil snapshot; check() fails-open when the registry is absent or stale.
	// Symmetric with the respawn gate in session_reconciler.go:2424.
	provName := strings.TrimSpace(cfgAgent.Provider)
	if provName == "" {
		provName = strings.TrimSpace(cfgAgent.InheritedProvider)
	}
	if provName == "" && bp.workspace != nil {
		provName = strings.TrimSpace(bp.workspace.Provider)
	}
	if healthy, present := bp.providerHealthSnapshot.check(provName); present && !healthy {
		delete(usedSlots, slot)
		return session.Info{}, 0, nil, errPoolSessionCreateProviderRed
	}

	if !bp.tryClaimPoolSessionCreate(template) {
		delete(usedSlots, slot)
		return session.Info{}, 0, nil, errPoolSessionCreateBudgetExhausted
	}

	plan := &poolSessionCreatePlan{
		qualifiedInstance: qualifiedInstance,
		slot:              slot,
		poolSlot:          poolSlot,
		metadata:          metadata,
	}
	return session.Info{}, 0, plan, nil
}

// freshPoolSlotUpperBound returns the highest concrete numbered slot a fresh
// create may claim. A zero upper bound with canonical=true denotes the enabled
// canonical singleton identity (slot 0). The configured agent-local maximum is
// the only numeric capacity bound here; rig/workspace inherited caps govern
// scheduling elsewhere and must not silently reshape concrete pool identities.
func freshPoolSlotUpperBound(cfgAgent *config.Agent) (upper int, unlimited, canonical bool, err error) {
	if cfgAgent == nil {
		return 0, false, false, fmt.Errorf("%w: pool agent unavailable", errPoolSessionNameUnavailable)
	}
	maxSessions := cfgAgent.EffectiveMaxActiveSessions()
	if maxSessions != nil && *maxSessions == 0 {
		return 0, false, false, fmt.Errorf("%w: pool template %q disables fresh sessions", errPoolSessionNameUnavailable, cfgAgent.QualifiedName())
	}
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return 0, false, true, nil
	}
	unlimited = maxSessions == nil || *maxSessions < 0
	if !unlimited {
		upper = *maxSessions
	}
	if namepoolBound := len(cfgAgent.NamepoolNames); namepoolBound > 0 {
		if unlimited || namepoolBound < upper {
			upper = namepoolBound
		}
		unlimited = false
	}
	if !unlimited && upper <= 0 {
		return 0, false, false, fmt.Errorf("%w: pool template %q has no fresh slot capacity", errPoolSessionNameUnavailable, cfgAgent.QualifiedName())
	}
	return upper, unlimited, false, nil
}

// freshPoolOccupancyInfos unions the frozen cross-store census with the
// mutable primary snapshot. The latter gains rows created earlier in this
// build, while the former retains foreign-store holders the primary snapshot
// cannot see. First occurrence wins by session ID, matching the census fold.
func freshPoolOccupancyInfos(bp *agentBuildParams) []session.Info {
	if bp == nil {
		return nil
	}
	primary := bp.sessionBeads.OpenInfos()
	infos := make([]session.Info, 0, len(bp.sessionOccupancyInfos)+len(primary))
	seen := make(map[string]bool, len(bp.sessionOccupancyInfos)+len(primary))
	appendInfo := func(info session.Info) {
		id := strings.TrimSpace(info.ID)
		if id != "" && seen[id] {
			return
		}
		if id != "" {
			seen[id] = true
		}
		infos = append(infos, info)
	}
	for _, info := range bp.sessionOccupancyInfos {
		appendInfo(info)
	}
	for _, info := range primary {
		appendInfo(info)
	}
	return infos
}

// freshPoolAvailabilityInfos re-reads every configured session-census leg
// while the caller holds all derived identifier locks, then conservatively
// unions that current view with the build's original complete census and the
// mutable primary snapshot. A holder that disappeared after planning therefore
// cannot open a name during the same build, while a holder created in any
// foreign leg after planning is visible before mutation.
//
// Exact duplicate projections are folded, but two projections of the same
// stable bead ID with different alias/name/identity fields are deliberately
// retained: either spelling may still be observed by a concurrent runtime and
// must continue to reserve its namespace for this build.
func freshPoolAvailabilityInfos(bp *agentBuildParams) ([]session.Info, error) {
	if bp == nil {
		return nil, fmt.Errorf("refreshing pool session availability: build params unavailable")
	}
	fresh, err := collectAllOpenSessionAvailabilityInfos(
		bp.cityPath,
		bp.city,
		bp.beadStore,
		bp.sessionCensusRigStores,
		bp.sessionCensusSuspendedRigPaths,
	)
	if err != nil {
		return nil, fmt.Errorf("refreshing complete pool session availability: %w", err)
	}
	primary := bp.sessionBeads.OpenInfos()
	infos := make([]session.Info, 0, len(fresh)+len(bp.sessionOccupancyInfos)+len(primary))
	seen := make(map[string]bool, cap(infos))
	appendInfo := func(info session.Info) {
		key := strings.Join([]string{
			strings.TrimSpace(info.ID),
			strings.TrimSpace(info.SessionNameMetadata),
			strings.TrimSpace(info.Alias),
			strings.TrimSpace(info.AgentName),
		}, "\x00")
		if seen[key] {
			return
		}
		seen[key] = true
		infos = append(infos, info)
	}
	for _, info := range fresh {
		appendInfo(info)
	}
	for _, info := range bp.sessionOccupancyInfos {
		appendInfo(info)
	}
	for _, info := range primary {
		appendInfo(info)
	}
	return infos, nil
}

// poolAliasCollisionFromInfos mirrors the live-session half of
// session.EnsureAliasAvailableWithConfig over an already complete typed census.
// Config reservations and primary-store query errors remain the exported
// helper's responsibility; this adds the foreign session legs it cannot see.
func poolAliasCollisionFromInfos(infos []session.Info, alias string) error {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil
	}
	for _, info := range infos {
		if info.Closed || isFailedCreateSessionInfo(info) {
			continue
		}
		switch {
		case strings.TrimSpace(info.SessionNameMetadata) == alias:
			return fmt.Errorf("%w: %q conflicts with session name on %s", session.ErrSessionAliasExists, alias, info.ID)
		case strings.TrimSpace(info.Alias) == alias:
			return fmt.Errorf("%w: %q already belongs to %s", session.ErrSessionAliasExists, alias, info.ID)
		case strings.TrimSpace(info.AgentName) == alias:
			return fmt.Errorf("%w: %q conflicts with concrete session identity on %s", session.ErrSessionAliasExists, alias, info.ID)
		}
	}
	return nil
}

// claimFreshPoolSlotInfo selects the lowest free in-cap concrete slot for a
// fresh pool row. Snapshot holders contribute to a local occupied union only:
// adding them to the shared request-order usedSlots map would let an anonymous
// request processed first block a later concrete resume of that same holder.
// Only the newly selected slot is published to usedSlots.
func claimFreshPoolSlotInfo(bp *agentBuildParams, cfgAgent *config.Agent, usedSlots map[int]bool) (int, error) {
	upper, unlimited, canonical, err := freshPoolSlotUpperBound(cfgAgent)
	if err != nil {
		return 0, err
	}
	occupancyInfos := freshPoolOccupancyInfos(bp)
	if canonical {
		if usedSlots[0] {
			return 0, fmt.Errorf("%w: canonical pool template %q is already selected", errPoolSessionNameUnavailable, cfgAgent.QualifiedName())
		}
		for _, info := range occupancyInfos {
			if isFailedCreateSessionInfo(info) {
				continue
			}
			if infoIdentifiesAsCanonical(info, cfgAgent.QualifiedName()) {
				return 0, fmt.Errorf("%w: canonical pool template %q is held by session %s", errPoolSessionNameUnavailable, cfgAgent.QualifiedName(), info.ID)
			}
		}
		usedSlots[0] = true
		return 0, nil
	}
	occupied := make(map[int]bool, len(usedSlots))
	for slot, used := range usedSlots {
		if used {
			occupied[slot] = true
		}
	}
	if bp != nil {
		for _, info := range occupancyInfos {
			// An open failed-create row keeps its exact runtime-name lease, but it
			// does not consume allocator capacity. The later name check therefore
			// retries this stable slot and fails it closed until rollback closes the
			// row, instead of leaking a replacement into the next slot.
			if isFailedCreateSessionInfo(info) {
				continue
			}
			slot := existingPoolSlotWithConfigInfo(bp.city, cfgAgent, info)
			if slot <= 0 || (!unlimited && slot > upper) {
				continue
			}
			occupied[slot] = true
		}
	}
	for slot := 1; unlimited || slot <= upper; slot++ {
		if occupied[slot] {
			continue
		}
		usedSlots[slot] = true
		return slot, nil
	}
	return 0, fmt.Errorf("%w: pool template %q has no free concrete slot", errPoolSessionNameUnavailable, cfgAgent.QualifiedName())
}

func poolTriggerMetadata(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, request SessionRequest) (map[string]string, error) {
	workID := strings.TrimSpace(request.WorkBeadID)
	if workID == "" {
		return nil, nil
	}
	metadata := map[string]string{
		beadmeta.TriggerBeadIDMetadataKey: workID,
	}
	if storeRef := strings.TrimSpace(request.WorkStoreRef); storeRef != "" {
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = storeRef
	}
	if parentSID := strings.TrimSpace(request.BrainParentSID); parentSID != "" {
		metadata[beadmeta.BrainParentSIDMetadataKey] = parentSID
	}
	if pack := strings.TrimSpace(request.WorkPack); pack != "" {
		metadata[beadmeta.PackMetadataKey] = pack
	}
	if workspace := packWorkspaceSlug(request); workspace != "" {
		metadata[beadmeta.PackWorkspaceMetadataKey] = workspace
	}
	workDir, err := verifiedPoolTriggerWorkDir(bp, cfgAgent, qualifiedName, request)
	if err != nil {
		return nil, err
	}
	if workDir != "" {
		metadata[beadmeta.WorkDirMetadataKey] = workDir
		metadata[beadmeta.LegacyWorkDirMetadataKey] = workDir
	}
	return metadata, nil
}

// executePlannedPoolSessionBeadCreate materializes a pool session bead from a
// plan produced by selectOrPlanPoolSessionBead. The underlying call is
// createPoolSessionBeadWithGuardedAlias, whose per-identity session locks make
// concurrent invocations safe across both distinct qualifiedInstance values
// and shared resolved tmux_alias values.
func executePlannedPoolSessionBeadCreate(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	plan poolSessionCreatePlan,
) (session.Info, error) {
	info, err := createPoolSessionBeadWithGuardedAlias(bp, cfgAgent, template, plan.qualifiedInstance, plan.slot, plan.metadata)
	if err != nil {
		bp.releasePoolSessionCreate()
	}
	return info, err
}

func beadIdentifiesAsCanonical(bead beads.Bead, canonical string) bool {
	canonical = strings.TrimSpace(canonical)
	if canonical == "" {
		return false
	}
	return strings.TrimSpace(bead.Metadata["agent_name"]) == canonical ||
		strings.TrimSpace(bead.Metadata["alias"]) == canonical ||
		strings.TrimSpace(bead.Title) == canonical ||
		containsString(bead.Labels, "agent:"+canonical)
}

// infoIdentifiesAsCanonical is the session.Info mirror of beadIdentifiesAsCanonical.
func infoIdentifiesAsCanonical(i session.Info, canonical string) bool {
	canonical = strings.TrimSpace(canonical)
	if canonical == "" {
		return false
	}
	return strings.TrimSpace(i.AgentName) == canonical ||
		strings.TrimSpace(i.Alias) == canonical ||
		strings.TrimSpace(i.Title) == canonical ||
		containsString(i.Labels, "agent:"+canonical)
}

type poolSessionIdentifierLockFunc func(string, []string, func() error) error

func poolSessionCreateLockIdentifiers(
	identifiers poolSessionIdentifiers,
	qualifiedAlias string,
	resolvedTmuxAlias string,
) []string {
	seen := make(map[string]bool, len(identifiers.availabilityNames)+2)
	lockIDs := make([]string, 0, len(identifiers.availabilityNames)+2)
	add := func(identifier string) {
		identifier = strings.TrimSpace(identifier)
		if identifier == "" || seen[identifier] {
			return
		}
		seen[identifier] = true
		lockIDs = append(lockIDs, identifier)
	}
	for _, identifier := range identifiers.availabilityNames {
		add(identifier)
	}
	// The qualified pool alias and configured base tmux alias are reservation
	// participants even when the proven-alias-collision path ultimately defers
	// alias persistence. Keeping both in the same city-scoped lock set joins
	// this create with every other creator that addresses either spelling.
	add(qualifiedAlias)
	add(resolvedTmuxAlias)
	sort.Strings(lockIDs)
	return lockIDs
}

func createPoolSessionBeadWithGuardedAlias(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	qualifiedInstance string,
	slot int,
	metadata map[string]string,
) (session.Info, error) {
	return createPoolSessionBeadWithGuardedAliasUsingLock(
		bp,
		cfgAgent,
		template,
		qualifiedInstance,
		slot,
		metadata,
		session.WithCitySessionIdentifierLocks,
	)
}

func createPoolSessionBeadWithGuardedAliasUsingLock(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	qualifiedInstance string,
	slot int,
	metadata map[string]string,
	withLocks poolSessionIdentifierLockFunc,
) (session.Info, error) {
	if bp == nil {
		return session.Info{}, fmt.Errorf("creating pool session for %q: build params unavailable", template)
	}
	if withLocks == nil {
		return session.Info{}, fmt.Errorf("creating pool session for %q: identifier locker unavailable", template)
	}
	if err := validateAgentSessionTransportForBuild(bp, cfgAgent, qualifiedInstance); err != nil {
		return session.Info{}, err
	}
	resolvedTmuxAlias, err := bp.resolveTmuxAliasForAgent(cfgAgent)
	if err != nil {
		return session.Info{}, err
	}
	resolvedTmuxAlias, err = validateResolvedPoolTmuxAlias(template, resolvedTmuxAlias)
	if err != nil {
		return session.Info{}, err
	}
	// A transient slot is a rebinding chair, not an occupant identity. It drives
	// two decisions here: its runtime session_name must step aside from the bare
	// slot (TransientSlot, consumed in derivePoolSessionName), and it is never
	// persisted as an alias (persistAlias below).
	transientSlot := usesTransientPoolSlotIdentity(cfgAgent)
	qualifiedInstance = strings.TrimSpace(qualifiedInstance)
	identityAgentName := qualifiedInstance
	if identityAgentName == "" {
		identityAgentName = template
	}
	identity := poolSessionCreateIdentity{
		AgentName:     identityAgentName,
		Slot:          slot,
		Metadata:      metadata,
		TransientSlot: transientSlot,
	}
	// A transient slot is never reserved as an alias: it is not an identity, so
	// there is nothing to guard against collision and nothing to persist. The
	// resolvedTmuxAlias lock lane below is unaffected — that one guards a
	// runtime handle, not ownership.
	//
	// alias and persistAlias are deliberately different things. alias is the
	// mutual-exclusion key: two controller goroutines racing the same slot must
	// not both create a bead for it, and that is true whether or not the slot is
	// an identity. persistAlias is what lands in the bead. For a transient slot
	// the exclusion still applies and the persistence does not.
	alias := qualifiedInstance
	persistAlias := alias
	if transientSlot {
		persistAlias = ""
	}
	identifiers, err := derivePoolSessionIdentifiers(bp.city, template, identity, resolvedTmuxAlias)
	if err != nil {
		return session.Info{}, err
	}
	if bp.beadStore == nil {
		return createPoolSessionBeadWithIdentifiers(bp.beadStore, template, bp.city, bp.sessionBeads, bp.sessionBeads, poolSessionCreateStartedAt(bp), identity, identifiers)
	}
	lockIDs := poolSessionCreateLockIdentifiers(identifiers, alias, resolvedTmuxAlias)

	var info session.Info
	lockErr := withLocks(bp.cityPath, lockIDs, func() error {
		createIdentity := identity
		// The exact identifier locks fence compliant creators across every store,
		// but the pre-lock planning census is not current enough to prove absence.
		// Re-read the full session topology under the lock. Any partial leg makes
		// both alias and runtime-name answers unprovable, so fail before mutation.
		availabilityInfos, availabilityErr := freshPoolAvailabilityInfos(bp)
		if availabilityErr != nil {
			return fmt.Errorf("checking locked pool availability for template %q: %w", template, availabilityErr)
		}
		if alias != "" {
			// A proven collision retains the long-standing alias-deferral contract:
			// the pool bead is created without a public alias and sync records
			// pool_alias_conflict. An unanswerable reservation query is different:
			// it is not proof of a collision and must fail the entire create closed.
			aliasErr := session.EnsureAliasAvailableWithConfig(bp.beadStore, bp.city, alias, "")
			if aliasErr == nil {
				aliasErr = poolAliasCollisionFromInfos(availabilityInfos, alias)
			}
			switch {
			case aliasErr == nil:
				createIdentity.Alias = persistAlias
			case errors.Is(aliasErr, session.ErrSessionAliasExists):
				// Proven collision: intentionally defer alias persistence.
			default:
				return fmt.Errorf("checking pool alias %q for template %q: %w", alias, template, aliasErr)
			}
		}
		var createErr error
		// Only the primary snapshot receives the successful-create writeback.
		availabilitySnapshot := newSessionBeadSnapshotFromInfos(availabilityInfos)
		info, createErr = createPoolSessionBeadWithIdentifiers(
			bp.beadStore,
			template,
			bp.city,
			availabilitySnapshot,
			bp.sessionBeads,
			poolSessionCreateStartedAt(bp),
			createIdentity,
			identifiers,
		)
		return createErr
	})
	return info, lockErr
}

func isFailedCreateSessionBead(bead beads.Bead) bool {
	return strings.TrimSpace(bead.Metadata["state"]) == string(session.StateFailedCreate)
}

// isFailedCreateSessionInfo is the session.Info mirror of
// isFailedCreateSessionBead. It reads the RAW metadata state (Info.MetadataState),
// not the normalized/closed-blanked Info.State.
func isFailedCreateSessionInfo(i session.Info) bool {
	return strings.TrimSpace(i.MetadataState) == string(session.StateFailedCreate)
}

// sessionBeadHasAssignedWorkInfo reports whether any open/in-progress work bead is
// assigned to the session: the SESSION side reads typed Info fields (ID,
// SessionNameMetadata, ConfiguredNamedIdentity) while the WORK bead slice stays raw
// (ClassWork — Bead is the domain object). It is the production reuse predicate the
// pool selection path calls; its behavior is pinned by TestSessionBeadHasAssignedWorkInfo
// (WI-7 W-delete retired the raw sessionBeadHasAssignedWork equivalence reference along
// with the rest of the raw pool cluster and re-pointed the pin to a golden).
func sessionBeadHasAssignedWorkInfo(workBeads []beads.Bead, info session.Info) bool {
	for _, wb := range workBeads {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" || (wb.Status != "open" && wb.Status != "in_progress") {
			continue
		}
		if assignee == info.ID || assignee == strings.TrimSpace(info.SessionNameMetadata) {
			return true
		}
		if namedIdentity := strings.TrimSpace(info.ConfiguredNamedIdentity); namedIdentity != "" && assignee == namedIdentity {
			return true
		}
	}
	return false
}

// poolRequestResumesAssignedWorkInfo proves that a concrete resume request is
// still backed by its exact actionable work bead and that the bead is assigned
// through any current or historical identity of the preferred session.
func poolRequestResumesAssignedWorkInfo(request SessionRequest, workBeads []beads.Bead, info session.Info) bool {
	workBeadID := strings.TrimSpace(request.WorkBeadID)
	if request.Tier != "resume" || request.SessionBeadID != info.ID || workBeadID == "" {
		return false
	}
	for _, wb := range workBeads {
		if wb.ID != workBeadID || (wb.Status != "open" && wb.Status != "in_progress") {
			continue
		}
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" {
			return false
		}
		for _, identity := range sessionBeadAssigneeIdentitiesInfo(info) {
			if assignee == identity {
				return true
			}
		}
		return false
	}
	return false
}

// sessionAssigneeMatch is an entry in the assignee-identity index: the session
// a work bead's Assignee resolves to, or ambiguous=true when more than one open
// session claims the same identity (a transient duplicate-alias state). An
// ambiguous identity is skipped, never guessed — the stamp is best-effort and
// must not attach the wrong session, mirroring the canonical resolver's
// fail-on-conflict posture (internal/session.ResolveSession) in a non-fatal
// form.
type sessionAssigneeMatch struct {
	info      session.Info
	ambiguous bool
}

// buildSessionAssigneeIndex maps every assignment identity an open session can
// be claimed under to that session, computed once per reconcile. OpenInfos()
// copies the session slice, so resolving per work bead would otherwise cost
// O(workBeads × openSessions). Identities come from sessionBeadAssigneeIdentitiesInfo
// — bead ID, session_name, configured named identity, current alias, AND prior
// aliases (alias_history) — so a bead assigned under a since-rotated pool alias
// still resolves. An identity claimed by two different sessions is marked
// ambiguous. The SESSION side is typed session.Info (WI-5 W3): OpenInfos()[i] is
// byte-identical to the Info projection of Open()[i].
func buildSessionAssigneeIndex(sessionBeads *sessionBeadSnapshot) map[string]sessionAssigneeMatch {
	index := make(map[string]sessionAssigneeMatch)
	if sessionBeads == nil {
		return index
	}
	for _, sb := range sessionBeads.OpenInfos() {
		for _, identity := range sessionBeadAssigneeIdentitiesInfo(sb) {
			if existing, ok := index[identity]; ok {
				if !existing.ambiguous && existing.info.ID != sb.ID {
					index[identity] = sessionAssigneeMatch{ambiguous: true}
				}
				continue
			}
			index[identity] = sessionAssigneeMatch{info: sb}
		}
	}
	return index
}

// sessionBeadIdentifier returns the most resolvable name for a session: its
// session_name when set (pool workers), else its alias or configured named
// identity (named sessions carry an empty session_name and identify by alias —
// e.g. "mayor"). All three appear in the supervisor session-list index that
// consumers match against, so this is the value to stamp as gc.session_name.
func sessionBeadIdentifier(sb beads.Bead) string {
	for _, key := range []string{"session_name", "alias", "configured_named_identity"} {
		if v := strings.TrimSpace(sb.Metadata[key]); v != "" {
			return v
		}
	}
	return ""
}

// sessionBeadIdentifierInfo is the session.Info form of sessionBeadIdentifier:
// it reads the RAW session_name (Info.SessionNameMetadata, no sessionNameFor
// fallback), then alias, then configured named identity — byte-identical to the
// raw form (oracle-pinned).
func sessionBeadIdentifierInfo(info session.Info) string {
	for _, v := range []string{info.SessionNameMetadata, info.Alias, info.ConfiguredNamedIdentity} {
		if v := strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// stampRunSessionIdentity durably records, on each in-progress assigned work
// bead, the session_name and work_dir of the session executing it.
//
// The session↔bead link (Assignee) is transient: it is cleared when the bead
// closes, so a consumer that reads a completed run (e.g. the dashboard's
// session-drill-in and per-run diff panels) has no way to resolve which
// session ran it or in which worktree. Stamping gc.session_name + gc.work_dir
// at execution time makes that link durable — the existing dashboard resolvers
// then attach the session and derive the worktree with no consumer changes.
//
// Idempotent by design: it writes only when the resolved value differs from
// what is already on the bead, so steady-state reconciles perform no writes;
// only a newly claimed (or reassigned) bead triggers a single write. A write
// failure is logged and skipped — stamping is best-effort observability and
// must never block reconciliation.
func stampRunSessionIdentity(workBeads []beads.Bead, workStores []beads.Store, sessionBeads *sessionBeadSnapshot, stderr io.Writer) {
	if sessionBeads == nil || len(workBeads) != len(workStores) {
		return
	}
	sessionByAssignee := buildSessionAssigneeIndex(sessionBeads)
	// Roots stamped this pass, so a multi-step run's shared root is resolved
	// once rather than per step.
	stampedRoots := map[string]struct{}{}
	for i, wb := range workBeads {
		if wb.Status != "in_progress" {
			continue
		}
		store := workStores[i]
		if store == nil {
			continue
		}
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" {
			continue
		}
		match, ok := sessionByAssignee[assignee]
		if !ok || match.ambiguous {
			continue
		}
		sbInfo := match.info
		sessionName := sessionBeadIdentifierInfo(sbInfo)
		workDir := strings.TrimSpace(sbInfo.WorkDir)
		if sessionName == "" && workDir == "" {
			continue
		}
		patch := map[string]string{}
		if sessionName != "" && strings.TrimSpace(wb.Metadata[beadmeta.SessionNameMetadataKey]) != sessionName {
			patch[beadmeta.SessionNameMetadataKey] = sessionName
		}
		if workDir != "" && strings.TrimSpace(wb.Metadata[beadmeta.WorkDirMetadataKey]) != workDir &&
			(!sbInfo.PoolManaged || workDirStampHasOwnershipEvidence(wb.Metadata, workDir)) {
			patch[beadmeta.WorkDirMetadataKey] = workDir
		}
		if len(patch) > 0 {
			if err := store.SetMetadataBatch(wb.ID, patch); err != nil && stderr != nil {
				fmt.Fprintf(stderr, "stampRunSessionIdentity: %s: %v\n", wb.ID, err) //nolint:errcheck
			}
		}
		// Propagate to the run root. The molecule root (gc.kind=workflow) is a
		// control-lane bead — never in_progress+assigned — so it is not in
		// workBeads and route-time stamping skips it for pool agents. The
		// dashboard's root-only snapshot reads the root's own metadata, so a
		// worked step back-fills its root via gc.root_bead_id. (#2843)
		stampRunRootFromStep(store, wb, sessionName, workDir, !sbInfo.PoolManaged, stampedRoots, stderr)
	}
}

// workDirStampHasOwnershipEvidence reports whether existing bead metadata
// already identifies workDir as the work artifact directory. Session
// reconciliation of a pool session observes a slot cwd; it does not create or
// own a managed worktree, so it must not manufacture gc.work_dir on an
// arbitrary routed bead. Doing so turns a pool slot label into incomplete
// worktree ownership evidence and makes demand fail closed. The worktree
// creator writes the legacy artifact path first; reconciliation may only mirror
// that value. Non-pool sessions retain the historical observability stamp.
func workDirStampHasOwnershipEvidence(metadata map[string]string, workDir string) bool {
	legacy := strings.TrimSpace(metadata[beadmeta.LegacyWorkDirMetadataKey])
	return legacy != "" && legacy == workDir
}

// stampRunRootFromStep copies a step's resolved session_name/work_dir onto its
// workflow root (gc.root_bead_id), once per root per pass. Idempotent: it reads
// the root and writes only the keys that differ. Best-effort — a root that is
// in another store, already gone, or already stamped is silently skipped (a
// cross-store root gets stamped on its own store's reconcile pass).
func stampRunRootFromStep(store beads.Store, step beads.Bead, sessionName, workDir string, allowUnownedWorkDir bool, stampedRoots map[string]struct{}, stderr io.Writer) {
	rootID := strings.TrimSpace(step.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" || rootID == step.ID {
		return
	}
	if _, done := stampedRoots[rootID]; done {
		return
	}
	root, err := store.Get(rootID)
	if err != nil {
		// Cross-store / missing / transient — do NOT mark stamped, so a later
		// step this pass (or a later reconcile) can retry resolving the root.
		return
	}
	stampedRoots[rootID] = struct{}{}
	patch := map[string]string{}
	if sessionName != "" && strings.TrimSpace(root.Metadata[beadmeta.SessionNameMetadataKey]) != sessionName {
		patch[beadmeta.SessionNameMetadataKey] = sessionName
	}
	if workDir != "" && strings.TrimSpace(root.Metadata[beadmeta.WorkDirMetadataKey]) != workDir &&
		(allowUnownedWorkDir || workDirStampHasOwnershipEvidence(root.Metadata, workDir)) {
		patch[beadmeta.WorkDirMetadataKey] = workDir
	}
	if len(patch) == 0 {
		return
	}
	if err := store.SetMetadataBatch(rootID, patch); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "stampRunSessionIdentity root %s: %v\n", rootID, err) //nolint:errcheck
	}
}

// canonicalizeLegacyBoundAssignedWork re-homes the Assignee and gc.routed_to of
// actionable pool work that is pre-assigned to a legacy template identity to
// that agent's current canonical identity. It handles both legacy bound forms
// for a now-unbound agent (e.g. "dir/binding.name" -> "dir/name") and legacy
// unbound forms for a now-binding-qualified import (e.g.
// "dir/name" -> "dir/binding.name").
//
// Why: after a binding-prefix migration, the awake/scale accounting wakes a
// canonical pool session for work persisted under the legacy identity (it
// normalizes template identities), but the woken session's work_query and
// `gc hook --claim` path match assignees and routes by raw string. A canonical
// session cannot derive the old spelling, so the triggering bead would surface
// to no one and stay unclaimed. Re-homing the persisted identity to canonical
// makes the bead behave exactly like ordinary canonical pool work, which the
// existing surface/claim machinery already resumes — closing the agent-side half
// of the migration recovery loop.
//
// The live-session guard preserves the resume tier: work a still-running
// session already owns under the legacy identity is left untouched so its own
// work_query keeps resolving it; only orphaned work with no live owner (the
// wake-known-identity case) is re-homed.
//
// Because a re-home moves an assignee away from its owner, it runs only on a
// complete session snapshot. Unlike the benign stampRunSessionIdentity pass, a
// nil or load-errored snapshot can omit a live legacy owner, and the
// live-session guard would then misread that absence as "orphaned" and re-home
// in-flight work out from under the running session. On a degraded snapshot the
// whole pass is skipped and a later complete tick converges it (NDI), mirroring
// releaseOrphanedPoolAssignmentsWhenSnapshotsComplete.
//
// Idempotent by design: it writes only when the canonical identity differs from
// what is already on the bead, so steady-state reconciles perform no writes. A
// write failure is logged and skipped — recovery is best-effort and must never
// block reconciliation.
func canonicalizeLegacyBoundAssignedWork(cfg *config.City, workBeads []beads.Bead, workStores []beads.Store, sessionBeads *sessionBeadSnapshot, stderr io.Writer) {
	if cfg == nil || len(workBeads) != len(workStores) {
		return
	}
	// A nil or load-errored snapshot is incomplete: the live-session guard below
	// cannot prove a legacy owner is gone, so re-homing could strand a live
	// session's in-flight work. Skip and let a later complete tick converge it.
	if sessionBeads == nil || sessionBeads.LoadError() != nil {
		return
	}
	sessionByAssignee := buildSessionAssigneeIndex(sessionBeads)
	for i, wb := range workBeads {
		if wb.Status != "in_progress" && wb.Status != "open" {
			continue
		}
		store := workStores[i]
		if store == nil {
			continue
		}
		assignee := strings.TrimSpace(wb.Assignee)
		// Only template-assigned pool work qualifies: real per-session
		// assignments (session names) and named-session work are not equivalent
		// to a pool template's qualified name and are left untouched.
		if assignee == "" || !isKnownPoolTemplate(assignee, cfg) {
			continue
		}
		canonicalAssignee := normalizeAgentTemplateIdentity(cfg, assignee)
		routedTo := strings.TrimSpace(wb.Metadata[beadmeta.RoutedToMetadataKey])
		canonicalRouted := normalizeAgentTemplateIdentity(cfg, routedTo)
		assigneeChanged := canonicalAssignee != "" && canonicalAssignee != assignee
		routedChanged := routedTo != "" && canonicalRouted != routedTo
		if !assigneeChanged && !routedChanged {
			continue
		}
		// A live session still owns this assignment under the legacy identity —
		// the resume tier handles it; re-homing would strand its work_query.
		if _, served := sessionByAssignee[assignee]; served {
			continue
		}
		opts := beads.UpdateOpts{}
		if assigneeChanged {
			opts.Assignee = &canonicalAssignee
		}
		if routedChanged {
			opts.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: canonicalRouted}
		}
		if err := store.Update(wb.ID, opts); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "canonicalizeLegacyBoundAssignedWork: %s: %v\n", wb.ID, err) //nolint:errcheck
		}
	}
}

// canonicalizeLegacyBoundUnassignedRoutedWork rewrites the gc.routed_to of open,
// unassigned pool work that is still routed to a legacy template identity onto
// the agent's current canonical identity. It handles both legacy bound forms for
// a now-unbound agent ("dir/binding.name" -> "dir/name") and legacy unbound
// forms for a now-binding-qualified import ("dir/name" -> "dir/binding.name").
//
// This closes the demand/claim half of the migration that the assignee-keyed
// canonicalizeLegacyBoundAssignedWork cannot reach: open work with an empty
// assignee never enters the assigned-work collection, and the canonical
// pool-demand probe (EffectivePoolDemandQuery), the worker work_query
// (EffectiveWorkQuery Tier 3), and the claim predicate (hookClaimMatchesRoute)
// all match gc.routed_to against the canonical target by raw string. A bead still
// routed to the legacy spelling is therefore invisible to the canonical pool —
// it neither contributes scale demand nor can be claimed — so migration-era
// ready work stays stuck until its route is canonicalized. Rewriting the route in
// place lets every existing canonical-only path surface it, keeping the legacy
// awareness confined to this migration pass instead of spread across the demand,
// work_query, and claim predicates.
//
// Unlike the assignee re-home, this needs no session-snapshot guard: open
// unassigned work has no live owner to strand, so rewriting its route can only
// make it discoverable. Idempotent by design: it writes only when the canonical
// identity differs from the persisted route, so steady-state reconciles perform
// no writes, and a route that is already canonical or resolves to no configured
// agent is left untouched. A write failure is logged and skipped — recovery is
// best-effort and must never block reconciliation.
func canonicalizeLegacyBoundUnassignedRoutedWork(cfg *config.City, workBeads []beads.Bead, workStores []beads.Store, stderr io.Writer) {
	if cfg == nil || len(workBeads) != len(workStores) {
		return
	}
	for i, wb := range workBeads {
		if wb.Status != "open" || strings.TrimSpace(wb.Assignee) != "" {
			continue
		}
		store := workStores[i]
		if store == nil {
			continue
		}
		routedTo := strings.TrimSpace(wb.Metadata[beadmeta.RoutedToMetadataKey])
		if routedTo == "" {
			continue
		}
		// No cheap pre-filter here: the import direction's legacy form is a bare
		// "dir/name" with no binding-separator dot to key on, so every open routed
		// bead must reach normalizeAgentTemplateIdentity. The former dot filter kept
		// the per-bead agent scan off the steady-state backlog; if that cost bites,
		// narrow it by config shape (any agent with a BindingName) rather than by
		// route spelling.
		canonicalRouted := normalizeAgentTemplateIdentity(cfg, routedTo)
		if canonicalRouted == "" || canonicalRouted == routedTo {
			continue
		}
		opts := beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: canonicalRouted}}
		if err := store.Update(wb.ID, opts); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "canonicalizeLegacyBoundUnassignedRoutedWork: %s: %v\n", wb.ID, err) //nolint:errcheck
		}
	}
}

// collectOpenUnassignedRoutedWork gathers open, unassigned, pool-routed work from
// every leg the runtime plane serves routed work from, index-aligned with the store
// and store ref that own each bead. It is the input collection for
// canonicalizeLegacyBoundUnassignedRoutedWork: empty-assignee open work is dropped
// by the assignee-keyed collectAssignedWorkBeadsWithStores passes, so the
// migration re-home needs its own scan. The scan issues one live backing
// read per leg per tick so the raw-status filter runs, which costs a
// backing-store round trip the cached read did not — the accepted tradeoff for
// not counting blocked work as demand.
//
// # Two things changed here for the tick (ga-l7jdg)
//
// The leg set is Plan(RoutedWork) narrowed to the RUNTIME plane, not the census
// sweep's. Routed work lives only in the graph store by operator ruling, so the
// work-ledger leg could not hold this arm's answer and cost a remote round trip
// per tick to prove it. routedWorkStoreCandidates carries the reasoning and the
// single-store degradation.
//
// The legs are read CONCURRENTLY. This arm was the last sequential per-store
// fan-out on the demand path — collectAssignedWorkBeadsWithStores has read its
// legs in parallel for as long as it has had more than one — and the reads are
// independent by construction: one live list per store, folded afterwards. The
// fold stays serial and in PLAN ORDER, so the rows and their refs come out in
// exactly the order the sequential loop produced.
//
// # What the repair passes riding on this read give up, and why it is nothing
//
// Three consumers besides the demand count read this set — the legacy-route
// canonicalization, the control-dispatcher route repair, and the ready
// selection — so narrowing the read narrows them too. It costs those passes
// nothing they could have used: their whole purpose is to make a bead
// POOL-DEMANDABLE, and a ledger-resident bead is not demandable from the runtime
// plane whether or not its route is canonical. Canonicalizing one would produce
// a tidier bead that still never spawns a seat. The lever that changes that is
// `gc storage migrate` moving it to the binding, after which this arm sees it on
// the very next tick; the lost-route half is separately converged off-tick by the
// route-recovery backstop, which reads every leg (route_recovery_lane.go).
func collectOpenUnassignedRoutedWork(cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, suspendedRigPaths map[string]bool, stderr io.Writer) ([]beads.Bead, []beads.Store, []string, bool) {
	if cfg == nil {
		return nil, nil, nil, false
	}
	// Refs are the canonical scoped spelling the rows' gc.root_store_ref is
	// matched against; a binding keeps its own "class:*" ref, which reads back as
	// city scope.
	stores, legErr := routedWorkStoreCandidates(cityPath, cfg, store, rigStores, suspendedRigPaths, censusRefScoped)
	if legErr != nil {
		// The only demand signal this set feeds is openControlDispatcherDemand,
		// and a refused city reporting zero would drain a live dispatcher. Say so
		// and retain.
		fmt.Fprintf(stderr, "collectOpenUnassignedRoutedWork: no routed-work leg set for this city: %v\n", legErr) //nolint:errcheck
		return nil, nil, nil, true
	}

	type legResult struct {
		rows []beads.Bead
		err  error
	}
	results := make([]legResult, len(stores))
	var wg sync.WaitGroup
	for i, source := range stores {
		if source.store == nil {
			continue
		}
		wg.Add(1)
		go func(i int, source classStoreCandidate) {
			defer wg.Done()
			// Live so the backing store's raw --status=open filter excludes blocked/
			// deferred work: this unassigned-routed set feeds openControlDispatcherDemand
			// and the route-repair passes, and mapBdStatus would otherwise collapse a
			// blocked bead to "open" and let it count as spawn capacity or get its route
			// re-stamped (EB-42o8/gc-nz5i; extends gc-4zb/#4395). See listOpenForControllerDemandLive.
			rows, err := listOpenForControllerDemandLive(source.store)
			results[i] = legResult{rows: rows, err: err}
		}(i, source)
	}
	wg.Wait()

	var workBeads []beads.Bead
	var workStores []beads.Store
	var workStoreRefs []string
	var partial bool
	seen := make(map[storeScopedBeadKey]struct{})
	for i, source := range stores {
		if source.store == nil {
			continue
		}
		storeRef := source.ref
		open, err := results[i].rows, results[i].err
		if err != nil {
			// A failed live demand read must NOT read as zero demand: the only
			// demand signal this set feeds is openControlDispatcherDemand (its
			// other consumers — canonicalizeLegacyBoundUnassignedRoutedWork,
			// repairControlDispatcherRoutesForStoreScope and
			// selectReadyUnassignedRoutedWork — degrade to no-ops on an
			// outage), so silently dropping a store's rows drains a live
			// control dispatcher
			// (gc-ft31x, the fail-open-to-zero sibling of the raw-status demand
			// fix). Report it partial so the caller retains affected dispatchers
			// this tick. A partial result still carries the rows it managed to
			// read, so fall through and use them; a hard failure carries none,
			// so skip only this store's population.
			partial = true
			if !beads.IsPartialResult(err) {
				fmt.Fprintf(stderr, "collectOpenUnassignedRoutedWork: %s: List(open): %v\n", storeRef, err) //nolint:errcheck
				continue
			}
		}
		for _, b := range open {
			if b.Type == sessionBeadType || strings.TrimSpace(b.Assignee) != "" {
				continue
			}
			if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) == "" {
				continue
			}
			if !rootStoreRefMatchesCandidate(b.Metadata[beadmeta.RootStoreRefMetadataKey], storeRef) {
				continue
			}
			key := storeScopedBeadKey{StoreRef: storeRef, ID: b.ID}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			workBeads = append(workBeads, b)
			workStores = append(workStores, source.store)
			workStoreRefs = append(workStoreRefs, storeRef)
		}
	}
	return workBeads, workStores, workStoreRefs, partial
}

// markControlDispatcherTemplatesPartial marks every deterministic control-
// dispatcher template partial. The only demand signal
// collectOpenUnassignedRoutedWork feeds is openControlDispatcherDemand — its
// other consumers degrade to no-ops on an outage — so when its live read fails
// the lost signal is exactly the control-dispatcher demand: without this the
// tick reads zero demand
// and drains a live dispatcher on a transient List outage. Marking the templates
// partial routes them through retainScaleCheckPartialPoolDesired, which preserves
// the running dispatcher session for this tick (gc-ft31x).
func markControlDispatcherTemplatesPartial(cfg *config.City, partials map[string]bool) map[string]bool {
	if cfg == nil {
		return partials
	}
	for i := range cfg.Agents {
		if !config.IsDeterministicControlDispatcher(&cfg.Agents[i]) {
			continue
		}
		partials = markScaleCheckPartialTemplate(partials, cfg.Agents[i].QualifiedName())
	}
	return partials
}

// selectReadyUnassignedRoutedWork intersects the broad open-routed snapshot
// with the concrete beads selected by the default ready-demand probes. The
// broad snapshot is intentionally retained for route repair, while this narrow
// result is safe for the idle-claim nudger: blocked or otherwise non-ready open
// work never enters scaleCheckDemandByTemplate.
func selectReadyUnassignedRoutedWork(
	candidates []beads.Bead,
	candidateStoreRefs []string,
	demandByTemplate map[string]scaleCheckDemand,
) ([]beads.Bead, []string) {
	wanted := make(map[storeScopedBeadKey]struct{})
	for _, demand := range demandByTemplate {
		for _, id := range demand.WorkBeadIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			storeRef := normalizeDemandStoreRef(demand.StoreRefs[id])
			wanted[storeScopedBeadKey{StoreRef: storeRef, ID: id}] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	work := make([]beads.Bead, 0, len(wanted))
	storeRefs := make([]string, 0, len(wanted))
	for i, candidate := range candidates {
		storeRef := ""
		if i < len(candidateStoreRefs) {
			storeRef = candidateStoreRefs[i]
		}
		key := storeScopedBeadKey{StoreRef: normalizeDemandStoreRef(storeRef), ID: candidate.ID}
		if _, ok := wanted[key]; !ok {
			continue
		}
		work = append(work, candidate)
		storeRefs = append(storeRefs, storeRef)
		delete(wanted, key)
	}
	return work, storeRefs
}

// normalizeDemandStoreRef makes the default-probe shorthand "city" compare
// equal to canonical city:<name> refs while preserving rig ownership.
func normalizeDemandStoreRef(storeRef string) string {
	storeRef = strings.TrimSpace(storeRef)
	switch {
	case storeRef == "city", strings.HasPrefix(storeRef, "city:"):
		return "city"
	// A class binding is a CITY-scope store, so it normalizes onto the city
	// exactly as it did when it WAS the census's city leg. Leaving it as its own
	// scope would split a graph-class row's demand key from the ready-routed key
	// the same row is matched against.
	case storeref.IsClassRef(storeRef):
		return "city"
	case strings.HasPrefix(storeRef, "rig:"):
		return "rig:" + strings.TrimSpace(strings.TrimPrefix(storeRef, "rig:"))
	default:
		return storeRef
	}
}

// rootStoreRefMatchesCandidate filters duplicate views of one physical graph
// in legacy unscoped file-store mode. There, the city and every rig store can
// all list the same row even though gc.root_store_ref still records its logical
// owner. Canonical refs are authoritative: city-owned rows are collected only
// through the city candidate and rig-owned rows only through their named rig.
// Legacy rows without a canonical ref remain visible through every independent
// store so same-ID beads in genuinely separate stores are not collapsed.
func rootStoreRefMatchesCandidate(rootStoreRef, candidateStoreRef string) bool {
	rootRig, rootScoped := storeref.ScopeRigContext(rootStoreRef)
	if !rootScoped {
		return true
	}
	candidateRig, candidateScoped := storeref.ScopeRigContext(candidateStoreRef)
	return candidateScoped && candidateRig == rootRig
}

// selectReadyContinuationClaimCandidates projects the already-collected
// assigned-work snapshot into the only rows the continuation nudge backstop may
// consider. It adds no broad query: one bounded root Get is issued per
// ready/open affinity candidate so the row's gc.root_bead_id is proven to name
// a live graph-v2 root in the exact physical store described by
// gc.root_store_ref.
//
// The slices must remain aligned and candidate rows must have an exact
// ReadyAssigned entry. Any alignment failure, root read failure, or duplicate
// disagreement is returned as a partial snapshot so callers preserve pacing
// markers. Definite ineligibility simply omits that row. Identity and
// exactly-one-per-session checks are intentionally deferred until after
// reconciliation, when the current raw session snapshot is available.
func selectReadyContinuationClaimCandidates(
	cityName string,
	work []beads.Bead,
	workStores []beads.Store,
	workStoreRefs []string,
	readyAssigned map[storeScopedBeadKey]bool,
) ([]ContinuationClaimCandidate, bool) {
	if len(work) != len(workStores) || len(work) != len(workStoreRefs) {
		return nil, true
	}
	if len(work) == 0 {
		return nil, false
	}

	// Group before eligibility filtering. Otherwise a valid copy can survive
	// beside a same-scope copy whose metadata or root read disagrees.
	groups := make(map[storeScopedBeadKey][]int)
	order := make([]storeScopedBeadKey, 0, len(work))
	partial := false
	for i, bead := range work {
		id := strings.TrimSpace(bead.ID)
		if id == "" {
			continue
		}
		storeRef, ok := canonicalContinuationClaimStoreRef(cityName, workStoreRefs[i])
		if !ok {
			if continuationRowCouldBeCandidate(bead, workStoreRefs[i], readyAssigned) {
				partial = true
			}
			continue
		}
		key := storeScopedBeadKey{StoreRef: storeRef, ID: id}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}

	result := make([]ContinuationClaimCandidate, 0, len(order))
	for _, key := range order {
		var (
			valid  []ContinuationClaimCandidate
			absent bool
			hold   bool
		)
		for _, i := range groups[key] {
			candidate, resolution := evaluateReadyContinuationClaimCandidate(
				work[i],
				workStores[i],
				workStoreRefs[i],
				key.StoreRef,
				readyAssigned,
			)
			switch resolution {
			case continuationCandidateAbsent:
				absent = true
			case continuationCandidateHold:
				hold = true
			case continuationCandidateValid:
				valid = append(valid, candidate)
			}
		}
		if hold {
			partial = true
			continue
		}
		if len(valid) == 0 {
			continue
		}
		if absent {
			partial = true
			continue
		}
		first := valid[0]
		identical := true
		for _, candidate := range valid[1:] {
			if !sameContinuationClaimCandidate(first, candidate) {
				identical = false
				break
			}
		}
		if !identical {
			partial = true
			continue
		}
		result = append(result, first)
	}
	return result, partial
}

type continuationCandidateResolution int

const (
	continuationCandidateAbsent continuationCandidateResolution = iota
	continuationCandidateHold
	continuationCandidateValid
)

func continuationRowCouldBeCandidate(
	bead beads.Bead,
	storeRef string,
	readyAssigned map[storeScopedBeadKey]bool,
) bool {
	id := strings.TrimSpace(bead.ID)
	return id != "" &&
		id == bead.ID &&
		strings.EqualFold(strings.TrimSpace(bead.Status), "open") &&
		strings.EqualFold(strings.TrimSpace(bead.Type), "task") &&
		strings.TrimSpace(bead.Assignee) != "" &&
		readyAssigned[storeScopedBeadKey{StoreRef: storeRef, ID: id}] &&
		strings.TrimSpace(bead.Metadata[beadmeta.ContinuationGroupMetadataKey]) != "" &&
		strings.TrimSpace(bead.Metadata[beadmeta.SessionAffinityMetadataKey]) == "require"
}

func evaluateReadyContinuationClaimCandidate(
	bead beads.Bead,
	store beads.Store,
	rawStoreRef string,
	canonicalStoreRef string,
	readyAssigned map[storeScopedBeadKey]bool,
) (ContinuationClaimCandidate, continuationCandidateResolution) {
	if !continuationRowCouldBeCandidate(bead, rawStoreRef, readyAssigned) {
		return ContinuationClaimCandidate{}, continuationCandidateAbsent
	}

	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	ownerStoreRef, owned := continuationClaimOwnerStoreRef(
		rawStoreRef,
		bead.Metadata[beadmeta.RootStoreRefMetadataKey],
		canonicalStoreRef,
	)
	if rootID == "" || !owned {
		return ContinuationClaimCandidate{}, continuationCandidateAbsent
	}
	if store == nil {
		return ContinuationClaimCandidate{}, continuationCandidateHold
	}
	root, err := store.Get(rootID)
	if err != nil {
		return ContinuationClaimCandidate{}, continuationCandidateHold
	}
	// The root proves the molecule is a live graph-v2 run in this exact store.
	// It deliberately does NOT gate on the root's gc.session_name: that key is a
	// dashboard back-fill stamped by stampRunRootFromStep from whichever step
	// most recently went in_progress (#2843), so on a multi-agent molecule it
	// names one participating template and contradicts every other one. Gating
	// here on it silently excluded every cross-template successor of formulas
	// like mol-adopt-pr-v2 (apply-fixes on a worker slot, quality-scorecard on a
	// reviewer slot) from ever being nudged. The binding that actually authorizes
	// delivery is the step's own assignee resolving to exactly one live session
	// (newPoolContinuationCandidateSnapshot).
	if root.ID != rootID ||
		!strings.EqualFold(strings.TrimSpace(root.Status), "in_progress") ||
		!strings.EqualFold(strings.TrimSpace(root.Type), "task") ||
		strings.TrimSpace(root.Metadata[beadmeta.RootStoreRefMetadataKey]) != ownerStoreRef ||
		strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]) != "graph.v2" ||
		strings.TrimSpace(root.Metadata[beadmeta.KindMetadataKey]) != "workflow" {
		return ContinuationClaimCandidate{}, continuationCandidateAbsent
	}
	return ContinuationClaimCandidate{
		WorkBeadID: strings.TrimSpace(bead.ID),
		RootBeadID: rootID,
		StoreRef:   ownerStoreRef,
		Assignee:   strings.TrimSpace(bead.Assignee),
		Store:      store,
	}, continuationCandidateValid
}

// continuationClaimOwnerStoreRef answers which canonical store ref OWNS one
// candidate row, which is not always the scope its leg serves. A class binding
// is a city-scope store but a many-scope container — the same distinction PR
// #5918 (ga-oytw9) drew for control-route repair. A rig-scoped workflow's steps
// are minted into the binding carrying gc.root_store_ref=rig:<name>, so on a
// split city the row's own root ref is the owner and the leg-derived city ref
// is only the grouping key. It is accepted only when canonical and local to
// this city: exactly the city ref, or a well-formed rig ref. A blank, class, or
// legacy value fails closed. Every other leg is single-scope, so there the
// leg-derived canonical ref stays the comparator.
func continuationClaimOwnerStoreRef(rawStoreRef, rootStoreRef, canonicalStoreRef string) (string, bool) {
	rootStoreRef = strings.TrimSpace(rootStoreRef)
	if rootStoreRef == "" {
		return "", false
	}
	if !storeref.IsClassRef(rawStoreRef) {
		if rootStoreRef != canonicalStoreRef {
			return "", false
		}
		return canonicalStoreRef, true
	}
	if rootStoreRef == canonicalStoreRef {
		return canonicalStoreRef, true
	}
	rigName, scoped := storeref.ScopeRigContext(rootStoreRef)
	if !scoped || rigName == "" || rootStoreRef != "rig:"+rigName {
		return "", false
	}
	return rootStoreRef, true
}

func sameContinuationClaimCandidate(a, b ContinuationClaimCandidate) bool {
	return a.WorkBeadID == b.WorkBeadID &&
		a.RootBeadID == b.RootBeadID &&
		a.StoreRef == b.StoreRef &&
		a.Assignee == b.Assignee
}

// canonicalContinuationClaimStoreRef turns the aligned assigned-work shorthand
// (empty city ref or bare rig name) into the exact canonical ref graph-v2 roots
// persist. Already-canonical refs are accepted only when they name this city or
// a non-empty rig; arbitrary/legacy values fail closed.
func canonicalContinuationClaimStoreRef(cityName, storeRef string) (string, bool) {
	cityName = strings.TrimSpace(cityName)
	storeRef = strings.TrimSpace(storeRef)
	switch {
	case storeRef == "":
		if cityName == "" {
			return "", false
		}
		return "city:" + cityName, true
	// A binding leg canonicalizes to the city, the scope it serves. Before the
	// census named it separately, a binding-resident continuation row arrived
	// under the city ref and was a candidate; answering "not canonical" here
	// would silently retire the continuation backstop for every graph-class step
	// on a split city. This is the GROUPING key and the city-rooted comparator
	// only: the binding holds rows from many scopes, and each row's own owner is
	// taken from its gc.root_store_ref by continuationClaimOwnerStoreRef.
	case storeref.IsClassRef(storeRef):
		if cityName == "" {
			return "", false
		}
		return "city:" + cityName, true
	case strings.HasPrefix(storeRef, "city:"):
		if cityName == "" || storeRef != "city:"+cityName {
			return "", false
		}
		return storeRef, true
	case strings.HasPrefix(storeRef, "rig:"):
		rigName := strings.TrimSpace(strings.TrimPrefix(storeRef, "rig:"))
		if rigName == "" || storeRef != "rig:"+rigName {
			return "", false
		}
		return storeRef, true
	case strings.Contains(storeRef, ":"):
		return "", false
	default:
		return "rig:" + storeRef, true
	}
}

// Keep migration writes within the same budget used for other reconciler
// recovery writes: each bd/Dolt mutation can take seconds and is followed by a
// cache refresh, so a larger burst can starve session starts in the same tick.
const controlDispatcherRouteRepairLimitPerTick = 5

var controlDispatcherRouteRepairCursors sync.Map // map[string]*atomic.Uint64

func controlDispatcherRouteRepairDomainKey(repairDomain string) string {
	repairDomain = strings.TrimSpace(repairDomain)
	if repairDomain == "" {
		return "<default>"
	}
	return filepath.Clean(repairDomain)
}

func controlDispatcherRouteRepairCursorForDomain(repairDomain string) *atomic.Uint64 {
	key := controlDispatcherRouteRepairDomainKey(repairDomain)
	created := &atomic.Uint64{}
	actual, _ := controlDispatcherRouteRepairCursors.LoadOrStore(key, created)
	return actual.(*atomic.Uint64)
}

// repairControlDispatcherRoutesForStoreScope durably repairs open control work
// whose persisted route names a dispatcher in a different store scope. Older
// builds could stamp a city route on a rig-resident graph; rewriting the route
// before demand evaluation lets the matching rig dispatcher start and claim it
// without teaching claimers to reinterpret dishonest route metadata. Writes are
// bounded per city pass so a large upgrade backlog cannot monopolize a
// reconciler tick. Each city owns its rotating start offset, preventing either
// a persistently bad row or another CityRuntime in the same supervisor from
// starving later scopes. Deferred or failed cross-scope route repairs are
// suppressed from this tick's demand snapshot and retried on later ticks;
// marker-only cleanup leaves an already-canonical route eligible for demand.
func repairControlDispatcherRoutesForStoreScope(
	repairDomain string,
	cfg *config.City,
	workBeads []beads.Bead,
	workStores []beads.Store,
	workStoreRefs []string,
	stderr io.Writer,
) {
	if cfg == nil || len(workBeads) == 0 {
		return
	}
	if len(workBeads) != len(workStores) || len(workBeads) != len(workStoreRefs) {
		if stderr != nil {
			fmt.Fprintf(stderr, "repairControlDispatcherRoutesForStoreScope: index-aligned input mismatch beads=%d stores=%d refs=%d\n", len(workBeads), len(workStores), len(workStoreRefs)) //nolint:errcheck
		}
		suppressControlDispatcherRoutes(workBeads)
		return
	}
	repair := newControlDispatcherRouteRepair(cfg, stderr)
	cursor := controlDispatcherRouteRepairCursorForDomain(repairDomain)
	start := int((cursor.Add(controlDispatcherRouteRepairLimitPerTick) - controlDispatcherRouteRepairLimitPerTick) % uint64(len(workBeads)))
	for offset := range workBeads {
		i := (start + offset) % len(workBeads)
		repair.repairBead(&workBeads[i], workStores[i], workStoreRefs[i])
	}
}

// controlDispatcherRouteLookup memoizes whether a store scope has a configured
// control-dispatcher and, if so, its qualified route.
type controlDispatcherRouteLookup struct {
	route string
	ok    bool
}

// controlDispatcherRouteRepair carries the per-pass state for a bounded
// cross-scope control-route repair sweep: per-scope route lookups are cached, a
// store scope with no configured dispatcher is reported once, and the remaining
// durable-write budget is tracked so a large upgrade backlog cannot monopolize a
// reconciler tick.
type controlDispatcherRouteRepair struct {
	cfg                  *config.City
	routeByScope         map[string]controlDispatcherRouteLookup
	reportedMissingScope map[string]bool
	writesRemaining      int
	stderr               io.Writer
}

func newControlDispatcherRouteRepair(cfg *config.City, stderr io.Writer) *controlDispatcherRouteRepair {
	return &controlDispatcherRouteRepair{
		cfg:                  cfg,
		routeByScope:         make(map[string]controlDispatcherRouteLookup),
		reportedMissingScope: make(map[string]bool),
		writesRemaining:      controlDispatcherRouteRepairLimitPerTick,
		stderr:               stderr,
	}
}

// repairBead realigns one control bead's persisted route with the dispatcher
// that owns its store scope, clearing any stale fallback marker in the same
// bounded write. Non-control, unscoped, and already-canonical beads are left
// untouched. Store ownership, not the current route, selects the dispatcher: an
// unscoped row cannot be repaired safely because #3765 itself stamped a valid
// city route onto rig-owned controls, so gc.routed_to is not ownership evidence.
// Graph v2 stamped gc.root_store_ref before that regression, so malformed/older
// rows are left untouched instead of guessing a store.
func (r *controlDispatcherRouteRepair) repairBead(bead *beads.Bead, store beads.Store, storeRef string) {
	if !beadmeta.IsControlKind(strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey])) {
		return
	}
	if _, scoped := storeref.ScopeRigContext(bead.Metadata[beadmeta.RootStoreRefMetadataKey]); !scoped {
		return
	}
	route, ok := r.desiredRoute(bead, storeRef)
	if !ok {
		return
	}
	current := strings.TrimSpace(bead.Metadata[beadmeta.RoutedToMetadataKey])
	needsRouteRepair := current != route
	clearFallback := strings.TrimSpace(bead.Metadata[beadmeta.ControlDispatcherFallbackMetadataKey]) != ""
	if !needsRouteRepair && !clearFallback {
		return
	}
	r.persist(bead, store, current, route, needsRouteRepair, clearFallback)
}

// desiredRoute returns the configured control-dispatcher route for the bead's
// store scope, caching lookups per rig context. When no dispatcher is
// configured it reports the gap once per scope and suppresses the bead's
// cross-scope route from this tick's demand snapshot so it cannot create phantom
// demand for a dispatcher that cannot read the store; the durable route is left
// in place for operator diagnosis and a later config repair.
func (r *controlDispatcherRouteRepair) desiredRoute(bead *beads.Bead, storeRef string) (string, bool) {
	rigContext := controlDispatcherRigContextForStoreRef(storeRef)
	lookup, cached := r.routeByScope[rigContext]
	if !cached {
		lookup.route, lookup.ok = configuredControlDispatcherRouteForScope(r.cfg, rigContext)
		r.routeByScope[rigContext] = lookup
	}
	if lookup.ok {
		return lookup.route, true
	}
	if !r.reportedMissingScope[rigContext] {
		if r.stderr != nil {
			fmt.Fprintf(r.stderr, "repairControlDispatcherRoutesForStoreScope: control bead %s in %s has no configured control-dispatcher for its store scope\n", bead.ID, controlDispatcherStoreRefLabel(storeRef)) //nolint:errcheck
		}
		r.reportedMissingScope[rigContext] = true
	}
	delete(bead.Metadata, beadmeta.RoutedToMetadataKey)
	return "", false
}

// persist durably rewrites one control bead's route and/or clears its fallback
// marker within the per-pass write budget. The budget is consumed the moment a
// repair is attempted, so a missing store or a failed write still spends a slot
// and the rotating cursor gives later beads a turn on the next tick. When the
// repair cannot be persisted this tick the pending route change is suppressed
// from the demand snapshot and retried later while the durable route is
// preserved.
func (r *controlDispatcherRouteRepair) persist(bead *beads.Bead, store beads.Store, current, route string, needsRouteRepair, clearFallback bool) {
	if r.writesRemaining <= 0 {
		deferRouteRepair(bead, needsRouteRepair)
		return
	}
	r.writesRemaining--
	if store == nil {
		deferRouteRepair(bead, needsRouteRepair)
		return
	}
	metadata := make(map[string]string, 2)
	if needsRouteRepair {
		metadata[beadmeta.RoutedToMetadataKey] = route
	}
	if clearFallback {
		// #3463 stamped this marker together with the cross-store fallback. Clear
		// its semantic value in the same bounded migration write, even when
		// another recovery path already repaired the route itself.
		metadata[beadmeta.ControlDispatcherFallbackMetadataKey] = ""
	}
	if err := store.Update(bead.ID, beads.UpdateOpts{Metadata: metadata}); err != nil {
		if r.stderr != nil {
			fmt.Fprintf(r.stderr, "repairControlDispatcherRoutesForStoreScope: control bead %s route %q -> %q: %v\n", bead.ID, current, route, err) //nolint:errcheck
		}
		deferRouteRepair(bead, needsRouteRepair)
		return
	}
	applyRouteRepairInMemory(bead, route, needsRouteRepair, clearFallback)
}

// applyRouteRepairInMemory mirrors a persisted route repair onto the in-memory
// bead snapshot so this tick's demand calculation sees the canonical route. The
// fallback marker is deleted from the snapshot rather than blanked, matching the
// durable write's cleared value.
func applyRouteRepairInMemory(bead *beads.Bead, route string, needsRouteRepair, clearFallback bool) {
	if bead.Metadata == nil {
		bead.Metadata = make(map[string]string)
	}
	if needsRouteRepair {
		bead.Metadata[beadmeta.RoutedToMetadataKey] = route
	}
	if clearFallback {
		delete(bead.Metadata, beadmeta.ControlDispatcherFallbackMetadataKey)
	}
}

// deferRouteRepair suppresses an un-persisted route change from this tick's
// demand snapshot, leaving the durable route untouched for a later retry. A
// fallback-only cleanup carries no pending route change and stays eligible for
// demand.
func deferRouteRepair(bead *beads.Bead, needsRouteRepair bool) {
	if needsRouteRepair {
		delete(bead.Metadata, beadmeta.RoutedToMetadataKey)
	}
}

func suppressControlDispatcherRoutes(workBeads []beads.Bead) {
	for i := range workBeads {
		if beadmeta.IsControlKind(strings.TrimSpace(workBeads[i].Metadata[beadmeta.KindMetadataKey])) {
			delete(workBeads[i].Metadata, beadmeta.RoutedToMetadataKey)
		}
	}
}

func configuredControlDispatcherRouteForScope(cfg *config.City, rigContext string) (string, bool) {
	if dispatcher, ok := config.ControlDispatcherForScope(cfg, rigContext); ok {
		return dispatcher.QualifiedName(), true
	}
	return "", false
}

func controlDispatcherRigContextForStoreRef(storeRef string) string {
	storeRef = strings.TrimSpace(storeRef)
	if storeRef == "" || storeRef == "city" || strings.HasPrefix(storeRef, "city:") {
		return ""
	}
	return strings.TrimPrefix(storeRef, "rig:")
}

func controlDispatcherStoreRefLabel(storeRef string) string {
	storeRef = strings.TrimSpace(storeRef)
	if storeRef == "" || storeRef == "city" || strings.HasPrefix(storeRef, "city:") {
		return "the city store"
	}
	return fmt.Sprintf("rig store %q", controlDispatcherRigContextForStoreRef(storeRef))
}

func selectOrCreateDependencyPoolSessionBead(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
) (session.Info, error) {
	info, _, err := selectOrCreateDependencyPoolSessionBeadWithSlot(bp, cfgAgent, template)
	return info, err
}

// selectOrCreateDependencyPoolSessionBeadWithSlot returns the concrete slot
// used to resolve the dependency template. Legacy dependency rows without a
// persisted pool_slot are assigned the same holder-aware lowest free identity
// as a fresh row instead of silently defaulting to slot 1.
func selectOrCreateDependencyPoolSessionBeadWithSlot(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
) (session.Info, int, error) {
	if cfgAgent == nil {
		cfgAgent = findAgentByTemplate(&config.City{Agents: bp.agents}, template)
	}
	if cfgAgent == nil {
		return session.Info{}, 0, fmt.Errorf("dependency pool template %q has no configured agent", template)
	}
	decisionTime := time.Now()
	if canonical, ok := findReusableCanonicalNonExpandingDependencyPoolSessionInfoAt(bp, cfgAgent, template, decisionTime); ok {
		info, err := normalizeNonExpandingPoolSessionInfoForSelection(bp, cfgAgent, canonical)
		return info, 0, err
	}
	reusable := reusableDependencyPoolSessionInfosAt(bp, template, decisionTime)
	if len(reusable) > 0 {
		info := reusable[0]
		slot := existingPoolSlotWithConfigInfo(bp.city, cfgAgent, info)
		if slot == 0 && !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
			var err error
			slot, err = claimFreshPoolSlotInfo(bp, cfgAgent, make(map[int]bool))
			if err != nil {
				return session.Info{}, 0, err
			}
		}
		normalized, err := normalizeNonExpandingPoolSessionInfoForSelection(bp, cfgAgent, info)
		return normalized, slot, err
	}
	if !bp.hasCompleteSessionSnapshot() {
		return session.Info{}, 0, errPoolSessionCreatePartial
	}
	usedSlots := make(map[int]bool)
	slot, err := claimFreshPoolSlotInfo(bp, cfgAgent, usedSlots)
	if err != nil {
		return session.Info{}, 0, err
	}
	_, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(cfgAgent, slot)
	// Dependency floors are bounded prerequisites for already-realized roots,
	// so they bypass the ordinary fresh pool create budget. The wake budget
	// still caps when those floor sessions can actually start.
	info, err := createPoolSessionBeadWithGuardedAlias(bp, cfgAgent, template, qualifiedInstance, poolSlot, nil)
	return info, slot, err
}

func reuseTemplateConfig(bp *agentBuildParams) *config.City {
	if bp == nil {
		return nil
	}
	if bp.city != nil {
		return bp.city
	}
	return &config.City{Agents: bp.agents}
}

func poolSessionCreateStartedAt(_ *agentBuildParams) time.Time {
	return time.Now().UTC()
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

// prepareTemplateResolution installs any hook-backed files that must exist
// before resolveTemplate fingerprints CopyFiles. This keeps generated hook
// files from looking like config drift on the next reconcile tick.
func prepareTemplateResolution(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, stderr io.Writer) {
	if bp == nil || cfgAgent == nil {
		return
	}
	resolved, err := config.ResolveProvider(cfgAgent, bp.workspace, bp.providers, bp.lookPath)
	if err != nil {
		return
	}
	workDir, err := resolveConfiguredWorkDir(bp.cityPath, bp.cityName, qualifiedName, cfgAgent, bp.rigs)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "agent %q: workdir: %v\n", qualifiedName, err) //nolint:errcheck
		}
		return
	}
	rigName := sessionSetupContextForAgent(bp.cityPath, bp.cityName, qualifiedName, cfgAgent, bp.rigs).Rig
	materializeProviderOverlaysBeforeFingerprint(bp, cfgAgent, resolved, qualifiedName, rigName, workDir, stderr)
	if ih := config.ResolveInstallHooks(cfgAgent, bp.workspace); len(ih) > 0 {
		resolver := func(name string) string { return config.BuiltinFamily(name, bp.providers) }
		if hErr := hooks.InstallWithResolver(bp.fs, bp.cityPath, workDir, ih, resolver); hErr != nil {
			fmt.Fprintf(stderr, "agent %q: hooks: %v\n", qualifiedName, hErr) //nolint:errcheck
		}
	}
}

func materializeProviderOverlaysBeforeFingerprint(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	resolved *config.ResolvedProvider,
	qualifiedName string,
	rigName string,
	workDir string,
	stderr io.Writer,
) {
	if bp == nil || cfgAgent == nil || resolved == nil || workDir == "" {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	installHooks := config.ResolveInstallHooks(cfgAgent, bp.workspace)
	packDirs := effectiveOverlayDirs(bp.packOverlayDirs, bp.rigOverlayDirs, rigName)
	overlayDir := resolveOverlayDir(cfgAgent.OverlayDir, bp.cityPath)
	overlayProviders := runtime.EffectiveOverlayProviderNames(runtime.Config{
		ProviderName:        resolvedProviderLaunchFamily(resolved),
		ProviderOverlayName: strings.TrimSpace(resolved.Name),
		InstallAgentHooks:   installHooks,
		PackOverlayDirs:     packDirs,
		OverlayDir:          overlayDir,
	})
	// Skip reconciler-owned mergeable hook/settings files here: hooks.Install
	// runs immediately after this staging on the SAME workDir (see
	// prepareTemplateResolution), so it must be the sole writer of those files
	// ON THE RECONCILE TICK. Staging them too leaves two writers with
	// disagreeing hook-entry matchers and a permanent codex-hooks-drift hybrid.
	// The runtime task-worktree staging path keeps staging them — it is their
	// sole writer.
	//
	// "Sole writer" is scoped to the tick on purpose. For a persistent
	// (non-task) agent the home dir is also the session workDir, and
	// session-start staging writes these same files through the NON-skipping
	// path — internal/runtime/tmux.stageStartFiles and
	// runtime.StageSessionWorkDir (subprocess/acp) both call
	// StageProviderOverlayDir with a nil skip. So a hybrid document can
	// reappear at session start; the next tick converges it. That turns the
	// permanent drift this fix targets into a transient one, which is the
	// actual invariant — not that nothing else ever writes these paths.
	for _, od := range packDirs {
		if err := runtime.StageProviderOverlayDirSkippingMergeable(od, workDir, overlayProviders, stderr); err != nil {
			fmt.Fprintf(stderr, "agent %q: pack overlay %q: %v\n", qualifiedName, od, err) //nolint:errcheck
		}
	}
	if overlayDir != "" {
		if err := runtime.StageProviderOverlayDirSkippingMergeable(overlayDir, workDir, overlayProviders, stderr); err != nil {
			fmt.Fprintf(stderr, "agent %q: overlay %q: %v\n", qualifiedName, overlayDir, err) //nolint:errcheck
		}
	}
}

func resolveTemplatePrepared(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, fpExtra map[string]string) (TemplateParams, error) {
	if err := validateAgentSessionTransportForBuild(bp, cfgAgent, qualifiedName); err != nil {
		return TemplateParams{}, err
	}
	prepareTemplateResolution(bp, cfgAgent, qualifiedName, bp.stderr)
	return resolveTemplate(bp, cfgAgent, qualifiedName, fpExtra)
}

func validateAgentSessionTransportForBuild(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string) error {
	if bp == nil || cfgAgent == nil {
		return nil
	}
	if bp.lookPath == nil {
		// Legacy unit tests construct minimal build params without provider
		// lookup plumbing. Production controller paths always install lookPath;
		// coverage below exercises that production-shaped validation path.
		return nil
	}
	workspace := bp.workspace
	if workspace == nil {
		workspace = &config.Workspace{}
	}
	resolved, err := config.ResolveProvider(cfgAgent, workspace, bp.providers, bp.lookPath)
	if err != nil {
		return fmt.Errorf("agent %q: %w", qualifiedName, err)
	}
	transport := config.ResolveSessionCreateTransport(cfgAgent.Session, resolved)
	if err := validateResolvedSessionTransport(resolved, transport, bp.sp); err != nil {
		return fmt.Errorf("agent %q: %w", qualifiedName, err)
	}
	return nil
}

// installAgentSideEffects performs idempotent side effects for a resolved
// agent: hook installation and ACP route registration. Called from
// buildDesiredState on every tick; safe to repeat.
//
// When the resolved provider is Claude, resolveTemplate has already projected
// managed Claude settings via ensureClaudeSettingsArgs (required so the
// --settings path exists before runtime fingerprinting). In that case the
// "claude" entry in install_agent_hooks is filtered out here to avoid
// duplicating filesystem I/O for every pool instance on every tick. Agents
// whose resolved provider is not Claude but which opt in explicitly via
// install_agent_hooks = ["claude"] still flow through hooks.Install here.
func installAgentSideEffects(bp *agentBuildParams, cfgAgent *config.Agent, tp TemplateParams, stderr io.Writer) {
	// Install provider hooks (idempotent filesystem side effect). Route
	// through the family resolver so wrapped custom aliases (e.g.
	// [providers.my-fast-claude] base = "builtin:claude") install their
	// ancestor's hook format rather than erroring with
	// "unsupported hook provider". Keep the "claude" dedup from main: if
	// the resolved provider family IS claude, ensureClaudeSettingsArgs
	// already projected the settings upstream in resolveTemplate, so
	// drop the explicit "claude" entry here to avoid duplicating the
	// filesystem write on every reconciler tick.
	ih := config.ResolveInstallHooks(cfgAgent, bp.workspace)
	if tp.ResolvedProvider != nil {
		family := resolvedProviderLaunchFamily(tp.ResolvedProvider)
		if family == "claude" || tp.ResolvedProvider.Name == "claude" {
			ih = hooksWithoutClaude(ih)
		}
	}
	if len(ih) > 0 {
		resolver := func(name string) string { return config.BuiltinFamily(name, bp.providers) }
		if hErr := hooks.InstallWithResolver(bp.fs, bp.cityPath, tp.WorkDir, ih, resolver); hErr != nil {
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

// hooksWithoutClaude returns ih with any "claude" entries filtered out.
// Used by installAgentSideEffects when the resolved provider is Claude —
// in that case resolveTemplate → ensureClaudeSettingsArgs already projected
// the settings, and running hooks.Install("claude") again would duplicate
// filesystem I/O on every reconciler tick.
func hooksWithoutClaude(ih []string) []string {
	if len(ih) == 0 {
		return ih
	}
	out := make([]string, 0, len(ih))
	for _, p := range ih {
		if p == "claude" {
			continue
		}
		out = append(out, p)
	}
	return out
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

// poolInstanceIdentity returns the (instanceName, qualifiedInstance) pair for
// a pool slot on the given agent. For agents that do NOT support instance
// expansion (max_active_sessions=1, no namepool), it returns the base
// identity and emits a defensive warning when a non-zero slot would have
// produced a phantom "{base}-N" name. The warning is the diagnostic
// breadcrumb the bug report (ga-fiw) asked for — it lets operators see when
// a non-expansion agent was about to be materialized with a numeric suffix.
func poolInstanceIdentity(cfgAgent *config.Agent, slot int, stderr io.Writer) (string, string) {
	if cfgAgent == nil {
		return "", ""
	}
	if !cfgAgent.SupportsInstanceExpansion() {
		if slot > 0 && stderr != nil {
			fmt.Fprintf(stderr, "buildDesiredState: pool %q: agent does not support instance expansion (max_active_sessions=%s) but slot %d was claimed; using base identity to avoid phantom %q-%d session\n", //nolint:errcheck
				cfgAgent.QualifiedName(), formatMaxSessions(cfgAgent), slot, cfgAgent.Name, slot)
		}
		return cfgAgent.Name, cfgAgent.QualifiedName()
	}
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		if slot > 0 && stderr != nil {
			fmt.Fprintf(stderr, "buildDesiredState: pool %q: agent uses canonical singleton identity (max_active_sessions=%s) but slot %d was claimed; using base identity to avoid phantom %q-%d session\n", //nolint:errcheck
				cfgAgent.QualifiedName(), formatMaxSessions(cfgAgent), slot, cfgAgent.Name, slot)
		}
		return cfgAgent.Name, cfgAgent.QualifiedName()
	}
	instanceName := poolInstanceName(cfgAgent.Name, slot, cfgAgent)
	return instanceName, cfgAgent.QualifiedInstanceName(instanceName)
}

func formatMaxSessions(a *config.Agent) string {
	if a == nil {
		return "<nil>"
	}
	m := a.EffectiveMaxActiveSessions()
	if m == nil {
		return "unlimited"
	}
	return strconv.Itoa(*m)
}
