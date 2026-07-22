package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// beadScope reads a bead's own declared scope. It deliberately does NOT walk
// the inheritance chain: these tests assert what the declaration protocol
// COMMITTED on the bead, and an inherited read would pass even when nothing
// was declared.
func beadScope(t *testing.T, deps SlingDeps, id string) targetscope.Resolution {
	t.Helper()
	b, err := deps.Store.Get(id)
	if err != nil {
		t.Fatalf("getting %s: %v", id, err)
	}
	return targetscope.Parse(b.Metadata[beadmeta.TargetScopeMetadataKey])
}

func openTaskBead(t *testing.T, deps SlingDeps, title string) beads.Bead {
	t.Helper()
	b, err := deps.Store.Create(beads.Bead{Title: title, Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("creating %s: %v", title, err)
	}
	return b
}

// THE ATTACH BOUNDARY'S LOAD-BEARING ASSERTION. An attached launch routes and
// closes the SOURCE bead, not the wisp root — the source carries gc.routed_to
// and molecule_id and is the claimable unit of work (sling_core.go's
// "Route the SOURCE bead" comment). The inheritance walk is
// bead -> ParentID -> gc.root_bead_id and does NOT follow molecule_id, so the
// source cannot reach the root's stamp. Stamping only the root would leave the
// one bead that is actually claimed and closed resolving ABSENT, and a boundary
// that stamps a root nobody reads is wired in appearance only.
func TestAttachedFormulaDeclaresScopeOnTheSourceBead(t *testing.T) {
	dir := scopedFormulaDir(t, "attach-scope")
	deps, agent := scopedLaunchDeps(t, dir)
	target := openTaskBead(t, deps, "attach target")

	opts := testOpts(agent, target.ID)
	opts.OnFormula = "attach-scope"
	opts.Vars = []string{"gc_target_branch=release"}

	if _, err := DoSling(opts, deps, deps.Store); err != nil {
		t.Fatalf("DoSling --on: %v", err)
	}

	got := beadScope(t, deps, target.ID)
	if !got.Valid() {
		t.Fatalf("source bead scope state = %v, want a present-valid declared object", got.State)
	}
	if got.Scope.Branch != "release" {
		t.Fatalf("source bead scope.branch = %q, want release", got.Scope.Branch)
	}
}

// E2E #16 at the attach boundary: the reconciled winner reaches the carrier
// the formula substitutes, so substitution and the close gate read one branch.
func TestAttachedFormulaConsumedCarrierEqualsTheDeclaredScope(t *testing.T) {
	dir := scopedFormulaDir(t, "attach-carrier")
	deps, agent := scopedLaunchDeps(t, dir)
	target := openTaskBead(t, deps, "carrier target")

	opts := testOpts(agent, target.ID)
	opts.OnFormula = "attach-carrier"
	opts.Vars = []string{"gc_target_branch=release"}

	if _, err := DoSling(opts, deps, deps.Store); err != nil {
		t.Fatalf("DoSling --on: %v", err)
	}

	scope := beadScope(t, deps, target.ID)
	if !scope.Valid() {
		t.Fatalf("source bead scope state = %v, want valid", scope.State)
	}
	steps, err := deps.Store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("listing store: %v", err)
	}
	wantTitle := "Work on " + scope.Scope.Branch
	for _, s := range steps {
		if s.Title == wantTitle {
			return
		}
	}
	t.Fatalf("no step titled %q — substitution and the declared scope disagree", wantTitle)
}

// Immutability at the boundary (§5b). A bead already governed by a valid scope
// cannot be silently retargeted by a launch that resolves a different one: the
// declaration observes present-valid-different and REJECTS before any root is
// materialized.
func TestAttachedFormulaRejectsRetargetingAnAlreadyDeclaredBead(t *testing.T) {
	dir := scopedFormulaDir(t, "attach-immutable")
	deps, agent := scopedLaunchDeps(t, dir)
	target := openTaskBead(t, deps, "already declared")

	pinned, err := targetscope.Marshal(targetscope.Scope{Branch: "pinned"})
	if err != nil {
		t.Fatalf("marshalling pinned scope: %v", err)
	}
	if err := deps.Store.SetMetadata(target.ID, beadmeta.TargetScopeMetadataKey, pinned); err != nil {
		t.Fatalf("pinning scope: %v", err)
	}

	opts := testOpts(agent, target.ID)
	opts.OnFormula = "attach-immutable"
	opts.Vars = []string{"gc_target_branch=release"}

	if _, err := DoSling(opts, deps, deps.Store); err == nil {
		t.Fatal("retargeting an already-declared bead launched, want a rejection")
	}
	got := beadScope(t, deps, target.ID)
	if got.Scope.Branch != "pinned" {
		t.Fatalf("declared scope.branch = %q after a rejected retarget, want the original pinned", got.Scope.Branch)
	}
}

// The member layer (§3a precedence 2). With no explicit branch, a bead's
// already-declared scope must decide — outranking the rig, the routing layer,
// and the formula default. Otherwise a re-sling of a governed bead resolves a
// lower layer and pins a contradiction.
func TestAttachedFormulaDeclaredMemberScopeOutranksTheFormulaDefault(t *testing.T) {
	dir := scopedFormulaDir(t, "attach-member-layer")
	deps, agent := scopedLaunchDeps(t, dir)
	target := openTaskBead(t, deps, "governed")

	pinned, err := targetscope.Marshal(targetscope.Scope{Branch: "governed-branch"})
	if err != nil {
		t.Fatalf("marshalling pinned scope: %v", err)
	}
	if err := deps.Store.SetMetadata(target.ID, beadmeta.TargetScopeMetadataKey, pinned); err != nil {
		t.Fatalf("pinning scope: %v", err)
	}

	opts := testOpts(agent, target.ID)
	opts.OnFormula = "attach-member-layer"

	if _, err := DoSling(opts, deps, deps.Store); err != nil {
		t.Fatalf("DoSling --on: %v", err)
	}

	root := launchedRoot(t, deps)
	got := targetscope.Parse(root.Metadata[beadmeta.TargetScopeMetadataKey])
	if !got.Valid() {
		t.Fatalf("root scope state = %v, want valid", got.State)
	}
	if got.Scope.Branch != "governed-branch" {
		t.Fatalf("root scope.branch = %q, want the declared member scope to outrank the formula default main", got.Scope.Branch)
	}
}

// A present-INVALID scope on the target bead is a launch rejection, never a
// fall-through to the lower layers. "I could not read the scope governing this
// bead" must not become permission to choose a new one (§2c).
func TestAttachedFormulaRejectsAnInvalidScopeOnTheTargetBead(t *testing.T) {
	dir := scopedFormulaDir(t, "attach-invalid")
	deps, agent := scopedLaunchDeps(t, dir)
	target := openTaskBead(t, deps, "corrupt scope")

	if err := deps.Store.SetMetadata(target.ID, beadmeta.TargetScopeMetadataKey, "{not json"); err != nil {
		t.Fatalf("writing corrupt scope: %v", err)
	}

	opts := testOpts(agent, target.ID)
	opts.OnFormula = "attach-invalid"

	if _, err := DoSling(opts, deps, deps.Store); err == nil {
		t.Fatal("launch proceeded against an unreadable target scope, want a rejection")
	}
}

// The batch boundary. A batch attaches the same formula to N children, and
// each child is the claimable unit for its own launch — so each must carry its
// own declaration, not merely share a root. The formula is loaded once per
// batch but the SCOPE is resolved per child, because the member layer and the
// repo dir are properties of the bead, not of the formula.
func TestBatchFormulaDeclaresScopeOnEveryChild(t *testing.T) {
	dir := scopedFormulaDir(t, "batch-scope")
	deps, agent := scopedLaunchDeps(t, dir)

	container, err := deps.Store.Create(beads.Bead{Title: "container", Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatalf("creating container: %v", err)
	}
	var childIDs []string
	for _, title := range []string{"child one", "child two"} {
		c, err := deps.Store.Create(beads.Bead{Title: title, Type: "task", Status: "open", ParentID: container.ID})
		if err != nil {
			t.Fatalf("creating %s: %v", title, err)
		}
		childIDs = append(childIDs, c.ID)
	}

	opts := testOpts(agent, container.ID)
	opts.OnFormula = "batch-scope"
	opts.Vars = []string{"gc_target_branch=release"}

	if _, err := DoSlingBatch(opts, deps, deps.Store); err != nil {
		t.Fatalf("DoSlingBatch: %v", err)
	}

	for _, id := range childIDs {
		got := beadScope(t, deps, id)
		if !got.Valid() {
			t.Fatalf("child %s scope state = %v, want a present-valid declared object", id, got.State)
		}
		if got.Scope.Branch != "release" {
			t.Fatalf("child %s scope.branch = %q, want release", id, got.Scope.Branch)
		}
	}
}
