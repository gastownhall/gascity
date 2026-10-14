package opjournal

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

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

// Propose writes the op req describes, create-if-absent at its pinned id. It
// reads the id first, so a retry never reaches Create. A new op starts
// submitted with its first transition recorded and returns created=true.
// Re-proposing the same request returns the stored op and created=false; the
// same key with another request is *KeyReuseError. Refused requests write
// nothing.
func (s *Store) Propose(req Request) (Op, bool, error) {
	spec, err := Lookup(req.Kind)
	if err != nil {
		return Op{}, false, err
	}
	now := s.clk.Now()
	op, err := normalizeRequest(req, spec, now)
	if err != nil {
		return Op{}, false, err
	}
	op.ID, err = DeriveID(s.prefix, op.Conversation, op.Producer.Principal, op.ClientKey)
	if err != nil {
		return Op{}, false, err
	}
	if op.After == op.ID {
		return Op{}, false, invalidf("op %s cannot come after itself", op.ID)
	}
	op.PayloadDigest = RequestDigest(op)
	op.Status = StatusSubmitted
	op.Transitions = []Transition{{Status: StatusSubmitted, At: op.CreatedAt, By: op.Producer.Principal}}
	b, err := EncodeOp(op)
	if err != nil {
		return Op{}, false, err
	}
	// Read first: a retry then never reaches Create, so a store that
	// overwrites a duplicate pinned id instead of refusing it cannot reset
	// the stored op. The create below still loses cleanly to a concurrent
	// proposer on every store that refuses duplicates.
	existing, getErr := s.store.Get(op.ID)
	switch {
	case getErr == nil:
		return s.resolveExisting(op, existing)
	case !errors.Is(getErr, beads.ErrNotFound):
		return Op{}, false, fmt.Errorf("reading op %s before creating it: %w", op.ID, getErr)
	}
	created, createErr := s.store.Create(b)
	if createErr == nil {
		if created.ID != op.ID {
			strayErr := s.writer.DeleteIfMatch(created.ID, created.Revision)
			return Op{}, false, errors.Join(
				fmt.Errorf("%w: wanted %s, store minted %s", ErrPinnedIDNotHonored, op.ID, created.ID), strayErr)
		}
		return op, true, nil
	}
	existing, getErr = s.store.Get(op.ID)
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

// ListOpen returns the conversation's open ops in CompareOrder order. It
// issues exactly one read, keyed by the conversation label, and fails loudly
// on any row that does not decode.
func (s *Store) ListOpen(conversation string) ([]Op, error) {
	if err := validateConversation(conversation); err != nil {
		return nil, err
	}
	rows, err := s.store.List(beads.ListQuery{
		Label:    ConversationLabel(conversation),
		Sort:     beads.SortCreatedAsc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("listing ops of conversation %s: %w", conversation, err)
	}
	out := make([]Op, 0, len(rows))
	for _, b := range rows {
		op, err := DecodeOp(b)
		if err != nil {
			return nil, fmt.Errorf("listing ops of conversation %s: %w", conversation, err)
		}
		if op.Conversation != conversation {
			return nil, corruptf(b.ID, "labeled for conversation %s but belongs to %s", conversation, op.Conversation)
		}
		if op.Status != StatusClosed {
			out = append(out, op)
		}
	}
	slices.SortFunc(out, CompareOrder)
	return out, nil
}

// legalEdges is the status machine. Only Close and Cancel reach closed.
var legalEdges = map[Status][]Status{
	StatusSubmitted: {StatusAccepted, StatusExecuting},
	StatusAccepted:  {StatusExecuting, StatusSettling},
	StatusExecuting: {StatusSettling},
}

// Transition moves op id from expect to to. A status other than expect is
// *StatusError; an edge outside the machine, a lifecycle op entering
// executing, and any move to submitted or closed are ErrIllegalTransition.
func (s *Store) Transition(id string, expect, to Status, by string) (Op, error) {
	if err := validateText("transition by", by, maxPrincipalLen); err != nil {
		return Op{}, err
	}
	return s.mutate(id, by, func(cur Op, spec Spec) (Op, bool, error) {
		if cur.Status != expect {
			return Op{}, false, &StatusError{ID: id, Expected: expect, Current: cur.Status}
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
			if cur.Outcome != outcome {
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
	next := cur
	next.Status = StatusClosed
	next.Outcome = outcome
	next.Lease = Lease{}
	next.Transitions = appendTransition(cur.Transitions, Transition{Status: StatusClosed, At: utc(clk.Now()), By: by})
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
// result.
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
// build's to remove.
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
func (s *Store) closeBead(id string) error {
	var lastConflict error
	for range casAttempts {
		b, err := s.store.Get(id)
		if err != nil {
			return fmt.Errorf("reading op %s: %w", id, err)
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
			return fmt.Errorf("closing op %s: %w", id, err)
		}
		lastConflict = err
	}
	return fmt.Errorf("%w: closing op %s lost %d revision races: %w", ErrContention, id, casAttempts, lastConflict)
}
