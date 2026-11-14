package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// Typed-stores T7g part 2: R4's close confirmation through CloseOwner, the
// event router's refs (N1), and the step reminder's Molecule{Active} readers.

// liveCloseOwnerReference is the pre-T7g liveCloseOwner's relic branch.
func liveCloseOwnerReference(stores []beads.Store, id string) (beads.Store, beads.Bead, error) {
	var openStore beads.Store
	var open beads.Bead
	for _, store := range stores {
		var b beads.Bead
		err := beads.FederatedRead(func() (err error) {
			b, err = beads.HandlesFor(store).Live.Get(id)
			return err
		})
		switch {
		case errors.Is(err, beads.ErrNotFound):
		case err != nil || b.Status == "closed":
			return store, b, err
		case openStore == nil:
			openStore, open = store, b
		}
	}
	if openStore == nil {
		return stores[0], beads.Bead{}, beads.ErrNotFound
	}
	return openStore, open, nil
}

// r4Store stages id in one of four states.
func r4Store(t *testing.T, prefix, id, state string) beads.Store {
	t.Helper()
	m := newTestMem(prefix)
	s := &downGetStore{Store: m}
	switch state {
	case "missing":
	case "unreadable":
		s.down.Store(true)
	default:
		fedCreate(t, m, beads.Bead{ID: id, Title: "row", Type: "task"})
		if state == "closed" {
			if err := m.Close(id); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s
}

// TestLiveCloseOwnerAgreesWithTheFrozenReference runs every combination of
// row states across the route (the configured prefix's store, the
// fallback's rigs then the city, or none for an unbound rig) and the relic
// binding, through the
// CloseOwner plan and through the pre-T7g loop over the same stores in the
// same order: the verdict, and the owner of a confirmed close, agree.
func TestLiveCloseOwnerAgreesWithTheFrozenReference(t *testing.T) {
	const id = "ga-7"
	states := []string{"open", "closed", "missing", "unreadable"}
	for _, kind := range []string{"configured", "fallback", "unbound rig"} {
		for _, city := range states {
			for _, rig := range states {
				for _, relic := range states {
					if kind != "fallback" && rig != "missing" {
						continue // the configured route is one store, an unbound rig's none
					}
					if kind == "unbound rig" && city != "missing" {
						continue // the route is empty: only the relic is read
					}
					name := fmt.Sprintf("route=%s/city=%s/rig=%s/relic=%s", kind, city, rig, relic)
					cityStore, rigStore, relicStore := r4Store(t, "ga", id, city), r4Store(t, "ra", id, rig), r4Store(t, "gcg", id, relic)
					epoch := testStoresWithRigs(cityStore, relicStore, map[string]testRig{"alpha": {store: rigStore, prefix: "ra"}})
					route := []storeref.StoreRef{storeref.WorkRef}
					stores := []beads.Store{cityStore, relicStore}
					switch kind {
					case "fallback":
						route = []storeref.StoreRef{storeref.RigRef("alpha"), storeref.WorkRef}
						stores = []beads.Store{rigStore, cityStore, relicStore}
					case "unbound rig":
						// The configured prefix names a rig with no open
						// store: no route, the relic alone (withRelicStore
						// over no event store).
						route = nil
						stores = []beads.Store{relicStore}
					}
					wantStore, wantRow, wantErr := liveCloseOwnerReference(stores, id)
					gotStore, gotRow, gotErr := liveCloseOwner(epoch, route, stores, id, relicStore)
					want, got := confirmInferredClose(wantRow, wantErr), confirmInferredClose(gotRow, gotErr)
					if got != want {
						t.Errorf("%s: verdict %v (%v), reference %v (%v)", name, got, gotErr, want, wantErr)
						continue
					}
					if want == closeConfirmed && !beads.SameStore(gotStore, wantStore) {
						t.Errorf("%s: the close is confirmed in another store than the reference's", name)
					}
				}
			}
		}
	}
}

// TestLiveCloseOwnerFailsClosedOffTheEpoch: a relic the epoch holds no
// binding for, or a route the epoch cannot name, leaves the close
// unconfirmed (the sweep re-reads it), never refuted.
func TestLiveCloseOwnerFailsClosedOffTheEpoch(t *testing.T) {
	city, relic := r4Store(t, "ga", "ga-1", "closed"), r4Store(t, "gcg", "ga-1", "closed")
	epoch := testStoresWithRigs(city, relic, nil)
	stray := r4Store(t, "gcg", "ga-1", "closed")
	if _, row, err := liveCloseOwner(epoch, []storeref.StoreRef{storeref.WorkRef}, []beads.Store{city, stray}, "ga-1", stray); confirmInferredClose(row, err) != closeUnconfirmed {
		t.Errorf("a relic off the epoch: %v, want unconfirmed", err)
	}
	if _, row, err := liveCloseOwner(epoch, []storeref.StoreRef{storeref.RigRef("gone")}, []beads.Store{city, relic}, "ga-1", relic); confirmInferredClose(row, err) != closeUnconfirmed {
		t.Errorf("a route off the epoch: %v, want unconfirmed", err)
	}
}

// TestBeadEventRouteLockedPinsOrderAndNilRigs (N1): the event route is the
// configured prefix's ref, and on the unconfigured fallback every open rig
// by name, then the city: the order the rig map's iteration gave
// beadEventStoresLocked varied run to run, and an unopened rig's nil entry,
// which liveReadOwner would have panicked reading, is not a leg. The route
// names exactly the stores the fallback lists, nil aside.
func TestBeadEventRouteLockedPinsOrderAndNilRigs(t *testing.T) {
	city := newTestMem("ga")
	rigs := map[string]beads.Store{"charlie": newTestMem("rc"), "alpha": newTestMem("ra"), "bravo": nil}
	cs := &controllerState{cityBeadStore: city, beadStores: rigs, cfg: &config.City{Workspace: config.Workspace{Name: "c", Prefix: "ga"}, Rigs: []config.Rig{{Name: "alpha", Prefix: "ra"}, {Name: "bravo", Prefix: "rb"}, {Name: "charlie", Prefix: "rc"}}}}
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"ga-1", `[""]`},
		{"ra-1", `[rig:alpha]`},
		{"rb-1", `[]`}, // configured, unopened: owned but absent here
		{"zz-1", `[rig:alpha rig:charlie ""]`},
	} {
		route := cs.beadEventRouteLocked(tc.id)
		var parts []string
		for _, r := range route {
			if r == storeref.WorkRef {
				parts = append(parts, `""`)
			} else {
				parts = append(parts, string(r))
			}
		}
		if got := "[" + strings.Join(parts, " ") + "]"; got != tc.want {
			t.Errorf("%s: route %s, want %s", tc.id, got, tc.want)
		}
		// The route's stores are the event stores, nil aside, in any order.
		stores := cs.beadEventStoresLocked(tc.id)
		var listed []string
		for _, s := range stores {
			if s != nil {
				listed = append(listed, fmt.Sprintf("%p", s))
			}
		}
		var routed []string
		for _, r := range route {
			if r == storeref.WorkRef {
				routed = append(routed, fmt.Sprintf("%p", city))
				continue
			}
			name, _ := storeref.ScopeRigContext(string(r))
			routed = append(routed, fmt.Sprintf("%p", rigs[name]))
		}
		sort.Strings(listed)
		sort.Strings(routed)
		if fmt.Sprint(listed) != fmt.Sprint(routed) {
			t.Errorf("%s: the route names %v, the event stores are %v", tc.id, routed, listed)
		}
	}
}

// --- the step reminder ---

// resolveActiveWispStepReference is the pre-T7g resolveActiveWispStep: the
// same resolution over the molecule readers the old activeStep built (graph,
// then work when the stores differ by the old pointer test), each one reader
// for the listing and for every other read.
func resolveActiveWispStepReference(workStore, graphStore beads.Store, assignees []string) (*beads.Bead, error) {
	if workStore == nil || len(assignees) == 0 {
		return nil, nil
	}
	if graphStore == nil {
		graphStore = workStore
	}
	oneStore := sameStoreHandleReference(workStore, graphStore)
	r := stepResolution{work: workStore, graph: graphStore, logf: func(string, ...any) {}}
	r.molecules = []stepMoleculeLeg{{list: graphStore, strict: graphStore}}
	if !oneStore {
		r.molecules = append(r.molecules, stepMoleculeLeg{list: workStore, strict: workStore})
	}
	return r.activeStep(oneStore, assignees)
}

func sameStoreHandleReference(a, b beads.Store) bool {
	if a == nil || b == nil {
		return false
	}
	return fmt.Sprintf("%T", a) == fmt.Sprintf("%T", b) && fmt.Sprintf("%p", a) == fmt.Sprintf("%p", b)
}

// TestActiveWispStepAgreesWithTheFrozenReference: the step reminder over the
// epoch's Molecule{Active} plan resolves the step the pre-T7g readers did,
// on one store and two, the molecule in either, a newer one in each, and
// a v1 molecule reached only through the molecule_id bridge.
func TestActiveWispStepAgreesWithTheFrozenReference(t *testing.T) {
	type seed func(t *testing.T, work, graph beads.Store)
	molecule := func(store beads.Store, id string, created time.Duration) {
		fedCreate(t, store, beads.Bead{ID: id, Title: id, Type: "molecule", Assignee: "alice", CreatedAt: time.Now().Add(created)})
		markInProgress(t, store, id)
		fedCreate(t, store, beads.Bead{ID: id + "-s", Title: "step of " + id, Type: "step", ParentID: id})
		markInProgress(t, store, id+"-s")
	}
	seeds := map[string]seed{
		"molecule in graph":         func(_ *testing.T, _, g beads.Store) { molecule(g, "gcg-m", 0) },
		"molecule on the work side": func(_ *testing.T, w, _ beads.Store) { molecule(w, "ga-m", 0) },
		"a newer one in each":       func(_ *testing.T, w, g beads.Store) { molecule(g, "gcg-old", -time.Hour); molecule(w, "ga-new", 0) },
		"bridged v1 molecule": func(t *testing.T, w, _ beads.Store) {
			fedCreate(t, w, beads.Bead{ID: "ga-root", Title: "root", Type: "molecule"})
			fedCreate(t, w, beads.Bead{ID: "ga-root-s", Title: "entry", Type: "step", ParentID: "ga-root"})
			fedCreate(t, w, beads.Bead{ID: "ga-src", Title: "src", Type: "task", Assignee: "alice", Metadata: map[string]string{"molecule_id": "ga-root"}})
			markInProgress(t, w, "ga-src")
		},
		"nothing": func(*testing.T, beads.Store, beads.Store) {},
	}
	for shape, split := range map[string]bool{"one store": false, "split": true} {
		for name, sd := range seeds {
			t.Run(shape+"/"+name, func(t *testing.T) {
				build := func() (beads.Store, beads.Store) {
					work := newTestMem("ga")
					graph := beads.Store(work)
					if split {
						graph = newTestMem("gcg")
					}
					sd(t, work, graph)
					return work, graph
				}
				rw, rg := build()
				want, werr := resolveActiveWispStepReference(rw, rg, []string{"alice"})
				cw, cg := build()
				got, gerr := resolveActiveWispStep(cw, testStores(cw, cg), []string{"alice"})
				if fmt.Sprint(r4StepID(got)) != fmt.Sprint(r4StepID(want)) || fmt.Sprint(gerr) != fmt.Sprint(werr) {
					t.Errorf("step = %v, %v; reference %v, %v", r4StepID(got), gerr, r4StepID(want), werr)
				}
				if name != "nothing" && want == nil {
					t.Errorf("the reference resolved no step: the row is vacuous")
				}
			})
		}
	}
}

func r4StepID(b *beads.Bead) string {
	if b == nil {
		return "<none>"
	}
	return b.ID
}

// TestActiveWispStepFollowOnReadsAreStrict (F6): the molecule listing reads
// through the plan's federated leg, and the reader the follow-on reads take
// (the molecule's owner, a root by id) is the leg's strict reader, so the
// gate polices those as it always has.
func TestActiveWispStepFollowOnReadsAreStrict(t *testing.T) {
	work, graph := newTestMem("ga"), newTestMem("gcg")
	fedCreate(t, graph, beads.Bead{ID: "gcg-m", Title: "m", Type: "molecule", Assignee: "alice"})
	markInProgress(t, graph, "gcg-m")
	plan, err := testStores(work, graph).Plan(storeref.Molecule{Kind: storeref.MoleculeActive, Scope: storeref.WorkRef, Near: work, NearRef: stepWorkStoreRef})
	if err != nil {
		t.Fatal(err)
	}
	r := stepResolution{work: work, logf: func(string, ...any) {}}
	storeref.EachTypedLeg(plan, func(leg storeref.TypedLeg) { r.molecules = append(r.molecules, stepMoleculeLegOf(leg, false)) })
	for i, leg := range r.molecules {
		if l, ok := leg.list.(beads.FederatedReader); !ok || l.Role() != beads.FederatedLegRole {
			t.Errorf("leg %d's listing reader is not a federated leg", i)
		}
		if s, ok := leg.strict.(beads.FederatedReader); !ok || s.Role() != beads.NoLegRole {
			t.Errorf("leg %d's follow-on reader is not strict", i)
		}
	}
	_, owner, err := r.activeMoleculeIn([]string{"alice"})
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := owner.(beads.FederatedReader); !ok || o.Role() != beads.NoLegRole {
		t.Errorf("the molecule's owner reader %T is not the strict reader", owner)
	}
}

// TestActiveWorkStepBindsEveryLegToTheRequest: the controller's reminder
// reads each plan leg through the clone the request binds it to, and a bind
// failure is reported as it always was.
func TestActiveWorkStepBindsEveryLegToTheRequest(t *testing.T) {
	work := newTestMem("ga")
	plan, err := testStores(work, work).Plan(storeref.Molecule{Kind: storeref.MoleculeActive, Scope: storeref.WorkRef, Near: work, NearRef: stepWorkStoreRef})
	if err != nil {
		t.Fatal(err)
	}
	clone := newTestMem("ga")
	fedCreate(t, clone, beads.Bead{ID: "ga-m", Title: "m", Type: "molecule", Assignee: "alice"})
	markInProgress(t, clone, "ga-m")
	fedCreate(t, clone, beads.Bead{ID: "ga-m-s", Title: "step", Type: "step", ParentID: "ga-m"})
	markInProgress(t, clone, "ga-m-s")
	bound, err := storeref.BindLegs(plan, func(beads.Store) (beads.Store, error) { return clone, nil })
	if err != nil {
		t.Fatal(err)
	}
	r := stepResolution{logf: func(string, ...any) {}}
	storeref.EachTypedLeg(bound, func(leg storeref.TypedLeg) { r.molecules = append(r.molecules, stepMoleculeLegOf(leg, true)) })
	r.graph, r.work = r.molecules[0].strict, r.molecules[len(r.molecules)-1].strict
	got, err := r.activeStep(len(plan.Legs) == 1, []string{"alice"})
	if err != nil || got == nil {
		t.Fatalf("active step = %v, %v; want the clone's molecule read", r4StepID(got), err)
	}
	if b, err := r.work.Get("ga-m"); err != nil || b.ID != "ga-m" {
		t.Fatalf("the work reader does not read the clone: %v", err)
	}
	_ = context.Background()
}

func markInProgress(t *testing.T, s beads.Store, id string) {
	t.Helper()
	status := "in_progress"
	if err := s.Update(id, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatal(err)
	}
}

// TestLiveCloseOwnerMatchesTheRelicThroughTheGatesView: under the
// split-strict gate the event's relic is the gate's class view of the
// binding (splitStrictEventStores). The CloseOwner plan names the binding by
// the view's front, and a close confirmed in the binding is handed back as
// the event's own relic handle, so relicStoreRef still takes the ID-only
// mode.
func TestLiveCloseOwnerMatchesTheRelicThroughTheGatesView(t *testing.T) {
	saved := splitStrictSeam
	splitStrictSeam = splitStrictGate{}
	t.Cleanup(func() { splitStrictSeam = saved })
	city, binding := r4Store(t, "ga", "ga-1", "open"), r4Store(t, "gcg", "ga-1", "closed")
	relicView := splitStrictGate{}.ClassView(binding)
	epoch := testStores(city, binding)
	owner, row, err := liveCloseOwner(epoch, []storeref.StoreRef{storeref.WorkRef}, []beads.Store{city, relicView}, "ga-1", relicView)
	if confirmInferredClose(row, err) != closeConfirmed || owner != relicView {
		t.Fatalf("close owner = %T (%v): want the relic's view, confirmed", owner, err)
	}
	if ref := relicStoreRef(owner, relicView, "rig:x"); ref != "" {
		t.Fatalf("relicStoreRef = %q, want the ID-only mode", ref)
	}
}

// TestActiveWorkStepFollowOnReadsAreStrictLive (F6, the controller path): with
// the legs bound and read live, the listing reader carries the federated
// role and the follow-on reader none, so the gate still polices the step
// reads the controller makes through its clones.
func TestActiveWorkStepFollowOnReadsAreStrictLive(t *testing.T) {
	work, graph := newTestMem("ga"), newTestMem("gcg")
	plan, err := testStores(work, graph).Plan(storeref.Molecule{Kind: storeref.MoleculeActive, Scope: storeref.WorkRef, Near: work, NearRef: stepWorkStoreRef})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := storeref.BindLegs(plan, func(s beads.Store) (beads.Store, error) { return s, nil })
	if err != nil {
		t.Fatal(err)
	}
	var legs []stepMoleculeLeg
	storeref.EachTypedLeg(bound, func(leg storeref.TypedLeg) { legs = append(legs, stepMoleculeLegOf(leg, true)) })
	if len(legs) != 2 {
		t.Fatalf("legs = %d, want graph and work", len(legs))
	}
	for i, leg := range legs {
		if lt := fmt.Sprintf("%T", leg.list); !strings.Contains(lt, "federatedReads") {
			t.Errorf("leg %d's live listing reader is %s, want the federated-role adapter", i, lt)
		}
		if st := fmt.Sprintf("%T", leg.strict); strings.Contains(st, "federated") || strings.Contains(st, "relic") {
			t.Errorf("leg %d's live follow-on reader is %s, want a reader with no leg role", i, st)
		}
	}
}

// TestLiveCloseOwnerFailsClosedOnAWorkerSite (round 2, M3): a worker site
// whose every class is refused mints its binding over the refusing store
// (T7c), so the relic the residence probe proved (the raw binding) is no
// binding of the epoch: the close stays unconfirmed, for the sweep, never
// refuted and never confirmed in a store the epoch does not serve.
func TestLiveCloseOwnerFailsClosedOnAWorkerSite(t *testing.T) {
	city, relic := r4Store(t, "ga", "ga-1", "open"), r4Store(t, "gcg", "ga-1", "closed")
	epoch := workerSiteEpoch(city, relic, nil)
	_, row, err := liveCloseOwner(epoch, []storeref.StoreRef{storeref.WorkRef}, []beads.Store{city, relic}, "ga-1", relic)
	if confirmInferredClose(row, err) != closeUnconfirmed {
		t.Fatalf("a worker site's relic close = %v (%v), want unconfirmed", confirmInferredClose(row, err), err)
	}
}
