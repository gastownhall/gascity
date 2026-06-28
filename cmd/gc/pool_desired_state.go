package main

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// SessionRequest represents a single session the reconciler should start.
type SessionRequest struct {
	Template     string // agent template qualified name (e.g., "gascity/claude")
	BeadPriority int    // priority of the driving work bead
	// Tier is "resume" for in-progress work with a live session,
	// "wake-known-identity" for in-progress work whose session exited but
	// template is configured, or "new" for ready unassigned work.
	Tier          string
	SessionBeadID string // concrete session to preserve for resume or in-flight new demand
	WorkBeadID    string // the work bead driving this request
	WorkBeadTitle string // title of the work bead driving this request, when known
	WorkPack      string // pack route key from the work bead, when known
	WorkWorkspace string // explicit pack workspace route key from the work bead, when known
	WorkStoreRef  string // city or rig:<name> store reference for WorkBeadID when known
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

func beadPriority(b beads.Bead) int {
	if b.Priority != nil {
		return *b.Priority
	}
	return 0
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
	sessionBeads []beads.Bead,
	scaleCheckCounts map[string]int,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, sessionBeads, scaleCheckCounts, nil, nil)
}

func ComputePoolDesiredStatesTraced(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionBeads []beads.Bead,
	scaleCheckCounts map[string]int,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, sessionBeads, scaleCheckCounts, nil, trace)
}

func ComputePoolDesiredStatesWithDemandTraced(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionBeads []beads.Bead,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, sessionBeads, scaleCheckCounts, scaleCheckDemand, trace)
}

func computePoolDesiredStates(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionBeads []beads.Bead,
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
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		if sessionHasProviderTerminalError(sb) {
			continue
		}
		template := strings.TrimSpace(normalizedSessionTemplate(sb, cfg))
		if template != "" {
			sessionBeadTemplate[sb.ID] = template
		}
		for _, id := range sessionBeadAssigneeIdentities(sb) {
			assigneeToSessionBeadID[id] = sb.ID
		}
		if isNamedSessionBead(sb) {
			namedSessionBeadIDs[sb.ID] = true
		}
	}

	aliasHeldTemplates := canonicalSingletonAliasHeldTemplates(cfg, sessionBeads)

	var resumeRequests []SessionRequest
	wakeRequestedTemplates := make(map[string]struct{})

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
			if _, ok := wakeRequestedTemplates[template]; ok {
				continue
			}
			wakeRequestedTemplates[template] = struct{}{}
			resumeRequests = append(resumeRequests, SessionRequest{
				Template:       template,
				BeadPriority:   beadPriority(wb),
				Tier:           "wake-known-identity",
				WorkBeadID:     wb.ID,
				WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
				WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
				BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
			})
			if trace != nil {
				trace.recordDecision(string(TraceSitePoolWakeKnownIdentity), template, "", "assigned_work", "scheduled", traceRecordPayload{
					"tier":      "wake-known-identity",
					"work_bead": wb.ID,
				}, nil, "")
			}
		}
	}

	limits := newNestedCapLimits(cfg)
	usage := acceptedNestedCapUsage(limits, resumeRequests)
	allRequests := append([]SessionRequest(nil), resumeRequests...)
	resumeSessionBeadIDs := make(map[string]struct{}, len(resumeRequests))
	for _, req := range resumeRequests {
		if req.SessionBeadID != "" {
			resumeSessionBeadIDs[req.SessionBeadID] = struct{}{}
		}
	}
	inFlightNewRequests := poolInFlightNewRequests(cfg, sessionBeads, resumeSessionBeadIDs)

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
				trace.recordDecision(string(TraceSitePoolInFlightReuse), template, "", string(TraceReasonInFlightReuse), "accepted", traceRecordPayload{
					"scale_check":   scaleCount,
					"in_flight":     len(inFlight),
					"reused":        inFlightCount,
					"anonymous_new": newCount - inFlightCount,
				}, nil, "")
			}
			for j := 0; j < inFlightCount; j++ {
				req := inFlight[j]
				allRequests = append(allRequests, req)
				usage.accept(req, limits)
			}
			for j := inFlightCount; j < newCount; j++ {
				workBeadID := ""
				workBeadTitle := ""
				workPack := ""
				workWorkspace := ""
				workStoreRef := ""
				workParentSID := ""
				if demand := scaleCheckDemand[template]; len(demand.WorkBeadIDs) > j {
					workBeadID = strings.TrimSpace(demand.WorkBeadIDs[j])
					if demand.Titles != nil {
						workBeadTitle = strings.TrimSpace(demand.Titles[workBeadID])
					}
					if demand.Packs != nil {
						workPack = strings.TrimSpace(demand.Packs[workBeadID])
					}
					if demand.Workspaces != nil {
						workWorkspace = strings.TrimSpace(demand.Workspaces[workBeadID])
					}
					if demand.StoreRefs != nil {
						workStoreRef = strings.TrimSpace(demand.StoreRefs[workBeadID])
					}
					if demand.ParentSIDs != nil {
						workParentSID = strings.TrimSpace(demand.ParentSIDs[workBeadID])
					}
				}
				req := SessionRequest{
					Template:       template,
					Tier:           "new",
					WorkBeadID:     workBeadID,
					WorkBeadTitle:  workBeadTitle,
					WorkPack:       workPack,
					WorkWorkspace:  workWorkspace,
					WorkStoreRef:   workStoreRef,
					BrainParentSID: workParentSID,
				}
				allRequests = append(allRequests, req)
				usage.accept(req, limits)
			}
		}
	}

	return applyNestedCaps(cfg, allRequests, aliasHeldTemplates, trace)
}

func canonicalSingletonAliasHeldTemplates(cfg *config.City, sessionBeads []beads.Bead) map[string]struct{} {
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
		for _, sb := range sessionBeads {
			// None of these own the canonical alias: a closed or drained named
			// session released it at close via the retire path, a pool-managed bead
			// never held it, and a failed-create bead released it via
			// failedCreateIdentityReleased (names.go). Counting any as a live holder
			// would suppress demand while the alias is actually free, hanging routed
			// work.
			if sb.Status == "closed" || isPoolManagedSessionBead(sb) || isDrainedSessionBead(sb) || isFailedCreateSessionBead(sb) {
				continue
			}
			if strings.TrimSpace(sb.Metadata["state"]) == "asleep" {
				continue
			}
			if strings.TrimSpace(sb.Metadata["alias"]) == template {
				held[template] = struct{}{}
				break
			}
		}
	}
	return held
}

func poolInFlightNewRequests(cfg *config.City, sessionBeads []beads.Bead, resumeSessionBeadIDs map[string]struct{}) map[string][]SessionRequest {
	requests := make(map[string][]SessionRequest)
	sortedSessionBeads := append([]beads.Bead(nil), sessionBeads...)
	sort.SliceStable(sortedSessionBeads, func(i, j int) bool {
		if !sortedSessionBeads[i].CreatedAt.Equal(sortedSessionBeads[j].CreatedAt) {
			return sortedSessionBeads[i].CreatedAt.Before(sortedSessionBeads[j].CreatedAt)
		}
		return sortedSessionBeads[i].ID < sortedSessionBeads[j].ID
	})
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		template := agent.QualifiedName()
		for _, sb := range sortedSessionBeads {
			if sb.ID == "" || sb.Status == "closed" {
				continue
			}
			if sessionHasProviderTerminalError(sb) {
				continue
			}
			if _, ok := resumeSessionBeadIDs[sb.ID]; ok {
				continue
			}
			if !isEphemeralSessionBeadForAgent(sb, agent) || !isPoolManagedSessionBead(sb) {
				continue
			}
			if normalizedSessionTemplate(sb, cfg) != template {
				continue
			}
			if !poolSessionConsumesNewDemand(sb) {
				continue
			}
			requests[template] = append(requests[template], SessionRequest{
				Template:       template,
				Tier:           "new",
				SessionBeadID:  sb.ID,
				WorkBeadID:     strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadIDMetadataKey]),
				WorkStoreRef:   strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]),
				BrainParentSID: strings.TrimSpace(sb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
			})
		}
	}
	return requests
}

func poolSessionConsumesNewDemand(session beads.Bead) bool {
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) == boolMetadata(true) {
		return true
	}
	// This pure desired-state pass has no reconciler clock. Creating sessions
	// still represent already-spent new demand; lifecycle code owns stale
	// creating recovery with its clock-aware predicate.
	state := strings.TrimSpace(session.Metadata["state"])
	return state == "creating" || state == string(sessionpkg.StateStartPending)
}

// applyNestedCaps enforces workspace, rig, and agent max_active_sessions caps.
// Accepts requests in priority order, rejecting any that would exceed a cap.
func applyNestedCaps(cfg *config.City, requests []SessionRequest, aliasHeldTemplates map[string]struct{}, trace *sessionReconcilerTraceCycle) []PoolDesiredState {
	// Sort by priority DESC, resume tier first within same priority.
	sort.SliceStable(requests, func(i, j int) bool {
		if requests[i].BeadPriority != requests[j].BeadPriority {
			return requests[i].BeadPriority > requests[j].BeadPriority
		}
		// Resume-like tiers before new tier at same priority.
		if requests[i].Tier != requests[j].Tier {
			return isResumeLikeTier(requests[i].Tier) && !isResumeLikeTier(requests[j].Tier)
		}
		return false
	})

	limits := newNestedCapLimits(cfg)
	usage := newNestedCapUsage()

	// Walk sorted requests, accepting each if all caps have room.
	accepted := make(map[string][]SessionRequest) // template → accepted requests

	for _, req := range requests {
		template := req.Template
		if usage.isDuplicateSessionRequest(req) {
			continue
		}
		if site, reason, payload, rejected := usage.rejection(req, limits); rejected {
			if trace != nil {
				trace.recordDecision(site, template, "", reason, "rejected", payload, nil, "")
			}
			continue
		}

		// Accept.
		accepted[template] = append(accepted[template], req)
		if trace != nil {
			trace.recordDecision("reconciler.pool.accept", template, "", "cap", "accepted", traceRecordPayload{
				"tier": req.Tier,
			}, nil, "")
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
				trace.recordDecision("reconciler.pool.min_fill", template, "", "min_fill", "accepted", traceRecordPayload{
					"min":     minSess,
					"current": usage.agentCount[template],
					"tier":    "new",
				}, nil, "")
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

func acceptedNestedCapUsage(limits nestedCapLimits, requests []SessionRequest) nestedCapUsage {
	usage := newNestedCapUsage()
	sorted := append([]SessionRequest(nil), requests...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].BeadPriority != sorted[j].BeadPriority {
			return sorted[i].BeadPriority > sorted[j].BeadPriority
		}
		if sorted[i].Tier != sorted[j].Tier {
			return isResumeLikeTier(sorted[i].Tier) && !isResumeLikeTier(sorted[j].Tier)
		}
		return false
	})
	for _, req := range sorted {
		if usage.canAccept(req, limits) {
			usage.accept(req, limits)
		}
	}
	return usage
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

func (u nestedCapUsage) rejection(req SessionRequest, limits nestedCapLimits) (string, string, traceRecordPayload, bool) {
	template := req.Template
	if agentMax := limits.agentMax[template]; agentMax >= 0 && u.agentCount[template] >= agentMax {
		return "reconciler.pool.agent_cap", "agent_cap", traceRecordPayload{
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
			return "reconciler.pool.rig_cap", "rig_cap", traceRecordPayload{
				"rig":     rig,
				"rig_max": rigMax,
				"current": u.rigCount[rig],
				"tier":    req.Tier,
			}, true
		}
	}
	if limits.workspaceMax >= 0 && u.workspaceCount >= limits.workspaceMax {
		return "reconciler.pool.workspace_cap", "workspace_cap", traceRecordPayload{
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
	trace.recordDecision(site, template, "", reason, "rejected", traceRecordPayload{
		"scale_check":          scaleCount,
		"accepted_new":         newCount,
		"blocked_new":          scaleCount - newCount,
		"current":              current,
		"max":                  capMax,
		"blocking_sessions":    blockingSessions,
		"blocking_work_beads":  blockingWork,
		"active_capacity_kind": reason,
	}, nil, "")
}

func newDemandBlockingScope(
	template string,
	agent *config.Agent,
	limits nestedCapLimits,
	usage nestedCapUsage,
	newCount int,
) (string, string, int, int, []SessionRequest) {
	if agentMax := limits.agentMax[template]; agentMax >= 0 && agentMax-usage.agentCount[template] <= newCount {
		return string(TraceSitePoolNewDemandCap), string(TraceReasonAgentCap), agentMax, usage.agentCount[template], filterCapBlockers(usage.requests, func(req SessionRequest) bool {
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
				return string(TraceSitePoolNewDemandCap), string(TraceReasonRigCap), rigMax, usage.rigCount[rig], filterCapBlockers(usage.requests, func(req SessionRequest) bool {
					return limits.agentRig[req.Template] == rig
				})
			}
		}
	}
	if limits.workspaceMax >= 0 && limits.workspaceMax-usage.workspaceCount <= newCount {
		return string(TraceSitePoolNewDemandCap), string(TraceReasonWorkspaceCap), limits.workspaceMax, usage.workspaceCount, usage.requests
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
