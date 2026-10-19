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
	// laneDeliveryKeyPrefix scopes a lane delivery key in the runtime's key
	// space (OJ-7's format).
	laneDeliveryKeyPrefix = "gc:"
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
}

// conversationLaneExec is the live opjournal.Executor for one conversation
// over worker.Handle, for keystroke-mode runtimes. It is built per pass from
// the session row the pass's view came from, so Probe and Deliver act on the
// incarnation that view names.
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
	// live is the handle the last probe resolved; Deliver types through it.
	live worker.Handle
	// lastCaps and capsRead are the capabilities the first Probe read,
	// which later probes of the pass and its re-probes reuse instead of
	// reading the runtime row again.
	lastCaps opjournal.Caps
	capsRead bool
}

var _ opjournal.Executor = (*conversationLaneExec)(nil)

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
	return e
}

// Probe implements opjournal.Executor: the runtime's caps, read once per
// executor (one pass), then one bounded look at the runtime (laneLook).
func (e *conversationLaneExec) Probe(ctx context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	if !e.capsRead {
		caps, err := e.caps(conv)
		if err != nil {
			return opjournal.Probe{}, err
		}
		e.lastCaps, e.capsRead = caps, true
	}
	return e.probe(ctx, conv, e.lastCaps)
}

// Reprobe is Probe for a host re-probing a busy runtime on its backoff
// (DES-O1 §4.3): it reuses the caps the pass's Probe read and observes the
// runtime by the row's bead id through the cached sessions store, so over
// the controller's cache it reads nothing from the backing store.
func (e *conversationLaneExec) Reprobe(ctx context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	return e.Probe(ctx, conv)
}

// probe is one look at the runtime under caps, the whole look bounded by
// e.budget.
func (e *conversationLaneExec) probe(ctx context.Context, conv opjournal.ConversationView, caps opjournal.Caps) (opjournal.Probe, error) {
	p := opjournal.Probe{Available: true, Fence: laneFence(conv), TurnLive: opjournal.TriUnknown, Caps: caps}
	e.live = nil
	octx, cancel := context.WithTimeout(ctx, e.budget)
	defer cancel()
	look, err := worker.ObserveBounded(octx, laneObservationKey(e.deps.cityPath, e.info.ID), func(lctx context.Context) (laneLook, error) {
		return e.look(lctx, caps)
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
	e.live = look.handle
	return p, nil
}

// laneLook is one look at the runtime.
type laneLook struct {
	running  bool
	block    string
	pending  string
	boundary opjournal.Tri
	handle   worker.Handle
}

// look observes the runtime, gates content on the row (session.ContentGate)
// and on the live incarnation, and, only for a running runtime content may
// reach, resolves its handle once and reads the pending interaction and the
// idle boundary through it.
func (e *conversationLaneExec) look(ctx context.Context, caps opjournal.Caps) (laneLook, error) {
	obs, err := e.observe(e.target)
	if err != nil {
		return laneLook{}, err
	}
	l := laneLook{running: obs.Running && obs.Alive}
	l.block = session.ContentGate(e.info, l.running, e.deps.clk.Now())
	if l.running && l.block == "" {
		matches, err := e.matches(e.target, obs)
		if err != nil {
			return laneLook{}, fmt.Errorf("matching session %s's live generation: %w", e.info.ID, err)
		}
		if !matches {
			l.block = laneContentBlockIncarnation
		}
	}
	if !l.running || l.block != "" || !caps.Live {
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
	l.boundary = opjournal.TriNo
	if e.idle(e.target, obs) {
		l.boundary = opjournal.TriYes
	}
	return l, nil
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
// probe resolved.
func (e *conversationLaneExec) Deliver(ctx context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	h := e.live
	if h == nil {
		return opjournal.NotDelivered{Transient: true, Cause: errLaneNoProbedHandle}
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
	out := laneDeliveryResult(res, err, at, fence.IsZero())
	if _, ok := out.(opjournal.Delivered); ok {
		stampLastNudgeDeliveredAt(sessionFrontDoor(e.deps.sessions), e.info.ID, e.deps.clk.Now())
	}
	return out
}

// laneDeliveryResult maps one nudge's answer to the lane's result (DES-O1
// §7.2). unfencedReq says the request carried no fence, because the row
// names no token. A submit the runtime could not confirm may have delivered,
// so it is Unknown; a nudge refused on a pending interaction, whichever
// runtime refused it, or one the runtime reports never sent, is a transient
// miss. Any other error may have come after input reached the session, so it
// is Unknown too: a retake under a fresh key would deliver it twice.
func laneDeliveryResult(res worker.NudgeResult, err error, at time.Time, unfencedReq bool) opjournal.DeliveryResult {
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
