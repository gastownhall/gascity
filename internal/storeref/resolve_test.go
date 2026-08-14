package storeref

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// ---------------------------------------------------------------------------
// §6 T3 — fail-loud. A read error is never absence; a federation leg never
// degrades silently.

func TestUnionBindingLegFailureIsFatalAndNamesTheStore(t *testing.T) {
	f := newT2()
	f.work.seed(t, "ga-1")
	boom := errors.New("binding unreachable")
	f.legStore(ClassRef(infraClasses)).listErr = boom

	plan := mustPlan(t, RoutedWork{}, f.topo)
	_, err := Union(plan, beadID, listOpenLeg)
	if err == nil {
		t.Fatal("a failed BINDING leg produced a successful union: that is the silent federation downgrade (D5)")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("union error does not wrap the leg failure: %v", err)
	}
	if !strings.Contains(err.Error(), string(ClassRef(infraClasses))) {
		t.Fatalf("union error does not name the failing store: %v", err)
	}
}

// A fatal leg failure still carries the degraded legs collected before it:
// without them "the graph plane is down" and "every store is down" read alike.
func TestUnionFatalFailureRetainsTheEarlierLegDiagnosis(t *testing.T) {
	f := newT2()
	f.work.seed(t, "ga-1")
	f.rigs["alpha"].listErr = errors.New("rig unreachable")
	f.legStore(ClassRef(infraClasses)).listErr = errors.New("binding unreachable")

	got, err := Union(mustPlan(t, RoutedWork{}, f.topo), beadID, listOpenLeg)
	if err == nil {
		t.Fatal("the fatal binding leg did not fail the union")
	}
	if !got.Partial {
		t.Fatal("the failed result is not marked Partial")
	}
	if len(got.LegErrors) != 1 || got.LegErrors[0].Ref != RigRef("alpha") {
		t.Fatalf("LegErrors = %v, want the degraded rig collected before the fatal leg", got.LegErrors)
	}
	if len(got.Items) != 0 {
		t.Fatalf("a failed union returned %d rows; the answer is the error, not a short array", len(got.Items))
	}
}

// The census vocabulary must not shift under a mixed-version city: every row
// written before the binding ref existed carries "" for the city work store.
func TestWorkRefStaysTheEmptyString(t *testing.T) {
	if WorkRef != "" {
		t.Fatalf("WorkRef = %q; widening it rewrites every persisted census row", WorkRef)
	}
	plan := mustPlan(t, RoutedWork{}, newT2().topo)
	want := []StoreRef{WorkRef, RigRef("alpha"), RigRef("bravo"), ClassRef(infraClasses)}
	got := plan.Refs()
	if len(got) != len(want) {
		t.Fatalf("Refs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Refs() = %v, want %v", got, want)
		}
	}
}

// Control: the SAME failure on a rig leg produces a different OUTCOME TYPE — a
// retained, Partial-marked result rather than an error. If both legs failed the
// same way the fail-loud rule would be untestable.
func TestUnionRigLegFailureIsRetainedAsPartial(t *testing.T) {
	f := newT2()
	f.work.seed(t, "ga-1")
	f.legStore(ClassRef(infraClasses)).seed(t, "gcg-1")
	boom := errors.New("rig unreachable")
	f.rigs["alpha"].listErr = boom

	plan := mustPlan(t, RoutedWork{}, f.topo)
	got, err := Union(plan, beadID, listOpenLeg)
	if err != nil {
		t.Fatalf("a failed RIG leg failed the whole union: %v", err)
	}
	if !got.Partial {
		t.Fatal("a failed rig leg produced a result not marked Partial — a smaller-legged answer presented as authoritative")
	}
	if len(got.LegErrors) != 1 || got.LegErrors[0].Ref != RigRef("alpha") {
		t.Fatalf("LegErrors = %v, want exactly the alpha rig", got.LegErrors)
	}
	if !containsID(got.Items, "ga-1") || !containsID(got.Items, "gcg-1") {
		t.Fatalf("the healthy legs' rows were dropped: %v", idsOf(got.Items))
	}
}

func TestRefusedTopologyFailsLoudWithTheRemedy(t *testing.T) {
	f := newT3()
	for _, in := range []Intent{RoutedWork{}, AssignedWork{}, AssignedWork{Purpose: AssignedWorkClaimEscalation}, Session{}, Census{}, Class{C: coordclass.ClassGraph}} {
		_, err := Plan(in, f.topo)
		if err == nil {
			t.Fatalf("Plan(%T) over a refused topology succeeded — the deleted-[storage] trap answers \"no work forever\"", in)
		}
		if !errors.Is(err, f.topo.Refused) {
			t.Fatalf("Plan(%T) error %v does not carry the standing refusal that names the remedy", in, err)
		}
	}
	// Work is untouched by a refusal: it still serves from its work ledger.
	for _, in := range []Intent{Class{C: coordclass.ClassWork}, Census{Classes: []coordclass.Class{coordclass.ClassWork}}} {
		if _, err := Plan(in, f.topo); err != nil {
			t.Fatalf("Plan(%T) over a refused topology failed, but a refused city still serves WORK: %v", in, err)
		}
	}
}

// Control: T0 never constructs a refusal, so the refusal rows cannot be passing
// because every topology refuses.
func TestSingleStoreTopologyNeverRefuses(t *testing.T) {
	f := newT0()
	if f.topo.Refused != nil {
		t.Fatalf("T0 carries a refusal: %v", f.topo.Refused)
	}
	for _, in := range []Intent{RoutedWork{}, AssignedWork{}, Session{}, Census{}, Class{C: coordclass.ClassGraph}, ByID{ID: workShapedID}} {
		if _, err := Plan(in, f.topo); err != nil {
			t.Fatalf("Plan(%T) over a single-store topology failed: %v", in, err)
		}
	}
}

// ---------------------------------------------------------------------------
// §6 T4 — the residence probe. `gc storage migrate` preserves ids, so a
// relocated bead keeps its work-era prefix; the namespace gate cannot decide
// and a PROBE must.

func TestResidenceProbePinsTheBindingForAMigratePreservedID(t *testing.T) {
	f := newT1()
	binding := f.legStore(ClassRef(infraClasses))
	binding.seed(t, workShapedID)

	plan := mustPlan(t, ByID{ID: workShapedID}, f.topo)
	store, ref, err := ResolveOwner(plan, workShapedID)
	if err != nil {
		t.Fatalf("ResolveOwner: %v", err)
	}
	if ref != ClassRef(infraClasses) {
		t.Fatalf("pinned %q (%s), want the binding: nil-from-the-namespace-gate must never mean \"work owns it\"", ref, storeNameOf(store))
	}
}

// Control: the SAME id, NOT in the binding, pins work — and produces no error
// at all, so "the probe found nothing" and "the probe failed" stay distinct.
func TestResidenceProbeMissPinsWorkWithoutError(t *testing.T) {
	f := newT1()
	plan := mustPlan(t, ByID{ID: workShapedID}, f.topo)
	store, ref, err := ResolveOwner(plan, workShapedID)
	if err != nil {
		t.Fatalf("a clean probe miss produced an error: %v", err)
	}
	if ref != WorkRef {
		t.Fatalf("pinned %q (%s), want the work leg", ref, storeNameOf(store))
	}
	if *f.work.gets != 0 {
		t.Fatalf("the work leg was probed %d times; it is the RESIDUAL answer and the caller's own read must produce its own error", *f.work.gets)
	}
}

// Control 2: the standing refusal is tolerated for a non-reserved id (a refused
// city still serves WORK) and surfaces for a reserved one (only the binding
// could own it). The two fail differently by construction.
func TestStandingRefusalIsToleratedOnlyOutsideTheReservedNamespace(t *testing.T) {
	f := newT3()

	plan := mustPlan(t, ByID{ID: workShapedID}, f.topo)
	_, ref, err := ResolveOwner(plan, workShapedID)
	if err != nil {
		t.Fatalf("a refused city must still answer a work-shaped id from its work ledger: %v", err)
	}
	if ref != WorkRef {
		t.Fatalf("pinned %q, want the work leg", ref)
	}

	reserved := mustPlan(t, ByID{ID: graphNamespacedID}, f.topo)
	if _, _, err := ResolveOwner(reserved, graphNamespacedID); err == nil {
		t.Fatal("a reserved-prefix id on a refused city resolved successfully; the refusal must surface")
	} else if !errors.Is(err, f.topo.Refused) {
		t.Fatalf("the surfaced error is not the refusal: %v", err)
	}
}

// ---------------------------------------------------------------------------
// §6 T5 — the identity fast-path. A single-store city pays nothing for a seam
// it cannot use (the ga-4qdfn short-circuit, as a resolver property).

func TestIdentityFastPathPerformsZeroProbes(t *testing.T) {
	f := newT0()
	f.work.seed(t, workShapedID)
	plan := mustPlan(t, ByID{ID: workShapedID}, f.topo)
	f.resetGets()

	store, ref, err := ResolveOwner(plan, workShapedID)
	if err != nil {
		t.Fatalf("ResolveOwner: %v", err)
	}
	if ref != WorkRef || store != beads.Store(f.work) {
		t.Fatalf("pinned %q (%s), want the caller's own work store back unchanged", ref, storeNameOf(store))
	}
	if got := f.totalGets(); got != 0 {
		t.Fatalf("a single-store topology performed %d probes, want 0", got)
	}
}

// Control: the counter works — a split topology performs at least one probe.
func TestSplitTopologyPerformsAtLeastOneProbe(t *testing.T) {
	f := newT1()
	plan := mustPlan(t, ByID{ID: workShapedID}, f.topo)
	f.resetGets()
	if _, _, err := ResolveOwner(plan, workShapedID); err != nil {
		t.Fatalf("ResolveOwner: %v", err)
	}
	if got := f.totalGets(); got < 1 {
		t.Fatalf("a split topology performed %d probes; the probe counter is not measuring anything", got)
	}
}

// ---------------------------------------------------------------------------
// §6 T8 — the retirement flip, as a differently-failing pair.

func TestResidenceProbeRetirementFlip(t *testing.T) {
	retired := mustPlan(t, ByID{ID: workShapedID}, newT6().topo)
	if len(retired.Legs) != 1 || retired.Legs[0].Role != RoleWorkFallback {
		t.Fatalf("mint-truthful binding with zero open relics still probes: %s", retired)
	}
	present := mustPlan(t, ByID{ID: workShapedID}, newT6Relics().topo)
	if len(present.Legs) != 2 || present.Legs[0].Role != RoleResidenceProbe {
		t.Fatalf("mint-truthful binding with OPEN relics dropped the probe: %s", present)
	}
}

// ---------------------------------------------------------------------------
// Executor guards.

func TestResolveOwnerRejectsAMismatchedID(t *testing.T) {
	f := newT1()
	plan := mustPlan(t, ByID{ID: graphNamespacedID}, f.topo)
	if _, _, err := ResolveOwner(plan, workShapedID); err == nil {
		t.Fatal("executing a ByID plan against a DIFFERENT id succeeded; the plan is id-specific")
	}
}

func TestResolveOwnerRejectsAUnionPlan(t *testing.T) {
	f := newT1()
	plan := mustPlan(t, RoutedWork{}, f.topo)
	if _, _, err := ResolveOwner(plan, workShapedID); err == nil {
		t.Fatal("ResolveOwner accepted a Union plan; the mode decides the executor")
	}
}

func TestUnionRejectsAFirstOwnerPlan(t *testing.T) {
	f := newT1()
	plan := mustPlan(t, ByID{ID: workShapedID}, f.topo)
	if _, err := Union(plan, beadID, listOpenLeg); err == nil {
		t.Fatal("Union accepted a FirstOwner plan; the mode decides the executor")
	}
}

func TestUnionDedupesFirstLegWins(t *testing.T) {
	f := newT2()
	f.work.seed(t, "ga-dup")
	f.rigs["alpha"].seed(t, "ga-dup")
	plan := mustPlan(t, RoutedWork{}, f.topo)
	got, err := Union(plan, beadID, listOpenLeg)
	if err != nil {
		t.Fatalf("Union: %v", err)
	}
	if n := len(got.Items); n != 1 {
		t.Fatalf("a co-resident id produced %d rows, want 1 (first-leg-wins): %v", n, idsOf(got.Items))
	}
}

func TestPlanByIDRejectsAnEmptyID(t *testing.T) {
	if _, err := Plan(ByID{}, newT1().topo); err == nil {
		t.Fatal("Plan(ByID{}) with no id succeeded")
	}
}

// A refusal with nowhere to attach it must not degrade to a work-only plan.
// ById and Class are the two intents that would otherwise find no binding to
// touch and quietly take the "work owns it" branch on a refused city.
func TestPlanRejectsARefusalWithNoBinding(t *testing.T) {
	refusal := newRefusal()
	topo := Topology{Work: Leg{Ref: WorkRef, Store: newNamedStore("work")}, Refused: refusal}
	for _, in := range []Intent{ByID{ID: workShapedID}, Class{C: coordclass.ClassGraph}, Class{C: coordclass.ClassWork}, RoutedWork{}, Census{Classes: []coordclass.Class{coordclass.ClassWork}}} {
		got, err := Plan(in, topo)
		if err == nil {
			t.Errorf("Plan(%T) over a refused topology with no binding returned %s; a refused city must never get a work-only plan", in, got)
			continue
		}
		if !errors.Is(err, refusal) {
			t.Errorf("Plan(%T) error %v does not carry the refusal", in, err)
		}
	}
}

// A topology with no work store has nothing to read. Returning a legless plan
// would let Union report zero rows as a complete answer.
func TestPlanRejectsALeglessTopology(t *testing.T) {
	for _, in := range []Intent{ByID{ID: workShapedID}, RoutedWork{}, AssignedWork{}, Session{}, Census{}, Class{C: coordclass.ClassWork}} {
		if _, err := Plan(in, Topology{}); err == nil {
			t.Errorf("Plan(%T) over an empty topology succeeded", in)
		}
	}
}

// ---------------------------------------------------------------------------
// ClassCandidates is now the ByID intent's in-namespace arm. The delegation
// must be exact: the existing class_candidates_test.go rows are the byte-
// identity pin, and this asserts the two answers are literally the same list.

func TestClassCandidatesIsTheByIDInNamespaceArm(t *testing.T) {
	f := newT1()
	binding := f.legStore(ClassRef(infraClasses))

	got := ClassCandidates(graphNamespacedID, ClassRouting{Prefix: "gcg", Class: binding, Work: f.work})
	plan := mustPlan(t, ByID{ID: graphNamespacedID}, f.topo)

	want := make([]beads.Store, 0, len(plan.Legs))
	for _, l := range plan.Legs {
		want = append(want, l.Leg.Store)
	}
	if len(got) != len(want) {
		t.Fatalf("ClassCandidates returned %d stores, the ByID plan has %d legs", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("leg %d: ClassCandidates gave %s, the ByID plan gave %s", i, storeNameOf(got[i]), storeNameOf(want[i]))
		}
	}
}

// ---------------------------------------------------------------------------
// The StoreRef vocabulary. A binding ref is derived from its class SET, so two
// bindings can never collide and the wake filter can name one.

func TestClassRefIsStableAndCollisionFree(t *testing.T) {
	seen := map[StoreRef]string{}
	for _, c := range coordclass.Classes() {
		if !c.IsInfrastructure() {
			continue
		}
		ref := ClassRef([]coordclass.Class{c})
		if prev, dup := seen[ref]; dup {
			t.Fatalf("classes %q and %q render the same ref %q; the ref must identify a binding uniquely", prev, c, ref)
		}
		seen[ref] = c.String()
	}
	// Order-independent: the ref names a SET.
	a := ClassRef([]coordclass.Class{coordclass.ClassSessions, coordclass.ClassGraph})
	b := ClassRef([]coordclass.Class{coordclass.ClassGraph, coordclass.ClassSessions})
	if a != b {
		t.Fatalf("ClassRef is order-dependent: %q != %q", a, b)
	}
	if got, want := ClassRef(infraClasses), StoreRef("class:gmnos"); got != want {
		t.Fatalf("whole-split ref = %q, want %q", got, want)
	}
}

func TestRigRefRoundTripsThroughScopeRigContext(t *testing.T) {
	rig, ok := ScopeRigContext(string(RigRef("alpha")))
	if !ok || rig != "alpha" {
		t.Fatalf("ScopeRigContext(%q) = (%q, %v), want (\"alpha\", true)", RigRef("alpha"), rig, ok)
	}
}

// ---------------------------------------------------------------------------
// Plan rendering is the corpus's artifact, so its own shape is pinned here.

func TestPlanStringShape(t *testing.T) {
	p := ResolvedPlan{Mode: ModeFirstOwner, Legs: []PlanLeg{
		{Leg: Leg{Ref: "class:g"}, Role: RoleAuthority, OnError: PolicyFatal},
		{Leg: Leg{Ref: WorkRef}, Role: RoleWorkFallback, OnError: PolicyFatal},
	}}
	if got, want := p.String(), `FirstOwner: class:g[Authority,Fatal] > ""[WorkFallback,Fatal]`; got != want {
		t.Fatalf("Plan.String() = %q, want %q", got, want)
	}
	if got, want := (ResolvedPlan{Mode: ModeUnion}).String(), "Union(first-leg-wins): <no legs>"; got != want {
		t.Fatalf("empty plan renders %q, want %q", got, want)
	}
}

// enumerateRoles keeps every Role and ErrPolicy printable: an unnamed constant
// would render as a number and silently rewrite every corpus row.
func TestEveryRoleAndPolicyRenders(t *testing.T) {
	for r := RoleAuthority; r <= RoleFederationTail; r++ {
		if s := r.String(); s == "" || strings.HasPrefix(s, "Role(") {
			t.Errorf("Role(%d) renders %q", int(r), s)
		}
	}
	for p := PolicyFatal; p <= PolicyRefusalTolerated; p++ {
		if s := p.String(); s == "" || strings.HasPrefix(s, "ErrPolicy(") {
			t.Errorf("ErrPolicy(%d) renders %q", int(p), s)
		}
	}
	for m := ModeFirstOwner; m <= ModeSingleOwner; m++ {
		if s := m.String(); s == "" || strings.HasPrefix(s, "Mode(") {
			t.Errorf("Mode(%d) renders %q", int(m), s)
		}
	}
}

// The corpus is only as good as its coverage of the intent set: a seventh
// intent added without rows would be untested.
func TestEveryIntentHasCorpusRows(t *testing.T) {
	declared := map[string]bool{}
	for _, in := range corpusIntents() {
		declared[fmt.Sprintf("%T", in.intent)] = true
	}
	for _, in := range []Intent{ByID{}, RoutedWork{}, AssignedWork{}, Session{}, Census{}, Class{}} {
		if !declared[fmt.Sprintf("%T", in)] {
			t.Errorf("intent %T has no corpus rows", in)
		}
	}
	var names []string
	for n := range declared {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) != 6 {
		t.Fatalf("corpus covers %d intent types (%v), want the six census intents", len(names), names)
	}
}
