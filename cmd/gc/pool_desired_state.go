package main

import (
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// SessionRequest represents a single session the reconciler should start.
type SessionRequest struct {
	Template     string // agent template qualified name (e.g., "gascity/claude")
	BeadPriority *int   // priority of the driving work bead; nil means P2
	// Tier is "resume" for in-progress work with a live session,
	// "wake-known-identity" for in-progress work whose session exited but
	// template is configured, or "new" for ready unassigned work.
	Tier          string
	SessionBeadID string    // concrete session to preserve for resume or in-flight new demand
	WorkBeadID    string    // the work bead driving this request
	WorkBeadTitle string    // title of the work bead driving this request, when known
	WorkCreatedAt time.Time // creation time used for FIFO within a priority band
	WorkPack      string    // pack route key from the work bead, when known
	WorkWorkspace string    // explicit pack workspace route key from the work bead, when known
	WorkStoreRef  string    // city or rig:<name> store reference for WorkBeadID when known
	// BrainParentSID is gc.brain_parent_sid from the driving work bead, when
	// set: the parent session to fork this launch off of (warm-arm fork-launch).
	BrainParentSID string
	// FloorGuarantee marks a "new" request created to satisfy an agent's
	// min_active_sessions floor (as opposed to elastic scale-check demand).
	// The per-tick create-budget allocator reserves a token for each
	// floor-bearing template before round-robining the remainder, so a cold
	// pool's floor spawn cannot be starved by a warm pool's large elastic
	// demand (follow-up to #2893).
	FloorGuarantee bool
}

func beadPriority(b beads.Bead) *int {
	if b.Priority == nil {
		return nil
	}
	priority := *b.Priority
	return &priority
}

// PoolDesiredState holds the desired state for a single agent template.
type PoolDesiredState struct {
	Template string
	Requests []SessionRequest // accepted requests (within all caps)
}

// ReconcileDecision is the output of the nested cap enforcement.
type ReconcileDecision struct {
	Start []SessionRequest // sessions to start
	// Stop is computed by the reconciler by comparing Start against running sessions.
}

func PoolDesiredCounts(states []PoolDesiredState) map[string]int {
	if len(states) == 0 {
		return nil
	}
	counts := make(map[string]int, len(states))
	for _, state := range states {
		counts[state.Template] = len(state.Requests)
	}
	return counts
}

// ComputePoolDesiredStates computes the desired state for all pool agents.
// assignedWorkBeads contains actionable assigned work beads only: in-progress
// work and open work that was already proven ready upstream. Routed but
// unassigned pool queue work must not be passed here; new-session demand comes
// from scale_check, while this function only preserves sessions that already
// own actionable work.
// Each bead's gc.routed_to determines which agent template it belongs to.
// scaleCheckCounts maps agent template → new session demand from scale_check.
// Pass nil for either when unavailable.
func ComputePoolDesiredStates(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, sessionInfos, scaleCheckCounts, nil, nil)
}

func ComputePoolDesiredStatesTraced(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, sessionInfos, scaleCheckCounts, nil, trace)
}

func ComputePoolDesiredStatesWithDemandTraced(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, sessionInfos, scaleCheckCounts, scaleCheckDemand, trace)
}

func computePoolDesiredStates(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	// Build reverse lookup: any identifier → session bead ID.
	// Assignee on work beads may be a bead ID, session name, alias, or
	// a prior alias preserved in alias_history. Resume-tier dispatch
	// drops in-progress work whose owning session can't be resolved
	// from this map, so missing identities cause live sessions to look
	// orphaned and let a duplicate spawn for the same bead.
	assigneeToSessionBeadID := make(map[string]string)
	sessionBeadTemplate := make(map[string]string)
	namedSessionBeadIDs := make(map[string]bool)
	for _, sb := range sessionInfos {
		if sb.Closed {
			continue
		}
		if sessionHasProviderTerminalErrorInfo(sb) {
			continue
		}
		template := strings.TrimSpace(normalizedSessionTemplateInfo(sb, cfg))
		if template != "" {
			sessionBeadTemplate[sb.ID] = template
		}
		for _, id := range sessionBeadAssigneeIdentitiesInfo(sb) {
			assigneeToSessionBeadID[id] = sb.ID
		}
		if isNamedSessionInfo(sb) {
			namedSessionBeadIDs[sb.ID] = true
		}
	}

	aliasHeldTemplates := canonicalSingletonAliasHeldTemplates(cfg, sessionInfos)

	var resumeRequests []SessionRequest
	// wakeRequestIndex maps a template to the position of its wake request in
	// resumeRequests, so a later bead the pool ranks higher can REPLACE the one
	// already recorded. Dedup runs before the canonical sort, so keeping the
	// first bead seen would let a P0 be dropped behind a P2 and make the template
	// compete for scarce capacity at the wrong band.
	wakeRequestIndex := make(map[string]int)
	wakePolicyFor := admissionPolicyForTemplates(cfg)

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		if !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		template := agent.QualifiedName()

		// Resume tier: actionable assigned work beads whose assignee resolves
		// to a non-closed session bead. These sessions must stay alive.
		for _, wb := range assignedWorkBeads {
			routedTo := routedToOrLegacyWorkflowTarget(wb)
			if wb.Status != "in_progress" && wb.Status != "open" {
				continue
			}
			assignee := strings.TrimSpace(wb.Assignee)
			if assignee == "" {
				continue
			}
			sessionBeadID := assigneeToSessionBeadID[assignee]
			if routedTo == "" && sessionBeadID != "" {
				routedTo = sessionBeadTemplate[sessionBeadID]
				if routedTo == "" && len(cfg.Agents) == 1 {
					routedTo = cfg.Agents[0].QualifiedName()
				}
			}
			routedTo = normalizeAgentTemplateIdentity(cfg, routedTo)
			if sessionBeadID != "" {
				sessionTemplate := strings.TrimSpace(sessionBeadTemplate[sessionBeadID])
				if sessionTemplate != "" && routedTo != "" && !agentTemplateIdentitiesEquivalent(cfg, routedTo, sessionTemplate) {
					continue
				}
			}
			if routedTo != template {
				continue
			}
			if sessionBeadID != "" {
				// Named-session beads are materialized by the named-session
				// loop in buildDesiredState, not by the pool path. Skipping
				// here prevents realizePoolDesiredSessions from renaming the
				// canonical named identity to a phantom "{name}-1" pool
				// instance — which would create two desired sessions for the
				// same agent even when max_active_sessions=1.
				if namedSessionBeadIDs[sessionBeadID] {
					continue
				}
				resumeRequests = append(resumeRequests, SessionRequest{
					Template:       template,
					BeadPriority:   beadPriority(wb),
					Tier:           "resume",
					SessionBeadID:  sessionBeadID,
					WorkBeadID:     wb.ID,
					WorkCreatedAt:  wb.CreatedAt,
					WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
					WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
					BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
				})
				continue
			}
			if !agentTemplateIdentitiesEquivalent(cfg, assignee, template) || !isKnownPoolTemplate(assignee, cfg) {
				// Assignee set but session closed/unknown and not a configured
				// pool template — orphaned work, not our job to respawn. The
				// identity-equivalence compare keeps work assigned under a
				// legacy bound form of this template eligible for the
				// wake-known-identity tier; the emitted request carries the
				// canonical template.
				continue
			}
			candidate := SessionRequest{
				Template:       template,
				BeadPriority:   beadPriority(wb),
				Tier:           "wake-known-identity",
				WorkBeadID:     wb.ID,
				WorkCreatedAt:  wb.CreatedAt,
				WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
				WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
				BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
			}
			if idx, ok := wakeRequestIndex[template]; ok {
				// One wake per template still, but it must represent the bead
				// this pool would actually claim next. Replacing the whole
				// request (not just its id) keeps the pack/workspace/brain-parent
				// context bound to the bead that won.
				if wakeCandidateLess(candidate, resumeRequests[idx], wakePolicyFor(template)) {
					resumeRequests[idx] = candidate
				}
				continue
			}
			wakeRequestIndex[template] = len(resumeRequests)
			resumeRequests = append(resumeRequests, candidate)
			if trace != nil {
				trace.RecordDecision(TraceSitePoolWakeKnownIdentity, TraceReasonAssignedWork, TraceOutcomeScheduled, template, "", traceRecordPayload{
					"tier":      "wake-known-identity",
					"work_bead": wb.ID,
				})
			}
		}
	}

	limits := newNestedCapLimits(cfg)
	usage := acceptedNestedCapUsage(limits, resumeRequests, cfg.SharedAdmissionPolicy())
	allRequests := append([]SessionRequest(nil), resumeRequests...)
	resumeSessionBeadIDs := make(map[string]struct{}, len(resumeRequests))
	for _, req := range resumeRequests {
		if req.SessionBeadID != "" {
			resumeSessionBeadIDs[req.SessionBeadID] = struct{}{}
		}
	}
	inFlightNewRequests := poolInFlightNewRequests(cfg, sessionInfos, resumeSessionBeadIDs)

	// Merge scale_check demand. In bead-backed reconciliation, scale_check is
	// the authoritative signal for new unassigned demand only; resume requests
	// are calculated independently from assigned work and must not be deducted
	// from that count. Pool-created sessions that have not claimed work yet
	// represent already-spent new demand, so they occupy the first new-demand
	// slots explicitly before anonymous creates are materialized.
	if len(scaleCheckCounts) > 0 {
		for i := range cfg.Agents {
			agent := &cfg.Agents[i]
			if agent.Suspended {
				continue
			}
			template := agent.QualifiedName()
			scaleCount, ok := scaleCheckCounts[template]
			if !ok {
				continue
			}
			if _, ok := aliasHeldTemplates[template]; ok {
				continue
			}
			newCount := capNewDemandCount(limits, usage, agent, scaleCount)
			recordNewDemandCapTrace(trace, template, agent, limits, usage, scaleCount, newCount)
			inFlight := inFlightNewRequests[template]
			inFlightCount := minInt(len(inFlight), newCount)
			if scaleCount > 0 && len(inFlight) > 0 && trace != nil {
				trace.RecordDecision(TraceSitePoolInFlightReuse, TraceReasonInFlightReuse, TraceOutcomeAccepted, template, "", traceRecordPayload{
					"scale_check":   scaleCount,
					"in_flight":     len(inFlight),
					"reused":        inFlightCount,
					"anonymous_new": newCount - inFlightCount,
				})
			}
			// Reused in-flight sessions are already bound to specific work, so
			// the demand they cover is removed by trigger identity rather than
			// by count. Advancing an index instead would let a session bound to
			// low-ranked work push the materializer past higher-ranked demand:
			// it would re-create the bead the session already owns and drop the
			// urgent bead's request metadata on the floor.
			boundWorkBeadIDs := make(map[string]struct{}, inFlightCount)
			for j := 0; j < inFlightCount; j++ {
				req := inFlight[j]
				if workBeadID := strings.TrimSpace(req.WorkBeadID); workBeadID != "" {
					boundWorkBeadIDs[workBeadID] = struct{}{}
				}
				allRequests = append(allRequests, req)
			}
			demand := scaleCheckDemand[template]
			remaining := unboundDemandWorkBeadIDs(demand, boundWorkBeadIDs)
			for j := 0; j < newCount-inFlightCount; j++ {
				workBeadID := ""
				if j < len(remaining) {
					workBeadID = remaining[j]
				}
				allRequests = append(allRequests, newDemandSessionRequest(template, workBeadID, demand))
			}
		}
	}

	return applyNestedCaps(cfg, allRequests, aliasHeldTemplates, trace)
}

// unboundDemandWorkBeadIDs returns demand's work bead IDs in rank order with
// every ID an in-flight session is already bound to removed. Fresh requests draw
// from this list in order, so the highest-ranked work no session already owns
// always gets materialized first. Blank IDs are dropped rather than allowed to
// occupy a slot: they carry no work to launch against.
func unboundDemandWorkBeadIDs(demand scaleCheckDemand, bound map[string]struct{}) []string {
	if len(demand.WorkBeadIDs) == 0 {
		return nil
	}
	remaining := make([]string, 0, len(demand.WorkBeadIDs))
	for _, id := range demand.WorkBeadIDs {
		workBeadID := strings.TrimSpace(id)
		if workBeadID == "" {
			continue
		}
		if _, ok := bound[workBeadID]; ok {
			continue
		}
		remaining = append(remaining, workBeadID)
	}
	return remaining
}

// newDemandSessionRequest materializes a fresh "new"-tier request for template,
// carrying workBeadID's launch context from demand. An empty workBeadID yields
// an anonymous request: scale_check reported demand that no bead in the demand
// map accounts for, so the slot is filled without work metadata.
func newDemandSessionRequest(template, workBeadID string, demand scaleCheckDemand) SessionRequest {
	req := SessionRequest{
		Template:   template,
		Tier:       "new",
		WorkBeadID: workBeadID,
	}
	if workBeadID == "" {
		return req
	}
	if demand.Titles != nil {
		req.WorkBeadTitle = strings.TrimSpace(demand.Titles[workBeadID])
	}
	if priority, ok := demand.Priorities[workBeadID]; ok {
		req.BeadPriority = &priority
	}
	req.WorkCreatedAt = demand.CreatedAt[workBeadID]
	if demand.Packs != nil {
		req.WorkPack = strings.TrimSpace(demand.Packs[workBeadID])
	}
	if demand.Workspaces != nil {
		req.WorkWorkspace = strings.TrimSpace(demand.Workspaces[workBeadID])
	}
	if demand.StoreRefs != nil {
		req.WorkStoreRef = strings.TrimSpace(demand.StoreRefs[workBeadID])
	}
	if demand.ParentSIDs != nil {
		req.BrainParentSID = strings.TrimSpace(demand.ParentSIDs[workBeadID])
	}
	return req
}

func canonicalSingletonAliasHeldTemplates(cfg *config.City, sessionInfos []sessionpkg.Info) map[string]struct{} {
	held := make(map[string]struct{})
	if cfg == nil {
		return held
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.UsesCanonicalSingletonPoolIdentity() {
			continue
		}
		template := agent.QualifiedName()
		for _, sb := range sessionInfos {
			// None of these own the canonical alias: a closed or drained named
			// session released it at close via the retire path, a pool-managed bead
			// never held it, and a failed-create bead released it via
			// failedCreateIdentityReleased (names.go). Counting any as a live holder
			// would suppress demand while the alias is actually free, hanging routed
			// work.
			if sb.Closed || isPoolManagedSessionInfo(sb) || isDrainedSessionInfo(sb) || isFailedCreateSessionInfo(sb) {
				continue
			}
			if strings.TrimSpace(sb.MetadataState) == "asleep" {
				continue
			}
			if strings.TrimSpace(sb.Alias) == template {
				held[template] = struct{}{}
				break
			}
		}
	}
	return held
}

func poolInFlightNewRequests(cfg *config.City, sessionInfos []sessionpkg.Info, resumeSessionBeadIDs map[string]struct{}) map[string][]SessionRequest {
	requests := make(map[string][]SessionRequest)
	sortedSessionInfos := append([]sessionpkg.Info(nil), sessionInfos...)
	sort.SliceStable(sortedSessionInfos, func(i, j int) bool {
		if !sortedSessionInfos[i].CreatedAt.Equal(sortedSessionInfos[j].CreatedAt) {
			return sortedSessionInfos[i].CreatedAt.Before(sortedSessionInfos[j].CreatedAt)
		}
		return sortedSessionInfos[i].ID < sortedSessionInfos[j].ID
	})
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		template := agent.QualifiedName()
		for _, sb := range sortedSessionInfos {
			if sb.ID == "" || sb.Closed {
				continue
			}
			if sessionHasProviderTerminalErrorInfo(sb) {
				continue
			}
			if _, ok := resumeSessionBeadIDs[sb.ID]; ok {
				continue
			}
			if !isEphemeralSessionInfoForAgent(sb, agent) || !isPoolManagedSessionInfo(sb) {
				continue
			}
			if normalizedSessionTemplateInfo(sb, cfg) != template {
				continue
			}
			if !poolSessionConsumesNewDemandInfo(sb) {
				continue
			}
			requests[template] = append(requests[template], SessionRequest{
				Template:       template,
				Tier:           "new",
				SessionBeadID:  sb.ID,
				WorkBeadID:     strings.TrimSpace(sb.TriggerBeadID),
				WorkStoreRef:   strings.TrimSpace(sb.TriggerBeadStoreRef),
				BrainParentSID: strings.TrimSpace(sb.BrainParentSID),
			})
		}
	}
	return requests
}

// poolSessionConsumesNewDemandInfo reports whether a pool session already
// represents spent "new" demand: it holds an active pending_create_claim, or
// its raw state is creating/start-pending. It reads PendingCreateClaim and the
// raw MetadataState. This pure desired-state pass has no reconciler clock:
// creating sessions still represent already-spent new demand; lifecycle code
// owns stale-creating recovery with its clock-aware predicate.
func poolSessionConsumesNewDemandInfo(info sessionpkg.Info) bool {
	if info.PendingCreateClaim {
		return true
	}
	state := strings.TrimSpace(info.MetadataState)
	return state == "creating" || state == string(sessionpkg.StateStartPending)
}

// applyNestedCaps enforces workspace, rig, and agent max_active_sessions caps.
// Preserves recovery requests first, then admits fresh capacity in the order the
// city's shared admission policy selects, rejecting any request that would
// exceed a cap. The city value arbitrates because these caps are shared: the
// competing requests can come from pools with different per-pool policies.
func applyNestedCaps(cfg *config.City, requests []SessionRequest, aliasHeldTemplates map[string]struct{}, trace *sessionReconcilerTraceCycle) []PoolDesiredState {
	sortedRequests := append([]SessionRequest(nil), requests...)
	sortSessionRequests(sortedRequests, cfg.SharedAdmissionPolicy())

	limits := newNestedCapLimits(cfg)
	usage := newNestedCapUsage()

	// Walk sorted requests, accepting each if all caps have room.
	accepted := make(map[string][]SessionRequest) // template → accepted requests

	for _, req := range sortedRequests {
		template := req.Template
		if usage.isDuplicateSessionRequest(req) {
			continue
		}
		if site, reason, payload, rejected := usage.rejection(req, limits); rejected {
			if trace != nil {
				trace.RecordDecision(site, reason, TraceOutcomeRejected, template, "", payload)
			}
			continue
		}

		// Accept.
		accepted[template] = append(accepted[template], req)
		if trace != nil {
			trace.RecordDecision(TraceSitePoolAccept, TraceReasonCap, TraceOutcomeAccepted, template, "", traceRecordPayload{
				"tier": req.Tier,
			})
		}
		usage.accept(req, limits)
	}

	// Fill agent mins (if caps allow).
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		template := agent.QualifiedName()
		minSess := agent.EffectiveMinActiveSessions()
		if _, ok := aliasHeldTemplates[template]; ok {
			continue
		}
		for usage.agentCount[template] < minSess {
			req := SessionRequest{
				Template:       template,
				Tier:           "new",
				FloorGuarantee: true,
			}
			if _, _, _, rejected := usage.rejection(req, limits); rejected {
				break
			}
			accepted[template] = append(accepted[template], req)
			if trace != nil {
				trace.RecordDecision(TraceSitePoolMinFill, TraceReasonMinFill, TraceOutcomeAccepted, template, "", traceRecordPayload{
					"min":     minSess,
					"current": usage.agentCount[template],
					"tier":    "new",
				})
			}
			usage.accept(req, limits)
		}
	}

	// Build output.
	var result []PoolDesiredState
	for template, reqs := range accepted {
		result = append(result, PoolDesiredState{
			Template: template,
			Requests: reqs,
		})
	}
	// Stable output order.
	sort.Slice(result, func(i, j int) bool {
		return result[i].Template < result[j].Template
	})
	return result
}

type nestedCapLimits struct {
	workspaceMax int
	rigMax       map[string]int
	agentMax     map[string]int
	agentRig     map[string]string
}

type nestedCapUsage struct {
	agentCount      map[string]int
	rigCount        map[string]int
	workspaceCount  int
	seenSessionBead map[string]bool
	requests        []SessionRequest
}

func newNestedCapLimits(cfg *config.City) nestedCapLimits {
	limits := nestedCapLimits{
		workspaceMax: -1,
		rigMax:       make(map[string]int),
		agentMax:     make(map[string]int),
		agentRig:     make(map[string]string),
	}
	if cfg.Workspace.MaxActiveSessions != nil {
		limits.workspaceMax = *cfg.Workspace.MaxActiveSessions
	}
	for _, rig := range cfg.Rigs {
		if rig.MaxActiveSessions != nil {
			limits.rigMax[rig.Name] = *rig.MaxActiveSessions
		} else {
			limits.rigMax[rig.Name] = -1
		}
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		template := agent.QualifiedName()
		limits.agentRig[template] = agent.Dir
		resolved := agent.ResolvedMaxActiveSessions(cfg)
		if resolved != nil {
			limits.agentMax[template] = *resolved
		} else {
			limits.agentMax[template] = -1
		}
	}
	return limits
}

func newNestedCapUsage() nestedCapUsage {
	return nestedCapUsage{
		agentCount:      make(map[string]int),
		rigCount:        make(map[string]int),
		seenSessionBead: make(map[string]bool),
	}
}

// wakeCandidateLess reports whether wake request a represents work the pool
// admits before b's, under policy. Wake requests always carry a real work bead
// (id and creation time), so the bead comparator applies directly.
func wakeCandidateLess(a, b SessionRequest, policy beads.AdmissionPolicy) bool {
	return beads.LessFunc(policy)(
		beads.Bead{ID: a.WorkBeadID, Priority: a.BeadPriority, CreatedAt: a.WorkCreatedAt},
		beads.Bead{ID: b.WorkBeadID, Priority: b.BeadPriority, CreatedAt: b.WorkCreatedAt},
	)
}

func acceptedNestedCapUsage(limits nestedCapLimits, requests []SessionRequest, policy beads.AdmissionPolicy) nestedCapUsage {
	usage := newNestedCapUsage()
	sorted := append([]SessionRequest(nil), requests...)
	sortSessionRequests(sorted, policy)
	for _, req := range sorted {
		if usage.canAccept(req, limits) {
			usage.accept(req, limits)
		}
	}
	return usage
}

// sortSessionRequests orders competing session requests for scarce capacity.
//
// Recovery of committed capacity always sorts first, ahead of any policy
// comparison, so no running work is displaced by newer or more urgent work
// under any policy. Only the ordering among peers is policy-dependent.
//
// This deliberately mirrors beads.LessFunc rather than delegating to it: a
// SessionRequest may carry no WorkCreatedAt or WorkBeadID, which the Bead
// comparator has no notion of. The two must stay in step by band semantics,
// not by shared code.
func sortSessionRequests(requests []SessionRequest, policy beads.AdmissionPolicy) {
	sort.SliceStable(requests, func(i, j int) bool {
		return sessionRequestLess(requests[i], requests[j], policy)
	})
}

// sessionRequestLess reports whether left outranks right for a scarce slot
// under policy. It is the comparator sortSessionRequests applies, and it is a
// strict weak ordering: every comparison either separates the two requests or
// falls through to one that does, ending at a total tie-break on stable
// identity.
//
// A request that carries no work bead is ordered by sentinel rather than by
// skipping the comparison. Skipping made an anonymous request compare equal to
// every peer in its band while those peers still ordered among themselves, so
// equivalence was not transitive and sort.SliceStable returned input-order
// dependent results — a newer bead could hold a slot ahead of an older one.
func sessionRequestLess(left, right SessionRequest, policy beads.AdmissionPolicy) bool {
	leftRecovery := preservesCommittedCapacity(left)
	rightRecovery := preservesCommittedCapacity(right)
	if leftRecovery != rightRecovery {
		return leftRecovery
	}
	if leftRecovery {
		leftResume := isResumeLikeTier(left.Tier)
		rightResume := isResumeLikeTier(right.Tier)
		if leftResume != rightResume {
			return leftResume
		}
	}
	if policy.HasPriorityBands() {
		leftPriority := beads.PriorityValue(left.BeadPriority)
		rightPriority := beads.PriorityValue(right.BeadPriority)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
	}
	// Sentinel ordering for missing work identity: a request with no
	// timestamp, and then a request with no work bead ID, sorts after every
	// peer that has one. An anonymous request is scale_check demand that could
	// not be attributed to a bead, so it is the least specific claim on the
	// slot and yields to work we can name. The sentinel is explicit because
	// the zero time.Time would otherwise sort as the oldest possible reading
	// and put anonymous requests first.
	if left.WorkCreatedAt.IsZero() != right.WorkCreatedAt.IsZero() {
		return right.WorkCreatedAt.IsZero()
	}
	if !left.WorkCreatedAt.Equal(right.WorkCreatedAt) {
		return left.WorkCreatedAt.Before(right.WorkCreatedAt)
	}
	if (left.WorkBeadID == "") != (right.WorkBeadID == "") {
		return right.WorkBeadID == ""
	}
	if left.WorkBeadID != right.WorkBeadID {
		return left.WorkBeadID < right.WorkBeadID
	}
	// Total tie-break on stable identity. Without it two distinct requests
	// could compare equal in both directions and leave the sort input-order
	// dependent again; both fields are stable across ticks, so the resulting
	// order is reproducible.
	if left.Template != right.Template {
		return left.Template < right.Template
	}
	return left.SessionBeadID < right.SessionBeadID
}

func preservesCommittedCapacity(request SessionRequest) bool {
	return isResumeLikeTier(request.Tier) || (request.Tier == "new" && request.SessionBeadID != "")
}

func capNewDemandCount(limits nestedCapLimits, usage nestedCapUsage, agent *config.Agent, demand int) int {
	if demand <= 0 {
		return 0
	}
	template := agent.QualifiedName()
	remaining := demand
	if agentMax := limits.agentMax[template]; agentMax >= 0 {
		remaining = minInt(remaining, agentMax-usage.agentCount[template])
	}
	if rig := limits.agentRig[template]; rig != "" {
		rigMax, ok := limits.rigMax[rig]
		if !ok {
			rigMax = -1
		}
		if rigMax >= 0 {
			remaining = minInt(remaining, rigMax-usage.rigCount[rig])
		}
	}
	if limits.workspaceMax >= 0 {
		remaining = minInt(remaining, limits.workspaceMax-usage.workspaceCount)
	}
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (u nestedCapUsage) canAccept(req SessionRequest, limits nestedCapLimits) bool {
	if u.isDuplicateSessionRequest(req) {
		return false
	}
	_, _, _, rejected := u.rejection(req, limits)
	return !rejected
}

func (u nestedCapUsage) isDuplicateSessionRequest(req SessionRequest) bool {
	return req.SessionBeadID != "" && u.seenSessionBead[req.SessionBeadID]
}

func (u nestedCapUsage) rejection(req SessionRequest, limits nestedCapLimits) (TraceSiteCode, TraceReasonCode, traceRecordPayload, bool) {
	template := req.Template
	if agentMax := limits.agentMax[template]; agentMax >= 0 && u.agentCount[template] >= agentMax {
		return TraceSitePoolAgentCap, TraceReasonAgentCap, traceRecordPayload{
			"agent_max": agentMax,
			"current":   u.agentCount[template],
			"tier":      req.Tier,
		}, true
	}
	rig := limits.agentRig[template]
	if rig != "" {
		rigMax, ok := limits.rigMax[rig]
		if !ok {
			rigMax = -1
		}
		if rigMax >= 0 && u.rigCount[rig] >= rigMax {
			return TraceSitePoolRigCap, TraceReasonRigCap, traceRecordPayload{
				"rig":     rig,
				"rig_max": rigMax,
				"current": u.rigCount[rig],
				"tier":    req.Tier,
			}, true
		}
	}
	if limits.workspaceMax >= 0 && u.workspaceCount >= limits.workspaceMax {
		return TraceSitePoolWorkspaceCap, TraceReasonWorkspaceCap, traceRecordPayload{
			"workspace_max": limits.workspaceMax,
			"current":       u.workspaceCount,
			"tier":          req.Tier,
		}, true
	}
	return "", "", nil, false
}

func (u *nestedCapUsage) accept(req SessionRequest, limits nestedCapLimits) {
	u.agentCount[req.Template]++
	if rig := limits.agentRig[req.Template]; rig != "" {
		u.rigCount[rig]++
	}
	u.workspaceCount++
	if req.SessionBeadID != "" {
		u.seenSessionBead[req.SessionBeadID] = true
	}
	u.requests = append(u.requests, req)
}

func recordNewDemandCapTrace(
	trace *sessionReconcilerTraceCycle,
	template string,
	agent *config.Agent,
	limits nestedCapLimits,
	usage nestedCapUsage,
	scaleCount int,
	newCount int,
) {
	if trace == nil || scaleCount <= 0 || newCount >= scaleCount {
		return
	}
	site, reason, capMax, current, blockers := newDemandBlockingScope(template, agent, limits, usage, newCount)
	if site == "" {
		return
	}
	blockingSessions := make([]string, 0, len(blockers))
	blockingWork := make([]string, 0, len(blockers))
	for _, req := range blockers {
		if req.SessionBeadID != "" {
			blockingSessions = append(blockingSessions, req.SessionBeadID)
		}
		if req.WorkBeadID != "" {
			blockingWork = append(blockingWork, req.WorkBeadID)
		}
	}
	trace.RecordDecision(site, reason, TraceOutcomeRejected, template, "", traceRecordPayload{
		"scale_check":          scaleCount,
		"accepted_new":         newCount,
		"blocked_new":          scaleCount - newCount,
		"current":              current,
		"max":                  capMax,
		"blocking_sessions":    blockingSessions,
		"blocking_work_beads":  blockingWork,
		"active_capacity_kind": string(reason),
	})
}

func newDemandBlockingScope(
	template string,
	agent *config.Agent,
	limits nestedCapLimits,
	usage nestedCapUsage,
	newCount int,
) (TraceSiteCode, TraceReasonCode, int, int, []SessionRequest) {
	if agentMax := limits.agentMax[template]; agentMax >= 0 && agentMax-usage.agentCount[template] <= newCount {
		return TraceSitePoolNewDemandCap, TraceReasonAgentCap, agentMax, usage.agentCount[template], filterCapBlockers(usage.requests, func(req SessionRequest) bool {
			return req.Template == template
		})
	}
	if agent != nil {
		if rig := limits.agentRig[template]; rig != "" {
			rigMax, ok := limits.rigMax[rig]
			if !ok {
				rigMax = -1
			}
			if rigMax >= 0 && rigMax-usage.rigCount[rig] <= newCount {
				return TraceSitePoolNewDemandCap, TraceReasonRigCap, rigMax, usage.rigCount[rig], filterCapBlockers(usage.requests, func(req SessionRequest) bool {
					return limits.agentRig[req.Template] == rig
				})
			}
		}
	}
	if limits.workspaceMax >= 0 && limits.workspaceMax-usage.workspaceCount <= newCount {
		return TraceSitePoolNewDemandCap, TraceReasonWorkspaceCap, limits.workspaceMax, usage.workspaceCount, usage.requests
	}
	return "", "", 0, 0, nil
}

func filterCapBlockers(requests []SessionRequest, keep func(SessionRequest) bool) []SessionRequest {
	out := make([]SessionRequest, 0, len(requests))
	for _, req := range requests {
		if keep(req) {
			out = append(out, req)
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func isKnownPoolTemplate(assignee string, cfg *config.City) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" || cfg == nil {
		return false
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		if agentTemplateIdentitiesEquivalent(cfg, assignee, agent.QualifiedName()) {
			return true
		}
	}
	return false
}

func isResumeLikeTier(tier string) bool {
	return tier == "resume" || tier == "wake-known-identity"
}
