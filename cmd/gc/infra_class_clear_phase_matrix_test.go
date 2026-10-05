package main

// Phase matrix for the retained-copy clear, contributed by the lane split151
// delta review (rel15/review-split151.md, A5): a kill at every store mutation
// of a clear, on file-like, cascading and Dolt-like stores. Every kill point
// must converge, settle cross-store edges, and keep every edge in the backup.

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// phaseStore is a MemStore that (optionally) cascades deletes onto every edge
// touching the row and (optionally) refuses an edge whose target it does not
// hold, like bd/native Dolt; and fails the k-th mutation, like a kill.
type phaseStore struct {
	*beads.MemStore
	cascade, refuseDangling bool
	failAt, n               int
}

var errKilled = errors.New("killed")

func (s *phaseStore) tick() error {
	s.n++
	if s.failAt > 0 && s.n == s.failAt {
		return errKilled
	}
	return nil
}

func (s *phaseStore) Delete(id string) error {
	if err := s.tick(); err != nil {
		return err
	}
	if s.cascade {
		for _, dir := range []string{"up", "down"} {
			deps, _ := s.DepList(id, dir)
			for _, d := range deps {
				_ = s.MemStore.DepRemove(d.IssueID, d.DependsOnID)
			}
		}
	}
	return s.MemStore.Delete(id)
}

func (s *phaseStore) DepRemove(a, b string) error {
	if err := s.tick(); err != nil {
		return err
	}
	return s.MemStore.DepRemove(a, b)
}

func (s *phaseStore) DepAdd(a, b, typ string) error {
	if err := s.tick(); err != nil {
		return err
	}
	if s.refuseDangling {
		if _, err := s.Get(b); err != nil {
			return fmt.Errorf("no issue found matching %q", b)
		}
	}
	return s.MemStore.DepAdd(a, b, typ)
}

type phaseFixture struct {
	root, s1, s2, done, sess, w1, w2, w3 beads.Bead
}

func seedPhaseFixture(t *testing.T, s beads.Store) phaseFixture {
	var f phaseFixture
	f.root = mustCreateInfraBead(t, s, beads.Bead{Title: "root", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	f.s1 = mustCreateInfraBead(t, s, beads.Bead{Title: "s1", Type: "task", Metadata: beads.StringMap{"gc.root_bead_id": f.root.ID}})
	f.s2 = mustCreateInfraBead(t, s, beads.Bead{Title: "s2", Type: "task", Metadata: beads.StringMap{"gc.root_bead_id": f.root.ID}})
	f.done = mustCreateInfraBead(t, s, beads.Bead{Title: "done graft", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	f.sess = mustCreateInfraBead(t, s, beads.Bead{Title: "sess", Type: "session", Labels: []string{"gc:session"}})
	f.w1 = mustCreateInfraBead(t, s, beads.Bead{Title: "w1 waits s2", Type: "task"})
	f.w2 = mustCreateInfraBead(t, s, beads.Bead{Title: "w2 waits done", Type: "task"})
	f.w3 = mustCreateInfraBead(t, s, beads.Bead{Title: "w3 related root", Type: "task"})
	for _, e := range []beads.Dep{
		{IssueID: f.s1.ID, DependsOnID: f.root.ID, Type: "parent-child"},
		{IssueID: f.s2.ID, DependsOnID: f.root.ID, Type: "parent-child"},
		{IssueID: f.s2.ID, DependsOnID: f.s1.ID, Type: "blocks"},
		{IssueID: f.w1.ID, DependsOnID: f.s2.ID, Type: "blocks"},
		{IssueID: f.w2.ID, DependsOnID: f.done.ID, Type: "blocks"},
		{IssueID: f.w3.ID, DependsOnID: f.root.ID, Type: "related"},
	} {
		if err := s.DepAdd(e.IssueID, e.DependsOnID, e.Type); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func runPhaseMatrix(t *testing.T, cascade, refuse bool) {
	for k := 1; k < 40; k++ {
		stop := false
		t.Run(fmt.Sprintf("kill@%d", k), func(t *testing.T) {
			stubInfraControllerPing(t, 0)
			ps := &phaseStore{MemStore: beads.NewMemStore(), cascade: cascade, refuseDangling: refuse}
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) { return ps, nil }
			t.Cleanup(func() { openInfraMigrationSource = prev })
			f := seedPhaseFixture(t, ps)
			cityPath := t.TempDir()
			cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
			var log bytes.Buffer
			if got := migrateInfraClassesRetainingSource(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
				t.Fatalf("cutover %s: %s", got.Outcome, log.String())
			}
			target := mustResolveInfraTarget(t, cityPath, cfg)
			b := openMigratedDestination(t, target)
			if err := b.Close(f.done.ID); err != nil {
				t.Fatal(err)
			}
			_ = closeBeadStoreHandle(b)

			ps.n, ps.failAt = 0, k
			first := migrateInfraClasses(t, cityPath, cfg, &log)
			fired := ps.n >= k
			ps.failAt = 0
			if !fired {
				stop = true
				if first.Outcome != infraMigrationConverged {
					t.Fatalf("unfaulted run = %s: %s", first.Outcome, log.String())
				}
			} else {
				if first.Outcome == infraMigrationConverged {
					// a fault swallowed into Unrestorable is acceptable only for restore adds
					t.Logf("fault at %d absorbed: lost=%v", k, first.LostCrossEdges)
				} else if got := checkInfraClassConvergence(cityPath, cfg, "gc start", &bytes.Buffer{}); got.serving() {
					t.Fatalf("boot SERVES after a kill at mutation %d (outcome %s)", k, got.Outcome)
				}
				log.Reset()
				if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
					t.Fatalf("resume = %s: %s", got.Outcome, log.String())
				}
			}
			if got := checkInfraClassConvergence(cityPath, cfg, "gc start", &bytes.Buffer{}); got.Outcome != infraMigrationConverged {
				t.Fatalf("boot after resume = %s", got.Outcome)
			}
			for _, id := range []string{f.root.ID, f.s1.ID, f.s2.ID, f.done.ID, f.sess.ID} {
				if _, err := ps.Get(id); err == nil {
					t.Errorf("%s still in work store", id)
				}
			}
			ready := reviewReadyIDs(t, ps.MemStore)
			if !slices.Contains(ready, f.w2.ID) {
				t.Errorf("w2 not ready though its blocker closed in binding")
			}
			note, _, err := readInfraClearedNote(cityPath)
			if err != nil || !note.Complete {
				t.Errorf("note incomplete: %+v %v", note, err)
			}
			w1Lost := false
			for _, e := range note.LostCrossEdges {
				if strings.HasPrefix(e, f.w1.ID+" ") {
					w1Lost = true
				}
			}
			if slices.Contains(ready, f.w1.ID) && !w1Lost {
				t.Errorf("w1 ready early with no lost_cross_edges record: %v", note.LostCrossEdges)
			}
			if refuse && !w1Lost {
				t.Errorf("refusing store but w1 edge not recorded lost: %v", note.LostCrossEdges)
			}
			// Backup integrity: every row and every edge.
			entries, _, err := readInfraRetainedBackup(target)
			if err != nil {
				t.Fatal(err)
			}
			edges := map[string]bool{}
			ids := map[string]bool{}
			for _, e := range entries {
				ids[e.Bead.ID] = true
				for _, d := range append(append([]infraBackupEdge{}, e.Deps...), e.Dependents...) {
					edges[d.IssueID+">"+d.DependsOnID] = true
				}
			}
			for _, id := range []string{f.root.ID, f.s1.ID, f.s2.ID, f.done.ID, f.sess.ID} {
				if !ids[id] {
					t.Errorf("backup lacks row %s", id)
				}
			}
			for _, e := range [][2]string{{f.s1.ID, f.root.ID}, {f.s2.ID, f.root.ID}, {f.s2.ID, f.s1.ID}, {f.w1.ID, f.s2.ID}, {f.w2.ID, f.done.ID}, {f.w3.ID, f.root.ID}} {
				if !edges[e[0]+">"+e[1]] {
					t.Errorf("backup LOST edge %s -> %s after kill@%d", e[0], e[1], k)
				}
			}
		})
		if stop {
			return
		}
	}
}

func TestReviewPhaseMatrixFileStore(t *testing.T) { runPhaseMatrix(t, false, false) }
func TestReviewPhaseMatrixCascading(t *testing.T) { runPhaseMatrix(t, true, false) }
func TestReviewPhaseMatrixDoltLike(t *testing.T)  { runPhaseMatrix(t, true, true) }
