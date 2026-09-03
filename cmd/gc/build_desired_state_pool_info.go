package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// This file holds the session.Info siblings of the raw pool selection/creation/
// reuse predicates in build_desired_state.go. W-pool types the pool create/reuse
// path so the two `InfoFromPersistedBead` projections at the raw pool-loop
// boundary disappear: selection returns session.Info and the normalize lane folds
// its store write onto Info instead of re-merging a raw bead. Each twin below is
// byte-identical to its raw form (which it replaces in the flip commit), reading
// projected Info fields where the raw form read bead metadata. The equivalence is
// pinned by the oracles in session_wpool_twins_test.go.

// sortSessionInfosByCreatedAtThenID is the Info sibling of
// sortSessionBeadsByCreatedAtThenID: it orders reuse candidates by CreatedAt then
// ID (stable), the deterministic general-reuse precedence.
func sortSessionInfosByCreatedAtThenID(candidates []session.Info) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
		}
		return candidates[i].ID < candidates[j].ID
	})
}

// poolRuntimeAliasIsDeferredInfo is the session.Info sibling of
// poolRuntimeAliasIsDeferred.
func poolRuntimeAliasIsDeferredInfo(info session.Info) bool {
	if strings.TrimSpace(info.Alias) != "" {
		return false
	}
	if strings.TrimSpace(info.PoolAliasConflict) != "" {
		return true
	}
	if strings.TrimSpace(info.PendingCreateClaimMetadata) == boolMetadata(true) {
		return true
	}
	state := strings.TrimSpace(info.MetadataState)
	return state == "creating" || state == string(session.StateStartPending)
}

// setPoolTemplateRuntimeIdentityInfo is the session.Info sibling of
// setPoolTemplateRuntimeIdentity.
func setPoolTemplateRuntimeIdentityInfo(tp *TemplateParams, desiredAlias string, info session.Info) {
	if tp == nil {
		return
	}
	if strings.TrimSpace(info.Alias) != strings.TrimSpace(desiredAlias) && poolRuntimeAliasIsDeferredInfo(info) {
		clearPoolTemplateRuntimeIdentity(tp)
		return
	}
	tp.Alias = desiredAlias
	setTemplateEnvIdentity(tp, desiredAlias)
}

// clearPoolTemplateRuntimeIdentity leaves a pool spawn with no public identity:
// no alias, an explicitly BLANK GC_ALIAS, and GC_AGENT on the session name.
//
// Blanking GC_ALIAS rather than skipping the stamp is load-bearing.
// resolveTemplate seeds GC_ALIAS with the agent's bare qualified name for every
// session (template_resolve.go), so a skipped stamp would leave every member of
// a pool advertising the SAME identity — worse than per-slot names, because
// then two live workers claim under one string. The empty value is also what
// tmux session creation reads as "unset this key" (`env -u`).
//
// This was already the deferred-alias behavior for a slot whose name was
// unavailable; unaliased pools make it the ordinary case.
func clearPoolTemplateRuntimeIdentity(tp *TemplateParams) {
	if tp == nil {
		return
	}
	tp.Alias = ""
	if tp.Env == nil {
		tp.Env = make(map[string]string)
	}
	tp.Env["GC_ALIAS"] = ""
	if tp.SessionName != "" {
		tp.Env["GC_AGENT"] = tp.SessionName
	}
	tp.EnvIdentityStamped = false
}

// claimPoolSlotWithConfigInfo is the session.Info sibling of
// claimPoolSlotWithConfig.
func claimPoolSlotWithConfigInfo(cfg *config.City, cfgAgent *config.Agent, info session.Info, used map[int]bool) int {
	if slot := existingPoolSlotWithConfigInfo(cfg, cfgAgent, info); slot > 0 {
		if used[slot] {
			return 0
		}
		used[slot] = true
		return slot
	}
	for slot := 1; ; slot++ {
		if used[slot] {
			continue
		}
		used[slot] = true
		return slot
	}
}

// preferredPoolSlotAboveCapacityInfo recovers a preferred session's concrete
// identity when the only configured bound it exceeds is max_active_sessions.
//
// Capacity shrink blocks new slots; it must not rename an already-assigned
// session. Requiring a matching stored template plus a concrete persisted
// agent/alias/session identity keeps stale, identity-less out-of-bounds
// pool_slot metadata on the existing bounded fallback path. Namepool length
// remains an identity bound even when max_active_sessions is temporarily lower.
func preferredPoolSlotAboveCapacityInfo(cfg *config.City, cfgAgent *config.Agent, info session.Info) int {
	if cfgAgent == nil || cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return 0
	}
	if cfg != nil && !storedTemplateMatchesPoolTemplate(
		sessionBeadStoredTemplateInfo(info),
		cfgAgent.QualifiedName(),
		cfg,
	) {
		return 0
	}
	maxSessions := cfgAgent.EffectiveMaxActiveSessions()
	if maxSessions == nil || *maxSessions <= 0 {
		return 0
	}

	slot := resolvePersistedPoolIdentitySlot(cfgAgent, true, sessionBeadAgentNameInfo(info))
	if slot == 0 {
		slot = resolvePersistedPoolIdentitySlot(cfgAgent, true, info.Alias)
	}
	if slot == 0 && strings.TrimSpace(info.Alias) == "" && !infoOwnsPoolSessionName(info) {
		slot = resolvePersistedPoolIdentitySlot(cfgAgent, true, info.SessionNameMetadata)
	}
	if slot <= *maxSessions {
		return 0
	}
	if len(cfgAgent.NamepoolNames) > 0 && slot > len(cfgAgent.NamepoolNames) {
		return 0
	}
	return slot
}

// claimPreferredPoolSlotWithConfigInfo preserves the concrete slot of a
// session carrying assigned work across a capacity reduction when
// preserveAboveCapacity is true. General reuse, in-flight-new, and
// fresh-create paths stay bounded by claimPoolSlotWithConfigInfo.
func claimPreferredPoolSlotWithConfigInfo(
	cfg *config.City,
	cfgAgent *config.Agent,
	info session.Info,
	preserveAboveCapacity bool,
	used map[int]bool,
) int {
	if cfgAgent == nil || cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return 0
	}
	if preserveAboveCapacity {
		if slot := preferredPoolSlotAboveCapacityInfo(cfg, cfgAgent, info); slot > 0 {
			if used[slot] {
				return 0
			}
			used[slot] = true
			return slot
		}
	}
	return claimPoolSlotWithConfigInfo(cfg, cfgAgent, info, used)
}

// claimDesiredPoolSlotInfo is the session.Info sibling of claimDesiredPoolSlot.
func claimDesiredPoolSlotInfo(cfg *config.City, cfgAgent *config.Agent, info session.Info, used map[int]bool) int {
	if cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return 0
	}
	return claimPoolSlotWithConfigInfo(cfg, cfgAgent, info, used)
}

// reusablePoolSessionInfo is the session.Info sibling of reusablePoolSessionBead.
// The SESSION side reads projected Info fields; the assigned-work slice stays raw
// (ClassWork — beads.Bead is its domain object) via sessionBeadHasAssignedWorkInfo.
func reusablePoolSessionInfo(bp *agentBuildParams, cfgAgent *config.Agent, template string, info session.Info, used map[string]bool) bool {
	if bp == nil {
		return false
	}
	if info.Closed {
		return false
	}
	if isDrainedSessionInfo(info) {
		return false
	}
	if isFailedCreateSessionInfo(info) {
		return false
	}
	// A draining session is being shut down (e.g. drain-ack-stop-pending) and
	// must not occupy the desired slot. Without this exclusion the min-floor
	// fires (draining doesn't consume demand) but the slot is filled by the
	// existing draining bead, so no replacement is started until the bead is
	// closed — one to several ticks later than necessary.
	if strings.TrimSpace(info.MetadataState) == string(session.StateDraining) {
		return false
	}
	if info.MetadataState == "asleep" {
		return false
	}
	if isManualSessionInfoForAgent(info, cfgAgent) {
		return false
	}
	if isNamedSessionInfo(info) {
		return false
	}
	if sessionBeadHasAssignedWorkInfo(bp.assignedWorkBeads, info) {
		return false
	}
	if used != nil && used[info.ID] {
		return false
	}
	return resolvedSessionTemplateInfo(info, reuseTemplateConfig(bp)) == template
}

// reusablePoolSessionInfos is the session.Info sibling of reusablePoolSessionBeads.
func reusablePoolSessionInfos(bp *agentBuildParams, cfgAgent *config.Agent, template string, used map[string]bool) []session.Info {
	if bp == nil || bp.sessionBeads == nil {
		return nil
	}
	candidates := []session.Info{}
	for _, info := range bp.sessionBeads.OpenInfos() {
		if reusablePoolSessionInfo(bp, cfgAgent, template, info, used) {
			candidates = append(candidates, info)
		}
	}
	sortSessionInfosByCreatedAtThenID(candidates)
	return candidates
}

// reusablePoolSessionInfosForRequest narrows ordinary reuse for anonymous
// generic demand. A currently held/quarantined session cannot satisfy that
// demand because the awake evaluator will suppress it. Dependency-only slots
// remain reusable: real demand promotes them and session sync clears the marker
// before awake evaluation. Concrete requests and pending creates retain their
// existing reuse semantics: the former preserve resume identity, while the
// latter must finish the create already counted as in-flight demand even if a
// hold lands mid-create.
func reusablePoolSessionInfosForRequest(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	request SessionRequest,
	decisionTime time.Time,
	used map[string]bool,
) []session.Info {
	candidates := reusablePoolSessionInfos(bp, cfgAgent, template, used)
	if request.SessionBeadID != "" {
		return candidates
	}
	filtered := candidates[:0]
	for _, info := range candidates {
		if poolSessionConsumesNewDemandInfo(info) {
			filtered = append(filtered, info)
			continue
		}
		if strings.TrimSpace(info.WaitHold) != "" ||
			metadataTimeInFuture(info.HeldUntil, decisionTime) ||
			metadataTimeInFuture(info.QuarantinedUntil, decisionTime) {
			continue
		}
		filtered = append(filtered, info)
	}
	return filtered
}

// findReusableCanonicalNonExpandingPoolSessionInfo is the session.Info sibling of
// findReusableCanonicalNonExpandingPoolSessionBead.
func findReusableCanonicalNonExpandingPoolSessionInfo(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	used map[string]bool,
) (session.Info, bool) {
	if bp == nil || bp.sessionBeads == nil || !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return session.Info{}, false
	}
	canonical := cfgAgent.QualifiedName()
	for _, info := range reusablePoolSessionInfos(bp, cfgAgent, template, used) {
		if strings.TrimSpace(info.SessionNameMetadata) == "" {
			continue
		}
		if staleNonExpandingPoolSessionBeadInfo(cfgAgent, info) {
			continue
		}
		if infoIdentifiesAsCanonical(info, canonical) {
			return info, true
		}
	}
	return session.Info{}, false
}

func findReusableCanonicalNonExpandingPoolSessionInfoForRequest(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	request SessionRequest,
	decisionTime time.Time,
	used map[string]bool,
) (session.Info, bool) {
	if bp == nil || bp.sessionBeads == nil || !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return session.Info{}, false
	}
	canonical := cfgAgent.QualifiedName()
	for _, info := range reusablePoolSessionInfosForRequest(bp, cfgAgent, template, request, decisionTime, used) {
		if strings.TrimSpace(info.SessionNameMetadata) == "" {
			continue
		}
		if staleNonExpandingPoolSessionBeadInfo(cfgAgent, info) {
			continue
		}
		if infoIdentifiesAsCanonical(info, canonical) {
			return info, true
		}
	}
	return session.Info{}, false
}

// reusableDependencyPoolSessionInfo is the session.Info sibling of
// reusableDependencyPoolSessionBead.
func reusableDependencyPoolSessionInfo(bp *agentBuildParams, template string, info session.Info) bool {
	if bp == nil {
		return false
	}
	if info.Closed || isManualSessionInfo(info) {
		return false
	}
	if isDrainedSessionInfo(info) {
		return false
	}
	if isFailedCreateSessionInfo(info) {
		return false
	}
	if isNamedSessionInfo(info) {
		return false
	}
	if info.DependencyOnlyMetadata != boolMetadata(true) {
		return false
	}
	if resolvedSessionTemplateInfo(info, reuseTemplateConfig(bp)) != template {
		return false
	}
	return strings.TrimSpace(info.SessionNameMetadata) != ""
}

// reusableDependencyPoolSessionInfos is the session.Info sibling of
// reusableDependencyPoolSessionBeads.
func reusableDependencyPoolSessionInfos(bp *agentBuildParams, template string) []session.Info {
	if bp == nil || bp.sessionBeads == nil {
		return nil
	}
	candidates := []session.Info{}
	for _, info := range bp.sessionBeads.OpenInfos() {
		if reusableDependencyPoolSessionInfo(bp, template, info) {
			candidates = append(candidates, info)
		}
	}
	sortSessionInfosByCreatedAtThenID(candidates)
	return candidates
}

// reusableDependencyPoolSessionInfosAt excludes dependency rows that cannot
// run at decisionTime. A held/quarantined dependency-only row still owns its
// concrete slot, but it cannot satisfy the floor; the holder-aware fresh-slot
// allocator therefore chooses another in-cap slot for the prerequisite.
func reusableDependencyPoolSessionInfosAt(bp *agentBuildParams, template string, decisionTime time.Time) []session.Info {
	candidates := reusableDependencyPoolSessionInfos(bp, template)
	filtered := candidates[:0]
	for _, info := range candidates {
		if strings.TrimSpace(info.WaitHold) != "" ||
			metadataTimeInFuture(info.HeldUntil, decisionTime) ||
			metadataTimeInFuture(info.QuarantinedUntil, decisionTime) {
			continue
		}
		filtered = append(filtered, info)
	}
	return filtered
}

func findReusableCanonicalNonExpandingDependencyPoolSessionInfoAt(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	template string,
	decisionTime time.Time,
) (session.Info, bool) {
	if bp == nil || bp.sessionBeads == nil || !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
		return session.Info{}, false
	}
	canonical := cfgAgent.QualifiedName()
	for _, info := range reusableDependencyPoolSessionInfosAt(bp, template, decisionTime) {
		if staleNonExpandingPoolSessionBeadInfo(cfgAgent, info) {
			continue
		}
		if infoIdentifiesAsCanonical(info, canonical) {
			return info, true
		}
	}
	return session.Info{}, false
}

// queueClearPoolAliasConflictMetadataInfo is the session.Info sibling of
// queueClearPoolAliasConflictMetadata: it queues an empty-string clear for each
// pool-alias-conflict key the session currently carries (reading the Info mirrors),
// so the collapse write drops the deferred-conflict bookkeeping.
func queueClearPoolAliasConflictMetadataInfo(metadata map[string]string, info session.Info) {
	if info.PoolAliasConflict != "" {
		metadata[poolAliasConflictMetadataKey] = ""
	}
	if info.PoolAliasConflictCount != "" {
		metadata[poolAliasConflictCountMetadataKey] = ""
	}
	if info.PoolAliasConflictAt != "" {
		metadata[poolAliasConflictAtMetadataKey] = ""
	}
}

// normalizeNonExpandingPoolSessionInfo is the session.Info sibling of
// normalizeNonExpandingPoolSessionBead. It computes the byte-identical singleton
// pool-identity collapse (agent_name/alias/pool_slot metadata, title, and
// agent:<slot> label pruning), persists the SAME bp.beadStore.Update the raw form
// issued, and — instead of re-merging the change set into a raw bead — folds it
// onto the returned Info: ApplyPatch of the metadata batch plus the same title and
// label mutations. The returned Info is the authoritative post-write value; callers
// must use it rather than re-reading the snapshot for this id this tick.
func normalizeNonExpandingPoolSessionInfo(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	info session.Info,
) (session.Info, error) {
	if bp == nil || bp.beadStore == nil || !cfgAgent.UsesCanonicalSingletonPoolIdentity() || isManualSessionInfoForAgent(info, cfgAgent) || isNamedSessionInfo(info) || info.ID == "" {
		return info, nil
	}
	canonical := cfgAgent.QualifiedName()
	metadata := map[string]string{}
	aliasNeedsUpdate := false
	clearAliasConflictMetadata := func() {
		queueClearPoolAliasConflictMetadataInfo(metadata, info)
	}
	alias := strings.TrimSpace(info.Alias)
	deferredAlias := strings.TrimSpace(info.PoolAliasConflict)
	if nonExpandingPoolIdentitySlot(cfgAgent, sessionBeadAgentNameInfo(info)) > 0 && strings.TrimSpace(info.AgentName) != canonical {
		metadata["agent_name"] = canonical
	}
	if (nonExpandingPoolIdentitySlot(cfgAgent, alias) > 0 && alias != canonical) || (alias == "" && deferredAlias == canonical) {
		for key, value := range session.UpdatedAliasMetadataFromInfo(info, canonical) {
			metadata[key] = value
		}
		clearAliasConflictMetadata()
		aliasNeedsUpdate = true
	}
	if alias == canonical {
		clearAliasConflictMetadata()
	}
	if strings.TrimSpace(info.PoolSlot) != "" {
		metadata["pool_slot"] = ""
	}

	var title *string
	if nonExpandingPoolIdentitySlot(cfgAgent, info.Title) > 0 && strings.TrimSpace(info.Title) != canonical {
		normalizedTitle := canonical
		title = &normalizedTitle
	}

	removeLabels := make([]string, 0, len(info.Labels))
	hasCanonicalAgentLabel := containsString(info.Labels, "agent:"+canonical)
	for _, label := range info.Labels {
		label = strings.TrimSpace(label)
		if strings.HasPrefix(label, "agent:") && nonExpandingPoolIdentitySlot(cfgAgent, strings.TrimPrefix(label, "agent:")) > 0 {
			removeLabels = append(removeLabels, label)
		}
	}
	var addLabels []string
	if (len(metadata) > 0 || title != nil || len(removeLabels) > 0) && !hasCanonicalAgentLabel {
		addLabels = []string{"agent:" + canonical}
	}
	if len(metadata) == 0 && title == nil && len(removeLabels) == 0 && len(addLabels) == 0 {
		return info, nil
	}

	apply := func() error {
		return bp.beadStore.Update(info.ID, beads.UpdateOpts{
			Title:        title,
			Metadata:     metadata,
			Labels:       addLabels,
			RemoveLabels: removeLabels,
		})
	}
	if aliasNeedsUpdate {
		if err := session.WithCitySessionAliasLock(bp.cityPath, canonical, func() error {
			if err := session.EnsureAliasAvailableWithConfigForOwner(bp.beadStore, bp.city, canonical, info.ID, canonical); err != nil {
				return err
			}
			return apply()
		}); err != nil {
			return info, fmt.Errorf("normalizing singleton pool identity for bead %s to %q: %w", info.ID, canonical, err)
		}
	} else if err := apply(); err != nil {
		return info, fmt.Errorf("normalizing singleton pool identity for bead %s to %q: %w", info.ID, canonical, err)
	}

	if bp.stderr != nil {
		fmt.Fprintf(bp.stderr, "buildDesiredState: pool %q: collapsing phantom pool identity for bead %s to %q\n", canonical, info.ID, canonical) //nolint:errcheck
	}
	folded := info.ApplyPatch(session.MetadataPatch(metadata))
	if title != nil {
		folded.Title = *title
	}
	if len(removeLabels) > 0 || len(addLabels) > 0 {
		remove := make(map[string]bool, len(removeLabels))
		for _, label := range removeLabels {
			remove[label] = true
		}
		filtered := make([]string, 0, len(folded.Labels)+len(addLabels))
		for _, label := range folded.Labels {
			if !remove[label] {
				filtered = append(filtered, label)
			}
		}
		folded.Labels = filtered
	}
	for _, label := range addLabels {
		if !containsString(folded.Labels, label) {
			folded.Labels = append(folded.Labels, label)
		}
	}
	return folded, nil
}

// recordDeferredNonExpandingPoolAliasConflictInfo is the session.Info sibling of
// recordDeferredNonExpandingPoolAliasConflict. It records the deferred-alias
// bookkeeping via the SAME bp.beadStore.Update and folds the batch onto the
// returned Info (ApplyPatch), the authoritative post-write value. Writes are
// throttled by the shared deferredSingletonAliasRetryDue backoff gate (see
// session_beads.go) keyed off the same pool_alias_conflict_at field the
// sync-time path reads, so this call site cannot reset that path's clock
// without also being subject to it.
func recordDeferredNonExpandingPoolAliasConflictInfo(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	info session.Info,
) (session.Info, error) {
	canonical := cfgAgent.QualifiedName()
	count := 0
	if existing, err := strconv.Atoi(strings.TrimSpace(info.PoolAliasConflictCount)); err == nil && existing > 0 {
		count = existing
	}
	// Canonical singleton pool identity retries share the same unresolvable-
	// conflict shape as the sync-time path in session_beads.go (a
	// max_active_sessions=1 agent with two live sessions never releases the
	// alias), so it must go through the SAME deferredSingletonAliasRetryDue
	// gate rather than writing on every buildDesiredState call. Do not
	// duplicate the backoff predicate here -- that duplication is exactly how
	// this call site went unguarded the first time.
	if !deferredSingletonAliasRetryDue(info.PoolAliasConflictAt, count, time.Now().UTC()) {
		return info, nil
	}
	metadata := session.UpdatedAliasMetadataFromInfo(info, "")
	metadata[poolAliasConflictMetadataKey] = canonical
	metadata[poolAliasConflictCountMetadataKey] = strconv.Itoa(count + 1)
	metadata[poolAliasConflictAtMetadataKey] = time.Now().UTC().Format(time.RFC3339)
	if bp != nil && bp.beadStore != nil && info.ID != "" {
		if err := bp.beadStore.Update(info.ID, beads.UpdateOpts{Metadata: metadata}); err != nil {
			return info, fmt.Errorf("recording deferred singleton pool alias conflict for bead %s: %w", info.ID, err)
		}
	}
	return info.ApplyPatch(session.MetadataPatch(metadata)), nil
}

// normalizeNonExpandingPoolSessionInfoForSelection is the session.Info sibling of
// normalizeNonExpandingPoolSessionBeadForSelection: it normalizes the singleton
// pool identity and, on a canonical alias collision, records the deferred-conflict
// bookkeeping instead of failing selection.
func normalizeNonExpandingPoolSessionInfoForSelection(
	bp *agentBuildParams,
	cfgAgent *config.Agent,
	info session.Info,
) (session.Info, error) {
	folded, err := normalizeNonExpandingPoolSessionInfo(bp, cfgAgent, info)
	if err == nil {
		return folded, nil
	}
	if !cfgAgent.UsesCanonicalSingletonPoolIdentity() || !errors.Is(err, session.ErrSessionAliasExists) {
		return folded, err
	}
	if bp != nil && bp.stderr != nil {
		fmt.Fprintf(bp.stderr, "buildDesiredState: pool %q: deferring singleton pool identity normalization for bead %s: %v\n", cfgAgent.QualifiedName(), info.ID, err) //nolint:errcheck
	}
	return recordDeferredNonExpandingPoolAliasConflictInfo(bp, cfgAgent, info)
}
