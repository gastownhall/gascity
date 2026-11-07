package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
)

const planTestStamp = "infra/20261107T000000Z"

// planRow is a binding row: stamp is its gc.infra_migrated_from ("" for a
// row the binding minted), scope its gc.root_store_ref.
func planRow(id, status, stamp, scope string) beads.Bead {
	m := beads.StringMap{"gc.kind": "task", beadmeta.RootBeadIDMetadataKey: id}
	if stamp != "" {
		m[infraMigrationStampKey] = stamp
	}
	if scope != "" {
		m[beadmeta.RootStoreRefMetadataKey] = scope
	}
	return beads.Bead{ID: id, Title: id, Type: "task", Status: status, Metadata: m}
}

func planGate(id, status, stamp, owner, blocker string) beads.Bead {
	b := planRow(id, status, stamp, "")
	b.Labels = []string{dispatch.CrossStoreGateLabel}
	b.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey] = owner
	b.Metadata[beadmeta.CrossStoreBlockerMetadataKey] = blocker
	return b
}

func withRecords(b beads.Bead, records ...rigCrossEdgeRecord) beads.Bead {
	raw, _ := json.Marshal(records)
	b.Metadata[beadmeta.MigratedCrossEdgesMetadataKey] = string(raw)
	return b
}

// planInput is a to-work input over rows and edges: the packs rig is
// configured and imported (its manifest records every rig:packs row), and
// both destinations carry payloads.
func planInput(rows []beads.Bead, edges []beads.Dep) reverseInput {
	in := reverseInput{
		Mode: reverseToWork, Stamp: planTestStamp, Binding: rows,
		Down: map[string][]beads.Dep{}, Payloads: map[beads.DepPair]string{},
		Manifests:      map[string]*infraCopyManifest{"packs": {IDs: map[string]bool{}, ScopeAnnotated: map[string]bool{}}},
		Configured:     map[string]bool{"packs": true},
		Held:           map[string]map[string]beads.Bead{reverseCityDest: {}, rigImportStamp("packs"): {}},
		CarriesPayload: map[string]bool{reverseCityDest: true, rigImportStamp("packs"): true},
		DerivesParent:  map[string]bool{}, DropsRef: map[string]bool{},
	}
	for _, b := range rows {
		if b.Metadata[infraMigrationStampKey] == rigImportStamp("packs") {
			in.Manifests["packs"].IDs[b.ID] = true
		}
	}
	for _, d := range edges {
		in.Down[d.IssueID] = append(in.Down[d.IssueID], d)
	}
	return in
}

func planRefusal(plan reversePlan) string {
	var lines []string
	for _, r := range plan.Refusals {
		lines = append(lines, r.String())
	}
	return strings.Join(lines, "\n")
}

func planDests(plan reversePlan) map[string]string {
	out := map[string]string{}
	for dest, rows := range plan.Rows {
		for _, b := range rows {
			out[b.ID] = dest
		}
	}
	return out
}

func planEdgeSet(plan reversePlan) string {
	var out []string
	for _, e := range plan.Edges[reverseCityDest] {
		out = append(out, fmt.Sprintf("%s>%s:%s:%s", e.IssueID, e.DependsOnID, e.Type, e.Payload))
	}
	return strings.Join(out, ",")
}

// TestPlanReverseRoutesEveryRow pins §4.2's table: a rig import goes to its
// rig, a closed binding-born rig-scope row and everything else to the city,
// a closed gate nowhere; each moved row is stamped with the run and loses the
// forward stamp, its record and (for an annotated import) its scope.
func TestPlanReverseRoutesEveryRow(t *testing.T) {
	annotated := planRow("gp-a", "closed", rigImportStamp("packs"), "rig:packs")
	in := planInput([]beads.Bead{
		planRow("gp-r", "open", rigImportStamp("packs"), "rig:packs"),
		annotated,
		planRow("mc-c", "open", infraMigrationCityStamp, "rig:packs"),
		planRow("gcg-born", "open", "", ""),
		planRow("gcg-d8", "closed", "", "rig:packs"),
		planGate("gcg-xsg-1", "closed", infraMigrationCityStamp, "mc-c", "mc-w"),
	}, nil)
	in.Manifests["packs"].ScopeAnnotated["gp-a"] = true
	plan := planReverse(in)
	if len(plan.Refusals) > 0 {
		t.Fatalf("refused: %s", planRefusal(plan))
	}
	want := map[string]string{"gp-r": rigImportStamp("packs"), "gp-a": rigImportStamp("packs"), "mc-c": reverseCityDest, "gcg-born": reverseCityDest, "gcg-d8": reverseCityDest}
	if got := planDests(plan); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	if plan.Gates != 1 {
		t.Fatalf("gates not carried = %d, want 1", plan.Gates)
	}
	for _, rows := range plan.Rows {
		for _, b := range rows {
			if b.Metadata[beadmeta.InfraReversedFromMetadataKey] != planTestStamp || b.Metadata[infraMigrationStampKey] != "" {
				t.Fatalf("%s carries %v", b.ID, b.Metadata)
			}
			if _, scoped := b.Metadata[beadmeta.RootStoreRefMetadataKey]; scoped == (b.ID == "gp-a" || b.ID == "gcg-born") {
				t.Fatalf("%s scope = %v", b.ID, b.Metadata[beadmeta.RootStoreRefMetadataKey])
			}
		}
	}
}

// TestPlanReverseRefusals pins every refusal by id, each with its fix.
func TestPlanReverseRefusals(t *testing.T) {
	cases := []struct {
		name  string
		in    func() reverseInput
		want  []string
		clean bool
	}{
		{"an open gate refuses once, not again through its owner's edge", func() reverseInput {
			return planInput([]beads.Bead{planRow("mc-d", "open", "work", ""), planGate("gcg-xsg-1", "open", "work", "mc-d", "mc-w")},
				[]beads.Dep{{IssueID: "mc-d", DependsOnID: "gcg-xsg-1", Type: "blocks"}, {IssueID: "gcg-xsg-1", DependsOnID: "mc-d", Type: "related"}})
		}, []string{"gcg-xsg-1"}, false},
		{"open gate", func() reverseInput {
			return planInput([]beads.Bead{planRow("mc-d", "open", "work", ""), planGate("gcg-xsg-1", "open", "work", "mc-d", "mc-w")},
				[]beads.Dep{{IssueID: "mc-d", DependsOnID: "gcg-xsg-1", Type: "blocks"}})
		}, []string{"gcg-xsg-1 (gate for mc-d on mc-w)", "open cross-store gate", "gc workflow delete"}, false},
		{"open binding-born rig-scope row (D8)", func() reverseInput {
			return planInput([]beads.Bead{planRow("gcg-1", "open", "", "rig:packs")}, nil)
		}, []string{"gcg-1 (rig:packs, open)", "(D8)", "closed ones move to the city store"}, false},
		{"in-progress binding-born rig-scope row (D8)", func() reverseInput {
			return planInput([]beads.Bead{planRow("gcg-1", "in_progress", "", "rig:packs")}, nil)
		}, []string{"gcg-1 (rig:packs, in_progress)"}, false},
		{"a rig no longer configured", func() reverseInput {
			in := planInput([]beads.Bead{planRow("gq-1", "closed", rigImportStamp("gone"), "")}, nil)
			return in
		}, []string{"gq-1 (rig gone)", "add the rig back"}, false},
		{"an interrupted import", func() reverseInput {
			in := planInput([]beads.Bead{planRow("gp-1", "closed", rigImportStamp("packs"), "")}, nil)
			delete(in.Manifests["packs"].IDs, "gp-1")
			return in
		}, []string{"gp-1 (rig packs)", "manifest does not record it", rigImportAllInstruction()}, false},
		{"an id the destination holds as another bead", func() reverseInput {
			in := planInput([]beads.Bead{planRow("mc-1", "closed", "work", "")}, nil)
			in.Held[reverseCityDest]["mc-1"] = planRow("mc-1", "open", "", "")
			return in
		}, []string{"mc-1 (the city work store)", "already holds each of these ids"}, false},
		{"this run's own earlier copy is not a reused id", func() reverseInput {
			in := planInput([]beads.Bead{planRow("mc-1", "closed", "work", "")}, nil)
			in.Held[reverseCityDest]["mc-1"] = reverseRow(planRow("mc-1", "closed", "work", ""), planTestStamp, false)
			return in
		}, nil, true},
		{"an edge joining two open rows in different stores", func() reverseInput {
			return planInput([]beads.Bead{planRow("gp-1", "open", rigImportStamp("packs"), ""), planRow("gcg-2", "open", "", "")},
				[]beads.Dep{{IssueID: "gcg-2", DependsOnID: "gp-1", Type: "blocks"}})
		}, []string{"gcg-2 -[blocks]-> gp-1 (the city work store, the store of rig packs)", "close one side"}, false},
		{"the same edge with one side closed is dropped", func() reverseInput {
			return planInput([]beads.Bead{planRow("gp-1", "closed", rigImportStamp("packs"), ""), planRow("gcg-2", "open", "", "")},
				[]beads.Dep{{IssueID: "gcg-2", DependsOnID: "gp-1", Type: "blocks"}})
		}, nil, true},
		{"a payload into a store without a writer", func() reverseInput {
			in := planInput([]beads.Bead{planRow("mc-1", "closed", "work", ""), planRow("mc-2", "closed", "work", "")},
				[]beads.Dep{{IssueID: "mc-1", DependsOnID: "mc-2", Type: "waits-for"}})
			in.Payloads[beads.DepPair{IssueID: "mc-1", DependsOnID: "mc-2"}] = `{"gate":"all-children"}`
			in.CarriesPayload[reverseCityDest] = false
			return in
		}, []string{"mc-1 -[waits-for]-> mc-2 (the city work store)", "native Dolt has no edge-payload writer", "nothing was written"}, false},
		{"a record a work bead holds that does not parse", func() reverseInput {
			in := planInput([]beads.Bead{planRow("mc-1", "closed", "work", "")}, nil)
			w := planRow("mc-w", "open", "", "")
			w.Metadata[beadmeta.MigratedCrossEdgesMetadataKey] = "{not json"
			in.Held[reverseCityDest]["mc-w"] = w
			return in
		}, []string{"mc-w (", "does not parse"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planReverse(tc.in())
			got := planRefusal(plan)
			if strings.HasPrefix(tc.name, "an open gate refuses once") && len(plan.Refusals) != 1 {
				t.Fatalf("refusals = %d, want 1:\n%s", len(plan.Refusals), got)
			}
			if tc.clean != (got == "") {
				t.Fatalf("refusals = %q, want clean=%v", got, tc.clean)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("the refusal does not say %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestPlanReverseEdges: an edge between rows landing in one store is carried
// with its payload, an external: reference is carried, an edge onto a closed
// gate is not, the gate's record restores the edge it stood for, a record on
// a work bead the city kept restores its edge, a record whose other end is
// gone is reported, and an edge onto a row no store holds is dropped.
func TestPlanReverseEdges(t *testing.T) {
	d := withRecords(planRow("mc-d", "open", "work", ""),
		rigCrossEdgeRecord{Dir: "out", Type: "blocks", Other: "mc-w", Action: rigEdgeGated},
		rigCrossEdgeRecord{Dir: "in", Type: "tracks", Other: "mc-gone", Action: rigEdgeDropped},
		rigCrossEdgeRecord{Dir: "out", Type: "blocks", Other: "external:x", Action: rigEdgeExternal})
	in := planInput([]beads.Bead{d, planRow("mc-e", "closed", "work", ""), planGate("gcg-xsg-1", "closed", "work", "mc-d", "mc-w")},
		[]beads.Dep{
			{IssueID: "mc-d", DependsOnID: "mc-e", Type: "waits-for"},
			{IssueID: "mc-d", DependsOnID: "gcg-xsg-1", Type: "blocks"},
			{IssueID: "mc-d", DependsOnID: "external:x", Type: "blocks"},
			{IssueID: "mc-e", DependsOnID: "mc-nowhere", Type: "related"},
		})
	in.Payloads[beads.DepPair{IssueID: "mc-d", DependsOnID: "mc-e"}] = `{"g":1}`
	in.Held[reverseCityDest]["mc-w"] = planRow("mc-w", "open", "", "")
	in.Held[reverseCityDest]["mc-v"] = withRecords(planRow("mc-v", "closed", "", ""), rigCrossEdgeRecord{Dir: "out", Type: "tracks", Other: "mc-e", Action: rigEdgeDropped})
	plan := planReverse(in)
	if len(plan.Refusals) > 0 {
		t.Fatalf("refused: %s", planRefusal(plan))
	}
	want := "mc-d>external:x:blocks:,mc-d>mc-e:waits-for:{\"g\":1},mc-d>mc-w:blocks:,mc-v>mc-e:tracks:"
	if got := planEdgeSet(plan); got != want {
		t.Fatalf("edges = %s\nwant    %s", got, want)
	}
	if plan.Restored != 2 {
		t.Fatalf("restored = %d, want 2 (the gated edge and the work bead's record)", plan.Restored)
	}
	dropped := strings.Join(plan.Dropped, "\n")
	for _, want := range []string{"mc-gone -[tracks]-> mc-d", "mc-e -[related]-> mc-nowhere: no store holds mc-nowhere"} {
		if !strings.Contains(dropped, want) {
			t.Fatalf("dropped does not name %q:\n%s", want, dropped)
		}
	}
}

// TestPlanReverseToRigsMovesOnlyTheNamedRig: a to-rigs plan moves the named
// rig's import and its closed gates, leaves every other row in the binding,
// and refuses an open edge between a row it moves and one the binding keeps.
func TestPlanReverseToRigsMovesOnlyTheNamedRig(t *testing.T) {
	rows := []beads.Bead{
		planRow("gp-1", "closed", rigImportStamp("packs"), "rig:packs"),
		planGate("gcg-xsg-1", "closed", rigImportStamp("packs"), "gp-1", "gp-w"),
		planGate("gcg-xsg-2", "closed", "work", "mc-1", "mc-w"),
		planRow("mc-1", "open", "work", ""),
		planRow("gcg-born", "open", "", "rig:packs"),
		planRow("go-1", "closed", rigImportStamp("other"), "rig:other"),
	}
	in := planInput(rows, nil)
	in.Mode, in.Rigs = reverseToRigs, map[string]bool{"packs": true}
	in.Configured["other"] = true
	in.Manifests["other"] = &infraCopyManifest{IDs: map[string]bool{"go-1": true}}
	plan := planReverse(in)
	if len(plan.Refusals) > 0 {
		t.Fatalf("refused: %s", planRefusal(plan))
	}
	if got := fmt.Sprint(planDests(plan)); got != "map[gp-1:rig:packs]" {
		t.Fatalf("routes = %s", got)
	}
	if got := strings.Join(plan.Leaving, ","); got != "gcg-xsg-1,gp-1" {
		t.Fatalf("leaving = %s, want the row and its rig's closed gate", got)
	}
	open := planInput(append(rows, planRow("gp-2", "open", rigImportStamp("packs"), "rig:packs")), []beads.Dep{{IssueID: "gcg-born", DependsOnID: "gp-2", Type: "parent-child"}})
	open.Mode, open.Rigs = reverseToRigs, map[string]bool{"packs": true}
	if got := planRefusal(planReverse(open)); !strings.Contains(got, "gcg-born -[parent-child]-> gp-2 (the binding, the store of rig packs)") {
		t.Fatalf("an open edge from a row the binding keeps did not refuse: %s", got)
	}
}

// TestPlanReverseRecordsWhatItDoesNotCarry (SHOULD 10): an edge the reverse
// drops is recorded as gc.migrated_cross_edges on the moved row it leaves,
// so a later import can restore it.
func TestPlanReverseRecordsWhatItDoesNotCarry(t *testing.T) {
	in := planInput([]beads.Bead{planRow("gp-1", "closed", rigImportStamp("packs"), ""), planRow("gcg-2", "open", "", "")},
		[]beads.Dep{{IssueID: "gcg-2", DependsOnID: "gp-1", Type: "blocks"}, {IssueID: "gcg-2", DependsOnID: "mc-gone", Type: "related"}})
	plan := planReverse(in)
	if len(plan.Refusals) > 0 {
		t.Fatalf("refused: %s", planRefusal(plan))
	}
	records := map[string]string{}
	for _, rows := range plan.Rows {
		for _, b := range rows {
			records[b.ID] = b.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]
		}
	}
	for id, want := range map[string][]string{
		"gp-1":  {`"dir":"in"`, `"other":"gcg-2"`, `"other_status_at_import":"open"`, `"action":"dropped"`},
		"gcg-2": {`"dir":"out"`, `"other":"gp-1"`, `"other":"mc-gone"`},
	} {
		for _, w := range want {
			if !strings.Contains(records[id], w) {
				t.Fatalf("%s records %s, want %s", id, records[id], w)
			}
		}
	}
}

// TestPlanReverseRestoresARuntimeGatesEdge (SHOULD 11): a closed gate the
// runtime minted carries no record on its owner; the edge it stood for comes
// back from the gate's own metadata.
func TestPlanReverseRestoresARuntimeGatesEdge(t *testing.T) {
	in := planInput([]beads.Bead{planRow("gcg-x", "open", "", ""), planGate("gcg-xsg-9", "closed", "", "gcg-x", "mc-w")},
		[]beads.Dep{{IssueID: "gcg-x", DependsOnID: "gcg-xsg-9", Type: "blocks"}})
	in.Held[reverseCityDest]["mc-w"] = planRow("mc-w", "closed", "", "")
	plan := planReverse(in)
	if got := planEdgeSet(plan); got != "gcg-x>mc-w:blocks:" || plan.Restored != 1 {
		t.Fatalf("edges = %s (restored %d), want the gate's owner -> blocker", got, plan.Restored)
	}
}

// TestPlanReverseParentsForAStoreThatDerivesThem (MUST 4): into a store that
// reads a parent from its parent-child edge (native Dolt), a parent with no
// edge gets one where the parent lands with the row, a row with only the edge
// gets the parent, and a parent that lands elsewhere is judged as the edge it
// would be. A store that keeps the field gets no edge.
func TestPlanReverseParentsForAStoreThatDerivesThem(t *testing.T) {
	retry := planRow("gcg-r", "open", "", "")
	retry.ParentID = "gcg-p"
	edgeOnly := planRow("gcg-e", "closed", "", "")
	away := planRow("gcg-a", "closed", "", "")
	away.ParentID = "gp-p"
	rows := []beads.Bead{retry, edgeOnly, away, planRow("gcg-p", "open", "", ""), planRow("gp-p", "closed", rigImportStamp("packs"), "")}
	deps := []beads.Dep{{IssueID: "gcg-e", DependsOnID: "gcg-p", Type: "parent-child"}}
	in := planInput(rows, deps)
	in.DerivesParent[reverseCityDest] = true
	plan := planReverse(in)
	if len(plan.Refusals) > 0 {
		t.Fatalf("refused: %s", planRefusal(plan))
	}
	if got := planEdgeSet(plan); got != "gcg-e>gcg-p:parent-child:,gcg-r>gcg-p:parent-child:" {
		t.Fatalf("edges = %s", got)
	}
	parents := map[string]string{}
	for _, b := range plan.Rows[reverseCityDest] {
		parents[b.ID] = b.ParentID
	}
	if parents["gcg-r"] != "gcg-p" || parents["gcg-e"] != "gcg-p" || parents["gcg-a"] != "" {
		t.Fatalf("parents = %v", parents)
	}
	if !strings.Contains(strings.Join(plan.Dropped, "\n"), "gcg-a -[parent-child]-> gp-p") {
		t.Fatalf("the parent in another store was not reported: %v", plan.Dropped)
	}
	keeps := planInput(rows, deps)
	if got := planEdgeSet(planReverse(keeps)); got != "gcg-e>gcg-p:parent-child:" {
		t.Fatalf("a store that keeps the field got edges %s", got)
	}
}

// TestPlanReverseKeepsARefTheStoreDrops: into a store that stores no step
// ref, the ref is kept in gc.step_ref when that is absent, and counted.
func TestPlanReverseKeepsARefTheStoreDrops(t *testing.T) {
	a, b := planRow("gcg-1", "closed", "", ""), planRow("gcg-2", "closed", "", "")
	a.Ref, b.Ref = "review", "build"
	b.Metadata[beadmeta.StepRefMetadataKey] = "already"
	in := planInput([]beads.Bead{a, b}, nil)
	in.DropsRef[reverseCityDest] = true
	plan := planReverse(in)
	got := map[string]string{}
	for _, r := range plan.Rows[reverseCityDest] {
		got[r.ID] = r.Metadata[beadmeta.StepRefMetadataKey]
	}
	if got["gcg-1"] != "review" || got["gcg-2"] != "already" || plan.RefsKept != 1 {
		t.Fatalf("step refs = %v, kept %d", got, plan.RefsKept)
	}
}
