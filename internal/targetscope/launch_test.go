package targetscope

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

func recipeWithRoot() *formula.Recipe {
	return &formula.Recipe{Steps: []formula.RecipeStep{{ID: "root", Title: "root"}}}
}

func rootScope(t *testing.T, recipe *formula.Recipe) Resolution {
	t.Helper()
	return Parse(recipe.Steps[0].Metadata[beadmeta.TargetScopeMetadataKey])
}

func TestStampRootWritesParsableScope(t *testing.T) {
	recipe := recipeWithRoot()
	want := Scope{V: 1, Branch: "release", Worktree: "/srv/wt"}

	if err := StampRoot(recipe, want); err != nil {
		t.Fatalf("StampRoot: %v", err)
	}
	got := rootScope(t, recipe)
	if !got.Valid() || !got.Scope.Equal(want) {
		t.Fatalf("root carries %+v (%v), want %+v", got.Scope, got.State, want)
	}
}

// §2c: a boundary that cannot resolve a scope must leave a present-valid
// field-empty object, never nothing. Absence is what lets a later claim or
// reconcile write cwd, so "unknown" has to be stamped rather than skipped.
func TestStampRootStampsUnknownRatherThanSkipping(t *testing.T) {
	recipe := recipeWithRoot()

	if err := StampRoot(recipe, Unknown()); err != nil {
		t.Fatalf("StampRoot: %v", err)
	}
	got := rootScope(t, recipe)
	if !got.Valid() {
		t.Fatalf("unknown scope resolved %v, want a present-valid object", got.State)
	}
	if !got.Scope.IsUnknown() {
		t.Fatalf("stamped %+v, want field-empty", got.Scope)
	}
}

func TestStampRootPreservesExistingMetadata(t *testing.T) {
	recipe := recipeWithRoot()
	recipe.Steps[0].Metadata = map[string]string{"gc.kind": "workflow"}

	if err := StampRoot(recipe, Scope{V: 1, Branch: "main"}); err != nil {
		t.Fatalf("StampRoot: %v", err)
	}
	if got := recipe.Steps[0].Metadata["gc.kind"]; got != "workflow" {
		t.Fatalf("gc.kind = %q, want it preserved", got)
	}
}

func TestStampRootRejectsSteplessRecipe(t *testing.T) {
	if err := StampRoot(&formula.Recipe{}, Scope{V: 1}); err == nil {
		t.Fatal("stamping a recipe with no steps succeeded, want an error")
	}
	if err := StampRoot(nil, Scope{V: 1}); err == nil {
		t.Fatal("stamping a nil recipe succeeded, want an error")
	}
}

// Only the root is stamped: stages inherit through the root chain, and a
// second copy on every stage is a second thing that can disagree.
func TestStampRootLeavesStagesUnstamped(t *testing.T) {
	recipe := &formula.Recipe{Steps: []formula.RecipeStep{{ID: "root"}, {ID: "stage-1"}}}

	if err := StampRoot(recipe, Scope{V: 1, Branch: "main"}); err != nil {
		t.Fatalf("StampRoot: %v", err)
	}
	if _, ok := recipe.Steps[1].Metadata[beadmeta.TargetScopeMetadataKey]; ok {
		t.Fatal("stage carries its own target scope, want inheritance through the root")
	}
}

func TestPrepareLaunchDeclaresMembersAndStampsRoot(t *testing.T) {
	store := beads.NewMemStore()
	member := newMember(t, store, "", nil)
	recipe := recipeWithRoot()
	want := Scope{V: 1, Branch: "release", Worktree: "/srv/wt"}

	err := PrepareLaunch(recipe, want, []Member{{ID: member.ID, Store: store, Scope: want}})
	if err != nil {
		t.Fatalf("PrepareLaunch: %v", err)
	}
	if got := declaredScope(t, store, member.ID); !got.Valid() || !got.Scope.Equal(want) {
		t.Fatalf("member carries %+v (%v), want %+v", got.Scope, got.State, want)
	}
	if got := rootScope(t, recipe); !got.Valid() || !got.Scope.Equal(want) {
		t.Fatalf("root carries %+v (%v), want %+v", got.Scope, got.State, want)
	}
}

// THE PHASE-ORDER INVARIANT. A member that refuses to declare must abort the
// launch with the root still unstamped, so the caller cannot materialize a
// root that points at members which never accepted the scope.
//
// This asserts the ORDER, not just the error: an implementation that stamped
// first and declared second would return the same error and still be wrong.
func TestPrepareLaunchLeavesRootUnstampedWhenAMemberRejects(t *testing.T) {
	store := beads.NewMemStore()
	declared := Scope{V: 1, Branch: "main"}
	member := newMember(t, store, "", nil)
	if _, err := Declare(store, member.ID, declared); err != nil {
		t.Fatalf("seeding the declared member: %v", err)
	}
	recipe := recipeWithRoot()

	conflicting := Scope{V: 1, Branch: "release"}
	err := PrepareLaunch(recipe, conflicting, []Member{{ID: member.ID, Store: store, Scope: conflicting}})
	if !errors.Is(err, ErrDeclarationConflict) {
		t.Fatalf("PrepareLaunch error = %v, want a declaration conflict", err)
	}
	if _, ok := recipe.Steps[0].Metadata[beadmeta.TargetScopeMetadataKey]; ok {
		t.Fatal("root was stamped despite a rejected member: the launch phase ran out of order")
	}
	if got := declaredScope(t, store, member.ID); !got.Scope.Equal(declared) {
		t.Fatalf("member scope changed to %+v, want the immutable %+v", got.Scope, declared)
	}
}

// A rejection partway through a multi-member set leaves the earlier
// declarations committed. They are immutable-correct and a retry re-declares
// them as no-ops; rolling them back would reintroduce the lost-update race.
func TestPrepareLaunchKeepsEarlierDeclarationsOnLaterRejection(t *testing.T) {
	store := beads.NewMemStore()
	want := Scope{V: 1, Branch: "release"}
	first := newMember(t, store, "", nil)
	blocked := newMember(t, store, "", nil)
	if _, err := Declare(store, blocked.ID, Scope{V: 1, Branch: "main"}); err != nil {
		t.Fatalf("seeding the blocked member: %v", err)
	}
	recipe := recipeWithRoot()

	err := PrepareLaunch(recipe, want, []Member{
		{ID: first.ID, Store: store, Scope: want},
		{ID: blocked.ID, Store: store, Scope: want},
	})
	if !errors.Is(err, ErrDeclarationConflict) {
		t.Fatalf("PrepareLaunch error = %v, want a declaration conflict", err)
	}
	if got := declaredScope(t, store, first.ID); !got.Valid() || !got.Scope.Equal(want) {
		t.Fatalf("first member carries %+v (%v), want its committed %+v", got.Scope, got.State, want)
	}
}

func TestFormulaDefaultVarsReadsTheDefaultLayer(t *testing.T) {
	main := "main"
	noDefault := &formula.VarDef{}
	f := &formula.Formula{Vars: map[string]*formula.VarDef{
		"base_branch": {Default: &main},
		"issue":       noDefault,
		"nil_def":     nil,
	}}

	got := FormulaDefaultVars(f)
	if got["base_branch"] != "main" {
		t.Fatalf("base_branch default = %q, want main", got["base_branch"])
	}
	if _, ok := got["issue"]; ok {
		t.Fatal("a var with no default leaked into the defaults layer")
	}
	if _, ok := got["nil_def"]; ok {
		t.Fatal("a nil var definition leaked into the defaults layer")
	}
}

func TestFormulaDefaultVarsHandlesEmptyFormula(t *testing.T) {
	if got := FormulaDefaultVars(nil); got != nil {
		t.Fatalf("nil formula produced %v, want nil", got)
	}
	if got := FormulaDefaultVars(&formula.Formula{}); got != nil {
		t.Fatalf("formula with no vars produced %v, want nil", got)
	}
}
