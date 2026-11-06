package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// TestMigratedCityStopsServingAClosedStepFromTheWorkCopy is #5987 end to end
// through the production `gc ready` reader on a city that really ran the
// migration: a step closed through the binding must not reappear as ready work
// from the work store's frozen copy.
//
// Red before the clear: `gc ready` served scc-2 as open, the retained copy
// winning the first-leg-wins dedupe over the binding's closed row.
func TestMigratedCityStopsServingAClosedStepFromTheWorkCopy(t *testing.T) {
	cityPath := oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
	stubInfraControllerPing(t, 0)

	work, err := openInfraMigrationSource(cityPath)
	if err != nil {
		t.Fatalf("opening the work store: %v", err)
	}
	root := mustCreateInfraBead(t, work, beads.Bead{Title: "wf root", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	step := mustCreateInfraBead(t, work, beads.Bead{Title: "wf step", Type: "task", Metadata: beads.StringMap{"gc.root_bead_id": root.ID}})
	if got := coordclass.Classify(step); got != coordclass.ClassGraph {
		t.Fatalf("the seeded step classifies as %v, want graph", got)
	}
	_ = closeBeadStoreHandle(work)

	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("loading the city config: %v", err)
	}
	var log bytes.Buffer
	if report := migrateInfraClasses(t, cityPath, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("migration outcome = %s, want converged: %s", report.Outcome, log.String())
	}
	resetCLIStorageRoutes(t)

	graph, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassGraph)
	if !relocated {
		t.Fatal("the graph class is not relocated on the migrated city")
	}
	if err := graph.Close(step.ID); err != nil {
		t.Fatalf("closing the step through the binding: %v", err)
	}

	var out, errb bytes.Buffer
	if rc := cmdReady(readyFlags{}, &out, &errb); rc != 0 {
		t.Fatalf("gc ready exited %d: %s", rc, errb.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("decoding gc ready: %v: %s", err, out.String())
	}
	for _, r := range rows {
		if r["id"] == step.ID {
			t.Fatalf("gc ready serves %s as %v although the binding closed it; a retained work-store copy is still answering (#5987)", step.ID, r["status"])
		}
	}
	work, err = openInfraMigrationSource(cityPath)
	if err != nil {
		t.Fatalf("reopening the work store: %v", err)
	}
	defer closeBeadStoreHandle(work) //nolint:errcheck
	if _, err := work.Get(step.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("the work store still answers for %s after the migration (err=%v); every relocated bead must live in one store", step.ID, err)
	}
}

// TestAnUnclearedMigratedCityRefusesBootAndTheMigrationRepairsIt is the
// upgrade path for a city migrated by a build that kept its source: boot
// refuses with the command, the command clears, and boot then serves.
func TestAnUnclearedMigratedCityRefusesBootAndTheMigrationRepairsIt(t *testing.T) {
	cityPath := t.TempDir()
	source := stubInfraMigrationSource(t)
	session := mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "plain work", Type: "task"})
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))

	var log bytes.Buffer
	if got := migrateInfraClassesRetainingSource(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the v1.5.0-style cutover = %s, want converged: %s", got.Outcome, log.String())
	}

	log.Reset()
	report := checkInfraClassConvergence(cityPath, cfg, "gc start", &log)
	if report.Outcome != infraMigrationRetained {
		t.Fatalf("boot outcome = %s, want retained-copies; a city whose work store still answers for relocated beads must not serve: %s", report.Outcome, log.String())
	}
	if report.serving() {
		t.Fatal("the retained-copies outcome serves")
	}
	if !slices.Equal(report.Retained, []string{session.ID}) {
		t.Fatalf("retained = %v, want [%s]", report.Retained, session.ID)
	}
	advice := infraMigrationOperatorAdvice(report, "gc start")
	if !strings.Contains(advice, storageClearInstruction()) {
		t.Fatalf("the refusal does not name the repair command %q: %s", storageClearInstruction(), advice)
	}
	if !strings.Contains(advice, session.ID) {
		t.Fatalf("the refusal does not name the retained id: %s", advice)
	}
	if !strings.Contains(advice, "Do NOT revert") {
		t.Fatalf("the refusal does not withhold the revert on a binding that holds the city's state: %s", advice)
	}
	if got := storageBindingEventTypes[infraMigrationRetained]; got != events.StorageBindingUnconverged {
		t.Fatalf("retained-copies publishes %q, want %q", got, events.StorageBindingUnconverged)
	}

	log.Reset()
	repaired := migrateInfraClasses(t, cityPath, cfg, &log)
	if repaired.Outcome != infraMigrationConverged {
		t.Fatalf("re-run outcome = %s, want converged: %s", repaired.Outcome, log.String())
	}
	if repaired.Clear.Cleared != 1 {
		t.Fatalf("the re-run cleared %d row(s), want 1: %s", repaired.Clear.Cleared, log.String())
	}
	if _, err := source.Get(session.ID); err == nil {
		t.Fatalf("the repair left %s in the work store", session.ID)
	}
	if _, err := source.Get(work.ID); err != nil {
		t.Fatalf("the repair removed the work bead %s: %v", work.ID, err)
	}
	log.Reset()
	if got := checkInfraClassConvergence(cityPath, cfg, "gc start", &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("boot after the repair = %s, want converged: %s", got.Outcome, log.String())
	}
}

// TestTheClearRefusesRowsTheProvenCopyNeverCarried pins that a strand is never
// cleared: nothing proves the binding holds it, so removing it would delete the
// only copy. The retained copies beside it stay too — the clear is all or
// nothing per pass.
func TestTheClearRefusesRowsTheProvenCopyNeverCarried(t *testing.T) {
	cityPath := t.TempDir()
	source := stubInfraMigrationSource(t)
	carried := mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClassesRetainingSource(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	stranded := mustCreateInfraBead(t, source, beads.Bead{Title: "landed after the proof", Type: "session", Labels: []string{"gc:session"}})

	log.Reset()
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.Outcome != infraMigrationStranded {
		t.Fatalf("outcome = %s, want stranded: %s", report.Outcome, log.String())
	}
	for _, id := range []string{carried.ID, stranded.ID} {
		if _, err := source.Get(id); err != nil {
			t.Fatalf("the refused clear removed %s: %v", id, err)
		}
	}

	// The clear's own refusal, reached when the boot check cannot see the
	// strand (it is in the binding but not the manifest: recovery residue).
	_, err := clearRetainedInfraCopies(cityPath, mustResolveInfraTarget(t, cityPath, cfg), io.Discard)
	var unproven *infraUnprovenSourceRows
	if !errors.As(err, &unproven) || !slices.Contains(unproven.IDs, stranded.ID) {
		t.Fatalf("clear error = %v, want a refusal naming %s", err, stranded.ID)
	}
	if !strings.Contains(err.Error(), storageRecoveryInstruction()) {
		t.Fatalf("the refusal does not name the recovery command: %v", err)
	}
}

// TestTheClearRefusesAnOpenCrossStoreBlockerAndDropsTheRest pins D9 on the
// clear of a city an earlier build converged with its copies retained (S51):
// a work bead still blocked by a moved bead the binding has not finished
// stops the clear before any row goes, and once the binding finishes it the
// clear removes every edge from a work bead onto a moved bead and re-adds
// none, each recorded in the backup as dropped.
//
// Red on main: the clear re-added the open blocker's edge and the related
// edge after the delete (infraRestoreKeptEdge), writing cross-store edges.
func TestTheClearRefusesAnOpenCrossStoreBlockerAndDropsTheRest(t *testing.T) {
	cityPath := t.TempDir()
	source := stubInfraMigrationSource(t)
	done := mustCreateInfraBead(t, source, beads.Bead{Title: "graft finished", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	running := mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	waitsOnDone := mustCreateInfraBead(t, source, beads.Bead{Title: "work waiting on the finished graft", Type: "task"})
	waitsOnRunning := mustCreateInfraBead(t, source, beads.Bead{Title: "work waiting on the running graft", Type: "task"})
	related := mustCreateInfraBead(t, source, beads.Bead{Title: "work related to the running graft", Type: "task"})
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClassesRetainingSource(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	// The edges an earlier build's copy never planned, present at the clear.
	for _, edge := range []beads.Dep{
		{IssueID: waitsOnDone.ID, DependsOnID: done.ID, Type: "blocks"},
		{IssueID: waitsOnRunning.ID, DependsOnID: running.ID, Type: "blocks"},
		{IssueID: related.ID, DependsOnID: running.ID, Type: "related"},
	} {
		if err := source.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type); err != nil {
			t.Fatalf("seeding %v: %v", edge, err)
		}
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	closeInBinding := func(id string) {
		t.Helper()
		binding := openMigratedDestination(t, target)
		if err := binding.Close(id); err != nil {
			t.Fatalf("closing %s in the binding: %v", id, err)
		}
		_ = closeBeadStoreHandle(binding)
	}
	closeInBinding(done.ID)

	log.Reset()
	refused := migrateInfraClasses(t, cityPath, cfg, &log)
	var refusal *infraClearBoundaryRefusal
	if refused.Outcome != infraMigrationRetained || !errors.As(refused.Fault, &refusal) {
		t.Fatalf("outcome = %s (%v), want the clear refused over the open blocker: %s", refused.Outcome, refused.Fault, log.String())
	}
	if len(refusal.Refusals) != 1 || !slices.Equal(refusal.Refusals[0].IDs, []string{waitsOnRunning.ID, running.ID}) || !strings.Contains(refusal.Refusals[0].Remedy, "gc bd dep remove "+waitsOnRunning.ID+" "+running.ID) {
		t.Fatalf("refusals = %+v, want exactly %s blocked by %s with its remedy", refusal.Refusals, waitsOnRunning.ID, running.ID)
	}
	for _, id := range []string{done.ID, running.ID} {
		if _, err := source.Get(id); err != nil {
			t.Fatalf("the refused clear removed %s: %v", id, err)
		}
	}
	if note, present, _ := readInfraClearedNote(cityPath); present && len(note.Pending) > 0 {
		t.Fatalf("the refused clear left a pending note %+v; nothing was removed", note)
	}

	closeInBinding(running.ID)
	log.Reset()
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("outcome = %s: %s", report.Outcome, log.String())
	}
	if len(report.Clear.Dropped) != 3 {
		t.Fatalf("dropped = %+v, want all three edges onto moved beads", report.Clear.Dropped)
	}
	for _, w := range []beads.Bead{waitsOnDone, waitsOnRunning, related} {
		deps, err := source.DepList(w.ID, "down")
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 0 {
			t.Fatalf("%s still holds %+v after the clear: a cross-store edge was re-added", w.ID, deps)
		}
	}
	backup, _, err := readInfraRetainedBackup(target)
	if err != nil {
		t.Fatalf("reading the backup: %v", err)
	}
	dependents := 0
	for _, e := range backup {
		for _, d := range e.Dependents {
			dependents++
			if d.Disposition != rigEdgeDropped {
				t.Errorf("backup records %s as %q, want %q", d, d.Disposition, rigEdgeDropped)
			}
		}
	}
	if dependents != 3 {
		t.Fatalf("the backup records %d cross-store dependents, want all 3", dependents)
	}
}

// TestTheClearBoundaryCheckFollowsTheSharedPlan is the clear's refusal
// table: an edge from a row that stays onto a row the clear removes refuses
// exactly when the shared plan refuses it, judged by the binding's copy of
// the removed row (a copy the binding's GC collected reads as closed), and
// only while the work store still holds the edge. A formula step that passed
// releases its dependents even when its work record says blocked, as
// readiness judges it.
func TestTheClearBoundaryCheckFollowsTheSharedPlan(t *testing.T) {
	passed := beads.StringMap{beadmeta.StepRefMetadataKey: "mol.review", beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked}
	blocked := beads.StringMap{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked}
	for _, tc := range []struct {
		name     string
		kind     string
		work     string // the dependent's status in the work store
		copy     *beads.Bead
		detached bool // an earlier run already removed the edge
		another  bool // the dependent still holds another edge
		refuse   bool
	}{
		{name: "blocks on an open copy", kind: "blocks", work: "open", copy: &beads.Bead{Status: "open"}, refuse: true},
		{name: "blocks on a closed copy", kind: "blocks", work: "open", copy: &beads.Bead{Status: "closed"}},
		{name: "blocks on a copy closed blocked", kind: "blocks", work: "open", copy: &beads.Bead{Status: "closed", Metadata: blocked}, refuse: true},
		{name: "blocks on a passed step closed blocked", kind: "blocks", work: "open", copy: &beads.Bead{Status: "closed", Metadata: passed}},
		{name: "blocks on a collected copy", kind: "blocks", work: "open"},
		{name: "a closed dependent blocked", kind: "blocks", work: "closed", copy: &beads.Bead{Status: "open"}},
		{name: "waits-for on an open copy", kind: "waits-for", work: "open", copy: &beads.Bead{Status: "open"}, refuse: true},
		{name: "conditional-blocks on an open copy", kind: "conditional-blocks", work: "open", copy: &beads.Bead{Status: "open"}, refuse: true},
		{name: "tracks, both open", kind: "tracks", work: "open", copy: &beads.Bead{Status: "open"}, refuse: true},
		{name: "tracks, the dependent closed", kind: "tracks", work: "closed", copy: &beads.Bead{Status: "open"}},
		{name: "tracks, the copy closed", kind: "tracks", work: "open", copy: &beads.Bead{Status: "closed"}},
		{name: "parent-child, both open", kind: "parent-child", work: "open", copy: &beads.Bead{Status: "open"}, refuse: true},
		{name: "related, both open", kind: "related", work: "open", copy: &beads.Bead{Status: "open"}},
		{name: "blocks on an open copy, already detached", kind: "blocks", work: "open", copy: &beads.Bead{Status: "open"}, detached: true},
		{name: "already detached, the dependent still holding another edge", kind: "blocks", work: "open", copy: &beads.Bead{Status: "open"}, detached: true, another: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := beads.NewMemStore()
			d := mustCreateInfraBead(t, source, beads.Bead{Title: "moved", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
			w := mustCreateInfraBead(t, source, beads.Bead{Title: "work", Type: "task"})
			if tc.work == "closed" {
				if err := source.Close(w.ID); err != nil {
					t.Fatal(err)
				}
				w.Status = "closed"
			}
			if !tc.detached {
				if err := source.DepAdd(w.ID, d.ID, tc.kind); err != nil {
					t.Fatal(err)
				}
			}
			if tc.another {
				other := mustCreateInfraBead(t, source, beads.Bead{Title: "other work", Type: "task"})
				if err := source.DepAdd(w.ID, other.ID, "blocks"); err != nil {
					t.Fatal(err)
				}
			}
			held := map[string]beads.Bead{}
			if tc.copy != nil {
				c := *tc.copy
				c.ID = d.ID
				held[d.ID] = c
			}
			entries := []infraBackupEntry{{Bead: d, Dependents: []infraBackupEdge{{IssueID: w.ID, DependsOnID: d.ID, Type: tc.kind}}}}
			refusals, dropped, err := infraClearBoundaryRefusals(source, entries, map[string]bool{d.ID: true}, map[string]beads.Bead{w.ID: w}, held, coordclass.CurrentGeneration)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(refusals) > 0; got != tc.refuse {
				t.Fatalf("refused = %v (%+v), want %v", got, refusals, tc.refuse)
			}
			if tc.refuse && !slices.Equal(refusals[0].IDs, []string{w.ID, d.ID}) {
				t.Fatalf("the refusal names %v, want %s and %s", refusals[0].IDs, w.ID, d.ID)
			}
			if len(dropped) != 1 || dropped[0].IssueID != w.ID {
				t.Fatalf("dropped = %+v, want the one edge", dropped)
			}
		})
	}
}

// TestAnInterruptedClearResumesWithoutLosingTheBackup pins idempotence: a clear
// stopped mid-delete leaves a city boot refuses, and the re-run finishes with a
// backup that still holds every row, including the ones the first run removed.
func TestAnInterruptedClearResumesWithoutLosingTheBackup(t *testing.T) {
	cityPath := t.TempDir()
	source := stubInfraMigrationSource(t)
	var ids []string
	for _, title := range []string{"one", "two", "three"} {
		ids = append(ids, mustCreateInfraBead(t, source, beads.Bead{Title: title, Type: "session", Labels: []string{"gc:session"}}).ID)
	}
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))

	deletes := 0
	prev := infraClearBeforeDelete
	infraClearBeforeDelete = func(string) error {
		deletes++
		if deletes == 2 {
			return errors.New("killed mid-clear")
		}
		return nil
	}
	var log bytes.Buffer
	first := migrateInfraClasses(t, cityPath, cfg, &log)
	infraClearBeforeDelete = prev
	if first.Outcome != infraMigrationRetained {
		t.Fatalf("interrupted outcome = %s, want retained-copies: %s", first.Outcome, log.String())
	}
	if first.Fault == nil || !strings.Contains(infraMigrationOperatorAdvice(first, "gc storage migrate"), "killed mid-clear") {
		t.Fatalf("the interrupted clear's refusal does not carry its cause: %+v", first)
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	if got := checkInfraClassConvergence(cityPath, cfg, "gc start", &bytes.Buffer{}); got.Outcome != infraMigrationRetained {
		t.Fatalf("boot after the interrupted clear = %s, want retained-copies", got.Outcome)
	}
	// Status counts what the session has left, not the session: the note's
	// Pending is the whole session and does not shrink.
	var status bytes.Buffer
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &status, &status); code == 0 || !strings.Contains(status.String(), "INTERRUPTED (2 of the 3 bead(s) in the unfinished session are still in the work store)") {
		t.Fatalf("status after the interrupted clear = %d: %s", code, status.String())
	}

	log.Reset()
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("resumed outcome = %s, want converged: %s", got.Outcome, log.String())
	}
	backup, _, err := readInfraRetainedBackup(target)
	if err != nil {
		t.Fatalf("reading the backup: %v", err)
	}
	var backedUp []string
	for _, e := range backup {
		backedUp = append(backedUp, e.Bead.ID)
	}
	slices.Sort(backedUp)
	slices.Sort(ids)
	if !slices.Equal(backedUp, ids) {
		t.Fatalf("the backup holds %v after the resume, want every cleared row %v", backedUp, ids)
	}
	for _, id := range ids {
		if _, err := source.Get(id); err == nil {
			t.Fatalf("%s is still in the work store after the resume", id)
		}
	}
}

// TestTheClearRemovesNothingWhenTheBackupDoesNotProve pins the order: the
// backup is re-read from disk and proven before the first delete.
func TestTheClearRemovesNothingWhenTheBackupDoesNotProve(t *testing.T) {
	cityPath := t.TempDir()
	source := stubInfraMigrationSource(t)
	session := mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))

	prev := infraClearBackupWritten
	infraClearBackupWritten = func(path string) {
		if err := os.WriteFile(path, []byte(`{"format":"`+infraRetainedBackupFormat+`","beads":0}`+"\n"), 0o644); err != nil {
			t.Fatalf("corrupting the backup: %v", err)
		}
	}
	defer func() { infraClearBackupWritten = prev }()

	var log bytes.Buffer
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.Outcome != infraMigrationRetained {
		t.Fatalf("outcome = %s, want retained-copies: %s", report.Outcome, log.String())
	}
	if _, err := source.Get(session.ID); err != nil {
		t.Fatalf("the clear removed %s although its backup did not prove: %v", session.ID, err)
	}
	if !strings.Contains(log.String(), "proving the retained-source backup") {
		t.Fatalf("the refusal does not name the failed proof: %s", log.String())
	}
}

// TestTheBackupCarriesEdgePayloadsThroughAReCopy pins the re-converge recipe
// on a cleared city: the binding's database is removed, the work-store re-copy
// refuses, and the backup re-copy restores rows, edges and payloads.
func TestTheBackupCarriesEdgePayloadsThroughAReCopy(t *testing.T) {
	backing, from, to, work := seedInfraEdgeSource(t)
	const payload = `{"gate":"any-children"}`
	source := &payloadCarryingSource{Store: backing, payloads: map[[2]string]string{{from.ID, to.ID}: payload}}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	defer func() { openInfraMigrationSource = prev }()

	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	if _, err := backing.Get(from.ID); err == nil {
		t.Fatalf("the work store still holds %s", from.ID)
	}
	if _, err := backing.Get(work.ID); err != nil {
		t.Fatalf("the work bead %s was removed: %v", work.ID, err)
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
	edge, err := infraReadEdgePayload(binding, from.ID, to.ID)
	if err != nil {
		t.Fatalf("reading the re-copied payload: %v", err)
	}
	if !edge.Carried || edge.Payload != payload {
		t.Fatalf("re-copied edge payload = %+v, want %q", edge, payload)
	}
}

// TestBackupReCopyRefusesABindingThatIsNotStale pins that --from-backup can
// never overwrite a serving binding.
func TestBackupReCopyRefusesABindingThatIsNotStale(t *testing.T) {
	cityPath, cfg, _, target := convergedInfraCity(t)
	var log bytes.Buffer
	got := runInfraClassMigrationFrom(cityPath, target, infraMigrationFromBackup, "gc storage migrate", &log)
	if got.Outcome != infraMigrationUncheckable || !strings.Contains(log.String(), "re-copies only a binding whose database is gone") {
		t.Fatalf("outcome = %s, want a refusal: %s", got.Outcome, log.String())
	}
	if report := checkInfraClassConvergence(cityPath, cfg, "gc start", &bytes.Buffer{}); report.Outcome != infraMigrationConverged {
		t.Fatalf("the refused backup re-copy disturbed a serving city: %s", report.Outcome)
	}
}

// TestStorageMigrateTakesExactlyOneSource pins the flag contract.
func TestStorageMigrateTakesExactlyOneSource(t *testing.T) {
	surface, err := parseOperatorCommandSpelling(storageMigrationCommand)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{}, {"--" + surface.Flag, "--" + storageFromBackupFlag}} {
		var stdout, stderr bytes.Buffer
		cmd := newStorageMigrateCmd(surface, &stdout, &stderr)
		cmd.SetArgs(args)
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("migrate %v succeeded; it must take exactly one source", args)
		}
		if !strings.Contains(stderr.String(), "exactly one of") {
			t.Fatalf("migrate %v: refusal does not state the contract: %s", args, stderr.String())
		}
	}
}

// TestTheCopyRefusesAWorkBeadBlockedByAnOpenMovedBead pins the W -> D row of
// D9 on the city copy: an open work bead blocked by an open infrastructure
// bead cannot be carried (a work store holds no cross-store gate), so the
// copy refuses by id, with the fix, before the binding holds a row.
//
// Red on main: the copy carried the moved bead, and the clear re-added the
// edge after the delete as a cross-store edge.
func TestTheCopyRefusesAWorkBeadBlockedByAnOpenMovedBead(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := stubInfraMigrationSource(t)
	running := mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	waiting := mustCreateInfraBead(t, source, beads.Bead{Title: "work waiting on the running graft", Type: "task"})
	if err := source.DepAdd(waiting.ID, running.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	var log bytes.Buffer
	report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log)
	var refusal *infraBoundaryRefusal
	if report.Outcome != infraMigrationUnconverged || !errors.As(report.Fault, &refusal) {
		t.Fatalf("migrate = %s (%v), want the copy refused: %s", report.Outcome, report.Fault, log.String())
	}
	if len(refusal.Refusals) != 1 || !slices.Equal(refusal.Refusals[0].IDs, []string{waiting.ID, running.ID}) {
		t.Fatalf("refusals = %+v", refusal.Refusals)
	}
	if !strings.Contains(log.String(), "gc bd dep remove "+waiting.ID+" "+running.ID) {
		t.Fatalf("the refusal does not name the fix: %s", log.String())
	}
	target := mustResolveInfraTarget(t, request.CityPath, request.Cfg)
	if count, known := infraBindingContents(t, target); !known || count != 0 {
		t.Fatalf("the binding holds %d row(s) (known %v) after a refused copy, want none", count, known)
	}
	if _, err := source.Get(running.ID); err != nil {
		t.Fatalf("the refused copy removed %s: %v", running.ID, err)
	}
}

// TestALostCrossEdgeAnEarlierClearRecordedStillReachesStatusAndBoot: a clear
// by an earlier build that could not keep an edge recorded it in the cleared
// note. This build re-adds no edge, so it loses none, but the record it reads
// still reaches `gc storage status` and the boot event.
func TestALostCrossEdgeAnEarlierClearRecordedStillReachesStatusAndBoot(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	cityPath, cfg := request.CityPath, request.Cfg
	var log bytes.Buffer
	if report := migrateInfraClasses(t, cityPath, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", report.Outcome, log.String())
	}
	edge := "gc-w1 -[blocks]-> gc-d1"
	note, present, err := readInfraClearedNote(cityPath)
	if err != nil || !present {
		t.Fatalf("cleared note present=%v err=%v", present, err)
	}
	note.LostCrossEdges = []string{edge + " (no issue found)"}
	if err := writeInfraClearedNote(cityPath, note); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	doStorageStatus(request, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "lost cross-store edges: 1") || !strings.Contains(stdout.String(), edge) {
		t.Fatalf("status does not list the lost edge: %s", stdout.String())
	}
	rec := events.NewFake()
	routes, err := storageBootGate(cityPath, cfg, "gc start", rec, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("boot refused a cleared city: %v", err)
	}
	_ = routes.close()
	found := false
	for _, e := range rec.Events {
		var payload storebinding.StorageBindingOutcomePayload
		if json.Unmarshal(e.Payload, &payload) == nil && len(payload.LostCrossEdges) == 1 && strings.HasPrefix(payload.LostCrossEdges[0], edge) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no boot event carries the lost edge: %+v", rec.Events)
	}
}

// TestRevertingToTheWorkBindingAfterAClearRefusesBoot pins A2: once the work
// store's copies are cleared, pointing every class back at work (or deleting
// [storage]) would start the city with no infrastructure state. Boot refuses
// and names the restore.
//
// The fixture is a city migrated by an EARLIER build (v1.5.0/v1.5.1): the test
// helper migrates the way those builds' `gc storage migrate` did, writing no
// served-binding note. The last assertion — the revert proceeds once the
// operator removes the cleared note — holds only for such a city. A city this
// build migrated carries the served-binding note as well, and its revert is
// also held until that note is removed (TestRevertOfACityThisBuildMigrated).
func TestRevertingToTheWorkBindingAfterAClearRefusesBoot(t *testing.T) {
	cityPath := t.TempDir()
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}

	allWork := &config.City{Storage: &config.StorageConfig{Classes: config.StorageClasses{
		Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding, Sessions: config.StorageWorkBinding,
		Messaging: config.StorageWorkBinding, Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
	}}}
	for name, revert := range map[string]*config.City{"classes back at work": allWork, "[storage] deleted": {}} {
		_, err := storageBootGate(cityPath, revert, "gc start", nil, &bytes.Buffer{})
		if err == nil {
			t.Fatalf("%s: a cleared city started with no infrastructure state", name)
		}
		for _, want := range []string{"NO infrastructure state", "Restoring the work store from the backup", infraRetainedBackupName, infraClearedNotePath(cityPath)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal does not mention %q: %v", name, want, err)
			}
		}
	}

	// Earlier-build cities only: no served-binding note to hold the revert.
	if err := os.Remove(infraClearedNotePath(cityPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := storageBootGate(cityPath, allWork, "gc start", nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("after the operator's attestation the revert of an earlier-build city still refuses: %v", err)
	}
}

// referenceRewritingStore models the delete of a native-Dolt or bd work store
// (`bd delete --force`, the issueops Deleter): it finds the deleted row's
// neighbors by their edges, in both directions, rewrites every word-boundary
// mention of the deleted id in their text to "[deleted:<id>]", and drops the
// edges. refuseDangling adds bd's refusal of an edge to a bead the store does
// not hold.
type referenceRewritingStore struct {
	*beads.MemStore
	refuseDangling bool
}

func (s referenceRewritingStore) Delete(id string) error {
	mention := regexp.MustCompile(`\b` + regexp.QuoteMeta(id) + `\b`)
	for _, dir := range []string{"up", "down"} {
		deps, err := s.DepList(id, dir)
		if err != nil {
			return err
		}
		for _, d := range deps {
			other := d.IssueID
			if other == id {
				other = d.DependsOnID
			}
			row, err := s.Get(other)
			if err != nil {
				return err
			}
			text := mention.ReplaceAllString(row.Description, "[deleted:"+id+"]")
			if text != row.Description {
				if err := s.Update(other, beads.UpdateOpts{Description: &text}); err != nil {
					return err
				}
			}
			_ = s.DepRemove(d.IssueID, d.DependsOnID)
		}
	}
	return s.MemStore.Delete(id)
}

func (s referenceRewritingStore) DepAdd(issueID, dependsOnID, depType string) error {
	if s.refuseDangling {
		if _, err := s.Get(dependsOnID); err != nil {
			return errors.New(`no issue found matching "` + dependsOnID + `"`)
		}
	}
	return s.MemStore.DepAdd(issueID, dependsOnID, depType)
}

// TestTheClearNeverRewritesWorkBeadText is ga-w4p3pw: the clear moves the
// infrastructure rows, it does not delete beads anyone cites. A native-Dolt
// work store's delete rewrites each linked neighbor's mention of the deleted
// id to "[deleted:<id>]", so a work bead that depends on (or is related to) a
// moved bead and names it in its text must keep that text byte for byte. The
// rehearsal of the trust split rewrote 84 work beads this way, 28 of them
// mirrored to Linear. A work bead that names a moved bead without an edge was
// never a neighbor and is the control.
func TestTheClearNeverRewritesWorkBeadText(t *testing.T) {
	for _, tc := range []struct {
		name           string
		refuseDangling bool
	}{
		{name: "store keeps dangling edges"},
		{name: "bd-like store refuses dangling edges", refuseDangling: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubInfraControllerPing(t, 0)
			source := referenceRewritingStore{MemStore: beads.NewMemStore(), refuseDangling: tc.refuseDangling}
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
			t.Cleanup(func() { openInfraMigrationSource = prev })

			moved := mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
			movedClosed := mustCreateInfraBead(t, source, beads.Bead{Title: "graft finished", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
			if err := source.Close(movedClosed.ID); err != nil {
				t.Fatal(err)
			}
			texts := map[string]string{}
			work := func(title, text string) beads.Bead {
				b := mustCreateInfraBead(t, source, beads.Bead{Title: title, Type: "task", Description: text})
				texts[b.ID] = text
				return b
			}
			blocked := work("blocked on the running graft", "Four were P1: tr-l0py1, tr-rikgb, tr-wso41, "+moved.ID+".")
			// Closed: an open one would refuse the copy (D9's W -> D row).
			if err := source.Close(blocked.ID); err != nil {
				t.Fatal(err)
			}
			released := work("waited on the finished graft", "follow-up of "+movedClosed.ID+"; see "+movedClosed.ID+" notes")
			related := work("related to the running graft", moved.ID+" is the graft this tracks")
			work("cites the graft with no edge", "context: "+moved.ID)
			for _, edge := range []beads.Dep{
				{IssueID: blocked.ID, DependsOnID: moved.ID, Type: "blocks"},
				{IssueID: released.ID, DependsOnID: movedClosed.ID, Type: "blocks"},
				{IssueID: related.ID, DependsOnID: moved.ID, Type: "related"},
			} {
				if err := source.MemStore.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type); err != nil {
					t.Fatalf("seeding %v: %v", edge, err)
				}
			}

			request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
			var log bytes.Buffer
			report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log)
			if report.Outcome != infraMigrationConverged {
				t.Fatalf("migrate = %s: %s", report.Outcome, log.String())
			}
			if report.Clear.Cleared != 2 {
				t.Fatalf("cleared %d rows, want both moved beads: %s", report.Clear.Cleared, log.String())
			}
			for id, want := range texts {
				got, err := source.Get(id)
				if err != nil {
					t.Fatalf("Get(%s): %v", id, err)
				}
				if got.Description != want {
					t.Errorf("work bead %s text rewritten by the clear:\n got %q\nwant %q", id, got.Description, want)
				}
			}
			if len(report.Clear.Dropped) != 3 {
				t.Errorf("dropped = %+v, want the three edges onto moved beads", report.Clear.Dropped)
			}
			for _, w := range []beads.Bead{blocked, released, related} {
				if deps, err := source.DepList(w.ID, "down"); err != nil || len(deps) != 0 {
					t.Errorf("%s deps = %+v (%v); the clear re-adds no cross-store edge", w.ID, deps, err)
				}
			}
		})
	}
}

// TestTheClearRefusesAnEdgeItsBackupDoesNotRecord pins the guard under
// ga-w4p3pw's fix. The clear detaches a row by the edges its backup entry
// records; an edge that reached the row after the backup (here, written between
// the backup and the delete) is one no backup can restore. Deleting the row
// would drop that edge and, on a bd or native-Dolt work store, rewrite the
// linked work bead's text, so the clear refuses by edge and the row stays.
func TestTheClearRefusesAnEdgeItsBackupDoesNotRecord(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := referenceRewritingStore{MemStore: beads.NewMemStore()}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	moved := mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	text := "follow-up of " + moved.ID
	late := mustCreateInfraBead(t, source, beads.Bead{Title: "linked after the backup", Type: "task", Description: text})
	prevBefore := infraClearBeforeDelete
	infraClearBeforeDelete = func(id string) error {
		if id != moved.ID {
			return nil
		}
		return source.MemStore.DepAdd(late.ID, moved.ID, "related")
	}
	t.Cleanup(func() { infraClearBeforeDelete = prevBefore })

	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	var log bytes.Buffer
	report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log)
	if report.Outcome != infraMigrationRetained {
		t.Fatalf("migrate = %s, want the clear refused over the unrecorded edge: %s", report.Outcome, log.String())
	}
	advice := infraMigrationOperatorAdvice(report, "gc storage migrate")
	if !strings.Contains(advice, late.ID+" -[related]-> "+moved.ID) || !strings.Contains(advice, "The row stays in the work store") {
		t.Fatalf("the refusal does not name the unrecorded edge %s -> %s and what became of the row: %s", late.ID, moved.ID, advice)
	}
	if _, err := source.Get(moved.ID); err != nil {
		t.Fatalf("the clear removed %s over an edge its backup does not record: %v", moved.ID, err)
	}
	got, err := source.Get(late.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != text {
		t.Fatalf("work bead %s text rewritten by the clear:\n got %q\nwant %q", late.ID, got.Description, text)
	}
	if deps, err := source.DepList(late.ID, "down"); err != nil || len(deps) != 1 || deps[0].DependsOnID != moved.ID {
		t.Fatalf("%s deps = %+v (%v); the unrecorded edge must survive the refusal", late.ID, deps, err)
	}
}

// TestTheBackupRecordsWhatTheCopyDidWithEachOwnEdge: the clear's backup marks
// each edge a removed row held with what the copy did with it: copied onto
// another moved row, gated onto open work, copied-external, dropped onto
// closed work. (Edges onto the row from rows that stay are marked dropped,
// TestTheClearRefusesAnOpenCrossStoreBlockerAndDropsTheRest.)
func TestTheBackupRecordsWhatTheCopyDidWithEachOwnEdge(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := stubInfraMigrationSource(t)
	wisp := func(title string) beads.Bead {
		return mustCreateInfraBead(t, source, beads.Bead{Title: title, Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	}
	owner, sibling := wisp("owner"), wisp("sibling")
	openWork := mustCreateInfraBead(t, source, beads.Bead{Title: "open work", Type: "task"})
	doneWork := mustCreateInfraBead(t, source, beads.Bead{Title: "finished work", Type: "task"})
	if err := source.Close(doneWork.ID); err != nil {
		t.Fatal(err)
	}
	for _, e := range []beads.Dep{
		{IssueID: owner.ID, DependsOnID: sibling.ID, Type: "blocks"},
		{IssueID: owner.ID, DependsOnID: openWork.ID, Type: "blocks"},
		{IssueID: owner.ID, DependsOnID: "external:ga:deploy", Type: "blocks"},
		{IssueID: owner.ID, DependsOnID: doneWork.ID, Type: "related"},
	} {
		if err := source.DepAdd(e.IssueID, e.DependsOnID, e.Type); err != nil {
			t.Fatal(err)
		}
	}
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if report := migrateInfraClasses(t, cityPath, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", report.Outcome, log.String())
	}
	backup, _, err := readInfraRetainedBackup(mustResolveInfraTarget(t, cityPath, cfg))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range backup {
		if e.Bead.ID == owner.ID {
			for _, d := range e.Deps {
				got[d.DependsOnID] = d.Disposition
			}
		}
	}
	want := map[string]string{sibling.ID: rigEdgeCopied, openWork.ID: rigEdgeGated, "external:ga:deploy": rigEdgeExternal, doneWork.ID: rigEdgeDropped}
	for target, disposition := range want {
		if got[target] != disposition {
			t.Errorf("the backup marks %s -> %s %q, want %q (all: %v)", owner.ID, target, got[target], disposition, got)
		}
	}
}

// TestALaterClearMarksAnEdgeOntoAnEarlierClearedRowCopied: a clear session
// after the one that removed a row marks an edge onto that row copied (the
// manifest carried it), not dropped: the edge is in the binding.
func TestALaterClearMarksAnEdgeOntoAnEarlierClearedRowCopied(t *testing.T) {
	entry := infraBackupEntry{
		Bead: beads.Bead{ID: "mc-new"},
		Deps: []infraBackupEdge{
			{IssueID: "mc-new", DependsOnID: "mc-earlier", Type: "blocks"},
			{IssueID: "mc-new", DependsOnID: "mc-work", Type: "related"},
		},
	}
	got := withInfraClearDispositions(entry, map[string]bool{"mc-new": true}, map[string]bool{"mc-new": true, "mc-earlier": true}, nil)
	if got.Deps[0].Disposition != rigEdgeCopied || got.Deps[1].Disposition != rigEdgeDropped {
		t.Fatalf("dispositions = %q, %q; want copied onto the earlier-cleared row, dropped onto work", got.Deps[0].Disposition, got.Deps[1].Disposition)
	}
}

// TestTheReCopyFromTheBackupPlansNonBlockingBoundaryEdges: the backup source
// serves every edge a row held, so the re-copy plans a non-blocking boundary
// edge as the copy did, dropped and recorded on its row, where it used to
// leave it out unrecorded.
func TestTheReCopyFromTheBackupPlansNonBlockingBoundaryEdges(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := stubInfraMigrationSource(t)
	moved := mustCreateInfraBead(t, source, beads.Bead{Title: "wisp", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "work", Type: "task"})
	if err := source.DepAdd(moved.ID, work.ID, "related"); err != nil {
		t.Fatal(err)
	}
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if report := migrateInfraClasses(t, cityPath, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", report.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	if err := os.RemoveAll(target.Dir); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if got := runInfraClassMigrationFrom(cityPath, target, infraMigrationFromBackup, "gc storage migrate", &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("re-copy = %s: %s", got.Outcome, log.String())
	}
	binding := openMigratedDestination(t, target)
	row, err := binding.Get(moved.ID)
	if err != nil {
		t.Fatal(err)
	}
	var recs []rigCrossEdgeRecord
	if err := json.Unmarshal([]byte(row.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]), &recs); err != nil {
		t.Fatalf("%s's record %q: %v", moved.ID, row.Metadata[beadmeta.MigratedCrossEdgesMetadataKey], err)
	}
	if len(recs) != 1 || recs[0].Other != work.ID || recs[0].Type != "related" || recs[0].Action != rigEdgeDropped {
		t.Fatalf("%s's re-copied record = %+v, want the related edge onto %s dropped", moved.ID, recs, work.ID)
	}
}

// TestInfraEdgeLiveAsksForTheEdgeNotTheDependent: a dependent that still has
// another edge does not make an edge an earlier run already removed live.
func TestInfraEdgeLiveAsksForTheEdgeNotTheDependent(t *testing.T) {
	store := beads.NewMemStore()
	w := mustCreateInfraBead(t, store, beads.Bead{Title: "work", Type: "task"})
	other := mustCreateInfraBead(t, store, beads.Bead{Title: "other", Type: "task"})
	if err := store.DepAdd(w.ID, other.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if live, err := infraEdgeLive(store, w.ID, "mc-detached"); err != nil || live {
		t.Fatalf("infraEdgeLive(%s, detached) = %v (%v), want false though %s still has an edge", w.ID, live, err, w.ID)
	}
	if live, err := infraEdgeLive(store, w.ID, other.ID); err != nil || !live {
		t.Fatalf("infraEdgeLive(%s, %s) = %v (%v), want true", w.ID, other.ID, live, err)
	}
}
