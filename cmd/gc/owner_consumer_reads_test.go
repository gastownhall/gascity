package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// Typed-stores T7g, the owner consumers (plan §3.7, R1, R2): the store
// identity checks the relic and suspended-rig paths make move to
// beads.SameStore, and the owner reads that peeled a cache by hand move to
// beads.LiveFor. Each converted function's pre-T7g body is frozen below as a
// reference, and each equivalence table runs it and the converted function
// over the same corpus: every raw shape the controller hands them today
// (where the two must agree exactly), the split-strict gate's views of one
// store (gate on), and each class handle and resident handle over those
// shapes, where the converted function must answer for the handle as the
// reference answers for the store it wraps.

// cmdHandleWraps are the shapes a T7 conversion hands an owner consumer: the
// raw store, a class handle over it, and the resident handles storeref's
// executors mint (over the store, and over a federated leg of it).
var cmdHandleWraps = map[string]func(beads.Store) beads.Store{
	"the store":               func(s beads.Store) beads.Store { return s },
	"a work handle":           func(s beads.Store) beads.Store { return beadstest.Work(s) },
	"a graph handle":          func(s beads.Store) beads.Store { return beadstest.Graph(s) },
	"a sessions handle":       func(s beads.Store) beads.Store { return beadstest.Sessions(s) },
	"a resident handle":       func(s beads.Store) beads.Store { return beadstest.Resident(s) },
	"a resident over its leg": func(s beads.Store) beads.Store { return beadstest.ResidentLeg(beadstest.FederatedLeg(s)) },
}

// --- frozen references: the pre-T7g bodies, verbatim but for their names ---

func withRelicStoreReference(stores []beads.Store, relic beads.Store) []beads.Store {
	for _, s := range stores {
		if sameBeadStore(s, relic) {
			return stores
		}
	}
	return append(append(make([]beads.Store, 0, len(stores)+1), stores...), relic)
}

func relicStoreRefReference(owner, relic beads.Store, prefixRef string) string {
	if relic != nil && sameBeadStore(owner, relic) {
		return ""
	}
	return prefixRef
}

func anySuspendedRigStoreLockedReference(cs *controllerState, stores []beads.Store) bool {
	for name, rigStore := range cs.beadStores {
		if !cs.rigSuspended(name) {
			continue
		}
		for _, store := range stores {
			if store == rigStore {
				return true
			}
		}
	}
	return false
}

type liveReadReference struct{ store beads.Store }

func (r liveReadReference) Get(id string) (beads.Bead, error) {
	return beads.HandlesFor(r.store).Live.Get(id)
}

func liveReadsReference(store beads.Store) liveReadReference {
	for {
		switch v := store.(type) {
		case beads.SessionStore:
			store = v.Store
		case beads.WorkStore:
			store = v.Store
		case readOnlyStore:
			store = v.Store
		case blindWriteRefusingStore:
			store = v.inner
		default:
			return liveReadReference{store: store}
		}
	}
}

// ownerRowReadReference is the read prevAssignedBeadStatus and
// freshAnchorBeadTerminal made of the by-id owner before T7g.
func ownerRowReadReference(topo storeref.Topology, id string) (beads.Bead, error) {
	owner, err := byIDOwnerRowForTopology(topo, id)
	if err != nil {
		return beads.Bead{}, err
	}
	return uncachedBeadStore(owner.Store).Get(id)
}

// readLiveReference is WorkLegs.readLive before T7g.
func readLiveReference(l WorkLegs, id string) (beads.Bead, bool, error) {
	if err := l.unusable(); err != nil {
		return beads.Bead{}, false, err
	}
	owner, err := byIDOwnerRowForTopology(l.topo, id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return beads.Bead{}, false, nil
	case err != nil:
		return beads.Bead{}, false, err
	case owner.Store == nil:
		return beads.Bead{}, false, nil
	}
	b, err := beads.HandlesFor(owner.Store).Live.Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return beads.Bead{}, false, nil
	case err != nil:
		return beads.Bead{}, false, err
	}
	return b, b.ID == id, nil
}

// --- the store corpus ---

// unhashableStore is a store whose dynamic type == panics on.
type unhashableStore struct {
	beads.Store
	tags []string
}

// identityCorpus is every store shape an identity check sees: two physical
// stores and a second name for one of them, a CachingStore, an unhashable
// store, a typed nil, nil, and the gate's work and class views of one store
// (gate on).
func identityCorpus() map[string]beads.Store {
	a, gated := beads.NewMemStore(), beads.NewMemStore()
	projection := splittest.ProjectionOf(gated)
	return map[string]beads.Store{
		"mem a":      a,
		"mem a, too": a,
		"mem b":      beads.NewMemStore(),
		"cache":      beads.NewCachingStoreForTest(beads.NewMemStore(), nil),
		"unhashable": unhashableStore{Store: beads.NewMemStore(), tags: []string{"x"}},
		"work view":  projection.WorkViewOf(gated),
		"class view": projection.ClassViewOf(gated),
		"typed nil":  (*beads.MemStore)(nil),
		"nil":        nil,
	}
}

// TestRelicIdentityAgreesWithTheFrozenReference: withRelicStore and
// relicStoreRef answer every raw pair as their == bodies did, and answer a
// handle over a store as the reference answers the store. The prefix-route
// shapes are the R4 corpus: unsplit (the relic is the prefix route), whole
// split (a distinct binding), relic-proven (the binding added beside the
// route), co-resident id (both stores hold it) and the gate's views.
func TestRelicIdentityAgreesWithTheFrozenReference(t *testing.T) {
	corpus := identityCorpus()
	for routeName, route := range corpus {
		for relicName, relic := range corpus {
			if relic == nil {
				continue // withRelicStore is called only with a proven relic
			}
			for wrapName, wrap := range cmdHandleWraps {
				if route == nil && wrapName != "the store" {
					continue
				}
				name := fmt.Sprintf("route=%s/relic=%s/%s", routeName, relicName, wrapName)
				wrapped := route
				if route != nil {
					wrapped = wrap(route)
				}
				want := withRelicStoreReference([]beads.Store{route}, relic)
				got := withRelicStore([]beads.Store{wrapped}, relic)
				if len(got) != len(want) {
					t.Errorf("%s: withRelicStore lists %d store(s), the reference %d", name, len(got), len(want))
				}
				wantRef := relicStoreRefReference(route, relic, "rig:alpha")
				if gotRef := relicStoreRef(wrapped, relic, "rig:alpha"); gotRef != wantRef {
					t.Errorf("%s: relicStoreRef = %q, the reference %q", name, gotRef, wantRef)
				}
			}
		}
	}
}

// TestAnySuspendedRigStoreAgreesWithTheFrozenReference: the suspended-rig
// skip answers every raw shape as its == body did, an unopened suspended rig
// (a nil map entry) included, and answers a handle over a suspended rig's
// store as the reference answers the store: a handle reaching the old check
// was read, which restarts the rig's retired proxy (R2).
func TestAnySuspendedRigStoreAgreesWithTheFrozenReference(t *testing.T) {
	alpha, beta, city := beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore()
	gated := beads.NewMemStore()
	view := splittest.ProjectionOf(gated).WorkViewOf(gated)
	for _, tc := range []struct {
		name      string
		rigs      map[string]beads.Store
		suspended map[string]bool
		stores    []beads.Store
	}{
		{name: "unsplit, no rig suspended", rigs: map[string]beads.Store{"alpha": alpha}, stores: []beads.Store{alpha}},
		{name: "the route is a suspended rig", rigs: map[string]beads.Store{"alpha": alpha, "beta": beta}, suspended: map[string]bool{"alpha": true}, stores: []beads.Store{alpha}},
		{name: "another rig is suspended", rigs: map[string]beads.Store{"alpha": alpha, "beta": beta}, suspended: map[string]bool{"beta": true}, stores: []beads.Store{alpha}},
		{name: "the fallback lists a suspended rig", rigs: map[string]beads.Store{"alpha": alpha, "beta": beta}, suspended: map[string]bool{"beta": true}, stores: []beads.Store{alpha, beta, city}},
		{name: "relic-proven: the binding beside a suspended rig's route", rigs: map[string]beads.Store{"alpha": alpha}, suspended: map[string]bool{"alpha": true}, stores: []beads.Store{alpha, city}},
		{name: "an unopened suspended rig in the fallback", rigs: map[string]beads.Store{"alpha": nil, "beta": beta}, suspended: map[string]bool{"alpha": true}, stores: []beads.Store{nil, beta, city}},
		{name: "an unopened suspended rig, no nil listed", rigs: map[string]beads.Store{"alpha": nil, "beta": beta}, suspended: map[string]bool{"alpha": true}, stores: []beads.Store{beta, city}},
		{name: "gate on: the rig's view", rigs: map[string]beads.Store{"alpha": view}, suspended: map[string]bool{"alpha": true}, stores: []beads.Store{view}},
		{name: "gate on: another view of the rig's store", rigs: map[string]beads.Store{"alpha": view}, suspended: map[string]bool{"alpha": true}, stores: []beads.Store{splittest.ProjectionOf(gated).ClassViewOf(gated)}},
	} {
		cs := &controllerState{beadStores: tc.rigs}
		cs.setSuspendedRigs(tc.suspended)
		want := anySuspendedRigStoreLockedReference(cs, tc.stores)
		for wrapName, wrap := range cmdHandleWraps {
			wrapped := make([]beads.Store, len(tc.stores))
			for i, s := range tc.stores {
				wrapped[i] = s
				if s != nil {
					wrapped[i] = wrap(s)
				}
			}
			if got := cs.anySuspendedRigStoreLocked(wrapped); got != want {
				t.Errorf("%s through %s: suspended = %v, the reference over the stores %v", tc.name, wrapName, got, want)
			}
		}
	}
}

// staleCachedRow is a CachingStore whose primed row id still reads open
// while its backing reads closed: a close another process made, which the
// cache has not seen. It skips the test when the cache already serves the
// close, since there is then nothing to read past.
func staleCachedRow(t *testing.T, id string) (*beads.CachingStore, *beads.MemStore) {
	t.Helper()
	backing := beads.NewMemStoreFrom(0, []beads.Bead{{ID: id, Status: "open", Title: "row", Type: "task", Assignee: "worker-1"}}, nil)
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := backing.Close(id); err != nil {
		t.Fatal(err)
	}
	if b, err := cache.Get(id); err != nil || b.Status != "open" {
		t.Skipf("the cache already serves the backing's close (%q, %v); nothing to read past", b.Status, err)
	}
	return cache, backing
}

// TestOwnerReadsReadPastAStaleCacheThroughAHandle pins every converted owner
// read to the backing's closed row over a CachingStore whose cache still
// reads it open, through every handle shape, and to the frozen reference
// over the raw cache, which read past the cache only because it saw the
// concrete type. The reference over a handle reads the stale open row: that
// is the peel R1 names.
func TestOwnerReadsReadPastAStaleCacheThroughAHandle(t *testing.T) {
	const id = "hq-1"
	for wrapName, wrap := range cmdHandleWraps {
		t.Run(wrapName, func(t *testing.T) {
			cache, _ := staleCachedRow(t, id)
			store := wrap(cache)
			topo := storeref.Topology{Work: storeref.Leg{Ref: storeref.WorkRef, Store: store}}

			rawWant, err := ownerRowReadReference(storeref.Topology{Work: storeref.Leg{Ref: storeref.WorkRef, Store: cache}}, id)
			if err != nil || rawWant.Status != "closed" {
				t.Fatalf("the reference over the raw cache = %q, %v; want the backing's closed row", rawWant.Status, err)
			}

			open, assignee, _, err := prevAssignedBeadStatus(topo, id)
			if err != nil || open || assignee != "worker-1" {
				t.Errorf("prevAssignedBeadStatus = open %v assignee %q err %v, want the backing's closed row", open, assignee, err)
			}
			if terminal, err := freshAnchorBeadTerminal(topo, id); err != nil || !terminal {
				t.Errorf("freshAnchorBeadTerminal = %v, %v; want the backing's closed row", terminal, err)
			}

			cfg := &config.City{Workspace: config.Workspace{Name: "c"}}
			legs := workLegsFromCensus(t.TempDir(), cfg, cityWorkLegOf(beads.WorkStore{Store: store}), nil)
			rawLegs := workLegsFromCensus(t.TempDir(), cfg, cityWorkLegOf(beads.WorkStore{Store: cache}), nil)
			want, wantFound, wantErr := readLiveReference(rawLegs, id)
			if b, found, err := legs.readLive(id); err != nil || !found || b.Status != "closed" ||
				b.Status != want.Status || found != wantFound || (wantErr != nil) {
				t.Errorf("readLive = %q found %v err %v, want the backing's closed row, as the reference reads the raw cache (%q found %v err %v)", b.Status, found, err, want.Status, wantFound, wantErr)
			}

			if b, err := liveBeadRead(store, id); err != nil || b.Status != "closed" {
				t.Errorf("liveBeadRead = %q, %v; want the backing's closed row", b.Status, err)
			}
			// A second store whose row also reads closed: the first store wins
			// only if its read went past the cache.
			other := beads.NewMemStoreFrom(0, []beads.Bead{{ID: id, Status: "closed", Title: "row", Type: "task"}}, nil)
			if got := committedCloseOwner([]beads.Store{store, other}, id); got != store {
				t.Errorf("committedCloseOwner did not take the first store, whose backing reads closed")
			}
			if owner, b, err := liveReadOwner([]beads.Store{store}, id); err != nil || b.Status != "closed" || owner != store {
				t.Errorf("liveReadOwner = %q, %v; want the backing's closed row", b.Status, err)
			}
		})
	}
}

// TestLiveReadsAgreesWithTheFrozenReference: liveReads answers every shape
// the frozen type switch peeled exactly as it did (the class handles and the
// v2 effect wrappers, nested), and answers a handle over a v2 wrapper, which
// the switch never named, as the reference answers the wrapper.
func TestLiveReadsAgreesWithTheFrozenReference(t *testing.T) {
	live := beads.NewMemStore()
	b, err := live.Create(beads.Bead{Title: "claimed", Type: "task", Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	cached := liveHandleStore{Store: beads.NewMemStore(), live: live}
	shapes := map[string]beads.Store{
		"the store":                cached,
		"SessionStore":             beads.SessionStore{Store: cached},
		"WorkStore":                beads.WorkStore{Store: cached},
		"readOnlyStore":            readOnlyStore{cached},
		"blindWriteRefusingStore":  blindWriteRefusingStore{inner: cached},
		"all":                      readOnlyStore{blindWriteRefusingStore{inner: beads.WorkStore{Store: beads.SessionStore{Store: cached}}}},
		"a resident":               beadstest.Resident(cached),
		"a work handle over an rw": beadstest.Work(readOnlyStore{cached}),
	}
	for name, store := range shapes {
		got, gotErr := liveBeadRead(store, b.ID)
		if gotErr != nil || got.ID != b.ID {
			t.Errorf("liveBeadRead through %s = %+v, %v; want the live bead", name, got, gotErr)
		}
	}
	// The shapes the switch named: identical answers.
	for _, name := range []string{"the store", "SessionStore", "WorkStore", "readOnlyStore", "blindWriteRefusingStore", "all"} {
		want, wantErr := liveReadsReference(shapes[name]).Get(b.ID)
		got, gotErr := liveReads(shapes[name]).Get(b.ID)
		if got.ID != want.ID || got.Status != want.Status || (gotErr == nil) != (wantErr == nil) {
			t.Errorf("%s: liveReads = %+v, %v; the reference %+v, %v", name, got, gotErr, want, wantErr)
		}
	}
}
