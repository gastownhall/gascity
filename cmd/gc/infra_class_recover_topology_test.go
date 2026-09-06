package main

// The two halves of the repair nobody had tested: the topology it restores, and
// the residue it leaves when it fails partway.
//
// Both are properties of a PAIR of runs rather than of one, which is why they
// live together. An edge belongs to two beads, and the two can arrive in the
// binding on different runs; a copied row and the manifest entry that records it
// are written at opposite ends of the same run, and a fault between them is the
// state every later run has to recognize.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// bindingEdges returns the ids one bead depends on, read from the binding
// through the production opener.
func bindingEdges(t *testing.T, target infraBindingTarget, id string) []string {
	t.Helper()
	deps, err := openMigratedDestination(t, target).DepList(id, "down")
	if err != nil {
		t.Fatalf("listing binding deps of %s: %v", id, err)
	}
	ids := make([]string, 0, len(deps))
	for _, d := range deps {
		ids = append(ids, d.DependsOnID)
	}
	sort.Strings(ids)
	return ids
}

// TestStorageRecoverStrandedRestoresAnInboundEdgeIntoARecoveredBead is the
// direction the first cut of this repair could not see.
//
// An edge is a property of a pair, and the two endpoints of a within-infra edge
// need not arrive in the binding on the same run: the dispatcher's normal wiring
// direction is a PRE-EXISTING bead blocking on a JUST-CREATED one
// (internal/dispatch/fanout.go, internal/molecule/molecule.go,
// internal/dispatch/ralph.go all write old -> new), so a bead that crossed at
// cutover routinely gains an edge into one written after it. Reading only the
// recovered bead's outbound edges drops every one of those, and drops them
// silently — the binding then reports the blocked bead READY with its blocker
// missing, because infraMigrationRow strips IsBlocked so readiness is
// dependency-derived.
//
// Red-before, on the edge pass that iterated only the beads this run copied:
//
//	the inbound edge gc-1 -> gc-4 did not cross: the binding holds no edge from
//	the bead that crossed at cutover into the one this run recovered. binding
//	deps of gc-1 = []; the source still holds [gc-4]
//
// and the run reported `copied: 1 bead(s), 0 dep edge(s)`, `verified: 1 bead(s)
// re-read field-, class- and dep-equal`, `residual stranded: 0` and exit 0.
func TestStorageRecoverStrandedRestoresAnInboundEdgeIntoARecoveredBead(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	// gc-1 crossed at cutover. It is in the binding and in the manifest, so the
	// gap classification never looks at it again.
	crossed := infraStoreFingerprint(t, openMigratedDestination(t, target))
	if len(crossed) == 0 {
		t.Fatal("the fixture put nothing in the binding; there is no already-crossed endpoint to test")
	}
	held := crossed[0]

	recovered := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	if err := source.DepAdd(held, recovered.ID, "blocks"); err != nil {
		t.Fatalf("seeding the inbound edge: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery exited %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if got := bindingEdges(t, target, held); !slices.Contains(got, recovered.ID) {
		t.Errorf("the inbound edge %s -> %s did not cross: the binding holds no edge from the bead that crossed at cutover into the one this run recovered. binding deps of %s = %v; the source still holds it",
			held, recovered.ID, held, got)
	}
	if !strings.Contains(stdout.String(), "1 restored") {
		t.Errorf("the report does not say an edge was restored, so an operator cannot tell the topology crossed: %s", stdout.String())
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("the boot guard still reads %d stranded bead(s): %v", len(got), got)
	}
}

// TestStorageRecoverStrandedRepairsATruncatedTopologyOnARerun is the property
// the header advertises and the first cut did not have: a re-run resumes.
//
// A run that dies between the copy loop and the edge pass leaves rows in the
// binding with a truncated topology. Nothing about those rows is stranded any
// more — the binding holds them — so a repair that only ever drives edges for
// the beads it copied THIS run can never repair them, and the workflow the
// refusal text tells the operator to follow ("a re-run resumes") is a lie for
// exactly the edges it lost.
//
// The truncation is produced by removing the edge from the binding after a
// successful run, which is the same state as a run that never wrote it.
//
// Red-before, on the edge pass that iterated only the beads this run copied:
//
//	the re-run did not restore the truncated edge gc-5 -> gc-4: binding deps of
//	gc-5 = []; a re-run that cannot repair a partial topology is not the resume
//	the refusal promises
func TestStorageRecoverStrandedRepairsATruncatedTopologyOnARerun(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	follow := mustCreateInfraBead(t, source, beads.Bead{Title: "order finalize", Type: "task", Labels: []string{"order-tracking"}})
	if err := source.DepAdd(follow.ID, tracker.ID, "blocks"); err != nil {
		t.Fatalf("seeding the within-infra edge: %v", err)
	}

	var first bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &first, &first); code != 0 {
		t.Fatalf("first run exited %d, want 0; log: %s", code, first.String())
	}

	// The state an interrupted edge pass leaves: both rows resident, the edge
	// between them absent, and the manifest recording both so nothing is
	// stranded.
	binding := openMigratedDestination(t, target)
	if err := binding.DepRemove(follow.ID, tracker.ID); err != nil {
		t.Fatalf("truncating the topology: %v", err)
	}
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatalf("closing the binding after truncating it: %v", err)
	}
	if got := bindingEdges(t, target, follow.ID); len(got) != 0 {
		t.Fatalf("the fixture did not truncate the topology: binding deps of %s = %v", follow.ID, got)
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Fatalf("the fixture left %v stranded; the scenario under test is a city where only an EDGE is missing", got)
	}

	var second bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &second, &second); code != 0 {
		t.Fatalf("the re-run exited %d; log: %s", code, second.String())
	}
	if got := bindingEdges(t, target, follow.ID); !slices.Contains(got, tracker.ID) {
		t.Errorf("the re-run did not restore the truncated edge %s -> %s: binding deps of %s = %v; a re-run that cannot repair a partial topology is not the resume the refusal promises",
			follow.ID, tracker.ID, follow.ID, got)
	}
	if !strings.Contains(second.String(), "1 restored") {
		t.Errorf("the re-run did not report restoring the edge: %s", second.String())
	}

	// And the third run is the no-op the runbook depends on: nothing left to
	// restore, and the report says so rather than re-driving every edge blind.
	var third bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &third, &third); code != 0 {
		t.Fatalf("the third run exited %d; log: %s", code, third.String())
	}
	if !strings.Contains(third.String(), "nothing to recover") {
		t.Errorf("the third run did work on a converged city: %s", third.String())
	}
}

// TestStorageRecoverStrandedNamesAWithinInfraEdgeItCannotRestore covers the
// edge whose far endpoint this run REFUSED to move.
//
// The first cut counted it into "cross-boundary edge(s) left as metadata
// linkage", which is wrong twice: it is not cross-boundary, and infraMigrationRow
// nils both Dependencies and Needs so no linkage is retained anywhere. An
// operator reading that line has no way to know a within-infra edge was dropped.
//
// The second half is the workflow the PR advertises: resolve the ambiguity, run
// again, and the edge that could not be written the first time is written now.
//
// Red-before, on the first cut:
//
//	the report calls a within-infra edge cross-boundary: "copied: 1 bead(s), 0
//	dep edge(s) (1 cross-boundary edge(s) left as metadata linkage)"
//	the report does not name the endpoints of the edge it dropped
//
// and, after the operator resolves the refusal and re-runs:
//
//	the re-run did not restore gc-5 -> gc-4 after the refusal was resolved:
//	binding deps of gc-5 = []
func TestStorageRecoverStrandedNamesAWithinInfraEdgeItCannotRestore(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	follow := mustCreateInfraBead(t, source, beads.Bead{Title: "a session that gates on it", Type: "session", Labels: []string{"gc:session"}})
	if err := source.DepAdd(follow.ID, tracker.ID, "blocks"); err != nil {
		t.Fatalf("seeding the within-infra edge: %v", err)
	}

	// The orders class temporarily outside the relocated set, which is how the
	// existing suite reaches a refusal without waiting for a seventh class.
	previous := infraMigrationClasses
	infraMigrationClasses = []config.StorageClass{
		config.StorageClassGraph,
		config.StorageClassSessions,
		config.StorageClassMessaging,
		config.StorageClassNudges,
	}

	var stdout, stderr bytes.Buffer
	code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr)
	infraMigrationClasses = previous
	if code == 0 {
		t.Fatalf("the run exited 0 while refusing to move %s; log: %s", tracker.ID, stdout.String())
	}
	report := stdout.String()
	if strings.Contains(report, "1 cross-boundary edge") {
		t.Errorf("the report calls a within-infra edge cross-boundary: %s", report)
	}
	if !strings.Contains(report, follow.ID+" -> "+tracker.ID) {
		t.Errorf("the report does not name the endpoints of the edge it dropped: %s", report)
	}

	// The advertised workflow: resolve the refusal, run again, and the edge the
	// first run could not write is written now.
	var second bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &second, &second); code != 0 {
		t.Fatalf("the re-run exited %d after the refusal was resolved; log: %s", code, second.String())
	}
	if got := bindingEdges(t, target, follow.ID); !slices.Contains(got, tracker.ID) {
		t.Errorf("the re-run did not restore %s -> %s after the refusal was resolved: binding deps of %s = %v",
			follow.ID, tracker.ID, follow.ID, got)
	}
}

// TestStorageRecoverStrandedLeavesCrossBoundaryEdgesAlone keeps the fix from
// eating the one edge class that is SUPPOSED to be left behind. An edge into a
// work bead has no far endpoint the binding owns, exactly as the migration's own
// import leaves it, and it must still be reported as such rather than as a
// dropped within-infra edge.
func TestStorageRecoverStrandedLeavesCrossBoundaryEdgesAlone(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "the work the order tracks", Type: "task"})
	if err := source.DepAdd(tracker.ID, work.ID, "blocks"); err != nil {
		t.Fatalf("seeding the cross-boundary edge: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery exited %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 cross-boundary edge") {
		t.Errorf("the report does not account for the work-boundary edge: %s", stdout.String())
	}
	if got := bindingEdges(t, target, tracker.ID); slices.Contains(got, work.ID) {
		t.Errorf("the cross-boundary edge %s -> %s was written into a binding that owns no row for its far endpoint: %v", tracker.ID, work.ID, got)
	}
}

// TestStorageRecoverStrandedRecordsAResidueAnInterruptedRunLeft is the partial
// run, reproduced through the atomic publish the manifest is written with.
//
// The failure is ordinary: ENOSPC, EROFS, or a kill between the copy and the
// rename. What made it dangerous was silence. The next run found those rows in
// the binding, filtered them out of the gap because the binding holds them,
// printed "nothing to recover" and exited 0 — and so did `gc storage status`.
// The manifest never recorded them, so when the binding's own GC later collected
// one, the boot guard re-classified it as a strand and the city refused to boot
// over a row the repair had correctly delivered.
//
// Red-before, on the gap loop that dropped the in-binding/unrecorded set:
//
//	the second run reported "nothing to recover: the binding already holds every
//	infrastructure bead the retained work store has that the manifest does not
//	record." and exited 0, leaving the manifest at [gc-1 gc-2] while the binding
//	holds gc-4
func TestStorageRecoverStrandedRecordsAResidueAnInterruptedRunLeft(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	before := manifestIDs(t, target)
	stranded := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	// Run one: the copy lands, the manifest publish does not.
	failingInfraRename(t, infraCopyManifestName)
	var first bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &first, &first); code == 0 {
		t.Fatalf("the interrupted run reported success: %s", first.String())
	}
	infraMigrationRename = os.Rename
	if _, err := openMigratedDestination(t, target).Get(stranded.ID); err != nil {
		t.Fatalf("the fixture did not produce the residue under test (%s is not in the binding): %v", stranded.ID, err)
	}
	if got := manifestIDs(t, target); !slices.Equal(got, before) {
		t.Fatalf("the interrupted run extended the manifest after all: %v", got)
	}

	// Run two: the resume the failure message tells the operator to perform.
	var second bytes.Buffer
	code := runStrandedRecovery(t, cityPath, cfg, &second, &second)
	if strings.Contains(second.String(), "nothing to recover") {
		t.Errorf("the second run reported a clean city over a row the binding holds and the manifest does not record: %s", second.String())
	}
	if code != 0 {
		t.Fatalf("the second run exited %d; log: %s", code, second.String())
	}
	if got := manifestIDs(t, target); !slices.Contains(got, stranded.ID) {
		t.Fatalf("the manifest still does not record %s, so the binding's own GC of that row would read back as a strand: %v", stranded.ID, got)
	}
	for _, id := range before {
		if !slices.Contains(manifestIDs(t, target), id) {
			t.Errorf("the extended manifest dropped %s, which the previous one recorded", id)
		}
	}

	// The consequence, closed: the binding legitimately deletes the row and the
	// boot guard reads it as collected rather than as a strand.
	binding := openMigratedDestination(t, target)
	if err := binding.Delete(stranded.ID); err != nil {
		t.Fatalf("deleting the recovered row the way the binding's own GC would: %v", err)
	}
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatalf("closing the binding: %v", err)
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("the boot guard calls %v stranded after the binding's own GC collected a row the repair had delivered; the city refuses to boot over a row nothing did wrong to", got)
	}
}

// TestStorageRecoverStrandedRecordsAResidueLeftByAnUnwritableBindingRoot is the
// same residue with NO seam stubbed at all.
//
// A read-only or full binding root fails the manifest BACKUP — a step that runs
// after the copy and after the equality stage, so the run has already delivered
// every row it was asked to deliver when it aborts. This is the reachability
// argument for the test above: the residue needs no crash, no signal and no
// injected fault, only a directory this process cannot write to during an
// incident.
//
// Red-before, on the gap loop that dropped the in-binding/unrecorded set: the
// same "nothing to recover" / exit 0 as above, from an ordinary permission
// fault.
func TestStorageRecoverStrandedRecordsAResidueLeftByAnUnwritableBindingRoot(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	before := manifestIDs(t, target)
	stranded := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	// The database lives in a component directory BELOW the binding root, so a
	// read-only root leaves the copy writable and takes only the manifest, its
	// backup and its temp file — which is precisely why the run gets as far as
	// "copied / verified" before it fails.
	if err := os.Chmod(target.Root, 0o555); err != nil {
		t.Fatalf("sealing the binding root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(target.Root, 0o755) })

	var first bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &first, &first); code == 0 {
		t.Fatalf("the run reported success with an unwritable binding root: %s", first.String())
	}
	if !strings.Contains(first.String(), "copied: 1 bead(s)") {
		t.Fatalf("the run did not get as far as the copy, so it is not the scenario under test: %s", first.String())
	}
	if err := os.Chmod(target.Root, 0o755); err != nil {
		t.Fatalf("unsealing the binding root: %v", err)
	}
	if _, err := openMigratedDestination(t, target).Get(stranded.ID); err != nil {
		t.Fatalf("the fixture did not produce the residue under test (%s is not in the binding): %v", stranded.ID, err)
	}

	var second bytes.Buffer
	code := runStrandedRecovery(t, cityPath, cfg, &second, &second)
	if strings.Contains(second.String(), "nothing to recover") {
		t.Errorf("the second run reported a clean city over a row the binding holds and the manifest does not record: %s", second.String())
	}
	if code != 0 {
		t.Fatalf("the second run exited %d; log: %s", code, second.String())
	}
	if got := manifestIDs(t, target); !slices.Contains(got, stranded.ID) {
		t.Errorf("the manifest still does not record %s: %v", stranded.ID, got)
	}
	for _, id := range before {
		if !slices.Contains(manifestIDs(t, target), id) {
			t.Errorf("the extended manifest dropped %s, which the previous one recorded", id)
		}
	}
}

// TestStorageRecoverStrandedRefusesToRecordAResidueItCannotProve is the other
// side of the residue fold: a row the binding holds under an id the source also
// holds is recorded ONLY if it is provably the source's row.
//
// The manifest's meaning is "the copy was proven to deliver this id". Folding in
// an id whose binding row is a different bead would record a proof nobody made,
// and every later boot would classify that id's absence against it.
//
// Red-before, if the fold recorded every unrecorded in-binding id on sight:
//
//	the manifest records gc-4, whose binding row is not the source's row
func TestStorageRecoverStrandedRefusesToRecordAResidueItCannotProve(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	stranded := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	// A row under the same id that is NOT the source's row: what an id
	// collision, or a hand-repair, leaves behind.
	binding := openMigratedDestination(t, target)
	creator, ok := binding.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("the binding cannot preserve ids: %T", binding)
	}
	divergent := infraMigrationRow(stranded)
	divergent.Title = "a different bead that happens to share the id"
	if _, err := creator.CreateWithForeignID(divergent); err != nil {
		t.Fatalf("seeding the divergent row: %v", err)
	}
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatalf("closing the binding: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr)
	if code == 0 {
		t.Errorf("the repair reported success over a binding row it could not prove is the source's: %s", stdout.String())
	}
	if slices.Contains(manifestIDs(t, target), stranded.ID) {
		t.Errorf("the manifest records %s, whose binding row is not the source's row", stranded.ID)
	}
	if !strings.Contains(stdout.String()+stderr.String(), stranded.ID) {
		t.Errorf("nothing names the row that could not be recorded: %s %s", stdout.String(), stderr.String())
	}
}

// transientDepListSource fails DepList a bounded number of times and then
// answers normally, which is what a work store under load, a locked SQLite file
// or a bd subprocess that lost a connection looks like. The inline projection is
// supplied the way bd's list JSON supplies it, so the fallback is available and
// the only question is whether the reader takes it.
type transientDepListSource struct {
	beads.Store
	remaining *int
	err       error
	inline    map[string][]beads.Dep
}

func (s transientDepListSource) DepList(id, direction string) ([]beads.Dep, error) {
	if *s.remaining > 0 {
		*s.remaining--
		return nil, s.err
	}
	return s.Store.DepList(id, direction)
}

// DepMetadata forwards to the leaf, for the reason every migration double needs
// to: embedding beads.Store strips the read, and a source the repair cannot ask
// about edge payloads is refused rather than assumed empty.
func (s transientDepListSource) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	return depMetadataThrough(s.Store, issueID, dependsOnID)
}

func (s transientDepListSource) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if deps, ok := s.inline[rows[i].ID]; ok {
			rows[i].Dependencies = deps
		}
	}
	return rows, nil
}

// TestStorageRecoverStrandedKeepsTheRelationReadOverATransientError pins the
// one thing the capability fallback may conclude from an error.
//
// The fallback exists for an adapter that CANNOT list relations — the
// bd/Postgres backend answers `operation "IssueRelations" not supported by the
// postgres backend`, and that is a fact about the adapter. A locked database, a
// lost connection or a subprocess that failed to fork is a fact about a moment.
// Reading the second as the first swaps the relation read for a projection this
// reader has not witnessed, and does it for the whole run.
//
// Red-before, on the reader that latched on ANY probe error:
//
//	the run downgraded the whole dependency read over a transient error: "source
//	dep edges: the store refuses relation listing (database is locked), so edges
//	are read from the inline per-bead projection, witnessed live on gc-1"
//	the real edge gc-5 -> gc-4 was dropped by the downgrade: binding deps of gc-5 = []
func TestStorageRecoverStrandedKeepsTheRelationReadOverATransientError(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	follow := mustCreateInfraBead(t, source, beads.Bead{Title: "order finalize", Type: "task", Labels: []string{"order-tracking"}})
	if err := source.DepAdd(follow.ID, tracker.ID, "blocks"); err != nil {
		t.Fatalf("seeding the within-infra edge: %v", err)
	}

	// One failure, consumed by the reader's capability probe, plus a live inline
	// projection on an unrelated row so the corpus witness would license the
	// downgrade if the reader took it.
	budget := 1
	swapRecoverySource(t, transientDepListSource{
		Store:     source,
		remaining: &budget,
		err:       fmt.Errorf("database is locked"),
		inline:    map[string][]beads.Dep{"gc-1": {{IssueID: "gc-1", DependsOnID: "gc-2", Type: "blocks"}}},
	})

	var stdout, stderr bytes.Buffer
	code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr)
	if strings.Contains(stdout.String(), "refuses relation listing") {
		t.Errorf("the run downgraded the whole dependency read over a transient error: %s", stdout.String())
	}
	if code != 0 {
		t.Fatalf("recovery exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if got := bindingEdges(t, target, follow.ID); !slices.Contains(got, tracker.ID) {
		t.Errorf("the real edge %s -> %s was dropped by the downgrade: binding deps of %s = %v", follow.ID, tracker.ID, follow.ID, got)
	}
}

// TestStorageRecoverStrandedRefusesABeadWhoseEdgesOneReadCannotState is the
// per-bead half: an error that is not the capability refusal makes THAT bead
// ambiguous rather than making the whole run read a weaker source.
func TestStorageRecoverStrandedRefusesABeadWhoseEdgesOneReadCannotState(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	// Enough failures that the bead's own read fails too, with no capability
	// refusal anywhere: the reader must refuse the bead rather than move it
	// edge-free.
	budget := 1000
	swapRecoverySource(t, transientDepListSource{
		Store:     source,
		remaining: &budget,
		err:       fmt.Errorf("database is locked"),
		inline:    map[string][]beads.Dep{"gc-1": {{IssueID: "gc-1", DependsOnID: "gc-2", Type: "blocks"}}},
	})

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code == 0 {
		t.Fatalf("the repair moved %s over a source that could not state its topology: %s", tracker.ID, stdout.String())
	}
	if !strings.Contains(stdout.String(), "database is locked") {
		t.Errorf("the refusal does not carry the error the source really answered with: %s", stdout.String())
	}
	if _, err := openMigratedDestination(t, target).Get(tracker.ID); err == nil {
		t.Errorf("bead %s was moved despite an unstatable topology", tracker.ID)
	}
}

// divergentSecondReadSource answers a bead's edges differently the second time
// it is asked, which is what a read that lost edges the first time looks like
// from the outside.
type divergentSecondReadSource struct {
	beads.Store
	id    string
	seen  map[string]int
	later []beads.Dep
}

func (s divergentSecondReadSource) DepList(id, direction string) ([]beads.Dep, error) {
	if id == s.id && direction == "down" {
		s.seen[id]++
		if s.seen[id] == 1 {
			return nil, nil
		}
		return s.later, nil
	}
	return s.Store.DepList(id, direction)
}

// payloadAppearingOnSecondReadSource reports an edge as carrying nothing the
// first time it is asked and carrying a payload every time after, which is what
// a payload the write silently failed to pick up looks like from the outside.
//
// It is the payload-side twin of divergentSecondReadSource, and it exists
// because the recovery's write and its equality stage are the only two callers
// that ask: the write takes the first answer and adds a bare edge, the verify
// takes the second and must refuse the binding it just wrote.
type payloadAppearingOnSecondReadSource struct {
	beads.Store
	pair    [2]string
	payload string
	seen    map[[2]string]int
}

func (s payloadAppearingOnSecondReadSource) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	key := [2]string{issueID, dependsOnID}
	if key != s.pair {
		return depMetadataThrough(s.Store, issueID, dependsOnID)
	}
	s.seen[key]++
	if s.seen[key] == 1 {
		return "", false, nil
	}
	return s.payload, true, nil
}

// TestStorageRecoverStrandedVerifiesRestoredEdgePayloads is the payload half of
// the equality stage, and the only thing that proves the stage asks at all.
//
// The presence loop cannot see this failure: a repair that wrote every edge in
// plan.missing and dropped every gate produces exactly the Dep set that loop
// demands, so the run reported "edges: N restored", extended the manifest, and
// exited 0 over a binding short of the gates it was being repaired toward — the
// same blindness the migration's own equality stage carried until it learned to
// witness payloads.
//
// Red-before, with the payload comparison removed from the stage:
//
//	the run reported success while the source's own second read names a payload
//	the binding does not carry
func TestStorageRecoverStrandedVerifiesRestoredEdgePayloads(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	gated := mustCreateInfraBead(t, source, beads.Bead{Title: "order finalize", Type: "task", Labels: []string{"order-tracking"}})
	gate := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	if err := source.DepAdd(gated.ID, gate.ID, "blocks"); err != nil {
		t.Fatalf("seeding the stranded edge: %v", err)
	}
	before := manifestIDs(t, target)

	swapRecoverySource(t, payloadAppearingOnSecondReadSource{
		Store:   source,
		pair:    [2]string{gated.ID, gate.ID},
		payload: `{"gate":"waits_for","threshold":3}`,
		seen:    map[[2]string]int{},
	})

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code == 0 {
		t.Fatalf("the run reported success while the source's own second read names a payload the binding does not carry: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "dep "+gated.ID+" -> "+gate.ID) {
		t.Errorf("the refusal does not name the edge whose payload went missing: %q", stderr.String())
	}
	if got := manifestIDs(t, target); !slices.Equal(got, before) {
		t.Errorf("the manifest was extended over an edge restored without its payload: %v -> %v", before, got)
	}
}

// TestStorageRecoverStrandedVerifiesEdgesAgainstAFreshSourceRead is why the
// equality stage re-reads.
//
// The first cut compared the destination against the same in-memory edge list it
// had written from, so the dep half of "verified: N bead(s) re-read field-,
// class- and dep-equal" was self-referential: whatever the source read lost, the
// proof lost too. verifyInfraCopy on the migration path re-reads source.DepList
// independently, and this stage now holds itself to the same standard.
//
// Red-before, on the stage that compared against the cached read:
//
//	the run reported "verified: 1 bead(s) re-read field-, class- and dep-equal"
//	and exited 0 while the source's own second read names an edge the binding
//	does not hold
func TestStorageRecoverStrandedVerifiesEdgesAgainstAFreshSourceRead(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	follow := mustCreateInfraBead(t, source, beads.Bead{Title: "order finalize", Type: "task", Labels: []string{"order-tracking"}})
	before := manifestIDs(t, target)

	swapRecoverySource(t, divergentSecondReadSource{
		Store: source,
		id:    follow.ID,
		seen:  map[string]int{},
		later: []beads.Dep{{IssueID: follow.ID, DependsOnID: tracker.ID, Type: "blocks"}},
	})

	var stdout, stderr bytes.Buffer
	code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("the run reported success while the source's own second read names an edge the binding does not hold: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "dep "+follow.ID+" -> "+tracker.ID) {
		t.Errorf("the refusal does not name the edge the second read found: %q", stderr.String())
	}
	if got := manifestIDs(t, target); !slices.Equal(got, before) {
		t.Errorf("the manifest was extended over an unproven topology: %v -> %v", before, got)
	}
}

// TestStorageRecoverStrandedReadsTheManifestUnderTheGuard closes the
// read-modify-write that straddled the lock.
//
// The manifest was read at the top of the command and rewritten hundreds of
// lines later under the guard, with a controller-socket dial (500ms/500ms/2s)
// sitting in between. A second run of this same verb that took the guard,
// published its ids and released inside that window had its ids silently
// dropped by the first run's stale replacement — and manifestIDsDropped could
// not fire, because it diffed the replacement against the same stale map.
//
// The controller probe is the seam the window lives behind, so a write injected
// there is exactly a concurrent run that finished while this one was dialing.
//
// Red-before, with the read above the guard:
//
//	the extended manifest dropped gc-concurrently-recovered-1, which the on-disk
//	manifest recorded before this run took the guard; manifest = [gc-1 gc-2 gc-4]
func TestStorageRecoverStrandedReadsTheManifestUnderTheGuard(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	const concurrent = "gc-concurrently-recovered-1"
	prev := infraMigrationControllerPing
	infraMigrationControllerPing = func(string) int {
		// A concurrent run of this verb that acquired the guard, published its
		// ids and released while this one was still dialing the socket.
		if err := writeInfraCopyManifest(target, append(manifestIDs(t, target), concurrent)); err != nil {
			t.Errorf("publishing the concurrent manifest: %v", err)
		}
		return 0
	}
	t.Cleanup(func() { infraMigrationControllerPing = prev })

	var stdout, stderr bytes.Buffer
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
	if code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{}, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if got := manifestIDs(t, target); !slices.Contains(got, concurrent) {
		t.Errorf("the extended manifest dropped %s, which the on-disk manifest recorded before this run took the guard; manifest = %v", concurrent, got)
	}
}

// TestStorageRecoverStrandedDumpRefusesToTruncate covers the one write in this
// file that walked past everything else's care.
//
// --dump took an operator path with os.Create, which truncates unconditionally,
// and it wrote before the dry-run branch and long before any backup exists. The
// three tab-completions available in a binding root are the manifest, the marker
// and the database directory, so every completion in the directory an operator
// is investigating is a fatal target — and `--dry-run --dump <manifest>` needs
// no attestation, prints "nothing was written", and exits 0.
//
// Red-before, on os.Create:
//
//	the dry run truncated the proven-copy manifest it was pointed at: "[]\n" —
//	stranded-write detection is off for this city and the only documented remedy
//	is a re-converge
func TestStorageRecoverStrandedDumpRefusesToTruncate(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	manifestBefore, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
	}

	run := func(dump string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		stubInfraControllerPing(t, 0)
		request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg}
		code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{DryRun: true, DumpPath: dump}, &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}

	// 1. The manifest, which is the worst reachable target: it lives in the
	//    directory the operator is investigating and its loss turns
	//    stranded-write detection off for the life of the city.
	_, stderr, code := run(target.ManifestPath())
	if code == 0 {
		t.Error("the dry run accepted a dump path inside the binding root")
	}
	if !strings.Contains(stderr, "binding root") {
		t.Errorf("the refusal does not say why the path is refused: %q", stderr)
	}
	after, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		t.Fatalf("reading the manifest after the refused dump: %v", err)
	}
	if !bytes.Equal(manifestBefore, after) {
		t.Errorf("the dry run truncated the proven-copy manifest it was pointed at: %q", string(after))
	}

	// 2. Any existing file: a dump is a new artifact, so an existing path is a
	//    typo rather than an instruction.
	existing := filepath.Join(t.TempDir(), "notes.json")
	if err := os.WriteFile(existing, []byte("an operator's notes\n"), 0o600); err != nil {
		t.Fatalf("seeding the existing file: %v", err)
	}
	_, stderr, code = run(existing)
	if code == 0 {
		t.Error("the dry run accepted a dump path that already exists")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("the refusal does not say the path exists: %q", stderr)
	}
	if contents, readErr := os.ReadFile(existing); readErr != nil || string(contents) != "an operator's notes\n" {
		t.Errorf("the refused dump truncated the file it named: %q (%v)", string(contents), readErr)
	}

	// 3. And the ordinary path still works, so the refusals did not cost the
	//    feature.
	fresh := filepath.Join(t.TempDir(), "stranded.json")
	stdout, stderr, code := run(fresh)
	if code != 0 {
		t.Fatalf("the dry run exited %d with a fresh dump path; stdout: %s stderr: %s", code, stdout, stderr)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the dump was not written to the fresh path: %v", err)
	}
}

// TestStorageRecoverStrandedOnABornSplitCityDefersToTheBootGuard stops two
// commands in one binary contradicting each other about the same bead.
//
// A born-split city's binding is served by a provider this build carries no
// migration discipline for, so the repair genuinely cannot run there. It said so
// by asserting a fact about the city's DATA — "nothing here is stranded" — which
// is false for exactly the city that is alarming: the boot guard names the ids
// and `gc storage status` prints "born-split: BLOCKED". An operator at 3am
// reading "nothing here is stranded" is one step from the born-split advice's
// own second clause, "then delete them from the work store".
//
// Red-before, on the refusal shared with the single-store city:
//
//	the refusal claims nothing is stranded on a city whose boot guard names gc-1:
//	"gc storage recover-stranded: this city's [storage.classes] do not assign
//	graph/sessions/messaging/orders/nudges to one shared non-work binding this
//	build can serve, so there is no binding to recover into and nothing here is
//	stranded. ..."
func TestStorageRecoverStrandedOnABornSplitCityDefersToTheBootGuard(t *testing.T) {
	cityPath := recoveryCityPath(t)
	source := stubStrandedRecoverySource(t)
	strayed := mustCreateInfraBead(t, source, beads.Bead{Title: "landed in work", Type: "session", Labels: []string{"gc:session"}})
	cfg := workspaceSplitConfig("infra")

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code == 0 {
		t.Fatal("the repair claimed success on a binding whose provider it cannot serve")
	}
	said := stdout.String() + stderr.String()
	if strings.Contains(said, "nothing here is stranded") {
		t.Errorf("the refusal claims nothing is stranded on a city whose boot guard names %s: %q", strayed.ID, said)
	}
	if !strings.Contains(said, strayed.ID) {
		t.Errorf("the refusal does not name the bead the boot guard names: %q", said)
	}
	if !strings.Contains(said, "born-split") {
		t.Errorf("the refusal does not name the discipline the city is serving under: %q", said)
	}

	// The two authorities must agree about the same bead.
	var statusOut, statusErr bytes.Buffer
	stubInfraControllerPing(t, 0)
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &statusOut, &statusErr); code == 0 {
		t.Fatalf("status exited 0 on a born-split city holding %s: %s", strayed.ID, statusOut.String())
	}
	if !strings.Contains(statusOut.String(), strayed.ID) {
		t.Fatalf("the fixture is not the contradicting one: status does not name %s: %s", strayed.ID, statusOut.String())
	}

	// And the work store is untouched by a verb that refuses.
	if got := infraStoreFingerprint(t, source); !slices.Equal(got, []string{strayed.ID}) {
		t.Errorf("the refused repair changed the work store: %v", got)
	}
}
