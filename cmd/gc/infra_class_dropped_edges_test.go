package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// droppedEdgeRecords parses the gc.migrated_cross_edges record on b.
func droppedEdgeRecords(t *testing.T, b beads.Bead) []rigCrossEdgeRecord {
	t.Helper()
	raw, ok := b.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]
	if !ok {
		return nil
	}
	var records []rigCrossEdgeRecord
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		t.Fatalf("%s carries a %s record that does not parse: %v (%q)", b.ID, beadmeta.MigratedCrossEdgesMetadataKey, err, raw)
	}
	return records
}

// TestTheClearRecordsEachDroppedCrossEdgeOnItsWorkBead is ga-4tnypl: a bd or
// native-Dolt work store refuses an edge onto a bead it no longer holds, so the
// clear drops every edge from a work bead into a moved bead (discovered-from,
// related, tracks...). The loss used to be recorded only in the city-side
// cleared note; the work bead kept no trace of its provenance. Each dropped
// edge is now recorded on the work bead itself, in the rig importer's
// gc.migrated_cross_edges shape, beside any record the bead already carries.
func TestTheClearRecordsEachDroppedCrossEdgeOnItsWorkBead(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := danglingRefusingStore{beads.NewMemStore()}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	foundIn := mustCreateInfraBead(t, source, beads.Bead{Title: "wisp the bug was found in", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	if err := source.Close(foundIn.ID); err != nil {
		t.Fatal(err)
	}
	related := mustCreateInfraBead(t, source, beads.Bead{Title: "open molecule", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	earlier := `[{"dir":"out","type":"tracks","other":"rig-1","action":"dropped"}]`
	bug := mustCreateInfraBead(t, source, beads.Bead{Title: "a bug", Type: "bug", Metadata: beads.StringMap{beadmeta.MigratedCrossEdgesMetadataKey: earlier}})
	plain := mustCreateInfraBead(t, source, beads.Bead{Title: "work linked to nothing moved", Type: "task"})
	for _, edge := range []beads.Dep{
		{IssueID: bug.ID, DependsOnID: foundIn.ID, Type: "discovered-from"},
		{IssueID: bug.ID, DependsOnID: related.ID, Type: "related"},
	} {
		if err := source.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type); err != nil {
			t.Fatalf("seeding %v: %v", edge, err)
		}
	}
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))

	var log bytes.Buffer
	report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log)
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", report.Outcome, log.String())
	}
	if len(report.LostCrossEdges) != 2 {
		t.Fatalf("lost cross edges = %v, want the two the store refused", report.LostCrossEdges)
	}

	got, err := source.Get(bug.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []rigCrossEdgeRecord{
		{Dir: "out", Type: "tracks", Other: "rig-1", Action: rigEdgeDropped},
		{Dir: "out", Type: "discovered-from", Other: foundIn.ID, OtherStatus: "closed", Action: rigEdgeDropped},
		{Dir: "out", Type: "related", Other: related.ID, OtherStatus: "open", Action: rigEdgeDropped},
	}
	records := droppedEdgeRecords(t, got)
	if len(records) != len(want) {
		t.Fatalf("%s records %+v, want %+v", bug.ID, records, want)
	}
	for i := range want {
		if records[i] != want[i] {
			t.Fatalf("%s record %d = %+v, want %+v (all: %+v)", bug.ID, i, records[i], want[i], records)
		}
	}
	untouched, err := source.Get(plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := untouched.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]; ok {
		t.Fatalf("%s, which links to nothing moved, gained a %s record", plain.ID, beadmeta.MigratedCrossEdgesMetadataKey)
	}
	if !strings.Contains(log.String(), "recorded on their work bead") {
		t.Fatalf("the migrate output does not say where the dropped edges are recorded: %s", log.String())
	}
}

// TestTheClearRecordsNothingOnAStoreThatKeepsTheEdges: a work store that can
// hold an edge onto a bead it no longer has keeps every edge, so no work bead
// gains a record of a loss that did not happen.
func TestTheClearRecordsNothingOnAStoreThatKeepsTheEdges(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := stubInfraMigrationSource(t)
	moved := mustCreateInfraBead(t, source, beads.Bead{Title: "wisp", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	bug := mustCreateInfraBead(t, source, beads.Bead{Title: "a bug", Type: "bug"})
	if err := source.DepAdd(bug.ID, moved.ID, "discovered-from"); err != nil {
		t.Fatal(err)
	}
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	var log bytes.Buffer
	if report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", report.Outcome, log.String())
	}
	got, err := source.Get(bug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw, ok := got.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]; ok {
		t.Fatalf("%s gained %s=%s though its edge was kept", bug.ID, beadmeta.MigratedCrossEdgesMetadataKey, raw)
	}
}

// TestPreflightListsTheCrossEdgesTheClearMayDrop is ga-4tnypl's preflight leg:
// the operator sees, before the window, every edge from a work bead into a bead
// the copy moves that a bd or native-Dolt work store will drop. A blocking edge
// onto a closed bead is released, not dropped, so it is not listed. It does
// not block: the clear records each loss.
func TestPreflightListsTheCrossEdgesTheClearMayDrop(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 0)
	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	moved := mustCreateInfraBead(t, source, beads.Bead{Title: "wisp", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	done := mustCreateInfraBead(t, source, beads.Bead{Title: "finished wisp", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	if err := source.Close(done.ID); err != nil {
		t.Fatal(err)
	}
	bug := mustCreateInfraBead(t, source, beads.Bead{Title: "a bug", Type: "bug"})
	for _, edge := range []beads.Dep{
		{IssueID: bug.ID, DependsOnID: moved.ID, Type: "discovered-from"},
		{IssueID: bug.ID, DependsOnID: done.ID, Type: "blocks"},
	} {
		if err := source.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight blocked on edges the clear records: exit %d stdout=%q", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "1 edge(s) from work beads") || !strings.Contains(out, bug.ID+" -[discovered-from]-> "+moved.ID) {
		t.Fatalf("preflight does not list the edge at risk: %s", out)
	}
	if strings.Contains(out, bug.ID+" -[blocks]-> "+done.ID) {
		t.Fatalf("preflight lists a satisfied blocking edge the clear releases: %s", out)
	}
}

// TestTheClearAdvisesCompactionOnAManagedDoltWorkStore is ga-3xxr0o's tail:
// removing the slice writes Dolt history the work store keeps, so a clear on a
// managed-Dolt work store ends with the advice to compact once AS OF recovery
// from before the clear is no longer wanted. It is advice, never automatic,
// and it is not given where it means nothing.
func TestTheClearAdvisesCompactionOnAManagedDoltWorkStore(t *testing.T) {
	for _, managed := range []bool{true, false} {
		t.Run(map[bool]string{true: "managed dolt", false: "other store"}[managed], func(t *testing.T) {
			stubInfraControllerPing(t, 0)
			source := stubInfraMigrationSource(t)
			mustCreateInfraBead(t, source, beads.Bead{Title: "wisp", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
			prevManaged := infraWorkStoreIsManagedDolt
			infraWorkStoreIsManagedDolt = func(string) bool { return managed }
			t.Cleanup(func() { infraWorkStoreIsManagedDolt = prevManaged })
			request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
			var log bytes.Buffer
			if report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log); report.Outcome != infraMigrationConverged {
				t.Fatalf("migrate = %s: %s", report.Outcome, log.String())
			}
			if got := strings.Contains(log.String(), "gc dolt compact"); got != managed {
				t.Fatalf("compaction advice given = %v, want %v: %s", got, managed, log.String())
			}
		})
	}
}
