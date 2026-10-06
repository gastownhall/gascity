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
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
)

const (
	memoryQueueClaimLease = 2 * time.Minute
	memoryQueueRetryDelay = 15 * time.Second
	memoryQueueMaxAttempt = 5
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
	q.expireLeases(time.Now())
	if _, exists := q.entries[item.ID]; exists {
		return nil
	}
	if item.Reference != nil && item.Reference.ID != "" {
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
	q.expireLeases(now)
	var claimed []nudgequeue.Item
	for _, id := range q.order {
		entry := q.entries[id]
		if entry == nil || entry.state != memoryQueuePending {
			continue
		}
		if !matchesQueueKeys(entry.item.Agent, target.QueueKeys) {
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
	return q.listMatching(now, func(item nudgequeue.Item) bool { return item.Agent == agentName })
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
	q.mu.Lock()
	defer q.mu.Unlock()
	var state nudgequeue.State
	for _, id := range q.order {
		entry := q.entries[id]
		if entry == nil {
			continue
		}
		switch entry.state {
		case memoryQueuePending:
			state.Pending = append(state.Pending, entry.item)
		case memoryQueueInFlight:
			state.InFlight = append(state.InFlight, entry.item)
		case memoryQueueDead:
			state.Dead = append(state.Dead, entry.item)
		}
	}
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
		if entry.state == memoryQueuePending || entry.state == memoryQueueInFlight {
			entry.state = memoryQueueTerminal
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
		if entry == nil || (entry.state != memoryQueuePending && entry.state != memoryQueueInFlight) {
			continue
		}
		entry.item.Attempts++
		entry.item.LastAttemptAt = now.UTC()
		if cause != nil {
			entry.item.LastError = cause.Error()
		}
		entry.item.ClaimedAt = time.Time{}
		entry.item.LeaseUntil = time.Time{}
		expired := !entry.item.ExpiresAt.IsZero() && !entry.item.ExpiresAt.After(now)
		if permanent || entry.item.Attempts >= memoryQueueMaxAttempt || expired {
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
	}
	return nil
}

func (q *MemoryNudgeQueue) insert(item nudgequeue.Item, state memoryQueueState) {
	q.entries[item.ID] = &memoryQueueEntry{item: item, state: state}
	q.order = append(q.order, item.ID)
}

// expireLeases returns in-flight items whose lease has run out to pending. It
// is the recovery pass a crashed deliverer depends on.
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
	q.expireLeases(now)
	for _, id := range q.order {
		entry := q.entries[id]
		if entry == nil || !match(entry.item) {
			continue
		}
		switch entry.state {
		case memoryQueuePending:
			pending = append(pending, entry.item)
		case memoryQueueInFlight:
			inFlight = append(inFlight, entry.item)
		case memoryQueueDead:
			dead = append(dead, entry.item)
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

func matchesQueueKeys(agent string, keys []string) bool {
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
