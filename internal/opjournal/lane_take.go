package opjournal

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
}

func takenIDs(tb *takenBatch) []string {
	out := make([]string, len(tb.ops))
	for i, op := range tb.ops {
		out[i] = op.ID
	}
	return out
}

// newLease mints this Take's lease. A conversation id long enough to push the
// lease past bd's assignee limit is refused before any write.
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
	lease, err := l.newLease(conv, spec.probe.Caps.leaseTTL(), now)
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
		cur := e.op
		next := cur
		next.Status = StatusExecuting
		next.Lease = lease
		next.Attempt = cur.Attempt + 1
		next.LaneSeq = spec.baseSeq + uint64(i) + 1
		next.RetryAt = time.Time{}
		if spec.kind == TakeContent {
			next.DeliveryKey = spec.key
		}
		if e.retake {
			next.Failures = cur.Failures + 1
			next.MaybeDelivered = cur.Attempt
			next.LastError = lastErrorLeaseExpired
		}
		next.Transitions = appendTransition(cur.Transitions, Transition{Status: StatusExecuting, At: now, By: by})
		item, err := opUpdateItem(cur, next)
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
	return l.resolveAmbiguousTake(conv.ID, spec, lease, ops, err)
}

// resolveAmbiguousTake decides whether a Take that ended with an unknown error
// committed, by re-reading the runtime row and, when it holds the lease, each
// op's revision.
func (l *ConversationLane) resolveAmbiguousTake(conv string, spec *takeSpec, lease Lease, ops []Op, cause error) (*takenBatch, takeOutcome, error) {
	b, found, err := l.store.runtimeBead(conv)
	if err != nil {
		return nil, takeFailed, errors.Join(fmt.Errorf("taking ops of conversation %s: %w", conv, cause), err)
	}
	if !found {
		return nil, takeLost, nil
	}
	row, err := DecodeRuntimeRow(b)
	if err != nil {
		return nil, takeFailed, errors.Join(fmt.Errorf("taking ops of conversation %s: %w", conv, cause), err)
	}
	if row.Lease != lease {
		return nil, takeLost, nil
	}
	for i := range ops {
		ob, err := l.store.store.Get(ops[i].ID)
		if err != nil {
			return nil, takeFailed, fmt.Errorf("re-reading op %s after an ambiguous Take: %w", ops[i].ID, err)
		}
		got, err := DecodeOp(ob)
		if err != nil {
			return nil, takeFailed, err
		}
		if got.Lease != lease || got.Status != StatusExecuting {
			return nil, takeFailed, fmt.Errorf("%w: op %s does not hold the Take's lease after the Take committed", ErrCorruptOp, ops[i].ID)
		}
		ops[i].Revision = ob.Revision
	}
	return &takenBatch{spec: spec, lease: lease, ops: ops, row: b}, takeOK, nil
}

// takeRowItem is the Take's runtime-row item and the row as the Take leaves
// it (revision unset).
func (l *ConversationLane) takeRowItem(rt runtimeSnap, spec *takeSpec, conv string, lease Lease, k uint64) (beads.GuardedItem, beads.Bead, error) {
	if !rt.found {
		b, err := EncodeRuntimeRow(l.store.prefix, RuntimeRow{
			Conversation: conv, Generation: spec.inc.gen, Epoch: spec.inc.epoch, Seq: k, Lease: lease,
		})
		if err != nil {
			return beads.GuardedItem{}, beads.Bead{}, err
		}
		create := b
		return beads.GuardedItem{Create: &create}, b, nil
	}
	if rt.row.Closed {
		return beads.GuardedItem{}, beads.Bead{}, fmt.Errorf("%w: conversation %s: the runtime row is closed and does not reopen", ErrIllegalTransition, conv)
	}
	next := rt.row
	next.Seq += k
	next.Lease = lease
	next.Generation, next.Epoch = spec.inc.gen, spec.inc.epoch
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

// applyLocal returns b with opts applied as a store applies them: metadata
// merged (an empty value removes the key), then assignee and status.
func applyLocal(b beads.Bead, opts beads.UpdateOpts) beads.Bead {
	out := b
	out.Metadata = maps.Clone(b.Metadata)
	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	for k, v := range opts.Metadata {
		if v == "" {
			delete(out.Metadata, k)
			continue
		}
		out.Metadata[k] = v
	}
	if opts.Assignee != nil {
		out.Assignee = *opts.Assignee
	}
	if opts.Status != nil {
		out.Status = *opts.Status
	}
	return out
}
