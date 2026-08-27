package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worker"
)

const routedWorkPoolReuseNudgeSource = "routed-work-pool-reuse"

type routedWorkPoolReuseDisposition uint8

const (
	routedWorkPoolReuseNotApplicable routedWorkPoolReuseDisposition = iota
	routedWorkPoolReuseReusable
	routedWorkPoolReuseBusy
	routedWorkPoolReuseRefused
)

type routedWorkPoolReuseLease struct {
	SessionID            string
	InstanceToken        string
	PoolTarget           string
	PreviousWorkID       string
	PreviousSourceStore  string
	ControllerGeneration uint64
	MembershipRevision   uint64
	MemberIDs            []string
	Binding              sessionpkg.TriggerBinding
}

type routedWorkPoolSkippedBusyCandidate struct {
	info      sessionpkg.Info
	persisted sessionpkg.PersistedResponse
	lease     routedWorkPoolReuseLease
}

func (cr *CityRuntime) reuseIdleRoutedWorkPoolMember(
	ctx context.Context,
	snapshot controllerSessionStartSnapshot,
	agent *config.Agent,
	work beads.Bead,
	hint routedWorkPoolAllocationHint,
	bp *agentBuildParams,
	request SessionRequest,
) (routedWorkPoolAllocationResult, routedWorkPoolReuseDisposition, error) {
	if strings.TrimSpace(agent.Nudge) == "" {
		return routedWorkPoolAllocationResult{}, routedWorkPoolReuseNotApplicable, nil
	}
	observation, memberIDs, exact := cr.poolMembershipShadow.observeMemberIDs(hint.PoolTarget)
	if !exact {
		if !observation.certified {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("reusing pool member: membership is uncertified")
		}
		if observation.members == 0 {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseNotApplicable, nil
		}
		return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, nil
	}
	if agent.UsesCanonicalSingletonPoolIdentity() {
		if observation.members != 1 || observation.occupied != 1 || len(memberIDs) != 1 {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, nil
		}
	} else {
		if observation.members == 1 && observation.occupied != 1 {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseNotApplicable, nil
		}
		if observation.members > 1 && observation.occupied < 2 {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseNotApplicable, nil
		}
	}
	candidates := make([]sessionpkg.Info, 0, len(memberIDs))
	persistedByID := make(map[string]sessionpkg.PersistedResponse, len(memberIDs))
	for _, sessionID := range memberIDs {
		info, persisted, err := getAuthoritativeSessionStartPersistedRecord(snapshot.Store, sessionID)
		if err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("reading reusable pool member %q: %w", sessionID, err)
		}
		if info.ID != sessionID {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("reading reusable pool member %q returned %q", sessionID, info.ID)
		}
		candidates = append(candidates, info)
		persistedByID[info.ID] = persisted
	}
	sortSessionInfosByCreatedAtThenID(candidates)
	assignedBusy, err := cr.routedWorkPoolReuseAssignedWork(snapshot, agent, candidates)
	if err != nil {
		return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, err
	}

	skippedBusy := make([]routedWorkPoolSkippedBusyCandidate, 0, len(candidates))
	for _, info := range candidates {
		workDir := poolTriggerWorkDir(bp, agent, sessionBeadQualifiedNameInfo(snapshot.CityPath, agent, snapshot.Config.Rigs, info), request)
		if strings.TrimSpace(workDir) == "" {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("reusing pool member %q: work directory is unavailable", info.ID)
		}
		lease := routedWorkPoolReuseLease{
			SessionID:            info.ID,
			InstanceToken:        strings.TrimSpace(info.InstanceToken),
			PoolTarget:           strings.TrimSpace(hint.PoolTarget),
			PreviousWorkID:       strings.TrimSpace(info.TriggerBeadID),
			PreviousSourceStore:  strings.TrimSpace(info.TriggerBeadStoreRef),
			ControllerGeneration: snapshot.Generation,
			MembershipRevision:   observation.revision,
			MemberIDs:            slices.Clone(memberIDs),
			Binding: sessionpkg.TriggerBinding{
				WorkID:         strings.TrimSpace(work.ID),
				StoreRef:       strings.TrimSpace(hint.SourceStore),
				BrainParentSID: strings.TrimSpace(request.BrainParentSID),
				Pack:           strings.TrimSpace(request.WorkPack),
				Workspace:      packWorkspaceSlug(request),
				WorkDir:        strings.TrimSpace(workDir),
			},
		}
		if err := validateRoutedWorkPoolReuseLease(lease); err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("validating reusable pool member %q: %w", info.ID, err)
		}
		disposition, err := cr.authorizeRoutedWorkPoolReuse(snapshot, info, lease, false, assignedBusy[info.ID])
		if err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, err
		}
		if disposition == routedWorkPoolReuseBusy {
			skippedBusy = append(skippedBusy, routedWorkPoolSkippedBusyCandidate{
				info:      info,
				persisted: persistedByID[info.ID],
				lease:     lease,
			})
			continue
		}
		if disposition != routedWorkPoolReuseReusable {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, nil
		}
		if len(skippedBusy) > 0 {
			valid, validateErr := cr.revalidateRoutedWorkPoolSkippedBusy(snapshot, agent, skippedBusy)
			if validateErr != nil {
				return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, validateErr
			}
			if !valid {
				return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, nil
			}
		}
		preWriteAssignedBusy, err := cr.routedWorkPoolReuseAssignedWork(snapshot, agent, []sessionpkg.Info{info})
		if err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, err
		}
		disposition, err = cr.authorizeRoutedWorkPoolReuse(snapshot, info, lease, false, preWriteAssignedBusy[info.ID])
		if err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, err
		}
		if disposition != routedWorkPoolReuseReusable {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, nil
		}
		preRebindPersisted := persistedByID[info.ID]
		_, committedPatch, err := sessionFrontDoor(snapshot.Store).RebindTriggerIfMatch(info, preRebindPersisted, lease.Binding)
		if err != nil {
			if beads.IsPreconditionFailed(err) {
				return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("rebinding reusable pool member %q lost its revision fence: %w", lease.SessionID, err)
			}
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, err
		}
		// The rebind's exact durable image: the fenced pre-image plus the patch
		// the write committed. This is what proves nothing else landed on the row
		// between the CAS and this re-read. Predicting the post-write REVISION
		// instead (`preRebindPersisted.Revision + 1`) was a contract violation —
		// revisions are opaque tokens testable only for equality, and bd mints
		// signed row_lock values, so the prediction named a row no bd-backed city
		// could ever produce and refused every reusable member (ga-f7v2ft.144).
		expectedReboundMetadata := committedPatch.Apply(preRebindPersisted.Metadata)
		current, currentPersisted, err := getAuthoritativeSessionStartPersistedRecord(snapshot.Store, lease.SessionID)
		if err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("rereading rebound pool member %q: %w", lease.SessionID, err)
		}
		if currentPersisted.Status != preRebindPersisted.Status || !maps.Equal(currentPersisted.Metadata, expectedReboundMetadata) {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("rereading rebound pool member %q: durable image is not the committed rebind", lease.SessionID)
		}
		// The revision the rebind LANDED on, read back from the row rather than
		// derived. It fences the post-idle-wait re-read below by equality only.
		reboundRevision := currentPersisted.Revision
		if !beads.RevisionKnown(reboundRevision) {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("rereading rebound pool member %q: revision is unavailable", lease.SessionID)
		}
		currentAssignedBusy, err := cr.routedWorkPoolReuseAssignedWork(snapshot, agent, []sessionpkg.Info{current})
		if err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, err
		}
		disposition, err = cr.authorizeRoutedWorkPoolReuse(snapshot, current, lease, true, currentAssignedBusy[current.ID])
		if err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, err
		}
		if disposition != routedWorkPoolReuseReusable {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("rebound pool member %q lost authorization before nudge", lease.SessionID)
		}
		handle, err := workerHandleForSessionWithConfig(snapshot.CityPath, snapshot.Store, snapshot.Provider, snapshot.Config, lease.SessionID)
		if err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("opening rebound pool member %q: %w", lease.SessionID, err)
		}
		authorizedHandle, ok := handle.(worker.AuthorizedIdleNudgeHandle)
		if !ok {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("opening rebound pool member %q: authorized idle nudge is unsupported", lease.SessionID)
		}
		nudge, err := authorizedHandle.NudgeWaitIdleAuthorized(ctx, worker.NudgeRequest{
			Text:     strings.TrimSpace(agent.Nudge),
			Delivery: worker.NudgeDeliveryWaitIdle,
			Source:   routedWorkPoolReuseNudgeSource,
			Wake:     worker.NudgeWakeLiveOnly,
		}, lease.InstanceToken, func(authorizeCtx context.Context) error {
			if err := authorizeCtx.Err(); err != nil {
				return err
			}
			latest, latestPersisted, err := getAuthoritativeSessionStartPersistedRecord(snapshot.Store, lease.SessionID)
			if err != nil {
				return fmt.Errorf("rereading rebound pool member after idle wait: %w", err)
			}
			if !beads.RevisionKnown(latestPersisted.Revision) || latestPersisted.Revision != reboundRevision {
				return fmt.Errorf("rereading rebound pool member after idle wait: revision %d, want %d", latestPersisted.Revision, reboundRevision)
			}
			assigned, err := cr.routedWorkPoolReuseAssignedWork(snapshot, agent, []sessionpkg.Info{latest})
			if err != nil {
				return err
			}
			disposition, err := cr.authorizeRoutedWorkPoolReuse(snapshot, latest, lease, true, assigned[latest.ID])
			if err != nil {
				return err
			}
			if disposition != routedWorkPoolReuseReusable {
				return fmt.Errorf("rebound pool member lost authorization after idle wait")
			}
			return nil
		})
		if err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("nudging rebound pool member %q: %w", lease.SessionID, err)
		}
		if !nudge.Delivered {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, fmt.Errorf("nudging rebound pool member %q: live idle delivery was not confirmed", lease.SessionID)
		}
		return routedWorkPoolAllocationResult{Session: current, Handled: true}, routedWorkPoolReuseReusable, nil
	}
	if len(skippedBusy) > 0 {
		valid, err := cr.revalidateRoutedWorkPoolSkippedBusy(snapshot, agent, skippedBusy)
		if err != nil {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, err
		}
		if !valid {
			return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, nil
		}
		return routedWorkPoolAllocationResult{}, routedWorkPoolReuseBusy, nil
	}
	return routedWorkPoolAllocationResult{}, routedWorkPoolReuseRefused, nil
}

func (cr *CityRuntime) routedWorkPoolReuseAssignedWork(
	snapshot controllerSessionStartSnapshot,
	agent *config.Agent,
	candidates []sessionpkg.Info,
) (map[string]bool, error) {
	busyBySession := make(map[string]bool, len(candidates))
	if len(candidates) == 0 {
		return busyBySession, nil
	}
	if agent == nil || snapshot.Config == nil {
		return nil, fmt.Errorf("checking reusable pool assignments: agent config is unavailable")
	}
	ownersByAssignee := make(map[string][]string)
	assignees := make([]string, 0, len(candidates)*2)
	for _, info := range candidates {
		candidateAgent := sessionAgentConfigInfo(snapshot.Config, info)
		if candidateAgent == nil || candidateAgent.QualifiedName() != agent.QualifiedName() {
			return nil, fmt.Errorf("checking reusable pool assignments for %q: candidate template is not the selected pool", info.ID)
		}
		identifiers := sessionAssignmentIdentifiersForConfigInfo(info, snapshot.Config)
		if len(identifiers) == 0 {
			return nil, fmt.Errorf("checking reusable pool assignments for %q: assignment identity is unavailable", info.ID)
		}
		busyBySession[info.ID] = false
		for _, identifier := range identifiers {
			identifier = strings.TrimSpace(identifier)
			if identifier == "" {
				continue
			}
			if _, first := ownersByAssignee[identifier]; !first {
				assignees = append(assignees, identifier)
			}
			ownersByAssignee[identifier] = append(ownersByAssignee[identifier], info.ID)
		}
	}
	plan, err := assignedWorkPlanForSessionInfo(snapshot.CityPath, snapshot.Config, snapshot.Store, cr.rigBeadStores(), candidates[0])
	if err != nil {
		return nil, fmt.Errorf("checking reusable pool assignments: resolving reachable stores: %w", err)
	}
	res, err := storeref.Walk(plan, func(leg storeref.Leg) (bool, error) {
		items, err := workAssignmentForStore(beads.WorkStore{Store: leg.Store}).OpenAssignedToAny(assignees)
		if err != nil {
			return false, err
		}
		for _, item := range items {
			for _, sessionID := range ownersByAssignee[strings.TrimSpace(item.Assignee)] {
				busyBySession[sessionID] = true
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("checking reusable pool assignments: %w", err)
	}
	// Every leg has to answer: a dark store could be holding the very claim
	// that makes a candidate busy, and reusing it would double-assign.
	if err := assignedWorkScanComplete(res); err != nil {
		return nil, fmt.Errorf("checking reusable pool assignments: %w", err)
	}
	return busyBySession, nil
}

func (cr *CityRuntime) revalidateRoutedWorkPoolSkippedBusy(
	snapshot controllerSessionStartSnapshot,
	agent *config.Agent,
	skipped []routedWorkPoolSkippedBusyCandidate,
) (bool, error) {
	current := make([]sessionpkg.Info, 0, len(skipped))
	for _, candidate := range skipped {
		info, persisted, err := getAuthoritativeSessionStartPersistedRecord(snapshot.Store, candidate.info.ID)
		if err != nil {
			return false, fmt.Errorf("rereading skipped busy pool member %q: %w", candidate.info.ID, err)
		}
		if !beads.RevisionKnown(candidate.persisted.Revision) || persisted.Revision != candidate.persisted.Revision ||
			persisted.Status != candidate.persisted.Status || !maps.Equal(persisted.Metadata, candidate.persisted.Metadata) {
			return false, nil
		}
		current = append(current, info)
	}
	assignedBusy, err := cr.routedWorkPoolReuseAssignedWork(snapshot, agent, current)
	if err != nil {
		return false, err
	}
	for i, info := range current {
		disposition, err := cr.authorizeRoutedWorkPoolReuse(snapshot, info, skipped[i].lease, false, assignedBusy[info.ID])
		if err != nil {
			return false, err
		}
		if disposition != routedWorkPoolReuseBusy {
			return false, nil
		}
	}
	return true, nil
}

func validateRoutedWorkPoolReuseLease(lease routedWorkPoolReuseLease) error {
	if lease.ControllerGeneration == 0 || lease.MembershipRevision == 0 {
		return fmt.Errorf("reuse generation and membership revision must be positive")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"session ID", lease.SessionID},
		{"instance token", lease.InstanceToken},
		{"pool target", lease.PoolTarget},
		{"work ID", lease.Binding.WorkID},
		{"source store", lease.Binding.StoreRef},
		{"work directory", lease.Binding.WorkDir},
	} {
		if field.value == "" || strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("reuse %s is not canonical", field.name)
		}
	}
	if (lease.PreviousWorkID == "") != (lease.PreviousSourceStore == "") {
		return fmt.Errorf("previous work ID and source store must both be set or empty")
	}
	if len(lease.MemberIDs) == 0 || !slices.IsSorted(lease.MemberIDs) {
		return fmt.Errorf("reuse member IDs must be nonempty and sorted")
	}
	for i, id := range lease.MemberIDs {
		if id == "" || strings.TrimSpace(id) != id || (i > 0 && lease.MemberIDs[i-1] == id) {
			return fmt.Errorf("reuse member IDs must be canonical and unique")
		}
	}
	if _, present := slices.BinarySearch(lease.MemberIDs, lease.SessionID); !present {
		return fmt.Errorf("reuse member IDs do not contain session ID")
	}
	return nil
}

// authorizeRoutedWorkPoolReuse repeats the full non-destructive reuse proof
// immediately before the fenced binding write and again before live delivery.
func (cr *CityRuntime) authorizeRoutedWorkPoolReuse(
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
	lease routedWorkPoolReuseLease,
	bound bool,
	assignedBusy bool,
) (routedWorkPoolReuseDisposition, error) {
	if cr == nil || cr.cs == nil || cr.poolMembershipShadow == nil || snapshot.Config == nil || snapshot.Provider == nil || snapshot.Store == nil {
		return routedWorkPoolReuseRefused, fmt.Errorf("authorizing pool reuse: keyed state is unavailable")
	}
	if err := validateRoutedWorkPoolReuseLease(lease); err != nil {
		return routedWorkPoolReuseRefused, err
	}
	cr.serviceStateMu.RLock()
	configCurrent := cr.cfg == snapshot.Config
	cr.serviceStateMu.RUnlock()
	agent := findAgentByTemplate(snapshot.Config, lease.PoolTarget)
	if !configCurrent || agent == nil || snapshot.Generation != lease.ControllerGeneration || info.ID != lease.SessionID || info.Closed ||
		strings.TrimSpace(info.InstanceToken) != lease.InstanceToken ||
		normalizedSessionTemplateInfo(info, snapshot.Config) != lease.PoolTarget ||
		!isPoolManagedSessionInfo(info) || isNamedSessionInfo(info) || isManualSessionInfoForAgent(info, agent) || info.DependencyOnly {
		return routedWorkPoolReuseRefused, nil
	}
	lifecycle := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInputFromInfo(info))
	if lifecycle.BaseState != sessionpkg.BaseStateActive || lifecycle.Terminal || !lifecycle.CountsAgainstCap {
		return routedWorkPoolReuseRefused, nil
	}
	if bound {
		if !lease.Binding.Matches(info) {
			return routedWorkPoolReuseRefused, nil
		}
	} else if strings.TrimSpace(info.TriggerBeadID) != lease.PreviousWorkID ||
		strings.TrimSpace(info.TriggerBeadStoreRef) != lease.PreviousSourceStore {
		return routedWorkPoolReuseRefused, nil
	}
	if strings.TrimSpace(agent.Nudge) == "" ||
		isAgentEffectivelySuspendedWith(snapshot.Config, snapshot.CityPath, agent, loadSuspensionStateBestEffort(snapshot.CityPath)) {
		return routedWorkPoolReuseRefused, nil
	}
	if !isEphemeralSessionInfoForAgent(info, agent) {
		return routedWorkPoolReuseRefused, nil
	}
	if agent.UsesCanonicalSingletonPoolIdentity() {
		if !isCanonicalPoolManagedSessionInfoForTemplate(info, lease.PoolTarget) {
			return routedWorkPoolReuseRefused, nil
		}
	} else if existingPoolSlotWithConfigInfo(snapshot.Config, agent, info) <= 0 {
		return routedWorkPoolReuseRefused, nil
	}
	namedTemplates := make(map[string]struct{}, len(snapshot.Config.NamedSessions))
	for i := range snapshot.Config.NamedSessions {
		namedTemplates[snapshot.Config.NamedSessions[i].TemplateQualifiedName()] = struct{}{}
	}
	policy := newPoolAllocationShadowPolicy(snapshot.Config, agent, namedTemplates).
		forSourceStore(snapshot.Config, agent, snapshot.CityPath, lease.Binding.StoreRef)
	if !policy.supported() || policy.maxActiveSessions == 0 {
		return routedWorkPoolReuseRefused, nil
	}
	observation, memberIDs, exact := cr.poolMembershipShadow.observeMemberIDs(lease.PoolTarget)
	if !exact || observation.revision != lease.MembershipRevision || !slices.Equal(memberIDs, lease.MemberIDs) {
		return routedWorkPoolReuseRefused, nil
	}
	occupiedObservation, occupied := cr.poolMembershipShadow.observeOccupiedMember(lease.PoolTarget, lease.SessionID)
	if !occupied || occupiedObservation.revision != lease.MembershipRevision {
		return routedWorkPoolReuseRefused, nil
	}
	if agent.UsesCanonicalSingletonPoolIdentity() {
		if observation.members != 1 || observation.occupied != 1 || len(memberIDs) != 1 || memberIDs[0] != lease.SessionID {
			return routedWorkPoolReuseRefused, nil
		}
	}
	if policy.maxActiveSessions > 0 && observation.occupied > policy.maxActiveSessions {
		return routedWorkPoolReuseRefused, nil
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" || !snapshot.Provider.IsRunning(name) {
		return routedWorkPoolReuseRefused, nil
	}
	busy := snapshot.Provider.IsAttached(name)
	for _, check := range []struct{ key, want string }{
		{"GC_SESSION_ID", lease.SessionID},
		{"GC_INSTANCE_TOKEN", lease.InstanceToken},
	} {
		got, err := snapshot.Provider.GetMeta(name, check.key)
		if err != nil {
			return routedWorkPoolReuseRefused, fmt.Errorf("authorizing pool reuse for %q: reading %s: %w", lease.SessionID, check.key, err)
		}
		if got != check.want {
			return routedWorkPoolReuseRefused, nil
		}
	}
	interactionProvider, ok := snapshot.Provider.(runtime.InteractionProvider)
	if !ok {
		return routedWorkPoolReuseRefused, fmt.Errorf("authorizing pool reuse for %q: provider cannot prove pending-interaction state", lease.SessionID)
	}
	pending, err := interactionProvider.Pending(name)
	if err != nil {
		return routedWorkPoolReuseRefused, fmt.Errorf("authorizing pool reuse for %q: checking pending interaction: %w", lease.SessionID, err)
	}
	if pending != nil {
		busy = true
	}
	if assignedBusy {
		busy = true
	}
	if lease.PreviousWorkID != "" {
		previousStore, ok := cr.cs.routedWorkStore(snapshot.Config, lease.PreviousSourceStore)
		if !ok || previousStore == nil {
			return routedWorkPoolReuseRefused, fmt.Errorf("authorizing pool reuse for %q: previous source store %q is unavailable", lease.SessionID, lease.PreviousSourceStore)
		}
		previous, err := beads.HandlesFor(previousStore).Live.Get(lease.PreviousWorkID)
		if errors.Is(err, beads.ErrNotFound) {
			return routedWorkPoolReuseRefused, nil
		}
		if err != nil {
			return routedWorkPoolReuseRefused, fmt.Errorf("authorizing pool reuse for %q: reading previous work %q: %w", lease.SessionID, lease.PreviousWorkID, err)
		}
		if previous.ID != lease.PreviousWorkID {
			return routedWorkPoolReuseRefused, nil
		}
		if previous.Status != "closed" {
			busy = true
		}
	}
	sourceStore, ok := cr.cs.routedWorkStore(snapshot.Config, lease.Binding.StoreRef)
	if !ok || sourceStore == nil {
		return routedWorkPoolReuseRefused, fmt.Errorf("authorizing pool reuse for %q: source store %q is unavailable", lease.SessionID, lease.Binding.StoreRef)
	}
	work, ready, err := authoritativeReadyRoutedWorkByID(sourceStore, lease.Binding.WorkID, time.Now().UTC())
	if err != nil {
		return routedWorkPoolReuseRefused, err
	}
	if !ready || !demandServableForTemplate(snapshot.Config, work, lease.PoolTarget) {
		return routedWorkPoolReuseRefused, nil
	}
	if busy {
		return routedWorkPoolReuseBusy, nil
	}
	return routedWorkPoolReuseReusable, nil
}
