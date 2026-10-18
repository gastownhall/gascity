package main

// The in-band hook pass (DES-O1 §8, OJ-13): `gc nudge drain --inject` runs
// one opjournal.ConversationLane pass over the hook's own conversation with
// the in-band executor below, so lane notices still reach the agent while the
// controller is down. The pass takes only notices whose `when` is boundary or
// after_turn; prompts, urgent content and if_idle wait for the lane host
// (DES-O1 §16, question 2), which the planner decides from Caps{InBand: true,
// Live: false}.
//
// The executor's effect is the hook's own output: the lane text joins the one
// hook context the drain writes (the clock, any legacy nudges, the lane
// notices, the step reminder). The hook gathers no turn evidence, so a
// delivery links exact only through the hook payload's turn id, and only for
// a family X11's live arm verified (worker.HookPayloadTurnID); every other
// delivery links none (no_source). When the hook deadline leaves no time to
// record the delivery, the batch is released as maybe delivered, so the next
// delivery carries the plain redelivery note (D-NOTE) and an at_most_once op
// closes failed{ambiguous_delivery}.
//
// An empty prompt costs one keyed list (hookLaneDue); the lane store, the
// session row and the runtime row are read only when the conversation holds
// an open lane op. The hook writes op rows and the runtime row's lane cells
// only, never turn facts, and pokes the controller's lane host after a Done.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

const (
	// laneHookHostName names the hook's lane in lease nonces and
	// transitions.
	laneHookHostName = "hook"
	// laneViaHook is the via of a delivery the hook injected in band.
	laneViaHook = "hook"
	// laneHookLeaseTTL is the hook's lease: long enough for one hook run,
	// short enough that a hook killed after its Take returns the batch soon.
	laneHookLeaseTTL = 30 * time.Second
	// laneHookPokeTimeout bounds each leg of the doorbell the hook rings
	// after a Done.
	laneHookPokeTimeout = 500 * time.Millisecond
)

// errLaneHookDeadline is the cause of a release when the hook deadline left
// no time to record a delivery whose text the hook already wrote.
var errLaneHookDeadline = errors.New("hook deadline too close to record the in-band delivery; the output may already be in the turn")

// errLaneHookControl is a control effect asked of the in-band executor. The
// planner never takes one for a runtime that is not live, so it is a bug;
// nothing is sent.
var errLaneHookControl = errors.New("conversation lane: the in-band hook runs no control effect")

// hookLaneDue reports whether conversation conv holds an open lane op, with
// one keyed read over the sessions-class store. A failed read is reported on
// stderr and read as not due: the hook must not fail the prompt, and the
// lane host and the next hook still see the op.
func hookLaneDue(sessions beads.Store, conv string, stderr io.Writer) bool {
	if conv == "" || isNilBeadStore(sessions) {
		return false
	}
	due, err := opjournal.HasOpenLaneOps(beads.SessionStore{Store: sessions}, conv)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge drain: conversation lanes: %v\n", err) //nolint:errcheck // best-effort stderr
		return false
	}
	return due
}

// hookLaneConversation is the conversation the in-band pass serves: the
// hook's own session (GC_SESSION_ID), and none for a drain that names its
// target, since the fence the pass checks is the hook's own environment.
func hookLaneConversation(args []string) string {
	if len(args) > 0 {
		return ""
	}
	return os.Getenv("GC_SESSION_ID")
}

// hookLaneFence is the incarnation the hook's environment names, zero when it
// names no instance token.
func hookLaneFence() runtime.InputFence {
	fence, _ := runtime.InputFenceFromEnv(map[string]string{
		runtime.RuntimeEpochEnv:      os.Getenv(runtime.RuntimeEpochEnv),
		runtime.ContinuationEpochEnv: os.Getenv(runtime.ContinuationEpochEnv),
		runtime.InstanceTokenEnv:     os.Getenv(runtime.InstanceTokenEnv),
	})
	return fence
}

// hookLanePayloadTurnID is the turn id a hook payload names for provider; a
// seam so tests can stand in for a family X11's live arm verified.
var hookLanePayloadTurnID = worker.HookPayloadTurnID

// hookLanePass is one in-band pass over the hook's conversation.
type hookLanePass struct {
	cityPath string
	// sessions is the sessions-class store, routed as the drain resolved it.
	sessions beads.Store
	conv     string
	// fence is the hook's own incarnation (hookLaneFence).
	fence runtime.InputFence
	// provider is the session's provider family; payload the hook's stdin.
	provider string
	payload  []byte
	clk      clock.Clock
	stderr   io.Writer
	// emit writes the one hook context, carrying text; the pass calls it at
	// most once, and only when it took a batch.
	emit func(text string) error
	// ackAllowed reports whether the hook deadline leaves time to record
	// the delivery of laneOps lane ops beside whatever else the drain wrote.
	ackAllowed func(laneOps int) bool
	// poke rings the controller's lane host for conv after a Done.
	poke func(cityPath, conv string)
	// stampDelivered stamps the session's last nudge delivery after an
	// accepted Done; the drain stamps it instead when legacy nudges rode
	// along, after their ack.
	stampDelivered bool
}

// hookLaneOutcome is what one in-band pass did to the hook's output.
type hookLaneOutcome struct {
	// emitted reports that the pass wrote the hook context (emit ran), with
	// emitErr its error.
	emitted bool
	emitErr error
	// ackAllowed is the deadline's answer when emitted without error: the
	// drain records its legacy nudges by the same answer.
	ackAllowed bool
	// accepted reports that the pass recorded a delivery.
	accepted bool
}

// run opens the lane over the sessions store, reads the conversation's view
// and runs one pass with the in-band executor. A store that cannot apply
// guarded batches hosts no lane: the pass is skipped with a note on stderr.
// Every failure is reported on stderr; the hook never fails the prompt.
func (p hookLanePass) run(ctx context.Context) hookLaneOutcome {
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: p.sessions}, p.clk)
	if err != nil {
		p.notef("opening the lane store: %v", err)
		return hookLaneOutcome{}
	}
	lane, err := opjournal.NewLane(st, laneHookHostName, p.clk)
	if err != nil {
		p.notef("%v", err)
		return hookLaneOutcome{}
	}
	view, info, _, err := readConversationLaneView(sessionFrontDoor(p.sessions), p.conv)
	if err != nil {
		p.notef("reading conversation %s: %v", p.conv, err)
		return hookLaneOutcome{}
	}
	exec := &hookLaneExec{pass: p, info: info}
	res, err := lane.Pass(ctx, view, exec)
	if err != nil {
		p.notef("pass over conversation %s: %v", p.conv, err)
	}
	accepted := res.Done == opjournal.DoneAccepted
	if accepted && p.stampDelivered && hookDeadlineStillLeavesTime() {
		stampLastNudgeDeliveredAt(sessionFrontDoor(p.sessions), p.conv, p.clk.Now())
	}
	if res.Done != "" && p.poke != nil && hookDeadlineStillLeavesTime() {
		p.poke(p.cityPath, p.conv)
	}
	return hookLaneOutcome{
		emitted:    exec.emitted,
		emitErr:    exec.emitErr,
		ackAllowed: exec.ackAllowed,
		accepted:   accepted,
	}
}

func (p hookLanePass) notef(format string, args ...any) {
	fmt.Fprintf(p.stderr, "gc nudge drain: conversation lanes: "+format+"\n", args...) //nolint:errcheck // best-effort stderr
}

// pokeLaneHost rings the controller's lane doorbell for conv (DES-O1 §7.3),
// best effort: with no controller, nothing answers and the lane host's scan
// finds the conversation when it starts.
func pokeLaneHost(cityPath, conv string) {
	_, _ = sendControllerCommandWithTimeouts(cityPath, laneSocketPrefix+conv, laneHookPokeTimeout, laneHookPokeTimeout, laneHookPokeTimeout)
}

// hookLaneExec is the in-band opjournal.Executor: the hook runs inside the
// agent's starting turn, so its probe always finds a boundary, and its effect
// is the hook's own output.
type hookLaneExec struct {
	pass hookLanePass
	info session.Info
	// emitted, emitErr and ackAllowed record the effect for the drain.
	emitted    bool
	emitErr    error
	ackAllowed bool
}

var _ opjournal.Executor = (*hookLaneExec)(nil)

// hookLaneCaps are the in-band executor's capabilities: no live input, an
// in-band injection into the starting turn, fenced by the check the probe
// makes, as a system-reminder block, gathering no turn evidence.
func hookLaneCaps() opjournal.Caps {
	return opjournal.Caps{
		InBand:   true,
		Input:    opjournal.FenceChecked,
		Text:     opjournal.TextReminder,
		LinkWait: 0,
		LeaseTTL: laneHookLeaseTTL,
	}
}

// Probe implements opjournal.Executor: the hook runs at the start of a turn
// in the runtime itself, so it is available, running and at a boundary.
// Content is blocked when the row's content gate says so, or when the hook's
// incarnation is not the one the row names (a zombie runtime).
func (e *hookLaneExec) Probe(_ context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	p := opjournal.Probe{
		Available: true,
		Running:   true,
		Boundary:  opjournal.TriYes,
		TurnLive:  opjournal.TriUnknown,
		Caps:      hookLaneCaps(),
	}
	p.ContentBlock = session.ContentGate(e.info, true, e.pass.clk.Now())
	if p.ContentBlock == "" && !hookLaneFenceMatches(e.pass.fence, conv) {
		p.ContentBlock = laneContentBlockIncarnation
	}
	return p, nil
}

// hookLaneFenceMatches reports whether the hook's incarnation is the one the
// view names: the same token, generation and epoch. A row and a hook that
// both name no token match: the delivery is recorded unfenced.
func hookLaneFenceMatches(hook runtime.InputFence, conv opjournal.ConversationView) bool {
	if conv.Token == "" || hook.Token == "" {
		return conv.Token == hook.Token
	}
	return hook.Token == conv.Token && hook.Generation == conv.Generation && hook.Epoch == conv.Epoch
}

// Deliver implements opjournal.Executor: it writes the delivery's text into
// the hook context, then accepts it when the deadline leaves time to record
// it, and otherwise reports it maybe delivered.
func (e *hookLaneExec) Deliver(_ context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	if e.emitted {
		return opjournal.NotDelivered{Transient: true, Cause: errors.New("conversation lane: the hook context was already written")}
	}
	at := e.pass.clk.Now()
	e.emitted = true
	if err := e.pass.emit(d.Text); err != nil {
		e.emitErr = err
		return opjournal.NotDelivered{Cause: fmt.Errorf("writing the hook context: %w", err)}
	}
	e.ackAllowed = e.pass.ackAllowed(len(d.Ops))
	if !e.ackAllowed {
		return opjournal.Unknown{Cause: errLaneHookDeadline}
	}
	return opjournal.Delivered{
		Record: opjournal.DeliveryRecord{
			At:     at,
			Ack:    opjournal.AckInBand,
			TurnID: hookLanePayloadTurnID(e.pass.provider, e.pass.payload),
		},
		Evidence: opjournal.TurnEvidence{HasSource: false},
		Via:      laneViaHook,
	}
}

// Interrupt implements opjournal.Executor: never taken in band.
func (e *hookLaneExec) Interrupt(context.Context, opjournal.InterruptCall) opjournal.ControlResult {
	return opjournal.NotDelivered{Transient: true, Cause: errLaneHookControl}
}

// Respond implements opjournal.Executor: never taken in band.
func (e *hookLaneExec) Respond(context.Context, opjournal.RespondCall) opjournal.ControlResult {
	return opjournal.NotDelivered{Transient: true, Cause: errLaneHookControl}
}

// hookLaneSessionStore is the sessions-class store the in-band pass reads and
// writes the conversation's ops through, nil when the city store does not
// open (reported on stderr). It is a seam so tests can stand in a store
// with guarded batches for the file store a test city opens.
var hookLaneSessionStore = openHookLaneSessionStore

func openHookLaneSessionStore(cityPath string, cfg *config.City, stderr io.Writer) beads.Store {
	store, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge drain: conversation lanes: opening the city store: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil
	}
	return cliSessionStore(store, cfg, cityPath)
}

// hookDrainLane runs the in-band pass for an injecting drain when conversation
// conv holds an open lane op. write writes the drain's one hook context with
// the lane text; legacy is the count of legacy nudges that context also
// carries, so one deadline answer covers both. With nothing due it costs the
// one keyed probe and writes nothing.
func hookDrainLane(cityPath string, sessions beads.Store, conv, provider string, payload []byte, legacy int, write func(laneText string) error, stderr io.Writer) hookLaneOutcome {
	if !hookLaneDue(sessions, conv, stderr) {
		return hookLaneOutcome{}
	}
	ctx, cancel := hookLaneContext()
	defer cancel()
	return hookLanePass{
		cityPath: cityPath,
		sessions: sessions,
		conv:     conv,
		fence:    hookLaneFence(),
		provider: provider,
		payload:  payload,
		clk:      clock.Real{},
		stderr:   stderr,
		emit:     write,
		ackAllowed: func(laneOps int) bool {
			return hookDeadlineAllowsNudgeAck(fmt.Sprintf("%d injected nudge(s)", legacy+laneOps), stderr)
		},
		poke:           pokeLaneHost,
		stampDelivered: legacy == 0,
	}.run(ctx)
}

// hookLaneContext is the pass's context: it ends at the deadline gc hook run
// handed this process, when there is one still ahead.
func hookLaneContext() (context.Context, context.CancelFunc) {
	if deadline, ok, err := inheritedHookDeadline(); err == nil && ok && deadline.After(time.Now()) {
		return context.WithDeadline(context.Background(), deadline)
	}
	return context.WithCancel(context.Background())
}
