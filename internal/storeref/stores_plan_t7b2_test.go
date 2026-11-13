package storeref

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// The T7b.2 storeref pieces: the OrderHistory intent and MoleculeAt. The
// API's order-history leg selection against today's leg builders is
// internal/api's order_history_legs_equivalence_test.go.

// orderHistoryIntents are the OrderHistory rows of the corpus, by printed name.
func orderHistoryIntents() []struct {
	name   string
	intent OrderHistory
} {
	reads, skips := WorkflowReadsSuspendedRigs, WorkflowSkipsSuspendedRigs
	return []struct {
		name   string
		intent OrderHistory
	}{
		{"One(city)", OrderHistory{Kind: OrderHistoryOne, Scope: WorkRef, SuspendedRigs: reads}},
		{"One(alpha)", OrderHistory{Kind: OrderHistoryOne, Scope: RigRef("alpha"), SuspendedRigs: reads}},
		{"One(bravo)", OrderHistory{Kind: OrderHistoryOne, Scope: RigRef("bravo"), SuspendedRigs: reads}},
		{"One(bravo,skips)", OrderHistory{Kind: OrderHistoryOne, Scope: RigRef("bravo"), SuspendedRigs: skips}},
		{"One(unopened)", OrderHistory{Kind: OrderHistoryOne, Scope: RigRef("unopened"), SuspendedRigs: reads}},
		{"One(binding ref)", OrderHistory{Kind: OrderHistoryOne, Scope: StoreRef("class:gmnos"), SuspendedRigs: reads}},
		{"One(unset)", OrderHistory{Kind: OrderHistoryOne, Scope: WorkRef}},
		{"All", OrderHistory{Kind: OrderHistoryAll, SuspendedRigs: reads}},
		{"All(skips)", OrderHistory{Kind: OrderHistoryAll, SuspendedRigs: skips}},
		{"All(unset)", OrderHistory{Kind: OrderHistoryAll}},
		{"Kind(9)", OrderHistory{Kind: OrderHistoryKind(9), SuspendedRigs: reads}},
	}
}

// t7b2StoreName names the physical store under a plan leg: a fixture store by
// its name, a gate view by its side and front.
func t7b2StoreName(s beads.Store) string {
	switch v := s.(type) {
	case taggedView:
		return v.side + "(" + t7b2StoreName(v.Store) + ")"
	case *namedStore:
		return v.name
	case nil:
		return "<nil>"
	default:
		return fmt.Sprintf("%T", s)
	}
}

// orderHistoryPlanString renders a plan row with each leg's store, so a leg
// planned under the right ref over the wrong store shows.
func orderHistoryPlanString(s Stores, in Intent) string {
	p, err := s.Plan(in)
	if err != nil {
		return "error: " + err.Error()
	}
	stores := make([]string, 0, len(p.Legs))
	for _, l := range p.Legs {
		stores = append(stores, t7b2StoreName(l.Leg.Store))
	}
	return p.String() + " @ " + strings.Join(stores, ",")
}

// TestOrderHistoryPlanCorpus pins every OrderHistory row over every corpus
// topology (storesTopologies). The rows to read first:
//
//   - T0, T2u: nothing is relocated, so the orders placement is the city
//     store and dedupes into the city leg; the scope legs are federated tails.
//   - T2: One reads [rig relic, city relic, orders binding]; All is the
//     Workflow legs (the graph binding, which is the orders binding too, so the
//     orders leg dedupes into it), then nothing more.
//   - T5o: orders on a binding apart from graph: All's orders leg stands.
//   - T2rf: a relic-free rig is dropped from One and All alike.
//   - T2s: a suspended rig is read under WorkflowReadsSuspendedRigs and
//     dropped under WorkflowSkipsSuspendedRigs.
//   - T0g, T2g: the gate's projection keeps the split shape: the orders leg
//     is the class view of the city store.
//   - T2cr: the plane refuses graph alone, so the graph leg is the refusal
//     and the orders leg the binding itself, two legs under one ref: a
//     reader tells them apart by store identity, never by ref.
//   - One(unopened) is the city and the binding, not an error.
func TestOrderHistoryPlanCorpus(t *testing.T) {
	var got []string
	for name, build := range storesTopologies() {
		f, opts := build()
		s := NewStores(f.topo, opts)
		for _, in := range orderHistoryIntents() {
			key := "OrderHistory(" + in.name + ") x " + name
			row := orderHistoryPlanString(s, in.intent)
			got = append(got, fmt.Sprintf("\t%q: `%s`,", key, row))
			if want, ok := orderHistoryCorpus[key]; !ok || want != row {
				t.Errorf("%s:\n got  %s\n want %s", key, row, want)
			}
		}
	}
	if len(got) != len(orderHistoryCorpus) || t.Failed() {
		sort.Strings(got)
		t.Errorf("the corpus pins %d rows, the axes produce %d:\n%s", len(orderHistoryCorpus), len(got), strings.Join(got, "\n"))
	}
}

// TestOrderHistoryIsNotATopologyQuestion: the plane topology planner refuses
// it, as it refuses every Stores.Plan intent.
func TestOrderHistoryIsNotATopologyQuestion(t *testing.T) {
	_, err := Plan(OrderHistory{Kind: OrderHistoryAll, SuspendedRigs: WorkflowReadsSuspendedRigs}, newT2().topo)
	if err == nil || !strings.Contains(err.Error(), "Stores.Plan") {
		t.Errorf("Plan(topology) of an OrderHistory = %v, want the Stores.Plan refusal", err)
	}
}

// TestOrderHistoryLegReaders: each leg reads in its plan's gate role over the
// leg's own store, and the strict reader declares nothing. The orders leg is
// federated wherever it stands; a relocated city's scope legs are relic.
func TestOrderHistoryLegReaders(t *testing.T) {
	for _, topo := range []string{"T2", "T2g", "T5o", "T2u"} {
		f, opts := storesTopologies()[topo]()
		s := NewStores(f.topo, opts)
		for _, in := range []OrderHistory{
			{Kind: OrderHistoryOne, Scope: RigRef("alpha"), SuspendedRigs: WorkflowReadsSuspendedRigs},
			{Kind: OrderHistoryAll, SuspendedRigs: WorkflowReadsSuspendedRigs},
		} {
			plan, err := s.Plan(in)
			if err != nil {
				t.Fatal(err)
			}
			EachTypedLeg(plan, func(l TypedLeg) {
				want := beads.FederatedLegRole
				if l.Role == RoleRelic {
					want = beads.RelicLegRole
				}
				if l.Reader.Role() != want || l.Strict.Role() != beads.NoLegRole {
					t.Errorf("%s %s %s: reader %v, strict %v; want %v, none", topo, in.Kind, legName(l.Ref), l.Reader.Role(), l.Strict.Role(), want)
				}
				if !beads.SameStore(l.Reader, l.planned.Leg.Store) || !beads.SameStore(l.Strict, l.planned.Leg.Store) {
					t.Errorf("%s %s %s: a reader is not over the leg's store", topo, in.Kind, legName(l.Ref))
				}
			})
		}
	}
}

// TestOrderHistoryOrdersLegIsTheCitysOrdersPlacement: the orders leg is the
// store under the epoch's orders handle, never a rig frame's placement (which
// under the gate is a class view of the rig's own store).
func TestOrderHistoryOrdersLegIsTheCitysOrdersPlacement(t *testing.T) {
	f, opts := storesTopologies()["T2g"]()
	s := NewStores(f.topo, opts)
	plan, err := s.Plan(OrderHistory{Kind: OrderHistoryOne, Scope: RigRef("alpha"), SuspendedRigs: WorkflowReadsSuspendedRigs})
	if err != nil {
		t.Fatal(err)
	}
	last := plan.Legs[len(plan.Legs)-1]
	if last.Role != RoleAuthority || !beads.SameStore(last.Leg.Store, s.Orders()) {
		t.Errorf("last leg %s over %s, want the Authority leg over the orders handle's store %s", legName(last.Leg.Ref), t7b2StoreName(last.Leg.Store), t7b2StoreName(s.Orders().Raw()))
	}
}

// TestMoleculeAtClassifiesTheRoot: the target's class is the root's, graph
// for a graph root, work for everything else, and the target wraps the owner
// handle whatever the class.
func TestMoleculeAtClassifiesTheRoot(t *testing.T) {
	owner := beads.MintResident(beads.NewMemStore(), "rig:alpha")
	for _, tc := range []struct {
		name  string
		root  beads.Bead
		graph bool
	}{
		{"graph.v2 root", beads.Bead{ID: "r-1", Type: "task", Metadata: map[string]string{beadmeta.FormulaContractMetadataKey: "graph.v2"}}, true},
		{"gc.kind=workflow root", beads.Bead{ID: "r-2", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}}, true},
		{"root-only wisp", beads.Bead{ID: "r-3", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}}, true},
		{"v1 molecule", beads.Bead{ID: "r-4", Type: "molecule"}, false},
		{"convoy", beads.Bead{ID: "r-5", Type: "convoy"}, false},
		{"order tracking", beads.Bead{ID: "r-6", Type: "task", Labels: []string{"order-tracking", "wisp"}}, false},
		{"zero bead", beads.Bead{}, false},
	} {
		if class := coordclass.Classify(tc.root); (class == coordclass.ClassGraph) != tc.graph {
			t.Fatalf("%s: the fixture classifies %s", tc.name, class)
		}
		target := MoleculeAt(owner, tc.root)
		if target.IsGraph() != tc.graph {
			t.Errorf("%s: IsGraph = %v, want %v", tc.name, target.IsGraph(), tc.graph)
		}
		if res, ok := target.Raw().(beads.ResidentStore); !ok || res.Ref() != "rig:alpha" || !beads.SameStore(target, owner) {
			t.Errorf("%s: the target wraps %T, want the owner handle itself", tc.name, target.Raw())
		}
		if target.Absent() {
			t.Errorf("%s: a target over an owner is absent", tc.name)
		}
	}
}

// TestMoleculeAtOfAZeroOwnerIsAbsent: no owner, no target, whatever the root.
func TestMoleculeAtOfAZeroOwnerIsAbsent(t *testing.T) {
	root := beads.Bead{ID: "r-1", Metadata: map[string]string{beadmeta.FormulaContractMetadataKey: "graph.v2"}}
	if target := MoleculeAt(beads.ResidentStore{}, root); !target.Absent() || !beads.IsNilStore(target) {
		t.Errorf("MoleculeAt(zero owner) = %+v, want the zero target", target)
	}
}

// TestMoleculeAtKeepsTheOwnersLegRoleAndFence: a target over a sweep leg's
// resident reads in that leg's role and its fenced writes resolve to the
// leg's store: the target is the owner handle, never the store beneath it.
func TestMoleculeAtKeepsTheOwnersLegRoleAndFence(t *testing.T) {
	store := newRoleStore("alpha")
	store.seed(t, "ra-1")
	owner := beads.MintResidentLeg(beads.MintRelicLeg(store, "rig:alpha"))
	target := MoleculeAt(owner, beads.Bead{ID: "ra-1", Type: "molecule"})
	if _, err := target.Get("ra-1"); err != nil {
		t.Fatal(err)
	}
	if got := store.last(t); got != beads.RelicLegRole {
		t.Errorf("a read through the target ran in role %v, want the owner leg's relic role", got)
	}
	if got := target.ConditionalWritesResolveTarget(); !beads.SameStore(got, store) {
		t.Errorf("the target's fence resolves to %T, want the leg's store", got)
	}
}

// TestMoleculeAtReadsLivePastTheOwnersCache: HandlesFor(target).Live reads
// the backing of a cached owner, so a molecule walk that asks for live rows
// still gets them (R1): the target forwards the owner's handle providers.
func TestMoleculeAtReadsLivePastTheOwnersCache(t *testing.T) {
	backing := newNamedStore("backing")
	backing.seed(t, "ga-1")
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := backing.Close("ga-1"); err != nil {
		t.Fatal(err)
	}
	target := MoleculeAt(beads.MintResident(cache, "rig:alpha"), beads.Bead{ID: "ga-1", Type: "molecule"})
	if cached, err := target.Get("ga-1"); err != nil || cached.Status == "closed" {
		t.Fatalf("fixture: the cache's copy is %q, %v; want a stale open copy", cached.Status, err)
	}
	if live, err := beads.HandlesFor(target).Live.Get("ga-1"); err != nil || live.Status != "closed" {
		t.Errorf("HandlesFor(target).Live.Get = %q, %v; want the backing's closed row", live.Status, err)
	}
}

// orderHistoryCorpus is the pinned artifact, keyed "OrderHistory(<row>) x
// <topology>"; each row is the plan and, after "@", its legs' stores.
var orderHistoryCorpus = map[string]string{
	"OrderHistory(All(skips)) x T0":          `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(All(skips)) x T0g":         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] @ class(work),work(work)`,
	"OrderHistory(All(skips)) x T2":          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work,rig:alpha,rig:bravo`,
	"OrderHistory(All(skips)) x T2cr":        `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ storeref.refusingStore,work,rig:alpha,rig:bravo,class:gmnos`,
	"OrderHistory(All(skips)) x T2d":         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work,rig:alpha,rig:bravo`,
	"OrderHistory(All(skips)) x T2g":         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class(work),work(work),work(rig:alpha),work(rig:bravo)`,
	"OrderHistory(All(skips)) x T2gp":        `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work(work),work(rig:alpha),work(rig:bravo)`,
	"OrderHistory(All(skips)) x T2rf":        `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work,rig:bravo`,
	"OrderHistory(All(skips)) x T2s":         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] @ class:gmnos,work,rig:alpha`,
	"OrderHistory(All(skips)) x T2u":         `Union(first-leg-wins): ""[FederationTail,Fatal,federated] > rig:alpha[FederationTail,Fatal,federated] > rig:bravo[FederationTail,Fatal,federated] @ work,rig:alpha,rig:bravo`,
	"OrderHistory(All(skips)) x T2wr":        `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ storeref.refusingStore,work,rig:alpha,rig:bravo`,
	"OrderHistory(All(skips)) x T2wrf":       `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work,rig:alpha,rig:bravo`,
	"OrderHistory(All(skips)) x T3":          `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] @ class:gmnos,work`,
	"OrderHistory(All(skips)) x T3k":         `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] @ class:gmnos,work`,
	"OrderHistory(All(skips)) x T5":          `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,Fatal,relic] @ class:g,work`,
	"OrderHistory(All(skips)) x T5o":         `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > class:o[Authority,Fatal,federated] @ class:g,work,class:o`,
	"OrderHistory(All(skips)) x Ts":          `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(All(unset)) x T0":          `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T0g":         `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T2":          `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T2cr":        `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T2d":         `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T2g":         `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T2gp":        `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T2rf":        `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T2s":         `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T2u":         `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T2wr":        `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T2wrf":       `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T3":          `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T3k":         `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T5":          `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x T5o":         `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All(unset)) x Ts":          `error: storeref: an OrderHistory(All) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(All) x T0":                 `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(All) x T0g":                `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] @ class(work),work(work)`,
	"OrderHistory(All) x T2":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work,rig:alpha,rig:bravo`,
	"OrderHistory(All) x T2cr":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ storeref.refusingStore,work,rig:alpha,rig:bravo,class:gmnos`,
	"OrderHistory(All) x T2d":                `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work,rig:alpha,rig:bravo`,
	"OrderHistory(All) x T2g":                `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class(work),work(work),work(rig:alpha),work(rig:bravo)`,
	"OrderHistory(All) x T2gp":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work(work),work(rig:alpha),work(rig:bravo)`,
	"OrderHistory(All) x T2rf":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work,rig:bravo`,
	"OrderHistory(All) x T2s":                `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work,rig:alpha,rig:bravo`,
	"OrderHistory(All) x T2u":                `Union(first-leg-wins): ""[FederationTail,Fatal,federated] > rig:alpha[FederationTail,Fatal,federated] > rig:bravo[FederationTail,Fatal,federated] @ work,rig:alpha,rig:bravo`,
	"OrderHistory(All) x T2wr":               `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ storeref.refusingStore,work,rig:alpha,rig:bravo`,
	"OrderHistory(All) x T2wrf":              `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > rig:alpha[Relic,Fatal,relic] > rig:bravo[Relic,Fatal,relic] @ class:gmnos,work,rig:alpha,rig:bravo`,
	"OrderHistory(All) x T3":                 `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] @ class:gmnos,work`,
	"OrderHistory(All) x T3k":                `Union(first-leg-wins): class:gmnos[Authority,Fatal,federated] > ""[Relic,Fatal,relic] @ class:gmnos,work`,
	"OrderHistory(All) x T5":                 `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,Fatal,relic] @ class:g,work`,
	"OrderHistory(All) x T5o":                `Union(first-leg-wins): class:g[Authority,Fatal,federated] > ""[Relic,Fatal,relic] > class:o[Authority,Fatal,federated] @ class:g,work,class:o`,
	"OrderHistory(All) x Ts":                 `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(Kind(9)) x T0":             `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T0g":            `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T2":             `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T2cr":           `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T2d":            `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T2g":            `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T2gp":           `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T2rf":           `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T2s":            `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T2u":            `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T2wr":           `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T2wrf":          `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T3":             `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T3k":            `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T5":             `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x T5o":            `error: storeref: unknown order history kind 9`,
	"OrderHistory(Kind(9)) x Ts":             `error: storeref: unknown order history kind 9`,
	"OrderHistory(One(alpha)) x T0":          `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(alpha)) x T0g":         `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(work),class(work)`,
	"OrderHistory(One(alpha)) x T2":          `Union(first-leg-wins): rig:alpha[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:alpha,work,class:gmnos`,
	"OrderHistory(One(alpha)) x T2cr":        `Union(first-leg-wins): rig:alpha[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:alpha,work,class:gmnos`,
	"OrderHistory(One(alpha)) x T2d":         `Union(first-leg-wins): rig:alpha[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:alpha,work,class:gmnos`,
	"OrderHistory(One(alpha)) x T2g":         `Union(first-leg-wins): rig:alpha[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(rig:alpha),work(work),class(work)`,
	"OrderHistory(One(alpha)) x T2gp":        `Union(first-leg-wins): rig:alpha[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(rig:alpha),work(work),class:gmnos`,
	"OrderHistory(One(alpha)) x T2rf":        `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(alpha)) x T2s":         `Union(first-leg-wins): rig:alpha[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:alpha,work,class:gmnos`,
	"OrderHistory(One(alpha)) x T2u":         `Union(first-leg-wins): rig:alpha[FederationTail,Fatal,federated] > ""[FederationTail,Fatal,federated] @ rig:alpha,work`,
	"OrderHistory(One(alpha)) x T2wr":        `Union(first-leg-wins): rig:alpha[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:alpha,work,storeref.refusingStore`,
	"OrderHistory(One(alpha)) x T2wrf":       `Union(first-leg-wins): rig:alpha[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:alpha,work,class:gmnos`,
	"OrderHistory(One(alpha)) x T3":          `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(alpha)) x T3k":         `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(alpha)) x T5":          `Union(first-leg-wins): ""[Relic,Fatal,relic] @ work`,
	"OrderHistory(One(alpha)) x T5o":         `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:o[Authority,Fatal,federated] @ work,class:o`,
	"OrderHistory(One(alpha)) x Ts":          `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(binding ref)) x T0":    `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T0g":   `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T2":    `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T2cr":  `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T2d":   `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T2g":   `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T2gp":  `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T2rf":  `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T2s":   `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T2u":   `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T2wr":  `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T2wrf": `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T3":    `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T3k":   `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T5":    `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x T5o":   `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(binding ref)) x Ts":    `error: storeref: an OrderHistory(One) plan's scope is the city or a rig, got "class:gmnos"`,
	"OrderHistory(One(bravo)) x T0":          `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(bravo)) x T0g":         `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(work),class(work)`,
	"OrderHistory(One(bravo)) x T2":          `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo)) x T2cr":        `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo)) x T2d":         `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo)) x T2g":         `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(rig:bravo),work(work),class(work)`,
	"OrderHistory(One(bravo)) x T2gp":        `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(rig:bravo),work(work),class:gmnos`,
	"OrderHistory(One(bravo)) x T2rf":        `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo)) x T2s":         `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo)) x T2u":         `Union(first-leg-wins): rig:bravo[FederationTail,Fatal,federated] > ""[FederationTail,Fatal,federated] @ rig:bravo,work`,
	"OrderHistory(One(bravo)) x T2wr":        `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,storeref.refusingStore`,
	"OrderHistory(One(bravo)) x T2wrf":       `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo)) x T3":          `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(bravo)) x T3k":         `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(bravo)) x T5":          `Union(first-leg-wins): ""[Relic,Fatal,relic] @ work`,
	"OrderHistory(One(bravo)) x T5o":         `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:o[Authority,Fatal,federated] @ work,class:o`,
	"OrderHistory(One(bravo)) x Ts":          `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(bravo,skips)) x T0":    `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(bravo,skips)) x T0g":   `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(work),class(work)`,
	"OrderHistory(One(bravo,skips)) x T2":    `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo,skips)) x T2cr":  `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo,skips)) x T2d":   `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo,skips)) x T2g":   `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(rig:bravo),work(work),class(work)`,
	"OrderHistory(One(bravo,skips)) x T2gp":  `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(rig:bravo),work(work),class:gmnos`,
	"OrderHistory(One(bravo,skips)) x T2rf":  `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo,skips)) x T2s":   `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(bravo,skips)) x T2u":   `Union(first-leg-wins): rig:bravo[FederationTail,Fatal,federated] > ""[FederationTail,Fatal,federated] @ rig:bravo,work`,
	"OrderHistory(One(bravo,skips)) x T2wr":  `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,storeref.refusingStore`,
	"OrderHistory(One(bravo,skips)) x T2wrf": `Union(first-leg-wins): rig:bravo[Relic,Fatal,relic] > ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ rig:bravo,work,class:gmnos`,
	"OrderHistory(One(bravo,skips)) x T3":    `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(bravo,skips)) x T3k":   `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(bravo,skips)) x T5":    `Union(first-leg-wins): ""[Relic,Fatal,relic] @ work`,
	"OrderHistory(One(bravo,skips)) x T5o":   `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:o[Authority,Fatal,federated] @ work,class:o`,
	"OrderHistory(One(bravo,skips)) x Ts":    `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(city)) x T0":           `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(city)) x T0g":          `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(work),class(work)`,
	"OrderHistory(One(city)) x T2":           `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(city)) x T2cr":         `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(city)) x T2d":          `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(city)) x T2g":          `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(work),class(work)`,
	"OrderHistory(One(city)) x T2gp":         `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(work),class:gmnos`,
	"OrderHistory(One(city)) x T2rf":         `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(city)) x T2s":          `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(city)) x T2u":          `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(city)) x T2wr":         `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,storeref.refusingStore`,
	"OrderHistory(One(city)) x T2wrf":        `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(city)) x T3":           `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(city)) x T3k":          `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(city)) x T5":           `Union(first-leg-wins): ""[Relic,Fatal,relic] @ work`,
	"OrderHistory(One(city)) x T5o":          `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:o[Authority,Fatal,federated] @ work,class:o`,
	"OrderHistory(One(city)) x Ts":           `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(unopened)) x T0":       `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(unopened)) x T0g":      `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(work),class(work)`,
	"OrderHistory(One(unopened)) x T2":       `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(unopened)) x T2cr":     `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(unopened)) x T2d":      `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(unopened)) x T2g":      `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(work),class(work)`,
	"OrderHistory(One(unopened)) x T2gp":     `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work(work),class:gmnos`,
	"OrderHistory(One(unopened)) x T2rf":     `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(unopened)) x T2s":      `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(unopened)) x T2u":      `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(unopened)) x T2wr":     `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,storeref.refusingStore`,
	"OrderHistory(One(unopened)) x T2wrf":    `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(unopened)) x T3":       `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(unopened)) x T3k":      `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:gmnos[Authority,Fatal,federated] @ work,class:gmnos`,
	"OrderHistory(One(unopened)) x T5":       `Union(first-leg-wins): ""[Relic,Fatal,relic] @ work`,
	"OrderHistory(One(unopened)) x T5o":      `Union(first-leg-wins): ""[Relic,Fatal,relic] > class:o[Authority,Fatal,federated] @ work,class:o`,
	"OrderHistory(One(unopened)) x Ts":       `Union(first-leg-wins): ""[FederationTail,Fatal,federated] @ work`,
	"OrderHistory(One(unset)) x T0":          `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T0g":         `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T2":          `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T2cr":        `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T2d":         `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T2g":         `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T2gp":        `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T2rf":        `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T2s":         `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T2u":         `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T2wr":        `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T2wrf":       `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T3":          `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T3k":         `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T5":          `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x T5o":         `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
	"OrderHistory(One(unset)) x Ts":          `error: storeref: an OrderHistory(One) plan needs the reader's choice about suspended rigs (WorkflowReadsSuspendedRigs or WorkflowSkipsSuspendedRigs), got 0`,
}
