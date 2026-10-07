package main

import (
	"errors"
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
	gateCreateRefused   = "create-refused:"
	gateWorktreeRefused = "worktree-refused"
	gateNameOccupied    = "name-occupied"
	gateLivenessUnknown = "liveness-unknown"
	gateQuarantine      = "startup-quarantine"
	gateEndpointShut    = "endpoint-open"
	gateProviderRed     = "provider-red"
	gatePartial         = "partial"
	gateTransport       = "transport-refused"
	gateNoSlot          = "no-slot"
	gateInFlight        = "in-flight"
	gateNamedConflict   = "named-conflict"
	gateOwnerPending    = "owner-pending"
	gateNameHeld        = "name-held:"
	gatePoolSlotShaped  = "named-identity-pool-slot-shaped"
)

// standInPrefix marks the ID of an in-flight create's stand-in row.
const standInPrefix = "pending-create:"

// plan is steps 4-8: demand, pool desired, the realization plan and the
// overlay.
func (p *decidePass) plan() {
	p.demand()
	p.computePoolDesired()
	p.newPlanParams()
	p.planNamed()
	p.realizePools()
	p.overlay()
}

// demand is step 4: the collected demand merged by legacy's rules, with the
// custom scale_check counts from I5 and the routed rows projected as
// legacy's in-tick route repair leaves them. The projection runs once, and
// every consumer reads its rows: control-dispatcher demand, the ready routed
// work the idle-claim nudge reads, and the default probe, which drops the
// control rows whose route the projection suppressed. Legacy's probe applies
// the same ownership rule through controlRowServableByTemplate (mc-zndi7.41),
// so this drop agrees with legacy and is not a difference for P3-5c.
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
			if p.in.ScaleCheck.partial(template) {
				collected.CustomPartials = markScaleCheckPartialTemplate(collected.CustomPartials, template)
			}
		}
	}
	projected, _ := projectControlDispatcherRoutes(p.cfg, collected.UnassignedRouted, collected.UnassignedRoutedRefs)
	collected.DefaultCounts, collected.DefaultDemand = withoutSuppressedRoutes(collected, projected)
	collected.UnassignedRouted = projected
	p.merged = mergeCollectedDemand(p.cfg, collected)
	for template := range p.merged.PoolScaleCheckPartial {
		p.markTemplate(template, true, "pool-scale-check-partial")
	}
	for template := range p.merged.PoolPartialRetention {
		p.markTemplate(template, true, "pool-partial-retention")
	}
	for template := range p.merged.NamedScaleCheckPartial {
		tp := p.snap.Partial.Templates[template]
		if !tp.Retain {
			p.markTemplate(template, false, "named-scale-check-partial")
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
// in-flight create whose row the census does not show yet (C5.13 row 1:
// in-flight demand is census ∪ the in-flight map).
func (p *decidePass) computePoolDesired() {
	rows := make([]session.Info, 0, len(p.decidable)+len(p.inFlight))
	rows = append(rows, p.decidable...)
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

// inFlightCreates is the planning reservation of each running or ambiguous
// create in the in-flight view whose token no census row carries, keyed by
// its token. Once a census row carries the token, that row holds the slot,
// the name and the work instead.
func (p *decidePass) inFlightCreates() []planReservation {
	tokens := make(map[string]bool)
	for _, row := range p.in.Census.Rows {
		if row.InstanceToken != "" {
			tokens[row.InstanceToken] = true
		}
	}
	var out []planReservation
	for _, e := range p.in.InFlight.Entries {
		if e.Kind != inflightCreate || tokens[e.Token] {
			continue
		}
		r := planReservation{ID: e.Token, Template: e.Template, QualifiedInstance: e.QualifiedInstance, Slot: e.Slot, WorkBeadID: e.WorkBeadID}
		if named, ok := strings.CutPrefix(e.Identity, "named:"); ok {
			r = planReservation{ID: e.Token, Template: e.Template, NamedIdentity: named, SessionName: e.SessionName}
		}
		out = append(out, r)
	}
	return out
}

// inFlightStandIns returns a pending-create row for each in-flight pool
// create the census does not show yet, stamped now: it is in flight now.
func (p *decidePass) inFlightStandIns() []session.Info {
	at := p.in.Now.UTC()
	var out []session.Info
	for _, r := range p.inFlight {
		if r.NamedIdentity != "" {
			continue
		}
		info := session.Info{
			ID:                     standInPrefix + r.ID,
			Template:               r.Template,
			AgentName:              r.QualifiedInstance,
			SessionOrigin:          "ephemeral",
			PoolManaged:            true,
			MetadataState:          string(session.StateStartPending),
			PendingCreateClaim:     true,
			PendingCreateStartedAt: at.Format(time.RFC3339Nano),
			CreatedAt:              at,
			TriggerBeadID:          r.WorkBeadID,
		}
		p.standIns[info.ID] = r.Template
		out = append(out, info)
	}
	return out
}

// newPlanParams builds the planner's plan-only build params from the census:
// reuse selects among the decidable rows, and fresh slots avoid every census
// row on every leg, the planning reservation of every in-flight create (C7.1
// tier 1), and every pool name a live create backoff from the fence refuses
// (F3): the name or its identity lease was taken, so the next free slot is
// planned instead, and the held name is traced (P-6). The planner runs
// unbudgeted: admission takes the plans in fair-share order. Its own
// census-completeness gate is off (no bead store: storeless builds read as
// complete): a partial non-sessions leg blocks no plan, and the create
// effect's locked live re-census fails closed instead (C2.8, C7.2).
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
	for _, r := range p.inFlight {
		p.reserve(r)
	}
	// A fence backoff's key is createBackoffKey(createIdentity.key()),
	// "create:<template>/<instance>"; C1b must keep that format. A prefix
	// match counts only when the instance is one of the agent's own slots,
	// so template "a" never claims "a/b"'s names.
	for _, key := range slices.Sorted(maps.Keys(p.in.Backoff)) {
		if r := p.in.Backoff[key]; r.Cause != createStageFence || !r.live(p.in.Now) {
			continue
		}
		for i := range p.cfg.Agents {
			a := &p.cfg.Agents[i]
			template := a.QualifiedName()
			instance, ok := strings.CutPrefix(key, createBackoffKey(template+"/"))
			held := session.Info{Template: template, AgentName: instance}
			if !ok || a.UsesCanonicalSingletonPoolIdentity() || existingPoolSlotWithConfigInfo(p.cfg, a, held) <= 0 {
				continue
			}
			p.reserve(planReservation{ID: "backoff:" + template + ":" + instance, Template: template, QualifiedInstance: instance})
			p.refuse(template, instance, rowKey{}, gateCreateRefused+createStageFence)
		}
	}
}

// reserve adds a planning reservation to the occupancy the planner claims
// fresh slots against, as the row it will become.
func (p *decidePass) reserve(r planReservation) {
	info := session.Info{
		ID:                 "reservation:" + r.ID,
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
// nothing to realize. Concrete requests come first, then the rest in order.
// A reused row is selected with its config ref and its binding candidate; a
// fresh plan must pass the plan-time gates.
func (p *decidePass) realizePools() {
	p.indexRealization()
	for _, state := range p.poolStates {
		cfgAgent := p.agentByTemplate(state.Template)
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
		bp := p.realizeParams(cfgAgent, state.Requests)
		used := make(map[string]bool)
		usedSlots := make(map[int]bool)
		for _, request := range state.Requests {
			if request.SessionBeadID == "" {
				continue
			}
			if p.standIns[request.SessionBeadID] != "" {
				continue
			}
			var prefer *session.Info
			if candidate, ok := p.bp.sessionBeads.FindInfoByID(request.SessionBeadID); ok {
				// A named row is never a pool instance (defense in depth,
				// as legacy).
				if isNamedSessionInfo(candidate) {
					continue
				}
				prefer = &candidate
			}
			p.realizeRequest(bp, cfgAgent, qualifiedName, prefer, request, used, usedSlots)
		}
		for _, request := range state.Requests {
			if request.SessionBeadID == "" {
				p.realizeRequest(bp, cfgAgent, qualifiedName, nil, request, used, usedSlots)
			}
		}
	}
}

// realizeRequest selects or plans one request over bp, the agent's
// realization params. A refusal stalls the request, as legacy's does
// (build_desired_state.go:5180).
func (p *decidePass) realizeRequest(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, prefer *session.Info, request SessionRequest, used map[string]bool, usedSlots map[int]bool) {
	info, slot, plan, err := selectOrPlanPoolSessionBead(bp, cfgAgent, qualifiedName, prefer, request, p.in.Now, used, usedSlots)
	switch {
	case err != nil:
		p.refuse(qualifiedName, "", rowKey{}, planErrorCause(err))
	case plan != nil:
		p.admitPoolPlan(cfgAgent, *plan, request)
	case !used[info.ID]:
		used[info.ID] = true
		p.selectPoolRow(cfgAgent, info, slot, request)
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
	sel := &selection{ref: ref, normalize: needsNormalize(cfgAgent, info)}
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

// admitPoolPlan runs the plan-time gates on a fresh pool plan and records it,
// with its desired-state membership, when they pass (POOL-047/048/050/052,
// C7.3, F8). A refusal refuses the request.
func (p *decidePass) admitPoolPlan(cfgAgent *config.Agent, plan poolSessionCreatePlan, request SessionRequest) {
	template := cfgAgent.QualifiedName()
	refuse := func(cause string) {
		p.refuse(template, plan.qualifiedInstance, rowKey{}, cause)
	}
	ap := allocPlan{
		Kind: createPool, Template: template, Plan: plan, Request: request,
		Endpoint: endpointKeyForAgent(p.cfg, cfgAgent, session.Info{}),
	}
	switch {
	case p.worktreeRefused(request):
		refuse(gateWorktreeRefused)
		return
	case p.endpointShut(ap.Endpoint):
		refuse(gateEndpointShut)
		return
	case p.createRefused(ap) != "":
		refuse(gateCreateRefused + p.createRefused(ap))
		return
	}
	identifiers, err := p.planIdentifiers(cfgAgent, template, plan)
	if err != nil {
		refuse(gateNoSlot)
		return
	}
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		switch readRuntimeName(p.in.Obs, identifiers.sessionName, p.in.Now, p.in.ObsMaxAge).state {
		case nameUnknown:
			refuse(gateLivenessUnknown)
			return
		case nameZombie, nameAlive:
			refuse(gateNameOccupied)
			return
		}
	}
	episodeKey := identifiers.sessionName
	if identifiers.beadScoped {
		episodeKey = boundSessionNameLength(poolIdentitySessionName(plan.qualifiedInstance, template) + poolRuntimeNameSuffix)
	}
	if p.quarantined(episodeKey) {
		refuse(gateQuarantine)
		return
	}
	// The plan takes no planning reservation in the pass: within a template
	// the planner's used slots keep its plans apart. A named plan does
	// reserve: its identity is another template's singleton alias.
	p.plans = append(p.plans, ap)
	resolveAgent, _, _ := poolDesiredRequestIdentity(cfgAgent, plan.slot)
	p.desired["plan:"+ap.identity()] = TemplateParams{
		TemplateName: templateNameFor(resolveAgent, plan.qualifiedInstance),
		InstanceName: plan.qualifiedInstance,
		Alias:        plan.qualifiedInstance,
	}
}

// planIdentifiers derives a plan's identifiers as the create effect will
// (derivePoolSessionIdentifiers), so the pass checks the same singleton
// runtime name and quarantine key.
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
	// Identity duplicates never stand for their identity: the canonical
	// lookup takes the first claimant in census order.
	candidates := make([]session.Info, 0, len(p.occupancy))
	for _, info := range p.occupancy {
		if k, ok := p.byID[info.ID]; !ok || p.none[k] != reasonIdentityDuplicate {
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
			ID: "plan:" + ap.identity(), Template: template,
			NamedIdentity: identity, SessionName: spec.SessionName,
		})
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
	for _, r := range p.inFlight {
		if r.NamedIdentity == plan.Identity {
			return gateInFlight
		}
	}
	switch {
	case p.createRefused(ap) != "":
		return gateCreateRefused + p.createRefused(ap)
	case resolvePoolSlot(plan.Identity, plan.Template) > 0:
		return gatePoolSlotShaped
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

// overlay is step 8 (POOL-059, 037): it adds open rows the config and pool
// passes did not select (manual, named, partial retention) by legacy's
// membership classification, without resolving templates. Dependency floors
// are gone: the latch refuses depends_on (C2.12).
func (p *decidePass) overlay() {
	for _, info := range p.decidable {
		k := p.byID[info.ID]
		v := classifyOverlaySession(p.in.CityPath, p.cfg, p.desired, info, p.in.SuspendedRigPaths,
			p.merged.PoolScaleCheckPartial, p.merged.NamedScaleCheckPartial, p.in.Now)
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
}

// needsNormalize reports a canonical singleton row whose stored identity is
// a phantom slot spelling (POOL-046): the session key collapses it before
// start.
func needsNormalize(cfgAgent *config.Agent, info session.Info) bool {
	return cfgAgent.UsesCanonicalSingletonPoolIdentity() && !isManualSessionInfoForAgent(info, cfgAgent) &&
		!isNamedSessionInfo(info) && !nonExpandingPoolIdentityPatchInfo(cfgAgent, info).empty()
}

// bindings is step 14 (C6 as amended by AM2 and S2-5): a selected start
// candidate whose trigger differs from its target is bound to it; a live row,
// or one whose liveness is unknown, never is. Bindings are recomputed every
// pass (C6.1). Within a pass a work item is bound at most once, and work a
// selected row that is alive or holds a start lease carries as its trigger is
// consumed (C6.3), by the first such row in key order. The lease is v5's
// (P4, S3): a pending-create claim or the creating state.
func (p *decidePass) bindings() {
	keys := slices.Collect(maps.Keys(p.selected))
	sortRowKeys(keys)
	consumed := make(map[string]rowKey)
	for _, k := range keys {
		info := p.in.Census.Rows[k].Info
		leased := info.PendingCreateClaim || info.MetadataState == string(session.StateCreating)
		trigger := strings.TrimSpace(info.TriggerBeadID)
		if _, taken := consumed[trigger]; trigger != "" && !taken && (p.snap.Entries[k].Liveness == livenessAlive || leased) {
			consumed[trigger] = k
		}
	}
	for _, k := range keys {
		sel := p.selected[k]
		if sel.binding == nil {
			continue
		}
		if id := sel.binding.WorkBeadID; id != "" {
			if holder, taken := consumed[id]; taken && holder != k {
				continue
			}
			consumed[id] = k
		}
		b := *sel.binding
		p.snap.Entries[k].Binding = &b
	}
}

// refuse traces a refused plan or skipped selection; it consumes nothing.
func (p *decidePass) refuse(template, instance string, k rowKey, cause string) {
	p.trace = append(p.trace, allocTraceRecord{Template: template, Instance: instance, Key: k, Reason: "ineligible:" + cause})
}

// createRefused returns the cause of a create backoff live at Now for the
// plan's identity, or "".
func (p *decidePass) createRefused(ap allocPlan) string {
	if r, ok := p.in.Backoff[createBackoffKey(ap.identity())]; ok && r.live(p.in.Now) {
		return firstNonEmpty(r.Cause, "unknown")
	}
	return ""
}

// worktreeRefused reports a request whose worktree evidence failed
// verification and whose backoff is live (#34): the work item is
// throttled, never the slot.
func (p *decidePass) worktreeRefused(request SessionRequest) bool {
	if request.WorktreeSpec == nil {
		return false
	}
	r, ok := p.in.Backoff[workBackoffKey(request.WorktreeSpec.BeadID)]
	return ok && r.live(p.in.Now) && r.Fingerprint == specFingerprint(*request.WorktreeSpec)
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
