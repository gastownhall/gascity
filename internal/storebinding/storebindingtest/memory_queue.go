package storebindingtest

// An in-memory reference nudge queue.
//
// The Nudges class has no canonical Beads-backed queue: the queue is a durable
// component in its own right, so the substitution leg the other four classes
// get for free has to be written here. This implementation exists only to give
// the Nudges suite a conforming counterpart, and it deliberately lives in this
// package rather than importing the SQLite class store: a harness whose own
// proofs need a storage provider cannot be consumed by a provider overlay that
// has not been built yet.
//
// The retry vocabulary below mirrors the deployed queue policy. The suite
// asserts only the provider-neutral half of it — an expired item or a fence
// mismatch dead-letters on its first failure while a live one retries — so the
// exact backoff and attempt ceiling are free to differ between providers
// without forking the contract.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
)

const (
	memoryQueueClaimLease        = 2 * time.Minute
	memoryQueueRetryDelay        = 15 * time.Second
	memoryQueueMaxAttempt        = 5
	memoryQueueDeadRetention     = time.Hour
	memoryQueueTerminalRetention = 24 * time.Hour
)

type memoryQueueState uint8

const (
	memoryQueuePending memoryQueueState = iota
	memoryQueueInFlight
	memoryQueueDead
	memoryQueueTerminal
)

type memoryQueueEntry struct {
	item  nudgequeue.Item
	state memoryQueueState
	// terminalAt is when the entry retired; Maintain sweeps it a retention
	// window later, which frees its id for a fresh submit.
	terminalAt time.Time
}

// MemoryNudgeQueue is a conforming in-process [storebinding.NudgeQueue].
type MemoryNudgeQueue struct {
	mu      sync.Mutex
	entries map[string]*memoryQueueEntry
	order   []string
	// policy is the legacy behavior this queue reproduces; the zero value is
	// the contract.
	policy NudgeQueuePolicy
}

// NewMemoryNudgeQueue returns an empty in-memory queue that keeps the contract.
func NewMemoryNudgeQueue() *MemoryNudgeQueue {
	return NewMemoryNudgeQueueWithPolicy(NudgeQueuePolicy{})
}

// NewMemoryNudgeQueueWithPolicy returns an empty in-memory queue that keeps the
// legacy behaviors policy declares. It is the counterpart that lets the Nudges
// suite prove its policy cases in both directions: a queue that keeps a legacy
// behavior passes when it declares it and fails when it does not, and a queue
// that keeps the contract fails when it declares a legacy behavior.
func NewMemoryNudgeQueueWithPolicy(policy NudgeQueuePolicy) *MemoryNudgeQueue {
	return &MemoryNudgeQueue{entries: map[string]*memoryQueueEntry{}, policy: policy}
}

// verbPrologueLocked is the whole-queue recovery a legacy queue runs inside
// every verb. A contract queue runs none: recovery is Maintain's alone.
func (q *MemoryNudgeQueue) verbPrologueLocked(now time.Time) {
	if q.policy.VerbsMaintain {
		q.maintainLocked(now)
	}
}

// Enqueue inserts item as pending. A duplicate ID is a no-op, matching the
// deployed queue's idempotent submit.
func (q *MemoryNudgeQueue) Enqueue(item nudgequeue.Item) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.verbPrologueLocked(time.Now().UTC())
	if _, exists := q.entries[item.ID]; exists {
		return nil
	}
	if item.Reference != nil && item.Reference.ID != "" && item.Agent != "" {
		for _, existing := range q.entries {
			if existing.state != memoryQueuePending && existing.state != memoryQueueInFlight {
				continue
			}
			if existing.item.Agent != item.Agent || existing.item.Source != item.Source {
				continue
			}
			if existing.item.Reference == nil || *existing.item.Reference != *item.Reference {
				continue
			}
			existing.state = memoryQueueDead
			existing.item.DeadAt = time.Now().UTC()
			existing.item.LastError = "superseded"
		}
	}
	q.insert(item, memoryQueuePending)
	return nil
}

// EnqueueDeferred inserts item as pending without maintenance or supersession.
func (q *MemoryNudgeQueue) EnqueueDeferred(item nudgequeue.Item) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.entries[item.ID]; exists {
		return nil
	}
	q.insert(item, memoryQueuePending)
	return nil
}

// ClaimDue claims every due pending item addressed to target, honoring the
// session-generation fence.
func (q *MemoryNudgeQueue) ClaimDue(target storebinding.ClaimTarget, now time.Time) ([]nudgequeue.Item, error) {
	if len(target.QueueKeys) == 0 {
		return nil, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.verbPrologueLocked(now)
	var claimed []nudgequeue.Item
	for _, entry := range q.candidatesLocked(target, now, newClaimFence(target)) {
		claimed = append(claimed, claimEntry(entry, now, entry.item.DeliveryBatch))
	}
	return claimed, nil
}

// ClaimBatch claims target's next delivery batch: the first pinned batch among
// the due candidates that can be claimed whole, or else every due unpinned
// candidate under a newly minted key.
func (q *MemoryNudgeQueue) ClaimBatch(target storebinding.ClaimTarget, now time.Time) ([]nudgequeue.Item, error) {
	if len(target.QueueKeys) == 0 {
		return nil, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.verbPrologueLocked(now)
	fence := newClaimFence(target)
	candidates := q.candidatesLocked(target, now, fence)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		batch := candidate.item.DeliveryBatch
		if batch == "" || seen[batch] {
			continue
		}
		seen[batch] = true
		members, whole := q.wholeBatchLocked(batch, target, now, fence)
		if !whole {
			continue
		}
		claimed := make([]nudgequeue.Item, 0, len(members))
		for _, member := range members {
			claimed = append(claimed, claimEntry(member, now, batch))
		}
		sortByDelivery(claimed)
		return claimed, nil
	}
	var claimed []nudgequeue.Item
	batch := ""
	for _, candidate := range candidates {
		if candidate.item.DeliveryBatch != "" {
			continue
		}
		if batch == "" {
			minted, err := newMemoryBatchKey()
			if err != nil {
				return nil, err
			}
			batch = minted
		}
		claimed = append(claimed, claimEntry(candidate, now, batch))
	}
	return claimed, nil
}

// candidatesLocked returns the entries target may claim now, in delivery
// order: live, unclaimed, addressed to one of its keys, due, and passing the
// fence. An expired entry it meets is dead-lettered instead.
func (q *MemoryNudgeQueue) candidatesLocked(target storebinding.ClaimTarget, now time.Time, fence *claimFence) []*memoryQueueEntry {
	var candidates []*memoryQueueEntry
	for _, id := range q.order {
		entry := q.entries[id]
		if entry == nil || !entry.live() || entry.inFlightAt(now) {
			continue
		}
		if !matchesQueueKeys(entry.item.Agent, target.QueueKeys) {
			continue
		}
		if expired(entry.item, now) {
			deadLetterExpired(entry)
			continue
		}
		if entry.item.DeliverAfter.After(now) {
			continue
		}
		if !fence.passes(entry.item) {
			continue
		}
		candidates = append(candidates, entry)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return deliveredBefore(candidates[i].item, candidates[j].item) })
	return candidates
}

// wholeBatchLocked returns batch's live members and whether target can claim
// them whole: none in flight, and every one due, addressed to one of target's
// keys, and passing the fence.
func (q *MemoryNudgeQueue) wholeBatchLocked(batch string, target storebinding.ClaimTarget, now time.Time, fence *claimFence) ([]*memoryQueueEntry, bool) {
	var members []*memoryQueueEntry
	for _, id := range q.order {
		entry := q.entries[id]
		if entry == nil || !entry.live() || entry.item.DeliveryBatch != batch || expired(entry.item, now) {
			continue
		}
		if entry.inFlightAt(now) || entry.item.DeliverAfter.After(now) ||
			!matchesQueueKeys(entry.item.Agent, target.QueueKeys) || !fence.passes(entry.item) {
			return nil, false
		}
		members = append(members, entry)
	}
	return members, len(members) > 0
}

// claimEntry leases entry to the caller under batch and returns its view.
func claimEntry(entry *memoryQueueEntry, now time.Time, batch string) nudgequeue.Item {
	entry.item.ClaimedAt = now.UTC()
	entry.item.LeaseUntil = now.Add(memoryQueueClaimLease).UTC()
	entry.item.DeliveryBatch = batch
	entry.state = memoryQueueInFlight
	return entry.item
}

// newMemoryBatchKey mints a delivery batch key.
func newMemoryBatchKey() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("minting nudge delivery batch key: %w", err)
	}
	return "nudge-batch-" + hex.EncodeToString(buf[:]), nil
}

// claimFence applies one claim's session-generation fence. It asks the
// target's census at most once, and only for a candidate pinned to a session
// other than the target's.
type claimFence struct {
	target    storebinding.ClaimTarget
	asked     bool
	live      map[string]struct{}
	available bool
}

func newClaimFence(target storebinding.ClaimTarget) *claimFence {
	return &claimFence{target: target}
}

// passes reports whether the target may claim item. A session-pinned item is
// claimable by its own session, or by another session the census names when
// the census no longer names the item's; an epoch-pinned unpinned-session item
// only by a target that carries a continuation epoch at all.
func (f *claimFence) passes(item nudgequeue.Item) bool {
	if item.SessionID == "" {
		return item.ContinuationEpoch == "" || f.target.ContinuationEpoch != ""
	}
	if f.target.SessionID == "" {
		return false
	}
	if item.SessionID == f.target.SessionID {
		return true
	}
	if f.target.LiveFenceSessions == nil {
		return false
	}
	if !f.asked {
		f.asked = true
		f.live, f.available = f.target.LiveFenceSessions()
	}
	if !f.available {
		return false
	}
	if _, successorLive := f.live[f.target.SessionID]; !successorLive {
		return false
	}
	_, stillLive := f.live[item.SessionID]
	return !stillLive
}

// ListForAgent returns the queue buckets addressed exactly to agentName.
func (q *MemoryNudgeQueue) ListForAgent(agentName string, now time.Time) (pending, inFlight, dead []nudgequeue.Item, err error) {
	return q.listMatching(now, func(item nudgequeue.Item) bool { return matchesQueueKeys(item.Agent, []string{agentName}) })
}

// ListFor returns the queue buckets matching any of target's queue keys.
func (q *MemoryNudgeQueue) ListFor(target storebinding.ClaimTarget, now time.Time) (pending, inFlight, dead []nudgequeue.Item, err error) {
	if len(target.QueueKeys) == 0 {
		return nil, nil, nil, nil
	}
	return q.listMatching(now, func(item nudgequeue.Item) bool {
		return matchesQueueKeys(item.Agent, target.QueueKeys)
	})
}

// Snapshot returns every queued item in the canonical bucket order.
func (q *MemoryNudgeQueue) Snapshot() (nudgequeue.State, error) {
	pending, inFlight, dead, err := q.listMatching(time.Now().UTC(), func(nudgequeue.Item) bool { return true })
	if err != nil {
		return nudgequeue.State{}, err
	}
	state := nudgequeue.State{Pending: pending, InFlight: inFlight, Dead: dead}
	nudgequeue.SortState(&state)
	return state, nil
}

// Ack terminalizes delivered ids. Unknown and already-terminal ids no-op.
func (q *MemoryNudgeQueue) Ack(ids []string, _, _, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC()
	q.verbPrologueLocked(now)
	for _, id := range ids {
		entry := q.entries[id]
		if entry == nil {
			continue
		}
		if entry.live() && expired(entry.item, now) {
			deadLetterExpired(entry)
			continue
		}
		if !entry.live() {
			continue
		}
		if q.policy.ForgetsRetiredIDs {
			q.forgetLocked(id)
			continue
		}
		entry.state = memoryQueueTerminal
		entry.terminalAt = now
		entry.item.ClaimedAt = time.Time{}
		entry.item.LeaseUntil = time.Time{}
	}
	return nil
}

// ReleaseClaims returns undelivered in-flight ids to pending, not claimable
// before notBefore when it is set. The delivery batch is kept. An item whose
// lease has already run out is no longer in flight, so it is left as it is:
// there is no claim to give back and no backoff to impose.
func (q *MemoryNudgeQueue) ReleaseClaims(ids []string, notBefore time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC()
	q.verbPrologueLocked(now)
	for _, id := range ids {
		entry := q.entries[id]
		if entry == nil || entry.state != memoryQueueInFlight {
			continue
		}
		if expired(entry.item, now) {
			deadLetterExpired(entry)
			continue
		}
		if !entry.inFlightAt(now) {
			continue
		}
		entry.state = memoryQueuePending
		entry.item.ClaimedAt = time.Time{}
		entry.item.LeaseUntil = time.Time{}
		if !notBefore.IsZero() {
			entry.item.DeliverAfter = notBefore.UTC()
		}
	}
	return nil
}

// RecordFailure applies the retry policy and returns ONLY the items that
// dead-lettered. Requeued items are not reported: the caller's dead-letter
// handling must not fire for an item that is going to be retried.
//
// A fence mismatch is permanent and dead-letters on the first failure. That
// rule is contract vocabulary rather than provider policy, so the reference
// carries it: the suite's own proof is that every assertion it does not
// capability-guard passes here, and an exempt reference would leave the
// assertion unproven against any implementation at all.
func (q *MemoryNudgeQueue) RecordFailure(ids []string, cause error, now time.Time) ([]nudgequeue.Item, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.verbPrologueLocked(now)
	permanent := errors.Is(cause, storebinding.ErrNudgeSessionFenceMismatch)
	rejected := errors.Is(cause, storebinding.ErrNudgeDeliveryRejected)
	var dead []nudgequeue.Item
	for _, id := range ids {
		entry := q.entries[id]
		if entry == nil || !entry.live() {
			continue
		}
		if expired(entry.item, now) {
			// It expired; it did not fail. No attempt is charged and the
			// dead letter is not reported as a delivery failure.
			deadLetterExpired(entry)
			continue
		}
		entry.item.Attempts++
		entry.item.LastAttemptAt = now.UTC()
		if cause != nil {
			entry.item.LastError = cause.Error()
		}
		entry.item.ClaimedAt = time.Time{}
		entry.item.LeaseUntil = time.Time{}
		if rejected {
			// Nothing was delivered, so the batch is no longer a delivery
			// to repeat: the next claim pins the item under a new key.
			entry.item.DeliveryBatch = ""
		}
		if permanent || entry.item.Attempts >= memoryQueueMaxAttempt {
			entry.item.DeadAt = now.UTC()
			entry.state = memoryQueueDead
			dead = append(dead, entry.item)
			continue
		}
		entry.item.DeliverAfter = now.Add(memoryQueueRetryDelay).UTC()
		entry.state = memoryQueuePending
	}
	return dead, nil
}

// Rollback dead-letters a queued item; an item that never landed is recorded
// so the failure stays observable.
func (q *MemoryNudgeQueue) Rollback(item nudgequeue.Item, reason string) error {
	if item.ID == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.verbPrologueLocked(time.Now().UTC())
	entry := q.entries[item.ID]
	if entry == nil {
		item.DeadAt = time.Now().UTC()
		item.LastError = reason
		q.insert(item, memoryQueueDead)
		return nil
	}
	entry.state = memoryQueueDead
	entry.item.DeadAt = time.Now().UTC()
	entry.item.LastError = reason
	return nil
}

// WithdrawQueuedWaitNudges removes still-queued nudges by ID.
func (q *MemoryNudgeQueue) WithdrawQueuedWaitNudges(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.verbPrologueLocked(time.Now().UTC())
	for _, id := range ids {
		entry := q.entries[id]
		if entry == nil || entry.state == memoryQueueTerminal {
			continue
		}
		entry.state = memoryQueueTerminal
		entry.terminalAt = time.Now().UTC()
	}
	return nil
}

// Maintain runs the whole-queue recovery pass: expired leases return to
// pending, expired items dead-letter, dead letters past their retention retire,
// and retired entries past theirs are forgotten so their ids can be reused.
// The pass is in-memory and holds the lock throughout, so ctx is only checked
// before it starts and it never reports Remaining work.
func (q *MemoryNudgeQueue) Maintain(ctx context.Context, now time.Time) (storebinding.NudgeMaintenance, error) {
	if err := ctx.Err(); err != nil {
		return storebinding.NudgeMaintenance{}, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.maintainLocked(now)
	return storebinding.NudgeMaintenance{}, nil
}

// maintainLocked is the whole-queue recovery pass.
func (q *MemoryNudgeQueue) maintainLocked(now time.Time) {
	q.expireLeases(now)
	kept := q.order[:0]
	for _, id := range q.order {
		entry := q.entries[id]
		if entry == nil {
			continue
		}
		switch entry.state {
		case memoryQueuePending, memoryQueueInFlight:
			if expired(entry.item, now) {
				deadLetterExpired(entry)
			}
		case memoryQueueDead:
			if !entry.item.DeadAt.IsZero() && entry.item.DeadAt.Before(now.Add(-memoryQueueDeadRetention)) {
				entry.state = memoryQueueTerminal
				entry.terminalAt = entry.item.DeadAt
			}
		}
		if entry.state == memoryQueueTerminal && !entry.terminalAt.IsZero() &&
			entry.terminalAt.Before(now.Add(-memoryQueueTerminalRetention)) {
			delete(q.entries, id)
			continue
		}
		kept = append(kept, id)
	}
	q.order = kept
}

// forgetLocked drops id from the queue entirely, as the legacy queue does when
// it acks: nothing remembers the id afterwards.
func (q *MemoryNudgeQueue) forgetLocked(id string) {
	delete(q.entries, id)
	kept := q.order[:0]
	for _, existing := range q.order {
		if existing != id {
			kept = append(kept, existing)
		}
	}
	q.order = kept
}

// Lookup reports the bucket each id is in as of now. Retired and unknown ids
// are absent; an expired item reads as dead.
func (q *MemoryNudgeQueue) Lookup(ids []string, now time.Time) (map[string]nudgequeue.Bucket, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.verbPrologueLocked(now)
	buckets := make(map[string]nudgequeue.Bucket, len(ids))
	for _, id := range ids {
		entry := q.entries[id]
		if entry == nil {
			continue
		}
		if bucket, ok := entry.bucketAt(now); ok {
			buckets[id] = bucket
		}
	}
	return buckets, nil
}

// DueKeys returns, sorted and unique, the non-empty queue keys with an item
// that reads as pending and is due at now.
func (q *MemoryNudgeQueue) DueKeys(now time.Time) ([]string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.verbPrologueLocked(now)
	seen := map[string]bool{}
	var keys []string
	for _, entry := range q.entries {
		if entry.item.Agent == "" || seen[entry.item.Agent] || entry.item.DeliverAfter.After(now) {
			continue
		}
		if bucket, ok := entry.bucketAt(now); !ok || bucket != nudgequeue.BucketPending {
			continue
		}
		seen[entry.item.Agent] = true
		keys = append(keys, entry.item.Agent)
	}
	sort.Strings(keys)
	return keys, nil
}

// Empty reports that the queue serves no pending, in-flight or dead item.
func (q *MemoryNudgeQueue) Empty() (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.verbPrologueLocked(time.Now().UTC())
	for _, entry := range q.entries {
		if entry.live() || entry.state == memoryQueueDead {
			return false, nil
		}
	}
	return true, nil
}

func (q *MemoryNudgeQueue) insert(item nudgequeue.Item, state memoryQueueState) {
	q.entries[item.ID] = &memoryQueueEntry{item: item, state: state}
	q.order = append(q.order, item.ID)
}

// expireLeases records the lease recovery the reads already project: in-flight
// items whose lease has run out return to pending. It runs only in Maintain.
func (q *MemoryNudgeQueue) expireLeases(now time.Time) {
	for _, entry := range q.entries {
		if entry.state != memoryQueueInFlight {
			continue
		}
		if entry.item.LeaseUntil.IsZero() || entry.item.LeaseUntil.After(now) {
			continue
		}
		entry.state = memoryQueuePending
		entry.item.ClaimedAt = time.Time{}
		entry.item.LeaseUntil = time.Time{}
	}
}

func (q *MemoryNudgeQueue) listMatching(now time.Time, match func(nudgequeue.Item) bool) (pending, inFlight, dead []nudgequeue.Item, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.verbPrologueLocked(now)
	for _, id := range q.order {
		entry := q.entries[id]
		if entry == nil || !match(entry.item) {
			continue
		}
		switch {
		case entry.state == memoryQueueDead:
			dead = append(dead, entry.item)
		case entry.live() && expired(entry.item, now):
			// An expired item reads as dead by arithmetic before Maintain
			// records it, the same way an expired lease reads as pending.
			dead = append(dead, expiredView(entry.item))
		case entry.inFlightAt(now):
			inFlight = append(inFlight, entry.item)
		case entry.live():
			pending = append(pending, unclaimed(entry.item))
		}
	}
	sortByDelivery(pending)
	sortByDelivery(inFlight)
	sortByDelivery(dead)
	return pending, inFlight, dead, nil
}

// expired reports whether item's deliver-by deadline has passed at now.
func expired(item nudgequeue.Item, now time.Time) bool {
	return !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now)
}

// expiredView is how an expired item reads, before and after it is recorded:
// dead since its deadline, with no claim.
func expiredView(item nudgequeue.Item) nudgequeue.Item {
	if item.LastError == "" {
		item.LastError = "expired"
	}
	item.DeadAt = item.ExpiresAt
	return unclaimed(item)
}

// unclaimed is item with its claim cleared.
func unclaimed(item nudgequeue.Item) nudgequeue.Item {
	item.ClaimedAt = time.Time{}
	item.LeaseUntil = time.Time{}
	return item
}

// deadLetterExpired records an expired entry as the dead letter it already
// reads as.
func deadLetterExpired(entry *memoryQueueEntry) {
	entry.item = expiredView(entry.item)
	entry.state = memoryQueueDead
}

// bucketAt is the bucket the entry reads as at now, or false when the queue no
// longer serves it.
func (e *memoryQueueEntry) bucketAt(now time.Time) (nudgequeue.Bucket, bool) {
	switch {
	case e.state == memoryQueueDead:
		return nudgequeue.BucketDead, true
	case e.live() && expired(e.item, now):
		return nudgequeue.BucketDead, true
	case e.inFlightAt(now):
		return nudgequeue.BucketInFlight, true
	case e.live():
		return nudgequeue.BucketPending, true
	default:
		return "", false
	}
}

// live reports whether the entry is still queued for delivery.
func (e *memoryQueueEntry) live() bool {
	return e.state == memoryQueuePending || e.state == memoryQueueInFlight
}

// inFlightAt reports whether the entry holds an unexpired claim at now. An
// expired lease reads as pending by arithmetic; Maintain records it.
func (e *memoryQueueEntry) inFlightAt(now time.Time) bool {
	return e.state == memoryQueueInFlight && e.item.LeaseUntil.After(now)
}

// matchesQueueKeys reports whether agent is one of keys. The empty key
// addresses nobody.
func matchesQueueKeys(agent string, keys []string) bool {
	if agent == "" {
		return false
	}
	for _, key := range keys {
		if key == agent {
			return true
		}
	}
	return false
}

func sortByDelivery(items []nudgequeue.Item) {
	sort.SliceStable(items, func(i, j int) bool { return deliveredBefore(items[i], items[j]) })
}

// deliveredBefore is the canonical delivery order.
func deliveredBefore(left, right nudgequeue.Item) bool {
	if !left.DeliverAfter.Equal(right.DeliverAfter) {
		return left.DeliverAfter.Before(right.DeliverAfter)
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.Before(right.CreatedAt)
	}
	return left.ID < right.ID
}
