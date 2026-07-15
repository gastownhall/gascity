package main

import (
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// reconcileTick owns the reconciler's coherent typed snapshot (infoByID) for a
// single tick and is the ONE front door for folding a mutation onto it.
//
// In the row-based tree every forward-pass metadata write is mirrored to two
// representations kept coherent by hand: the store (via a sessFront front-door
// write inside the write helper — healStateWithRollbackInfo, checkStability,
// checkChurn, attemptRollbackPendingCreate, …) and this typed snapshot. The
// store write stays where the helper performs it; this type owns the second
// write: the infoByID fold. Historically that fold was an open-coded
// `infoByID[id] = infoByID[id].ApplyPatch(patch)` (or a direct assignment of a
// helper's returned Info) repeated at ~40 sites, and a forgotten fold was a
// silent, compile-clean coherence bug in the cross-session min-floor / awake /
// drain scans that read the snapshot. Routing every fold through
// apply/applyResult/markClosed/set makes that bug class unrepresentable: there
// is one fold path, guarded by TestReconcileTickFoldFrontDoor (which forbids a
// bare `infoByID[...] =` outside this file) and by the property tests in
// reconcile_tick_test.go.
//
// The struct holds the same map instance the reconciler reads from, so callers
// keep reading through a plain `infoByID` alias and passing it to scan helpers;
// only the write path is funneled here.
//
// (The 0-Get atomic write-returns-Info sites — `infoByID[id], _ =
// sessFront.ApplyPatchInfo(infoByID[id], patch)` — persist the store write and
// return the folded Info in one call, so the fold is inherent to the call and
// cannot be forgotten; they are not part of the manual-fold bug class the guard
// polices and stay as-is.)
type reconcileTick struct {
	// infoByID is the coherent typed snapshot of the tick's working set, keyed by
	// session ID. Built once from the tick's ordered, already-projected Info feed.
	infoByID map[string]sessionpkg.Info
	// orderedIDs carries the tick's topo order as plain session IDs. Order is
	// load-bearing: ComputeAwakeSet resolves the non-unique SessionName
	// last-write-wins, so order-sensitive rebuilds walk this instead of ranging
	// the (unordered) map.
	orderedIDs []string
}

// newReconcileTick builds the tick snapshot from the tick's ordered, already-
// projected working set. Each entry is the row's Info verbatim — there is NO
// codec call here (the rows were projected once at the store edge; the typed
// migration's §2.3 fold-then-build invariant), so the tree's "rows carry Info"
// contract and the codec census guard are both preserved. The forward pass
// mutates only the current iteration's session, so no entry goes stale before it
// is visited.
func newReconcileTick(ordered []sessionpkg.Info) *reconcileTick {
	t := &reconcileTick{
		infoByID:   make(map[string]sessionpkg.Info, len(ordered)),
		orderedIDs: make([]string, len(ordered)),
	}
	for i := range ordered {
		t.orderedIDs[i] = ordered[i].ID
		t.infoByID[ordered[i].ID] = ordered[i]
	}
	return t
}

// apply folds a metadata patch onto the snapshot entry for id and returns the
// updated Info. Equivalent to the former `infoByID[id] = infoByID[id].ApplyPatch
// (patch)`; the store write is performed by the caller's write helper before
// this fold.
func (t *reconcileTick) apply(id string, patch sessionpkg.MetadataPatch) sessionpkg.Info {
	next := t.infoByID[id].ApplyPatch(patch)
	t.infoByID[id] = next
	return next
}

// applyResult folds a drainAckFinalizeResult onto the snapshot entry for id and
// returns the updated Info. Equivalent to the former
// `infoByID[id] = result.applyTo(infoByID[id])`.
func (t *reconcileTick) applyResult(id string, r drainAckFinalizeResult) sessionpkg.Info {
	next := r.applyTo(t.infoByID[id])
	t.infoByID[id] = next
	return next
}

// markClosed records an in-memory close on the snapshot entry for id (Closed
// =true, State=""). Equivalent to the former
// `infoByID[id] = infoByID[id].MarkClosed()`; the store close was already
// stamped by the caller's close helper.
func (t *reconcileTick) markClosed(id string) sessionpkg.Info {
	next := t.infoByID[id].MarkClosed()
	t.infoByID[id] = next
	return next
}

// set records a pre-computed Info onto the snapshot entry for id and returns it.
// It is the front door for the tree's write-returns-Info fold shapes, where a
// write helper (checkRateLimitStability, checkStability, checkChurn,
// clearWakeFailures, clearChurn, markProviderTerminalError,
// markDrainAckStopPending, persistSleepPolicyMetadataInfo, …) already persisted
// the store write and returned the coherent post-write Info as a plain value.
// Equivalent to the former `infoByID[id] = <computedInfo>`.
func (t *reconcileTick) set(id string, info sessionpkg.Info) sessionpkg.Info {
	t.infoByID[id] = info
	return info
}

// applyStore is the store-write + snapshot-fold mutator whose fold REFLECTS
// PERSISTENCE: it persists patch through the session front door (ApplyPatchInfo)
// and folds the returned Info into the tick snapshot in one call. ApplyPatchInfo
// folds the patch onto Info only on a SUCCESSFUL write; on a store-write failure
// it returns the INPUT Info UNCHANGED (internal/session/store.go), so the
// discarded error here leaves the snapshot entry exactly reflecting what the
// store holds. Use applyStore where a stale snapshot value on write failure is
// correct — the value is not read again this tick, or must not advance past a
// write the store rejected. Routing tuples through this mutator lets the fold
// front-door guard forbid the bare tuple form outright.
//
// Contrast applyOptimistic, whose local fold must SURVIVE a failed write (the
// kill/sleep sites).
func (t *reconcileTick) applyStore(id string, front *sessionpkg.Store, patch sessionpkg.MetadataPatch) sessionpkg.Info {
	next, _ := front.ApplyPatchInfo(t.infoByID[id], patch)
	t.infoByID[id] = next
	return next
}

// applyOptimistic is the kill/sleep-site mutator whose local fold SURVIVES a
// failed write. It attempts the durable write (front.ApplyPatch; the error is
// intentionally discarded, matching the pre-migration `_ = ApplyPatch(...)` at
// these sites) and then ALWAYS folds patch onto the snapshot entry for id.
//
// This is required at sites that killed a session's runtime and then folded
// SleepPatch (or a marker clear) UNCONDITIONALLY on origin/main: the kill already
// happened, so the snapshot MUST record the sleep even if its persistence failed.
// If the fold were dropped on write failure (applyStore's behavior), the killed
// session would still look awake to the same-tick awake scan and be respawned in
// the same tick — or its stale last_woke_at would skew wake-budget fairness and
// steal a peer's slot. applyStore is wrong here for exactly that reason: it
// reflects the (failed) persistence rather than the completed kill.
func (t *reconcileTick) applyOptimistic(id string, front *sessionpkg.Store, patch sessionpkg.MetadataPatch) {
	// Error intentionally discarded (matches origin/main's `_ = ApplyPatch` at these
	// sites): the local fold below must survive a failed sleep write.
	_ = front.ApplyPatch(id, patch)
	t.infoByID[id] = t.infoByID[id].ApplyPatch(patch)
}
