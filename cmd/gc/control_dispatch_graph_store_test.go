package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// seedCLIStorageRoutes installs routes for cityPath in the one-shot memo, so a
// test can drive the CLI class resolvers without standing up a real binding.
func seedCLIStorageRoutes(t *testing.T, cityPath string, routes *storageRoutes) {
	t.Helper()
	resetCLIStorageRoutes(t)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() { entry.routes = routes })
}

// newOrphanControlFixture writes a control bead whose workflow root was
// canceled. Both are graph class. The returned ids are identical in whichever
// store they are written to, so the same fixture models the live binding copy
// and the stale copy the migration retained in the work ledger.
func newControlBead(t *testing.T, store beads.Store, rootID string) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		Title: "check 1",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:          "check",
			beadmeta.RootBeadIDMetadataKey:    rootID,
			beadmeta.RootStoreRefMetadataKey:  "city:test-city",
			beadmeta.StepIDMetadataKey:        "implement",
			beadmeta.CheckModeMetadataKey:     "exec",
			beadmeta.MaxAttemptsMetadataKey:   "1",
			beadmeta.LogicalBeadIDMetadataKey: "logical-1",
		},
	})
	if err != nil {
		t.Fatalf("create control bead: %v", err)
	}
	return bead
}

func beadByID(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}

// TestControlDispatchReadsAndWritesTheGraphStoreOnSplitCity pins both halves of
// the control-dispatch class hop on a split city.
//
// The fixture is the shape a converged city actually has: the migration COPIES
// infrastructure into the binding and retains the source, so the same control
// bead id exists in both stores. Only the binding's copy is live.
//
// Both directions are asserted, and the disposition is what separates them.
// The dispatcher closes a control bead differently depending on where it finds
// the workflow root: root present and canceled means gc.outcome=canceled, root
// absent means gc.final_disposition=orphaned_workflow. Writing the canceled root
// ONLY into the graph store therefore makes the outcome a direct readout of
// which store the dispatcher READ. Which copy ends up closed is the readout of
// which store it WROTE.
func TestControlDispatchReadsAndWritesTheGraphStoreOnSplitCity(t *testing.T) {
	cityPath := t.TempDir()
	scopeStore := beads.NewMemStore()
	graphStore := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graphStore))

	// Both stores hold the workflow root, because the migration copied it and
	// retained the source. Only the BINDING's copy carries the cancellation:
	// the operator canceled after cutover, through the graph-routed API, so the
	// retained source still holds the pre-cutover version.
	staleRoot, err := scopeStore.Create(beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatalf("create retained workflow root: %v", err)
	}
	root, err := graphStore.Create(beads.Bead{
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
	if staleRoot.ID != root.ID {
		t.Fatalf("fixture root ids diverged (%s vs %s)", staleRoot.ID, root.ID)
	}
	stale := newControlBead(t, scopeStore, root.ID)
	live := newControlBead(t, graphStore, root.ID)
	if live.ID != stale.ID {
		t.Fatalf("fixture ids diverged (%s vs %s); the retained copy must share the binding copy's id", live.ID, stale.ID)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, cityPath, scopeStore, live.ID, cfg, &stdout, &stderr); err != nil {
		t.Fatalf("control dispatch: %v", err)
	}

	// READ: the root was resolved from the graph store, so the control bead was
	// closed as canceled rather than as an orphan of a missing root.
	got := beadByID(t, graphStore, live.ID)
	if disposition := got.Metadata[beadmeta.FinalDispositionMetadataKey]; disposition == beadmeta.DispositionOrphanedWorkflow {
		t.Fatalf("control bead closed as %q; the dispatcher read the scope store, where the canceled root does not exist", disposition)
	}
	if outcome := got.Metadata[beadmeta.OutcomeMetadataKey]; outcome != beadmeta.OutcomeCanceled {
		t.Fatalf("gc.outcome = %q, want %q from the canceled root in the graph binding", outcome, beadmeta.OutcomeCanceled)
	}

	// WRITE: the binding's copy advanced.
	if got.Status != "closed" {
		t.Fatalf("graph-store control bead status = %q, want closed; the dispatcher's mutation went somewhere else", got.Status)
	}

	// And the retained source did NOT: a write there is invisible to every
	// graph-routed reader and becomes a strand at the next boot.
	if stayed := beadByID(t, scopeStore, stale.ID); stayed.Status == "closed" {
		t.Fatalf("retained work-store copy was closed too; control mutations must not land in the source the migration left behind")
	}
}

// TestControlDispatchSingleStoreUsesTheOneStore is the compatibility guarantee.
// A city that relocates nothing routes nothing, so control dispatch runs against
// the exact scope store it always did — same instance, so the bd command runner,
// the scope issue prefix and the optional-capability assertion the scope-skip
// paths make (DepListBatch) all still land on the store they used to.
//
// Green before and after by design; its teeth are proven by mutation.
func TestControlDispatchSingleStoreUsesTheOneStore(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, nil)

	if got := controlGraphStore(cityPath, cityPath, nil, store); got != beads.Store(store) {
		t.Fatalf("controlGraphStore returned %T(%p), want the identical scope store %p", got, got, store)
	}

	root, err := store.Create(beads.Bead{
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
	control := newControlBead(t, store, root.ID)

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, cityPath, store, control.ID, cfg, &stdout, &stderr); err != nil {
		t.Fatalf("control dispatch: %v", err)
	}
	got := beadByID(t, store, control.ID)
	if got.Status != "closed" {
		t.Fatalf("control bead status = %q, want closed in the single store", got.Status)
	}
	if outcome := got.Metadata[beadmeta.OutcomeMetadataKey]; outcome != beadmeta.OutcomeCanceled {
		t.Fatalf("gc.outcome = %q, want %q", outcome, beadmeta.OutcomeCanceled)
	}
}

// resetControlReadyCache clears the per-dir readiness snapshot registry so a
// test observes a fresh scan rather than one memoized inside controlReadyCacheTTL.
func resetControlReadyCache(t *testing.T) {
	t.Helper()
	flush := func() {
		controlReadyCacheRegistry.mu.Lock()
		defer controlReadyCacheRegistry.mu.Unlock()
		controlReadyCacheRegistry.byDir = make(map[string]*controlReadyCacheEntry)
	}
	flush()
	t.Cleanup(flush)
}

// newRoutedControlBead writes a control bead routed to the control dispatcher,
// so the readiness scan's route filter admits it.
func newRoutedControlBead(t *testing.T, store beads.Store, rootID, route string) beads.Bead {
	t.Helper()
	bead := newControlBead(t, store, rootID)
	if err := store.Update(bead.ID, beads.UpdateOpts{
		Metadata: map[string]string{beadmeta.RunTargetMetadataKey: route},
	}); err != nil {
		t.Fatalf("route control bead: %v", err)
	}
	return beadByID(t, store, bead.ID)
}

// controlReadyScan runs the production readiness scan the serve loop's queue
// comes from, for a control dispatcher whose scope directory is dir.
func controlReadyScan(t *testing.T, dir string, agentCfg config.Agent, beadsCfg config.BeadsConfig) []string {
	t.Helper()
	resetControlReadyCache(t)
	queue, handled, err := tryControlReadyFromCacheOrFallback(
		workflowServeControlReadyQueryForBeads(agentCfg, beadsCfg), dir, nil)
	if err != nil {
		t.Fatalf("control-ready scan: %v", err)
	}
	if !handled {
		t.Fatal("control-ready scan: the generated query was not recognized as a control-ready query")
	}
	ids := make([]string, 0, len(queue))
	for _, item := range queue {
		ids = append(ids, item.ID)
	}
	return ids
}

// TestControlReadyScanEnumeratesTheGraphBindingOnSplitCity pins the producer
// side of the control dispatcher against the same ledger the consumer mutates.
//
// The dispatch hop is only half a fix. The serve loop's queue comes from
// nextWorkflowServeBeads -> tryControlReadyFromCacheOrFallback, and if that scan
// keeps reading the work store while the dispatch closes the binding's copy, the
// retained work copy stays open and ready forever: it is re-offered on every
// tick, ProcessControl no-ops on the already-closed binding copy, and
// drainWorkflowServeWork counts a no-op as progress and immediately re-queries.
// The loop never returns. Symmetrically, the beads a dispatch CREATES land in
// the binding, which a work-store scan never reads, so the workflow cannot
// advance past its first hop.
//
// Both arms of the scan are covered, because they reach the store by different
// routes: the cached arm wraps a CachingStore around the resolved class store,
// and the fallback arm (taken under bd-1.0.5 compatibility, which needs a tier
// CachedReady cannot serve) would otherwise shell `bd ready` in a directory that
// no longer holds the class.
func TestControlReadyScanEnumeratesTheGraphBindingOnSplitCity(t *testing.T) {
	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName}
	route := agentCfg.QualifiedName()

	for _, tc := range []struct {
		name  string
		beads config.BeadsConfig
	}{
		{name: "cached arm", beads: config.BeadsConfig{}},
		{name: "fallback arm", beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			graphStore := beads.NewMemStore()
			seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graphStore))

			root, err := graphStore.Create(beads.Bead{
				Title:    "workflow",
				Type:     "task",
				Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
			})
			if err != nil {
				t.Fatalf("create workflow root: %v", err)
			}
			live := newRoutedControlBead(t, graphStore, root.ID, route)

			got := controlReadyScan(t, cityPath, agentCfg, tc.beads)
			if len(got) != 1 || got[0] != live.ID {
				t.Fatalf("control-ready queue = %v, want [%s] from the graph binding; the scan read a ledger the dispatch does not write", got, live.ID)
			}
		})
	}
}

// TestControlReadyScanStopsOfferingAControlBeadTheDispatchClosed is the
// termination property the drain loop depends on, asserted directly.
//
// drainWorkflowServeWork loops until its queue comes back empty, and it has no
// iteration bound and no sleep between passes. What makes it terminate is that
// the copy it enumerates is the copy the dispatch closes. On a converged split
// city the migration retains the source verbatim, so the same control-bead id is
// open in the work store and live in the binding; a scan pointed at the work
// store therefore keeps returning an id the dispatch has already finished.
//
// The assertion is the fixed point: after the dispatch, a fresh scan is empty
// even though the retained work copy is still open.
func TestControlReadyScanStopsOfferingAControlBeadTheDispatchClosed(t *testing.T) {
	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName}
	route := agentCfg.QualifiedName()

	cityPath := t.TempDir()
	scopeStore := beads.NewMemStore()
	graphStore := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graphStore))

	staleRoot, err := scopeStore.Create(beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatalf("create retained workflow root: %v", err)
	}
	root, err := graphStore.Create(beads.Bead{
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
	stale := newRoutedControlBead(t, scopeStore, staleRoot.ID, route)
	live := newRoutedControlBead(t, graphStore, root.ID, route)
	if live.ID != stale.ID {
		t.Fatalf("fixture ids diverged (%s vs %s); the retained copy must share the binding copy's id", live.ID, stale.ID)
	}

	if got := controlReadyScan(t, cityPath, agentCfg, config.BeadsConfig{}); len(got) != 1 || got[0] != live.ID {
		t.Fatalf("control-ready queue before dispatch = %v, want [%s]", got, live.ID)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, cityPath, scopeStore, live.ID, cfg, &stdout, &stderr); err != nil {
		t.Fatalf("control dispatch: %v", err)
	}
	if closed := beadByID(t, graphStore, live.ID); closed.Status != "closed" {
		t.Fatalf("binding control bead status = %q, want closed", closed.Status)
	}
	// The precondition that makes this test meaningful: the work-store copy the
	// unrouted scan reads is still open, exactly as the migration left it.
	if retained := beadByID(t, scopeStore, stale.ID); retained.Status != "open" {
		t.Fatalf("retained work-store copy status = %q, want open; the fixture no longer models a converged city", retained.Status)
	}

	if got := controlReadyScan(t, cityPath, agentCfg, config.BeadsConfig{}); len(got) != 0 {
		t.Fatalf("control-ready queue after dispatch = %v, want empty; the serve loop re-offers a bead the dispatch already finished and never returns", got)
	}
}

// TestControlDispatchRigScopeStaysOnItsOwnStore guards the scope half of the
// class hop.
//
// There is no per-scope graph binding: resolveClassStore holds one city-level
// store per class, and `gc storage migrate` copies only the CITY work store, so
// a rig's control beads were never carried into the binding. Redirecting a rig
// scope there would look up every rig-scoped control bead in a database that has
// never held it — "bead not found", which IsTransientControllerError does not
// match, so drainWorkflowServeWork returns it as fatal and the dispatcher
// session exits and crash-loops.
func TestControlDispatchRigScopeStaysOnItsOwnStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "fixture")
	rigStore := beads.NewMemStore()
	graphStore := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graphStore))

	if got := controlGraphStore(cityPath, rigPath, nil, rigStore); got != beads.Store(rigStore) {
		t.Fatalf("controlGraphStore for a rig scope returned %T(%p), want the rig's own store %p; the city binding never held this rig's beads", got, got, rigStore)
	}
	if controlGraphRelocated(cityPath, rigPath) {
		t.Fatal("controlGraphRelocated for a rig scope = true; the readiness scan would go in-process against a binding with none of this rig's beads")
	}

	// End to end: a rig-scoped control bead dispatches against the rig store,
	// with the city binding empty throughout.
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
		t.Fatalf("rig-scoped control dispatch: %v", err)
	}
	if got := beadByID(t, rigStore, control.ID); got.Status != "closed" {
		t.Fatalf("rig control bead status = %q, want closed in the rig's own store", got.Status)
	}
	empty, err := graphStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("list city binding: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("city binding holds %d bead(s) after a rig-scoped dispatch, want 0", len(empty))
	}
}

// TestControlDispatchGatesOnTheStoreItMutates pins the idempotence gate to the
// copy the dispatch is about to write.
//
// ProcessControl decides whether to act from bead.Status and per-kind spawn
// metadata. The manual `gc convoy control <id>` path resolves its bead through
// findBeadAcrossStores, which opens unrouted scope stores, so on a split city
// that value is the frozen copy the migration retained. Gating on it while
// mutating the binding re-enters a control kind the graph store had already
// finished: a finished check re-runs its gate command, a finished fanout
// re-enters its spawn branch and the resulting quarantine rewrites a terminal
// outcome. runControlDispatcherWithStoreAndConfig therefore reads the bead
// itself, from graphStore, rather than accepting a caller's value.
func TestControlDispatchGatesOnTheStoreItMutates(t *testing.T) {
	cityPath := t.TempDir()
	scopeStore := beads.NewMemStore()
	graphStore := beads.NewMemStore()
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graphStore))

	staleRoot, err := scopeStore.Create(beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatalf("create retained workflow root: %v", err)
	}
	root, err := graphStore.Create(beads.Bead{
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
	stale := newControlBead(t, scopeStore, staleRoot.ID)
	live := newControlBead(t, graphStore, root.ID)
	if live.ID != stale.ID {
		t.Fatalf("fixture ids diverged (%s vs %s)", live.ID, stale.ID)
	}

	// The binding's copy is already finished; the retained work copy is not.
	// This is what a split city looks like on the second `gc convoy control`.
	closed := "closed"
	if err := graphStore.Update(live.ID, beads.UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass},
	}); err != nil {
		t.Fatalf("close the binding copy: %v", err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, cityPath, scopeStore, live.ID, cfg, &stdout, &stderr); err != nil {
		t.Fatalf("control dispatch: %v", err)
	}
	if action := stdout.String(); action != "" {
		t.Fatalf("control dispatch acted (%q) on a bead the graph store had already finished; the idempotence gate read the retained work copy", strings.TrimSpace(action))
	}
	if got := beadByID(t, graphStore, live.ID); got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("gc.outcome = %q, want the terminal %q left untouched", got.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomePass)
	}
}

// splitClassDrainFormulaDir writes the item formula a drain expands into and
// returns the search path the city config exposes for it.
func splitClassDrainFormulaDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	content := `formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work {{convoy_id}}"
`
	if err := os.WriteFile(filepath.Join(dir, "drain-item.formula.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write drain item formula: %v", err)
	}
	return dir
}

// TestControlDispatchDrainNamesTheWorkLegForConvoyMembership drives the real
// dispatcher entry point on a converged split city and pins that a drain still
// sees its convoy.
//
// A drain is the one control kind that reads beads it did not create. Its
// control and item roots are graph class and the dispatch hops them to the
// binding, but the graph.v2 input convoy is minted alongside its work members
// and stays in the scope store — the same store this function hands to
// EmitCurrent as the work leg that owns that convoy's tracks edges. If the
// dispatch does not name that leg, the membership read runs entirely against the
// binding, which has never seen the convoy and answers EMPTY rather than
// failing. A zero-member drain is a drain SUCCESS: it closes gc.outcome=pass with
// an empty manifest, the whole convoy is left open and never dispatched, and the
// command exits 0 with no operator signal — strictly worse than the loud
// orphaned_workflow the unrouted read produced before.
//
// The assertion is therefore on the manifest, not on the drain completing: the
// convoy must expand to one row per member, and the pass-with-nothing-expanded
// state must not be reachable over a convoy that has members.
func TestControlDispatchDrainNamesTheWorkLegForConvoyMembership(t *testing.T) {
	cityPath := t.TempDir()
	scopeStore := beads.NewMemStore()
	graphStore := beads.NewMemStoreFrom(1000, nil, nil)
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graphStore))

	convoy, err := scopeStore.Create(beads.Bead{
		Title:    "input convoy",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	})
	if err != nil {
		t.Fatalf("create input convoy: %v", err)
	}
	var memberIDs []string
	for _, title := range []string{"first", "second"} {
		member, err := scopeStore.Create(beads.Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatalf("create member: %v", err)
		}
		if err := convoycore.TrackItem(scopeStore, convoy.ID, member.ID); err != nil {
			t.Fatalf("track member: %v", err)
		}
		memberIDs = append(memberIDs, member.ID)
	}

	root, err := graphStore.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
			"gc.formula_contract":    "graph.v2",
			"gc.input_convoy_id":     convoy.ID,
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	drain, err := graphStore.Create(beads.Bead{
		Title: "drain",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       "drain",
			beadmeta.RootBeadIDMetadataKey: root.ID,
			"gc.drain_context":             "separate",
			"gc.drain_formula":             "drain-item",
			"gc.drain_member_access":       "read",
		},
	})
	if err != nil {
		t.Fatalf("create drain control: %v", err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	cfg.FormulaLayers.City = []string{splitClassDrainFormulaDir(t)}
	var stdout, stderr bytes.Buffer
	// The dispatch may fail loudly downstream of the expansion; what it may not
	// do is silently report the convoy as drained.
	_ = runControlDispatcherWithStoreAndConfig(cityPath, cityPath, scopeStore, drain.ID, cfg, &stdout, &stderr)

	got := beadByID(t, graphStore, drain.ID)
	raw := got.Metadata[beadmeta.DrainManifestMetadataKey]
	if strings.TrimSpace(raw) == "" {
		t.Fatalf("drain recorded no manifest at all; status=%q outcome=%q stderr=%q", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], stderr.String())
	}
	var manifest struct {
		Rows []struct {
			MemberID string `json:"member_id"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("parse drain manifest %q: %v", raw, err)
	}
	if got.Metadata[beadmeta.OutcomeMetadataKey] == beadmeta.OutcomePass && len(manifest.Rows) == 0 {
		t.Fatalf("drain closed gc.outcome=pass with an empty manifest over a convoy of %d members; every member is left open and the run reports green. stdout=%q", len(memberIDs), stdout.String())
	}
	if len(manifest.Rows) != len(memberIDs) {
		t.Fatalf("manifest rows = %d, want %d — one per convoy member; the dispatch did not name the work leg that owns this convoy's tracks edges", len(manifest.Rows), len(memberIDs))
	}
	for i, id := range memberIDs {
		if manifest.Rows[i].MemberID != id {
			t.Fatalf("manifest row %d member = %q, want %q", i, manifest.Rows[i].MemberID, id)
		}
	}
}

// TestSourceWorkflowStoresScanTheLedgerThatHoldsWorkflowRoots pins the
// precondition that guards a destructive close.
//
// workflow-finalize will not close a source bead while another live workflow
// root still references it — that is what keeps an "Adopt PR" request open while
// its second workflow is still executing. Workflow roots are graph class, so on
// a converged split city they live in the binding, while the source bead they
// were launched from stays in the work store. Feed that scan the work store and
// it answers "no live roots" for exactly the arrangement it exists to catch, and
// the answer is acted on: the source bead is closed and terminally stamped under
// a running workflow. The launch-side singleton guard is work-store-only too, so
// a split city reaches the two-live-roots state without --force.
//
// The assertion is behavioral rather than structural: the store this lister
// hands the scan must actually find a root that exists only in the binding.
func TestSourceWorkflowStoresScanTheLedgerThatHoldsWorkflowRoots(t *testing.T) {
	cityPath := t.TempDir()
	graphStore := beads.NewMemStoreFrom(1000, nil, nil)
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(graphStore))

	const sourceBeadID = "gc-1"
	const storeRef = "city:test-city"
	live, err := graphStore.Create(beads.Bead{
		Title: "workflow B",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:           beadmeta.KindWorkflow,
			beadmeta.SourceBeadIDMetadataKey:   sourceBeadID,
			beadmeta.SourceStoreRefMetadataKey: storeRef,
		},
	})
	if err != nil {
		t.Fatalf("create live workflow root: %v", err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	stores, err := makeSourceWorkflowStoresLister(cityPath, cfg)()
	if err != nil {
		t.Fatalf("source workflow stores: %v", err)
	}
	if len(stores) == 0 {
		t.Fatal("no source workflow stores to scan")
	}

	var found []string
	for _, info := range stores {
		roots, err := sourceworkflow.ListLiveRoots(info.Store, sourceBeadID, storeRef, storeRef)
		if err != nil {
			t.Fatalf("ListLiveRoots(%s): %v", info.StoreRef, err)
		}
		for _, root := range roots {
			found = append(found, root.ID)
		}
	}
	if !slices.Contains(found, live.ID) {
		t.Fatalf("live-root scan found %v, want the binding-resident root %s; the scan reads a ledger that holds no workflow roots, so the guard answers \"none live\" and workflow-finalize closes the source bead under a running workflow", found, live.ID)
	}
}

// TestSourceWorkflowStoresStayOnTheScopeStoreWithNoRelocation is the
// compatibility half: a city that relocates nothing resolves the same scope
// store the scan always used, and no graph binding is consulted.
func TestSourceWorkflowStoresStayOnTheScopeStoreWithNoRelocation(t *testing.T) {
	cityPath := t.TempDir()
	resetCLIStorageRoutes(t)
	graphStore := beads.NewMemStoreFrom(1000, nil, nil)

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	stores, err := makeSourceWorkflowStoresLister(cityPath, cfg)()
	if err != nil {
		t.Fatalf("source workflow stores: %v", err)
	}
	if len(stores) == 0 {
		t.Fatal("no source workflow stores to scan")
	}
	for _, info := range stores {
		if info.Store == beads.Store(graphStore) {
			t.Fatal("a city with no [storage] resolved a graph binding for the live-root scan")
		}
		if info.Store == nil {
			t.Fatal("nil store in the live-root scan set")
		}
	}
}
