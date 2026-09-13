package main

import (
	"errors"
	"fmt"
	"log"
	"strings"

	sessionpkg "github.com/gastownhall/gascity/internal/session"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// workAssignment is the typed boundary façade the SESSION reconciler uses to
// read WORK beads keyed by a session identity. The reconciler's stranded/awake/
// drain probes are WORK queries (List{Assignee,Status,...} / ReadyLive) that
// happen to be keyed by a session's assignment identifiers; left raw they reach
// the WORK store *through* the session store. Routing them through this façade
// makes the WORK store the explicit source.
//
// The façade carries a beads.WorkStore (the work class), but every optional
// capability (CachedList, ReadyLive's Backing()) is asserted on the embedded
// .Store, never the wrapper — wrapping a Store in WorkStore does NOT promote
// optional capabilities, and asserting on the wrapper would silently drop the
// cache/live fast-paths (the typed-nil trap, OBJECT-MODEL-FRONT-DOOR-DESIGN.md
// invariant 3/4). On a single-store city the work store IS the same object the
// session arm uses, so the bead reads emitted here are byte-identical to the
// raw ops they replace.
type workAssignment struct {
	store beads.WorkStore
}

// workAssignmentForStore wraps a resolved WORK store as the typed assignment
// façade. The caller passes the work store value (cityWorkStore / a rig work
// store); the underlying store is unchanged, so reads stay byte-identical.
func workAssignmentForStore(store beads.WorkStore) workAssignment {
	return workAssignment{store: store}
}

// unwrapped returns the underlying generic Store for optional-capability
// assertions (CachedList, ReadyLive Backing()). Asserting on the WorkStore
// wrapper instead would fail because the wrapper only embeds the Store
// interface and does not promote optional capabilities.
func (w workAssignment) unwrapped() beads.Store {
	return w.store.Store
}

// OpenAssignedTo returns the open or in-progress WORK beads in this store
// assigned to the given identity for the given tier mode, excluding session
// beads and mail message beads. It is the typed form of the raw
// List{Assignee,Status,Live,TierMode} probe the reconciler ran directly.
// status selects the bead status ("open" / "in_progress"); live mirrors the
// raw ListQuery.Live flag. Session beads (and repairable session beads) are
// filtered out, matching the raw probes; mail message beads are filtered out
// here too (ra-59207) — a mail wisp has no claim/routing semantics, so every
// caller that reassigns or releases what this returns must never see one.
func (w workAssignment) OpenAssignedTo(assignee, status string, tierMode beads.TierMode, live bool) ([]beads.Bead, error) {
	store := w.unwrapped()
	if store == nil {
		return nil, nil
	}
	items, err := store.List(beads.ListQuery{Assignee: assignee, Status: status, Live: live, TierMode: tierMode})
	if err != nil {
		return nil, err
	}
	return excludeMailMessageBeads(items), nil
}

// CachedOpenAssignedWisps returns cached open-assigned wisp-tier WORK beads when
// the underlying store exposes the CachedList fast-path, plus whether the cache
// answered. It is the typed form of the positive-only cache probe in
// sessionHasOpenAssignedWispWork; the assertion is on the embedded .Store so the
// fast-path is preserved.
func (w workAssignment) CachedOpenAssignedWisps(assignee, status string) ([]beads.Bead, bool) {
	store := w.unwrapped()
	if store == nil {
		return nil, false
	}
	query := beads.ListQuery{Assignee: assignee, Status: status, TierMode: beads.TierWisps}
	cache, ok := store.(interface {
		CachedList(beads.ListQuery) ([]beads.Bead, bool)
	})
	if !ok {
		return nil, false
	}
	return cache.CachedList(query)
}

// ReadyAssignedTo returns the ready (unblocked, actionable) WORK beads assigned
// to the given identity for the given tier mode. It is the typed form of the raw
// beads.ReadyLive(ReadyQuery{Assignee,TierMode}) probe. ReadyLive is called on
// the embedded .Store so its Backing() live-read fast-path is preserved.
func (w workAssignment) ReadyAssignedTo(assignee string, tierMode beads.TierMode) ([]beads.Bead, error) {
	store := w.unwrapped()
	if store == nil {
		return nil, nil
	}
	return beads.ReadyLive(store, beads.ReadyQuery{Assignee: assignee, TierMode: tierMode})
}

// HasNonSessionWork reports whether any bead in items is non-session WORK
// (skipping session beads and repairable session beads). Shared filter for the
// boolean readiness/open probes.
func (w workAssignment) HasNonSessionWork(items []beads.Bead) bool {
	for _, item := range items {
		if sessionpkg.IsSessionBeadOrRepairable(item) {
			continue
		}
		return true
	}
	return false
}

// OpenAssignedToBasic returns the WORK beads assigned to the given identity with
// the given status, using the no-flags List{Assignee,Status} query (no Live, no
// TierMode). It is the typed form of the raw probe in
// releaseWorkFromClosedSessionBead, kept distinct from OpenAssignedTo because the
// close-release path deliberately runs the unflagged query — making it byte-
// identical to OpenAssignedTo's flagged query would change the emitted bead op.
// Like OpenAssignedTo, mail message beads are excluded (ra-59207): they are not
// WORK and have no claim/routing semantics for the release path to act on.
func (w workAssignment) OpenAssignedToBasic(assignee, status string) ([]beads.Bead, error) {
	store := w.unwrapped()
	if store == nil {
		return nil, nil
	}
	items, err := store.List(beads.ListQuery{Assignee: assignee, Status: status})
	if err != nil {
		return nil, err
	}
	return excludeMailMessageBeads(items), nil
}

// excludeMailMessageBeads filters mail message beads (beadmail.IsMessageBead)
// out of a WORK query result. A mail wisp is a delivery route, not a claimable
// unit of work — it can be neither released nor reassigned — so every WORK
// enumeration in this file (and every caller downstream, all of which treat
// their results as releasable/reassignable WORK) must exclude it at the source
// rather than repeat the check at each call site (ra-59207: the session-close
// WORK-RELEASE sweep clearing a mail bead's assignee silently destroyed its
// only route to an inbox).
func excludeMailMessageBeads(items []beads.Bead) []beads.Bead {
	if len(items) == 0 {
		return items
	}
	out := items[:0:0]
	for _, item := range items {
		if beadmail.IsMessageBead(item) {
			continue
		}
		out = append(out, item)
	}
	return out
}

// ReleaseWorkBead detaches one WORK bead from its (closed/retired) session: it
// clears the assignee (empty-string clear), clears stale session-affinity
// metadata, and resets an in_progress bead to open so a fresh worker can
// re-claim it via the routed queue. When runTargetFallback is non-empty AND the
// bead carries neither run_target nor routed_to, the fallback route is stamped so
// the reopened work stays reachable by the controller demand query. Pass
// runTargetFallback="" for the close-release path, which never stamps a fallback.
//
// The release is CONDITIONAL on the caller's snapshot still being true. Callers
// compute their release set from a List taken earlier in the tick, so by the time
// the write lands a fresh worker may already hold the bead; the raw ops this
// replaced wrote unconditionally and destroyed that claim (dr-huhn). Two tiers
// narrow the window: the store's atomic ReleaseIfCurrent when it has one,
// otherwise a live re-read immediately before an unconditional write.
//
// Only the tier-2 write stays byte-identical to the raw release ops in
// releaseWorkFromClosedSessionBead and unclaimWorkAssignedToRetiredSessionBead
// (pinned by the recording-fake write tests). Tier 1 differs by design:
// ReleaseIfCurrent swaps status/assignee itself, so when that tier applies the
// metadata clear rides a second write — which is also why it does not always
// apply (see singleWriteRequired below).
func (w workAssignment) ReleaseWorkBead(item beads.Bead, runTargetFallback string) error {
	store := w.unwrapped()
	if store == nil {
		return nil
	}
	metadata := clearedSessionAffinityMetadata()
	stampFallbackRoute := runTargetFallback != "" &&
		strings.TrimSpace(item.Metadata[beadmeta.RunTargetMetadataKey]) == "" &&
		strings.TrimSpace(item.Metadata[beadmeta.RoutedToMetadataKey]) == ""
	if stampFallbackRoute {
		metadata[beadmeta.RunTargetMetadataKey] = runTargetFallback
	}
	// Tier 1 clears status/assignee atomically but leaves the metadata to a second
	// write, so it is usable only when that second write is not routing-
	// consequential. Both conditions below make it so, and in the gap between the
	// two writes the bead is open and unassigned:
	//
	//   - a fallback route to stamp: the bead carries neither run_target nor
	//     routed_to, leaving it invisible to the controller demand query and to
	//     the orphan sweep (which scans only assigned work). If the second write
	//     then fails, nothing revisits the bead and it is stranded silently.
	//   - an active continuation group: gc.continuation_group is still set, and a
	//     concurrent `gc hook --claim` reads it to vacuum this bead and its
	//     {root, group} siblings onto the claiming session. This is the same
	//     hazard releaseOrphanedPoolAssignment bypasses tier 1 for, and it is read
	//     from the same snapshot for the same reason: a group stamped after the
	//     caller's List but before this write still takes tier 1. Narrowing that
	//     would need a live metadata re-read on every release, and it is the
	//     assignee — not the group — that tier 1 verifies atomically.
	//
	// Either way the release must land as ONE write, which is what the tier-2
	// recheck does: status, assignee and metadata in a single Update, so the bead
	// is never claimable in a half-released state.
	singleWriteRequired := stampFallbackRoute || beadHasActiveContinuationGroup(item)
	// Tier 1: the store's atomic conditional release. A release computed from a
	// snapshot must never write over an assignee that changed after that
	// snapshot was taken.
	if !singleWriteRequired {
		released, handled, err := releaseWorkAssignmentIfCurrent(store, item)
		if err != nil {
			// Surface the failure. Callers gate on it: a swallowed error here
			// reads as a successful release, and repairStrandedPoolWorkerBead
			// would then close the session bead while its work is still
			// assigned to it, with nothing left watching the work.
			return err
		}
		if handled {
			if !released {
				return nil
			}
			// If this metadata clear fails the error propagates, but no retry
			// follows: the bead is already unassigned, so the next tick's
			// OpenAssignedTo sweep will not see it again. A bead whose SNAPSHOT
			// carried a continuation group took the single-write path above and
			// cannot be here, so what is normally left behind is only the
			// advisory SessionAffinityMetadataKey. The exception is the same
			// snapshot-bound gap singleWriteRequired documents: a group stamped
			// after the caller's List still reaches this second write, and if
			// this write fails that group outlives the release.
			if err := store.Update(item.ID, beads.UpdateOpts{Metadata: metadata}); err != nil {
				return fmt.Errorf("clearing session affinity on %q after conditional release: %w", item.ID, err)
			}
			return nil
		}
	}
	// Tier 2: no usable conditional verb (or a route to stamp), so re-verify the
	// snapshot with a live read immediately before the unconditional write. This
	// shrinks the window rather than closing it; the residual recheck->write gap
	// needs a store-level conditional write, which is exactly what tier 1 is.
	stillCurrent, err := liveWorkAssignmentAssigneeMatches(store, item.ID, item.Status, item.Assignee)
	if err != nil {
		return err
	}
	if !stillCurrent {
		log.Printf("ReleaseWorkBead: skipping release for %s: assignment changed between snapshot and release write", item.ID)
		return nil
	}
	empty := ""
	update := beads.UpdateOpts{
		Assignee: &empty,
		Metadata: metadata,
	}
	if item.Status == "in_progress" {
		open := "open"
		update.Status = &open
	}
	return store.Update(item.ID, update)
}

// releaseWorkAssignmentIfCurrent attempts the store's atomic conditional
// release. handled reports whether the conditional path owns the outcome (so
// the caller must not fall through to an unconditional write); released reports
// whether the assignment was actually cleared.
//
// It mirrors releasePoolAssignmentIfCurrent: the verb's contract covers
// in_progress assignments with a known assignee, and the capability is asserted
// on the unwrapped Store because a class wrapper does not promote optional
// capabilities.
func releaseWorkAssignmentIfCurrent(store beads.Store, item beads.Bead) (released, handled bool, err error) {
	expectedAssignee := strings.TrimSpace(item.Assignee)
	if item.Status != "in_progress" || expectedAssignee == "" {
		return false, false, nil
	}
	releaser, ok := store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, false, nil
	}
	released, err = releaser.ReleaseIfCurrent(item.ID, expectedAssignee)
	if err != nil {
		if errors.Is(err, beads.ErrConditionalReleaseUnsupported) {
			return false, false, nil
		}
		// The store supports conditional release but this attempt failed. Do NOT
		// downgrade to an unconditional write that could clobber a concurrent
		// re-claim, and do NOT report success: the assignment is still live and
		// the caller must count this as a failed unassign.
		return false, true, fmt.Errorf("conditional release of %q: %w", item.ID, err)
	}
	if !released {
		// Not a failure: the bead now belongs to someone else, so there is
		// nothing for this release to detach and the caller may proceed.
		log.Printf("ReleaseWorkBead: skipping release for %s: assignment changed since snapshot (re-claimed or transitioned)", item.ID)
	}
	return released, true, nil
}

// liveWorkAssignmentAssigneeMatches reports whether a WORK bead still carries the
// (status, assignee) pair a caller's earlier snapshot recorded. It is the single
// implementation of the pre-write staleness check for both the work path here and
// the pool path in pool_session_name.go, because the subtleties below are easy to
// get wrong once and impossible to keep in sync twice.
//
// It uses a LIVE list query, not Get, and that choice is load-bearing:
// CachingStore.Get serves a clone straight from the in-memory cache for a bead
// that is tracked and not dirty (internal/beads/caching_store_reads.go). A
// cached read here would re-verify the caller's stale snapshot against an
// equally stale cache and confirm it, which reproduces the exact clobber this
// guard exists to prevent.
//
// expectedStatus must be the status the caller observed: if the bead has since
// transitioned (a concurrent claim moved open→in_progress, or another release
// moved in_progress→open) the snapshot's decision is no longer safe. A bead
// absent from the live result no longer holds that status, so it is not current
// and not an error.
//
// A read failure returns the error rather than a verdict. Writing on an
// unverified snapshot can destroy a live worker's claim, and reporting the write
// as done when the read failed would let a caller close a session bead whose work
// is still assigned to it.
func liveWorkAssignmentAssigneeMatches(store beads.Store, id, expectedStatus, expectedAssignee string) (bool, error) {
	id = strings.TrimSpace(id)
	expectedStatus = strings.TrimSpace(expectedStatus)
	if store == nil || id == "" || expectedStatus == "" {
		return false, nil
	}
	work, err := store.List(beads.ListQuery{
		Status:   expectedStatus,
		Live:     true,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return false, fmt.Errorf("live work-assignment verification of %q: %w", id, err)
	}
	for _, wb := range work {
		if wb.ID != id {
			continue
		}
		return strings.TrimSpace(wb.Assignee) == strings.TrimSpace(expectedAssignee), nil
	}
	return false, nil
}

// ReassignWorkBead re-homes one WORK bead onto a new session identity, emitting
// the exact Update{Assignee:&new} the raw reassign op in
// reassignWorkAssignedToRetiredSessionBead emitted. It deliberately touches
// neither Status nor Metadata.
//
// The write is CONDITIONAL on item still being assigned to the session the
// caller's snapshot saw. Both callers walk an OpenAssignedTo list taken earlier
// in the tick (session_beads.go), so this carries the same lost-update hazard as
// the release path (dr-huhn): a fresh worker can claim the bead between the list
// and the write, and an unconditional reassign then stamps the retired session's
// successor over that live claim. There is no conditional-reassign verb to reach
// for — ReleaseIfCurrent only clears — so the guard is the live re-read.
func (w workAssignment) ReassignWorkBead(item beads.Bead, newSessionID string) error {
	store := w.unwrapped()
	if store == nil {
		return nil
	}
	stillCurrent, err := liveWorkAssignmentAssigneeMatches(store, item.ID, item.Status, item.Assignee)
	if err != nil {
		return err
	}
	if !stillCurrent {
		log.Printf("ReassignWorkBead: skipping reassign for %s: assignment changed between snapshot and reassign write", item.ID)
		return nil
	}
	return store.Update(item.ID, beads.UpdateOpts{Assignee: &newSessionID})
}

// ClearDetachedProbe clears the detached-probe metadata contract on a WORK bead,
// emitting SetMetadata(id, gc.detached, "") — the empty-string clear semantics
// the raw clearDetachedProbeMetadata op used. Best-effort: a nil store or empty
// id is a no-op, and a write error is returned for the caller to log.
func (w workAssignment) ClearDetachedProbe(beadID string) error {
	store := w.unwrapped()
	if store == nil || beadID == "" {
		return nil
	}
	return store.SetMetadata(beadID, beadmeta.DetachedMetadataKey, "")
}
