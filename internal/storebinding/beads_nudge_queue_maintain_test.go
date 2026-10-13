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
