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
	SessionBeadID string // for resume tier: the session bead to restart
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
// assignedWorkBeads contains work beads that have an assignee matching a
// session — the cross-reference of in-progress/ready work with live sessions.
// Each bead's gc.routed_to determines which agent template it belongs to.
// scaleCheckCounts maps agent template → desired count from scale_check.
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
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		assigneeToSessionBeadID[sb.ID] = sb.ID
		if sn := strings.TrimSpace(sb.Metadata["session_name"]); sn != "" {
			assigneeToSessionBeadID[sn] = sb.ID
		}
		if ni := strings.TrimSpace(sb.Metadata["configured_named_identity"]); ni != "" {
			assigneeToSessionBeadID[ni] = sb.ID
		}
	}

	// Collect uncapped requests per agent template.
	var allRequests []SessionRequest

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		template := agent.QualifiedName()

		// Resume tier: assigned work beads whose assignee resolves to a
		// non-closed session bead. These sessions must stay alive.
		// New tier (bead-driven): work beads routed to this template
		// but with no assignee — on-demand work from gc sling that
		// needs a new pool worker to be spawned.
		for _, wb := range assignedWorkBeads {
			routedTo := wb.Metadata["gc.routed_to"]
			if routedTo != template {
				continue
			}
			if wb.Status != "in_progress" && wb.Status != "open" {
				continue
			}
			assignee := strings.TrimSpace(wb.Assignee)
			sessionBeadID := assigneeToSessionBeadID[assignee]
			if sessionBeadID != "" {
				allRequests = append(allRequests, SessionRequest{
					Template:      template,
					BeadPriority:  beadPriority(wb),
					Tier:          "resume",
					SessionBeadID: sessionBeadID,
					WorkBeadID:    wb.ID,
				})
			} else if assignee == "" {
				// Routed but unassigned — demand for a new worker.
				allRequests = append(allRequests, SessionRequest{
					Template:     template,
					BeadPriority: beadPriority(wb),
					Tier:         "new",
					WorkBeadID:   wb.ID,
				})
			}
			// Else: assignee set but session closed/unknown — orphaned
			// work, not our job to respawn.
		}
	}

	// Merge scale_check demand: for each agent, if scale_check wants more
	// sessions than bead-driven requests already cover, add the difference
	// as "new" tier requests. This ensures the scale_check command (which
	// runs in the correct rig directory) is always the authoritative demand
	// signal, while bead-driven resume requests preserve running sessions.
	if len(scaleCheckCounts) > 0 {
		beadDriven := make(map[string]int, len(allRequests))
		for _, r := range allRequests {
			beadDriven[r.Template]++
		}
		for _, agent := range cfg.Agents {
			if agent.Suspended {
				continue
			}
			template := agent.QualifiedName()
			scaleCount, ok := scaleCheckCounts[template]
			if !ok {
				continue
			}
			deficit := scaleCount - beadDriven[template]
			for j := 0; j < deficit; j++ {
				allRequests = append(allRequests, SessionRequest{
					Template: template,
					Tier:     "new",
				})
			}
		}
	}

	return applyNestedCaps(cfg, allRequests, trace)
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

	// Counters for nested caps.
	agentCount := make(map[string]int) // template → count
	rigCount := make(map[string]int)   // rig name → count
	workspaceCount := 0

	// Resolve caps.
	workspaceMax := -1 // -1 = unlimited
	if cfg.Workspace.MaxActiveSessions != nil {
		workspaceMax = *cfg.Workspace.MaxActiveSessions
	}
	rigMaxMap := make(map[string]int) // rig name → max (-1 = unlimited)
	for _, rig := range cfg.Rigs {
		if rig.MaxActiveSessions != nil {
			rigMaxMap[rig.Name] = *rig.MaxActiveSessions
		} else {
			rigMaxMap[rig.Name] = -1
		}
	}
	agentMaxMap := make(map[string]int)    // template → max (-1 = unlimited)
	agentRigMap := make(map[string]string) // template → rig name
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		template := agent.QualifiedName()
		agentRigMap[template] = agent.Dir
		resolved := agent.ResolvedMaxActiveSessions(cfg)
		if resolved != nil {
			agentMaxMap[template] = *resolved
		} else {
			agentMaxMap[template] = -1
		}
	}

	// Walk sorted requests, accepting each if all caps have room.
	accepted := make(map[string][]SessionRequest) // template → accepted requests
	// Dedup: don't accept multiple requests for the same session bead.
	seenSessionBeads := make(map[string]bool)

	for _, req := range requests {
		// Dedup resume requests for the same session bead.
		if req.Tier == "resume" && req.SessionBeadID != "" {
			if seenSessionBeads[req.SessionBeadID] {
				continue
			}
		}

		template := req.Template
		rig := agentRigMap[template]

		// Check agent cap.
		agentMax := agentMaxMap[template]
		if agentMax >= 0 && agentCount[template] >= agentMax {
			if trace != nil {
				trace.recordDecision("reconciler.pool.agent_cap", template, "", "agent_cap", "rejected", traceRecordPayload{
					"agent_max": agentMax,
					"current":   agentCount[template],
					"tier":      req.Tier,
				}, nil, "")
			}
			continue
		}
		// Check rig cap.
		if rig != "" {
			rigMax, ok := rigMaxMap[rig]
			if !ok {
				rigMax = -1
			}
			if rigMax >= 0 && rigCount[rig] >= rigMax {
				if trace != nil {
					trace.recordDecision("reconciler.pool.rig_cap", template, "", "rig_cap", "rejected", traceRecordPayload{
						"rig":     rig,
						"rig_max": rigMax,
						"current": rigCount[rig],
						"tier":    req.Tier,
					}, nil, "")
				}
				continue
			}
		}
		// Check workspace cap.
		if workspaceMax >= 0 && workspaceCount >= workspaceMax {
			if trace != nil {
				trace.recordDecision("reconciler.pool.workspace_cap", template, "", "workspace_cap", "rejected", traceRecordPayload{
					"workspace_max": workspaceMax,
					"current":       workspaceCount,
					"tier":          req.Tier,
				}, nil, "")
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
		agentCount[template]++
		if rig != "" {
			rigCount[rig]++
		}
		workspaceCount++
		if req.Tier == "resume" && req.SessionBeadID != "" {
			seenSessionBeads[req.SessionBeadID] = true
		}
	}

	// Fill agent mins (if caps allow).
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		template := agent.QualifiedName()
		minSess := agent.EffectiveMinActiveSessions()
		for agentCount[template] < minSess {
			rig := agentRigMap[template]
			// Check caps before adding idle session.
			agentMax := agentMaxMap[template]
			if agentMax >= 0 && agentCount[template] >= agentMax {
				break
			}
			if rig != "" {
				rigMax, ok := rigMaxMap[rig]
				if !ok {
					rigMax = -1
				}
				if rigMax >= 0 && rigCount[rig] >= rigMax {
					break
				}
			}
			if workspaceMax >= 0 && workspaceCount >= workspaceMax {
				break
			}
			accepted[template] = append(accepted[template], SessionRequest{
				Template: template,
				Tier:     "new",
			})
			if trace != nil {
				trace.recordDecision("reconciler.pool.min_fill", template, "", "min_fill", "accepted", traceRecordPayload{
					"min":     minSess,
					"current": agentCount[template],
					"tier":    "new",
				}, nil, "")
			}
			agentCount[template]++
			if rig != "" {
				rigCount[rig]++
			}
			workspaceCount++
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
