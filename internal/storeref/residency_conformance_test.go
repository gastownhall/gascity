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
	// queueNamespacedID is a nudge-queue record: inside a namespace the nudges
	// binding HOLDS but does not mint. Its rows must match graphNamespacedID's
	// wherever a binding carries the namespace, because "who minted it" is not a
	// question the resolver asks — and must match the work-shaped rows on T5,
	// where no binding carries it.
	queueNamespacedID = "gcnq-abc-q"
)

// corpusRouter is the routed work axis the ",routed" rows plan over: the shape
// internal/api's by-id surface has, where the plane's own router — not this
// package's shadow rule — decides which work stores can hold the id.
//
// Its legs are fixed, so a row shows exactly what the intent contributed: the
// binding legs come from the topology, the work tail comes from the router, and
// no row can quietly mix the two.
func corpusRouter() WorkAxisRouter { return newCountingRouter(routedLegs()...) }

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
		{"ByID(" + queueNamespacedID + ")", ByID{ID: queueNamespacedID}},
		{"ByID(" + workShapedID + ",routed)", ByID{ID: workShapedID, WorkAxis: corpusRouter()}},
		{"ByID(" + graphNamespacedID + ",routed)", ByID{ID: graphNamespacedID, WorkAxis: corpusRouter()}},
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
	"ByID(ga-xyz,routed) x T0":            `FirstOwner: ""[WorkFallback,Fatal] > rig:routed[Shadow,Fatal]`,
	"ByID(gcg-abc,routed) x T0":           `FirstOwner: ""[WorkFallback,Fatal] > rig:routed[Shadow,Fatal]`,
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
	"ByID(ga-xyz,routed) x T1":            `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal] > rig:routed[Shadow,Fatal]`,
	"ByID(gcg-abc,routed) x T1":           `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
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
	"ByID(ga-xyz,routed) x T2":            `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal] > rig:routed[Shadow,Fatal]`,
	"ByID(gcg-abc,routed) x T2":           `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
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
	"ByID(ga-xyz,routed) x T3":            `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal] > rig:routed[Shadow,Fatal]`,
	"ByID(gcg-abc,routed) x T3":           `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
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

	// ---- T3k: T3 whose binding a durable census PROVED holds ids outside its
	// reserved namespaces. Every row is character-for-character T3's except the
	// three residence probes, which become Fatal. The carve-out above rests on
	// "this leg was only ever a probe for an id no relocated class could own",
	// and that sentence is known to be false here: the migration preserved ids,
	// so a work-shaped id CAN be resident in this binding, and the retained
	// pre-migration copy is sitting in the work store ready to answer for it.
	"ByID(gcg-abc) x T3k":                  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz) x T3k":                   `FirstOwner: class:gmnos[ResidenceProbe,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ra-7) x T3k":                     `FirstOwner: class:gmnos[ResidenceProbe,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz,routed) x T3k":            `FirstOwner: class:gmnos[ResidenceProbe,Fatal] > ""[WorkFallback,Fatal] > rig:routed[Shadow,Fatal]`,
	"ByID(gcg-abc,routed) x T3k":           `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"RoutedWork x T3k":                     "error: storage refused: run `gc storage migrate`",
	"AssignedWork(sweep) x T3k":            "error: storage refused: run `gc storage migrate`",
	"AssignedWork(claim-escalation) x T3k": "error: storage refused: run `gc storage migrate`",
	"Session x T3k":                        "error: storage refused: run `gc storage migrate`",
	"Census(all) x T3k":                    "error: storage refused: run `gc storage migrate`",
	"Census(work) x T3k":                   `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Census(graph) x T3k":                  "error: storage refused: run `gc storage migrate`",
	"Class(work) x T3k":                    `SingleOwner: ""[Authority,Fatal]`,
	"Class(graph) x T3k":                   "error: storage refused: run `gc storage migrate`",
	"Class(sessions) x T3k":                "error: storage refused: run `gc storage migrate`",

	// ---- T4: T2 with the bravo rig suspended. The constructor is TOLD which
	// rigs to include; the resolver never re-invents an excluded leg.
	"ByID(gcg-abc) x T4":                  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(ga-xyz) x T4":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"ByID(ra-7) x T4":                     `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal] > rig:alpha[Shadow,Fatal]`,
	"ByID(ga-xyz,routed) x T4":            `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal] > rig:routed[Shadow,Fatal]`,
	"ByID(gcg-abc,routed) x T4":           `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
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
	"ByID(ga-xyz,routed) x T5":            `FirstOwner: class:g[ResidenceProbe,RefusalTolerated] > class:s[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal] > rig:routed[Shadow,Fatal]`,
	"ByID(gcg-abc,routed) x T5":           `FirstOwner: class:g[Authority,Fatal] > ""[WorkFallback,Fatal]`,
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
	"ByID(ga-xyz,routed) x T6":            `FirstOwner: ""[WorkFallback,Fatal] > rig:routed[Shadow,Fatal]`,
	"ByID(gcg-abc,routed) x T6":           `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
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
	"ByID(ga-xyz,routed) x T6r":            `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal] > rig:routed[Shadow,Fatal]`,
	"ByID(gcg-abc,routed) x T6r":           `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
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

	// ---- The held-not-minted namespace, kept together because the claim is a
	// comparison rather than a shape. Wherever a binding carries "gcnq" these
	// rows are character-for-character the gcg-abc rows above: the resolver
	// grants authority over a namespace the binding HOLDS, and never asks which
	// sequence minted the id. On T5, where the two bindings carry only "gcg" and
	// "gcs", they are the ga-xyz rows instead — an unheld namespace is not a
	// half-claim, it is no claim, and the id gets the same probe order any
	// unrecognized id gets.
	"ByID(gcnq-abc-q) x T0":  `FirstOwner: ""[WorkFallback,Fatal]`,
	"ByID(gcnq-abc-q) x T1":  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(gcnq-abc-q) x T2":  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(gcnq-abc-q) x T3":  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(gcnq-abc-q) x T3k": `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(gcnq-abc-q) x T4":  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(gcnq-abc-q) x T5":  `FirstOwner: class:g[ResidenceProbe,RefusalTolerated] > class:s[ResidenceProbe,RefusalTolerated] > ""[WorkFallback,Fatal]`,
	"ByID(gcnq-abc-q) x T6":  `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
	"ByID(gcnq-abc-q) x T6r": `FirstOwner: class:gmnos[Authority,Fatal] > ""[WorkFallback,Fatal]`,
}

// runtimeCorpusIntents is the intent axis of the RUNTIME-PLANE corpus: the
// questions a controller tick, a hook or a claim asks. The plane narrows them to
// the infra binding, and these rows are what a consumer of the relevance
// descriptor actually reads.
func runtimeCorpusIntents() []struct {
	name   string
	intent Intent
} {
	return []struct {
		name   string
		intent Intent
	}{
		{"RoutedWork", RoutedWork{}},
		{"AssignedWork(sweep)", AssignedWork{}},
		{"AssignedWork(claim-escalation)", AssignedWork{Purpose: AssignedWorkClaimEscalation}},
		{"Session", Session{}},
		{"Census(all)", Census{}},
		{"ByID(" + workShapedID + ")", ByID{ID: workShapedID}},
	}
}

// residencyRuntimeCorpus pins the SAME plans narrowed to the runtime plane —
// the operator invariant as a golden table (bd memory
// gascity-runtime-infra-store-invariant).
//
// Read it beside residencyCorpus: every row here is a SUBSEQUENCE of the row
// above with the same key, never a re-ordering. That is what makes the
// descriptor a latency decision rather than a residency one, and
// TestRuntimePlaneCorpus asserts the subsequence relation on every row rather
// than trusting the two tables to be edited together.
var residencyRuntimeCorpus = map[string]string{
	// ---- T0: nothing to narrow. A city that relocates no class has no binding,
	// and there the work store IS the infra store: the rule degrades to "the only
	// store there is", never to "no store at all".
	"RoutedWork@runtime x T0":                     `Union(first-leg-wins): ""[Authority,Fatal]`,
	"AssignedWork(sweep)@runtime x T0":            `Union(first-leg-wins): ""[Authority,Fatal]`,
	"AssignedWork(claim-escalation)@runtime x T0": `FirstOwner: ""[Authority,Fatal]`,
	"Session@runtime x T0":                        `Union(first-leg-wins): ""[Authority,Fatal]`,
	"Census(all)@runtime x T0":                    `Union(first-leg-wins): ""[Authority,Fatal]`,
	"ByID(ga-xyz)@runtime x T0":                   `FirstOwner: ""[WorkFallback,Fatal]`,

	// ---- T1: the shape maintainer-city runs. Every runtime question is one
	// local sqlite read; the remote work ledger is not a leg the tick can be
	// handed. Note the ROLES are untouched — the binding is still the plan's
	// FederationTail, because narrowing does not promote a leg it kept.
	"RoutedWork@runtime x T1":                     `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(sweep)@runtime x T1":            `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation)@runtime x T1": `FirstOwner: class:gmnos[FederationTail,Fatal]`,
	"Session@runtime x T1":                        `Union(first-leg-wins): class:gmnos[Authority,Fatal]`,
	"Census(all)@runtime x T1":                    `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"ByID(ga-xyz)@runtime x T1":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated]`,

	// ---- T2/T4: the rig legs go too. A rig work store is a work ledger like any
	// other, and the invariant is about the PLANE, not about which work store.
	"RoutedWork@runtime x T2":                     `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(sweep)@runtime x T2":            `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation)@runtime x T2": `FirstOwner: class:gmnos[FederationTail,Fatal]`,
	"Session@runtime x T2":                        `Union(first-leg-wins): class:gmnos[Authority,Fatal]`,
	"Census(all)@runtime x T2":                    `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"ByID(ga-xyz)@runtime x T2":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated]`,

	// ---- T3: a refused city has no plan to narrow. The descriptor is applied
	// AFTER Plan, so the refusal still arrives first and still names its remedy —
	// narrowing must never be a way to get a work-only answer out of a refusal.
	"RoutedWork@runtime x T3":                     "error: storage refused: run `gc storage migrate`",
	"AssignedWork(sweep)@runtime x T3":            "error: storage refused: run `gc storage migrate`",
	"AssignedWork(claim-escalation)@runtime x T3": "error: storage refused: run `gc storage migrate`",
	"Session@runtime x T3":                        "error: storage refused: run `gc storage migrate`",
	"Census(all)@runtime x T3":                    "error: storage refused: run `gc storage migrate`",
	"ByID(ga-xyz)@runtime x T3":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated]`,

	// T3k narrows identically — narrowing keeps a leg's role and policy, so the
	// Fatal probe stays Fatal here too.
	"RoutedWork@runtime x T3k":                     "error: storage refused: run `gc storage migrate`",
	"AssignedWork(sweep)@runtime x T3k":            "error: storage refused: run `gc storage migrate`",
	"AssignedWork(claim-escalation)@runtime x T3k": "error: storage refused: run `gc storage migrate`",
	"Session@runtime x T3k":                        "error: storage refused: run `gc storage migrate`",
	"Census(all)@runtime x T3k":                    "error: storage refused: run `gc storage migrate`",
	"ByID(ga-xyz)@runtime x T3k":                   `FirstOwner: class:gmnos[ResidenceProbe,Fatal]`,

	"RoutedWork@runtime x T4":                     `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(sweep)@runtime x T4":            `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation)@runtime x T4": `FirstOwner: class:gmnos[FederationTail,Fatal]`,
	"Session@runtime x T4":                        `Union(first-leg-wins): class:gmnos[Authority,Fatal]`,
	"Census(all)@runtime x T4":                    `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"ByID(ga-xyz)@runtime x T4":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated]`,

	// ---- T5: per-class split. BOTH bindings survive, in the plan's order. The
	// plane says "the infra stores", not "one infra store" — a runtime reader
	// that kept only the first would lose the sessions binding's rows.
	"RoutedWork@runtime x T5":                     `Union(first-leg-wins): class:g[FederationTail,Fatal] > class:s[FederationTail,Fatal]`,
	"AssignedWork(sweep)@runtime x T5":            `Union(first-leg-wins): class:g[FederationTail,Fatal] > class:s[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation)@runtime x T5": `FirstOwner: class:g[FederationTail,Fatal] > class:s[FederationTail,Fatal]`,
	"Session@runtime x T5":                        `Union(first-leg-wins): class:s[Authority,Fatal]`,
	"Census(all)@runtime x T5":                    `Union(first-leg-wins): class:g[FederationTail,Fatal] > class:s[FederationTail,Fatal]`,
	"ByID(ga-xyz)@runtime x T5":                   `FirstOwner: class:g[ResidenceProbe,RefusalTolerated] > class:s[ResidenceProbe,RefusalTolerated]`,

	// ---- T6: the retirement shape, and the one row that is a HAZARD rather than
	// a win. With the residence probe retired, ByID of a work-shaped id plans to
	// the work leg ALONE — so there is no binding leg to narrow to and the
	// runtime plane hands back the ledger. That is correct (the bead really is
	// only reachable there) and it is exactly why the by-id path is not narrowed
	// in production: on this topology narrowing buys nothing and the ledger read
	// survives. Retiring it is resolver S5's mint-truthful work, not this
	// descriptor's.
	"RoutedWork@runtime x T6":                     `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(sweep)@runtime x T6":            `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation)@runtime x T6": `FirstOwner: class:gmnos[FederationTail,Fatal]`,
	"Session@runtime x T6":                        `Union(first-leg-wins): class:gmnos[Authority,Fatal]`,
	"Census(all)@runtime x T6":                    `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"ByID(ga-xyz)@runtime x T6":                   `FirstOwner: ""[WorkFallback,Fatal]`,

	"RoutedWork@runtime x T6r":                     `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(sweep)@runtime x T6r":            `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"AssignedWork(claim-escalation)@runtime x T6r": `FirstOwner: class:gmnos[FederationTail,Fatal]`,
	"Session@runtime x T6r":                        `Union(first-leg-wins): class:gmnos[Authority,Fatal]`,
	"Census(all)@runtime x T6r":                    `Union(first-leg-wins): class:gmnos[FederationTail,Fatal]`,
	"ByID(ga-xyz)@runtime x T6r":                   `FirstOwner: class:gmnos[ResidenceProbe,RefusalTolerated]`,
}

// TestRuntimePlaneCorpus is the golden diff for the relevance descriptor, plus
// the two properties a golden table alone cannot state: every runtime row is a
// SUBSEQUENCE of its full row, and the reconcile plane changes nothing.
func TestRuntimePlaneCorpus(t *testing.T) {
	seen := make(map[string]bool, len(residencyRuntimeCorpus))
	narrowedSomewhere := false
	for _, f := range allTopologies() {
		for _, in := range runtimeCorpusIntents() {
			key := in.name + "@runtime x " + f.name
			want, ok := residencyRuntimeCorpus[key]
			if !ok {
				t.Errorf("runtime corpus row %q is missing — every intent x topology pair must be pinned", key)
				continue
			}
			seen[key] = true

			full, err := Plan(in.intent, f.topo)
			if err != nil {
				if got := "error: " + err.Error(); got != want {
					t.Errorf("%s\n got: %s\nwant: %s", key, got, want)
				}
				continue
			}
			narrowed, err := Narrow(full, PlaneRuntime)
			if err != nil {
				t.Errorf("%s: Narrow(runtime): %v", key, err)
				continue
			}
			if got := narrowed.String(); got != want {
				t.Errorf("%s\n got: %s\nwant: %s", key, got, want)
			}
			if !isLegSubsequence(narrowed, full) {
				t.Errorf("%s: the runtime row\n %s\nis not a subsequence of the full row\n %s\n— the descriptor reordered a leg, which is the D6-in-reverse shape it exists to prevent", key, narrowed, full)
			}
			if len(narrowed.Legs) < len(full.Legs) {
				narrowedSomewhere = true
			}
			// The reconcile plane is the same plan, always.
			reconciled, err := Narrow(full, PlaneReconcile)
			if err != nil {
				t.Errorf("%s: Narrow(reconcile): %v", key, err)
				continue
			}
			if got, wantFull := reconciled.String(), full.String(); got != wantFull {
				t.Errorf("%s: the reconcile plane rendered\n %s\nwant the full plan\n %s", key, got, wantFull)
			}
		}
	}
	// Non-vacuity: if no row narrowed anything, this whole table is asserting
	// that a no-op is a no-op.
	if !narrowedSomewhere {
		t.Fatal("no runtime row dropped a leg; the corpus is pinning a descriptor that narrows nothing")
	}
	var stale []string
	for key := range residencyRuntimeCorpus {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("runtime corpus rows match no intent x topology pair (stale): %s", strings.Join(stale, ", "))
	}
	if len(seen) != len(runtimeCorpusIntents())*len(allTopologies()) {
		t.Fatalf("runtime corpus covered %d rows, want %d", len(seen), len(runtimeCorpusIntents())*len(allTopologies()))
	}
}

// isLegSubsequence reports whether narrowed's legs appear in full, in order,
// with their roles and policies unchanged.
func isLegSubsequence(narrowed, full ResolvedPlan) bool {
	if narrowed.Mode != full.Mode {
		return false
	}
	i := 0
	for _, l := range full.Legs {
		if i < len(narrowed.Legs) && narrowed.Legs[i].String() == l.String() {
			i++
		}
	}
	return i == len(narrowed.Legs)
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
