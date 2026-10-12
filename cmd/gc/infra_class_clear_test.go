package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	if rc := cmdReady(readyOpts{}, &out, &errb); rc != 0 {
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

// TestTheClearReleasesSatisfiedCrossStoreBlocksAndKeepsTheRest pins the edge
// rule. A work bead blocked on a relocated bead the binding has closed stops
// waiting on its frozen copy; one blocked on a relocated bead still open keeps
// waiting; a non-blocking edge is kept.
func TestTheClearReleasesSatisfiedCrossStoreBlocksAndKeepsTheRest(t *testing.T) {
	cityPath := t.TempDir()
	source := stubInfraMigrationSource(t)
	done := mustCreateInfraBead(t, source, beads.Bead{Title: "graft finished", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	running := mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	waitsOnDone := mustCreateInfraBead(t, source, beads.Bead{Title: "work waiting on the finished graft", Type: "task"})
	waitsOnRunning := mustCreateInfraBead(t, source, beads.Bead{Title: "work waiting on the running graft", Type: "task"})
	related := mustCreateInfraBead(t, source, beads.Bead{Title: "work related to the running graft", Type: "task"})
	for _, edge := range []beads.Dep{
		{IssueID: waitsOnDone.ID, DependsOnID: done.ID, Type: "blocks"},
		{IssueID: waitsOnRunning.ID, DependsOnID: running.ID, Type: "blocks"},
		{IssueID: related.ID, DependsOnID: running.ID, Type: "related"},
	} {
		if err := source.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type); err != nil {
			t.Fatalf("seeding %v: %v", edge, err)
		}
	}
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClassesRetainingSource(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	binding := openMigratedDestination(t, target)
	if err := binding.Close(done.ID); err != nil {
		t.Fatalf("closing %s in the binding: %v", done.ID, err)
	}
	_ = closeBeadStoreHandle(binding)

	log.Reset()
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("outcome = %s: %s", report.Outcome, log.String())
	}
	if len(report.Clear.Released) != 1 || report.Clear.Released[0].IssueID != waitsOnDone.ID {
		t.Fatalf("released = %+v, want exactly %s -> %s", report.Clear.Released, waitsOnDone.ID, done.ID)
	}

	edgesOf := func(id string) []string {
		deps, err := source.DepList(id, "down")
		if err != nil {
			t.Fatalf("DepList(%s): %v", id, err)
		}
		var out []string
		for _, d := range deps {
			out = append(out, d.DependsOnID)
		}
		return out
	}
	if got := edgesOf(waitsOnDone.ID); len(got) != 0 {
		t.Fatalf("%s still waits on %v after the binding closed its blocker", waitsOnDone.ID, got)
	}
	if got := edgesOf(waitsOnRunning.ID); !slices.Equal(got, []string{running.ID}) {
		t.Fatalf("%s edges = %v; a dependent of a still-open relocated bead must keep waiting", waitsOnRunning.ID, got)
	}
	if got := edgesOf(related.ID); !slices.Equal(got, []string{running.ID}) {
		t.Fatalf("%s edges = %v; a non-blocking cross edge must be kept", related.ID, got)
	}
	ready, err := source.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	var readyIDs []string
	for _, b := range ready {
		readyIDs = append(readyIDs, b.ID)
	}
	if !slices.Contains(readyIDs, waitsOnDone.ID) {
		t.Errorf("%s is not ready after its blocker closed in the binding: %v", waitsOnDone.ID, readyIDs)
	}
	if slices.Contains(readyIDs, waitsOnRunning.ID) {
		t.Errorf("%s became ready while its blocker is still open in the binding: %v", waitsOnRunning.ID, readyIDs)
	}

	backup, _, err := readInfraRetainedBackup(target)
	if err != nil {
		t.Fatalf("reading the backup: %v", err)
	}
	dependents := 0
	for _, e := range backup {
		dependents += len(e.Dependents)
	}
	if dependents != 3 {
		t.Fatalf("the backup records %d cross-store dependents, want all 3", dependents)
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

// danglingRefusingStore models a bd or native-Dolt work store: Delete drops
// every edge touching the row, and DepAdd refuses an edge to a bead the store
// does not hold.
type danglingRefusingStore struct{ *beads.MemStore }

func (s danglingRefusingStore) Delete(id string) error {
	for _, dir := range []string{"up", "down"} {
		deps, _ := s.DepList(id, dir)
		for _, d := range deps {
			_ = s.DepRemove(d.IssueID, d.DependsOnID)
		}
	}
	return s.MemStore.Delete(id)
}

func (s danglingRefusingStore) DepAdd(issueID, dependsOnID, depType string) error {
	if _, err := s.Get(dependsOnID); err != nil {
		return errors.New(`no issue found matching "` + dependsOnID + `"`)
	}
	return s.MemStore.DepAdd(issueID, dependsOnID, depType)
}

// TestCrossStoreEdgesADoltStoreCannotKeepAreAnnouncedAndRecorded pins the
// Dolt-shaped loss: the edges at risk are listed before any row is removed,
// and the ones the store then refuses are recorded where they outlive the
// run — the cleared note, `gc storage status`, and the boot event.
func TestCrossStoreEdgesADoltStoreCannotKeepAreAnnouncedAndRecorded(t *testing.T) {
	stubInfraControllerPing(t, 0)
	mem := beads.NewMemStore()
	source := danglingRefusingStore{mem}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	running := mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	waiting := mustCreateInfraBead(t, source, beads.Bead{Title: "work waiting on the running graft", Type: "task"})
	if err := source.DepAdd(waiting.ID, running.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	cityPath, cfg := request.CityPath, request.Cfg

	var announced bytes.Buffer
	deleted := false
	prevDel := infraClearBeforeDelete
	infraClearBeforeDelete = func(string) error {
		if !deleted && !strings.Contains(announced.String(), "AT RISK") {
			t.Errorf("a row was removed before the at-risk cross edges were announced: %q", announced.String())
		}
		deleted = true
		return nil
	}
	t.Cleanup(func() { infraClearBeforeDelete = prevDel })
	report := migrateInfraClasses(t, cityPath, cfg, &announced)
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", report.Outcome, announced.String())
	}
	edge := waiting.ID + " -[blocks]-> " + running.ID
	if !strings.Contains(announced.String(), edge) {
		t.Fatalf("the migrate output does not list the edge at risk %q: %s", edge, announced.String())
	}
	if len(report.LostCrossEdges) != 1 || !strings.HasPrefix(report.LostCrossEdges[0], edge) {
		t.Fatalf("report.LostCrossEdges = %v, want the refused edge %q", report.LostCrossEdges, edge)
	}
	note, present, err := readInfraClearedNote(cityPath)
	if err != nil || !present || !note.Complete || len(note.LostCrossEdges) != 1 {
		t.Fatalf("cleared note = %+v present=%v err=%v; want complete with the lost edge", note, present, err)
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

	if err := os.Remove(infraClearedNotePath(cityPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := storageBootGate(cityPath, allWork, "gc start", nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("after the operator's attestation the revert still refuses: %v", err)
	}
}
