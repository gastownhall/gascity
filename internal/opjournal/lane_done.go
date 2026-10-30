package opjournal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
)

// errLeaseTooShort is the cause of a release when no time is left to run the
// effect inside the lease.
var errLeaseTooShort = errors.New("opjournal: no time left in the lease to run the effect")

// errSettleByPassed ends a Done whose result's SettleBy passed before the
// Done landed: the batch stays executing until its lease lapses.
var errSettleByPassed = errors.New("opjournal: the effect's settle-by time passed before its Done landed; it is taken again, as maybe delivered, once its lease lapses")

// resultKind is an effect result's arm.
type resultKind int

const (
	resultDelivered resultKind = iota
	resultSettled
	resultNotDelivered
	resultRejected
	resultUnknown
)

// effectResult is one effect's result, normalized across content and
// control.
type effectResult struct {
	kind         resultKind
	delivered    Delivered
	settled      Settled
	notDelivered NotDelivered
	rejected     Rejected
	unknown      Unknown
	// contractErr reports an executor that broke its contract; the result
	// is then handled as Unknown.
	contractErr error
}

// unknownResult is the Unknown an executor's contract breach becomes.
func unknownResult(contract error) effectResult {
	return effectResult{kind: resultUnknown, contractErr: contract}
}

// effect runs the taken batch's effect inside the lease: its context gets
// the lease's time left on the lane's clock, less ExecutorMargin, as a
// wall-clock timeout. With no time left, or an abandoned Take, nothing runs
// and the batch is released as transient.
func (l *ConversationLane) effect(ctx context.Context, exec Executor, tb *takenBatch) (effectResult, time.Time) {
	now := l.now()
	if tb.abandon != nil {
		return effectResult{kind: resultNotDelivered, notDelivered: NotDelivered{Transient: true, Cause: tb.abandon}}, now
	}
	budget := tb.lease.Deadline.Sub(now) - ExecutorMargin
	if budget <= 0 {
		return effectResult{kind: resultNotDelivered, notDelivered: NotDelivered{Transient: true, Cause: errLeaseTooShort}}, now
	}
	ectx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	spec := tb.spec
	preCall := l.now()
	if spec.kind == TakeContent {
		r := exec.Deliver(ectx, Delivery{
			Fence:      spec.fence,
			Key:        spec.key,
			Seq:        tb.ops[0].LaneSeq,
			Mode:       spec.mode,
			Steer:      spec.steer,
			Ops:        slices.Clone(tb.ops),
			Redelivery: spec.redelivery,
			Text:       spec.text,
		})
		return normalizeDelivery(r), preCall
	}
	op := tb.ops[0]
	var r ControlResult
	switch op.Kind {
	case KindInterrupt:
		r = exec.Interrupt(ectx, InterruptCall{Fence: spec.fence, Op: op, Turn: op.Scope.Turn, Seq: op.LaneSeq})
	case KindRespond:
		call, refused := respondCall(spec.fence, op)
		if refused != nil {
			return normalizeControl(*refused), preCall
		}
		r = exec.Respond(ectx, call)
	default:
		return unknownResult(fmt.Errorf("op %s: %s is not a control kind", op.ID, op.Kind)), preCall
	}
	return normalizeControl(r), preCall
}

func normalizeDelivery(r DeliveryResult) effectResult {
	switch v := r.(type) {
	case Delivered:
		return effectResult{kind: resultDelivered, delivered: v}
	case NotDelivered:
		return effectResult{kind: resultNotDelivered, notDelivered: v}
	case Rejected:
		return effectResult{kind: resultRejected, rejected: v}
	case Unknown:
		return effectResult{kind: resultUnknown, unknown: v}
	default:
		return unknownResult(fmt.Errorf("executor returned delivery result %T", r))
	}
}

func normalizeControl(r ControlResult) effectResult {
	switch v := r.(type) {
	case Settled:
		return effectResult{kind: resultSettled, settled: v}
	case NotDelivered:
		return effectResult{kind: resultNotDelivered, notDelivered: v}
	case Rejected:
		return effectResult{kind: resultRejected, rejected: v}
	case Unknown:
		return effectResult{kind: resultUnknown, unknown: v}
	default:
		return unknownResult(fmt.Errorf("executor returned control result %T", r))
	}
}

// doneArm is how the result settles the batch.
func (er effectResult) doneArm() DoneArm {
	switch er.kind {
	case resultDelivered, resultSettled:
		return DoneAccepted
	case resultRejected:
		if er.rejected.KeyRejected {
			return DoneReleased
		}
		return DoneRejected
	default:
		return DoneReleased
	}
}

// settleBy is the last instant the lane may record the result, zero for no
// bound: a delivery's or a release's SettleBy.
func (er effectResult) settleBy() time.Time {
	switch er.kind {
	case resultDelivered:
		return er.delivered.SettleBy
	case resultNotDelivered:
		return er.notDelivered.SettleBy
	default:
		return time.Time{}
	}
}

// settle validates the result against the executor contract and computes the
// succeeded outcome of an accepted batch. A Delivered without a valid ack
// becomes Unknown. Once the runtime acked, a defect in any other recording
// field never makes the effect run again: the field is clamped or dropped,
// the outcome records DegradationExecutorContract, and the breach is
// reported in contractErr.
func (l *ConversationLane) settle(tb *takenBatch, er effectResult, preCall time.Time) (effectResult, Outcome) {
	spec := tb.spec
	switch er.kind {
	case resultDelivered:
		d := er.delivered
		if !slices.Contains(ackKinds, d.Record.Ack) {
			return unknownResult(fmt.Errorf("executor delivery ack %q is not delivered, delivered_unobserved or in_band", d.Record.Ack)), Outcome{}
		}
		var breaches []error
		breach := func(err error) { breaches = append(breaches, err) }
		rec := d.Record
		switch {
		case rec.At.IsZero():
			breach(errors.New("executor delivery record has no time"))
			rec.At = preCall
		case !storableTime(rec.At):
			breach(fmt.Errorf("executor delivery record time %s is outside years 0 through 9999", rec.At))
			rec.At = preCall
		case rec.At.Before(preCall):
			breach(fmt.Errorf("executor delivery record time %s is before the call at %s", formatTime(rec.At), formatTime(preCall)))
			rec.At = preCall
		}
		if !storableTime(rec.MessageAt) {
			breach(fmt.Errorf("executor delivery message time %s is outside years 0 through 9999", rec.MessageAt))
			rec.MessageAt = time.Time{}
		}
		if rec.MessageID != "" {
			if err := validateText("delivery message id", rec.MessageID, maxMessageIDLen); err != nil {
				breach(err)
				rec.MessageID = ""
			}
		}
		if err := validateOptionalText("delivery turn id", rec.TurnID); err != nil {
			breach(err)
			rec.TurnID = ""
		}
		rec.Seq = tb.ops[0].LaneSeq
		rec.Generation, rec.Epoch = spec.fence.Generation, spec.fence.Epoch
		rec.Mode = spec.mode
		ev := d.Evidence
		ev.LiveAtProbe = spec.probe.TurnLive
		ev.LiveTurn = spec.probe.Turn
		ev.Known = slices.Clone(spec.probe.KnownTurns)
		link, _ := CorrelateTurn(rec, ev)
		if err := validateOptionalText("result turn", link.Turn); err != nil {
			breach(err)
			link = TurnLink{Confidence: LinkNone, Basis: link.Basis}
		}
		degradation := d.Degradation
		if degradation == "" && (spec.probe.Caps.Input == FenceNone || spec.view.Token == "") {
			degradation = DegradationUnfencedDelivery
		}
		if degradation == "" {
			// A post-unit deliver-now message's mark (its interrupt's
			// failure) yields to the delivery's own.
			degradation = spec.degradation
		}
		o := Outcome{Arm: ArmSucceeded, Degradation: degradation, Result: Result{
			Generation:   spec.fence.Generation,
			ContextEpoch: spec.fence.Epoch,
			Turn:         link.Turn,
			Via:          d.Via,
			Delivery: RecordedDelivery{
				Seq: rec.Seq, At: utc(rec.At), Ack: rec.Ack, MessageID: rec.MessageID, MessageAt: utc(rec.MessageAt),
			},
			TurnLink:  link.Confidence,
			TurnBasis: link.Basis,
		}}
		return acceptedWithin(er, o, breaches, "executor delivery result")
	case resultSettled:
		s := er.settled
		var breaches []error
		turn := s.Turn
		if err := validateOptionalText("result turn", turn); err != nil {
			breaches = append(breaches, err)
			turn = ""
		}
		o := Outcome{Arm: ArmSucceeded, Degradation: s.Degradation, Result: Result{
			Generation: spec.fence.Generation, ContextEpoch: spec.fence.Epoch, Turn: turn, Via: s.Via,
		}}
		return acceptedWithin(er, o, breaches, "executor control result")
	case resultRejected:
		r := er.rejected
		if r.KeyRejected {
			if spec.kind != TakeContent {
				return unknownResult(errors.New("executor rejected a delivery key on a control op")), Outcome{}
			}
			return er, Outcome{}
		}
		o := Outcome{Arm: r.Arm, Reason: r.Reason}
		if (r.Arm != ArmFailed && r.Arm != ArmSkipped) || validateOutcome(o, Cancel{}) != nil || r.Reason == ReasonCoalescedInto {
			return unknownResult(fmt.Errorf("executor rejected with %s{%s}; want failed or skipped with a reason token", r.Arm, r.Reason)), Outcome{}
		}
		if r.Turn != "" {
			if err := validateOptionalText("result turn", r.Turn); err != nil {
				er.contractErr = fmt.Errorf("executor control rejection: %w", err)
			} else if spec.kind == TakeControl {
				o.Result.Turn = r.Turn
			}
		}
		return er, o
	}
	return er, Outcome{}
}

// acceptedWithin finishes the succeeded outcome of an acked effect: a via or
// degradation that breaks the contract is dropped or replaced, any breach
// marks the outcome DegradationExecutorContract, and the breaches are
// reported in contractErr. An outcome that is still invalid is a lane bug,
// handled as Unknown.
func acceptedWithin(er effectResult, o Outcome, breaches []error, what string) (effectResult, Outcome) {
	if err := validateOptionalText("result via", o.Result.Via); err != nil {
		breaches = append(breaches, err)
		o.Result.Via = ""
	}
	if o.Degradation != "" {
		if err := validateToken("degradation", o.Degradation); err != nil {
			breaches = append(breaches, err)
			o.Degradation = ""
		}
	}
	if len(breaches) > 0 {
		o.Degradation = DegradationExecutorContract
		er.contractErr = fmt.Errorf("%s: %w", what, errors.Join(breaches...))
	}
	if err := validateOutcome(o, Cancel{}); err != nil {
		return unknownResult(errors.Join(er.contractErr, fmt.Errorf("%s: %w", what, err))), Outcome{}
	}
	return er, o
}

// doneNext is the op Done writes for cur, an op of this Take as it now
// stands, under result er (DES-O1 §4.7 as refined by OJ-10's design).
// success is the accepted batch's outcome.
func (l *ConversationLane) doneNext(tb *takenBatch, er effectResult, success Outcome, cur Op, now time.Time) Op {
	by := "lane:" + tb.lease.Nonce
	canceled := Outcome{Arm: ArmCanceled}
	release := func(c Op, backoff time.Duration, lastErr string) Op {
		next := c
		next.Status = StatusAccepted
		next.Lease = Lease{}
		next.RetryAt = now.Add(backoff)
		next.LastError = lastErr
		next.Transitions = appendTransition(c.Transitions, Transition{Status: StatusAccepted, At: now, By: by})
		return next
	}
	// failed counts one failed attempt: poison at MaxFailures, else a
	// release after the executor-error backoff.
	failed := func(c Op, lastErr string) Op {
		c.Failures++
		if c.Failures >= MaxFailures {
			c.Failures = MaxFailures
			c.LastError = lastErr
			return closedAt(c, Outcome{Arm: ArmFailed, Reason: ReasonPoison}, by, now)
		}
		return release(c, ExecutorErrorBackoff, lastErr)
	}
	switch er.kind {
	case resultDelivered, resultSettled:
		return closedAt(cur, success, by, now)
	case resultNotDelivered:
		if cur.Cancel.Recorded() {
			return closedAt(cur, canceled, by, now)
		}
		cause := er.notDelivered.Cause
		if er.notDelivered.Transient {
			token := lastErrorTransient
			if er.notDelivered.Unavailable {
				token = lastErrorUnavailable
			}
			return release(cur, transientBackoff(cur.Attempt-cur.Failures), lastError(token, cause))
		}
		return failed(cur, lastError(lastErrorNotDelivered, cause))
	case resultUnknown:
		cur.MaybeDelivered = cur.Attempt
		switch {
		case cur.Cancel.Recorded():
			return closedAt(cur, canceled, by, now)
		case cur.Policy.Guarantee == GuaranteeAtMostOnce:
			return closedAt(cur, Outcome{Arm: ArmFailed, Reason: ReasonAmbiguousDelivery}, by, now)
		}
		return failed(cur, lastError(lastErrorUnknown, errors.Join(er.unknown.Cause, er.contractErr)))
	default: // resultRejected
		if cur.Cancel.Recorded() {
			return closedAt(cur, canceled, by, now)
		}
		if er.rejected.KeyRejected {
			return failed(cur, lastError(lastErrorKeyRejected, errors.New(er.rejected.Reason)))
		}
		if cause := er.rejected.Cause; cause != nil {
			cur.LastError = lastError(er.rejected.Reason, cause)
		}
		return closedAt(cur, success, by, now)
	}
}

// transientBackoff is TransientBackoffBase·2^(k-1), capped at
// TransientBackoffMax; k counts the transient attempts (attempt − failures),
// at least 1.
func transientBackoff(k int) time.Duration {
	d := TransientBackoffBase
	for i := 1; i < k && d < TransientBackoffMax; i++ {
		d *= 2
	}
	return min(d, TransientBackoffMax)
}

// lastError renders gc.op.last_error: token, then ": " and the cause's text
// with control characters replaced, cut at a rune boundary to fit.
func lastError(token string, cause error) string {
	if cause == nil || cause.Error() == "" {
		return token
	}
	text := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(cause.Error(), "?"))
	out := token + ": " + text
	for len(out) > maxLastErrorLen {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	return strings.TrimRight(out, " ")
}

// doneItemKind names a Done batch item.
type doneItemKind int

const (
	doneRow doneItemKind = iota
	doneOp
	doneFact
)

// doneItem is one row Done writes, kept current through reconciliation.
type doneItem struct {
	kind doneItemKind
	id   string
	row  beads.Bead // doneRow: the row as last read
	cur  Op         // doneOp: the op as last read
	next Op         // doneOp: what Done writes
	fact Fact       // doneFact
	// done says the item needs no more writing: written, landed earlier or
	// dropped.
	done       bool
	written    bool
	superseded bool
	// stale says a refusal or an unknown error left the item's row unread
	// since: it is re-read before it is written again.
	stale bool
	// redo, when set, is what Done writes for the op as re-read under this
	// Take's lease: a deliver-now unit settles each of its ops under its
	// own result. Unset, the batch's one result decides (doneNext).
	redo func(cur Op, now time.Time) Op
}

// done settles a Take with one guarded batch (DES-O1 §4.7), reconciled after
// a refusal or an unknown error by re-reading what moved: a row still under
// this Take's lease is rewritten at its fresh revision; an op another lane
// re-took, or that closed otherwise, is left alone (I9). It runs on past ctx,
// for the time left in the lease bounded by doneMinBudget and doneMaxBudget,
// but never past the result's SettleBy. Every respond it closes then
// re-checks the end of its occurrence (Store.endClosed).
func (l *ConversationLane) done(ctx context.Context, tb *takenBatch, er effectResult, preCall time.Time, res *PassResult) error {
	er, success := l.settle(tb, er, preCall)
	now := l.now()
	items := []*doneItem{{kind: doneRow, id: tb.row.ID, row: tb.row}}
	for _, op := range tb.ops {
		items = append(items, &doneItem{kind: doneOp, id: op.ID, cur: op, next: l.doneNext(tb, er, success, op, now)})
	}
	dctx, cancel := doneContext(ctx, tb, er.settleBy(), now)
	defer cancel()
	endErr, err := l.writeDone(dctx, tb, er, success, items, now)
	if err == nil {
		res.Done = er.doneArm()
	}
	endErr = errors.Join(endErr, l.reportDone(res, items, err, now))
	return errors.Join(err, er.contractErr, endErr)
}

// doneContext is a Done's context: it runs on past ctx, for the time left in
// tb's lease bounded by doneMinBudget and doneMaxBudget, but never past
// settleBy when one is set.
func doneContext(ctx context.Context, tb *takenBatch, settleBy, now time.Time) (context.Context, context.CancelFunc) {
	budget := min(max(tb.lease.Deadline.Sub(now)-doneBudgetSlack, doneMinBudget), doneMaxBudget)
	var cause error
	if !settleBy.IsZero() && settleBy.Sub(now) < budget {
		budget, cause = settleBy.Sub(now), errSettleByPassed
	}
	return context.WithTimeoutCause(context.WithoutCancel(ctx), budget, cause)
}

// reportDone reports a Done's op items in res: a superseded item makes the
// Done superseded, a written close is reported and re-checks the end of its
// occurrence (Store.endClosed), and a written release's gate is the next
// timer. It returns the re-checks' failures.
func (l *ConversationLane) reportDone(res *PassResult, items []*doneItem, err error, now time.Time) error {
	var endErr error
	for _, it := range items {
		switch {
		case it.kind != doneOp:
		case it.superseded:
			if err == nil {
				res.Done = DoneSuperseded
			}
		case it.written && it.next.Status == StatusClosed:
			res.Closed = append(res.Closed, ClosedOp{ID: it.id, Arm: it.next.Outcome.Arm, Reason: it.next.Outcome.Reason})
			endErr = errors.Join(endErr, l.store.endClosed(it.next, now))
		case it.written:
			res.Next = minTime(res.Next, it.next.RetryAt)
		}
	}
	return endErr
}

// writeDone applies Done's items, with the ends of the occurrences their
// closes give (doneEnds), until every one is written or dropped. A refusal
// or an unknown error leaves the items stale; a transient error re-reading
// them backs off and retries, like an unknown batch error. The ends follow
// the ops as reconciled: a cancel that turned a release into a close gets
// its end. endErr is a failure reading the other answers to an occurrence,
// whose end then landed anyway. Once ctx ends it writes nothing more.
func (l *ConversationLane) writeDone(ctx context.Context, tb *takenBatch, er effectResult, success Outcome, base []*doneItem, now time.Time) (endErr, err error) {
	backoff := doneBackoffStart
	var lastErr error
	items := base
	reconciled := true
	for range casAttempts {
		if err := context.Cause(ctx); err != nil {
			return endErr, errors.Join(fmt.Errorf("settling ops of conversation %s: %w", tb.spec.view.ID, err), lastErr)
		}
		var readErr error
		for _, it := range items {
			if it.done || !it.stale {
				continue
			}
			rerr, err := l.reconcile(tb, er, success, it)
			if err != nil {
				return endErr, err
			}
			if rerr != nil {
				readErr = rerr
				continue
			}
			it.stale, reconciled = false, true
		}
		if readErr != nil {
			lastErr = readErr
			if err := l.sleep(ctx, backoff); err != nil {
				return endErr, errors.Join(fmt.Errorf("settling ops of conversation %s: %w", tb.spec.view.ID, lastErr), err)
			}
			backoff = min(2*backoff, doneBackoffMax)
			continue
		}
		if reconciled {
			if items, endErr, err = l.doneEnds(tb, base, items, now); err != nil {
				return endErr, err
			}
			reconciled = false
		}
		var batch []beads.GuardedItem
		var pending []*doneItem
		for _, it := range items {
			if it.done {
				continue
			}
			gi, err := l.doneGuardedItem(it)
			if err != nil {
				return endErr, err
			}
			batch = append(batch, gi)
			pending = append(pending, it)
		}
		if len(batch) == 0 {
			return endErr, nil
		}
		_, kind, err := l.store.apply(ctx, "opjournal: lane done", batch)
		switch kind {
		case batchOK:
			for _, it := range pending {
				it.done, it.written = true, true
			}
			return endErr, nil
		case batchInvalid:
			return endErr, fmt.Errorf("settling ops of conversation %s: %w", tb.spec.view.ID, err)
		case batchRefused:
			// A store refuses the whole batch at its first stale item, so
			// every pending item is re-read: k moved rows cost one retry,
			// not k.
			if !refusedByPending(err, pending) {
				return endErr, fmt.Errorf("settling ops of conversation %s: refused by a row it did not write: %w", tb.spec.view.ID, err)
			}
			lastErr = err
		default:
			lastErr = err
			if err := l.sleep(ctx, backoff); err != nil {
				return endErr, errors.Join(fmt.Errorf("settling ops of conversation %s: %w", tb.spec.view.ID, lastErr), err)
			}
			backoff = min(2*backoff, doneBackoffMax)
		}
		for _, it := range pending {
			it.stale = true
		}
	}
	return endErr, fmt.Errorf("%w: settling ops of conversation %s lost %d tries: %w", ErrContention, tb.spec.view.ID, casAttempts, lastErr)
}

// doneEnds is Done's items: base, the row and op items, then the end of the
// occurrence of every respond whose next closes it (DES-O1 §4.7, §10.2): its
// answer, or nothing sent while no other answer to it is open
// (Store.occurrenceEnd). An end already among items is kept, with what it
// knows of having landed; one no op gives any more is dropped. When the
// other answers cannot be read the end lands anyway, and endErr says so: an
// open answer then closes skipped{stale} and its requester answers the next
// occurrence, where leaving the occurrence open could spend its answers.
func (l *ConversationLane) doneEnds(tb *takenBatch, base, items []*doneItem, now time.Time) (out []*doneItem, endErr, err error) {
	have := make(map[string]*doneItem)
	for _, it := range items {
		if it.kind == doneFact {
			have[it.id] = it
		}
	}
	closing := make(map[string]bool, len(tb.ops))
	for _, op := range tb.ops {
		closing[op.ID] = true
	}
	out = slices.Clip(base)
	seen := make(map[string]bool)
	for _, it := range base {
		if it.kind != doneOp || it.superseded {
			continue
		}
		f, ok, rerr := l.store.occurrenceEnd(it.next, closing, now)
		if rerr != nil {
			endErr = errors.Join(endErr, rerr)
			f, ok = respondEnd(it.next, now)
		}
		if !ok {
			continue
		}
		id, err := FactID(l.store.prefix, f)
		if err != nil {
			return nil, endErr, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if prev, found := have[id]; found {
			out = append(out, prev)
			continue
		}
		f.ID = id
		out = append(out, &doneItem{kind: doneFact, id: id, fact: f})
	}
	return out, endErr, nil
}

// refusedByPending reports whether err, a batch refusal, names a row of
// pending.
func refusedByPending(err error, pending []*doneItem) bool {
	var item *beads.BatchItemError
	return errors.As(err, &item) && slices.ContainsFunc(pending, func(it *doneItem) bool { return it.id == item.ID })
}

// doneGuardedItem renders a Done item as a guarded batch item.
func (l *ConversationLane) doneGuardedItem(it *doneItem) (beads.GuardedItem, error) {
	switch it.kind {
	case doneRow:
		row, err := DecodeRuntimeRow(it.row)
		if err != nil {
			return beads.GuardedItem{}, err
		}
		row.Lease = Lease{}
		opts, err := RuntimeRowPatch(it.row, row)
		if err != nil {
			return beads.GuardedItem{}, err
		}
		rev := it.row.Revision
		return beads.GuardedItem{Update: &beads.GuardedUpdate{ID: it.id, Opts: opts, ExpectedRevision: &rev}}, nil
	case doneOp:
		return opUpdateItem(it.cur, it.next)
	default:
		return FactCreateItem(l.store.prefix, it.fact)
	}
}

// reconcile re-reads one Done item's row after a refusal or an unknown error
// and decides what is left to write. readErr is a failed read, to retry; err
// is a row Done cannot settle.
func (l *ConversationLane) reconcile(tb *takenBatch, er effectResult, success Outcome, it *doneItem) (readErr, err error) {
	switch it.kind {
	case doneRow:
		b, found, err := l.store.runtimeBead(tb.spec.view.ID)
		if err != nil {
			return fmt.Errorf("re-reading the runtime row of conversation %s while settling: %w", tb.spec.view.ID, err), nil
		}
		if !found {
			it.done = true
			return nil, nil
		}
		row, err := DecodeRuntimeRow(b)
		if err != nil {
			return nil, err
		}
		if row.Lease != tb.lease {
			it.done = true
			return nil, nil
		}
		it.row = b
		return nil, nil
	case doneOp:
		b, err := l.store.store.Get(it.id)
		if errors.Is(err, beads.ErrNotFound) {
			it.done, it.superseded = true, true
			return nil, nil
		}
		if err != nil {
			return fmt.Errorf("re-reading op %s while settling it: %w", it.id, err), nil
		}
		cur, err := DecodeOp(b)
		if err != nil {
			return nil, err
		}
		switch {
		case cur.Status == StatusClosed:
			it.done = true
			if it.next.Status == StatusClosed && cur.Outcome.Equal(it.next.Outcome) {
				it.written = true
			} else {
				it.superseded = true
			}
		case cur.Status == StatusExecuting && cur.Lease == tb.lease && it.redo != nil:
			it.cur = cur
			it.next = it.redo(cur, l.now())
		case cur.Status == StatusExecuting && cur.Lease == tb.lease:
			it.cur = cur
			it.next = l.doneNext(tb, er, success, cur, l.now())
		case releasedBy(cur, it.next):
			it.done, it.written = true, true
		default:
			it.done, it.superseded = true, true
		}
		return nil, nil
	default:
		b, err := l.store.store.Get(it.id)
		if errors.Is(err, beads.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return fmt.Errorf("re-reading fact %s while settling: %w", it.id, err), nil
		}
		got, err := DecodeFact(b)
		if err != nil {
			return nil, err
		}
		if got.Kind != it.fact.Kind || got.Conversation != it.fact.Conversation ||
			got.Generation != it.fact.Generation || got.occurrence() != it.fact.occurrence() {
			return nil, corruptf(it.id, "stored fact records another occurrence (hash collision)")
		}
		it.done = true
		return nil, nil
	}
}

// releasedBy reports an op that already holds the release next describes: an
// earlier ambiguous Done of this Take landed.
func releasedBy(cur, next Op) bool {
	if next.Status != StatusAccepted || cur.Status != StatusAccepted || !cur.Lease.IsZero() || len(cur.Transitions) == 0 {
		return false
	}
	last, want := cur.Transitions[len(cur.Transitions)-1], next.Transitions[len(next.Transitions)-1]
	return last.Status == want.Status && last.By == want.By && cur.Attempt == next.Attempt
}

// writeCloses writes rule closes, each with the end it gives its occurrence
// (closeBatch), and the runtime row's close when closeRow is set, in batches
// of at most beads.MaxGuardedItems items. done is false when a write was
// refused or ended unknown: the pass re-reads and re-plans.
func (l *ConversationLane) writeCloses(ctx context.Context, closes []ruleClose, closeRow bool, rt runtimeSnap, res *PassResult) (bool, error) {
	var rowItem *beads.GuardedItem
	if closeRow {
		row := rt.row
		row.Closed = true
		row.Lease = Lease{}
		opts, err := RuntimeRowPatch(rt.bead, row)
		if err != nil {
			return false, err
		}
		rev := rt.bead.Revision
		rowItem = &beads.GuardedItem{Update: &beads.GuardedUpdate{ID: rt.bead.ID, Opts: opts, ExpectedRevision: &rev}}
	}
	for len(closes) > 0 {
		batch, n, err := l.closeBatch(closes)
		if err != nil {
			return false, err
		}
		bc := closes[:n]
		closes = closes[n:]
		if len(closes) == 0 && rowItem != nil && len(batch) < beads.MaxGuardedItems {
			batch = append(batch, *rowItem)
			rowItem = nil
		}
		if ok, err := l.applyCloses(ctx, batch); !ok || err != nil {
			return false, err
		}
		for _, c := range bc {
			res.Closed = append(res.Closed, ClosedOp{ID: c.cur.ID, Arm: c.next.Outcome.Arm, Reason: c.next.Outcome.Reason})
		}
		if err := l.endClosedOccurrences(bc); err != nil {
			return false, err
		}
	}
	if rowItem != nil {
		if ok, err := l.applyCloses(ctx, []beads.GuardedItem{*rowItem}); !ok || err != nil {
			return false, err
		}
	}
	return true, nil
}

// closeBatch is the next close batch from the head of closes: each close and
// the end it gives its occurrence (Store.endItem), with room left for every
// close's end; n says how many closes it takes. An end is decided with the
// batch's closes so far as closing: of two answers to one occurrence closing
// in the batch the later gives the end, and an end withheld for an answer
// closing in a later batch is re-checked once its batch lands
// (endClosedOccurrences).
func (l *ConversationLane) closeBatch(closes []ruleClose) (items []beads.GuardedItem, n int, err error) {
	closing := map[string]bool{}
	ends := map[string]bool{}
	for ; n < len(closes) && len(items)+2 <= beads.MaxGuardedItems; n++ {
		c := closes[n]
		item, err := opUpdateItem(c.cur, c.next)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
		closing[c.cur.ID] = true
		end, ok, err := l.store.endItem(c.next, closing, l.now())
		if err != nil {
			return nil, 0, err
		}
		if ok && !ends[end.Create.ID] {
			ends[end.Create.ID] = true
			items = append(items, end)
		}
	}
	return items, n, nil
}

// applyCloses applies one close batch: ok when it landed, false when it must
// be re-planned.
func (l *ConversationLane) applyCloses(ctx context.Context, batch []beads.GuardedItem) (bool, error) {
	_, kind, err := l.store.apply(ctx, "opjournal: lane rules", batch)
	switch kind {
	case batchOK:
		return true, nil
	case batchInvalid:
		return false, fmt.Errorf("writing lane rule closes: %w", err)
	default:
		return false, nil
	}
}
