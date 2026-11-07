package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// TestSlingDepsHandlesWrapTheStoresTheyReplace pins the `gc sling` deps
// (typed-stores T6d): the work handle cliStores mints over the scope store the
// sling opened wraps that store, and its graph handle wraps the store
// resolveGraphStore returned for it, by identity, at city and rig scope, on a
// city that relocates nothing and on a split one. On the first the graph store
// is the scope store itself; on the second it is the binding.
func TestSlingDepsHandlesWrapTheStoresTheyReplace(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "single-store"
		bindingRoot := ""
		if split {
			name = "split"
			bindingRoot = filepath.Join(t.TempDir(), "store")
		}
		t.Run(name, func(t *testing.T) {
			cityPath := oneShotCLICity(t, bindingRoot)
			city, err := openStoreAtForCity(cityPath, cityPath)
			if err != nil {
				t.Fatalf("open the city store: %v", err)
			}
			binding, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassGraph)
			if relocated != split {
				t.Fatalf("graph relocated = %t, want %t: the fixture is not the shape it names", relocated, split)
			}
			for scope, store := range map[string]beads.Store{"city": city, "rig": beads.NewMemStore()} {
				cs := cliStores(cityPath, store)
				if got, want := cs.Work().Store, splitStrictWorkView(store); got != want {
					t.Errorf("%s scope: work handle wraps %T(%p), want the scope store %T(%p)", scope, got, got, want, want)
				}
				oldGraph := resolveGraphStore(cliStorageRoutes(cityPath), store, nil, cityPath, nil)
				if got := cs.Graph().Store; got != oldGraph {
					t.Errorf("%s scope: graph handle wraps %T(%p), want resolveGraphStore's %T(%p)", scope, got, got, oldGraph, oldGraph)
				}
				if got, want := beads.SameStore(cs.Graph(), binding), split; got != want {
					t.Errorf("%s scope: graph handle over the binding = %t, want %t", scope, got, want)
				}
			}
		})
	}
}

// TestCmdSlingConvoyRoutesItsTracksOnlyMembers pins R10 at the CLI door: `gc
// sling` hands the batch its work store as the querier, so a convoy's members
// are read through convoy.MembersIn, which follows its tracks edges. A member a
// convoy tracks but does not parent is routed; through a querier's DB children
// alone it would not be seen.
func TestCmdSlingConvoyRoutesItsTracksOnlyMembers(t *testing.T) {
	cityDir, rigDir := setupCmdSlingMultiDefaultTargetsFixture(t, nil)
	writeTestFileStoreBeads(t, rigDir, []beads.Bead{
		{ID: "fo-cvy", Title: "convoy", Type: "convoy", Status: "open", Metadata: map[string]string{}},
		{ID: "fo-member", Title: "tracked member", Type: "task", Status: "open", Metadata: map[string]string{}},
	})
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open the rig store: %v", err)
	}
	if err := rigStore.DepAdd("fo-cvy", "fo-member", "tracks"); err != nil {
		t.Fatalf("tracking the member: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSling(
		[]string{"foundations/worker-a", "fo-cvy"},
		false, false, false,
		"", nil, "",
		true, false, false, "",
		false, false, false,
		"", "",
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("gc sling of a convoy = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	rigStore, err = openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("reopen the rig store: %v", err)
	}
	member, err := rigStore.Get("fo-member")
	if err != nil {
		t.Fatalf("Get(fo-member): %v", err)
	}
	if got := member.Metadata[beadmeta.RoutedToMetadataKey]; got != "foundations/worker-a" {
		t.Errorf("tracks-only member gc.routed_to = %q, want foundations/worker-a; stdout=%s", got, stdout.String())
	}
}

// TestCheckBeadStateWithZeroDepsDoesNotPanic pins checkBeadState's zero
// SlingDeps (R11): its work, graph and sessions handles are zero, and a zero
// handle boxed into an interface is non-nil and panics on its first read. A
// routed bead, claimed by a session bead id, reaches the pool-claim and
// convoy-recovery reads, which must skip the absent stores.
func TestCheckBeadStateWithZeroDepsDoesNotPanic(t *testing.T) {
	store := beads.NewMemStore()
	pool := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(3)}
	b, err := store.Create(beads.Bead{
		Title:    "claimed",
		Assignee: "gc-session-1",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "polecat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("checkBeadState with zero deps panicked: %v", r)
		}
	}()
	result := checkBeadState(store, b.ID, pool)
	if result.Idempotent {
		t.Errorf("checkBeadState = %+v: with no sessions store the claimant is not recognized as the pool's own", result)
	}
}
