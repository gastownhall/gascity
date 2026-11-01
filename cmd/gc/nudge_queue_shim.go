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
// A moved copy keeps every field but its claim, its delivery batch and, when
// it moves into the file queue, its BeadID, which the file queue points at the
// shadow it saves for the item (beads → file, below). The destination pins the
// copy under a key of its own when it next claims it. A send under a key is
// the same delivery to a runtime that deduplicates by key, so a part of a
// batch sent under the batch's key, followed by a send of the rest under that
// key, would replay the part's receipt and ack the rest unseen (DES-NQ
// invariant 5). The source keeps the key and only ever loses members, so every
// later send under it is a subset of an earlier one, which a replay answers
// truthfully; the destination never sees the key, so a move that lands only
// part of a batch (a failure part way through it, or a member the destination
// already retired) cannot send that part under it. The cost is a duplicate
// turn when a send under the old key had already committed with an unknown
// outcome, never a loss.
//
// Every step is idempotent, so a crash between the two steps of a move re-runs
// as a no-op on the destination, unless the destination delivers the copy
// before the re-run. The re-run then cannot tell that the copy was made:
// beads → file imports the item again, and file → beads leaves its batch in
// the file queue and reports it. Either is at most a duplicate delivery,
// never a loss. The two steps are:
//
//   - file → beads: EnqueueDeferred into beads (idempotent by the id-derived
//     bead id), then Lookup the ids and remove from the file only the batches
//     whose every member is live in beads, whole or not at all. An id the
//     beads queue already retired inside its retired-id memory does not come
//     back to life there, so its batch stays in the file queue and is
//     reported. After a quick flip back (beads → file → beads) that strands
//     the batch: its record is retired "migrated", and no beads-mode process
//     delivers from the file queue. The sweep deletes the record no sooner
//     than 24h after the move, later than the default deadline, 24h from the
//     nudge's creation, so such a nudge expires in the file queue undelivered.
//     NQ-3b owes the fix, ReinstateMigrated (DES-NQ D12, ga-w0hgvz). Both
//     steps go by id, so an id the beads queue holds live under another
//     creation time reads as moved, and the file item is removed without
//     being copied (ga-1d75jm).
//   - beads → file: one indexed read of the live records, then append to the
//     file queue unless it already holds the item, matched by (ID,
//     CreatedAt), then Ack the records "migrated". File mode's wait readers
//     find an item's gc:nudge shadow by its nudge id, and its Ack terminalizes
//     the shadow BeadID names. A beads-born item never had a shadow, and a
//     beads-mode sweep may have closed the one a file-born item left, so the
//     append saves each item an open shadow, as Enqueue does, and points
//     BeadID at it; a re-run reuses the open shadow it finds rather than
//     saving a second one. The beads shadow view reads a record retired
//     "migrated" as no record, so its readers fall through to that shadow. An
//     item the file queue holds as a dead letter is not imported; its record
//     is retired with a reason saying so, and reported. An id the file queue
//     holds under another creation time is a different nudge: it is not
//     imported, its record stays live, and it is reported.
//
// A process still writing the old mode can claim an item between the shim's
// read and its write. Both queues are at least once (DES-NQ §9), and the
// result is at most a duplicate delivery, never a loss: file → beads leaves
// the claimed item's whole batch in the file queue (Raced), and beads → file
// retires a record whose claimant still delivers it.
//
// A withdrawal or a rollback racing the move is not carried across. The shim
// has no fence spanning the two stores, so a member withdrawn (its wait
// canceled) or rolled back in the source after the read is still written to
// the destination and may be delivered from there. Copying its fate across
// afterwards would only narrow the window, since the destination can claim
// the copy first. Nothing is lost; NQ-3b closes the window (ga-1d75jm).
//
// One duplicate is still open (ga-c9fwn7). File mode learns that a wait
// reminder was delivered only from its gc:nudge shadow, but beads → file moves
// only live records, so a reminder the beads queue delivered before the switch
// back, whose wait was not yet finalized, has no terminal shadow for file mode
// to find. Wait readiness queues it again: at once if the reminder was born in
// beads mode, or, if it was born in file mode, once the stale sweep closes the
// shadow it left open, with the wait stalled until then.

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
//
// nudgequeue.ClassifyTerminalState deliberately has no outcome for
// "migrated", so the code reads as not terminal: the nudge was moved, neither
// delivered nor failed. No wait helper is handed the code. The beads shadow
// view reads a record retired migrated as no record, so the helpers answer
// from the gc:nudge shadow the file queue saved for the moved nudge. A
// deliberate classifier outcome for the code is ga-0qe8xy.
const (
	nudgeQueueMigratedOutcome = storebinding.NudgeQueueMigratedOutcome
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
	// unclaimed, each with an open gc:nudge shadow its BeadID names, unless
	// the queue already holds their id, and returns the skipped ids it holds
	// as dead letters or under another creation time.
	importMigrated(items []nudgequeue.Item) (nudgeImportSkips, error)
}

// nudgeImportSkips lists the moved items importMigrated did not import
// because the file queue already holds their id, other than as the same
// live item.
type nudgeImportSkips struct {
	// Dead ids name items the queue holds as dead letters, same id and
	// creation time.
	Dead []string
	// Collided ids name items the queue holds under another creation time:
	// a different nudge sharing the id, which the queue keeps one item for.
	Collided []string
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
	// file queue changed a member between the shim's read and its removal.
	// If it claimed a member, nothing is removed and the batch stays whole in
	// the file queue too, so it may be delivered twice. If it replaced or
	// removed a member, the remaining members leave the file queue and the
	// change is reported. Either way nothing is lost, but the beads copy of a
	// member it withdrew or rolled back is still delivered (ga-1d75jm).
	Raced int
	// Retired batches stay in the file queue because the beads queue does
	// not serve every member: it already retired the id or dead-lettered it.
	// After a quick flip back the id is retired "migrated" and the batch is
	// stranded, undelivered in beads mode (file → beads, in the header).
	Retired int
	// RetiredIDs lists the ids that kept their batches in the file queue.
	RetiredIDs []string
	// AlreadyDead batches reached the file queue only in part: the file
	// queue holds a member as a dead letter, same id and creation time, so
	// that member was not imported, and its beads record is retired as such.
	AlreadyDead int
	// AlreadyDeadIDs lists the ids the file queue already held dead.
	AlreadyDeadIDs []string
	// Collided batches reached the file queue only in part: the file queue
	// holds a member's id under another creation time, a different nudge, so
	// that member was not imported and its beads record stays live.
	Collided int
	// CollidedIDs lists the ids the file queue held under another creation
	// time.
	CollidedIDs []string
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
// its beads records as migrated, and counts it: Collided if a member collided,
// else AlreadyDead if one was dead in the file queue, else Moved. A collided
// member's record stays live. An error leaves the batch's count to the caller.
func moveNudgeBatchToFile(report *nudgeQueueShimReport, batch nudgeMigrationBatch, file nudgeQueueShimFile, beadsQueue storebinding.NudgeQueue) error {
	skips, err := file.importMigrated(batch.Items)
	if err != nil {
		return fmt.Errorf("importing into the file queue: %w", err)
	}
	skipped := make(map[string]bool, len(skips.Dead)+len(skips.Collided))
	for _, ids := range [][]string{skips.Dead, skips.Collided} {
		for _, id := range ids {
			skipped[id] = true
		}
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
	if err := beadsQueue.Ack(skips.Dead, nudgeQueueMigratedOutcome, nudgeQueueMigratedDeadReason, nudgeQueueMigratedBoundary); err != nil {
		return fmt.Errorf("retiring the records the file queue holds dead: %w", err)
	}
	report.AlreadyDeadIDs = append(report.AlreadyDeadIDs, skips.Dead...)
	report.CollidedIDs = append(report.CollidedIDs, skips.Collided...)
	switch {
	case len(skips.Collided) > 0:
		report.Collided++
	case len(skips.Dead) > 0:
		report.AlreadyDead++
	default:
		report.Moved++
	}
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
// The destination pins it under a key of its own, and the file queue points
// its BeadID at the shadow it saves for it (importMigrated).
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
	// Not reached: groupNudgeMigrationBatches gives every unpinned item a
	// batch of its own.
	return "(unpinned)"
}
