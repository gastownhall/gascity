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
	"errors"
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
}

// NewMemoryNudgeQueue returns an empty in-memory queue.
func NewMemoryNudgeQueue() *MemoryNudgeQueue {
	return &MemoryNudgeQueue{entries: map[string]*memoryQueueEntry{}}
}

// Enqueue inserts item as pending. A duplicate ID is a no-op, matching the
// deployed queue's idempotent submit.
func (q *MemoryNudgeQueue) Enqueue(item nudgequeue.Item) error {
	q.mu.Lock()
	defer q.mu.Unlock()
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
	var claimed []nudgequeue.Item
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
		if !passesFence(entry.item, target) {
			continue
		}
		entry.item.ClaimedAt = now.UTC()
		entry.item.LeaseUntil = now.Add(memoryQueueClaimLease).UTC()
		entry.state = memoryQueueInFlight
		claimed = append(claimed, entry.item)
	}
	sortByDelivery(claimed)
	return claimed, nil
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
	for _, id := range ids {
		entry := q.entries[id]
		if entry == nil {
			continue
		}
		if entry.live() && expired(entry.item, time.Now()) {
			deadLetterExpired(entry)
			continue
		}
		if entry.live() {
			entry.state = memoryQueueTerminal
			entry.terminalAt = time.Now().UTC()
			entry.item.ClaimedAt = time.Time{}
			entry.item.LeaseUntil = time.Time{}
		}
	}
	return nil
}

// ReleaseClaims returns undelivered in-flight ids to pending.
func (q *MemoryNudgeQueue) ReleaseClaims(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range ids {
		entry := q.entries[id]
		if entry == nil || entry.state != memoryQueueInFlight {
			continue
		}
		if expired(entry.item, time.Now()) {
			deadLetterExpired(entry)
			continue
		}
		entry.state = memoryQueuePending
		entry.item.ClaimedAt = time.Time{}
		entry.item.LeaseUntil = time.Time{}
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
	permanent := errors.Is(cause, storebinding.ErrNudgeSessionFenceMismatch)
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
	return storebinding.NudgeMaintenance{}, nil
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

// passesFence applies the claim fence: a session-pinned item is claimable only
// by that exact session, and an epoch-pinned unpinned-session item only by a
// target that carries a continuation epoch at all.
func passesFence(item nudgequeue.Item, target storebinding.ClaimTarget) bool {
	if item.SessionID != "" {
		return target.SessionID != "" && item.SessionID == target.SessionID
	}
	return item.ContinuationEpoch == "" || target.ContinuationEpoch != ""
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
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].DeliverAfter.Equal(items[j].DeliverAfter) {
			return items[i].DeliverAfter.Before(items[j].DeliverAfter)
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
}
