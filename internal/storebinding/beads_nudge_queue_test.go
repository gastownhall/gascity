package storebinding

// The queue's failure modes are all silent ones: a claim handed to two
// deliverers, a lease that expires without releasing, a supersede that reports
// success while the entry stays claimable, a dead-letter report that includes
// the items about to be retried, and a transition that never reached the disk.
// Every test below is aimed at one of those, and the durability assertions run
// against a REOPENED store so in-memory state cannot answer for the record.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

const testNudgeAgent = "alpha/polly"

type reopenableStore interface {
	CloseStore() error
}

func openNudgeQueueStore(t *testing.T, dir string) beads.Store {
	t.Helper()
	store, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := store.(reopenableStore); ok {
			if err := closer.CloseStore(); err != nil {
				t.Logf("closing sqlite store: %v", err)
			}
		}
	})
	return store
}

func newNudgeQueue(t *testing.T, store beads.Store) NudgeQueue {
	t.Helper()
	queue, err := NewBeadsNudgeQueue(beads.NudgesStore{Store: store})
	if err != nil {
		t.Fatalf("NewBeadsNudgeQueue: %v", err)
	}
	return queue
}

// reopenNudgeQueue closes the store and opens a fresh one over the same
// directory, so the next assertion can only be answered from disk.
func reopenNudgeQueue(t *testing.T, dir string, store beads.Store) NudgeQueue {
	t.Helper()
	closer, ok := store.(reopenableStore)
	if !ok {
		t.Fatalf("store %T cannot be closed, so durability cannot be proven", store)
	}
	if err := closer.CloseStore(); err != nil {
		t.Fatalf("closing sqlite store: %v", err)
	}
	return newNudgeQueue(t, openNudgeQueueStore(t, dir))
}

func testNudgeItem(id, sessionID string, now time.Time) nudgequeue.Item {
	return nudgequeue.Item{
		ID:           id,
		Agent:        testNudgeAgent,
		SessionID:    sessionID,
		Source:       "test",
		Message:      "check your hook",
		CreatedAt:    now,
		DeliverAfter: now,
		ExpiresAt:    now.Add(24 * time.Hour),
	}
}

func nudgeItemIDs(items []nudgequeue.Item) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func claimAll(t *testing.T, queue NudgeQueue, now time.Time) []nudgequeue.Item {
	t.Helper()
	claimed, err := queue.ClaimDue(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	return claimed
}

// queueRecordIDs reads the queue's beads straight off the store, terminal ones
// included. The queue's own front doors deliberately hide retired records, so
// this is the only way to see whether a retired record was actually removed or
// merely stopped being served.
func queueRecordIDs(t *testing.T, store beads.Store) []string {
	t.Helper()
	records, err := store.List(beads.ListQuery{
		Label:         nudgeQueueLabel,
		IncludeClosed: true,
		Sort:          beads.SortCreatedAsc,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		t.Fatalf("listing queue records: %v", err)
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.Metadata[nudgeMetaNudgeID])
	}
	return ids
}

// TestBeadsNudgeQueueRejectsStoresWithoutTheClaimCAS pins the composition-time
// veto. A store without the compare-and-swap primitives must be refused, not
// served by a read-then-write that looks like a claim and is not one.
func TestBeadsNudgeQueueRejectsStoresWithoutTheClaimCAS(t *testing.T) {
	if _, err := NewBeadsNudgeQueue(beads.NudgesStore{Store: narrowStore{Store: beads.NewMemStore()}}); !errors.Is(err, ErrBeadsAdapterCapability) {
		t.Fatalf("NewBeadsNudgeQueue over a store without the claim CAS = %v, want ErrBeadsAdapterCapability", err)
	}
	if _, err := NewBeadsNudgeQueue(beads.NudgesStore{}); err == nil {
		t.Fatal("NewBeadsNudgeQueue accepted an absent store")
	}
}

// TestBeadsNudgeQueueEnqueueIsIdempotentAndDurable proves the submit path
// survives a reopen and that a duplicate id never creates a second record.
func TestBeadsNudgeQueueEnqueueIsIdempotentAndDurable(t *testing.T) {
	dir := t.TempDir()
	store := openNudgeQueueStore(t, dir)
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	item := testNudgeItem("n-1", "", now)
	if err := queue.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := queue.Enqueue(item); err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}

	queue = reopenNudgeQueue(t, dir, store)
	pending, inFlight, dead, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n-1" {
		t.Fatalf("pending after reopen = %v, want exactly n-1", nudgeItemIDs(pending))
	}
	if len(inFlight)+len(dead) != 0 {
		t.Fatalf("a freshly enqueued nudge landed in-flight=%v dead=%v", nudgeItemIDs(inFlight), nudgeItemIDs(dead))
	}
	restored := pending[0]
	if !restored.CreatedAt.Equal(item.CreatedAt) || !restored.DeliverAfter.Equal(item.DeliverAfter) || !restored.ExpiresAt.Equal(item.ExpiresAt) {
		t.Fatalf("timestamps did not round-trip: %+v", restored)
	}
	if restored.Message != item.Message || restored.Source != item.Source {
		t.Fatalf("payload did not round-trip: %+v", restored)
	}
}

// TestBeadsNudgeQueueClaimHasExactlyOneWinner runs real concurrent claimants
// against one store. A queue whose claim is a read-then-write, or whose claim
// token is shared across callers, hands the same nudge to more than one of
// them; this is the assertion that catches it.
func TestBeadsNudgeQueueClaimHasExactlyOneWinner(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	const items = 24
	for i := 0; i < items; i++ {
		if err := queue.Enqueue(testNudgeItem(fmt.Sprintf("n-%02d", i), "", now)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	const claimants = 8
	var (
		mu      sync.Mutex
		winners = map[string][]int{}
		start   = make(chan struct{})
		wg      sync.WaitGroup
	)
	errs := make([]error, claimants)
	for claimant := 0; claimant < claimants; claimant++ {
		wg.Add(1)
		go func(claimant int) {
			defer wg.Done()
			<-start
			claimed, err := queue.ClaimDue(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now)
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
	close(start)
	wg.Wait()

	for claimant, err := range errs {
		if err != nil {
			t.Fatalf("claimant %d: ClaimDue: %v", claimant, err)
		}
	}
	if len(winners) != items {
		t.Fatalf("claimed %d distinct nudges, want all %d", len(winners), items)
	}
	for id, claimants := range winners {
		if len(claimants) != 1 {
			t.Fatalf("nudge %s was claimed by %d claimants %v; the claim is not a single-winner CAS", id, len(claimants), claimants)
		}
	}

	pending, inFlight, _, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != items {
		t.Fatalf("after the race pending=%d in-flight=%d, want 0 and %d", len(pending), len(inFlight), items)
	}
}

// TestBeadsNudgeQueueExpiredLeaseReleasesAcrossReopen is the crashed-deliverer
// path: the process that held the claim is gone, so nothing will ever release
// it explicitly. The item must become claimable again on its own, and the
// recovery must be visible to a completely fresh process.
func TestBeadsNudgeQueueExpiredLeaseReleasesAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	store := openNudgeQueueStore(t, dir)
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	if err := queue.Enqueue(testNudgeItem("n-lease", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed := claimAll(t, queue, now)
	if len(claimed) != 1 {
		t.Fatalf("ClaimDue = %v, want the queued nudge", nudgeItemIDs(claimed))
	}
	if !claimed[0].LeaseUntil.After(now) || claimed[0].ClaimedAt.IsZero() {
		t.Fatalf("claimed nudge carries no lease: %+v", claimed[0])
	}

	// Before the lease runs out the item stays held.
	if again := claimAll(t, queue, now.Add(time.Second)); len(again) != 0 {
		t.Fatalf("ClaimDue re-handed a leased nudge: %v", nudgeItemIDs(again))
	}

	queue = reopenNudgeQueue(t, dir, store)
	afterLease := claimed[0].LeaseUntil.Add(time.Second)
	pending, inFlight, _, err := queue.ListForAgent(testNudgeAgent, afterLease)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 {
		t.Fatalf("after lease expiry pending=%v in-flight=%v, want the nudge back in pending",
			nudgeItemIDs(pending), nudgeItemIDs(inFlight))
	}
	if !pending[0].ClaimedAt.IsZero() || !pending[0].LeaseUntil.IsZero() {
		t.Fatalf("a released nudge still reports a claim: %+v", pending[0])
	}
	// The real proof that the lease released: the item can be claimed again.
	reclaimed := claimAll(t, queue, afterLease)
	if len(reclaimed) != 1 || reclaimed[0].ID != "n-lease" {
		t.Fatalf("ClaimDue after lease expiry = %v, want the recovered nudge", nudgeItemIDs(reclaimed))
	}
	if !reclaimed[0].LeaseUntil.After(claimed[0].LeaseUntil) {
		t.Fatalf("the recovered claim reused the expired lease: %+v", reclaimed[0])
	}
}

// TestBeadsNudgeQueueSupersedeMakesTheEntryUnclaimable is the second named
// silent-success trap: a supersede that returns nil while the superseded entry
// is still handed out delivers a nudge the caller already replaced.
func TestBeadsNudgeQueueSupersedeMakesTheEntryUnclaimable(t *testing.T) {
	dir := t.TempDir()
	store := openNudgeQueueStore(t, dir)
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	reference := nudgequeue.Reference{Kind: "wait", ID: "w-1"}
	first := testNudgeItem("n-old", "", now)
	first.Reference = &reference
	second := testNudgeItem("n-new", "", now)
	second.Reference = &reference
	if err := queue.Enqueue(first); err != nil {
		t.Fatalf("Enqueue(first): %v", err)
	}
	if err := queue.Enqueue(second); err != nil {
		t.Fatalf("Enqueue(second): %v", err)
	}

	queue = reopenNudgeQueue(t, dir, store)
	claimed := claimAll(t, queue, now)
	if ids := nudgeItemIDs(claimed); len(ids) != 1 || ids[0] != "n-new" {
		t.Fatalf("ClaimDue = %v, want only the superseding nudge", ids)
	}
	pending, _, dead, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %v, want the superseded nudge out of the queue", nudgeItemIDs(pending))
	}
	if len(dead) != 1 || dead[0].ID != "n-old" || dead[0].LastError != "superseded" {
		t.Fatalf("dead = %+v, want the superseded nudge dead-lettered", dead)
	}
}

// TestBeadsNudgeQueueRecordFailureReportsOnlyDeadLetters pins the partial
// result. Reporting a retried item as a dead letter makes the caller discard
// work it was about to redeliver.
func TestBeadsNudgeQueueRecordFailureReportsOnlyDeadLetters(t *testing.T) {
	dir := t.TempDir()
	store := openNudgeQueueStore(t, dir)
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	for _, id := range []string{"n-retry", "n-bystander"} {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	cause := errors.New("delivery refused")
	var reported []nudgequeue.Item
	const ceiling = 20
	for attempt := 1; attempt <= ceiling; attempt++ {
		dead, err := queue.RecordFailure([]string{"n-retry"}, cause, now)
		if err != nil {
			t.Fatalf("RecordFailure (attempt %d): %v", attempt, err)
		}
		if len(dead) == 0 {
			continue
		}
		reported = dead
		break
	}
	if ids := nudgeItemIDs(reported); len(ids) != 1 || ids[0] != "n-retry" {
		t.Fatalf("RecordFailure reported %v, want exactly the dead-lettered nudge", ids)
	}
	if reported[0].Attempts != nudgeQueueMaxAttempts || reported[0].LastError != cause.Error() {
		t.Fatalf("dead letter = %+v, want the failure recorded on it", reported[0])
	}

	queue = reopenNudgeQueue(t, dir, store)
	pending, _, dead, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != "n-retry" {
		t.Fatalf("dead bucket after reopen = %v, want the failed nudge", nudgeItemIDs(dead))
	}
	if len(pending) != 1 || pending[0].ID != "n-bystander" {
		t.Fatalf("pending after reopen = %v, want the untouched nudge still queued", nudgeItemIDs(pending))
	}
	if claimed := claimAll(t, queue, now); len(claimed) != 1 || claimed[0].ID != "n-bystander" {
		t.Fatalf("ClaimDue = %v, want a dead-lettered nudge to stay out of delivery", nudgeItemIDs(claimed))
	}
}

// TestBeadsNudgeQueueRetryBacksOffThenRedelivers proves the requeued half of
// the policy: the item is not dead, it is scheduled.
func TestBeadsNudgeQueueRetryBacksOffThenRedelivers(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	if err := queue.Enqueue(testNudgeItem("n-retry", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := queue.ClaimDue(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now); err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	dead, err := queue.RecordFailure([]string{"n-retry"}, errors.New("transient"), now)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("RecordFailure reported %v on the first failure, want nothing", nudgeItemIDs(dead))
	}
	if claimed := claimAll(t, queue, now); len(claimed) != 0 {
		t.Fatalf("ClaimDue = %v inside the retry backoff, want nothing", nudgeItemIDs(claimed))
	}
	after := now.Add(nudgeQueueRetryDelay).Add(time.Second)
	claimed := claimAll(t, queue, after)
	if len(claimed) != 1 || claimed[0].Attempts != 1 {
		t.Fatalf("ClaimDue after the backoff = %+v, want the retried nudge with one attempt", claimed)
	}
}

// TestBeadsNudgeQueueHonorsTheSessionFence keeps a nudge pinned to a dead
// session out of a live session's delivery.
func TestBeadsNudgeQueueHonorsTheSessionFence(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	for _, item := range []nudgequeue.Item{
		testNudgeItem("n-unfenced", "", now),
		testNudgeItem("n-fenced", "session-live", now),
		testNudgeItem("n-stale", "session-gone", now),
	} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	claimed, err := queue.ClaimDue(ClaimTarget{
		QueueKeys: []string{testNudgeAgent},
		SessionID: "session-live",
	}, now)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	ids := nudgeItemIDs(claimed)
	if len(ids) != 2 {
		t.Fatalf("ClaimDue = %v, want the unfenced nudge and the live-session nudge", ids)
	}
	for _, want := range []string{"n-unfenced", "n-fenced"} {
		found := false
		for _, id := range ids {
			found = found || id == want
		}
		if !found {
			t.Fatalf("ClaimDue = %v, want it to include %s", ids, want)
		}
	}
	pending, inFlight, _, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(inFlight) != 2 {
		t.Fatalf("in-flight = %v, want the two claimed nudges", nudgeItemIDs(inFlight))
	}
	if len(pending) != 1 || pending[0].ID != "n-stale" {
		t.Fatalf("pending = %v, want the fenced-out nudge to stay queued", nudgeItemIDs(pending))
	}
}

// TestBeadsNudgeQueueClaimDueSkipsUndeliverableItems keeps a scheduled nudge
// out of delivery until its time comes.
func TestBeadsNudgeQueueClaimDueSkipsUndeliverableItems(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	later := testNudgeItem("n-later", "", now)
	later.DeliverAfter = now.Add(time.Hour)
	if err := queue.Enqueue(later); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if claimed := claimAll(t, queue, now); len(claimed) != 0 {
		t.Fatalf("ClaimDue = %v, want nothing before deliver_after", nudgeItemIDs(claimed))
	}
	due := now.Add(2 * time.Hour)
	if claimed := claimAll(t, queue, due); len(claimed) != 1 {
		t.Fatalf("ClaimDue after deliver_after = %v, want the scheduled nudge", nudgeItemIDs(claimed))
	}
	if claimed := claimAll(t, queue, due); len(claimed) != 0 {
		t.Fatalf("ClaimDue = %v, want the claimed nudge to stay held", nudgeItemIDs(claimed))
	}
}

// TestBeadsNudgeQueueAckAndReleaseAreDurable pins the two terminal delivery
// outcomes against a reopened store.
func TestBeadsNudgeQueueAckAndReleaseAreDurable(t *testing.T) {
	dir := t.TempDir()
	store := openNudgeQueueStore(t, dir)
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	for _, id := range []string{"n-ack", "n-release"} {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	if claimed := claimAll(t, queue, now); len(claimed) != 2 {
		t.Fatalf("ClaimDue = %v, want both nudges", nudgeItemIDs(claimed))
	}
	if err := queue.Ack([]string{"n-ack"}, "delivered", "", "commit"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := queue.ReleaseClaims([]string{"n-release"}); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}

	queue = reopenNudgeQueue(t, dir, store)
	pending, inFlight, dead, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %v, want nothing dead-lettered", nudgeItemIDs(dead))
	}
	if len(inFlight) != 0 {
		t.Fatalf("in-flight = %v, want no held nudges after reopen", nudgeItemIDs(inFlight))
	}
	if len(pending) != 1 || pending[0].ID != "n-release" {
		t.Fatalf("pending = %v, want only the released nudge", nudgeItemIDs(pending))
	}
	state, err := queue.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(state.Pending) != 1 || len(state.InFlight) != 0 || len(state.Dead) != 0 {
		t.Fatalf("Snapshot = %+v, want the acked nudge gone and the released one queued", state)
	}
	// A second Ack of an already-terminal nudge is a no-op, not a resurrection.
	if err := queue.Ack([]string{"n-ack"}, "delivered", "", "commit"); err != nil {
		t.Fatalf("second Ack: %v", err)
	}
}

// TestBeadsNudgeQueueSnapshotSeesEveryBucket pins the bucket projection.
func TestBeadsNudgeQueueSnapshotSeesEveryBucket(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	for _, id := range []string{"n-a", "n-b", "n-dead"} {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	if err := queue.Rollback(testNudgeItem("n-dead", "", now), "rolled back"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if claimed := claimAll(t, queue, now); len(claimed) != 2 {
		t.Fatalf("ClaimDue = %v, want the two live nudges", nudgeItemIDs(claimed))
	}
	state, err := queue.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(state.InFlight) != 2 {
		t.Fatalf("Snapshot in-flight = %v, want both claimed nudges", nudgeItemIDs(state.InFlight))
	}
	if len(state.Dead) != 1 || state.Dead[0].ID != "n-dead" {
		t.Fatalf("Snapshot dead = %v, want the rolled-back nudge", nudgeItemIDs(state.Dead))
	}
	if len(state.Pending) != 0 {
		t.Fatalf("Snapshot pending = %v, want nothing left queued", nudgeItemIDs(state.Pending))
	}
}

// TestBeadsNudgeQueueRollbackRecordsANudgeThatNeverLanded keeps a failure
// observable even when there is no queue row to move.
func TestBeadsNudgeQueueRollbackRecordsANudgeThatNeverLanded(t *testing.T) {
	dir := t.TempDir()
	store := openNudgeQueueStore(t, dir)
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	if err := queue.Rollback(testNudgeItem("n-ghost", "", now), "enqueue transaction failed"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	queue = reopenNudgeQueue(t, dir, store)
	// The record is terminal history, not a queued nudge, and re-enqueueing
	// the same id must not resurrect it.
	state, err := queue.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(state.Pending)+len(state.InFlight)+len(state.Dead) != 0 {
		t.Fatalf("Snapshot = %+v, want a rolled-back ghost to be invisible", state)
	}
	if err := queue.Enqueue(testNudgeItem("n-ghost", "", now)); err != nil {
		t.Fatalf("Enqueue after rollback: %v", err)
	}
	if claimed := claimAll(t, queue, now); len(claimed) != 0 {
		t.Fatalf("ClaimDue = %v, want the terminal record to keep the id retired", nudgeItemIDs(claimed))
	}
}

// TestBeadsNudgeQueueWithdrawRetiresQueuedWaitNudges pins the wait-cancel path.
func TestBeadsNudgeQueueWithdrawRetiresQueuedWaitNudges(t *testing.T) {
	dir := t.TempDir()
	store := openNudgeQueueStore(t, dir)
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	for _, id := range []string{"n-wait", "n-keep"} {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	if err := queue.WithdrawQueuedWaitNudges([]string{"n-wait", "n-wait", "", "n-missing"}); err != nil {
		t.Fatalf("WithdrawQueuedWaitNudges: %v", err)
	}
	queue = reopenNudgeQueue(t, dir, store)
	pending, _, dead, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %v, want a withdrawal to retire rather than dead-letter", nudgeItemIDs(dead))
	}
	if len(pending) != 1 || pending[0].ID != "n-keep" {
		t.Fatalf("pending = %v, want only the untouched nudge", nudgeItemIDs(pending))
	}
}

// TestBeadsNudgeQueueExpiresAndAgesDeadLetters pins the two clock-driven
// transitions: a nudge past its deadline dead-letters, and a dead letter past
// its retention leaves the visible buckets.
func TestBeadsNudgeQueueExpiresAndAgesDeadLetters(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	item := testNudgeItem("n-expiring", "", now)
	item.ExpiresAt = now.Add(time.Minute)
	if err := queue.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	afterExpiry := now.Add(2 * time.Minute)
	pending, _, dead, err := queue.ListForAgent(testNudgeAgent, afterExpiry)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %v, want the expired nudge out of the queue", nudgeItemIDs(pending))
	}
	if len(dead) != 1 || dead[0].LastError != "expired" {
		t.Fatalf("dead = %+v, want the expired nudge dead-lettered", dead)
	}
	if claimed := claimAll(t, queue, afterExpiry); len(claimed) != 0 {
		t.Fatalf("ClaimDue = %v, want an expired nudge to stay out of delivery", nudgeItemIDs(claimed))
	}

	afterRetention := afterExpiry.Add(nudgeQueueDeadRetention).Add(time.Minute)
	_, _, dead, err = queue.ListForAgent(testNudgeAgent, afterRetention)
	if err != nil {
		t.Fatalf("ListForAgent after retention: %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %v, want the dead letter aged into the terminal record", nudgeItemIDs(dead))
	}
}

// TestBeadsNudgeQueueListForMatchesEveryQueueKey pins the multi-key read used
// by a target whose alias history addresses the same agent.
func TestBeadsNudgeQueueListForMatchesEveryQueueKey(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	other := testNudgeItem("n-other", "", now)
	other.Agent = "alpha/former"
	if err := queue.Enqueue(testNudgeItem("n-mine", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := queue.Enqueue(other); err != nil {
		t.Fatalf("Enqueue(other): %v", err)
	}
	pending, _, _, err := queue.ListFor(ClaimTarget{QueueKeys: []string{testNudgeAgent, "alpha/former"}}, now)
	if err != nil {
		t.Fatalf("ListFor: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("ListFor pending = %v, want both queue keys", nudgeItemIDs(pending))
	}
	if pendingOne, _, _, err := queue.ListFor(ClaimTarget{}, now); err != nil || len(pendingOne) != 0 {
		t.Fatalf("ListFor with no queue keys = %v, %v, want nothing", nudgeItemIDs(pendingOne), err)
	}
	if claimed, err := queue.ClaimDue(ClaimTarget{}, now); err != nil || len(claimed) != 0 {
		t.Fatalf("ClaimDue with no queue keys = %v, %v, want nothing", nudgeItemIDs(claimed), err)
	}
}

// TestBeadsNudgeQueueDeferredEnqueueSkipsSupersession pins the deferred-submit
// shape: it lands the row and touches nothing else.
func TestBeadsNudgeQueueDeferredEnqueueSkipsSupersession(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	reference := nudgequeue.Reference{Kind: "wait", ID: "w-1"}
	first := testNudgeItem("n-first", "", now)
	first.Reference = &reference
	second := testNudgeItem("n-second", "", now)
	second.Reference = &reference
	if err := queue.EnqueueDeferred(first); err != nil {
		t.Fatalf("EnqueueDeferred: %v", err)
	}
	if err := queue.EnqueueDeferred(second); err != nil {
		t.Fatalf("EnqueueDeferred(second): %v", err)
	}
	if err := queue.EnqueueDeferred(second); err != nil {
		t.Fatalf("duplicate EnqueueDeferred: %v", err)
	}
	pending, _, _, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %v, want both deferred nudges", nudgeItemIDs(pending))
	}
}

// TestNudgeQueueBeadIDNeverEndsInADigit guards the id shape. A store that
// recovers its sequence from the largest numeric id suffix would read a
// digit-tailed hash as a sequence floor and burn the id space.
func TestNudgeQueueBeadIDNeverEndsInADigit(t *testing.T) {
	for _, id := range []string{"", "n-1", "nudge/with/slashes", "9999999999999999999"} {
		got := nudgeQueueBeadID(id)
		last := got[len(got)-1]
		if last >= '0' && last <= '9' {
			t.Fatalf("nudgeQueueBeadID(%q) = %q, want a non-numeric tail", id, got)
		}
	}
	if nudgeQueueBeadID("a") == nudgeQueueBeadID("b") {
		t.Fatal("nudgeQueueBeadID collided on distinct nudge ids")
	}
}

// TestNudgeQueueBeadIDLandsInTheNudgesNamespace closes the loop between the
// package that MINTS a queue record's id and the package that ROUTES it.
//
// Deriving the prefix from config.NudgeQueueIDPrefix removes the second literal
// but not the second convention: the router matches the segment before the
// first "-", and the minter is the one that appends it. Moving the separator to
// the other side of that seam compiles, keeps both sides spelling "gcnq", and
// silently stops the table matching — queue records land in the work ledger,
// the queue reads its own binding, finds nothing, and stops draining without an
// error anywhere.
func TestNudgeQueueBeadIDLandsInTheNudgesNamespace(t *testing.T) {
	id := nudgeQueueBeadID("n-1")
	head, rest, ok := strings.Cut(id, "-")
	if !ok || rest == "" {
		t.Fatalf("nudgeQueueBeadID minted %q, want a prefix and a body separated by %q", id, "-")
	}
	if !config.IsReservedClassPrefix(head) {
		t.Errorf("a minted queue id %q carries prefix %q, which config does not reserve — a rig could be configured with it", id, head)
	}
	for _, prefix := range config.ReservedClassPrefixesFor(config.BeadClassNudges) {
		if prefix == head {
			return
		}
	}
	t.Errorf("a minted queue id %q carries prefix %q, which is reserved for some other class; the nudges binding holds %v", id, head, config.ReservedClassPrefixesFor(config.BeadClassNudges))
}

// TestBeadsNudgeQueueSweepsTerminalRecordsPastRetention pins the end of a
// record's life. Every other transition merely stops SERVING a record; without
// a sweep the terminal bead is immortal, and a city accumulates one forever
// per nudge it has ever delivered. The assertion therefore reads the store
// directly — through the front doors a retired record is invisible whether it
// was deleted or not, which is exactly how this gap stayed invisible.
func TestBeadsNudgeQueueSweepsTerminalRecordsPastRetention(t *testing.T) {
	dir := t.TempDir()
	store := openNudgeQueueStore(t, dir)
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	if err := queue.Enqueue(testNudgeItem("n-delivered", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if claimed := claimAll(t, queue, now); len(claimed) != 1 {
		t.Fatalf("ClaimDue = %v, want the enqueued nudge", nudgeItemIDs(claimed))
	}
	if err := queue.Ack([]string{"n-delivered"}, "delivered", "", "commit"); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	// Inside the retention window the record is retired but still on disk, so
	// an operator can still answer "what happened to that nudge?". The probe
	// sits just short of the boundary rather than at the Ack, so a sweep keyed
	// on a shorter window than it claims is a failure and not a coincidence of
	// wall-clock timing.
	justInside := now.Add(nudgeQueueTerminalRetention).Add(-time.Minute)
	if _, _, _, err := queue.ListForAgent(testNudgeAgent, justInside); err != nil {
		t.Fatalf("ListForAgent inside retention: %v", err)
	}
	if ids := queueRecordIDs(t, store); len(ids) != 1 || ids[0] != "n-delivered" {
		t.Fatalf("records inside retention = %v, want the terminal record kept", ids)
	}

	// Past it the maintenance pass every operation runs removes it.
	afterRetention := now.Add(nudgeQueueTerminalRetention).Add(time.Minute)
	if _, _, _, err := queue.ListForAgent(testNudgeAgent, afterRetention); err != nil {
		t.Fatalf("ListForAgent past retention: %v", err)
	}
	if ids := queueRecordIDs(t, store); len(ids) != 0 {
		t.Fatalf("records past retention = %v, want the terminal record swept; it would live as long as the city", ids)
	}

	// The sweep is a delete, not a hidden row: it survives a reopen, and a
	// nudge id it collected is free to be enqueued again.
	queue = reopenNudgeQueue(t, dir, store)
	if err := queue.Enqueue(testNudgeItem("n-delivered", "", afterRetention)); err != nil {
		t.Fatalf("re-enqueueing a swept nudge id: %v", err)
	}
	pending, _, _, err := queue.ListForAgent(testNudgeAgent, afterRetention)
	if err != nil {
		t.Fatalf("ListForAgent after reopen: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n-delivered" {
		t.Fatalf("pending after reopen = %v, want the re-enqueued nudge", nudgeItemIDs(pending))
	}
}

// TestBeadsNudgeQueueSweepKeepsLiveAndDeadRecords keeps the sweep honest in
// the other direction. It collects TERMINAL records only: a dead letter inside
// its own dead-bucket retention is still served to an operator, and a pending
// item older than the terminal ttl has not been delivered yet.
func TestBeadsNudgeQueueSweepKeepsLiveAndDeadRecords(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	longLived := testNudgeItem("n-longlived", "", now)
	longLived.ExpiresAt = now.Add(30 * 24 * time.Hour)
	if err := queue.Enqueue(longLived); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// A dead letter created just past the terminal ttl: old enough that a
	// sweep keyed on the wrong clock would take it, young enough that its own
	// dead-bucket retention has not run out.
	late := now.Add(nudgeQueueTerminalRetention).Add(time.Minute)
	dying := testNudgeItem("n-dying", "", late)
	dying.ExpiresAt = late.Add(time.Minute)
	if err := queue.Enqueue(dying); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	afterExpiry := late.Add(2 * time.Minute)
	pending, _, dead, err := queue.ListForAgent(testNudgeAgent, afterExpiry)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != "n-dying" {
		t.Fatalf("dead = %v, want the freshly dead-lettered nudge still in the dead bucket", nudgeItemIDs(dead))
	}
	if len(pending) != 1 || pending[0].ID != "n-longlived" {
		t.Fatalf("pending = %v, want the undelivered nudge still queued", nudgeItemIDs(pending))
	}
}

// TestBeadsNudgeQueueDeadLettersFenceMismatchImmediately pins the permanent
// failure. The fenced session generation is gone, so redelivery cannot
// succeed: retrying to the ceiling holds a nudge for the whole backoff
// schedule and then dead-letters it anyway, and every one of those attempts is
// a delivery to a session that no longer exists.
func TestBeadsNudgeQueueDeadLettersFenceMismatchImmediately(t *testing.T) {
	dir := t.TempDir()
	store := openNudgeQueueStore(t, dir)
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	if err := queue.Enqueue(testNudgeItem("n-fenced", "session-gone", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Wrapped, because a caller reports the mismatch with its own context
	// attached; matching only the bare sentinel would miss every real report.
	cause := fmt.Errorf("delivering to session-gone: %w", ErrNudgeSessionFenceMismatch)
	deadLettered, err := queue.RecordFailure([]string{"n-fenced"}, cause, now)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if ids := nudgeItemIDs(deadLettered); len(ids) != 1 || ids[0] != "n-fenced" {
		t.Fatalf("RecordFailure reported %v on a fence mismatch, want it dead-lettered on the first failure", ids)
	}
	if got := deadLettered[0].Attempts; got != 1 {
		t.Fatalf("dead letter recorded %d attempts, want 1; the item was retried before it died", got)
	}

	queue = reopenNudgeQueue(t, dir, store)
	pending, inFlight, dead, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != "n-fenced" {
		t.Fatalf("dead = %v, want the fence-mismatched nudge dead-lettered", nudgeItemIDs(dead))
	}
	if len(pending)+len(inFlight) != 0 {
		t.Fatalf("pending=%v in-flight=%v, want the fence-mismatched nudge out of delivery",
			nudgeItemIDs(pending), nudgeItemIDs(inFlight))
	}
	if claimed := claimAll(t, queue, now.Add(nudgeQueueRetryDelay).Add(time.Second)); len(claimed) != 0 {
		t.Fatalf("ClaimDue after the backoff = %v, want a fence mismatch never redelivered", nudgeItemIDs(claimed))
	}
}

// TestBeadsNudgeQueueRetriesOrdinaryFailuresAfterAFenceMismatch keeps the
// permanent classification narrow. A queue that treats every failure as
// permanent also passes the test above; this is the half that rejects it.
func TestBeadsNudgeQueueRetriesOrdinaryFailuresAfterAFenceMismatch(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	if err := queue.Enqueue(testNudgeItem("n-transient", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	dead, err := queue.RecordFailure([]string{"n-transient"}, errors.New("connection reset"), now)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("RecordFailure reported %v for a transient cause, want it requeued", nudgeItemIDs(dead))
	}
	after := now.Add(nudgeQueueRetryDelay).Add(time.Second)
	if claimed := claimAll(t, queue, after); len(claimed) != 1 || claimed[0].ID != "n-transient" {
		t.Fatalf("ClaimDue after the backoff = %v, want the transient failure redelivered", nudgeItemIDs(claimed))
	}
}

// TestNudgeLeaseRoundTrips pins the lease codec the delivery axis rests on.
func TestNudgeLeaseRoundTrips(t *testing.T) {
	now := time.Now().UTC()
	lease, err := newNudgeLease(now, nudgeQueueClaimLease)
	if err != nil {
		t.Fatalf("newNudgeLease: %v", err)
	}
	parsed, ok := parseNudgeLease(lease.assignee)
	if !ok {
		t.Fatalf("parseNudgeLease(%q) failed", lease.assignee)
	}
	if !parsed.claimedAt.Equal(lease.claimedAt) || !parsed.until.Equal(lease.until) {
		t.Fatalf("lease round-trip = %+v, want %+v", parsed, lease)
	}
	other, err := newNudgeLease(now, nudgeQueueClaimLease)
	if err != nil {
		t.Fatalf("newNudgeLease: %v", err)
	}
	if other.assignee == lease.assignee {
		t.Fatal("two leases minted the same token; concurrent claimants would collide")
	}
	for _, assignee := range []string{"", "worker", "nudge-lease/abc", "nudge-lease//x/y", "nudge-lease/abc/not-a-time/also-not"} {
		if _, ok := parseNudgeLease(assignee); ok {
			t.Fatalf("parseNudgeLease(%q) accepted a non-lease assignee", assignee)
		}
	}
}

// deployedNudgeQueuePolicySource is the file that holds the queue policy
// production actually applies. `gc` still drives the on-disk nudge queue
// directly, so its constants — not any store's copy of them — are the
// authority this queue must not drift from.
const deployedNudgeQueuePolicySource = "cmd/gc/cmd_nudge.go"

// TestBeadsNudgeQueuePolicyMatchesTheDeployedQueue pins this queue's private
// policy vocabulary to the deployed file queue's own constants.
//
// The constants here are copies, and a copy with no guard drifts silently:
// every timing assertion in this file computes its probe instants FROM the
// constant, so halving a retention still passes while the two queues then
// disagree about when a record is gone.
//
// The guard reads cmd/gc's source rather than importing it, because a main
// package cannot be imported. That is not a workaround for a missing
// dependency — it is the only way to pin against the values production runs.
// The evaluator below is deliberately strict: a constant respelled into a form
// it does not understand fails the test instead of quietly dropping out of the
// comparison.
func TestBeadsNudgeQueuePolicyMatchesTheDeployedQueue(t *testing.T) {
	deployed := parseDeployedNudgeQueuePolicy(t)
	durations := []struct {
		name, constant string
		here           time.Duration
	}{
		{"claim lease", "defaultQueuedNudgeClaimTTL", nudgeQueueClaimLease},
		{"retry delay", "defaultQueuedNudgeRetryDelay", nudgeQueueRetryDelay},
		{"dead retention", "defaultQueuedNudgeDeadRetention", nudgeQueueDeadRetention},
		{"terminal retention", "defaultQueuedNudgeTTL", nudgeQueueTerminalRetention},
	}
	for _, policy := range durations {
		want, ok := deployed[policy.constant]
		if !ok {
			t.Errorf("%s is not declared in %s; the guard can no longer see the deployed policy",
				policy.constant, deployedNudgeQueuePolicySource)
			continue
		}
		if policy.here != time.Duration(want) {
			t.Errorf("%s = %s, want the deployed %s = %s; the two queues have forked the class",
				policy.name, policy.here, policy.constant, time.Duration(want))
		}
	}
	want, ok := deployed["defaultQueuedNudgeMaxAttempts"]
	if !ok {
		t.Fatalf("defaultQueuedNudgeMaxAttempts is not declared in %s", deployedNudgeQueuePolicySource)
	}
	if int64(nudgeQueueMaxAttempts) != want {
		t.Errorf("attempt ceiling = %d, want the deployed %d; the two queues have forked the class",
			nudgeQueueMaxAttempts, want)
	}
}

// parseDeployedNudgeQueuePolicy returns every `defaultQueuedNudge*` constant
// declared in the deployed queue, as a plain integer (nanoseconds for the
// durations). It fails the test rather than returning a partial map, so a
// moved or renamed constant is a failure and not a silently skipped check.
func parseDeployedNudgeQueuePolicy(t *testing.T) map[string]int64 {
	t.Helper()
	path := filepath.Join(censusModuleRoot(t), filepath.FromSlash(deployedNudgeQueuePolicySource))
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", deployedNudgeQueuePolicySource, err)
	}
	policy := map[string]int64{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != len(value.Values) {
				continue
			}
			for i, name := range value.Names {
				if !strings.HasPrefix(name.Name, "defaultQueuedNudge") {
					continue
				}
				n, err := evalDeployedPolicyConstant(value.Values[i])
				if err != nil {
					t.Fatalf("evaluating %s in %s: %v", name.Name, deployedNudgeQueuePolicySource, err)
				}
				policy[name.Name] = n
			}
		}
	}
	if len(policy) == 0 {
		t.Fatalf("no defaultQueuedNudge* constants found in %s; the guard is looking at the wrong file",
			deployedNudgeQueuePolicySource)
	}
	return policy
}

// evalDeployedPolicyConstant evaluates the small expression grammar the
// deployed policy constants are written in: an integer literal, a time unit,
// or a product of the two.
func evalDeployedPolicyConstant(expr ast.Expr) (int64, error) {
	switch node := expr.(type) {
	case *ast.BasicLit:
		if node.Kind != token.INT {
			return 0, fmt.Errorf("unsupported literal kind %s", node.Kind)
		}
		return strconv.ParseInt(node.Value, 0, 64)
	case *ast.SelectorExpr:
		pkg, ok := node.X.(*ast.Ident)
		if !ok || pkg.Name != "time" {
			return 0, fmt.Errorf("unsupported selector %s", types.ExprString(expr))
		}
		unit, ok := map[string]time.Duration{
			"Nanosecond":  time.Nanosecond,
			"Microsecond": time.Microsecond,
			"Millisecond": time.Millisecond,
			"Second":      time.Second,
			"Minute":      time.Minute,
			"Hour":        time.Hour,
		}[node.Sel.Name]
		if !ok {
			return 0, fmt.Errorf("unsupported time unit %s", node.Sel.Name)
		}
		return int64(unit), nil
	case *ast.BinaryExpr:
		if node.Op != token.MUL {
			return 0, fmt.Errorf("unsupported operator %s", node.Op)
		}
		left, err := evalDeployedPolicyConstant(node.X)
		if err != nil {
			return 0, err
		}
		right, err := evalDeployedPolicyConstant(node.Y)
		if err != nil {
			return 0, err
		}
		return left * right, nil
	case *ast.ParenExpr:
		return evalDeployedPolicyConstant(node.X)
	default:
		return 0, fmt.Errorf("unsupported expression %s", types.ExprString(expr))
	}
}
