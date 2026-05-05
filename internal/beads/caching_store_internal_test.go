package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCachingStoreRunReconciliationDetectsLabelContentChanges(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "Task", Labels: []string{"old"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := backing.Update(bead.ID, UpdateOpts{
		Labels:       []string{"new"},
		RemoveLabels: []string{"old"},
	}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	cache.runReconciliation()

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "new" {
		t.Fatalf("Labels = %v, want [new]", got.Labels)
	}
}

func TestCachingStoreListInProgressUsesCacheByDefault(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{
		Title:    "claimed work",
		Assignee: "worker",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	status := "in_progress"
	if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	got, err := cache.List(ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List(in_progress) = %+v, want cached result before reconcile", got)
	}
}

func TestCachingStoreListLiveBypassesCache(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{
		Title:    "claimed work",
		Assignee: "worker",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	status := "in_progress"
	if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	got, err := cache.List(ListQuery{Status: "in_progress", Live: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != bead.ID {
		t.Fatalf("List(in_progress, Live) = %+v, want %s from backing store", got, bead.ID)
	}
}

func TestCachingStoreApplyEventRecordsBackingVerificationErrorAndAppliesUpdate(t *testing.T) {
	t.Parallel()

	backing := &cacheEventVerificationFailStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "original"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	localTitle := "local"
	if err := cache.Update(bead.ID, UpdateOpts{Title: &localTitle}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	cache.mu.Lock()
	delete(cache.beadSeq, bead.ID)
	cache.mu.Unlock()
	backing.failNextGet = true

	cache.ApplyEvent("bead.updated", json.RawMessage(`{"id":"`+bead.ID+`","title":"external"}`))

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "external" {
		t.Fatalf("Title after verification error = %q, want external", got.Title)
	}
	stats := cache.Stats()
	if stats.ProblemCount == 0 {
		t.Fatal("ProblemCount = 0, want verification error recorded")
	}
	if !strings.Contains(stats.LastProblem, "verify bead.updated event") {
		t.Fatalf("LastProblem = %q, want verify bead.updated event", stats.LastProblem)
	}
}

func TestCachingStoreIgnoresStaleClosedEventAfterLocalReopenBeyondRecentWindow(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "reopen me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.Reopen(bead.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	cache.mu.Lock()
	cache.localBeadAt[bead.ID] = time.Now().Add(-10 * time.Second)
	cache.mu.Unlock()

	cache.ApplyEvent("bead.closed", json.RawMessage(`{"id":"`+bead.ID+`","status":"closed"}`))

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("Status after stale closed event = %q, want open", got.Status)
	}
}

func TestCachingStoreIgnoresStaleClosedEventAfterLocalReopenAndLiveRefresh(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "reopen me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.Reopen(bead.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	cache.mu.Lock()
	cache.localBeadAt[bead.ID] = time.Now().Add(-10 * time.Second)
	cache.mu.Unlock()
	if got, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatalf("Live List: %v", err)
	} else if len(got) != 1 || got[0].ID != bead.ID {
		t.Fatalf("Live List = %+v, want reopened bead %s", got, bead.ID)
	}
	cache.mu.RLock()
	_, locallyMutated := cache.beadSeq[bead.ID]
	recentlyLocal := recentLocalMutation(cache.localBeadAt[bead.ID], time.Now())
	cache.mu.RUnlock()
	if locallyMutated || recentlyLocal {
		t.Fatalf("local markers after live refresh: locallyMutated=%v recentlyLocal=%v, want both false", locallyMutated, recentlyLocal)
	}

	cache.ApplyEvent("bead.closed", json.RawMessage(`{"id":"`+bead.ID+`","status":"closed"}`))

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("Status after stale closed event = %q, want open", got.Status)
	}
}

func TestCachingStoreClosedEventRechecksLocalReopenBeforeCommit(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "reopen me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}
	payload := json.RawMessage(`{"id":"` + bead.ID + `","status":"closed"}`)
	cache.ApplyEvent("bead.closed", payload)

	beforeCommit := make(chan struct{})
	releaseCommit := make(chan struct{})
	cache.applyEventBeforeCommitForTest = func() {
		close(beforeCommit)
		<-releaseCommit
	}

	done := make(chan struct{})
	go func() {
		cache.ApplyEvent("bead.closed", payload)
		close(done)
	}()

	<-beforeCommit
	if err := cache.Reopen(bead.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	close(releaseCommit)
	<-done

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("Status after stale closed event race = %q, want open", got.Status)
	}
}

func TestCachingStoreRecordsClosedEventVerificationErrorAndPreservesLocalReopen(t *testing.T) {
	backing := &cacheEventVerificationFailStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "reopen me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.Reopen(bead.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	backing.failNextGet = true

	cache.ApplyEvent("bead.closed", json.RawMessage(`{"id":"`+bead.ID+`","status":"closed"}`))

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("Status after verification error = %q, want open", got.Status)
	}
	stats := cache.Stats()
	if stats.ProblemCount == 0 {
		t.Fatal("ProblemCount = 0, want verification error recorded")
	}
	if !strings.Contains(stats.LastProblem, "verify bead.closed event") {
		t.Fatalf("LastProblem = %q, want verify bead.closed event", stats.LastProblem)
	}
}

type cacheEventVerificationFailStore struct {
	Store
	failNextGet bool
}

func (s *cacheEventVerificationFailStore) Get(id string) (Bead, error) {
	if s.failNextGet {
		s.failNextGet = false
		return Bead{}, errors.New("backing verification failed")
	}
	return s.Store.Get(id)
}

func TestCachingStoreRunReconciliationDetectsPriorityChanges(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	initialPriority := 1
	bead, err := backing.Create(Bead{Title: "Task", Priority: &initialPriority})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	updatedPriority := 2
	if err := backing.Update(bead.ID, UpdateOpts{Priority: &updatedPriority}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	cache.runReconciliation()

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Priority == nil || *got.Priority != updatedPriority {
		t.Fatalf("Priority = %v, want %d", got.Priority, updatedPriority)
	}
}

func TestCachingStoreRunReconciliationDetectsDepOnlyChangesAndNotifies(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "Blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	bead, err := backing.Create(Bead{Title: "Task"})
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}

	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	deps, err := cache.DepList(bead.ID, "down")
	if err != nil {
		t.Fatalf("DepList before dep add: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("initial deps = %v, want empty", deps)
	}

	if err := backing.DepAdd(bead.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd backing: %v", err)
	}

	cache.runReconciliation()

	deps, err = cache.DepList(bead.ID, "down")
	if err != nil {
		t.Fatalf("DepList after reconcile: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != blocker.ID {
		t.Fatalf("deps after reconcile = %v, want blocker %s", deps, blocker.ID)
	}
	if len(events) != 1 || events[0] != "bead.updated:"+bead.ID {
		t.Fatalf("events = %v, want [bead.updated:%s]", events, bead.ID)
	}
}

func TestCachingStoreRunReconciliationPublishesCallbacksAfterDepsCommitted(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "Blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	bead, err := backing.Create(Bead{Title: "Task"})
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}

	var observedDeps int
	var cache *CachingStore
	cache = NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		if eventType != "bead.updated" || beadID != bead.ID {
			return
		}
		deps, err := cache.DepList(beadID, "down")
		if err != nil {
			t.Fatalf("DepList during callback: %v", err)
		}
		observedDeps = len(deps)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if _, err := cache.DepList(bead.ID, "down"); err != nil {
		t.Fatalf("DepList before changes: %v", err)
	}

	title := "Task updated"
	if err := backing.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}
	if err := backing.DepAdd(bead.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd backing: %v", err)
	}

	cache.runReconciliation()

	if observedDeps != 1 {
		t.Fatalf("observed deps during callback = %d, want 1", observedDeps)
	}
}

func TestCachingStoreUpdateInvalidatesStaleCacheWhenRefreshFails(t *testing.T) {
	t.Parallel()

	backing := &refreshFailingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "after"
	backing.failNextGet = true
	if err := cache.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Title != "after" {
		t.Fatalf("Title = %q, want after", got.Title)
	}

	stats := cache.Stats()
	if stats.ProblemCount != 1 {
		t.Fatalf("ProblemCount = %d, want 1", stats.ProblemCount)
	}
	if !strings.Contains(stats.LastProblem, "refresh bead after update") {
		t.Fatalf("LastProblem = %q, want refresh context", stats.LastProblem)
	}
	if stats.LastProblemAt.IsZero() {
		t.Fatal("LastProblemAt should be set")
	}
}

func TestCachingStoreUpdateLogsRefreshFailure(t *testing.T) {
	backing := &refreshFailingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	var logged []string
	cache.problemf = func(msg string) {
		logged = append(logged, msg)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "after"
	backing.failNextGet = true
	if err := cache.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(logged) != 1 {
		t.Fatalf("logged = %v, want single refresh failure", logged)
	}
	if !strings.Contains(logged[0], "refresh bead after update") {
		t.Fatalf("logged[0] = %q, want refresh context", logged[0])
	}
	if !strings.Contains(logged[0], bead.ID) {
		t.Fatalf("logged[0] = %q, want bead id", logged[0])
	}
}

func TestCachingStoreDepListUpFallsThroughToBackingTruth(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	root, err := backing.Create(Bead{Title: "root"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	left, err := backing.Create(Bead{Title: "left"})
	if err != nil {
		t.Fatalf("Create left: %v", err)
	}
	right, err := backing.Create(Bead{Title: "right"})
	if err != nil {
		t.Fatalf("Create right: %v", err)
	}
	if err := backing.DepAdd(left.ID, root.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd left: %v", err)
	}
	if err := backing.DepAdd(right.ID, root.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd right: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Populate only one downward dep entry in the cache, leaving reverse lookups
	// incomplete unless they fall through to the backing store.
	if _, err := cache.DepList(left.ID, "down"); err != nil {
		t.Fatalf("DepList left down: %v", err)
	}

	deps, err := cache.DepList(root.ID, "up")
	if err != nil {
		t.Fatalf("DepList root up: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("DepList(root, up) = %d deps, want 2", len(deps))
	}
}

func TestCachingStoreApplyEventRecordsProblemOnMalformedPayload(t *testing.T) {
	t.Parallel()

	cache := NewCachingStoreForTest(NewMemStore(), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cache.ApplyEvent("bead.updated", []byte(`{`))

	stats := cache.Stats()
	if stats.ProblemCount != 1 {
		t.Fatalf("ProblemCount = %d, want 1", stats.ProblemCount)
	}
	if !strings.Contains(stats.LastProblem, "apply bead.updated event") {
		t.Fatalf("LastProblem = %q, want apply-event context", stats.LastProblem)
	}
	if stats.LastProblemAt.IsZero() {
		t.Fatal("LastProblemAt should be set")
	}
}

func TestCachingStoreApplyEventRechecksLocalMutationBeforeCommit(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{
		Title:    "mail",
		Type:     "message",
		Labels:   []string{"thread:abc"},
		Metadata: map[string]string{"mail.read": "false"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.Update(bead.ID, UpdateOpts{
		Labels:   []string{"read"},
		Metadata: map[string]string{"mail.read": "true"},
	}); err != nil {
		t.Fatalf("Mark read update: %v", err)
	}
	staleRead, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get stale read payload: %v", err)
	}
	payload, err := json.Marshal(staleRead)
	if err != nil {
		t.Fatalf("Marshal stale read payload: %v", err)
	}

	beforeCommit := make(chan struct{})
	releaseCommit := make(chan struct{})
	cache.applyEventBeforeCommitForTest = func() {
		close(beforeCommit)
		<-releaseCommit
	}

	done := make(chan struct{})
	go func() {
		cache.ApplyEvent("bead.updated", payload)
		close(done)
	}()

	<-beforeCommit
	if err := cache.Update(bead.ID, UpdateOpts{
		RemoveLabels: []string{"read"},
		Metadata:     map[string]string{"mail.read": "false"},
	}); err != nil {
		t.Fatalf("Mark unread update: %v", err)
	}
	close(releaseCommit)
	<-done

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after stale event race: %v", err)
	}
	for _, label := range got.Labels {
		if label == "read" {
			t.Fatalf("labels after stale event race = %#v, want read removed", got.Labels)
		}
	}
	if got.Metadata["mail.read"] != "false" {
		t.Fatalf("mail.read after stale event race = %q, want false; metadata=%v", got.Metadata["mail.read"], got.Metadata)
	}
}

func TestCachingStoreApplyEventRechecksRecentLocalAfterGetRefresh(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	localTitle := "local"
	if err := cache.Update(bead.ID, UpdateOpts{Title: &localTitle}); err != nil {
		t.Fatalf("Update local title: %v", err)
	}
	cache.mu.Lock()
	cache.dirty[bead.ID] = struct{}{}
	cache.mu.Unlock()
	if _, err := cache.Get(bead.ID); err != nil {
		t.Fatalf("Get refresh after local update: %v", err)
	}

	cache.mu.RLock()
	_, locallyMutated := cache.beadSeq[bead.ID]
	recentlyLocal := recentLocalMutation(cache.localBeadAt[bead.ID], time.Now())
	cache.mu.RUnlock()
	if locallyMutated || !recentlyLocal {
		t.Fatalf("markers after Get refresh: locallyMutated=%v recentlyLocal=%v, want false/true", locallyMutated, recentlyLocal)
	}

	externalTitle := "external"
	if err := backing.Update(bead.ID, UpdateOpts{Title: &externalTitle}); err != nil {
		t.Fatalf("Update backing external title: %v", err)
	}
	payload := json.RawMessage(fmt.Sprintf(`{"id":%q,"title":%q}`, bead.ID, externalTitle))

	beforeCommit := make(chan struct{})
	releaseCommit := make(chan struct{})
	cache.applyEventBeforeCommitForTest = func() {
		close(beforeCommit)
		<-releaseCommit
	}

	done := make(chan struct{})
	go func() {
		cache.ApplyEvent("bead.updated", payload)
		close(done)
	}()

	<-beforeCommit
	newerTitle := "newer local cache"
	if err := backing.Update(bead.ID, UpdateOpts{Title: &newerTitle}); err != nil {
		t.Fatalf("Update backing newer title: %v", err)
	}
	cache.mu.Lock()
	cache.dirty[bead.ID] = struct{}{}
	cache.mu.Unlock()
	if _, err := cache.Get(bead.ID); err != nil {
		t.Fatalf("Get refresh before event commit: %v", err)
	}
	close(releaseCommit)
	<-done

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after stale event race: %v", err)
	}
	if got.Title != newerTitle {
		t.Fatalf("Title after stale event race = %q, want %q", got.Title, newerTitle)
	}
}

func TestCachingStoreRunReconciliationRecordsProblemAndDegrades(t *testing.T) {
	t.Parallel()

	backing := &listFailingStore{Store: NewMemStore()}
	if _, err := backing.Create(Bead{Title: "Task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.failList = true
	for i := 0; i < maxCacheSyncFailures; i++ {
		cache.runReconciliation()
	}

	if cache.state != cacheDegraded {
		t.Fatalf("state = %v, want degraded", cache.state)
	}

	stats := cache.Stats()
	if stats.SyncFailures != maxCacheSyncFailures {
		t.Fatalf("SyncFailures = %d, want %d", stats.SyncFailures, maxCacheSyncFailures)
	}
	if stats.ProblemCount != int64(maxCacheSyncFailures) {
		t.Fatalf("ProblemCount = %d, want %d", stats.ProblemCount, maxCacheSyncFailures)
	}
	if !strings.Contains(stats.LastProblem, "reconcile cache") {
		t.Fatalf("LastProblem = %q, want reconcile context", stats.LastProblem)
	}
}

func TestCachingStorePrimeActiveUsesPartialResultRows(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:           NewMemStore(),
		partialStatuses: map[string]bool{"open": true},
	}
	open, err := backing.Create(Bead{Title: "open survivor"})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	inProgress, err := backing.Create(Bead{Title: "in progress survivor"})
	if err != nil {
		t.Fatalf("Create(in_progress): %v", err)
	}
	status := "in_progress"
	if err := backing.Update(inProgress.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(in_progress): %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	cache.mu.RLock()
	_, hasOpen := cache.beads[open.ID]
	_, hasInProgress := cache.beads[inProgress.ID]
	cache.mu.RUnlock()
	if !hasOpen || !hasInProgress {
		t.Fatalf("cache.beads has open=%v in_progress=%v, want both partial rows retained", hasOpen, hasInProgress)
	}
	stats := cache.Stats()
	if stats.ProblemCount != 1 {
		t.Fatalf("ProblemCount = %d, want 1", stats.ProblemCount)
	}
	if !strings.Contains(stats.LastProblem, "prime active (open)") {
		t.Fatalf("LastProblem = %q, want prime active context", stats.LastProblem)
	}
	if cache.state != cachePartial {
		t.Fatalf("state = %v, want cachePartial", cache.state)
	}
}

func TestCachingStorePrimeUsesPartialResultRows(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:            NewMemStore(),
		partialAllowScan: true,
	}
	survivor, err := backing.Create(Bead{Title: "prime survivor"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cache.mu.RLock()
	_, hasSurvivor := cache.beads[survivor.ID]
	cache.mu.RUnlock()
	if !hasSurvivor {
		t.Fatalf("cache.beads missing partial survivor %s", survivor.ID)
	}
	stats := cache.Stats()
	if stats.ProblemCount != 1 {
		t.Fatalf("ProblemCount = %d, want 1", stats.ProblemCount)
	}
	if !strings.Contains(stats.LastProblem, "prime cache") {
		t.Fatalf("LastProblem = %q, want prime cache context", stats.LastProblem)
	}
	if cache.state != cacheLive {
		t.Fatalf("state = %v, want cacheLive", cache.state)
	}
}

func TestCachingStoreCachedListRejectsPartialPrime(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:            NewMemStore(),
		partialAllowScan: true,
	}
	survivor, err := backing.Create(Bead{Title: "survives partial prime"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	if _, err := backing.Create(Bead{Title: "dropped by bd parse"}); err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	backing.partialRows = []Bead{survivor}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	items, ok := cache.CachedList(ListQuery{AllowScan: true})
	if ok {
		t.Fatalf("CachedList ok = true with items %+v, want ok=false while primePartialErr is set", items)
	}
}

func TestCachingStorePrimePartialDoesNotServeActiveListAsComplete(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:            NewMemStore(),
		partialAllowScan: true,
	}
	survivor, err := backing.Create(Bead{Title: "survives partial prime"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	dropped, err := backing.Create(Bead{Title: "dropped by bd parse"})
	if err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	backing.partialRows = []Bead{survivor}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	items, err := cache.List(ListQuery{AllowScan: true})
	var partial *PartialResultError
	if !errors.As(err, &partial) {
		t.Fatalf("List() error = %v, want *PartialResultError after partial prime", err)
	}
	if hasBead(items, dropped.ID) {
		t.Fatalf("List() returned dropped bead %s despite backing partial rows: %+v", dropped.ID, items)
	}
	if !hasBead(items, survivor.ID) {
		t.Fatalf("List() = %+v, want partial survivor %s", items, survivor.ID)
	}
}

func TestCachingStorePrimeActivePartialFallsBackForActiveList(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:           NewMemStore(),
		partialStatuses: map[string]bool{"open": true},
	}
	survivor, err := backing.Create(Bead{Title: "survives partial active prime"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	dropped, err := backing.Create(Bead{Title: "dropped from primed status"})
	if err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	backing.partialRows = []Bead{survivor}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	items, err := cache.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List() error = %v, want clean backing fallback", err)
	}
	if !hasBead(items, survivor.ID) || !hasBead(items, dropped.ID) {
		t.Fatalf("List() = %+v, want backing fallback to return survivor %s and dropped %s", items, survivor.ID, dropped.ID)
	}
}

func TestCachingStoreReadyFallsBackAfterPartialPrime(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:            NewMemStore(),
		partialAllowScan: true,
	}
	survivor, err := backing.Create(Bead{Title: "survives partial prime"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	dropped, err := backing.Create(Bead{Title: "dropped by bd parse"})
	if err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	backing.partialRows = []Bead{survivor}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	items, err := cache.Ready()
	if err != nil {
		t.Fatalf("Ready() error = %v, want backing fallback success", err)
	}
	if !hasBead(items, survivor.ID) || !hasBead(items, dropped.ID) {
		t.Fatalf("Ready() = %+v, want backing fallback to include survivor %s and dropped %s", items, survivor.ID, dropped.ID)
	}
}

func TestCachingStoreRunReconciliationDoesNotTreatPartialResultAsAuthoritative(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{Store: NewMemStore()}
	survivor, err := backing.Create(Bead{Title: "survives partial list"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	dropped, err := backing.Create(Bead{Title: "dropped by bd parse"})
	if err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.partialAllowScan = true
	claimedStatus := "in_progress"
	if err := backing.Update(survivor.ID, UpdateOpts{Status: &claimedStatus}); err != nil {
		t.Fatalf("Update(survivor): %v", err)
	}
	updatedSurvivor, err := backing.Get(survivor.ID)
	if err != nil {
		t.Fatalf("Get(updated survivor): %v", err)
	}
	backing.partialRows = []Bead{updatedSurvivor}
	cache.runReconciliation()
	for i := 1; i < maxCacheSyncFailures; i++ {
		cache.runReconciliation()
	}

	for _, event := range events {
		if event == "bead.closed:"+dropped.ID {
			t.Fatalf("partial reconcile emitted synthetic close for dropped row: %v", events)
		}
		if event == "bead.updated:"+survivor.ID {
			t.Fatalf("partial reconcile emitted update for survivor row: %v", events)
		}
	}
	cache.mu.RLock()
	_, stillCached := cache.beads[dropped.ID]
	cachedSurvivor := cache.beads[survivor.ID]
	state := cache.state
	syncFailures := cache.syncFailures
	cache.mu.RUnlock()
	if !stillCached {
		t.Fatalf("dropped row %s was evicted from cache after partial reconcile", dropped.ID)
	}
	if cachedSurvivor.Status == claimedStatus {
		t.Fatalf("survivor status = %q, want partial reconcile to leave cached status non-authoritative", cachedSurvivor.Status)
	}
	if state != cacheDegraded {
		t.Fatalf("state = %v, want cacheDegraded after repeated partial list failures", state)
	}
	if syncFailures != maxCacheSyncFailures {
		t.Fatalf("syncFailures = %d, want %d", syncFailures, maxCacheSyncFailures)
	}
	stats := cache.Stats()
	if stats.ProblemCount != int64(maxCacheSyncFailures) {
		t.Fatalf("ProblemCount = %d, want %d", stats.ProblemCount, maxCacheSyncFailures)
	}
}

func TestCachingStoreRunReconciliationDegradesImmediatelyOnPartialResult(t *testing.T) {
	t.Parallel()

	backing := &readyCountingPartialListStore{
		partialListErrorStore: &partialListErrorStore{
			Store:           NewMemStore(),
			partialStatuses: map[string]bool{"open": true},
		},
	}
	survivor, err := backing.Create(Bead{Title: "survives partial list"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	if _, err := backing.Create(Bead{Title: "dropped by bd parse"}); err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.partialAllowScan = true
	backing.partialRows = []Bead{survivor}
	cache.runReconciliation()

	cache.mu.RLock()
	state := cache.state
	cache.mu.RUnlock()
	if state != cacheDegraded {
		t.Fatalf("state = %v, want cacheDegraded after one partial reconcile", state)
	}
	items, err := cache.List(ListQuery{Status: "open"})
	if !IsPartialResult(err) {
		t.Fatalf("List() error = %v, want PartialResultError", err)
	}
	if !hasBead(items, survivor.ID) {
		t.Fatalf("List() = %+v, want survivor %s from backing fallback", items, survivor.ID)
	}
	if cached, ok := cache.CachedList(ListQuery{Status: "open"}); ok {
		t.Fatalf("CachedList() = %+v, true; want unavailable after partial reconcile", cached)
	}
	readyCalls := backing.readyCalls
	if _, err := cache.Ready(); err != nil {
		t.Fatalf("Ready(): %v", err)
	}
	if backing.readyCalls == readyCalls {
		t.Fatalf("Ready() did not fall back to backing store after partial reconcile")
	}
}

func TestCachingStoreRunReconciliationDegradesPartialCache(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{Store: NewMemStore()}
	if _, err := backing.Create(Bead{Title: "active bead"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	backing.partialAllowScan = true
	for i := 0; i < maxCacheSyncFailures; i++ {
		cache.runReconciliation()
	}

	cache.mu.RLock()
	state := cache.state
	syncFailures := cache.syncFailures
	cache.mu.RUnlock()
	if state != cacheDegraded {
		t.Fatalf("state = %v, want cacheDegraded after repeated partial reconcile failures from cachePartial", state)
	}
	if syncFailures != maxCacheSyncFailures {
		t.Fatalf("syncFailures = %d, want %d", syncFailures, maxCacheSyncFailures)
	}
}

func TestCachingStoreNextReconcileDelayUsesFreshnessWatchdog(t *testing.T) {
	t.Parallel()

	cache := NewCachingStoreForTest(NewMemStore(), nil)
	cache.state = cacheLive
	cache.lastFreshAt = time.Unix(100, 0)

	if got := cache.nextReconcileDelay(time.Unix(110, 0)); got != 20*time.Second {
		t.Fatalf("nextReconcileDelay(fresh) = %s, want 20s", got)
	}

	cache.stats.LastReconcileAt = time.Unix(70, 0)
	cache.lastFreshAt = time.Unix(109, 0)
	if got := cache.nextReconcileDelay(time.Unix(110, 0)); got != 0 {
		t.Fatalf("nextReconcileDelay(stale full scan with fresh writes) = %s, want immediate reconcile", got)
	}

	cache.stats.LastReconcileAt = time.Time{}
	cache.lastFreshAt = time.Unix(70, 0)
	if got := cache.nextReconcileDelay(time.Unix(110, 0)); got != 0 {
		t.Fatalf("nextReconcileDelay(stale) = %s, want immediate reconcile", got)
	}

	cache.state = cacheDegraded
	cache.lastFreshAt = time.Unix(109, 0)
	if got := cache.nextReconcileDelay(time.Unix(110, 0)); got != 0 {
		t.Fatalf("nextReconcileDelay(degraded) = %s, want immediate reconcile", got)
	}
}

func TestCachingStoreCloseAllRefreshesOnlyActuallyClosedBeads(t *testing.T) {
	t.Parallel()

	backing := &partialCloseAllStore{Store: NewMemStore()}
	first, err := backing.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := backing.Create(Bead{Title: "second"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closed, err := cache.CloseAll([]string{first.ID, second.ID}, map[string]string{"source": "wave1"})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	gotFirst, err := cache.Get(first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if gotFirst.Status != "closed" {
		t.Fatalf("first status = %q, want closed", gotFirst.Status)
	}
	if gotFirst.Metadata["source"] != "wave1" {
		t.Fatalf("first metadata = %v, want source=wave1", gotFirst.Metadata)
	}

	gotSecond, err := cache.Get(second.ID)
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	if gotSecond.Status != "open" {
		t.Fatalf("second status = %q, want open", gotSecond.Status)
	}
	if gotSecond.Metadata["source"] != "" {
		t.Fatalf("second metadata = %v, want no source metadata", gotSecond.Metadata)
	}
}

func TestCachingStoreCloseAllRefreshesPartialSuccessBeforeReturningError(t *testing.T) {
	t.Parallel()

	backing := &partialCloseAllErrorStore{Store: NewMemStore()}
	first, err := backing.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := backing.Create(Bead{Title: "second"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closed, err := cache.CloseAll([]string{first.ID, second.ID}, map[string]string{"source": "wave1"})
	if err == nil {
		t.Fatal("expected CloseAll error")
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	gotFirst, err := cache.Get(first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if gotFirst.Status != "closed" {
		t.Fatalf("first status = %q, want closed", gotFirst.Status)
	}
	if gotFirst.Metadata["source"] != "wave1" {
		t.Fatalf("first metadata = %v, want source=wave1", gotFirst.Metadata)
	}
	stats := cache.Stats()
	if stats.State != "live" {
		t.Fatalf("cache state = %q, want live", stats.State)
	}
}

func TestCachingStoreCloseAllRefreshesNonPrefixPartialSuccess(t *testing.T) {
	t.Parallel()

	backing := &nonPrefixCloseAllErrorStore{Store: NewMemStore()}
	first, err := backing.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := backing.Create(Bead{Title: "second"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closed, err := cache.CloseAll([]string{first.ID, second.ID}, map[string]string{"source": "wave1"})
	if err == nil {
		t.Fatal("expected CloseAll error")
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	gotFirst, err := cache.Get(first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if gotFirst.Status != "open" {
		t.Fatalf("first status = %q, want open", gotFirst.Status)
	}
	gotSecond, err := cache.Get(second.ID)
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	if gotSecond.Status != "closed" {
		t.Fatalf("second status = %q, want closed", gotSecond.Status)
	}
	if gotSecond.Metadata["source"] != "wave1" {
		t.Fatalf("second metadata = %v, want source=wave1", gotSecond.Metadata)
	}
}

func TestCachingStoreCloseAllMarksRefreshFailuresDirty(t *testing.T) {
	t.Parallel()

	backing := &closeAllRefreshFailingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.failGetID = bead.ID
	closed, err := cache.CloseAll([]string{bead.ID}, nil)
	if err == nil {
		t.Fatal("expected CloseAll refresh error")
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	if _, err := cache.List(ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if backing.listCalls == 0 {
		t.Fatal("List did not fall back to backing store after dirty refresh failure")
	}
}

func TestCachingStoreCachedListReturnsSnapshotWithDirtyEntries(t *testing.T) {
	t.Parallel()

	backing := &refreshFailingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "updated while refresh fails"
	backing.failNextGet = true
	if err := cache.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rows, ok := cache.CachedList(ListQuery{Status: "open"})
	if !ok {
		t.Fatal("CachedList returned ok=false for dirty cache, want snapshot")
	}
	if len(rows) != 1 || rows[0].ID != bead.ID {
		t.Fatalf("CachedList = %#v, want dirty snapshot row %s", rows, bead.ID)
	}
	if rows[0].Title == title {
		t.Fatalf("CachedList returned refreshed title %q; test setup did not create a dirty stale snapshot", rows[0].Title)
	}
}

type refreshFailingStore struct {
	Store
	failNextGet bool
}

func (s *refreshFailingStore) Get(id string) (Bead, error) {
	if s.failNextGet {
		s.failNextGet = false
		return Bead{}, errors.New("transient get failure")
	}
	return s.Store.Get(id)
}

type listFailingStore struct {
	Store
	failList bool
}

func (s *listFailingStore) List(query ListQuery) ([]Bead, error) {
	if s.failList {
		return nil, errors.New("transient list failure")
	}
	return s.Store.List(query)
}

type partialListErrorStore struct {
	Store
	partialStatuses  map[string]bool
	partialAllowScan bool
	partialRows      []Bead
}

func (s *partialListErrorStore) List(query ListQuery) ([]Bead, error) {
	items, err := s.Store.List(query)
	if err != nil {
		return nil, err
	}
	if s.partialStatuses[query.Status] || (s.partialAllowScan && query.AllowScan) {
		if s.partialRows != nil {
			items = make([]Bead, len(s.partialRows))
			for i := range s.partialRows {
				items[i] = cloneBead(s.partialRows[i])
			}
		}
		return items, &PartialResultError{
			Op:  "bd list",
			Err: errors.New("skipped 1 corrupt bead"),
		}
	}
	return items, nil
}

type readyCountingPartialListStore struct {
	*partialListErrorStore
	readyCalls int
}

func (s *readyCountingPartialListStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	s.readyCalls++
	return s.partialListErrorStore.Ready(query...)
}

func hasBead(items []Bead, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

type partialCloseAllStore struct {
	Store
}

func (s *partialCloseAllStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if err := s.SetMetadataBatch(ids[0], metadata); err != nil {
		return 0, err
	}
	if err := s.Close(ids[0]); err != nil {
		return 0, err
	}
	return 1, nil
}

type partialCloseAllErrorStore struct {
	Store
}

func (s *partialCloseAllErrorStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if len(ids) == 0 {
		return 0, errors.New("no ids")
	}
	if err := s.SetMetadataBatch(ids[0], metadata); err != nil {
		return 0, err
	}
	if err := s.Close(ids[0]); err != nil {
		return 0, err
	}
	return 1, errors.New("second close failed")
}

type nonPrefixCloseAllErrorStore struct {
	Store
}

func (s *nonPrefixCloseAllErrorStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if len(ids) < 2 {
		return 0, errors.New("need two ids")
	}
	if err := s.SetMetadataBatch(ids[1], metadata); err != nil {
		return 0, err
	}
	if err := s.Close(ids[1]); err != nil {
		return 0, err
	}
	return 1, errors.New("first close failed")
}

type closeAllRefreshFailingStore struct {
	Store
	failGetID string
	listCalls int
}

func (s *closeAllRefreshFailingStore) Get(id string) (Bead, error) {
	if id == s.failGetID {
		s.failGetID = ""
		return Bead{}, errors.New("refresh failed")
	}
	return s.Store.Get(id)
}

func (s *closeAllRefreshFailingStore) CloseAll(ids []string, _ map[string]string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if err := s.Close(ids[0]); err != nil {
		return 0, err
	}
	return 1, nil
}

func (s *closeAllRefreshFailingStore) List(query ListQuery) ([]Bead, error) {
	s.listCalls++
	return s.Store.List(query)
}

// Reconciliation must not re-emit bead.closed for a cache entry whose status
// is already "closed". When ApplyEvent ingests an external bead.closed event
// (from the bus), it stores the closed bead in c.beads. List({AllowScan:true})
// filters out closed beads, so the next reconcile sees the entry as missing
// from the fresh DB read and would re-emit a duplicate close notification.
// Routed back through the event bus, that notification re-applies into every
// caching store and reconciles into another spurious close — the storm.
func TestCachingStoreRunReconciliationDoesNotEmitBeadClosedForAlreadyClosedCacheEntry(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// External writer closes the bead in the backing store, then the close
	// event is delivered through the bus and applied to this cache.
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	closed := bead
	closed.Status = "closed"
	payload, err := json.Marshal(closed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cache.ApplyEvent("bead.closed", payload)
	events = nil // ignore notifications from prime/apply; only assert on reconcile output

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+bead.ID {
			t.Fatalf("reconciler emitted duplicate bead.closed for an already-closed cache entry; events=%v", events)
		}
	}
}

func TestCachingStoreBdPrimeAndReconcileSkipFullDepScan(t *testing.T) {
	t.Parallel()

	var depListCalls int
	var readyCalls int
	issueJSON := []byte(`[{
		"id":"bd-1",
		"title":"task",
		"status":"open",
		"issue_type":"task",
		"created_at":"2026-01-01T00:00:00Z",
		"labels":["task"],
		"metadata":{}
	}]`)
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			t.Fatalf("command name = %q, want bd", name)
		}
		if len(args) > 0 && args[0] == "dep" {
			depListCalls++
			t.Fatalf("unexpected dep scan command: %v", args)
		}
		if len(args) > 0 && args[0] == "ready" {
			readyCalls++
			return issueJSON, nil
		}
		if len(args) > 0 && args[0] == "list" {
			return issueJSON, nil
		}
		return []byte(`[]`), nil
	}
	cache := NewCachingStore(NewBdStore("/city", runner), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.runReconciliation()
	if depListCalls != 0 {
		t.Fatalf("dep list calls = %d, want 0", depListCalls)
	}
	if _, err := cache.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if readyCalls != 1 {
		t.Fatalf("Ready calls = %d, want backing Ready fallback when deps are incomplete", readyCalls)
	}
}

func TestCachingStoreBdPrimeActiveUsesListDependenciesForCachedReady(t *testing.T) {
	t.Parallel()

	var depListCalls int
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			t.Fatalf("command name = %q, want bd", name)
		}
		if len(args) > 0 && args[0] == "dep" {
			depListCalls++
			t.Fatalf("unexpected dep scan command: %v", args)
		}
		if len(args) > 0 && args[0] == "list" {
			argLine := strings.Join(args, " ")
			if strings.Contains(argLine, "--status=open") {
				return []byte(`[
					{"id":"bd-blocker","title":"blocker","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z","labels":["task"],"metadata":{}},
					{"id":"bd-blocked","title":"blocked","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:01Z","labels":["task"],"metadata":{},"dependencies":[{"issue_id":"bd-blocked","depends_on_id":"bd-blocker","type":"blocks"}]}
				]`), nil
			}
			if strings.Contains(argLine, "--status=in_progress") {
				return []byte(`[]`), nil
			}
		}
		return []byte(`[]`), nil
	}
	cache := NewCachingStoreForTest(NewBdStore("/city", runner), nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	ready, ok := cache.CachedReady()
	if !ok {
		t.Fatal("CachedReady reported cache unavailable")
	}
	ids := map[string]bool{}
	for _, b := range ready {
		ids[b.ID] = true
	}
	if !ids["bd-blocker"] || ids["bd-blocked"] {
		t.Fatalf("CachedReady ids = %v, want blocker ready and blocked excluded", ids)
	}
	if depListCalls != 0 {
		t.Fatalf("dep list calls = %d, want 0", depListCalls)
	}
}

func TestCachingStoreBdReconcileRefreshesListDependenciesForCachedReady(t *testing.T) {
	t.Parallel()

	runner := newCachingStoreBdDepRunner(t)
	cache := NewCachingStore(NewBdStore("/city", runner.run), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	assertCachedReadyContains := func(wantReady bool) {
		t.Helper()
		ready, ok := cache.CachedReady()
		if !ok {
			t.Fatal("CachedReady reported cache unavailable")
		}
		readyByID := make(map[string]bool, len(ready))
		for _, bead := range ready {
			readyByID[bead.ID] = true
		}
		if readyByID["bd-1"] != wantReady {
			t.Fatalf("CachedReady includes bd-1 = %v, want %v; ready=%v", readyByID["bd-1"], wantReady, readyByID)
		}
	}

	assertCachedReadyContains(true)

	runner.deps["bd-1"] = []Dep{{IssueID: "bd-1", DependsOnID: "bd-2", Type: "blocks"}}
	cache.runReconciliation()
	assertCachedReadyContains(false)

	runner.deps["bd-1"] = nil
	cache.runReconciliation()
	assertCachedReadyContains(true)

	if runner.depScanCalls != 0 {
		t.Fatalf("dep scan calls = %d, want 0", runner.depScanCalls)
	}
}

func TestCachingStoreBdIncompleteDepsUseBackingForDownDepList(t *testing.T) {
	t.Parallel()

	runner := newCachingStoreBdDepRunner(t)
	cache := NewCachingStore(NewBdStore("/city", runner.run), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	deps, err := cache.DepList("bd-1", "down")
	if err != nil {
		t.Fatalf("initial DepList: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("initial deps = %v, want empty", deps)
	}

	runner.deps["bd-1"] = []Dep{{IssueID: "bd-1", DependsOnID: "bd-2", Type: "blocks"}}
	cache.runReconciliation()

	deps, err = cache.DepList("bd-1", "down")
	if err != nil {
		t.Fatalf("DepList after external dep add: %v", err)
	}
	if !hasDep(deps, "bd-2") {
		t.Fatalf("deps after external dep add = %v, want bd-1 -> bd-2 from backing store", deps)
	}
	if runner.depScanCalls != 0 {
		t.Fatalf("dep scan calls = %d, want 0", runner.depScanCalls)
	}
}

func TestCachingStoreBdIncompleteDepsDepAddDoesNotDropExistingBackingDeps(t *testing.T) {
	t.Parallel()

	runner := newCachingStoreBdDepRunner(t)
	runner.deps["bd-1"] = []Dep{{IssueID: "bd-1", DependsOnID: "bd-2", Type: "blocks"}}
	cache := NewCachingStore(NewBdStore("/city", runner.run), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.DepAdd("bd-1", "bd-3", "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	deps, err := cache.DepList("bd-1", "down")
	if err != nil {
		t.Fatalf("DepList after DepAdd: %v", err)
	}
	if !hasDep(deps, "bd-2") || !hasDep(deps, "bd-3") {
		t.Fatalf("deps after DepAdd = %v, want existing bd-2 and added bd-3", deps)
	}
}

func TestCachingStoreBdIncompleteDepsDepRemoveDoesNotDropExternalBackingDeps(t *testing.T) {
	t.Parallel()

	runner := newCachingStoreBdDepRunner(t)
	runner.deps["bd-1"] = []Dep{
		{IssueID: "bd-1", DependsOnID: "bd-2", Type: "blocks"},
		{IssueID: "bd-1", DependsOnID: "bd-3", Type: "blocks"},
	}
	cache := NewCachingStore(NewBdStore("/city", runner.run), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if _, err := cache.DepList("bd-1", "down"); err != nil {
		t.Fatalf("DepList before external add: %v", err)
	}
	runner.deps["bd-1"] = append(runner.deps["bd-1"], Dep{IssueID: "bd-1", DependsOnID: "bd-4", Type: "blocks"})

	if err := cache.DepRemove("bd-1", "bd-3"); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}

	deps, err := cache.DepList("bd-1", "down")
	if err != nil {
		t.Fatalf("DepList after DepRemove: %v", err)
	}
	if hasDep(deps, "bd-3") {
		t.Fatalf("deps after DepRemove = %v, still contains removed bd-3", deps)
	}
	if !hasDep(deps, "bd-2") || !hasDep(deps, "bd-4") {
		t.Fatalf("deps after DepRemove = %v, want retained bd-2 and external bd-4", deps)
	}
}

type cachingStoreBdDepRunner struct {
	t            *testing.T
	deps         map[string][]Dep
	depScanCalls int
}

func newCachingStoreBdDepRunner(t *testing.T) *cachingStoreBdDepRunner {
	t.Helper()
	return &cachingStoreBdDepRunner{
		t:    t,
		deps: make(map[string][]Dep),
	}
}

func (r *cachingStoreBdDepRunner) run(_, name string, args ...string) ([]byte, error) {
	r.t.Helper()
	if name != "bd" {
		r.t.Fatalf("command name = %q, want bd", name)
	}
	if len(args) == 0 {
		r.t.Fatal("empty bd command")
	}
	switch args[0] {
	case "list":
		return r.listOutput(), nil
	case "ready":
		return []byte(`[]`), nil
	case "dep":
		return r.runDep(args[1:]...)
	default:
		return []byte(`[]`), nil
	}
}

func (r *cachingStoreBdDepRunner) runDep(args ...string) ([]byte, error) {
	r.t.Helper()
	if len(args) == 0 {
		r.t.Fatal("empty bd dep command")
	}
	switch args[0] {
	case "list":
		if len(args) > 1 && args[1] == "bd-1" {
			return r.depListOutput("bd-1"), nil
		}
		r.depScanCalls++
		r.t.Fatalf("unexpected dep scan command: %v", args)
	case "add":
		if len(args) < 5 || args[3] != "--type" {
			r.t.Fatalf("unexpected dep add args: %v", args)
		}
		r.addDep(args[1], args[2], args[4])
		return []byte(`[]`), nil
	case "remove":
		if len(args) < 3 {
			r.t.Fatalf("unexpected dep remove args: %v", args)
		}
		r.removeDep(args[1], args[2])
		return []byte(`[]`), nil
	}
	r.t.Fatalf("unexpected dep command: %v", args)
	return nil, nil
}

func (r *cachingStoreBdDepRunner) listOutput() []byte {
	var b strings.Builder
	ids := []string{"bd-1", "bd-2", "bd-3", "bd-4"}
	b.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%q,"title":%q,"status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z","labels":["task"],"metadata":{}`, id, "dep "+strings.TrimPrefix(id, "bd-"))
		if deps := r.deps[id]; len(deps) > 0 {
			b.WriteString(`,"dependencies":[`)
			for depIdx, dep := range deps {
				if depIdx > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `{"issue_id":%q,"depends_on_id":%q,"type":%q}`, dep.IssueID, dep.DependsOnID, dep.Type)
			}
			b.WriteByte(']')
		}
		b.WriteByte('}')
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func (r *cachingStoreBdDepRunner) depListOutput(issueID string) []byte {
	deps := r.deps[issueID]
	if len(deps) == 0 {
		return []byte(`[]`)
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, dep := range deps {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%q,"title":"dep","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z","dependency_type":%q}`, dep.DependsOnID, dep.Type)
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func (r *cachingStoreBdDepRunner) addDep(issueID, dependsOnID, depType string) {
	deps := r.deps[issueID]
	for i, dep := range deps {
		if dep.DependsOnID == dependsOnID {
			deps[i].Type = depType
			r.deps[issueID] = deps
			return
		}
	}
	r.deps[issueID] = append(deps, Dep{IssueID: issueID, DependsOnID: dependsOnID, Type: depType})
}

func (r *cachingStoreBdDepRunner) removeDep(issueID, dependsOnID string) {
	deps := r.deps[issueID]
	for i, dep := range deps {
		if dep.DependsOnID == dependsOnID {
			r.deps[issueID] = append(deps[:i], deps[i+1:]...)
			return
		}
	}
}

func hasDep(deps []Dep, dependsOnID string) bool {
	for _, dep := range deps {
		if dep.IssueID == "bd-1" && dep.DependsOnID == dependsOnID {
			return true
		}
	}
	return false
}
