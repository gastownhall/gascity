package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// TestRigControlDispatchOnARealSplitCity is W4/W5 on the real fixture: a rig
// dispatcher's primary ledger is the SQLite binding, a binding-resident tree is
// dispatched there, a pre-S4 rig tree is dispatched in place, and an imported
// rig reads only the binding.
func TestRigControlDispatchOnARealSplitCity(t *testing.T) {
	f := newS4RigFixture(t, true)
	rigPath, rig := f.rigPath("ra"), f.rigs["ra"]

	newCanceledTree := func(t *testing.T, store beads.Store) beads.Bead {
		t.Helper()
		root, err := store.Create(beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.CancelRequestedMetadataKey: "operator",
			beadmeta.RootStoreRefMetadataKey:    "rig:ra",
		}})
		if err != nil {
			t.Fatal(err)
		}
		return newControlBead(t, store, root.ID)
	}
	dispatch := func(t *testing.T, scope beads.Store, id string) {
		t.Helper()
		var stdout, stderr strings.Builder
		if err := runControlDispatcherWithStoreAndConfig(f.cityPath, rigPath, scope, id, f.cfg, &stdout, &stderr); err != nil {
			t.Fatalf("rig-scoped dispatch of %s: %v\n%s", id, err, stderr.String())
		}
	}

	inBinding := newCanceledTree(t, f.binding)
	dispatch(t, rig, inBinding.ID)
	if got := beadByID(t, f.binding, inBinding.ID); got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeCanceled {
		t.Errorf("binding-resident control = (%s, %q), want closed and canceled in the binding", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey])
	}

	inRig := newCanceledTree(t, rig)
	dispatch(t, rig, inRig.ID)
	if got := beadByID(t, rig, inRig.ID); got.Status != "closed" {
		t.Errorf("pre-S4 rig control status = %q, want closed in the rig store", got.Status)
	}
	if _, err := f.binding.Get(inRig.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("dispatching a rig-resident tree copied %s into the binding (err=%v)", inRig.ID, err)
	}

	if sources, _, err := controlReadyLegSources(rigPath, f.cityPath, f.cfg); err != nil || len(sources) != 2 || !sameStorePtr(sources[0], f.binding) {
		t.Errorf("ready legs for a rig awaiting import = %d (err=%v), want the binding then the rig store", len(sources), err)
	}
	f.markImported(t, "ra", true)
	if controlGraphRelicLeg(f.cityPath, rigPath) {
		t.Error("an imported rig keeps its relic leg")
	}
	if sources, owned, err := controlReadyLegSources(rigPath, f.cityPath, f.cfg); err != nil || len(sources) != 1 || !sameStorePtr(sources[0], f.binding) || len(owned) != 0 {
		t.Errorf("ready legs for an imported rig = %d leg(s), %d owned (err=%v), want the binding only, opening nothing", len(sources), len(owned), err)
	}
}

// TestControlReadyFallbackPrefersTheBindingCopyOfATwin pins leg ORDER in the
// shell fallback: an id both legs hold (an import's copy before its clear)
// resolves to the binding's row.
func TestControlReadyFallbackPrefersTheBindingCopyOfATwin(t *testing.T) {
	cityPath, rigPath, _ := rigFederationFixture(t, `[{"id":"gc-twin","title":"rig copy"}]`)
	binding := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gc-twin", Title: "binding copy", Type: "task", Status: "open"}}, nil)
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(binding))
	got, err := controlReadyFallbackReady(rigPath, cityPath, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, b := range got {
		if b.ID == "gc-twin" {
			titles = append(titles, b.Title)
		}
	}
	if len(titles) != 1 || titles[0] != "binding copy" {
		t.Errorf("twin rows = %v, want exactly the binding's copy", titles)
	}
}

// TestDeferredEmitProjectsAPreS4RigRootAgainstItsOwnStore pins the serve
// drain's relic flag: a root whose controls were read from the rig store (the
// relic leg) is projected against that store, and every other root against the
// binding.
func TestDeferredEmitProjectsAPreS4RigRootAgainstItsOwnStore(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	f := newS4RigFixture(t, true)
	writeS4FileCityTOML(t, f)
	var opened beads.Store
	prevOpen, prevEmit := openControlStoreForDispatch, executionEmitCurrent
	t.Cleanup(func() { openControlStoreForDispatch, executionEmitCurrent = prevOpen, prevEmit })
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		opened = newCloseCountingStore(t, false)
		return opened, nil
	}
	projectedOn := map[string]beads.Store{}
	executionEmitCurrent = func(_ events.Recorder, graph beads.GraphStore, _ beads.WorkStore, rootID, _ string) error {
		projectedOn[rootID] = graph.Store
		return nil
	}
	d := newExecutionEmitDeferral(f.cityPath, f.rigPath("ra"), io.Discard)
	d.add("ra-relic-root", true)
	d.add("gcg-binding-root", false)
	d.flush()
	if got := projectedOn["ra-relic-root"]; got == nil || got != opened {
		t.Errorf("relic root projected against %T, want the rig's own (opened scope) store", got)
	}
	if got := projectedOn["gcg-binding-root"]; !sameStorePtr(got, f.binding) {
		t.Errorf("binding root projected against %T, want the binding", got)
	}
}

// TestSourceWorkflowListerListsTheBindingOnceUnderTheCityRef pins the
// lister's city-only hop: on a split city the binding is listed once, under
// the city ref, and each rig keeps its own store (its pre-S4 roots).
func TestSourceWorkflowListerListsTheBindingOnceUnderTheCityRef(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	f := newS4RigFixture(t, true)
	writeS4FileCityTOML(t, f)
	t.Setenv("GC_CITY", f.cityPath)
	// Scoped file-store roots, so each rig is a candidate store of its own.
	if err := ensureScopedFileStoreLayout(f.cityPath); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{f.cityPath, f.rigPath("ra"), f.rigPath("rb")} {
		if err := ensurePersistedScopeLocalFileStore(root); err != nil {
			t.Fatal(err)
		}
	}
	stores, err := makeSourceWorkflowStoresLister(f.cityPath, f.cfg)()
	if err != nil {
		t.Fatal(err)
	}
	var bindingRefs, rigRefs []string
	for _, s := range stores {
		if sameStorePtr(s.Store, f.binding) {
			bindingRefs = append(bindingRefs, s.StoreRef)
		} else if strings.HasPrefix(s.StoreRef, "rig:") {
			rigRefs = append(rigRefs, s.StoreRef)
		}
	}
	if len(bindingRefs) != 1 || !strings.HasPrefix(bindingRefs[0], "city:") {
		t.Errorf("binding listed under %v, want exactly once under the city ref", bindingRefs)
	}
	if len(rigRefs) != 2 {
		for _, s := range stores {
			t.Logf("entry ref=%q store=%T", s.StoreRef, s.Store)
		}
		t.Errorf("rig entries = %v, want each rig's own store", rigRefs)
	}
}
