package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
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
	// WorktreeSpec is the single-owner evidence published by the provisioner.
	// When present, the pool path verifies it before publishing work_dir on a
	// session bead; a mismatch fails the whole atomic metadata update.
	WorktreeSpec *worktree.Spec
	// WorktreeError carries incomplete/conflicting worktree evidence discovered
	// while building demand. The realization path fails closed before creating
	// or updating a session bead.
	WorktreeError string
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

// legacyWorkDirNoticeSeen deduplicates the unmanaged-workspace notice keyed by
// work bead ID. worktreeSpecForBead runs for every ready bead on every
// scale-check tick and nothing backfills ownership metadata onto beads that
// never published it, so an unguarded log line would repeat for the life of
// the process.
var legacyWorkDirNoticeSeen sync.Map // work bead ID -> struct{}

// worktreeSpecForBead reconstructs the exact single-owner verification input
// from metadata published after gc worktree ensure succeeds. A work_dir with
// incomplete or conflicting evidence is an error, never permission to launch
// a session into an unverified directory.
func worktreeSpecForBead(bead beads.Bead, storeRef string) (*worktree.Spec, error) {
	canonicalPath := strings.TrimSpace(bead.Metadata[beadmeta.WorkDirMetadataKey])
	legacyPath := strings.TrimSpace(bead.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	if canonicalPath != "" && legacyPath != "" && canonicalPath != legacyPath {
		return nil, fmt.Errorf("work bead %s has conflicting %s=%q and %s=%q",
			bead.ID, beadmeta.WorkDirMetadataKey, canonicalPath, beadmeta.LegacyWorkDirMetadataKey, legacyPath)
	}
	path := canonicalPath
	pathKey := beadmeta.WorkDirMetadataKey
	if path == "" {
		path = legacyPath
		pathKey = beadmeta.LegacyWorkDirMetadataKey
	}
	if path == "" {
		return nil, nil
	}
	values := []struct {
		key   string
		value string
	}{
		{beadmeta.WorktreeRepoMetadataKey, bead.Metadata[beadmeta.WorktreeRepoMetadataKey]},
		{beadmeta.WorktreeRootMetadataKey, bead.Metadata[beadmeta.WorktreeRootMetadataKey]},
		{beadmeta.WorkBranchMetadataKey, bead.Metadata[beadmeta.WorkBranchMetadataKey]},
		{beadmeta.WorktreeBaseRefMetadataKey, bead.Metadata[beadmeta.WorktreeBaseRefMetadataKey]},
		{beadmeta.WorktreeBaseSHAMetadataKey, bead.Metadata[beadmeta.WorktreeBaseSHAMetadataKey]},
		{beadmeta.WorktreeCreatorMetadataKey, bead.Metadata[beadmeta.WorktreeCreatorMetadataKey]},
		{beadmeta.WorktreeOwnerMetadataKey, bead.Metadata[beadmeta.WorktreeOwnerMetadataKey]},
		{beadmeta.WorktreeGenerationMetadataKey, bead.Metadata[beadmeta.WorktreeGenerationMetadataKey]},
		{beadmeta.WorktreeLifecycleMetadataKey, bead.Metadata[beadmeta.WorktreeLifecycleMetadataKey]},
	}
	missing := make([]string, 0, len(values))
	present := make([]string, 0, len(values))
	for _, item := range values {
		if strings.TrimSpace(item.value) == "" {
			missing = append(missing, item.key)
		} else {
			present = append(present, item.key)
		}
	}
	if len(missing) == len(values) {
		// A bead carrying work_dir without any ownership evidence is not
		// incomplete evidence -- it never claimed to publish any. Recipe steps
		// are still minted this way (stampDrainItemRecipe in
		// internal/dispatch/drain.go copies both work_dir spellings and none
		// of the nine ownership keys), so treat it as an unmanaged workspace
		// and let the seat spawn as it always did, instead of erroring it into
		// permanent starvation.
		if _, dup := legacyWorkDirNoticeSeen.LoadOrStore(bead.ID, struct{}{}); !dup {
			log.Printf("worktreeSpecForBead: work bead %s has %s=%q with no worktree ownership metadata; treating as unmanaged",
				bead.ID, pathKey, path)
		}
		return nil, nil
	}
	if len(present) == 1 && present[0] == beadmeta.WorkBranchMetadataKey {
		// gc.work_branch alone is not incomplete evidence either -- it is the
		// shape a hook claim's ambient branch stamp produces on an otherwise
		// unmanaged bead (hookClaimIdentityPatch stamps gc.work_branch onto
		// any bead with a resolvable branch, independent of whether the bead
		// ever published worktree ownership evidence). Treat it the same as
		// the zero-key case above instead of hard-erroring the seat into
		// permanent starvation the first time a hook claim touches it.
		if _, dup := legacyWorkDirNoticeSeen.LoadOrStore(bead.ID, struct{}{}); !dup {
			log.Printf("worktreeSpecForBead: work bead %s has %s=%q with only %s published; treating as unmanaged",
				bead.ID, pathKey, path, beadmeta.WorkBranchMetadataKey)
		}
		return nil, nil
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("work bead %s has %s=%q but is missing %s",
			bead.ID, beadmeta.WorkDirMetadataKey, path, missing[0])
	}
	// The bead's own gc.root_store_ref is the canonical spelling
	// ("city:<name>", "rig:<name>") and is what the creating side recorded in
	// the workspace provenance. The caller's storeRef is a probe shorthand
	// ("city"), so verification against durable provenance compares unequal
	// strings and refuses a workspace that is in fact ours. Prefer the bead's
	// value and keep the shorthand only as a fallback for beads that carry no
	// canonical ref.
	resolvedStoreRef := strings.TrimSpace(bead.Metadata[beadmeta.RootStoreRefMetadataKey])
	if resolvedStoreRef == "" {
		resolvedStoreRef = strings.TrimSpace(storeRef)
	}
	if resolvedStoreRef == "" {
		return nil, fmt.Errorf("work bead %s has %s=%q but no store reference", bead.ID, beadmeta.WorkDirMetadataKey, path)
	}
	return &worktree.Spec{
		RepoDir:    strings.TrimSpace(bead.Metadata[beadmeta.WorktreeRepoMetadataKey]),
		Root:       strings.TrimSpace(bead.Metadata[beadmeta.WorktreeRootMetadataKey]),
		Path:       path,
		Branch:     strings.TrimSpace(bead.Metadata[beadmeta.WorkBranchMetadataKey]),
		Base:       strings.TrimSpace(bead.Metadata[beadmeta.WorktreeBaseRefMetadataKey]),
		BaseSHA:    strings.TrimSpace(bead.Metadata[beadmeta.WorktreeBaseSHAMetadataKey]),
		BeadID:     strings.TrimSpace(bead.ID),
		StoreRef:   resolvedStoreRef,
		Creator:    strings.TrimSpace(bead.Metadata[beadmeta.WorktreeCreatorMetadataKey]),
		Owner:      strings.TrimSpace(bead.Metadata[beadmeta.WorktreeOwnerMetadataKey]),
		Generation: strings.TrimSpace(bead.Metadata[beadmeta.WorktreeGenerationMetadataKey]),
		Lifecycle:  strings.TrimSpace(bead.Metadata[beadmeta.WorktreeLifecycleMetadataKey]),
	}, nil
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

// ComputePoolDesiredStatesAt computes pool demand at a caller-supplied
// decision time so post-create retention is consistent with its session
// snapshot.
func ComputePoolDesiredStatesAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	decisionTime time.Time,
) []PoolDesiredState {
	return computePoolDesiredStatesAt(cfg, assignedWorkBeads, sessionInfos, scaleCheckCounts, nil, decisionTime, nil)
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

// ComputePoolDesiredStatesTracedAt is ComputePoolDesiredStatesAt with
// reconciler decision tracing.
func ComputePoolDesiredStatesTracedAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	decisionTime time.Time,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStatesAt(cfg, assignedWorkBeads, sessionInfos, scaleCheckCounts, nil, decisionTime, trace)
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

// ComputePoolDesiredStatesWithDemandTracedAt computes traced pool demand at a
// caller-supplied decision time while preserving per-work demand provenance.
func ComputePoolDesiredStatesWithDemandTracedAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	decisionTime time.Time,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStatesAt(cfg, assignedWorkBeads, sessionInfos, scaleCheckCounts, scaleCheckDemand, decisionTime, trace)
}

func computePoolDesiredStates(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	trace *sessionReconcilerTraceCycle,
) []PoolDesiredState {
	return computePoolDesiredStatesAt(cfg, assignedWorkBeads, sessionInfos, scaleCheckCounts, scaleCheckDemand, time.Time{}, trace)
}

func computePoolDesiredStatesAt(
	cfg *config.City,
	assignedWorkBeads []beads.Bead,
	sessionInfos []sessionpkg.Info,
	scaleCheckCounts map[string]int,
	scaleCheckDemand map[string]scaleCheckDemand,
	decisionTime time.Time,
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
	// asleepSessionBeadIDs marks non-closed sessions whose runtime state is
	// asleep (normalizeInfoState also folds "drained" into StateAsleep). A
	// wake_mode="fresh" agent must not resume one of these stale rows — see the
	// resume-tier guard below.
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
				// Named-session beads are materialized by the named-session
				// loop in buildDesiredState, not by the pool path. Skipping
				// here prevents realizePoolDesiredSessions from renaming the
				// canonical named identity to a phantom "{name}-1" pool
				// instance — which would create two desired sessions for the
				// same agent even when max_active_sessions=1.
				if namedSessionBeadIDs[sessionBeadID] {
					continue
				}
				// A wake_mode="fresh" pool agent must not inherit a stale
				// *asleep* session bead: by configuration it never wants the
				// old session's state back, so resuming that row leaves the
				// pool bound to a session it will not reuse and the assigned
				// work stranded. Plan a clean session bound to the same work
				// instead — the wake-known-identity tier with an empty
				// SessionBeadID, the same shape the closed-session case
				// already uses. A live (non-asleep) session still resumes;
				// agents with unset or wake_mode="resume" are unaffected.
				// (gastownhall/gascity#4849)
				if agent.EffectiveWakeMode() == "fresh" && asleepSessionBeadIDs[sessionBeadID] {
					if _, ok := wakeRequestedTemplates[template]; ok {
						continue
					}
					wakeRequestedTemplates[template] = struct{}{}
					resumeRequests = append(resumeRequests, SessionRequest{
						Template:     template,
						BeadPriority: beadPriority(wb),
						Tier:         "wake-known-identity",
						// SessionBeadID intentionally empty: the stale asleep
						// bead must not be reused, so realizePoolDesiredSessions
						// plans a clean create.
						WorkBeadID:     wb.ID,
						WorkBeadTitle:  strings.TrimSpace(wb.Title),
						WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
						WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
						BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
					})
					if trace != nil {
						trace.RecordDecision(TraceSitePoolWakeKnownIdentity, TraceReasonAssignedWork, TraceOutcomeScheduled, template, "", traceRecordPayload{
							"tier":                   "wake-known-identity",
							"work_bead":              wb.ID,
							"skipped_asleep_session": sessionBeadID,
						})
					}
					continue
				}
				resumeRequests = append(resumeRequests, SessionRequest{
					Template:       template,
					BeadPriority:   beadPriority(wb),
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
				// A configured named session's own bare identity never
				// generates pool demand — namedWorkReady recovers it
				// instead (ga-i1d0tr Candidate B). Mirrors the resume
				// tier's namedSessionBeadIDs skip above, extended to the
				// case where no live session bead resolves the assignee
				// at all.
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
				WorkBeadTitle:  strings.TrimSpace(wb.Title),
				WorkPack:       strings.TrimSpace(wb.Metadata[beadmeta.PackMetadataKey]),
				WorkWorkspace:  strings.TrimSpace(wb.Metadata[beadmeta.PackWorkspaceMetadataKey]),
				BrainParentSID: strings.TrimSpace(wb.Metadata[beadmeta.BrainParentSIDMetadataKey]),
			})
			if trace != nil {
				trace.RecordDecision(TraceSitePoolWakeKnownIdentity, TraceReasonAssignedWork, TraceOutcomeScheduled, template, "", traceRecordPayload{
					"tier":      "wake-known-identity",
					"work_bead": wb.ID,
				})
			}
		}
	}

	limits := newNestedCapLimits(cfg)
	resumeSessionBeadIDs := make(map[string]struct{}, len(resumeRequests))
	for _, req := range resumeRequests {
		if req.SessionBeadID != "" {
			resumeSessionBeadIDs[req.SessionBeadID] = struct{}{}
		}
	}
	protectedNewRequests, inFlightNewRequests := poolNewDemandRequests(cfg, sessionInfos, resumeSessionBeadIDs, decisionTime)
	// A wake-known request has assigned work but no surviving concrete session
	// identity. Freshly completed unassigned pool sessions are valid concrete
	// capacity for that request. Bind them before calculating residual protected
	// demand so wake-known + protection cannot materialize the same capacity
	// twice.
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
	allRequests := append([]SessionRequest(nil), resumeRequests...)

	// Merge scale_check demand. In bead-backed reconciliation, scale_check is
	// the authoritative signal for new unassigned demand only; resume requests
	// are calculated independently from assigned work and must not be deducted
	// from that count. Pool-created sessions that have not claimed work yet
	// represent already-spent new demand, so they occupy the first new-demand
	// slots explicitly before anonymous creates are materialized. Freshly
	// completed sessions also establish a short-lived demand floor so a sibling
	// startup is not drained merely because another worker claimed first.
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
		effectiveDemand := max(scaleCount, len(protected))
		newCount := capNewDemandCount(limits, usage, agent, effectiveDemand)
		recordNewDemandCapTrace(trace, template, agent, limits, usage, effectiveDemand, newCount)
		inFlight := inFlightNewRequests[template]
		protectedCount := minInt(len(protected), newCount)
		inFlightCount := minInt(len(inFlight), newCount-protectedCount)
		reusedCount := protectedCount + inFlightCount
		selectedConcrete := make([]SessionRequest, 0, reusedCount)
		selectedConcrete = append(selectedConcrete, protected[:protectedCount]...)
		selectedConcrete = append(selectedConcrete, inFlight[:inFlightCount]...)
		demand := scaleCheckDemand[template]
		selectedConcrete, residualWorkBeadIDs := allocateScaleDemandToConcrete(demand, selectedConcrete)
		if scaleCount > 0 && len(protected)+len(inFlight) > 0 && trace != nil {
			trace.RecordDecision(TraceSitePoolInFlightReuse, TraceReasonInFlightReuse, TraceOutcomeAccepted, template, "", traceRecordPayload{
				"scale_check":   scaleCount,
				"in_flight":     len(inFlight),
				"protected":     len(protected),
				"reused":        reusedCount,
				"anonymous_new": newCount - reusedCount,
			})
		}
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

// allocateScaleDemandToConcrete matches concrete reused capacity against scale
// demand by identity, then rebinds unmatched concrete capacity to the earliest
// unclaimed demand. A concrete request carrying B covers B even when demand is
// ordered [A, B]; a stale X (or blank trigger) is rebound to A rather than
// silently consuming A while remaining pointed at X. Surplus concrete capacity
// with no residual demand keeps its prior trigger because its protection floor
// is independent of scale demand.
func allocateScaleDemandToConcrete(demand scaleCheckDemand, concrete []SessionRequest) ([]SessionRequest, []string) {
	allocated := append([]SessionRequest(nil), concrete...)
	uniqueDemand := make([]string, 0, len(demand.WorkBeadIDs))
	demandSet := make(map[string]struct{}, len(demand.WorkBeadIDs))
	for _, rawID := range demand.WorkBeadIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, seen := demandSet[id]; seen {
			continue
		}
		demandSet[id] = struct{}{}
		uniqueDemand = append(uniqueDemand, id)
	}

	matched := make(map[string]struct{}, len(allocated))
	unmatchedConcrete := make([]int, 0, len(allocated))
	for i, request := range allocated {
		id := strings.TrimSpace(request.WorkBeadID)
		if _, demanded := demandSet[id]; demanded {
			if _, alreadyMatched := matched[id]; !alreadyMatched {
				matched[id] = struct{}{}
				allocated[i] = requestWithScaleDemandProvenance(request, demand, id)
				continue
			}
		}
		unmatchedConcrete = append(unmatchedConcrete, i)
	}

	remaining := make([]string, 0, len(uniqueDemand)-len(matched))
	for _, id := range uniqueDemand {
		if _, covered := matched[id]; !covered {
			remaining = append(remaining, id)
		}
	}
	rebound := minInt(len(unmatchedConcrete), len(remaining))
	for i := 0; i < rebound; i++ {
		idx := unmatchedConcrete[i]
		allocated[idx] = requestWithScaleDemandProvenance(allocated[idx], demand, remaining[i])
	}
	return allocated, remaining[rebound:]
}

func requestWithScaleDemandProvenance(request SessionRequest, demand scaleCheckDemand, workBeadID string) SessionRequest {
	workBeadID = strings.TrimSpace(workBeadID)
	request.WorkBeadID = workBeadID
	request.WorkBeadTitle = strings.TrimSpace(demand.Titles[workBeadID])
	request.WorkPack = strings.TrimSpace(demand.Packs[workBeadID])
	request.WorkWorkspace = strings.TrimSpace(demand.Workspaces[workBeadID])
	request.WorkStoreRef = strings.TrimSpace(demand.StoreRefs[workBeadID])
	request.BrainParentSID = strings.TrimSpace(demand.ParentSIDs[workBeadID])
	// Worktree evidence is per-bead like every field above it. Carrying the
	// previous bead's spec into a rebound request would hand the session a
	// workspace verified for other work.
	request.WorktreeSpec = nil
	request.WorktreeError = ""
	if demand.WorktreeSpecs != nil {
		request.WorktreeSpec = demand.WorktreeSpecs[workBeadID]
	}
	if demand.WorktreeErrors != nil {
		request.WorktreeError = strings.TrimSpace(demand.WorktreeErrors[workBeadID])
	}
	return request
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
			if strings.TrimSpace(sb.Alias) == template {
				held[template] = struct{}{}
				break
			}
			// A named session's Alias holds its own configured identity, not
			// the backing template (build_desired_state.go sets tp.Alias =
			// identity for every named session, e.g. "primary" bound to
			// template "worker"). When identity != template, the Alias check
			// above never matches even though this bead is the singleton
			// slot's sole occupant. Its Template field is always the backing
			// template's qualified name, so use that as the named-session
			// match instead of Alias.
			if isNamedSessionInfo(sb) && strings.TrimSpace(sb.Template) == template {
				held[template] = struct{}{}
				break
			}
		}
	}
	return held
}

func poolNewDemandRequests(
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

// poolSessionWithinPostCreateProtection reports whether a successfully-created,
// reusable pool session is still in the same grace window used by the
// steady-state sweep. decisionTime is injected by the desired-state build so all
// decisions in a tick share one clock observation. A future marker fails
// closed: the writer and reader share a host clock, so negative age indicates
// corrupt metadata rather than a legitimate grace window.
func poolSessionWithinPostCreateProtection(info sessionpkg.Info, decisionTime time.Time) bool {
	if decisionTime.IsZero() {
		return false
	}
	if info.Closed || isDrainedSessionInfo(info) || isFailedCreateSessionInfo(info) ||
		sessionHasProviderTerminalErrorInfo(info) {
		return false
	}
	state := strings.TrimSpace(info.MetadataState)
	if state != "active" && state != "awake" {
		return false
	}
	if strings.TrimSpace(info.StateReason) != "creation_complete" {
		return false
	}
	creationCompleteAt, ok := parseRFC3339Metadata(info.CreationCompleteAt)
	if !ok {
		return false
	}
	age := decisionTime.Sub(creationCompleteAt)
	return age >= 0 && age < postCreateProtectionTimeout
}

// poolSessionEligibleForProtectedDemand is intentionally narrower than the
// sweep-retention predicate above. A fresh dependency-only or lifecycle-blocked
// session should survive the post-create sweep window, but it is not runnable
// generic capacity and therefore must neither establish a demand floor nor bind
// a wake-known request.
func poolSessionEligibleForProtectedDemand(info sessionpkg.Info, decisionTime time.Time) bool {
	return poolSessionWithinPostCreateProtection(info, decisionTime) &&
		!info.DependencyOnly &&
		strings.TrimSpace(info.WaitHold) == "" &&
		!metadataTimeInFuture(info.HeldUntil, decisionTime) &&
		!metadataTimeInFuture(info.QuarantinedUntil, decisionTime)
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

// isConfiguredNamedSessionIdentity reports whether assignee names a
// configured [[named_session]]'s own identity — checked structurally via
// cfg.NamedSessions, with no live-session/store lookup, so it holds even
// when the named session has no live session bead at all. A named session
// whose backing agent is suspended is excluded: the named-session tier
// never claims work for a suspended agent (mirrors the namedSpecs filter in
// build_desired_state.go), so exempting it here too would orphan the bead
// with neither side picking it up.
func isConfiguredNamedSessionIdentity(cfg *config.City, assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" || cfg == nil {
		return false
	}
	spec, ok := findNamedSessionSpec(cfg, "", assignee)
	if !ok || spec.Agent == nil {
		return false
	}
	return !spec.Agent.Suspended
}
