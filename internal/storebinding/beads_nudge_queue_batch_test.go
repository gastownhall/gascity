package storebinding

// The beads queue's batch claim and its owner reads, at the request level.
//
// The conformance corpus proves what ClaimBatch, Lookup, DueKeys and Empty
// answer. These tests pin what they ASK the store: a batch claim is the
// target's keyed reads plus one membership read for a resumed batch, every
// whole-queue read is indexed on the queue's own metadata and bypasses the
// controller's cache, and a resumed batch that loses one member to another
// claimant takes nothing at all.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// queueOnlyMetadataKeys are the metadata keys only queue records write. A
// queue List indexed on one of them visits queue rows alone.
var queueOnlyMetadataKeys = []string{nudgeMetaQueueKey, nudgeMetaQueueState, nudgeMetaDeliveryBatch}

// assertQueueListsAreIndexedAndLive requires every List in ops to be keyed on
// one queue-only metadata key and to bypass the cache.
func assertQueueListsAreIndexedAndLive(t *testing.T, verb string, ops []beadstest.RecordedOp) {
	t.Helper()
	for _, op := range ops {
		if op.Kind != beadstest.OpList {
			continue
		}
		keyed := false
		for _, key := range queueOnlyMetadataKeys {
			if op.Query.Metadata[key] != "" {
				keyed = true
			}
		}
		if !keyed {
			t.Errorf("%s listed %+v; want a read indexed on one of the queue-only keys %v, never the family label alone", verb, op.Query, queueOnlyMetadataKeys)
		}
		if !op.Query.Live {
			t.Errorf("%s listed %+v without Live; a caching store would hide another process's insert", verb, op.Query)
		}
	}
}

// listsOn returns the Lists in ops keyed on metadata key.
func listsOn(ops []beadstest.RecordedOp, key string) []beads.ListQuery {
	var queries []beads.ListQuery
	for _, op := range ops {
		if op.Kind == beadstest.OpList && op.Query.Metadata[key] != "" {
			queries = append(queries, op.Query)
		}
	}
	return queries
}

func countOps(ops []beadstest.RecordedOp, kind string) int {
	n := 0
	for _, op := range ops {
		if op.Kind == kind {
			n++
		}
	}
	return n
}

// TestBeadsNudgeQueueClaimBatchReadsAreKeyed pins the batch claim's requests:
// a fresh batch is one keyed List per queue key and nothing else; a resumed
// batch adds exactly one membership read, on its key. Neither changes with a
// thousand unrelated records in the store.
func TestBeadsNudgeQueueClaimBatchReadsAreKeyed(t *testing.T) {
	queue, recorder, backing := newRecordedNudgeQueue(t)
	now := time.Now().UTC()
	seedUnrelatedNudges(t, backing, 1000, now)
	for _, id := range []string{"n-a", "n-b"} {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	target := ClaimTarget{QueueKeys: targetQueueKeys}

	recorder.reset()
	fresh, err := queue.ClaimBatch(target, now)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(fresh) != 2 {
		t.Fatalf("ClaimBatch = %v, want both nudges", nudgeItemIDs(fresh))
	}
	ops := recorder.Ops()
	assertKeyedTargetReads(t, "ClaimBatch (new batch)", ops)
	assertQueueListsAreIndexedAndLive(t, "ClaimBatch (new batch)", ops)
	if got := len(listsOn(ops, nudgeMetaQueueKey)); got != len(targetQueueKeys) {
		t.Errorf("a new batch issued %d keyed reads, want one per queue key (%d)", got, len(targetQueueKeys))
	}
	if got := countOps(ops, beadstest.OpUpdateIfMatch); got != 2 {
		t.Errorf("a new batch of two issued %d guarded writes, want one per item", got)
	}
	if recorder.rows.Load() != 2 {
		t.Errorf("a new batch read %d rows, want the target's two records", recorder.rows.Load())
	}

	if err := queue.ReleaseClaims([]string{"n-a", "n-b"}, time.Time{}); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}
	recorder.reset()
	resumed, err := queue.ClaimBatch(target, now)
	if err != nil {
		t.Fatalf("resumed ClaimBatch: %v", err)
	}
	if len(resumed) != 2 || resumed[0].DeliveryBatch != fresh[0].DeliveryBatch {
		t.Fatalf("resumed ClaimBatch = %+v, want both nudges under %q", resumed, fresh[0].DeliveryBatch)
	}
	ops = recorder.Ops()
	assertQueueListsAreIndexedAndLive(t, "ClaimBatch (resumed)", ops)
	if got := len(listsOn(ops, nudgeMetaQueueKey)); got != len(targetQueueKeys) {
		t.Errorf("a resumed batch issued %d keyed reads, want one per queue key (%d)", got, len(targetQueueKeys))
	}
	membership := listsOn(ops, nudgeMetaDeliveryBatch)
	if len(membership) != 1 || membership[0].Metadata[nudgeMetaDeliveryBatch] != fresh[0].DeliveryBatch {
		t.Errorf("a resumed batch issued membership reads %+v, want exactly one on %q", membership, fresh[0].DeliveryBatch)
	}
	if got := countOps(ops, beadstest.OpList); got != len(targetQueueKeys)+1 {
		t.Errorf("a resumed batch issued %d Lists, want %d", got, len(targetQueueKeys)+1)
	}
}

// TestBeadsNudgeQueueNeverListsByLabelAlone drives every verb, the owner's
// included, and requires every List to be indexed on a queue-only key and
// Live.
func TestBeadsNudgeQueueNeverListsByLabelAlone(t *testing.T) {
	queue, recorder, backing := newRecordedNudgeQueue(t)
	now := time.Now().UTC()
	seedSameKeyNeighbors(t, backing, 5)
	reference := nudgequeue.Reference{Kind: "wait", ID: "w-1"}
	first := testNudgeItem("n-first", "", now)
	first.Reference = &reference
	second := testNudgeItem("n-second", "", now)
	second.Reference = &reference
	for _, item := range []nudgequeue.Item{first, second, testNudgeItem("n-other", "", now)} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	if err := queue.EnqueueDeferred(testNudgeItem("n-deferred", "", now)); err != nil {
		t.Fatalf("EnqueueDeferred: %v", err)
	}
	target := ClaimTarget{QueueKeys: []string{testNudgeAgent}}
	claimed, err := queue.ClaimBatch(target, now)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("ClaimBatch = %v, %v; want the three live nudges", nudgeItemIDs(claimed), err)
	}
	if err := queue.ReleaseClaims([]string{"n-other", "n-deferred"}, now.Add(time.Minute)); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}
	if err := queue.Ack([]string{"n-second"}, "delivered", "", "commit"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if _, err := queue.RecordFailure([]string{"n-first"}, fmt.Errorf("refused: %w", ErrNudgeDeliveryRejected), now); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if _, err := queue.ClaimDue(target, now.Add(time.Hour)); err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if _, err := queue.Lookup([]string{"n-first", "n-second"}, now); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, err := queue.DueKeys(now); err != nil {
		t.Fatalf("DueKeys: %v", err)
	}
	if _, err := queue.Empty(); err != nil {
		t.Fatalf("Empty: %v", err)
	}
	if _, err := queue.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := queue.Maintain(context.Background(), now.Add(48*time.Hour)); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	ops := recorder.Ops()
	if countOps(ops, beadstest.OpList) == 0 {
		t.Fatal("no verb listed anything; the assertion below would pass vacuously")
	}
	assertQueueListsAreIndexedAndLive(t, "the queue", ops)
}

// TestBeadsNudgeQueueEmptyIsTwoBoundedProbes pins Empty's cost: at most two
// indexed probes, pending first and then dead, each asking for one row in a
// shape the store can bound to one row.
func TestBeadsNudgeQueueEmptyIsTwoBoundedProbes(t *testing.T) {
	queue, recorder, backing := newRecordedNudgeQueue(t)
	now := time.Now().UTC()
	seedSameKeyNeighbors(t, backing, 5)

	probes := func() []beads.ListQuery {
		var queries []beads.ListQuery
		for _, op := range recorder.Ops() {
			if op.Kind == beadstest.OpList {
				queries = append(queries, op.Query)
			}
		}
		return queries
	}
	assertProbes := func(when string, want ...string) {
		t.Helper()
		got := probes()
		if len(got) != len(want) {
			t.Fatalf("Empty %s issued %d Lists %+v, want %d", when, len(got), got, len(want))
		}
		for i, query := range got {
			if query.Metadata[nudgeMetaQueueState] != want[i] || query.Limit != 1 || !query.Live {
				t.Errorf("Empty %s probe %d = %+v, want a Live Limit:1 probe on queue_state=%s", when, i, query, want[i])
			}
			// The native store pushes a limit down only when no residual
			// filter runs after its page is cut: every status, no type, both
			// tiers. Any of those would make each probe fetch every record in
			// its state and cut the answer in Go.
			if query.Type != "" || !query.IncludeClosed || query.Status != "" || query.TierMode != beads.TierBoth {
				t.Errorf("Empty %s probe %d = %+v, want a probe the native store can bound to one row (no Type, IncludeClosed, no Status, TierBoth)", when, i, query)
			}
		}
	}

	recorder.reset()
	if empty, err := queue.Empty(); err != nil || !empty {
		t.Fatalf("Empty on a queue with only other families = %v, %v; want true", empty, err)
	}
	assertProbes("on an empty queue", nudgeQueueStateLive, nudgeQueueStateDead)

	if err := queue.Enqueue(testNudgeItem("n-pending", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	recorder.reset()
	if empty, err := queue.Empty(); err != nil || empty {
		t.Fatalf("Empty with a pending nudge = %v, %v; want false", empty, err)
	}
	assertProbes("with a pending nudge", nudgeQueueStateLive)
}

// listFailingStore is the SQLite store with every List failing.
type listFailingStore struct {
	*beads.SQLiteStore
	err error
}

func (s *listFailingStore) List(beads.ListQuery) ([]beads.Bead, error) { return nil, s.err }

// TestBeadsNudgeQueueUnreadableQueueIsNotEmpty pins the hook fast path's
// fail-safe: a queue that cannot be read must not be skipped as empty.
func TestBeadsNudgeQueueUnreadableQueueIsNotEmpty(t *testing.T) {
	failure := errors.New("store unreachable")
	queue := newNudgeQueue(t, &listFailingStore{SQLiteStore: openSQLiteNudgeQueueStore(t), err: failure})
	empty, err := queue.Empty()
	if !errors.Is(err, failure) {
		t.Fatalf("Empty over an unreadable store error = %v, want the store's failure", err)
	}
	if empty {
		t.Fatal("Empty over an unreadable store = true, want an unreadable queue reported as not empty")
	}
	if _, err := queue.DueKeys(time.Now()); !errors.Is(err, failure) {
		t.Fatalf("DueKeys over an unreadable store = %v, want the store's failure", err)
	}
}

// TestBeadsNudgeQueueLookupIsOneGetPerID pins Lookup's cost and its
// quarantine: one Get per distinct id, no List, and a corrupt record gives a
// partial result beside the buckets it could read.
func TestBeadsNudgeQueueLookupIsOneGetPerID(t *testing.T) {
	queue, recorder, backing := newRecordedNudgeQueue(t)
	now := time.Now().UTC()
	if err := queue.Enqueue(testNudgeItem("n-good", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	seedCorruptNudgeRecord(t, backing, "n-corrupt", testNudgeAgent)
	recorder.reset()
	buckets, err := queue.Lookup([]string{"n-good", "n-corrupt", "n-unknown", "n-good"}, now)
	if !beads.IsPartialResult(err) || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
		t.Fatalf("Lookup error = %v, want a partial result naming the corrupt record", err)
	}
	if len(buckets) != 1 || buckets["n-good"] != nudgequeue.BucketPending {
		t.Fatalf("Lookup = %v, want n-good pending and nothing else", buckets)
	}
	ops := recorder.Ops()
	if got := countOps(ops, beadstest.OpGet); got != 3 {
		t.Errorf("Lookup of three distinct ids issued %d Gets, want 3", got)
	}
	if got := countOps(ops, beadstest.OpList); got != 0 {
		t.Errorf("Lookup issued %d Lists, want none", got)
	}
}

// TestBeadsNudgeQueueDueKeysReportsACorruptRecordAsPartial pins DueKeys'
// quarantine: a corrupt record does not hide every other key from the
// dispatcher.
func TestBeadsNudgeQueueDueKeysReportsACorruptRecordAsPartial(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()
	if err := queue.Enqueue(testNudgeItem("n-good", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	seedCorruptNudgeRecord(t, store, "n-corrupt", "beta/broken")
	keys, err := queue.DueKeys(now)
	if !beads.IsPartialResult(err) || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
		t.Fatalf("DueKeys error = %v, want a partial result naming the corrupt record", err)
	}
	if len(keys) != 1 || keys[0] != testNudgeAgent {
		t.Fatalf("DueKeys = %v, want the good record's key", keys)
	}
}

// TestBeadsNudgeQueueReleaseWithNotBeforeIsAGuardedUpdate pins the backoff
// release's write: one revision-fenced update that clears the lease, sets
// deliver_after and leaves the pin alone.
func TestBeadsNudgeQueueReleaseWithNotBeforeIsAGuardedUpdate(t *testing.T) {
	queue, recorder, backing := newRecordedNudgeQueue(t)
	now := time.Now().UTC()
	if err := queue.Enqueue(testNudgeItem("n-a", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := queue.ClaimBatch(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimBatch = %v, %v", nudgeItemIDs(claimed), err)
	}
	notBefore := now.Add(5 * time.Minute)
	recorder.reset()
	if err := queue.ReleaseClaims([]string{"n-a"}, notBefore); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}
	ops := recorder.Ops()
	if got := countOps(ops, beadstest.OpUpdateIfMatch); got != 1 {
		t.Errorf("a backoff release issued %d guarded updates, want 1", got)
	}
	if got := countOps(ops, beadstest.OpReleaseIfCurrent) + countOps(ops, beadstest.OpUpdate); got != 0 {
		t.Errorf("a backoff release issued %d unguarded or two-step writes, want none", got)
	}
	record, err := backing.Get(nudgeQueueBeadID("n-a"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record.Assignee != "" || record.Status != "open" {
		t.Errorf("released record assignee=%q status=%q, want the lease cleared", record.Assignee, record.Status)
	}
	if got := record.Metadata[nudgeMetaDeliverAfter]; got != formatNudgeTime(notBefore) {
		t.Errorf("released record deliver_after = %q, want %q", got, formatNudgeTime(notBefore))
	}
	if got := record.Metadata[nudgeMetaDeliveryBatch]; got != claimed[0].DeliveryBatch {
		t.Errorf("released record delivery_batch = %q, want the pin %q kept", got, claimed[0].DeliveryBatch)
	}
}

// TestBeadsNudgeQueueRejectionClearsThePin pins RecordFailure's write for a
// definitive rejection: the record loses its delivery_batch, and an ordinary
// failure keeps it.
func TestBeadsNudgeQueueRejectionClearsThePin(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()
	for _, id := range []string{"n-rejected", "n-timed-out"} {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	claimed, err := queue.ClaimBatch(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("ClaimBatch = %v, %v", nudgeItemIDs(claimed), err)
	}
	key := claimed[0].DeliveryBatch
	if _, err := queue.RecordFailure([]string{"n-rejected"}, fmt.Errorf("t3: %w", ErrNudgeDeliveryRejected), now); err != nil {
		t.Fatalf("RecordFailure(rejected): %v", err)
	}
	if _, err := queue.RecordFailure([]string{"n-timed-out"}, errors.New("timed out"), now); err != nil {
		t.Fatalf("RecordFailure(timed out): %v", err)
	}
	for id, want := range map[string]string{"n-rejected": "", "n-timed-out": key} {
		record, err := store.Get(nudgeQueueBeadID(id))
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got := record.Metadata[nudgeMetaDeliveryBatch]; got != want {
			t.Errorf("%s delivery_batch = %q, want %q", id, got, want)
		}
		if record.Metadata[nudgeMetaAttempts] != "1" {
			t.Errorf("%s attempts = %q, want the normal retry policy to charge one", id, record.Metadata[nudgeMetaAttempts])
		}
	}
}

// TestNudgeQueueCodecCarriesTheDeliveryBatch pins the codec: the pin is
// written under its queue-only key and read back.
func TestNudgeQueueCodecCarriesTheDeliveryBatch(t *testing.T) {
	now := time.Now().UTC()
	item := testNudgeItem("n-pinned", "", now)
	item.DeliveryBatch = "nudge-batch-0123456789abcdef"
	record := nudgeQueueRecord(item, nudgeQueueStateLive, nil)
	if got := record.Metadata[nudgeMetaDeliveryBatch]; got != item.DeliveryBatch {
		t.Fatalf("encoded delivery_batch = %q, want %q", got, item.DeliveryBatch)
	}
	entry, err := decodeNudgeQueueBead(record, now)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if entry.item.DeliveryBatch != item.DeliveryBatch {
		t.Fatalf("decoded DeliveryBatch = %q, want %q", entry.item.DeliveryBatch, item.DeliveryBatch)
	}
	unpinned := nudgeQueueRecord(testNudgeItem("n-unpinned", "", now), nudgeQueueStateLive, nil)
	if _, written := unpinned.Metadata[nudgeMetaDeliveryBatch]; written {
		t.Fatal("an unpinned record carries a delivery_batch key; it would be indexed under the empty batch")
	}
}

// stealingStore is the SQLite store with a rival ClaimDue taking one bead
// between the batch claim's membership read and its guarded write on that bead.
type stealingStore struct {
	*beads.SQLiteStore
	victim string
	rival  func()
	once   sync.Once
}

func (s *stealingStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if id == s.victim && opts.Assignee != nil && *opts.Assignee != "" {
		s.once.Do(s.rival)
	}
	return s.SQLiteStore.UpdateIfMatch(id, revision, opts)
}

// TestBeadsNudgeQueueResumedBatchThatLosesAMemberTakesNothing pins the lost
// write on a resumed batch: what the call took goes back, the pin stays, and
// the call reports no items and no error.
func TestBeadsNudgeQueueResumedBatchThatLosesAMemberTakesNothing(t *testing.T) {
	backing := openSQLiteNudgeQueueStore(t)
	plain := newNudgeQueue(t, backing)
	now := time.Now().UTC()
	target := ClaimTarget{QueueKeys: []string{testNudgeAgent}}
	for _, id := range []string{"n-a", "n-b"} {
		if err := plain.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	first, err := plain.ClaimBatch(target, now)
	if err != nil || len(first) != 2 {
		t.Fatalf("ClaimBatch = %v, %v", nudgeItemIDs(first), err)
	}
	key := first[0].DeliveryBatch
	if err := plain.ReleaseClaims([]string{"n-a", "n-b"}, time.Time{}); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}

	// n-a sorts first and is taken; the rival then claims n-b alone before
	// the batch claim's write on it lands.
	var rivalClaimed []nudgequeue.Item
	contended := &stealingStore{SQLiteStore: backing, victim: nudgeQueueBeadID("n-b")}
	contended.rival = func() {
		claimer := backing
		lease, err := newNudgeLease(now)
		if err != nil {
			t.Errorf("minting the rival lease: %v", err)
			return
		}
		if _, ok, err := claimer.Claim(nudgeQueueBeadID("n-b"), lease.assignee); err != nil || !ok {
			t.Errorf("the rival claim of n-b = %v, %v; want it to win", ok, err)
			return
		}
		rivalClaimed = append(rivalClaimed, nudgequeue.Item{ID: "n-b"})
	}
	queue := newNudgeQueue(t, contended)
	resumed, err := queue.ClaimBatch(target, now)
	if err != nil {
		t.Fatalf("ClaimBatch against a rival = %v, want no error: a contended batch waits", err)
	}
	if len(resumed) != 0 {
		t.Fatalf("ClaimBatch against a rival = %v, want nothing: the batch can no longer be claimed whole", nudgeItemIDs(resumed))
	}
	if len(rivalClaimed) != 1 {
		t.Fatal("the rival never ran; the test proved nothing")
	}
	record, err := backing.Get(nudgeQueueBeadID("n-a"))
	if err != nil {
		t.Fatalf("Get(n-a): %v", err)
	}
	if record.Assignee != "" || record.Status != "open" {
		t.Errorf("n-a assignee=%q status=%q, want what the losing call took released", record.Assignee, record.Status)
	}
	if got := record.Metadata[nudgeMetaDeliveryBatch]; got != key {
		t.Errorf("n-a delivery_batch = %q, want the pin %q kept", got, key)
	}
}

// TestBeadsNudgeQueueBatchAndItemClaimsHaveOneWinner races batch claims and
// item claims over one pinned batch. A batch claim returns the whole batch or
// nothing, and no item is ever handed to two claimants.
func TestBeadsNudgeQueueBatchAndItemClaimsHaveOneWinner(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()
	target := ClaimTarget{QueueKeys: []string{testNudgeAgent}}
	members := []string{"n-1", "n-2", "n-3", "n-4"}
	for _, id := range members {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	pinned, err := queue.ClaimBatch(target, now)
	if err != nil || len(pinned) != len(members) {
		t.Fatalf("ClaimBatch = %v, %v", nudgeItemIDs(pinned), err)
	}
	if err := queue.ReleaseClaims(members, time.Time{}); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}

	const claimants = 32
	var (
		mu      sync.Mutex
		winners = map[string]int{}
		wg      sync.WaitGroup
		start   = make(chan struct{})
	)
	for i := 0; i < claimants; i++ {
		wg.Add(1)
		go func(batch bool) {
			defer wg.Done()
			<-start
			var claimed []nudgequeue.Item
			var err error
			if batch {
				claimed, err = queue.ClaimBatch(target, now)
			} else {
				claimed, err = queue.ClaimDue(target, now)
			}
			if err != nil {
				t.Errorf("claim (batch=%v): %v", batch, err)
				return
			}
			if batch && len(claimed) != 0 && len(claimed) != len(members) {
				t.Errorf("ClaimBatch returned %v, a strict subset of the pinned batch", nudgeItemIDs(claimed))
			}
			mu.Lock()
			defer mu.Unlock()
			for _, item := range claimed {
				winners[item.ID]++
			}
		}(i%2 == 0)
	}
	close(start)
	wg.Wait()

	// Whatever the race left unclaimed (a batch claim that lost a member
	// gives back what it took) is still claimable exactly once.
	rest, err := queue.ClaimDue(target, now)
	if err != nil {
		t.Fatalf("final ClaimDue: %v", err)
	}
	for _, item := range rest {
		winners[item.ID]++
	}
	ids := make([]string, 0, len(winners))
	for id, count := range winners {
		ids = append(ids, id)
		if count != 1 {
			t.Errorf("%s was claimed %d times, want exactly one winner", id, count)
		}
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != strings.Join(members, ",") {
		t.Errorf("claimed %v, want every member exactly once", ids)
	}
}

// unstampedListStore is the SQLite store with its List leaving revisions
// unstamped, as the native store's does, and an optional rival that runs
// before the first Get of one bead.
type unstampedListStore struct {
	*beads.SQLiteStore
	victim string
	rival  func()
	once   sync.Once
}

func (s *unstampedListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	records, err := s.SQLiteStore.List(query)
	for i := range records {
		records[i].Revision = 0
	}
	return records, err
}

func (s *unstampedListStore) Get(id string) (beads.Bead, error) {
	if id == s.victim && s.rival != nil {
		s.once.Do(s.rival)
	}
	return s.SQLiteStore.Get(id)
}

// TestBeadsNudgeQueueClaimBatchOverAnUnstampedList pins the batch claim over
// a store whose List carries no revision. It reads each record again for the
// revision its guarded write presents, and a record that a rival claimed
// between the List and that read is skipped rather than overwritten at the
// rival's own fresh revision.
func TestBeadsNudgeQueueClaimBatchOverAnUnstampedList(t *testing.T) {
	now := time.Now().UTC()
	target := ClaimTarget{QueueKeys: []string{testNudgeAgent}}

	quiet := &unstampedListStore{SQLiteStore: openSQLiteNudgeQueueStore(t)}
	queue := newNudgeQueue(t, quiet)
	for _, id := range []string{"n-a", "n-b"} {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	claimed, err := queue.ClaimBatch(target, now)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("ClaimBatch over an unstamped List = %v, %v; want both nudges", nudgeItemIDs(claimed), err)
	}

	backing := openSQLiteNudgeQueueStore(t)
	contended := &unstampedListStore{SQLiteStore: backing}
	var rivalLease string
	contended.rival = func() {
		lease, err := newNudgeLease(now)
		if err != nil {
			t.Errorf("minting the rival lease: %v", err)
			return
		}
		if _, ok, err := backing.Claim(nudgeQueueBeadID("n-b"), lease.assignee); err != nil || !ok {
			t.Errorf("the rival claim of n-b = %v, %v; want it to win", ok, err)
			return
		}
		rivalLease = lease.assignee
	}
	queue = newNudgeQueue(t, contended)
	for _, id := range []string{"n-a", "n-b"} {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	// Armed only now: Enqueue reads the record too.
	contended.victim = nudgeQueueBeadID("n-b")
	claimed, err = queue.ClaimBatch(target, now)
	if err != nil {
		t.Fatalf("ClaimBatch against a rival: %v", err)
	}
	if ids := nudgeItemIDs(claimed); len(ids) != 1 || ids[0] != "n-a" {
		t.Fatalf("ClaimBatch against a rival = %v, want only n-a: n-b belongs to the rival", ids)
	}
	if rivalLease == "" {
		t.Fatal("the rival never ran; the test proved nothing")
	}
	record, err := backing.Get(nudgeQueueBeadID("n-b"))
	if err != nil {
		t.Fatalf("Get(n-b): %v", err)
	}
	if record.Assignee != rivalLease {
		t.Fatalf("n-b assignee = %q, want the rival's lease %q left in place", record.Assignee, rivalLease)
	}
}

// claimBatchPastARivalRoundTrip runs a new batch claim over an unstamped List
// of n-a and n-b while a rival, between that List and the claim's re-read of
// n-b, claims n-b and gives it back with metadata rewritten. Assignee, status
// and queue_state read back as the List saw them, so only the rewritten key
// tells the claim its decision is stale. It returns the claimed items and
// n-b's record as the rival left it.
func claimBatchPastARivalRoundTrip(t *testing.T, now time.Time, rewritten map[string]string) ([]nudgequeue.Item, beads.Bead) {
	t.Helper()
	backing := openSQLiteNudgeQueueStore(t)
	contended := &unstampedListStore{SQLiteStore: backing}
	victim := nudgeQueueBeadID("n-b")
	ran := false
	contended.rival = func() {
		lease, err := newNudgeLease(now)
		if err != nil {
			t.Errorf("minting the rival lease: %v", err)
			return
		}
		if _, ok, err := backing.Claim(victim, lease.assignee); err != nil || !ok {
			t.Errorf("the rival claim of n-b = %v, %v; want it to win", ok, err)
			return
		}
		if err := backing.Update(victim, nudgeQueueUnclaim(rewritten)); err != nil {
			t.Errorf("the rival release of n-b: %v", err)
			return
		}
		ran = true
	}
	queue := newNudgeQueue(t, contended)
	for _, id := range []string{"n-a", "n-b"} {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	before, err := backing.Get(victim)
	if err != nil {
		t.Fatalf("Get(n-b) before the claim: %v", err)
	}
	// Armed only now: Enqueue reads the record too.
	contended.victim = victim
	claimed, err := queue.ClaimBatch(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now)
	if err != nil {
		t.Fatalf("ClaimBatch against a rival: %v", err)
	}
	if !ran {
		t.Fatal("the rival never ran; the test proved nothing")
	}
	after, err := backing.Get(victim)
	if err != nil {
		t.Fatalf("Get(n-b) after the claim: %v", err)
	}
	for _, item := range claimed {
		if item.ID == "n-b" {
			t.Fatalf("ClaimBatch claimed n-b straight through the rival's rewrite %v; n-b is now %q in batch %q",
				rewritten, after.Assignee, after.Metadata[nudgeMetaDeliveryBatch])
		}
	}
	if after.Assignee != before.Assignee || after.Status != before.Status ||
		after.Metadata[nudgeMetaQueueState] != before.Metadata[nudgeMetaQueueState] {
		t.Fatalf("the rival left n-b at assignee %q status %q queue_state %q, want %q %q %q: only the rewritten key may differ",
			after.Assignee, after.Status, after.Metadata[nudgeMetaQueueState],
			before.Assignee, before.Status, before.Metadata[nudgeMetaQueueState])
	}
	return claimed, after
}

// TestBeadsNudgeQueueClaimBatchSkipsARivalBackoff pins the batch claim's
// re-read on deliver_after: a rival that claimed n-b and failed it into a
// backoff between the List and the claim's write leaves n-b not yet due, and
// the claim skips it rather than claiming straight through the backoff.
func TestBeadsNudgeQueueClaimBatchSkipsARivalBackoff(t *testing.T) {
	now := time.Now().UTC()
	backoff := formatNudgeTime(now.Add(time.Hour))
	claimed, record := claimBatchPastARivalRoundTrip(t, now, map[string]string{nudgeMetaDeliverAfter: backoff})
	if ids := nudgeItemIDs(claimed); len(ids) != 1 || ids[0] != "n-a" {
		t.Fatalf("ClaimBatch against a rival backoff = %v, want only n-a: n-b is not due until %s", ids, backoff)
	}
	if record.Metadata[nudgeMetaDeliveryBatch] != "" {
		t.Fatalf("n-b delivery_batch = %q, want it left unpinned", record.Metadata[nudgeMetaDeliveryBatch])
	}
	if record.Metadata[nudgeMetaDeliverAfter] != backoff {
		t.Fatalf("n-b deliver_after = %q, want the rival's backoff %q", record.Metadata[nudgeMetaDeliverAfter], backoff)
	}
}

// TestBeadsNudgeQueueClaimBatchSkipsARivalPin pins the batch claim's re-read
// on delivery_batch: a rival that claimed n-b into its own batch and released
// it with the pin kept between the List and the claim's write leaves n-b a
// member of that batch, and a new batch skips it rather than repinning it.
func TestBeadsNudgeQueueClaimBatchSkipsARivalPin(t *testing.T) {
	now := time.Now().UTC()
	rivalKey, err := newNudgeQueueBatchKey()
	if err != nil {
		t.Fatalf("minting the rival batch key: %v", err)
	}
	claimed, record := claimBatchPastARivalRoundTrip(t, now, map[string]string{nudgeMetaDeliveryBatch: rivalKey})
	if ids := nudgeItemIDs(claimed); len(ids) != 1 || ids[0] != "n-a" {
		t.Fatalf("ClaimBatch against a rival pin = %v, want only n-a: n-b belongs to batch %q", ids, rivalKey)
	}
	if record.Metadata[nudgeMetaDeliveryBatch] != rivalKey {
		t.Fatalf("n-b delivery_batch = %q, want the rival's batch %q left in place", record.Metadata[nudgeMetaDeliveryBatch], rivalKey)
	}
}

// openCachedUnstampedNudgeQueue is the controller's view of a nudges store
// whose List leaves revisions unstamped: a primed CachingStore over it. A Live
// List installs the unstamped row in the cache, and the cache's Get then serves
// revision 0, so no guarded write may trust the first revision it is handed.
func openCachedUnstampedNudgeQueue(t *testing.T) (*unstampedListStore, NudgeQueue) {
	t.Helper()
	backing := &unstampedListStore{SQLiteStore: openSQLiteNudgeQueueStore(t)}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return backing, newNudgeQueue(t, cache)
}

// TestBeadsNudgeQueueClaimBatchOverACacheOfAnUnstampedList pins the batch
// claim over the controller's cache of a store whose List carries no revision:
// a new batch, a batch resumed after a release, and a backoff release after a
// Live List of the in-flight rows all land rather than reading as lost races.
func TestBeadsNudgeQueueClaimBatchOverACacheOfAnUnstampedList(t *testing.T) {
	backing, queue := openCachedUnstampedNudgeQueue(t)
	other := newNudgeQueue(t, backing) // another process writes straight to the store
	now := time.Now().UTC()
	target := ClaimTarget{QueueKeys: []string{testNudgeAgent}}
	for _, id := range []string{"n-a", "n-b"} {
		if err := other.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	fresh, err := queue.ClaimBatch(target, now)
	if err != nil || len(fresh) != 2 {
		t.Fatalf("new ClaimBatch over the cache = %v, %v; want both nudges", nudgeItemIDs(fresh), err)
	}
	key := fresh[0].DeliveryBatch
	if err := queue.ReleaseClaims([]string{"n-a", "n-b"}, time.Time{}); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}
	resumed, err := queue.ClaimBatch(target, now)
	if err != nil || len(resumed) != 2 || resumed[0].DeliveryBatch != key {
		t.Fatalf("resumed ClaimBatch over the cache = %+v, %v; want both nudges under %q", resumed, err, key)
	}
	if _, _, _, err := queue.ListFor(target, now); err != nil { // a Live List of the in-flight rows
		t.Fatalf("ListFor: %v", err)
	}
	notBefore := now.Add(10 * time.Minute)
	if err := queue.ReleaseClaims([]string{"n-a", "n-b"}, notBefore); err != nil {
		t.Fatalf("ReleaseClaims(notBefore): %v", err)
	}
	for _, id := range []string{"n-a", "n-b"} {
		assertReleasedUntil(t, backing, id, notBefore)
	}
}

// TestBeadsNudgeQueueReleaseWithNotBeforeOverACacheOfAnUnstampedList pins the
// backoff release after a ClaimDue: a Live List that put the unstamped row in
// the cache must not turn the release into a silent no-op.
func TestBeadsNudgeQueueReleaseWithNotBeforeOverACacheOfAnUnstampedList(t *testing.T) {
	backing, queue := openCachedUnstampedNudgeQueue(t)
	now := time.Now().UTC()
	target := ClaimTarget{QueueKeys: []string{testNudgeAgent}}
	if err := queue.Enqueue(testNudgeItem("n-a", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if claimed, err := queue.ClaimDue(target, now); err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDue = %v, %v", nudgeItemIDs(claimed), err)
	}
	if _, _, _, err := queue.ListFor(target, now); err != nil {
		t.Fatalf("ListFor: %v", err)
	}
	notBefore := now.Add(10 * time.Minute)
	if err := queue.ReleaseClaims([]string{"n-a"}, notBefore); err != nil {
		t.Fatalf("ReleaseClaims(notBefore): %v", err)
	}
	assertReleasedUntil(t, backing, "n-a", notBefore)
}

// TestBeadsNudgeQueueReleaseWithNotBeforeLeavesAnUnheldItem pins the guard on
// the backoff release: it moves only an item still held in flight, so an item
// nobody holds keeps its deliver-after and stays due.
func TestBeadsNudgeQueueReleaseWithNotBeforeLeavesAnUnheldItem(t *testing.T) {
	backing := openSQLiteNudgeQueueStore(t)
	queue := newNudgeQueue(t, backing)
	now := time.Now().UTC()
	target := ClaimTarget{QueueKeys: []string{testNudgeAgent}}
	if err := queue.Enqueue(testNudgeItem("n-a", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := queue.ReleaseClaims([]string{"n-a"}, now.Add(time.Minute)); err != nil {
		t.Fatalf("ReleaseClaims of a pending item: %v", err)
	}
	record, err := backing.Get(nudgeQueueBeadID("n-a"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record.Metadata[nudgeMetaDeliverAfter] == formatNudgeTime(now.Add(time.Minute)) {
		t.Fatalf("ReleaseClaims(notBefore) moved the deliver_after of an item nobody held")
	}
	if claimed, err := queue.ClaimDue(target, now); err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDue after a no-op release = %v, %v; want the item still due", nudgeItemIDs(claimed), err)
	}
}

func assertReleasedUntil(t *testing.T, store beads.Store, id string, notBefore time.Time) {
	t.Helper()
	record, err := store.Get(nudgeQueueBeadID(id))
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if record.Assignee != "" || record.Status != "open" || record.Metadata[nudgeMetaDeliverAfter] != formatNudgeTime(notBefore) {
		t.Errorf("%s after ReleaseClaims(notBefore) status=%q assignee=%q deliver_after=%q; want the lease cleared and deliver_after %q",
			id, record.Status, record.Assignee, record.Metadata[nudgeMetaDeliverAfter], formatNudgeTime(notBefore))
	}
}

// refusingGuardedWriteStore refuses every revision-fenced write while leaving
// the record itself unchanged, as a store whose revision token never matches
// what its reads hand out would. Every retry sees the decided fields intact,
// so only the attempt bound ends the loop.
type refusingGuardedWriteStore struct {
	*beads.SQLiteStore
	refuseUpdates bool
	refuseDeletes bool
	refused       int
}

func (s *refusingGuardedWriteStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if !s.refuseUpdates {
		return s.SQLiteStore.UpdateIfMatch(id, revision, opts)
	}
	s.refused++
	return &beads.PreconditionFailedError{ID: id, Expected: revision, Current: revision + 1}
}

func (s *refusingGuardedWriteStore) DeleteIfMatch(id string, revision int64) error {
	if !s.refuseDeletes {
		return s.SQLiteStore.DeleteIfMatch(id, revision)
	}
	s.refused++
	return &beads.PreconditionFailedError{ID: id, Expected: revision, Current: revision + 1}
}

// A batch claim whose guarded write is refused on every attempt, while the
// record never changes, is a fault: reporting it as an idle claim would leave
// the dispatcher looking idle while the work sits stuck.
func TestBeadsNudgeQueueClaimBatchReportsExhaustedRetries(t *testing.T) {
	backing := &refusingGuardedWriteStore{SQLiteStore: openSQLiteNudgeQueueStore(t)}
	queue := newNudgeQueue(t, backing)
	now := time.Now().UTC()
	if err := queue.Enqueue(testNudgeItem("n-a", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	backing.refuseUpdates = true
	claimed, err := queue.ClaimBatch(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now)
	if err == nil {
		t.Fatalf("ClaimBatch = %v, nil after %d refused guarded writes; want the exhausted retries reported", nudgeItemIDs(claimed), backing.refused)
	}
	if !strings.Contains(err.Error(), "revision conflicts") {
		t.Fatalf("ClaimBatch error = %v, want it to name the revision conflicts", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("ClaimBatch claimed %v with every write refused", nudgeItemIDs(claimed))
	}
	if backing.refused != nudgeQueueCASAttempts {
		t.Fatalf("ClaimBatch made %d guarded writes, want %d", backing.refused, nudgeQueueCASAttempts)
	}
}
