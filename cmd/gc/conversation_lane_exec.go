package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// laneDegradationTurnStartUnobserved marks a delivery the runtime
	// accepted without seeing the turn it should start
	// (runtime.ErrNudgeDeliveredUnobserved).
	laneDegradationTurnStartUnobserved = "turn_start_unobserved"
	// laneContentBlockIncarnation blocks content while the live runtime is
	// not the incarnation the row names.
	laneContentBlockIncarnation = "incarnation"
	// lanePendingUnknown is Probe.Pending when the look failed.
	lanePendingUnknown = "?"
	// laneDeliveryKeyPrefix scopes a lane delivery key in the runtime's key
	// space (OJ-7's format).
	laneDeliveryKeyPrefix = "gc:"
	// laneProbeBudget bounds one probe's runtime observation.
	laneProbeBudget = 10 * time.Second
)

// laneExecDeps is what the live executor needs, for any host that runs it:
// the controller's lane host, and later a single-conversation host (BX-3).
type laneExecDeps struct {
	cityPath string
	cfg      *config.City
	// sessions is the sessions-class store the executor reads the session
	// through and stamps it in.
	sessions beads.Store
	sp       runtime.Provider
	// lane is the op store; Probe reads the runtime row's host stamp from it.
	lane *opjournal.Store
	clk  clock.Clock
}

// conversationLaneExec is the live opjournal.Executor for one conversation
// over worker.Handle, for keystroke-mode runtimes. It is built per pass from
// the session row the pass's view came from, so Probe and Deliver act on the
// incarnation that view names.
type conversationLaneExec struct {
	deps   laneExecDeps
	info   session.Info
	target nudgeTarget
	// observe, handle and idle are the runtime seams; production uses the
	// cmd_nudge.go helpers.
	observe func(nudgeTarget) (worker.LiveObservation, error)
	handle  func(nudgeTarget) (worker.Handle, error)
	idle    func(nudgeTarget, worker.LiveObservation) bool
}

var _ opjournal.Executor = (*conversationLaneExec)(nil)

// newConversationLaneExec returns the live executor for info's conversation.
func newConversationLaneExec(deps laneExecDeps, info session.Info) *conversationLaneExec {
	e := &conversationLaneExec{deps: deps, info: info, target: resolveNudgeTargetFromSessionInfo(deps.cityPath, deps.cfg, info)}
	e.observe = func(t nudgeTarget) (worker.LiveObservation, error) {
		return workerObserveNudgeTarget(t, e.deps.sessions, e.deps.sp)
	}
	e.handle = func(t nudgeTarget) (worker.Handle, error) {
		return workerHandleForNudgeTarget(t, e.deps.sessions, e.deps.sp)
	}
	e.idle = func(t nudgeTarget, obs worker.LiveObservation) bool {
		return pollerSessionIdleEnough(t, e.deps.sp, defaultNudgePollQuiescence, obs)
	}
	return e
}

// Probe implements opjournal.Executor: the live observation, bounded by ctx,
// the row's content gate, the pending interaction, the idle boundary, the
// view's fence and the runtime's caps.
func (e *conversationLaneExec) Probe(ctx context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	p := opjournal.Probe{Available: true, Fence: laneFence(conv), TurnLive: opjournal.TriUnknown}
	caps, err := e.caps(conv)
	if err != nil {
		return opjournal.Probe{}, err
	}
	p.Caps = caps
	octx, cancel := context.WithTimeout(ctx, laneProbeBudget)
	defer cancel()
	obs, err := worker.ObserveBounded(octx, laneObservationKey(e.deps.cityPath, e.info.ID), func(context.Context) (worker.LiveObservation, error) {
		return e.observe(e.target)
	})
	switch {
	case errors.Is(err, runtime.ErrRuntimeUnavailable):
		p.Available = false
		return p, nil
	case errors.Is(err, runtime.ErrSessionNotFound), errors.Is(err, session.ErrSessionNotFound):
		return p, nil
	case err != nil:
		return opjournal.Probe{}, fmt.Errorf("observing session %s: %w", e.info.ID, err)
	}
	p.Running = obs.Running && obs.Alive
	p.ContentBlock = session.ContentGate(e.info, p.Running, e.deps.clk.Now())
	if p.ContentBlock == "" {
		matches, err := nudgeTargetLiveGenerationMatches(e.target, obs, e.deps.sp)
		if err != nil {
			return opjournal.Probe{}, fmt.Errorf("matching session %s's live generation: %w", e.info.ID, err)
		}
		if !matches {
			p.ContentBlock = laneContentBlockIncarnation
		}
	}
	if !p.Running {
		return p, nil
	}
	p.Pending = e.pending(ctx)
	p.Boundary = opjournal.TriNo
	if e.idle(e.target, obs) {
		p.Boundary = opjournal.TriYes
	}
	return p, nil
}

// caps are the runtime's capabilities for conv. Content is live only for a
// keystroke-mode runtime the conversation's row does not stamp as hosted
// elsewhere for its current generation (DES-BX D7). Nothing deduplicates,
// steers, responds or injects in band here.
func (e *conversationLaneExec) caps(conv opjournal.ConversationView) (opjournal.Caps, error) {
	name := e.target.sessionName
	caps := opjournal.Caps{
		Live:     runtime.TurnInterruptModeFor(e.deps.sp, name) != runtime.TurnInterruptProtocol,
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
	return caps, nil
}

// hostedElsewhere reports whether stamp says another host serves message for
// generation gen. A stamp for another generation is ignored.
func hostedElsewhere(stamp opjournal.HostStamp, gen int64) bool {
	return stamp.Host != "" && stamp.Generation == gen && slices.Contains(stamp.Kinds, opjournal.KindMessage)
}

// pending is the pending interaction's request id, "" for none and
// lanePendingUnknown when the look failed.
func (e *conversationLaneExec) pending(ctx context.Context) string {
	h, err := e.handle(e.target)
	if err != nil {
		return lanePendingUnknown
	}
	p, _, err := h.PendingStatus(ctx)
	switch {
	case err != nil:
		return lanePendingUnknown
	case p == nil:
		return ""
	case p.RequestID == "":
		return lanePendingUnknown
	default:
		return p.RequestID
	}
}

// Deliver implements opjournal.Executor: one fenced live-only nudge of the
// delivery's text, verbatim, under its delivery key.
func (e *conversationLaneExec) Deliver(ctx context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	h, err := e.handle(e.target)
	if err != nil {
		return laneHandleError(err)
	}
	fence := runtime.InputFence{}
	if d.Fence.Token != "" {
		fence = runtime.InputFence{Generation: d.Fence.Generation, Epoch: d.Fence.Epoch, Token: d.Fence.Token}
	}
	at := e.deps.clk.Now()
	res, err := h.Nudge(ctx, sessionActor(session.ActorController, e.deps.cityPath), worker.NudgeRequest{
		Text:          d.Text,
		Delivery:      worker.NudgeDeliveryDefault,
		Wake:          worker.NudgeWakeLiveOnly,
		DeliveryKey:   laneDeliveryKeyPrefix + d.Key,
		Fence:         fence,
		AllowUnfenced: true,
	})
	out := laneDeliveryResult(ctx, res, err, at, fence.IsZero())
	if _, ok := out.(opjournal.Delivered); ok {
		stampLastNudgeDeliveredAt(sessionFrontDoor(e.deps.sessions), e.info.ID, e.deps.clk.Now())
	}
	return out
}

// laneDeliveryResult maps one nudge's answer to the lane's result (DES-O1
// §7.2). unfencedReq says the request carried no fence, because the row
// names no token.
func laneDeliveryResult(ctx context.Context, res worker.NudgeResult, err error, at time.Time, unfencedReq bool) opjournal.DeliveryResult {
	delivered := func(ack opjournal.AckKind, degradation string) opjournal.Delivered {
		if res.Unfenced || unfencedReq {
			degradation = opjournal.DegradationUnfencedDelivery
		}
		return opjournal.Delivered{
			Record:      opjournal.DeliveryRecord{At: at, Ack: ack},
			Evidence:    opjournal.TurnEvidence{HasSource: false},
			Via:         laneViaLive,
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
	case errors.As(err, &miss) && miss.Partial:
		return opjournal.Unknown{Cause: err}
	case errors.As(err, &miss):
		return opjournal.NotDelivered{Transient: true, Cause: err}
	case errors.Is(err, runtime.ErrNudgeRefusedPendingInteraction), errors.Is(err, runtime.ErrRuntimeUnavailable):
		return opjournal.NotDelivered{Transient: true, Cause: err}
	case ctx.Err() != nil, errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return opjournal.Unknown{Cause: err}
	default:
		return opjournal.NotDelivered{Cause: err}
	}
}

// laneHandleError maps a failure to build the delivery's handle: nothing was
// sent. A session that is gone or unobservable is transient; anything else
// counts.
func laneHandleError(err error) opjournal.DeliveryResult {
	if errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, session.ErrSessionNotFound) {
		return opjournal.NotDelivered{Transient: true, Cause: err}
	}
	return opjournal.NotDelivered{Cause: err}
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
