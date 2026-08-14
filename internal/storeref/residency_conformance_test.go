package storeref

import (
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

// The residency conformance corpus.
//
// Every row is (intent x topology) -> Plan.String(), written before the
// resolver and pinned here as the artifact both the demand side and the claim
// side of a migration slice assert against. A slice that changes a leg order,
// drops a leg, or re-derives an error policy changes a row, and the diff is the
// review.
//
// The failure signatures are deliberately distinct (§6): a MIS-ORDERED plan
// fails this golden diff and nothing else, while a MISSING LEG additionally
// fails the reader-agreement property in residency_agreement_test.go. Both
// facts are asserted as controls below, so a golden regeneration cannot quietly
// absorb a lost leg.

const (
	graphNamespacedID = "gcg-abc" // minted by the binding: inside the reserved namespace
	workShapedID      = "ga-xyz"  // the migrate-preserved id: outside every namespace
	rigShapedID       = "ra-7"    // inside a rig's CONFIGURED prefix: the shadow row
)

// corpusIntents is the intent axis, in a stable printed order.
func corpusIntents() []struct {
	name   string
	intent Intent
} {
	return []struct {
		name   string
		intent Intent
	}{
		{"ByID(" + graphNamespacedID + ")", ByID{ID: graphNamespacedID}},
		{"ByID(" + workShapedID + ")", ByID{ID: workShapedID}},
		{"ByID(" + rigShapedID + ")", ByID{ID: rigShapedID}},
		{"RoutedWork", RoutedWork{}},
		{"AssignedWork(sweep)", AssignedWork{}},
		{"AssignedWork(claim-escalation)", AssignedWork{Purpose: AssignedWorkClaimEscalation}},
		{"Session", Session{}},
		{"Census(all)", Census{}},
		{"Census(work)", Census{Classes: []coordclass.Class{coordclass.ClassWork}}},
		{"Census(graph)", Census{Classes: []coordclass.Class{coordclass.ClassGraph}}},
		{"Class(work)", Class{C: coordclass.ClassWork}},
		{"Class(graph)", Class{C: coordclass.ClassGraph}},
		{"Class(sessions)", Class{C: coordclass.ClassSessions}},
	}
}

// residencyCorpus is the pinned artifact, keyed "<intent> x <topology>".
var residencyCorpus = map[string]string{
	// ---- T0: single-store. The identity rows. Every intent collapses to the
	// one work leg, and ByID performs zero probes (asserted separately).
	"ByID(gcg-abc) x T0":                  `FirstOwner: ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz) x T0":                   `FirstOwner: ""[WorkFallback,Fatal]`,
	"ByID(ra-7) x T0":                     `FirstOwner: ""[WorkFallback,Fatal]`,
	"RoutedWork x T0":                     `Union(first-leg-wins): ""[Authority,Fatal]`,
	"AssignedWork(sweep) x T0":            `Union(first-leg-wins): ""[Authority,Fatal]`,
	"AssignedWork(claim-escalation) x T0": `FirstOwner: ""[Authority,Fatal]`,
	"Session x T0":                        `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Census(all) x T0":                    `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Census(work) x T0":                   `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Census(graph) x T0":                  `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Class(work) x T0":                    `SingleOwner: ""[Authority,Fatal]`,
	"Class(graph) x T0":                   `SingleOwner: ""[Authority,Fatal]`,
	"Class(sessions) x T0":                `SingleOwner: ""[Authority,Fatal]`,

	// ---- T1: whole split. The binding leads by-id inside its namespace
	// (sole minter) and is a tolerated residence PROBE outside it (migrate
	// preserves ids — ga-axin6/#5245). It is LAST on the federation intents so
	// a co-resident id answers from work, agreeing with claim.
	"ByID(gcg-abc) x T1":                  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz) x T1":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"ByID(ra-7) x T1":                     `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"RoutedWork x T1":                     `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(sweep) x T1":            `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation) x T1": `FirstOwner: ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Session x T1":                        `Union(first-leg-wins): class:gmnos[Authority,Fatal] > ""[FederationTail,Fatal]`,
	"Census(all) x T1":                    `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Census(work) x T1":                   `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Census(graph) x T1":                  `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Class(work) x T1":                    `SingleOwner: ""[Authority,Fatal]`,
	"Class(graph) x T1":                   `SingleOwner: class:gmnos[Authority,Fatal]`,
	"Class(sessions) x T1":                `SingleOwner: class:gmnos[Authority,Fatal]`,

	// ---- T2: whole split + two rigs. Rig legs are PartialDegrade on the
	// unions (a scope reporting a hole) and Fatal on claim escalation (a rig
	// read error is not proof of not-found, and escalation may only fire on
	// proof). ByID(ra-7) gains the rig as a SHADOW behind work.
	"ByID(gcg-abc) x T2":                  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz) x T2":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"ByID(ra-7) x T2":                     `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal] > rig:alpha[Shadow,Fatal]`,
	"RoutedWork x T2":                     `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > rig:bravo[FederationTail,PartialDegrade] > class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(sweep) x T2":            `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > rig:bravo[FederationTail,PartialDegrade] > class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation) x T2": `FirstOwner: ""[Authority,Fatal] > rig:alpha[Authority,Fatal] > rig:bravo[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Session x T2":                        `Union(first-leg-wins): class:gmnos[Authority,Fatal] > ""[FederationTail,Fatal] > rig:alpha[FederationTail,PartialDegrade] > rig:bravo[FederationTail,PartialDegrade]`,
	"Census(all) x T2":                    `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > rig:bravo[FederationTail,PartialDegrade] > class:gmnos[FederationTail,Fatal]`,
	"Census(work) x T2":                   `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > rig:bravo[FederationTail,PartialDegrade]`,
	"Census(graph) x T2":                  `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > rig:bravo[FederationTail,PartialDegrade] > class:gmnos[FederationTail,Fatal]`,
	"Class(work) x T2":                    `SingleOwner: ""[Authority,Fatal]`,
	"Class(graph) x T2":                   `SingleOwner: class:gmnos[Authority,Fatal]`,
	"Class(sessions) x T2":                `SingleOwner: class:gmnos[Authority,Fatal]`,

	// ---- T3: standing refusal (the deleted-[storage] trap). Every intent
	// that touches a binding fails LOUD with the refusal that names the
	// remedy — never "no work forever". The exceptions are exact and both are
	// about work: a work-only census, the work class, and the by-id carve-out
	// where a non-reserved id is still served from the work ledger.
	"ByID(gcg-abc) x T3":                  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz) x T3":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"ByID(ra-7) x T3":                     `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"RoutedWork x T3":                     "error: storage refused: run `gc storage migrate`",
	"AssignedWork(sweep) x T3":            "error: storage refused: run `gc storage migrate`",
	"AssignedWork(claim-escalation) x T3": "error: storage refused: run `gc storage migrate`",
	"Session x T3":                        "error: storage refused: run `gc storage migrate`",
	"Census(all) x T3":                    "error: storage refused: run `gc storage migrate`",
	"Census(work) x T3":                   `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Census(graph) x T3":                  "error: storage refused: run `gc storage migrate`",
	"Class(work) x T3":                    `SingleOwner: ""[Authority,Fatal]`,
	"Class(graph) x T3":                   "error: storage refused: run `gc storage migrate`",
	"Class(sessions) x T3":                "error: storage refused: run `gc storage migrate`",

	// ---- T4: T2 with the bravo rig suspended. The constructor is TOLD which
	// rigs to include; the resolver never re-invents an excluded leg.
	"ByID(gcg-abc) x T4":                  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz) x T4":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"ByID(ra-7) x T4":                     `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal] > rig:alpha[Shadow,Fatal]`,
	"RoutedWork x T4":                     `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(sweep) x T4":            `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation) x T4": `FirstOwner: ""[Authority,Fatal] > rig:alpha[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Session x T4":                        `Union(first-leg-wins): class:gmnos[Authority,Fatal] > ""[FederationTail,Fatal] > rig:alpha[FederationTail,PartialDegrade]`,
	"Census(all) x T4":                    `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > class:gmnos[FederationTail,Fatal]`,
	"Census(work) x T4":                   `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade]`,
	"Census(graph) x T4":                  `Union(first-leg-wins): ""[Authority,Fatal] > rig:alpha[FederationTail,PartialDegrade] > class:gmnos[FederationTail,Fatal]`,
	"Class(work) x T4":                    `SingleOwner: ""[Authority,Fatal]`,
	"Class(graph) x T4":                   `SingleOwner: class:gmnos[Authority,Fatal]`,
	"Class(sessions) x T4":                `SingleOwner: class:gmnos[Authority,Fatal]`,

	// ---- T5: per-class split. This build's CONSTRUCTORS cannot produce it
	// (TestTopologyConstructorsServeOnlyTheWholeSplit is the tripwire), but the
	// resolver answers for it today, so these rows are asserted rather than
	// skipped — a skip-until row rots, and the tripwire now lives in one place.
	// Note the by-id rule these rows pin: an id inside ONE binding's reserved
	// namespace probes only that binding; the reserved namespaces are disjoint
	// and a gcg- id is never resident in the sessions binding.
	"ByID(gcg-abc) x T5":                  `FirstOwner: class:g[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz) x T5":                   `FirstOwner: class:g[ResidenceProbe,RefusalTolerated] > class:s[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"ByID(ra-7) x T5":                     `FirstOwner: class:g[ResidenceProbe,RefusalTolerated] > class:s[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"RoutedWork x T5":                     `Union(first-leg-wins): ""[Authority,Fatal] > class:g[FederationTail,Fatal] > class:s[FederationTail,Fatal]`,
	"AssignedWork(sweep) x T5":            `Union(first-leg-wins): ""[Authority,Fatal] > class:g[FederationTail,Fatal] > class:s[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation) x T5": `FirstOwner: ""[Authority,Fatal] > class:g[FederationTail,Fatal] > class:s[FederationTail,Fatal]`,
	"Session x T5":                        `Union(first-leg-wins): class:s[Authority,Fatal] > ""[FederationTail,Fatal]`,
	"Census(all) x T5":                    `Union(first-leg-wins): ""[Authority,Fatal] > class:g[FederationTail,Fatal] > class:s[FederationTail,Fatal]`,
	"Census(work) x T5":                   `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Census(graph) x T5":                  `Union(first-leg-wins): ""[Authority,Fatal] > class:g[FederationTail,Fatal]`,
	"Class(work) x T5":                    `SingleOwner: ""[Authority,Fatal]`,
	"Class(graph) x T5":                   `SingleOwner: class:g[Authority,Fatal]`,
	"Class(sessions) x T5":                `SingleOwner: class:s[Authority,Fatal]`,

	// ---- T6: the retirement shape. The binding mints inside the reserved
	// namespace and holds no open legacy residents, so every NEW bead's
	// residence is decidable from its id alone and the residence probe is GONE.
	// This is the pre-written flip row of write-routing §8.1 / design §5.
	"ByID(gcg-abc) x T6":                  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz) x T6":                   `FirstOwner: ""[WorkFallback,Fatal]`,
	"ByID(ra-7) x T6":                     `FirstOwner: ""[WorkFallback,Fatal]`,
	"RoutedWork x T6":                     `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(sweep) x T6":            `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation) x T6": `FirstOwner: ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Session x T6":                        `Union(first-leg-wins): class:gmnos[Authority,Fatal] > ""[FederationTail,Fatal]`,
	"Census(all) x T6":                    `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Census(work) x T6":                   `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Census(graph) x T6":                  `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Class(work) x T6":                    `SingleOwner: ""[Authority,Fatal]`,
	"Class(graph) x T6":                   `SingleOwner: class:gmnos[Authority,Fatal]`,
	"Class(sessions) x T6":                `SingleOwner: class:gmnos[Authority,Fatal]`,

	// ---- T6r: mint-truthful but relics still open. The OTHER half of the
	// retirement pair — the probe stays, because a point-in-time "zero open
	// relics" is the only thing that may retire it.
	"ByID(gcg-abc) x T6r":                  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz) x T6r":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"ByID(ra-7) x T6r":                     `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"RoutedWork x T6r":                     `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(sweep) x T6r":            `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation) x T6r": `FirstOwner: ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Session x T6r":                        `Union(first-leg-wins): class:gmnos[Authority,Fatal] > ""[FederationTail,Fatal]`,
	"Census(all) x T6r":                    `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Census(work) x T6r":                   `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Census(graph) x T6r":                  `Union(first-leg-wins): ""[Authority,Fatal] > class:gmnos[FederationTail,Fatal]`,
	"Class(work) x T6r":                    `SingleOwner: ""[Authority,Fatal]`,
	"Class(graph) x T6r":                   `SingleOwner: class:gmnos[Authority,Fatal]`,
	"Class(sessions) x T6r":                `SingleOwner: class:gmnos[Authority,Fatal]`,
}

// TestResidencyCorpus is the golden diff: every (intent, topology) pair the
// corpus declares must render exactly the pinned plan.
func TestResidencyCorpus(t *testing.T) {
	seen := make(map[string]bool, len(residencyCorpus))
	for _, f := range allTopologies() {
		for _, in := range corpusIntents() {
			key := in.name + " x " + f.name
			want, ok := residencyCorpus[key]
			if !ok {
				t.Errorf("corpus row %q is missing — every intent x topology pair must be pinned", key)
				continue
			}
			seen[key] = true
			if got := planString(t, in.intent, f.topo); got != want {
				t.Errorf("%s\n got: %s\nwant: %s", key, got, want)
			}
		}
	}
	// Rule C, the non-empty denominator: a row left in the corpus that no
	// topology/intent pair produces is a row policing nothing.
	var stale []string
	for key := range residencyCorpus {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("corpus rows match no intent x topology pair (stale): %s", strings.Join(stale, ", "))
	}
	if len(seen) != len(corpusIntents())*len(allTopologies()) {
		t.Fatalf("corpus covered %d rows, want %d — the golden table is evaluating less than the matrix", len(seen), len(corpusIntents())*len(allTopologies()))
	}
}

// TestCorpusControlMisorderedPlanFailsGoldenOnly is the first half of the
// differently-failing pair (§6 T1 control): a plan with two legs SWAPPED
// changes the golden row and nothing else. Its leg SET is unchanged, so the
// reader-agreement property still holds over it — which is precisely why the
// golden diff has to exist as a separate signal.
func TestCorpusControlMisorderedPlanFailsGoldenOnly(t *testing.T) {
	f := newT2()
	plan := mustPlan(t, RoutedWork{}, f.topo)
	if len(plan.Legs) < 2 {
		t.Fatalf("RoutedWork x T2 has %d legs, want at least 2", len(plan.Legs))
	}
	mutated := plan
	mutated.Legs = append([]PlanLeg(nil), plan.Legs...)
	mutated.Legs[0], mutated.Legs[len(mutated.Legs)-1] = mutated.Legs[len(mutated.Legs)-1], mutated.Legs[0]

	if mutated.String() == residencyCorpus["RoutedWork x T2"] {
		t.Fatal("a mis-ordered plan still renders the golden row: the golden diff cannot see leg order")
	}
	// Same leg SET: agreement is blind to the swap, which is the point.
	if got, want := refSet(mutated), refSet(plan); !equalStringSets(got, want) {
		t.Fatalf("the control mutated the leg set (%v vs %v); it must mutate only the order", got, want)
	}
}

// TestCorpusControlMissingLegFailsGoldenAndAgreement is the second half: a
// plan with a leg REMOVED also fails the golden diff, but it is caught a
// second time by the agreement property (residency_agreement_test.go), and the
// two failures do not look alike.
func TestCorpusControlMissingLegFailsGoldenAndAgreement(t *testing.T) {
	f := newT2()
	plan := mustPlan(t, RoutedWork{}, f.topo)
	mutated := plan
	mutated.Legs = append([]PlanLeg(nil), plan.Legs[:len(plan.Legs)-1]...) // drop the binding

	if mutated.String() == residencyCorpus["RoutedWork x T2"] {
		t.Fatal("a plan missing its binding leg still renders the golden row")
	}
	if got, want := refSet(mutated), refSet(plan); equalStringSets(got, want) {
		t.Fatal("the control did not remove a leg")
	}
	// The agreement half: a bead only the dropped leg holds vanishes from
	// demand while staying claimable. Asserted here as the demand side; the
	// full property runs in TestReaderAgreement.
	binding := f.legStore(ClassRef(infraClasses))
	binding.seed(t, "gcg-only-1")
	got, err := Union(mutated, beadID, listOpenLeg)
	if err != nil {
		t.Fatalf("Union over the mutated plan: %v", err)
	}
	if containsID(got.Items, "gcg-only-1") {
		t.Fatal("the dropped binding leg still contributed rows; the fixture is not exercising the leg")
	}
	if got.Partial {
		t.Fatal("a plan that simply LACKS a leg reports a complete answer — that is the silent-shrink shape the agreement property exists to catch")
	}
}

func refSet(p ResolvedPlan) []string {
	out := make([]string, 0, len(p.Legs))
	for _, l := range p.Legs {
		out = append(out, string(l.Leg.Ref))
	}
	sort.Strings(out)
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
