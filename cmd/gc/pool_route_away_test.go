package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// routeAwayNow is the fixed clock every route-away test judges at.
var routeAwayNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// routeAwayCity is a city with two pool agents, alpha (the holder's agent) and
// beta (the agent the bead is handed off to).
func routeAwayCity() *config.City {
	return &config.City{Agents: []config.Agent{
		{Name: "alpha", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		{Name: "beta", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
	}}
}

// routeAwayHolder is the live pool session a-1 of agent alpha.
func routeAwayHolder() session.Info {
	return session.Info{ID: "a-1", Template: "alpha", SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "1"}
}

// seedRouteAwayWork creates bead X: claimed in_progress by holder, routed to
// beta, with meta merged over the route (a "" value deletes the key).
func seedRouteAwayWork(t *testing.T, store beads.Store, holder string, meta map[string]string) beads.Bead {
	t.Helper()
	metadata := map[string]string{beadmeta.RoutedToMetadataKey: "beta"}
	for k, v := range meta {
		if v == "" {
			delete(metadata, k)
			continue
		}
		metadata[k] = v
	}
	work, err := store.Create(beads.Bead{Title: "escalated ticket", Type: "task", Status: "open", Assignee: holder, Metadata: metadata})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	return work
}

// routedAtAgo is a gc.routed_at stamp d before routeAwayNow.
func routedAtAgo(d time.Duration) map[string]string {
	return map[string]string{beadmeta.RoutedAtMetadataKey: beadmeta.FormatRoutedAt(routeAwayNow.Add(-d))}
}

// routeAwayTick runs the production composition the controller tick runs:
// the wake filter, its protection keys, then the snapshot-gated sweep, at now.
func routeAwayTick(
	store beads.Store,
	sessionStore beads.Store,
	cfg *config.City,
	cityPath string,
	open []session.Info,
	work []beads.Bead,
	stores []beads.Store,
	refs []string,
	rigStores map[string]beads.Store,
	now time.Time,
) []releasedPoolAssignment {
	wake, wakeRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, store, open, work, refs)
	result := DesiredStateResult{AssignedWorkBeads: work, AssignedWorkStores: stores, AssignedWorkStoreRefs: refs}
	return releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(store, beads.SessionStore{Store: sessionStore}, cfg, cityPath, open, result, rigStores, protectedWakeWorkKeys(wake, wakeRefs), nil, nil, now)
}

// singleStoreRouteAwayTick is routeAwayTick on a single-store city whose
// census records every bead under the city ref.
func singleStoreRouteAwayTick(store beads.Store, cfg *config.City, open []session.Info, work beads.Bead) []releasedPoolAssignment {
	return routeAwayTick(store, store, cfg, "", open, []beads.Bead{work}, []beads.Store{store}, []string{""}, nil, routeAwayNow)
}

func requireReleasedRouteAway(t *testing.T, store beads.Store, released []releasedPoolAssignment, work beads.Bead) {
	t.Helper()
	if len(released) != 1 || released[0].ID != work.ID || !released[0].RoutedAway {
		t.Fatalf("released = %+v, want exactly [{ID:%s RoutedAway:true}]: the bead is routed away from its live holder %q", released, work.ID, work.Assignee)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("work = status %q assignee %q, want open and unassigned", got.Status, got.Assignee)
	}
}

func requireKeptByHolder(t *testing.T, store beads.Store, released []releasedPoolAssignment, work beads.Bead) {
	t.Helper()
	if len(released) != 0 {
		t.Fatalf("released = %+v, want none: the holder %q keeps its claim", released, work.Assignee)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != work.Assignee {
		t.Fatalf("work = status %q assignee %q, want in_progress held by %q", got.Status, got.Assignee, work.Assignee)
	}
}

// TestRouteAwayReleasedThroughProductionComposition is #7004: a live session
// a-1 of alpha hands bead X off to beta with gc sling, which leaves a-1 as the
// assignee. Past the settle window the tick's sweep releases X to beta's pool,
// and beta's scale check then counts it as demand. The protection map is the
// real one the wake filter builds: it holds X, because a-1 is open.
func TestRouteAwayReleasedThroughProductionComposition(t *testing.T) {
	store := beads.NewMemStore()
	cfg := routeAwayCity()
	work := seedRouteAwayWork(t, store, "a-1", routedAtAgo(2*routeAwaySettleWindow))

	released := singleStoreRouteAwayTick(store, cfg, []session.Info{routeAwayHolder()}, work)
	requireReleasedRouteAway(t, store, released, work)

	counts, _, _, errs := defaultScaleCheckCountsAndDemand(cfg, []defaultScaleCheckTarget{
		{template: "alpha", storeKey: "city", store: store},
		{template: "beta", storeKey: "city", store: store},
	})
	if len(errs) != 0 {
		t.Fatalf("scale check errors: %v", errs)
	}
	if counts["beta"] != 1 || counts["alpha"] != 0 {
		t.Fatalf("scale-check demand = %v, want beta=1 alpha=0 once the handoff is released", counts)
	}
}

// TestRouteAwaySettleWindowBoundary pins the lower bound of the settle window:
// a handoff stamped one second inside the window is still settling, so the
// holder keeps it; one stamped exactly a window ago has settled and is
// released to the target pool.
func TestRouteAwaySettleWindowBoundary(t *testing.T) {
	cfg := routeAwayCity()
	open := []session.Info{routeAwayHolder()}

	t.Run("one second inside the window is kept", func(t *testing.T) {
		store := beads.NewMemStore()
		work := seedRouteAwayWork(t, store, "a-1", routedAtAgo(routeAwaySettleWindow-time.Second))
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(store, cfg, open, work), work)
	})
	t.Run("exactly one window old is released", func(t *testing.T) {
		store := beads.NewMemStore()
		work := seedRouteAwayWork(t, store, "a-1", routedAtAgo(routeAwaySettleWindow))
		requireReleasedRouteAway(t, store, singleStoreRouteAwayTick(store, cfg, open, work), work)
	})
}

// TestRouteAwayUnsettlingStampsAreSettled pins that no gc.routed_at value can
// hold a routed-away claim from its target: an absent, blank or malformed
// stamp, or one more than a window in the future, counts as settled, and the
// claim is released.
func TestRouteAwayUnsettlingStampsAreSettled(t *testing.T) {
	cfg := routeAwayCity()
	open := []session.Info{routeAwayHolder()}
	for _, tc := range []struct {
		name string
		meta map[string]string
	}{
		{name: "absent", meta: nil},
		{name: "blank", meta: map[string]string{beadmeta.RoutedAtMetadataKey: "   "}},
		{name: "malformed", meta: map[string]string{beadmeta.RoutedAtMetadataKey: "yesterday"}},
		{name: "more than a window in the future", meta: routedAtAgo(-(routeAwaySettleWindow + time.Second))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			work := seedRouteAwayWork(t, store, "a-1", tc.meta)
			requireReleasedRouteAway(t, store, singleStoreRouteAwayTick(store, cfg, open, work), work)
		})
	}
}

// TestRouteAwayKeepsClaimsTheSweepCannotJudge pins the settled handoffs the
// sweep still keeps for their live holder, each because something it cannot
// see past may still be serving the bead: a session of the target agent, an
// owner of no known agent, an identity this city cannot observe, a control
// bead the dispatcher owns, a bead waiting on a person, or a graph-resident
// session in the bead's own owner store.
func TestRouteAwayKeepsClaimsTheSweepCannotJudge(t *testing.T) {
	cfg := routeAwayCity()
	settled := routedAtAgo(2 * routeAwaySettleWindow)

	t.Run("an open session of the target agent shares the identity", func(t *testing.T) {
		store := beads.NewMemStore()
		work := seedRouteAwayWork(t, store, "a-1", settled)
		sibling := session.Info{ID: "b-1", Template: "beta", Alias: "a-1", SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "1"}
		open := []session.Info{routeAwayHolder(), sibling}
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(store, cfg, open, work), work)
	})
	t.Run("the owner resolves to no agent", func(t *testing.T) {
		store := beads.NewMemStore()
		work := seedRouteAwayWork(t, store, "a-1", settled)
		holder := routeAwayHolder()
		holder.Template = "retired"
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(store, cfg, []session.Info{holder}, work), work)
	})
	t.Run("the identity is unobservable to this city", func(t *testing.T) {
		store := beads.NewMemStore()
		work := seedRouteAwayWork(t, store, "far/a-1", settled)
		holder := routeAwayHolder()
		holder.Alias = "far/a-1"
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(store, cfg, []session.Info{holder}, work), work)
	})
	t.Run("the bead is a control bead", func(t *testing.T) {
		store := beads.NewMemStore()
		meta := routedAtAgo(2 * routeAwaySettleWindow)
		meta[beadmeta.KindMetadataKey] = beadmeta.KindFanout
		work := seedRouteAwayWork(t, store, "a-1", meta)
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(store, cfg, []session.Info{routeAwayHolder()}, work), work)
	})
	t.Run("the bead waits on a person", func(t *testing.T) {
		store := beads.NewMemStore()
		work := seedRouteAwayWork(t, store, "a-1", settled)
		if err := store.Update(work.ID, beads.UpdateOpts{Labels: []string{beadmeta.HumanLabel}}); err != nil {
			t.Fatalf("label work human: %v", err)
		}
		work, err := store.Get(work.ID)
		if err != nil {
			t.Fatalf("reload work: %v", err)
		}
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(store, cfg, []session.Info{routeAwayHolder()}, work), work)
	})
	t.Run("a graph-resident session in a separate owner store answers to the assignee", func(t *testing.T) {
		sessionStore := beads.NewMemStore()
		ownerStore := beads.NewMemStore()
		work := seedRouteAwayWork(t, ownerStore, "a-1", settled)
		if _, err := ownerStore.Create(beads.Bead{
			Title:    "graph run session",
			Type:     sessionBeadType,
			Status:   "open",
			Labels:   []string{sessionBeadLabel},
			Metadata: map[string]string{"session_name": "a-1"},
		}); err != nil {
			t.Fatalf("create graph-resident session: %v", err)
		}
		released := routeAwayTick(sessionStore, sessionStore, cfg, "", []session.Info{routeAwayHolder()}, []beads.Bead{work}, []beads.Store{ownerStore}, []string{""}, nil, routeAwayNow)
		requireKeptByHolder(t, ownerStore, released, work)
	})
}
