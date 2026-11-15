package main

// Crash/resume rows for the retained-copy clear. The clear never re-adds an
// edge (D9), so a run interrupted anywhere leaves no edge to restore: what it
// must keep is the boundary check, run again by every resumed run before the
// delete, and the backup's record of every edge it dropped.

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

// depAddCounting counts every edge written to the work store, and cascades
// a delete onto the row's edges when cascade is set.
type depAddCounting struct {
	*beads.MemStore
	cascade bool
	adds    *int
}

func (s depAddCounting) DepAdd(issueID, dependsOnID, depType string) error {
	*s.adds++
	return s.MemStore.DepAdd(issueID, dependsOnID, depType)
}

func (s depAddCounting) Delete(id string) error {
	if s.cascade {
		return cascadingStore{s.MemStore}.Delete(id)
	}
	return s.MemStore.Delete(id)
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

// retainedCrossEdgeCity is a city an earlier build converged with its copies
// retained (S51), whose work beads depend on the moved ones: waitsOnDone
// blocks on done (closed in the binding), related is related to running
// (open). source counts the edges the clear writes.
type retainedCrossEdgeCity struct {
	cityPath                   string
	cfg                        *config.City
	target                     infraBindingTarget
	mem                        *beads.MemStore
	adds                       *int
	done, running              beads.Bead
	waitsOnDone, related, more beads.Bead
}

func newRetainedCrossEdgeCity(t *testing.T, cascading bool) retainedCrossEdgeCity {
	t.Helper()
	stubInfraControllerPing(t, 0)
	c := retainedCrossEdgeCity{mem: beads.NewMemStore(), adds: new(int)}
	source := depAddCounting{MemStore: c.mem, cascade: cascading, adds: c.adds}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	c.done = mustCreateInfraBead(t, c.mem, beads.Bead{Title: "graft finished", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	c.running = mustCreateInfraBead(t, c.mem, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	c.more = mustCreateInfraBead(t, c.mem, beads.Bead{Title: "a session", Type: "session", Labels: []string{"gc:session"}})
	c.waitsOnDone = mustCreateInfraBead(t, c.mem, beads.Bead{Title: "work waiting on done", Type: "task"})
	c.related = mustCreateInfraBead(t, c.mem, beads.Bead{Title: "work related to running", Type: "task"})
	c.cityPath = t.TempDir()
	c.cfg = infraSplitConfig(filepath.Join(c.cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClassesRetainingSource(t, c.cityPath, c.cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	for _, e := range []beads.Dep{{IssueID: c.waitsOnDone.ID, DependsOnID: c.done.ID, Type: "blocks"}, {IssueID: c.related.ID, DependsOnID: c.running.ID, Type: "related"}} {
		if err := c.mem.DepAdd(e.IssueID, e.DependsOnID, e.Type); err != nil {
			t.Fatal(err)
		}
	}
	c.target = mustResolveInfraTarget(t, c.cityPath, c.cfg)
	c.setInBinding(t, c.done.ID, "closed")
	*c.adds = 0
	return c
}

func (c retainedCrossEdgeCity) setInBinding(t *testing.T, id, status string) {
	t.Helper()
	binding := openMigratedDestination(t, c.target)
	if err := binding.Update(id, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
}

// assertClearedWithoutReAdding: converged, no edge from a work bead onto a
// moved bead left or written, and each recorded in the backup as dropped.
func (c retainedCrossEdgeCity) assertClearedWithoutReAdding(t *testing.T) {
	t.Helper()
	if *c.adds != 0 {
		t.Fatalf("the clear wrote %d edge(s) into the work store; it never re-adds one", *c.adds)
	}
	for _, w := range []beads.Bead{c.waitsOnDone, c.related} {
		deps, err := c.mem.DepList(w.ID, "down")
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 0 {
			t.Fatalf("%s still depends on %+v after the clear", w.ID, deps)
		}
	}
	backup, _, err := readInfraRetainedBackup(c.target)
	if err != nil {
		t.Fatal(err)
	}
	recorded := map[string]string{}
	for _, e := range backup {
		for _, d := range e.Dependents {
			recorded[d.IssueID+">"+d.DependsOnID] = d.Disposition
		}
	}
	for _, key := range []string{c.waitsOnDone.ID + ">" + c.done.ID, c.related.ID + ">" + c.running.ID} {
		if recorded[key] != rigEdgeDropped {
			t.Fatalf("the backup records %s as %q, want %q: the edge is lost without a record", key, recorded[key], rigEdgeDropped)
		}
	}
	if !slices.Contains(reviewReadyIDs(t, c.mem), c.waitsOnDone.ID) {
		t.Fatalf("%s is not ready after the binding finished its blocker", c.waitsOnDone.ID)
	}
}

// TestTheClearNeverReAddsAnEdgeAcrossACrash kills the clear after its first
// row went and resumes it, on a store whose delete keeps edges and on one
// whose delete cascades them: the resumed run writes no edge into the work
// store, and the backup records every edge it dropped.
//
// Red on main: the resumed run re-added the related edge onto the removed
// running graft (infraRestoreKeptEdge), a cross-store edge.
func TestTheClearNeverReAddsAnEdgeAcrossACrash(t *testing.T) {
	for _, cascading := range []bool{false, true} {
		t.Run(map[bool]string{false: "keeping", true: "cascading"}[cascading], func(t *testing.T) {
			c := newRetainedCrossEdgeCity(t, cascading)
			n := 0
			prev := infraClearBeforeDelete
			infraClearBeforeDelete = func(string) error {
				n++
				if n == 2 {
					return errors.New("killed mid-clear")
				}
				return nil
			}
			var log bytes.Buffer
			first := migrateInfraClasses(t, c.cityPath, c.cfg, &log)
			infraClearBeforeDelete = prev
			if first.Outcome != infraMigrationRetained || !strings.Contains(log.String(), "killed mid-clear") {
				t.Fatalf("first = %s: %s", first.Outcome, log.String())
			}
			log.Reset()
			if got := migrateInfraClasses(t, c.cityPath, c.cfg, &log); got.Outcome != infraMigrationConverged {
				t.Fatalf("resume = %s: %s", got.Outcome, log.String())
			}
			c.assertClearedWithoutReAdding(t)
		})
	}
}

// TestTheClearChecksItsBoundaryAgainAfterAKill is the crash seam between the
// clear's boundary check and its delete: a run killed there, after the check
// passed, whose blocker then reopens in the binding, is refused by the
// resumed run before it removes a row, and the edge stays. Both kill points
// are covered: before the cleared note goes pending (the "clear checked"
// boundary), and after (before the first row's delete).
func TestTheClearChecksItsBoundaryAgainAfterAKill(t *testing.T) {
	for _, point := range []string{"clear checked", "before the first delete"} {
		t.Run(point, func(t *testing.T) {
			c := newRetainedCrossEdgeCity(t, true)
			killed := errors.New("killed at " + point)
			prevPoint, prevDelete := infraEngineCrashPoint, infraClearBeforeDelete
			t.Cleanup(func() { infraEngineCrashPoint, infraClearBeforeDelete = prevPoint, prevDelete })
			if point == "clear checked" {
				infraEngineCrashPoint = func(p string) error {
					if p == point {
						return killed
					}
					return nil
				}
			} else {
				infraClearBeforeDelete = func(string) error { return killed }
			}
			var log bytes.Buffer
			if first := migrateInfraClasses(t, c.cityPath, c.cfg, &log); first.Outcome != infraMigrationRetained || !strings.Contains(log.String(), killed.Error()) {
				t.Fatalf("first = %s: %s", first.Outcome, log.String())
			}
			infraEngineCrashPoint, infraClearBeforeDelete = prevPoint, prevDelete
			c.setInBinding(t, c.done.ID, "open")
			log.Reset()
			resumed := migrateInfraClasses(t, c.cityPath, c.cfg, &log)
			var refusal *infraClearBoundaryRefusal
			if resumed.Outcome != infraMigrationRetained || !errors.As(resumed.Fault, &refusal) {
				t.Fatalf("resume = %s (%v), want the boundary refused again: %s", resumed.Outcome, resumed.Fault, log.String())
			}
			if len(refusal.Refusals) != 1 || !slices.Equal(refusal.Refusals[0].IDs, []string{c.waitsOnDone.ID, c.done.ID}) {
				t.Fatalf("refusals = %+v", refusal.Refusals)
			}
			for _, id := range []string{c.done.ID, c.running.ID, c.more.ID} {
				if _, err := c.mem.Get(id); err != nil {
					t.Fatalf("the refused resume removed %s: %v", id, err)
				}
			}
			if deps, _ := c.mem.DepList(c.waitsOnDone.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != c.done.ID {
				t.Fatalf("%s deps = %+v; the edge the check refused must stay", c.waitsOnDone.ID, deps)
			}
			// Finished in the binding again, the same command completes.
			c.setInBinding(t, c.done.ID, "closed")
			log.Reset()
			if got := migrateInfraClasses(t, c.cityPath, c.cfg, &log); got.Outcome != infraMigrationConverged {
				t.Fatalf("final = %s: %s", got.Outcome, log.String())
			}
			c.assertClearedWithoutReAdding(t)
		})
	}
}
