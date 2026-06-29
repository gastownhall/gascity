package main

import (
	sessionpkg "github.com/gastownhall/gascity/internal/session"

	"github.com/gastownhall/gascity/internal/beads"
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
// beads. It is the typed form of the raw
// List{Assignee,Status,Live,TierMode} probe the reconciler ran directly.
// status selects the bead status ("open" / "in_progress"); live mirrors the
// raw ListQuery.Live flag. Session beads (and repairable session beads) are
// filtered out, matching the raw probes.
func (w workAssignment) OpenAssignedTo(assignee, status string, tierMode beads.TierMode, live bool) ([]beads.Bead, error) {
	store := w.unwrapped()
	if store == nil {
		return nil, nil
	}
	items, err := store.List(beads.ListQuery{Assignee: assignee, Status: status, Live: live, TierMode: tierMode})
	if err != nil {
		return nil, err
	}
	return items, nil
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
