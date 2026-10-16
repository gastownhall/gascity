package main

// The reversible queue-mode shim (DES-NQ D12). A city switches its nudge queue
// between the flock'd state.json file and the beads queue by config, and each
// process reads the mode for itself, so for a while some processes write the
// side the city has left. The shim closes that window: run in the direction
// of the configured mode, it moves the inactive side's live work to the
// active side.
//
// What moves is live work, in whole delivery batches. Dead letters stay where
// they died and age out there. A batch moves only when no live member holds an
// unexpired lease, and then every live member moves together.
//
// A moved copy keeps every field but its claim and its delivery batch: the
// destination pins it under a key of its own when it next claims it. A send
// under a key is the same delivery to a runtime that deduplicates by key, so a
// part of a batch sent under the batch's key, followed by a send of the rest
// under that key, would replay the part's receipt and ack the rest unseen
// (DES-NQ invariant 5). The source keeps the key and only ever loses members,
// so every later send under it is a subset of an earlier one, which a replay
// answers truthfully; the destination never sees the key, so a move that
// lands only part of a batch (a failure part way through it, or a member the
// destination already retired) cannot send that part under it. The cost is a
// duplicate turn when a send under the old key had already committed with an
// unknown outcome, never a loss.
//
// Every step is idempotent, so a crash between the two steps of a move re-runs
// as a no-op on the destination:
//
//   - file → beads: EnqueueDeferred into beads (idempotent by the id-derived
//     bead id), then Lookup the ids and remove from the file only the batches
//     whose every member is live in beads, whole or not at all. An id the
//     beads queue already retired inside its retired-id memory (a quick flip
//     back) does not come back to life there, so its batch stays in the file
//     queue and is reported, never dropped.
//   - beads → file: one indexed read of the live records, then append to the
//     file queue unless the id is already there, then Ack the records
//     "migrated". An id the file queue holds as a dead letter is not
//     imported; its record is retired with a reason saying so, and reported.
//
// A process still writing the old mode can claim an item between the shim's
// read and its write. Both queues are at least once (DES-NQ §9), and the
// result is at most a duplicate delivery, never a loss: file → beads leaves
// the claimed item's whole batch in the file queue (Raced), and beads → file
// retires a record whose claimant still delivers it.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// nudgeQueueShimDirection is the side a shim pass moves live work to.
type nudgeQueueShimDirection int

const (
	// nudgeQueueShimToBeads moves the file queue's live work into the beads
	// queue: the direction of beads mode.
	nudgeQueueShimToBeads nudgeQueueShimDirection = iota + 1
	// nudgeQueueShimToFile moves the beads queue's live work into the file
	// queue: the direction of file mode.
	nudgeQueueShimToFile
)

// String names the direction for diagnostics.
func (d nudgeQueueShimDirection) String() string {
	switch d {
	case nudgeQueueShimToBeads:
		return "file-to-beads"
	case nudgeQueueShimToFile:
		return "beads-to-file"
	default:
		return fmt.Sprintf("nudgeQueueShimDirection(%d)", int(d))
	}
}

// Ack vocabulary for a beads record the shim moved to the file queue.
const (
	nudgeQueueMigratedOutcome = "migrated"
	nudgeQueueMigratedReason  = "imported into the file queue"
	// nudgeQueueMigratedDeadReason retires a record whose id the file queue
	// already holds as a dead letter, so it was not imported.
	nudgeQueueMigratedDeadReason = "already a dead letter in the file queue"
	nudgeQueueMigratedBoundary   = "queue-mode-switch"
)

// nudgeQueueShimBeads is the beads queue's side of the shim: the queue, plus
// the one indexed read of its live records that the beads → file direction
// moves from. The beads queue NewBeadsNudgeQueue builds implements it.
type nudgeQueueShimBeads interface {
	storebinding.NudgeQueue
	// ListLive returns the live items, pending and in flight, skipping an
	// undecodable record and reporting it as a partial result.
	ListLive(now time.Time) (pending, inFlight []nudgequeue.Item, err error)
}

// nudgeQueueShimFile is the file queue's side of the shim. Its writes run
// under the file queue's lock; *fileNudgeQueue implements it.
type nudgeQueueShimFile interface {
	// migrationCandidates reads, without the lock, the live items that may
	// move out of the file queue, grouped into whole batches.
	migrationCandidates(now time.Time) (nudgeMigrationCandidates, error)
	// removeMigrated removes one moved batch's items, all or none: none
	// when any of them holds an unexpired lease. It returns the ones it left
	// in place.
	removeMigrated(items []nudgequeue.Item, now time.Time) ([]nudgequeue.Item, error)
	// importMigrated appends the moved items as pending, unpinned and
	// unclaimed, unless their id is already queued, and returns the ids it skipped because the queue holds
	// them as dead letters.
	importMigrated(items []nudgequeue.Item) (dead []string, err error)
}

var _ nudgeQueueShimFile = (*fileNudgeQueue)(nil)

// nudgeMigrationBatch is one delivery batch the shim moves or holds whole.
type nudgeMigrationBatch struct {
	// Key is the members' delivery batch; an unpinned item is a batch of its
	// own with the empty key.
	Key   string
	Items []nudgequeue.Item
}

// nudgeMigrationCandidates is one side's live work, in whole batches.
type nudgeMigrationCandidates struct {
	// Movable batches have no live member holding an unexpired lease.
	Movable []nudgeMigrationBatch
	// LeaseHeld batches have at least one; they wait for the lease to end.
	LeaseHeld []nudgeMigrationBatch
}

// nudgeQueueShimReport counts what one shim pass did, one count per batch.
type nudgeQueueShimReport struct {
	// Direction is the side the pass moved work to.
	Direction nudgeQueueShimDirection
	// Moved batches now live only on the destination side.
	Moved int
	// LeaseHeld batches wait for a member's unexpired lease to end.
	LeaseHeld int
	// Raced batches reached the beads queue, but a process still writing the
	// file queue claimed a member, or replaced or removed one, between the
	// shim's read and its removal. The batch stays whole in the file queue
	// too, so it may be delivered twice; it is never lost.
	Raced int
	// Retired batches stay in the file queue because the beads queue does
	// not serve every member: it already retired the id (a quick flip back
	// inside its retired-id memory) or dead-lettered it.
	Retired int
	// RetiredIDs lists the ids that kept their batches in the file queue.
	RetiredIDs []string
	// AlreadyDead batches reached the file queue only in part: the file
	// queue holds a member's id as a dead letter, so that member was not
	// imported, and its beads record is retired as such.
	AlreadyDead int
	// AlreadyDeadIDs lists the ids the file queue already held dead.
	AlreadyDeadIDs []string
	// Failed batches hit an error; each is in the pass's returned error, and
	// the next pass retries them.
	Failed int
}

// shimNudgeQueue runs one shim pass at now in direction. A read that fails
// before any batch moves fails the pass; a read of the beads queue that skips
// undecodable records moves the rest and joins the partial result into the
// returned error. A batch that fails is counted and joined into the returned
// error, and the pass goes on to the next batch; ctx is checked between
// batches.
func shimNudgeQueue(ctx context.Context, direction nudgeQueueShimDirection, file nudgeQueueShimFile, beadsQueue nudgeQueueShimBeads, now time.Time) (nudgeQueueShimReport, error) {
	report := nudgeQueueShimReport{Direction: direction}
	if file == nil || beadsQueue == nil {
		return report, fmt.Errorf("nudge queue shim %s: both queues are required", direction)
	}
	switch direction {
	case nudgeQueueShimToBeads:
		return shimNudgeQueueToBeads(ctx, report, file, beadsQueue, now)
	case nudgeQueueShimToFile:
		return shimNudgeQueueToFile(ctx, report, file, beadsQueue, now)
	default:
		return report, fmt.Errorf("nudge queue shim: unknown direction %s", direction)
	}
}

func shimNudgeQueueToBeads(ctx context.Context, report nudgeQueueShimReport, file nudgeQueueShimFile, beadsQueue nudgeQueueShimBeads, now time.Time) (nudgeQueueShimReport, error) {
	candidates, err := file.migrationCandidates(now)
	if err != nil {
		return report, fmt.Errorf("nudge queue shim %s: reading the file queue: %w", report.Direction, err)
	}
	report.LeaseHeld = len(candidates.LeaseHeld)
	var failures []error
	for _, batch := range candidates.Movable {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(append(failures, err)...)
		}
		if err := moveNudgeBatchToBeads(&report, batch, file, beadsQueue, now); err != nil {
			report.Failed++
			failures = append(failures, fmt.Errorf("nudge queue shim %s: batch %s: %w", report.Direction, nudgeMigrationBatchName(batch), err))
		}
	}
	return report, errors.Join(failures...)
}

// moveNudgeBatchToBeads moves one batch from the file queue into beads and
// counts it. An error leaves the batch's count to the caller.
func moveNudgeBatchToBeads(report *nudgeQueueShimReport, batch nudgeMigrationBatch, file nudgeQueueShimFile, beadsQueue nudgeQueueShimBeads, now time.Time) error {
	ids := make([]string, 0, len(batch.Items))
	for _, item := range batch.Items {
		if err := beadsQueue.EnqueueDeferred(nudgeItemForMove(item)); err != nil {
			return fmt.Errorf("enqueueing %q into the beads queue: %w", item.ID, err)
		}
		ids = append(ids, item.ID)
	}
	// A partial Lookup fails the batch too: the record it skipped is one of
	// the batch's own, and removing the rest would split the batch.
	buckets, err := beadsQueue.Lookup(ids, now)
	if err != nil {
		return fmt.Errorf("looking up the moved ids in the beads queue: %w", err)
	}
	var unserved []string
	for _, id := range ids {
		switch buckets[id] {
		case nudgequeue.BucketPending, nudgequeue.BucketInFlight:
		default:
			unserved = append(unserved, id)
		}
	}
	if len(unserved) > 0 {
		report.Retired++
		report.RetiredIDs = append(report.RetiredIDs, unserved...)
		return nil
	}
	kept, err := file.removeMigrated(batch.Items, now)
	if err != nil {
		return fmt.Errorf("removing the moved items from the file queue: %w", err)
	}
	if len(kept) > 0 {
		report.Raced++
		return nil
	}
	report.Moved++
	return nil
}

func shimNudgeQueueToFile(ctx context.Context, report nudgeQueueShimReport, file nudgeQueueShimFile, beadsQueue nudgeQueueShimBeads, now time.Time) (nudgeQueueShimReport, error) {
	pending, inFlight, err := beadsQueue.ListLive(now)
	var failures []error
	if err != nil {
		if !beads.IsPartialResult(err) {
			return report, fmt.Errorf("nudge queue shim %s: reading the beads queue: %w", report.Direction, err)
		}
		failures = append(failures, fmt.Errorf("nudge queue shim %s: reading the beads queue: %w", report.Direction, err))
	}
	live := make([]nudgequeue.Item, 0, len(pending)+len(inFlight))
	live = append(live, pending...)
	live = append(live, inFlight...)
	// A record the read returns as in flight holds an unexpired lease by
	// construction; an expired lease reads as pending.
	leased := make(map[string]bool, len(inFlight))
	for _, item := range inFlight {
		leased[item.ID] = true
	}
	candidates := groupNudgeMigrationBatches(live, func(item nudgequeue.Item) bool { return leased[item.ID] })
	report.LeaseHeld = len(candidates.LeaseHeld)
	for _, batch := range candidates.Movable {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(append(failures, err)...)
		}
		if err := moveNudgeBatchToFile(&report, batch, file, beadsQueue); err != nil {
			report.Failed++
			failures = append(failures, fmt.Errorf("nudge queue shim %s: batch %s: %w", report.Direction, nudgeMigrationBatchName(batch), err))
		}
	}
	return report, errors.Join(failures...)
}

// moveNudgeBatchToFile imports one batch into the file queue, then retires
// its beads records as migrated, and counts it. An error leaves the batch's
// count to the caller.
func moveNudgeBatchToFile(report *nudgeQueueShimReport, batch nudgeMigrationBatch, file nudgeQueueShimFile, beadsQueue storebinding.NudgeQueue) error {
	dead, err := file.importMigrated(batch.Items)
	if err != nil {
		return fmt.Errorf("importing into the file queue: %w", err)
	}
	skipped := make(map[string]bool, len(dead))
	for _, id := range dead {
		skipped[id] = true
	}
	imported := make([]string, 0, len(batch.Items))
	for _, item := range batch.Items {
		if !skipped[item.ID] {
			imported = append(imported, item.ID)
		}
	}
	if err := beadsQueue.Ack(imported, nudgeQueueMigratedOutcome, nudgeQueueMigratedReason, nudgeQueueMigratedBoundary); err != nil {
		return fmt.Errorf("retiring the imported records as migrated: %w", err)
	}
	if len(dead) == 0 {
		report.Moved++
		return nil
	}
	if err := beadsQueue.Ack(dead, nudgeQueueMigratedOutcome, nudgeQueueMigratedDeadReason, nudgeQueueMigratedBoundary); err != nil {
		return fmt.Errorf("retiring the records the file queue holds dead: %w", err)
	}
	report.AlreadyDead++
	report.AlreadyDeadIDs = append(report.AlreadyDeadIDs, dead...)
	return nil
}

// groupNudgeMigrationBatches groups live items into whole delivery batches,
// in the order their first member appears. An unpinned item is a batch of its
// own. A batch with a member for which leased reports true is held.
func groupNudgeMigrationBatches(live []nudgequeue.Item, leased func(nudgequeue.Item) bool) nudgeMigrationCandidates {
	var batches []nudgeMigrationBatch
	held := map[int]bool{}
	index := map[string]int{}
	for _, item := range live {
		at, pinned := index[item.DeliveryBatch]
		if item.DeliveryBatch == "" || !pinned {
			at = len(batches)
			batches = append(batches, nudgeMigrationBatch{Key: item.DeliveryBatch})
			if item.DeliveryBatch != "" {
				index[item.DeliveryBatch] = at
			}
		}
		batches[at].Items = append(batches[at].Items, item)
		if leased(item) {
			held[at] = true
		}
	}
	var candidates nudgeMigrationCandidates
	for at, batch := range batches {
		if held[at] {
			candidates.LeaseHeld = append(candidates.LeaseHeld, batch)
			continue
		}
		candidates.Movable = append(candidates.Movable, batch)
	}
	return candidates
}

// nudgeLeaseHeld reports whether item holds a claim lease that has not run
// out at now.
func nudgeLeaseHeld(item nudgequeue.Item, now time.Time) bool {
	return !item.LeaseUntil.IsZero() && item.LeaseUntil.After(now)
}

// nudgeItemForMove is the copy of item the shim writes to the destination
// queue: its claim and its delivery batch cleared, every other field kept.
// The destination pins it under a key of its own.
func nudgeItemForMove(item nudgequeue.Item) nudgequeue.Item {
	item.ClaimedAt = time.Time{}
	item.LeaseUntil = time.Time{}
	item.DeliveryBatch = ""
	return item
}

// nudgeMigrationKey identifies one queued item across a move, as
// WithdrawWaitNudges identifies one: a re-queued id with a new creation time
// is a different item.
type nudgeMigrationKey struct {
	id        string
	createdAt time.Time
}

func nudgeMigrationKeyOf(item nudgequeue.Item) nudgeMigrationKey {
	return nudgeMigrationKey{id: item.ID, createdAt: item.CreatedAt.UTC()}
}

// nudgeMigrationBatchName names a batch in an error: its delivery key, or its
// one unpinned item.
func nudgeMigrationBatchName(batch nudgeMigrationBatch) string {
	if batch.Key != "" {
		return fmt.Sprintf("%q", batch.Key)
	}
	if len(batch.Items) == 1 {
		return fmt.Sprintf("of nudge %q", batch.Items[0].ID)
	}
	return "(unpinned)"
}
