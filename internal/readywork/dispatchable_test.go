package readywork_test

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/readywork"
)

// The four exclusion classes, one test each. city-qgqc records the measurement
// that motivated them: on this city's gk store at 2026-11-06T15:1xZ, 40 raw
// ready rows classified to 0 dispatchable, and every one of the four classes
// was present. Each case below is a row taken from that census, reduced to the
// fields that classify it.

// TestDispatchableExcludesSyntheticConvoyCarriers pins the class that
// dominates a busy city: gc mints one convoy carrier per dispatch, they are
// open by construction, and they sort ahead of real work — so a count that
// admits them grows with throughput and never falls.
func TestDispatchableExcludesSyntheticConvoyCarriers(t *testing.T) {
	cases := map[string]readywork.ReadyBead{
		"carrier by issue type": {
			ID:   "gk-h9sxj",
			Type: "convoy",
		},
		"carrier by synthetic stamp": {
			// A convoy whose type the surface does not report still carries the
			// stamp gc writes when it mints one.
			ID:       "gk-synth",
			Type:     "task",
			Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
		},
		// The other container types gc mints open. Excluding only convoys
		// recreates the same inflation one layer down.
		"epic container": {ID: "gk-epic", Type: "epic"},
		"molecule root":  {ID: "gk-mol", Type: "molecule"},
		"milestone":      {ID: "gk-mile", Type: "milestone"},
	}
	// The stamp is convoy.IsSynthetic's rule verbatim: only "true" is synthetic.
	// A blank or "false" value is a cleared stamp, not a carrier.
	cleared := readywork.ReadyBead{ID: "gk-cleared", Type: "task", Metadata: map[string]string{
		beadmeta.SyntheticMetadataKey: "false",
	}}
	if got := readywork.Classify(cleared); !got.Dispatchable {
		t.Fatalf("a cleared gc.synthetic classified %q; only \"true\" mints a carrier", got.Class)
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			got := readywork.Classify(row)
			if got.Dispatchable {
				t.Fatalf("%s classified dispatchable; a container is not work a worker can be handed", name)
			}
			if got.Class != readywork.ClassSynthetic {
				t.Fatalf("class = %q, want %q", got.Class, readywork.ClassSynthetic)
			}
		})
	}
}

// TestDispatchableExcludesMoleculeStepBeads pins the second class: a step bead
// is the execution DAG of a workflow already in flight. Handing one to a pool
// worker would run the same step twice, and a city with many live workflows has
// one of these open at every hop.
func TestDispatchableExcludesMoleculeStepBeads(t *testing.T) {
	cases := map[string]readywork.ReadyBead{
		"step id alone": {
			ID:       "gk-9a2pu",
			Type:     "task",
			Metadata: map[string]string{beadmeta.StepIDMetadataKey: "mol-polecat-work.load-context"},
		},
		"root bead id alone": {
			ID:       "gk-rooted",
			Type:     "task",
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gk-zk4aj"},
		},
		"the observed live row": {
			// gk-z4rw2 as `gc bd ready --json` reported it: a step of a live
			// molecule, already claimed by a polecat.
			ID:   "gk-z4rw2",
			Type: "task",
			Metadata: map[string]string{
				beadmeta.StepIDMetadataKey:     "mol-polecat-work.implement",
				beadmeta.RootBeadIDMetadataKey: "gk-awp07",
				beadmeta.RoutedToMetadataKey:   "gk/gastown.polecat",
			},
		},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			got := readywork.Classify(row)
			if got.Dispatchable {
				t.Fatalf("%s classified dispatchable; a molecule step belongs to its workflow", name)
			}
			if got.Class != readywork.ClassStep {
				t.Fatalf("class = %q, want %q (step is reported ahead of routed: it is not work whatever else it carries)", got.Class, readywork.ClassStep)
			}
		})
	}
}

// TestDispatchableExcludesParkedLabels pins the third class: a park is a
// deliberate decision that no worker can advance the bead, so it is not supply
// no matter how long it sits. Both hold values are covered by the prefix rule,
// which is what makes a future sanctioned hold value work without touching
// this file.
func TestDispatchableExcludesParkedLabels(t *testing.T) {
	for _, label := range []string{
		"deploy-handoff",
		"hold:mayor",
		"hold:external",
		"hold:boot-wisp-sweep",
		"owner-action",
		"human-decision",
		"human",
	} {
		t.Run(label, func(t *testing.T) {
			row := readywork.ReadyBead{ID: "gk-parked", Type: "task", Labels: []string{"bible-gap", label}}
			got := readywork.Classify(row)
			if got.Dispatchable {
				t.Fatalf("%q is a park, not supply, and the row classified dispatchable", label)
			}
			if got.Class != readywork.ClassParked {
				t.Fatalf("class = %q, want %q", got.Class, readywork.ClassParked)
			}
		})
	}
	// A label that merely CONTAINS a park's spelling is ordinary work. The
	// comparison is exact (hold: is the one sanctioned prefix), so work tagged
	// "humanizer" or "owners-manual" is not silently parked.
	for _, label := range []string{"humanizer", "owners-manual", "deploy-handoff-notes", "holdings"} {
		t.Run("not parked: "+label, func(t *testing.T) {
			row := readywork.ReadyBead{ID: "gk-work", Type: "task", Labels: []string{label}}
			if got := readywork.Classify(row); !got.Dispatchable {
				t.Fatalf("label %q classified %q; it names no park", label, got.Class)
			}
		})
	}
}

// TestDispatchableExcludesAlreadyRoutedBeads pins the fourth class: a routed
// bead has been handed to a seat and is waiting only to be claimed, so counting
// it as supply is exactly the gap city-7l25 records (a filter on `.assignee` is
// vacuous against a payload that has no such key — the route is in metadata).
func TestDispatchableExcludesAlreadyRoutedBeads(t *testing.T) {
	cases := map[string]readywork.ReadyBead{
		"gc.routed_to": {
			ID:       "gk-routed",
			Type:     "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gk/gastown.polecat"},
		},
		"gc.execution_routed_to": {
			ID:       "gk-erouted",
			Type:     "task",
			Metadata: map[string]string{beadmeta.ExecutionRoutedToMetadataKey: "gk/gastown.polecat"},
		},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			got := readywork.Classify(row)
			if got.Dispatchable {
				t.Fatalf("%s classified dispatchable; it is already handed to a seat", name)
			}
			if got.Class != readywork.ClassRouted {
				t.Fatalf("class = %q, want %q", got.Class, readywork.ClassRouted)
			}
		})
	}
	// An EMPTY route is not a route. gk-jbg30 carried `"gc.routed_to": ""`
	// while it was mid-merge; treating the empty value as routed would hide a
	// bead a worker can still act on.
	row := readywork.ReadyBead{ID: "gk-jbg30", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "",
	}}
	if got := readywork.Classify(row); !got.Dispatchable {
		t.Fatalf("an empty gc.routed_to classified %q; empty is not a route", got.Class)
	}
}

// TestDispatchableExcludesAssignedBeads pins the fifth class, added on the live
// check: 12 of the 54 rows that survived the bead's original four classes were
// already assigned. Every automatic dispatch tier asks the store for
// --unassigned rows, so an assigned open bead is owned work the pool cannot be
// handed; counting it as supply reproduces the over-report one layer down.
func TestDispatchableExcludesAssignedBeads(t *testing.T) {
	cases := map[string]readywork.ReadyBead{
		"assigned to a polecat": {ID: "gk-assigned", Type: "task", Assignee: "gk/gastown.polecat"},
		"assigned to a person":  {ID: "gk-person", Type: "bug", Assignee: "gt"},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			got := readywork.Classify(row)
			if got.Dispatchable {
				t.Fatalf("%s classified dispatchable; an assigned row is owned work", name)
			}
			if got.Class != readywork.ClassAssigned {
				t.Fatalf("class = %q, want %q", got.Class, readywork.ClassAssigned)
			}
		})
	}
	// Blank is not an assignment: a whitespace assignee must not park a bead.
	for _, assignee := range []string{"", "   "} {
		row := readywork.ReadyBead{ID: "gk-blank", Type: "task", Assignee: assignee}
		if got := readywork.Classify(row); !got.Dispatchable {
			t.Fatalf("assignee %q classified %q; a blank assignee is not an owner", assignee, got.Class)
		}
	}
}

// TestDispatchableAdmitsPlainAndUnknownTypedWork forbids over-filtering. The
// exclusions are a fixed set of STRUCTURAL non-work shapes; anything else is
// work, including a type this build has never heard of and a row with no type
// at all. The triage sweep states the same rule ("unknown type is not evidence
// of not-work"), and a classifier that dropped unknown types would starve the
// pool silently.
func TestDispatchableAdmitsPlainAndUnknownTypedWork(t *testing.T) {
	cases := map[string]readywork.ReadyBead{
		"plain task":       {ID: "gk-vginw", Type: "task"},
		"bug":              {ID: "gk-bug", Type: "bug"},
		"chore":            {ID: "gk-chore", Type: "chore"},
		"feature":          {ID: "gk-feature", Type: "feature"},
		"spike":            {ID: "gk-spike", Type: "spike"},
		"unknown to build": {ID: "gk-new", Type: "enabler"},
		"no type at all":   {ID: "gk-untyped"},
		"unparked labels":  {ID: "gk-label", Type: "task", Labels: []string{"bible-gap", "polecat-ready"}},
		"unrouted metadata": {ID: "gk-meta", Type: "task", Metadata: map[string]string{
			"artifact_cleanup_state": "pending",
			"branch":                 "polecat/gk-meta",
		}},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			got := readywork.Classify(row)
			if !got.Dispatchable {
				t.Fatalf("%s classified %q; nothing about it says not-work", name, got.Class)
			}
			if got.Class != readywork.ClassDispatchable {
				t.Fatalf("class = %q, want %q", got.Class, readywork.ClassDispatchable)
			}
		})
	}
}

// TestSummarizeReproducesTheObservedCensus is the end-to-end fixture: the row
// classes measured on the live gk store, in the proportions that made the raw
// number misleading, classified to the number a queue-health read should see.
//
// The counts are the 2026-11-06T15:1xZ census (40 rows, 0 dispatchable)
// reduced by half so the arithmetic is checkable by eye; the SHAPE is what is
// pinned, not the city's size.
func TestSummarizeReproducesTheObservedCensus(t *testing.T) {
	rows := []readywork.ReadyBead{
		// 11 synthetic carriers
		{ID: "convoy-1", Type: "convoy"},
		{ID: "convoy-2", Type: "convoy"},
		{ID: "convoy-3", Type: "convoy"},
		{ID: "convoy-4", Type: "convoy"},
		{ID: "convoy-5", Type: "convoy"},
		{ID: "convoy-6", Type: "convoy"},
		{ID: "convoy-7", Type: "convoy"},
		{ID: "convoy-8", Type: "convoy"},
		{ID: "convoy-9", Type: "convoy"},
		{ID: "convoy-10", Type: "convoy"},
		{ID: "convoy-11", Type: "convoy"},
		// 2 molecule step beads
		{ID: "step-1", Type: "task", Metadata: map[string]string{beadmeta.StepIDMetadataKey: "mol.x"}},
		{ID: "step-2", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gk-root"}},
		// 3 parked (hold labels)
		{ID: "hold-1", Type: "bug", Labels: []string{"hold:mayor"}},
		{ID: "hold-2", Type: "task", Labels: []string{"hold:external"}},
		{ID: "hold-3", Type: "task", Labels: []string{"owner-action"}},
		// 2 parked deploy-handoff carriers
		{ID: "park-1", Type: "task", Labels: []string{"deploy-handoff"}},
		{ID: "park-2", Type: "bug", Labels: []string{"deploy-handoff", "gk-build-12"}},
		// 1 already routed
		{ID: "routed-1", Type: "task", Metadata: map[string]string{beadmeta.ExecutionRoutedToMetadataKey: "gk/gastown.polecat"}},
		// 2 already assigned
		{ID: "assigned-1", Type: "task", Assignee: "gk/gastown.polecat"},
		{ID: "assigned-2", Type: "bug", Assignee: "gt"},
		// 1 genuinely free bead
		{ID: "free-1", Type: "task", Labels: []string{"bible-gap"}},
	}
	got := readywork.Summarize(rows)

	if got.Total != len(rows) {
		t.Fatalf("total = %d, want %d", got.Total, len(rows))
	}
	if got.Dispatchable != 1 {
		t.Fatalf("dispatchable = %d, want 1 — the raw count of %d over-reports by %d", got.Dispatchable, got.Total, got.Total-1)
	}
	want := map[readywork.DispatchClass]int{
		readywork.ClassDispatchable: 1,
		readywork.ClassSynthetic:    11,
		readywork.ClassStep:         2,
		readywork.ClassParked:       5,
		readywork.ClassRouted:       1,
		readywork.ClassAssigned:     2,
	}
	if !reflect.DeepEqual(got.ByClass, want) {
		t.Fatalf("by_class = %v, want %v (the classes must complement the dispatchable count)", got.ByClass, want)
	}
	// The busiest class is the monotonic one: carriers are minted per dispatch.
	if got.ByClass[readywork.ClassSynthetic] != 11 || got.ByClass[readywork.ClassSynthetic] < got.Dispatchable {
		t.Fatalf("synthetic = %d, dispatchable = %d; the tracking class dominates the pool, which is the defect", got.ByClass[readywork.ClassSynthetic], got.Dispatchable)
	}
	// In-flight is step + routed + assigned work: parked beads are NOT in
	// flight, or a parked city would read as busy.
	if got.InFlight() != 5 {
		t.Fatalf("in_flight = %d, want 5 (2 step + 1 routed + 2 assigned); parked beads are not in-flight work", got.InFlight())
	}
	if len(got.Rows) != 1 || got.Rows[0].ID != "free-1" {
		t.Fatalf("dispatchable rows = %v, want the one free bead", got.Rows)
	}
}

// TestSummarizeCountsEveryClassEvenAtZero: a consumer reading the JSON never
// has to tell "absent" from "zero", which is the difference between a class
// this build knows about and one it does not.
func TestSummarizeCountsEveryClassEvenAtZero(t *testing.T) {
	got := readywork.Summarize(nil)
	if got.Total != 0 || got.Dispatchable != 0 || got.InFlight() != 0 {
		t.Fatalf("empty census = %+v, want all zero", got)
	}
	if len(got.ByClass) != len(readywork.DispatchClasses()) {
		t.Fatalf("by_class has %d entries, want every class (%d): %v", len(got.ByClass), len(readywork.DispatchClasses()), got.ByClass)
	}
	for _, class := range readywork.DispatchClasses() {
		if _, ok := got.ByClass[class]; !ok {
			t.Fatalf("by_class is missing class %q on an empty census", class)
		}
	}
}
