package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// F1: a committed close on a backing whose ordinary Get hides closed rows must
// eventually deliver a close-edge publication through reconcile recovery, and
// must never be misclassified as an out-of-band delete.
func TestCachingStoreClosedHidingBackingRecoversCloseEdge(t *testing.T) {
	backing := &casBackingStore{Store: NewMemStore(), hideClosedFromGet: true}
	created, err := backing.Create(Bead{Title: "closed-hiding close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	notes = nil

	if err := cache.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	expireCacheMutationRecencyForTest(cache, created.ID)
	cache.runReconciliation()
	cache.runReconciliation()

	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want exactly one recovered bead.closed", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" {
		t.Fatalf("close payload = %+v ok=%v, want closed durable row", payload, ok)
	}
	if cache.pendingPublications.hasAny() {
		t.Fatal("pending intent still retained after successful close recovery")
	}
	if got := cache.Stats().LastProblem; strings.Contains(got, "out-of-band delete") {
		t.Fatalf("LastProblem = %q, want no false out-of-band delete for a recoverable close", got)
	}
}

// F1 (hardening): an intent retained as bead.updated whose bead is then closed
// out-of-band on a closed-hiding backing must still be recovered through the
// closed projection — regardless of the retained event type — never falsely
// cleared as an out-of-band delete.
func TestCachingStoreClosedHidingBackingRecoversUpdateRetainedIntent(t *testing.T) {
	backing := &casBackingStore{Store: NewMemStore(), hideClosedFromGet: true}
	created, err := backing.Create(Bead{Title: "update-then-hidden-close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	notes = nil

	// A failed-refresh Update retains a bead.updated intent while the bead is open.
	backing.failNextGet = true
	title := "pending update"
	if err := cache.Update(created.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update(failed refresh): %v", err)
	}
	if pub, ok := cache.pendingPublications.current(created.ID); !ok || pub.containsEvent("bead.closed") {
		t.Fatalf("intent = %+v ok=%v, want a retained non-close intent for the hardening path", pub, ok)
	}

	// The bead is closed out-of-band; the closed-hiding backing hides it from Get.
	if err := backing.Close(created.ID); err != nil {
		t.Fatalf("out-of-band Close: %v", err)
	}

	expireCacheMutationRecencyForTest(cache, created.ID)
	cache.runReconciliation()
	cache.runReconciliation()

	if got := cache.Stats().LastProblem; strings.Contains(got, "out-of-band delete") {
		t.Fatalf("LastProblem = %q, want no false out-of-band delete for a recoverable hidden close", got)
	}
	if cache.pendingPublications.hasAny() {
		t.Fatal("pending intent still retained after the closed projection recovered it")
	}
	sawClose := false
	for _, note := range notes {
		if note.eventType == "bead.closed" && note.beadID == created.ID {
			sawClose = true
		}
	}
	if !sawClose {
		t.Fatalf("notifications = %+v, want a delivered close edge for the hidden out-of-band close", notes)
	}
}

// F2: resolving a pending gate must not let the resolving mutation's own current
// event overtake a strictly older notification enqueued for the same bead after
// the gate was reserved. The newest snapshot must deliver last.
func TestCachingStoreGateResolutionDeliversCurrentAfterIntermediate(t *testing.T) {
	backing := &casBackingStore{Store: NewMemStore()}
	created, err := backing.Create(Bead{Title: "gate-order target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	notes = nil

	// A failed-refresh Update reserves the pending gate without publishing.
	backing.failNextGet = true
	recoveredTitle := "recovered-update"
	if err := cache.Update(created.ID, UpdateOpts{Title: &recoveredTitle}); err != nil {
		t.Fatalf("Update(failed refresh): %v", err)
	}
	if !cache.pendingPublications.hasAny() {
		t.Fatal("failed-refresh Update did not reserve a pending gate")
	}
	if len(notes) != 0 {
		t.Fatalf("failed-refresh Update published %+v, want nothing until the gate resolves", notes)
	}

	// A strictly older non-resolving snapshot is enqueued behind the gate.
	intermediate := cloneBead(created)
	intermediate.Title = "intermediate-snapshot"
	if drain, _ := cache.enqueueOrderedChangeAt("bead.updated", intermediate, time.Now()); drain != nil {
		drain()
	}
	if len(notes) != 0 {
		t.Fatalf("intermediate snapshot published %+v, want it blocked behind the gate", notes)
	}

	// A SetMetadata with a successful refresh resolves the gate.
	if err := cache.SetMetadata(created.ID, "phase", "resolved"); err != nil {
		t.Fatalf("SetMetadata(resolving): %v", err)
	}

	if len(notes) != 3 {
		t.Fatalf("delivery = %+v, want 3 notifications [recovered, intermediate, current]", notes)
	}
	titleOf := func(n cacheWriteNotification) string {
		payload, ok := DecodeBeadEventPayload(n.payload)
		if !ok {
			t.Fatalf("DecodeBeadEventPayload failed: %s", n.payload)
		}
		return payload.Title
	}
	metaPhase := func(n cacheWriteNotification) string {
		payload, ok := DecodeBeadEventPayload(n.payload)
		if !ok {
			t.Fatalf("DecodeBeadEventPayload failed: %s", n.payload)
		}
		return payload.Metadata["phase"]
	}
	// The intermediate (older) snapshot must sit BEFORE the current one, and the
	// newest snapshot (carrying the resolved metadata) must deliver LAST.
	if titleOf(notes[1]) != "intermediate-snapshot" {
		t.Fatalf("notes[1] title = %q, want the intermediate snapshot delivered before the current event", titleOf(notes[1]))
	}
	if metaPhase(notes[2]) != "resolved" {
		t.Fatalf("notes[2] = %+v, want the newest (resolved) snapshot delivered last", notes[2])
	}
	if metaPhase(notes[1]) == "resolved" {
		t.Fatal("the older intermediate snapshot was overtaken by the newer resolved snapshot")
	}
}

// getFailFirstNStore fails Get for its first n calls with a transport error,
// then delegates. Update and List always delegate. It models a bead created
// out-of-band whose pre-write snapshot and post-write refresh both fail, while a
// later reconcile targeted read succeeds.
type getFailFirstNStore struct {
	*MemStore
	remainingGetFails int
}

func (s *getFailFirstNStore) Get(id string) (Bead, error) {
	if s.remainingGetFails > 0 {
		s.remainingGetFails--
		return Bead{}, errors.New("injected transport read failure")
	}
	return s.MemStore.Get(id)
}

// F4: when reconcile first observes a bead (a synthesized bead.created) while a
// pending [updated] intent exists for a bead this cache never held, the creation
// edge must survive.
func TestCachingStoreFirstObservationUpdatePreservesCreatedEdge(t *testing.T) {
	backing := &getFailFirstNStore{MemStore: NewMemStore()}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Bead created out-of-band after prime, so the cache never held it.
	created, err := backing.Create(Bead{Title: "out-of-band creation"})
	if err != nil {
		t.Fatalf("Create out-of-band: %v", err)
	}
	notes = nil

	// The Update's pre-read Get and post-write refresh Get both fail.
	backing.remainingGetFails = 2
	title := "first-contact update"
	if err := cache.Update(created.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update(first observation): %v", err)
	}
	if !cache.pendingPublications.hasAny() {
		t.Fatal("first-observation Update did not retain a pending intent")
	}

	expireCacheMutationRecencyForTest(cache, created.ID)
	cache.runReconciliation()
	cache.runReconciliation()

	if len(notes) != 1 || notes[0].eventType != "bead.created" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want exactly one first-observation bead.created", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.ID != created.ID {
		t.Fatalf("created payload = %+v ok=%v, want the complete recovered bead", payload, ok)
	}
}

// laggedCloseScanStore models the F5 visibility lag: after a conditional close,
// the targeted Get already sees the closed row while the next active full scan
// still returns the pre-close OPEN row.
type laggedCloseScanStore struct {
	*postCommitConditionalRefreshFailStore
	staleOpen *Bead
}

func (s *laggedCloseScanStore) List(query ListQuery) ([]Bead, error) {
	if !query.IncludeClosed && s.staleOpen != nil {
		row := cloneBead(*s.staleOpen)
		s.staleOpen = nil
		return []Bead{row}, nil
	}
	return s.postCommitConditionalRefreshFailStore.List(query)
}

// F5: a recovered close plus a lagging full scan that still lists the row as open
// must not deliver bead.created after bead.closed for the same bead. The close
// supersedes the stale first-observation created.
func TestCachingStoreRecoveredCloseSupersedesLaggingCreatedScan(t *testing.T) {
	backing := &laggedCloseScanStore{
		postCommitConditionalRefreshFailStore: &postCommitConditionalRefreshFailStore{
			casBackingStore: &casBackingStore{Store: NewMemStore()},
		},
	}
	created, err := backing.Create(Bead{Title: "lagged close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	openRow, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get open row: %v", err)
	}
	notes = nil

	// CloseIfMatch commits; its post-close refresh fails (retains [bead.closed]).
	if err := cache.CloseIfMatch(created.ID, openRow.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}
	if !cache.pendingPublications.hasAny() {
		t.Fatal("failed-refresh CloseIfMatch did not retain a close intent")
	}

	// The next active full scan lags and still returns the pre-close OPEN row.
	backing.staleOpen = &openRow
	expireCacheMutationRecencyForTest(cache, created.ID)
	cache.runReconciliation()
	cache.runReconciliation()

	createdEdges := 0
	closedEdges := 0
	for _, note := range notes {
		switch note.eventType {
		case "bead.created":
			createdEdges++
		case "bead.closed":
			closedEdges++
		}
	}
	if createdEdges != 0 {
		t.Fatalf("notifications = %+v, want no bead.created after a recovered close", notes)
	}
	if closedEdges != 1 {
		t.Fatalf("notifications = %+v, want exactly one bead.closed", notes)
	}
	if got := notes[len(notes)-1].eventType; got != "bead.closed" {
		t.Fatalf("last delivered event = %q, want the close to be the terminal edge", got)
	}
}

// persistentGetFailStore fails every Get with a transport error while List
// succeeds, modeling a pending intent whose targeted recovery never converges.
type persistentGetFailStore struct {
	*MemStore
	failGet bool
}

func (s *persistentGetFailStore) Get(id string) (Bead, error) {
	if s.failGet {
		return Bead{}, errors.New("persistent targeted read fault")
	}
	return s.MemStore.Get(id)
}

// F6: a persistently unreadable pending intent must back off the pending
// reconcile deadline (bounded) instead of pinning full-scan reconciliation at
// the poll floor, and a successful recovery must reset the backoff.
func TestCachingStorePendingRecoveryBackoffBounded(t *testing.T) {
	interval := cacheReconcileIntervalSmall
	if got := pendingRecoveryBackoff(interval, 0); got != interval {
		t.Fatalf("pendingRecoveryBackoff(_, 0) = %v, want the eager one-interval deadline %v", got, interval)
	}
	if pendingRecoveryBackoff(interval, 2) <= pendingRecoveryBackoff(interval, 1) {
		t.Fatal("pendingRecoveryBackoff must grow with consecutive failures")
	}
	if got := pendingRecoveryBackoff(interval, 100); got != cacheReconcileFailureBackoff {
		t.Fatalf("pendingRecoveryBackoff(_, 100) = %v, want it clamped at %v", got, cacheReconcileFailureBackoff)
	}

	base := NewMemStore()
	target, err := base.Create(Bead{Title: "backoff target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(base, func(string, string, json.RawMessage) {})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	now := time.Now()
	cache.mu.Lock()
	cache.noteLocalMutationLocked(target.ID)
	cache.mu.Unlock()
	cache.retainPendingPublicationAt(target.ID, "bead.updated", now.Add(-2*cacheReconcileIntervalSmall), true)

	cache.mu.Lock()
	cache.lastFreshAt = now
	cache.stats.LastReconcileAt = now
	cache.mu.Unlock()

	// With no recorded failures the pending anchor keeps the eager deadline (0).
	if got := cache.nextReconcileDelay(now); got != 0 {
		t.Fatalf("eager first deadline nextReconcileDelay = %v, want 0", got)
	}

	// Consecutive failed recoveries back the pending deadline off past the floor.
	cache.mu.Lock()
	for i := 0; i < 4; i++ {
		cache.recordPendingRecoveryOutcomeLocked(now, true, false, true)
	}
	failures := cache.pendingRecoveryFailures
	cache.mu.Unlock()
	if failures == 0 {
		t.Fatal("consecutive failed recoveries did not advance pendingRecoveryFailures")
	}
	if got := cache.nextReconcileDelay(now); got == 0 {
		t.Fatal("a persistently unreadable intent still pinned nextReconcileDelay at 0")
	}

	// A successful recovery resets the backoff to the eager deadline.
	cache.mu.Lock()
	cache.recordPendingRecoveryOutcomeLocked(now, true, true, false)
	reset := cache.pendingRecoveryFailures
	cache.mu.Unlock()
	if reset != 0 {
		t.Fatalf("pendingRecoveryFailures = %d after a successful recovery, want 0", reset)
	}
	if got := cache.nextReconcileDelay(now); got != 0 {
		t.Fatalf("after reset nextReconcileDelay = %v, want the eager 0 deadline", got)
	}
}

// F6 (integration): a real reconcile pass whose targeted recovery read keeps
// failing must advance pendingRecoveryFailures, and a pass that finally reads
// the row must reset it and deliver the pending edge.
func TestCachingStorePendingRecoveryFailuresAdvanceAndReset(t *testing.T) {
	backing := &persistentGetFailStore{MemStore: NewMemStore()}
	created, err := backing.Create(Bead{Title: "persistent-fault target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// A failed-refresh Update retains a pending [updated] intent.
	backing.failGet = true
	title := "faulted update"
	if err := cache.Update(created.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	notes = nil

	for i := 0; i < 3; i++ {
		expireCacheMutationRecencyForTest(cache, created.ID)
		cache.runReconciliation()
	}
	if got := cache.Stats().PendingRecoveryFailures; got == 0 {
		t.Fatal("persistent recovery failures did not advance the backoff counter")
	}

	// Once the targeted read converges, recovery delivers the edge and resets.
	backing.failGet = false
	expireCacheMutationRecencyForTest(cache, created.ID)
	cache.runReconciliation()
	if got := cache.Stats().PendingRecoveryFailures; got != 0 {
		t.Fatalf("PendingRecoveryFailures = %d after a successful recovery, want 0", got)
	}
	if len(notes) == 0 {
		t.Fatal("recovered pending intent delivered no notification")
	}
}
