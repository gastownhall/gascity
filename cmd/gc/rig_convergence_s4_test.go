package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
)

// TestRigConvergenceLivesInTheBindingOnASplitCity is W1/W2: a rig's loops are
// created in the binding stamped with their scope, its wisps follow, every
// scope lists only its own loops, and a v1 poured formula is refused naming
// the supported alternative.
func TestRigConvergenceLivesInTheBindingOnASplitCity(t *testing.T) {
	f := newS4RigFixture(t, true)
	f.markImported(t, "rb", true) // rb is relic-free; ra is awaiting import
	scopes := f.runtime().buildConvergenceScopes()
	city, ra, rb := scopes[""], scopes["ra"], scopes["rb"]
	if city == nil || ra == nil || rb == nil {
		t.Fatalf("buildConvergenceScopes = %v, want city, ra and rb", scopes)
	}
	for name, scope := range map[string]*convergenceScope{"ra": ra, "rb": rb} {
		if !sameStorePtr(scope.store, f.binding) || !scope.adapter.relocated {
			t.Errorf("rig %s convergence scope is not served from the binding (relocated=%v)", name, scope.adapter.relocated)
		}
		if scope.storePath != f.rigPath(name) {
			t.Errorf("rig %s storePath = %q, want the rig root (gate commands run there)", name, scope.storePath)
		}
	}
	if ra.adapter.relic == nil {
		t.Error("rig ra awaits import but has no relic leg")
	}
	if rb.adapter.relic != nil {
		t.Error("rig rb is proven relic-free but still has a relic leg")
	}

	cityRoot := convergenceRootIn(t, city, "city loop")
	raRoot := convergenceRootIn(t, ra, "ra loop")
	rbRoot := convergenceRootIn(t, rb, "rb loop")
	for _, tc := range []struct {
		root beads.Bead
		want string
	}{{cityRoot, "city:s4-city"}, {raRoot, "rig:ra"}, {rbRoot, "rig:rb"}} {
		got, err := f.binding.Get(tc.root.ID)
		if err != nil {
			t.Fatalf("loop %s is not in the binding: %v", tc.root.ID, err)
		}
		if ref := got.Metadata[beadmeta.RootStoreRefMetadataKey]; ref != tc.want {
			t.Errorf("loop %s gc.root_store_ref = %q, want %q", tc.root.ID, ref, tc.want)
		}
		for name, rig := range f.rigs {
			if _, err := rig.Get(tc.root.ID); !errors.Is(err, beads.ErrNotFound) {
				t.Errorf("loop %s resolves in rig %s's work store (err=%v); rig stores hold only work", tc.root.ID, name, err)
			}
		}
	}

	// Isolation: one binding, three scopes, no scope sees another's loops.
	for name, tc := range map[string]struct {
		scope *convergenceScope
		want  string
	}{"city": {city, cityRoot.ID}, "ra": {ra, raRoot.ID}, "rb": {rb, rbRoot.ID}} {
		listed, err := tc.scope.adapter.listConvergence()
		if err != nil {
			t.Fatal(err)
		}
		if ids := controlLegIDs(listed); len(ids) != 1 || ids[0] != tc.want {
			t.Errorf("scope %s lists %v, want only %s", name, ids, tc.want)
		}
	}
	for _, id := range []string{cityRoot.ID, raRoot.ID, rbRoot.ID} {
		if err := f.binding.SetMetadata(id, convergence.FieldTarget, "worker"); err != nil {
			t.Fatal(err)
		}
		if err := f.binding.SetMetadata(id, convergence.FieldState, convergence.StateActive); err != nil {
			t.Fatal(err)
		}
	}
	for name, scope := range map[string]*convergenceScope{"city": city, "ra": ra, "rb": rb} {
		if n, err := scope.adapter.CountActiveConvergenceLoops("worker"); err != nil || n != 1 {
			t.Errorf("scope %s counts %d active loop(s) for worker (err=%v), want 1; concurrency limits must not cross scopes", name, n, err)
		}
	}

	// W2: a vapor wisp follows its root into the binding, carrying the scope.
	assertConvergencePours(t, ra, raRoot, convergenceVaporFormula, 1, f.binding, "rig ra")
	children, err := f.binding.List(beads.ListQuery{ParentID: raRoot.ID})
	if err != nil || len(children) != 1 {
		t.Fatalf("ra root children = %d (err=%v), want the one wisp", len(children), err)
	}
	if ref := children[0].Metadata[beadmeta.RootStoreRefMetadataKey]; ref != "rig:ra" {
		t.Errorf("rig wisp gc.root_store_ref = %q, want rig:ra", ref)
	}

	// D6: a v1 poured formula is refused in a rig scope, naming the alternative.
	before := countBeads(t, f.binding)
	_, err = ra.adapter.PourWisp(raRoot.ID, convergencePouredFormula, "converge:"+raRoot.ID+":iter:2", nil, "")
	if err == nil {
		t.Fatal("a rig scope on a split city poured a v1 poured (work-class) formula")
	}
	for _, want := range []string{"work-class", "v1 poured", `phase = "vapor"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "move the loop to a rig scope") {
		t.Errorf("refusal %q still names a rig scope as the remedy; on a split city a rig scope refuses too", err)
	}
	if after := countBeads(t, f.binding); after != before {
		t.Errorf("the refused pour wrote to the binding: %d -> %d", before, after)
	}
	if n := countBeads(t, f.rigs["ra"]); n != 0 {
		t.Errorf("rig ra's work store holds %d bead(s) after rig convergence; it must hold only work", n)
	}
}

// TestRigConvergenceKeepsAPreS4LoopRunningUntilImport is the relic leg: a loop
// the rig created in its own store before S4 is listed by its scope and every
// by-id write follows it there; once the rig is imported, its store is not read.
func TestRigConvergenceKeepsAPreS4LoopRunningUntilImport(t *testing.T) {
	f := newS4RigFixture(t, true)
	legacy, err := f.rigs["ra"].Create(beads.Bead{Title: "pre-S4 loop", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	ra := f.runtime().buildConvergenceScopes()["ra"]
	listed, err := ra.adapter.listConvergence()
	if err != nil {
		t.Fatal(err)
	}
	if !containsID(listed, legacy.ID) {
		t.Fatalf("rig ra lists %v, missing its pre-S4 loop %s", controlLegIDs(listed), legacy.ID)
	}
	if err := ra.adapter.SetMetadata(legacy.ID, convergence.FieldState, convergence.StateActive); err != nil {
		t.Fatalf("writing the pre-S4 loop through the adapter: %v", err)
	}
	if got := beadByID(t, f.rigs["ra"], legacy.ID); got.Metadata[convergence.FieldState] != convergence.StateActive {
		t.Error("the write on a rig-resident loop did not land in the rig store that holds it")
	}
	assertConvergencePours(t, ra, legacy, convergenceVaporFormula, 1, f.rigs["ra"], "rig ra (relic)")
	if err := ra.adapter.CloseBead(legacy.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if got := beadByID(t, f.rigs["ra"], legacy.ID); got.Status != "closed" {
		t.Errorf("relic loop status = %q, want closed in the rig store", got.Status)
	}

	// After the import the rig store is never read.
	f.markImported(t, "ra", true)
	f.rigs["ra"] = failingReadStore{Store: f.rigs["ra"]}
	ra = f.runtime().buildConvergenceScopes()["ra"]
	if ra.adapter.relic != nil {
		t.Fatal("an imported rig still has a relic leg")
	}
	if _, err := ra.adapter.listConvergence(); err != nil {
		t.Errorf("listing an imported rig's loops read its work store: %v", err)
	}
}

// TestRigConvergenceFailsClosedOnAFailedRelicRead: a pre-S4 loop whose rig
// store does not answer is a failure, not a missing loop. Every by-id call
// returns the read's error and no pour writes anywhere. Read as a miss, the
// idempotency check finds no iteration, and the pour lands in the binding
// under a parent only the rig store holds or, once the rig store answers
// again, pours a second copy of an iteration that store already holds.
func TestRigConvergenceFailsClosedOnAFailedRelicRead(t *testing.T) {
	f := newS4RigFixture(t, true)
	relic := &relicGetFault{Store: f.rigs["ra"]}
	f.rigs["ra"] = relic
	ra := f.runtime().buildConvergenceScopes()["ra"]
	if ra.adapter.relic != beads.Store(relic) {
		t.Fatal("rig ra awaits import but its relic leg is not its own store")
	}
	legacy, err := relic.Create(beads.Bead{Title: "pre-S4 loop", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	assertConvergencePours(t, ra, legacy, convergenceVaporFormula, 1, relic, "rig ra (relic)")
	iter1 := "converge:" + legacy.ID + ":iter:1"
	beforeBinding, beforeRig := countBeads(t, f.binding), countBeads(t, relic)

	// One failed read, after which the rig store answers again.
	relic.fails = 1
	if id, err := ra.adapter.PourWisp(legacy.ID, convergenceVaporFormula, iter1, nil, ""); !errors.Is(err, errRelicDown) {
		t.Errorf("re-pouring iteration 1 across one failed relic read = (%q, %v), want the read's error: the idempotency check must fail closed", id, err)
	}

	// A rig store that keeps failing.
	relic.fails = -1
	mustFail := func(call string, err error) {
		t.Helper()
		if !errors.Is(err, errRelicDown) || errors.Is(err, beads.ErrNotFound) {
			t.Errorf("%s on a pre-S4 loop whose rig store does not answer = %v, want the read's error and no not-found", call, err)
		}
	}
	_, err = ra.adapter.GetBead(legacy.ID)
	mustFail("GetBead", err)
	_, err = ra.adapter.storeHolding(legacy.ID)
	mustFail("storeHolding", err)
	_, err = ra.adapter.Children(legacy.ID)
	mustFail("Children", err)
	_, _, err = ra.adapter.FindByIdempotencyKey(iter1)
	mustFail("FindByIdempotencyKey", err)
	_, err = ra.adapter.PourWisp(legacy.ID, convergenceVaporFormula, "converge:"+legacy.ID+":iter:2", nil, "")
	mustFail("PourWisp", err)
	_, err = convergeAdapterReader{adapter: ra.adapter}.Children(legacy.ID)
	mustFail("gc converge's Children", err)

	relic.fails = 0
	if after := countBeads(t, f.binding); after != beforeBinding {
		t.Errorf("pours across failed relic reads wrote %d bead(s) to the binding", after-beforeBinding)
	}
	if after := countBeads(t, relic); after != beforeRig {
		t.Errorf("pours across failed relic reads wrote %d bead(s) to the rig store", after-beforeRig)
	}
}

// TestRigConvergenceNeverServesARelicTwinOfABindingRow: an id the shared
// binding holds belongs to the binding's row, whether or not that row is this
// scope's. A rig scope that rejects the row (an unstamped import copy, which
// the city scope owns) must not fall through to its own store's twin of it,
// or two scopes drive one loop.
func TestRigConvergenceNeverServesARelicTwinOfABindingRow(t *testing.T) {
	const twin = "ra-1"
	binding := beads.NewMemStoreFrom(0, []beads.Bead{{ID: twin, Title: "unstamped binding copy", Type: "convergence", Status: "in_progress"}}, nil)
	relic := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: twin, Title: "rig copy", Type: "convergence", Status: "in_progress"},
		{ID: "ra-2", Title: "pre-S4 loop", Type: "convergence", Status: "in_progress"},
	}, nil)
	ra := newConvergenceStoreAdapter(binding, nil, true)
	ra.scopeRef = rigGraphScope("ra").rootStoreRef("")
	ra.relic = relic

	if b, err := ra.GetBead(twin); !errors.Is(err, errGraphOutOfScope) {
		t.Errorf("GetBead(%s) = (%+v, %v), want the binding row's out-of-scope refusal, not the rig store's twin", twin, b, err)
	}
	if err := ra.SetMetadata(twin, convergence.FieldState, convergence.StateActive); !errors.Is(err, errGraphOutOfScope) {
		t.Errorf("SetMetadata(%s) = %v, want the out-of-scope refusal", twin, err)
	}
	if got := beadByID(t, relic, twin); got.Metadata[convergence.FieldState] != "" {
		t.Errorf("a write on %s reached the rig store's twin (state %q)", twin, got.Metadata[convergence.FieldState])
	}
	listed, err := ra.listConvergence()
	if err != nil {
		t.Fatal(err)
	}
	if ids := controlLegIDs(listed); len(ids) != 1 || ids[0] != "ra-2" {
		t.Errorf("rig ra lists %v, want only its pre-S4 loop ra-2; the twin of a binding row is not its loop", ids)
	}
	// Control: the city scope owns the unstamped binding row, so exactly one
	// scope drives it.
	city := newConvergenceStoreAdapter(binding, nil, true)
	city.scopeRef = cityGraphScope().rootStoreRef("s4-city")
	if got, err := city.GetBead(twin); err != nil || got.ID != twin {
		t.Errorf("the city scope reads the unstamped binding row %s as (%+v, %v), want that row", twin, got, err)
	}
}

// TestSingleStoreConvergenceIsUnchanged is the single-store control: every rig
// keeps its own store, relocated=false, no stamp, and a v1 poured formula
// still pours.
func TestSingleStoreConvergenceIsUnchanged(t *testing.T) {
	f := newS4RigFixture(t, false)
	ra := f.runtime().buildConvergenceScopes()["ra"]
	if !sameStorePtr(ra.store, f.rigs["ra"]) || ra.adapter.relocated || ra.adapter.scopeRef != "" || ra.adapter.relic != nil {
		t.Fatalf("single-store rig scope = (store %T, relocated %v, scopeRef %q, relic %v), want its own store and nothing else",
			ra.store, ra.adapter.relocated, ra.adapter.scopeRef, ra.adapter.relic)
	}
	root := convergenceRootIn(t, ra, "ra loop")
	if _, ok := root.Metadata[beadmeta.RootStoreRefMetadataKey]; ok {
		t.Errorf("a single-store convergence root carries gc.root_store_ref=%q; no new metadata on single-store cities", root.Metadata[beadmeta.RootStoreRefMetadataKey])
	}
	assertConvergencePours(t, ra, root, convergencePouredFormula, 1, f.rigs["ra"], "rig ra")
}

// TestConvergeCLIReadsTheScopeTheControllerWrites is the reader half of W1:
// `gc converge` lists and gets a rig scope from the binding (scope-filtered),
// unions the relic leg until the rig is imported, and reads nothing else after.
func TestConvergeCLIReadsTheScopeTheControllerWrites(t *testing.T) {
	f := newS4RigFixture(t, true)
	scopes := f.runtime().buildConvergenceScopes()
	raRoot := convergenceRootIn(t, scopes["ra"], "ra loop")
	convergenceRootIn(t, scopes["rb"], "rb loop")
	convergenceRootIn(t, scopes[""], "city loop")
	legacy, err := f.rigs["ra"].Create(beads.Bead{Title: "pre-S4 loop", Type: "convergence"})
	if err != nil {
		t.Fatal(err)
	}

	reader := convergeScopeStore(f.cityPath, "ra", f.rigPath("ra"), f.rigs["ra"])
	listed, err := reader.List(beads.ListQuery{Type: "convergence"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := controlLegIDs(listed); len(ids) != 2 || !containsID(listed, raRoot.ID) || !containsID(listed, legacy.ID) {
		t.Errorf("gc converge list --rig ra = %v, want exactly %s (binding) and %s (relic)", ids, raRoot.ID, legacy.ID)
	}
	for _, id := range []string{raRoot.ID, legacy.ID} {
		if _, err := reader.Get(id); err != nil {
			t.Errorf("gc converge status %s: %v", id, err)
		}
	}

	f.markImported(t, "ra", true)
	reader = convergeScopeStore(f.cityPath, "ra", f.rigPath("ra"), failingReadStore{Store: f.rigs["ra"]})
	listed, err = reader.List(beads.ListQuery{Type: "convergence"})
	if err != nil {
		t.Fatalf("an imported rig's converge list read its work store: %v", err)
	}
	if ids := controlLegIDs(listed); len(ids) != 1 || ids[0] != raRoot.ID {
		t.Errorf("gc converge list --rig ra after import = %v, want only %s", ids, raRoot.ID)
	}

	single := newS4RigFixture(t, false)
	if got := convergeScopeStore(single.cityPath, "ra", single.rigPath("ra"), single.rigs["ra"]); got != convergeScopeReader(single.rigs["ra"]) {
		t.Errorf("single-store converge reader = %T, want the rig store itself", got)
	}
}

// TestConvergenceScopesRefuseAnotherScopesLoop is the isolation half of the
// shared binding: the city scope and a second rig scope must fail every by-id
// call on rig ra's loop — no read, no write, no index entry, no pour — so a
// `gc converge iterate|approve|stop|retry <id>` with the wrong --rig cannot
// drive another scope's loop.
func TestConvergenceScopesRefuseAnotherScopesLoop(t *testing.T) {
	f := newS4RigFixture(t, true)
	scopes := f.runtime().buildConvergenceScopes()
	raRoot := convergenceRootIn(t, scopes["ra"], "ra loop")
	for _, field := range [][2]string{{convergence.FieldState, convergence.StateActive}, {convergence.FieldTarget, "worker"}, {convergence.FieldMaxIterations, "3"}, {convergence.FieldFormula, convergenceVaporFormula}, {convergence.FieldGateMode, convergence.GateModeManual}} {
		if err := f.binding.SetMetadata(raRoot.ID, field[0], field[1]); err != nil {
			t.Fatal(err)
		}
	}
	before := beadByID(t, f.binding, raRoot.ID)

	for _, name := range []string{"", "rb"} {
		scope := scopes[name]
		a := scope.adapter
		if err := a.populateIndex(); err != nil {
			t.Fatal(err)
		}
		mustRefuse := func(call string, err error) {
			t.Helper()
			if err == nil {
				t.Errorf("scope %q: %s on rig ra's loop %s succeeded; another scope's loop must read as not found", name, call, raRoot.ID)
			} else if !errors.Is(err, beads.ErrNotFound) {
				t.Errorf("scope %q: %s = %v, want a not-found", name, call, err)
			}
		}
		_, err := a.GetBead(raRoot.ID)
		mustRefuse("GetBead", err)
		_, err = a.GetMetadata(raRoot.ID)
		mustRefuse("GetMetadata", err)
		mustRefuse("SetMetadata", a.SetMetadata(raRoot.ID, convergence.FieldState, convergence.StateActive))
		mustRefuse("CloseBead", a.CloseBead(raRoot.ID, "x"))
		mustRefuse("DeleteBead", a.DeleteBead(raRoot.ID))
		_, err = a.Children(raRoot.ID)
		mustRefuse("Children", err)
		_, _, err = a.FindByIdempotencyKey("converge:" + raRoot.ID + ":iter:1")
		mustRefuse("FindByIdempotencyKey", err)
		_, err = a.PourWisp(raRoot.ID, convergenceVaporFormula, "converge:"+raRoot.ID+":iter:1", nil, "")
		mustRefuse("PourWisp", err)
		mustRefuse("ActivateWisp", a.ActivateWisp(raRoot.ID))
		if _, indexed := a.activeIndex[raRoot.ID]; indexed {
			t.Errorf("scope %q indexed rig ra's loop into its active set; it would be ticked twice", name)
		}
		ctx := context.Background()
		_, err = scope.handler.ApproveHandler(ctx, raRoot.ID, "op", "")
		mustRefuse("ApproveHandler", err)
		_, err = scope.handler.IterateHandler(ctx, raRoot.ID, "op", "")
		mustRefuse("IterateHandler", err)
		_, err = scope.handler.StopHandler(ctx, raRoot.ID, "op", "")
		mustRefuse("StopHandler", err)
		_, err = scope.handler.RetryHandler(ctx, raRoot.ID, "op", 1)
		mustRefuse("RetryHandler", err)
	}
	after := beadByID(t, f.binding, raRoot.ID)
	if after.Status != before.Status || len(after.Metadata) != len(before.Metadata) {
		t.Errorf("rig ra's loop changed under other scopes' calls: %+v -> %+v", before, after)
	}
	if kids, err := f.binding.List(beads.ListQuery{ParentID: raRoot.ID}); err != nil || len(kids) != 0 {
		t.Errorf("other scopes poured %d wisp(s) under rig ra's loop (err=%v)", len(kids), err)
	}
	// Control: rig ra's own scope still reaches it.
	if _, err := scopes["ra"].adapter.GetBead(raRoot.ID); err != nil {
		t.Fatalf("rig ra cannot read its own loop: %v", err)
	}
}

// TestPreS4PouredRigLoopKeepsPouringWhereItLives: an in-flight v1 poured loop
// in the rig's own store pours its next iteration there (the rig's work
// ledger, so nothing is stranded), while a NEW rig loop with the same formula
// is refused.
func TestPreS4PouredRigLoopKeepsPouringWhereItLives(t *testing.T) {
	f := newS4RigFixture(t, true)
	legacy, err := f.rigs["ra"].Create(beads.Bead{Title: "pre-S4 poured loop", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	ra := f.runtime().buildConvergenceScopes()["ra"]
	before := countBeads(t, f.binding)
	assertConvergencePours(t, ra, legacy, convergencePouredFormula, 1, f.rigs["ra"], "rig ra (pre-S4 poured)")
	if after := countBeads(t, f.binding); after != before {
		t.Errorf("a relic loop's poured iteration wrote %d bead(s) to the binding", after-before)
	}
	fresh := convergenceRootIn(t, ra, "new ra loop")
	if _, err := ra.adapter.PourWisp(fresh.ID, convergencePouredFormula, "converge:"+fresh.ID+":iter:1", nil, ""); err == nil {
		t.Error("a NEW rig loop poured a v1 poured formula into the binding")
	}
}

// TestRigAtCityRootHasNoRelicLeg: a rig registered at the city root has the
// city's store, whose convergence rows are the city's retained copies; it must
// not read them as its own loops (design §3.2), in the controller or the CLI.
func TestRigAtCityRootHasNoRelicLeg(t *testing.T) {
	f := newS4RigFixture(t, true)
	f.cfg.Rigs = append(f.cfg.Rigs, config.Rig{Name: "root", Path: f.cityPath, Prefix: "gc"})
	f.rigs["root"] = f.work
	retained, err := f.work.Create(beads.Bead{Title: "retained city loop", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	scope := f.runtime().buildConvergenceScopes()["root"]
	if scope == nil {
		t.Fatal("no scope for the city-root rig")
	}
	if scope.adapter.relic != nil {
		t.Fatal("a rig whose store is the city's got a relic leg")
	}
	if _, err := scope.adapter.GetBead(retained.ID); err == nil {
		t.Error("the city-root rig reads the city's retained loop copy")
	}
	reader := convergeScopeStore(f.cityPath, "root", f.cityPath, f.work)
	listed, err := reader.List(beads.ListQuery{Type: "convergence"})
	if err != nil {
		t.Fatal(err)
	}
	if containsID(listed, retained.ID) {
		t.Error("gc converge list --rig root lists the city's retained loop copy")
	}
}
