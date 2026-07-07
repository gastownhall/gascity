package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// reconcileTick owns the reconciler's coherent typed snapshot (infoByID) for a
// single tick and is the ONE front door for folding a mutation onto it.
//
// Every forward-pass metadata write in the reconciler produces a patch that is
// mirrored to three representations kept coherent by hand: the store (via
// sessFront.ApplyPatch), the raw beads.Bead (session.Metadata[k]=v), and this
// typed snapshot. The store write and raw-bead mirror stay where the write
// helpers perform them (healStateWithRollback, checkRateLimitStability,
// rollbackPendingCreate, …) — this type does not duplicate them. What it owns
// is the third write: the infoByID fold. Historically that fold was an open-
// coded `infoByID[id] = infoByID[id].ApplyPatch(patch)` repeated at ~30 sites,
// and a forgotten fold was a silent, compile-clean coherence bug in the
// cross-session min-floor / awake / drain scans that read the snapshot. Routing
// every fold through apply/applyResult/markClosed makes that bug class
// unrepresentable: there is one fold path, guarded by TestReconcileTickFold
// FrontDoor (which forbids a bare `infoByID[...] =` outside this file) and by
// the property test TestReconcileTickApplyMatchesRawFold.
//
// The struct holds the same map instances the reconciler reads from, so callers
// may keep reading through a plain `infoByID` alias and passing it to scan
// helpers; only the write path is funneled here.
type reconcileTick struct {
	// infoByID is the coherent typed snapshot of the tick's working set, keyed
	// by session ID. Built once from the post-Phase-0.5 `ordered` beads.
	infoByID map[string]sessionpkg.Info
	// orderedIDs carries the tick's topo order as plain session IDs. Order is
	// load-bearing: ComputeAwakeSet resolves the non-unique SessionName
	// last-write-wins, so order-sensitive rebuilds walk this instead of ranging
	// the (unordered) map.
	orderedIDs []string
}

// newReconcileTick builds the tick snapshot from the tick's ordered working
// set. Each entry is byte-identical to a fresh projection of that session's
// bead at loop entry; the forward pass mutates only the current iteration's
// session, so no entry goes stale before it is visited.
func newReconcileTick(ordered []beads.Bead) *reconcileTick {
	t := &reconcileTick{
		infoByID:   make(map[string]sessionpkg.Info, len(ordered)),
		orderedIDs: make([]string, len(ordered)),
	}
	for i := range ordered {
		t.orderedIDs[i] = ordered[i].ID
		t.infoByID[ordered[i].ID] = sessionpkg.InfoFromPersistedBead(ordered[i])
	}
	return t
}

// apply folds a metadata patch onto the snapshot entry for id and returns the
// updated Info. Equivalent to the former `infoByID[id] = infoByID[id].ApplyPatch
// (patch)`; the store write and raw-bead mirror are performed by the caller's
// write helper before this fold.
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
