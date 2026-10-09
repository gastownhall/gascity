package main

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The allocator's decide (P3 spec §4.4 steps 1-14): desire, create plans,
// bindings and canonical named rows for one pass, computed in memory from
// inputs the pass gathered. It reuses legacy's pure pool code (the demand
// merge, computePoolDesiredStatesAt, the plan-only planner, the overlay
// classification, computeAwakeSetKeyed) and never reads a store, a provider,
// the filesystem, the environment or the clock: every fact, the time
// included, comes in through allocInputs. TestDecideIsPure pins that.
//
// It lands in two slices: P3-5a1 is steps 1-3 and 9-13 (each row's class,
// the canonical named rows, the partial causes, the awake set,
// classification with its Keep conversions, and floors), and P3-5a2 steps
// 4-8 and 14 (demand, pool desired, realization, named planning, the overlay
// and bindings). Admission is not part of it: the planner admits the pass's
// intents after the decide (CONTRACT v5 P4). Unwired: the planner gathers
// the inputs.

// errDecideNoClock refuses a pass without its one clock: a zero Now would
// read every lease as expired and every fact as stale.
var errDecideNoClock = errors.New("allocator decide: zero Now")

// errDecideNoCensus refuses a pass without its census: no census is not an
// empty city.
var errDecideNoCensus = errors.New("allocator decide: no census")

// allocInputs is everything one pass decides from.
type allocInputs struct {
	// Now is the pass's one clock (POOL-026, #35). It must be set.
	Now       time.Time
	Epoch     string
	Cfg       *config.City
	ConfigRev string
	EnvGen    uint64
	CityPath  string
	CityName  string
	// CitySuspended is effectiveCitySuspended(cfg, I9), and
	// SuspendedRigPaths suspendedRigPathsWithState(cfg, I9): both read the
	// environment or a file, so the gather phase computes them.
	CitySuspended     bool
	SuspendedRigPaths map[string]bool
	Census            *sessionCensus
	Demand            demandView
	// ScaleCheck is the external-reads lane's scale_check result at Now
	// (externalReadsRecording.scaleCheck), nil when missing or stale (I5).
	ScaleCheck *scaleCheckResult
	Obs        *ObservationSnapshot
	ObsMaxAge  time.Duration
	// Endpoints is each config-only endpoint key's breaker reading.
	Endpoints map[endpointKey]endpointView
	// ProviderHealth is I7 at Now; nil reads as no registry (fail open).
	ProviderHealth *providerHealthSnapshot
	// Episodes are the startup-health episodes (#46) by episode key.
	Episodes map[string]session.StartupHealthEpisode
	// SleepPolicies is resolveSessionSleepPolicyInfo per row, by bead ID; it
	// asks the provider for the row's sleep capability, so the gather phase
	// resolves it. A row without one is never config-suppressed.
	SleepPolicies map[string]resolvedSessionSleepPolicy
	// TransportRefused names templates whose session transport the provider
	// cannot carry (validateAgentSessionTransportForBuild, which resolves the
	// provider binary): legacy realizes nothing for them.
	TransportRefused map[string]string
	ReadyWaits       map[string]bool // I10; nil until P4.3
	// InFlight is the in-flight map's view (inflightMap.view): its running
	// and ambiguous creates stand in for their rows until the census shows
	// them (inFlightStandIns).
	InFlight inflightView
	// Backoff is the backoff table (backoffTable.Snapshot). A create record
	// live at Now refuses its plan identity (AM-N8); a work record live at
	// Now refuses its bead's worktree evidence while its fingerprint matches
	// (#34).
	Backoff map[string]backoffRecord
}

// demandView is the demand collectors' output for one pass (P3 spec §4.3),
// read through v2DemandReads in the gather phase. Rows alias the backstop
// recording and the read memo, so the pass never edits them.
type demandView struct {
	// AssignedWork, AssignedStoreRefs and AssignedStores are
	// collectAssignedWorkBeadsWithStores' index-aligned rows; ReadyAssigned
	// their store-scoped readiness; StorePartial its partial flag (POOL-018).
	// A suspended city reads them too: legacy's suspend drain spares a row
	// with open assigned work.
	AssignedWork      []beads.Bead
	AssignedStores    []beads.Store
	AssignedStoreRefs []string
	ReadyAssigned     map[storeScopedBeadKey]bool
	StorePartial      bool
	// Collected is the rest of the demand pass's collection. The pass fills
	// its custom counts from ScaleCheck and projects its routed rows.
	Collected collectedDemand
	// CustomCheckTemplates are the pools with a custom scale_check (the
	// demand targets' pendingPools); only their I5 partials mean anything.
	CustomCheckTemplates []string
	// NamedDefault is defaultNamedSessionDemand's demand.
	NamedDefault map[string]bool
	// RelocatedClaimRefs (assignedWorkRelocatedClaimRefs) and WakeClaimRefs
	// (assignedWorkClaimRefs) are residency-topology reads, resolved per
	// environment generation.
	RelocatedClaimRefs []string
	WakeClaimRefs      []string
}

// endpointView is one endpoint's breaker as the pass reads it.
type endpointView struct {
	Gate endpointGate
	// HoldsPendingCreate keeps never-started pending creates on the
	// endpoint (endpointCapacityGuard.HoldsPendingCreate).
	HoldsPendingCreate bool
	// Refusals are the endpoint's refusals this episode by row ID: probe
	// rotation puts the most refused last (endpointCapacityGuard).
	Refusals map[string]int
}

// allocDecision is one pass's output.
type allocDecision struct {
	Snapshot *selectionSnapshot
	// Plans are the fresh rows the pass would create, in planning order:
	// named, then pool by template.
	Plans []allocPlan
	// Planning is the census the plans were made against, every canonical
	// row on every leg: the create effects' planning census
	// (createPass.planning). It holds census rows only (C7.2), never a
	// planning reservation, which would fence a create on its own name.
	Planning []session.Info
	// ReadyRouted and ReadyRoutedRefs are the ready unassigned routed work
	// the demand selected, from the projected rows, index-aligned: the
	// idle-claim nudge's input (P4.3; legacy's ReadyUnassignedRoutedWorkBeads).
	ReadyRouted     []beads.Bead
	ReadyRoutedRefs []string
	Trace           []allocTraceRecord
	// Alerts are the pass's operator alerts: each configured named identity
	// with more than one open row (C2.13). OBS1 surfaces them.
	Alerts []string
}

// decidePass is one decide's working state.
type decidePass struct {
	in   allocInputs
	cfg  *config.City
	snap *selectionSnapshot
	obs  map[rowKey]rowObservation

	sessionsLeg string
	// none holds the rows the allocator does not manage, by reason (AM11,
	// C2.11, C2.13).
	none map[rowKey]string
	// managed are the canonical sessions-leg rows outside step 2's none, in
	// census order; byID indexes them.
	managed []session.Info
	byID    map[string]rowKey
	// decidable are the managed rows the pass decides from: without identity
	// duplicates, so a named session never resolves to whichever claimant
	// sorts first (P3-1 obligation).
	decidable []session.Info
	// occupancy is every canonical census row, on every leg and in every
	// class: what holds a slot or a name (C7.1).
	occupancy []session.Info

	merged      mergedDemand
	named       namedSessionDemand
	poolStates  []PoolDesiredState
	poolDesired map[string]int
	poolWork    []beads.Bead
	// inFlight are the planning reservations of the in-flight creates the
	// census does not show yet (inFlightCreates), and standIns the templates
	// of the pending-create rows the pool ones stand in for, by stand-in ID.
	inFlight []planReservation
	standIns map[string]string

	// sessions is decidableSnapshot's: the pool retention and the planner
	// read one snapshot.
	sessions *sessionBeadSnapshot
	bp       *agentBuildParams
	desired  map[string]TemplateParams // membership only (classifyOverlaySession)
	selected map[rowKey]*selection
	plans    []allocPlan
	trace    []allocTraceRecord
	alerts   []string

	// index is the realization index (allocator_index.go); nil realizes
	// over the pass's own params, the index's oracle.
	index *passIndex
}

// selection is a row the pass placed InDesired.
type selection struct {
	ref     desiredConfigRef
	binding *bindingTarget // candidate; step 14 decides
	// normalize marks a pool-selected canonical singleton whose stored
	// identity is a phantom slot spelling (POOL-046).
	normalize bool
}

// decideAllocation is the allocator's whole decision for one pass. It is
// pure: identical inputs give identical outputs.
func decideAllocation(in allocInputs) (allocDecision, error) {
	if in.Now.IsZero() {
		return allocDecision{}, errDecideNoClock
	}
	if in.Census == nil {
		return allocDecision{}, errDecideNoCensus
	}
	p := newDecidePass(in)
	p.inFlight = p.inFlightCreates()
	p.prepare()
	if !in.CitySuspended {
		p.plan()
	}
	return p.finish(), nil
}

func newDecidePass(in allocInputs) *decidePass {
	cfg := in.Cfg
	if cfg == nil {
		cfg = &config.City{}
	}
	p := &decidePass{
		in:  in,
		cfg: cfg,
		snap: &selectionSnapshot{
			Epoch:      in.Epoch,
			ConfigRev:  in.ConfigRev,
			EnvGen:     in.EnvGen,
			DecisionAt: in.Now,
			Entries:    make(map[rowKey]*selectionEntry),
		},
		none:     make(map[rowKey]string),
		byID:     make(map[string]rowKey),
		index:    newPassIndex(),
		standIns: make(map[string]string),
		desired:  make(map[string]TemplateParams),
		selected: make(map[rowKey]*selection),
	}
	if c := p.in.Census; len(c.Legs) > 0 {
		p.sessionsLeg = c.Legs[0].Ref
		p.obs = observeCensus(in.Obs, c, in.Now, in.ObsMaxAge)
	}
	return p
}

// prepare is steps 1-3 and 9: every census row's entry and class, the
// canonical named rows, the rows the pass decides from, and the partial
// causes. A suspended city runs it too, so its Keep conversions see them.
func (p *decidePass) prepare() {
	p.classifyRows()
	p.identityDuplicates()
	p.selectDecidable()
	p.partials()
}

// finish is steps 10-14, or step 1's classification for a suspended city.
func (p *decidePass) finish() allocDecision {
	if p.in.CitySuspended {
		p.suspended()
		return p.decision()
	}
	p.classify(p.awake())
	p.floors()
	p.bindings()
	return p.decision()
}

// classifyRows is step 2: every census row gets an entry, and the rows the
// allocator does not manage are set aside with legacy's predicates. They
// keep the slots and names legacy gives them (fail-closed): every one stays
// in occupancy. A pending create (one holding pending_create_claim) is a
// rollback candidate only when its own runtime is absent, absent-unconfirmed
// or a name another row holds. Unknown liveness keeps it managed, and so does
// a dead pane: legacy's IsRunning reads a corpse as not running, but v2's
// start path recycles it. A creating row with no claim is never one (v5 C3,
// B5): it stays managed and reusable, as legacy's is, and A6 heals it.
// Any other row whose name another bead's runtime holds is None (C11).
func (p *decidePass) classifyRows() {
	c := p.in.Census
	var startupTimeout time.Duration
	if p.in.Cfg != nil {
		startupTimeout = p.cfg.Session.StartupTimeoutDuration()
	}
	clk := &clock.Fake{Time: p.in.Now}
	keys := make([]rowKey, 0, len(c.Rows))
	for k := range c.Rows {
		keys = append(keys, k)
	}
	sortRowKeys(keys)
	// An Info is kilobytes: size the row lists once.
	p.occupancy = slices.Grow(p.occupancy, len(keys))
	p.managed = slices.Grow(p.managed, len(keys))
	for _, k := range keys {
		row := c.Rows[k]
		template := resolvedSessionTemplateInfo(row.Info, p.cfg)
		e := &selectionEntry{
			Key:      k,
			Basis:    rowBasis{Incarnation: row.Incarnation, InstanceToken: row.InstanceToken},
			Template: template,
			Endpoint: endpointKeyForAgent(p.cfg, p.agentByTemplate(template), row.Info),
		}
		o, observed := p.obs[k]
		if observed {
			e.Liveness, e.ObservationUncertain = o.Liveness, o.Uncertain
		}
		p.snap.Entries[k] = e
		if row.DuplicateOf != "" {
			p.none[k] = reasonDuplicate
			e.Identity = &identityView{DuplicateOf: row.DuplicateOf}
			continue
		}
		p.occupancy = append(p.occupancy, row.Info)
		info := row.Info
		notRunning := observed && (o.Liveness == livenessGone || o.Liveness == livenessOccupied)
		// A guarded endpoint the pass has no view of holds (fail closed).
		ep, viewed := p.in.Endpoints[e.Endpoint]
		endpointHolds := e.Endpoint != "" && (!viewed || ep.HoldsPendingCreate)
		switch {
		case k.Leg != p.sessionsLeg:
			p.none[k] = reasonCensusOnly
		case row.UnknownState:
			p.none[k] = reasonUnknownState
		case isFailedCreateSessionInfo(info):
			p.none[k] = reasonFailedCreate
		case notRunning && info.PendingCreateClaim && pendingCreateLeaseExpiredForRollbackInfo(info, clk, startupTimeout) &&
			!endpointHolds:
			p.none[k] = reasonRollbackCandidate
		case o.Liveness == livenessOccupied:
			p.none[k] = reasonNameOccupied
		default:
			p.managed = append(p.managed, info)
			p.byID[info.ID] = k
		}
	}
}

// identityDuplicates is step 9 (C2.13): among the open configured named rows
// of one identity, legacy's rule picks the canonical row (SESS-012) from the
// managed rows, and every other managed row is None(identity-duplicate),
// traced. An identity with more than one row, census-only rows on other legs
// included, raises one alert. Nothing is retired (PAR-RETIRE): C11 refuses
// duplicates at boot.
func (p *decidePass) identityDuplicates() {
	canonical := make(map[string]rowKey)
	rows := make(map[string][]rowKey)
	keys := slices.Collect(maps.Keys(p.in.Census.Rows))
	sortRowKeys(keys)
	for _, k := range keys {
		info := p.in.Census.Rows[k].Info
		mk, managed := p.byID[info.ID]
		managed = managed && mk == k
		identity := namedSessionIdentityInfo(info)
		if (!managed && p.none[k] != reasonCensusOnly) || !isNamedSessionInfo(info) ||
			!session.NamedSessionInfoContinuityEligible(info) || identity == "" {
			continue
		}
		spec, ok := findNamedSessionSpec(p.cfg, p.in.CityName, identity)
		if !ok {
			continue
		}
		rows[identity] = append(rows[identity], k)
		if c, seen := canonical[identity]; managed && (!seen || namedSessionWinsCanonicalRepairInfo(info, p.in.Census.Rows[c].Info, spec.SessionName)) {
			canonical[identity] = k
		}
	}
	for _, identity := range slices.Sorted(maps.Keys(rows)) {
		c, hasCanonical := canonical[identity]
		var dups []string
		for _, k := range rows[identity] {
			if hasCanonical && k == c {
				p.snap.Entries[k].Identity = &identityView{Identity: identity, Canonical: true}
				continue
			}
			dups = append(dups, k.Leg+"/"+k.ID)
			if p.none[k] == "" {
				e := p.snap.Entries[k]
				e.Identity = &identityView{Identity: identity}
				p.none[k] = reasonIdentityDuplicate
				p.trace = append(p.trace, allocTraceRecord{Template: e.Template, Instance: identity, Key: k, Reason: reasonIdentityDuplicate})
			}
		}
		if len(rows[identity]) > 1 {
			canonicalID := "none"
			if hasCanonical {
				canonicalID = c.Leg + "/" + c.ID
			}
			p.alerts = append(p.alerts, fmt.Sprintf("allocator: named identity %s has %d open rows: canonical %s, duplicates %s (C2.13; none retired)",
				identity, len(rows[identity]), canonicalID, strings.Join(dups, ", ")))
		}
	}
}

func (p *decidePass) selectDecidable() {
	p.decidable = slices.Grow(p.decidable, len(p.managed))
	for _, info := range p.managed {
		if _, none := p.none[p.byID[info.ID]]; !none {
			p.decidable = append(p.decidable, info)
		}
	}
}

// partials is step 3: the global cause, with legacy's effect. A partial
// demand or census read makes the snapshot partial (retain everything); it
// refuses no create. The demand's template causes join in step 4.
func (p *decidePass) partials() {
	if p.storePartial() {
		p.snap.Partial.Global = append(p.snap.Partial.Global, causeStoreQueryPartial)
		p.snap.Mode = modePartial
	}
}

// suspended is step 1 for a suspended city (C2.1, POOL-001): no Wake, no
// plans. Every managed row drains as suspended, except a canonical named
// row, which stays InDesired and sleeps. Nothing legacy's suspend drain
// leaves alone is shrunk (session_reconciler.go:2414-2436, owner decision
// at P3-5a review): a partial read, an uncertain observation, a pending
// create legacy still leases (pendingCreateSessionStillLeasedInfo) and open
// assigned work each keep the row, and the row carries its assigned work.
func (p *decidePass) suspended() {
	p.snap.Mode = modeSuspended
	for k, e := range p.snap.Entries {
		if reason, ok := p.none[k]; ok {
			e.Desired, e.Reason = desireNone, reason
			continue
		}
		info := p.in.Census.Rows[k].Info
		e.Reason, e.DrainReason = reasonSuspendedCity, drainSuspended
		e.AssignedWork = p.openAssignedWork(info)
		switch {
		case e.Identity != nil && e.Identity.Canonical:
			e.InDesired, e.Desired = true, desireSleep
		default:
			e.Desired = desireDrain
		}
		if reason := p.keepReason(e, info); reason != "" {
			e.Desired, e.Reason = desireKeep, reason
		}
	}
}

// openAssignedWork is the first open or in-progress assigned work bead
// whose assignee is one of the row's assignment identifiers: legacy's
// suspend drain veto (sessionHasOpenAssignedWorkForConfigInfo).
func (p *decidePass) openAssignedWork(info session.Info) *assignedWorkView {
	ids := sessionAssignmentIdentifiersForConfigInfo(info, p.cfg)
	for _, w := range p.in.Demand.AssignedWork {
		if w.Status != "open" && w.Status != "in_progress" {
			continue
		}
		if slices.Contains(ids, strings.TrimSpace(w.Assignee)) {
			return &assignedWorkView{BeadID: w.ID, Claimed: w.Status == "in_progress"}
		}
	}
	return nil
}

// keepReason is the Keep conversion of a Sleep or Drain (POOL-035, P-3,
// C2.9), or "": a partial read (a global cause, which a stale leg implies,
// or its template retained), then an uncertain observation. In a suspended
// city a pending create legacy still leases and a row with open assigned
// work keep too.
func (p *decidePass) keepReason(e *selectionEntry, info session.Info) string {
	if e.Desired != desireSleep && e.Desired != desireDrain {
		return ""
	}
	switch {
	case len(p.snap.Partial.Global) > 0 || p.retains(e.Template, info):
		return reasonPartialRetain
	case e.ObservationUncertain:
		return reasonObservationUncertain
	case !p.in.CitySuspended:
		return ""
	case pendingCreateSessionStillLeasedInfo(info, p.cfg, &clock.Fake{Time: p.in.Now}):
		return reasonPendingCreate
	case e.AssignedWork != nil:
		return reasonAssignedWork
	}
	return ""
}

// awake is steps 10 and 11: the awake set keyed by bead ID over the
// decidable rows, with runtime facts from I3 under AM3, then config sleep
// suppression.
func (p *decidePass) awake() map[string]AwakeDecision {
	infos := p.decidable
	work, workRefs, _ := filterAssignedWorkBeadsForSessionWakeOn(p.cfg, p.in.CityPath, p.in.Demand.WakeClaimRefs,
		infos, p.in.Demand.AssignedWork, p.in.Demand.AssignedStoreRefs, nil)
	agentSuspended := func(a *config.Agent) bool {
		return a.Suspended || agentInSuspendedRig(p.in.CityPath, a, p.cfg.Rigs, p.in.SuspendedRigPaths)
	}
	input := newAwakeInputFromSnapshot(p.cfg, agentSuspended, infos, p.poolDesired, p.named.workReady,
		p.named.routedDemand, nil, p.in.ReadyWaits, work, readyAssignedFlagsForBeads(p.in.Demand.ReadyAssigned, work, workRefs), p.in.Now)
	if p.index != nil {
		input.workIndex = newAwakeWorkIndex(input.WorkBeads)
	}
	for _, info := range infos {
		o := p.obs[p.byID[info.ID]]
		// Unknown liveness reads running, and an uncertain attach on a live
		// or unknown runtime reads attached (C2.9, P-2); the row is Keep.
		if o.Liveness == livenessAlive || o.Liveness == livenessUnknown {
			input.RunningSessions[info.ID] = true
		}
		if o.Attached || (o.Uncertain && strings.HasPrefix(o.Reason, observeReasonAttach)) {
			input.AttachedSessions[info.ID] = true
		}
		if o.Pending {
			input.PendingSessions[info.ID] = true
		}
	}
	decisions := computeAwakeSetKeyed(input, awakeKeyBeadID)
	for _, info := range infos {
		d, ok := decisions[info.ID]
		if !ok || !d.ShouldWake || strings.TrimSpace(info.PinAwake) == "true" {
			continue
		}
		k := p.byID[info.ID]
		o := p.obs[k]
		if o.Liveness == livenessAlive && o.Pending {
			continue
		}
		if p.configSleepSuppressed(info, o, d) {
			d.ShouldWake, d.Reason = false, reasonConfigSleep
			decisions[info.ID] = d
		}
	}
	return decisions
}

// configSleepSuppressed is stage 6b (SESS-585/586/652, AM3): legacy's
// configWakeSuppressedInfo with no provider, so its idle reference is
// detached_at, which is legacy's answer for a row that is not alive. A live
// row is suppressed only with a known I3 activity fact, measured from
// max(detached_at, activity); without one it is not suppressed. The
// override matrix decides whether the row's demand outranks the policy.
func (p *decidePass) configSleepSuppressed(info session.Info, o rowObservation, d AwakeDecision) bool {
	policy, ok := p.in.SleepPolicies[info.ID]
	if !ok {
		return false
	}
	ref := info
	if o.Liveness == livenessAlive {
		obs := readRuntimeName(p.in.Obs, info.SessionNameMetadata, p.in.Now, p.in.ObsMaxAge).obs
		if !obs.LastActivityKnown {
			return false
		}
		detached, _ := time.Parse(time.RFC3339, info.DetachedAt)
		if obs.LastActivity.After(detached) {
			ref.DetachedAt = obs.LastActivity.UTC().Format(time.RFC3339Nano)
		}
	}
	if !configWakeSuppressedInfo(ref, policy, nil, &clock.Fake{Time: p.in.Now}) {
		return false
	}
	eval := awakeSetToWakeEvals(map[string]AwakeDecision{info.SessionNameMetadata: d},
		[]AwakeSessionBead{{ID: info.ID, SessionName: info.SessionNameMetadata}})[info.ID]
	template := normalizedSessionTemplateInfo(info, p.cfg)
	return !wakeDemandOverridesSleepSuppression(d, eval, policy, p.poolDesired, template, info.SleepIntent != "", explicitWakePendingInfo(info))
}

// classify is step 12 (CONTRACT §2.2): InDesired ∧ ShouldWake is Wake,
// InDesired ∧ ¬ShouldWake is Sleep, ¬InDesired is Drain, and the set-aside
// rows None; then a partial read or an uncertain observation turns a Sleep
// or a Drain into Keep (POOL-035, P-3, C2.9).
func (p *decidePass) classify(decisions map[string]AwakeDecision) {
	for k, e := range p.snap.Entries {
		if reason, ok := p.none[k]; ok {
			e.Desired, e.Reason = desireNone, reason
			continue
		}
		info := p.in.Census.Rows[k].Info
		sel := p.selected[k]
		e.InDesired = sel != nil
		if sel != nil {
			ref := sel.ref
			e.Config = &ref
			e.Normalize = sel.normalize
		}
		d := decisions[info.ID]
		if d.HasAssignedWork || d.AssignedWorkBeadID != "" {
			e.AssignedWork = &assignedWorkView{BeadID: d.AssignedWorkBeadID, Claimed: d.AssignedWorkClaimed, RequiresFreshCycle: d.RequiresFreshCycle}
		}
		eval := awakeSetToWakeEvals(map[string]AwakeDecision{info.SessionNameMetadata: d},
			[]AwakeSessionBead{{ID: info.ID, SessionName: info.SessionNameMetadata}})[info.ID]
		e.WakeReasons = eval.Reasons
		switch {
		case e.InDesired && d.ShouldWake:
			e.Desired, e.Reason = desireWake, d.Reason
			// A wake that rests only on an uncertain attach keeps the row
			// and never starts it (stage 3a as amended).
			if d.Reason == "attached" && e.ObservationUncertain {
				e.Desired, e.Reason = desireKeep, reasonObservationUncertain
			}
		case e.InDesired:
			e.Desired, e.Reason = desireSleep, firstNonEmpty(d.Reason, reasonNoWake)
		default:
			e.Desired, e.Reason = desireDrain, firstNonEmpty(d.Reason, reasonNoWake)
			e.DrainReason = drainOrphaned
			if p.agentSuspended(e.Template) {
				e.DrainReason = drainSuspended
			}
		}
		if reason := p.keepReason(e, info); reason != "" {
			e.Desired, e.Reason = desireKeep, reason
		}
	}
}

// floors is step 13 (C2.6): each template's min_active_sessions members are
// its warm floor candidates with the lowest (BeadID, Leg).
func (p *decidePass) floors() {
	byTemplate := make(map[string][]rowKey)
	for _, info := range p.decidable {
		k := p.byID[info.ID]
		if !isWarmFloorCandidate(info) || isNamedSessionInfo(info) ||
			strings.TrimSpace(info.ConfiguredNamedIdentity) != "" || isManualSessionInfo(info) || info.DependencyOnly {
			continue
		}
		template := normalizedSessionTemplateInfo(info, p.cfg)
		byTemplate[template] = append(byTemplate[template], k)
	}
	for template, keys := range byTemplate {
		agent := p.agentByTemplate(template)
		if agent == nil {
			continue
		}
		minActive := agent.EffectiveMinActiveSessions()
		if minActive <= 0 {
			continue
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].ID != keys[j].ID {
				return keys[i].ID < keys[j].ID
			}
			return keys[i].Leg < keys[j].Leg
		})
		keys = keys[:min(minActive, len(keys))]
		if p.snap.Floors == nil {
			p.snap.Floors = make(map[string][]rowKey)
		}
		p.snap.Floors[template] = keys
		for rank, k := range keys {
			p.snap.Entries[k].Floor = &floorView{Rank: rank, MinActive: minActive}
		}
	}
}

// decision assembles the pass's output.
func (p *decidePass) decision() allocDecision {
	return allocDecision{
		Snapshot:        p.snap,
		Plans:           p.plans,
		Planning:        slices.Clip(p.occupancy), // the pass is done with it
		ReadyRouted:     p.merged.ReadyUnassignedRouted,
		ReadyRoutedRefs: p.merged.ReadyUnassignedRoutedRefs,
		Trace:           p.trace,
		Alerts:          p.alerts,
	}
}

// storePartial reports a partial demand or census read: what it returned is
// kept, so nothing shrinks.
func (p *decidePass) storePartial() bool {
	return p.in.Demand.StorePartial || p.in.Census.Partial()
}

// markTemplate records a template partial cause.
func (p *decidePass) markTemplate(template string, retain bool, cause string) {
	ps := &p.snap.Partial
	if ps.Templates == nil {
		ps.Templates = make(map[string]templatePartial)
	}
	tp := ps.Templates[template]
	tp.Retain = tp.Retain || retain
	if !slices.Contains(tp.Causes, cause) {
		tp.Causes = append(tp.Causes, cause)
	}
	ps.Templates[template] = tp
}

// retains reports whether a template partial keeps row: pool retention
// keeps every row of the template, a named scale-check partial only its
// named rows (POOL-035/037).
func (p *decidePass) retains(template string, info session.Info) bool {
	tp, ok := p.snap.Partial.Templates[template]
	if !ok {
		return false
	}
	if tp.Retain {
		return true
	}
	return slices.Contains(tp.Causes, "named-scale-check-partial") && isNamedSessionInfo(info)
}

// agentSuspended reports a configured template whose agent or rig is
// suspended: its undesired rows drain as suspended, not orphaned.
func (p *decidePass) agentSuspended(template string) bool {
	agent := p.agentByTemplate(template)
	return agent != nil && (agent.Suspended || agentInSuspendedRig(p.in.CityPath, agent, p.cfg.Rigs, p.in.SuspendedRigPaths))
}

// sortRowKeys orders keys by leg, then bead ID.
func sortRowKeys(keys []rowKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Leg != keys[j].Leg {
			return keys[i].Leg < keys[j].Leg
		}
		return keys[i].ID < keys[j].ID
	})
}

// runtimeNameState is the observation cache's reading of a runtime name
// that no census row need own: a planned singleton's or a named session's
// (POOL-052, C7.3, AM-N6).
type runtimeNameState uint8

const (
	nameUnknown runtimeNameState = iota
	// nameAbsent: not listed by a fresh pass that every backend listed
	// completely and attested (readPresence's gone).
	nameAbsent
	// nameCorpse: listed, but its pane is not running. Legacy's IsRunning
	// reads it as not running, so it frees a singleton's name.
	nameCorpse
	// nameZombie: the pane is running but its process is dead. IsRunning
	// reads it as running, so it holds a singleton's name.
	nameZombie
	nameAlive
)

type runtimeNameReading struct {
	state runtimeNameState
	obs   RuntimeObservation
}

// readRuntimeName reads a runtime name from I3 by name, in observeRow's
// presence order (readPresence; POOL-052): a name a fresh pass listed on an
// unprimed backend is present, and only a complete pass proves it absent.
func readRuntimeName(snap *ObservationSnapshot, name string, now time.Time, maxAge time.Duration) runtimeNameReading {
	name = strings.TrimSpace(name)
	if name == "" || snap == nil {
		return runtimeNameReading{state: nameUnknown}
	}
	listed, complete := inventoryAbsence(snap, now, maxAge)
	obs, present, gone, _ := readPresence(snap, name, listed, complete, now, maxAge)
	r := runtimeNameReading{state: nameUnknown, obs: obs}
	switch {
	case gone:
		r.state = nameAbsent
	case !present:
	case obs.Running.Value == ObsNo:
		r.state = nameCorpse
	case obs.ProcessAlive.Value == ObsNo:
		r.state = nameZombie
	default:
		r.state = nameAlive
	}
	return r
}
