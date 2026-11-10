package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
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

// planGate is a cross-store gate of owner on blocker. A closed one is closed
// as the release lane closes a gate whose blocker closed (released, with its
// reason), so it reads as closed by the controller
// (dispatch.IsControllerClosedCrossStoreGate); gateClosedAs closes it another
// way.
func planGate(id, status, stamp, owner, blocker string) beads.Bead {
	b := planRow(id, status, stamp, "")
	b.Labels = []string{dispatch.CrossStoreGateLabel}
	b.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey] = owner
	b.Metadata[beadmeta.CrossStoreBlockerMetadataKey] = blocker
	if status == "closed" {
		b.Metadata[beadmeta.CrossStoreGateStateMetadataKey] = dispatch.CrossStoreGateStateReleased
		b.Metadata["close_reason"] = "blocker " + blocker + " closed"
	}
	return b
}

// gateClosedAs is the closed gate g with the state marker and the close reason
// given ("" for none) in place of planGate's release.
func gateClosedAs(g beads.Bead, state, reason string) beads.Bead {
	delete(g.Metadata, beadmeta.CrossStoreGateStateMetadataKey)
	delete(g.Metadata, "close_reason")
	if state != "" {
		g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] = state
	}
	if reason != "" {
		g.Metadata["close_reason"] = reason
	}
	return g
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
// runtime minted carries no record; the edge it stood for comes back from the
// gate's own metadata, on the row that waited on it. The runtime mints a gate
// for a drain (internal/dispatch/drain.go): its owner, the drain control,
// waits only on its item, so the owner gets no edge.
func TestPlanReverseRestoresARuntimeGatesEdge(t *testing.T) {
	in := planInput([]beads.Bead{planRow("gcg-ctl", "open", "", ""), planRow("gcg-x", "open", "", ""), planGate("gcg-xsg-9", "closed", "", "gcg-ctl", "mc-w")},
		[]beads.Dep{{IssueID: "gcg-ctl", DependsOnID: "gcg-x", Type: "blocks"}, {IssueID: "gcg-x", DependsOnID: "gcg-xsg-9", Type: "blocks"}})
	in.Held[reverseCityDest]["mc-w"] = planRow("mc-w", "closed", "", "")
	plan := planReverse(in)
	if got := planEdgeSet(plan); got != "gcg-ctl>gcg-x:blocks:,gcg-x>mc-w:blocks:" || plan.Restored != 1 {
		t.Fatalf("edges = %s (restored %d), want the item's edge onto the gate's blocker and none from the control", got, plan.Restored)
	}
}

// TestPlanReverseRoutesARuntimeGateByItsOwner: the runtime mints a gate as a
// drain does (internal/dispatch/drain.go): owned by the drain control, which
// waits only on its item root, and waited on by the item's rows. It carries
// no import stamp (dispatch.NewCrossStoreGate), so --to-rigs routes it by its
// owner. Both reverses give each moved row that waited on a closed one the
// edge it stood for, row -> blocker, where the row lands with the blocker,
// and the row records it as dropped where it does not; the owner gets
// neither. --to-rigs takes the gate out of the binding only when every row
// that waits on it leaves: a row the binding keeps keeps it there, with its
// edge onto it. A --to-rigs refuses such a gate open, and leaves a gate whose
// owner the binding keeps as it is, open or closed.
func TestPlanReverseRoutesARuntimeGateByItsOwner(t *testing.T) {
	packs := rigImportStamp("packs")
	// drain is a finished drain: its control gp-drain, which the rig's import
	// moved, holds its item root waiting[0], and each row of waiting waits on
	// the gate the runtime minted for the control on blocker.
	drain := func(mode reverseMode, status, blocker string, waiting ...beads.Bead) reverseInput {
		control := planRow("gp-drain", "closed", packs, "rig:packs")
		rows := []beads.Bead{control, planGate("gcg-drain-gate", status, "", control.ID, blocker)}
		deps := []beads.Dep{{IssueID: control.ID, DependsOnID: waiting[0].ID, Type: "blocks"}}
		for _, w := range waiting {
			rows = append(rows, w)
			deps = append(deps, beads.Dep{IssueID: w.ID, DependsOnID: "gcg-drain-gate", Type: "blocks"})
		}
		in := planInput(rows, deps)
		in.Mode, in.Rigs = mode, map[string]bool{"packs": true}
		in.Held[packs]["gp-work"] = planRow("gp-work", "closed", "", "")
		in.Held[reverseCityDest]["mc-work"] = planRow("mc-work", "closed", "", "")
		return in
	}
	minted := planRow("gcg-item", "closed", "", "") // the binding minted it after the import
	item, member := planRow("gp-item", "closed", packs, "rig:packs"), planRow("gp-member", "closed", packs, "rig:packs")
	controlEdge := "gp-drain -[blocks]-> gcg-item: it would cross stores, and one side is closed"
	for _, tc := range []struct {
		name     string
		in       reverseInput
		edges    string
		restored int
		dropped  []string
		leaving  string
		gates    int
	}{
		{
			"to-rigs: the binding keeps the item, and the gate with it", drain(reverseToRigs, "closed", "gp-work", minted),
			"", 0,
			[]string{controlEdge},
			"gp-drain", 0,
		},
		{
			"to-rigs: a blocker in another store changes nothing while the gate stays", drain(reverseToRigs, "closed", "mc-work", minted),
			"", 0,
			[]string{controlEdge},
			"gp-drain", 0,
		},
		{
			"to-rigs: a returned row gets the edge, and the row the binding keeps keeps the gate", drain(reverseToRigs, "closed", "gp-work", minted, member),
			"rig:packs gp-member>gp-work", 1,
			[]string{controlEdge},
			"gp-drain,gp-member", 0,
		},
		{
			"to-rigs: every row that waits on it returns, so the gate leaves with them", drain(reverseToRigs, "closed", "gp-work", item, member),
			"rig:packs gp-drain>gp-item,rig:packs gp-item>gp-work,rig:packs gp-member>gp-work", 2, nil, "gcg-drain-gate,gp-drain,gp-item,gp-member", 1,
		},
		{
			"to-work: the item gets the edge where it lands with the blocker", drain(reverseToWork, "closed", "mc-work", minted),
			"city gcg-item>mc-work", 1,
			[]string{controlEdge},
			"", 1,
		},
		{
			"to-work: the item records the edge where it would cross stores", drain(reverseToWork, "closed", "gp-work", minted),
			"", 0,
			[]string{"gcg-item -[blocks]-> gp-work: the edge closed gate gcg-drain-gate stood for, and it would cross stores (the store of rig packs)", controlEdge},
			"", 1,
		},
		{
			"to-work: every row that waits on it lands with the blocker", drain(reverseToWork, "closed", "gp-work", item, member),
			"rig:packs gp-drain>gp-item,rig:packs gp-item>gp-work,rig:packs gp-member>gp-work", 2, nil, "", 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planReverse(tc.in)
			if len(plan.Refusals) != 0 {
				t.Fatalf("unexpected refusal: %s", planRefusal(plan))
			}
			var edges []string
			for _, dest := range sortedMapKeys(plan.Edges) {
				for _, e := range plan.Edges[dest] {
					edges = append(edges, fmt.Sprintf("%s %s>%s", dest, e.IssueID, e.DependsOnID))
				}
			}
			if got := strings.Join(edges, ","); got != tc.edges || plan.Restored != tc.restored {
				t.Fatalf("edges = %q (restored %d), want %q (restored %d)", got, plan.Restored, tc.edges, tc.restored)
			}
			if got, want := strings.Join(plan.Dropped, "\n"), strings.Join(tc.dropped, "\n"); got != want {
				t.Fatalf("dropped:\n%s\nwant:\n%s", got, want)
			}
			if got := strings.Join(plan.Leaving, ","); got != tc.leaving || plan.Gates != tc.gates {
				t.Fatalf("leaving = %q, gates not carried %d; want %q and %d", got, plan.Gates, tc.leaving, tc.gates)
			}
			for _, b := range plan.Rows[packs] {
				if b.ID == "gp-drain" && strings.Contains(b.Metadata[beadmeta.MigratedCrossEdgesMetadataKey], "-work") {
					t.Fatalf("the control records %s: it never waited on the gate's blocker", b.Metadata[beadmeta.MigratedCrossEdgesMetadataKey])
				}
			}
		})
	}
	t.Run("to-work: the item's record holds the edge it does not carry", func(t *testing.T) {
		plan := planReverse(drain(reverseToWork, "closed", "gp-work", minted))
		records := map[string]string{}
		for _, rows := range plan.Rows {
			for _, b := range rows {
				records[b.ID] = b.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]
			}
		}
		for id, want := range map[string]string{
			"gcg-item": `[{"dir":"in","type":"blocks","other":"gp-drain","other_status_at_import":"closed","action":"dropped"},{"dir":"out","type":"blocks","other":"gp-work","other_status_at_import":"closed","action":"dropped"}]`,
			"gp-drain": `[{"dir":"out","type":"blocks","other":"gcg-item","other_status_at_import":"closed","action":"dropped"}]`,
		} {
			if records[id] != want {
				t.Fatalf("%s records %s, want %s", id, records[id], want)
			}
		}
	})
	if got := planRefusal(planReverse(drain(reverseToRigs, "open", "gp-work", minted))); !strings.Contains(got, "gcg-drain-gate (gate for gp-drain on gp-work)") || !strings.Contains(got, "open cross-store gate") {
		t.Fatalf("--to-rigs over an open gate of a row it returns: %q", got)
	}
	for _, status := range []string{"open", "closed"} {
		in := planInput([]beads.Bead{planRow("gp-1", "closed", packs, "rig:packs"), planRow("gcg-ctl", "open", "", ""), planRow("gcg-it", "open", "", ""), planGate("gcg-ctl-gate", status, "", "gcg-ctl", "gp-work")},
			[]beads.Dep{{IssueID: "gcg-ctl", DependsOnID: "gcg-it", Type: "blocks"}, {IssueID: "gcg-it", DependsOnID: "gcg-ctl-gate", Type: "blocks"}})
		in.Mode, in.Rigs = reverseToRigs, map[string]bool{"packs": true}
		plan := planReverse(in)
		if len(plan.Refusals) != 0 || strings.Join(plan.Leaving, ",") != "gp-1" || plan.Gates != 0 || len(plan.Edges[packs]) != 0 || len(plan.Dropped) != 0 {
			t.Fatalf("--to-rigs over a %s gate of a control the binding keeps: refusals %q, leaving %v, gates %d, rig edges %v, dropped %v", status, planRefusal(plan), plan.Leaving, plan.Gates, plan.Edges[packs], plan.Dropped)
		}
	}
}

// TestPlanReverseKeepsOneRecordOfAGatedEdge: a gate a rig's import minted
// stands for an edge its owner's record already holds (gated). Where the
// blocker lands in another store, the owner carries that record on and the
// edge is reported once: the gate adds no second record and no second line.
func TestPlanReverseKeepsOneRecordOfAGatedEdge(t *testing.T) {
	packs := rigImportStamp("packs")
	for _, mode := range []reverseMode{reverseToWork, reverseToRigs} {
		t.Run(string(mode), func(t *testing.T) {
			owner := withRecords(planRow("gp-owner", "open", packs, "rig:packs"), rigCrossEdgeRecord{Dir: "out", Type: "blocks", Other: "mc-work", Action: rigEdgeGated})
			in := planInput([]beads.Bead{owner, planGate("gcg-xsg-7", "closed", packs, owner.ID, "mc-work")},
				[]beads.Dep{{IssueID: owner.ID, DependsOnID: "gcg-xsg-7", Type: "blocks"}})
			in.Mode, in.Rigs = mode, map[string]bool{"packs": true}
			in.Held[reverseCityDest]["mc-work"] = planRow("mc-work", "closed", "", "")
			plan := planReverse(in)
			if len(plan.Refusals) != 0 {
				t.Fatalf("unexpected refusal: %s", planRefusal(plan))
			}
			if got := strings.Join(plan.Dropped, "\n"); got != "gp-owner -[blocks]-> mc-work: recorded on gp-owner, and its other end is not in the store of rig packs" {
				t.Fatalf("dropped:\n%s\nwant the gated edge once", got)
			}
			if got := plan.Rows[packs][0].Metadata[beadmeta.MigratedCrossEdgesMetadataKey]; got != `[{"dir":"out","type":"blocks","other":"mc-work","action":"gated"}]` {
				t.Fatalf("the owner records %s, want its gated record alone", got)
			}
		})
	}
}

// TestPlanReverseRefusesToHoldWhatAnOperatorReleased: an operator's close of a
// cross-store gate is final (dispatch.CrossStoreGateStateOverridden): the rows
// that waited on it run whatever its blocker does. A single store has no gate
// to keep that, so where the edge the gate stood for, row -> blocker, would
// hold an open row on a blocker that lands with it and does not release it,
// both reverses refuse, naming the row, the blocker and the gate, and restore
// nothing for it. That holds for a drain's gate, through its item's edge onto
// the gate, and for a migration's, through its owner's gated record, which the
// owner's edge onto the gate meets again (one line), or alone. The
// controller's closes (a release, a retire, a teardown's), which the runtime
// takes back when the blocker holds again, restore the edge, and so do a
// blocker that releases, a closed row and an edge that does not block; a
// blocker in another store is recorded as before.
func TestPlanReverseRefusesToHoldWhatAnOperatorReleased(t *testing.T) {
	packs := rigImportStamp("packs")
	// drain is a live drain whose control gp-drain the rig's import moved: the
	// control holds its item root gp-item, which waits on the gate the runtime
	// minted for the control on blocker.
	drain := func(gate func(beads.Bead) beads.Bead, row, blocker beads.Bead, depType string) reverseInput {
		control := planRow("gp-drain", "open", packs, "rig:packs")
		return planInput([]beads.Bead{control, row, gate(planGate("gcg-drain-gate", "closed", "", control.ID, blocker.ID))}, []beads.Dep{
			{IssueID: control.ID, DependsOnID: row.ID, Type: "blocks"},
			{IssueID: row.ID, DependsOnID: "gcg-drain-gate", Type: depType},
		})
	}
	// migrated is a gate the rig's import minted for the edge row -> blocker:
	// the row records it as gated and, when waits is set, waits on the gate.
	migrated := func(waits bool) func(func(beads.Bead) beads.Bead, beads.Bead, beads.Bead, string) reverseInput {
		return func(gate func(beads.Bead) beads.Bead, row, blocker beads.Bead, depType string) reverseInput {
			row = withRecords(row, rigCrossEdgeRecord{Dir: "out", Type: "blocks", Other: blocker.ID, Action: rigEdgeGated})
			var deps []beads.Dep
			if waits {
				deps = append(deps, beads.Dep{IssueID: row.ID, DependsOnID: "gp-xsg-7", Type: depType})
			}
			return planInput([]beads.Bead{row, gate(planGate("gp-xsg-7", "closed", packs, row.ID, blocker.ID))}, deps)
		}
	}
	shapes := []struct {
		name, row, gate string
		in              func(gate func(beads.Bead) beads.Bead, row, blocker beads.Bead, depType string) reverseInput
	}{
		{"a drain's gate", "gp-item", "gcg-drain-gate", drain},
		{"a migration's gate", "gp-owner", "gp-xsg-7", migrated(true)},
		{"a migration's gate its owner no longer waits on", "gp-owner", "gp-xsg-7", migrated(false)},
	}
	released := func(g beads.Bead) beads.Bead { return g }
	overridden := func(g beads.Bead) beads.Bead { return gateClosedAs(g, dispatch.CrossStoreGateStateOverridden, "") }
	openBlocker := planRow("gp-work", "open", "", "")
	closedBlocked := planRow("gp-work", "closed", "", "")
	closedBlocked.Metadata[beadmeta.WorkOutcomeMetadataKey] = beadmeta.WorkOutcomeBlocked
	cases := []struct {
		name      string
		gate      func(beads.Bead) beads.Bead
		rowStatus string
		blocker   beads.Bead
		dest      string // where the blocker is held
		depType   string
		refused   bool
		restored  bool
	}{
		{"marked overridden", overridden, "open", openBlocker, packs, "blocks", true, false},
		{"closed with no reason", func(g beads.Bead) beads.Bead { return gateClosedAs(g, "", "") }, "open", openBlocker, packs, "blocks", true, false},
		{"closed with an operator's reason", func(g beads.Bead) beads.Bead { return gateClosedAs(g, "", "unblocked by hand") }, "open", openBlocker, packs, "blocks", true, false},
		{"overridden, its blocker closed blocked", overridden, "open", closedBlocked, packs, "blocks", true, false},
		{"released by the controller", released, "open", openBlocker, packs, "blocks", false, true},
		{"retired by the controller", func(g beads.Bead) beads.Bead {
			return gateClosedAs(g, dispatch.CrossStoreGateStateRetired, "drain control gp-drain is closed or gone")
		}, "open", openBlocker, packs, "blocks", false, true},
		{"closed by a run's cancel", func(g beads.Bead) beads.Bead { return gateClosedAs(g, "", sourceworkflow.RunCanceledCloseReason) }, "open", openBlocker, packs, "blocks", false, true},
		{"overridden, its blocker closed", overridden, "open", planRow("gp-work", "closed", "", ""), packs, "blocks", false, true},
		{"overridden, its row closed", overridden, "closed", openBlocker, packs, "blocks", false, true},
		{"overridden, its blocker in another store", overridden, "open", openBlocker, reverseCityDest, "blocks", false, false},
	}
	for _, mode := range []reverseMode{reverseToWork, reverseToRigs} {
		for _, shape := range shapes {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/%s/%s", mode, shape.name, tc.name), func(t *testing.T) {
					in := shape.in(tc.gate, planRow(shape.row, tc.rowStatus, packs, "rig:packs"), tc.blocker, tc.depType)
					in.Mode, in.Rigs = mode, map[string]bool{"packs": true}
					in.Held[tc.dest][tc.blocker.ID] = tc.blocker
					plan := planReverse(in)
					edge := beads.Dep{IssueID: shape.row, DependsOnID: "gp-work", Type: tc.depType}
					restored := slices.ContainsFunc(plan.Edges[packs], func(e beads.DepWithPayload) bool { return e.Dep == edge })
					wantRestored := 0
					if tc.restored {
						wantRestored = 1
					}
					if restored != tc.restored || plan.Restored != wantRestored {
						t.Fatalf("%s -[%s]-> gp-work restored: %v (restored %d), want %v", shape.row, tc.depType, restored, plan.Restored, tc.restored)
					}
					got := planRefusal(plan)
					if !tc.refused {
						if got != "" {
							t.Fatalf("unexpected refusal: %s", got)
						}
						return
					}
					if len(plan.Refusals) != 1 || len(plan.Refusals[0].IDs) != 1 {
						t.Fatalf("refusals = %q, want the one edge once", got)
					}
					for _, want := range []string{
						fmt.Sprintf("%s -[blocks]-> gp-work (gate %s)", shape.row, shape.gate),
						"an operator closed",
						"finish or close the blocker, or close the row",
						"convoy member's edge onto the blocker",
					} {
						if !strings.Contains(got, want) {
							t.Fatalf("the refusal does not say %q:\n%s", want, got)
						}
					}
				})
			}
		}
	}
	t.Run("an edge that does not block onto a drain's gate", func(t *testing.T) {
		for _, mode := range []reverseMode{reverseToWork, reverseToRigs} {
			in := drain(overridden, planRow("gp-item", "open", packs, "rig:packs"), openBlocker, "related")
			in.Mode, in.Rigs = mode, map[string]bool{"packs": true}
			in.Held[packs][openBlocker.ID] = openBlocker
			plan := planReverse(in)
			if got := planRefusal(plan); got != "" || plan.Restored != 1 {
				t.Fatalf("%s: refusals %q, restored %d; want gp-item -[related]-> gp-work restored", mode, got, plan.Restored)
			}
		}
	})
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
	a, b, root := planRow("gcg-1", "closed", "", ""), planRow("gcg-2", "closed", "", ""), planRow("gcg-r", "closed", "", "")
	a.Metadata[beadmeta.RootBeadIDMetadataKey], b.Metadata[beadmeta.RootBeadIDMetadataKey] = "gcg-r", "gcg-r"
	a.Ref, b.Ref, root.Ref = "review", "build", "mol-polecat-work"
	b.Metadata[beadmeta.StepRefMetadataKey] = "already"
	in := planInput([]beads.Bead{a, b, root}, nil)
	in.DropsRef[reverseCityDest] = true
	plan := planReverse(in)
	got := map[string]string{}
	for _, r := range plan.Rows[reverseCityDest] {
		got[r.ID] = r.Metadata[beadmeta.StepRefMetadataKey]
	}
	if got["gcg-1"] != "review" || got["gcg-2"] != "already" || got["gcg-r"] != "" || plan.RefsKept != 1 || plan.RefsDropped != 1 {
		t.Fatalf("step refs = %v, kept %d, roots dropped %d; a root's formula name is not a step ref", got, plan.RefsKept, plan.RefsDropped)
	}
}
