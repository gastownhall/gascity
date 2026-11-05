package storebinding

// The beads queue's batch claim.
//
// A delivery batch is the set of nudges delivered together under one key. The
// key is pinned on each item at claim time, and until the batch is acked,
// dead-lettered or definitively rejected its items are claimed and delivered
// together under that key, so a redelivery after an unknown outcome is the same
// delivery to a runtime that deduplicates by key. A strict subset would let the
// runtime commit the partial text and later replay that receipt for the
// missing items, acking them unseen. So a pinned batch is claimed whole or not
// at all.
//
// ONE GUARDED WRITE PER ITEM. The batch claim takes each item with
// UpdateIfMatch(revision, {assignee: lease, status: in_progress,
// delivery_batch: key}), so claiming and pinning land together. It contends
// with ClaimDue's Claim on the same records, and both kinds change the row: a
// Claim after the update sees a foreign assignee, and an update after a Claim
// fails its revision. Either way one claimant wins each item.
//
// MEMBERSHIP BY KEY. A target's keyed reads cannot see a member addressed to a
// queue key outside its current key set, so whether a pinned batch is whole is
// decided from one indexed read of the batch's own records.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// nudgeQueueBatchKeyPrefix names a minted delivery batch key; the deployed
// queue mints its keys in the same shape.
const nudgeQueueBatchKeyPrefix = "nudge-batch-"

// ClaimBatch claims target's next delivery batch.
//
// It reads the target's candidates with one keyed List per queue key. For each
// distinct batch key among the candidates, in canonical candidate order, it
// reads the batch's members; the first batch that can be claimed whole (no
// member in flight, every live member due, addressed to one of the target's
// keys and passing the fence) is claimed: every member, under its key, and
// nothing else. If one of those writes loses to another claimant, what this
// call took is released with the pin kept, and it returns no items and no
// error: the batch waits for the next pass. Otherwise it mints a new key and
// claims every due unpinned candidate under it; a lost write there skips that
// item, because a batch that has never been sent is safe to make smaller.
//
// The items come back in canonical order with ClaimedAt, LeaseUntil and
// DeliveryBatch set. Records it could not decode are skipped and reported as a
// partial result.
func (q *beadsNudgeQueue) ClaimBatch(target ClaimTarget, now time.Time) ([]nudgequeue.Item, error) {
	if len(target.QueueKeys) == 0 {
		return nil, nil
	}
	entries, corrupt, err := q.keyedEntries(target.QueueKeys, now)
	if err != nil {
		return nil, err
	}
	fence := newNudgeClaimFence(target)
	candidates, err := q.claimCandidates(entries, fence, now)
	if err != nil {
		return nil, err
	}

	examined := map[string]bool{}
	for _, candidate := range candidates {
		key := candidate.item.DeliveryBatch
		if key == "" || examined[key] {
			continue
		}
		examined[key] = true
		members, whole, skipped, err := q.wholeBatch(key, target, fence, now)
		corrupt = append(corrupt, skipped...)
		if err != nil {
			return nil, err
		}
		if !whole {
			continue
		}
		claimed, err := q.claimResumedBatch(key, members, now)
		if err != nil {
			return nil, err
		}
		return claimed, nudgeQueueQuarantined("ClaimBatch", corrupt)
	}

	claimed, err := q.claimNewBatch(candidates, now)
	if err != nil {
		return claimed, err
	}
	return claimed, nudgeQueueQuarantined("ClaimBatch", corrupt)
}

// wholeBatch reads the open members of batch key and reports whether target
// can claim them whole: none in flight, and every live member due, addressed
// to one of target's keys, and passing the fence. Members past their deadline
// read as dead and are not part of the batch any more. A member that does not
// decode cannot be vouched for, so its batch is not whole; it is returned for
// the partial result.
func (q *beadsNudgeQueue) wholeBatch(key string, target ClaimTarget, fence *nudgeClaimFence, now time.Time) ([]nudgeQueueEntry, bool, []error, error) {
	records, err := q.store.List(nudgeQueueBatchQuery(key))
	if err != nil {
		return nil, false, nil, fmt.Errorf("listing nudge delivery batch %q: %w", key, err)
	}
	entries, corrupt := decodeNudgeQueueRecords(records, now)
	if len(corrupt) > 0 {
		return nil, false, corrupt, nil
	}
	var members []nudgeQueueEntry
	for _, entry := range entries {
		if entry.state != nudgeQueueStateLive && entry.state != nudgeQueueInFlight {
			continue
		}
		if nudgeQueueItemExpired(entry.item, now) {
			continue
		}
		if entry.state == nudgeQueueInFlight || entry.item.DeliverAfter.After(now) ||
			!nudgeQueueKeysContain(target.QueueKeys, entry.item.Agent) || !fence.passes(entry.item) {
			return nil, false, nil, nil
		}
		members = append(members, entry)
	}
	nudgeQueueSortPending(members)
	return members, len(members) > 0, nil, nil
}

// claimResumedBatch claims every member of a whole pinned batch under one
// lease, keeping its key. If any write loses, it gives back what it took and
// claims nothing.
func (q *beadsNudgeQueue) claimResumedBatch(key string, members []nudgeQueueEntry, now time.Time) ([]nudgequeue.Item, error) {
	lease, err := newNudgeLease(now)
	if err != nil {
		return nil, err
	}
	claimed := make([]nudgequeue.Item, 0, len(members))
	for _, member := range members {
		won, err := q.claimAndPin(member, lease, key)
		if err == nil && won {
			claimed = append(claimed, nudgeQueueClaimedItem(member.item, lease, key))
			continue
		}
		if releaseErr := q.releaseTaken(claimed, lease); releaseErr != nil {
			return nil, errors.Join(err, releaseErr)
		}
		return nil, err
	}
	return claimed, nil
}

// claimNewBatch pins every due unpinned candidate to a newly minted key. A
// candidate another claimant took first is skipped.
func (q *beadsNudgeQueue) claimNewBatch(candidates []nudgeQueueEntry, now time.Time) ([]nudgequeue.Item, error) {
	var claimed []nudgequeue.Item
	var lease nudgeLease
	key := ""
	for _, candidate := range candidates {
		if candidate.item.DeliveryBatch != "" {
			continue
		}
		if key == "" {
			minted, err := newNudgeQueueBatchKey()
			if err != nil {
				return nil, err
			}
			if lease, err = newNudgeLease(now); err != nil {
				return nil, err
			}
			key = minted
		}
		won, err := q.claimAndPin(candidate, lease, key)
		if err != nil {
			return claimed, err
		}
		if won {
			claimed = append(claimed, nudgeQueueClaimedItem(candidate.item, lease, key))
		}
	}
	return claimed, nil
}

// claimAndPin is the batch claim's one guarded write: lease and pin entry at
// the revision its decision rests on. It reports false when the record moved
// since the claim decided on it. A write refused at a revision that was not
// the row's current one, while the fields the decision rested on are
// unchanged, is presented again at the current revision: a store whose List
// leaves revisions unstamped, or a cache that served such a row to the
// re-read, hands over no usable token on the first attempt. Refusals that
// outlast nudgeQueueCASAttempts with the decided fields still unchanged are a
// fault, not a lost race, and are returned as an error.
func (q *beadsNudgeQueue) claimAndPin(entry nudgeQueueEntry, lease nudgeLease, key string) (bool, error) {
	revision := entry.bead.Revision
	for attempt := 0; attempt < nudgeQueueCASAttempts; attempt++ {
		if revision == 0 || attempt > 0 {
			current, unchanged, err := q.decidedRecord(entry)
			if err != nil || !unchanged {
				return false, err
			}
			revision = current
		}
		assignee, inProgress := lease.assignee, "in_progress"
		err := q.writer.UpdateIfMatch(entry.bead.ID, revision, beads.UpdateOpts{
			Assignee: &assignee,
			Status:   &inProgress,
			Metadata: map[string]string{nudgeMetaDeliveryBatch: key},
		})
		if err == nil {
			return true, nil
		}
		if errors.Is(err, beads.ErrNotFound) {
			return false, nil
		}
		var stale *beads.PreconditionFailedError
		if !errors.As(err, &stale) {
			return false, fmt.Errorf("claiming nudge %q into batch %q: %w", entry.item.ID, key, err)
		}
	}
	return false, fmt.Errorf("claiming nudge %q into batch %q: %d revision conflicts", entry.item.ID, key, nudgeQueueCASAttempts)
}

// decidedRecord reads entry's record again and reports its revision and
// whether the fields the claim decided on (lifecycle, lease, status, pin and
// deliver-after) are unchanged since entry was read.
func (q *beadsNudgeQueue) decidedRecord(entry nudgeQueueEntry) (int64, bool, error) {
	record, err := q.store.Get(entry.bead.ID)
	if errors.Is(err, beads.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading nudge %q before claiming it: %w", entry.item.ID, err)
	}
	if record.Assignee != entry.bead.Assignee || record.Status != entry.bead.Status {
		return 0, false, nil
	}
	for _, key := range []string{nudgeMetaQueueState, nudgeMetaDeliveryBatch, nudgeMetaDeliverAfter} {
		if record.Metadata[key] != entry.bead.Metadata[key] {
			return 0, false, nil
		}
	}
	return record.Revision, true, nil
}

// releaseTaken returns the items a losing resumed claim took, by the lease it
// took them under. The pin is untouched, so the batch stays whole for the
// next pass.
func (q *beadsNudgeQueue) releaseTaken(items []nudgequeue.Item, lease nudgeLease) error {
	var failures []error
	for _, item := range items {
		if _, err := q.releaser.ReleaseIfCurrent(nudgeQueueBeadID(item.ID), lease.assignee); err != nil {
			failures = append(failures, fmt.Errorf("releasing nudge %q from a batch that could not be claimed whole: %w", item.ID, err))
		}
	}
	return errors.Join(failures...)
}

// nudgeQueueClaimedItem is item as a claim under lease in batch key hands it
// back.
func nudgeQueueClaimedItem(item nudgequeue.Item, lease nudgeLease, key string) nudgequeue.Item {
	item.ClaimedAt = lease.claimedAt
	item.LeaseUntil = lease.until
	item.DeliveryBatch = key
	return item
}

// nudgeQueueBatchQuery is a batch's membership read. delivery_batch is written
// only by queue records, so the engine's metadata index visits the batch's
// rows alone; closed (retired) records are excluded by the read itself.
func nudgeQueueBatchQuery(key string) beads.ListQuery {
	return beads.ListQuery{
		Label:    nudgeQueueLabel,
		Type:     nudgeQueueBeadType,
		Metadata: map[string]string{nudgeMetaDeliveryBatch: key},
		Sort:     beads.SortCreatedAsc,
		TierMode: beads.TierBoth,
		Live:     true,
	}
}

// nudgeQueueKeysContain reports whether key is one of keys. The empty key
// addresses nobody.
func nudgeQueueKeysContain(keys []string, key string) bool {
	if key == "" {
		return false
	}
	for _, candidate := range keys {
		if candidate == key {
			return true
		}
	}
	return false
}

// newNudgeQueueBatchKey mints a delivery batch key.
func newNudgeQueueBatchKey() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("minting nudge delivery batch key: %w", err)
	}
	return nudgeQueueBatchKeyPrefix + hex.EncodeToString(buf[:]), nil
}
