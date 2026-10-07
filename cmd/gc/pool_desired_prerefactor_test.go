package main

// Frozen copies of the pool desired-state code A3 bucketed by template, taken
// from main at 0e1cb8bccd with comment lines and the trace-only branches
// removed and functions renamed. They are the "before" side of
// TestPoolDesiredIndexOracle and go away with the legacy reconciler.

import (
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

func computePoolDesiredStatesAtPreA3(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	decisionTime time.Time,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	assigneeToSessionBeadID := make(map[string]string)
	sessionBeadTemplate := make(map[string]string)
	namedSessionBeadIDs := make(map[string]bool)
	asleepSessionBeadIDs := make(map[string]bool)
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
		if sb.State == sessionpkg.StateAsleep {
			asleepSessionBeadIDs[sb.ID] = true
		}
	}

	aliasHeldTemplates := canonicalSingletonAliasHeldTemplates(cfg, sessionInfos)

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
			routedTo = normalizeAgentTemplateIdentity(cfg, agentutil.NormalizePoolRouteTarget(cfg, routedTo))
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
				if namedSessionBeadIDs[sessionBeadID] {
					continue
				}
				if agent.EffectiveWakeMode() == "fresh" && asleepSessionBeadIDs[sessionBeadID] {
					if _, ok := wakeRequestedTemplates[template]; ok {
						continue
					}
					wakeRequestedTemplates[template] = struct{}{}
					resumeRequests = append(resumeRequests, SessionRequest{
						Template:       template,
						BeadPriority:   beadPriorityRank(beadPriority(wb)),
						Tier:           "wake-known-identity",
						WorkBeadID:     wb.ID,
						WorkBeadTitle:  strings.TrimSpace(wb.Title),
						WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
						WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
						BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
					})
					continue
				}
				resumeRequests = append(resumeRequests, SessionRequest{
					Template:       template,
					BeadPriority:   beadPriorityRank(beadPriority(wb)),
					Tier:           "resume",
					SessionBeadID:  sessionBeadID,
					WorkBeadID:     wb.ID,
					WorkBeadTitle:  strings.TrimSpace(wb.Title),
					WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
					WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
					BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
				})
				continue
			}
			if isConfiguredNamedSessionIdentity(cfg, assignee) {
				continue
			}
			if !agentTemplateIdentitiesEquivalent(cfg, assignee, template) || !isKnownPoolTemplate(assignee, cfg) {
				continue
			}
			if _, ok := wakeRequestedTemplates[template]; ok {
				continue
			}
			wakeRequestedTemplates[template] = struct{}{}
			resumeRequests = append(resumeRequests, SessionRequest{
				Template:       template,
				BeadPriority:   beadPriorityRank(beadPriority(wb)),
				Tier:           "wake-known-identity",
				WorkBeadID:     wb.ID,
				WorkBeadTitle:  strings.TrimSpace(wb.Title),
				WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
				WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
				BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
			})
		}
	}

	limits := newNestedCapLimits(cfg)
	resumeSessionBeadIDs := make(map[string]struct{}, len(resumeRequests))
	for _, req := range resumeRequests {
		if req.SessionBeadID != "" {
			resumeSessionBeadIDs[req.SessionBeadID] = struct{}{}
		}
	}
	protectedNewRequests, inFlightNewRequests := poolNewDemandRequestsPreA3(cfg, sessionInfos, resumeSessionBeadIDs, decisionTime)
	sessionInfoByID := make(map[string]sessionpkg.Info, len(sessionInfos))
	for _, info := range sessionInfos {
		if info.ID == "" {
			continue
		}
		sessionInfoByID[info.ID] = info
	}
	for i := range resumeRequests {
		req := &resumeRequests[i]
		if req.Tier != "wake-known-identity" || req.SessionBeadID != "" {
			continue
		}
		candidates := protectedNewRequests[req.Template]
		if len(candidates) == 0 {
			continue
		}
		req.SessionBeadID = candidates[0].SessionBeadID
		protectedNewRequests[req.Template] = candidates[1:]
	}
	usage := acceptedNestedCapUsage(limits, resumeRequests)
	floorUsage := acceptedNestedCapUsage(limits, concreteNestedCapRequests(cfg, resumeRequests, protectedNewRequests, inFlightNewRequests))
	floorReservations := newNestedCapFloorReservations(cfg, aliasHeldTemplates, limits, floorUsage)
	allRequests := append([]SessionRequest(nil), resumeRequests...)

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		template := agent.QualifiedName()
		scaleCount, hasScaleCount := scaleCheckCounts[template]
		protected := protectedNewRequests[template]
		if !hasScaleCount && len(protected) == 0 {
			continue
		}
		if _, ok := aliasHeldTemplates[template]; ok {
			continue
		}
		inFlight := inFlightNewRequests[template]
		inFlightFloor := 0
		for _, req := range inFlight {
			if info, ok := sessionInfoByID[req.SessionBeadID]; ok && poolSessionWithinPendingCreateLease(info, cfg, decisionTime) {
				inFlightFloor++
			}
		}
		effectiveDemand := max(scaleCount, len(protected), inFlightFloor)
		newCount := capNewDemandCount(limits, usage, floorReservations, agent, effectiveDemand)
		recordNewDemandCapTrace(trace, template, agent, limits, usage, effectiveDemand, newCount)
		concreteLimit := concreteNestedCapLimit(limits, usage, template, newCount, effectiveDemand, len(protected)+len(inFlight))
		protectedCount := minInt(len(protected), concreteLimit)
		inFlightCount := minInt(len(inFlight), concreteLimit-protectedCount)
		reusedCount := protectedCount + inFlightCount
		selectedConcrete := make([]SessionRequest, 0, reusedCount)
		selectedConcrete = append(selectedConcrete, protected[:protectedCount]...)
		selectedConcrete = append(selectedConcrete, inFlight[:inFlightCount]...)
		demand := scaleCheckDemand[template]
		selectedConcrete, residualWorkBeadIDs := allocateScaleDemandToConcrete(demand, selectedConcrete)
		for _, req := range selectedConcrete {
			allRequests = append(allRequests, req)
			usage.accept(req, limits)
		}
		for j := 0; j < newCount-reusedCount; j++ {
			workBeadID := ""
			workBeadTitle := ""
			workPack := ""
			workWorkspace := ""
			workStoreRef := ""
			workParentSID := ""
			var worktreeSpec *worktree.Spec
			worktreeError := ""
			if len(residualWorkBeadIDs) > j {
				workBeadID = residualWorkBeadIDs[j]
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
				if demand.WorktreeSpecs != nil {
					worktreeSpec = demand.WorktreeSpecs[workBeadID]
				}
				if demand.WorktreeErrors != nil {
					worktreeError = strings.TrimSpace(demand.WorktreeErrors[workBeadID])
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
				WorktreeSpec:   worktreeSpec,
				WorktreeError:  worktreeError,
			}
			allRequests = append(allRequests, req)
			usage.accept(req, limits)
		}
	}

	return applyNestedCaps(cfg, allRequests, aliasHeldTemplates, trace)
}

func poolNewDemandRequestsPreA3(
	cfg *config.City,
	sessionInfos []sessionpkg.Info,
	resumeSessionBeadIDs map[string]struct{},
	decisionTime time.Time,
) (map[string][]SessionRequest, map[string][]SessionRequest) {
	protected := make(map[string][]SessionRequest)
	inFlight := make(map[string][]SessionRequest)
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
			req := SessionRequest{
				Template:       template,
				Tier:           "new",
				SessionBeadID:  sb.ID,
				WorkBeadID:     strings.TrimSpace(sb.TriggerBeadID),
				WorkPack:       strings.TrimSpace(sb.Pack),
				WorkWorkspace:  strings.TrimSpace(sb.PackWorkspace),
				WorkStoreRef:   strings.TrimSpace(sb.TriggerBeadStoreRef),
				BrainParentSID: strings.TrimSpace(sb.BrainParentSID),
			}
			if poolSessionEligibleForProtectedDemand(sb, decisionTime) {
				protected[template] = append(protected[template], req)
				continue
			}
			if poolSessionConsumesNewDemandInfo(sb) {
				inFlight[template] = append(inFlight[template], req)
			}
		}
	}
	return protected, inFlight
}
