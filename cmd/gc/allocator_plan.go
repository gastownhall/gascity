package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The allocator's demand and realization plan (P3 spec §4.4 steps 4-8 and
// 14): the accepted pool demand, which existing rows the pass selects for
// it, the create plans for the rest, and the bindings of selected rows that
// are not alive. It runs legacy's planner in planOnly mode (P3-1) over build
// params built from the census, never through newAgentBuildParams, which
// installs exec.LookPath and the OS filesystem and loads skill catalogs
// from disk. A fresh plan passes the plan-time gates first; each refusal is
// traced and consumes nothing.

// Plan-gate causes, published as ineligible:<cause> (C2.2, C5.10).
const (
	gateCensusIncomplete = "census-incomplete"
	gateBlockCreate      = "block-create"
	gateCreateRefused    = "create-refused:"
	gateWorktreeRefused  = "worktree-refused"
	gateIdentityLease    = "identity-lease-held"
	gateNameOccupied     = "name-occupied"
	gateLivenessUnknown  = "liveness-unknown"
	gateQuarantine       = "startup-quarantine"
	gateEndpointShut     = "endpoint-open"
	gateProviderRed      = "provider-red"
	gatePartial          = "partial"
	gateTransport        = "transport-refused"
	gateNoSlot           = "no-slot"
	gateInFlight         = "in-flight"
	gateNamedConflict    = "named-conflict"
	gateOwnerPending     = "owner-pending"
	gateNameHeld         = "name-held:"
	gatePoolSlotShaped   = "named-identity-pool-slot-shaped"
)

// standInPrefix marks the ID of an uncleared create's stand-in row.
const standInPrefix = "pending-create:"

// plan is steps 4-8: demand, pool desired, the realization plan and the
// overlay and dependency floors.
func (p *decidePass) plan() {
	p.demand()
	p.computePoolDesired()
	p.newPlanParams()
	p.planNamed()
	p.realizePools()
	p.overlayAndFloors()
}

// demand is step 4: the collected demand merged by legacy's rules, with the
// custom scale_check counts from I5 and the routed rows projected as
// legacy's in-tick route repair leaves them. The projection runs once, and
// every consumer reads its rows: control-dispatcher demand, the ready routed
// work the idle-claim nudge reads, and the default probe, which drops the
// control rows whose route the projection suppressed. Legacy's Ready-based
// probe still counts them (mc-zndi7.41, open for legacy); the v2-only fix is
// an explained difference for P3-5c.
func (p *decidePass) demand() {
	collected := p.in.Demand.Collected
	collected.CustomCounts, collected.CustomPartials = nil, nil
	if templates := p.in.Demand.CustomCheckTemplates; len(templates) > 0 {
		collected.CustomCounts = make(map[string]int, len(templates))
		for _, template := range templates {
			var count int
			if p.in.ScaleCheck != nil {
				count = p.in.ScaleCheck.Counts[template]
			}
			collected.CustomCounts[template] = count
			if p.in.ScaleCheck.partial(template, p.in.Now, p.in.ScaleCheckMaxAge) {
				collected.CustomPartials = markScaleCheckPartialTemplate(collected.CustomPartials, template)
			}
		}
	}
	projected, _ := projectControlDispatcherRoutes(p.cfg, collected.UnassignedRouted, collected.UnassignedRoutedRefs)
	collected.DefaultCounts, collected.DefaultDemand = withoutSuppressedRoutes(collected, projected)
	collected.UnassignedRouted = projected
	p.merged = mergeCollectedDemand(p.cfg, collected)
	claimed := make(map[string]bool, len(p.in.Demand.AssignedWork))
	for _, w := range p.in.Demand.AssignedWork {
		claimed[w.ID] = true
	}
	p.unclaimed = make(map[string]bool)
	for _, d := range p.merged.ScaleCheckDemand {
		for _, id := range d.WorkBeadIDs {
			if id = strings.TrimSpace(id); id != "" && !claimed[id] {
				p.unclaimed[id] = true
			}
		}
	}
	for template := range p.merged.PoolScaleCheckPartial {
		p.markTemplate(template, true, true, "pool-scale-check-partial")
	}
	for template := range p.merged.PoolPartialRetention {
		p.markTemplate(template, true, false, "pool-partial-retention")
	}
	for template := range p.merged.NamedScaleCheckPartial {
		tp := p.snap.Partial.Templates[template]
		if !tp.Retain {
			p.markTemplate(template, false, false, "named-scale-check-partial")
		}
	}
	refs := p.in.Demand.RelocatedClaimRefs
	p.named = computeNamedSessionDemandOn(p.in.CityName, p.in.CityPath, p.cfg, func() []string { return refs },
		p.in.SuspendedRigPaths, p.in.Demand.NamedDefault, p.in.Demand.AssignedWork, p.in.Demand.AssignedStoreRefs,
		p.in.Demand.ReadyAssigned, p.merged.ScaleCheckCounts, io.Discard)
}

// withoutSuppressedRoutes returns the default-probe counts and demand
// without the rows whose route the control-dispatcher projection dropped or
// changed: a suppressed route is not demand for the template it names. It
// never edits c's maps; with nothing suppressed it returns them as they are.
func withoutSuppressedRoutes(c collectedDemand, projected []beads.Bead) (map[string]int, map[string]scaleCheckDemand) {
	suppressed := make(map[storeScopedBeadKey]bool)
	for i, row := range c.UnassignedRouted {
		if i >= len(projected) || i >= len(c.UnassignedRoutedRefs) {
			break
		}
		key := beadmeta.RoutedToMetadataKey
		if row.Metadata[key] != projected[i].Metadata[key] {
			suppressed[storeScopedBeadKey{StoreRef: normalizeDemandStoreRef(c.UnassignedRoutedRefs[i]), ID: row.ID}] = true
		}
	}
	if len(suppressed) == 0 {
		return c.DefaultCounts, c.DefaultDemand
	}
	counts := maps.Clone(c.DefaultCounts)
	demand := maps.Clone(c.DefaultDemand)
	for template, d := range c.DefaultDemand {
		var kept []string
		for _, id := range d.WorkBeadIDs {
			if suppressed[storeScopedBeadKey{StoreRef: normalizeDemandStoreRef(d.StoreRefs[id]), ID: id}] {
				counts[template]--
				continue
			}
			kept = append(kept, id)
		}
		if len(kept) != len(d.WorkBeadIDs) {
			d.WorkBeadIDs, d.Count = kept, len(kept)
			demand[template] = d
		}
	}
	return counts, demand
}

// computePoolDesired is step 5: accepted pool requests within the nested
// caps at Now (POOL-021..034), partial retention and the named merge. Caps
// count accepted requests, not live sessions (POOL-033). Templates in a
// suspended rig are excluded before the caps (V-D1, an explained difference:
// legacy still gives them min-fill and resume requests that consume caps).
//
// The rows demand counts are the decidable rows plus a stand-in for each
// uncleared create whose row the census does not show yet (C5.13 row 1:
// in-flight demand is census ∪ ledger), and each row that holds work C6.6
// counts as consumed carries that work as its trigger, so POOL-030's
// match-by-trigger keeps the row on it and the work is not planned again.
func (p *decidePass) computePoolDesired() {
	p.boundWork()
	rows := make([]session.Info, 0, len(p.decidable)+len(p.in.Reservations))
	for _, info := range p.decidable {
		if b := p.stickyBinding(p.byID[info.ID]); b != nil {
			info.TriggerBeadID = b.WorkBeadID
		}
		rows = append(rows, info)
	}
	rows = append(rows, p.inFlightStandIns()...)
	p.poolWork = filterAssignedWorkBeadsForPoolDemandAt(p.cfg, p.in.CityPath, p.in.Demand.RelocatedClaimRefs,
		rows, p.in.Demand.AssignedWork, p.in.Demand.AssignedStoreRefs, p.in.Now.UTC())
	poolCfg := p.cfg
	if len(p.in.SuspendedRigPaths) > 0 {
		clone := *p.cfg
		clone.Agents = slices.Clone(p.cfg.Agents)
		for i := range clone.Agents {
			if agentInSuspendedRig(p.in.CityPath, &clone.Agents[i], clone.Rigs, p.in.SuspendedRigPaths) {
				clone.Agents[i].Suspended = true
			}
		}
		poolCfg = &clone
	}
	p.poolStates = computePoolDesiredStatesAt(poolCfg, p.poolWork, rows, p.merged.ScaleCheckCounts, p.merged.ScaleCheckDemand, p.in.Now, nil)
	p.poolDesired = retainScaleCheckPartialPoolDesired(p.cfg, PoolDesiredCounts(p.poolStates),
		newSessionBeadSnapshotFromInfos(p.decidable), p.merged.PoolPartialRetention)
	if p.poolDesired == nil {
		p.poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(p.poolDesired, p.named.workReady, p.cfg)
	p.snap.PoolDesired = p.poolDesired
}

// inFlightStandIns returns a pending-create row for each uncleared pool or
// dependency create the census does not show yet, matched by its entry's
// marker: its row ID or instance token (C5.13; a create's Key.ID is its
// marker's row ID). A reservation whose entry the ledger view lacks still
// stands in: counting a create twice for one pass is safe, planning its
// work again is not.
func (p *decidePass) inFlightStandIns() []session.Info {
	entries := make(map[string]ledgerEntry)
	for _, e := range p.in.Ledger {
		entries[e.ID] = e
	}
	var lc ledgerCensus
	if len(entries) > 0 {
		lc = p.in.Census.Ledger(p.cfg)
	}
	var out []session.Info
	for _, r := range p.in.Reservations {
		if r.NamedIdentity != "" {
			continue
		}
		if e, ok := entries[r.EntryID]; ok && e.markerVisible(lc) {
			continue
		}
		at := r.ReservedAt
		if at.IsZero() || at.After(p.in.Now) {
			// An uncleared create is in flight now, whatever its stamp.
			at = p.in.Now
		}
		info := session.Info{
			ID:                     standInPrefix + r.EntryID,
			Template:               r.Template,
			AgentName:              r.QualifiedInstance,
			SessionOrigin:          "ephemeral",
			PoolManaged:            true,
			MetadataState:          string(session.StateStartPending),
			PendingCreateClaim:     true,
			PendingCreateStartedAt: at.UTC().Format(time.RFC3339Nano),
			CreatedAt:              at,
			TriggerBeadID:          r.WorkBeadID,
		}
		p.standIns[info.ID] = r.Template
		out = append(out, info)
	}
	return out
}

// boundWork records the work C6.6 counts as consumed before planning, by
// the row holding it: work the previous snapshot bound to a decidable row
// that is still a start candidate, and the trigger of a decidable live row.
// Realization pairs a request for such work with its row, so a fresh plan
// never carries it; work that is no longer demand names no request.
func (p *decidePass) boundWork() {
	for _, info := range p.decidable {
		k := p.byID[info.ID]
		if b := p.stickyBinding(k); b != nil {
			p.bound[b.WorkBeadID] = info.ID
			continue
		}
		if trigger := strings.TrimSpace(info.TriggerBeadID); trigger != "" && p.snap.Entries[k].Liveness == livenessAlive {
			if _, taken := p.bound[trigger]; !taken {
				p.bound[trigger] = info.ID
			}
		}
	}
}

// stickyBinding is the binding the previous snapshot published for k, when
// k is still a start candidate (only those carry bindings) and its work is
// still unclaimed demand, or nil. Work a row claimed is no longer the bound
// row's, and closed work is no longer demand (F2).
func (p *decidePass) stickyBinding(k rowKey) *bindingTarget {
	if !p.snap.Entries[k].Liveness.startCandidate() {
		return nil
	}
	b := p.prevBinding(k)
	if b == nil || !p.unclaimed[strings.TrimSpace(b.WorkBeadID)] {
		return nil
	}
	return b
}

// prevBinding is the binding the previous snapshot published for k, or nil.
func (p *decidePass) prevBinding(k rowKey) *bindingTarget {
	if p.in.Prev == nil || p.in.Prev.Entries[k] == nil {
		return nil
	}
	return p.in.Prev.Entries[k].Binding
}

// newPlanParams builds the planner's plan-only build params from the census:
// reuse selects among the decidable rows, and fresh slots avoid every census
// row on every leg plus the planning reservation of every uncleared create
// (C7.1 tier 1). The planner runs unbudgeted: P3-5b admits plans in fair-share order. Its
// own census-completeness gate is off (no bead store: storeless builds read
// as complete); admitPoolPlan refuses a fresh plan on an incomplete census
// instead, with its own cause.
func (p *decidePass) newPlanParams() {
	health := p.in.ProviderHealth
	if health == nil {
		health = &providerHealthSnapshot{}
	}
	p.bp = &agentBuildParams{
		city:                           p.cfg,
		cityName:                       p.in.CityName,
		cityPath:                       p.in.CityPath,
		workspace:                      &p.cfg.Workspace,
		agents:                         p.cfg.Agents,
		providers:                      p.cfg.Providers,
		rigs:                           p.cfg.Rigs,
		beaconTime:                     p.in.Now,
		stderr:                         io.Discard,
		sessionBeads:                   newSessionBeadSnapshotFromInfos(p.decidable),
		sessionOccupancyInfos:          slices.Clone(p.occupancy),
		assignedWorkBeads:              p.poolWork,
		poolScaleCheckPartialTemplates: p.merged.PoolScaleCheckPartial,
		providerHealthSnapshot:         health,
		planOnly:                       true,
		beadNames:                      make(map[string]string),
	}
	for _, r := range p.in.Reservations {
		p.reserve(r)
	}
}

// reserve adds a planning reservation to the occupancy the planner claims
// fresh slots against, as the row it will become.
func (p *decidePass) reserve(r planReservation) {
	info := session.Info{
		ID:                 "reservation:" + r.EntryID,
		Template:           r.Template,
		AgentName:          r.QualifiedInstance,
		PoolManaged:        r.NamedIdentity == "",
		MetadataState:      string(session.StateStartPending),
		PendingCreateClaim: true,
	}
	if r.Slot > 0 {
		info.PoolSlot = strconv.Itoa(r.Slot)
	}
	if r.NamedIdentity != "" {
		info.AgentName = r.NamedIdentity
		info.Alias = r.NamedIdentity
		info.SessionNameMetadata = r.SessionName
		info.ConfiguredNamedSession = true
		info.ConfiguredNamedIdentity = r.NamedIdentity
	}
	p.bp.sessionOccupancyInfos = append(p.bp.sessionOccupancyInfos, info)
}

// realizePools is step 6 (POOL-043..053): legacy's selection phase
// (realizePoolDesiredSessionsAt, phase A) for each accepted request, in
// planOnly mode. A request for a stand-in is the in-flight create itself:
// nothing to realize. Concrete requests come first, then each request whose
// work C6.6 binds to a reusable row, paired with that row, then the rest in
// order. A reused row is selected with its config ref and its binding
// candidate; a fresh plan must pass the plan-time gates, and a refusal
// specific to its name (a create backoff from the fence, or an identity
// lease a dead or absent row holds) moves the request to the next free slot.
func (p *decidePass) realizePools() {
	for _, state := range p.poolStates {
		cfgAgent := findAgentByTemplate(p.cfg, state.Template)
		if cfgAgent == nil {
			p.refuse(state.Template, "", rowKey{}, "no-agent")
			continue
		}
		if agentInSuspendedRig(p.in.CityPath, cfgAgent, p.cfg.Rigs, p.in.SuspendedRigPaths) {
			continue
		}
		qualifiedName := cfgAgent.QualifiedName()
		if why := p.in.TransportRefused[qualifiedName]; why != "" {
			p.refuse(qualifiedName, "", rowKey{}, gateTransport)
			continue
		}
		used := make(map[string]bool)
		usedSlots := make(map[int]bool)
		done := make([]bool, len(state.Requests))
		realize := func(i int, prefer *session.Info) {
			done[i] = true
			p.realizeRequest(cfgAgent, qualifiedName, prefer, state.Requests[i], used, usedSlots)
		}
		for i, request := range state.Requests {
			if request.SessionBeadID == "" {
				continue
			}
			if p.standIns[request.SessionBeadID] != "" {
				done[i] = true
				continue
			}
			var prefer *session.Info
			if candidate, ok := p.bp.sessionBeads.FindInfoByID(request.SessionBeadID); ok {
				// A named row is never a pool instance (defense in depth,
				// as legacy).
				if isNamedSessionInfo(candidate) {
					done[i] = true
					continue
				}
				prefer = &candidate
			}
			realize(i, prefer)
		}
		for i, request := range state.Requests {
			holder := p.bound[strings.TrimSpace(request.WorkBeadID)]
			if done[i] || holder == "" {
				continue
			}
			for _, candidate := range reusablePoolSessionInfosForRequest(p.bp, cfgAgent, qualifiedName, request, p.in.Now, used) {
				if candidate.ID == holder {
					// A pairing that refuses (unusable worktree evidence, a
					// claimed slot) falls through to fresh realization.
					realize(i, &candidate)
					done[i] = p.selected[p.byID[holder]] != nil
					break
				}
			}
		}
		for i := range state.Requests {
			if !done[i] {
				realize(i, nil)
			}
		}
	}
}

// realizeRequest selects or plans one request. A refusal specific to the
// planned name keeps its slot used and plans the next free one. Any other
// refusal stalls the request, as legacy's does (build_desired_state.go:5180).
// The plans tried are bounded by the slot range, and for an unlimited pool by
// the names that can refuse (a fence veto or an identity lease names one), so
// a refusal misread as name-specific cannot loop.
func (p *decidePass) realizeRequest(cfgAgent *config.Agent, qualifiedName string, prefer *session.Info, request SessionRequest, used map[string]bool, usedSlots map[int]bool) {
	tries, unlimited, _, _ := freshPoolSlotUpperBound(cfgAgent)
	if unlimited {
		tries = len(p.in.CreateVetoes) + len(p.in.Census.Rows) + 1
	}
	for ; ; tries-- {
		info, slot, plan, err := selectOrPlanPoolSessionBead(p.bp, cfgAgent, qualifiedName, prefer, request, p.in.Now, used, usedSlots)
		if err != nil {
			p.refuse(qualifiedName, "", rowKey{}, planErrorCause(err))
			return
		}
		if plan == nil {
			if !used[info.ID] {
				used[info.ID] = true
				p.selectPoolRow(cfgAgent, info, slot, request)
			}
			return
		}
		if ok, retry := p.admitPoolPlan(createPool, cfgAgent, *plan, request); ok || !retry || tries <= 1 || cfgAgent.UsesCanonicalSingletonPoolIdentity() {
			return
		}
	}
}

// selectPoolRow selects a reused pool row (phase C without its effects): its
// config ref, its binding candidate, and its desired-state membership.
func (p *decidePass) selectPoolRow(cfgAgent *config.Agent, info session.Info, slot int, request SessionRequest) {
	k, ok := p.byID[info.ID]
	if !ok {
		return
	}
	ref := desiredConfigRef{ConfigRev: p.in.ConfigRev, AgentTemplate: cfgAgent.QualifiedName()}
	var resolveAgent *config.Agent
	if isManualSessionInfoForAgent(info, cfgAgent) {
		ref.ResolveKind, ref.ManualSession = resolveManual, true
		ref.QualifiedInstance = sessionBeadQualifiedNameInfo(p.in.CityPath, cfgAgent, p.cfg.Rigs, info)
		resolveAgent = sessionBeadConfigAgent(cfgAgent, ref.QualifiedInstance)
		ref.Alias = strings.TrimSpace(info.Alias)
		ref.InstanceName = firstNonEmpty(ref.QualifiedInstance, info.SessionNameMetadata)
	} else {
		resolveAgent, ref.QualifiedInstance, ref.PoolSlot = poolDesiredRequestIdentity(cfgAgent, slot)
		ref.ResolveKind = resolveInstance
		if ref.PoolSlot == 0 {
			ref.ResolveKind = resolveBase
		}
		ref.InstanceName = ref.QualifiedInstance
		ref.TransientSlot = usesTransientPoolSlotIdentity(cfgAgent)
		if !ref.TransientSlot {
			ref.Alias = ref.QualifiedInstance
		}
	}
	sel := &selection{
		ref:       ref,
		resume:    request.Tier == "resume" && request.SessionBeadID == info.ID,
		normalize: needsNormalize(cfgAgent, info),
	}
	// Only a start candidate is bound (AM2): the binding is computed as
	// legacy's bind computes it, from the plan-only work dir (worktree.Verify
	// moves to the session key that applies it). Unusable evidence leaves a
	// start candidate out of desired, as legacy skips the item; refused
	// evidence (#34) is unusable while its verdict stands. A live row, or one
	// whose liveness is unknown, keeps its selection and gets no binding.
	if p.snap.Entries[k].Liveness.startCandidate() {
		workDir, err := verifiedPoolTriggerWorkDir(p.bp, cfgAgent, ref.QualifiedInstance, request)
		if err == nil && p.worktreeRefused(request) {
			err = errPoolTriggerWorktreeEvidence
		}
		if err != nil {
			p.refuse(cfgAgent.QualifiedName(), ref.QualifiedInstance, k, gateWorktreeRefused)
			return
		}
		if patch := computePoolTriggerBindingPatch(info, request, workDir); len(patch) > 0 {
			sel.binding = bindingOf(request, workDir)
		}
	}
	p.selected[k] = sel
	p.desired[info.SessionNameMetadata] = TemplateParams{
		TemplateName: templateNameFor(resolveAgent, ref.QualifiedInstance),
		InstanceName: ref.InstanceName,
		Alias:        ref.Alias,
	}
}

// admitPoolPlan runs the plan-time gates on a fresh pool or dependency plan
// and records it, with its reservation and desired-state membership, when
// they pass (POOL-047/048/050/052, C7.3, F8). It reports whether the plan
// was admitted and, if not, whether the refusal is specific to the plan's
// name, so another slot may pass: a create backoff whose cause is the fence
// (the name was taken), or an identity lease a dead or absent row holds.
// A template-wide create failure and a #46 quarantine refuse the request.
func (p *decidePass) admitPoolPlan(kind createKind, cfgAgent *config.Agent, plan poolSessionCreatePlan, request SessionRequest) (admitted, nameRefused bool) {
	template := cfgAgent.QualifiedName()
	refuse := func(cause string, identity bool) (bool, bool) {
		p.refuse(template, plan.qualifiedInstance, rowKey{}, cause)
		return false, identity
	}
	ap := allocPlan{
		Kind: kind, Template: template, Plan: plan, Request: request,
		Endpoint: endpointKeyForAgent(p.cfg, cfgAgent, session.Info{}),
	}
	switch {
	case p.censusIncomplete():
		return refuse(gateCensusIncomplete, false)
	case p.snap.Partial.Templates[template].BlockCreate:
		return refuse(gateBlockCreate, false)
	case p.worktreeRefused(request):
		return refuse(gateWorktreeRefused, false)
	case p.endpointShut(ap.Endpoint):
		return refuse(gateEndpointShut, false)
	case p.createRefused(ap) != "":
		return refuse(gateCreateRefused+p.createRefused(ap), p.createRefused(ap) == createStageFence)
	}
	identifiers, err := p.planIdentifiers(cfgAgent, template, plan)
	if err != nil {
		return refuse(gateNoSlot, false)
	}
	agentName := plan.qualifiedInstance
	if identifiers.beadScoped {
		if holder, held := p.in.Census.IdentityLeaseHolder(p.cfg, template, agentName); held {
			if dup := p.in.Census.Rows[holder].DuplicateOf; dup != "" {
				holder.Leg = dup // the bead's runtime is observed on its canonical copy
			}
			return refuse(gateIdentityLease, p.obs[holder].Liveness.startCandidate())
		}
	}
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		switch readRuntimeName(p.in.Obs, identifiers.sessionName, p.in.Now, p.in.ObsMaxAge).state {
		case nameUnknown:
			return refuse(gateLivenessUnknown, false)
		case nameZombie, nameAlive:
			return refuse(gateNameOccupied, false)
		}
	}
	episodeKey := identifiers.sessionName
	if identifiers.beadScoped {
		episodeKey = boundSessionNameLength(poolIdentitySessionName(agentName, template) + poolRuntimeNameSuffix)
	}
	if p.quarantined(episodeKey) {
		return refuse(gateQuarantine, false)
	}
	// The plan takes no planning reservation in the pass: within a template
	// the planner's used slots keep its plans apart, and a dependency floor
	// plans only for a template with nothing desired, which a plan of the
	// pass makes desired below. A named plan does reserve: its identity is
	// another template's singleton alias.
	p.plans = append(p.plans, ap)
	p.roots[template] = true
	resolveAgent, _, _ := poolDesiredRequestIdentity(cfgAgent, plan.slot)
	p.desired["plan:"+ap.identity()] = TemplateParams{
		TemplateName:   templateNameFor(resolveAgent, plan.qualifiedInstance),
		InstanceName:   plan.qualifiedInstance,
		Alias:          plan.qualifiedInstance,
		DependencyOnly: kind == createDependency,
	}
	return true, false
}

// planIdentifiers derives a plan's identifiers as the create effect will
// (derivePoolSessionIdentifiers), so the pass checks the same identity lease
// and the same singleton runtime name.
func (p *decidePass) planIdentifiers(cfgAgent *config.Agent, template string, plan poolSessionCreatePlan) (poolSessionIdentifiers, error) {
	alias, err := p.bp.resolveTmuxAliasForAgent(cfgAgent)
	if err != nil {
		return poolSessionIdentifiers{}, err
	}
	identity := poolSessionCreateIdentity{
		AgentName:     plan.qualifiedInstance,
		Slot:          plan.slot,
		TransientSlot: usesTransientPoolSlotIdentity(cfgAgent),
	}
	return derivePoolSessionIdentifiers(p.cfg, template, identity, alias)
}

// planNamed is step 7 (POOL-039..042, P3-6b §3.1): a configured named
// session's canonical row is InDesired; with no canonical row, no
// conflicting holder and no create in flight for the identity, an always
// session (or an on_demand one with work) gets a named plan, once I3 gives
// a definite answer for its runtime name (AM-N6). The census is multi-leg,
// so a named row on any leg counts as canonical; legacy checks only the
// sessions store.
func (p *decidePass) planNamed() {
	// Identity losers never stand for their identity: the canonical lookup
	// takes the first claimant in census order.
	candidates := make([]session.Info, 0, len(p.occupancy))
	for _, info := range p.occupancy {
		if k, ok := p.byID[info.ID]; !ok || !p.losers[k] {
			candidates = append(candidates, info)
		}
	}
	for _, identity := range slices.Sorted(maps.Keys(p.named.specs)) {
		spec := p.named.specs[identity]
		template := namedSessionBackingTemplate(spec)
		if why := p.in.TransportRefused[spec.Agent.QualifiedName()]; why != "" {
			p.refuse(template, identity, rowKey{}, gateTransport)
			continue
		}
		if canonical, ok := session.FindCanonicalNamedSessionInfo(candidates, spec); ok {
			p.selectNamedRow(canonical, spec, identity)
			continue
		}
		if _, conflict := session.FindNamedSessionConflictInfo(candidates, spec); conflict {
			p.refuse(template, identity, rowKey{}, gateNamedConflict)
			continue
		}
		if spec.Mode != "always" && !p.named.workReady[identity] {
			continue
		}
		plan := &namedCreatePlan{
			Identity: identity, SessionName: spec.SessionName, Template: template,
			Mode: spec.Mode, BoundStepID: p.named.workBeadID[identity],
		}
		ap := allocPlan{
			Kind: createNamed, Template: template, Named: plan,
			Endpoint: endpointKeyForAgent(p.cfg, spec.Agent, session.Info{}),
		}
		if cause := p.namedGate(ap, spec); cause != "" {
			p.refuse(template, identity, rowKey{}, cause)
			continue
		}
		p.plans = append(p.plans, ap)
		p.reserve(planReservation{
			EntryID: "plan:" + ap.identity(), Template: template,
			NamedIdentity: identity, SessionName: spec.SessionName,
		})
		p.roots[template] = true
		p.desired["plan:"+ap.identity()] = TemplateParams{
			TemplateName: template, InstanceName: identity,
			Alias: identity, ConfiguredNamedIdentity: identity,
		}
	}
}

// namedGate returns why a named plan is refused, or "". It sets the plan's
// AdoptLive from I3 by the AM-N6 table.
func (p *decidePass) namedGate(ap allocPlan, spec namedSessionSpec) string {
	plan := ap.Named
	for _, r := range p.in.Reservations {
		if r.NamedIdentity == plan.Identity {
			return gateInFlight
		}
	}
	switch {
	case p.createRefused(ap) != "":
		return gateCreateRefused + p.createRefused(ap)
	case resolvePoolSlot(plan.Identity, plan.Template) > 0:
		return gatePoolSlotShaped
	case p.censusIncomplete():
		return gateCensusIncomplete
	case p.snap.Partial.Templates[plan.Template].BlockCreate:
		return gateBlockCreate
	case p.providerRed(spec.Agent):
		return gateProviderRed
	case p.quarantined(plan.SessionName):
		return gateQuarantine
	case p.endpointShut(ap.Endpoint):
		return gateEndpointShut
	}
	r := readRuntimeName(p.in.Obs, plan.SessionName, p.in.Now, p.in.ObsMaxAge)
	switch r.state {
	case nameUnknown:
		return gateLivenessUnknown
	case nameAbsent, nameCorpse, nameZombie:
		return ""
	}
	switch {
	case r.obs.OwnerState == OwnerNone, r.obs.OwnerState == OwnerUnknown && r.obs.Incarnation == "":
		plan.AdoptLive = true
		return ""
	case r.obs.OwnerState == OwnerUnknown:
		return gateOwnerPending
	}
	return gateNameHeld + r.obs.Owner.SessionID
}

// selectNamedRow selects a configured named session's canonical row, when
// the allocator manages it.
func (p *decidePass) selectNamedRow(info session.Info, spec namedSessionSpec, identity string) {
	k, ok := p.byID[info.ID]
	if !ok {
		return
	}
	template := namedSessionBackingTemplate(spec)
	p.selected[k] = &selection{ref: desiredConfigRef{
		ConfigRev:     p.in.ConfigRev,
		AgentTemplate: spec.Agent.QualifiedName(),
		ResolveKind:   resolveNamed,
		Alias:         identity,
		InstanceName:  identity,
		NamedIdentity: identity,
		NamedMode:     spec.Mode,
	}}
	p.desired[info.SessionNameMetadata] = TemplateParams{
		TemplateName: template, InstanceName: identity,
		Alias: identity, ConfiguredNamedIdentity: identity,
	}
}

// overlayAndFloors is step 8 (POOL-058/059, 037): the overlay adds open rows
// the config and pool passes did not select (manual, named, partial
// retention) by legacy's membership classification, without resolving
// templates; the templates of desired rows and plans are dependency-floor
// roots, and each root's dependencies get a floor member, reused or planned.
func (p *decidePass) overlayAndFloors() {
	for _, info := range p.decidable {
		k := p.byID[info.ID]
		v := classifyOverlaySession(p.in.CityPath, p.cfg, p.desired, info, p.in.SuspendedRigPaths,
			p.merged.PoolScaleCheckPartial, p.merged.NamedScaleCheckPartial, p.in.Now)
		if v.root {
			p.roots[v.template] = true
		}
		if !v.include || p.selected[k] != nil {
			continue
		}
		ref := desiredConfigRef{ConfigRev: p.in.ConfigRev, AgentTemplate: v.agent.QualifiedName()}
		var resolveAgent *config.Agent
		if isManualSessionInfoForAgent(info, v.agent) {
			ref.ResolveKind, ref.ManualSession = resolveManual, true
			ref.QualifiedInstance = sessionBeadQualifiedNameInfo(p.in.CityPath, v.agent, p.cfg.Rigs, info)
			resolveAgent = sessionBeadConfigAgent(v.agent, ref.QualifiedInstance)
			ref.Alias = strings.TrimSpace(info.Alias)
		} else {
			resolveAgent, ref.QualifiedInstance = canonicalSessionIdentityWithConfigInfo(p.cfg, v.agent, info)
			ref.ResolveKind = resolveInstance
		}
		ref.InstanceName = firstNonEmpty(ref.QualifiedInstance, info.SessionNameMetadata)
		if isNamedSessionInfo(info) {
			// A named row resolves to its own stored identity
			// (canonicalSessionIdentityWithConfigInfo).
			ref.ResolveKind = resolveNamed
			ref.NamedIdentity, ref.NamedMode = info.ConfiguredNamedIdentity, info.ConfiguredNamedMode
			ref.Alias = strings.TrimSpace(info.Alias)
		}
		p.selected[k] = &selection{ref: ref}
		p.desired[info.SessionNameMetadata] = TemplateParams{
			TemplateName:            templateNameFor(resolveAgent, ref.QualifiedInstance),
			InstanceName:            ref.InstanceName,
			Alias:                   ref.Alias,
			ConfiguredNamedIdentity: ref.NamedIdentity,
		}
	}
	visited := make(map[string]bool)
	var visit func(string)
	visit = func(template string) {
		if template == "" || visited[template] {
			return
		}
		visited[template] = true
		agent := findAgentByTemplate(p.cfg, template)
		if agent == nil {
			return
		}
		for _, dep := range agent.DependsOn {
			depAgent := findAgentByTemplate(p.cfg, dep)
			if depAgent == nil || depAgent.Suspended || agentInSuspendedRig(p.in.CityPath, depAgent, p.cfg.Rigs, p.in.SuspendedRigPaths) {
				continue
			}
			p.dependencyFloor(depAgent)
			visit(dep)
		}
	}
	for _, template := range slices.Sorted(maps.Keys(p.roots)) {
		visit(template)
	}
}

// dependencyFloor is ensureDependencyOnlyTemplate in planOnly mode: a
// dependency template with nothing desired and no create in flight gets a
// reusable dependency-only row, or a dependency plan (P3-5b admits it with
// the pool plans). An uncleared create of the template, floor or pool, will
// be a row of it, so a second plan would duplicate it across passes (F1).
// Its fresh slot is claimed against the pass's reservations, so it never
// names a slot a pool plan of the same pass took (P3-1 obligation).
func (p *decidePass) dependencyFloor(cfgAgent *config.Agent) {
	template := cfgAgent.QualifiedName()
	if !cfgAgent.SupportsGenericEphemeralSessions() || desiredHasTemplate(p.desired, template) ||
		slices.Contains(slices.Collect(maps.Values(p.standIns)), template) {
		return
	}
	if why := p.in.TransportRefused[template]; why != "" {
		p.refuse(template, "", rowKey{}, gateTransport)
		return
	}
	info, slot, plan, err := selectOrPlanDependencyPoolSessionBead(p.bp, cfgAgent, template, p.in.Now)
	if err != nil {
		p.refuse(template, "", rowKey{}, planErrorCause(err))
		return
	}
	if plan != nil {
		// The floor row is dependency-only from its create, as legacy's
		// same-tick sync stamps it, so the next pass reuses it.
		plan.metadata = map[string]string{"dependency_only": boolMetadata(true)}
		p.admitPoolPlan(createDependency, cfgAgent, *plan, SessionRequest{Template: template})
		return
	}
	k, ok := p.byID[info.ID]
	if !ok {
		return
	}
	resolveAgent, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(cfgAgent, slot)
	ref := desiredConfigRef{
		ConfigRev: p.in.ConfigRev, AgentTemplate: template, ResolveKind: resolveInstance,
		QualifiedInstance: qualifiedInstance, PoolSlot: poolSlot, InstanceName: info.SessionNameMetadata,
		DependencyOnly: true, TransientSlot: usesTransientPoolSlotIdentity(cfgAgent),
	}
	p.selected[k] = &selection{ref: ref, normalize: needsNormalize(cfgAgent, info)}
	p.desired[info.SessionNameMetadata] = TemplateParams{
		TemplateName: templateNameFor(resolveAgent, qualifiedInstance),
		InstanceName: info.SessionNameMetadata, DependencyOnly: true,
	}
}

// needsNormalize reports a canonical singleton row whose stored identity is
// a phantom slot spelling (POOL-046): the session key collapses it before
// start.
func needsNormalize(cfgAgent *config.Agent, info session.Info) bool {
	return cfgAgent.UsesCanonicalSingletonPoolIdentity() && !isManualSessionInfoForAgent(info, cfgAgent) &&
		!isNamedSessionInfo(info) && !nonExpandingPoolIdentityPatchInfo(cfgAgent, info).empty()
}

// bindings is step 14 (C6.1 as amended by AM2): a selected start candidate
// whose trigger differs from its target is bound to it; a live row, or one
// whose liveness is unknown, never is. A binding the previous snapshot
// published for a row still selected keeps its work and its ID while the
// work is still unclaimed demand, whichever request the row realized this
// pass, so a row's binding never flips between passes; claimed work and work
// a resume request names end the pairing (F2). Within a pass a work
// item is bound at most once; work the previous snapshot bound to a row
// still selected, or that a selected live row already carries as its
// trigger, is consumed (C6.3, C6.6). Binding IDs carry the epoch and the
// generation that first published them (C6.1).
func (p *decidePass) bindings() {
	// A sticky pairing holds only on unclaimed work (stickyBinding) that a
	// request of the pass still names; resume-tier requests name only
	// assigned work.
	demand := make(map[string]bool)
	for _, state := range p.poolStates {
		for _, r := range state.Requests {
			if id := strings.TrimSpace(r.WorkBeadID); id != "" {
				demand[id] = true
			}
		}
	}
	consumed := make(map[string]rowKey)
	for k := range p.selected {
		if p.snap.Entries[k].Liveness != livenessAlive {
			continue
		}
		if trigger := strings.TrimSpace(p.in.Census.Rows[k].Info.TriggerBeadID); trigger != "" {
			consumed[trigger] = k
		}
	}
	keys := slices.Collect(maps.Keys(p.selected))
	sortRowKeys(keys)
	// Sticky first: a pairing the previous snapshot published holds while
	// the row is still selected and the work is still demand, unless the
	// row now resumes its own claimed work. Once applied (the row's trigger
	// is the work), the row holds the work with no binding.
	held := make(map[rowKey]bool)
	for _, k := range keys {
		prev := p.stickyBinding(k)
		if prev == nil || p.selected[k].resume || !demand[prev.WorkBeadID] {
			continue
		}
		if holder, taken := consumed[prev.WorkBeadID]; taken && holder != k {
			continue
		}
		consumed[prev.WorkBeadID], held[k] = k, true
		if strings.TrimSpace(p.in.Census.Rows[k].Info.TriggerBeadID) != prev.WorkBeadID {
			b := *prev
			p.snap.Entries[k].Binding = &b
		}
	}
	for _, k := range keys {
		sel, e := p.selected[k], p.snap.Entries[k]
		if held[k] || sel.binding == nil {
			continue
		}
		b := *sel.binding
		if b.WorkBeadID != "" {
			if holder, taken := consumed[b.WorkBeadID]; taken && holder != k {
				continue
			}
			consumed[b.WorkBeadID] = k
		}
		// The same (row, work) pair keeps its ID (C6.1): a start whose
		// binding ID changed is abandoned.
		if prev := p.prevBinding(k); prev != nil && prev.WorkBeadID == b.WorkBeadID {
			b.ID = prev.ID
		} else {
			b.ID = fmt.Sprintf("bind:%s:%s/%s:%s@%d", p.in.Epoch, k.Leg, k.ID, b.WorkBeadID, p.in.SelGen)
		}
		e.Binding = &b
	}
}

// refuse traces a refused plan or skipped selection; it consumes nothing.
func (p *decidePass) refuse(template, instance string, k rowKey, cause string) {
	p.trace = append(p.trace, allocTraceRecord{Template: template, Instance: instance, Key: k, Reason: "ineligible:" + cause})
}

// createRefused returns the cause of a create veto live at Now for the
// plan's identity, or "".
func (p *decidePass) createRefused(ap allocPlan) string {
	if v, ok := p.in.CreateVetoes[ap.identity()]; ok && v.live(p.in.Now) {
		return firstNonEmpty(v.Cause, "unknown")
	}
	return ""
}

// worktreeRefused reports a request whose worktree evidence failed
// verification and whose verdict still stands (#34): the work item is
// throttled, never the slot.
func (p *decidePass) worktreeRefused(request SessionRequest) bool {
	if request.WorktreeSpec == nil {
		return false
	}
	spec, ok := p.in.WorktreeRefused[request.WorktreeSpec.BeadID]
	return ok && spec == *request.WorktreeSpec
}

// quarantined reports a #46 startup-health episode in quarantine at Now.
func (p *decidePass) quarantined(key string) bool {
	ep, ok := p.in.Episodes[key]
	return ok && ep.QuarantinedUntil.After(p.in.Now)
}

// endpointShut reports an endpoint whose breaker admits nothing. An empty
// key is unguarded.
func (p *decidePass) endpointShut(k endpointKey) bool {
	return k != "" && p.in.Endpoints[k].Gate == gateShut
}

// providerRed reports I7's verdict for an agent's provider, keyed by
// provider name as legacy's create gate keys it (AM9).
func (p *decidePass) providerRed(agent *config.Agent) bool {
	name := strings.TrimSpace(agent.Provider)
	if name == "" {
		name = strings.TrimSpace(agent.InheritedProvider)
	}
	if name == "" {
		name = strings.TrimSpace(p.cfg.Workspace.Provider)
	}
	healthy, present := p.bp.providerHealthSnapshot.check(name)
	return present && !healthy
}

// planErrorCause names the planner's refusal.
func planErrorCause(err error) string {
	switch {
	case errors.Is(err, errPoolSessionCreatePartial):
		return gatePartial
	case errors.Is(err, errPoolSessionCreateProviderRed):
		return gateProviderRed
	case errors.Is(err, errPoolSessionNameUnavailable):
		return gateNoSlot
	case errors.Is(err, errPoolTriggerWorktreeEvidence):
		return gateWorktreeRefused
	}
	return "plan-error"
}

// bindingOf is the binding target a request names.
func bindingOf(request SessionRequest, workDir string) *bindingTarget {
	b := &bindingTarget{
		WorkBeadID:     strings.TrimSpace(request.WorkBeadID),
		WorkStoreRef:   strings.TrimSpace(request.WorkStoreRef),
		WorkPack:       strings.TrimSpace(request.WorkPack),
		WorkWorkspace:  packWorkspaceSlug(request),
		BrainParentSID: strings.TrimSpace(request.BrainParentSID),
		WorkDir:        workDir,
	}
	if request.WorktreeSpec != nil {
		spec := *request.WorktreeSpec
		b.WorktreeSpec = &spec
	}
	return b
}
