package storebinding

// The nudge shadow view for a city whose queue is the beads queue (DES-NQ
// D11). In the file queue the gc:nudge shadow bead is what wait readiness and
// wait finalization read; the beads queue writes no shadow, because its record
// already is the observable record. This view answers the shadow contract from
// queue records first and falls back to the legacy gc:nudge shadows that a city
// accumulated before it switched, so a wait that straddles the switch still
// resolves. A record the queue-mode shim retired as migrated answers nothing:
// the nudge moved into the file queue, which saved it an open gc:nudge shadow,
// so the view reads that shadow instead.
//
// The view is read-only over queue records. Retiring a record outside the
// queue's own transitions would break the queue's single-winner lifecycle, so
// the shadow writers refuse (ErrNudgeShadowViewReadOnly), and Save only
// confirms a record the queue already holds.

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// ErrNudgeShadowViewReadOnly reports a shadow write against the beads nudge
// shadow view. A queue record is retired only by the queue's own verbs (Ack,
// RecordFailure, Rollback, WithdrawQueuedWaitNudges, Maintain).
var ErrNudgeShadowViewReadOnly = errors.New("beads nudge shadow view is read-only: queue records are retired through the queue")

// NudgeQueueMigratedOutcome is the terminal state the queue-mode shim retires
// a beads queue record with once it has moved the nudge into the file queue.
// The nudge lives on under the gc:nudge shadow the file queue saved for it, so
// the shadow view reads such a record as no record and never projects the
// state, which nudgequeue.ClassifyTerminalState does not classify (ga-0qe8xy).
const NudgeQueueMigratedOutcome = "migrated"

// nudgeShadowQueuedState is the shadow state of a nudge that is still queued.
const nudgeShadowQueuedState = "queued"

// NewBeadsNudgeFrontDoors returns the nudge front doors of a city whose queue
// is the beads queue over store: the queue itself and the shadow view that
// projects its records. It fails when store cannot host the queue (see
// [NewBeadsNudgeQueue]).
func NewBeadsNudgeFrontDoors(store beads.NudgesStore) (NudgeFrontDoors, error) {
	queue, err := NewBeadsNudgeQueue(store)
	if err != nil {
		return NudgeFrontDoors{}, err
	}
	return NudgeFrontDoors{
		Queue:   queue,
		Shadows: &beadsNudgeShadowView{store: store, legacy: nudgequeue.NewStore(store)},
	}, nil
}

// IsNudgeQueueRecord reports whether b is a beads nudge queue record.
func IsNudgeQueueRecord(b beads.Bead) bool {
	if b.Type != nudgeQueueBeadType {
		return false
	}
	for _, label := range b.Labels {
		if label == nudgeQueueLabel {
			return true
		}
	}
	return false
}

// beadsNudgeShadowView is the [NudgeShadows] projection of the beads queue.
type beadsNudgeShadowView struct {
	store  beads.NudgesStore
	legacy *nudgequeue.Store
}

var _ NudgeShadows = (*beadsNudgeShadowView)(nil)

// Find returns the queued nudge nudgeID. A queue record answers whenever one
// exists: a live record is projected open, and a dead or retired one is not
// open, so it is not found. Only an id with no queue record, or with one
// retired migrated (NudgeQueueMigratedOutcome), falls back to the legacy
// gc:nudge shadow, which Find returns only while it is open.
//
// A queue record that does not decode is an error wrapping
// ErrBeadsNudgeQueueCorrupt, never a legacy answer: the record exists, so the
// legacy shadow would answer for the wrong thing. A caller that looks up many
// nudges in one pass skips only the lookup that reports it (errors.Is), so
// one corrupt record does not stop the others (DES-NQ invariant 6).
func (v *beadsNudgeShadowView) Find(nudgeID string) (nudgequeue.NudgeShadow, bool, error) {
	shadow, found, err := v.recordShadow(nudgeID)
	if err != nil {
		return nudgequeue.NudgeShadow{}, false, err
	}
	if found {
		if !shadow.Open {
			return nudgequeue.NudgeShadow{}, false, nil
		}
		return shadow, true, nil
	}
	return v.legacy.Find(nudgeID)
}

// FindIncludingTerminal returns nudgeID's queue record projected in any state
// but retired migrated, or else its legacy gc:nudge shadow, terminal ones
// included. A queue record that does not decode is reported as Find reports
// it.
func (v *beadsNudgeShadowView) FindIncludingTerminal(nudgeID string) (nudgequeue.NudgeShadow, bool, error) {
	shadow, found, err := v.recordShadow(nudgeID)
	if err != nil || found {
		return shadow, found, err
	}
	return v.legacy.FindIncludingTerminal(nudgeID)
}

// ShadowHistorySince lists, oldest first, every live and dead queue record,
// every retired one whose terminal_at is at or after since, and the legacy
// shadows legacy ShadowHistorySince reports, answering for each nudge as Find
// does: a nudge with a queue record only from the record, and one with none,
// or with a record retired migrated, only from its legacy shadows. The queue
// records are read by lifecycle state, one indexed List each, never by the
// family label alone. A record that does not decode or project is skipped and
// reported as a partial result beside the rest, and its legacy shadow is not
// listed in its place.
func (v *beadsNudgeShadowView) ShadowHistorySince(since time.Time) ([]nudgequeue.NudgeShadow, error) {
	var records []beads.Bead
	for _, state := range []string{nudgeQueueStateLive, nudgeQueueStateDead, nudgeQueueStateTerminal} {
		query := nudgeQueueStateQuery(state)
		query.IncludeClosed = state == nudgeQueueStateTerminal
		found, err := v.store.List(query)
		if err != nil {
			return nil, fmt.Errorf("listing %s nudge queue history: %w", state, err)
		}
		records = append(records, found...)
	}
	now := time.Now().UTC()
	var history []nudgequeue.NudgeShadow
	var corrupt []error
	recorded := make(map[string]bool, len(records))
	for _, record := range records {
		entry, err := decodeNudgeQueueBead(record, now)
		if err != nil {
			if id := record.Metadata[nudgeMetaNudgeID]; id != "" {
				recorded[id] = true
			}
			corrupt = append(corrupt, err)
			continue
		}
		shadow, err := projectNudgeQueueRecord(entry, now)
		if err != nil {
			recorded[entry.item.ID] = true
			corrupt = append(corrupt, err)
			continue
		}
		if nudgeQueueRecordMigrated(entry) {
			continue
		}
		recorded[entry.item.ID] = true
		if _, served := nudgeQueueBucketOf(entry, now); !served && shadow.TerminalAt.Before(since) {
			continue
		}
		history = append(history, shadow)
	}
	legacy, err := v.legacy.ShadowHistorySince(since)
	if err != nil {
		return nil, fmt.Errorf("listing legacy nudge shadow history: %w", err)
	}
	for _, shadow := range legacy {
		if !recorded[shadow.ID] {
			history = append(history, shadow)
		}
	}
	sort.SliceStable(history, func(i, j int) bool { return history[i].CreatedAt.Before(history[j].CreatedAt) })
	return history, nudgeQueueQuarantined("ShadowHistorySince", corrupt)
}

// StaleShadowsBefore lists the stale legacy gc:nudge shadows. Queue records
// age out through the queue's Maintain, not through the shadow sweep.
func (v *beadsNudgeShadowView) StaleShadowsBefore(before time.Time, limit int, liveExcludeIDs map[string]bool) ([]nudgequeue.NudgeShadow, error) {
	return v.legacy.StaleShadowsBefore(before, limit, liveExcludeIDs)
}

// SweepStale closes one stale legacy gc:nudge shadow.
func (v *beadsNudgeShadowView) SweepStale(beadID, closeReason string, now time.Time) error {
	return v.legacy.SweepStale(beadID, closeReason, now)
}

// Save confirms that the queue holds a record for item, returning the record's
// bead id and created=false. The record is the shadow, so there is nothing to
// create: a nudge with no record has to be enqueued first. The confirmation
// reads no state, so it also confirms a retired record, even one retired
// migrated, which Find reads as absent and answers from the legacy gc:nudge
// shadow. Find is the state-aware read (ga-w0hgvz).
func (v *beadsNudgeShadowView) Save(item nudgequeue.Item) (string, bool, error) {
	if item.ID == "" {
		return "", false, errors.New("saving nudge shadow: empty nudge id")
	}
	beadID := nudgeQueueBeadID(item.ID)
	_, err := v.store.Get(beadID)
	if errors.Is(err, beads.ErrNotFound) {
		return "", false, fmt.Errorf("saving nudge shadow %q: the beads queue holds no record for it; enqueue first", item.ID)
	}
	if err != nil {
		return "", false, fmt.Errorf("saving nudge shadow %q: %w", item.ID, err)
	}
	return beadID, false, nil
}

// Terminalize refuses: the queue retires its own records.
func (v *beadsNudgeShadowView) Terminalize(item nudgequeue.Item, _, _, _ string, _ time.Time) error {
	return fmt.Errorf("terminalizing nudge shadow %q: %w", item.ID, ErrNudgeShadowViewReadOnly)
}

// RollbackEnqueue refuses: the queue rolls back its own records.
func (v *beadsNudgeShadowView) RollbackEnqueue(beadID string) error {
	return fmt.Errorf("rolling back nudge shadow %q: %w", beadID, ErrNudgeShadowViewReadOnly)
}

// recordShadow reads nudgeID's queue record and projects it, reporting false
// when the queue holds no record for it or holds one retired migrated.
func (v *beadsNudgeShadowView) recordShadow(nudgeID string) (nudgequeue.NudgeShadow, bool, error) {
	if nudgeID == "" {
		return nudgequeue.NudgeShadow{}, false, nil
	}
	record, err := v.store.Get(nudgeQueueBeadID(nudgeID))
	if errors.Is(err, beads.ErrNotFound) {
		return nudgequeue.NudgeShadow{}, false, nil
	}
	if err != nil {
		return nudgequeue.NudgeShadow{}, false, fmt.Errorf("reading nudge %q: %w", nudgeID, err)
	}
	now := time.Now().UTC()
	entry, err := decodeNudgeQueueBead(record, now)
	if err != nil {
		return nudgequeue.NudgeShadow{}, false, err
	}
	shadow, err := projectNudgeQueueRecord(entry, now)
	if err != nil {
		return nudgequeue.NudgeShadow{}, false, err
	}
	if nudgeQueueRecordMigrated(entry) {
		return nudgequeue.NudgeShadow{}, false, nil
	}
	return shadow, true, nil
}

// nudgeQueueRecordMigrated reports whether the queue-mode shim retired entry
// as migrated (NudgeQueueMigratedOutcome).
func nudgeQueueRecordMigrated(entry nudgeQueueEntry) bool {
	return entry.state == nudgeQueueStateTerminal && entry.bead.Metadata[nudgeMetaTerminalState] == NudgeQueueMigratedOutcome
}

// projectNudgeQueueRecord projects one queue record onto the shadow view, as
// of now:
//
//   - live (pending or in flight): open, state "queued", no terminal fields;
//   - dead: not open, state terminal_state, with the reason, commit boundary
//     and time as stamped. A live record past its deliver-by deadline reads
//     as dead by arithmetic, exactly as the queue's own reads project it, so
//     it is expired since its deadline before Maintain records that;
//   - retired: not open, state terminal_state, as stamped.
//
// A dead record is not open because the file queue closes an item's shadow
// when it dead-letters the item; wait readiness (open only) and finalization
// (any state) therefore get the same answers in both queue modes.
//
// A dead or retired record's time is its terminal_at, else its dead_at. Every
// queue transition that retires or dead-letters a record stamps terminal_at,
// so a record with neither is corrupt (ErrBeadsNudgeQueueCorrupt), like one
// whose stamp does not parse.
func projectNudgeQueueRecord(entry nudgeQueueEntry, now time.Time) (nudgequeue.NudgeShadow, error) {
	item := entry.item
	shadow := nudgequeue.NudgeShadow{
		ID:           item.ID,
		BeadID:       entry.bead.ID,
		Agent:        item.Agent,
		SessionID:    item.SessionID,
		Source:       item.Source,
		Message:      item.Message,
		DeliverAfter: item.DeliverAfter,
		ExpiresAt:    item.ExpiresAt,
		CreatedAt:    item.CreatedAt,
	}
	if shadow.CreatedAt.IsZero() {
		shadow.CreatedAt = entry.bead.CreatedAt
	}
	if item.Reference != nil {
		ref := *item.Reference
		shadow.Reference = &ref
	}
	bucket, served := nudgeQueueBucketOf(entry, now)
	switch {
	case served && bucket != nudgequeue.BucketDead:
		shadow.Open = true
		shadow.State = nudgeShadowQueuedState
		return shadow, nil
	case served && entry.state != nudgeQueueStateDead:
		expired := nudgeQueueExpiredView(item)
		shadow.State = "expired"
		shadow.TerminalReason = expired.LastError
		shadow.TerminalAt = expired.DeadAt
		return shadow, nil
	}
	meta := entry.bead.Metadata
	shadow.State = meta[nudgeMetaTerminalState]
	if shadow.State == "" {
		shadow.State = nudgeQueueTerminalStateFor(item.LastError)
	}
	shadow.TerminalReason = meta[nudgeMetaTerminalReason]
	shadow.CommitBoundary = meta[nudgeMetaCommitBoundary]
	terminalAt, err := parseNudgeTime(meta[nudgeMetaTerminalAt])
	if err != nil {
		return nudgequeue.NudgeShadow{}, fmt.Errorf("%w: bead %q field %s: %w", ErrBeadsNudgeQueueCorrupt, entry.bead.ID, nudgeMetaTerminalAt, err)
	}
	if terminalAt.IsZero() {
		terminalAt = item.DeadAt
	}
	if terminalAt.IsZero() {
		return nudgequeue.NudgeShadow{}, fmt.Errorf("%w: bead %q has neither %s nor %s", ErrBeadsNudgeQueueCorrupt, entry.bead.ID, nudgeMetaTerminalAt, nudgeMetaDeadAt)
	}
	shadow.TerminalAt = terminalAt
	return shadow, nil
}
