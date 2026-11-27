package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		b    beads.Bead
		want string
	}{
		{"undated deferral", beads.Bead{Status: "open", IndefinitelyDeferred: true}, "deferred"},
		{"raw deferred alone", beads.Bead{Status: "open", SourceStatus: "deferred"}, "deferred"},
		{"dated deferral", beads.Bead{Status: "open", IndefinitelyDeferred: true, SourceStatus: "deferred", DeferUntil: &future}, ""},
		// bd keeps it parked until its own sweep; it crosses as its date and
		// wakes in the binding, as that sweep would wake it.
		{"deferral past its date", beads.Bead{Status: "open", IndefinitelyDeferred: true, SourceStatus: "deferred", DeferUntil: &past}, ""},
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
	} else if got.DeferUntil == nil || !got.DeferUntil.Equal(past) {
		// The sentinel is the copy's, not the row's: a row reopened later in
		// the work store would stay hidden until 9999.
		t.Errorf("a row closed in the split city came back deferred until %v, want the %v it had", got.DeferUntil, past)
	}
	claimed := infraCarryParkedHold(beads.Bead{ID: "gc-3", Status: "open", SourceStatus: "blocked"})
	claimed.Status = "in_progress"
	if got, err := reverseParkedHold(claimed); err != nil || got.Status != "in_progress" || got.DeferUntil != nil {
		t.Errorf("a row claimed in the split city came back %q until %v (%v), want in_progress with no deferral", got.Status, got.DeferUntil, err)
	}

	broken := beads.Bead{ID: "gc-2", Status: "open", Metadata: beads.StringMap{beadmeta.MigratedParkedHoldMetadataKey: "{not json"}}
	if _, err := reverseParkedHold(broken); err == nil {
		t.Error("an unreadable record was accepted")
	}

	// The copy's deferral with its record gone says the row was parked but
	// not how: returned as it is, the row would stay hidden for good, and
	// nothing on it would say why.
	stripped := infraCarryParkedHold(beads.Bead{ID: "gc-4", Status: "open", SourceStatus: "blocked"})
	delete(stripped.Metadata, beadmeta.MigratedParkedHoldMetadataKey)
	for _, status := range []string{"open", "closed"} {
		stripped.Status = status
		if _, err := reverseParkedHold(stripped); err == nil {
			t.Errorf("a %s row deferred until %v with no record was returned as it is", status, infraParkedDeferUntil)
		}
	}
	dated := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, plain := range []beads.Bead{{ID: "gc-5", Status: "open"}, {ID: "gc-6", Status: "open", DeferUntil: &dated}} {
		if got, err := reverseParkedHold(plain); err != nil || got.Status != plain.Status || got.DeferUntil != plain.DeferUntil {
			t.Errorf("a row the copy never parked came back %q until %v (%v)", got.Status, got.DeferUntil, err)
		}
	}
}

// TestACarryRecordsARowAlreadyAtTheParkedDate: a row its source store already
// defers to exactly infraParkedDeferUntil (bd's `defer --until 9999-12-31` on
// a UTC host) is not parked by the copy, but it gets the record a parked row
// gets: with none, the reverse would take its date for the copy's and refuse
// the row as a parked one whose record was lost. The row keeps its own status
// and date, carrying it again changes nothing, and the reverse returns it as
// it was. A row the split city moved out of the status the copy carried it
// with keeps the split city's status.
func TestACarryRecordsARowAlreadyAtTheParkedDate(t *testing.T) {
	sentinel := infraParkedDeferUntil
	for _, tc := range []struct {
		name string
		src  beads.Bead
		raw  string
	}{
		{"bd deferral", beads.Bead{ID: "gc-1", Status: "open", SourceStatus: "deferred", IndefinitelyDeferred: true}, "deferred"},
		{"raw open", beads.Bead{ID: "gc-2", Status: "open", SourceStatus: "open"}, "open"},
		{"in progress", beads.Bead{ID: "gc-3", Status: "in_progress", SourceStatus: "in_progress"}, "in_progress"},
		{"closed", beads.Bead{ID: "gc-4", Status: "closed", SourceStatus: "closed"}, "closed"},
		{"no raw status", beads.Bead{ID: "gc-5", Status: "open"}, "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.src.DeferUntil = &sentinel
			carried := infraCarryParkedHold(tc.src)
			if carried.Status != tc.src.Status || carried.DeferUntil == nil || !carried.DeferUntil.Equal(sentinel) {
				t.Fatalf("carried as %q until %v, want its own %q until %v", carried.Status, carried.DeferUntil, tc.src.Status, sentinel)
			}
			hold, err := parseInfraParkedHold(carried.Metadata[beadmeta.MigratedParkedHoldMetadataKey])
			if err != nil {
				t.Fatalf("the carried row's record: %v", err)
			}
			if until, err := hold.deferUntil(); hold.Status != tc.raw || err != nil || until == nil || !until.Equal(sentinel) {
				t.Fatalf("the record holds %+v, want status %q until %v", hold, tc.raw, sentinel)
			}
			if again := infraCarryParkedHold(carried); beadCopyDifference(carried, again) != "" {
				t.Fatalf("carrying the carried row changed it: %s", beadCopyDifference(carried, again))
			}

			back, err := reverseParkedHold(carried)
			if err != nil {
				t.Fatalf("the reverse refused the row: %v", err)
			}
			if back.Status != tc.raw || back.DeferUntil == nil || !back.DeferUntil.Equal(sentinel) {
				t.Errorf("the reverse returns %q until %v, want %q until %v", back.Status, back.DeferUntil, tc.raw, sentinel)
			}
			if _, ok := back.Metadata[beadmeta.MigratedParkedHoldMetadataKey]; ok {
				t.Error("the reverse kept the record")
			}
		})
	}

	// Released or reopened in the split city: open, still at the row's own
	// date. The record's status is not put back over the split city's.
	for _, raw := range []string{"in_progress", "closed"} {
		moved := infraCarryParkedHold(beads.Bead{ID: "gc-" + raw, Status: raw, SourceStatus: raw, DeferUntil: &sentinel})
		moved.Status = "open"
		back, err := reverseParkedHold(moved)
		if err != nil || back.Status != "open" || back.DeferUntil == nil || !back.DeferUntil.Equal(sentinel) {
			t.Errorf("a %s row the split city reopened came back %q until %v (%v), want open until %v", raw, back.Status, back.DeferUntil, err, sentinel)
		}
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

	stripped := planRow("gc-p3", "open", infraMigrationCityStamp, "")
	stripped.SourceStatus = "hooked"
	stripped = infraCarryParkedHold(stripped)
	delete(stripped.Metadata, beadmeta.MigratedParkedHoldMetadataKey)
	plan = planReverse(planInput([]beads.Bead{stripped}, nil))
	if len(plan.Refusals) != 1 || len(plan.Refusals[0].IDs) != 1 || !strings.HasPrefix(plan.Refusals[0].IDs[0], "gc-p3 (") {
		t.Fatalf("refusals %+v, want gc-p3, deferred until %v with no record, refused", plan.Refusals, infraParkedDeferUntil)
	}
	if !strings.Contains(plan.Refusals[0].Remedy, "--set-metadata") {
		t.Errorf("the remedy %q does not name the write that repairs the record", plan.Refusals[0].Remedy)
	}

	// A deferral bd itself set to the parked date crosses with a record, so
	// the reverse writes it back as bd held it rather than refusing it.
	sentinel := infraParkedDeferUntil
	own := planRow("gc-p4", "open", infraMigrationCityStamp, "")
	own.SourceStatus, own.IndefinitelyDeferred, own.DeferUntil = "deferred", true, &sentinel
	plan = planReverse(planInput([]beads.Bead{infraCarryParkedHold(own)}, nil))
	if len(plan.Refusals) != 0 {
		t.Fatalf("refusals %+v, want gc-p4's own deferral written back", plan.Refusals)
	}
	if rows := plan.Rows[reverseCityDest]; len(rows) != 1 || rows[0].Status != "deferred" || rows[0].DeferUntil == nil || !rows[0].DeferUntil.Equal(sentinel) {
		t.Fatalf("the reverse writes %+v, want gc-p4 deferred until %v", rows, sentinel)
	}
}

// TestReverseToWorkReturnsAParkedRowParked is the reverse end to end: --to-work
// writes a row the copy parked back into the work store with the raw status
// it had, through the reverse's write and its read-back verify, which compares
// the status exactly. A parked row the split city closed returns closed,
// without the copy's deferral.
func TestReverseToWorkReturnsAParkedRowParked(t *testing.T) {
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	carried := func(title, status string) beads.Bead {
		return infraCarryParkedHold(beads.Bead{Title: title, Type: "task", Status: "open", SourceStatus: status, Metadata: beads.StringMap{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}})
	}
	parked := createInBinding(t, city, carried("a step an agent marked blocked", "blocked"), false)
	closed := createInBinding(t, city, carried("a step the split city closed", "hooked"), true)

	code, stdout, stderr := city.reverse(t, reverseOptions{})
	if code != 0 {
		t.Fatalf("--to-work = %d: %s%s", code, stdout, stderr)
	}
	for _, want := range []struct{ id, status string }{{parked.ID, "blocked"}, {closed.ID, "closed"}} {
		got, err := city.source.Get(want.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want.status || got.DeferUntil != nil {
			t.Errorf("%s returned %q until %v, want %q with no deferral", want.id, got.Status, got.DeferUntil, want.status)
		}
		if _, ok := got.Metadata[beadmeta.MigratedParkedHoldMetadataKey]; ok {
			t.Errorf("%s returned with the parked-hold record", want.id)
		}
	}
}

// TestRigImportCarriesAParkedHold is ga-uv5rch: a rig infrastructure bead
// bd parks by status crosses into the binding parked, as the city copy
// carries it, not open and ready, and the import's backup records the hold
// as the city clear's does.
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
	// The import's backup is the rig rollback's only source for the rows it
	// cleared, and beads.Bead keeps the raw-status signals off its JSON, so
	// the entry records them beside the row.
	backup, present, err := readInfraRetainedBackupAt(f.loc("packs").BackupPath())
	if err != nil || !present {
		t.Fatalf("reading the rig's backup: present=%v err=%v", present, err)
	}
	entries := map[string]infraBackupEntry{}
	for _, e := range backup {
		entries[e.Bead.ID] = e
	}
	if e := entries["gp-d"]; e.SourceStatus != "pinned" || e.IndefinitelyDeferred {
		t.Errorf("the rig's backup records gp-d with source_status %q, indefinitely_deferred %v; want pinned, false", e.SourceStatus, e.IndefinitelyDeferred)
	}
	if e := entries["gp-e"]; !e.IndefinitelyDeferred {
		t.Errorf("the rig's backup records gp-e as not indefinitely deferred (source_status %q)", e.SourceStatus)
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

// TestTheReCopyFromTheBackupKeepsAParkedHold is the status-hold twin of
// TestTheReCopyFromTheBackupKeepsTheHoldOnOpenWork: once the clear has removed
// the work store's copies, the retained-source backup is the only source a
// re-copy has (`gc storage migrate --from-backup`, the recovery for a lost
// binding), and it must carry each parked bead parked again, as the first copy
// did. beads.Bead keeps the raw-status signals off its JSON, so the backup
// records them beside the row.
func TestTheReCopyFromTheBackupKeepsAParkedHold(t *testing.T) {
	cityPath := t.TempDir()
	source := stubInfraMigrationSource(t)
	wisp := func(title string, b beads.Bead) beads.Bead {
		b.Title, b.Type, b.Metadata = title, "task", beads.StringMap{"gc.kind": "workflow"}
		return mustCreateInfraBead(t, source, b)
	}
	parked := map[string]beads.Bead{
		"blocked":  wisp("blocked", beads.Bead{SourceStatus: "blocked"}),
		"deferred": wisp("undated deferral", beads.Bead{IndefinitelyDeferred: true}),
	}
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if report := migrateInfraClasses(t, cityPath, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", report.Outcome, log.String())
	}
	for _, b := range parked {
		if _, err := source.Get(b.ID); err == nil {
			t.Fatalf("fixture: the clear left %s in the work store", b.ID)
		}
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	if err := os.RemoveAll(target.Dir); err != nil {
		t.Fatal(err)
	}

	log.Reset()
	if got := runInfraClassMigrationFrom(cityPath, target, infraMigrationFromBackup, "gc storage migrate", &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("backup re-copy = %s: %s", got.Outcome, log.String())
	}
	binding := openMigratedDestination(t, target)
	ready := readyIDsOf(t, binding)
	for status, b := range parked {
		got, err := binding.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		hold, err := parseInfraParkedHold(got.Metadata[beadmeta.MigratedParkedHoldMetadataKey])
		if err != nil || hold.Status != status || got.DeferUntil == nil || !got.DeferUntil.Equal(infraParkedDeferUntil) {
			t.Errorf("%s re-copied with defer_until %v and hold %+v (%v), want parked %s", b.ID, got.DeferUntil, hold, err, status)
		}
		if slices.Contains(ready, b.ID) {
			t.Errorf("%s is ready in the re-copied binding although its source store parked it", b.ID)
		}
	}
}

// TestStorageRecoverStrandedCarriesAParkedHold: recover-stranded copies a row
// bd parks by its status parked, as the copy carries it, and its own equality
// check compares that carried form, so the repair converges rather than
// reading the deferral it wrote as a difference.
func TestStorageRecoverStrandedCarriesAParkedHold(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	parked := mustCreateInfraBead(t, source, beads.Bead{Title: "parked wisp", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}, SourceStatus: "blocked"})
	if got, err := source.Get(parked.ID); err != nil || infraParkedStatus(got) != "blocked" {
		t.Fatalf("fixture: the work store reads %s as %+v (%v), not parked", parked.ID, got, err)
	}
	if got := strandedIDs(t, cityPath, target); !slices.Equal(got, []string{parked.ID}) {
		t.Fatalf("fixture: stranded %v, want only %s", got, parked.ID)
	}

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery exited %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	binding := openMigratedDestination(t, target)
	got, err := binding.Get(parked.ID)
	if err != nil {
		t.Fatalf("the binding cannot read %s after the repair: %v", parked.ID, err)
	}
	hold, err := parseInfraParkedHold(got.Metadata[beadmeta.MigratedParkedHoldMetadataKey])
	if err != nil || hold.Status != "blocked" || got.DeferUntil == nil || !got.DeferUntil.Equal(infraParkedDeferUntil) {
		t.Errorf("%s crossed with defer_until %v and hold %+v (%v), want parked blocked", parked.ID, got.DeferUntil, hold, err)
	}
	if slices.Contains(readyIDsOf(t, binding), parked.ID) {
		t.Errorf("%s is ready in the binding although the work store parked it", parked.ID)
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

// TestTheRunbookRestorePutsTheRawStatusBack runs the jq transform of the
// runbook's "Restoring the work store from the backup", which the rig
// rollback runs too, over a backup this build writes. Each row that was not
// closed goes back to bd in the raw status bd held it in, a parked one
// included, rather than in the "open" gc folds it to, which bd would import
// ready. A dated deferral keeps its date. A row whose entry records no raw
// status, as an earlier build wrote it, keeps the status the entry holds.
func TestTheRunbookRestorePutsTheRawStatusBack(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not installed")
	}
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "runbooks", "split-storage-classes.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(doc), "cat > /tmp/gc-restore.jq <<'JQ'\n")
	program, _, closed := strings.Cut(rest, "\nJQ\n")
	if !ok || !closed {
		t.Fatal("the runbook has no gc-restore.jq transform")
	}

	dated := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := []struct {
		bead beads.Bead
		want string
	}{
		{beads.Bead{ID: "gc-blocked", Status: "open", SourceStatus: "blocked"}, "blocked"},
		{beads.Bead{ID: "gc-hooked", Status: "open", SourceStatus: "hooked"}, "hooked"},
		{beads.Bead{ID: "gc-undated", Status: "open", SourceStatus: "deferred", IndefinitelyDeferred: true}, "deferred"},
		{beads.Bead{ID: "gc-dated", Status: "open", SourceStatus: "deferred", IndefinitelyDeferred: true, DeferUntil: &dated}, "deferred"},
		{beads.Bead{ID: "gc-flag-only", Status: "open", IndefinitelyDeferred: true}, "deferred"},
		{beads.Bead{ID: "gc-open", Status: "open", SourceStatus: "open"}, "open"},
		{beads.Bead{ID: "gc-working", Status: "in_progress", SourceStatus: "in_progress"}, "in_progress"},
		{beads.Bead{ID: "gc-closed", Status: "closed", SourceStatus: "closed"}, "closed"},
		{beads.Bead{ID: "gc-unrecorded", Status: "open"}, "open"},
	}
	entries := make([]infraBackupEntry, 0, len(rows))
	for _, r := range rows {
		r.bead.Title, r.bead.Type = r.bead.ID, "task"
		entries = append(entries, newInfraBackupEntry(r.bead))
	}
	dir := t.TempDir()
	backup := filepath.Join(dir, "infra.retained-source.jsonl")
	if err := writeInfraRetainedBackupAt(dir, backup, "infra", entries); err != nil {
		t.Fatal(err)
	}
	programPath := filepath.Join(dir, "gc-restore.jq")
	if err := os.WriteFile(programPath, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(jq, "-c", "-f", programPath, backup).Output()
	if err != nil {
		t.Fatalf("the runbook's transform failed: %v", err)
	}
	type restored struct {
		ID         string     `json:"id"`
		Status     string     `json:"status"`
		DeferUntil *time.Time `json:"defer_until"`
	}
	got := map[string]restored{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var r restored
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("decoding %q: %v", line, err)
		}
		got[r.ID] = r
	}
	for _, r := range rows {
		if got[r.bead.ID].Status != r.want {
			t.Errorf("%s restores with status %q, want %q", r.bead.ID, got[r.bead.ID].Status, r.want)
		}
	}
	if until := got["gc-dated"].DeferUntil; until == nil || !until.Equal(dated) {
		t.Errorf("the dated deferral restores until %v, want %v", until, dated)
	}
}

// TestBdByIDClaimRefusesACarriedParkedBead: `gc bd update <id> --claim` of a
// bead the storage migration carried parked is refused, and the row stays
// parked. The binding's claim refuses a row deferred into the future, as bd
// refuses to claim a parked bead. The refusal says why, with the status the
// record holds, and names the ways out, since no gc bd verb lifts the
// deferral.
func TestBdByIDClaimRefusesACarriedParkedBead(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	parked := mustCreateClassBead(t, classStore, infraCarryParkedHold(beads.Bead{Title: "a step an agent marked blocked", Type: "task", Status: "open", SourceStatus: "blocked"}))

	t.Setenv("BEADS_ACTOR", "by-id-tester")
	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"update", parked.ID, "--claim"}, &stdout, &stderr)
	if !handled || code == 0 {
		t.Fatalf("claiming the parked %s = (%d, %t), want a refusal: %s%s", parked.ID, code, handled, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"deferred until " + infraParkedDeferUntil.Format(time.RFC3339),
		`"blocked"`,
		beadmeta.MigratedParkedHoldMetadataKey,
		storageReverseInstruction(reverseToWork, nil),
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the refusal does not say %q: %s", want, stderr.String())
		}
	}
	got, err := classStore.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "open" || got.Assignee != "" || got.DeferUntil == nil || !got.DeferUntil.Equal(infraParkedDeferUntil) {
		t.Errorf("the refused claim changed the parked bead: status %q, assignee %q, defer_until %v", got.Status, got.Assignee, got.DeferUntil)
	}
}

// TestBdByIDStatusWriteRefusesACarriedParkedBead: `gc bd update <id> --status
// in_progress`, with or without --assignee, of a bead the storage migration
// carried parked is refused, and nothing is written. A status write does not
// lift the deferral, so the bead would be in progress while every hook drops
// it as deferred and the controller counts it as its claimant's work: the
// state the claim refusal keeps out. A bead deferred into the future is moved
// to no status but open or closed, so a metadata write and a release to open
// stay served and leave it parked, closing it, its exit in the split city,
// stays served, and the closed row, which keeps the deferral, is not set in
// progress either. An unparked bead takes the refused write.
func TestBdByIDStatusWriteRefusesACarriedParkedBead(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	update := func(args ...string) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code, handled := maybeRouteBdByID(cityPath, "", append([]string{"update"}, args...), &stdout, &stderr)
		if !handled {
			t.Fatalf("update %q fell through to the bd subprocess: %s", args, stderr.String())
		}
		return code, stdout.String() + stderr.String()
	}
	parked := mustCreateClassBead(t, classStore, infraCarryParkedHold(beads.Bead{Title: "a step an agent marked blocked", Type: "task", Status: "open", SourceStatus: "blocked"}))
	record := parked.Metadata[beadmeta.MigratedParkedHoldMetadataKey]
	if record == "" {
		t.Fatalf("the carried bead holds no %s: %v", beadmeta.MigratedParkedHoldMetadataKey, parked.Metadata)
	}
	unchanged := func(status string) {
		t.Helper()
		got, err := classStore.Get(parked.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != status || got.Assignee != "" || got.Metadata["note"] != "" ||
			got.DeferUntil == nil || !got.DeferUntil.Equal(infraParkedDeferUntil) ||
			got.Metadata[beadmeta.MigratedParkedHoldMetadataKey] != record {
			t.Errorf("the parked bead is no longer parked as it was: status %q (want %q), assignee %q, note %q, defer_until %v, record %q",
				got.Status, status, got.Assignee, got.Metadata["note"], got.DeferUntil, got.Metadata[beadmeta.MigratedParkedHoldMetadataKey])
		}
	}
	refused := func(args ...string) {
		t.Helper()
		code, out := update(args...)
		if code == 0 {
			t.Errorf("update %q was served, want a refusal: %s", args, out)
			return
		}
		for _, want := range []string{
			"refused: it is deferred until " + infraParkedDeferUntil.Format(time.RFC3339),
			`"blocked"`,
			beadmeta.MigratedParkedHoldMetadataKey,
			storageReverseInstruction(reverseToWork, nil),
			"Nothing was written.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the refusal of update %q does not say %q: %s", args, want, out)
			}
		}
	}

	for _, args := range [][]string{
		{parked.ID, "--status", "in_progress"},
		{parked.ID, "--status", "in_progress", "--assignee", "worker"},
		{parked.ID, "--status=hooked", "--set-metadata", "note=x"},
	} {
		refused(args...)
		unchanged("open")
	}

	// A write that moves the bead to no new status is served and keeps the
	// deferral: the record's remedy writes metadata alone, and a release to
	// open, which the session-recovery scripts run, leaves it parked.
	for _, args := range [][]string{
		{parked.ID, "--set-metadata", "seen=y"},
		{parked.ID, "--status", "open", "--assignee", ""},
	} {
		if code, out := update(args...); code != 0 {
			t.Errorf("update %q of the parked bead exited %d, want it served: %s", args, code, out)
		}
		unchanged("open")
	}

	plain := mustCreateClassBead(t, classStore, beads.Bead{Title: "a step nobody parked", Type: "task"})
	if code, out := update(plain.ID, "--status", "in_progress", "--assignee", "worker"); code != 0 {
		t.Fatalf("the same write on an unparked bead exited %d: %s", code, out)
	}
	if got, err := classStore.Get(plain.ID); err != nil || got.Status != "in_progress" || got.Assignee != "worker" {
		t.Errorf("the unparked bead after the write: status %q, assignee %q, err %v; want in_progress held by worker", got.Status, got.Assignee, err)
	}

	// Closing is the parked bead's exit in the split city. The closed row
	// keeps the copy's deferral, so it is not set in progress either, and
	// reopening it parks it again.
	if code, out := update(parked.ID, "--status", "closed"); code != 0 {
		t.Fatalf("closing the parked bead exited %d: %s", code, out)
	}
	refused(parked.ID, "--status", "in_progress")
	unchanged("closed")
	if code, out := update(parked.ID, "--status", "open"); code != 0 {
		t.Errorf("reopening the closed parked bead exited %d, want it served: %s", code, out)
	}
	unchanged("open")

	// A row a claim left in progress under the deferral, before claims were
	// refused, is released by the write the session-recovery scripts run, and
	// so parked again.
	stuck := infraCarryParkedHold(beads.Bead{Title: "a parked step a claim took", Type: "task", Status: "open", SourceStatus: "blocked"})
	stuck.Status, stuck.Assignee = "in_progress", "gone"
	stuck = mustCreateClassBead(t, classStore, stuck)
	if code, out := update(stuck.ID, "--status", "open", "--assignee", ""); code != 0 {
		t.Errorf("releasing the row stuck in progress exited %d, want it served: %s", code, out)
	}
	if got, err := classStore.Get(stuck.ID); err != nil || got.Status != "open" || got.Assignee != "" || got.DeferUntil == nil || !got.DeferUntil.Equal(infraParkedDeferUntil) {
		t.Errorf("the released row: status %q, assignee %q, defer_until %v, err %v; want it open, unassigned and parked", got.Status, got.Assignee, got.DeferUntil, err)
	}
}

// TestBdByIDDeferredStatusConflict pins which status writes the by-id door
// refuses: one that would leave a future defer_until on a bead neither open
// nor closed. A deferral held in the status alone, as bd's undated deferred
// status, is lifted by the write, a date already past holds nothing, and a
// write naming the row's own status moves nothing.
func TestBdByIDDeferredStatusConflict(t *testing.T) {
	now := time.Date(2026, time.November, 13, 0, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	status := func(s string) beads.UpdateOpts { return beads.UpdateOpts{Status: &s} }
	for _, tc := range []struct {
		name    string
		current beads.Bead
		update  beads.UpdateOpts
		refused bool
	}{
		{"open and deferred, to in_progress", beads.Bead{Status: "open", DeferUntil: &future}, status("in_progress"), true},
		{"open and deferred, to another status", beads.Bead{Status: "open", DeferUntil: &future}, status("hooked"), true},
		{"closed and deferred, to in_progress", beads.Bead{Status: "closed", DeferUntil: &future}, status("in_progress"), true},
		{"open and deferred, to open", beads.Bead{Status: "open", DeferUntil: &future}, status("open"), false},
		{"open and deferred, to closed", beads.Bead{Status: "open", DeferUntil: &future}, status("closed"), false},
		{"in_progress and deferred, to in_progress", beads.Bead{Status: "in_progress", DeferUntil: &future}, status("in_progress"), false},
		{"in_progress and deferred, to open", beads.Bead{Status: "in_progress", DeferUntil: &future}, status("open"), false},
		{"open, deferral past, to in_progress", beads.Bead{Status: "open", DeferUntil: &past}, status("in_progress"), false},
		{"open, deferred by its status alone, to in_progress", beads.Bead{Status: "open", SourceStatus: "deferred", IndefinitelyDeferred: true}, status("in_progress"), false},
		{"open and deferred, no status", beads.Bead{Status: "open", DeferUntil: &future}, beads.UpdateOpts{Metadata: map[string]string{"k": "v"}}, false},
	} {
		if got := bdByIDDeferredStatusConflict(tc.current, tc.update, now); (got != "") != tc.refused {
			t.Errorf("%s: conflict %q, want refused=%t", tc.name, got, tc.refused)
		}
	}
}
