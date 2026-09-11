package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// stubStrandedRecoverySource points the repair at the split kit's strict work
// leaf rather than a bare MemStore.
//
// The strictness is load-bearing for this suite specifically. The repair's job
// is to move rows ACROSS a store boundary, and a MemStore source accepts a
// foreign-prefix create and a dangling dep edge without a word — so a fixture
// built on one could seed a "work store" holding rows no work store could hold,
// and the crossing under test would be a crossing of nothing. The kit's
// BdSemantics leaf answers both the way the bd backend a work store really runs
// on answers them. The DESTINATION is the production opener throughout, which
// means a real beads.SQLiteStore under the graph class's reserved prefix — the
// store internal/storebinding/sqlite's OpenEngine opens.
func stubStrandedRecoverySource(t *testing.T) beads.Store {
	t.Helper()
	source := splittest.NewWorkStore(t, "gc")
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	return source
}

// convergedRecoveryCity builds a city that has really cut over: pre-cutover
// infrastructure state in the work store, the operator migration run over it,
// and therefore a marker and a non-empty proven-copy manifest.
//
// Non-empty matters. A manifest with nothing in it makes the superset property
// vacuous and makes "removed since cutover" unreachable, so a repair that
// replaced the manifest instead of extending it would pass.
func convergedRecoveryCity(t *testing.T) (cityPath string, cfg *config.City, source beads.Store, target infraBindingTarget) {
	t.Helper()
	cityPath = recoveryCityPath(t)
	source = stubStrandedRecoverySource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "live session", Type: "session", Labels: []string{"gc:session"}})
	mustCreateInfraBead(t, source, beads.Bead{Title: "delivered mail", Type: "message"})
	mustCreateInfraBead(t, source, beads.Bead{Title: "ordinary backlog work", Type: "task"})

	cfg = infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	return cityPath, cfg, source, mustResolveInfraTarget(t, cityPath, cfg)
}

// recoveryCityPath returns a canonical city root with the .gc directory the
// migration guard locks.
func recoveryCityPath(t *testing.T) string {
	t.Helper()
	cityPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalizing the city path: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("creating the city .gc directory: %v", err)
	}
	return cityPath
}

// runStrandedRecovery is the test-side spelling of
// `gc storage recover-stranded --from-work --fleet-stopped`, with the
// controller probe answering "nothing is serving this city".
func runStrandedRecovery(t *testing.T, cityPath string, cfg *config.City, stdout, stderr io.Writer) int {
	t.Helper()
	stubInfraControllerPing(t, 0)
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
	return doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{}, stdout, stderr)
}

// strandedIDs returns the ids the containment check currently calls stranded.
func strandedIDs(t *testing.T, cityPath string, target infraBindingTarget) []string {
	t.Helper()
	proven, recorded, err := readInfraCopyManifest(target)
	if err != nil {
		t.Fatalf("readInfraCopyManifest: %v", err)
	}
	if !recorded {
		t.Fatal("the city has no proven-copy manifest; the containment check is off and this assertion means nothing")
	}
	gap, err := classifyInfraContainmentGap(cityPath, target, proven)
	if err != nil {
		t.Fatalf("classifyInfraContainmentGap: %v", err)
	}
	return gap.Stranded
}

// manifestIDs returns the sorted contents of the proven-copy manifest.
func manifestIDs(t *testing.T, target infraBindingTarget) []string {
	t.Helper()
	proven, recorded, err := readInfraCopyManifest(target)
	if err != nil {
		t.Fatalf("readInfraCopyManifest: %v", err)
	}
	if !recorded {
		t.Fatal("no proven-copy manifest")
	}
	ids := make([]string, 0, len(proven))
	for id := range proven {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// TestStorageRecoverStrandedCopiesTheGapAndLeavesTheSourceIntact is the whole
// repair on the shape it was built for: a converged, serving city that has
// since acquired infrastructure writes in its retained work store.
//
// It asserts the four properties that make the repair safe rather than merely
// effective — the binding gains exactly the gap, the source is byte-for-byte
// what it was, the manifest grows rather than being replaced, and the boot
// guard's own containment check reads zero afterwards.
//
// Red-before, with the copy loop skipped (the equality stage catches it before
// the manifest is touched, which is the fail-closed order):
//
//	recovery exited 1, want 0; ... stderr: gc storage recover-stranded: bead gc-4
//	missing from the reopened binding: getting bead "gc-4": bead not found. The
//	manifest was NOT extended, so this bead is still named as stranded
//
// Red-before, if the repair deleted the rows it moved out of the source:
//
//	the work store changed during the repair: before [gc-1 gc-2 gc-3 gc-4 gc-5 gc-6],
//	after [gc-1 gc-2 gc-3 gc-6]; the source is retained verbatim so a bad outcome is
//	a duplicate rather than a loss
func TestStorageRecoverStrandedCopiesTheGapAndLeavesTheSourceIntact(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	provenBefore := manifestIDs(t, target)

	// The writes that lost the race: they landed in the retained source after
	// the equality stage, so the marker blessed a binding that does not hold
	// them. One carries a dep on the other (a within-infra edge the repair must
	// carry) and a dep on a work bead (a cross-boundary edge it must not).
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	follow := mustCreateInfraBead(t, source, beads.Bead{Title: "order finalize", Type: "task", Labels: []string{"order-tracking"}})
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "the work the order tracks", Type: "task"})
	if err := source.DepAdd(follow.ID, tracker.ID, "blocks"); err != nil {
		t.Fatalf("seeding the within-infra edge: %v", err)
	}
	if err := source.DepAdd(follow.ID, work.ID, "blocks"); err != nil {
		t.Fatalf("seeding the cross-boundary edge: %v", err)
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 2 {
		t.Fatalf("the fixture produced %d stranded bead(s) (%v), want 2; the scenario under test was never created", len(got), got)
	}
	sourceBefore := infraStoreFingerprint(t, source)

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery exited %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	binding := openMigratedDestination(t, target)
	for _, want := range []beads.Bead{tracker, follow} {
		got, err := binding.Get(want.ID)
		if err != nil {
			t.Fatalf("the binding still cannot read %s; recovery reported success and moved nothing: %v", want.ID, err)
		}
		if diff := beadCopyDifference(want, got); diff != "" {
			t.Errorf("bead %s crossed with a changed field: %s", want.ID, diff)
		}
		if diff := infraCopyClassDifference(want, got); diff != "" {
			t.Errorf("bead %s %s", want.ID, diff)
		}
	}
	if _, err := binding.Get(work.ID); err == nil {
		t.Errorf("the work bead %s crossed into the infrastructure binding; work never crosses", work.ID)
	}

	deps, err := binding.DepList(follow.ID, "down")
	if err != nil {
		t.Fatalf("listing copied deps: %v", err)
	}
	var carried []string
	for _, d := range deps {
		carried = append(carried, d.DependsOnID)
	}
	if !slices.Contains(carried, tracker.ID) {
		t.Errorf("the within-infra edge %s -> %s did not cross; carried = %v", follow.ID, tracker.ID, carried)
	}
	if slices.Contains(carried, work.ID) {
		t.Errorf("the cross-boundary edge %s -> %s was written into the binding, which owns no row for its far endpoint; carried = %v", follow.ID, work.ID, carried)
	}

	if after := infraStoreFingerprint(t, source); !slices.Equal(sourceBefore, after) {
		t.Errorf("the work store changed during the repair: before %v, after %v; the source is retained verbatim so a bad outcome is a duplicate rather than a loss", sourceBefore, after)
	}
	provenAfter := manifestIDs(t, target)
	for _, id := range provenBefore {
		if !slices.Contains(provenAfter, id) {
			t.Errorf("the extended manifest dropped %s, which the previous one recorded", id)
		}
	}
	for _, id := range []string{tracker.ID, follow.ID} {
		if !slices.Contains(provenAfter, id) {
			t.Errorf("the manifest does not record %s, so the next boot still calls it stranded", id)
		}
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("the boot guard still reads %d stranded bead(s) after a successful repair: %v", len(got), got)
	}
}

// TestStorageRecoverStrandedIsANoOpOnASecondRun pins idempotence, which is what
// makes the command safe to put in a runbook: an operator who cannot tell
// whether it already ran can simply run it again.
//
// Red-before, if the manifest were not extended (so the same ids stay stranded
// forever and every run re-copies them):
//
//	the second run exited 1 and reported 2 stranded bead(s); a repair that cannot
//	converge is a repair an operator cannot finish
func TestStorageRecoverStrandedIsANoOpOnASecondRun(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	mustCreateInfraBead(t, source, beads.Bead{Title: "queued nudge", Type: "task", Labels: []string{"gc:nudge"}})

	var first bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &first, &first); code != 0 {
		t.Fatalf("first run exited %d, want 0; log: %s", code, first.String())
	}
	firstManifest := manifestIDs(t, target)

	var second bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &second, &second); code != 0 {
		t.Fatalf("the second run exited %d and reported %s; a repair that cannot converge is a repair an operator cannot finish", code, second.String())
	}
	if !strings.Contains(second.String(), "nothing to recover") {
		t.Errorf("the second run did work instead of reporting a converged city: %s", second.String())
	}
	if got := manifestIDs(t, target); !slices.Equal(firstManifest, got) {
		t.Errorf("the second run changed the manifest: %v -> %v", firstManifest, got)
	}
}

// TestStorageRecoverStrandedExtendsTheManifestAsASuperset is the check that
// bites, and it bites on a city whose binding has legitimately LOST rows.
//
// A converged city's binding hard-deletes expired closed wisps and read mail;
// the retained source keeps those rows forever. The manifest is the only record
// that tells those two apart from a write the copy never carried, so an id it
// has ever recorded has to stay recorded. A repair that wrote its own moved set
// as the manifest would erase that history and turn every garbage-collected row
// into a stranded write on the next boot — the city would refuse to start over
// rows nothing did wrong to.
//
// The fixture is the migration suite's own real-GC one (migratedThenCollectedCity
// runs memoryWispGC over the binding rather than simulating a delete), so the
// divergence this asserts against is the one a healthy city really produces.
//
// Red-before, with `extended` built from the moved beads alone — the guard
// refuses the write and the previous manifest stands:
//
//	recovery exited 1; ... stderr: gc storage recover-stranded: the extended
//	manifest would drop 4 id(s) the previous one recorded (gc-1, gc-2, gc-3, gc-4).
//	It is written as a superset or not at all
//
// Red-before with the guard ALSO removed, which is the production consequence
// the guard exists for — the residual re-check names the rows the binding's own
// GC legitimately collected, and that is a city refusing to boot:
//
//	recovery exited 1; ... residual stranded: 3 / residual ids: gc-1, gc-2, gc-3
func TestStorageRecoverStrandedExtendsTheManifestAsASuperset(t *testing.T) {
	cityPath, cfg, source, collected := migratedThenCollectedCity(t)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("creating the city .gc directory: %v", err)
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	provenBefore := manifestIDs(t, target)
	if len(collected) == 0 {
		t.Fatal("the fixture collected nothing; the superset property would be vacuous")
	}

	stranded := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	provenAfter := manifestIDs(t, target)
	for _, id := range provenBefore {
		if !slices.Contains(provenAfter, id) {
			t.Errorf("the extended manifest dropped %s, which the previous manifest recorded; the binding's own GC collected it and the next boot would call it a stranded write", id)
		}
	}
	for _, id := range collected {
		if !slices.Contains(provenAfter, id) {
			t.Errorf("the manifest no longer records the GC-collected bead %s", id)
		}
		if _, err := openMigratedDestination(t, target).Get(id); err == nil {
			t.Errorf("the repair re-imported %s, which the binding's own GC deliberately deleted", id)
		}
	}
	if !slices.Contains(provenAfter, stranded.ID) {
		t.Errorf("the manifest does not record the recovered bead %s", stranded.ID)
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("the boot guard still reads %d stranded bead(s): %v", len(got), got)
	}
}

// TestManifestIDsDroppedNamesEveryIDTheReplacementLoses is the unit half of the
// superset guard. The end-to-end test above proves the extension is a superset
// today; this proves the guard that would catch it if it stopped being one,
// which is the part that has to stay non-vacuous on its own.
func TestManifestIDsDroppedNamesEveryIDTheReplacementLoses(t *testing.T) {
	previous := map[string]bool{"gc-1": true, "gc-2": true, "gc-3": true}
	if got := manifestIDsDropped(previous, []string{"gc-1", "gc-2", "gc-3", "gc-4"}); len(got) != 0 {
		t.Errorf("a superset reported %v as dropped", got)
	}
	got := manifestIDsDropped(previous, []string{"gc-2"})
	if want := []string{"gc-1", "gc-3"}; !slices.Equal(got, want) {
		t.Errorf("manifestIDsDropped = %v, want %v", got, want)
	}
}

// TestStorageRecoverStrandedRefusesAnAmbiguousClass proves the refusal fires
// rather than the repair guessing.
//
// The ambiguity it simulates is the real one: readInfraSnapshot selects the gap
// with a not-work filter, so it hands the repair every non-work bead, while the
// repair will only place a bead in a binding that actually serves that bead's
// class. Those two sets coincide today (pinned by
// TestStrandedRecoveryClassifiesWithTheSameAuthorityAsTheBootGuard) and the
// question this answers is what happens the moment they do not: a class the
// split does not relocate arrives in the gap, and there is no store anyone can
// say it belongs in.
//
// Narrowing infraMigrationClasses is how that state is reached without waiting
// for a seventh coordination class to exist. Nothing else is stubbed.
//
// Red-before, if the class gate were dropped and every gap bead moved:
//
//	recovery exited 0 and copied gc-4 into a binding that does not serve its class
func TestStorageRecoverStrandedRefusesAnAmbiguousClass(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	session := mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session", Labels: []string{"gc:session"}})

	previous := infraMigrationClasses
	infraMigrationClasses = []config.StorageClass{
		config.StorageClassGraph,
		config.StorageClassSessions,
		config.StorageClassMessaging,
		config.StorageClassNudges,
	}
	t.Cleanup(func() { infraMigrationClasses = previous })

	var stdout, stderr bytes.Buffer
	code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("recovery exited 0 and copied %s into a binding that does not serve its class; stdout: %s", tracker.ID, stdout.String())
	}
	report := stdout.String()
	if !strings.Contains(report, "REFUSED to move") || !strings.Contains(report, tracker.ID) {
		t.Errorf("the report does not name the bead it declined to move: %s", report)
	}
	if !strings.Contains(report, "classifies as orders") {
		t.Errorf("the refusal does not say WHY the bead was declined, which is the only thing that makes it actionable: %s", report)
	}

	binding := openMigratedDestination(t, target)
	if _, err := binding.Get(tracker.ID); err == nil {
		t.Errorf("the ambiguous bead %s was moved anyway", tracker.ID)
	}
	if _, err := source.Get(tracker.ID); err != nil {
		t.Errorf("the ambiguous bead %s is not intact in the work store: %v", tracker.ID, err)
	}
	// Refusing one bead must not refuse the others: a repair that stopped at the
	// first thing it could not state would leave a city needing N runs.
	if _, err := binding.Get(session.ID); err != nil {
		t.Errorf("the unambiguous bead %s was not moved: %v", session.ID, err)
	}
}

// TestStrandedRecoveryClassifiesWithTheSameAuthorityAsTheBootGuard pins the one
// agreement the repair cannot be useful without.
//
// readInfraSnapshot — the selector confirmInfraConvergence names strands with —
// treats every coordclass class other than work as infrastructure. If the
// repair's movable set were narrower than that, it would move some beads,
// declare itself done, and the boot would go on refusing over the rest; if it
// were wider it would place a work bead in a binding no work reader looks at.
//
// Red-before, with graph dropped from infraMigrationClasses:
//
//	coordclass class graph is infrastructure (readInfraSnapshot hands it to the
//	repair) but the repair will not move it; the boot guard and the repair would
//	disagree about the same bead
func TestStrandedRecoveryClassifiesWithTheSameAuthorityAsTheBootGuard(t *testing.T) {
	allowed := infraRecoverableClasses()
	for _, class := range coordclass.Classes() {
		switch {
		case class.IsInfrastructure() && !allowed[class]:
			t.Errorf("coordclass class %s is infrastructure (readInfraSnapshot hands it to the repair) but the repair will not move it; the boot guard and the repair would disagree about the same bead", class)
		case !class.IsInfrastructure() && allowed[class]:
			t.Errorf("the repair would move coordclass class %s into the infrastructure binding; work never crosses", class)
		}
	}
	if len(allowed) == 0 {
		t.Fatal("the repair will move nothing at all; this assertion proved nothing")
	}
}

// unlistableRelationSource models the bd/Postgres work store this repair was
// built against: it answers DepList with the adapter's own refusal while its
// rows are otherwise readable. The inline per-bead projection is supplied
// separately, exactly as bd's list JSON supplies it beside dependency_count.
type unlistableRelationSource struct {
	beads.Store
	err    error
	inline map[string][]beads.Dep
}

func (s unlistableRelationSource) DepList(string, string) ([]beads.Dep, error) { return nil, s.err }

// DepMetadata forwards to the leaf. Embedding beads.Store strips the read, and
// the repair treats a source it cannot ask as one it may not copy from, so
// without this the double is refused before the topology fallback it exists to
// exercise is ever reached.
func (s unlistableRelationSource) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	return depMetadataThrough(s.Store, issueID, dependsOnID)
}

func (s unlistableRelationSource) List(query beads.ListQuery) ([]beads.Bead, error) {
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

// postgresRelationRefusal is the message the deployed bd/Postgres backend
// answers a relation read with, verbatim.
const postgresRelationRefusal = `operation "IssueRelations" not supported by the postgres backend`

// swapRecoverySource re-points the source seam at a wrapper over the store the
// city was converged with, so the wrapper is installed for the repair alone and
// the cutover above it stays a real cutover.
func swapRecoverySource(t *testing.T, replacement beads.Store) {
	t.Helper()
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return replacement, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
}

// TestStorageRecoverStrandedReadsEdgesFromTheInlineProjection pins the fallback
// the live recovery actually ran on.
//
// The city this command was built for keeps its work ledger on bd/Postgres,
// whose adapter answers DepList with `operation "IssueRelations" not supported`.
// The migration's importInfraSnapshot and verifyInfraCopy both call DepList
// unconditionally, so neither can run against such a source at all. The repair
// falls back to beads.Bead.Dependencies — the inline projection bd's own list
// JSON carries — and only after witnessing that the projection is live
// somewhere in the store.
//
// Red-before, with the fallback removed (DepList's refusal propagating) — every
// bead becomes ambiguous and nothing crosses at all:
//
//	recovery exited 1; ...
//	  gc-4 (dependency topology unreadable: operation "IssueRelations" not supported by the postgres backend)
//	  gc-5 (dependency topology unreadable: operation "IssueRelations" not supported by the postgres backend)
//
// Red-before, if the fallback dropped edges instead of reading them:
//
//	the within-infra edge gc-5 -> gc-4 did not cross a source that cannot list
//	relations; the inline projection carried it and the repair ignored it
func TestStorageRecoverStrandedReadsEdgesFromTheInlineProjection(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	follow := mustCreateInfraBead(t, source, beads.Bead{Title: "order finalize", Type: "task", Labels: []string{"order-tracking"}})

	swapRecoverySource(t, unlistableRelationSource{
		Store: source,
		err:   fmt.Errorf("%s", postgresRelationRefusal),
		inline: map[string][]beads.Dep{
			follow.ID: {{IssueID: follow.ID, DependsOnID: tracker.ID, Type: "blocks"}},
		},
	})

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "inline per-bead projection") {
		t.Errorf("the report does not say which dependency read it used, so an operator cannot tell whether the topology was carried or guessed: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "witnessed live on "+follow.ID) {
		t.Errorf("the report does not name the bead that witnessed the projection, so the claim is not falsifiable from the report: %s", stdout.String())
	}

	binding := openMigratedDestination(t, target)
	deps, err := binding.DepList(follow.ID, "down")
	if err != nil {
		t.Fatalf("listing copied deps: %v", err)
	}
	var carried []string
	for _, d := range deps {
		carried = append(carried, d.DependsOnID)
	}
	if !slices.Contains(carried, tracker.ID) {
		t.Errorf("the within-infra edge %s -> %s did not cross a source that cannot list relations; the inline projection carried it and the repair ignored it. carried = %v", follow.ID, tracker.ID, carried)
	}
}

// TestStorageRecoverStrandedCarriesEdgePayloads pins the repair against the
// same loss the migration was carrying: an edge restored without its payload.
//
// The repair re-adds edges the binding lacks, and on this destination adding an
// edge does not merely fail to write the payload — setGraphEdgeMetadataTx clears
// the pair's sidecar first, so a plain DepAdd over an edge that had a gate
// DELETES it. That makes the failure worse here than in the copy: a repair run
// to close a gap would report "edges: N restored" while stripping gates off
// edges that were already fine. Both must hold, which is why the test restores
// one payload-carrying edge and one payloadless one in the same run.
//
// Red-before, if the repair re-added edges with a bare DepAdd:
//
//	the repaired edge carries ("", false), want the gate the source holds
func TestStorageRecoverStrandedCarriesEdgePayloads(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	gated := mustCreateInfraBead(t, source, beads.Bead{Title: "order finalize", Type: "task", Labels: []string{"order-tracking"}})
	gate := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	plain := mustCreateInfraBead(t, source, beads.Bead{Title: "an edge with nothing on it", Type: "task"})
	for _, to := range []string{gate.ID, plain.ID} {
		if err := source.DepAdd(gated.ID, to, "blocks"); err != nil {
			t.Fatalf("seeding the stranded edge %s -> %s: %v", gated.ID, to, err)
		}
	}

	const payload = `{"gate":"waits_for","threshold":3}`
	swapRecoverySource(t, &payloadCarryingSource{
		Store:    source,
		payloads: map[[2]string]string{{gated.ID, gate.ID}: payload},
	})

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	binding, ok := openMigratedDestination(t, target).(beads.DepMetadataReader)
	if !ok {
		t.Fatal("the binding cannot be asked about edge payloads, so this test cannot see what the repair wrote")
	}
	got, carried, err := binding.DepMetadata(gated.ID, gate.ID)
	if err != nil {
		t.Fatalf("reading the repaired edge's payload: %v", err)
	}
	if !carried || got != payload {
		t.Errorf("the repaired edge %s -> %s carries (%q, %v), want (%q, true): the repair restored the edge and dropped the gate",
			gated.ID, gate.ID, got, carried, payload)
	}
	got, carried, err = binding.DepMetadata(gated.ID, plain.ID)
	if err != nil {
		t.Fatalf("reading the payloadless repaired edge: %v", err)
	}
	if carried || got != "" {
		t.Errorf("an edge the source holds nothing for carries (%q, %v) after the repair", got, carried)
	}
}

// TestStorageRecoverStrandedRefusesAnUnwitnessedDependencyProjection is the
// other half of the fallback, and the reason the fallback is safe to have.
//
// An adapter that refuses relation listing AND never populates the inline field
// answers "no edges" for every bead. Reading that as an empty topology would
// move the whole gap edge-free and silently flatten the graph. So the projection
// has to be witnessed live somewhere in the store before an empty answer counts
// as evidence, and when it is not, every bead is ambiguous rather than
// edge-free.
//
// Red-before, with the witness requirement removed:
//
//	recovery exited 0 over a source that can state no bead's topology; the gap
//	crossed edge-free
func TestStorageRecoverStrandedRefusesAnUnwitnessedDependencyProjection(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	swapRecoverySource(t, unlistableRelationSource{Store: source, err: fmt.Errorf("%s", postgresRelationRefusal)})

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code == 0 {
		t.Fatalf("recovery exited 0 over a source that can state no bead's topology; the gap crossed edge-free. stdout: %s", stdout.String())
	}
	report := stdout.String()
	if !strings.Contains(report, "UNREADABLE") {
		t.Errorf("the report does not say the dependency read failed: %s", report)
	}
	if !strings.Contains(report, tracker.ID) || !strings.Contains(report, "dependency topology unreadable") {
		t.Errorf("the refusal does not name the bead and the reason: %s", report)
	}
	if _, err := openMigratedDestination(t, target).Get(tracker.ID); err == nil {
		t.Errorf("bead %s was moved despite an unstatable topology", tracker.ID)
	}
}

// TestStorageRecoverStrandedDryRunWritesNothing pins the reporting mode: it is
// what an operator runs first on a city that is already refusing to boot, so it
// must not be able to change anything.
func TestStorageRecoverStrandedDryRunWritesNothing(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	tracker := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	binding := openMigratedDestination(t, target)
	bindingBefore := infraStoreFingerprint(t, binding)
	manifestBefore := manifestIDs(t, target)
	dumpPath := filepath.Join(t.TempDir(), "stranded.json")

	var stdout, stderr bytes.Buffer
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg}
	stubInfraControllerPing(t, 0)
	// No attestation: a report that changes nothing does not need one, and
	// requiring one would make the first thing an operator runs the hardest.
	if code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{DryRun: true, DumpPath: dumpPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry run exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "dry-run: nothing was written") {
		t.Errorf("the dry run did not say it wrote nothing: %s", stdout.String())
	}
	if after := infraStoreFingerprint(t, openMigratedDestination(t, target)); !slices.Equal(bindingBefore, after) {
		t.Errorf("the dry run changed the binding: before %v, after %v", bindingBefore, after)
	}
	if after := manifestIDs(t, target); !slices.Equal(manifestBefore, after) {
		t.Errorf("the dry run changed the manifest: before %v, after %v", manifestBefore, after)
	}

	contents, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("reading the dump: %v", err)
	}
	var dumped []strandedBeadDump
	if err := json.Unmarshal(contents, &dumped); err != nil {
		t.Fatalf("decoding the dump: %v", err)
	}
	if len(dumped) != 1 || dumped[0].Bead.ID != tracker.ID {
		t.Fatalf("the dump does not carry the stranded bead: %+v", dumped)
	}
	if dumped[0].Class != coordclass.ClassOrders.String() {
		t.Errorf("the dump records class %q, want %q", dumped[0].Class, coordclass.ClassOrders)
	}
}

// TestStorageRecoverStrandedRefusesASingleStoreCity proves a city with no
// infrastructure split is untouched, by mutation rather than by assertion: the
// source seam fails the test if it is opened at all, so a repair that lost its
// target check would open the work store and go red here.
//
// Red-before, with the unresolved-target refusal removed — the next gate down
// answers about a binding that does not exist, which is a refusal an operator
// cannot act on:
//
//	the refusal does not say why there is nothing to do: "gc storage
//	recover-stranded: this city has not converged onto binding \"\" (infra.migrated
//	is absent or its database is gone) ..."
//
// Red-before with the convergence and manifest gates removed too, which is when
// the seam's own mutation assertion fires:
//
//	the repair opened the work store of a city with no infrastructure binding
//	the refusal does not say why there is nothing to do: "gc storage
//	recover-stranded: opening binding \"\" at : opening sqlite store: mkdir : no
//	such file or directory"
func TestStorageRecoverStrandedRefusesASingleStoreCity(t *testing.T) {
	cityPath := recoveryCityPath(t)
	// Every class on the reserved work binding: the legacy, pre-split city.
	cfg := &config.City{}

	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		t.Error("the repair opened the work store of a city with no infrastructure binding")
		return beads.NewMemStore(), nil
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code == 0 {
		t.Fatal("the repair claimed success on a city with nothing to recover into")
	}
	if !strings.Contains(stderr.String(), "no binding to recover into") {
		t.Errorf("the refusal does not say why there is nothing to do: %q", stderr.String())
	}
	entries, err := os.ReadDir(filepath.Join(cityPath, ".gc"))
	if err != nil {
		t.Fatalf("reading the city .gc directory: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == "store" {
			t.Error("the repair created a binding store on a single-store city")
		}
	}
}

// TestStorageRecoverStrandedRefusesAnUnconvergedCity keeps the repair from
// standing in for the cutover. A city that never converged owes the whole copy,
// and the copy is a different operation with a different proof.
func TestStorageRecoverStrandedRefusesAnUnconvergedCity(t *testing.T) {
	cityPath := recoveryCityPath(t)
	source := stubStrandedRecoverySource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session", Labels: []string{"gc:session"}})
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code == 0 {
		t.Fatal("the repair ran on a city that has not converged")
	}
	if !strings.Contains(stderr.String(), storageMigrationCommand) {
		t.Errorf("the refusal does not name the command that owes the copy: %q", stderr.String())
	}
}

// TestStorageRecoverStrandedRefusesAnUnprovenCity covers the city that
// converged before the manifest existed. Without the record, a bead absent from
// the binding cannot be told apart from one the binding's own GC collected, and
// recovering on that basis re-imports rows the binding deliberately deleted.
func TestStorageRecoverStrandedRefusesAnUnprovenCity(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	if err := os.Remove(target.ManifestPath()); err != nil {
		t.Fatalf("removing the manifest: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code == 0 {
		t.Fatal("the repair ran on a city with no proven-copy manifest")
	}
	if !strings.Contains(stderr.String(), "GC has since collected") {
		t.Errorf("the refusal does not say what cannot be distinguished: %q", stderr.String())
	}
}

// TestStorageRecoverStrandedRefusesAMovingSource covers the two writer gates.
// A repair proven against a source another controller is still writing proves
// nothing, and the writers this binary cannot probe are an operator
// attestation.
func TestStorageRecoverStrandedRefusesAMovingSource(t *testing.T) {
	cityPath, cfg, source, _ := convergedRecoveryCity(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}

	stubInfraControllerPing(t, os.Getpid()+1)
	var stdout, stderr bytes.Buffer
	if code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{}, &stdout, &stderr); code == 0 {
		t.Fatal("the repair ran while another controller was writing the source")
	}
	if !strings.Contains(stderr.String(), "is live on this city") {
		t.Errorf("the refusal does not name the live controller: %q", stderr.String())
	}

	stubInfraControllerPing(t, 0)
	request.FleetStopped = false
	stdout.Reset()
	stderr.Reset()
	if code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{}, &stdout, &stderr); code == 0 {
		t.Fatal("the repair wrote without the fleet-stopped attestation")
	}
	if !strings.Contains(stderr.String(), "--"+storageFleetStoppedFlag) {
		t.Errorf("the refusal does not name the attestation flag: %q", stderr.String())
	}
}

// TestStorageRecoverStrandedRequiresItsSourceExplicitly mirrors the migration's
// own rule: the source is stated, not detected.
func TestStorageRecoverStrandedRequiresItsSourceExplicitly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStorageCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"recover-stranded"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err == nil {
		t.Fatal("recover-stranded with no source succeeded")
	}
	if !strings.Contains(stderr.String(), "--from-work") {
		t.Errorf("the refusal does not name the source flag: %q", stderr.String())
	}
}

// TestStorageRecoveryCommandNamesTheVerbTheTreeCarries pins the spelling
// against the tree and against the diagnostics.
//
// This is the defect the whole command exists to close, in its smallest form: a
// refusal that names a command the binary does not carry, or names none at all,
// is an operator instruction that fails at the shell.
func TestStorageRecoveryCommandNamesTheVerbTheTreeCarries(t *testing.T) {
	surface, err := parseOperatorCommandSpelling(storageRecoveryCommand)
	if err != nil {
		t.Fatalf("the recovery spelling does not decompose: %v", err)
	}
	root := newStorageCmd(io.Discard, io.Discard)
	found, _, err := root.Find([]string{surface.Verb})
	if err != nil {
		t.Fatalf("resolving %q: %v", surface.Verb, err)
	}
	for _, flag := range []string{surface.Flag, storageFleetStoppedFlag, "dry-run", "dump"} {
		if found.Flags().Lookup(flag) == nil {
			t.Errorf("the resolved command registers no --%s flag", flag)
		}
	}
	// The instruction an operator is handed has to be the command plus the
	// attestation the repair requires, and its head has to be the spelling the
	// tree was built from.
	instruction := storageRecoveryInstruction()
	if !strings.HasPrefix(instruction, storageRecoveryCommand+" ") {
		t.Errorf("the operator instruction %q does not start with the spelling the tree is built from", instruction)
	}
	if !strings.Contains(instruction, "--"+storageFleetStoppedFlag) {
		t.Errorf("the operator instruction %q omits the attestation the repair requires, so it fails at the shell", instruction)
	}
	// The diagnostics prefix is the same spelling without its source flag, and
	// it is the one the command really prints rather than a second spelling
	// written out beside it. The comment on the old constant claimed this test
	// pinned it; no test could see it, so a rename of the verb would have left
	// every diagnostic in the file naming a command the binary no longer had —
	// which is the exact defect this command exists to close.
	want := "gc " + surface.Namespace + " " + surface.Verb
	if !strings.HasPrefix(storageRecoveryCommand, want+" ") {
		t.Errorf("the diagnostics prefix %q is not the head of %q", want, storageRecoveryCommand)
	}
	if got := storageRecoveryLogPrefix(); got != want {
		t.Errorf("the prefix the command prints is %q, want %q", got, want)
	}
	// And it is really what reaches an operator: the refusal below is the
	// cheapest one to provoke, so this is the derivation end to end.
	var stdout, stderr bytes.Buffer
	if code := doStorageRecoverStranded(context.Background(), storageOperatorRequest{CityPath: t.TempDir(), Cfg: &config.City{}, FleetStopped: true}, strandedRecoveryOptions{}, &stdout, &stderr); code == 0 {
		t.Fatal("the repair claimed success on a city with no infrastructure binding")
	}
	if !strings.HasPrefix(stderr.String(), want+":") {
		t.Errorf("the refusal does not lead with the spelling the tree is built from: %q", stderr.String())
	}
}

// TestStrandedAlarmsNameTheRecoveryCommand is the other half of the reported
// bug. The strand was already detected and already named its ids; what an
// operator had no way to learn was what to RUN. Every surface that reports a
// strand now says.
//
// Red-before, on the advice restored to its pre-fix wording:
//
//	the stranded advice names no runnable recovery: "gc start: this city converged
//	on binding \"infra\" ... The named beads are intact in the retained work store.
//	Stop every writer, recover them into the binding's database, and re-check with
//	`gc storage status`, which exits zero once the binding contains them. ..."
//
// Red-before, on `gc storage status` restored to its pre-fix output:
//
//	status names no runnable recovery, which is half the reported bug: city: ...
func TestStrandedAlarmsNameTheRecoveryCommand(t *testing.T) {
	cityPath, cfg, source, _ := convergedRecoveryCity(t)
	stranded := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	instruction := storageRecoveryInstruction()

	// 1. The boot-path advice, which is what a supervisor records.
	var log bytes.Buffer
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.Outcome != infraMigrationStranded {
		t.Fatalf("outcome = %v, want stranded; log: %s", report.Outcome, log.String())
	}
	advice := infraMigrationOperatorAdvice(report, "gc start")
	if !strings.Contains(advice, instruction) {
		t.Errorf("the stranded advice names no runnable recovery: %q", advice)
	}
	// 2. The containment re-check's own stderr, which is what a boot prints.
	if !strings.Contains(log.String(), instruction) {
		t.Errorf("the containment re-check names no runnable recovery: %q", log.String())
	}

	// 3. `gc storage status`, the deploy gate an operator reaches for after a
	// refusal. It used to print the ids and stop.
	var stdout, stderr bytes.Buffer
	stubInfraControllerPing(t, 0)
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr); code == 0 {
		t.Fatalf("status exited 0 on a stranded city: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), stranded.ID) {
		t.Errorf("status does not name the stranded bead: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), instruction) {
		t.Errorf("status names no runnable recovery, which is half the reported bug: %s", stdout.String())
	}
}

// TestBornSplitAdviceDoesNotNameARepairItCannotRun keeps the remedy honest in
// the one stranded-shaped outcome the recovery command cannot serve.
//
// A born-split binding is served by a provider this build carries no migration
// discipline for, and resolveInfraBindingTarget answers only for a binding
// backed by this build's own bead engine — so the repair would refuse before it
// opened anything. Naming it there would send an operator to a command that
// cannot help, which is the same class of defect as naming none.
func TestBornSplitAdviceDoesNotNameARepairItCannotRun(t *testing.T) {
	report := infraMigrationReport{
		Outcome:  infraMigrationBornSplitBlocked,
		Stranded: []string{"gc-1"},
		Target:   infraBindingTarget{Binding: "external", Root: t.TempDir()},
	}
	advice := infraMigrationOperatorAdvice(report, "gc start")
	if advice == "" {
		t.Fatal("the born-split outcome renders no advice at all")
	}
	verb := strings.Fields(storageRecoveryCommand)[2]
	if strings.Contains(advice, verb) {
		t.Errorf("the born-split advice names %q, which refuses on this binding's provider: %q", verb, advice)
	}
	if !strings.Contains(advice, "no repair command for that provider") {
		t.Errorf("the born-split advice does not say that this build carries no repair for it: %q", advice)
	}
}
