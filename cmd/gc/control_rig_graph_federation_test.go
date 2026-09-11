package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The rig arm of the control-class split.
//
// TestControlDispatchRigScopeStaysOnItsOwnStore pins the other half: a rig scope
// keeps its own store, because `gc storage migrate` copies only the CITY work
// store, so a rig has no frozen retained copies to be misled by. That reasoning
// is sound and unchanged. What it did not cover is that a rig dispatcher's
// control beads are not all rig-resident in the first place.
//
// A workflow materializes its control beads into the ledger that owns the
// graph class, and the class binding is CITY-keyed — one per city, regardless
// of which rig's dispatcher the beads are then routed to. So a rig dispatcher's
// queue is split across two ledgers: the beads its own workflows minted in the
// rig store, and the beads a city-scoped molecule minted in the binding and
// routed to it. Measured on a live city: 148 open control beads carrying
// gc.routed_to=<rig>/core.control-dispatcher sat in the binding while the
// dispatcher's every readiness scan asked only the rig store and got `[]` —
// exit 0, empty array, "no work", forever.
//
// The two sets have DISJOINT ids, which is what makes a union safe rather than
// ambiguous: the rig store was never a migration target, so no id is co-resident
// and no leg can shadow a live copy with a stale one. Verified on the same live
// city — all 20 graph-class control beads carrying the rig's own id prefix were
// absent from the rig store.
//
// The scope store stays the FIRST leg, so every id it holds resolves exactly
// where it resolves today and the rig dispatchers already draining their own
// ledgers are untouched.

// rigFederationFixture stands up a split city with a rig scope, a fake `bd` on
// PATH answering for the rig leg, and a memory-backed city graph binding.
//
// The fake bd is what makes the scope leg observable: the shell arm of the
// fallback reads through it, so a bead in its payload can only have come from
// the scope leg, and a bead in the binding can only have come from the graph
// leg. Without that separation a union test cannot tell a merged answer from a
// single leg that happened to hold both.
//
// GC_CITY is set because that is how a rig-scoped dispatcher actually finds its
// city. tryControlReadyFromCacheOrFallback derives the city from the scope dir
// via cityForStoreDir, which prefers the env over walking up the filesystem —
// and it has to: rigs are routinely OUTSIDE the city tree (on the live city,
// GC_DIR=/data/projects/beads under GC_CITY=/data/projects/maintainer-city), so
// filesystem discovery from the rig dir would find no city at all and the whole
// federation would silently not apply. The .gc marker is what makes the env
// value pass validateCityPath.
func rigFederationFixture(t *testing.T, bdPayload string) (cityPath, rigPath string, binding *beads.MemStore) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)

	cityPath = t.TempDir()
	rigPath = filepath.Join(cityPath, "rigs", "fixture")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir rig path: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir city runtime root: %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	// The payload is baked in, so the fake bd answers every invocation with the
	// rig leg's ready set whatever flags the caller passed.
	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte("#!/bin/sh\nprintf '%s' '"+bdPayload+"'\n"), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")

	return cityPath, rigPath, beads.NewMemStore()
}

func controlLegIDs(items []beads.Bead) []string {
	out := make([]string, 0, len(items))
	for _, b := range items {
		out = append(out, b.ID)
	}
	return out
}

func containsID(items []beads.Bead, id string) bool {
	for _, b := range items {
		if b.ID == id {
			return true
		}
	}
	return false
}

// TestControlReadyFallbackRigScopeFederatesTheCityGraphBinding is the readiness
// half: a rig-scoped scan must see the control beads a city-scoped molecule
// routed to it, not just the ones its own rig minted.
func TestControlReadyFallbackRigScopeFederatesTheCityGraphBinding(t *testing.T) {
	cityPath, rigPath, binding := rigFederationFixture(t, `[{"id":"ga-rig-resident"}]`)
	routed, err := binding.Create(beads.Bead{Title: "check 1", Type: "task"})
	if err != nil {
		t.Fatalf("seed the binding: %v", err)
	}

	// PREMISE. Without the routes seeded there is no second leg at all, so the
	// scan sees only the fake bd's payload. If this control ever returns the
	// binding's bead the test below proves nothing: the id would be reachable
	// through the scope leg and a union would be indistinguishable from today.
	seedCLIStorageRoutes(t, cityPath, nil)
	unsplit, err := controlReadyFallbackReady(rigPath, cityPath, nil, false)
	if err != nil {
		t.Fatalf("premise scan: %v", err)
	}
	if containsID(unsplit, routed.ID) {
		t.Fatalf("premise failed: an unsplit city already returned the binding's bead (%v); "+
			"the scope leg can reach it, so this fixture cannot distinguish a federated answer", controlLegIDs(unsplit))
	}
	if !containsID(unsplit, "ga-rig-resident") {
		t.Fatalf("premise failed: the scope leg did not answer at all (%v); the fake bd is not wired", controlLegIDs(unsplit))
	}

	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))
	got, err := controlReadyFallbackReady(rigPath, cityPath, nil, false)
	if err != nil {
		t.Fatalf("federated scan: %v", err)
	}
	if !containsID(got, routed.ID) {
		t.Errorf("the rig-scoped readiness scan returned %v, missing the binding's control bead; "+
			"every control bead a city-scoped molecule routes to this rig strands unread and the dispatcher idles forever", controlLegIDs(got))
	}
	if !containsID(got, "ga-rig-resident") {
		t.Errorf("the rig-scoped readiness scan returned %v, missing its OWN store's bead; "+
			"the graph leg replaced the scope leg instead of joining it, trading one dead arm for another", controlLegIDs(got))
	}
}

// TestControlReadyFallbackCityScopeStillReadsOnlyTheBinding is the control that
// keeps the city arm from being swept into the union.
//
// A city scope reads the binding INSTEAD of its own store, and that asymmetry is
// load-bearing: `gc storage migrate` retains a frozen copy of every migrated
// control bead in the work ledger, so unioning the two legs there would re-offer
// ids the binding has already finished and the drain loop would never return.
func TestControlReadyFallbackCityScopeStillReadsOnlyTheBinding(t *testing.T) {
	cityPath, _, binding := rigFederationFixture(t, `[{"id":"ga-stale-retained-copy"}]`)
	live, err := binding.Create(beads.Bead{Title: "check 1", Type: "task"})
	if err != nil {
		t.Fatalf("seed the binding: %v", err)
	}
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))

	got, err := controlReadyFallbackReady(cityPath, cityPath, nil, false)
	if err != nil {
		t.Fatalf("city-scoped scan: %v", err)
	}
	if containsID(got, "ga-stale-retained-copy") {
		t.Errorf("the city-scoped scan returned %v, which includes the work ledger's retained copy; "+
			"those are the frozen ids the migration left behind and re-offering them re-enters control kinds the binding already finished", controlLegIDs(got))
	}
	if !containsID(got, live.ID) {
		t.Errorf("the city-scoped scan returned %v, missing the binding's live bead", controlLegIDs(got))
	}
}

// TestControlReadyFallbackSingleStoreRigScopeIsUnchanged pins the invisibility
// rule: a city that relocates no class gets exactly the one leg it always had.
func TestControlReadyFallbackSingleStoreRigScopeIsUnchanged(t *testing.T) {
	cityPath, rigPath, _ := rigFederationFixture(t, `[{"id":"ga-only-leg"}]`)
	seedCLIStorageRoutes(t, cityPath, nil)

	got, err := controlReadyFallbackReady(rigPath, cityPath, nil, false)
	if err != nil {
		t.Fatalf("single-store scan: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ga-only-leg" {
		t.Errorf("single-store rig scan = %v, want exactly [ga-only-leg]; "+
			"a city with no [storage] section must take none of the federation", controlLegIDs(got))
	}
}

// TestControlDispatchRigScopeResolvesAGraphResidentControlBead is the dispatch
// half, and it is the half that makes the readiness half safe to ship.
//
// A federated scan hands the drain loop ids the scope store does not hold. If
// the dispatch still resolved every id against the scope store, each one would
// come back "bead not found" — which IsTransientControllerError does not match,
// so drainWorkflowServeWork returns it as fatal and the dispatcher session exits
// and crash-loops. Enumerating a bead the dispatch cannot then load is strictly
// worse than not enumerating it, so the two must land together.
func TestControlDispatchRigScopeResolvesAGraphResidentControlBead(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "fixture")
	rigStore := beads.NewMemStore()
	binding := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))

	// The workflow root is canceled and lives ONLY in the binding, so the
	// dispatcher's disposition is a direct readout of which ledger it read:
	// root found and canceled means gc.outcome=canceled, root absent means
	// gc.final_disposition=orphaned_workflow.
	root, err := binding.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.CancelRequestedMetadataKey: "operator",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	control := newControlBead(t, binding, root.ID)

	// PREMISE: the rig store does not hold this id. Without this the dispatch
	// could be resolving it locally and the assertions below would pass for the
	// wrong reason.
	if _, err := rigStore.Get(control.ID); err == nil {
		t.Fatalf("premise failed: the rig store already holds %s, so this test cannot show the graph leg was consulted", control.ID)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, rigPath, rigStore, control.ID, cfg, &stdout, &stderr); err != nil {
		t.Fatalf("rig-scoped dispatch of a graph-resident control bead: %v; "+
			"a federated readiness scan enumerates these ids, so a fatal not-found here crash-loops the dispatcher session", err)
	}

	got := beadByID(t, binding, control.ID)
	if got.Status != "closed" {
		t.Errorf("graph-resident control bead status = %q, want closed in the binding; the dispatch wrote somewhere else", got.Status)
	}
	if outcome := got.Metadata[beadmeta.OutcomeMetadataKey]; outcome != beadmeta.OutcomeCanceled {
		t.Errorf("gc.outcome = %q, want %q; the dispatcher resolved the root somewhere other than the binding",
			outcome, beadmeta.OutcomeCanceled)
	}
}

// TestRunControlDispatcherResolvesACityGraphBindingResidentControlBead is the
// MANUAL twin of the serve-path dispatch test above, and it closes the operator
// half of the same gap.
//
// The automated serve loop reaches a binding-resident control bead because it
// scans every scope and federates the city graph binding as an extra leg. The
// manual `gc convoy control <id>` entry point instead resolves a single id
// through findBeadScopeAcrossStores, which probes only the work store and the rig
// control stores — never the binding. On a [beads.classes.graph]-relocated city a
// city-scoped molecule materializes its graph-class control beads into that
// binding, so the operator's recovery command returned `loading bead <id>: ...
// not found` for exactly the class of beads this routing exists to serve, and
// precisely when the automated path is wedged and an operator reaches for the
// manual one.
//
// The canceled workflow root lives ONLY in the binding, so the dispatcher's
// disposition is a direct readout of which ledger it resolved the bead from: a
// canceled root closes the control bead gc.outcome=canceled; a missing root
// closes it gc.final_disposition=orphaned_workflow.
func TestRunControlDispatcherResolvesACityGraphBindingResidentControlBead(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	binding := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))

	root, err := binding.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.CancelRequestedMetadataKey: "operator",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	control := newControlBead(t, binding, root.ID)

	// PREMISE: the city work store does not hold this id, so a pass can only come
	// from consulting the binding rather than from a local resolve.
	workStore, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	if _, err := workStore.Get(control.ID); err == nil {
		t.Fatalf("premise failed: the city work store already holds %s, so this test cannot show the binding was consulted", control.ID)
	}

	var stdout, stderr bytes.Buffer
	if err := runControlDispatcher(control.ID, &stdout, &stderr); err != nil {
		t.Fatalf("manual dispatch of a binding-resident control bead: %v; "+
			"`gc convoy control <id>` cannot reach a graph-class control bead the serve loop already federates", err)
	}

	got := beadByID(t, binding, control.ID)
	if got.Status != "closed" {
		t.Errorf("binding-resident control bead status = %q, want closed in the binding; the manual dispatch wrote somewhere else", got.Status)
	}
	if outcome := got.Metadata[beadmeta.OutcomeMetadataKey]; outcome != beadmeta.OutcomeCanceled {
		t.Errorf("gc.outcome = %q, want %q; the manual dispatch resolved the root somewhere other than the binding",
			outcome, beadmeta.OutcomeCanceled)
	}
}

// TestFindBeadScopeAcrossStoresRoutesABindingResidentRigBeadToItsRigScope pins
// the WORK leg the manual entry point must keep for a rig-routed binding-resident
// control bead.
//
// controlBeadLedger keeps the scope store FIRST and consults the binding as an
// ADDITIONAL leg, so the scope findBeadScopeAcrossStores returns becomes the work
// leg — the ledger that owns the input convoy an execution snapshot reads. A
// city-scoped molecule stamps gc.routed_to=<rig>/core.control-dispatcher on the
// control beads it materializes into the binding and hands to a rig; the input
// convoy those beads name lives in that RIG's store, not the city's. Resolving
// such a bead to the city scope would satisfy the graph read (the binding
// federates either way) but strand the input convoy, so residence must be derived
// from the bead's route rather than defaulted to the city.
func TestFindBeadScopeAcrossStoresRoutesABindingResidentRigBeadToItsRigScope(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "fixture")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir rig path: %v", err)
	}
	cityTOML := "[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n" +
		"[[rigs]]\nname = \"fixture\"\npath = \"" + rigPath + "\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	binding := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))

	root, err := binding.Create(beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	control := newControlBead(t, binding, root.ID)
	// The live stranded beads carry gc.routed_to=<rig>/core.control-dispatcher;
	// gc.run_target (what newRoutedControlBead sets) is a different key the
	// execution-route resolver does not read, so route the bead explicitly.
	if err := binding.Update(control.ID, beads.UpdateOpts{
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "fixture/core.control-dispatcher"},
	}); err != nil {
		t.Fatalf("route control bead to the rig: %v", err)
	}

	// PREMISE: neither the city work store nor the rig control store holds the id,
	// so the scope can only have come from consulting the binding.
	cityStore, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	if _, err := cityStore.Get(control.ID); err == nil {
		t.Fatalf("premise failed: the city work store holds %s", control.ID)
	}
	rigStore, err := openStoreAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	if _, err := rigStore.Get(control.ID); err == nil {
		t.Fatalf("premise failed: the rig store holds %s", control.ID)
	}

	_, storePath, err := findBeadScopeAcrossStores(cityPath, control.ID, io.Discard)
	if err != nil {
		t.Fatalf("findBeadScopeAcrossStores for a rig-routed binding-resident control bead: %v; "+
			"the manual entry point never consults the city graph binding", err)
	}
	if filepath.Clean(storePath) != filepath.Clean(rigPath) {
		t.Errorf("resolved scope path = %q, want the rig path %q; a rig-routed binding-resident bead must keep its rig work leg, "+
			"or its input convoy is stranded on the wrong store", storePath, rigPath)
	}
}

// TestControlBeadLedgerResolvesTheGraphLegWithoutMovingTheWorkLeg pins the
// asymmetry that makes the rig arm correct, and is the reason the dispatch
// resolves residence rather than re-entering the whole run at the city scope.
//
// The binding is city-keyed for the GRAPH class only; the work class is not
// relocated at all. So a rig scope lands on (rig work store, city graph
// binding) — the same shape as the city arm's (city work store, city graph
// binding), with only the graph leg moving.
//
// Measured on the live city: the stranded control beads and their workflow root
// are `gcg-` and binding-resident, while the input convoy those same beads name
// in gc.input_convoy_id is `ga-xz2hu` — absent from the binding, present in the
// rig store. Moving the work leg to the city would fix the graph read and break
// EmitCurrent, which reads that convoy's tracks edges from the work leg.
func TestControlBeadLedgerResolvesTheGraphLegWithoutMovingTheWorkLeg(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "fixture")
	rigStore := beads.NewMemStore()
	// MemStore assigns its own sequential ids and each store counts
	// independently, so a bead in each would land on the SAME id and the scope
	// leg would legitimately shadow the binding. Offset the binding's sequence so
	// the two ledgers are disjoint, which is what residence is on the live city.
	binding := beads.NewMemStoreFrom(100, nil, nil)
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	local, err := rigStore.Create(beads.Bead{Title: "rig-resident", Type: "task"})
	if err != nil {
		t.Fatalf("create rig bead: %v", err)
	}
	resident, err := binding.Create(beads.Bead{Title: "binding-resident", Type: "task"})
	if err != nil {
		t.Fatalf("create binding bead: %v", err)
	}

	// PREMISE: the two ledgers hold DISJOINT ids. Without this the assertions
	// below cannot distinguish "resolved the binding" from "found a same-id bead
	// in the rig store first".
	if _, err := rigStore.Get(resident.ID); err == nil {
		t.Fatalf("premise failed: the rig store also holds %s, so this test cannot show which leg answered", resident.ID)
	}
	if _, err := binding.Get(local.ID); err == nil {
		t.Fatalf("premise failed: the binding also holds %s, so the leg-order assertion below is vacuous", local.ID)
	}

	gotStore, gotBead, err := controlBeadLedger(cityPath, rigPath, cfg, rigStore, resident.ID)
	if err != nil {
		t.Fatalf("controlBeadLedger for a binding-resident id: %v", err)
	}
	if gotBead.ID != resident.ID {
		t.Errorf("resolved bead = %q, want %q", gotBead.ID, resident.ID)
	}
	// Identity is the wrong assertion here — the routes layer hands back an
	// equivalent handle, not the seeded pointer. Prove it is the binding by
	// writing through it and reading the result out of the binding.
	probe, err := gotStore.Create(beads.Bead{Title: "probe", Type: "task"})
	if err != nil {
		t.Fatalf("write through the resolved graph leg: %v", err)
	}
	if _, err := binding.Get(probe.ID); err != nil {
		t.Errorf("wrote %s through the resolved graph leg but the city graph binding does not hold it (%v); "+
			"the dispatch would gate and mutate a ledger nothing else reads", probe.ID, err)
	}
	if _, err := rigStore.Get(probe.ID); err == nil {
		t.Errorf("the resolved graph leg wrote %s into the rig's own store, want the city graph binding", probe.ID)
	}

	gotStore, gotBead, err = controlBeadLedger(cityPath, rigPath, cfg, rigStore, local.ID)
	if err != nil {
		t.Fatalf("controlBeadLedger for a rig-resident id: %v", err)
	}
	if gotStore != beads.Store(rigStore) {
		t.Errorf("graph leg for a rig-resident id = %T, want the rig's own store; the scope store must stay the first leg", gotStore)
	}
	if gotBead.ID != local.ID {
		t.Errorf("resolved bead = %q, want %q", gotBead.ID, local.ID)
	}
}

// TestControlDispatchRigScopePrefersItsOwnStore pins leg ORDER.
//
// The scope store is the first leg. On the live city the two ledgers hold
// disjoint ids so order cannot change an answer, but that disjointness is a
// property of today's migration scope (rig stores are never migration targets),
// not an invariant anything enforces. Pinning the order means a future migration
// that does produce a co-resident id degrades to "the rig's own copy wins" —
// exactly what a rig dispatcher does today — rather than silently flipping every
// rig-scoped dispatch onto the binding.
func TestControlDispatchRigScopePrefersItsOwnStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "fixture")
	rigStore := beads.NewMemStore()
	binding := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))

	root, err := rigStore.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.CancelRequestedMetadataKey: "operator",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	control := newControlBead(t, rigStore, root.ID)

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, rigPath, rigStore, control.ID, cfg, &stdout, &stderr); err != nil {
		t.Fatalf("rig-scoped dispatch of a rig-resident control bead: %v", err)
	}
	if got := beadByID(t, rigStore, control.ID); got.Status != "closed" {
		t.Errorf("rig control bead status = %q, want closed in the rig's own store", got.Status)
	}
	empty, err := binding.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("list city binding: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("city binding holds %d bead(s) after a rig-resident dispatch, want 0; "+
			"the graph leg took precedence over the rig's own store", len(empty))
	}
}

// TestControlGraphExtraLegRoutingRule is the routing rule itself, stated once
// for every scope shape, so the cache leg and the fallback leg cannot drift
// apart the way control dispatch and convergence did.
func TestControlGraphExtraLegRoutingRule(t *testing.T) {
	binding := beads.NewMemStore()

	t.Run("split rig scope gains the binding as a second leg", func(t *testing.T) {
		cityPath := t.TempDir()
		seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))
		got, federated := controlGraphExtraLeg(cityPath, filepath.Join(cityPath, "rigs", "ra"))
		if !federated || !sameStorePtr(got, binding) {
			t.Errorf("controlGraphExtraLeg for a split rig scope = (%v, %v), want the city binding; "+
				"the control beads a city molecule routes to this rig are unreachable without it", got, federated)
		}
	})

	t.Run("split city scope takes no extra leg", func(t *testing.T) {
		cityPath := t.TempDir()
		seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))
		if got, federated := controlGraphExtraLeg(cityPath, cityPath); federated {
			t.Errorf("controlGraphExtraLeg for a split city scope = (%v, true), want no extra leg; "+
				"the city reads the binding INSTEAD of its store, and unioning in the retained copies re-offers finished ids", got)
		}
	})

	t.Run("single-store rig scope takes no extra leg", func(t *testing.T) {
		cityPath := t.TempDir()
		seedCLIStorageRoutes(t, cityPath, nil)
		if got, federated := controlGraphExtraLeg(cityPath, filepath.Join(cityPath, "rigs", "ra")); federated {
			t.Errorf("controlGraphExtraLeg on a city that relocates nothing = (%v, true), want no extra leg; "+
				"this whole change must be invisible to a city with no [storage] section", got)
		}
	})
}

// TestControlGraphExtraLegSkipsARefusingBinding keeps a storage refusal from
// being converted into a per-tick failure of the whole readiness scan.
//
// A refusal is this build's standing verdict about the CITY, not a fault in any
// one read: it says the [storage] shape is one this binary cannot resolve, and a
// refused city still serves work from its work ledger. Federating that verdict
// in as a second leg would make every leg-fails-loud path in
// controlReadyFallbackReady return it, so a rig dispatcher that reads its own
// ledger perfectly well would stop scanning at all — a strictly worse outcome
// than the pre-federation behavior on the same city.
func TestControlGraphExtraLegSkipsARefusingBinding(t *testing.T) {
	refusal := errors.New("unsupported [storage] shape")

	t.Run("a refusing binding is not federated", func(t *testing.T) {
		cityPath := t.TempDir()
		seedCLIStorageRoutes(t, cityPath, refusingStorageRoutes("infra", refusal))
		if got, federated := controlGraphExtraLeg(cityPath, filepath.Join(cityPath, "rigs", "ra")); federated {
			t.Errorf("controlGraphExtraLeg on a refused city = (%v, true), want no extra leg; "+
				"every readiness scan on this rig now fails on a verdict about the city instead of reading the ledger it can reach", got)
		}
	})

	// The control. It must fail DIFFERENTLY: same call, same scope shape, a
	// binding that is merely relocated rather than refused. Without it the
	// negative above would also pass if controlGraphExtraLeg had simply stopped
	// federating anything.
	t.Run("control: a resolvable binding on the same shape still federates", func(t *testing.T) {
		cityPath := t.TempDir()
		binding := beads.NewMemStore()
		seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))
		got, federated := controlGraphExtraLeg(cityPath, filepath.Join(cityPath, "rigs", "ra"))
		if !federated || !sameStorePtr(got, binding) {
			t.Errorf("controlGraphExtraLeg for a resolvable split rig scope = (%v, %v), want the binding; "+
				"the refusal skip above is vacuous if nothing federates", got, federated)
		}
	})
}

// TestControlReadyCacheSourcesRouteTheSameLegsAsTheFallback pins the two scan
// arms to one routing rule.
//
// The serve loop takes the CACHE arm first on every tick; the fallback runs only
// when a leg is dirty, still priming, or the bd compatibility mode needs a tier
// CachedReady cannot serve. So the cache arm is the production path, and the two
// agreeing about WHICH ledgers hold this scope's queue is the whole safety
// property: a queue drawn from a different set of stores than the dispatch
// mutates is exactly the divergence this defect was.
//
// Asserted structurally — leg count and the binding's position — rather than by
// comparing scan output, because the two arms legitimately return different bead
// values (one snapshots, one reads live) and only their ROUTING has to match.
func TestControlReadyCacheSourcesRouteTheSameLegsAsTheFallback(t *testing.T) {
	t.Run("split rig scope snapshots its own store AND the binding", func(t *testing.T) {
		cityPath, rigPath, binding := rigFederationFixture(t, `[]`)
		seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))

		sources, err := controlReadyCacheSources(rigPath, cityPath, nil)
		if err != nil {
			t.Fatalf("controlReadyCacheSources for a split rig scope: %v", err)
		}
		if len(sources) != 2 {
			t.Fatalf("cache sources for a split rig scope = %d leg(s), want 2; "+
				"the cached arm reads a narrower set than the fallback and the queue flips with cache freshness", len(sources))
		}
		if sameStorePtr(sources[0], binding) {
			t.Errorf("cache leg[0] is the binding, want the scope's own store first; leg order decides which copy wins on a co-resident id")
		}
		if !sameStorePtr(sources[1], binding) {
			t.Errorf("cache leg[1] = %T, want the city graph binding; the beads a city molecule routed to this rig stay unread on the cached arm", sources[1])
		}
	})

	t.Run("split city scope snapshots only the binding", func(t *testing.T) {
		cityPath, _, binding := rigFederationFixture(t, `[]`)
		seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))

		sources, err := controlReadyCacheSources(cityPath, cityPath, nil)
		if err != nil {
			t.Fatalf("controlReadyCacheSources for a split city scope: %v", err)
		}
		if len(sources) != 1 || !sameStorePtr(sources[0], binding) {
			t.Fatalf("cache sources for a split city scope = %d leg(s), want exactly the binding; "+
				"unioning the work store back in re-offers the frozen copies the migration retained", len(sources))
		}
	})

	t.Run("single-store rig scope snapshots one store", func(t *testing.T) {
		cityPath, rigPath, binding := rigFederationFixture(t, `[]`)
		seedCLIStorageRoutes(t, cityPath, nil)

		sources, err := controlReadyCacheSources(rigPath, cityPath, nil)
		if err != nil {
			t.Fatalf("controlReadyCacheSources for a single-store rig scope: %v", err)
		}
		if len(sources) != 1 {
			t.Fatalf("cache sources for a single-store rig scope = %d leg(s), want 1; "+
				"a city with no [storage] section must take none of the federation", len(sources))
		}
		if sameStorePtr(sources[0], binding) {
			t.Errorf("the single leg is the unrouted binding, want the scope's own store")
		}
	})
}

// TestCachedControlReadyUnionRequiresEveryLeg pins all-or-nothing.
//
// A federated scope's queue is only complete if every leg answered. Returning
// the warm legs' beads when one is cold would be indistinguishable from a
// complete answer at the call site — the fallback would never run, and the scan
// would silently report a short queue as the whole queue. That is the same
// "exit 0, empty array, no work" shape as the defect this federation fixes, just
// intermittent, which is worse.
func TestCachedControlReadyUnionRequiresEveryLeg(t *testing.T) {
	newPrimed := func(t *testing.T, title string) (*beads.CachingStore, string) {
		t.Helper()
		store := beads.NewMemStore()
		bead, err := store.Create(beads.Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
		cache := beads.NewCachingStore(store, nil)
		if err := cache.PrimeActive(); err != nil {
			t.Fatalf("prime %s: %v", title, err)
		}
		return cache, bead.ID
	}

	warm, warmID := newPrimed(t, "warm leg")
	other, otherID := newPrimed(t, "second warm leg")
	cold := beads.NewCachingStore(beads.NewMemStore(), nil)

	// PREMISE: an unprimed cache really is cold, so the negative below is about
	// the union's policy and not about a cache that happens to answer anyway.
	if _, ok := cold.CachedReady(); ok {
		t.Fatal("premise failed: an unprimed CachingStore answered CachedReady; this test cannot show a cold leg forces the fallback")
	}

	if got, ok := cachedControlReadyUnion([]*beads.CachingStore{warm, cold}); ok {
		t.Errorf("cachedControlReadyUnion with one cold leg = (%d bead(s), true), want a miss; "+
			"a partial union reads as a complete queue and the fallback never runs", len(got))
	}
	if got, ok := cachedControlReadyUnion([]*beads.CachingStore{cold, warm}); ok {
		t.Errorf("cachedControlReadyUnion with the cold leg FIRST = (%d bead(s), true), want a miss; "+
			"leg order must not decide whether a short answer escapes", len(got))
	}

	// The control: every leg warm unions, so the miss above is the cold leg and
	// not the union refusing to merge at all.
	got, ok := cachedControlReadyUnion([]*beads.CachingStore{warm, other})
	if !ok {
		t.Fatal("cachedControlReadyUnion with every leg warm missed; the cached arm can never answer a federated scope and every tick pays the fallback")
	}
	if !containsID(got, warmID) || !containsID(got, otherID) {
		t.Errorf("warm union = %v, want both %s and %s; one leg's beads were dropped", controlLegIDs(got), warmID, otherID)
	}
}

// TestControlReadyScanRigScopeFederatesTheBindingOnBothArms is the end-to-end
// producer assertion, taken through the exact entry point the serve loop calls.
//
// The three fallback tests above call controlReadyFallbackReady directly, which
// leaves the production path — nextWorkflowServeBeads ->
// tryControlReadyFromCacheOrFallback -> the cache arm — unexercised for a
// federated scope. This runs the real entry for both arms against one fixture,
// so cache/fallback parity is pinned on OUTPUT and not only on routing.
func TestControlReadyScanRigScopeFederatesTheBindingOnBothArms(t *testing.T) {
	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName}
	route := agentCfg.QualifiedName()
	rigResident := `[{"id":"ga-rig-resident","title":"rig check","issue_type":"task","status":"open",` +
		`"metadata":{"` + beadmeta.RunTargetMetadataKey + `":"` + route + `"}}]`

	for _, tc := range []struct {
		name  string
		beads config.BeadsConfig
	}{
		{name: "cached arm", beads: config.BeadsConfig{}},
		{name: "fallback arm", beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, rigPath, binding := rigFederationFixture(t, rigResident)
			seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))

			root, err := binding.Create(beads.Bead{
				Title:    "workflow",
				Type:     "task",
				Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
			})
			if err != nil {
				t.Fatalf("create workflow root: %v", err)
			}
			routed := newRoutedControlBead(t, binding, root.ID, route)

			got := controlReadyScan(t, rigPath, agentCfg, tc.beads)
			if !slices.Contains(got, routed.ID) {
				t.Errorf("rig-scoped control-ready queue = %v, missing the binding's control bead %s; "+
					"this is the live defect — 148 beads routed here sat unread while the dispatcher reported idle", got, routed.ID)
			}
			if !slices.Contains(got, "ga-rig-resident") {
				t.Errorf("rig-scoped control-ready queue = %v, missing the rig's OWN control bead; "+
					"the binding replaced the scope leg instead of joining it", got)
			}
		})
	}
}

// TestControlReadyScanRigScopeStopsOfferingABeadTheDispatchClosed is the
// termination property at a rig scope, and it is the assertion that makes the
// federated queue safe rather than merely fuller.
//
// drainWorkflowServeWork loops until its queue comes back empty, with no
// iteration bound and no sleep between passes. Adding a leg to the scan without
// the dispatch resolving that same leg would produce ids the dispatch cannot
// find (fatal, crash-loop) or ids it closes in a different ledger than the scan
// reads (a queue that never empties, a loop that never returns). The fixed point
// below is what rules both out: scan, dispatch, scan again, empty.
func TestControlReadyScanRigScopeStopsOfferingABeadTheDispatchClosed(t *testing.T) {
	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName}
	route := agentCfg.QualifiedName()

	cityPath, rigPath, binding := rigFederationFixture(t, `[]`)
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))
	rigStore := beads.NewMemStore()

	root, err := binding.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.CancelRequestedMetadataKey: "operator",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	live := newRoutedControlBead(t, binding, root.ID, route)

	if got := controlReadyScan(t, rigPath, agentCfg, config.BeadsConfig{}); !slices.Contains(got, live.ID) {
		t.Fatalf("control-ready queue before dispatch = %v, want it to contain %s", got, live.ID)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, rigPath, rigStore, live.ID, cfg, &stdout, &stderr); err != nil {
		t.Fatalf("rig-scoped control dispatch: %v", err)
	}
	if closed := beadByID(t, binding, live.ID); closed.Status != "closed" {
		t.Fatalf("binding control bead status = %q, want closed; the dispatch wrote a ledger the scan does not read", closed.Status)
	}

	if got := controlReadyScan(t, rigPath, agentCfg, config.BeadsConfig{}); len(got) != 0 {
		t.Fatalf("control-ready queue after dispatch = %v, want empty; "+
			"the serve loop re-offers a bead the dispatch already finished and drainWorkflowServeWork never returns", got)
	}
}
