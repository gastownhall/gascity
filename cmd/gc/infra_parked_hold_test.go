package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestInfraParkedStatus pins which rows are parked: an open or in-progress
// row bd holds by an undated deferral or a status the binding cannot record.
func TestInfraParkedStatus(t *testing.T) {
	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		b    beads.Bead
		want string
	}{
		{"undated deferral", beads.Bead{Status: "open", IndefinitelyDeferred: true}, "deferred"},
		{"raw deferred alone", beads.Bead{Status: "open", SourceStatus: "deferred"}, "deferred"},
		{"dated deferral", beads.Bead{Status: "open", IndefinitelyDeferred: true, SourceStatus: "deferred", DeferUntil: &future}, ""},
		{"blocked", beads.Bead{Status: "open", SourceStatus: "blocked"}, "blocked"},
		{"pinned", beads.Bead{Status: "open", SourceStatus: "pinned"}, "pinned"},
		{"custom", beads.Bead{Status: "open", SourceStatus: "qa"}, "qa"},
		{"raw open", beads.Bead{Status: "open", SourceStatus: "open"}, ""},
		{"in progress", beads.Bead{Status: "in_progress", SourceStatus: "in_progress"}, ""},
		{"closed and blocked", beads.Bead{Status: "closed", SourceStatus: "blocked"}, ""},
		{"no raw status", beads.Bead{Status: "open"}, ""},
	} {
		if got := infraParkedStatus(tc.b); got != tc.want {
			t.Errorf("%s: infraParkedStatus = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestInfraCarryParkedHoldIsPureAndLeavesTheCallersMap: the carry is a pure
// function of the row, so every stage that compares reaches the same form,
// and it does not write into the source row's metadata map.
func TestInfraCarryParkedHoldIsPureAndLeavesTheCallersMap(t *testing.T) {
	src := beads.Bead{ID: "gc-1", Status: "open", SourceStatus: "blocked", Metadata: beads.StringMap{"k": "v"}}
	first, second := infraCarryParkedHold(src), infraCarryParkedHold(src)
	if diff := beadCopyDifference(first, second); diff != "" {
		t.Fatalf("two carries of one row differ: %s", diff)
	}
	if _, ok := src.Metadata[beadmeta.MigratedParkedHoldMetadataKey]; ok {
		t.Fatal("the carry wrote into the source row's metadata map")
	}
	if again := infraCarryParkedHold(first); beadCopyDifference(first, again) != "" {
		t.Fatal("carrying a carried row changed it")
	}
}

// TestReverseParkedHold: the reverse restores the recorded hold on a row the
// split city still holds parked, keeps what the split city made of a row it
// released, and refuses a record that does not parse.
func TestReverseParkedHold(t *testing.T) {
	past := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	carried := infraCarryParkedHold(beads.Bead{ID: "gc-1", Status: "open", SourceStatus: "hooked", DeferUntil: &past, Metadata: beads.StringMap{"k": "v"}})

	got, err := reverseParkedHold(carried)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "hooked" || got.DeferUntil == nil || !got.DeferUntil.Equal(past) {
		t.Errorf("restored status %q until %v, want hooked until %v", got.Status, got.DeferUntil, past)
	}
	if _, ok := got.Metadata[beadmeta.MigratedParkedHoldMetadataKey]; ok || got.Metadata["k"] != "v" {
		t.Errorf("restored metadata %v: want the record gone and the rest kept", got.Metadata)
	}

	released := carried
	released.DeferUntil = nil
	if got, err := reverseParkedHold(released); err != nil || got.Status != "open" || got.DeferUntil != nil {
		t.Errorf("a released row came back as %q until %v (%v), want the open row the split city holds", got.Status, got.DeferUntil, err)
	}
	closed := carried
	closed.Status = "closed"
	if got, err := reverseParkedHold(closed); err != nil || got.Status != "closed" {
		t.Errorf("a row closed in the split city came back %q (%v)", got.Status, err)
	}

	broken := beads.Bead{ID: "gc-2", Status: "open", Metadata: beads.StringMap{beadmeta.MigratedParkedHoldMetadataKey: "{not json"}}
	if _, err := reverseParkedHold(broken); err == nil {
		t.Error("an unreadable record was accepted")
	}
}

// TestReversePlanRestoresAParkedHold: --to-work writes a parked row back with
// the status bd parked it with, and refuses a row whose record does not
// parse rather than return it ready.
func TestReversePlanRestoresAParkedHold(t *testing.T) {
	source := planRow("gc-p1", "open", infraMigrationCityStamp, "")
	source.SourceStatus = "blocked"
	parked := infraCarryParkedHold(source)
	broken := planRow("gc-p2", "open", infraMigrationCityStamp, "")
	broken.Metadata[beadmeta.MigratedParkedHoldMetadataKey] = "{}"

	plan := planReverse(planInput([]beads.Bead{parked}, nil))
	if len(plan.Refusals) != 0 {
		t.Fatalf("refusals %+v", plan.Refusals)
	}
	rows := plan.Rows[reverseCityDest]
	if len(rows) != 1 || rows[0].Status != "blocked" || rows[0].DeferUntil != nil {
		t.Fatalf("the reverse writes %+v, want gc-p1 blocked with no defer_until", rows)
	}
	if _, ok := rows[0].Metadata[beadmeta.MigratedParkedHoldMetadataKey]; ok {
		t.Error("the reverse wrote the parked-hold record back into the work store")
	}

	plan = planReverse(planInput([]beads.Bead{broken}, nil))
	if len(plan.Refusals) != 1 || !slices.Contains(plan.Refusals[0].IDs, "gc-p2 (it names no status)") {
		t.Fatalf("refusals %+v, want gc-p2's unreadable record refused", plan.Refusals)
	}
}

// TestRigImportCarriesAParkedHold is ga-uv5rch: a rig infrastructure bead
// bd parks by status crosses into the binding parked, as the city copy
// carries it, not open and ready.
func TestRigImportCarriesAParkedHold(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	rig := beads.NewMemStore()
	rig.IDPrefix = "gp"
	rig.HonorExplicitIDs = true
	f.fixed["packs"] = rig
	parked := graphRow("gp-d", "gp-d", "open", nil)
	parked.SourceStatus = "pinned"
	deferred := graphRow("gp-e", "gp-e", "open", nil)
	deferred.IndefinitelyDeferred = true
	for _, b := range []beads.Bead{parked, deferred} {
		if _, err := rig.Create(b); err != nil {
			t.Fatal(err)
		}
	}
	if code, stdout, stderr := f.importRigs("packs"); code != 0 {
		t.Fatalf("import = %d: %s%s", code, stdout, stderr)
	}
	binding := f.binding()
	ready := readyIDsOf(t, binding)
	for id, status := range map[string]string{"gp-d": "pinned", "gp-e": "deferred"} {
		got, err := binding.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		hold, err := parseInfraParkedHold(got.Metadata[beadmeta.MigratedParkedHoldMetadataKey])
		if err != nil || hold.Status != status || got.DeferUntil == nil || !got.DeferUntil.Equal(infraParkedDeferUntil) {
			t.Errorf("%s imported as %+v, hold %+v (%v); want parked %s", id, got, hold, err, status)
		}
		if slices.Contains(ready, id) {
			t.Errorf("%s is ready in the binding although its rig parked it", id)
		}
	}
}

// TestTheClearRecordsTwoEdgeTypesOnOnePair is ga-wcqnj5's case: a work bead
// holds a parent-child and a related edge onto one bead the copy moves, as a
// file or memory work store allows (a parent-child beside one edge of another
// type). The clear drops and records both, with their types, and loses
// neither. The moved bead is closed: a work child of an open workflow is a
// membership the plan refuses before the clear.
func TestTheClearRecordsTwoEdgeTypesOnOnePair(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := stubInfraMigrationSource(t)
	moved := mustCreateInfraBead(t, source, beads.Bead{Title: "finished molecule", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	if err := source.Close(moved.ID); err != nil {
		t.Fatal(err)
	}
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "work under it", Type: "task"})
	for _, typ := range []string{"parent-child", "related"} {
		if err := source.DepAdd(work.ID, moved.ID, typ); err != nil {
			t.Fatalf("seeding %s: %v", typ, err)
		}
	}
	if deps, err := source.DepList(work.ID, "down"); err != nil || len(deps) != 2 {
		t.Fatalf("the fixture's store keeps %d edge(s) on the pair (%v), want 2", len(deps), err)
	}
	request := storageTestRequest(t, infraSplitConfig(t.TempDir()+"/store"))
	var log bytes.Buffer
	report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log)
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", report.Outcome, log.String())
	}
	if len(report.LostCrossEdges) != 0 {
		t.Fatalf("lost %v", report.LostCrossEdges)
	}
	got, err := source.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, r := range droppedEdgeRecords(t, got) {
		if r.Other == moved.ID {
			types = append(types, r.Type)
		}
	}
	slices.Sort(types)
	if strings.Join(types, ",") != "parent-child,related" {
		t.Fatalf("%s records edge types %v onto %s, want both parent-child and related", work.ID, types, moved.ID)
	}
}
