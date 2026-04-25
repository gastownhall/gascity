package beads

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

type reconcileRaceStore struct {
	Store
	started chan struct{}
	release chan struct{}
	stale   []Bead

	mu    sync.Mutex
	block bool
	once  sync.Once
}

func (s *reconcileRaceStore) List(query ListQuery) ([]Bead, error) {
	if !query.AllowScan {
		return s.Store.List(query)
	}

	s.mu.Lock()
	block := s.block
	s.mu.Unlock()
	if !block {
		return s.Store.List(query)
	}

	s.once.Do(func() {
		close(s.started)
	})
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Bead(nil), s.stale...), nil
}

func TestCachingStoreReconciliationPreservesConcurrentMutation(t *testing.T) {
	mem := NewMemStore()
	original, err := mem.Create(Bead{Title: "before reconcile"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{original},
	}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	title := "after concurrent update"
	if err := cs.Update(original.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	close(backing.release)
	<-done

	items, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 1 || items[0].Title != title {
		t.Fatalf("ListOpen = %#v, want updated title %q", items, title)
	}
}

func TestCachingStoreReconciliationPreservesConcurrentEvent(t *testing.T) {
	mem := NewMemStore()
	original, err := mem.Create(Bead{Title: "before reconcile"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{original},
	}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	eventBead := cloneBead(original)
	eventBead.Title = "after concurrent event"
	payload, err := json.Marshal(eventBead)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	cs.ApplyEvent("bead.updated", payload)
	close(backing.release)
	<-done

	items, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 1 || items[0].Title != eventBead.Title {
		t.Fatalf("ListOpen = %#v, want event title %q", items, eventBead.Title)
	}
}

func TestCachingStoreReconciliationMergesFreshDataWithConcurrentMutation(t *testing.T) {
	mem := NewMemStore()
	mutated, err := mem.Create(Bead{Title: "before mutate"})
	if err != nil {
		t.Fatalf("Create(mutated): %v", err)
	}
	refreshed, err := mem.Create(Bead{Title: "before refresh"})
	if err != nil {
		t.Fatalf("Create(refreshed): %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{mutated, refreshed},
	}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	title := "after concurrent update"
	if err := cs.Update(mutated.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update(mutated): %v", err)
	}
	refreshedTitle := "after reconcile refresh"
	if err := mem.Update(refreshed.ID, UpdateOpts{Title: &refreshedTitle}); err != nil {
		t.Fatalf("Update(refreshed backing): %v", err)
	}
	refreshedBead, err := mem.Get(refreshed.ID)
	if err != nil {
		t.Fatalf("Get(refreshed backing): %v", err)
	}
	backing.mu.Lock()
	backing.stale = []Bead{
		cloneBead(mutated),
		cloneBead(refreshedBead),
	}
	backing.mu.Unlock()
	close(backing.release)
	<-done

	items, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	gotTitles := map[string]string{}
	for _, item := range items {
		gotTitles[item.ID] = item.Title
	}
	if gotTitles[mutated.ID] != title {
		t.Fatalf("mutated title = %q, want %q", gotTitles[mutated.ID], title)
	}
	if gotTitles[refreshed.ID] != refreshedTitle {
		t.Fatalf("refreshed title = %q, want %q", gotTitles[refreshed.ID], refreshedTitle)
	}
}
