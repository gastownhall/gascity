package opjournal

// Re-lowering (OJ-17 ruling 2026-10-21): a namespace move (gc storage
// migrate) carries a lowered deliver-now unit into a store of another mint
// prefix with its ids unchanged, so its derived interrupt is pinned where no
// lowering here would pin it. RelowerNowUnits moves each such interrupt to
// the id this store's lowering gives it, one guarded batch per unit, and
// leaves the old row closed as a forward pointer. The lane pairs a unit by
// identity either way; the move makes the derived id name the interrupt
// again and closes the window in which a proposal keyed <M>:interrupt here
// mints a second one.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// RelowerReason says why RelowerNowUnits left a unit where it is.
type RelowerReason string

// Reasons a unit is left where it is; its lane runs it by identity.
const (
	// RelowerInFlight: the message or its interrupt is executing or
	// settling, or the interrupt may have run, so a copy could run its
	// effect twice.
	RelowerInFlight RelowerReason = "in_flight"
	// RelowerAmbiguous: the message has more than one open derived
	// interrupt, and the rows cannot tell which one its lowering made.
	RelowerAmbiguous RelowerReason = "ambiguous"
	// RelowerHomeTaken: another row already holds the id the interrupt
	// would move to.
	RelowerHomeTaken RelowerReason = "home_taken"
)

// ErrRelowerRaced reports a unit that changed between the step's read and
// its write. Nothing was written for that unit; running the step again
// re-reads it.
var ErrRelowerRaced = errors.New("opjournal: a deliver-now unit changed while it was being re-lowered")

// Relowered is one unit RelowerNowUnits moved: message Message's derived
// interrupt, which was From and is now To.
type Relowered struct{ Message, From, To string }

// RelowerLeft is one unit RelowerNowUnits left where it is: message Message,
// its open derived interrupts, the id a move would have given its interrupt
// (Home), and why.
type RelowerLeft struct {
	Message    string
	Interrupts []string
	Home       string
	Reason     RelowerReason
}

// RelowerReport is what one RelowerNowUnits run did.
type RelowerReport struct {
	// Prefix is the store's namespace prefix, which every moved interrupt
	// is now pinned in.
	Prefix string
	// Relowered are the units this run moved.
	Relowered []Relowered
	// Home counts the lowered messages whose one open derived interrupt is
	// already pinned here.
	Home int
	// Left are the units this run left where they are.
	Left []RelowerLeft
	// Quarantined are the open rows of the scanned conversations this build
	// cannot read; they are never written.
	Quarantined []QuarantinedRow
}

// relowerUnitStatuses are the statuses of a lowered deliver-now message:
// the only messages RelowerNowUnits classifies.
var relowerUnitStatuses = []Status{StatusAccepted, StatusExecuting, StatusSettling}

// hasLoweredNowPage is the page size of HasLoweredNowMessages' reads.
const hasLoweredNowPage = 64

// HasLoweredNowMessages reports whether store holds a lowered deliver-now
// message (a lane message whose when is now or interrupt, accepted,
// executing or settling): the only rows RelowerNowUnits can move or report.
// It reads keyed as the lane scan does (OpenLaneConversations), one list per
// status in pages of hasLoweredNowPage rows until it meets one, and needs no
// write capability or namespace, so a caller with nothing to re-lower (a
// migrate of a city with no lane ops, over a binding that cannot apply
// guarded batches) learns it without opening a lane store. It reads the
// row's keys, not the op, so a row that does not decode counts:
// RelowerNowUnits reports it.
func HasLoweredNowMessages(store beads.SessionStore) (bool, error) {
	if isNilStore(store.Store) {
		return false, errors.New("probing for lowered deliver-now messages: store is required")
	}
	for _, status := range relowerUnitStatuses {
		q := beads.ListQuery{
			Label:    LabelOp,
			Metadata: map[string]string{keyExecutor: string(ExecutorLane), keyStatus: string(status)},
			Sort:     beads.SortCreatedAsc,
			Limit:    hasLoweredNowPage,
			TierMode: beads.TierBoth,
		}
		for {
			rows, err := store.List(q)
			if err != nil {
				return false, fmt.Errorf("probing for lowered deliver-now messages (%s): %w", status, err)
			}
			for _, b := range rows {
				when := When(b.Metadata[keyWhen])
				if Kind(b.Metadata[keyKind]) == KindMessage && (when == WhenNow || when == WhenInterrupt) {
					return true, nil
				}
			}
			if len(rows) < hasLoweredNowPage {
				break
			}
			last := rows[len(rows)-1]
			q.SeekAfter = &beads.SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
		}
	}
	return false, nil
}

// relowerUnitReason is the batch reason of a re-lowering.
const relowerUnitReason = "opjournal: re-lower deliver-now"

// RelowerNowUnits re-pins, under s's prefix, the derived interrupt of every
// movable deliver-now unit: a lowered message (accepted) with exactly one
// open derived interrupt, neither in flight, pinned at another id than
// NowInterruptID(s.Prefix(), M), with that id free. Per unit one guarded
// batch creates the interrupt at that id as a copy of the row it read (every
// key and label as stored, the id, revision and migration stamp aside) and closes the row it
// read, under that revision, skipped{coalesced_into: <copy>} by by. The
// message is never written. An open derived interrupt never reached its
// runtime (an at most once op closes on any attempt that may have run), so
// the copy runs nothing twice. A unit in flight, with more than one open
// derived interrupt, or whose home id is held is left where it is and
// reported; a lowered message with no open derived interrupt is not a unit
// to move. It requires a lane store. Its reads are keyed: one list per open
// status (OpenLaneConversations with limit 0), one ListOpenTolerant per
// conversation, and Gets. Run it with the store's writers stopped; a unit
// that changed under it is ErrRelowerRaced, with nothing written for it.
// It returns the report so far, joined with every error it met. A re-run
// moves what a failed or interrupted run left and writes nothing else.
func (s *Store) RelowerNowUnits(ctx context.Context, by string) (RelowerReport, error) {
	rep := RelowerReport{Prefix: s.prefix}
	if s.batcher == nil {
		return rep, fmt.Errorf("re-lowering deliver-now units: %w: the op store was not opened with NewLaneStore", beads.ErrGuardedBatchUnsupported)
	}
	if err := validateText("re-lower by", by, maxPrincipalLen); err != nil {
		return rep, err
	}
	convs, _, err := s.OpenLaneConversations(0, LaneScanCursor{})
	if err != nil {
		return rep, fmt.Errorf("re-lowering deliver-now units: %w", err)
	}
	var errs []error
	for _, conv := range convs {
		if err := ctx.Err(); err != nil {
			return rep, errors.Join(append(errs, err)...)
		}
		ops, quarantined, err := s.ListOpenTolerant(conv)
		if err != nil {
			errs = append(errs, fmt.Errorf("re-lowering deliver-now units: %w", err))
			continue
		}
		rep.Quarantined = append(rep.Quarantined, quarantined...)
		for _, m := range ops {
			if err := s.relowerMessage(ctx, &rep, ops, m, by); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return rep, errors.Join(errs...)
}

// relowerMessage classifies op m of a conversation whose open ops are ops,
// and moves its unit when it is movable.
func (s *Store) relowerMessage(ctx context.Context, rep *RelowerReport, ops []Op, m Op, by string) error {
	if !isNowMessage(m) || m.Status == StatusSubmitted {
		return nil
	}
	var derived []Op
	for _, op := range ops {
		if isDerivedOf(op, m) {
			derived = append(derived, op)
		}
	}
	if len(derived) == 0 {
		return nil
	}
	home, err := NowInterruptID(s.prefix, m)
	if err != nil {
		return fmt.Errorf("re-lowering deliver-now message %s: %w", m.ID, err)
	}
	if len(derived) > 1 {
		rep.Left = append(rep.Left, RelowerLeft{Message: m.ID, Interrupts: opIDs(derived), Home: home, Reason: RelowerAmbiguous})
		return nil
	}
	i := derived[0]
	switch {
	case i.ID == home:
		rep.Home++
		return nil
	case m.Status != StatusAccepted || !relowerable(i):
		rep.Left = append(rep.Left, RelowerLeft{Message: m.ID, Interrupts: []string{i.ID}, Home: home, Reason: RelowerInFlight})
		return nil
	}
	moved, left, err := s.relowerOne(ctx, m, i.ID, home, by)
	switch {
	case err != nil:
		return err
	case left:
		rep.Left = append(rep.Left, RelowerLeft{Message: m.ID, Interrupts: []string{i.ID}, Home: home, Reason: RelowerHomeTaken})
	case moved:
		rep.Relowered = append(rep.Relowered, Relowered{Message: m.ID, From: i.ID, To: home})
	}
	return nil
}

// relowerable reports a derived interrupt the step may copy: open, not in
// flight, and with no attempt that may have run (R0).
func relowerable(i Op) bool {
	return (i.Status == StatusSubmitted || i.Status == StatusAccepted) && i.MaybeDelivered == 0
}

// relowerOne moves message m's derived interrupt iid to home in one guarded
// batch, from a fresh read of iid. moved says the batch landed; homeTaken
// says another row holds home, so nothing was written.
func (s *Store) relowerOne(ctx context.Context, m Op, iid, home, by string) (moved, homeTaken bool, err error) {
	if s.beforeRelowerRead != nil {
		s.beforeRelowerRead(m)
	}
	b, err := s.store.Get(iid)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return false, false, fmt.Errorf("%w: interrupt %s of deliver-now message %s is gone", ErrRelowerRaced, iid, m.ID)
		}
		return false, false, fmt.Errorf("re-lowering deliver-now message %s: reading interrupt %s: %w", m.ID, iid, err)
	}
	cur, err := DecodeOp(b)
	if err != nil {
		return false, false, fmt.Errorf("re-lowering deliver-now message %s: %w", m.ID, err)
	}
	if !isDerivedOf(cur, m) || !relowerable(cur) {
		return false, false, fmt.Errorf("%w: interrupt %s of deliver-now message %s is %s", ErrRelowerRaced, iid, m.ID, cur.Status)
	}
	switch _, err := s.store.Get(home); {
	case err == nil:
		return false, true, nil
	case !errors.Is(err, beads.ErrNotFound):
		return false, false, fmt.Errorf("re-lowering deliver-now message %s: reading %s: %w", m.ID, home, err)
	}
	c, err := relowerCopy(b, cur, home)
	if err != nil {
		return false, false, fmt.Errorf("re-lowering deliver-now message %s: %w", m.ID, err)
	}
	next := closedAt(cur, Outcome{Arm: ArmSkipped, Reason: ReasonCoalescedInto, Result: Result{CoalescedInto: home}}, by, utc(s.clk.Now()))
	if err := validateOutcome(next.Outcome, cur.Cancel); err != nil {
		return false, false, fmt.Errorf("re-lowering deliver-now message %s: %w", m.ID, err)
	}
	update, err := opUpdateItem(cur, next)
	if err != nil {
		return false, false, fmt.Errorf("re-lowering deliver-now message %s: %w", m.ID, err)
	}
	if s.beforeRelowerBatch != nil {
		s.beforeRelowerBatch(cur)
	}
	_, kind, applyErr := s.apply(ctx, relowerUnitReason, []beads.GuardedItem{{Create: &c}, update})
	switch kind {
	case batchOK:
		return true, false, nil
	case batchInvalid:
		return false, false, fmt.Errorf("re-lowering deliver-now message %s: %w", m.ID, applyErr)
	case batchRefused:
		return false, false, fmt.Errorf("%w: re-lowering deliver-now message %s: %w", ErrRelowerRaced, m.ID, applyErr)
	}
	return s.relowerSettle(m, b.Revision, iid, home, applyErr)
}

// relowerSettle decides a re-lowering batch whose outcome is unknown, from
// fresh reads of the interrupt (read at rev) and its home id: moved when the
// interrupt points at home and home exists; applyErr when neither changed;
// ErrRelowerRaced otherwise.
func (s *Store) relowerSettle(m Op, rev int64, iid, home string, applyErr error) (bool, bool, error) {
	unknown := fmt.Errorf("re-lowering deliver-now message %s: %w", m.ID, applyErr)
	b, err := s.store.Get(iid)
	if err != nil {
		return false, false, errors.Join(unknown, fmt.Errorf("reading interrupt %s after the batch: %w", iid, err))
	}
	_, homeErr := s.store.Get(home)
	homeFound := homeErr == nil
	if homeErr != nil && !errors.Is(homeErr, beads.ErrNotFound) {
		return false, false, errors.Join(unknown, fmt.Errorf("reading %s after the batch: %w", home, homeErr))
	}
	cur, err := DecodeOp(b)
	if err != nil {
		return false, false, errors.Join(unknown, err)
	}
	switch {
	case homeFound && isForwardPointer(cur) && cur.Outcome.Result.CoalescedInto == home:
		return true, false, nil
	case !homeFound && b.Revision == rev:
		return false, false, unknown
	default:
		return false, false, fmt.Errorf("%w: %w", ErrRelowerRaced, unknown)
	}
}

// relowerCopy is row b, which decoded as cur, re-pinned at id: every field,
// key and label as stored, with no revision (the store assigns one), no read
// projection and no migration stamp (beadmeta.InfraMigratedFromMetadataKey),
// since the migration did not write the copy. It refuses a copy that does
// not decode as cur at id.
func relowerCopy(b beads.Bead, cur Op, id string) (beads.Bead, error) {
	c := b
	c.ID = id
	c.Revision = 0
	c.IsBlocked = nil
	c.Labels = slices.Clone(b.Labels)
	c.Needs = slices.Clone(b.Needs)
	c.Dependencies = slices.Clone(b.Dependencies)
	c.Metadata = maps.Clone(b.Metadata)
	delete(c.Metadata, beadmeta.InfraMigratedFromMetadataKey)
	if b.Priority != nil {
		p := *b.Priority
		c.Priority = &p
	}
	if b.DeferUntil != nil {
		d := *b.DeferUntil
		c.DeferUntil = &d
	}
	got, err := DecodeOp(c)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("copying interrupt %s to %s: %w", cur.ID, id, err)
	}
	got.ID, got.Revision = cur.ID, cur.Revision
	if !reflect.DeepEqual(got, cur) {
		return beads.Bead{}, fmt.Errorf("copying interrupt %s to %s: the copy does not decode as the interrupt it copies", cur.ID, id)
	}
	return c, nil
}

// isForwardPointer reports a row a re-lowering closed: skipped{coalesced_into},
// its effect carried by the op it names. It is never a unit's outcome.
func isForwardPointer(op Op) bool {
	return op.Status == StatusClosed && op.Outcome.Arm == ArmSkipped && op.Outcome.Reason == ReasonCoalescedInto
}

// opIDs are the ids of ops, in order.
func opIDs(ops []Op) []string {
	ids := make([]string, len(ops))
	for i, op := range ops {
		ids[i] = op.ID
	}
	return ids
}
