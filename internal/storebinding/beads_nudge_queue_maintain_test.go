package storebinding

// Maintain is the owner-run pass, so its failure modes are the ones a
// scheduled caller cannot see coming: one stubborn record that stops the whole
// pass, a pass that runs past the owner's deadline, and a sweep that tries to
// drain a city's entire history in one call.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// failingUpdateStore is the SQLite store with UpdateIfMatch failing for one
// bead, so a pass can be shown to carry on past a record it cannot write.
type failingUpdateStore struct {
	*beads.SQLiteStore
	failID string
	err    error
}

func (s *failingUpdateStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	if id == s.failID {
		return s.err
	}
	return s.SQLiteStore.UpdateIfMatch(id, expectedRevision, opts)
}

// failingDeleteStore is the SQLite store with DeleteIfMatch failing for one
// bead, so the sweep can be shown to carry on past a record it cannot delete.
type failingDeleteStore struct {
	*beads.SQLiteStore
	failID string
	err    error
}

func (s *failingDeleteStore) DeleteIfMatch(id string, expectedRevision int64) error {
	if id == s.failID {
		return s.err
	}
	return s.SQLiteStore.DeleteIfMatch(id, expectedRevision)
}

// cancelingUpdateStore is the SQLite store that ends a pass's context once
// a write lands, so the live pass can be shown to stop between transitions
// and not only before the first. It is disarmed while cancel is nil.
type cancelingUpdateStore struct {
	*beads.SQLiteStore
	cancel context.CancelFunc
}

func (s *cancelingUpdateStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	err := s.SQLiteStore.UpdateIfMatch(id, expectedRevision, opts)
	if err == nil && s.cancel != nil {
		s.cancel()
	}
	return err
}

// cancelingDeleteStore is the SQLite store that ends a pass's context once a
// delete lands, so the sweep can be shown to stop between deletes. It is
// disarmed while cancel is nil.
type cancelingDeleteStore struct {
	*beads.SQLiteStore
	cancel context.CancelFunc
}

func (s *cancelingDeleteStore) DeleteIfMatch(id string, expectedRevision int64) error {
	err := s.SQLiteStore.DeleteIfMatch(id, expectedRevision)
	if err == nil && s.cancel != nil {
		s.cancel()
	}
	return err
}

func openSQLiteNudgeQueueStore(t *testing.T) *beads.SQLiteStore {
	t.Helper()
	store, ok := openNudgeQueueStore(t, t.TempDir()).(*beads.SQLiteStore)
	if !ok {
		t.Fatalf("OpenSQLiteStore returned a store that is not *beads.SQLiteStore")
	}
	return store
}

// TestBeadsNudgeQueueMaintainContinuesPastAFailedWrite pins that one record
// the pass cannot write fails only that record: the rest of the pass runs, and
// the failure is returned naming it.
func TestBeadsNudgeQueueMaintainContinuesPastAFailedWrite(t *testing.T) {
	backing := openSQLiteNudgeQueueStore(t)
	failure := errors.New("disk said no")
	store := &failingUpdateStore{SQLiteStore: backing, failID: nudgeQueueBeadID("n-stuck"), err: failure}
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	for _, id := range []string{"n-stuck", "n-movable"} {
		item := testNudgeItem(id, "", now)
		item.ExpiresAt = now.Add(time.Minute)
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	_, err := queue.Maintain(context.Background(), now.Add(2*time.Minute))
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "n-stuck") {
		t.Fatalf("Maintain = %v, want the failed write reported with its nudge", err)
	}
	moved, getErr := backing.Get(nudgeQueueBeadID("n-movable"))
	if getErr != nil {
		t.Fatalf("Get movable: %v", getErr)
	}
	if moved.Metadata[nudgeMetaQueueState] != nudgeQueueStateDead {
		t.Fatalf("movable record queue_state = %q, want the pass to carry on past the failed write", moved.Metadata[nudgeMetaQueueState])
	}
}

// TestBeadsNudgeQueueMaintainStopsWhenItsContextEnds pins the time bound: a
// pass whose context has ended does no work and says why.
func TestBeadsNudgeQueueMaintainStopsWhenItsContextEnds(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	item := testNudgeItem("n-expiring", "", now)
	item.ExpiresAt = now.Add(time.Minute)
	if err := queue.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := queue.Maintain(ctx, now.Add(2*time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Maintain with an ended context = %v, want context.Canceled", err)
	}
	record, err := store.Get(nudgeQueueBeadID("n-expiring"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record.Metadata[nudgeMetaQueueState] != nudgeQueueStateLive {
		t.Fatalf("queue_state = %q, want a pass whose context ended to do nothing", record.Metadata[nudgeMetaQueueState])
	}
}

// TestBeadsNudgeQueueMaintainStopsBetweenTransitions pins the bound inside
// the live pass: a context that ends once the first transition lands stops the
// pass before the next one, so an owner's deadline cuts a long pass short
// rather than only refusing to start it.
func TestBeadsNudgeQueueMaintainStopsBetweenTransitions(t *testing.T) {
	store := &cancelingUpdateStore{SQLiteStore: openSQLiteNudgeQueueStore(t)}
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	ids := []string{"n-expiring-0", "n-expiring-1"}
	for _, id := range ids {
		item := testNudgeItem(id, "", now)
		item.ExpiresAt = now.Add(time.Minute)
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.cancel = cancel
	if _, err := queue.Maintain(ctx, now.Add(2*time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Maintain whose context ended mid-pass = %v, want context.Canceled", err)
	}
	states := map[string]string{}
	for _, id := range ids {
		record, err := store.Get(nudgeQueueBeadID(id))
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		states[id] = record.Metadata[nudgeMetaQueueState]
	}
	dead, live := 0, 0
	for _, state := range states {
		switch state {
		case nudgeQueueStateDead:
			dead++
		case nudgeQueueStateLive:
			live++
		}
	}
	if dead != 1 || live != 1 {
		t.Fatalf("queue_state after a pass canceled at its first transition = %v, want one record dead-lettered and the other still live", states)
	}
}

// TestBeadsNudgeQueueSweepIsBoundedOldestFirst pins the sweep's batch: one pass
// deletes at most a batch of retired records, oldest first, says when it
// filled the batch, and the next pass takes the rest.
func TestBeadsNudgeQueueSweepIsBoundedOldestFirst(t *testing.T) {
	backing := openNudgeQueueStore(t, t.TempDir())
	recorder := &claimingOpRecorder{OpRecordingStore: beadstest.NewOpRecordingStore(backing), claimer: backing.(beadsAssignmentClaimer)}
	queue := newNudgeQueue(t, recorder)
	queue.(*beadsNudgeQueue).sweepBatch = 2
	now := time.Now().UTC()

	var ids []string
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("n-retired-%d", i)
		ids = append(ids, id)
		if err := queue.Rollback(nudgequeue.Item{ID: id, Agent: testNudgeAgent, Source: "test", CreatedAt: now}, "never landed"); err != nil {
			t.Fatalf("Rollback(%s): %v", id, err)
		}
	}
	later := now.Add(nudgeQueueTerminalRetention).Add(time.Minute)
	recorder.reset()
	report, err := queue.Maintain(context.Background(), later)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if !report.Remaining {
		t.Fatalf("a pass that filled its batch reported no remaining work")
	}
	if got := queueRecordIDs(t, backing); len(got) != 1 || got[0] != ids[2] {
		t.Fatalf("records after one bounded pass = %v, want only the newest %s left", got, ids[2])
	}
	for _, op := range recorder.Ops() {
		if op.Kind != beadstest.OpList || op.Query.Status != "closed" {
			continue
		}
		query := op.Query
		if query.Limit != 2 || query.UpdatedBefore.IsZero() || query.Metadata[nudgeMetaQueueState] != nudgeQueueStateTerminal || query.Sort != beads.SortCreatedAsc {
			t.Errorf("the sweep listed %+v; want an oldest-first, limited read of terminal records past the cutoff", query)
		}
	}
	report, err = queue.Maintain(context.Background(), later)
	if err != nil {
		t.Fatalf("second Maintain: %v", err)
	}
	if report.Remaining {
		t.Fatalf("a pass that swept the last record reported remaining work")
	}
	if got := queueRecordIDs(t, backing); len(got) != 0 {
		t.Fatalf("records after the second pass = %v, want none", got)
	}
}

// TestBeadsNudgeQueueSweepContinuesPastAFailedDelete pins that one retired
// record the sweep cannot delete fails only that record: every other due
// record is still deleted, and the failure is returned naming the record.
func TestBeadsNudgeQueueSweepContinuesPastAFailedDelete(t *testing.T) {
	backing := openSQLiteNudgeQueueStore(t)
	failure := errors.New("disk said no")
	store := &failingDeleteStore{SQLiteStore: backing, failID: nudgeQueueBeadID("n-retired-1"), err: failure}
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("n-retired-%d", i)
		if err := queue.Rollback(nudgequeue.Item{ID: id, Agent: testNudgeAgent, Source: "test", CreatedAt: now}, "never landed"); err != nil {
			t.Fatalf("Rollback(%s): %v", id, err)
		}
	}
	_, err := queue.Maintain(context.Background(), now.Add(nudgeQueueTerminalRetention).Add(time.Minute))
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), nudgeQueueBeadID("n-retired-1")) {
		t.Fatalf("Maintain = %v, want the failed delete reported with its record", err)
	}
	if got := queueRecordIDs(t, backing); len(got) != 1 || got[0] != "n-retired-1" {
		t.Fatalf("records after the sweep = %v, want only the undeletable n-retired-1 left", got)
	}
}

// TestBeadsNudgeQueueSweepStopsBetweenDeletes pins the bound inside the sweep:
// a context that ends once the first delete lands stops the sweep before the
// next one, and the pass reports the rest as remaining although it never
// filled its batch.
func TestBeadsNudgeQueueSweepStopsBetweenDeletes(t *testing.T) {
	backing := openSQLiteNudgeQueueStore(t)
	store := &cancelingDeleteStore{SQLiteStore: backing}
	queue := newNudgeQueue(t, store)
	// A batch the three records cannot fill, so only a sweep cut short can
	// report remaining work.
	queue.(*beadsNudgeQueue).sweepBatch = 8
	now := time.Now().UTC()

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("n-retired-%d", i)
		if err := queue.Rollback(nudgequeue.Item{ID: id, Agent: testNudgeAgent, Source: "test", CreatedAt: now}, "never landed"); err != nil {
			t.Fatalf("Rollback(%s): %v", id, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.cancel = cancel
	report, err := queue.Maintain(ctx, now.Add(nudgeQueueTerminalRetention).Add(time.Minute))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Maintain whose context ended mid-sweep = %v, want context.Canceled", err)
	}
	if !report.Remaining {
		t.Fatalf("a sweep cut short reported no remaining work")
	}
	if got := queueRecordIDs(t, backing); len(got) != 2 {
		t.Fatalf("records after a sweep canceled at its first delete = %v, want exactly one of the three deleted", got)
	}
}

// TestBeadsNudgeQueueStaleLeaseReclaimHasExactlyOneWinner races the
// revoke-before-claim path: every item carries a lease that has run out,
// several claimants revoke and reclaim at once, and Maintain revokes the same
// stale leases alongside them. Each round, every item must go to exactly one
// claimant.
func TestBeadsNudgeQueueStaleLeaseReclaimHasExactlyOneWinner(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	const items = 24
	for i := 0; i < items; i++ {
		if err := queue.Enqueue(testNudgeItem(fmt.Sprintf("n-%02d", i), "", now)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if claimed := claimAll(t, queue, now); len(claimed) != items {
		t.Fatalf("initial ClaimDue = %d items, want %d", len(claimed), items)
	}

	const (
		rounds      = 5
		claimants   = 8
		maintainers = 2
	)
	at := now
	for round := 0; round < rounds; round++ {
		// Every lease from the previous round has run out by now.
		at = at.Add(nudgeQueueClaimLease + time.Minute)
		var (
			mu      sync.Mutex
			winners = map[string][]int{}
			start   = make(chan struct{})
			wg      sync.WaitGroup
		)
		errs := make([]error, claimants+maintainers)
		for claimant := 0; claimant < claimants; claimant++ {
			wg.Add(1)
			go func(claimant int) {
				defer wg.Done()
				<-start
				claimed, err := queue.ClaimDue(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, at)
				if err != nil {
					errs[claimant] = err
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, item := range claimed {
					winners[item.ID] = append(winners[item.ID], claimant)
				}
			}(claimant)
		}
		for maintainer := 0; maintainer < maintainers; maintainer++ {
			wg.Add(1)
			go func(slot int) {
				defer wg.Done()
				<-start
				_, errs[slot] = queue.Maintain(context.Background(), at)
			}(claimants + maintainer)
		}
		close(start)
		wg.Wait()

		for slot, err := range errs {
			if err != nil {
				t.Fatalf("round %d: participant %d: %v", round, slot, err)
			}
		}
		if len(winners) != items {
			t.Fatalf("round %d: reclaimed %d distinct nudges, want all %d", round, len(winners), items)
		}
		for id, who := range winners {
			if len(who) != 1 {
				t.Fatalf("round %d: nudge %s was reclaimed by %d claimants %v; revoke-before-claim is not single-winner", round, id, len(who), who)
			}
		}
	}
}

// TestBeadsNudgeQueueSweepDeletesOverAnUnstampedList pins the sweep over a
// store whose List carries no revision, as the native store's does: the delete
// presents the revision of a fresh read, so a retired record past retention is
// removed rather than skipped as stale on every pass.
func TestBeadsNudgeQueueSweepDeletesOverAnUnstampedList(t *testing.T) {
	backing := &unstampedListStore{SQLiteStore: openSQLiteNudgeQueueStore(t)}
	queue := newNudgeQueue(t, backing)
	now := time.Now().UTC()
	// Delivered and acked, so the record's revision has moved past zero.
	if err := queue.Enqueue(testNudgeItem("n-retired", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := queue.Ack([]string{"n-retired"}, "delivered", "", "commit"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if _, err := queue.Maintain(context.Background(), now.Add(nudgeQueueTerminalRetention).Add(time.Hour)); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if got := queueRecordIDs(t, backing.SQLiteStore); len(got) != 0 {
		t.Fatalf("records after the sweep = %v, want the retired record deleted", got)
	}
}

// A retired record whose delete is refused on every attempt, while it stays
// closed, terminal and past retention, is a fault Maintain reports rather than
// a record it silently keeps forever.
func TestBeadsNudgeQueueSweepReportsExhaustedRetries(t *testing.T) {
	backing := &refusingGuardedWriteStore{SQLiteStore: openSQLiteNudgeQueueStore(t)}
	queue := newNudgeQueue(t, backing)
	now := time.Now().UTC()
	if err := queue.Enqueue(testNudgeItem("n-retired", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := queue.Ack([]string{"n-retired"}, "delivered", "", "commit"); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	backing.refuseDeletes = true
	_, err := queue.Maintain(context.Background(), now.Add(nudgeQueueTerminalRetention).Add(time.Hour))
	if err == nil {
		t.Fatalf("Maintain = nil after %d refused deletes; want the exhausted retries reported", backing.refused)
	}
	if !strings.Contains(err.Error(), "revision conflicts") {
		t.Fatalf("Maintain error = %v, want it to name the revision conflicts", err)
	}
	if backing.refused != nudgeQueueCASAttempts {
		t.Fatalf("the sweep made %d guarded deletes, want %d", backing.refused, nudgeQueueCASAttempts)
	}
}
