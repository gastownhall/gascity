package main

import (
	"context"
	"errors"
	"log"
	"path"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// sessionBeadAssigneeIdentities returns every identifier under which a work
// bead could be assigned to this session: the session bead ID, session_name,
// configured_named_identity, current alias, and any prior aliases preserved
// in alias_history. Pool polecat aliases (e.g. "nux") are first-class
// assignment identities, so leaving them out of orphan-detection causes
// in-progress work to be reset under a live owner — see the
// SkipsLiveSessionAssignedByAlias regression tests.
func sessionBeadAssigneeIdentities(sb beads.Bead) []string {
	identities := make([]string, 0, 5)
	if id := strings.TrimSpace(sb.ID); id != "" {
		identities = append(identities, id)
	}
	if sn := strings.TrimSpace(sb.Metadata["session_name"]); sn != "" {
		identities = append(identities, sn)
	}
	if ni := strings.TrimSpace(sb.Metadata["configured_named_identity"]); ni != "" {
		identities = append(identities, ni)
	}
	if al := strings.TrimSpace(sb.Metadata["alias"]); al != "" {
		identities = append(identities, al)
	}
	for _, prior := range session.AliasHistory(sb.Metadata) {
		if prior = strings.TrimSpace(prior); prior != "" {
			identities = append(identities, prior)
		}
	}
	return identities
}

// sessionBeadAssigneeIdentitiesInfo is the session.Info mirror of
// sessionBeadAssigneeIdentities. It reads the RAW session_name
// (Info.SessionNameMetadata) and the pre-normalized Info.AliasHistory. The body
// is the confined session.AssigneeIdentities codec; the bead-form peer above
// stays inline to avoid a per-iteration Info projection in the hot reconciler
// loops (the classifier-equivalence oracle guards their agreement).
func sessionBeadAssigneeIdentitiesInfo(i session.Info) []string {
	return session.AssigneeIdentities(i)
}

type releasedPoolAssignment struct {
	ID    string
	Index int
}

// PoolSessionName derives the tmux session name for a pool worker session.
// Format: {basename(template)}-{beadID} (e.g., "claude-mc-xyz").
// Named sessions with an alias use the alias instead.
func PoolSessionName(template, beadID string) string {
	base := path.Base(template)
	return agent.SanitizeQualifiedNameForSession(base) + "-" + beadID
}

// GCSweepSessionBeads closes open session beads that have no remaining
// open/in-progress work beads anywhere — primary store OR any attached
// rig store. Work-bead assignment is verified by a live cross-store
// query inside closeSessionInfoIfUnassigned, so the caller does not
// pass a work snapshot — that pattern was retired to prevent pre-close
// tick snapshots from poisoning close decisions. Candidates arrive as the
// typed session.Info projection (WI-5 W4); the close is a session-class op
// routed through the session front door. Returns the IDs of session beads
// that were closed.
func GCSweepSessionBeads(store beads.Store, rigStores map[string]beads.Store, sessionInfos []session.Info) []string {
	var closed []string
	for _, info := range sessionInfos {
		if info.Closed {
			continue
		}
		if !closeSessionInfoIfUnassigned(store, rigStores, nil, info, "gc_swept", time.Now().UTC(), nil) {
			continue
		}
		closed = append(closed, info.ID)
	}
	return closed
}

// releaseOrphanedPoolAssignmentsWhenSnapshotsComplete skips orphan release
// unless both the assigned-work and open-session snapshots are complete.
func releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(
	store beads.Store,
	cfg *config.City,
	cityPath string,
	openSessionInfos []session.Info,
	result DesiredStateResult,
	rigStores map[string]beads.Store,
) []releasedPoolAssignment {
	// Partial input snapshots can make active work look orphaned for this
	// tick only: missing work affects drain decisions, and missing sessions
	// affects assigned-work orphan release.
	if result.snapshotQueryPartial() {
		return nil
	}
	return releaseOrphanedPoolAssignments(store, cfg, cityPath, openSessionInfos, result.AssignedWorkBeads, result.AssignedWorkStores, result.AssignedWorkStoreRefs, rigStores)
}

// releaseOrphanedPoolAssignments reopens active pool-routed work whose
// assignee no longer maps to any open session bead. This also recovers
// pool-routed work left in_progress with no assignee, which cannot be claimed
// again until it is moved back to open.
func releaseOrphanedPoolAssignments(
	store beads.Store,
	cfg *config.City,
	cityPath string,
	openSessionInfos []session.Info,
	assignedWorkBeads []beads.Bead,
	assignedWorkStores []beads.Store,
	assignedWorkStoreRefs []string,
	rigStores map[string]beads.Store,
) []releasedPoolAssignment {
	if store == nil || cfg == nil || len(assignedWorkBeads) == 0 {
		return nil
	}
	storeAware := len(assignedWorkStores) > 0
	if storeAware && len(assignedWorkStores) != len(assignedWorkBeads) {
		log.Printf("releaseOrphanedPoolAssignments: assigned work/store length mismatch: work=%d stores=%d", len(assignedWorkBeads), len(assignedWorkStores))
	}
	storeRefAware := len(assignedWorkStoreRefs) == len(assignedWorkBeads)
	if len(assignedWorkStoreRefs) > 0 && !storeRefAware {
		log.Printf("releaseOrphanedPoolAssignments: assigned work/store-ref length mismatch: work=%d storeRefs=%d", len(assignedWorkBeads), len(assignedWorkStoreRefs))
	}

	openIdentifiers := makeOpenSessionStoreRefIndex(cityPath, cfg, openSessionInfos, storeRefAware)
	legacyOpenIdentifiers := make(map[string]struct{}, len(openSessionInfos)*5)
	for _, info := range openSessionInfos {
		if info.Closed {
			continue
		}
		for _, id := range sessionBeadAssigneeIdentitiesInfo(info) {
			legacyOpenIdentifiers[id] = struct{}{}
		}
	}

	var released []releasedPoolAssignment
	for i, wb := range assignedWorkBeads {
		if wb.Status != "open" && wb.Status != "in_progress" {
			continue
		}
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" && wb.Status == "in_progress" && isCanonicalWorkflowRoot(wb) {
			continue
		}
		template := routedToOrLegacyWorkflowTarget(wb)
		if template == "" {
			continue
		}
		agentCfg := findAgentByTemplate(cfg, template)
		if agentCfg == nil || !agentCfg.SupportsGenericEphemeralSessions() {
			continue
		}
		if assignee == "" {
			if wb.Status != "in_progress" {
				continue
			}
		} else {
			workStoreRef := ""
			if storeRefAware {
				workStoreRef = assignedWorkStoreRefs[i]
			}
			if openSessionOwnsWork(legacyOpenIdentifiers, openIdentifiers, assignee, workStoreRef, storeRefAware) {
				continue
			}
			if assigneePreservesNamedSessionRoute(cfg, cityPath, template, assignee, workStoreRef, storeRefAware) {
				continue
			}
			if liveOpenSessionAssignmentExists(store, assignee) {
				continue
			}
		}

		var ownerStore beads.Store
		if storeAware {
			if i >= len(assignedWorkStores) || assignedWorkStores[i] == nil {
				log.Printf("releaseOrphanedPoolAssignments: missing owner store for assigned work %q at index %d", wb.ID, i)
				continue
			}
			ownerStore = assignedWorkStores[i]
		} else {
			ownerStore = storeForPoolAssignment(cfg, store, rigStores, wb)
			if ownerStore == nil {
				continue
			}
		}
		if !liveWorkAssignmentStillReleasable(ownerStore, wb.ID, wb.Status, assignee) {
			continue
		}
		allowsRelease, clearDetached := detachedProbeAllowsOrphanRelease(wb)
		if !allowsRelease {
			continue
		}
		if !releaseOrphanedPoolAssignment(ownerStore, wb, clearDetached) {
			continue
		}
		released = append(released, releasedPoolAssignment{ID: wb.ID, Index: i})
	}
	return released
}

func detachedProbeAllowsOrphanRelease(wb beads.Bead) (bool, bool) {
	spec := strings.TrimSpace(wb.Metadata[detachedProbeMetadataKey])
	if spec == "" {
		clearDetachedProbeErrorCount(wb.ID)
		return true, false
	}

	result := probeDetachedWork(context.Background(), spec)
	switch result.Status {
	case detachedProbeAlive:
		clearDetachedProbeErrorCount(wb.ID)
		log.Printf("releaseOrphanedPoolAssignments: skipping release: detached probe alive for %s: %s", wb.ID, spec)
		return false, false
	case detachedProbeDead:
		clearDetachedProbeErrorCount(wb.ID)
		log.Printf("releaseOrphanedPoolAssignments: releasing %s: detached probe dead: %s", wb.ID, spec)
		return true, true
	case detachedProbeError, detachedProbeTimeout:
		count := incrementDetachedProbeErrorCount(wb.ID)
		if count < detachedProbeErrorThreshold {
			log.Printf("releaseOrphanedPoolAssignments: detached probe %s for %s: %v (error %d/%d)", result.Status, wb.ID, result.Err, count, detachedProbeErrorThreshold)
			return false, false
		}
		clearDetachedProbeErrorCount(wb.ID)
		log.Printf("releaseOrphanedPoolAssignments: releasing %s: detached probe %s after %d errors: %v", wb.ID, result.Status, count, result.Err)
		return true, true
	default:
		count := incrementDetachedProbeErrorCount(wb.ID)
		if count < detachedProbeErrorThreshold {
			log.Printf("releaseOrphanedPoolAssignments: detached probe unknown result for %s: %q (error %d/%d)", wb.ID, result.Status, count, detachedProbeErrorThreshold)
			return false, false
		}
		clearDetachedProbeErrorCount(wb.ID)
		return true, true
	}
}

func clearDetachedProbeMetadata(store beads.Store, id string) {
	if store == nil || id == "" {
		return
	}
	// The detached-probe metadata contract lives on a WORK bead, so route the
	// clear through the work-assignment front door rather than reaching the WORK
	// store directly. The façade emits the same SetMetadata(id, gc.detached, "")
	// empty-string clear (proven byte-identical by the recording-fake write test).
	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	if err := wa.ClearDetachedProbe(id); err != nil {
		log.Printf("clearing detached probe metadata for %s: %v", id, err)
	}
}

const unresolvedOpenSessionStoreRef = "\x00unresolved"

// crossStoreOpenSessionStoreRef marks an open session whose backing agent is
// cross-store eligible (city-scoped). Such a session federates across every
// store (vp-kvp), so openSessionOwnsWork matches it against any work store-ref.
// The \x00 prefix cannot collide with a real rig name.
const crossStoreOpenSessionStoreRef = "\x00crossstore"

func makeOpenSessionStoreRefIndex(cityPath string, cfg *config.City, openSessionInfos []session.Info, storeRefAware bool) map[string]map[string]struct{} {
	index := make(map[string]map[string]struct{}, len(openSessionInfos)*5)
	if !storeRefAware {
		return index
	}
	for _, info := range openSessionInfos {
		if info.Closed {
			continue
		}
		// The caller feeds the typed snapshot (OpenInfos()); read the session
		// through Info for both the store-ref resolution and the assignee
		// identities (WI-5 W4 — the boundary projection this loop used to carry
		// moved to the snapshot's load edge).
		storeRef := openSessionReachableStoreRefInfo(cityPath, cfg, info)
		for _, id := range sessionBeadAssigneeIdentitiesInfo(info) {
			addOpenSessionStoreRef(index, id, storeRef)
		}
	}
	return index
}

func addOpenSessionStoreRef(index map[string]map[string]struct{}, identifier, storeRef string) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return
	}
	refs := index[identifier]
	if refs == nil {
		refs = make(map[string]struct{}, 1)
		index[identifier] = refs
	}
	refs[storeRef] = struct{}{}
}

func openSessionOwnsWork(legacyIdentifiers map[string]struct{}, scopedIdentifiers map[string]map[string]struct{}, assignee, workStoreRef string, storeRefAware bool) bool {
	if !storeRefAware {
		_, ok := legacyIdentifiers[assignee]
		return ok
	}
	refs := scopedIdentifiers[assignee]
	if refs == nil {
		return false
	}
	if _, ok := refs[unresolvedOpenSessionStoreRef]; ok {
		return true
	}
	if _, ok := refs[crossStoreOpenSessionStoreRef]; ok {
		return true
	}
	_, ok := refs[workStoreRef]
	return ok
}

func storeForPoolAssignment(cfg *config.City, cityStore beads.Store, rigStores map[string]beads.Store, wb beads.Bead) beads.Store {
	if cfg == nil || len(rigStores) == 0 {
		return cityStore
	}
	routed := routedToOrLegacyWorkflowTarget(wb)
	if routed != "" {
		if slash := strings.IndexByte(routed, '/'); slash > 0 {
			if store := rigStores[routed[:slash]]; store != nil {
				return store
			}
		}
	}
	idPrefix := sling.BeadPrefixForCity(cfg, wb.ID)
	for _, rig := range cfg.Rigs {
		if strings.EqualFold(idPrefix, rig.EffectivePrefix()) {
			if store := rigStores[rig.Name]; store != nil {
				return store
			}
		}
	}
	return cityStore
}

func isRecoverableUnassignedInProgressPoolWork(cfg *config.City, wb beads.Bead) bool {
	if wb.Status != "in_progress" || strings.TrimSpace(wb.Assignee) != "" {
		return false
	}
	template := routedToOrLegacyWorkflowTarget(wb)
	if template == "" {
		return false
	}
	if isCanonicalWorkflowRoot(wb) {
		return false
	}
	agentCfg := findAgentByTemplate(cfg, template)
	return agentCfg != nil && agentCfg.SupportsGenericEphemeralSessions()
}

func isCanonicalWorkflowRoot(wb beads.Bead) bool {
	return sourceworkflow.IsWorkflowRoot(wb) && legacyWorkflowRunTarget(wb) == ""
}

// releaseOrphanedPoolAssignment clears wb's assignment (assignee -> "",
// status -> open) plus the session-affinity metadata, preferring the store's
// atomic conditional release so a legitimate re-claim landing between the
// orphan staleness check and the release write is never clobbered.
//
// Release order:
//
//  1. beads.ConditionalAssignmentReleaser.ReleaseIfCurrent when the store
//     offers it for this snapshot shape (in_progress with a non-empty assignee
//     — the verb's contract) AND the bead carries no active continuation-group
//     routing vector (see beadHasActiveContinuationGroup). On BdStore this
//     currently rides raw `bd sql`; when bd grows a native conditional-release
//     verb it slots in inside BdStore.ReleaseIfCurrent (feature-detect the
//     verb, fall back to `bd sql` on unsupported) and this caller needs no
//     change.
//  2. Otherwise the tightest conditional path the store layer offers:
//     beads.UpdateOpts has no conditional fields, so re-verify the snapshot
//     with a live read immediately before the unconditional write and re-read
//     after it, logging loudly when a concurrent claim raced the release. The
//     residual recheck->write window cannot be closed without a store-level
//     conditional write; it is shrunk and made observable instead of silent.
//     This single Update also clears the affinity metadata alongside
//     status/assignee, so it is the correct path for continuation-group beads:
//     the group is never exposed on an open, unassigned bead.
func releaseOrphanedPoolAssignment(store beads.Store, wb beads.Bead, clearDetached bool) bool {
	if store == nil || strings.TrimSpace(wb.ID) == "" {
		return false
	}
	// Continuation-group beads bypass the CAS fast path: ReleaseIfCurrent swaps
	// only status/assignee, so clearing the group would need a second write, and
	// that gap would expose the routing vector on a claimable bead. The recheck
	// fallback clears status, assignee, and affinity metadata in one Update.
	if !beadHasActiveContinuationGroup(wb) {
		if released, handled := releasePoolAssignmentIfCurrent(store, wb); handled {
			if !released {
				return false
			}
			clearReleasedPoolAssignmentMetadata(store, wb.ID, clearDetached)
			return true
		}
	}
	return releasePoolAssignmentWithRecheck(store, wb, clearDetached)
}

// beadHasActiveContinuationGroup reports whether wb still advertises the active
// continuation-group routing vector (gc.continuation_group). Such beads must
// skip the two-write CAS release path: ReleaseIfCurrent swaps only
// status/assignee, so the follow-up metadata clear rides a separate write, and
// in that gap the bead is open and unassigned while gc.continuation_group is
// still set. A concurrent `gc hook --claim` can then vacuum the bead (or its
// {root, group} siblings) onto a new session via the stale group —
// preassignHookContinuationGroup / hookListContinuationWithBdStore route on
// gc.continuation_group + gc.root_bead_id. Routing these beads through
// releasePoolAssignmentWithRecheck clears status, assignee, and the affinity
// metadata in a single Update, so the group is never visible on a claimable
// bead. gc.session_affinity is an advisory marker no routing path reads (see the
// beadmeta.SessionAffinityMetadataKeys doc), so it needs no such guard and the
// CAS path still clears it. Lift this once bd's native conditional-release verb
// can clear the metadata in the same guarded write (BdStore.ReleaseIfCurrent
// SEAM).
func beadHasActiveContinuationGroup(wb beads.Bead) bool {
	return strings.TrimSpace(wb.Metadata[beadmeta.ContinuationGroupMetadataKey]) != ""
}

// releasePoolAssignmentIfCurrent attempts the store's atomic conditional
// release. handled=false means the store cannot conditionally release this
// snapshot (no ConditionalAssignmentReleaser, ErrConditionalReleaseUnsupported,
// or a snapshot shape outside the verb's contract) and the caller must take
// the recheck fallback. handled=true with released=false means the store
// answered authoritatively and the release must NOT be retried unconditionally.
func releasePoolAssignmentIfCurrent(store beads.Store, wb beads.Bead) (released, handled bool) {
	expectedAssignee := strings.TrimSpace(wb.Assignee)
	// ReleaseIfCurrent's contract covers in_progress assignments only, and bd
	// backends may persist an unassigned bead as SQL NULL rather than '', so
	// open-status strands (issue #2793) and assignee-less in_progress recovery
	// take the recheck fallback.
	if wb.Status != "in_progress" || expectedAssignee == "" {
		return false, false
	}
	releaser, ok := store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, false
	}
	released, err := releaser.ReleaseIfCurrent(wb.ID, expectedAssignee)
	if err != nil {
		if errors.Is(err, beads.ErrConditionalReleaseUnsupported) {
			return false, false
		}
		// The store supports conditional release but this attempt failed
		// (transient backend error). Skip the tick rather than downgrade to an
		// unconditional write that could clobber a concurrent re-claim; the
		// reconciler retries next tick.
		log.Printf("releaseOrphanedPoolAssignments: conditional release failed for %s: %v", wb.ID, err)
		return false, true
	}
	if !released {
		log.Printf("releaseOrphanedPoolAssignments: skipping release for %s: assignment changed since snapshot (re-claimed or transitioned)", wb.ID)
	}
	return released, true
}

// clearReleasedPoolAssignmentMetadata clears session-affinity (and optionally
// detached-probe) metadata after a successful conditional release. The clear
// rides a separate metadata-only write because ReleaseIfCurrent only swaps
// status/assignee. A failure here does not undo the release: stale affinity
// keys are overwritten on the next assignment, and detached-probe metadata is
// only consulted for release candidates, which an open unassigned bead is not.
func clearReleasedPoolAssignmentMetadata(store beads.Store, id string, clearDetached bool) {
	metadata := clearedSessionAffinityMetadata()
	if clearDetached {
		metadata[detachedProbeMetadataKey] = ""
	}
	if err := store.Update(id, beads.UpdateOpts{Metadata: metadata}); err != nil {
		log.Printf("releaseOrphanedPoolAssignments: clearing metadata after releasing %s: %v", id, err)
	}
}

// releasePoolAssignmentWithRecheck is the conditional-release fallback for
// stores without a usable ReleaseIfCurrent: re-verify (status, assignee) with
// a live read immediately before the unconditional write — after the earlier
// staleness gate and the potentially slow detached probe — then verify after
// the write that no concurrent claim raced the release.
func releasePoolAssignmentWithRecheck(store beads.Store, wb beads.Bead, clearDetached bool) bool {
	expectedAssignee := strings.TrimSpace(wb.Assignee)
	if !liveWorkAssignmentStillReleasable(store, wb.ID, wb.Status, expectedAssignee) {
		log.Printf("releaseOrphanedPoolAssignments: skipping release for %s: assignment changed between staleness check and release write", wb.ID)
		return false
	}
	opts := beads.UpdateOpts{
		Assignee: stringPtr(""),
		Status:   stringPtr("open"),
		Metadata: clearedSessionAffinityMetadata(),
	}
	if clearDetached {
		opts.Metadata[detachedProbeMetadataKey] = ""
	}
	if err := store.Update(wb.ID, opts); err != nil {
		log.Printf("releaseOrphanedPoolAssignments: releasing orphaned pool assignment %s: %v", wb.ID, err)
		return false
	}
	verifyReleasedPoolAssignment(store, wb.ID, expectedAssignee)
	return true
}

// verifyReleasedPoolAssignment makes a lost release race observable: when a
// concurrent claim lands around the unconditional release write, the ordering
// that survives (claim after release) shows up here as a foreign assignee. A
// claim clobbered BY the release write (claim between recheck and write)
// reads back empty and stays undetectable without a store-level conditional
// write — that ordering is why ReleaseIfCurrent is preferred.
func verifyReleasedPoolAssignment(store beads.Store, id, expectedAssignee string) {
	got, err := store.Get(id)
	if err != nil {
		log.Printf("releaseOrphanedPoolAssignments: verify-after read failed for %s: %v", id, err)
		return
	}
	observed := strings.TrimSpace(got.Assignee)
	if observed == "" || observed == expectedAssignee {
		return
	}
	log.Printf("releaseOrphanedPoolAssignments: RELEASE RACE on %s: observed assignee %q immediately after releasing %q — a concurrent claim raced the orphan release", id, observed, expectedAssignee)
}

func liveOpenSessionAssignmentExists(store beads.Store, assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if store == nil || assignee == "" {
		return false
	}
	if liveSessionBeadExistsByIdentity(store, assignee) {
		return true
	}
	// NOTE: this call site intentionally keeps a label-only query — not
	// the Type+Label union from session.ListAllSessionBeads. The
	// orphan-release tests (TestReleaseOrphanedPoolAssignments_*) set up
	// city session beads with Type=session but no gc:session label and
	// assert that rig work pointing at a session_name only reachable via
	// the typed bead IS released. Switching this query to the union
	// would surface those typed beads as "live" and cause the work to
	// be skipped instead of released, regressing
	// ReopensRigStoreMissingPoolAssignee and
	// ReleasesRigWorkAssignedToUnreachableOpenSession. The label-loss
	// bug this PR is fixing manifests in the snapshot/list/reconciler
	// paths; orphan release continues to treat the label as the
	// authoritative liveness signal.
	sessions, err := store.List(beads.ListQuery{
		Label: sessionBeadLabel,
		Live:  true,
	})
	if err != nil {
		log.Printf("releaseOrphanedPoolAssignments: live session validation failed for assignee %q: %v", assignee, err)
		return true
	}
	for _, sb := range sessions {
		if sb.Status == "closed" || !isSessionBead(sb) {
			continue
		}
		for _, id := range sessionBeadAssigneeIdentities(sb) {
			if assignee == id {
				return true
			}
		}
	}
	return false
}

func liveSessionBeadExistsByIdentity(store beads.Store, assignee string) bool {
	for _, id := range directSessionBeadIDCandidates(assignee) {
		sb, err := store.Get(id)
		if err != nil {
			continue
		}
		if sb.Status == "closed" || !isSessionBead(sb) {
			continue
		}
		for _, candidate := range sessionBeadAssigneeIdentities(sb) {
			if assignee == candidate {
				return true
			}
		}
	}
	return false
}

func directSessionBeadIDCandidates(assignee string) []string {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return nil
	}
	candidates := []string{assignee}
	if idx := strings.LastIndex(assignee, "-mc-"); idx >= 0 {
		candidates = append(candidates, assignee[idx+1:])
	}
	return candidates
}

// liveWorkAssignmentStillReleasable confirms the snapshot is not stale
// before clearing assignee. The expectedStatus must match the snapshot
// status the caller observed: if the bead has since transitioned (e.g. a
// concurrent claim moved open→in_progress, or another release moved
// in_progress→open) the snapshot's release decision is no longer safe.
// Open status is required for the issue #2793 path — graph.v2 step
// beads stuck on a dead session's long-form assignee are status=open,
// not in_progress.
func liveWorkAssignmentStillReleasable(store beads.Store, id, expectedStatus, assignee string) bool {
	id = strings.TrimSpace(id)
	expectedStatus = strings.TrimSpace(expectedStatus)
	if store == nil || id == "" || expectedStatus == "" {
		return false
	}
	work, err := store.List(beads.ListQuery{
		Status:   expectedStatus,
		Live:     true,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		log.Printf("releaseOrphanedPoolAssignments: live work validation failed for %q: %v", id, err)
		return false
	}
	for _, wb := range work {
		if wb.ID != id {
			continue
		}
		return strings.TrimSpace(wb.Assignee) == strings.TrimSpace(assignee)
	}
	return false
}

func assigneePreservesNamedSessionRoute(cfg *config.City, cityPath, template, assignee, workStoreRef string, storeRefAware bool) bool {
	if cfg == nil {
		return false
	}
	spec, ok := findNamedSessionSpec(cfg, cfg.EffectiveCityName(), assignee)
	if !ok {
		return false
	}
	if namedSessionBackingTemplate(spec) != template {
		return false
	}
	if !storeRefAware {
		return true
	}
	// City-scoped named sessions federate across every store (vp-kvp), exactly
	// as filterAssignedWorkBeadsForSessionWake already treats them. Without this
	// a live city-scoped named holder's rig-routed claim is released and a backup
	// worker is minted on the same bead — the named-route analog of the
	// pool-worker openSessionOwnsWork cross-store fix (#3453).
	if agentIsCrossStoreEligible(spec.Agent) {
		return true
	}
	return assignedWorkStoreRefForAgent(cityPath, cfg, spec.Agent) == workStoreRef
}

func stringPtr(s string) *string { return &s }
