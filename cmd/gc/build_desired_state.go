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
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
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

// DesiredStateResult bundles the desired session state with the scale_check
// counts that produced it. Callers that need poolDesired for wake decisions
// can pass ScaleCheckCounts to ComputePoolDesiredStates without re-running
// scale_check commands.
type DesiredStateResult struct {
	State            map[string]TemplateParams
	BaseState        map[string]TemplateParams
	ScaleCheckCounts map[string]int // nil when store is nil or scale_check not run
	// ScaleCheckPartialTemplates records all templates whose bead-backed demand
	// probe failed. PoolScaleCheckPartialTemplates drives generic pool retention;
	// NamedScaleCheckPartialTemplates only protects configured named sessions.
	ScaleCheckPartialTemplates      map[string]bool
	PoolScaleCheckPartialTemplates  map[string]bool
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
	// NamedSessionDemand records which named-session identities have active
	// direct assignee demand (Assignee == identity). The reconciler merges this
	// into poolDesired so that on-demand named sessions remain config-eligible.
	NamedSessionDemand map[string]bool
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
	// StoreQueryPartial is true when one or more bead store work queries
	// failed. When set, the reconciler must NOT drain sessions based on the
	// incomplete desired state — a transient failure would cause running
	// sessions to be falsely orphaned and interrupted via Ctrl-C.
	StoreQueryPartial bool
	// SessionQueryPartial is true when session-bead snapshot loading failed.
	// Orphan-release and drain decisions must treat this like an incomplete
	// work snapshot because missing live session beads make assigned work look
	// orphaned.
	SessionQueryPartial bool
	BeaconTime          time.Time
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
	Count       int
	WorkBeadIDs []string
	Titles      map[string]string
	Packs       map[string]string
	Workspaces  map[string]string
	StoreRefs   map[string]string
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

type poolSessionCreateBudget struct {
	mu                sync.Mutex
	remaining         int
	templateRemaining map[string]int
	spare             int
}

func newPoolSessionCreateBudget(limit int) *poolSessionCreateBudget {
	if limit <= 0 {
		return nil
	}
	return &poolSessionCreateBudget{remaining: limit}
}

func (b *poolSessionCreateBudget) configureFairShare(states []PoolDesiredState, seed uint64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	shares, spare := fairPoolSessionCreateShares(states, b.remaining, seed)
	b.templateRemaining = shares
	b.spare = spare
}

func fairPoolSessionCreateShares(states []PoolDesiredState, limit int, seed uint64) (map[string]int, int) {
	if limit <= 0 {
		return nil, 0
	}
	type demand struct {
		template string
		count    int
		floor    bool
	}
	var demands []demand
	for _, state := range states {
		count := 0
		floor := false
		for _, request := range state.Requests {
			// Requests with a session bead ID represent in-flight capacity and
			// should not reserve fresh-create budget for this template.
			if request.Tier == "new" && request.SessionBeadID == "" {
				count++
				if request.FloorGuarantee {
					floor = true
				}
			}
		}
		if count > 0 {
			demands = append(demands, demand{template: state.Template, count: count, floor: floor})
		}
	}
	if len(demands) <= 1 {
		return nil, 0
	}
	shares := make(map[string]int, len(demands))
	remaining := limit
	// start rotates the per-tick allocation by seed so neither the floor
	// reservation (Phase 1) nor the elastic round-robin (Phase 2) deterministically
	// favors the same (e.g. alphabetically-first) templates every tick. Without
	// this rotation, when floor-bearing templates exceed the budget the same
	// late-order floor templates would be starved on every tick and never spawn
	// their floor (the starvation pattern fixed in fair wake-budget selection).
	start := int(seed % uint64(len(demands)))
	// Reserve a slice of the budget for elastic (non-floor) demand so a large
	// floor set can't consume the whole budget in Phase 1 and starve elastic
	// pools to zero. Without this, when floor-bearing demand >= the budget, an
	// elastic pool with real demand (e.g. a high-queue rig executor sitting
	// behind ~budget floor pools) gets zero create tokens every tick and never
	// spawns a single session. Floors keep priority (3/4 of the budget) but the
	// reserve guarantees elastic progress; for tiny budgets (< 4) the reserve is
	// 0, preserving the original floor-first behavior.
	elasticDemand := 0
	for _, d := range demands {
		if !d.floor {
			elasticDemand += d.count
		}
	}
	elasticReserve := limit / 4
	if elasticReserve > elasticDemand {
		elasticReserve = elasticDemand
	}
	floorBudget := limit - elasticReserve
	// Phase 1: guarantee one create token per floor-bearing template
	// (min_active_sessions floor) before elastic scale-check demand competes for
	// the budget. Without this, a cold pool's lone floor request loses the
	// round-robin to a warm pool's large demand and its floor never spawns.
	// Reserved in seed-rotated order, capped at floorBudget so floors can't zero
	// the elastic reserve; if floor-bearing templates exceed floorBudget, a
	// different subset is prioritized each tick so none is permanently starved.
	floorUsed := 0
	for off := 0; off < len(demands); off++ {
		if floorUsed >= floorBudget {
			break
		}
		d := demands[(start+off)%len(demands)]
		if d.floor {
			shares[d.template]++
			remaining--
			floorUsed++
		}
	}
	// Phase 2a: hand the reserved elastic slice to elastic (non-floor) demand
	// before the general round-robin, so floors deferred out of Phase 1 can't
	// reclaim it. Seed-rotated, capped at each template's request count.
	elasticGiven := 0
	for elasticGiven < elasticReserve && remaining > 0 {
		progressed := false
		for offset := 0; offset < len(demands) && remaining > 0 && elasticGiven < elasticReserve; offset++ {
			d := demands[(start+offset)%len(demands)]
			if d.floor || shares[d.template] >= d.count {
				continue
			}
			shares[d.template]++
			remaining--
			elasticGiven++
			progressed = true
		}
		if !progressed {
			break
		}
	}
	// Phase 2b: round-robin the remaining budget across all demand, capped at
	// each template's request count (a reserved floor token counts toward that
	// cap, so a floor-only template is not topped up further here).
	for remaining > 0 {
		progressed := false
		for offset := 0; offset < len(demands) && remaining > 0; offset++ {
			d := demands[(start+offset)%len(demands)]
			if shares[d.template] >= d.count {
				continue
			}
			shares[d.template]++
			remaining--
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return shares, remaining
}

func (b *poolSessionCreateBudget) tryClaim(template string) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.remaining <= 0 {
		return false
	}
	if b.templateRemaining != nil {
		switch {
		case b.templateRemaining[template] > 0:
			b.templateRemaining[template]--
		case b.spare > 0:
			b.spare--
		default:
			return false
		}
	}
	b.remaining--
	return true
}

func (b *poolSessionCreateBudget) release() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.remaining++
	if b.templateRemaining != nil {
		b.spare++
	}
}

func (bp *agentBuildParams) configurePoolSessionCreateFairShare(states []PoolDesiredState) {
	if bp == nil || bp.poolSessionCreateBudget == nil {
		return
	}
	seed := poolSessionCreateFairShareCounter.Add(1) - 1
	bp.poolSessionCreateBudget.configureFairShare(states, seed)
}

func (bp *agentBuildParams) tryClaimPoolSessionCreate(template string) bool {
	if bp == nil || bp.poolSessionCreateBudget == nil {
		return true
	}
	return bp.poolSessionCreateBudget.tryClaim(template)
}

func (bp *agentBuildParams) releasePoolSessionCreate() {
	if bp == nil || bp.poolSessionCreateBudget == nil {
		return
	}
	bp.poolSessionCreateBudget.release()
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
	result := buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, cfg, sp, store, nil, sessionBeads, nil, stderr)
	result.SessionQueryPartial = result.SessionQueryPartial || sessionQueryPartial
	return result
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
	citySt, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	if effectiveCitySuspended(cfg, citySt) {
		return DesiredStateResult{}
	}

	bp := newAgentBuildParams(cityName, cityPath, cfg, sp, beaconTime, store, stderr)
	bp.sessionBeads = sessionBeads

	// Pre-compute suspended rig paths (config + runtime state).
	suspendedRigPaths := buildSuspendedRigPathsForCity(cfg, cityPath)

	// Collect all open session beads from all stores to correctly count
	// running sessions for each pool. A partial/failed collection is logged,
	// not swallowed: undercounting running sessions can misclassify a pool as
	// cold and trigger a spurious scale-from-zero probe.
	allOpenSessionBeads, openSessionBeadsErr := collectAllOpenSessionBeads(cfg, store, rigStores, suspendedRigPaths)
	if openSessionBeadsErr != nil {
		fmt.Fprintf(stderr, "collectAllOpenSessionBeads: PARTIAL — %v (cold-pool detection may undercount running sessions)\n", openSessionBeadsErr) //nolint:errcheck
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

		sp := scaleParamsForBeads(&cfg.Agents[i], cfg.Beads)
		// Expand {{.Rig}}/{{.AgentBase}} before the scale_check enters the
		// controller probe pool so rig-scoped agents query their own rig.
		sp.Check = expandAgentCommandTemplate(cityPath, cityName, &cfg.Agents[i], cfg.Rigs, "scale_check", sp.Check, stderr)

		if !cfg.Agents[i].SupportsGenericEphemeralSessions() {
			continue
		}

		hasCustomScaleCheck := strings.TrimSpace(cfg.Agents[i].ScaleCheck) != ""
		template := cfg.Agents[i].QualifiedName()
		runningSessions := 0
		for _, sb := range allOpenSessionBeads {
			if isPoolManagedSessionBead(sb) && poolSessionIsLive(sb) {
				// Match the qualified template by identity equivalence.
				// allOpenSessionBeads is aggregated across the city + every rig
				// store, and pool session beads store the qualified name
				// (agent.QualifiedName(), see session_sleep.go); adopted beads may
				// still carry a legacy bound form of the same identity, which must
				// count here or the cold-wake probe over-wakes a pool that already
				// has a live session. Equivalence preserves the cross-rig guarantee:
				// an unqualified base name never normalizes to a dir-scoped agent,
				// and a same-base-name pool in another rig (e.g. rigB/planner)
				// normalizes to itself, so neither inflates this rig's count.
				if agentTemplateIdentitiesEquivalent(cfg, sb.Metadata["template"], template) {
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
				// Cross-store cold-wake for named-backing pools (vp-cl4): mirror the
				// generic-pool guard (vp-s37 / #3078 line ~598). A cold rig pool that
				// backs a named session and has no custom scale_check must also probe
				// the city store so that routed demand delivered there (vp-kvp) can
				// wake the pool. Same guard conditions apply: healthy own rig store,
				// not city-aliased, not city-scoped. The named-session target list
				// mirrors these probes only for partial-query retention bookkeeping.
				if isCold && ownTarget.storeKey != "city" && ownTarget.store != nil && ownTarget.err == nil && ownTarget.store != store {
					cityTarget := defaultScaleCheckTarget{template: template, store: store, storeKey: "city"}
					if namedSessionMode != "always" {
						defaultScaleTargets = append(defaultScaleTargets, cityTarget)
					}
					defaultNamedScaleTargets = append(defaultNamedScaleTargets, cityTarget)
				}
				continue
			}
			if store != nil && isCold {
				for _, source := range activeStores {
					defaultNamedScaleTargets = append(defaultNamedScaleTargets, defaultScaleCheckTarget{template: template, store: source.store, storeKey: source.ref})
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
			// Cross-store cold-wake (FR-S0.1 / vp-s37): a cold rig pool's routed
			// demand may live in the city store (vp-kvp cross-store delivery),
			// which the own-rig probe above cannot see while the pool sleeps —
			// so a sleeping rig pool would never wake to discover it. Add a
			// city-store probe for cold rig pools so their demand reflects
			// routed work in either store. No clamp: unlike a custom-scale_check
			// pool — where the probe is clamped so it cannot override the custom
			// count (see coldWakeTemplates below) — the default probe IS the
			// authoritative count, so it scales to total routed demand (bounded
			// by max_active and the daemon's max_wakes_per_tick), matching the
			// retired cold-pool-spawner's scale-to-want. A city-scoped pool's
			// own target is already the city store, so it needs no extra probe.
			//
			// Gated on a healthy own rig store: when the rig store is missing or
			// errored we stay partial and do NOT wake on cross-store demand —
			// a rig executor cannot do its work while its rig store is
			// unreachable, and the partial flag must keep suppressing drain
			// decisions rather than be overridden by a spurious city-store wake.
			//
			// ownTarget.store != store guards the case where the rig store
			// aliases the city store (an unbound rig falling back to the city
			// scope): a separate "city" group over the same store would
			// double-count the same beads, since defaultScaleCheckCounts dedups
			// per group, not across groups. Current store-map builders skip
			// such rigs, so this is defense-in-depth against future callers.
			if isCold && ownTarget.storeKey != "city" && ownTarget.store != nil && ownTarget.err == nil && ownTarget.store != store {
				defaultScaleTargets = append(defaultScaleTargets, defaultScaleCheckTarget{template: template, store: store, storeKey: "city"})
			}
			continue
		}
		if store != nil && isCold {
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
	var readyAssigned map[storeScopedBeadKey]bool
	var storePartial bool
	var scaleCheckCounts map[string]int
	var scaleCheckDemandByTemplate map[string]scaleCheckDemand
	var poolScaleCheckPartialTemplates map[string]bool
	var namedScaleCheckPartialTemplates map[string]bool
	var scaleCheckPartialTemplates map[string]bool
	var namedDefaultDemand map[string]bool
	if store != nil {
		assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs, readyAssigned, storePartial = collectAssignedWorkBeadsWithStores(cfg, store, rigStores, suspendedRigPaths, sessionBeads)
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
		// Re-home work pre-assigned to a legacy bound form of a now-unbound pool
		// agent onto the canonical identity, so the canonical session the
		// awake/scale accounting wakes for it can actually surface and claim it
		// (the agent-side work_query/claim path matches identities by raw string).
		canonicalizeLegacyBoundAssignedWork(cfg, assignedWorkBeads, assignedWorkStores, sessionBeads, stderr)
		// Re-home open, unassigned work still routed to a legacy bound form of a
		// now-unbound pool agent. This is the demand/claim half of the migration:
		// empty-assignee open work never enters the assigned-work collection above,
		// and the canonical pool-demand probe below (defaultScaleCheckCounts) plus
		// the worker work_query/claim path match gc.routed_to canonically by raw
		// string, so the route must be canonicalized before demand is counted or
		// the cold pool never wakes for it.
		unassignedRoutedBeads, unassignedRoutedStores := collectOpenUnassignedRoutedWork(cfg, store, rigStores, suspendedRigPaths, stderr)
		canonicalizeLegacyBoundUnassignedRoutedWork(cfg, unassignedRoutedBeads, unassignedRoutedStores, stderr)
		controlDispatcherOpenDemand := openControlDispatcherDemand(cfg, unassignedRoutedBeads)
		scaleCheckCounts, poolScaleCheckPartialTemplates = evaluatePendingPoolsMap(cfg, pendingPools, stderr, trace)
		if len(defaultScaleTargets) > 0 {
			defaultCounts, defaultDemand, partialTemplates, errs := defaultScaleCheckCountsAndDemand(defaultScaleTargets)
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
		if len(defaultNamedScaleTargets) > 0 {
			var namedErrs []error
			var partialTemplates map[string]bool
			namedDefaultDemand, partialTemplates, namedErrs = defaultNamedSessionDemand(defaultNamedScaleTargets, cfg, cityName)
			for _, err := range namedErrs {
				fmt.Fprintf(stderr, "buildDesiredState: %v (using named demand=false)\n", err) //nolint:errcheck
			}
			namedScaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(namedScaleCheckPartialTemplates, partialTemplates)
		}
		scaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(scaleCheckPartialTemplates, poolScaleCheckPartialTemplates)
		scaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(scaleCheckPartialTemplates, namedScaleCheckPartialTemplates)
		if len(scaleCheckPartialTemplates) > 0 {
			fmt.Fprintf(stderr, "scaleCheck: PARTIAL — scale_check failed for %s, retaining affected sessions\n", strings.Join(sortedBoolMapKeys(scaleCheckPartialTemplates), ",")) //nolint:errcheck
		}
		poolWorkBeads := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, sessionBeads.Open(), assignedWorkBeads, assignedWorkStoreRefs)
		bp.assignedWorkBeads = poolWorkBeads
		bp.poolScaleCheckPartialTemplates = poolScaleCheckPartialTemplates
		bp.providerHealthSnapshot = loadProviderHealthSnapshot(cityPath)
		poolDesiredStates := ComputePoolDesiredStatesWithDemandTraced(cfg, poolWorkBeads, sessionBeads.Open(), scaleCheckCounts, scaleCheckDemandByTemplate, trace)
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
			realizePoolDesiredSessions(bp, cfgAgent, poolState, desired, stderr)
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
				setTemplateEnvIdentity(&tp, qualifiedInstance)
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
			if assignee != identity {
				continue
			}
			if !assignedWorkIndexReachableFromAgent(cityPath, cfg, spec.Agent, assignedWorkStoreRefs, i) {
				continue
			}
			fmt.Fprintf(stderr, "namedWorkReady: %s matched by bead %s (assignee=%s status=%s)\n", identity, wb.ID, assignee, wb.Status) //nolint:errcheck
			namedWorkReady[identity] = true
			break
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
	applySessionBeadDesiredOverlay(bp, cfg, desired, suspendedRigPaths, poolScaleCheckPartialTemplates, namedScaleCheckPartialTemplates, stderr)

	return DesiredStateResult{
		State:                           desired,
		BaseState:                       baseDesired,
		ScaleCheckCounts:                scaleCheckCounts,
		ScaleCheckPartialTemplates:      scaleCheckPartialTemplates,
		PoolScaleCheckPartialTemplates:  poolScaleCheckPartialTemplates,
		NamedScaleCheckPartialTemplates: namedScaleCheckPartialTemplates,
		AssignedWorkBeads:               assignedWorkBeads,
		AssignedWorkStores:              assignedWorkStores,
		AssignedWorkStoreRefs:           assignedWorkStoreRefs,
		ReadyAssigned:                   readyAssigned,
		NamedSessionDemand:              namedWorkReady,
		StoreQueryPartial:               storePartial,
		BeaconTime:                      beaconTime,
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

func collectAllOpenSessionBeads(
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
) ([]beads.Bead, error) {
	// Sessions arm of the reconciler frame: iterate the session-class candidate
	// fan-out (city + non-suspended rigs). CachingStore-wrapped stores are used
	// when available. On a single-store city this hits the same store the work
	// arm queries (identity); routing through the shared per-class candidate
	// builder keeps the work-vs-session split structurally explicit.
	stores := coordClassStoreCandidates(cfg, cityStore, rigStores, suspendedRigPaths, "city")

	type storeResult struct {
		beads []beads.Bead
		err   error
	}
	results := make([]storeResult, len(stores))
	var wg sync.WaitGroup
	for idx, source := range stores {
		idx, source := idx, source
		wg.Add(1)
		go func() {
			defer wg.Done()
			sessions, err := session.ListAllSessionBeads(source.store, beads.ListQuery{})
			results[idx] = storeResult{beads: sessions, err: err}
		}()
	}
	wg.Wait()

	var allBeads []beads.Bead
	var errs []error
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			if beads.IsPartialResult(r.err) {
				for _, b := range r.beads {
					if b.Status != "closed" {
						allBeads = append(allBeads, b)
					}
				}
			}
			continue
		}
		for _, b := range r.beads {
			if b.Status != "closed" {
				allBeads = append(allBeads, b)
			}
		}
	}
	if len(errs) > 0 {
		return allBeads, errors.Join(errs...)
	}
	return allBeads, nil
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
	applySessionBeadDesiredOverlay(bp, cfg, refreshed.State, buildSuspendedRigPathsForCity(cfg, cityPath), result.PoolScaleCheckPartialTemplates, result.NamedScaleCheckPartialTemplates, stderr)
	return refreshed
}

// collectAssignedWorkBeads queries each store (city + rigs) for actionable
// assigned work. It includes in-progress assigned work plus open assigned
// work that is actually ready. Routed-but-unassigned pool queue work is
// intentionally excluded here, except stranded in-progress pool work with no
// assignee is included so reconciliation can reopen it for normal claiming.
func collectAssignedWorkBeads(
	cfg *config.City,
	cityStore beads.Store,
) ([]beads.Bead, bool) {
	result, _, _, _, partial := collectAssignedWorkBeadsWithStores(cfg, cityStore, nil, nil, nil)
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
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	sessionBeads *sessionBeadSnapshot,
) ([]beads.Bead, []beads.Store, []string, map[storeScopedBeadKey]bool, bool) {
	// Work arm of the reconciler frame: iterate the work-class candidate fan-out
	// (city + non-suspended rigs). The city store carries the empty store-ref so
	// the index-aligned workBeads/workStores slices stay per-bead aligned for the
	// canonicalize/stamp writers. CachingStore-wrapped stores are used; creating
	// raw bdStoreForCity per rig spawns bd subprocesses on every tick, saturating
	// dolt. On a single-store city this is the same store the sessions arm
	// iterates (identity).
	stores := coordClassStoreCandidates(cfg, cityStore, rigStores, suspendedRigPaths, "")

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
			if openRouted, err := listBothTiersForControllerDemand(source.store, beads.ListQuery{Status: "open"}); err == nil {
				appendOpenAssignedMoleculeWorkUnique(&result, &resultStores, &resultStoreRefs, readyIDs, openRouted, seen, source.store, source.ref)
				appendOpenRoutedWorkUnique(&result, &resultStores, &resultStoreRefs, openRouted, seen, source.store, source.ref)
			} else {
				errs = append(errs, fmt.Errorf("List(open): %w", err))
				if beads.IsPartialResult(err) && len(openRouted) > 0 {
					appendOpenAssignedMoleculeWorkUnique(&result, &resultStores, &resultStoreRefs, readyIDs, openRouted, seen, source.store, source.ref)
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
				ready, err = liveReadyForControllerDemandQuery(source.store, beads.ReadyQuery{Limit: assignedWorkReadyLimit(cfg)})
				if err != nil {
					errs = append(errs, fmt.Errorf("Ready(): %w", err))
				}
			} else {
				for _, assignee := range assignees {
					part, partErr := liveReadyForControllerDemandQuery(source.store, beads.ReadyQuery{Assignee: assignee, Limit: assignedWorkReadyLimit(cfg)})
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
	for _, session := range sessionBeads.Open() {
		ids := sessionBeadAssigneeIdentities(session)
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
		for _, session := range sessionBeads.Open() {
			if session.Status == "closed" {
				continue
			}
			for _, id := range sessionBeadAssigneeIdentities(session) {
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
// and named-session demand so they are intentionally excluded here.
func defaultScaleCheckCounts(targets []defaultScaleCheckTarget) (map[string]int, map[string]bool, []error) {
	counts, _, partialTemplates, errs := defaultScaleCheckCountsAndDemand(targets)
	return counts, partialTemplates, errs
}

func defaultScaleCheckCountsAndDemand(targets []defaultScaleCheckTarget) (map[string]int, map[string]scaleCheckDemand, map[string]bool, []error) {
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

	for key, group := range groups {
		// Ready()/CachedReady() iteration surfaces actionable work
		// matched against gc.routed_to/gc.run_target. Formula orders that
		// should wake pools must create an actionable root, such as a
		// vapor/root-only wisp. Molecule containers and formula step
		// beads remain hidden by readyExcludeTypes.
		ready, readyErr := readyForControllerDemand(group.store)
		if readyErr != nil {
			errs = append(errs, fmt.Errorf("default scale_check %s templates=%s: Ready(): %w", key, strings.Join(sortedStringSet(group.templates), ","), readyErr))
			partialTemplates = markScaleCheckPartialSet(partialTemplates, group.templates)
			if !beads.IsPartialResult(readyErr) {
				ready = nil
			}
		}
		for _, b := range ready {
			if strings.TrimSpace(b.Assignee) != "" {
				continue
			}
			template := controllerDemandRouteTarget(b, group.templates)
			if _, ok := group.templates[template]; !ok {
				continue
			}
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
	}
	existing.Count = len(existing.WorkBeadIDs)
	if existing.Count < count {
		existing.Count = count
	}
	return existing
}

func defaultNamedSessionDemand(targets []defaultScaleCheckTarget, _ *config.City, _ string) (map[string]bool, map[string]bool, []error) {
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
		_, err := readyForControllerDemand(group.store)
		if err != nil {
			errs = append(errs, fmt.Errorf("default scale_check %s templates=%s: Ready(): %w", key, strings.Join(sortedStringSet(group.templates), ","), err))
			partialTemplates = markScaleCheckPartialSet(partialTemplates, group.templates)
			continue
		}
	}
	return demand, partialTemplates, errs
}

func controllerDemandRouteTarget(b beads.Bead, templates map[string]struct{}) string {
	for _, candidate := range controllerDemandRouteCandidates(b) {
		if _, ok := templates[candidate]; ok {
			return candidate
		}
	}
	return ""
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
	for _, b := range sessionBeads.Open() {
		// Adopted session beads can persist a legacy bound template identity;
		// normalize to the current canonical name before the membership check,
		// because partialTemplates is keyed canonically. Without this a transient
		// scale_check partial failure would drop legacy-bound pool sessions.
		template := normalizeAgentTemplateIdentity(cfg, strings.TrimSpace(b.Metadata["template"]))
		if !partialTemplates[template] || !isPoolManagedSessionBead(b) || !scaleCheckPartialSessionRetainable(b) {
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

// Preserve dormant affected-template beads during transient scale_check
// failures, but do not count them as awake demand. Sessions that are already
// mid-drain or past-drain (draining/drained/archived) are not preserved so a
// partial read cannot interrupt an in-progress drain lifecycle.
func scaleCheckPartialSessionPreservable(b beads.Bead) bool {
	switch strings.TrimSpace(b.Metadata["state"]) {
	case "", "active", "awake", "start-pending", "creating", "asleep", "stopped", "suspended", "quarantined":
		return true
	default:
		return isPendingPoolCreate(b)
	}
}

func scaleCheckPartialSessionRetainable(b beads.Bead) bool {
	switch strings.TrimSpace(b.Metadata["state"]) {
	case "active", "awake":
		return true
	default:
		// A fresh in-flight create that still holds an active pending_create_claim
		// lease counts as retained capacity. Stale creates (lease expired/cleared)
		// return false so they stop inflating the desired count.
		return isPendingPoolCreate(b)
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
	for _, b := range sessionBeads.Open() {
		if b.Status == "closed" {
			continue
		}
		sn := b.Metadata["session_name"]
		if sn == "" {
			continue
		}
		if isFailedCreateSessionBead(b) {
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
		template := resolvedSessionTemplate(b, cfg)
		if template == "" {
			continue
		}
		poolScaleCheckPartial := poolScaleCheckPartialTemplates[template]
		namedScaleCheckPartial := namedScaleCheckPartialTemplates[template] && isNamedSessionBead(b)
		scaleCheckPartial := scaleCheckPartialSessionPreservable(b) && (poolScaleCheckPartial || namedScaleCheckPartial)
		// Find the config agent for this template.
		cfgAgent := findAgentByTemplate(cfg, template)
		if cfgAgent == nil {
			continue
		}
		if agentInSuspendedRig(bp.cityPath, cfgAgent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		roots[template] = true
		if !sessionAlreadyDesired && !isManualSessionBeadForAgent(b, cfgAgent) && !isNamedSessionBead(b) &&
			desiredHasCanonicalNonExpandingPoolSession(desired, template, cfgAgent) && staleNonExpandingPoolSessionBead(cfgAgent, b) {
			continue
		}
		if !isManualSessionBead(b) && !isNamedSessionBead(b) && !isPoolManagedSessionBead(b) && desiredHasConfiguredNamedTemplate(desired, template) {
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
		if isEphemeralSessionBeadForAgent(b, cfgAgent) {
			manualSession := isManualSessionBeadForAgent(b, cfgAgent)
			creating := b.Metadata["state"] == "creating" || b.Metadata["state"] == string(session.StateStartPending)
			pendingCreate := isPendingPoolCreate(b)
			templateDesired := desiredHasTemplate(desired, template)
			// Pool-managed beads are controller-created capacity. A pending
			// or creating bead that the pool pass did not select is stale
			// capacity, not a reason to spawn a worker with an empty hook.
			controllerManagedPool := strings.TrimSpace(b.Metadata[poolManagedMetadataKey]) == boolMetadata(true) ||
				strings.TrimSpace(b.Metadata["pool_slot"]) != "" || pendingCreate
			if controllerManagedPool && isDrainedSessionBead(b) {
				continue
			}
			if controllerManagedPool && !manualSession && !isNamedSessionBead(b) &&
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
				(isPendingPoolCreate(b) || (!creating && scaleCheckPartialSessionPreservable(b)))
			if controllerManagedPool && !manualSession && !isNamedSessionBead(b) &&
				!sessionAlreadyDesired && !templateDesired && !poolPartialAlive {
				continue
			}
			if !manualSession && (!creating || isStaleCreating(b)) && !templateDesired && !pendingCreate && !scaleCheckPartial {
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
		var (
			resolveAgent         *config.Agent
			sessionQualifiedName string
		)
		if isManualSessionBeadForAgent(b, cfgAgent) {
			sessionQualifiedName = sessionBeadQualifiedName(bp.cityPath, cfgAgent, bp.rigs, b)
			resolveAgent = sessionBeadConfigAgent(cfgAgent, sessionQualifiedName)
		} else {
			// Canonicalize agent identity before calling resolveTemplate so a
			// pool-managed bead with pool_slot stamped resolves as the
			// pool-instance form here — the same shape realizePoolDesiredSessions
			// uses. Before GC_ALIAS was excluded from CoreFingerprint, this
			// identity mismatch caused config-drift drains; the canonical shape
			// still keeps routing/display identity and remaining fingerprint
			// inputs aligned across buildDesiredState paths. Named beads
			// intentionally pass through with the base shape (see
			// canonicalSessionIdentity).
			resolveAgent, sessionQualifiedName = canonicalSessionIdentityWithConfig(cfg, cfgAgent, b)
		}
		fpExtra := buildFingerprintExtra(resolveAgent)
		tp, err := resolveTemplateForSessionBead(bp, resolveAgent, sessionQualifiedName, fpExtra, b)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: bead %s template %q: %v (skipping)\n", b.ID, template, err) //nolint:errcheck
			continue
		}
		tp.ManualSession = isManualSessionBeadForAgent(b, cfgAgent)
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

func isPendingPoolCreate(b beads.Bead) bool {
	return isPoolManagedSessionBead(b) && strings.TrimSpace(b.Metadata["pending_create_claim"]) == boolMetadata(true)
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
			ensureDependencyOnlyTemplate(bp, cfg, depAgent, desired, stderr)
			visit(dep)
		}
	}
	for template := range roots {
		visit(template)
	}
}

func ensureDependencyOnlyTemplate(
	bp *agentBuildParams,
	cfg *config.City,
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
		resolveAgent, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(cfgAgent, 1)
		fpExtra := buildFingerprintExtra(resolveAgent)
		tp, err := resolveTemplatePrepared(bp, resolveAgent, qualifiedInstance, fpExtra)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedInstance, err) //nolint:errcheck
			return
		}
		tp.DependencyOnly = true
		tp.PoolSlot = poolSlot
		setTemplateEnvIdentity(&tp, qualifiedInstance)
		installAgentSideEffects(bp, resolveAgent, tp, stderr)
		desired[tp.SessionName] = tp
		return
	}

	// Bead selection keys off the configured base template, not the pool-
	// instance form, because normalizedSessionTemplate reads the bead's
	// "template" metadata which is always the base.
	sessionBead, err := selectOrCreateDependencyPoolSessionBead(bp, cfgAgent, qualifiedName)
	if err != nil {
		fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
		return
	}
	// Env/fingerprint resolution, on the other hand, must use the same
	// canonical-or-instance identity as both the no-store dependency-floor
	// path above and realizePoolDesiredSessions. Otherwise GC_ALIAS can
	// oscillate across ticks and trigger the reconciler's config-drift drain
	// on the live dependency-floor session.
	resolveAgent, resolveQN := canonicalSessionIdentityWithConfig(cfg, cfgAgent, sessionBead)
	// Dep-floor slot-1 fallback. The guard triggers when the helper returned
	// the BASE form — meaning no pool_slot was stamped yet. Keying off
	// resolveQN (a stable value) rather than pointer identity keeps the
	// fallback correct if the helper ever normalizes fields into a copy of
	// the base agent. The !isNamedSessionBead guard is defensive:
	// selectOrCreateDependencyPoolSessionBead already filters named beads
	// (dependency_only beads are never named), but the guard keeps intent
	// explicit so a future change that relaxes that filter can't silently
	// overwrite a named identity with "rig/<agent>-1".
	if cfgAgent.SupportsInstanceExpansion() && !cfgAgent.UsesCanonicalSingletonPoolIdentity() && resolveQN == cfgAgent.QualifiedName() && !isNamedSessionBead(sessionBead) {
		// No pool_slot stamp yet on this freshly-created dep-floor bead.
		// Default to slot 1, mirroring the no-store path above.
		instanceName := poolInstanceName(cfgAgent.Name, 1, cfgAgent)
		qualifiedInstance := cfgAgent.QualifiedInstanceName(instanceName)
		instanceAgent := deepCopyAgent(cfgAgent, instanceName, cfgAgent.Dir)
		resolveAgent = &instanceAgent
		resolveQN = qualifiedInstance
	}
	fpExtra := buildFingerprintExtra(resolveAgent)
	tp, err := resolveTemplateForSessionBead(bp, resolveAgent, resolveQN, fpExtra, sessionBead)
	if err != nil {
		fmt.Fprintf(stderr, "buildDesiredState: dependency floor %q: %v (skipping)\n", qualifiedName, err) //nolint:errcheck
		return
	}
	tp.Alias = ""
	tp.InstanceName = sessionBead.Metadata["session_name"]
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
// either sessionBead+slot (reuse path) or plan+slot (create path); Phase B
// (parallel-bounded) materializes plans into sessionBead/createErr; Phase C
// (serial) resolves the template and installs side effects.
type poolRealizeWorkItem struct {
	request     SessionRequest
	skip        bool
	plan        *poolSessionCreatePlan
	sessionBead beads.Bead
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
			var prefer *beads.Bead
			if request.SessionBeadID != "" {
				if bead, ok := findOpenSessionBeadByID(bp.sessionBeads, request.SessionBeadID); ok {
					// Defense in depth: ComputePoolDesiredStates filters out
					// named-session beads from pool resume requests. If one
					// slipped through, materializing it here would create a
					// phantom "{name}-N" sibling to the canonical named session.
					if isNamedSessionBead(bead) {
						fmt.Fprintf(stderr, "buildDesiredState: pool %q: refusing to materialize named-session bead %s as pool instance (would create phantom %q-N sibling)\n", qualifiedName, bead.ID, cfgAgent.Name) //nolint:errcheck
						item.skip = true
						return item
					}
					prefer = &bead
				}
			}
			sessionBead, slot, plan, err := selectOrPlanPoolSessionBead(bp, cfgAgent, qualifiedName, prefer, request, used, usedSlots)
			if err != nil {
				switch {
				case errors.Is(err, errPoolSessionCreateBudgetExhausted):
					fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (fresh create deferred)\n", qualifiedName, err) //nolint:errcheck
				case errors.Is(err, errPoolSessionCreatePartial):
					fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (partial demand read, fresh create blocked)\n", qualifiedName, err) //nolint:errcheck
				case errors.Is(err, errPoolSessionCreateProviderRed):
					// debug-level: fires every tick during a red episode; not operator noise
					fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (provider red, fresh create blocked)\n", qualifiedName, err) //nolint:errcheck
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
			if used[sessionBead.ID] {
				item.skip = true
				return item
			}
			used[sessionBead.ID] = true
			item.sessionBead = sessionBead
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
					bead, err := executePlannedPoolSessionBeadCreate(bp, cfgAgent, qualifiedName, plan)
					if err != nil {
						items[idx].createErr = err
						continue
					}
					items[idx].sessionBead = bead
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
			if used[item.sessionBead.ID] {
				continue
			}
			used[item.sessionBead.ID] = true
		}
		sessionBead := item.sessionBead
		if bound, err := bindPoolSessionTriggerBead(bp, cfgAgent, qualifiedName, sessionBead, item.request); err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: pool %q session %s trigger bead %s: %v (continuing without trigger env)\n", qualifiedName, sessionBead.ID, item.request.WorkBeadID, err) //nolint:errcheck
		} else {
			sessionBead = bound
			item.sessionBead = bound
		}
		slot := item.slot
		manualSession := isManualSessionBeadForAgent(sessionBead, cfgAgent)
		var (
			resolveAgent      *config.Agent
			qualifiedInstance string
			poolSlot          int
		)
		if manualSession {
			qualifiedInstance = sessionBeadQualifiedName(bp.cityPath, cfgAgent, bp.rigs, sessionBead)
			resolveAgent = sessionBeadConfigAgent(cfgAgent, qualifiedInstance)
		} else {
			resolveAgent, qualifiedInstance, poolSlot = poolDesiredRequestIdentity(cfgAgent, slot)
		}
		fpExtra := buildFingerprintExtra(resolveAgent)
		tp, err := resolveTemplateForSessionBead(bp, resolveAgent, qualifiedInstance, fpExtra, sessionBead)
		if err != nil {
			fmt.Fprintf(stderr, "buildDesiredState: pool %q session %s: %v (skipping)\n", qualifiedName, sessionBead.ID, err) //nolint:errcheck
			continue
		}
		if manualSession {
			tp.ManualSession = true
			if manualAlias := strings.TrimSpace(sessionBead.Metadata["alias"]); manualAlias != "" {
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
			tp.Alias = qualifiedInstance
			tp.InstanceName = qualifiedInstance
			tp.PoolSlot = poolSlot
			setPoolTemplateRuntimeIdentity(&tp, qualifiedInstance, sessionBead)
		}
		installAgentSideEffects(bp, resolveAgent, tp, stderr)
		desired[tp.SessionName] = tp
	}
}

func bindPoolSessionTriggerBead(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, sessionBead beads.Bead, request SessionRequest) (beads.Bead, error) {
	workBeadID := strings.TrimSpace(request.WorkBeadID)
	if sessionBead.ID == "" {
		return sessionBead, nil
	}
	metadata := map[string]string{}
	if workBeadID == "" {
		if strings.TrimSpace(sessionBead.Metadata[beadmeta.TriggerBeadIDMetadataKey]) != "" {
			metadata[beadmeta.TriggerBeadIDMetadataKey] = ""
		}
		if strings.TrimSpace(sessionBead.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]) != "" {
			metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = ""
		}
		// Q1: a re-pointed session is a new work item; it must not silently
		// inherit the prior fork's "warm" provenance. Clear the parent sid so the
		// selection layer must re-stamp it deterministically to stay warm.
		if strings.TrimSpace(sessionBead.Metadata[beadmeta.BrainParentSIDMetadataKey]) != "" {
			metadata[beadmeta.BrainParentSIDMetadataKey] = ""
		}
		if len(metadata) == 0 {
			return sessionBead, nil
		}
		if bp != nil && bp.beadStore != nil {
			if err := bp.beadStore.Update(sessionBead.ID, beads.UpdateOpts{Metadata: metadata}); err != nil {
				return sessionBead, err
			}
		}
		sessionBead.Metadata = cloneStringMap(sessionBead.Metadata)
		for key, value := range metadata {
			sessionBead.Metadata[key] = value
		}
		return sessionBead, nil
	}
	oldWorkBeadID := strings.TrimSpace(sessionBead.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	if oldWorkBeadID != workBeadID {
		metadata[beadmeta.TriggerBeadIDMetadataKey] = workBeadID
		// Q1: on a genuine reassign to a different work bead, reconcile the fork
		// parent to the new work's value (set when the new bead carries one,
		// clear otherwise) so a re-pointed session never inherits the old fork.
		newParentSID := strings.TrimSpace(request.BrainParentSID)
		if strings.TrimSpace(sessionBead.Metadata[beadmeta.BrainParentSIDMetadataKey]) != newParentSID {
			metadata[beadmeta.BrainParentSIDMetadataKey] = newParentSID
		}
	}
	workStoreRef := strings.TrimSpace(request.WorkStoreRef)
	if workStoreRef != "" && strings.TrimSpace(sessionBead.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]) != workStoreRef {
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = workStoreRef
	} else if workStoreRef == "" && oldWorkBeadID != workBeadID && strings.TrimSpace(sessionBead.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]) != "" {
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = ""
	}
	if pack := strings.TrimSpace(request.WorkPack); strings.TrimSpace(sessionBead.Metadata[beadmeta.PackMetadataKey]) != pack {
		metadata[beadmeta.PackMetadataKey] = pack
	}
	if workspace := packWorkspaceSlug(request); strings.TrimSpace(sessionBead.Metadata[beadmeta.PackWorkspaceMetadataKey]) != workspace {
		metadata[beadmeta.PackWorkspaceMetadataKey] = workspace
	}
	if workDir := poolTriggerWorkDir(bp, cfgAgent, qualifiedName, request); workDir != "" {
		if strings.TrimSpace(sessionBead.Metadata[beadmeta.WorkDirMetadataKey]) != workDir {
			metadata[beadmeta.WorkDirMetadataKey] = workDir
		}
		if strings.TrimSpace(sessionBead.Metadata[beadmeta.LegacyWorkDirMetadataKey]) != workDir {
			metadata[beadmeta.LegacyWorkDirMetadataKey] = workDir
		}
	}
	if len(metadata) == 0 {
		return sessionBead, nil
	}
	if bp != nil && bp.beadStore != nil {
		if err := bp.beadStore.Update(sessionBead.ID, beads.UpdateOpts{Metadata: metadata}); err != nil {
			return sessionBead, err
		}
	}
	sessionBead.Metadata = cloneStringMap(sessionBead.Metadata)
	if sessionBead.Metadata == nil {
		sessionBead.Metadata = map[string]string{}
	}
	for key, value := range metadata {
		sessionBead.Metadata[key] = value
	}
	return sessionBead, nil
}

func poolTriggerWorkDir(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, request SessionRequest) string {
	if bp == nil || cfgAgent == nil || strings.TrimSpace(request.WorkBeadID) == "" {
		return ""
	}
	base, err := resolveConfiguredWorkDir(bp.cityPath, bp.cityName, qualifiedName, cfgAgent, bp.rigs)
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
	if slug := triggerBeadPathSlug(request.WorkBeadID, request.WorkBeadTitle); slug != "" {
		return filepath.Join(base, slug)
	}
	return ""
}

func packWorkspaceSlug(request SessionRequest) string {
	if explicit := safeWorkspaceName(request.WorkWorkspace, 96); explicit != "" {
		return explicit
	}
	return ""
}

func triggerBeadPathSlug(beadID, title string) string {
	id := safePathSlug(beadID, 32)
	titleSlug := safePathSlug(title, 72)
	switch {
	case id != "" && titleSlug != "":
		return id + "-" + titleSlug
	case id != "":
		return id
	default:
		return titleSlug
	}
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

func safePathSlug(value string, maxLen int) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		var out rune
		switch {
		case r >= 'a' && r <= 'z':
			out = r
		case r >= '0' && r <= '9':
			out = r
		default:
			out = '-'
		}
		if out == '-' {
			if b.Len() == 0 || lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(out)
		if maxLen > 0 && b.Len() >= maxLen {
			break
		}
	}
	return strings.Trim(b.String(), "-")
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

// setPoolTemplateRuntimeIdentity stamps the pool alias unless this bead is in a
// known deferred-alias state. Stable legacy pool beads can lack alias metadata;
// those keep their historic instance identity until syncSessionBeads backfills.
func setPoolTemplateRuntimeIdentity(tp *TemplateParams, desiredAlias string, sessionBead beads.Bead) {
	if tp == nil {
		return
	}
	if strings.TrimSpace(sessionBead.Metadata["alias"]) != strings.TrimSpace(desiredAlias) && poolRuntimeAliasIsDeferred(sessionBead) {
		tp.Alias = ""
		if tp.Env == nil {
			tp.Env = make(map[string]string)
		}
		tp.Env["GC_ALIAS"] = ""
		if tp.SessionName != "" {
			tp.Env["GC_AGENT"] = tp.SessionName
		}
		tp.EnvIdentityStamped = false
		return
	}
	tp.Alias = desiredAlias
	setTemplateEnvIdentity(tp, desiredAlias)
}

func poolRuntimeAliasIsDeferred(sessionBead beads.Bead) bool {
	if strings.TrimSpace(sessionBead.Metadata["alias"]) != "" {
		return false
	}
	if strings.TrimSpace(sessionBead.Metadata[poolAliasConflictMetadataKey]) != "" {
		return true
	}
	if strings.TrimSpace(sessionBead.Metadata["pending_create_claim"]) == boolMetadata(true) {
		return true
	}
	state := strings.TrimSpace(sessionBead.Metadata["state"])
	return state == "creating" || state == string(session.StateStartPending)
}

func normalizeNonExpandingPoolSessionBead(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	sessionBead beads.Bead,
) (beads.Bead, error) {
	// The store write is authoritative; callers must use the returned bead
	// rather than re-reading bp.sessionBeads for this ID in the same tick.
	// If alias acquisition collides, this helper records the deferred state;
	// syncSessionBeads owns the retry once the canonical alias holder closes.
	if bp == nil || bp.beadStore == nil || !cfgAgent.UsesCanonicalSingletonPoolIdentity() || isManualSessionBeadForAgent(sessionBead, cfgAgent) || isNamedSessionBead(sessionBead) || sessionBead.ID == "" {
		return sessionBead, nil
	}
	canonical := cfgAgent.QualifiedName()
	metadata := map[string]string{}
	aliasNeedsUpdate := false
	clearAliasConflictMetadata := func() {
		queueClearPoolAliasConflictMetadata(metadata, sessionBead.Metadata)
	}
	alias := strings.TrimSpace(sessionBead.Metadata["alias"])
	deferredAlias := strings.TrimSpace(sessionBead.Metadata[poolAliasConflictMetadataKey])
	if nonExpandingPoolIdentitySlot(cfgAgent, sessionBeadAgentName(sessionBead)) > 0 && strings.TrimSpace(sessionBead.Metadata["agent_name"]) != canonical {
		metadata["agent_name"] = canonical
	}
	if (nonExpandingPoolIdentitySlot(cfgAgent, alias) > 0 && alias != canonical) || (alias == "" && deferredAlias == canonical) {
		for key, value := range session.UpdatedAliasMetadata(sessionBead.Metadata, canonical) {
			metadata[key] = value
		}
		clearAliasConflictMetadata()
		aliasNeedsUpdate = true
	}
	if alias == canonical {
		clearAliasConflictMetadata()
	}
	if strings.TrimSpace(sessionBead.Metadata["pool_slot"]) != "" {
		metadata["pool_slot"] = ""
	}

	var title *string
	if nonExpandingPoolIdentitySlot(cfgAgent, sessionBead.Title) > 0 && strings.TrimSpace(sessionBead.Title) != canonical {
		normalizedTitle := canonical
		title = &normalizedTitle
	}

	removeLabels := make([]string, 0, len(sessionBead.Labels))
	hasCanonicalAgentLabel := containsString(sessionBead.Labels, "agent:"+canonical)
	for _, label := range sessionBead.Labels {
		label = strings.TrimSpace(label)
		if strings.HasPrefix(label, "agent:") && nonExpandingPoolIdentitySlot(cfgAgent, strings.TrimPrefix(label, "agent:")) > 0 {
			removeLabels = append(removeLabels, label)
		}
	}
	var addLabels []string
	if (len(metadata) > 0 || title != nil || len(removeLabels) > 0) && !hasCanonicalAgentLabel {
		addLabels = []string{"agent:" + canonical}
	}
	if len(metadata) == 0 && title == nil && len(removeLabels) == 0 && len(addLabels) == 0 {
		return sessionBead, nil
	}

	apply := func() error {
		return bp.beadStore.Update(sessionBead.ID, beads.UpdateOpts{
			Title:        title,
			Metadata:     metadata,
			Labels:       addLabels,
			RemoveLabels: removeLabels,
		})
	}
	if aliasNeedsUpdate {
		if err := session.WithCitySessionAliasLock(bp.cityPath, canonical, func() error {
			if err := session.EnsureAliasAvailableWithConfig(bp.beadStore, bp.city, canonical, sessionBead.ID); err != nil {
				return err
			}
			return apply()
		}); err != nil {
			return sessionBead, fmt.Errorf("normalizing singleton pool identity for bead %s to %q: %w", sessionBead.ID, canonical, err)
		}
	} else if err := apply(); err != nil {
		return sessionBead, fmt.Errorf("normalizing singleton pool identity for bead %s to %q: %w", sessionBead.ID, canonical, err)
	}

	if bp.stderr != nil {
		fmt.Fprintf(bp.stderr, "buildDesiredState: pool %q: collapsing phantom pool identity for bead %s to %q\n", canonical, sessionBead.ID, canonical) //nolint:errcheck
	}
	if len(metadata) > 0 && sessionBead.Metadata != nil {
		sessionBead.Metadata = cloneStringMap(sessionBead.Metadata)
	}
	if sessionBead.Metadata == nil {
		sessionBead.Metadata = map[string]string{}
	}
	for key, value := range metadata {
		sessionBead.Metadata[key] = value
	}
	if title != nil {
		sessionBead.Title = *title
	}
	if len(removeLabels) > 0 || len(addLabels) > 0 {
		remove := make(map[string]bool, len(removeLabels))
		for _, label := range removeLabels {
			remove[label] = true
		}
		filtered := make([]string, 0, len(sessionBead.Labels)+len(addLabels))
		for _, label := range sessionBead.Labels {
			if !remove[label] {
				filtered = append(filtered, label)
			}
		}
		sessionBead.Labels = filtered
	}
	for _, label := range addLabels {
		if !containsString(sessionBead.Labels, label) {
			sessionBead.Labels = append(sessionBead.Labels, label)
		}
	}
	return sessionBead, nil
}

func staleNonExpandingPoolSessionBead(cfgAgent *config.Agent, sessionBead beads.Bead) bool {
	if !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return false
	}
	if isManualSessionBeadForAgent(sessionBead, cfgAgent) {
		return false
	}
	if nonExpandingPoolIdentitySlot(cfgAgent, sessionBeadAgentName(sessionBead)) > 0 {
		return true
	}
	if nonExpandingPoolIdentitySlot(cfgAgent, sessionBead.Metadata["alias"]) > 0 {
		return true
	}
	if nonExpandingPoolIdentitySlot(cfgAgent, sessionBead.Title) > 0 {
		return true
	}
	for _, label := range sessionBead.Labels {
		label = strings.TrimSpace(label)
		if strings.HasPrefix(label, "agent:") && nonExpandingPoolIdentitySlot(cfgAgent, strings.TrimPrefix(label, "agent:")) > 0 {
			return true
		}
	}
	return strings.TrimSpace(sessionBead.Metadata["pool_slot"]) != ""
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

func resolveTemplateForSessionBead(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	qualifiedName string,
	fpExtra map[string]string,
	sessionBead beads.Bead,
) (TemplateParams, error) {
	local := *bp
	local.beadNames = map[string]string{qualifiedName: sessionBead.Metadata["session_name"]}
	tp, err := resolveTemplatePrepared(&local, cfgAgent, qualifiedName, fpExtra)
	if err != nil {
		return tp, err
	}
	if triggerID := strings.TrimSpace(sessionBead.Metadata[beadmeta.TriggerBeadIDMetadataKey]); triggerID != "" {
		if tp.Env == nil {
			tp.Env = make(map[string]string)
		}
		tp.Env["GC_TRIGGER_BEAD_ID"] = triggerID
		tp.Env["GC_TRIGGER_WORK_BEAD_ID"] = triggerID
		if storeRef := strings.TrimSpace(sessionBead.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]); storeRef != "" {
			tp.Env["GC_TRIGGER_BEAD_STORE_REF"] = storeRef
			tp.Env["GC_TRIGGER_WORK_STORE_REF"] = storeRef
		}
		if pack := strings.TrimSpace(sessionBead.Metadata[beadmeta.PackMetadataKey]); pack != "" {
			tp.Env["GC_PACKER_PACK"] = pack
		}
	}
	return tp, nil
}

// canonicalSessionIdentity returns the agent and qualified name to use when
// resolving a pool-managed session bead through resolveTemplate /
// resolveTemplateForSessionBead. Scoped to the pool case on purpose:
// realizePoolDesiredSessions uses a deep-copied instance agent +
// qualifiedInstance, and this helper is what makes the other pool-backed
// paths (rediscovery, store-backed dependency-floor) agree. GC_ALIAS and
// FingerprintExtra are part of CoreFingerprint, so divergent shapes across
// ticks trip the reconciler's config-drift drain.
//
// Named beads are deliberately NOT canonicalized here. The named-session
// TemplateParams contract (ConfiguredNamedIdentity/Mode, GC_SESSION_ORIGIN,
// canonical session_name, ...) is authored by the main named-session loop
// and reconstructNamedSessionTemplateParams; rewriting only the (agent,
// qualifiedName) pair in rediscovery while leaving the rest of the shape
// as plain ephemeral would produce a partially-named TemplateParams that
// downstream consumers don't expect. The Env-side drift that named beads
// can still exhibit across rediscovery vs. the named-session loop is a
// separate fix — the accompanying PR explicitly scopes it out.
//
// Rules:
//   - Named bead → (cfgAgent, cfgAgent.QualifiedName()). Identical to the
//     pre-change rediscovery shape so named-bead handling is unchanged.
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

func claimPoolSlotWithConfig(cfg *config.City, cfgAgent *config.Agent, sessionBead beads.Bead, used map[int]bool) int {
	if slot := existingPoolSlotWithConfig(cfg, cfgAgent, sessionBead); slot > 0 {
		if used[slot] {
			return 0
		}
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
	preferred *beads.Bead,
	used map[string]bool,
	usedSlots map[int]bool,
) (beads.Bead, int, error) {
	bead, slot, plan, err := selectOrPlanPoolSessionBead(bp, cfgAgent, template, preferred, SessionRequest{}, used, usedSlots)
	if err != nil {
		return beads.Bead{}, 0, err
	}
	if plan == nil {
		return bead, slot, nil
	}
	bead, err = executePlannedPoolSessionBeadCreate(bp, cfgAgent, template, *plan)
	if err != nil {
		delete(usedSlots, plan.slot)
		return bead, 0, err
	}
	return bead, plan.poolSlot, nil
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
	preferred *beads.Bead,
	request SessionRequest,
	used map[string]bool,
	usedSlots map[int]bool,
) (beads.Bead, int, *poolSessionCreatePlan, error) {
	if cfgAgent == nil {
		cfgAgent = findAgentByTemplate(&config.City{Agents: bp.agents}, template)
	}
	if cfgAgent == nil {
		return beads.Bead{}, 0, nil, fmt.Errorf("pool template %q has no configured agent", template)
	}
	// Resume tier: reuse the session that has in-progress work assigned.
	if preferred != nil && preferred.ID != "" && !used[preferred.ID] && !isFailedCreateSessionBead(*preferred) {
		slot := claimDesiredPoolSlot(bp.city, cfgAgent, *preferred, usedSlots)
		if slot == 0 && !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
			return beads.Bead{}, 0, nil, fmt.Errorf("pool session %s concrete slot already claimed", preferred.ID)
		}
		if isManualSessionBeadForAgent(*preferred, cfgAgent) {
			return *preferred, slot, nil, nil
		}
		bead, err := normalizeNonExpandingPoolSessionBeadForSelection(bp, cfgAgent, *preferred)
		return bead, slot, nil, err
	}
	if canonical, ok := findReusableCanonicalNonExpandingPoolSessionBead(bp, cfgAgent, template, used); ok {
		slot := claimDesiredPoolSlot(bp.city, cfgAgent, canonical, usedSlots)
		bead, err := normalizeNonExpandingPoolSessionBeadForSelection(bp, cfgAgent, canonical)
		return bead, slot, nil, err
	}
	// Reuse an existing active/creating session bead. Skip drained, closed,
	// and asleep — asleep ephemerals are not restarted; a fresh session is
	// created instead. The reconciler closes orphaned asleep beads.
	for _, bead := range reusablePoolSessionBeads(bp, cfgAgent, template, used) {
		if desiredName := strings.TrimSpace(bead.Metadata["session_name"]); desiredName != "" {
			slot := claimDesiredPoolSlot(bp.city, cfgAgent, bead, usedSlots)
			if slot == 0 && !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
				continue
			}
			bead, err := normalizeNonExpandingPoolSessionBeadForSelection(bp, cfgAgent, bead)
			return bead, slot, nil, err
		}
	}
	slot := claimDesiredPoolSlot(bp.city, cfgAgent, beads.Bead{}, usedSlots)
	_, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(cfgAgent, slot)
	metadata := poolTriggerMetadata(bp, cfgAgent, qualifiedInstance, request)

	if bp.poolScaleCheckPartialTemplates[template] {
		delete(usedSlots, slot)
		return beads.Bead{}, 0, nil, errPoolSessionCreatePartial
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
		return beads.Bead{}, 0, nil, errPoolSessionCreateProviderRed
	}

	if !bp.tryClaimPoolSessionCreate(template) {
		delete(usedSlots, slot)
		return beads.Bead{}, 0, nil, errPoolSessionCreateBudgetExhausted
	}

	plan := &poolSessionCreatePlan{
		qualifiedInstance: qualifiedInstance,
		slot:              slot,
		poolSlot:          poolSlot,
		metadata:          metadata,
	}
	return beads.Bead{}, 0, plan, nil
}

func poolTriggerMetadata(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, request SessionRequest) map[string]string {
	workID := strings.TrimSpace(request.WorkBeadID)
	if workID == "" {
		return nil
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
	if workDir := poolTriggerWorkDir(bp, cfgAgent, qualifiedName, request); workDir != "" {
		metadata[beadmeta.WorkDirMetadataKey] = workDir
		metadata[beadmeta.LegacyWorkDirMetadataKey] = workDir
	}
	return metadata
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
) (beads.Bead, error) {
	bead, err := createPoolSessionBeadWithGuardedAlias(bp, cfgAgent, template, plan.qualifiedInstance, plan.slot, plan.metadata)
	if err != nil {
		bp.releasePoolSessionCreate()
	}
	return bead, err
}

func claimDesiredPoolSlot(cfg *config.City, cfgAgent *config.Agent, sessionBead beads.Bead, used map[int]bool) int {
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return 0
	}
	return claimPoolSlotWithConfig(cfg, cfgAgent, sessionBead, used)
}

func reusablePoolSessionBead(bp *agentBuildParams, cfgAgent *config.Agent, template string, bead beads.Bead, used map[string]bool) bool {
	if bp == nil {
		return false
	}
	if bead.Status == "closed" {
		return false
	}
	if isDrainedSessionBead(bead) {
		return false
	}
	if isFailedCreateSessionBead(bead) {
		return false
	}
	if bead.Metadata["state"] == "asleep" {
		return false
	}
	if isManualSessionBeadForAgent(bead, cfgAgent) {
		return false
	}
	if isNamedSessionBead(bead) {
		return false
	}
	if sessionBeadHasAssignedWork(bp.assignedWorkBeads, bead) {
		return false
	}
	if used != nil && used[bead.ID] {
		return false
	}
	return resolvedSessionTemplate(bead, reuseTemplateConfig(bp)) == template
}

func reusablePoolSessionBeads(bp *agentBuildParams, cfgAgent *config.Agent, template string, used map[string]bool) []beads.Bead {
	if bp == nil || bp.sessionBeads == nil {
		return nil
	}
	candidates := []beads.Bead{}
	for _, bead := range bp.sessionBeads.Open() {
		if reusablePoolSessionBead(bp, cfgAgent, template, bead, used) {
			candidates = append(candidates, bead)
		}
	}
	sortSessionBeadsByCreatedAtThenID(candidates)
	return candidates
}

func sortSessionBeadsByCreatedAtThenID(candidates []beads.Bead) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
		}
		return candidates[i].ID < candidates[j].ID
	})
}

func findReusableCanonicalNonExpandingPoolSessionBead(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	used map[string]bool,
) (beads.Bead, bool) {
	if bp == nil || bp.sessionBeads == nil || !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return beads.Bead{}, false
	}
	canonical := cfgAgent.QualifiedName()
	for _, bead := range reusablePoolSessionBeads(bp, cfgAgent, template, used) {
		if strings.TrimSpace(bead.Metadata["session_name"]) == "" {
			continue
		}
		if staleNonExpandingPoolSessionBead(cfgAgent, bead) {
			continue
		}
		if beadIdentifiesAsCanonical(bead, canonical) {
			return bead, true
		}
	}
	return beads.Bead{}, false
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

func normalizeNonExpandingPoolSessionBeadForSelection(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	sessionBead beads.Bead,
) (beads.Bead, error) {
	bead, err := normalizeNonExpandingPoolSessionBead(bp, cfgAgent, sessionBead)
	if err == nil {
		return bead, nil
	}
	if !cfgAgent.UsesCanonicalSingletonPoolIdentity() || !errors.Is(err, session.ErrSessionAliasExists) {
		return bead, err
	}
	if bp != nil && bp.stderr != nil {
		fmt.Fprintf(bp.stderr, "buildDesiredState: pool %q: deferring singleton pool identity normalization for bead %s: %v\n", cfgAgent.QualifiedName(), sessionBead.ID, err) //nolint:errcheck
	}
	return recordDeferredNonExpandingPoolAliasConflict(bp, cfgAgent, sessionBead)
}

func recordDeferredNonExpandingPoolAliasConflict(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	sessionBead beads.Bead,
) (beads.Bead, error) {
	// The store write is authoritative; callers must use the returned bead
	// rather than re-reading bp.sessionBeads for this ID in the same tick.
	canonical := cfgAgent.QualifiedName()
	count := 0
	if existing, err := strconv.Atoi(strings.TrimSpace(sessionBead.Metadata[poolAliasConflictCountMetadataKey])); err == nil && existing > 0 {
		count = existing
	}
	metadata := session.UpdatedAliasMetadata(sessionBead.Metadata, "")
	metadata[poolAliasConflictMetadataKey] = canonical
	metadata[poolAliasConflictCountMetadataKey] = strconv.Itoa(count + 1)
	metadata[poolAliasConflictAtMetadataKey] = time.Now().UTC().Format(time.RFC3339)
	if bp != nil && bp.beadStore != nil && sessionBead.ID != "" {
		if err := bp.beadStore.Update(sessionBead.ID, beads.UpdateOpts{Metadata: metadata}); err != nil {
			return sessionBead, fmt.Errorf("recording deferred singleton pool alias conflict for bead %s: %w", sessionBead.ID, err)
		}
	}
	sessionBead.Metadata = cloneStringMap(sessionBead.Metadata)
	if sessionBead.Metadata == nil {
		sessionBead.Metadata = map[string]string{}
	}
	for key, value := range metadata {
		sessionBead.Metadata[key] = value
	}
	return sessionBead, nil
}

func queueClearPoolAliasConflictMetadata(metadata, existing map[string]string) {
	if existing == nil {
		return
	}
	for _, key := range []string{
		poolAliasConflictMetadataKey,
		poolAliasConflictCountMetadataKey,
		poolAliasConflictAtMetadataKey,
	} {
		if existing[key] != "" {
			metadata[key] = ""
		}
	}
}

func createPoolSessionBeadWithGuardedAlias(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	qualifiedInstance string,
	slot int,
	metadata map[string]string,
) (beads.Bead, error) {
	if bp == nil {
		return beads.Bead{}, fmt.Errorf("creating pool session for %q: build params unavailable", template)
	}
	if err := validateAgentSessionTransportForBuild(bp, cfgAgent, qualifiedInstance); err != nil {
		return beads.Bead{}, err
	}
	resolvedTmuxAlias, err := bp.resolveTmuxAliasForAgent(cfgAgent)
	if err != nil {
		return beads.Bead{}, err
	}
	resolvedTmuxAlias, err = validateResolvedPoolTmuxAlias(template, resolvedTmuxAlias)
	if err != nil {
		return beads.Bead{}, err
	}
	identity := poolSessionCreateIdentity{
		AgentName: qualifiedInstance,
		Slot:      slot,
		Metadata:  metadata,
	}
	alias := strings.TrimSpace(qualifiedInstance)
	if bp.beadStore == nil {
		return createPoolSessionBeadWithAlias(bp.beadStore, template, bp.city, bp.sessionBeads, poolSessionCreateStartedAt(bp), identity, resolvedTmuxAlias)
	}
	lockIDs := []string{}
	if alias != "" {
		lockIDs = append(lockIDs, alias)
	}
	if resolvedTmuxAlias != "" {
		lockIDs = append(lockIDs, resolvedTmuxAlias)
	}
	if len(lockIDs) == 0 {
		return createPoolSessionBeadWithAlias(bp.beadStore, template, bp.city, bp.sessionBeads, poolSessionCreateStartedAt(bp), identity, resolvedTmuxAlias)
	}

	var bead beads.Bead
	createdWithLock := false
	lockErr := session.WithCitySessionIdentifierLocks(bp.cityPath, lockIDs, func() error {
		createIdentity := identity
		if alias != "" {
			if err := session.EnsureAliasAvailableWithConfig(bp.beadStore, bp.city, alias, ""); err == nil {
				createIdentity.Alias = alias
			}
		}
		var err error
		bead, err = createPoolSessionBeadWithAlias(bp.beadStore, template, bp.city, bp.sessionBeads, poolSessionCreateStartedAt(bp), createIdentity, resolvedTmuxAlias)
		createdWithLock = true
		return err
	})
	if createdWithLock {
		return bead, lockErr
	}
	if lockErr != nil && bp.stderr != nil {
		fmt.Fprintf(bp.stderr, "createPoolSessionBeadWithGuardedAlias: locking alias %q for %s: %v; creating without alias\n", alias, template, lockErr) //nolint:errcheck
	}
	return createPoolSessionBeadWithAlias(bp.beadStore, template, bp.city, bp.sessionBeads, poolSessionCreateStartedAt(bp), identity, "")
}

func isFailedCreateSessionBead(bead beads.Bead) bool {
	return strings.TrimSpace(bead.Metadata["state"]) == string(session.StateFailedCreate)
}

func sessionBeadHasAssignedWork(workBeads []beads.Bead, sessionBead beads.Bead) bool {
	for _, wb := range workBeads {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" || (wb.Status != "open" && wb.Status != "in_progress") {
			continue
		}
		if assignee == sessionBead.ID || assignee == strings.TrimSpace(sessionBead.Metadata["session_name"]) {
			return true
		}
		if namedIdentity := strings.TrimSpace(sessionBead.Metadata["configured_named_identity"]); namedIdentity != "" && assignee == namedIdentity {
			return true
		}
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
	bead      beads.Bead
	ambiguous bool
}

// buildSessionAssigneeIndex maps every assignment identity an open session can
// be claimed under to that session, computed once per reconcile. Open() copies
// the session slice, so resolving per work bead would otherwise cost
// O(workBeads × openSessions). Identities come from sessionBeadAssigneeIdentities
// — bead ID, session_name, configured named identity, current alias, AND prior
// aliases (alias_history) — so a bead assigned under a since-rotated pool alias
// still resolves. An identity claimed by two different sessions is marked
// ambiguous.
func buildSessionAssigneeIndex(sessionBeads *sessionBeadSnapshot) map[string]sessionAssigneeMatch {
	index := make(map[string]sessionAssigneeMatch)
	if sessionBeads == nil {
		return index
	}
	for _, sb := range sessionBeads.Open() {
		for _, identity := range sessionBeadAssigneeIdentities(sb) {
			if existing, ok := index[identity]; ok {
				if !existing.ambiguous && existing.bead.ID != sb.ID {
					index[identity] = sessionAssigneeMatch{ambiguous: true}
				}
				continue
			}
			index[identity] = sessionAssigneeMatch{bead: sb}
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
		sb := match.bead
		sessionName := sessionBeadIdentifier(sb)
		workDir := strings.TrimSpace(sb.Metadata["work_dir"])
		if sessionName == "" && workDir == "" {
			continue
		}
		patch := map[string]string{}
		if sessionName != "" && strings.TrimSpace(wb.Metadata[beadmeta.SessionNameMetadataKey]) != sessionName {
			patch[beadmeta.SessionNameMetadataKey] = sessionName
		}
		if workDir != "" && strings.TrimSpace(wb.Metadata[beadmeta.WorkDirMetadataKey]) != workDir {
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
		stampRunRootFromStep(store, wb, sessionName, workDir, stampedRoots, stderr)
	}
}

// stampRunRootFromStep copies a step's resolved session_name/work_dir onto its
// workflow root (gc.root_bead_id), once per root per pass. Idempotent: it reads
// the root and writes only the keys that differ. Best-effort — a root that is
// in another store, already gone, or already stamped is silently skipped (a
// cross-store root gets stamped on its own store's reconcile pass).
func stampRunRootFromStep(store beads.Store, step beads.Bead, sessionName, workDir string, stampedRoots map[string]struct{}, stderr io.Writer) {
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
	if workDir != "" && strings.TrimSpace(root.Metadata[beadmeta.WorkDirMetadataKey]) != workDir {
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
// actionable pool work that is pre-assigned to a legacy bound form of a
// configured unbound pool agent (e.g. "dir/binding.name") to that agent's
// current canonical identity ("dir/name").
//
// Why: after a bound→unbound agent migration, the awake/scale accounting wakes
// a canonical pool session for work persisted under the legacy bound identity
// (it normalizes template identities), but the woken session's work_query and
// `gc hook --claim` path match assignees and routes by raw string. A canonical
// session can derive neither the old binding name nor the legacy assignee, so
// the triggering bead would surface to no one and stay unclaimed. Re-homing the
// persisted identity to canonical makes the bead behave exactly like ordinary
// canonical pool work, which the existing surface/claim machinery already
// resumes — closing the agent-side half of the migration recovery loop.
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
// unassigned pool work that is still routed to the legacy bound form of a
// now-unbound pool agent ("dir/binding.name") onto the agent's current canonical
// identity ("dir/name").
//
// This closes the demand/claim half of the bound→unbound migration that the
// assignee-keyed canonicalizeLegacyBoundAssignedWork cannot reach: open work with
// an empty assignee never enters the assigned-work collection, and the canonical
// pool-demand probe (EffectivePoolDemandQuery), the worker work_query
// (EffectiveWorkQuery Tier 3), and the claim predicate (hookClaimMatchesRoute)
// all match gc.routed_to against the canonical target by raw string. A bead still
// routed to "dir/binding.name" is therefore invisible to the canonical "dir/name"
// pool — it neither contributes scale demand nor can be claimed — so migration-era
// ready work stays stuck until its route is canonicalized. Rewriting the route in
// place lets every existing canonical-only path surface it, keeping the legacy
// awareness confined to this migration pass instead of spread across the demand,
// work_query, and claim predicates.
//
// Unlike the assignee re-home, this needs no session-snapshot guard: open
// unassigned work has no live owner to strand, so rewriting its route can only
// make it discoverable. Idempotent by design: it writes only when the canonical
// identity differs from the persisted route, so steady-state reconciles perform
// no writes, and a route that is already canonical, resolves to no configured
// agent, or still matches a configured bound agent is left untouched. A write
// failure is logged and skipped — recovery is best-effort and must never block
// reconciliation.
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
		// Cheap pre-filter: a legacy bound form is "dir/binding.name", so only a
		// route whose local segment carries the binding-separator dot can be one.
		// Canonical unbound routes ("dir/name") skip the per-bead agent scan in
		// normalizeAgentTemplateIdentity, keeping the steady-state cost off the
		// full open-routed backlog.
		if _, local := config.ParseQualifiedName(routedTo); !strings.Contains(local, ".") {
			continue
		}
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
// the city store and every non-suspended rig store, index-aligned with the store
// that owns each bead. It is the input collection for
// canonicalizeLegacyBoundUnassignedRoutedWork: empty-assignee open work is dropped
// by the assignee-keyed collectAssignedWorkBeadsWithStores passes, so the
// migration re-home needs its own scan. Active-only List queries are served from
// the CachingStore in steady state, so this adds no backing-store round trip.
func collectOpenUnassignedRoutedWork(cfg *config.City, store beads.Store, rigStores map[string]beads.Store, suspendedRigPaths map[string]bool, stderr io.Writer) ([]beads.Bead, []beads.Store) {
	if cfg == nil {
		return nil, nil
	}
	// Work arm (unassigned-routed re-home scan): iterate the work-class
	// candidate fan-out, labeling the city store "city" for the diagnostic
	// ref. Identity on a single-store city.
	stores := coordClassStoreCandidates(cfg, store, rigStores, suspendedRigPaths, "city")

	var workBeads []beads.Bead
	var workStores []beads.Store
	seen := make(map[string]struct{})
	for _, source := range stores {
		if source.store == nil {
			continue
		}
		open, err := listBothTiersForControllerDemand(source.store, beads.ListQuery{Status: "open"})
		if err != nil && !beads.IsPartialResult(err) {
			fmt.Fprintf(stderr, "collectOpenUnassignedRoutedWork: %s: List(open): %v\n", source.ref, err) //nolint:errcheck
			continue
		}
		for _, b := range open {
			if b.Type == sessionBeadType || strings.TrimSpace(b.Assignee) != "" {
				continue
			}
			if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) == "" {
				continue
			}
			if _, ok := seen[b.ID]; ok {
				continue
			}
			seen[b.ID] = struct{}{}
			workBeads = append(workBeads, b)
			workStores = append(workStores, source.store)
		}
	}
	return workBeads, workStores
}

func selectOrCreateDependencyPoolSessionBead(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
) (beads.Bead, error) {
	if cfgAgent == nil {
		cfgAgent = findAgentByTemplate(&config.City{Agents: bp.agents}, template)
	}
	if cfgAgent == nil {
		return beads.Bead{}, fmt.Errorf("dependency pool template %q has no configured agent", template)
	}
	if canonical, ok := findReusableCanonicalNonExpandingDependencyPoolSessionBead(bp, cfgAgent, template); ok {
		return normalizeNonExpandingPoolSessionBeadForSelection(bp, cfgAgent, canonical)
	}
	for _, bead := range reusableDependencyPoolSessionBeads(bp, template) {
		return normalizeNonExpandingPoolSessionBeadForSelection(bp, cfgAgent, bead)
	}
	_, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(cfgAgent, 1)
	// Dependency floors are bounded prerequisites for already-realized roots,
	// so they bypass the ordinary fresh pool create budget. The wake budget
	// still caps when those floor sessions can actually start.
	return createPoolSessionBeadWithGuardedAlias(bp, cfgAgent, template, qualifiedInstance, poolSlot, nil)
}

func reusableDependencyPoolSessionBeads(bp *agentBuildParams, template string) []beads.Bead {
	if bp == nil || bp.sessionBeads == nil {
		return nil
	}
	candidates := []beads.Bead{}
	for _, bead := range bp.sessionBeads.Open() {
		if reusableDependencyPoolSessionBead(bp, template, bead) {
			candidates = append(candidates, bead)
		}
	}
	sortSessionBeadsByCreatedAtThenID(candidates)
	return candidates
}

func reusableDependencyPoolSessionBead(bp *agentBuildParams, template string, bead beads.Bead) bool {
	if bp == nil {
		return false
	}
	if bead.Status == "closed" || isManualSessionBead(bead) {
		return false
	}
	if isDrainedSessionBead(bead) {
		return false
	}
	if isFailedCreateSessionBead(bead) {
		return false
	}
	if isNamedSessionBead(bead) {
		return false
	}
	if bead.Metadata["dependency_only"] != boolMetadata(true) {
		return false
	}
	if resolvedSessionTemplate(bead, reuseTemplateConfig(bp)) != template {
		return false
	}
	return strings.TrimSpace(bead.Metadata["session_name"]) != ""
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

func findReusableCanonicalNonExpandingDependencyPoolSessionBead(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
) (beads.Bead, bool) {
	if bp == nil || bp.sessionBeads == nil || !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return beads.Bead{}, false
	}
	canonical := cfgAgent.QualifiedName()
	for _, bead := range reusableDependencyPoolSessionBeads(bp, template) {
		if staleNonExpandingPoolSessionBead(cfgAgent, bead) {
			continue
		}
		if beadIdentifiesAsCanonical(bead, canonical) {
			return bead, true
		}
	}
	return beads.Bead{}, false
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
	for _, od := range packDirs {
		if err := runtime.StageProviderOverlayDir(od, workDir, overlayProviders, stderr); err != nil {
			fmt.Fprintf(stderr, "agent %q: pack overlay %q: %v\n", qualifiedName, od, err) //nolint:errcheck
		}
	}
	if overlayDir != "" {
		if err := runtime.StageProviderOverlayDir(overlayDir, workDir, overlayProviders, stderr); err != nil {
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
