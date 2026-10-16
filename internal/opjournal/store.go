package opjournal

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// casAttempts bounds a revision-fenced read-decide-write loop.
const casAttempts = 8

// prefixHops bounds the walk through declared resolve targets when looking
// for the store's mint prefix.
const prefixHops = 8

// Store is the shadow op store over the sessions-class beads store that holds
// the conversation rows. It writes ops and moves their status under revision
// CAS; it decides and executes nothing.
type Store struct {
	store  beads.SessionStore
	writer beads.ConditionalWriter
	clk    clock.Clock
	prefix string
	// batcher applies the conversation lane's guarded batches; nil for a
	// store opened with NewStore rather than NewLaneStore.
	batcher beads.GuardedBatcher
}

// Option configures NewStore.
type Option func(*storeOptions)

type storeOptions struct {
	prefix string
}

// WithIDPrefix sets the namespace prefix of pinned op ids instead of
// discovering the store's mint prefix.
func WithIDPrefix(prefix string) Option {
	return func(o *storeOptions) { o.prefix = prefix }
}

// NewStore returns the op store over store. It refuses a store that cannot do
// revision-fenced writes — running the store's capability probe, so a bd
// without --if-revision is refused here rather than on the first Transition —
// and one whose mint prefix cannot be resolved.
func NewStore(store beads.SessionStore, clk clock.Clock, opts ...Option) (*Store, error) {
	if isNilStore(store.Store) {
		return nil, errors.New("opening op store: store is required")
	}
	if clk == nil {
		return nil, errors.New("opening op store: clock is required")
	}
	var o storeOptions
	for _, opt := range opts {
		opt(&o)
	}
	writer, ok := beads.ConditionalWriterForTarget(store)
	if !ok {
		return nil, fmt.Errorf("opening op store: %w", beads.ErrConditionalWriteUnsupported)
	}
	if capable, reason := beads.ProbeConditionalWrites(store); !capable {
		return nil, fmt.Errorf("opening op store: %w: %s", beads.ErrConditionalWriteUnsupported, reason)
	}
	prefix := o.prefix
	if prefix == "" {
		prefix = discoverPrefix(store)
	}
	if prefix == "" {
		return nil, fmt.Errorf("opening op store: %w", ErrNoNamespace)
	}
	if err := validatePrefix(prefix); err != nil {
		return nil, fmt.Errorf("opening op store: %w", err)
	}
	return &Store{store: store, writer: writer, clk: clk, prefix: prefix}, nil
}

func isNilStore(s beads.Store) bool {
	if s == nil {
		return true
	}
	v := reflect.ValueOf(s)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// discoverPrefix returns the first non-empty IDPrefix() along the store's
// declared resolve targets.
func discoverPrefix(store beads.Store) string {
	cur := store
	for range prefixHops {
		if p, ok := cur.(interface{ IDPrefix() string }); ok {
			if prefix := p.IDPrefix(); prefix != "" {
				return prefix
			}
		}
		t, ok := cur.(beads.ConditionalWritesResolveTargeter)
		if !ok {
			return ""
		}
		next := t.ConditionalWritesResolveTarget()
		if isNilStore(next) {
			return ""
		}
		cur = next
	}
	return ""
}

// Prefix returns the namespace prefix of the ids this store pins.
func (s *Store) Prefix() string { return s.prefix }

// Preview returns the op Propose would write for req, stamped now: the
// request normalized and defaulted, its pinned id and request digest, status
// submitted with its first transition. It does no I/O, so a producer can key
// in-process state by the id before writing. It refuses what Propose refuses.
func (s *Store) Preview(req Request) (Op, error) {
	spec, err := Lookup(req.Kind)
	if err != nil {
		return Op{}, err
	}
	op, err := normalizeRequest(req, spec, s.clk.Now())
	if err != nil {
		return Op{}, err
	}
	op.ID, err = DeriveID(s.prefix, op.Conversation, op.Producer.Principal, op.ClientKey)
	if err != nil {
		return Op{}, err
	}
	if op.After == op.ID {
		return Op{}, invalidf("op %s cannot come after itself", op.ID)
	}
	op.PayloadDigest = RequestDigest(op)
	op.Status = StatusSubmitted
	op.Transitions = []Transition{{Status: StatusSubmitted, At: op.CreatedAt, By: op.Producer.Principal}}
	return op, nil
}

// Propose writes the op req describes, create-if-absent at its pinned id. It
// reads the id first, so a retry never reaches Create, unless req.MintedKey
// says the key is new in this call. A new op starts submitted with its first
// transition recorded and returns created=true. Re-proposing the same request
// returns the stored op and created=false, whatever executor it names; the
// same key with another request is *KeyReuseError. Refused requests write
// nothing.
func (s *Store) Propose(req Request) (Op, bool, error) {
	op, err := s.Preview(req)
	if err != nil {
		return Op{}, false, err
	}
	b, err := EncodeOp(op)
	if err != nil {
		return Op{}, false, err
	}
	// Read first: a retry then never reaches Create, so a store that
	// overwrites a duplicate pinned id instead of refusing it cannot reset
	// the stored op. The create below still loses cleanly to a concurrent
	// proposer on every store that refuses duplicates. A key minted in this
	// call names no prior op, so the read is skipped.
	if !req.MintedKey {
		existing, getErr := s.store.Get(op.ID)
		switch {
		case getErr == nil:
			return s.resolveExisting(op, existing)
		case !errors.Is(getErr, beads.ErrNotFound):
			return Op{}, false, fmt.Errorf("reading op %s before creating it: %w", op.ID, getErr)
		}
	}
	created, createErr := s.store.Create(b)
	if createErr == nil {
		if created.ID != op.ID {
			strayErr := s.writer.DeleteIfMatch(created.ID, created.Revision)
			return Op{}, false, errors.Join(
				fmt.Errorf("%w: wanted %s, store minted %s", ErrPinnedIDNotHonored, op.ID, created.ID), strayErr)
		}
		op.Revision = created.Revision
		return op, true, nil
	}
	existing, getErr := s.store.Get(op.ID)
	switch {
	case getErr == nil:
		return s.resolveExisting(op, existing)
	case errors.Is(getErr, beads.ErrNotFound):
		if strings.Contains(createErr.Error(), "invalid issue type") {
			return Op{}, false, errors.Join(ErrBeadTypeUnregistered, createErr)
		}
		return Op{}, false, fmt.Errorf("creating op %s: %w", op.ID, createErr)
	default:
		return Op{}, false, errors.Join(fmt.Errorf("creating op %s: %w", op.ID, createErr),
			fmt.Errorf("reading op %s after a failed create: %w", op.ID, getErr))
	}
}

// resolveExisting answers a create that lost to an existing row.
func (s *Store) resolveExisting(want Op, b beads.Bead) (Op, bool, error) {
	got, err := DecodeOp(b)
	if err != nil {
		return Op{}, false, err
	}
	if got.Conversation != want.Conversation || got.Producer.Principal != want.Producer.Principal ||
		got.ClientKey != want.ClientKey {
		return Op{}, false, corruptf(want.ID, "stored row has another key (hash collision)")
	}
	if got.PayloadDigest != want.PayloadDigest {
		return Op{}, false, &KeyReuseError{Existing: got}
	}
	return got, false, nil
}

// Get reads one op. A missing op wraps beads.ErrNotFound.
func (s *Store) Get(id string) (Op, error) {
	b, err := s.store.Get(id)
	if err != nil {
		return Op{}, fmt.Errorf("reading op %s: %w", id, err)
	}
	return DecodeOp(b)
}

// QuarantinedRow is an open row ListOpenTolerant could not serve: one that
// does not decode (an unknown kind, schema or value from a newer build, or
// corruption) or that is keyed to another conversation. Err says why. A
// reader reports it and never writes it.
type QuarantinedRow struct {
	ID  string
	Err error
}

// ListOpen returns the conversation's open ops in CompareOrder order. It
// issues exactly one read, keyed by gc.op.open, which only open rows carry,
// and fails loudly on any row it cannot serve.
func (s *Store) ListOpen(conversation string) ([]Op, error) {
	ops, quarantined, err := s.ListOpenTolerant(conversation)
	if err != nil {
		return nil, err
	}
	if len(quarantined) > 0 {
		return nil, fmt.Errorf("listing ops of conversation %s: %w", conversation, quarantined[0].Err)
	}
	return ops, nil
}

// ListOpenTolerant is ListOpen for lanes: one undecodable row must not stall a
// conversation, so a row it cannot serve is returned in quarantined instead of
// failing the read, and the rest are served. It issues the same one read.
func (s *Store) ListOpenTolerant(conversation string) ([]Op, []QuarantinedRow, error) {
	if err := validateConversation(conversation); err != nil {
		return nil, nil, err
	}
	rows, err := s.store.List(beads.ListQuery{
		Metadata: map[string]string{keyOpen: conversation},
		Sort:     beads.SortCreatedAsc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listing ops of conversation %s: %w", conversation, err)
	}
	out := make([]Op, 0, len(rows))
	var quarantined []QuarantinedRow
	for _, b := range rows {
		op, err := DecodeOp(b)
		if err != nil {
			quarantined = append(quarantined, QuarantinedRow{ID: b.ID, Err: err})
			continue
		}
		if op.Conversation != conversation {
			quarantined = append(quarantined, QuarantinedRow{
				ID:  b.ID,
				Err: corruptf(b.ID, "open for conversation %s but belongs to %s", conversation, op.Conversation),
			})
			continue
		}
		if op.Status != StatusClosed {
			out = append(out, op)
		}
	}
	slices.SortFunc(out, CompareOrder)
	return out, quarantined, nil
}

// legacyScanPages bounds ListOpenLegacy's read: at most this many pages of
// limit rows.
const legacyScanPages = 8

// ListOpenLegacy returns up to limit open legacy ops (every executor but
// lane), oldest first, for the sweeper. It reads keyed on gc.op.status =
// submitted, because a legacy op never leaves submitted until it closes, in
// pages of limit rows (SeekAfter), dropping lane ops and setting aside rows
// that do not decode, until it has limit legacy ops, the matches run out or
// it has read legacyScanPages pages; so a head of unsweepable rows (a lane
// backlog, a newer build's rows) shorter than that does not starve the
// sweeper. Undecodable rows are returned by id rather than failing the sweep.
// The bool reports a truncated read: it stopped at that cap after a full
// page, so legacy ops behind the head may remain unread, and the next read
// starts again at the oldest row. A limit below 1 reads every match in one
// read.
func (s *Store) ListOpenLegacy(limit int) ([]Op, []string, bool, error) {
	if limit < 0 {
		limit = 0
	}
	q := beads.ListQuery{
		Label:    LabelOp,
		Metadata: map[string]string{keyStatus: string(StatusSubmitted)},
		Sort:     beads.SortCreatedAsc,
		Limit:    limit,
		TierMode: beads.TierBoth,
	}
	var out []Op
	var undecodable []string
	for page := 1; ; page++ {
		rows, err := s.store.List(q)
		if err != nil {
			return nil, nil, false, fmt.Errorf("listing open legacy ops: %w", err)
		}
		for _, b := range rows {
			if limit > 0 && len(out) == limit {
				break
			}
			op, err := DecodeOp(b)
			if err != nil {
				undecodable = append(undecodable, b.ID)
				continue
			}
			if op.Status == StatusClosed || !op.Executor.Legacy() {
				continue
			}
			out = append(out, op)
		}
		if limit == 0 || len(out) == limit || len(rows) < limit {
			return out, undecodable, false, nil
		}
		if page == legacyScanPages {
			return out, undecodable, true, nil
		}
		last := rows[len(rows)-1]
		q.SeekAfter = &beads.SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}

// Adopt hands an open legacy:api op to a new execution attempt: one revision
// CAS that sets attempt to expectAttempt+1 and finish_by to finishBy and
// appends an unchanged-status transition by by. It refuses a closed op
// (ErrAlreadyTerminal), any op whose executor is not legacy:api
// (ErrIllegalTransition), an attempt other than expectAttempt
// (ErrAttemptMoved: another adopter won), and a finishBy before the op's
// start_by (ErrInvalidRequest). Of concurrent adopters of one attempt exactly
// one wins.
func (s *Store) Adopt(id string, expectAttempt int, by string, finishBy time.Time) (Op, error) {
	if err := validateText("adopt by", by, maxPrincipalLen); err != nil {
		return Op{}, err
	}
	if finishBy.IsZero() {
		return Op{}, invalidf("adopting op %s: finish_by is required", id)
	}
	return s.mutate(id, by, func(cur Op, _ Spec) (Op, bool, error) {
		switch {
		case cur.Status == StatusClosed:
			return Op{}, false, fmt.Errorf("%w: op %s closed %s{%s}", ErrAlreadyTerminal, id, cur.Outcome.Arm, cur.Outcome.Reason)
		case cur.Executor != ExecutorLegacyAPI:
			return Op{}, false, fmt.Errorf("%w: op %s has executor %s; only a %s op is adopted", ErrIllegalTransition, id, cur.Executor, ExecutorLegacyAPI)
		case cur.Attempt != expectAttempt:
			return Op{}, false, fmt.Errorf("%w: op %s is at attempt %d, expected %d", ErrAttemptMoved, id, cur.Attempt, expectAttempt)
		case utc(finishBy).Before(cur.StartBy):
			return Op{}, false, invalidf("adopting op %s: finish_by %s is before start_by %s", id, formatTime(finishBy), formatTime(cur.StartBy))
		}
		next := cur
		next.Attempt = cur.Attempt + 1
		next.FinishBy = utc(finishBy)
		next.Transitions = appendTransition(cur.Transitions, Transition{Status: cur.Status, At: utc(s.clk.Now()), By: by})
		return next, true, nil
	})
}

// CloseKnown closes op, as read at op.Revision, with outcome in one
// UpdateIfMatch at that revision, saving Close's read on the common path. When
// the row has moved (*beads.PreconditionFailedError) it falls back to Close,
// which re-reads and keeps Close's rules: the same outcome is idempotent and
// another is ErrAlreadyTerminal. It does not re-check why the caller closes;
// a closer whose reason can lapse while the row moves uses CloseExpired.
func (s *Store) CloseKnown(op Op, outcome Outcome, by string) (Op, error) {
	if err := validateText("close by", by, maxPrincipalLen); err != nil {
		return Op{}, err
	}
	if err := validateOutcomeUpdateText(outcome); err != nil {
		return Op{}, err
	}
	if op.Status == StatusClosed {
		return s.Close(op.ID, outcome, by)
	}
	if err := validateOutcome(outcome, op.Cancel); err != nil {
		return Op{}, err
	}
	next, moved, err := s.writeKnown(op, closed(op, outcome, by, s.clk))
	if !moved {
		return next, err
	}
	return s.Close(op.ID, outcome, by)
}

// writeKnown writes next over op, as read at op.Revision, in one
// UpdateIfMatch at that revision. moved says the row has moved since: nothing
// was written and the caller re-reads.
func (s *Store) writeKnown(op, next Op) (Op, bool, error) {
	have, err := EncodeOp(op)
	if err != nil {
		return Op{}, false, err
	}
	opts, err := patch(have, op, next)
	if err != nil {
		return Op{}, false, err
	}
	err = s.writer.UpdateIfMatch(op.ID, op.Revision, opts)
	if err == nil {
		return next, false, nil
	}
	var stale *beads.PreconditionFailedError
	if errors.As(err, &stale) {
		return Op{}, true, nil
	}
	return Op{}, false, fmt.Errorf("closing op %s: %w", op.ID, err)
}

// CloseExpired closes op failed{deadline_in_flight} by by when the op is
// open, legacy and past its finish_by at now, deciding on the row it writes:
// one UpdateIfMatch at op.Revision when op, as read, is expired, else (or when
// the row has moved) a revision-fenced re-read and re-decide. closed says it
// closed the op. Otherwise it writes nothing and returns the op as it stands:
// an op an adopter moved (a later finish_by), a lane op, an op not yet expired
// and an op another closer reached first all stay as they are. The sweeper
// and the CLI's expiry rule close through it, so neither ever closes an op
// before its current finish_by (DES-O0API I7).
func (s *Store) CloseExpired(op Op, now time.Time, by string) (Op, bool, error) {
	if err := validateText("close by", by, maxPrincipalLen); err != nil {
		return Op{}, false, err
	}
	expired := Outcome{Arm: ArmFailed, Reason: ReasonDeadlineInFlight}
	isExpired := func(cur Op) bool {
		return cur.Status != StatusClosed && cur.Executor.Legacy() && now.After(cur.FinishBy)
	}
	if isExpired(op) {
		next, moved, err := s.writeKnown(op, closed(op, expired, by, s.clk))
		if !moved {
			return next, err == nil, err
		}
	}
	closedNow := false
	cur, err := s.mutate(op.ID, by, func(cur Op, _ Spec) (Op, bool, error) {
		closedNow = isExpired(cur)
		if !closedNow {
			return cur, false, nil
		}
		return closed(cur, expired, by, s.clk), true, nil
	})
	if err != nil {
		return Op{}, false, err
	}
	if cur.Status == StatusClosed {
		if err := s.closeBead(op.ID); err != nil {
			return Op{}, false, err
		}
	}
	return cur, closedNow, nil
}

// legalEdges is the status machine. Only Close and Cancel reach closed.
var legalEdges = map[Status][]Status{
	StatusSubmitted: {StatusAccepted, StatusExecuting},
	StatusAccepted:  {StatusExecuting, StatusSettling},
	StatusExecuting: {StatusSettling},
}

// Transition moves op id from expect to to. A status other than expect is
// *StatusError; an edge outside the machine, a lifecycle op entering
// executing, any move of a legacy op (it stays submitted until it closes) and
// any move to submitted or closed are ErrIllegalTransition.
func (s *Store) Transition(id string, expect, to Status, by string) (Op, error) {
	if err := validateText("transition by", by, maxPrincipalLen); err != nil {
		return Op{}, err
	}
	return s.mutate(id, by, func(cur Op, spec Spec) (Op, bool, error) {
		if cur.Status != expect {
			return Op{}, false, &StatusError{ID: id, Expected: expect, Current: cur.Status}
		}
		if cur.Executor.Legacy() {
			return Op{}, false, fmt.Errorf("%w: %s op %s stays submitted until it closes", ErrIllegalTransition, cur.Executor, id)
		}
		if !slices.Contains(legalEdges[cur.Status], to) {
			return Op{}, false, fmt.Errorf("%w: op %s %s -> %s", ErrIllegalTransition, id, cur.Status, to)
		}
		if spec.Lane == LaneLifecycle && to == StatusExecuting {
			return Op{}, false, fmt.Errorf("%w: lifecycle op %s never executes", ErrIllegalTransition, id)
		}
		next := cur
		next.Status = to
		next.Transitions = appendTransition(cur.Transitions, Transition{Status: to, At: utc(s.clk.Now()), By: by})
		return next, true, nil
	})
}

// Close records op id's single terminal outcome and closes its bead in one
// fenced write. Closing again with the same outcome is idempotent; another
// outcome is ErrAlreadyTerminal. Either way a torn close (metadata closed,
// bead open) is repaired, and a bead closed outside the journal is first
// stored as closed failed{closed_externally}.
func (s *Store) Close(id string, outcome Outcome, by string) (Op, error) {
	if err := validateText("close by", by, maxPrincipalLen); err != nil {
		return Op{}, err
	}
	if err := validateOutcomeUpdateText(outcome); err != nil {
		return Op{}, err
	}
	op, err := s.mutate(id, by, func(cur Op, _ Spec) (Op, bool, error) {
		if err := validateOutcome(outcome, cur.Cancel); err != nil {
			return Op{}, false, err
		}
		if cur.Status == StatusClosed {
			if !cur.Outcome.Equal(outcome) {
				return Op{}, false, fmt.Errorf("%w: op %s closed %s{%s}", ErrAlreadyTerminal, id, cur.Outcome.Arm, cur.Outcome.Reason)
			}
			return cur, false, nil
		}
		return closed(cur, outcome, by, s.clk), true, nil
	})
	if errors.Is(err, ErrAlreadyTerminal) {
		return Op{}, errors.Join(err, s.closeBead(id))
	}
	if err != nil {
		return Op{}, err
	}
	return op, s.closeBead(id)
}

// validateOutcomeUpdateText refuses outcome text an update would not store
// as a string.
func validateOutcomeUpdateText(o Outcome) error {
	return errors.Join(
		validateUpdateText("outcome reason", o.Reason),
		validateUpdateText("outcome degradation", o.Degradation),
		validateUpdateText("result turn", o.Result.Turn),
		validateUpdateText("result via", o.Result.Via),
	)
}

// closed returns cur moved to closed with outcome and no lease.
func closed(cur Op, outcome Outcome, by string, clk clock.Clock) Op {
	return closedAt(cur, outcome, by, utc(clk.Now()))
}

// closedAt returns cur moved to closed at now with outcome, no lease and no
// retry gate.
func closedAt(cur Op, outcome Outcome, by string, now time.Time) Op {
	next := cur
	next.Status = StatusClosed
	next.Outcome = outcome
	next.Lease = Lease{}
	next.RetryAt = time.Time{}
	next.Transitions = appendTransition(cur.Transitions, Transition{Status: StatusClosed, At: now, By: by})
	return next
}

// CancelResult says what Cancel did.
type CancelResult string

// Cancel results.
const (
	// CancelClosed means the op had not started and is now closed canceled.
	CancelClosed CancelResult = "closed"
	// CancelRequested means the op is executing (or, for lifecycle ops,
	// lowered); the cancel is recorded and the lane's outcome says who won.
	CancelRequested CancelResult = "requested"
	// CancelAlreadyDecided means the op is settling; nothing was written.
	CancelAlreadyDecided CancelResult = "already_decided"
	// CancelAlreadyTerminal means the op is closed; the cancel is not
	// recorded (a torn close or a close made outside the journal is still
	// repaired or stored).
	CancelAlreadyTerminal CancelResult = "already_terminal"
)

// Cancel cancels op id. Before execution (for lifecycle ops, before lowering)
// the op closes canceled. While executing, or once a lifecycle op is lowered,
// the request is recorded — the first recorded cancel wins — and the lane
// decides the outcome. A settling op is AlreadyDecided and a closed op
// AlreadyTerminal; neither is written, beyond repairing a torn close or
// storing a close made outside the journal, and the caller audits the typed
// result. An open legacy op is not cancelable (ErrIllegalTransition): only its
// legacy path, an adopter or the sweeper closes one.
func (s *Store) Cancel(id, by, reason string) (CancelResult, Op, error) {
	if err := validateText("cancel by", by, maxPrincipalLen); err != nil {
		return "", Op{}, err
	}
	if err := validateOptionalText("cancel reason", reason); err != nil {
		return "", Op{}, err
	}
	if err := errors.Join(validateUpdateText("cancel by", by), validateUpdateText("cancel reason", reason)); err != nil {
		return "", Op{}, err
	}
	var result CancelResult
	op, err := s.mutate(id, by, func(cur Op, spec Spec) (Op, bool, error) {
		cancel := cur.Cancel
		if !cancel.Recorded() {
			cancel = Cancel{By: by, At: utc(s.clk.Now()), Reason: reason}
		}
		switch {
		case cur.Status == StatusClosed:
			result = CancelAlreadyTerminal
			return cur, false, nil
		case cur.Executor.Legacy():
			return Op{}, false, fmt.Errorf("%w: %s op %s is closed only by its legacy path, an adopter or the sweeper", ErrIllegalTransition, cur.Executor, id)
		case cur.Status == StatusSettling:
			result = CancelAlreadyDecided
			return cur, false, nil
		case cur.Status == StatusSubmitted,
			cur.Status == StatusAccepted && spec.Lane == LaneConversation:
			result = CancelClosed
			next := cur
			next.Cancel = cancel
			return closed(next, Outcome{Arm: ArmCanceled}, by, s.clk), true, nil
		default:
			result = CancelRequested
			if cur.Cancel.Recorded() {
				return cur, false, nil
			}
			next := cur
			next.Cancel = cancel
			return next, true, nil
		}
	})
	if err != nil {
		return "", Op{}, err
	}
	if result == CancelClosed || result == CancelAlreadyTerminal {
		if err := s.closeBead(id); err != nil {
			return "", Op{}, err
		}
	}
	return result, op, nil
}

// mutate runs one revision-fenced read-decide-write on op id, retrying when a
// concurrent writer moves the revision. decide returns the next op and
// whether to write it; the write carries only the fields that changed. A row
// whose bead was closed outside the journal is first stored as DecodeOp reads
// it, closed failed{closed_externally} with no lease and a closed transition
// by by, and decide then sees the stored row, so reopening the bead cannot
// bring the op back. A revision of 0 is a real revision on some backends
// (SQLite starts there), so it is passed through; a store that cannot fence
// answers ErrConditionalWriteUnsupported itself and that error is returned as
// is.
func (s *Store) mutate(id, by string, decide func(cur Op, spec Spec) (Op, bool, error)) (Op, error) {
	var lastConflict error
	for range casAttempts {
		b, err := s.store.Get(id)
		if err != nil {
			return Op{}, fmt.Errorf("reading op %s: %w", id, err)
		}
		cur, err := DecodeOp(b)
		if err != nil {
			return Op{}, err
		}
		var next Op
		external := closedExternally(b)
		if external {
			next = closed(cur, cur.Outcome, by, s.clk)
		} else {
			spec, err := Lookup(cur.Kind)
			if err != nil {
				return Op{}, err
			}
			var write bool
			next, write, err = decide(cur, spec)
			if err != nil {
				return Op{}, err
			}
			if !write {
				return next, nil
			}
		}
		opts, err := patch(b, cur, next)
		if err != nil {
			return Op{}, err
		}
		err = s.writer.UpdateIfMatch(id, b.Revision, opts)
		if err == nil && external {
			continue
		}
		if err == nil {
			return next, nil
		}
		var stale *beads.PreconditionFailedError
		if !errors.As(err, &stale) {
			return Op{}, fmt.Errorf("updating op %s: %w", id, err)
		}
		lastConflict = err
	}
	return Op{}, fmt.Errorf("%w: updating op %s lost %d revision races: %w", ErrContention, id, casAttempts, lastConflict)
}

// patch is the update that turns row b, which decoded as cur, into next's
// encoding: the changed metadata keys, the assignee and, when next is closed,
// the bead status, so an outcome and its bead close commit together. A key is
// written empty only when cur's encoding has it and next's does not, so a
// gc.op.* key a newer build added — which decoding ignores — is never this
// build's to remove. gc.op.open is this build's key on every row: it is
// blanked whenever next does not carry it, even where cur's encoding lacks it
// (a bead closed outside the journal still carries it).
func patch(b beads.Bead, cur, next Op) (beads.UpdateOpts, error) {
	have, err := EncodeOp(cur)
	if err != nil {
		return beads.UpdateOpts{}, err
	}
	want, err := EncodeOp(next)
	if err != nil {
		return beads.UpdateOpts{}, err
	}
	meta := map[string]string{}
	for k, v := range want.Metadata {
		if b.Metadata[k] != v {
			meta[k] = v
		}
	}
	for k := range have.Metadata {
		if _, keep := want.Metadata[k]; !keep {
			meta[k] = ""
		}
	}
	if _, keep := want.Metadata[keyOpen]; !keep && b.Metadata[keyOpen] != "" {
		meta[keyOpen] = ""
	}
	opts := beads.UpdateOpts{Metadata: meta}
	if want.Status == beadStatusClosed && b.Status != beadStatusClosed {
		status := beadStatusClosed
		opts.Status = &status
	}
	if want.Assignee != b.Assignee {
		assignee := want.Assignee
		opts.Assignee = &assignee
	}
	return opts, nil
}

// closeBead repairs a torn close: it closes op id's bead when the metadata is
// already closed and the bead is not. An already-closed bead is done.
func (s *Store) closeBead(id string) error { return s.closeRow("op", id) }

// closeRow closes row id, a row of the named kind, unless it is closed
// already. Its errors name the row by kind.
func (s *Store) closeRow(kind, id string) error {
	var lastConflict error
	for range casAttempts {
		b, err := s.store.Get(id)
		if err != nil {
			return fmt.Errorf("reading %s %s: %w", kind, id, err)
		}
		if b.Status == beadStatusClosed {
			return nil
		}
		status := beadStatusClosed
		err = s.writer.UpdateIfMatch(id, b.Revision, beads.UpdateOpts{Status: &status})
		if err == nil {
			return nil
		}
		var stale *beads.PreconditionFailedError
		if !errors.As(err, &stale) {
			return fmt.Errorf("closing %s %s: %w", kind, id, err)
		}
		lastConflict = err
	}
	return fmt.Errorf("%w: closing %s %s lost %d revision races: %w", ErrContention, kind, id, casAttempts, lastConflict)
}
