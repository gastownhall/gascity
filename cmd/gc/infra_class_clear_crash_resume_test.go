package main

// Crash/resume rows for the retained-copy clear, contributed by the lane
// split151 review (rel15/review-split151.md, A1). They pin that a clear
// interrupted mid-delete settles the cross-store edges of the rows the earlier
// run already removed: a satisfied blocking edge is released, and an edge to a
// still-open moved bead survives a cascading delete.

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// cascadingStore models a backend whose Delete drops every edge touching the
// row (bd/Dolt-like cascade).
type cascadingStore struct{ *beads.MemStore }

func (s cascadingStore) Delete(id string) error {
	for _, dir := range []string{"up", "down"} {
		deps, _ := s.DepList(id, dir)
		for _, d := range deps {
			_ = s.DepRemove(d.IssueID, d.DependsOnID)
		}
	}
	return s.MemStore.Delete(id)
}

func reviewSeedCrossEdges(t *testing.T, source beads.Store) (done, running, waitsOnDone, waitsOnRunning beads.Bead) {
	done = mustCreateInfraBead(t, source, beads.Bead{Title: "graft finished", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	running = mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	waitsOnDone = mustCreateInfraBead(t, source, beads.Bead{Title: "work waiting on done", Type: "task"})
	waitsOnRunning = mustCreateInfraBead(t, source, beads.Bead{Title: "work waiting on running", Type: "task"})
	for _, e := range []beads.Dep{{IssueID: waitsOnDone.ID, DependsOnID: done.ID, Type: "blocks"}, {IssueID: waitsOnRunning.ID, DependsOnID: running.ID, Type: "blocks"}} {
		if err := source.DepAdd(e.IssueID, e.DependsOnID, e.Type); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func reviewReadyIDs(t *testing.T, s beads.Store) []string {
	ready, err := s.Ready()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range ready {
		out = append(out, b.ID)
	}
	return out
}

// Non-cascading backend (file store / MemStore): crash after the satisfied
// blocker's row is deleted but before the release pass. The resume no longer
// sees that row, so the satisfied edge is never released.
func TestReviewCrashResumeReleasesSatisfiedEdge(t *testing.T) {
	stubInfraControllerPing(t, 0)
	mem := beads.NewMemStore()
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return mem, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	done, _, waitsOnDone, _ := reviewSeedCrossEdges(t, mem)
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClassesRetainingSource(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	binding := openMigratedDestination(t, target)
	if err := binding.Close(done.ID); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)

	n := 0
	prevDel := infraClearBeforeDelete
	infraClearBeforeDelete = func(string) error {
		n++
		if n == 2 { // first row (done) already deleted
			return errors.New("killed mid-clear")
		}
		return nil
	}
	first := migrateInfraClasses(t, cityPath, cfg, &log)
	infraClearBeforeDelete = prevDel
	if first.Outcome != infraMigrationRetained {
		t.Fatalf("first = %s", first.Outcome)
	}
	if _, err := mem.Get(done.ID); err == nil {
		t.Fatalf("precondition: %s should be deleted by first pass", done.ID)
	}
	log.Reset()
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("resume = %s: %s", got.Outcome, log.String())
	}
	if !slices.Contains(reviewReadyIDs(t, mem), waitsOnDone.ID) {
		deps, _ := mem.DepList(waitsOnDone.ID, "down")
		t.Errorf("after crash+resume %s is NOT ready though its blocker %s is closed in the binding; dangling edges: %+v", waitsOnDone.ID, done.ID, deps)
	}
}

// Cascading backend: crash after the still-open blocker's row is deleted (its
// inbound edge cascaded) but before the keep/restore pass. The resume no longer
// sees that row, so the kept edge is never restored: the dependent unblocks early,
// with no warning.
func TestReviewCrashResumeKeepsOpenBlockerEdgeOnCascadingBackend(t *testing.T) {
	stubInfraControllerPing(t, 0)
	mem := beads.NewMemStore()
	src := cascadingStore{mem}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return src, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	_, running, _, waitsOnRunning := reviewSeedCrossEdges(t, src)
	mustCreateInfraBead(t, src, beads.Bead{Title: "a session after", Type: "session", Labels: []string{"gc:session"}})
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClassesRetainingSource(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	// Sanity: an uninterrupted clear on a cascading store keeps the edge.
	n := 0
	prevDel := infraClearBeforeDelete
	infraClearBeforeDelete = func(string) error {
		n++
		if n == 3 { // done and running already deleted
			return errors.New("killed mid-clear")
		}
		return nil
	}
	first := migrateInfraClasses(t, cityPath, cfg, &log)
	infraClearBeforeDelete = prevDel
	if first.Outcome != infraMigrationRetained {
		t.Fatalf("first = %s: %s", first.Outcome, log.String())
	}
	if _, err := mem.Get(running.ID); err == nil {
		t.Fatalf("precondition: %s should be deleted by the first pass", running.ID)
	}
	log.Reset()
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("resume = %s: %s", got.Outcome, log.String())
	}
	if slices.Contains(reviewReadyIDs(t, mem), waitsOnRunning.ID) {
		t.Errorf("after crash+resume %s became READY although its blocker %s is still open in the binding (kept edge lost; no warning). log: %s", waitsOnRunning.ID, running.ID, log.String())
	}
}

// Same cascading backend, no crash: control that the uninterrupted path keeps it.
func TestReviewUninterruptedClearKeepsOpenBlockerOnCascadingBackend(t *testing.T) {
	stubInfraControllerPing(t, 0)
	mem := beads.NewMemStore()
	src := cascadingStore{mem}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return src, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	_, running, _, waitsOnRunning := reviewSeedCrossEdges(t, src)
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", got.Outcome, log.String())
	}
	if slices.Contains(reviewReadyIDs(t, mem), waitsOnRunning.ID) {
		t.Errorf("%s ready while %s open", waitsOnRunning.ID, running.ID)
	}
}
