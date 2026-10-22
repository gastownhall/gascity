package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// The live executor's vocabulary (DES-O1 §7.2).
const (
	// laneViaLive is the via of a delivery typed into a live runtime.
	laneViaLive = "live"
	// laneViaSteer is the via of a steer typed into a running turn
	// (DES-O1 §10.4).
	laneViaSteer = "steer"
	// laneViaReplayed is the via of a redelivery the runtime already held:
	// nothing was sent now (OJ-7).
	laneViaReplayed = "replayed"
	// laneDeliveryKeyPrefix scopes a lane delivery key in the runtime's key
	// space. It is opjournal.RuntimeDeliveryKeyPrefix, so the key Deliver
	// hands the runtime is opjournal.RuntimeDeliveryKey of the journal key,
	// the one an API op's delivery carries.
	laneDeliveryKeyPrefix = opjournal.RuntimeDeliveryKeyPrefix
	// laneDegradationTurnStartUnobserved marks a delivery the runtime
	// accepted without seeing the turn it should start
	// (runtime.ErrNudgeDeliveredUnobserved).
	laneDegradationTurnStartUnobserved = "turn_start_unobserved"
	// laneContentBlockIncarnation blocks content while the live runtime is
	// not the incarnation the row names.
	laneContentBlockIncarnation = "incarnation"
	// lanePendingUnknown is Probe.Pending for a pending interaction the
	// runtime names no request id for.
	lanePendingUnknown = "?"
	// laneProbeBudget bounds one probe's runtime calls.
	laneProbeBudget = 10 * time.Second
)

// laneExecDeps is what the live executor needs, for any host that runs it:
// the controller's lane host, and later a single-conversation host (BX-3).
type laneExecDeps struct {
	cityPath string
	cfg      *config.City
	// sessions is the sessions-class store the executor resolves the
	// session's runtime through and stamps it in: the controller's cached
	// store, as the legacy nudge path reads it. The pass reads the view and
	// the op rows live; the executor's lookups need not.
	sessions beads.Store
	sp       runtime.Provider
	// lane is the op store; Probe reads the runtime row's host stamp from it.
	lane *opjournal.Store
	clk  clock.Clock
	// note records a degradation the executor fell back from, by stage:
	// the host's noteError. Nil notes nothing.
	note func(stage string, err error)
}

// conversationLaneExec is the live opjournal.Executor for one conversation
// over worker.Handle. A keystroke-mode runtime's boundary is the quiescence
// rule; a protocol-mode one (t3bridge) is live when it reports its turns,
// their starts included, and its boundary is CurrentTurn showing no turn in
// progress (DES-T3I §14 item 6). It is built per pass from the session row
// the pass's view came from, so Probe and Deliver act on the incarnation
// that view names.
type conversationLaneExec struct {
	deps   laneExecDeps
	info   session.Info
	target nudgeTarget
	// budget bounds one probe's runtime calls; laneProbeBudget in
	// production.
	budget time.Duration
	// observe, matches, handle and idle are the runtime seams; production
	// uses the cmd_nudge.go helpers. observe reads the live runtime the
	// target names unmasked, so matches can tell a stale incarnation from a
	// stopped one; handle resolves the handle of a running runtime that
	// matches.
	observe func(nudgeTarget) (worker.LiveObservation, error)
	matches func(nudgeTarget, worker.LiveObservation) (bool, error)
	handle  func(nudgeTarget) (worker.Handle, error)
	idle    func(nudgeTarget, worker.LiveObservation) bool
	// steerable says whether a session row takes a steer from a runtime
	// that steers in a mode; production reads session.SupportsSteerForInfo,
	// the source session responses read too.
	steerable func(session.Info, runtime.SteerMode) bool
	// live is the handle the last probe resolved; Deliver types through it.
	live worker.Handle
	// lastCaps and capsRead are the capabilities the first Probe read,
	// which later probes of the pass and its re-probes reuse instead of
	// reading the runtime row again.
	lastCaps opjournal.Caps
	capsRead bool
	// turns resolves the handle the executor reads the session's turns
	// through (conversation_lane_turn.go), resolveTurns in production;
	// sessionTurns, its seam, builds the session's own handle by its row's
	// bead id, which can read a transcript. turnHandle and traits are what
	// the first Probe resolved and read with the caps; protocol says the
	// runtime is protocol-mode.
	turns        func() (worker.TurnHandle, error)
	sessionTurns func(id string) (worker.TurnHandle, error)
	turnHandle   worker.TurnHandle
	traits       laneTurnTraits
	protocol     bool
	// lastTurn is the last probe's look at the session's turns, which
	// Deliver's link evidence starts from.
	lastTurn laneTurnLook
	// now and wait pace Deliver's link wait: the wall clock in production.
	now  func() time.Time
	wait func(ctx context.Context, d time.Duration) bool
}

var _ opjournal.Executor = (*conversationLaneExec)(nil)

// resolveTurns is the production turns: the session's own handle by its
// row's bead id, else, with no row id or a row no session handle is built
// from (noted), the runtime's. A session that is gone is gone either way.
func (e *conversationLaneExec) resolveTurns() (worker.TurnHandle, error) {
	if e.info.ID != "" {
		h, err := e.sessionTurns(e.info.ID)
		if err == nil || errors.Is(err, session.ErrSessionNotFound) || errors.Is(err, runtime.ErrSessionNotFound) {
			return h, err
		}
		// A row the session handle cannot be built from still has its
		// runtime's own turns, if it reports them; its op records the
		// link's basis either way.
		e.note("turns", fmt.Errorf("building session %s's handle for its turns; reading the runtime's: %w", e.info.ID, err))
	}
	return e.handle(e.target)
}

// note records a degradation through the host's note, if it has one.
func (e *conversationLaneExec) note(stage string, err error) {
	if e.deps.note != nil {
		e.deps.note(stage, err)
	}
}

// newConversationLaneExec returns the live executor for info's conversation.
func newConversationLaneExec(deps laneExecDeps, info session.Info) *conversationLaneExec {
	e := &conversationLaneExec{deps: deps, info: info, target: resolveNudgeTargetFromSessionInfo(deps.cityPath, deps.cfg, info), budget: laneProbeBudget}
	e.observe = func(t nudgeTarget) (worker.LiveObservation, error) {
		switch {
		case e.info.ID != "":
			// By the row's bead id, which the controller's cache holds, so
			// a busy re-probe reads no row from the backing store. The
			// session name is no bead id: resolving it as one misses the
			// cache.
			return workerObserveSessionTargetWithConfig(t.cityPath, e.deps.sessions, e.deps.sp, t.cfg, e.info.ID)
		case t.sessionName == "":
			return workerObserveNudgeTarget(t, e.deps.sessions, e.deps.sp)
		default:
			return workerObserveSessionTargetWithConfig(t.cityPath, e.deps.sessions, e.deps.sp, t.cfg, t.sessionName)
		}
	}
	e.matches = func(t nudgeTarget, obs worker.LiveObservation) (bool, error) {
		return nudgeTargetLiveGenerationMatches(t, obs, e.deps.sp)
	}
	e.handle = func(t nudgeTarget) (worker.Handle, error) {
		if t.sessionName == "" {
			return workerHandleForNudgeTarget(t, e.deps.sessions, e.deps.sp)
		}
		// The probe observed this runtime running and matched its
		// generation, so the handle is the runtime's, as
		// workerHandleForNudgeTarget resolves a running, matching target.
		return runtimeWorkerHandleWithConfig(t.cityPath, e.deps.sessions, e.deps.sp, t.cfg, t.sessionName,
			strings.TrimSpace(t.providerFamily()), strings.TrimSpace(t.sessionTransport()), t.processNames())
	}
	e.idle = func(t nudgeTarget, obs worker.LiveObservation) bool {
		return pollerSessionIdleEnough(t, e.deps.sp, defaultNudgePollQuiescence, obs)
	}
	e.steerable = session.SupportsSteerForInfo
	e.sessionTurns = func(id string) (worker.TurnHandle, error) {
		return workerHandleForSessionWithConfig(e.deps.cityPath, e.deps.sessions, e.deps.sp, e.deps.cfg, id)
	}
	e.turns = e.resolveTurns
	e.now, e.wait = time.Now, laneLinkWait
	return e
}

// Probe implements opjournal.Executor: the session's turn traits and the
// runtime's caps, read once per executor (one pass), then one bounded look
// at the runtime (laneLook), its turns included.
func (e *conversationLaneExec) Probe(ctx context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	if err := e.readCaps(ctx, conv); err != nil {
		return laneCapsProbe(conv, err)
	}
	return e.probe(ctx, conv, e.lastCaps, true)
}

// Reprobe is Probe for a host re-probing a busy runtime on its backoff
// (DES-O1 §4.3): it reuses the caps the pass's Probe read and observes the
// runtime by the row's bead id through the cached sessions store, so over
// the controller's cache it reads nothing from the backing store. It reads
// the session's turns only where they decide the boundary (protocol mode).
func (e *conversationLaneExec) Reprobe(ctx context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	if err := e.readCaps(ctx, conv); err != nil {
		return laneCapsProbe(conv, err)
	}
	return e.probe(ctx, conv, e.lastCaps, e.protocol)
}

// readCaps reads the runtime's interrupt mode, the session's turn traits
// and then the runtime's caps, once per executor; a read that failed is read
// again by the next probe.
func (e *conversationLaneExec) readCaps(ctx context.Context, conv opjournal.ConversationView) error {
	if e.capsRead {
		return nil
	}
	e.protocol = runtime.TurnInterruptModeFor(e.deps.sp, e.target.sessionName) == runtime.TurnInterruptProtocol
	if err := e.readTurnTraits(ctx); err != nil {
		return fmt.Errorf("reading session %s's turn capabilities: %w", e.info.ID, err)
	}
	caps, err := e.caps(conv)
	if err != nil {
		return err
	}
	e.lastCaps, e.capsRead = caps, true
	return nil
}

// laneUnreadCaps are the caps of a probe that could not read the runtime's:
// valid, so the pass plans from the probe, and not live.
var laneUnreadCaps = opjournal.Caps{Input: opjournal.FenceNone, Text: opjournal.TextReminder}

// laneCapsProbe is the probe of a readCaps that failed with err: unavailable
// when the runtime's turn capabilities were (errLaneTurnsUnavailable), as a
// look that was unavailable is, else err.
func laneCapsProbe(conv opjournal.ConversationView, err error) (opjournal.Probe, error) {
	if errors.Is(err, errLaneTurnsUnavailable) {
		return opjournal.Probe{Fence: laneFence(conv), TurnLive: opjournal.TriUnknown, Caps: laneUnreadCaps}, nil
	}
	return opjournal.Probe{}, err
}

// probe is one look at the runtime under caps, the whole look bounded by
// e.budget; withTurns reads the session's turns too.
func (e *conversationLaneExec) probe(ctx context.Context, conv opjournal.ConversationView, caps opjournal.Caps, withTurns bool) (opjournal.Probe, error) {
	p := opjournal.Probe{Available: true, Fence: laneFence(conv), TurnLive: opjournal.TriUnknown, Caps: caps}
	e.live, e.lastTurn = nil, laneTurnLook{live: opjournal.TriUnknown}
	octx, cancel := context.WithTimeout(ctx, e.budget)
	defer cancel()
	look, err := worker.ObserveBounded(octx, laneObservationKey(e.deps.cityPath, e.info.ID), func(lctx context.Context) (laneLook, error) {
		return e.look(lctx, caps, withTurns)
	})
	switch {
	case errors.Is(err, runtime.ErrRuntimeUnavailable):
		p.Available = false
		return p, nil
	case errors.Is(err, runtime.ErrSessionNotFound), errors.Is(err, session.ErrSessionNotFound):
		return p, nil
	case err != nil:
		return opjournal.Probe{}, fmt.Errorf("probing session %s: %w", e.info.ID, err)
	}
	p.Running, p.ContentBlock, p.Pending, p.Boundary = look.running, look.block, look.pending, look.boundary
	if look.block != "" && look.drainNoticeBlock == "" {
		p.ContentAdmit = []opjournal.ContentClass{laneDrainNoticeClass}
	}
	p.TurnLive, p.Turn, p.KnownTurns = look.turn.live, look.turn.turn, look.turn.known
	e.live, e.lastTurn = look.handle, look.turn
	return p, nil
}

// laneLook is one look at the runtime.
type laneLook struct {
	running bool
	// block is why content may not start, drainNoticeBlock why the drain
	// reminder's notice (laneDrainNoticeClass) may not: the one content a
	// draining row admits (session.GateContentDrainNotice).
	block            string
	drainNoticeBlock string
	pending          string
	boundary         opjournal.Tri
	handle           worker.Handle
	turn             laneTurnLook
}

// look observes the runtime, gates content on the row (session.ContentGate,
// for any content and for the drain reminder's notice) and on the live
// incarnation, and, only for a running runtime some content may reach,
// resolves its handle once and reads the pending interaction through it
// and, with withTurns, the session's turns. The boundary is the quiescence
// rule for a keystroke-mode runtime, and for a protocol-mode one its turns:
// never the quiescence rule.
//
// A draining row admits the drain notice, so its look resolves the handle
// and reads the pending interaction and the boundary whatever its open ops
// are: the probe does not see them, and a pass with only prompts open pays
// one handle and the reads that its content_block wait does not use.
// A pass runs only for a row with open ops, so an idle draining row pays
// nothing.
func (e *conversationLaneExec) look(ctx context.Context, caps opjournal.Caps, withTurns bool) (laneLook, error) {
	obs, err := e.observe(e.target)
	if err != nil {
		return laneLook{}, err
	}
	l := laneLook{running: obs.Running && obs.Alive, turn: laneTurnLook{live: opjournal.TriUnknown}}
	now := e.deps.clk.Now()
	l.block = session.ContentGate(e.info, l.running, now, session.GateContentAny)
	l.drainNoticeBlock = session.ContentGate(e.info, l.running, now, session.GateContentDrainNotice)
	if l.running && (l.block == "" || l.drainNoticeBlock == "") {
		matches, err := e.matches(e.target, obs)
		if err != nil {
			return laneLook{}, fmt.Errorf("matching session %s's live generation: %w", e.info.ID, err)
		}
		if !matches {
			l.block, l.drainNoticeBlock = laneContentBlockIncarnation, laneContentBlockIncarnation
		}
	}
	if !l.running || (l.block != "" && l.drainNoticeBlock != "") || !caps.Live {
		return l, nil
	}
	h, err := e.handle(e.target)
	if err != nil {
		return laneLook{}, fmt.Errorf("resolving session %s's handle: %w", e.info.ID, err)
	}
	l.handle = h
	if l.pending, err = lanePending(h.PendingStatus(ctx)); err != nil {
		return laneLook{}, fmt.Errorf("reading session %s's pending interaction: %w", e.info.ID, err)
	}
	if withTurns && e.traits.readable {
		l.turn = laneTurnLookOf(e.readTurns(ctx))
	}
	if e.protocol {
		l.boundary = l.turn.boundary()
		return l, nil
	}
	l.boundary = opjournal.TriNo
	if e.idle(e.target, obs) {
		l.boundary = opjournal.TriYes
	}
	return l, nil
}

// caps are the runtime's capabilities for conv. Content is live for a
// keystroke-mode runtime, and for a protocol-mode one whose turns, starts
// included, its own protocol reports (its boundary: a turn whose start is not
// recorded never reads as in progress), that the conversation's row does not
// stamp as hosted elsewhere for its current generation (DES-BX D7). Dedup is
// the worker boundary's read of whether the routed runtime deduplicates
// keyed deliveries. A live runtime steers where its steer mode and the
// session row allow it (e.steerable; D-STEER), per laneSteerCap. Nothing
// responds or injects in band here.
func (e *conversationLaneExec) caps(conv opjournal.ConversationView) (opjournal.Caps, error) {
	name := e.target.sessionName
	caps := opjournal.Caps{
		Live:     !e.protocol || e.traits.protocol && e.traits.starts,
		Dedup:    worker.DeliveryKeysDeduplicated(e.deps.sp, name),
		Input:    laneFenceMode(runtime.InputFencingFor(e.deps.sp, name)),
		Text:     opjournal.TextReminder,
		LinkWait: opjournal.DefaultLinkWait,
		LeaseTTL: opjournal.DefaultLeaseTTL,
	}
	if e.target.sessionTransport() == "acp" {
		caps.Text = opjournal.TextPlain
	}
	if caps.Live && e.deps.lane != nil {
		row, _, found, err := e.deps.lane.Runtime(conv.ID)
		if err != nil {
			return opjournal.Caps{}, fmt.Errorf("reading conversation %s's runtime row: %w", conv.ID, err)
		}
		if found && hostedElsewhere(row.Host, conv.Generation) {
			caps.Live = false
		}
	}
	caps.Steer = laneSteerCap(caps, e.steerable(e.info, runtime.SteerModeFor(e.deps.sp, name)))
	return caps, nil
}

// laneSteerCap is Caps.Steer for caps whose session row and runtime steer
// mode allow a steer (steerable): only a live runtime steers, and never one
// that deduplicates by delivery key (Caps.Dedup). To such a runtime a keyed
// immediate nudge goes out through its keyed send, a deduplicated turn start
// that is not its steer (runtime.NudgeWithReceipt); its steer, NudgeNow,
// takes no key (runtime.SteerProtocol). Steering it would either not steer
// or, unkeyed, apply a re-taken steer again while the plan, trusting Dedup,
// leaves out the D-NOTE repeat line.
func laneSteerCap(caps opjournal.Caps, steerable bool) bool {
	return caps.Live && !caps.Dedup && steerable
}

// laneTypesLive reports whether the live executor can type content into
// the runtime named name: a keystroke-mode runtime, not a protocol-mode one
// (runtime.TurnInterruptProtocol), whose turns no keystroke starts. The
// executor's Caps.Live also takes a protocol-mode runtime whose own protocol
// reports its turns, starts included, which only its turn-traits read tells.
func laneTypesLive(sp runtime.Provider, name string) bool {
	return runtime.TurnInterruptModeFor(sp, name) != runtime.TurnInterruptProtocol
}

// hostedElsewhere reports whether stamp says another host serves message for
// generation gen. A stamp for another generation is ignored.
func hostedElsewhere(stamp opjournal.HostStamp, gen int64) bool {
	return stamp.Host != "" && stamp.Generation == gen && slices.Contains(stamp.Kinds, opjournal.KindMessage)
}

// lanePending is the pending interaction's request id from a PendingStatus
// answer: "" for none, lanePendingUnknown for one with no request id, and the
// look's error when it failed, so a failed look is never read as a prompt.
func lanePending(p *worker.PendingInteraction, _ bool, err error) (string, error) {
	switch {
	case err != nil:
		return "", err
	case p == nil:
		return "", nil
	case p.RequestID == "":
		return lanePendingUnknown, nil
	default:
		return p.RequestID, nil
	}
}

// errLaneNoProbedHandle is a Deliver with no handle from its probe. The lane
// delivers only after a probe that found the runtime running, live and
// unblocked, which resolves the handle, so it is a bug; nothing is sent.
var errLaneNoProbedHandle = errors.New("conversation lane: delivery without a handle from its probe")

// Deliver implements opjournal.Executor: one fenced live-only nudge of the
// delivery's text, verbatim, under its delivery key, through the handle its
// probe resolved. A steer goes out with immediate delivery, into the running
// turn without waiting for idle (a keystroke runtime types it; a protocol
// runtime's immediate nudge is its steer, runtime.SteerProtocol), and is
// recorded via steer. A redelivery (d.Redelivery) asks a runtime that keeps
// keyed deliveries first, and one it already holds is replayed: nothing is
// sent, nothing is stamped and no turn is read. A delivery the runtime took
// now records the runtime's receipt and gathers its turn evidence within the
// link wait (linkEvidence).
func (e *conversationLaneExec) Deliver(ctx context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	h := e.live
	if h == nil {
		return opjournal.NotDelivered{Transient: true, Cause: errLaneNoProbedHandle}
	}
	fence := runtime.InputFence{}
	if d.Fence.Token != "" {
		fence = runtime.InputFence{Generation: d.Fence.Generation, Epoch: d.Fence.Epoch, Token: d.Fence.Token}
	}
	delivery := worker.NudgeDeliveryDefault
	if d.Steer {
		delivery = worker.NudgeDeliveryImmediate
	}
	at := e.deps.clk.Now()
	res, err := h.Nudge(ctx, sessionActor(session.ActorController, e.deps.cityPath), worker.NudgeRequest{
		Text:          d.Text,
		Delivery:      delivery,
		Wake:          worker.NudgeWakeLiveOnly,
		DeliveryKey:   laneDeliveryKeyPrefix + d.Key,
		Fence:         fence,
		AllowUnfenced: true,
		Redelivery:    d.Redelivery,
	})
	out := laneDeliveryResult(res, err, at, fence.IsZero(), d.Steer)
	if dv, ok := out.(opjournal.Delivered); ok {
		rec := dv.Record
		rec.Generation, rec.Epoch, rec.Mode = d.Fence.Generation, d.Fence.Epoch, d.Mode
		if res.Replayed {
			// Its turn predates the probe, so no read could link it.
			dv.Evidence = e.linkStart(rec)
			return dv
		}
		stampLastNudgeDeliveredAt(sessionFrontDoor(e.deps.sessions), e.info.ID, e.deps.clk.Now())
		dv.Evidence = e.linkEvidence(ctx, rec)
		out = dv
	}
	return out
}

// laneDeliveryResult maps one nudge's answer to the lane's result (DES-O1
// §7.2). unfencedReq says the request carried no fence, because the row
// names no token; steer says it was a steer, recorded via steer. A delivery
// records the runtime's receipt (its message id, stamp and turn); a replay
// of one the runtime already held is recorded via replayed. A replayed
// record's At is this redelivery's clock, not the held delivery's: unlike
// DES-O1 §4.1's delivered_at it is no lower bound on the turn that delivery
// started, which may have begun earlier. The held delivery's own time is its
// MessageAt, the stamp the runtime stored. A submit
// the runtime could not confirm may have delivered, so it is Unknown; a
// nudge refused on a pending interaction, whichever runtime refused it, one
// the runtime reports never sent, or one it deferred because the session was
// running a turn its submit would interrupt (runtime.ErrSessionBusy, nothing
// left typed), is a transient miss, which counts no failure. Any other error
// may have come after input reached the session, so it is Unknown too: a
// retake under a fresh key would deliver it twice.
func laneDeliveryResult(res worker.NudgeResult, err error, at time.Time, unfencedReq, steer bool) opjournal.DeliveryResult {
	via := laneViaLive
	switch {
	case res.Replayed:
		via = laneViaReplayed
	case steer:
		via = laneViaSteer
	}
	delivered := func(ack opjournal.AckKind, degradation string) opjournal.Delivered {
		if res.Unfenced || unfencedReq {
			degradation = opjournal.DegradationUnfencedDelivery
		}
		r := res.Receipt
		return opjournal.Delivered{
			Record:      opjournal.DeliveryRecord{At: at, Ack: ack, MessageID: r.MessageID, MessageAt: r.MessageAt, TurnID: r.TurnID},
			Evidence:    opjournal.TurnEvidence{HasSource: false},
			Via:         via,
			Degradation: degradation,
		}
	}
	var miss *runtime.FenceMismatchError
	switch {
	case err == nil && res.Delivered:
		return delivered(opjournal.AckDelivered, "")
	case err == nil:
		return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("runtime did not take the delivery (%s)", res.Undelivered)}
	case errors.Is(err, runtime.ErrNudgeDeliveredUnobserved):
		return delivered(opjournal.AckDeliveredUnobserved, laneDegradationTurnStartUnobserved)
	case errors.Is(err, runtime.ErrNudgeRejected):
		return opjournal.Rejected{KeyRejected: true, Reason: err.Error()}
	case errors.Is(err, runtime.ErrSessionBusy):
		return opjournal.NotDelivered{Transient: true, Cause: err}
	case errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed), errors.As(err, &miss) && miss.Partial:
		return opjournal.Unknown{Cause: err}
	case errors.As(err, &miss):
		return opjournal.NotDelivered{Transient: true, Cause: err}
	case errors.Is(err, runtime.ErrPendingInteraction), errors.Is(err, runtime.ErrNudgeRefusedPendingInteraction),
		errors.Is(err, runtime.ErrRuntimeUnavailable), errors.Is(err, runtime.ErrNudgeNotSent):
		return opjournal.NotDelivered{Transient: true, Cause: err}
	default:
		return opjournal.Unknown{Cause: err}
	}
}

// Interrupt implements opjournal.Executor: unsupported until OJ-15.
func (e *conversationLaneExec) Interrupt(context.Context, opjournal.InterruptCall) opjournal.ControlResult {
	return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
}

// Respond implements opjournal.Executor: unsupported until OJ-16.
func (e *conversationLaneExec) Respond(context.Context, opjournal.RespondCall) opjournal.ControlResult {
	return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
}

// laneFence is the fence of view conv.
func laneFence(conv opjournal.ConversationView) opjournal.Fence {
	return opjournal.Fence{Conversation: conv.ID, Generation: conv.Generation, Epoch: conv.Epoch, Token: conv.Token}
}

// laneFenceMode is the lane's name for a runtime's input fencing.
func laneFenceMode(f runtime.InputFencing) opjournal.FenceMode {
	switch f {
	case runtime.InputFencingAtomic:
		return opjournal.FenceAtomic
	case runtime.InputFencingChecked:
		return opjournal.FenceChecked
	default:
		return opjournal.FenceNone
	}
}

// laneObservationKey keys the executor's bounded observations: one session
// in one city, as postStartObservationKey does, under its own namespace so a
// wedged lane probe never blocks a lifecycle observation.
func laneObservationKey(cityPath, sessionID string) string {
	return "lane\x00" + cityPath + "\x00" + sessionID
}
