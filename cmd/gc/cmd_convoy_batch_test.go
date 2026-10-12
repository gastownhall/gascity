package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// batchingConvoyStore gives a MemStore the batched dependent read the bd store
// answers with one bd sql statement, and counts those reads.
type batchingConvoyStore struct {
	*beads.MemStore
	dependentBatches int
}

func (s *batchingConvoyStore) DependentIDsBatch(ids []string, depType string) ([]string, error) {
	s.dependentBatches++
	var out []string
	for _, id := range ids {
		deps, err := s.DepList(id, "up")
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if d.Type == depType {
				out = append(out, d.IssueID)
			}
		}
		if depType == "parent-child" {
			children, err := s.List(beads.ListQuery{ParentID: id, IncludeClosed: true})
			if err != nil {
				return nil, err
			}
			for _, c := range children {
				out = append(out, c.ID)
			}
		}
	}
	return out, nil
}

func seedConvoyListFixture(t *testing.T, store *beads.MemStore) {
	t.Helper()
	_, _ = store.Create(beads.Bead{Title: "legacy", Type: "convoy"})   // gc-1
	_, _ = store.Create(beads.Bead{Title: "child", ParentID: "gc-1"})  // gc-2
	_, _ = store.Create(beads.Bead{Title: "tracking", Type: "convoy"}) // gc-3
	_, _ = store.Create(beads.Bead{Title: "open"})                     // gc-4
	_, _ = store.Create(beads.Bead{Title: "done"})                     // gc-5
	_, _ = store.Create(beads.Bead{Title: "empty", Type: "convoy"})    // gc-6
	requireNoError(t, store.DepAdd("gc-3", "gc-4", "tracks"))
	requireNoError(t, store.DepAdd("gc-3", "gc-5", "tracks"))
	requireNoError(t, store.DepAdd("gc-6", "gc-missing", "tracks"))
	requireNoError(t, store.Close("gc-5"))
}

func TestConvoyListBatchedReadMatchesPerConvoyRead(t *testing.T) {
	perConvoy := beads.NewMemStore()
	seedConvoyListFixture(t, perConvoy)
	batched := &batchingConvoyStore{MemStore: beads.NewMemStore()}
	seedConvoyListFixture(t, batched.MemStore)

	run := func(store beads.Store) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := doConvoyListAcrossStores([]convoyStoreView{{store: store}}, true, &stdout, &stderr); code != 0 {
			t.Fatalf("doConvoyListAcrossStores = %d; stderr: %s", code, stderr.String())
		}
		return stdout.String()
	}
	want := run(perConvoy)
	got := run(batched)
	if got != want {
		t.Errorf("batched list differs from the per-convoy list:\n got: %s\nwant: %s", got, want)
	}
	if batched.dependentBatches != 1 {
		t.Errorf("dependent batches = %d, want 1", batched.dependentBatches)
	}
	for _, fragment := range []string{`"child_ids":["gc-2"]`, `"child_ids":["gc-4","gc-5"]`, `"dangling_tracks":1`} {
		if !strings.Contains(got, fragment) {
			t.Errorf("list JSON missing %s:\n%s", fragment, got)
		}
	}
}

// A convoy that tracks another convoy closes in the same pass when the inner
// convoy closes first: the old loop read each convoy's children after the
// earlier closes, and the batched read must answer the same.
func TestConvoyCheckClosesAConvoyWhoseConvoyClosedEarlierInThePass(t *testing.T) {
	store := &batchingConvoyStore{MemStore: beads.NewMemStore()}
	_, _ = store.Create(beads.Bead{Title: "inner", Type: "convoy"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task"})                  // gc-2
	_, _ = store.Create(beads.Bead{Title: "outer", Type: "convoy"}) // gc-3
	requireNoError(t, store.DepAdd("gc-1", "gc-2", "tracks"))
	requireNoError(t, store.DepAdd("gc-3", "gc-1", "tracks"))
	requireNoError(t, store.Close("gc-2"))

	var stdout, stderr bytes.Buffer
	if code := doConvoyCheck(store, events.Discard, &stdout, &stderr); code != 0 {
		t.Fatalf("doConvoyCheck = %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "2 convoy(s) auto-closed") {
		t.Errorf("want both convoys closed in one pass:\n%s", stdout.String())
	}
	for _, id := range []string{"gc-1", "gc-3"} {
		b, err := store.Get(id)
		requireNoError(t, err)
		if b.Status != "closed" {
			t.Errorf("%s status = %q, want closed", id, b.Status)
		}
	}
}
