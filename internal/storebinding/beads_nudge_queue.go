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
// KEYED READS, OWNER-RUN MAINTENANCE. Every delivery verb reads only the
// records addressed to its queue keys: one List per key, on the queue family
// label and bead type, keyed by the record's "nudge_queue_key" metadata. A
// metadata equality is what the engines index (on SQLite,
// idx_metadata_key_value; the label, type and status are residual checks on
// the rows it finds), so the indexed key decides which rows a read visits. It
// must therefore be one only queue records write. The "agent:<key>" label and
// the "agent" metadata both fail that: session beads carry them, and so does
// every nudge shadow in the same store, which is closed but never deleted, so
// keying on them would make a target's read visit its whole nudge history. So
// one target's claim visits, and gets back, its own queue records however many
// other records the queue or the store holds. The whole-queue reads (Snapshot,
// DueKeys, Empty, Maintain and the terminal sweep) are indexed the same way,
// on the queue-only "queue_state" key, so even they visit queue rows alone.
// Every List is Live: the controller's nudges store is a caching store, and a
// cached List would hide another process's insert until a reconcile. The verbs
// repair only what they touch: an expired lease already reads as pending, a
// candidate claim revokes the stale lease it is about to replace, and an
// expired candidate is dead-lettered instead of claimed. The whole-queue pass —
// lease recovery, expiry, dead-letter aging and the terminal sweep — is
// Maintain, which the infrastructure owner runs on its own cadence.
//
// QUARANTINE. A record that does not decode is skipped by the keyed reads it
// appears in, so it can never fail a delivery to any target. The skip is still
// reported: the read returns every record it could decode together with a
// beads.PartialResultError naming the skipped records, so the caller delivers
// what it got and reports the rest. Direct access by its nudge id still
// reports ErrBeadsNudgeQueueCorrupt, and Maintain reports every corrupt record
// it saw.
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
	"context"
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
	"github.com/gastownhall/gascity/internal/config"
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
	// nudgeQueueSweepBatch bounds how many retired records one Maintain pass
	// deletes. The oldest go first; a pass that fills the batch says so, and
	// the owner's next pass takes the rest.
	nudgeQueueSweepBatch = 256
	// nudgeQueueCASAttempts bounds one contended state transition. A caller
	// that loses this many revision races is contending with a writer that is
	// not this queue, which is a fault rather than a retry.
	nudgeQueueCASAttempts = 8
)

// Queue bead identity.
const (
	nudgeQueueLabel    = "gc:nudge-queue"
	nudgeQueueBeadType = "chore"
	// nudgeQueueAgentLabelPrefix labels a record with the queue key it is
	// addressed to, in the vocabulary other bead families use for the same
	// identity. Reads key on the "nudge_queue_key" metadata instead: the label is not
	// specific to the queue.
	nudgeQueueAgentLabelPrefix = "agent:"
	// nudgeQueueTargetLabelPrefix keys a session-pinned record by the exact
	// session it is fenced to. Records written before it existed lack it, so
	// no read depends on it being present.
	nudgeQueueTargetLabelPrefix = "target:"
	// nudgeQueueIDPrefix is derived from config's reserved-prefix table rather
	// than spelled again here. The two have to agree — that table is what routes
	// an id minted below into the nudges binding — and a second literal would let
	// one of them change alone, which drops queue records into the work ledger
	// and stops the queue draining with no error anywhere.
	nudgeQueueIDPrefix = config.NudgeQueueIDPrefix + "-"
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
	// nudgeMetaQueueKey is the queue key a record is addressed to: the same
	// value as "agent", under a name only queue records carry. It is what
	// keyed reads look up, so the engine's metadata index hands a target's
	// read its queue records alone — "agent" is also written by every nudge
	// shadow in the same store, and shadows are closed, never deleted.
	nudgeMetaQueueKey = "nudge_queue_key"
	// nudgeMetaDeliveryBatch is the delivery key of the batch the record is
	// pinned to (Item.DeliveryBatch). Only queue records write it, so a
	// batch's membership read is indexed on it. It is written by the batch
	// claim's guarded write and cleared by a definitive rejection.
	nudgeMetaDeliveryBatch = "delivery_batch"
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
	// sweepBatch is nudgeQueueSweepBatch; tests lower it to reach the bound.
	sweepBatch int
}

// NewBeadsNudgeQueue returns the durable nudge queue backed by store.
//
// The delivery verbs do not maintain the queue. Whoever owns the queue must run
// [NudgeQueue.Maintain] on a schedule: without it expired leases are only
// revoked when their target claims again, dead letters never age out, and
// retired records are never deleted, so the queue grows without bound.
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
	return &beadsNudgeQueue{
		store:      store,
		claimer:    claimer,
		releaser:   releaser,
		writer:     writer,
		sweepBatch: nudgeQueueSweepBatch,
	}, nil
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
	entries, corrupt, err := q.keyedEntries(target.QueueKeys, now)
	if err != nil {
		return nil, err
	}
	candidates, err := q.claimCandidates(entries, newNudgeClaimFence(target), now)
	if err != nil {
		return nil, err
	}

	var claimed []nudgequeue.Item
	for _, candidate := range candidates {
		// A live candidate that still carries an assignee holds a lease that
		// has run out. Claim refuses a bead another owner holds, so the stale
		// lease is revoked first — the one targeted repair a claim needs.
		if candidate.bead.Assignee != "" {
			if err := q.revokeLease(candidate, now); err != nil {
				return claimed, err
			}
		}
		lease, err := newNudgeLease(now)
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
	return claimed, nudgeQueueQuarantined("ClaimDue", corrupt)
}

// claimCandidates returns, in canonical delivery order, the entries a claim may
// take: live, unclaimed, due, and passing fence. An expired entry it meets is
// touched, so it is repaired: dead-lettered here rather than left for the next
// maintenance pass.
func (q *beadsNudgeQueue) claimCandidates(entries []nudgeQueueEntry, fence *nudgeClaimFence, now time.Time) ([]nudgeQueueEntry, error) {
	candidates := make([]nudgeQueueEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.state != nudgeQueueStateLive {
			continue
		}
		if nudgeQueueItemExpired(entry.item, now) {
			if err := q.expire(entry.item.ID, now); err != nil {
				return nil, err
			}
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
	nudgeQueueSortPending(candidates)
	return candidates, nil
}

// ListForAgent returns the queue buckets addressed exactly to agentName.
func (q *beadsNudgeQueue) ListForAgent(agentName string, now time.Time) (pending, inFlight, dead []nudgequeue.Item, err error) {
	return q.buckets("ListForAgent", []string{agentName}, now)
}

// ListFor returns the queue buckets matching any of target's queue keys.
func (q *beadsNudgeQueue) ListFor(target ClaimTarget, now time.Time) (pending, inFlight, dead []nudgequeue.Item, err error) {
	if len(target.QueueKeys) == 0 {
		return nil, nil, nil, nil
	}
	return q.buckets("ListFor", target.QueueKeys, now)
}

// Snapshot returns every queued item in the canonical bucket order. Terminal
// records are history and stay invisible, matching the queue the file backend
// served where an acked item simply left.
//
// Snapshot is a whole-queue diagnostic, not a delivery read: it lists every
// pending and dead record and fails on an undecodable one rather than hiding
// it.
func (q *beadsNudgeQueue) Snapshot() (nudgequeue.State, error) {
	now := time.Now().UTC()
	entries, corrupt, err := q.wholeQueueEntries(now, nudgeQueueServedStates...)
	if err != nil {
		return nudgequeue.State{}, err
	}
	if len(corrupt) > 0 {
		return nudgequeue.State{}, corrupt[0]
	}
	return nudgeQueueBucketState(entries, now), nil
}

// Lookup reports the bucket each id is in, as of now, with one Get per distinct
// id. Retired and unknown ids are absent; an item past its deliver-by deadline
// reads as dead, as every other read projects it. An undecodable record is
// left out and reported as a partial result.
func (q *beadsNudgeQueue) Lookup(ids []string, now time.Time) (map[string]nudgequeue.Bucket, error) {
	buckets := make(map[string]nudgequeue.Bucket, len(ids))
	var corrupt []error
	for _, id := range nudgeQueueUniqueIDs(ids) {
		record, err := q.store.Get(nudgeQueueBeadID(id))
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading nudge %q: %w", id, err)
		}
		entry, err := decodeNudgeQueueBead(record, now)
		if err != nil {
			corrupt = append(corrupt, err)
			continue
		}
		if bucket, ok := nudgeQueueBucketOf(entry, now); ok {
			buckets[id] = bucket
		}
	}
	return buckets, nudgeQueueQuarantined("Lookup", corrupt)
}

// ListLive returns the queue's live items as of now, split into pending and
// in flight, for the queue-mode shim that moves live work out of the beads
// queue. It is one indexed read of the live records. Dead letters are not
// live, and neither is an item past its deliver-by deadline, which reads as
// dead. An undecodable record is skipped and reported as a partial result
// beside the items that did decode.
func (q *beadsNudgeQueue) ListLive(now time.Time) (pending, inFlight []nudgequeue.Item, err error) {
	entries, corrupt, err := q.wholeQueueEntries(now, nudgeQueueStateLive)
	if err != nil {
		return nil, nil, err
	}
	state := nudgeQueueBucketState(entries, now)
	return state.Pending, state.InFlight, nudgeQueueQuarantined("ListLive", corrupt)
}

// DueKeys returns, sorted and unique, the non-empty queue keys with an item
// that reads as pending and is due at now: a due pending item, or an in-flight
// item whose lease has run out. It is one indexed read of the live records,
// filtered in memory; undecodable records are reported as a partial result.
func (q *beadsNudgeQueue) DueKeys(now time.Time) ([]string, error) {
	entries, corrupt, err := q.wholeQueueEntries(now, nudgeQueueStateLive)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var keys []string
	for _, entry := range entries {
		key := entry.item.Agent
		if key == "" || seen[key] || entry.item.DeliverAfter.After(now) {
			continue
		}
		if bucket, ok := nudgeQueueBucketOf(entry, now); !ok || bucket != nudgequeue.BucketPending {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nudgeQueueQuarantined("DueKeys", corrupt)
}

// Empty reports that the queue serves no pending, in-flight or dead item. It is
// at most two indexed probes of one row each: live records, then dead ones. A
// queue that cannot be read is not empty.
//
// Each probe names no type and admits closed records, so the native store can
// push its one-row limit down instead of fetching every record in the state
// and cutting the answer in Go. That widening is exact: queue_state is written
// only by queue records, and the queue closes a record only after recording
// it terminal, so no closed record carries a served state. A record closed
// outside the queue with a served state still on it would read as not empty,
// which costs the caller a full read and never skips work.
func (q *beadsNudgeQueue) Empty() (bool, error) {
	for _, state := range nudgeQueueServedStates {
		query := nudgeQueueStateQuery(state)
		query.Type = ""
		query.IncludeClosed = true
		query.Limit = 1
		records, err := q.store.List(query)
		if err != nil {
			return false, fmt.Errorf("probing the nudge queue for %s records: %w", state, err)
		}
		if len(records) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// Ack terminalizes delivered ids with the outcome vocabulary the shadow bead
// carries. Unknown and already-terminal ids no-op.
func (q *beadsNudgeQueue) Ack(ids []string, outcome, reason, commitBoundary string) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for _, id := range nudgeQueueUniqueIDs(ids) {
		if err := q.terminalize(id, outcome, reason, commitBoundary, now); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseClaims returns undelivered in-flight ids to pending. With a zero
// notBefore it revokes their leases with a compare-and-swap on the exact lease
// it observed, so a claim that has already moved on is never disturbed. With a
// non-zero notBefore it is a revision-fenced read-modify-write (mutate) that
// clears the lease and sets deliver_after, so the item is not claimable before
// then; it re-reads and retries a refused write, and applies only while the
// item is still in flight under the lease first observed. Either way the delivery
// batch is kept. An item past its deliver-by deadline is dead-lettered as
// expired instead.
func (q *beadsNudgeQueue) ReleaseClaims(ids []string, notBefore time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for _, id := range nudgeQueueUniqueIDs(ids) {
		entry, found, err := q.entry(id, now)
		if err != nil {
			return err
		}
		if !found || entry.state != nudgeQueueInFlight {
			continue
		}
		if nudgeQueueItemExpired(entry.item, now) {
			if err := q.expire(id, now); err != nil {
				return err
			}
			continue
		}
		if notBefore.IsZero() {
			if _, err := q.releaser.ReleaseIfCurrent(entry.bead.ID, entry.bead.Assignee); err != nil {
				return fmt.Errorf("releasing nudge claim %q: %w", id, err)
			}
			continue
		}
		held := entry.bead.Assignee
		err = q.mutate(id, now, func(current nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
			if current.state != nudgeQueueInFlight || current.bead.Assignee != held {
				return beads.UpdateOpts{}, false, nil
			}
			return nudgeQueueUnclaim(map[string]string{nudgeMetaDeliverAfter: formatNudgeTime(notBefore)}), true, nil
		})
		if err != nil {
			return fmt.Errorf("releasing nudge claim %q until %s: %w", id, formatNudgeTime(notBefore), err)
		}
	}
	return nil
}

// RecordFailure applies the retry policy and returns ONLY the items that
// dead-lettered. A requeued item is not reported: the caller's dead-letter
// handling must not fire for work that is about to be redelivered.
//
// A permanent cause ([ErrNudgeSessionFenceMismatch]) dead-letters on the first
// failure; everything else retries until the attempt ceiling. A definitive
// rejection ([ErrNudgeDeliveryRejected]) delivered nothing, so it also clears
// the item's delivery batch: the next claim pins it under a new key. An item already
// past its deliver-by deadline expired rather than failed: it is
// dead-lettered as expired, charged no attempt, and not reported.
func (q *beadsNudgeQueue) RecordFailure(ids []string, cause error, now time.Time) ([]nudgequeue.Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	permanent := errors.Is(cause, ErrNudgeSessionFenceMismatch)
	rejected := errors.Is(cause, ErrNudgeDeliveryRejected)
	var deadLettered []nudgequeue.Item
	for _, id := range nudgeQueueUniqueIDs(ids) {
		var failed nudgequeue.Item
		var dead bool
		err := q.mutate(id, now, func(entry nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
			failed, dead = nudgequeue.Item{}, false
			if entry.state != nudgeQueueStateLive && entry.state != nudgeQueueInFlight {
				return beads.UpdateOpts{}, false, nil
			}
			if nudgeQueueItemExpired(entry.item, now) {
				return nudgeQueueExpiryUpdate(entry.item), true, nil
			}
			item := entry.item
			item.Attempts++
			item.LastAttemptAt = now.UTC()
			if cause != nil {
				item.LastError = cause.Error()
			}
			item.ClaimedAt = time.Time{}
			item.LeaseUntil = time.Time{}
			update := map[string]string{
				nudgeMetaAttempts:      strconv.Itoa(item.Attempts),
				nudgeMetaLastAttemptAt: formatNudgeTime(item.LastAttemptAt),
				nudgeMetaLastError:     item.LastError,
			}
			if rejected && item.DeliveryBatch != "" {
				item.DeliveryBatch = ""
				update[nudgeMetaDeliveryBatch] = ""
			}
			if permanent || item.Attempts >= nudgeQueueMaxAttempts {
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

// Maintain applies the recovery transitions the deployed queue applies inside
// its transaction — expiry, lease recovery, dead-letter aging, the repair of a
// terminal record whose bead close did not land — and then sweeps retired
// records past retention. Each transition is individually fenced and
// idempotent, so running them as a sequence rather than as one transaction
// converges on the same state.
//
// It is the only whole-queue pass. ctx bounds its transitions: it is checked
// before each one, and the pass stops when it ends. One pass deletes at most
// sweepBatch retired records, oldest first. A failed transition does not stop
// it; every failure is joined into the returned error. An undecodable record
// is not a failure either: it is named in the report's Corrupt list, each
// entry wrapping ErrBeadsNudgeQueueCorrupt.
//
// The reads are not bounded by either. beads.Store.List takes no ctx, so a
// List cannot be interrupted once issued. The pass reads the live records, the
// dead letters, and the terminal records whose close did not land, each one
// indexed read on queue_state. The sweep's read is sized by the retained
// terminal records, not the batch: see sweepTerminalRecords.
func (q *beadsNudgeQueue) Maintain(ctx context.Context, now time.Time) (NudgeMaintenance, error) {
	var report NudgeMaintenance
	if err := ctx.Err(); err != nil {
		return report, err
	}
	entries, corrupt, err := q.wholeQueueEntries(now, nudgeQueueStateLive, nudgeQueueStateDead, nudgeQueueStateTerminal)
	if err != nil {
		return report, err
	}
	report.Corrupt = corrupt
	var failures []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(append(failures, err)...)
		}
		if err := q.maintainEntry(entry, now); err != nil {
			failures = append(failures, err)
		}
	}
	remaining, sweepFailures, err := q.sweepTerminalRecords(ctx, now)
	report.Remaining = remaining
	failures = append(failures, sweepFailures...)
	if err != nil {
		failures = append(failures, err)
	}
	return report, errors.Join(failures...)
}

// maintainEntry applies the recovery transition one live record needs, if any.
func (q *beadsNudgeQueue) maintainEntry(entry nudgeQueueEntry, now time.Time) error {
	switch entry.state {
	case nudgeQueueStateLive, nudgeQueueInFlight:
		if nudgeQueueItemExpired(entry.item, now) {
			return q.expire(entry.item.ID, now)
		}
		if entry.state == nudgeQueueStateLive && entry.bead.Assignee != "" {
			return q.revokeLease(entry, now)
		}
	case nudgeQueueStateDead:
		if entry.item.DeadAt.IsZero() || !entry.item.DeadAt.Before(now.Add(-nudgeQueueDeadRetention)) {
			return nil
		}
		return q.retireDeadLetter(entry, now)
	case nudgeQueueStateTerminal:
		// A terminal record whose bead is still open is a torn Ack: the
		// lifecycle write committed and the close did not.
		return q.closeRecord(entry.bead.ID)
	}
	return nil
}

// sweepTerminalRecords deletes up to sweepBatch retired queue records whose
// last write — their retirement — is older than nudgeQueueTerminalRetention,
// oldest first. Without it a terminal record is immortal: retirement closes
// the bead and nothing ever removes it, so a long-lived city accumulates one
// dead row per nudge it has ever delivered, forever. It reports whether the
// batch filled, which means more may be due.
//
// Every predicate is pushed to the store — the family label, the terminal
// lifecycle (the indexed key), closed status, the retention cutoff and the
// batch limit — so every record the sweep lists is due, and the rows it gets
// back and the deletes it issues are bounded by the batch. The engine's work
// is not: on SQLite the queue_state=terminal lookup visits and sorts every
// retained terminal record before the limit applies, so it scales with the
// records retired within the retention window.
//
// Deleting a queue bead destroys no history. The queue's beads are its own
// family (label "gc:nudge-queue"); the nudge SHADOW beads the audit trail is
// read from carry "gc:nudge" and are untouched.
//
// Each delete is revision-fenced (deleteRetired), so a record another writer
// has just moved is left alone and collected on a later pass rather than
// removed out from under it. A delete that fails does not stop the sweep; the
// failures are returned.
func (q *beadsNudgeQueue) sweepTerminalRecords(ctx context.Context, now time.Time) (bool, []error, error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	cutoff := now.Add(-nudgeQueueTerminalRetention)
	records, err := q.store.List(beads.ListQuery{
		Label:         nudgeQueueLabel,
		Metadata:      map[string]string{nudgeMetaQueueState: nudgeQueueStateTerminal},
		Status:        "closed",
		UpdatedBefore: cutoff,
		Limit:         q.sweepBatch,
		Sort:          beads.SortCreatedAsc,
		TierMode:      beads.TierBoth,
		Live:          true,
	})
	if err != nil {
		return false, nil, fmt.Errorf("listing retired nudge queue records: %w", err)
	}
	var failures []error
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return true, failures, err
		}
		if err := q.deleteRetired(record, cutoff); err != nil {
			failures = append(failures, err)
		}
	}
	return len(records) >= q.sweepBatch, failures, nil
}

// deleteRetired deletes one retired record the sweep listed, fenced on its
// revision. A store whose List leaves revisions unstamped (BdStore, mc-3c8h5),
// or a cache that served such a row, hands over no usable token, so a refused
// delete is presented again at the revision of a fresh read — but only while
// that read still shows a closed, terminal record last written before cutoff.
// A record another writer moved is left for a later pass. Refusals that
// outlast nudgeQueueCASAttempts while the record still qualifies are a fault
// and are returned as an error.
func (q *beadsNudgeQueue) deleteRetired(record beads.Bead, cutoff time.Time) error {
	revision := record.Revision
	for attempt := 0; attempt < nudgeQueueCASAttempts; attempt++ {
		if revision == 0 || attempt > 0 {
			current, err := q.store.Get(record.ID)
			if errors.Is(err, beads.ErrNotFound) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("reading retired nudge record %q before deleting it: %w", record.ID, err)
			}
			if current.Status != "closed" || current.Metadata[nudgeMetaQueueState] != nudgeQueueStateTerminal || !current.UpdatedAt.Before(cutoff) {
				return nil
			}
			revision = current.Revision
		}
		err := q.writer.DeleteIfMatch(record.ID, revision)
		if err == nil || errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		var stale *beads.PreconditionFailedError
		if !errors.As(err, &stale) {
			return fmt.Errorf("deleting retired nudge record %q: %w", record.ID, err)
		}
	}
	return fmt.Errorf("deleting retired nudge record %q: %d revision conflicts", record.ID, nudgeQueueCASAttempts)
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

// terminalize retires a delivered nudge. An item past its deliver-by deadline
// is dead-lettered as expired instead, as the deployed queue's expiry pass
// does before it acks.
func (q *beadsNudgeQueue) terminalize(nudgeID, outcome, reason, commitBoundary string, now time.Time) error {
	moved := false
	err := q.mutate(nudgeID, now, func(entry nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
		moved = false
		if entry.state != nudgeQueueStateLive && entry.state != nudgeQueueInFlight {
			return beads.UpdateOpts{}, false, nil
		}
		if nudgeQueueItemExpired(entry.item, now) {
			return nudgeQueueExpiryUpdate(entry.item), true, nil
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

// expire dead-letters a live item past its deliver-by deadline, recording it
// exactly as the reads already project it.
func (q *beadsNudgeQueue) expire(nudgeID string, now time.Time) error {
	return q.mutate(nudgeID, now, func(entry nudgeQueueEntry) (beads.UpdateOpts, bool, error) {
		if entry.state != nudgeQueueStateLive && entry.state != nudgeQueueInFlight {
			return beads.UpdateOpts{}, false, nil
		}
		if !nudgeQueueItemExpired(entry.item, now) {
			return beads.UpdateOpts{}, false, nil
		}
		return nudgeQueueExpiryUpdate(entry.item), true, nil
	})
}

// nudgeQueueExpiryUpdate is the dead-letter write for an expired item: dead
// since its deadline, as [nudgeQueueExpiredView] reads it before the write, so
// recording the expiry never changes how the item reads.
func nudgeQueueExpiryUpdate(item nudgequeue.Item) beads.UpdateOpts {
	view := nudgeQueueExpiredView(item)
	deadAt := formatNudgeTime(view.DeadAt)
	return nudgeQueueUnclaim(map[string]string{
		nudgeMetaQueueState:     nudgeQueueStateDead,
		nudgeMetaDeadAt:         deadAt,
		nudgeMetaLastError:      view.LastError,
		nudgeMetaTerminalState:  "expired",
		nudgeMetaTerminalReason: view.LastError,
		nudgeMetaTerminalAt:     deadAt,
	})
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
//
// Supersession is scoped to one (agent, source, reference), so it reads only
// the incoming item's own queue key. A corrupt record under that key does not
// fail the submit: it cannot be superseded, and the delivery reads for the key
// and Maintain are what report it.
func (q *beadsNudgeQueue) supersede(item nudgequeue.Item, now time.Time) error {
	entries, _, err := q.keyedEntries([]string{item.Agent}, now)
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
		Labels:   nudgeQueueLabels(item),
		Metadata: encodeNudgeItem(item, state, extra),
	}
	if state == nudgeQueueStateTerminal {
		record.Status = "closed"
	}
	return record
}

// nudgeQueueLabels is a record's label set: the family label, the queue key it
// is addressed to, and — for a session-pinned item — the exact session.
func nudgeQueueLabels(item nudgequeue.Item) []string {
	labels := []string{nudgeQueueLabel, nudgeQueueAgentLabelPrefix + item.Agent}
	if item.SessionID != "" {
		labels = append(labels, nudgeQueueTargetLabelPrefix+item.SessionID)
	}
	return labels
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

// nudgeQueueServedStates are the lifecycle states whose records the queue
// still serves: live (pending or in flight) and dead.
var nudgeQueueServedStates = []string{nudgeQueueStateLive, nudgeQueueStateDead}

// wholeQueueEntries reads the open records in each lifecycle state, one
// indexed List per state, for the whole-queue reads and Maintain. Retired
// records are closed beads and are excluded by the read itself, so a terminal
// read finds only a record whose close did not land. Undecodable records are
// returned as errors beside the entries that did decode.
func (q *beadsNudgeQueue) wholeQueueEntries(now time.Time, states ...string) ([]nudgeQueueEntry, []error, error) {
	var entries []nudgeQueueEntry
	var corrupt []error
	for _, state := range states {
		records, err := q.store.List(nudgeQueueStateQuery(state))
		if err != nil {
			return nil, nil, fmt.Errorf("listing %s nudge queue records: %w", state, err)
		}
		decoded, skipped := decodeNudgeQueueRecords(records, now)
		entries = append(entries, decoded...)
		corrupt = append(corrupt, skipped...)
	}
	return entries, corrupt, nil
}

// nudgeQueueStateQuery is the whole-queue read of the open records in one
// lifecycle state. queue_state is written only by queue records, so the
// engine's metadata index visits queue rows alone; the family label and bead
// type are residual checks.
func nudgeQueueStateQuery(state string) beads.ListQuery {
	return beads.ListQuery{
		Label:    nudgeQueueLabel,
		Type:     nudgeQueueBeadType,
		Metadata: map[string]string{nudgeMetaQueueState: state},
		Sort:     beads.SortCreatedAsc,
		TierMode: beads.TierBoth,
		Live:     true,
	}
}

// keyedEntries reads the live records addressed to any of keys: one List per
// distinct key, never the city-wide family. An undecodable queue record is
// quarantined on its own instead of failing the read: it is returned as an
// error beside the entries that did decode.
func (q *beadsNudgeQueue) keyedEntries(keys []string, now time.Time) ([]nudgeQueueEntry, []error, error) {
	var entries []nudgeQueueEntry
	var corrupt []error
	for _, key := range nudgeQueueUniqueIDs(keys) {
		records, err := q.store.List(nudgeQueueKeyedQuery(key))
		if err != nil {
			return nil, nil, fmt.Errorf("listing nudge queue for %q: %w", key, err)
		}
		decoded, skipped := decodeNudgeQueueRecords(records, now)
		entries = append(entries, decoded...)
		corrupt = append(corrupt, skipped...)
	}
	return entries, corrupt, nil
}

// nudgeQueueQuarantined reports the records a delivery read skipped as a
// partial result, or nil when it skipped none.
func nudgeQueueQuarantined(verb string, corrupt []error) error {
	if len(corrupt) == 0 {
		return nil
	}
	return &beads.PartialResultError{Op: "nudge queue " + verb, Err: errors.Join(corrupt...)}
}

// nudgeQueueKeyedQuery is the one read a target-scoped verb issues per queue
// key. Every predicate is pushed to the store. The queue-only metadata key is
// the indexed predicate that drives the lookup, so the rows visited are the
// key's queue records; the family label and bead type are residual checks.
func nudgeQueueKeyedQuery(key string) beads.ListQuery {
	return beads.ListQuery{
		Label:    nudgeQueueLabel,
		Type:     nudgeQueueBeadType,
		Metadata: map[string]string{nudgeMetaQueueKey: key},
		Sort:     beads.SortCreatedAsc,
		TierMode: beads.TierBoth,
		Live:     true,
	}
}

// decodeNudgeQueueRecords decodes records, separating the ones that do not
// decode instead of failing on the first.
func decodeNudgeQueueRecords(records []beads.Bead, now time.Time) ([]nudgeQueueEntry, []error) {
	entries := make([]nudgeQueueEntry, 0, len(records))
	var corrupt []error
	for _, record := range records {
		entry, err := decodeNudgeQueueBead(record, now)
		if err != nil {
			corrupt = append(corrupt, err)
			continue
		}
		entries = append(entries, entry)
	}
	return entries, corrupt
}

// buckets returns the three visible buckets for the records addressed to keys.
// It is a pure read: nothing it sees is repaired.
func (q *beadsNudgeQueue) buckets(verb string, keys []string, now time.Time) (pending, inFlight, dead []nudgequeue.Item, err error) {
	entries, corrupt, err := q.keyedEntries(keys, now)
	if err != nil {
		return nil, nil, nil, err
	}
	state := nudgeQueueBucketState(entries, now)
	return state.Pending, state.InFlight, state.Dead, nudgeQueueQuarantined(verb, corrupt)
}

// nudgeQueueBucketState projects entries onto the visible buckets in the
// canonical order. A live item past its deliver-by deadline reads as dead by
// arithmetic, exactly as an expired lease reads as pending: the projection
// does not wait for Maintain to record what the clock already decided.
func nudgeQueueBucketState(entries []nudgeQueueEntry, now time.Time) nudgequeue.State {
	var state nudgequeue.State
	for _, entry := range entries {
		bucket, ok := nudgeQueueBucketOf(entry, now)
		if !ok {
			continue
		}
		switch bucket {
		case nudgequeue.BucketDead:
			if entry.state == nudgeQueueStateDead {
				state.Dead = append(state.Dead, entry.item)
			} else {
				state.Dead = append(state.Dead, nudgeQueueExpiredView(entry.item))
			}
		case nudgequeue.BucketPending:
			state.Pending = append(state.Pending, entry.item)
		case nudgequeue.BucketInFlight:
			state.InFlight = append(state.InFlight, entry.item)
		}
	}
	nudgequeue.SortState(&state)
	return state
}

// nudgeQueueBucketOf is the bucket entry reads as at now, or false when the
// queue no longer serves it. It is the one definition of the projection:
// nudgeQueueBucketState, Lookup and DueKeys all read buckets through it.
func nudgeQueueBucketOf(entry nudgeQueueEntry, now time.Time) (nudgequeue.Bucket, bool) {
	switch {
	case entry.state == nudgeQueueStateDead:
		return nudgequeue.BucketDead, true
	case (entry.state == nudgeQueueStateLive || entry.state == nudgeQueueInFlight) && nudgeQueueItemExpired(entry.item, now):
		return nudgequeue.BucketDead, true
	case entry.state == nudgeQueueStateLive:
		return nudgequeue.BucketPending, true
	case entry.state == nudgeQueueInFlight:
		return nudgequeue.BucketInFlight, true
	default:
		return "", false
	}
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
		DeliveryBatch:     record.Metadata[nudgeMetaDeliveryBatch],
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
		nudgeMetaQueueKey:   item.Agent,
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
		nudgeMetaDeliveryBatch: item.DeliveryBatch,
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

// nudgeClaimFence applies one claim's session-generation fence. It asks the
// target's census at most once, and only for an item pinned to a session other
// than the target's, so the ordinary claim never lists sessions.
type nudgeClaimFence struct {
	target    ClaimTarget
	asked     bool
	live      map[string]struct{}
	available bool
}

func newNudgeClaimFence(target ClaimTarget) *nudgeClaimFence {
	return &nudgeClaimFence{target: target}
}

// passes reports whether the target may claim item:
//   - a session-pinned item by its own session (Leg A);
//   - a session-pinned item by another session when the census is available,
//     names the target's session, and no longer names the item's: the item's
//     session was superseded and the target adopts its work (Leg B);
//   - an unpinned item with a continuation epoch only by a target that carries
//     an epoch at all, and any other unpinned item by anyone.
func (f *nudgeClaimFence) passes(item nudgequeue.Item) bool {
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

// nudgeQueueItemExpired reports whether item's deliver-by deadline has passed.
func nudgeQueueItemExpired(item nudgequeue.Item, now time.Time) bool {
	return !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now)
}

// nudgeQueueExpiryReason is the last error an expired item is dead-lettered
// with: the failure that preceded the expiry, if there was one.
func nudgeQueueExpiryReason(item nudgequeue.Item) string {
	if item.LastError != "" {
		return item.LastError
	}
	return "expired"
}

// nudgeQueueExpiredView is how an expired item reads, before and after its
// expiry is recorded: dead since its deadline, with no claim.
func nudgeQueueExpiredView(item nudgequeue.Item) nudgequeue.Item {
	item.LastError = nudgeQueueExpiryReason(item)
	item.DeadAt = item.ExpiresAt
	item.ClaimedAt = time.Time{}
	item.LeaseUntil = time.Time{}
	return item
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

// newNudgeLease mints a fresh claim lease held from now for
// nudgeQueueClaimLease.
func newNudgeLease(now time.Time) (nudgeLease, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nudgeLease{}, fmt.Errorf("minting nudge claim lease: %w", err)
	}
	claimedAt, until := now.UTC(), now.Add(nudgeQueueClaimLease).UTC()
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
