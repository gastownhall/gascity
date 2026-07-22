package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// A drain member can live only in a per-class member store, absent from the
// control's graph store. Declaring it there — where the close gate will read it
// — must resolve the OWNING store; declaring against the graph store would
// fail-close the drain for the one bead that matters. A member in no known store
// (deleted before unit creation) resolves to nil, which the caller skips rather
// than fails.
func TestDrainMemberDeclarationStoreResolvesOwningStore(t *testing.T) {
	// Per-assertion fresh stores: MemStore auto-assigns ids, so two fresh stores
	// would both hand out "gc-1" and collide. In production ids are prefix-
	// disjoint across stores, so a member lives in exactly one — each assertion
	// models one placement.

	// Member in the primary graph store.
	primary := beads.NewMemStore()
	empty := beads.NewMemStore()
	m1, err := primary.Create(beads.Bead{Title: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := drainMemberDeclarationStore(primary, m1.ID, ProcessOptions{MemberStores: []beads.Store{empty}}); err != nil || got != beads.Store(primary) {
		t.Fatalf("primary member: got (%v, %v), want the primary store", got, err)
	}

	// Member only in a per-class member store, absent from the graph store.
	graph := beads.NewMemStore()
	memberStore := beads.NewMemStore()
	m2, err := memberStore.Create(beads.Bead{Title: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := drainMemberDeclarationStore(graph, m2.ID, ProcessOptions{MemberStores: []beads.Store{memberStore}}); err != nil || got != beads.Store(memberStore) {
		t.Fatalf("cross-store member: got (%v, %v), want the member store", got, err)
	}

	// Member in no known store (deleted before unit creation): nil, so the
	// caller skips the declaration rather than fail-closing the drain.
	if got, err := drainMemberDeclarationStore(graph, "no-such-bead", ProcessOptions{MemberStores: []beads.Store{memberStore}}); err != nil || got != nil {
		t.Fatalf("absent member: got (%v, %v), want (nil, nil) so the caller skips", got, err)
	}
}

// A drain inherits the scope of the workflow it runs inside. The control reaches
// the parent root through gc.root_bead_id, so a scope stamped there governs
// every item root AND every tracked member the items will close. Inheriting is
// sound because the parent's #16 equality makes the inherited scope equal to
// what the drain's runtimeVars substitute; re-resolving would risk drift.
func TestDrainItemRootAndMemberInheritControlScope(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)

	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	inherited := targetscope.Unknown()
	inherited.Branch = "release"
	blob, err := targetscope.Marshal(inherited)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(root.ID, beadmeta.TargetScopeMetadataKey, blob); err != nil {
		t.Fatalf("stamp control scope: %v", err)
	}
	members, err := convoycore.Members(store, root.Metadata["gc.input_convoy_id"], false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) == 0 {
		t.Fatal("seed produced no tracked members")
	}

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	if result.Action != "drain-expanded" {
		t.Fatalf("action = %q, want drain-expanded", result.Action)
	}

	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	if len(manifest.Rows) == 0 {
		t.Fatal("no drain rows materialized")
	}
	for _, row := range manifest.Rows {
		itemRoot := mustGetBead(t, store, row.ItemRootID)
		res := targetscope.Parse(itemRoot.Metadata[beadmeta.TargetScopeMetadataKey])
		if !res.Valid() || res.Scope.Branch != "release" {
			t.Fatalf("item root %s scope = %v/%q, want valid release (inherited from control)", row.ItemRootID, res.State, res.Scope.Branch)
		}
	}
	for _, m := range members {
		got := mustGetBead(t, store, m.ID)
		res := targetscope.Parse(got.Metadata[beadmeta.TargetScopeMetadataKey])
		if !res.Valid() || res.Scope.Branch != "release" {
			t.Fatalf("tracked member %s scope = %v/%q, want valid release (declared under CAS)", m.ID, res.State, res.Scope.Branch)
		}
	}
}

// With no scoped parent (the pre-seam population) and an item formula that
// consumes no branch carrier, the item root still gets a present-valid
// field-empty object (§2c). Absence is the only state that re-enables the cwd
// writers on the item's stages, so "no branch to record" must never be
// implemented as writing nothing.
func TestDrainItemRootAbsentControlStampsFieldEmptyScope(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	if result.Action != "drain-expanded" {
		t.Fatalf("action = %q, want drain-expanded", result.Action)
	}

	drain = mustGetBead(t, store, drain.ID)
	manifest := mustDrainManifest(t, drain)
	if len(manifest.Rows) == 0 {
		t.Fatal("no drain rows materialized")
	}
	for _, row := range manifest.Rows {
		itemRoot := mustGetBead(t, store, row.ItemRootID)
		res := targetscope.Parse(itemRoot.Metadata[beadmeta.TargetScopeMetadataKey])
		if !res.Valid() {
			t.Fatalf("item root %s scope state = %v, want present-valid field-empty (§2c), never absent", row.ItemRootID, res.State)
		}
		if res.Scope.Branch != "" {
			t.Fatalf("item root %s scope.branch = %q, want empty for a carrierless item under an unscoped control", row.ItemRootID, res.Scope.Branch)
		}
	}
}
