package api

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// S4a readers in the API: on a split city a rig's graph rows live in the city
// binding, recorded by gc.root_store_ref = rig:<name> (scope, never location).
// The fixtures use a real SQLite store as the binding and a real file store as
// the rig's work store.

// relicReportingState is the fake state plus the S4 relic-free predicate the
// controller state implements.
type relicReportingState struct {
	*fakeState
	relicFree map[string]bool
}

func (s relicReportingState) RigGraphRelicFree(rig string) bool { return s.relicFree[rig] }

func newS4APIState(t *testing.T, split bool) (relicReportingState, beads.Store, beads.Store) {
	t.Helper()
	fs := newFakeState(t)
	rig, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"), beads.WithFileStoreIDPrefix("my"))
	if err != nil {
		t.Fatal(err)
	}
	fs.stores = map[string]beads.Store{"myrig": rig}
	fs.cityBeadStore = beads.NewMemStore()
	var binding beads.Store
	if split {
		binding, err = beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = binding.(interface{ CloseStore() error }).CloseStore() })
		fs.graphBeadStore = binding
	}
	return relicReportingState{fakeState: fs, relicFree: map[string]bool{}}, binding, rig
}

func workflowRoot(t *testing.T, store beads.Store, title string) beads.Bead {
	t.Helper()
	root, err := store.Create(beads.Bead{Title: title, Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:         beadmeta.KindWorkflow,
		beadmeta.RootStoreRefMetadataKey: "rig:myrig",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestWorkflowEventRootProbesTheBindingBeforeTheScopeItNames: the metadata
// names the rig, but on a split city the root lives in the binding, which is
// probed first; the rig store is probed as a relic leg only while the rig may
// still hold pre-S4 trees.
func TestWorkflowEventRootProbesTheBindingBeforeTheScopeItNames(t *testing.T) {
	state, binding, rig := newS4APIState(t, true)
	root := workflowRoot(t, binding, "binding root")
	step := beads.Bead{ID: "evt", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:   root.ID,
		beadmeta.RootStoreRefMetadataKey: "rig:myrig",
	}}
	info, got, ok := workflowEventRoot(state, step)
	if !ok || got.ID != root.ID || info.store != binding {
		t.Fatalf("workflowEventRoot = (%v, %s, store %T), want the binding's root", ok, got.ID, info.store)
	}

	legacy := workflowRoot(t, rig, "pre-S4 rig root")
	step.Metadata[beadmeta.RootBeadIDMetadataKey] = legacy.ID
	if info, got, ok := workflowEventRoot(state, step); !ok || got.ID != legacy.ID || info.store != rig {
		t.Fatalf("workflowEventRoot for a pre-S4 rig root = (%v, %s), want it from the rig store", ok, got.ID)
	}
	state.relicFree["myrig"] = true
	if _, got, ok := workflowEventRoot(state, step); ok {
		t.Errorf("workflowEventRoot read %s from a relic-free rig's store; once imported, a rig store holds only work", got.ID)
	}
}

// TestWorkflowEventRootIsUnchangedOnASingleStoreCity is the control: no
// binding, so the store the metadata names is probed first, as before.
func TestWorkflowEventRootIsUnchangedOnASingleStoreCity(t *testing.T) {
	state, _, rig := newS4APIState(t, false)
	root := workflowRoot(t, rig, "rig root")
	step := beads.Bead{ID: "evt", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:   root.ID,
		beadmeta.RootStoreRefMetadataKey: "rig:myrig",
	}}
	plain := state.fakeState
	if info, got, ok := workflowEventRoot(plain, step); !ok || got.ID != root.ID || info.store != rig {
		t.Fatalf("single-store workflowEventRoot = (%v, %s), want the rig's root", ok, got.ID)
	}
}

// TestIsGraphConvoyIDSkipsARelicFreeRig is isGraphConvoyID's rig leg: it is
// asked only while the rig may still hold pre-S4 graph rows.
func TestIsGraphConvoyIDSkipsARelicFreeRig(t *testing.T) {
	state, binding, rig := newS4APIState(t, true)
	inBinding := workflowRoot(t, binding, "binding convoy")
	legacy := workflowRoot(t, rig, "pre-S4 rig convoy")
	srv := New(state)
	if !isGraphConvoyID(srv, inBinding.ID) {
		t.Error("a binding-resident graph convoy is not recognized")
	}
	if !isGraphConvoyID(srv, legacy.ID) {
		t.Error("a pre-S4 rig graph convoy is not recognized while the rig awaits import")
	}
	state.relicFree["myrig"] = true
	srv = New(state)
	if isGraphConvoyID(srv, legacy.ID) {
		t.Error("isGraphConvoyID still reads a relic-free rig's store")
	}
	if !isGraphConvoyID(srv, inBinding.ID) {
		t.Error("retiring the rig leg lost the binding's graph convoy")
	}
}

// TestWorkflowEventRootResolvesATwinToTheBinding: an id both the binding and
// the rig store hold (an import's copy before its clear) resolves to the
// binding's row, even though gc.root_store_ref names the rig.
func TestWorkflowEventRootResolvesATwinToTheBinding(t *testing.T) {
	state, binding, rig := newS4APIState(t, true)
	rigRoot := workflowRoot(t, rig, "rig copy")
	creator, ok := binding.(beads.ForeignIDCreator)
	if !ok {
		t.Fatal("the SQLite binding does not accept foreign ids")
	}
	if _, err := creator.CreateWithForeignID(beads.Bead{ID: rigRoot.ID, Title: "binding copy", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:         beadmeta.KindWorkflow,
		beadmeta.RootStoreRefMetadataKey: "rig:myrig",
	}}); err != nil {
		t.Fatal(err)
	}
	step := beads.Bead{ID: "evt", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:   rigRoot.ID,
		beadmeta.RootStoreRefMetadataKey: "rig:myrig",
	}}
	info, got, ok := workflowEventRoot(state, step)
	if !ok || got.Title != "binding copy" || info.store != binding {
		t.Fatalf("workflowEventRoot for a twin = (%v, %q), want the binding's copy", ok, got.Title)
	}
}
