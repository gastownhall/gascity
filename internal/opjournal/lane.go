package opjournal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// Conversation lane limits and timings (DES-O1 §4).
const (
	// MaxBatchOps caps one content Take: the Take's runtime-row item is the
	// guarded batch's last allowed item.
	MaxBatchOps = beads.MaxGuardedItems - 1
	// MaxBatchPayload caps the payload bytes of one content Take.
	MaxBatchPayload = 64 << 10
	// MaxFailures is the poison threshold: an op that fails this many
	// attempts closes failed{poison}.
	MaxFailures = 5
	// DefaultLeaseTTL is a Take's lease when the executor declares none.
	DefaultLeaseTTL = 2 * time.Minute
	// MinLeaseTTL is the shortest lease an executor may declare.
	MinLeaseTTL = 10 * time.Second
	// LeaseSkew is the allowance a lane that does not hold a lease adds
	// before it calls the lease expired.
	LeaseSkew = 10 * time.Second
	// ExecutorMargin is how long before its lease ends an effect's context
	// ends.
	ExecutorMargin = 5 * time.Second
	// ExecutorErrorBackoff gates an op released after an executor error.
	ExecutorErrorBackoff = 15 * time.Second
	// TransientBackoffBase and TransientBackoffMax bound the backoff of an
	// op released for a transient cause: base·2^(k-1), capped at max.
	TransientBackoffBase = 2 * time.Second
	TransientBackoffMax  = 5 * time.Minute
	// DefaultLinkWait is how long an executor gathers turn evidence after a
	// delivery when its caps declare none.
	DefaultLinkWait = 3 * time.Second
	// DegradationUnfencedDelivery marks a delivery made without an
	// incarnation fence.
	DegradationUnfencedDelivery = "unfenced_delivery"
	// DegradationExecutorContract marks an effect the runtime acked whose
	// result broke the executor contract in a recording field; the lane
	// recorded it with that field clamped or dropped.
	DegradationExecutorContract = "executor_contract"
)

// Lane-only internal timings.
const (
	doneMinBudget     = 5 * time.Second
	doneMaxBudget     = 30 * time.Second
	doneBudgetSlack   = time.Second
	doneBackoffStart  = 100 * time.Millisecond
	doneBackoffMax    = 2 * time.Second
	laneLeaseNonceLen = 6  // bytes; 12 hex characters
	maxLaneHostLen    = 16 // laneHostRe
	// maxLaneLeaseOverhead is the most a lane lease adds around its
	// conversation id: the lane, the largest generation, the longest host
	// and nonce, and a deadline with all nine fractional digits.
	maxLaneLeaseOverhead = len(LaneConversation+"@#9223372036854775807#") + maxLaneHostLen +
		len(".") + 2*laneLeaseNonceLen + len("@2006-01-02T15:04:05.999999999Z")
)

// FenceMode is how a runtime fences input to an incarnation (DES-O1 §6).
type FenceMode string

// Fence modes.
const (
	FenceAtomic  FenceMode = "atomic"
	FenceChecked FenceMode = "checked"
	FenceNone    FenceMode = "none"
)

// Fence is the incarnation an effect must land in. Token is never written to
// any row.
type Fence struct {
	Conversation string
	Generation   int64
	Epoch        int64
	Token        string
}

// ConversationView is the conversation state a pass needs, built by the host
// from the session row (opjournal never imports internal/session).
type ConversationView struct {
	ID     string
	Closed bool
	// Generation is the current session generation; 0 means no session yet.
	Generation int64
	Epoch      int64
	// Token is the incarnation token effects fence on; "" means nothing to
	// fence on, so deliveries record DegradationUnfencedDelivery.
	Token string
	// Dormant is an asleep or drained conversation: never probed.
	Dormant bool
	// ContentHold holds normal-precedence content (D21).
	ContentHold bool
}

// Caps are a runtime's capabilities, static per runtime and provider family
// for one generation.
type Caps struct {
	// Live can start a turn from this process.
	Live bool
	// InBand injects into a starting turn (the hook).
	InBand bool
	Steer  bool
	// Respond answers pending interactions (the runtime is an
	// InteractionProvider). Without it a respond closes failed{unsupported}
	// (a respond is at most once, so none waiting may have answered).
	Respond bool
	// Dedup deduplicates deliveries by delivery key: a send under a key the
	// runtime already applied is answered Delivered and not applied again,
	// never KeyRejected (see Rejected).
	Dedup bool
	// Input is the incarnation fence for input (DES-O1 §6.3's
	// Caps.Fencing).
	Input FenceMode
	// Text is the notice block style.
	Text TextStyle
	// LinkWait is how long the executor gathers turn evidence after a
	// delivery.
	LinkWait time.Duration
	// LeaseTTL is a Take's lease; 0 means DefaultLeaseTTL, and a value below
	// MinLeaseTTL is refused.
	LeaseTTL time.Duration
	// MaxText, when positive, caps the bytes of one content Take's text
	// (Delivery.Text), for a runtime that keeps only so much text whole (a
	// hook's context, which its provider cuts to a preview past a cap): the
	// batch takes ops only while their text fits, and a notice whose text
	// alone does not waits text_too_long, holding the content behind it, for
	// a runtime with room. 0 means no cap; a negative value is refused.
	MaxText int
	// FusedNow runs a deliver-now unit's replacement and message as one
	// runtime command (T3 protocol 2's restart_active, DES-O1 §10.3); the
	// executor must implement FusedNowExecutor.
	FusedNow bool
}

// leaseTTL is the lease a Take under c holds.
func (c Caps) leaseTTL() time.Duration {
	if c.LeaseTTL == 0 {
		return DefaultLeaseTTL
	}
	return c.LeaseTTL
}

func (c Caps) validate() error {
	switch c.Input {
	case FenceAtomic, FenceChecked, FenceNone:
	default:
		return fmt.Errorf("probe caps: unknown input fence %q", c.Input)
	}
	if c.Text != TextReminder && c.Text != TextPlain {
		return fmt.Errorf("probe caps: unknown text style %q", c.Text)
	}
	if c.LeaseTTL != 0 && c.LeaseTTL < MinLeaseTTL {
		return fmt.Errorf("probe caps: lease ttl %s is below the minimum %s", c.LeaseTTL, MinLeaseTTL)
	}
	if c.LinkWait < 0 {
		return fmt.Errorf("probe caps: link wait %s is negative", c.LinkWait)
	}
	if c.MaxText < 0 {
		return fmt.Errorf("probe caps: max text %d is negative", c.MaxText)
	}
	return nil
}

// Probe is the executor's live look at a conversation, taken only when a
// Take is possible.
type Probe struct {
	// Available false: the runtime is unreachable; nothing is taken.
	Available bool
	Running   bool
	// Boundary is whether the runtime is idle at input.
	Boundary Tri
	// Turn is the turn running at the probe, "" when unknown.
	Turn     string
	TurnLive Tri
	// KnownTurns are the turn ids the probe's read returned.
	KnownTurns []string
	// Pending is a pending interaction's request id; PendingUnknown for one
	// the runtime names no id for. A failed look is a Probe error, not
	// Pending. A pass that sees a request id records its raising
	// (Store.RecordRaising).
	Pending string
	// ContentBlock is why content may not start now (I7), or "".
	ContentBlock string
	// ContentAdmit names the content ContentBlock does not hold: a host's
	// narrow exemption from its own block, which the host has already
	// gated by every other reason. Empty admits nothing; it means nothing
	// without a ContentBlock. An admitted op never asks for a wake: the
	// block, not the op, decides whether the row may start.
	ContentAdmit []ContentClass
	// Fence is the incarnation the executor will guard on. A non-zero fence
	// must match the view's generation and epoch.
	Fence Fence
	Caps  Caps
}

// ContentClass is a class of content ops: those of role Role and when
// When from principal Principal. An op is of the class only when all three
// match.
type ContentClass struct {
	Role      Role
	When      When
	Principal string
}

// admits reports whether op is of a class p.ContentAdmit names.
func (p Probe) admits(op Op) bool {
	m := op.Policy.Message
	return slices.ContainsFunc(p.ContentAdmit, func(c ContentClass) bool {
		return c.Role == m.Role && c.When == m.When && c.Principal == op.Producer.Principal
	})
}

// Executor runs a conversation's effects. Hosts implement it over their
// runtime; the lane calls it only between a durable Take and its Done.
type Executor interface {
	Probe(ctx context.Context, conv ConversationView) (Probe, error)
	Deliver(ctx context.Context, d Delivery) DeliveryResult
	Interrupt(ctx context.Context, c InterruptCall) ControlResult
	Respond(ctx context.Context, c RespondCall) ControlResult
}

// Delivery is one content effect.
type Delivery struct {
	Fence Fence
	// Key is the runtime's idempotency key, <head op id>:<n>. It is never
	// in Text.
	Key string
	// Seq is the lane_seq of the head op.
	Seq  uint64
	Mode LinkMode
	// Steer delivers into the running turn.
	Steer bool
	// Ops are the ops as taken, in delivery order.
	Ops []Op
	// Redelivery says some op may already have reached the agent.
	Redelivery bool
	// Replacement says the delivery follows its deliver-now unit's
	// interrupt step at the boundary that step reached: the executor sends
	// it at once, as interrupt_now sends its message once the interrupt
	// settled.
	Replacement bool
	// Text is RenderDelivery(Ops, Caps.Text, Redelivery && !Caps.Dedup).
	Text string
}

// InterruptCall is one interrupt effect. It carries no delivery key: Op.ID
// is the idempotency key, the same on every re-take of the op.
type InterruptCall struct {
	Fence Fence
	Op    Op
	// Turn is the turn the op names, "" for the current turn.
	Turn string
	// Seq is this attempt's lane_seq; every Take assigns a new one.
	Seq uint64
	// ForReplacement marks a deliver-now unit's interrupt step (X11 D11):
	// once the turn settles the executor prepares the session for the
	// message the unit then delivers, and reports in Settled.After whether
	// the message may go now.
	ForReplacement bool
}

// RespondCall is one respond effect. It carries no delivery key: Op.ID is
// the idempotency key, the same on every re-take of the op. The lane calls
// it only when the probe showed RequestID pending and Interaction, the
// occurrence the op answers, is its latest raising, still unresolved; the
// executor answers only that request (RequestID is always set), and reports
// Rejected skipped{stale} when the runtime shows another or none, or when
// Interaction has ended since (Store.Raising): an identical prompt raised
// later shares the request id. A respond
// is at most once: after an Unknown result, or a lease that expired over its
// attempt, it closes failed{ambiguous_delivery} and is never called again.
type RespondCall struct {
	Fence Fence
	Op    Op
	// Interaction is the occurrence the op answers, <request id>#<n>.
	Interaction string
	// RequestID is the runtime request id of the occurrence.
	RequestID string
	// Response is the op's decoded payload.
	Response RespondPayload
	// Seq is this attempt's lane_seq; every Take assigns a new one.
	Seq uint64
}

// DeliveryResult is a content effect's result: Delivered, NotDelivered,
// Rejected or Unknown. A nil result is handled as Unknown and reported.
type DeliveryResult interface{ isDeliveryResult() }

// ControlResult is a control effect's result: Settled, NotDelivered,
// Rejected or Unknown. A nil result is handled as Unknown and reported.
type ControlResult interface{ isControlResult() }

// Delivered is a content delivery the runtime accepted. The executor supplies
// Record.At (required, not before the call), Ack, MessageID, MessageAt and
// TurnID, with both times in years 0 through 9999; the lane sets Seq,
// Generation, Epoch and Mode. Evidence comes from
// the executor except LiveAtProbe, LiveTurn and Known, which the lane sets
// from its own probe.
type Delivered struct {
	Record      DeliveryRecord
	Evidence    TurnEvidence
	Via         string
	Degradation string
	// SettleBy, when set, is the last instant on the lane's clock the lane
	// may record the delivery, set by an executor whose delivery is kept only
	// if its process outlives the record (a hook's output, which its runner
	// discards on a timeout). The Done's context ends at SettleBy: a Done that
	// has not landed by then leaves the batch executing, and a pass takes it
	// again, as maybe delivered, once its lease lapses.
	SettleBy time.Time
}

// Settled is a control effect that took place.
type Settled struct {
	Turn        string
	Via         string
	Degradation string
	// After is the session's turns as a replacement step left them
	// (InterruptCall.ForReplacement); zero, Boundary unknown, otherwise.
	After TurnAfter
}

// NotDelivered says the effect definitely did not happen. Transient causes
// back off without counting a failure.
type NotDelivered struct {
	Transient bool
	// Unavailable, on a transient release, says the provider could not be
	// reached, or could not answer whether the effect could run: the release
	// is recorded as such, and an interrupt whose start_by passes on it
	// closes failed{provider_unavailable} (X11 §5.4). Any other transient
	// release is recorded as transient.
	Unavailable bool
	Cause       error
	// SettleBy, when set, is the last instant on the lane's clock the lane
	// may record the release, set by an executor whose process ends then (a
	// hook its runner kills at a deadline). As for Delivered, the Done's
	// context ends at SettleBy: a release that has not landed by then leaves
	// the batch executing, and a pass takes it again, as maybe delivered, once
	// its lease lapses.
	SettleBy time.Time
}

// Rejected is a definitive answer: the op closes Arm{Reason} (Arm is failed
// or skipped). KeyRejected instead says the runtime refused the delivery key;
// the op is released, a failure counted, and its next delivery uses a new
// key. A runtime refuses only a key that never landed; one that already
// applied a send under the key answers Delivered. The send under the new key
// then repeats nothing, which matters on a deduplicating runtime: its
// redeliveries carry no RedeliveryNote.
type Rejected struct {
	Arm         Arm
	Reason      string
	KeyRejected bool
	// Turn, on a control op's rejection, is the turn its effect ran on,
	// recorded as the outcome's Result.Turn: an interrupt whose turn was
	// not seen to end (failed{no_boundary}) names the turn it sent into.
	Turn string
}

// Unknown says the effect may have happened.
type Unknown struct {
	Cause error
}

func (Delivered) isDeliveryResult()    {}
func (NotDelivered) isDeliveryResult() {}
func (Rejected) isDeliveryResult()     {}
func (Unknown) isDeliveryResult()      {}
func (Settled) isControlResult()       {}
func (NotDelivered) isControlResult()  {}
func (Rejected) isControlResult()      {}
func (Unknown) isControlResult()       {}

// TakeKind says what a pass took.
type TakeKind string

// Take kinds.
const (
	TakeNone    TakeKind = ""
	TakeContent TakeKind = "content"
	TakeControl TakeKind = "control"
	// TakeNow is a deliver-now unit: its derived interrupt and its message
	// under one lease (DES-O1 §10.3).
	TakeNow TakeKind = "now"
)

// DoneArm says how a pass's Done ended.
type DoneArm string

// Done arms.
const (
	DoneAccepted   DoneArm = "accepted"
	DoneReleased   DoneArm = "released"
	DoneRejected   DoneArm = "rejected"
	DoneSuperseded DoneArm = "superseded"
)

// WaitReason says why an open op was not taken.
type WaitReason string

// Wait reasons.
const (
	WaitInFlight        WaitReason = "in_flight"
	WaitScopeFuture     WaitReason = "scope_future"
	WaitAfterPending    WaitReason = "after_pending"
	WaitAfterUnreadable WaitReason = "after_unreadable"
	WaitNotBefore       WaitReason = "not_before"
	WaitRetryBackoff    WaitReason = "retry_backoff"
	// WaitNowUnreadable is a deliver-now message, or its derived
	// interrupt, whose partner row could not be read: on a timer when the
	// read failed, with none when the row will not decode on a retry either
	// (a newer build's row, which a newer host may serve).
	WaitNowUnreadable      WaitReason = "now_unreadable"
	WaitContentHold        WaitReason = "content_hold"
	WaitContentBlock       WaitReason = "content_block"
	WaitPendingInteraction WaitReason = "pending_interaction"
	WaitBusy               WaitReason = "busy"
	WaitBoundaryUnknown    WaitReason = "boundary_unknown"
	WaitAfterTurn          WaitReason = "after_turn"
	WaitDormant            WaitReason = "dormant"
	WaitNotRunning         WaitReason = "not_running"
	WaitNoSession          WaitReason = "no_session"
	WaitUnavailable        WaitReason = "unavailable"
	WaitNotLive            WaitReason = "not_live"
	WaitIncarnation        WaitReason = "incarnation"
	WaitSettling           WaitReason = "settling"
	WaitBlocked            WaitReason = "blocked"
	WaitBatchFull          WaitReason = "batch_full"
	// WaitTextTooLong is a notice whose text alone is over the runtime's
	// Caps.MaxText: it waits for a runtime with room and, as a blocking
	// waiter, holds the content behind it.
	WaitTextTooLong WaitReason = "text_too_long"
	// WaitInteractionUnknown is a respond whose occurrence could not be read,
	// or whose runtime could not be looked at: it retries on a timer.
	WaitInteractionUnknown WaitReason = "interaction_unknown"
	// WaitReplayGroup is the head of a maybe-delivered group that a
	// deduplicating runtime must get whole: another open member of the group
	// is not eligible yet, and a part sent under the group's key would make
	// the runtime drop the rest.
	WaitReplayGroup WaitReason = "replay_group"
)

// ClosedOp is an op a pass closed.
type ClosedOp struct {
	ID     string
	Arm    Arm
	Reason string
}

// OpWait is an open op a pass left waiting.
type OpWait struct {
	ID     string
	Reason WaitReason
	Detail string
	// Until is when the wait can end by itself, zero when nothing but a
	// change in state ends it.
	Until time.Time
}

// PassResult reports one pass.
type PassResult struct {
	Took TakeKind
	// Taken are the taken op ids in delivery order.
	Taken []string
	// Done is how the Take ended; "" when nothing was taken, or when Done
	// could not settle it (the pass then returns the error).
	Done DoneArm
	// Closed are the ops this pass closed: its Done's closes and the rule
	// closes it saw land. A rule close batch that ended with an unknown
	// error goes unreported even when it committed, since the re-read no
	// longer sees the ops it closed.
	Closed      []ClosedOp
	Waits       []OpWait
	Quarantined []QuarantinedRow
	// Lowered are the deliver-now messages this pass lowered.
	Lowered []string
	// Wake: an if_needed content op met a dormant, sessionless or
	// not-running runtime.
	Wake bool
	// Busy: content waits only for a boundary; the host re-probes on its
	// backoff.
	Busy bool
	// More: something else is eligible now; pass again at once.
	More bool
	// Next is the earliest time a waiting op can change by itself, always
	// after the instant the pass planned at; zero means none.
	Next time.Time
}

// laneStage names the points the test hooks run at.
type laneStage string

const (
	stageAfterRead   laneStage = "after_read"
	stageAfterTake   laneStage = "after_take"
	stageAfterEffect laneStage = "after_effect"
)

// laneHooks are test seams. A hook that returns an error ends the pass with
// it at once, as a crash would.
type laneHooks struct {
	at func(laneStage) error
}

func (h laneHooks) run(stage laneStage) error {
	if h.at == nil {
		return nil
	}
	return h.at(stage)
}

// ConversationLane is the conversation lane's core (Lane names the op lanes
// themselves): one Pass reads a conversation's open
// ops, closes what its rules decide, and takes, executes and settles at most
// one batch. It holds no state between passes.
type ConversationLane struct {
	store *Store
	host  string
	clk   clock.Clock
	nonce func() (string, error)
	sleep func(context.Context, time.Duration) error
	hooks laneHooks
}

var laneHostRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// NewLane returns a lane over store, a store opened with NewLaneStore. host
// names this process in lease nonces and transitions (ctl, hook, cli, box…).
func NewLane(store *Store, host string, clk clock.Clock) (*ConversationLane, error) {
	if store == nil || store.batcher == nil {
		return nil, fmt.Errorf("opening lane: %w: open the store with NewLaneStore", beads.ErrGuardedBatchUnsupported)
	}
	if !laneHostRe.MatchString(host) {
		return nil, fmt.Errorf("opening lane: host %q is not ^[a-z][a-z0-9-]{0,15}$", host)
	}
	if clk == nil {
		return nil, errors.New("opening lane: clock is required")
	}
	return &ConversationLane{store: store, host: host, clk: clk, nonce: randomNonce, sleep: sleepCtx}, nil
}

func randomNonce() (string, error) {
	var b [laneLeaseNonceLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("minting a lease nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

// by is the transition author of the lane's rule writes.
func (l *ConversationLane) by() string { return "lane:" + l.host }

// Pass runs one pass over conversation conv: read, close what the rules
// decide, and, when a Take is possible, probe, take one batch, run its effect
// and settle it. Hosts call it whenever something may have changed and again
// at once when the result says More, else by Next.
func (l *ConversationLane) Pass(ctx context.Context, conv ConversationView, exec Executor) (PassResult, error) {
	var res PassResult
	if err := validateConversation(conv.ID); err != nil {
		return res, err
	}
	if conv.Generation < 0 || conv.Epoch < 0 {
		return res, invalidf("conversation %s view generation and epoch must be >= 0", conv.ID)
	}
	if exec == nil {
		return res, errors.New("lane pass: executor is required")
	}
	for range casAttempts {
		// A write that ended unknown under a canceled context re-plans;
		// the context's error, not contention, is why the pass ends.
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("lane pass over conversation %s: %w", conv.ID, err)
		}
		snap, err := l.read(conv)
		if err != nil {
			return res, err
		}
		res.Quarantined = snap.quarantined
		if err := l.hooks.run(stageAfterRead); err != nil {
			return res, err
		}
		now := l.now()
		rp := planRules(snap, conv, now, l.by())
		l.fillWaits(&res, rp.waits, rp.wake, rp.next)
		switch done, err := l.writeCloses(ctx, rp.closes, rp.closeRow, snap.rt, &res); {
		case err != nil:
			return res, err
		case !done:
			continue
		}
		if rp.rowErr != nil {
			return res, rp.rowErr
		}
		// A lowering re-reads before any Take, so no content goes ahead of
		// the unit it formed.
		switch done, err := l.writeLowerings(ctx, rp.lowerings, &res); {
		case err != nil:
			return res, err
		case !done, len(rp.lowerings) > 0:
			continue
		}
		if !rp.takePossible {
			return res, nil
		}
		// Every attempt probes: a probe answers for the state its attempt
		// read, and an attempt after a lost race read a moved state.
		probe, err := exec.Probe(ctx, conv)
		if err == nil {
			err = probe.Caps.validate()
		}
		if err == nil && probe.Caps.FusedNow {
			if _, ok := exec.(FusedNowExecutor); !ok {
				err = errors.New("probe caps: fused_now without DeliverNow")
			}
		}
		if err != nil {
			return res, fmt.Errorf("probing conversation %s: %w", conv.ID, err)
		}
		var moved bool
		switch probe, rp.raisings, moved, err = l.noteRaisings(ctx, &snap, rp, probe, conv); {
		case err != nil:
			return res, err
		case moved:
			continue
		}
		tp, err := planTake(rp, probe, conv, now)
		if err != nil {
			return res, err
		}
		l.fillWaits(&res, append(rp.waits, tp.waits...), rp.wake || tp.wake, minTime(rp.next, tp.next))
		res.Busy, res.More = tp.busy, tp.more
		switch done, err := l.writeCloses(ctx, tp.closes, false, snap.rt, &res); {
		case err != nil:
			return res, err
		case !done:
			continue
		}
		if tp.take == nil {
			return res, nil
		}
		taken, outcome, err := l.take(ctx, snap, tp.take, conv)
		switch outcome {
		case takeLost:
			continue
		case takeFailed:
			return res, err
		}
		res.Took = tp.take.kind
		res.Taken = takenIDs(taken)
		if err := l.hooks.run(stageAfterTake); err != nil {
			return res, err
		}
		if tp.take.kind == TakeNow {
			return res, l.runNow(ctx, exec, taken, &res)
		}
		result, preCall := l.effect(ctx, exec, taken)
		if err := l.hooks.run(stageAfterEffect); err != nil {
			return res, err
		}
		return res, l.done(ctx, taken, result, preCall, &res)
	}
	return res, fmt.Errorf("%w: conversation %s lost %d Take races", ErrContention, conv.ID, casAttempts)
}

func (l *ConversationLane) now() time.Time { return utc(l.clk.Now()) }

// fillWaits replaces the result's waits and timers with a plan's, and clears
// Busy and More, which only planTake sets.
func (l *ConversationLane) fillWaits(res *PassResult, waits []OpWait, wake bool, next time.Time) {
	res.Waits = append([]OpWait(nil), waits...)
	res.Wake = wake
	res.Next = next
	res.Busy, res.More = false, false
}

// minTime is the earlier non-zero time.
func minTime(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero() || a.Before(b):
		return a
	default:
		return b
	}
}

// laneSnapshot is one read of a conversation.
type laneSnapshot struct {
	// ops are the open conversation-lane lane ops in CompareOrder order.
	ops []Op
	// openIDs holds every open row of the conversation, of any executor,
	// quarantined rows included.
	openIDs     map[string]bool
	quarantined []QuarantinedRow
	rt          runtimeSnap
	// after holds each after target that is not open.
	after map[string]afterTarget
	// prefix is the store's namespace prefix, which this lane's lowerings
	// pin derived interrupt ids in.
	prefix string
	// nowRows holds the deliver-now partner rows that are not open, read by
	// id (readNow).
	nowRows map[string]nowRow
}

// runtimeSnap is the runtime row as read.
type runtimeSnap struct {
	read  bool
	found bool
	bead  beads.Bead
	row   RuntimeRow
	err   error
}

type afterState int

const (
	afterOpen afterState = iota
	afterClosed
	afterMissing
	afterUnreadable
)

type afterTarget struct {
	state        afterState
	arm          Arm
	conversation string
	// err is why an afterUnreadable target could not be read; undecodable
	// is set when the row was read but will not decode on a retry either.
	err         error
	undecodable bool
}

// detail is the wait detail for an unreadable target: its read or decode
// error.
func (t afterTarget) detail() string {
	if t.err == nil {
		return ""
	}
	return t.err.Error()
}

// read takes one snapshot: the open ops, the runtime row when any lane op is
// open or the conversation closed, and each after target that is not open.
func (l *ConversationLane) read(conv ConversationView) (laneSnapshot, error) {
	ops, quarantined, err := l.store.ListOpenTolerant(conv.ID)
	if err != nil {
		return laneSnapshot{}, err
	}
	snap := laneSnapshot{openIDs: map[string]bool{}, quarantined: quarantined, after: map[string]afterTarget{}}
	for _, q := range quarantined {
		snap.openIDs[q.ID] = true
	}
	for _, op := range ops {
		snap.openIDs[op.ID] = true
		spec, err := Lookup(op.Kind)
		if err != nil || op.Executor != ExecutorLane || spec.Lane != LaneConversation {
			continue
		}
		snap.ops = append(snap.ops, op)
	}
	if len(snap.ops) > 0 || conv.Closed {
		b, found, err := l.store.runtimeBead(conv.ID)
		if err != nil {
			return laneSnapshot{}, err
		}
		snap.rt = runtimeSnap{read: true, found: found, bead: b}
		if found {
			snap.rt.row, snap.rt.err = DecodeRuntimeRow(b)
		}
	}
	for _, op := range snap.ops {
		if op.After == "" || snap.openIDs[op.After] {
			continue
		}
		if _, seen := snap.after[op.After]; seen {
			continue
		}
		snap.after[op.After] = l.readAfter(op.After)
	}
	l.readNow(&snap)
	return snap, nil
}

func (l *ConversationLane) readAfter(id string) afterTarget {
	b, err := l.store.store.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return afterTarget{state: afterMissing}
	}
	if err != nil {
		return afterTarget{state: afterUnreadable, err: err}
	}
	op, err := DecodeOp(b)
	if err != nil {
		undecodable := errors.Is(err, ErrUnsupportedSchema) || errors.Is(err, ErrUnsupportedValue) || errors.Is(err, ErrCorruptOp)
		return afterTarget{state: afterUnreadable, err: err, undecodable: undecodable}
	}
	if op.Status != StatusClosed {
		return afterTarget{state: afterOpen, conversation: op.Conversation}
	}
	return afterTarget{state: afterClosed, arm: op.Outcome.Arm, conversation: op.Conversation}
}
