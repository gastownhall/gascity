package beads

import (
	"context"
	"encoding/json"
	"strings"
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

	afterStaleDepListID string
	afterStaleDepList   func()
	depOnce             sync.Once
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

func (s *reconcileRaceStore) DepList(id, direction string) ([]Dep, error) {
	deps, err := s.Store.DepList(id, direction)
	if err == nil && id == s.afterStaleDepListID && s.afterStaleDepList != nil {
		s.depOnce.Do(s.afterStaleDepList)
	}
	return deps, err
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

func TestCachingStoreReconciliationPreservesConcurrentDependencyInvalidation(t *testing.T) {
	mem := NewMemStore()
	blocker, err := mem.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	target, err := mem.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatalf("Create(target): %v", err)
	}

	backing := &reconcileRaceStore{Store: mem}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.afterStaleDepListID = target.ID
	backing.afterStaleDepList = func() {
		if err := mem.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
			t.Errorf("DepAdd: %v", err)
			return
		}
		payload, err := json.Marshal(target)
		if err != nil {
			t.Errorf("Marshal: %v", err)
			return
		}
		cs.ApplyEvent("bead.updated", payload)
	}

	cs.runReconciliation()

	if ready, ok := cs.CachedReady(); ok {
		t.Fatalf("CachedReady answered from stale dependency cache after concurrent invalidation: %v", ready)
	}
	ready, err := cs.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, bead := range ready {
		if bead.ID == target.ID {
			t.Fatalf("Ready includes %s after backing dependency add; ready=%v", target.ID, ready)
		}
	}
}

func TestCachingStoreReconciliationSkipsReemitForAlreadyClosedBead(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "to be closed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events []string
	cs := NewCachingStoreForTest(mem, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cs.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wantClose := "bead.closed:" + bead.ID
	closeSeen := false
	for _, e := range events {
		if e == wantClose {
			closeSeen = true
			break
		}
	}
	if !closeSeen {
		t.Fatalf("events after Close = %v, want to include %q", events, wantClose)
	}
	events = nil

	cs.runReconciliation()

	for _, e := range events {
		if strings.HasPrefix(e, "bead.closed:") {
			t.Fatalf("reconciliation re-emitted close event: %v", events)
		}
	}

	cs.mu.RLock()
	_, stillCached := cs.beads[bead.ID]
	cs.mu.RUnlock()
	if stillCached {
		t.Fatalf("closed bead %s should be evicted from cache after reconcile", bead.ID)
	}
}

func TestCachingStoreReconciliationSkipsReemitForAlreadyClosedBeadWithConcurrentMutation(t *testing.T) {
	mem := NewMemStore()
	closedBead, err := mem.Create(Bead{Title: "closed before reconcile"})
	if err != nil {
		t.Fatalf("Create(closed): %v", err)
	}
	other, err := mem.Create(Bead{Title: "concurrent target"})
	if err != nil {
		t.Fatalf("Create(other): %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{other},
	}

	var events []string
	var eventsMu sync.Mutex
	cs := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, eventType+":"+beadID)
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cs.Close(closedBead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	eventsMu.Lock()
	events = nil
	eventsMu.Unlock()

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
	if err := cs.Update(other.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update(other): %v", err)
	}
	close(backing.release)
	<-done

	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, e := range events {
		if strings.HasPrefix(e, "bead.closed:") {
			t.Fatalf("reconciliation re-emitted close event in race path: %v", events)
		}
	}

	cs.mu.RLock()
	_, stillCached := cs.beads[closedBead.ID]
	cs.mu.RUnlock()
	if stillCached {
		t.Fatalf("closed bead %s should be evicted from cache after reconcile", closedBead.ID)
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
