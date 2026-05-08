package main

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// SessionRequest represents a single session the reconciler should start.
type SessionRequest struct {
	Template      string // agent template qualified name (e.g., "gascity/claude")
	BeadPriority  int    // priority of the driving work bead
	Tier          string // "resume" (in-progress work with assigned session) or "new" (ready unassigned work)
	SessionBeadID string // concrete session to preserve for resume or in-flight new demand
	WorkBeadID    string // the work bead driving this request
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
	return computePoolDesiredStates(cfg, assignedWorkBeads, sessionBeads, scaleCheckCounts, nil)
}

func ComputePoolDesiredStatesTraced(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionBeads []beads.Bead,
	scaleCheckCounts map[string]int,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStates(cfg, assignedWorkBeads, sessionBeads, scaleCheckCounts, trace)
}

func computePoolDesiredStates(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionBeads []beads.Bead,
	scaleCheckCounts map[string]int,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	// Build reverse lookup: any identifier → session bead ID.
	// Assignee on work beads may be a bead ID, session name, or alias.
	assigneeToSessionBeadID := make(map[string]string)
	sessionBeadTemplate := make(map[string]string)
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		template := strings.TrimSpace(sb.Metadata["template"])
		if template != "" {
			sessionBeadTemplate[sb.ID] = template
		}
		assigneeToSessionBeadID[sb.ID] = sb.ID
		if sn := strings.TrimSpace(sb.Metadata["session_name"]); sn != "" {
			assigneeToSessionBeadID[sn] = sb.ID
		}
		if ni := strings.TrimSpace(sb.Metadata["configured_named_identity"]); ni != "" {
			assigneeToSessionBeadID[ni] = sb.ID
		}
	}

	var resumeRequests []SessionRequest

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
			routedTo := wb.Metadata["gc.routed_to"]
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
			if routedTo != template {
				continue
			}
			if sessionBeadID != "" {
				resumeRequests = append(resumeRequests, SessionRequest{
					Template:      template,
					BeadPriority:  beadPriority(wb),
					Tier:          "resume",
					SessionBeadID: sessionBeadID,
					WorkBeadID:    wb.ID,
				})
			}
			// Else: assignee set but session closed/unknown — orphaned
			// work, not our job to respawn.
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
			newCount := capNewDemandCount(limits, usage, agent, scaleCount)
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
				req := SessionRequest{
					Template: template,
					Tier:     "new",
				}
				allRequests = append(allRequests, req)
				usage.accept(req, limits)
			}
		}
	}

	return applyNestedCaps(cfg, allRequests, trace)
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
				Template:      template,
				Tier:          "new",
				SessionBeadID: sb.ID,
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
	return strings.TrimSpace(session.Metadata["state"]) == "creating"
}

// applyNestedCaps enforces workspace, rig, and agent max_active_sessions caps.
// Accepts requests in priority order, rejecting any that would exceed a cap.
func applyNestedCaps(cfg *config.City, requests []SessionRequest, trace *sessionReconcilerTraceCycle) []PoolDesiredState {
	// Sort by priority DESC, resume tier first within same priority.
	sort.SliceStable(requests, func(i, j int) bool {
		if requests[i].BeadPriority != requests[j].BeadPriority {
			return requests[i].BeadPriority > requests[j].BeadPriority
		}
		// Resume tier before new tier at same priority.
		if requests[i].Tier != requests[j].Tier {
			return requests[i].Tier == "resume"
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
		for usage.agentCount[template] < minSess {
			req := SessionRequest{
				Template: template,
				Tier:     "new",
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
			return sorted[i].Tier == "resume"
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
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
