package opjournal

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// takeOutcome says how a Take ended.
type takeOutcome int

const (
	// takeOK: the Take is durable; the effect may run.
	takeOK takeOutcome = iota
	// takeLost: another writer moved a row; re-read and re-plan.
	takeLost
	// takeFailed: stop the pass with the error.
	takeFailed
)

// takenBatch is a durable Take: what the effect and Done work from.
type takenBatch struct {
	spec  *takeSpec
	lease Lease
	// ops are the ops as the Take wrote them, each at its revision after
	// the Take.
	ops []Op
	// row is the runtime row as the Take left it, at its revision.
	row beads.Bead
	// abandon, when set, says the effect must not run: an op of the Take
	// closed before its effect (the text names content that is no longer
	// open), so the Take is released as transient with this cause.
	abandon error
}

func takenIDs(tb *takenBatch) []string {
	out := make([]string, len(tb.ops))
	for i, op := range tb.ops {
		out[i] = op.ID
	}
	return out
}

// newLease mints this Take's lease. maxConversationLen keeps it within bd's
// assignee limit; validate still refuses an invalid lease before any write.
func (l *ConversationLane) newLease(conv ConversationView, ttl time.Duration, now time.Time) (Lease, error) {
	n, err := l.nonce()
	if err != nil {
		return Lease{}, err
	}
	lease := Lease{
		Lane:         string(LaneConversation),
		Conversation: conv.ID,
		Generation:   conv.Generation,
		Nonce:        l.host + "." + n,
		Deadline:     now.Add(ttl),
	}
	if err := lease.validate(); err != nil {
		return Lease{}, fmt.Errorf("minting a lease for conversation %s: %w", conv.ID, err)
	}
	return lease, nil
}

// take writes one Take as a single guarded batch (DES-O1 §4.6): the runtime
// row (created at its pinned id, or updated under its revision) with seq += k,
// the lease cell and the effective incarnation, then each op under its
// revision: executing, the lease, attempt+1, its lane_seq and delivery key.
// An unknown error is resolved by re-reading: the Take committed exactly when
// the runtime row holds this Take's lease, and no effect runs before that is
// known (I5).
func (l *ConversationLane) take(ctx context.Context, snap laneSnapshot, spec *takeSpec, conv ConversationView) (*takenBatch, takeOutcome, error) {
	now := l.now()
	lease, err := l.newLease(conv, takeLeaseTTL(spec.probe.Caps, spec.ops), now)
	if err != nil {
		return nil, takeFailed, err
	}
	k := uint64(len(spec.ops))
	rowItem, rowBead, err := l.takeRowItem(snap.rt, spec, conv.ID, lease, k)
	if err != nil {
		return nil, takeFailed, err
	}
	items := []beads.GuardedItem{rowItem}
	ops := make([]Op, len(spec.ops))
	by := "lane:" + lease.Nonce
	for i, e := range spec.ops {
		// e.cur carries a re-take's expiry bookkeeping; e.op guards.
		next := e.cur
		next.Status = StatusExecuting
		next.Lease = lease
		next.Attempt = e.op.Attempt + 1
		next.LaneSeq = spec.baseSeq + uint64(i) + 1
		next.RetryAt = time.Time{}
		// Only content carries a delivery key: a control op's is its id,
		// the same on every re-take, and a unit's interrupt is one.
		if spec.kind == TakeContent || spec.kind == TakeNow && e.op.Kind == KindMessage {
			next.DeliveryKey = spec.key
		}
		next.Transitions = appendTransition(e.op.Transitions, Transition{Status: StatusExecuting, At: now, By: by})
		item, err := opUpdateItem(e.op, next)
		if err != nil {
			return nil, takeFailed, err
		}
		items = append(items, item)
		ops[i] = next
	}
	res, kind, err := l.store.apply(ctx, "opjournal: lane take", items)
	switch kind {
	case batchOK:
		rowBead.Revision = res.Revisions[rowBead.ID]
		for i := range ops {
			ops[i].Revision = res.Revisions[ops[i].ID]
		}
		return &takenBatch{spec: spec, lease: lease, ops: ops, row: rowBead}, takeOK, nil
	case batchRefused:
		return nil, takeLost, nil
	case batchInvalid:
		return nil, takeFailed, fmt.Errorf("taking ops of conversation %s: %w", conv.ID, err)
	}
	return l.resolveAmbiguousTake(ctx, conv.ID, spec, lease, ops, err)
}

// resolveAmbiguousTake decides whether a Take that ended with an unknown error
// committed, by re-reading the runtime row and, when it holds the lease, each
// op; a read that fails is retried (reread). The batch goes on with the ops as
// re-read, so a cancel recorded in the window reaches the effect and Done. An
// op that no longer holds the lease closed in the window: it is dropped, and
// the rest are released without the effect. An error names cause too.
func (l *ConversationLane) resolveAmbiguousTake(ctx context.Context, conv string, spec *takeSpec, lease Lease, ops []Op, cause error) (*takenBatch, takeOutcome, error) {
	failed := func(err error) (*takenBatch, takeOutcome, error) {
		return nil, takeFailed, errors.Join(fmt.Errorf("taking ops of conversation %s: %w", conv, cause), err)
	}
	var b beads.Bead
	var found bool
	if err := l.reread(ctx, lease, func() (err error) {
		b, found, err = l.store.runtimeBead(conv)
		return err
	}); err != nil {
		return failed(err)
	}
	if !found {
		return nil, takeLost, nil
	}
	row, err := DecodeRuntimeRow(b)
	if err != nil {
		return failed(err)
	}
	if row.Lease != lease {
		return nil, takeLost, nil
	}
	tb := &takenBatch{spec: spec, lease: lease, row: b}
	var lost []string
	for _, op := range ops {
		var ob beads.Bead
		if err := l.reread(ctx, lease, func() (err error) {
			ob, err = l.store.store.Get(op.ID)
			return err
		}); err != nil {
			return failed(fmt.Errorf("re-reading op %s after an ambiguous Take: %w", op.ID, err))
		}
		got, err := DecodeOp(ob)
		if err != nil {
			return failed(err)
		}
		if got.Lease != lease || got.Status != StatusExecuting {
			lost = append(lost, op.ID)
			continue
		}
		tb.ops = append(tb.ops, got)
	}
	if len(lost) > 0 {
		tb.abandon = fmt.Errorf("op %s closed between the Take and its effect", strings.Join(lost, ", "))
	}
	return tb, takeOK, nil
}

// reread runs read, a re-read that resolves an unknown Take, and retries it
// on Done's backoff while it fails, up to casAttempts tries and while the
// lease lasts: past it the effect cannot run, so the error stands.
func (l *ConversationLane) reread(ctx context.Context, lease Lease, read func() error) error {
	backoff := doneBackoffStart
	for try := 1; ; try++ {
		err := read()
		if err == nil || try == casAttempts || !l.now().Before(lease.Deadline) {
			return err
		}
		if serr := l.sleep(ctx, backoff); serr != nil {
			return errors.Join(err, serr)
		}
		backoff = min(2*backoff, doneBackoffMax)
	}
}

// takeRowItem is the Take's runtime-row item and the row as the Take leaves
// it (revision unset).
func (l *ConversationLane) takeRowItem(rt runtimeSnap, spec *takeSpec, conv string, lease Lease, k uint64) (beads.GuardedItem, beads.Bead, error) {
	next := RuntimeRow{Conversation: conv}
	if rt.found {
		if rt.row.Closed {
			return beads.GuardedItem{}, beads.Bead{}, fmt.Errorf("%w: conversation %s: the runtime row is closed and does not reopen", ErrIllegalTransition, conv)
		}
		next = rt.row
	}
	next.Seq += k
	next.Lease = lease
	next.Generation, next.Epoch = spec.inc.gen, spec.inc.epoch
	return l.rowWrite(rt, next)
}

// rowWrite is the guarded item that writes next over the runtime row as read
// in rt: a create at its pinned id when there is none, else an update under
// its revision. It returns the row as the item leaves it (revision unset).
func (l *ConversationLane) rowWrite(rt runtimeSnap, next RuntimeRow) (beads.GuardedItem, beads.Bead, error) {
	if !rt.found {
		b, err := EncodeRuntimeRow(l.store.prefix, next)
		if err != nil {
			return beads.GuardedItem{}, beads.Bead{}, err
		}
		create := b
		return beads.GuardedItem{Create: &create}, b, nil
	}
	opts, err := RuntimeRowPatch(rt.bead, next)
	if err != nil {
		return beads.GuardedItem{}, beads.Bead{}, err
	}
	rev := rt.bead.Revision
	return beads.GuardedItem{Update: &beads.GuardedUpdate{ID: rt.bead.ID, Opts: opts, ExpectedRevision: &rev}},
		applyLocal(rt.bead, opts), nil
}

// opUpdateItem is the guarded update that turns cur, as read at its revision,
// into next. It writes only the owned keys that change, so a newer build's
// keys survive.
func opUpdateItem(cur, next Op) (beads.GuardedItem, error) {
	have, err := EncodeOp(cur)
	if err != nil {
		return beads.GuardedItem{}, err
	}
	opts, err := patch(have, cur, next)
	if err != nil {
		return beads.GuardedItem{}, err
	}
	rev := cur.Revision
	return beads.GuardedItem{Update: &beads.GuardedUpdate{ID: cur.ID, Opts: opts, ExpectedRevision: &rev}}, nil
}

// applyLocal returns b with opts applied as the stores' appliers apply them:
// metadata merged by assignment, so an empty value stays and clears the key
// (a store's callers read absent and empty alike), then assignee and status.
func applyLocal(b beads.Bead, opts beads.UpdateOpts) beads.Bead {
	out := b
	out.Metadata = maps.Clone(b.Metadata)
	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	maps.Copy(out.Metadata, opts.Metadata)
	if opts.Assignee != nil {
		out.Assignee = *opts.Assignee
	}
	if opts.Status != nil {
		out.Status = *opts.Status
	}
	return out
}
