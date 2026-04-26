package beads_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCachingStoreReadThrough(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	b1, _ := mem.Create(beads.Bead{Title: "Task 1"})
	b2, _ := mem.Create(beads.Bead{Title: "Task 2"})
	if err := mem.DepAdd(b2.ID, b1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if !cs.IsLive() {
		t.Fatal("should be live after prime")
	}

	// List
	list, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}

	// Get
	got, err := cs.Get(b1.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "Task 1" {
		t.Fatalf("title = %q, want Task 1", got.Title)
	}

	// DepList
	deps, err := cs.DepList(b2.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != b1.ID {
		t.Fatalf("deps = %v, want 1 dep on %s", deps, b1.ID)
	}

	// Ready (b1 has no deps, b2 is blocked)
	ready, err := cs.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != b1.ID {
		t.Fatalf("Ready = %v, want only %s", ready, b1.ID)
	}
}

func TestCachingStorePrimePreservesConcurrentUpdate(t *testing.T) {
	mem := beads.NewMemStore()
	original, err := mem.Create(beads.Bead{Title: "before prime"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	backing := &primeRaceStore{
		Store:   mem,
		started: started,
		release: release,
		stale:   []beads.Bead{original},
	}
	cs := beads.NewCachingStoreForTest(backing, nil)

	done := make(chan error, 1)
	go func() {
		done <- cs.Prime(context.Background())
	}()

	<-started
	title := "after update"
	if err := cs.Update(original.ID, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Prime: %v", err)
	}

	got, err := cs.Get(original.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != title {
		t.Fatalf("title after concurrent prime = %q, want %q", got.Title, title)
	}
}

func TestCachingStorePrimeDoesNotResurrectConcurrentDelete(t *testing.T) {
	mem := beads.NewMemStore()
	original, err := mem.Create(beads.Bead{Title: "before delete"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	backing := &primeRaceStore{
		Store:   mem,
		started: started,
		release: release,
		stale:   []beads.Bead{original},
	}
	cs := beads.NewCachingStoreForTest(backing, nil)

	done := make(chan error, 1)
	go func() {
		done <- cs.Prime(context.Background())
	}()

	<-started
	if err := cs.Delete(original.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Prime: %v", err)
	}

	got, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListOpen = %#v, want deleted bead to stay absent", got)
	}
}

func TestCachingStoreCreateRefreshesSparseBead(t *testing.T) {
	backing := &sparseCreateStore{Store: beads.NewMemStore()}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	created, err := cs.Create(beads.Bead{
		Title:    "new task",
		ParentID: "parent-1",
		Labels:   []string{"urgent"},
		Metadata: map[string]string{"k": "v"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ParentID != "parent-1" {
		t.Fatalf("ParentID = %q, want parent-1", created.ParentID)
	}
	if len(created.Labels) != 1 || created.Labels[0] != "urgent" {
		t.Fatalf("Labels = %#v, want [urgent]", created.Labels)
	}
	if created.Metadata["k"] != "v" {
		t.Fatalf("Metadata = %#v, want k=v", created.Metadata)
	}
}

func TestCachingStoreParentListUsesBackingStore(t *testing.T) {
	mem := beads.NewMemStore()
	parent, err := mem.Create(beads.Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	child, err := mem.Create(beads.Bead{Title: "child", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	got, err := cs.List(beads.ListQuery{ParentID: parent.ID})
	if err != nil {
		t.Fatalf("List(parent): %v", err)
	}
	if len(got) != 1 || got[0].ID != child.ID {
		t.Fatalf("children = %#v, want child %s", got, child.ID)
	}
}

func TestCachingStoreParentListRefreshesCachedChildren(t *testing.T) {
	mem := beads.NewMemStore()
	parent, err := mem.Create(beads.Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	child, err := mem.Create(beads.Bead{Title: "child", Labels: []string{"mc-live-contract"}})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := mem.Update(child.ID, beads.UpdateOpts{ParentID: &parent.ID}); err != nil {
		t.Fatalf("backing Update(parent): %v", err)
	}

	children, err := cs.List(beads.ListQuery{ParentID: parent.ID})
	if err != nil {
		t.Fatalf("List(parent): %v", err)
	}
	if len(children) != 1 || children[0].ParentID != parent.ID {
		t.Fatalf("children = %#v, want refreshed parent %s", children, parent.ID)
	}

	labeled, err := cs.List(beads.ListQuery{Label: "mc-live-contract"})
	if err != nil {
		t.Fatalf("List(label): %v", err)
	}
	if len(labeled) != 1 || labeled[0].ParentID != parent.ID {
		t.Fatalf("cached label result = %#v, want parent %s", labeled, parent.ID)
	}
}

func TestCachingStoreParentListRefreshesReparentedChildren(t *testing.T) {
	mem := beads.NewMemStore()
	oldParent, err := mem.Create(beads.Bead{Title: "old-parent"})
	if err != nil {
		t.Fatalf("Create(old parent): %v", err)
	}
	newParent, err := mem.Create(beads.Bead{Title: "new-parent"})
	if err != nil {
		t.Fatalf("Create(new parent): %v", err)
	}
	child, err := mem.Create(beads.Bead{Title: "child", ParentID: oldParent.ID, Labels: []string{"mc-live-contract"}})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := mem.Update(child.ID, beads.UpdateOpts{ParentID: &newParent.ID}); err != nil {
		t.Fatalf("backing Update(parent): %v", err)
	}

	children, err := cs.List(beads.ListQuery{ParentID: oldParent.ID})
	if err != nil {
		t.Fatalf("List(old parent): %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("old parent children = %#v, want empty after reparent", children)
	}

	labeled, err := cs.List(beads.ListQuery{Label: "mc-live-contract"})
	if err != nil {
		t.Fatalf("List(label): %v", err)
	}
	if len(labeled) != 1 || labeled[0].ParentID != newParent.ID {
		t.Fatalf("cached label result = %#v, want parent %s", labeled, newParent.ID)
	}
}

func TestCachingStoreParentListPreservesConcurrentUpdate(t *testing.T) {
	mem := beads.NewMemStore()
	parent, err := mem.Create(beads.Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	child, err := mem.Create(beads.Bead{Title: "before", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	backing := &parentListRaceStore{
		Store:    mem,
		parentID: parent.ID,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		stale:    []beads.Bead{child},
	}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	childrenCh := make(chan []beads.Bead, 1)
	errCh := make(chan error, 1)
	go func() {
		children, listErr := cs.List(beads.ListQuery{ParentID: parent.ID})
		if listErr != nil {
			errCh <- listErr
			return
		}
		childrenCh <- children
	}()

	<-backing.started
	title := "after concurrent update"
	if err := cs.Update(child.ID, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	close(backing.release)

	select {
	case listErr := <-errCh:
		t.Fatalf("List(parent): %v", listErr)
	case children := <-childrenCh:
		if len(children) != 1 || children[0].Title != title {
			t.Fatalf("List(parent) = %#v, want updated title %q", children, title)
		}
	}

	got, err := cs.Get(child.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != title {
		t.Fatalf("Get title = %q, want %q", got.Title, title)
	}
}

func TestCachingStoreParentListDoesNotResurrectConcurrentDelete(t *testing.T) {
	mem := beads.NewMemStore()
	parent, err := mem.Create(beads.Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	child, err := mem.Create(beads.Bead{Title: "before", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	backing := &parentListRaceStore{
		Store:    mem,
		parentID: parent.ID,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		stale:    []beads.Bead{child},
	}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	childrenCh := make(chan []beads.Bead, 1)
	errCh := make(chan error, 1)
	go func() {
		children, listErr := cs.List(beads.ListQuery{ParentID: parent.ID})
		if listErr != nil {
			errCh <- listErr
			return
		}
		childrenCh <- children
	}()

	<-backing.started
	if err := cs.Delete(child.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	close(backing.release)

	select {
	case listErr := <-errCh:
		t.Fatalf("List(parent): %v", listErr)
	case children := <-childrenCh:
		if len(children) != 0 {
			t.Fatalf("List(parent) = %#v, want deleted child omitted", children)
		}
	}

	if _, err := cs.Get(child.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get after delete error = %v, want ErrNotFound", err)
	}
}

func TestCachingStoreDirtyGetPreservesConcurrentEvent(t *testing.T) {
	mem := beads.NewMemStore()
	original, err := mem.Create(beads.Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &dirtyGetRaceStore{Store: mem}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	firstTitle := "after first update"
	backing.mu.Lock()
	backing.failNextGet = true
	backing.mu.Unlock()
	if err := cs.Update(original.ID, beads.UpdateOpts{Title: &firstTitle}); err != nil {
		t.Fatalf("Update(first): %v", err)
	}

	stale, err := mem.Get(original.ID)
	if err != nil {
		t.Fatalf("Get(backing stale): %v", err)
	}
	backing.mu.Lock()
	backing.stale = stale
	backing.started = make(chan struct{})
	backing.release = make(chan struct{})
	backing.blockNextGet = true
	backing.mu.Unlock()

	gotCh := make(chan beads.Bead, 1)
	errCh := make(chan error, 1)
	go func() {
		got, getErr := cs.Get(original.ID)
		if getErr != nil {
			errCh <- getErr
			return
		}
		gotCh <- got
	}()

	<-backing.started
	secondTitle := "after concurrent event"
	if err := mem.Update(original.ID, beads.UpdateOpts{Title: &secondTitle}); err != nil {
		t.Fatalf("Update(backing second): %v", err)
	}
	eventBead, err := mem.Get(original.ID)
	if err != nil {
		t.Fatalf("Get(backing second): %v", err)
	}
	payload, err := json.Marshal(eventBead)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	cs.ApplyEvent("bead.updated", payload)
	close(backing.release)

	select {
	case getErr := <-errCh:
		t.Fatalf("Get: %v", getErr)
	case got := <-gotCh:
		if got.Title != secondTitle {
			t.Fatalf("Get title = %q, want %q", got.Title, secondTitle)
		}
	}

	got, err := cs.Get(original.ID)
	if err != nil {
		t.Fatalf("Get cached: %v", err)
	}
	if got.Title != secondTitle {
		t.Fatalf("cached title = %q, want %q", got.Title, secondTitle)
	}
}

func TestCachingStoreUpdateReflectsWriteIntentWhenImmediateReadIsStale(t *testing.T) {
	mem := beads.NewMemStore()
	original, err := mem.Create(beads.Bead{
		Title:    "root",
		Labels:   []string{"root", "needs-update"},
		Metadata: map[string]string{"mc.contract.run_id": "r1"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &staleReadAfterUpdateStore{
		Store: mem,
		stale: original,
	}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	status := "in_progress"
	if err := cs.Update(original.ID, beads.UpdateOpts{
		Status:       &status,
		Labels:       []string{"verified"},
		RemoveLabels: []string{"needs-update"},
		Metadata: map[string]string{
			"mc.contract.metadata_update": "true",
		},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := cs.Get(original.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", got.Status)
	}
	if got.Metadata["mc.contract.metadata_update"] != "true" || got.Metadata["mc.contract.run_id"] != "r1" {
		t.Fatalf("metadata = %#v, want original plus update", got.Metadata)
	}
	if !containsString(got.Labels, "verified") || containsString(got.Labels, "needs-update") {
		t.Fatalf("labels = %#v, want verified without needs-update", got.Labels)
	}
}

func TestCachingStoreUpdateDoesNotDuplicateAuthoritativeLabels(t *testing.T) {
	mem := beads.NewMemStore()
	original, err := mem.Create(beads.Bead{
		Title:  "root",
		Labels: []string{"root"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cs.Update(original.ID, beads.UpdateOpts{Labels: []string{"verified"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListOpen returned %d beads, want 1", len(got))
	}
	verifiedCount := 0
	for _, label := range got[0].Labels {
		if label == "verified" {
			verifiedCount++
		}
	}
	if verifiedCount != 1 {
		t.Fatalf("labels = %#v, want exactly one verified label", got[0].Labels)
	}
}

type staleReadAfterUpdateStore struct {
	beads.Store
	mu        sync.Mutex
	stale     beads.Bead
	returnOld bool
}

type sparseCreateStore struct {
	beads.Store
}

func (s *sparseCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err != nil {
		return beads.Bead{}, err
	}
	return beads.Bead{ID: created.ID, Title: created.Title}, nil
}

func (s *staleReadAfterUpdateStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.Store.Update(id, opts); err != nil {
		return err
	}
	s.mu.Lock()
	s.returnOld = true
	s.mu.Unlock()
	return nil
}

func (s *staleReadAfterUpdateStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	if s.returnOld && id == s.stale.ID {
		s.returnOld = false
		stale := s.stale
		s.mu.Unlock()
		return stale, nil
	}
	s.mu.Unlock()
	return s.Store.Get(id)
}

type primeRaceStore struct {
	beads.Store
	started chan struct{}
	release chan struct{}
	stale   []beads.Bead
	once    sync.Once
}

func (s *primeRaceStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if !query.AllowScan {
		return s.Store.List(query)
	}
	s.once.Do(func() {
		close(s.started)
	})
	<-s.release
	return append([]beads.Bead(nil), s.stale...), nil
}

type parentListRaceStore struct {
	beads.Store
	parentID string
	started  chan struct{}
	release  chan struct{}
	stale    []beads.Bead
	once     sync.Once
}

func (s *parentListRaceStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.ParentID != s.parentID {
		return s.Store.List(query)
	}
	s.once.Do(func() {
		close(s.started)
	})
	<-s.release
	return append([]beads.Bead(nil), s.stale...), nil
}

type dirtyGetRaceStore struct {
	beads.Store
	mu           sync.Mutex
	failNextGet  bool
	blockNextGet bool
	started      chan struct{}
	release      chan struct{}
	stale        beads.Bead
}

func (s *dirtyGetRaceStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	switch {
	case s.failNextGet:
		s.failNextGet = false
		s.mu.Unlock()
		return beads.Bead{}, errors.New("transient get failure")
	case s.blockNextGet && id == s.stale.ID:
		s.blockNextGet = false
		started := s.started
		release := s.release
		stale := s.stale
		s.mu.Unlock()
		close(started)
		<-release
		return stale, nil
	default:
		s.mu.Unlock()
		return s.Store.Get(id)
	}
}

func TestCachingStoreGetFallsBackForClosedBeadsAfterPrime(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	closed, err := mem.Create(beads.Bead{Title: "Closed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mem.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if items, err := cs.ListOpen(); err != nil {
		t.Fatalf("ListOpen: %v", err)
	} else if len(items) != 0 {
		t.Fatalf("ListOpen len = %d, want 0 active items", len(items))
	}

	got, err := cs.Get(closed.ID)
	if err != nil {
		t.Fatalf("Get(closed): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

type countingGetStore struct {
	beads.Store
	mu   sync.Mutex
	gets int
}

func (s *countingGetStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.Get(id)
}

func (s *countingGetStore) resetGets() {
	s.mu.Lock()
	s.gets = 0
	s.mu.Unlock()
}

func (s *countingGetStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func TestCachingStoreReadyTreatsMissingDepTargetAsClosedWithoutBackingGet(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	blocker, err := mem.Create(beads.Bead{Title: "Closed blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	if err := mem.Close(blocker.ID); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}
	ready, err := mem.Create(beads.Bead{Title: "Ready after closed blocker"})
	if err != nil {
		t.Fatalf("Create(ready): %v", err)
	}
	if err := mem.DepAdd(ready.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	backing := &countingGetStore{Store: mem}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.resetGets()

	got, err := cs.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 1 || got[0].ID != ready.ID {
		t.Fatalf("Ready() = %v, want only %s", got, ready.ID)
	}
	if gets := backing.getCount(); gets != 0 {
		t.Fatalf("Ready() performed %d backing Get calls, want 0", gets)
	}
}

func TestCachingStoreListPartialAllowScanReturnsCompleteActiveSnapshot(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	openBead, err := mem.Create(beads.Bead{Title: "Open"})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	inProgress, err := mem.Create(beads.Bead{Title: "In progress", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(in_progress): %v", err)
	}
	closed, err := mem.Create(beads.Bead{Title: "Closed"})
	if err != nil {
		t.Fatalf("Create(closed): %v", err)
	}
	if err := mem.Close(closed.ID); err != nil {
		t.Fatalf("Close(closed): %v", err)
	}

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if cs.IsLive() {
		t.Fatal("cache should remain partial after PrimeActive")
	}

	got, err := cs.List(beads.ListQuery{AllowScan: true, Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatalf("List(AllowScan): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(AllowScan) len = %d, want 2 active beads", len(got))
	}
	if got[0].ID != openBead.ID || got[1].ID != inProgress.ID {
		t.Fatalf("List(AllowScan) IDs = [%s %s], want [%s %s]", got[0].ID, got[1].ID, openBead.ID, inProgress.ID)
	}
}

func TestCachingStoreListPartialMetadataMatchesActiveBeads(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	openBead, err := mem.Create(beads.Bead{Title: "Open"})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	if err := mem.SetMetadata(openBead.ID, "gc.kind", "workflow"); err != nil {
		t.Fatalf("SetMetadata(open): %v", err)
	}
	inProgress, err := mem.Create(beads.Bead{Title: "In progress", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(in_progress): %v", err)
	}
	if err := mem.SetMetadata(inProgress.ID, "gc.kind", "workflow"); err != nil {
		t.Fatalf("SetMetadata(in_progress): %v", err)
	}
	closed, err := mem.Create(beads.Bead{Title: "Closed"})
	if err != nil {
		t.Fatalf("Create(closed): %v", err)
	}
	if err := mem.SetMetadata(closed.ID, "gc.kind", "workflow"); err != nil {
		t.Fatalf("SetMetadata(closed): %v", err)
	}
	if err := mem.Close(closed.ID); err != nil {
		t.Fatalf("Close(closed): %v", err)
	}

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	got, err := cs.List(beads.ListQuery{
		Metadata: map[string]string{"gc.kind": "workflow"},
		Sort:     beads.SortCreatedAsc,
	})
	if err != nil {
		t.Fatalf("List(metadata): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(metadata) len = %d, want 2 active workflow beads", len(got))
	}
	if got[0].ID != openBead.ID || got[1].ID != inProgress.ID {
		t.Fatalf("List(metadata) IDs = [%s %s], want [%s %s]", got[0].ID, got[1].ID, openBead.ID, inProgress.ID)
	}
}

func TestCachingStoreWriteThrough(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Create through caching store
	b, err := cs.Create(beads.Bead{Title: "New"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Should be in cache
	got, err := cs.Get(b.ID)
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if got.Title != "New" {
		t.Fatalf("title = %q, want New", got.Title)
	}

	// Update
	if err := cs.Update(b.ID, beads.UpdateOpts{Title: strPtr("Updated")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = cs.Get(b.ID)
	if got.Title != "Updated" {
		t.Fatalf("title after update = %q, want Updated", got.Title)
	}

	// Close
	if err := cs.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, _ = cs.Get(b.ID)
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

func TestCachingStoreCloseNotifiesWhenBeadIsMissingFromCache(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	created, err := mem.Create(beads.Bead{Title: "external"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events []string
	cs = beads.NewCachingStoreForTest(mem, func(eventType, beadID string, payload json.RawMessage) {
		var b beads.Bead
		if err := json.Unmarshal(payload, &b); err != nil {
			t.Fatalf("unmarshal callback payload: %v", err)
		}
		events = append(events, eventType+":"+beadID+":"+b.Status)
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime after callback install: %v", err)
	}
	if err := cs.Delete(created.ID); err != nil {
		t.Fatalf("Delete setup: %v", err)
	}
	created, err = mem.Create(beads.Bead{Title: "external"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	if err := cs.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(events) != 1 || events[0] != "bead.closed:"+created.ID+":closed" {
		t.Fatalf("events = %#v, want bead.closed for missing cached bead", events)
	}
}

func TestCachingStoreListByLabelSeesCreatedBeadAfterMetadataWrite(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		prime func(*beads.CachingStore) error
	}{
		{
			name:  "partial",
			prime: func(cs *beads.CachingStore) error { return cs.PrimeActive() },
		},
		{
			name:  "live",
			prime: func(cs *beads.CachingStore) error { return cs.Prime(context.Background()) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mem := beads.NewMemStore()
			cs := beads.NewCachingStoreForTest(mem, nil)
			if err := tc.prime(cs); err != nil {
				t.Fatalf("prime: %v", err)
			}

			created, err := cs.Create(beads.Bead{
				Title:  "worker",
				Type:   "session",
				Labels: []string{"gc.session", "agent:worker"},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := cs.SetMetadata(created.ID, "session_name", "s-"+created.ID); err != nil {
				t.Fatalf("SetMetadata(session_name): %v", err)
			}

			got, err := cs.List(beads.ListQuery{
				Label: "gc.session",
				Sort:  beads.SortCreatedAsc,
			})
			if err != nil {
				t.Fatalf("List(label): %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("List(label) len = %d, want 1", len(got))
			}
			if got[0].ID != created.ID {
				t.Fatalf("List(label) ID = %q, want %q", got[0].ID, created.ID)
			}
			if got[0].Metadata["session_name"] != "s-"+created.ID {
				t.Fatalf("session_name = %q, want %q", got[0].Metadata["session_name"], "s-"+created.ID)
			}
		})
	}
}

func TestCachingStoreApplyEvent(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	b1, _ := mem.Create(beads.Bead{Title: "Existing"})

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Apply a create event for a bead that doesn't exist in cache yet.
	newBead := beads.Bead{ID: "ext-1", Title: "External", Status: "open"}
	payload, _ := json.Marshal(newBead)
	cs.ApplyEvent("bead.created", payload)

	got := requireCachedBead(t, cs, "ext-1", false)
	if got.Title != "External" {
		t.Fatalf("title = %q, want External", got.Title)
	}

	// Apply an update event.
	updatedTitle := "Modified by agent"
	if err := mem.Update(b1.ID, beads.UpdateOpts{
		Title:    &updatedTitle,
		Metadata: map[string]string{"gc.step_ref": "mol.review"},
	}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}
	updated := beads.Bead{ID: b1.ID, Title: updatedTitle, Status: "open", Metadata: map[string]string{"gc.step_ref": "mol.review"}}
	payload, _ = json.Marshal(updated)
	cs.ApplyEvent("bead.updated", payload)

	got = requireCachedBead(t, cs, b1.ID, false)
	if got.Title != "Modified by agent" {
		t.Fatalf("title after update event = %q, want Modified by agent", got.Title)
	}
	if got.Metadata["gc.step_ref"] != "mol.review" {
		t.Fatalf("metadata after update = %v, want gc.step_ref=mol.review", got.Metadata)
	}

	// Apply a close event with the full closed bead payload.
	closedTitle := "Closed by agent"
	if err := mem.Update(b1.ID, beads.UpdateOpts{
		Title:    &closedTitle,
		Labels:   []string{"done"},
		Metadata: map[string]string{"gc.outcome": "pass"},
	}); err != nil {
		t.Fatalf("Update backing before close: %v", err)
	}
	if err := mem.Close(b1.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}
	closed := beads.Bead{
		ID:       b1.ID,
		Title:    closedTitle,
		Status:   "closed",
		Labels:   []string{"done"},
		Metadata: map[string]string{"gc.outcome": "pass"},
	}
	payload, _ = json.Marshal(closed)
	cs.ApplyEvent("bead.closed", payload)

	got = requireCachedBead(t, cs, b1.ID, true)
	if got.Status != "closed" {
		t.Fatalf("status after close event = %q, want closed", got.Status)
	}
	if got.Title != "Closed by agent" {
		t.Fatalf("title after close event = %q, want Closed by agent", got.Title)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "done" {
		t.Fatalf("labels after close event = %v, want [done]", got.Labels)
	}
	if got.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("outcome = %q, want pass", got.Metadata["gc.outcome"])
	}
	if got.Metadata["gc.step_ref"] != "" {
		t.Fatalf("close event should replace stale metadata, got %v", got.Metadata)
	}
}

func TestCachingStoreApplyEventRefreshesPartialHookPayload(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	parent, err := mem.Create(beads.Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := mem.Create(beads.Bead{
		Title:    "child",
		ParentID: parent.ID,
		Labels:   []string{"mc-live-contract"},
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}

	backing := &eventGetFailStore{Store: mem}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.failGet = true

	updatedTitle := "child updated externally"
	if err := mem.Update(child.ID, beads.UpdateOpts{Title: &updatedTitle}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"id":         child.ID,
		"title":      updatedTitle,
		"status":     "open",
		"issue_type": "task",
		"owner":      "agent@example.com",
		"updated_at": "2026-04-25T04:45:55Z",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	cs.ApplyEvent("bead.updated", payload)
	if stats := cs.Stats(); stats.ProblemCount != 0 {
		t.Fatalf("ProblemCount = %d, want 0 (last problem: %s)", stats.ProblemCount, stats.LastProblem)
	}

	labeled, err := cs.List(beads.ListQuery{Label: "mc-live-contract"})
	if err != nil {
		t.Fatalf("List(label): %v", err)
	}
	if len(labeled) != 1 || labeled[0].ID != child.ID {
		t.Fatalf("labeled = %#v, want child %s", labeled, child.ID)
	}
	if labeled[0].ParentID != parent.ID {
		t.Fatalf("ParentID = %q, want %q", labeled[0].ParentID, parent.ID)
	}
	if labeled[0].Title != updatedTitle {
		t.Fatalf("Title = %q, want %q", labeled[0].Title, updatedTitle)
	}
}

type eventGetFailStore struct {
	beads.Store
	failGet bool
}

func (s *eventGetFailStore) Get(id string) (beads.Bead, error) {
	if s.failGet {
		return beads.Bead{}, errors.New("unexpected event backing get")
	}
	return s.Store.Get(id)
}

func TestCachingStoreApplyEventCoercesNonStringMetadata(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	payload, err := json.Marshal(map[string]any{
		"id":         "ext-1",
		"title":      "mayor",
		"status":     "open",
		"issue_type": "session",
		"metadata": map[string]any{
			"generation":           3,
			"pending_create_claim": true,
			"state":                "creating",
			"wake_attempts":        0,
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	cs.ApplyEvent("bead.updated", payload)

	stats := cs.Stats()
	if stats.ProblemCount != 0 {
		t.Fatalf("ProblemCount = %d, want 0 (last problem: %s)", stats.ProblemCount, stats.LastProblem)
	}

	got := requireCachedBead(t, cs, "ext-1", false)
	if got.Type != "session" {
		t.Fatalf("Type = %q, want session", got.Type)
	}
	if got.Metadata["generation"] != "3" {
		t.Fatalf("generation = %q, want 3; metadata=%v", got.Metadata["generation"], got.Metadata)
	}
	if got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want true; metadata=%v", got.Metadata["pending_create_claim"], got.Metadata)
	}
	if got.Metadata["wake_attempts"] != "0" {
		t.Fatalf("wake_attempts = %q, want 0; metadata=%v", got.Metadata["wake_attempts"], got.Metadata)
	}
}

func requireCachedBead(t *testing.T, cs *beads.CachingStore, id string, includeClosed bool) beads.Bead {
	t.Helper()
	items, err := cs.List(beads.ListQuery{AllowScan: true, IncludeClosed: includeClosed})
	if err != nil {
		t.Fatalf("List cached beads: %v", err)
	}
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("cached bead %q missing from %#v", id, items)
	return beads.Bead{}
}

func TestCachingStoreApplyEventIgnoredWhenDegraded(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	cs := beads.NewCachingStoreForTest(mem, nil)
	// Don't prime — stays uninitialized.

	payload, _ := json.Marshal(beads.Bead{ID: "gc-1", Title: "Test"})
	cs.ApplyEvent("bead.created", payload)

	// Should not be findable (not live).
	_, err := cs.Get("gc-1")
	if err == nil {
		t.Fatal("Get should fail when not live")
	}
}

func TestCachingStoreDegradedFallsThrough(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	b, _ := mem.Create(beads.Bead{Title: "Backing"})

	cs := beads.NewCachingStoreForTest(mem, nil)
	// Don't prime — reads fall through to backing.

	got, err := cs.Get(b.ID)
	if err != nil {
		t.Fatalf("Get fallthrough: %v", err)
	}
	if got.Title != "Backing" {
		t.Fatalf("title = %q, want Backing", got.Title)
	}
}

func TestCachingStoreOnChangeCallback(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()

	var events []string
	cs := beads.NewCachingStoreForTest(mem, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	b, _ := cs.Create(beads.Bead{Title: "Test"})
	_ = cs.Update(b.ID, beads.UpdateOpts{Title: strPtr("Changed")})
	_ = cs.Close(b.ID)

	if len(events) != 3 {
		t.Fatalf("events = %v, want 3", events)
	}
	if events[0] != "bead.created:"+b.ID {
		t.Errorf("events[0] = %q", events[0])
	}
	if events[1] != "bead.updated:"+b.ID {
		t.Errorf("events[1] = %q", events[1])
	}
	if events[2] != "bead.closed:"+b.ID {
		t.Errorf("events[2] = %q", events[2])
	}
}

func TestCachingStoreReconcilerStopsOnCancel(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cs.StartReconciler(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
	// Should not hang.
}

func TestCachingStoreListByMetadata(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	b1, _ := mem.Create(beads.Bead{Title: "A"})
	_ = mem.SetMetadata(b1.ID, "gc.kind", "workflow")
	b2, _ := mem.Create(beads.Bead{Title: "B"})
	_ = mem.SetMetadata(b2.ID, "gc.kind", "task")

	cs := beads.NewCachingStoreForTest(mem, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	results, err := cs.ListByMetadata(map[string]string{"gc.kind": "workflow"}, 0)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if len(results) != 1 || results[0].ID != b1.ID {
		t.Fatalf("results = %v, want only %s", results, b1.ID)
	}
}

func TestCachingStoreListIncludeClosedFallsBackToCachedMatches(t *testing.T) {
	t.Parallel()
	backing := &failingIncludeClosedMetadataStore{MemStore: beads.NewMemStore()}
	match, _ := backing.Create(beads.Bead{Title: "A"})
	_ = backing.SetMetadata(match.ID, "gc.kind", "workflow")
	other, _ := backing.Create(beads.Bead{Title: "B"})
	_ = backing.SetMetadata(other.ID, "gc.kind", "task")

	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	results, err := cs.List(beads.ListQuery{
		Metadata:      map[string]string{"gc.kind": "workflow"},
		IncludeClosed: true,
	})
	if err != nil {
		t.Fatalf("List(include closed): %v", err)
	}
	if len(results) != 1 || results[0].ID != match.ID {
		t.Fatalf("results = %v, want only %s", results, match.ID)
	}
}

type failingIncludeClosedMetadataStore struct {
	*beads.MemStore
}

func (s *failingIncludeClosedMetadataStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.IncludeClosed && len(query.Metadata) > 0 {
		return nil, errors.New("history unavailable")
	}
	return s.MemStore.List(query)
}

func strPtr(s string) *string { return &s }

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
