package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
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
	// Unclaimed routed work reaches the convoy-recovery read of the work store.
	unclaimed, err := store.Create(beads.Bead{Title: "routed", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "polecat"}})
	if err != nil {
		t.Fatal(err)
	}
	if result := checkBeadState(store, unclaimed.ID, pool); result.Idempotent {
		t.Errorf("checkBeadState = %+v: with no work store no convoy tracks the work, so finalize must re-run", result)
	}
}

// TestCLIBeadRouterWithNoStoreRefuses pins cliBeadRouter's absent store: with
// no store named by the request and a zero work store it refuses instead of
// writing through a zero handle.
func TestCLIBeadRouterWithNoStoreRefuses(t *testing.T) {
	err := cliBeadRouter{deps: &slingDeps{}}.Route(t.Context(), sling.RouteRequest{BeadID: "gc-1", Target: "worker"})
	if err == nil || !strings.Contains(err.Error(), "requires a store") {
		t.Fatalf("Route with no store = %v, want the requires-a-store refusal", err)
	}
}

// TestCmdSlingRigScopeReslingReadsTheClaimantInTheCityStore pins the `gc
// sling` door's sessions store at rig scope (OD-3c): a rig store holds no
// session beads, so a rig-scope sling reads them in the city store it opens.
// A re-sling of rig work its own pool session claimed under its session bead
// id is then the idempotent no-op it is at city scope, on a city that
// relocates nothing; read through the rig store the claimant was missed and
// the work was routed again beside the session holding it.
func TestCmdSlingRigScopeReslingReadsTheClaimantInTheCityStore(t *testing.T) {
	cityDir, rigDir := setupCmdSlingMultiDefaultTargetsFixture(t, nil)
	cityToml := "[workspace]\nname = \"demo\"\n\n[[rigs]]\nname = \"foundations\"\npath = \"foundations\"\nprefix = \"fo\"\n\n[[agent]]\nname = \"worker-a\"\ndir = \"foundations\"\nmax_active_sessions = 3\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	writeTestFileStoreBeads(t, cityDir, []beads.Bead{{
		ID:       "gc-sess-1",
		Title:    "pool session",
		Type:     session.BeadType,
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"template": "foundations/worker-a", "pool_managed": "true"},
	}})
	writeTestFileStoreBeads(t, rigDir, []beads.Bead{
		{ID: "fo-held", Title: "held work", Type: "task", Status: "in_progress", Assignee: "gc-sess-1", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "foundations/worker-a"}},
		{ID: "fo-cvy", Title: sling.AutoConvoyRootTitle("fo-held"), Type: "convoy", Status: "open", Metadata: map[string]string{}},
	})
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open the rig store: %v", err)
	}
	if err := rigStore.DepAdd("fo-cvy", "fo-held", "tracks"); err != nil {
		t.Fatalf("tracking the held work: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSling(
		[]string{"foundations/worker-a", "fo-held"},
		false, false, false,
		"", nil, "",
		true, false, false, "",
		false, false, false,
		"", "",
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("gc sling = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "already routed") || strings.Contains(stderr.String(), "assigned to") {
		t.Errorf("re-sling of work pool session gc-sess-1 holds: stdout=%q stderr=%q, want an idempotent skip", stdout.String(), stderr.String())
	}
}
