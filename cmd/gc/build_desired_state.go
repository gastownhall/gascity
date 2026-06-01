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

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

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

var errPoolSessionCreateBudgetExhausted = errors.New("pool session create budget exhausted")

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
	}
	var demands []demand
	for _, state := range states {
		count := 0
		for _, request := range state.Requests {
			// Requests with a session bead ID represent in-flight capacity and
			// should not reserve fresh-create budget for this template.
			if request.Tier == "new" && request.SessionBeadID == "" {
				count++
			}
		}
		if count > 0 {
			demands = append(demands, demand{template: state.Template, count: count})
		}
	}
	if len(demands) <= 1 {
		return nil, 0
	}
	shares := make(map[string]int, len(demands))
	start := int(seed % uint64(len(demands)))
	remaining := limit
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
	if cfg.Workspace.Suspended {
		return DesiredStateResult{}
	}

	bp := newAgentBuildParams(cityName, cityPath, cfg, sp, beaconTime, store, stderr)
	bp.sessionBeads = sessionBeads

	// Pre-compute suspended rig paths.
	suspendedRigPaths := buildSuspendedRigPaths(cfg)

	desired := make(map[string]TemplateParams)
	var pendingPools []poolEvalWork
	var defaultScaleTargets []defaultScaleCheckTarget
	var defaultNamedScaleTargets []defaultScaleCheckTarget

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
		// Expand {{.Rig}}/{{.AgentBase}} before the scale_check enters the
		// controller probe pool so rig-scoped agents query their own rig.
		sp.Check = expandAgentCommandTemplate(cityPath, cityName, &cfg.Agents[i], cfg.Rigs, "scale_check", sp.Check, stderr)

		if !cfg.Agents[i].SupportsGenericEphemeralSessions() {
			continue
		}
		if backsNamedSession {
			rigName := configuredRigName(cityPath, &cfg.Agents[i], cfg.Rigs)
			if rigName != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rigName, cfg.Rigs))] {
				continue
			}
			// Named-session materialization is handled in the named-session pass,
			// but explicit scale_check/min demand for the backing template still
			// creates ephemeral capacity through the pool pipeline. The implicit
			// routed-work scale_check feeds named demand separately so it does
			// not create a parallel generic worker for the same backing template.
			poolDir := agentCommandDir(cityPath, &cfg.Agents[i], cfg.Rigs)
			if store != nil && strings.TrimSpace(cfg.Agents[i].ScaleCheck) == "" {
				defaultNamedScaleTargets = append(defaultNamedScaleTargets, defaultScaleCheckTargetForAgent(cityPath, cfg, &cfg.Agents[i], store, rigStores))
				continue
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
		if store != nil && strings.TrimSpace(cfg.Agents[i].ScaleCheck) == "" {
			defaultScaleTargets = append(defaultScaleTargets, defaultScaleCheckTargetForAgent(cityPath, cfg, &cfg.Agents[i], store, rigStores))
			continue
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
	var storePartial bool
	var scaleCheckCounts map[string]int
	var poolScaleCheckPartialTemplates map[string]bool
	var namedScaleCheckPartialTemplates map[string]bool
	var scaleCheckPartialTemplates map[string]bool
	var namedDefaultDemand map[string]bool
	if store != nil {
		assignedWorkBeads, assignedWorkStores, assignedWorkStoreRefs, storePartial = collectAssignedWorkBeadsWithStores(cfg, store, rigStores, suspendedRigPaths, sessionBeads)
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
		// Durably record which session is executing each in-progress work
		// bead. The Assignee link is transient (cleared on close), so without
		// this a completed run carries no session/worktree reference. See
		// stampRunSessionIdentity. Unlike drain decisions, this is not gated on
		// storePartial: stamping the beads that WERE collected is always
		// correct, and any bead missed by a partial query simply gets stamped
		// on a later tick.
		stampRunSessionIdentity(assignedWorkBeads, assignedWorkStores, sessionBeads, stderr)
		scaleCheckCounts, poolScaleCheckPartialTemplates = evaluatePendingPoolsMap(cfg, pendingPools, stderr, trace)
		if len(defaultScaleTargets) > 0 {
			defaultCounts, partialTemplates, errs := defaultScaleCheckCounts(defaultScaleTargets)
			for _, err := range errs {
				// defaultScaleCheckCounts can fail on either of two
				// demand sources (Ready iteration or pool-demand list);
				// the wrapped error message names which one ("Ready()"
				// vs "List(gc.pool_demand)") so this generic outer log
				// stays honest about the partial nature without
				// claiming the demand is necessarily zero. A failing
				// pool-demand list does not zero the Ready-source
				// contributions to scaleCheckCounts[template].
				fmt.Fprintf(stderr, "buildDesiredState: %v (counts above may be a partial of one demand source)\n", err) //nolint:errcheck
			}
			poolScaleCheckPartialTemplates = mergeScaleCheckPartialTemplates(poolScaleCheckPartialTemplates, partialTemplates)
			for template, count := range defaultCounts {
				scaleCheckCounts[template] = count
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
		poolDesiredStates := ComputePoolDesiredStatesTraced(cfg, poolWorkBeads, sessionBeads.Open(), scaleCheckCounts, trace)
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
			if wb.Status != "open" && wb.Status != "in_progress" {
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
		NamedSessionDemand:              namedWorkReady,
		StoreQueryPartial:               storePartial,
		BeaconTime:                      beaconTime,
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
	applySessionBeadDesiredOverlay(bp, cfg, refreshed.State, buildSuspendedRigPaths(cfg), result.PoolScaleCheckPartialTemplates, result.NamedScaleCheckPartialTemplates, stderr)
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
	result, _, _, partial := collectAssignedWorkBeadsWithStores(cfg, cityStore, nil, nil, nil)
	return result, partial
}

func collectAssignedWorkBeadsWithStores(
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	sessionBeads *sessionBeadSnapshot,
) ([]beads.Bead, []beads.Store, []string, bool) {
	// Use CachingStore-wrapped stores. Creating raw bdStoreForCity per rig
	// spawns bd subprocesses on every tick, saturating dolt.
	type workStore struct {
		store beads.Store
		ref   string
	}
	stores := []workStore{{store: cityStore}}
	for _, rig := range cfg.Rigs {
		if suspendedRigPaths[filepath.Clean(rig.Path)] {
			continue
		}
		if s, ok := rigStores[rig.Name]; ok {
			stores = append(stores, workStore{store: s, ref: rig.Name})
		}
	}

	type storeAssignedWorkResult struct {
		beads     []beads.Bead
		stores    []beads.Store
		storeRefs []string
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
			seen := make(map[string]struct{})
			// In-progress beads with an assignee (active work), plus stranded
			// unassigned pool work that needs to be reopened. This pass runs
			// across every store before any ready handoff probes, so already
			// active work never waits behind unrelated ready scans.
			if inProgress, err := listBothTiersForControllerDemand(source.store, beads.ListQuery{Status: "in_progress"}); err == nil {
				appendInProgressWorkUnique(cfg, &result, &resultStores, &resultStoreRefs, inProgress, seen, source.store, source.ref)
			} else {
				errs = append(errs, fmt.Errorf("List(in_progress): %w", err))
				if beads.IsPartialResult(err) && len(inProgress) > 0 {
					appendInProgressWorkUnique(cfg, &result, &resultStores, &resultStoreRefs, inProgress, seen, source.store, source.ref)
				}
			}
			results[idx] = storeAssignedWorkResult{beads: result, stores: resultStores, storeRefs: resultStoreRefs, errs: errs}
		}()
	}
	wg.Wait()

	var result []beads.Bead
	var resultStores []beads.Store
	var resultStoreRefs []string
	var partial bool
	for _, r := range results {
		result = append(result, r.beads...)
		resultStores = append(resultStores, r.stores...)
		resultStoreRefs = append(resultStoreRefs, r.storeRefs...)
		for _, err := range r.errs {
			log.Printf("collectAssignedWorkBeads: %v", err)
			partial = true
		}
	}
	skipReadyAssignees := assignedWorkAssigneeSet(result)
	expandSkipAssigneesWithSessionIdentities(skipReadyAssignees, sessionBeads)
	assignees := readyAssignedWorkAssignees(cfg, sessionBeads, skipReadyAssignees)
	if len(skipReadyAssignees) > 0 && len(assignees) == 0 {
		return result, resultStores, resultStoreRefs, partial
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
				ready, err = readyForControllerDemandQuery(source.store, beads.ReadyQuery{Limit: assignedWorkReadyLimit(cfg)})
				if err != nil {
					errs = append(errs, fmt.Errorf("Ready(): %w", err))
				}
			} else {
				for _, assignee := range assignees {
					part, partErr := readyForControllerDemandQuery(source.store, beads.ReadyQuery{Assignee: assignee, Limit: assignedWorkReadyLimit(cfg)})
					if partErr != nil {
						errs = append(errs, fmt.Errorf("Ready(assignee=%q): %w", assignee, partErr))
					}
					ready = append(ready, part...)
				}
			}
			var readyBeads []beads.Bead
			var readyStores []beads.Store
			var readyStoreRefs []string
			seen := make(map[string]struct{})
			appendAssignedUnique(&readyBeads, &readyStores, &readyStoreRefs, ready, seen, source.store, source.ref)
			readyResults[idx] = storeAssignedWorkResult{beads: readyBeads, stores: readyStores, storeRefs: readyStoreRefs, errs: errs}
		}()
	}
	wg.Wait()
	for _, r := range readyResults {
		result = append(result, r.beads...)
		resultStores = append(resultStores, r.stores...)
		resultStoreRefs = append(resultStoreRefs, r.storeRefs...)
		for _, err := range r.errs {
			log.Printf("collectAssignedWorkBeads: %v", err)
			partial = true
		}
	}
	return result, resultStores, resultStoreRefs, partial
}

func assignedWorkReadyLimit(cfg *config.City) int {
	if cfg == nil {
		return config.DefaultMaxWakesPerTick
	}
	return cfg.Daemon.MaxWakesPerTickOrDefault()
}

func assignedWorkAssigneeSet(work []beads.Bead) map[string]struct{} {
	if len(work) == 0 {
		return nil
	}
	result := make(map[string]struct{})
	for _, bead := range work {
		assignee := strings.TrimSpace(bead.Assignee)
		if assignee == "" {
			continue
		}
		if bead.Status != "open" && bead.Status != "in_progress" {
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
			add(cfg.NamedSessions[i].QualifiedName())
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
	counts := make(map[string]int, len(targets))
	if len(targets) == 0 {
		return counts, nil, nil
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
			group = &scaleStoreGroup{store: target.store, templates: make(map[string]struct{})}
			groups[key] = group
		}
		group.templates[template] = struct{}{}
	}

	for key, group := range groups {
		// counted dedups across the two demand sources below so a bead
		// surfaced by both Ready() and the gc.pool_demand list (rare —
		// only if a task-shaped routed bead also happens to carry the
		// flag) is counted exactly once per template.
		counted := make(map[string]struct{})

		// Source 1: Ready()/CachedReady() iteration. Surfaces the
		// actionable-type set (task, etc.) matched against canonical routing
		// metadata, with the temporary gc.run_target migration fallback.
		// Legacy formula step beads are NOT here because PR #1154 added
		// "step" to readyExcludeTypes; molecule wisps are NOT here
		// because workflow containers were already excluded.
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
			if _, dup := counted[b.ID]; dup {
				continue
			}
			counted[b.ID] = struct{}{}
			counts[template]++
		}

		// Source 2: explicit pool-demand path. Two writers stamp the wisp
		// when a.Pool != "" — doOrderRunWithJSON (cmd_order.go, the
		// gc order run CLI path) and memoryOrderDispatcher.dispatchOne
		// (order_dispatch.go, the supervisor's in-process cron path).
		// Both write poolDemandMetadataPair() alongside the routing key,
		// so cron-fired pool orders surface scale_check demand even when
		// the wisp lands as a molecule that readyExcludeTypes filters out
		// (per PR #1154 / issue #1039 — formula steps are not actionable
		// work, the molecule is the container). The list filter is
		// metadata-only (open + gc.pool_demand=<sentinel>); the
		// unassigned + matching-route checks apply below as for the
		// Ready source.
		//
		// Live: true skips the CachingStore in-memory snapshot and reads
		// the backing store directly. The cache populates from PrimeActive
		// at supervisor startup and is maintained by the event stream, but
		// gc order run is a sibling subprocess so the cache lag would
		// otherwise stretch demand observation by an unbounded number of
		// reconcile ticks. Mirrors openSessionBeadExists in
		// adoption_barrier.go, which uses Live: true for the same
		// cross-process freshness reason.
		demand, demandErr := group.store.List(beads.ListQuery{
			Status:   "open",
			Metadata: poolDemandMetadataPair(),
			Live:     true,
			TierMode: beads.TierBoth,
		})
		if demandErr != nil {
			errs = append(errs, fmt.Errorf("default scale_check %s templates=%s: List(%s): %w", key, strings.Join(sortedStringSet(group.templates), ","), poolDemandMetadataKey, demandErr))
			partialTemplates = markScaleCheckPartialSet(partialTemplates, group.templates)
			if !beads.IsPartialResult(demandErr) {
				demand = nil
			}
		}
		now := time.Now().UTC()
		for _, b := range demand {
			if strings.TrimSpace(b.Assignee) != "" {
				continue
			}
			if beads.IsDeferred(b, now) {
				continue
			}
			template := controllerDemandRouteTarget(b, group.templates)
			if _, ok := group.templates[template]; !ok {
				continue
			}
			if _, dup := counted[b.ID]; dup {
				continue
			}
			counted[b.ID] = struct{}{}
			counts[template]++
		}
	}
	return counts, partialTemplates, errs
}

func defaultNamedSessionDemand(targets []defaultScaleCheckTarget, cfg *config.City, cityName string) (map[string]bool, map[string]bool, []error) {
	demand := make(map[string]bool)
	if len(targets) == 0 || cfg == nil {
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

	namedByIdentity := make(map[string]namedSessionSpec)
	identitiesByTemplate := make(map[string][]string)
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(cfg, cityName, identity)
		if !ok || spec.Mode == "always" {
			continue
		}
		template := strings.TrimSpace(namedSessionBackingTemplate(spec))
		if template == "" {
			continue
		}
		namedByIdentity[spec.Identity] = spec
		identitiesByTemplate[template] = append(identitiesByTemplate[template], spec.Identity)
	}

	// NOTE: this loop intentionally only consults Ready(), not the
	// gc.pool_demand list path that defaultScaleCheckCounts uses for
	// pool agents. All current pack-shipped cron orders route to pool
	// agents (none target named on_demand sessions), so this function
	// is never the load-bearing demand source for cron-fired wisps in
	// practice. If a future named on_demand cron order surfaces — i.e.
	// a wisp lands with gc.routed_to=<named-identity> AND the molecule
	// type filters it out of Ready() — mirror the Source-2 List path
	// from defaultScaleCheckCounts here (query open + poolDemandMetadataPair()
	// from the same group.store, apply the unassigned + routed-to
	// match, dedup against the Ready source) and add a parallel test
	// next to TestDefaultScaleCheckCountsCountsCronPoolDemandViaMetadataFlag.
	for key, group := range groups {
		ready, err := readyForControllerDemand(group.store)
		if err != nil {
			errs = append(errs, fmt.Errorf("default scale_check %s templates=%s: Ready(): %w", key, strings.Join(sortedStringSet(group.templates), ","), err))
			partialTemplates = markScaleCheckPartialSet(partialTemplates, group.templates)
			if !beads.IsPartialResult(err) || len(ready) == 0 {
				continue
			}
		}
		for _, b := range ready {
			if strings.TrimSpace(b.Assignee) != "" {
				continue
			}
			routedTo := routedToOrLegacyWorkflowTarget(b)
			if routedTo == "" {
				continue
			}
			if spec, ok := namedByIdentity[routedTo]; ok {
				template := strings.TrimSpace(namedSessionBackingTemplate(spec))
				if _, targetTemplate := group.templates[template]; targetTemplate {
					demand[spec.Identity] = true
				}
				continue
			}
			if _, targetTemplate := group.templates[routedTo]; !targetTemplate {
				continue
			}
			identities := identitiesByTemplate[routedTo]
			if len(identities) == 1 {
				demand[identities[0]] = true
			}
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

func retainScaleCheckPartialPoolDesired(counts map[string]int, sessionBeads *sessionBeadSnapshot, partialTemplates map[string]bool) map[string]int {
	if len(partialTemplates) == 0 || sessionBeads == nil {
		return counts
	}
	retained := make(map[string]int)
	for _, b := range sessionBeads.Open() {
		template := strings.TrimSpace(b.Metadata["template"])
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
// failures, but do not count them as awake demand.
func scaleCheckPartialSessionPreservable(b beads.Bead) bool {
	switch strings.TrimSpace(b.Metadata["state"]) {
	case "", "active", "awake", "start-pending", "creating", "asleep", "stopped", "suspended", "quarantined", "draining", "drained", "archived":
		return true
	default:
		return isPendingPoolCreate(b)
	}
}

func scaleCheckPartialSessionRetainable(b beads.Bead) bool {
	switch strings.TrimSpace(b.Metadata["state"]) {
	case "active", "awake", "start-pending", "creating":
		return true
	default:
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
	handles := beads.HandlesFor(store)
	rows, err := handles.Cached.Ready()
	if errors.Is(err, beads.ErrCacheUnavailable) {
		return handles.Live.Ready()
	}
	return rows, err
}

func readyForControllerDemandQuery(store beads.Store, query beads.ReadyQuery) ([]beads.Bead, error) {
	handles := beads.HandlesFor(store)
	rows, err := handles.Cached.Ready(query)
	if errors.Is(err, beads.ErrCacheUnavailable) {
		return handles.Live.Ready(query)
	}
	return rows, err
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

func appendInProgressWorkUnique(cfg *config.City, dst *[]beads.Bead, stores *[]beads.Store, storeRefs *[]string, beadList []beads.Bead, seen map[string]struct{}, store beads.Store, storeRef string) {
	for _, b := range beadList {
		if strings.TrimSpace(b.Assignee) == "" && !isRecoverableUnassignedInProgressPoolWork(cfg, b) {
			continue
		}
		appendWorkUnique(dst, stores, storeRefs, b, seen, store, storeRef)
	}
}

func appendAssignedUnique(dst *[]beads.Bead, stores *[]beads.Store, storeRefs *[]string, beadList []beads.Bead, seen map[string]struct{}, store beads.Store, storeRef string) {
	for _, b := range beadList {
		if strings.TrimSpace(b.Assignee) == "" {
			continue
		}
		appendWorkUnique(dst, stores, storeRefs, b, seen, store, storeRef)
	}
}

func appendWorkUnique(dst *[]beads.Bead, stores *[]beads.Store, storeRefs *[]string, b beads.Bead, seen map[string]struct{}, store beads.Store, storeRef string) {
	// Invariant: dst, stores, and storeRefs are kept index-aligned by this
	// shared growth path and the shared seen guard.
	// Session beads are not actionable work — filter them at the source
	// so all consumers see only real tasks. Message beads are NOT filtered
	// here because they represent mail that should wake/materialize sessions;
	// idle nudge filters messages locally since mail nudging is handled
	// separately by the mail system.
	if b.Type == sessionBeadType {
		return
	}
	if _, ok := seen[b.ID]; ok {
		return
	}
	seen[b.ID] = struct{}{}
	*dst = append(*dst, b)
	if stores != nil {
		*stores = append(*stores, store)
	}
	if storeRefs != nil {
		*storeRefs = append(*storeRefs, storeRef)
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
			if controllerManagedPool && !manualSession && !isNamedSessionBead(b) &&
				!sessionAlreadyDesired && !templateDesired && !scaleCheckPartial {
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
			sessionBead, slot, plan, err := selectOrPlanPoolSessionBead(bp, cfgAgent, qualifiedName, prefer, used, usedSlots)
			if err != nil {
				if errors.Is(err, errPoolSessionCreateBudgetExhausted) {
					fmt.Fprintf(stderr, "buildDesiredState: pool %q request: %v (fresh create deferred)\n", qualifiedName, err) //nolint:errcheck
				} else {
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
	return resolveTemplatePrepared(&local, cfgAgent, qualifiedName, fpExtra)
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
}

func selectOrCreatePoolSessionBead(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	preferred *beads.Bead,
	used map[string]bool,
	usedSlots map[int]bool,
) (beads.Bead, int, error) {
	bead, slot, plan, err := selectOrPlanPoolSessionBead(bp, cfgAgent, template, preferred, used, usedSlots)
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
	if !bp.tryClaimPoolSessionCreate(template) {
		delete(usedSlots, slot)
		return beads.Bead{}, 0, nil, errPoolSessionCreateBudgetExhausted
	}
	plan := &poolSessionCreatePlan{
		qualifiedInstance: qualifiedInstance,
		slot:              slot,
		poolSlot:          poolSlot,
	}
	return beads.Bead{}, 0, plan, nil
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
	bead, err := createPoolSessionBeadWithGuardedAlias(bp, cfgAgent, template, plan.qualifiedInstance, plan.slot)
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
		patch := map[string]string{}
		if sessionName != "" && strings.TrimSpace(wb.Metadata["gc.session_name"]) != sessionName {
			patch["gc.session_name"] = sessionName
		}
		if workDir != "" && strings.TrimSpace(wb.Metadata["gc.work_dir"]) != workDir {
			patch["gc.work_dir"] = workDir
		}
		if len(patch) == 0 {
			continue
		}
		if err := store.SetMetadataBatch(wb.ID, patch); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "stampRunSessionIdentity: %s: %v\n", wb.ID, err) //nolint:errcheck
		}
	}
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
	return createPoolSessionBeadWithGuardedAlias(bp, cfgAgent, template, qualifiedInstance, poolSlot)
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
	overlayProviders := runtime.OverlayProviderNamesFromParts(
		resolvedProviderLaunchFamily(resolved),
		strings.TrimSpace(resolved.Name),
		installHooks,
	)
	for _, overlayDir := range effectiveOverlayDirs(bp.packOverlayDirs, bp.rigOverlayDirs, rigName) {
		if err := runtime.StageProviderOverlayDir(overlayDir, workDir, overlayProviders, stderr); err != nil {
			fmt.Fprintf(stderr, "agent %q: pack overlay %q: %v\n", qualifiedName, overlayDir, err) //nolint:errcheck
		}
	}
	if overlayDir := resolveOverlayDir(cfgAgent.OverlayDir, bp.cityPath); overlayDir != "" {
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
