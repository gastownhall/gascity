package main

import (
	"errors"
	"strings"
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

// singleStoreRouteAwayTick is routeAwayTick on a single-store city: every
// open session's bead lives in the work store, and the census records every
// bead under the city ref. The holder's own session bead is what the
// owner-store liveness probe would find, so a route-away release here also
// pins that the sweep skips that probe where the owner store is the sessions
// store.
func singleStoreRouteAwayTick(t *testing.T, store beads.Store, cfg *config.City, open []session.Info, work beads.Bead) []releasedPoolAssignment {
	t.Helper()
	for _, info := range open {
		seedOpenSessionBead(t, store, info)
	}
	return routeAwayTick(store, store, cfg, "", open, []beads.Bead{work}, []beads.Store{store}, []string{""}, nil, routeAwayNow)
}

// seedOpenSessionBead puts info's open session bead in store, answering to
// info's ID as its session name.
func seedOpenSessionBead(t *testing.T, store beads.Store, info session.Info) {
	t.Helper()
	if _, err := store.Create(beads.Bead{
		Title:    "session " + info.ID,
		Type:     sessionBeadType,
		Status:   "open",
		Labels:   []string{sessionBeadLabel},
		Metadata: map[string]string{"session_name": info.ID, "template": info.Template},
	}); err != nil {
		t.Fatalf("create session bead for %s: %v", info.ID, err)
	}
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

	released := singleStoreRouteAwayTick(t, store, cfg, []session.Info{routeAwayHolder()}, work)
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
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(t, store, cfg, open, work), work)
	})
	t.Run("exactly one window old is released", func(t *testing.T) {
		store := beads.NewMemStore()
		work := seedRouteAwayWork(t, store, "a-1", routedAtAgo(routeAwaySettleWindow))
		requireReleasedRouteAway(t, store, singleStoreRouteAwayTick(t, store, cfg, open, work), work)
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
			requireReleasedRouteAway(t, store, singleStoreRouteAwayTick(t, store, cfg, open, work), work)
		})
	}
}

// TestRouteAwayKeepsClaimsTheSweepCannotJudge pins the settled handoffs the
// sweep still keeps for their live holder, each because something it cannot
// see past may still be serving the bead: a session of the target agent, an
// owner of no known agent, an identity this city cannot observe, a control
// bead the dispatcher owns, a bead waiting on a person, a graph-resident
// session in the bead's own owner store, or a city-scoped owner serving its
// own role's route in a rig scope.
func TestRouteAwayKeepsClaimsTheSweepCannotJudge(t *testing.T) {
	cfg := routeAwayCity()
	settled := routedAtAgo(2 * routeAwaySettleWindow)

	t.Run("an open session of the target agent shares the identity", func(t *testing.T) {
		store := beads.NewMemStore()
		work := seedRouteAwayWork(t, store, "a-1", settled)
		sibling := session.Info{ID: "b-1", Template: "beta", Alias: "a-1", SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "1"}
		open := []session.Info{routeAwayHolder(), sibling}
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(t, store, cfg, open, work), work)
	})
	t.Run("the owner resolves to no agent", func(t *testing.T) {
		store := beads.NewMemStore()
		work := seedRouteAwayWork(t, store, "a-1", settled)
		holder := routeAwayHolder()
		holder.Template = "retired"
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(t, store, cfg, []session.Info{holder}, work), work)
	})
	t.Run("the identity is unobservable to this city", func(t *testing.T) {
		store := beads.NewMemStore()
		work := seedRouteAwayWork(t, store, "far/a-1", settled)
		holder := routeAwayHolder()
		holder.Alias = "far/a-1"
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(t, store, cfg, []session.Info{holder}, work), work)
	})
	t.Run("the bead is a control bead", func(t *testing.T) {
		store := beads.NewMemStore()
		meta := routedAtAgo(2 * routeAwaySettleWindow)
		meta[beadmeta.KindMetadataKey] = beadmeta.KindFanout
		work := seedRouteAwayWork(t, store, "a-1", meta)
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(t, store, cfg, []session.Info{routeAwayHolder()}, work), work)
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
		requireKeptByHolder(t, store, singleStoreRouteAwayTick(t, store, cfg, []session.Info{routeAwayHolder()}, work), work)
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
	t.Run("a cross-store-eligible owner keeps a claim routed to its own role in another scope", func(t *testing.T) {
		store := beads.NewMemStore()
		scoped := &config.City{
			Rigs: []config.Rig{{Name: "riga", Path: t.TempDir()}},
			Agents: []config.Agent{
				{Name: "watcher", Scope: "city", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
				{Name: "watcher", Dir: "riga", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			},
		}
		meta := routedAtAgo(2 * routeAwaySettleWindow)
		meta[beadmeta.RoutedToMetadataKey] = "riga/watcher"
		work := seedRouteAwayWork(t, store, "w-1", meta)
		holder := session.Info{ID: "w-1", Template: "watcher", SessionOrigin: "ephemeral", PoolManaged: true, PoolSlot: "1"}
		// The claim sits under the riga ref, outside the city watcher's own
		// scope, but its route names the watcher's rig-scoped twin: a
		// city-wide singleton serves its rig-routed role, so this is not a
		// handoff and the holder keeps the claim.
		released := routeAwayTick(store, store, scoped, "", []session.Info{holder}, []beads.Bead{work}, []beads.Store{store}, []string{"riga"}, nil, routeAwayNow)
		requireKeptByHolder(t, store, released, work)
	})
}

// TestRouteAwayReachesClaimsAcrossStores pins that a holder reaches a claim
// for route-away exactly where the sweep's ownership guard says it holds one:
// on a split city through the claim refs (assignedWorkClaimRefs), and from a
// city-scoped agent into every rig store. A settled handoff to another pool is
// released there; one routed to the holder's own agent is not.
func TestRouteAwayReachesClaimsAcrossStores(t *testing.T) {
	settled := routedAtAgo(2 * routeAwaySettleWindow)

	t.Run("a split-topology claim on the leading arm is released", func(t *testing.T) {
		cfg, cityPath, infos := rigScopedWakeFixture(t)
		cfg.Agents = append(cfg.Agents, config.Agent{Name: "reviewer", Dir: "riga", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)})
		binding := beads.NewMemStore()
		seedSplitRoutes(t, cityPath, binding)
		work := beads.NewMemStore()
		meta := routedAtAgo(2 * routeAwaySettleWindow)
		meta[beadmeta.RoutedToMetadataKey] = "riga/reviewer"
		claim := seedRouteAwayWork(t, work, "test-city--worker-1", meta)

		released := routeAwayTick(work, work, cfg, cityPath, infos, []beads.Bead{claim}, []beads.Store{work}, []string{""}, nil, routeAwayNow)
		requireReleasedRouteAway(t, work, released, claim)
	})

	cityScopedHolderTick := func(t *testing.T, routedTo string) (beads.Store, beads.Bead, []releasedPoolAssignment) {
		t.Helper()
		cityPath := t.TempDir()
		seedNoRoutes(t, cityPath)
		cfg := &config.City{
			Rigs: []config.Rig{{Name: "repo", Path: t.TempDir()}},
			Agents: []config.Agent{
				{Name: "alpha", Scope: "city", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
				{Name: "beta", Dir: "repo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			},
		}
		cityStore := beads.NewMemStore()
		rigStore := beads.NewMemStore()
		meta := map[string]string{beadmeta.RoutedToMetadataKey: routedTo}
		for k, v := range settled {
			meta[k] = v
		}
		claim := seedRouteAwayWork(t, rigStore, "a-1", meta)
		released := routeAwayTick(cityStore, cityStore, cfg, cityPath, []session.Info{routeAwayHolder()}, []beads.Bead{claim}, []beads.Store{rigStore}, []string{"repo"}, map[string]beads.Store{"repo": rigStore}, routeAwayNow)
		return rigStore, claim, released
	}
	t.Run("a city-scoped holder's rig claim routed to a rig pool is released", func(t *testing.T) {
		rigStore, claim, released := cityScopedHolderTick(t, "repo/beta")
		requireReleasedRouteAway(t, rigStore, released, claim)
	})
	t.Run("a city-scoped holder's rig claim routed to its own agent is kept", func(t *testing.T) {
		rigStore, claim, released := cityScopedHolderTick(t, "alpha")
		requireKeptByHolder(t, rigStore, released, claim)
	})
}

// TestRouteAwayReleasesACrossClaimOnARoutedBead pins, as a deliberate choice,
// that a bead's route overrides any live claim on it: bead X was routed to
// beta long before a live session a-1 of alpha claimed it straight off the
// ready list (gc bd update --claim checks no route). No handoff moved X away
// from a-1, yet the sweep releases a-1's claim to beta's pool on its next
// tick, exactly as it releases a handoff, though a-1 may still be working X.
func TestRouteAwayReleasesACrossClaimOnARoutedBead(t *testing.T) {
	store := beads.NewMemStore()
	cfg := routeAwayCity()
	routed, err := store.Create(beads.Bead{Title: "ticket for beta", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "beta",
		beadmeta.RoutedAtMetadataKey: beadmeta.FormatRoutedAt(routeAwayNow.Add(-time.Hour)),
	}})
	if err != nil {
		t.Fatalf("create routed work: %v", err)
	}
	// a-1's claim, as gc bd update --claim writes it: the route is untouched.
	if err := store.Update(routed.ID, beads.UpdateOpts{Assignee: stringPtr("a-1"), Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("claim routed work as a-1: %v", err)
	}
	claimed, err := store.Get(routed.ID)
	if err != nil {
		t.Fatalf("reload claimed work: %v", err)
	}
	released := singleStoreRouteAwayTick(t, store, cfg, []session.Info{routeAwayHolder()}, claimed)
	requireReleasedRouteAway(t, store, released, claimed)
}

// listedRouteAwayWork is bead id as the tick's census lists it from store,
// which on a listWithoutRevisionStore, as on BdStore, carries no revision.
func listedRouteAwayWork(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatalf("list work: %v", err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("work %s not listed: %+v", id, rows)
	return beads.Bead{}
}

// TestRouteAwayRejudgesTheLiveRow pins that a route-away release judges the
// claim's live row, not the tick's listed one. The census lists X with no
// revision, as BdStore lists every row, so the orphan release's fence checks
// only status and assignee; whatever changes the route after the list must
// still keep a-1's claim. Routed back to a-1's own agent, re-routed with a
// fresh stamp (whose settle window restarts), re-stamped by a fresh handoff
// to the same pool, or labeled for a person, X stays with a-1. With nothing
// changed it is released.
func TestRouteAwayRejudgesTheLiveRow(t *testing.T) {
	cfg := routeAwayCity()
	cfg.Agents = append(cfg.Agents, config.Agent{Name: "gamma", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)})
	fresh := beadmeta.FormatRoutedAt(routeAwayNow.Add(-time.Second))
	for _, tc := range []struct {
		name     string
		after    beads.UpdateOpts
		released bool
	}{
		{name: "nothing changed since the list", released: true},
		{name: "routed back to the holder's agent", after: beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "alpha"}}},
		{name: "re-routed to another pool with a fresh stamp", after: beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gamma", beadmeta.RoutedAtMetadataKey: fresh}}},
		{name: "re-stamped by a fresh handoff to the same pool", after: beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedAtMetadataKey: fresh}}},
		{name: "labeled for a person", after: beads.UpdateOpts{Labels: []string{beadmeta.HumanLabel}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &listWithoutRevisionStore{MemStore: beads.NewMemStore()}
			work := seedRouteAwayWork(t, store, "a-1", routedAtAgo(2*routeAwaySettleWindow))
			listed := listedRouteAwayWork(t, store, work.ID)
			if listed.Revision != 0 {
				t.Fatalf("listed revision %d, want a revision-less list row", listed.Revision)
			}
			if tc.after.Metadata != nil || tc.after.Labels != nil {
				if err := store.Update(work.ID, tc.after); err != nil {
					t.Fatalf("change work after the list: %v", err)
				}
			}
			released := singleStoreRouteAwayTick(t, store, cfg, []session.Info{routeAwayHolder()}, listed)
			if tc.released {
				requireReleasedRouteAway(t, store, released, listed)
				return
			}
			requireKeptByHolder(t, store, released, listed)
		})
	}
}

// rerouteAfterReadStore lists rows with no revision, and routes bead id back
// to alpha right after the first Get of it returns: a write landing between
// a route-away release's live re-read and its write. It fences a write both
// ways, as a bd with --if-revision does: on a revision, and on a status and
// assignee (UpdateIfAssignment), which that route change does not fail.
type rerouteAfterReadStore struct {
	*listWithoutRevisionStore
	id    string
	fired bool
}

func (s *rerouteAfterReadStore) Get(id string) (beads.Bead, error) {
	b, err := s.listWithoutRevisionStore.Get(id)
	if err != nil || id != s.id || s.fired {
		return b, err
	}
	s.fired = true
	if err := s.SetMetadata(id, beadmeta.RoutedToMetadataKey, "alpha"); err != nil {
		return beads.Bead{}, err
	}
	return b, nil
}

// UpdateIfAssignment applies opts only while bead id still has
// expectedStatus and expectedAssignee, as bd's --if-status / --if-assignee
// guards do.
func (s *rerouteAfterReadStore) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts beads.UpdateOpts) (bool, error) {
	current, err := s.MemStore.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current.Status != expectedStatus || strings.TrimSpace(current.Assignee) != expectedAssignee {
		return false, nil
	}
	return true, s.Update(id, opts)
}

// TestRouteAwayReleaseIsFencedOnTheRereadRevision pins the write's fence: a
// route change landing after the release re-read X live, which that read
// cannot see, still keeps a-1's claim, because the write is fenced on the
// revision of the row the release judged. The store could also fence the
// write on X's status and assignee, which the route change leaves as they
// were, so this also pins that the revision fence is taken wherever the
// store has one.
func TestRouteAwayReleaseIsFencedOnTheRereadRevision(t *testing.T) {
	inner := &listWithoutRevisionStore{MemStore: beads.NewMemStore()}
	work := seedRouteAwayWork(t, inner, "a-1", routedAtAgo(2*routeAwaySettleWindow))
	listed := listedRouteAwayWork(t, inner, work.ID)
	store := &rerouteAfterReadStore{listWithoutRevisionStore: inner, id: work.ID}
	judge := newRouteAwayJudge("", routeAwayCity(), nil, []session.Info{routeAwayHolder()}, false, routeAwayNow)
	if !judge.releasable(listed, "") {
		t.Fatal("the listed claim is not routed away; the fixture no longer exercises the release")
	}

	out := judge.release(seatWorkHit{store: store, bead: listed}, "")
	if !store.fired {
		t.Fatal("the release never re-read the claim live")
	}
	if len(out.Released) != 0 || out.Why[work.ID] != keptMoved {
		t.Fatalf("outcome = released %d, why %q; want the claim kept as %q", len(out.Released), out.Why[work.ID], keptMoved)
	}
	got, err := inner.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "a-1" || got.Metadata[beadmeta.RoutedToMetadataKey] != "alpha" {
		t.Fatalf("work = status %q assignee %q routed to %q, want a-1's claim kept on its route back to alpha", got.Status, got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// TestRouteAwayKeepsTheClaimWhereTheStoreCanFenceNeitherWay pins that a
// store unable to fence a write either on a revision or on a status and
// assignee (a MemStore with conditional writes off has no guarded update)
// keeps a routed-away claim rather than release it unfenced.
func TestRouteAwayKeepsTheClaimWhereTheStoreCanFenceNeitherWay(t *testing.T) {
	store := &listWithoutRevisionStore{MemStore: beads.NewMemStore()}
	work := seedRouteAwayWork(t, store, "a-1", routedAtAgo(2*routeAwaySettleWindow))
	listed := listedRouteAwayWork(t, store, work.ID)
	store.DisableConditionalWrites = true
	judge := newRouteAwayJudge("", routeAwayCity(), nil, []session.Info{routeAwayHolder()}, false, routeAwayNow)

	out := judge.release(seatWorkHit{store: store, bead: listed}, "")
	if len(out.Released) != 0 || out.Why[work.ID] != keptUnfenced {
		t.Fatalf("outcome = released %d, why %q; want the claim kept as %q", len(out.Released), out.Why[work.ID], keptUnfenced)
	}
	requireKeptByHolder(t, store, singleStoreRouteAwayTick(t, store, routeAwayCity(), []session.Info{routeAwayHolder()}, listed), listed)
}

// bdRouteAwayLedger is bead X on the pinned bd (bdGuardLedger: no
// --if-revision, but bd update's --if-status and --if-assignee guards),
// claimed in_progress by a-1 of alpha and routed to beta past the settle
// window.
func bdRouteAwayLedger() *bdGuardLedger {
	return newBdGuardLedger("in_progress", "a-1", map[string]string{
		beadmeta.RoutedToMetadataKey: "beta",
		beadmeta.RoutedAtMetadataKey: beadmeta.FormatRoutedAt(routeAwayNow.Add(-2 * routeAwaySettleWindow)),
	})
}

// requireBdGuardedRouteAwayRelease checks that ledger took exactly one write,
// bd's guarded update on a-1's in_progress claim, and that it released X and
// cleared X's affinity metadata.
func requireBdGuardedRouteAwayRelease(t *testing.T, ledger *bdGuardLedger) {
	t.Helper()
	if len(ledger.updates) != 1 {
		t.Fatalf("bd updates = %v, want exactly one guarded write", ledger.updates)
	}
	update := ledger.updates[0]
	if got := testFlagValue(update, "--if-status"); got != "in_progress" {
		t.Errorf("--if-status = %q, want the judged row's in_progress", got)
	}
	if got := testFlagValue(update, "--if-assignee"); got != "a-1" {
		t.Errorf("--if-assignee = %q, want the judged row's a-1", got)
	}
	got := ledger.row()
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("row = status %q assignee %q, want open and unassigned", got.Status, got.Assignee)
	}
	for key, want := range clearedSessionAffinityMetadata() {
		if got.Metadata[key] != want {
			t.Errorf("metadata[%q] = %q, want it cleared in the same write", key, got.Metadata[key])
		}
	}
}

// TestRouteAwayReleasedOnTheDefaultBdThroughProductionComposition is #7004 on
// the default bd provider: one bd store holds the city's work and sessions,
// and the pinned bd cannot fence a write on a revision. The tick's sweep
// still releases X from its live holder a-1 to beta's pool, in one bd update
// guarded on X's status and assignee.
func TestRouteAwayReleasedOnTheDefaultBdThroughProductionComposition(t *testing.T) {
	ledger := bdRouteAwayLedger()
	store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
	listed := listedRouteAwayWork(t, store, ledger.row().ID)

	released := routeAwayTick(store, store, routeAwayCity(), "", []session.Info{routeAwayHolder()}, []beads.Bead{listed}, []beads.Store{store}, []string{""}, nil, routeAwayNow)
	if len(released) != 1 || released[0].ID != listed.ID || !released[0].RoutedAway {
		t.Fatalf("released = %+v, want exactly [{ID:%s RoutedAway:true}]; bd updates %v", released, listed.ID, ledger.updates)
	}
	requireBdGuardedRouteAwayRelease(t, ledger)
}

// TestRouteAwayReleaseFencesTheJudgedRowOnTheDefaultBd pins the release's
// write on the pinned bd, which cannot fence a revision: it is fenced on the
// status and assignee of the row the release read live and judged, in bd
// update's own guards. The claim is released in that one write, also through
// a resolve-target wrapper. A route back to a-1's agent after the tick's list
// is seen by the live read, which keeps the claim with no write. A claim that
// changes hands between that read and the write fails the guard: bd writes
// nothing, and the claim is kept for the next tick. A bd that has neither
// fence keeps the claim as unfenced.
func TestRouteAwayReleaseFencesTheJudgedRowOnTheDefaultBd(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wrap        bool
		noGuards    bool
		afterList   func(*beads.Bead) // lands between the tick's list and the release's live read
		beforeWrite func(*beads.Bead) // lands between the live read and the guarded write
		wantWhy     string            // "" is released
		wantWrites  int
		// The kept row's status, assignee and route.
		wantStatus, wantAssignee, wantRoute string
	}{
		{name: "released in one guarded write"},
		{name: "through a resolve-target wrapper", wrap: true},
		{
			name:      "routed back to the holder's agent after the list",
			afterList: func(b *beads.Bead) { b.Metadata[beadmeta.RoutedToMetadataKey] = "alpha" },
			wantWhy:   keptMoved, wantStatus: "in_progress", wantAssignee: "a-1", wantRoute: "alpha",
		},
		{
			name:        "claimed by another session between the read and the write",
			beforeWrite: func(b *beads.Bead) { b.Assignee = "b-1" },
			wantWhy:     keptMoved, wantWrites: 1, wantStatus: "in_progress", wantAssignee: "b-1", wantRoute: "beta",
		},
		{
			name:     "a bd with neither fence",
			noGuards: true,
			wantWhy:  keptUnfenced, wantWrites: 1, wantStatus: "in_progress", wantAssignee: "a-1", wantRoute: "beta",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := bdRouteAwayLedger()
			ledger.noGuards = tc.noGuards
			var store beads.Store = beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
			if tc.wrap {
				// The typed class wrappers declare their resolve target
				// rather than promote the store's capabilities.
				store = beads.WorkStore{Store: store}
			}
			listed := listedRouteAwayWork(t, store, ledger.row().ID)
			judge := newRouteAwayJudge("", routeAwayCity(), nil, []session.Info{routeAwayHolder()}, false, routeAwayNow)
			if !judge.releasable(listed, "") {
				t.Fatal("the listed claim is not routed away; the fixture no longer exercises the release")
			}
			if tc.afterList != nil {
				tc.afterList(&ledger.bead)
			}
			ledger.beforeUpdate = tc.beforeWrite

			out := judge.release(seatWorkHit{store: store, bead: listed}, "")
			if tc.wantWhy == "" {
				if len(out.Released) != 1 {
					t.Fatalf("outcome = released %d, why %q; want the claim released; bd updates %v", len(out.Released), out.Why[listed.ID], ledger.updates)
				}
				requireBdGuardedRouteAwayRelease(t, ledger)
				return
			}
			if len(out.Released) != 0 || out.Why[listed.ID] != tc.wantWhy {
				t.Fatalf("outcome = released %d, why %q; want the claim kept as %q", len(out.Released), out.Why[listed.ID], tc.wantWhy)
			}
			if len(ledger.updates) != tc.wantWrites {
				t.Fatalf("bd updates = %v, want %d", ledger.updates, tc.wantWrites)
			}
			got := ledger.row()
			if got.Status != tc.wantStatus || got.Assignee != tc.wantAssignee || got.Metadata[beadmeta.RoutedToMetadataKey] != tc.wantRoute {
				t.Fatalf("row = status %q assignee %q routed to %q, want %q, %q, %q", got.Status, got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey], tc.wantStatus, tc.wantAssignee, tc.wantRoute)
			}
		})
	}
}
