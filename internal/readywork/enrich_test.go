package readywork

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fanout/fanouttest"
)

// depGatedStore parks every DepList on probe, so a synctest bubble can count
// how many rows' edges are being read at once.
type depGatedStore struct {
	*beads.MemStore
	probe *fanouttest.Probe
}

func (s depGatedStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.probe.Call(0)
	return s.MemStore.DepList(id, direction)
}

// inProgressWithBlockers seeds n in_progress rows, each with one closed and one
// open blocker, in that edge order.
func inProgressWithBlockers(t *testing.T, store *beads.MemStore, n int) {
	t.Helper()
	inProgress := StatusInProgress
	for i := range n {
		row := mustCreate(t, store, beads.Bead{Title: fmt.Sprintf("row %d", i), Type: "task"})
		if err := store.Update(row.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("claim %s: %v", row.ID, err)
		}
		closed := mustCreate(t, store, beads.Bead{Title: fmt.Sprintf("closed blocker %d", i), Type: "task"})
		if err := store.Close(closed.ID); err != nil {
			t.Fatalf("close %s: %v", closed.ID, err)
		}
		open := mustCreate(t, store, beads.Bead{Title: fmt.Sprintf("open blocker %d", i), Type: "task"})
		for _, blocker := range []string{closed.ID, open.ID} {
			if err := store.DepAdd(row.ID, blocker, "blocks"); err != nil {
				t.Fatalf("dep %s -> %s: %v", row.ID, blocker, err)
			}
		}
	}
}

// TestInProgressEnrichmentReadsRowsConcurrently pins ga-ylpbsy.3.4: the
// crash-recovery arm's blocked_by enrichment reads its rows' edges at once
// rather than one row after another, so an unbounded in_progress read costs the
// slowest row rather than the sum of them.
func TestInProgressEnrichmentReadsRowsConcurrently(t *testing.T) {
	const rows = 5
	store := beads.NewMemStore()
	inProgressWithBlockers(t, store, rows)
	synctest.Test(t, func(t *testing.T) {
		probe := fanouttest.NewProbe()
		defer probe.Open()
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err = Read([]Leg{testLeg("city", depGatedStore{MemStore: store, probe: probe})}, Query{Status: StatusInProgress})
		}()
		synctest.Wait()
		if got := probe.Running(); got != rows {
			t.Fatalf("%d of %d rows' edges were being read at once, want all of them", got, rows)
		}
		probe.Open()
		<-done
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	})
}

// TestInProgressEnrichmentKeepsEachRowsBlockersInEdgeOrder pins that the
// concurrent enrichment answers every row exactly as the per-row walk did: its
// blockers, each with its status, in the order the row's own edge read returns
// them.
func TestInProgressEnrichmentKeepsEachRowsBlockersInEdgeOrder(t *testing.T) {
	store := beads.NewMemStore()
	inProgressWithBlockers(t, store, 3)
	got, err := Read([]Leg{testLeg("city", store)}, Query{Status: StatusInProgress})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Read = %d rows, want 3", len(got))
	}
	for _, row := range got {
		deps, err := store.DepList(row.ID, "down")
		if err != nil {
			t.Fatalf("DepList: %v", err)
		}
		var want []ReadyBeadBlocker
		for _, dep := range deps {
			blocker, err := store.Get(dep.DependsOnID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			want = append(want, ReadyBeadBlocker{ID: blocker.ID, Status: blocker.Status})
		}
		if !reflect.DeepEqual(row.BlockedBy, want) {
			t.Fatalf("row %s blocked_by = %+v, want %+v", row.ID, row.BlockedBy, want)
		}
	}
}

// cancellingListStore ends its read's context as its list returns, and counts
// every dependency and blocker read made through it.
type cancellingListStore struct {
	*beads.MemStore
	cancel context.CancelFunc
	reads  *atomic.Int64
}

func (s cancellingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	defer s.cancel()
	return s.MemStore.List(query)
}

func (s cancellingListStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.reads.Add(1)
	return s.MemStore.DepList(id, direction)
}

func (s cancellingListStore) Get(id string) (beads.Bead, error) {
	s.reads.Add(1)
	return s.MemStore.Get(id)
}

// TestInProgressEnrichmentIsNotReadOnceTheContextEnds pins cancellation between
// the phases: a context that ends while the candidates are being read stops the
// read before any row's blockers are, and the answer is the context's error.
func TestInProgressEnrichmentIsNotReadOnceTheContextEnds(t *testing.T) {
	store := beads.NewMemStore()
	inProgressWithBlockers(t, store, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reads atomic.Int64
	leg := testLeg("city", cancellingListStore{MemStore: store, cancel: cancel, reads: &reads})
	rows, err := ReadContext(ctx, []Leg{leg}, Query{Status: StatusInProgress})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadContext = %v, %v; want the context's error once it ended", rows, err)
	}
	if got := reads.Load(); got != 0 {
		t.Fatalf("%d dependency or blocker reads after the context ended, want none", got)
	}
}

// failingBlockerStore fails Get for one blocker id.
type failingBlockerStore struct {
	*beads.MemStore
	failGet string
}

func (s failingBlockerStore) Get(id string) (beads.Bead, error) {
	if id == s.failGet {
		return beads.Bead{}, errors.New("blocker read refused")
	}
	return s.MemStore.Get(id)
}

// TestInProgressEnrichmentFailureNamesTheRowAndBlocker pins fail-loud: a blocker
// the serving leg cannot resolve fails the answer, naming the leg, the blocker
// and the row, exactly as the per-row walk named them.
func TestInProgressEnrichmentFailureNamesTheRowAndBlocker(t *testing.T) {
	store := beads.NewMemStore()
	inProgressWithBlockers(t, store, 2)
	rows, err := Read([]Leg{testLeg("city", store)}, Query{Status: StatusInProgress})
	if err != nil || len(rows) == 0 || len(rows[0].BlockedBy) == 0 {
		t.Fatalf("fixture read = %+v, %v", rows, err)
	}
	failing := rows[0].BlockedBy[0].ID
	_, err = Read([]Leg{testLeg("city", failingBlockerStore{MemStore: store, failGet: failing})}, Query{Status: StatusInProgress})
	if err == nil {
		t.Fatal("an unresolvable blocker produced an answer; a gated bead must not read as unblocked")
	}
	want := fmt.Sprintf("city store: resolving blocker %q of %q", failing, rows[0].ID)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}
