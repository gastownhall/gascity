package storebinding

// The durable nudge queue as BEADS. Every other storage class is already a
// projection of one canonical Beads store; the queue was the last front door
// with no Beads-backed implementation, so NewBeadsAdapters had to be handed a
// queue built on some other substrate. This file closes that gap: one queued
// nudge is one bead, and the same code path runs over the SQLite engine, over
// a Postgres beads store, or over anything else that carries the same
// compare-and-swap primitives.
//
// TWO ORTHOGONAL AXES, ONE AUTHORITY EACH. A queued nudge has four observable
// states (pending, in-flight, dead, terminal), and mapping four states onto a
// single field is what makes queues lose work. So they are split:
//
//   - The lifecycle axis (live | dead | terminal) lives in metadata
//     "queue_state" and is moved only by a revision-fenced UpdateIfMatch.
//   - The delivery axis (pending | in-flight) lives in the bead's ASSIGNEE,
//     which holds the claim lease, and is moved only by Claim (acquire) and
//     ReleaseIfCurrent (release).
//
// A live bead is in-flight exactly while its assignee carries an unexpired
// lease. That is why the lease deadline is IN the assignee rather than in
// metadata: Claim writes owner and deadline in one atomic swap, so there is no
// window in which an item is claimed but not yet leased — a window in which a
// concurrent recovery pass would revoke a fresh claim. It also means an
// expired lease releases by arithmetic, not by a write that might never
// happen: the item reads as pending the instant its deadline passes, whether
// or not any maintenance pass has run.
//
// SINGLE WINNER. ClaimDue mints a fresh lease token per call, so two racing
// claimants never present the same assignee and Claim's idempotent self-claim
// cannot hand the same item to both. The CAS is the gate; the metadata read
// that follows only rejects an item that raced into the dead bucket.
//
// The queue's beads are its own family (label "gc:nudge-queue"), disjoint from
// the nudge SHADOW beads that internal/nudgequeue projects over the same store
// (label "gc:nudge"). Queue and shadow stay separate records here, unlike the
// merged SQL port; the shadow front door is unchanged.
//
// Policy (lease, backoff, attempt ceiling, dead retention) mirrors the values
// the deployed queue applies. They are duplicated rather than imported because
// the authority lives in cmd/gc, a main package nothing can import; the copies
// are pinned to it by a test that reads its source, which is the only place the
// two may meet.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

const (
	// nudgeQueueClaimLease is how long a claim holds an item before the item
	// reads as pending again.
	nudgeQueueClaimLease = 2 * time.Minute
	// nudgeQueueRetryDelay is the redelivery backoff after a failed attempt.
	nudgeQueueRetryDelay = 15 * time.Second
	// nudgeQueueMaxAttempts dead-letters an item after this many failures.
	nudgeQueueMaxAttempts = 5
	// nudgeQueueDeadRetention is how long a dead letter stays in the dead
	// bucket before it ages into the invisible terminal record.
	nudgeQueueDeadRetention = time.Hour
	// nudgeQueueTerminalRetention is how long a terminal record survives
	// before the sweep deletes it. It is the ttl the deployed queue's
	// retention sweeper runs with (cmd/gc's defaultQueuedNudgeTTL, also a
	// queued nudge's deliver-by deadline).
	nudgeQueueTerminalRetention = 24 * time.Hour
	// nudgeQueueCASAttempts bounds one contended state transition. A caller
	// that loses this many revision races is contending with a writer that is
	// not this queue, which is a fault rather than a retry.
	nudgeQueueCASAttempts = 8
)

// Queue bead identity.
const (
	nudgeQueueLabel    = "gc:nudge-queue"
	nudgeQueueBeadType = "chore"
	nudgeQueueIDPrefix = "gcnq-"
	// nudgeQueueIDSuffix keeps the minted bead id from ending in a digit.
	// A store that recovers its id sequence from the maximum numeric suffix
	// would otherwise read a hash that happens to end in digits as a
	// sequence floor and burn the whole id space.
	nudgeQueueIDSuffix = "-q"
)

// Lifecycle-axis values carried in metadata "queue_state". "pending" names the
// live lifecycle, not the delivery state: a live bead is in-flight while its
// assignee holds an unexpired lease and pending otherwise.
const (
	nudgeQueueStateLive     = "pending"
	nudgeQueueStateDead     = "dead"
	nudgeQueueStateTerminal = "terminal"
)

// nudgeQueueInFlight is the derived delivery state; it is never stored.
const nudgeQueueInFlight = "in_flight"

// Metadata keys. The names match the deployed nudges columns so an operator
// reading a queue bead sees the same field vocabulary.
const (
	nudgeMetaNudgeID        = "nudge_id"
	nudgeMetaBeadID         = "bead_id"
	nudgeMetaAgent          = "agent"
	nudgeMetaSessionID      = "session_id"
	nudgeMetaEpoch          = "continuation_epoch"
	nudgeMetaSource         = "source"
	nudgeMetaMessage        = "message"
	nudgeMetaRefKind        = "ref_kind"
	nudgeMetaRefID          = "ref_id"
	nudgeMetaCreatedAt      = "created_at"
	nudgeMetaDeliverAfter   = "deliver_after"
	nudgeMetaExpiresAt      = "expires_at"
	nudgeMetaAttempts       = "attempts"
	nudgeMetaLastAttemptAt  = "last_attempt_at"
	nudgeMetaLastError      = "last_error"
	nudgeMetaDeadAt         = "dead_at"
	nudgeMetaQueueState     = "queue_state"
	nudgeMetaTerminalState  = "terminal_state"
	nudgeMetaTerminalReason = "terminal_reason"
	nudgeMetaCommitBoundary = "commit_boundary"
	nudgeMetaTerminalAt     = "terminal_at"
)

// ErrBeadsNudgeQueueCorrupt reports a queue bead that cannot be projected back
// onto an Item. The queue owns every bead carrying its label, so a bead
// without a durable nudge id is corruption, not an empty answer.
var ErrBeadsNudgeQueueCorrupt = errors.New("beads nudge queue record is corrupt")

// beadsNudgeQueue is the Beads-backed [NudgeQueue].
type beadsNudgeQueue struct {
	store    beads.NudgesStore
	claimer  beadsAssignmentClaimer
	releaser beads.ConditionalAssignmentReleaser
	writer   beads.ConditionalWriter
}

// NewBeadsNudgeQueue returns the durable nudge queue backed by store.
//
// The store must carry the compare-and-swap primitives the queue's
// single-winner claim is built on: Claim, ReleaseIfCurrent, and the
// conditional writer. A store without them is rejected here rather than
// emulated with a read-then-write, which would silently hand one nudge to two
// deliverers.
func NewBeadsNudgeQueue(store beads.NudgesStore) (NudgeQueue, error) {
	if nilInterface(store.Store) {
		return nil, errors.New("opening beads nudge queue: store is required")
	}
	claimer, ok := store.Store.(beadsAssignmentClaimer)
	if !ok {
		return nil, fmt.Errorf("opening beads nudge queue: %w", unsupportedBeadsCapability("assignment claim"))
	}
	releaser, ok := store.Store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return nil, fmt.Errorf("opening beads nudge queue: %w", unsupportedBeadsCapability("conditional assignment release"))
	}
	writer, ok := beads.ConditionalWriterFor(store.Store)
	if !ok {
		return nil, fmt.Errorf("opening beads nudge queue: %w", unsupportedBeadsCapability("conditional update"))
	}
	return &beadsNudgeQueue{store: store, claimer: claimer, releaser: releaser, writer: writer}, nil
}

// Enqueue inserts item as pending, superseding the queued items that carry the
// same (agent, source, reference). A duplicate nudge id is a no-op: the bead id
// is derived from the nudge id, so the store's own duplicate-id contract makes
// the insert idempotent even against a concurrent submitter.
func (q *beadsNudgeQueue) Enqueue(item nudgequeue.Item) error {
	if item.ID == "" {
		return errors.New("enqueueing nudge: empty nudge id")
	}
	now := time.Now().UTC()
	if err := q.maintain(now); err != nil {
		return err
	}
	exists, err := q.exists(item.ID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if item.Reference != nil && item.Reference.ID != "" {
		if err := q.supersede(item, now); err != nil {
			return err
		}
	}
	return q.insert(item, nudgeQueueStateLive, nil)
}

// EnqueueDeferred inserts item as pending with no maintenance and no
// supersession — the deferred-submit shape.
func (q *beadsNudgeQueue) EnqueueDeferred(item nudgequeue.Item) error {
	if item.ID == "" {
		return errors.New("enqueueing deferred nudge: empty nudge id")
	}
	return q.insert(item, nudgeQueueStateLive, nil)
}

// ClaimDue claims every due pending item addressed to target, honoring the
// session-generation fence. Each claim is one compare-and-swap on the bead's
// assignee, so exactly one concurrent claimant wins each item; a claimant that
// loses simply moves to the next candidate.
func (q *beadsNudgeQueue) ClaimDue(target ClaimTarget, now time.Time) ([]nudgequeue.Item, error) {
	if len(target.QueueKeys) == 0 {
		return nil, nil
	}
	if err := q.maintain(now); err != nil {
		return nil, err
	}
	entries, err := q.liveEntries(now)
	if err != nil {
		return nil, err
	}
	candidates := make([]nudgeQueueEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.state != nudgeQueueStateLive {
			continue
		}
		if !nudgeQueueMatchesKeys(entry.item.Agent, target.QueueKeys) {
			continue
		}
		if entry.item.DeliverAfter.After(now) {
			continue
		}
		if !nudgeQueuePassesFence(entry.item, target) {
			continue
		}
		candidates = append(candidates, entry)
	}
	nudgeQueueSortPending(candidates)

	var claimed []nudgequeue.Item
	for _, candidate := range candidates {
		lease, err := newNudgeLease(now, nudgeQueueClaimLease)
		if err != nil {
			return claimed, err
		}
		held, ok, err := q.claimer.Claim(candidate.bead.ID, lease.assignee)
		if err != nil {
			return claimed, fmt.Errorf("claiming nudge %q: %w", candidate.item.ID, err)
		}
		if !ok {
			continue
		}
		// The claim CAS gates on the delivery axis only. An item that raced
		// into the dead bucket between selection and claim is still claimable
		// at the bead level, so the post-claim snapshot decides.
		if held.Metadata[nudgeMetaQueueState] != nudgeQueueStateLive {
			if _, err := q.releaser.ReleaseIfCurrent(held.ID, lease.assignee); err != nil {
				return claimed, fmt.Errorf("releasing retired nudge %q: %w", candidate.item.ID, err)
			}
			continue
		}
		item := candidate.item
		item.ClaimedAt = lease.claimedAt
		item.LeaseUntil = lease.until
		claimed = append(claimed, item)
	}
	return claimed, nil
}

// ListForAgent returns the queue buckets addressed exactly to agentName.
func (q *beadsNudgeQueue) ListForAgent(agentName string, now time.Time) (pending, inFlight, dead []nudgequeue.Item, err error) {
	return q.buckets(now, func(item nudgequeue.Item) bool { return item.Agent == agentName })
}

// ListFor returns the queue buckets matching any of target's queue keys.
func (q *beadsNudgeQueue) ListFor(target ClaimTarget, now time.Time) (pending, inFlight, dead []nudgequeue.Item, err error) {
	if len(target.QueueKeys) == 0 {
		return nil, nil, nil, nil
	}
	return q.buckets(now, func(item nudgequeue.Item) bool {
		return nudgeQueueMatchesKeys(item.Agent, target.QueueKeys)
	})
}

// Snapshot returns every queued item in the canonical bucket order. Terminal
// records are history and stay invisible, matching the queue the file backend
// served where an acked item simply left.
func (q *beadsNudgeQueue) Snapshot() (nudgequeue.State, error) {
	state, err := q.bucketState(time.Now().UTC(), func(nudgequeue.Item) bool { return true })
	if err != nil {
		return nudgequeue.State{}, err
	}
	return state, nil
}

// Ack terminalizes delivered ids with the outcome vocabulary the shadow bead
// carries. Unknown and already-terminal ids no-op.
func (q *beadsNudgeQueue) Ack(ids []string, outcome, reason, commitBoundary string) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC()
	if err := q.maintain(now); err != nil {
		return err
	}
	for _, id := range nudgeQueueUniqueIDs(ids) {
		if err := q.terminalize(id, outcome, reason, commitBoundary, now); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseClaims returns undelivered in-flight ids to pending by revoking their
// leases. The revocation is a compare-and-swap on the exact lease the caller
// observed, so a claim that has already moved on is never disturbed.
func (q *beadsNudgeQueue) ReleaseClaims(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC()
	if err := q.maintain(now); err != nil {
		return err
	}
	for _, id := range nudgeQueueUniqueIDs(ids) {
		entry, found, err := q.entry(id, now)
		if err != nil {
			return err
		}
		if !found || entry.state != nudgeQueueInFlight {
			continue
		}
		if _, err := q.releaser.ReleaseIfCurrent(entry.bead.ID, entry.bead.Assignee); err != nil {
			return fmt.Errorf("releasing nudge claim %q: %w", id, err)
		}
	}
	return nil
}

// RecordFailure applies the retry policy and returns ONLY the items that
// dead-lettered. A requeued item is not reported: the caller's dead-letter
// handling must not fire for work that is about to be redelivered.
//
// A permanent cause ([ErrNudgeSessionFenceMismatch]) dead-letters on the first
// failure; everything else retries until the attempt ceiling or the item's
// deliver-by deadline.
func (q *beadsNudgeQueue) RecordFailure(ids []string, cause error, now time.Time) ([]nudgequeue.Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if err := q.maintain(now); err != nil {
		return nil, err
	}
	permanent := errors.Is(cause, ErrNudgeSessionFenceMismatch)
	var deadLettered []nudgequeue.Item
	for _, id := range nudgeQueueUniqueIDs(ids) {
		var failed nudgequeue.Item
		var dead bool
		err := q.mutate(id, now, func(entry nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
			failed, dead = nudgequeue.Item{}, false
			if entry.state != nudgeQueueStateLive && entry.state != nudgeQueueInFlight {
				return beads.UpdateOpts{}, false, nil
			}
			item := entry.item
			item.Attempts++
			item.LastAttemptAt = now.UTC()
			if cause != nil {
				item.LastError = cause.Error()
			}
			item.ClaimedAt = time.Time{}
			item.LeaseUntil = time.Time{}
			expired := !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now)
			update := map[string]string{
				nudgeMetaAttempts:      strconv.Itoa(item.Attempts),
				nudgeMetaLastAttemptAt: formatNudgeTime(item.LastAttemptAt),
				nudgeMetaLastError:     item.LastError,
			}
			if permanent || item.Attempts >= nudgeQueueMaxAttempts || expired {
				item.DeadAt = now.UTC()
				update[nudgeMetaQueueState] = nudgeQueueStateDead
				update[nudgeMetaDeadAt] = formatNudgeTime(item.DeadAt)
				update[nudgeMetaTerminalState] = nudgeQueueTerminalStateFor(item.LastError)
				update[nudgeMetaTerminalReason] = item.LastError
				update[nudgeMetaTerminalAt] = formatNudgeTime(item.DeadAt)
				failed, dead = item, true
				return nudgeQueueUnclaim(update), true, nil
			}
			item.DeliverAfter = now.Add(nudgeQueueRetryDelay).UTC()
			update[nudgeMetaDeliverAfter] = formatNudgeTime(item.DeliverAfter)
			failed = item
			return nudgeQueueUnclaim(update), true, nil
		})
		if err != nil {
			return nil, err
		}
		if dead {
			deadLettered = append(deadLettered, failed)
		}
	}
	return deadLettered, nil
}

// Rollback dead-letters a queued item. An item that never landed is recorded
// as a terminal failure so it stays observable.
func (q *beadsNudgeQueue) Rollback(item nudgequeue.Item, reason string) error {
	if item.ID == "" {
		return nil
	}
	now := time.Now().UTC()
	entry, found, err := q.entry(item.ID, now)
	if err != nil {
		return err
	}
	if !found {
		record := item
		record.LastError = reason
		record.DeadAt = now
		return q.insert(record, nudgeQueueStateTerminal, map[string]string{
			nudgeMetaTerminalState:  "failed",
			nudgeMetaTerminalReason: reason,
			nudgeMetaTerminalAt:     formatNudgeTime(now),
		})
	}
	if entry.state == nudgeQueueStateDead || entry.state == nudgeQueueStateTerminal {
		return q.stampMissingTerminalFields(item.ID, reason, now)
	}
	return q.markDead(item.ID, reason, "failed", now)
}

// WithdrawQueuedWaitNudges retires still-queued wait nudges. The bead is the
// record, so the withdrawal is one transition.
func (q *beadsNudgeQueue) WithdrawQueuedWaitNudges(ids []string) error {
	unique := nudgeQueueUniqueIDs(ids)
	if len(unique) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for _, id := range unique {
		if err := q.withdraw(id, now); err != nil {
			return err
		}
	}
	return nil
}

// maintain applies the recovery transitions the deployed queue applies inside
// its transaction: expiry, lease recovery, dead-letter aging, and the repair
// of a terminal record whose bead close did not land. Each transition is
// individually fenced and idempotent, so running them as a sequence rather
// than as one transaction converges on the same state.
func (q *beadsNudgeQueue) maintain(now time.Time) error {
	entries, err := q.liveEntries(now)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.state {
		case nudgeQueueStateLive, nudgeQueueInFlight:
			expired := !entry.item.ExpiresAt.IsZero() && !entry.item.ExpiresAt.After(now)
			if expired {
				reason := entry.item.LastError
				if reason == "" {
					reason = "expired"
				}
				if err := q.markDead(entry.item.ID, reason, "expired", now); err != nil {
					return err
				}
				continue
			}
			if entry.state == nudgeQueueStateLive && entry.bead.Assignee != "" {
				if err := q.revokeLease(entry, now); err != nil {
					return err
				}
			}
		case nudgeQueueStateDead:
			if entry.item.DeadAt.IsZero() || !entry.item.DeadAt.Before(now.Add(-nudgeQueueDeadRetention)) {
				continue
			}
			if err := q.retireDeadLetter(entry, now); err != nil {
				return err
			}
		case nudgeQueueStateTerminal:
			// A terminal record whose bead is still open is a torn Ack: the
			// lifecycle write committed and the close did not.
			if err := q.closeRecord(entry.bead.ID); err != nil {
				return err
			}
		}
	}
	return q.sweepTerminalRecords(now)
}

// sweepTerminalRecords deletes terminal queue records past
// nudgeQueueTerminalRetention. Without it a terminal record is immortal:
// retirement closes the bead and nothing ever removes it, so a long-lived
// city accumulates one dead row per nudge it has ever delivered, forever.
//
// The deployed queue solves this with a periodic SweepRetention the caller
// tickers. This queue has no ticker and the closed NudgeQueue contract has no
// sweep verb, so the sweep rides the maintenance pass every queue operation
// already runs. That makes it self-bounding with no lifecycle plumbing: the
// scan it adds is over exactly the records it is there to remove.
//
// Deleting a queue bead destroys no history. The queue's beads are its own
// family (label "gc:nudge-queue"); the nudge SHADOW beads the audit trail is
// read from carry "gc:nudge" and are untouched.
//
// Each delete is revision-fenced, so a record another writer has just moved is
// left alone and collected on the next pass rather than removed out from under
// it.
func (q *beadsNudgeQueue) sweepTerminalRecords(now time.Time) error {
	cutoff := now.Add(-nudgeQueueTerminalRetention)
	records, err := q.store.List(beads.ListQuery{
		Label:    nudgeQueueLabel,
		Status:   "closed",
		Sort:     beads.SortCreatedAsc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return fmt.Errorf("listing retired nudge queue records: %w", err)
	}
	for _, record := range records {
		terminalAt, err := parseNudgeTime(record.Metadata[nudgeMetaTerminalAt])
		if err != nil {
			return fmt.Errorf("%w: bead %q field %s: %w", ErrBeadsNudgeQueueCorrupt, record.ID, nudgeMetaTerminalAt, err)
		}
		if terminalAt.IsZero() || !terminalAt.Before(cutoff) {
			continue
		}
		err = q.writer.DeleteIfMatch(record.ID, record.Revision)
		if err == nil {
			continue
		}
		var stale *beads.PreconditionFailedError
		if errors.Is(err, beads.ErrNotFound) || errors.As(err, &stale) {
			continue
		}
		return fmt.Errorf("deleting retired nudge record %q: %w", record.ID, err)
	}
	return nil
}

// revokeLease clears a lease that has run out. ReleaseIfCurrent is the precise
// verb, but it moves only an in-progress bead; the fenced update behind it
// clears an assignment that was somehow left on an open bead, which would
// otherwise make the item permanently unclaimable.
func (q *beadsNudgeQueue) revokeLease(entry nudgeQueueEntry, now time.Time) error {
	released, err := q.releaser.ReleaseIfCurrent(entry.bead.ID, entry.bead.Assignee)
	if err != nil {
		return fmt.Errorf("recovering expired nudge lease %q: %w", entry.item.ID, err)
	}
	if released {
		return nil
	}
	return q.mutate(entry.item.ID, now, func(current nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
		if current.bead.Assignee == "" || current.state != nudgeQueueStateLive {
			return beads.UpdateOpts{}, false, nil
		}
		return nudgeQueueUnclaim(nil), true, nil
	})
}

// retireDeadLetter ages a dead letter past its retention into the terminal
// record the queue no longer serves.
func (q *beadsNudgeQueue) retireDeadLetter(entry nudgeQueueEntry, now time.Time) error {
	err := q.mutate(entry.item.ID, now, func(current nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
		if current.state != nudgeQueueStateDead {
			return beads.UpdateOpts{}, false, nil
		}
		update := map[string]string{nudgeMetaQueueState: nudgeQueueStateTerminal}
		if current.bead.Metadata[nudgeMetaTerminalState] == "" {
			update[nudgeMetaTerminalState] = nudgeQueueTerminalStateFor(current.item.LastError)
		}
		if current.bead.Metadata[nudgeMetaTerminalReason] == "" {
			reason := current.item.LastError
			if reason == "" {
				reason = "failed"
			}
			update[nudgeMetaTerminalReason] = reason
		}
		if current.bead.Metadata[nudgeMetaTerminalAt] == "" {
			update[nudgeMetaTerminalAt] = formatNudgeTime(current.item.DeadAt)
		}
		return beads.UpdateOpts{Metadata: update}, true, nil
	})
	if err != nil {
		return err
	}
	return q.closeRecord(entry.bead.ID)
}

// terminalize retires a delivered nudge.
func (q *beadsNudgeQueue) terminalize(nudgeID, outcome, reason, commitBoundary string, now time.Time) error {
	moved := false
	err := q.mutate(nudgeID, now, func(entry nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
		moved = false
		if entry.state != nudgeQueueStateLive && entry.state != nudgeQueueInFlight {
			return beads.UpdateOpts{}, false, nil
		}
		moved = true
		return nudgeQueueUnclaim(map[string]string{
			nudgeMetaQueueState:     nudgeQueueStateTerminal,
			nudgeMetaTerminalState:  outcome,
			nudgeMetaTerminalReason: reason,
			nudgeMetaCommitBoundary: commitBoundary,
			nudgeMetaTerminalAt:     formatNudgeTime(now),
		}), true, nil
	})
	if err != nil || !moved {
		return err
	}
	return q.closeRecord(nudgeQueueBeadID(nudgeID))
}

// withdraw retires a still-queued wait nudge.
func (q *beadsNudgeQueue) withdraw(nudgeID string, now time.Time) error {
	moved := false
	err := q.mutate(nudgeID, now, func(entry nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
		moved = false
		if entry.state != nudgeQueueStateLive && entry.state != nudgeQueueInFlight {
			return beads.UpdateOpts{}, false, nil
		}
		moved = true
		return nudgeQueueUnclaim(map[string]string{
			nudgeMetaQueueState:     nudgeQueueStateTerminal,
			nudgeMetaTerminalState:  "failed",
			nudgeMetaTerminalReason: "wait-canceled",
			nudgeMetaCommitBoundary: "delivery-withdrawn",
			nudgeMetaTerminalAt:     formatNudgeTime(now),
		}), true, nil
	})
	if err != nil || !moved {
		return err
	}
	return q.closeRecord(nudgeQueueBeadID(nudgeID))
}

// markDead moves a queued item into the dead bucket and releases any claim on
// it, so a superseded or expired entry can never be handed out again.
func (q *beadsNudgeQueue) markDead(nudgeID, lastError, terminalState string, now time.Time) error {
	return q.mutate(nudgeID, now, func(entry nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
		if entry.state != nudgeQueueStateLive && entry.state != nudgeQueueInFlight {
			return beads.UpdateOpts{}, false, nil
		}
		return nudgeQueueUnclaim(map[string]string{
			nudgeMetaQueueState:     nudgeQueueStateDead,
			nudgeMetaDeadAt:         formatNudgeTime(now),
			nudgeMetaLastError:      lastError,
			nudgeMetaTerminalState:  terminalState,
			nudgeMetaTerminalReason: lastError,
			nudgeMetaTerminalAt:     formatNudgeTime(now),
		}), true, nil
	})
}

// stampMissingTerminalFields records a rollback against an item that has
// already left the queue without rewriting a terminal verdict it already has.
func (q *beadsNudgeQueue) stampMissingTerminalFields(nudgeID, reason string, now time.Time) error {
	return q.mutate(nudgeID, now, func(entry nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
		if entry.bead.Metadata[nudgeMetaTerminalState] != "" {
			return beads.UpdateOpts{}, false, nil
		}
		return beads.UpdateOpts{Metadata: map[string]string{
			nudgeMetaTerminalState:  "failed",
			nudgeMetaTerminalReason: reason,
			nudgeMetaTerminalAt:     formatNudgeTime(now),
		}}, true, nil
	})
}

// supersede retires the queued entries the incoming item replaces.
func (q *beadsNudgeQueue) supersede(item nudgequeue.Item, now time.Time) error {
	entries, err := q.liveEntries(now)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.state != nudgeQueueStateLive && entry.state != nudgeQueueInFlight {
			continue
		}
		if entry.item.Agent != item.Agent || entry.item.Source != item.Source {
			continue
		}
		if entry.item.Reference == nil || *entry.item.Reference != *item.Reference {
			continue
		}
		if err := q.markDead(entry.item.ID, "superseded", "superseded", now); err != nil {
			return err
		}
	}
	return nil
}

// insert writes one queue bead. The bead id is derived from the nudge id, so a
// duplicate submit loses the store's duplicate-id check rather than creating a
// second record for the same nudge.
func (q *beadsNudgeQueue) insert(item nudgequeue.Item, state string, extra map[string]string) error {
	record := nudgeQueueRecord(item, state, extra)
	beadID := record.ID
	created, err := q.store.Create(record)
	if err != nil {
		if _, getErr := q.store.Get(beadID); getErr == nil {
			return nil
		}
		return fmt.Errorf("enqueueing nudge %q: %w", item.ID, err)
	}
	if created.ID != beadID {
		return fmt.Errorf("enqueueing nudge %q: store minted bead id %q, want %q", item.ID, created.ID, beadID)
	}
	return nil
}

// nudgeQueueRecord builds one queue bead: the id derived from the nudge id, the
// queue's label pair, and the item encoded into the deployed column vocabulary.
// It is the queue's single record shape — one function rather than an insert
// that spells the bead out inline, so any future writer of a queue record has
// to go through the shape the queue itself reads back. It deliberately leaves
// CreatedAt zero: production wants the store's clock on a fresh submit.
func nudgeQueueRecord(item nudgequeue.Item, state string, extra map[string]string) beads.Bead {
	record := beads.Bead{
		ID:       nudgeQueueBeadID(item.ID),
		Title:    "nudge-queue:" + item.ID,
		Type:     nudgeQueueBeadType,
		Labels:   []string{nudgeQueueLabel, "agent:" + item.Agent},
		Metadata: encodeNudgeItem(item, state, extra),
	}
	if state == nudgeQueueStateTerminal {
		record.Status = "closed"
	}
	return record
}

// mutate applies one revision-fenced transition to a queue bead, retrying the
// read-decide-write cycle when a concurrent writer moves the revision.
func (q *beadsNudgeQueue) mutate(nudgeID string, now time.Time, apply func(nudgeQueueEntry) (beads.UpdateOpts, bool, error)) error {
	beadID := nudgeQueueBeadID(nudgeID)
	for attempt := 0; attempt < nudgeQueueCASAttempts; attempt++ {
		record, err := q.store.Get(beadID)
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading nudge %q: %w", nudgeID, err)
		}
		entry, err := decodeNudgeQueueBead(record, now)
		if err != nil {
			return err
		}
		opts, change, err := apply(entry)
		if err != nil {
			return err
		}
		if !change {
			return nil
		}
		err = q.writer.UpdateIfMatch(beadID, record.Revision, opts)
		if err == nil {
			return nil
		}
		var stale *beads.PreconditionFailedError
		if errors.As(err, &stale) {
			continue
		}
		return fmt.Errorf("updating nudge %q: %w", nudgeID, err)
	}
	return fmt.Errorf("updating nudge %q: %d revision conflicts", nudgeID, nudgeQueueCASAttempts)
}

// closeRecord closes a retired queue bead. The lifecycle metadata is the
// commit point, so an already-closed or vanished bead is a no-op.
func (q *beadsNudgeQueue) closeRecord(beadID string) error {
	for attempt := 0; attempt < nudgeQueueCASAttempts; attempt++ {
		record, err := q.store.Get(beadID)
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading nudge record %q: %w", beadID, err)
		}
		if record.Status == "closed" {
			return nil
		}
		err = q.writer.CloseIfMatch(beadID, record.Revision)
		if err == nil {
			return nil
		}
		var stale *beads.PreconditionFailedError
		if errors.As(err, &stale) {
			continue
		}
		return fmt.Errorf("closing nudge record %q: %w", beadID, err)
	}
	return fmt.Errorf("closing nudge record %q: %d revision conflicts", beadID, nudgeQueueCASAttempts)
}

// exists reports whether the queue holds any record for nudgeID, in any state.
func (q *beadsNudgeQueue) exists(nudgeID string) (bool, error) {
	_, err := q.store.Get(nudgeQueueBeadID(nudgeID))
	if errors.Is(err, beads.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading nudge %q: %w", nudgeID, err)
	}
	return true, nil
}

// entry reads one queue record and derives its state.
func (q *beadsNudgeQueue) entry(nudgeID string, now time.Time) (nudgeQueueEntry, bool, error) {
	record, err := q.store.Get(nudgeQueueBeadID(nudgeID))
	if errors.Is(err, beads.ErrNotFound) {
		return nudgeQueueEntry{}, false, nil
	}
	if err != nil {
		return nudgeQueueEntry{}, false, fmt.Errorf("reading nudge %q: %w", nudgeID, err)
	}
	entry, err := decodeNudgeQueueBead(record, now)
	if err != nil {
		return nudgeQueueEntry{}, false, err
	}
	return entry, true, nil
}

// liveEntries reads every record the queue still serves. Terminal records are
// closed beads and are excluded by the read itself.
func (q *beadsNudgeQueue) liveEntries(now time.Time) ([]nudgeQueueEntry, error) {
	records, err := q.store.List(beads.ListQuery{
		Label:    nudgeQueueLabel,
		Sort:     beads.SortCreatedAsc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("listing nudge queue: %w", err)
	}
	entries := make([]nudgeQueueEntry, 0, len(records))
	for _, record := range records {
		entry, err := decodeNudgeQueueBead(record, now)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// buckets returns the three visible buckets for the matching items.
func (q *beadsNudgeQueue) buckets(now time.Time, match func(nudgequeue.Item) bool) (pending, inFlight, dead []nudgequeue.Item, err error) {
	if err := q.maintain(now); err != nil {
		return nil, nil, nil, err
	}
	state, err := q.bucketState(now, match)
	if err != nil {
		return nil, nil, nil, err
	}
	return state.Pending, state.InFlight, state.Dead, nil
}

func (q *beadsNudgeQueue) bucketState(now time.Time, match func(nudgequeue.Item) bool) (nudgequeue.State, error) {
	entries, err := q.liveEntries(now)
	if err != nil {
		return nudgequeue.State{}, err
	}
	var state nudgequeue.State
	for _, entry := range entries {
		if !match(entry.item) {
			continue
		}
		switch entry.state {
		case nudgeQueueStateLive:
			state.Pending = append(state.Pending, entry.item)
		case nudgeQueueInFlight:
			state.InFlight = append(state.InFlight, entry.item)
		case nudgeQueueStateDead:
			state.Dead = append(state.Dead, entry.item)
		}
	}
	nudgequeue.SortState(&state)
	return state, nil
}

// nudgeQueueEntry is one decoded queue record and its derived state.
type nudgeQueueEntry struct {
	bead  beads.Bead
	item  nudgequeue.Item
	state string
}

// decodeNudgeQueueBead projects a queue bead back onto an Item and derives its
// state from the two authorities: the lifecycle metadata and the claim lease.
func decodeNudgeQueueBead(record beads.Bead, now time.Time) (nudgeQueueEntry, error) {
	id := record.Metadata[nudgeMetaNudgeID]
	if id == "" {
		return nudgeQueueEntry{}, fmt.Errorf("%w: bead %q carries no nudge id", ErrBeadsNudgeQueueCorrupt, record.ID)
	}
	item := nudgequeue.Item{
		ID:                id,
		BeadID:            record.Metadata[nudgeMetaBeadID],
		Agent:             record.Metadata[nudgeMetaAgent],
		SessionID:         record.Metadata[nudgeMetaSessionID],
		ContinuationEpoch: record.Metadata[nudgeMetaEpoch],
		Source:            record.Metadata[nudgeMetaSource],
		Message:           record.Metadata[nudgeMetaMessage],
		LastError:         record.Metadata[nudgeMetaLastError],
	}
	if kind, refID := record.Metadata[nudgeMetaRefKind], record.Metadata[nudgeMetaRefID]; kind != "" || refID != "" {
		item.Reference = &nudgequeue.Reference{Kind: kind, ID: refID}
	}
	times := []struct {
		key string
		out *time.Time
	}{
		{nudgeMetaCreatedAt, &item.CreatedAt},
		{nudgeMetaDeliverAfter, &item.DeliverAfter},
		{nudgeMetaExpiresAt, &item.ExpiresAt},
		{nudgeMetaLastAttemptAt, &item.LastAttemptAt},
		{nudgeMetaDeadAt, &item.DeadAt},
	}
	for _, field := range times {
		parsed, err := parseNudgeTime(record.Metadata[field.key])
		if err != nil {
			return nudgeQueueEntry{}, fmt.Errorf("%w: bead %q field %s: %w", ErrBeadsNudgeQueueCorrupt, record.ID, field.key, err)
		}
		*field.out = parsed
	}
	if raw := record.Metadata[nudgeMetaAttempts]; raw != "" {
		attempts, err := strconv.Atoi(raw)
		if err != nil {
			return nudgeQueueEntry{}, fmt.Errorf("%w: bead %q field %s: %w", ErrBeadsNudgeQueueCorrupt, record.ID, nudgeMetaAttempts, err)
		}
		item.Attempts = attempts
	}

	state := record.Metadata[nudgeMetaQueueState]
	if state == "" {
		state = nudgeQueueStateLive
	}
	if record.Status == "closed" {
		state = nudgeQueueStateTerminal
	}
	if state == nudgeQueueStateLive {
		if lease, ok := parseNudgeLease(record.Assignee); ok && lease.until.After(now) {
			state = nudgeQueueInFlight
			item.ClaimedAt = lease.claimedAt
			item.LeaseUntil = lease.until
		}
	}
	return nudgeQueueEntry{bead: record, item: item, state: state}, nil
}

// encodeNudgeItem is the write half of the codec. Empty fields are left out so
// a queue bead reads as the record it is.
func encodeNudgeItem(item nudgequeue.Item, state string, extra map[string]string) map[string]string {
	metadata := map[string]string{
		nudgeMetaNudgeID:    item.ID,
		nudgeMetaAgent:      item.Agent,
		nudgeMetaSource:     item.Source,
		nudgeMetaQueueState: state,
	}
	optional := map[string]string{
		nudgeMetaBeadID:        item.BeadID,
		nudgeMetaSessionID:     item.SessionID,
		nudgeMetaEpoch:         item.ContinuationEpoch,
		nudgeMetaMessage:       item.Message,
		nudgeMetaCreatedAt:     formatNudgeTime(item.CreatedAt),
		nudgeMetaDeliverAfter:  formatNudgeTime(item.DeliverAfter),
		nudgeMetaExpiresAt:     formatNudgeTime(item.ExpiresAt),
		nudgeMetaLastAttemptAt: formatNudgeTime(item.LastAttemptAt),
		nudgeMetaLastError:     item.LastError,
		nudgeMetaDeadAt:        formatNudgeTime(item.DeadAt),
	}
	if item.Attempts != 0 {
		optional[nudgeMetaAttempts] = strconv.Itoa(item.Attempts)
	}
	if item.Reference != nil {
		optional[nudgeMetaRefKind] = item.Reference.Kind
		optional[nudgeMetaRefID] = item.Reference.ID
	}
	for key, value := range optional {
		if value != "" {
			metadata[key] = value
		}
	}
	for key, value := range extra {
		if value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

// nudgeQueueUnclaim builds the update that clears a claim alongside update.
// Releasing the lease and moving the lifecycle in one fenced write is what
// keeps a retired item from staying claimable.
func nudgeQueueUnclaim(update map[string]string) beads.UpdateOpts {
	unassigned, open := "", "open"
	return beads.UpdateOpts{Assignee: &unassigned, Status: &open, Metadata: update}
}

// nudgeQueueBeadID derives the queue bead id for a nudge id. It is a hash so
// the id is a fixed, opaque shape whatever the nudge id contains.
func nudgeQueueBeadID(nudgeID string) string {
	sum := sha256.Sum256([]byte(nudgeID))
	return nudgeQueueIDPrefix + hex.EncodeToString(sum[:16]) + nudgeQueueIDSuffix
}

// nudgeQueueTerminalStateFor maps a dead letter's last error onto the terminal
// vocabulary.
func nudgeQueueTerminalStateFor(lastError string) string {
	switch strings.TrimSpace(lastError) {
	case "expired":
		return "expired"
	case "superseded":
		return "superseded"
	default:
		return "failed"
	}
}

// nudgeQueuePassesFence applies the claim fence: a session-pinned item is
// claimable only by that exact session, and an epoch-pinned unpinned-session
// item only by a target that carries a continuation epoch at all.
func nudgeQueuePassesFence(item nudgequeue.Item, target ClaimTarget) bool {
	if item.SessionID != "" {
		return target.SessionID != "" && item.SessionID == target.SessionID
	}
	return item.ContinuationEpoch == "" || target.ContinuationEpoch != ""
}

func nudgeQueueMatchesKeys(agent string, keys []string) bool {
	for _, key := range keys {
		if key == agent {
			return true
		}
	}
	return false
}

func nudgeQueueSortPending(entries []nudgeQueueEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i].item, entries[j].item
		if !left.DeliverAfter.Equal(right.DeliverAfter) {
			return left.DeliverAfter.Before(right.DeliverAfter)
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.ID < right.ID
	})
}

func nudgeQueueUniqueIDs(ids []string) []string {
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	return unique
}

// nudgeLease is one claim: an owner nonce plus the deadline it holds until,
// carried together in the bead's assignee so Claim writes both atomically.
type nudgeLease struct {
	// assignee is the opaque owner string this lease is held under: it is the
	// exact value Claim and ReleaseIfCurrent compare, so it is named for what
	// it identifies rather than for its encoding.
	assignee  string
	claimedAt time.Time
	until     time.Time
}

const nudgeLeasePrefix = "nudge-lease"

func newNudgeLease(now time.Time, ttl time.Duration) (nudgeLease, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nudgeLease{}, fmt.Errorf("minting nudge claim lease: %w", err)
	}
	claimedAt, until := now.UTC(), now.Add(ttl).UTC()
	return nudgeLease{
		assignee: strings.Join([]string{
			nudgeLeasePrefix,
			hex.EncodeToString(nonce[:]),
			formatNudgeTime(claimedAt),
			formatNudgeTime(until),
		}, "/"),
		claimedAt: claimedAt,
		until:     until,
	}, nil
}

func parseNudgeLease(assignee string) (nudgeLease, bool) {
	parts := strings.Split(assignee, "/")
	if len(parts) != 4 || parts[0] != nudgeLeasePrefix || parts[1] == "" {
		return nudgeLease{}, false
	}
	claimedAt, err := parseNudgeTime(parts[2])
	if err != nil || claimedAt.IsZero() {
		return nudgeLease{}, false
	}
	until, err := parseNudgeTime(parts[3])
	if err != nil || until.IsZero() {
		return nudgeLease{}, false
	}
	return nudgeLease{assignee: assignee, claimedAt: claimedAt, until: until}, true
}

// formatNudgeTime renders a timestamp so it parses back to the same instant.
func formatNudgeTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339Nano)
}

func parseNudgeTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

var _ NudgeQueue = (*beadsNudgeQueue)(nil)
