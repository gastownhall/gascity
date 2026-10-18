package main

// The in-band hook pass (DES-O1 §8, OJ-13): `gc nudge drain --inject` runs
// one opjournal.ConversationLane pass over the hook's own conversation with
// the in-band executor below, so lane notices still reach the agent while the
// controller is down. It runs only where the prompt hook's output reaches the
// model (hookLaneReachesModel); every other family's notices wait for the
// lane host, which types them at the next boundary. The pass takes only
// notices whose `when` is boundary or after_turn; prompts, urgent content and
// if_idle wait for the lane host (DES-O1 §16, question 2), which the planner
// decides from Caps{InBand: true, Live: false}.
//
// The executor's effect is the hook's own output: the lane text joins the one
// hook context the drain writes (the clock, any legacy nudges, the lane
// notices, the step reminder). The hook gathers no turn evidence, so a
// delivery links exact only through the hook payload's turn id, and only for
// a family X11's live arm verified (worker.HookPayloadTurnID); every other
// delivery links none (no_source). When the hook deadline leaves no time to
// record the delivery, the batch is released as maybe delivered, so the next
// delivery carries the plain redelivery note (D-NOTE) and an at_most_once op
// closes failed{ambiguous_delivery}. A delivery is recorded only by the
// deadline less the archive margin (hookLaneSettleBy): a Done that has not
// landed by then leaves the batch executing, and the lane takes it again as
// maybe delivered once the hook's lease lapses, so a slow store never records
// a delivery whose output gc hook run then discards.
//
// An empty prompt costs one keyed list (hookLaneDue), and nothing on a store
// that cannot host a lane; the lane store, the session row and the runtime row
// are read only when the conversation holds an open lane op. The hook writes
// op rows, the runtime row's lane cells and, after an accepted lane-only
// delivery, the session's last-nudge-delivered stamp; never turn facts. The
// drain rings the controller's lane host after a Done, last of all, once its
// own output and bookkeeping are done (hookLaneRing).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

const (
	// laneHookHostName names the hook's lane in lease nonces and
	// transitions.
	laneHookHostName = "hook"
	// laneViaHook is the via of a delivery the hook injected in band.
	laneViaHook = "hook"
	// laneHookLeaseGrace is what the hook's lease outlives the hook
	// deadline by (DES-O1 §4.6): a hook killed at its deadline after its Take
	// returns the batch soon after.
	laneHookLeaseGrace = 10 * time.Second
	// laneHookLeaseTTLNoDeadline is the hook's lease when gc hook run handed
	// it no deadline: long enough for one hook run.
	laneHookLeaseTTLNoDeadline = 30 * time.Second
	// laneHookPokeTimeout bounds each leg of the doorbell the drain rings
	// after a Done.
	laneHookPokeTimeout = 500 * time.Millisecond
	// laneHookPokeLegFloor is the shortest doorbell leg worth trying; with less
	// time left the doorbell is skipped and the lane host's scan finds the
	// conversation.
	laneHookPokeLegFloor = 20 * time.Millisecond
)

// hookLaneReachesModel reports whether a prompt hook of hookFormat, run by
// provider (as GC_PROVIDER names it), puts its output in front of the model:
// the JSON formats gc writes for codex, gemini and antigravity, and plain
// text for the claude family and the opencode and mimocode plugins, the
// family read from the name (worker.HookFamily), so groq and cerebras run the
// opencode plugin. Other families' prompt hooks read stdout as a control
// document (cursor) or drop plain text (copilot), so an in-band delivery
// there would be recorded and never seen.
func hookLaneReachesModel(hookFormat, provider string) bool {
	switch strings.ToLower(strings.TrimSpace(hookFormat)) {
	case hookOutputFormatCodex, hookOutputFormatGemini, hookOutputFormatAntigravity:
		return true
	case "":
		switch worker.HookFamily(provider) {
		case "claude", "opencode", "mimocode":
			return true
		}
	}
	return false
}

// errLaneHookDeadline is the cause of a release when the hook deadline left
// no time to record a delivery whose text the hook already wrote.
var errLaneHookDeadline = errors.New("hook deadline too close to record the in-band delivery; the output may already be in the turn")

// errLaneHookControl is a control effect asked of the in-band executor. The
// planner never takes one for a runtime that is not live, so it is a bug;
// nothing is sent.
var errLaneHookControl = errors.New("conversation lane: the in-band hook runs no control effect")

// hookLaneDue reports whether conversation conv holds an open lane op, with
// one keyed read over the sessions-class store. A store that cannot apply
// guarded batches hosts no lane, so it is not read at all. A failed read is
// reported on stderr and read as not due: the hook must not fail the prompt,
// and the lane host and the next hook still see the op.
func hookLaneDue(sessions beads.Store, conv string, stderr io.Writer) bool {
	if conv == "" || isNilBeadStore(sessions) {
		return false
	}
	store := beads.SessionStore{Store: sessions}
	if batcher, ok := beads.GuardedBatcherFor(store); !ok || batcher == nil {
		return false
	}
	due, err := opjournal.HasOpenLaneOps(store, conv)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge drain: conversation lanes: %v\n", err) //nolint:errcheck // best-effort stderr
		return false
	}
	return due
}

// hookLaneConversation is the conversation the in-band pass serves: the
// hook's own session (GC_SESSION_ID). It is none for a drain that names its
// target, since the fence the pass checks is the hook's own environment, and
// none when the hook's output does not reach the model (hookLaneReachesModel,
// with the provider the session's runtime environment names in GC_PROVIDER).
func hookLaneConversation(args []string, hookFormat string) string {
	if len(args) > 0 || !hookLaneReachesModel(hookFormat, os.Getenv("GC_PROVIDER")) {
		return ""
	}
	return strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
}

// hookLaneFence is the incarnation the hook's environment names: its
// generation and epoch always, as the view reads a row's (conversationLaneView),
// and its instance token, empty when it names none.
func hookLaneFence() runtime.InputFence {
	// InputFenceFromEnv reads the generation and epoch only beside a token,
	// so it reads a copy that names one and the hook's own token is kept.
	fence, _ := runtime.InputFenceFromEnv(map[string]string{
		runtime.RuntimeEpochEnv:      os.Getenv(runtime.RuntimeEpochEnv),
		runtime.ContinuationEpochEnv: os.Getenv(runtime.ContinuationEpochEnv),
		runtime.InstanceTokenEnv:     "-",
	})
	fence.Token = os.Getenv(runtime.InstanceTokenEnv)
	return fence
}

// hookLaneLeaseTTL is the hook's lease at now: the time left to the deadline
// gc hook run handed this process plus laneHookLeaseGrace (DES-O1 §4.6), at
// least opjournal.MinLeaseTTL; laneHookLeaseTTLNoDeadline without one.
func hookLaneLeaseTTL(now time.Time) time.Duration {
	deadline, ok, err := inheritedHookDeadline()
	if err != nil || !ok {
		return laneHookLeaseTTLNoDeadline
	}
	return max(opjournal.MinLeaseTTL, deadline.Sub(now)+laneHookLeaseGrace)
}

// hookLaneSettleBy is the last instant the in-band pass may record a
// delivery: the deadline gc hook run handed this process less the archive
// margin, the time the drain keeps to ack or release its legacy nudges and
// exit before the runner discards its output. It is zero without a deadline,
// and with one that does not parse, whose ack check refuses the delivery.
func hookLaneSettleBy() time.Time {
	deadline, ok, err := inheritedHookDeadline()
	if err != nil || !ok {
		return time.Time{}
	}
	return deadline.Add(-hookArchiveMargin(deadline, hookProcessStart))
}

// hookLanePayloadTurnID is the turn id a hook payload names for provider; a
// seam so tests can stand in for a family X11's live arm verified.
var hookLanePayloadTurnID = worker.HookPayloadTurnID

// hookLanePass is one in-band pass over the hook's conversation.
type hookLanePass struct {
	// sessions is the sessions-class store, routed as the drain resolved it.
	sessions beads.Store
	conv     string
	// fence is the hook's own incarnation (hookLaneFence).
	fence runtime.InputFence
	// leaseTTL is the lease the pass takes its batch under (hookLaneLeaseTTL).
	leaseTTL time.Duration
	// settleBy is the last instant the pass may record a delivery
	// (hookLaneSettleBy), zero for no bound.
	settleBy time.Time
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
	// ackAllowed is the deadline's answer when emitted without error, asked
	// before the pass's Done. A no releases the legacy nudges the context
	// carries too; after a yes the drain asks again before it acks them,
	// since the Done has run in between.
	ackAllowed bool
	// accepted reports that the pass recorded a delivery.
	accepted bool
	// done reports that the pass recorded a Done, so the drain rings the
	// lane host (hookLaneRing) once it has finished.
	done bool
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
	view, info, absent, err := readConversationLaneView(sessionFrontDoor(p.sessions), p.conv)
	if err != nil {
		p.notef("reading conversation %s: %v", p.conv, err)
		return hookLaneOutcome{}
	}
	if absent {
		p.notef("conversation %s has no session row on two reads; its open ops close as skipped{conversation_closed}", p.conv)
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
	return hookLaneOutcome{
		emitted:    exec.emitted,
		emitErr:    exec.emitErr,
		ackAllowed: exec.ackAllowed,
		accepted:   accepted,
		done:       res.Done != "",
	}
}

func (p hookLanePass) notef(format string, args ...any) {
	fmt.Fprintf(p.stderr, "gc nudge drain: conversation lanes: "+format+"\n", args...) //nolint:errcheck // best-effort stderr
}

// hookLanePoke rings the controller's lane doorbell for conv with legs of
// leg each; a seam so tests can record the doorbell.
var hookLanePoke = pokeLaneHost

// pokeLaneHost rings the controller's lane doorbell for conv (DES-O1 §7.3),
// best effort: with no controller, nothing answers and the lane host's scan
// finds the conversation when it starts.
func pokeLaneHost(cityPath, conv string, leg time.Duration) {
	_, _ = sendControllerCommandWithTimeouts(cityPath, laneSocketPrefix+conv, leg, leg, leg)
}

// hookLanePokeLeg is the timeout of each of the doorbell's three legs at now:
// laneHookPokeTimeout, cut so the whole doorbell takes at most half the time
// left to the hook deadline. It is zero when the deadline does not parse or
// leaves less than laneHookPokeLegFloor a leg.
func hookLanePokeLeg(now time.Time) time.Duration {
	deadline, ok, err := inheritedHookDeadline()
	if err != nil {
		return 0
	}
	if !ok {
		return laneHookPokeTimeout
	}
	leg := min(laneHookPokeTimeout, deadline.Sub(now)/6)
	if leg < laneHookPokeLegFloor {
		return 0
	}
	return leg
}

// hookLaneRing rings the lane host for conv after a pass that recorded a
// Done. The drain calls it last, after its own output, acks and stamps, so
// the doorbell never spends the time those need.
func hookLaneRing(cityPath, conv string, out hookLaneOutcome) {
	if !out.done || conv == "" {
		return
	}
	if leg := hookLanePokeLeg(time.Now()); leg > 0 {
		hookLanePoke(cityPath, conv, leg)
	}
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
// makes, as a system-reminder block, gathering no turn evidence, under a
// lease of leaseTTL.
func hookLaneCaps(leaseTTL time.Duration) opjournal.Caps {
	return opjournal.Caps{
		InBand:   true,
		Input:    opjournal.FenceChecked,
		Text:     opjournal.TextReminder,
		LinkWait: 0,
		LeaseTTL: leaseTTL,
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
		Caps:      hookLaneCaps(e.pass.leaseTTL),
	}
	p.ContentBlock = session.ContentGate(e.info, true, e.pass.clk.Now())
	if p.ContentBlock == "" && !hookLaneFenceMatches(e.pass.fence, conv) {
		p.ContentBlock = laneContentBlockIncarnation
	}
	return p, nil
}

// hookLaneFenceMatches reports whether the hook's incarnation is the one the
// view names: the same token, generation and epoch (DES-O1 §8). A row and a
// hook that both name no token match on generation and epoch alone: the
// delivery is recorded unfenced.
func hookLaneFenceMatches(hook runtime.InputFence, conv opjournal.ConversationView) bool {
	return hook.Token == conv.Token && hook.Generation == conv.Generation && hook.Epoch == conv.Epoch
}

// Deliver implements opjournal.Executor: it writes the delivery's text into
// the hook context, then accepts it, to be recorded by the pass's settleBy,
// when the deadline leaves time to record it, and otherwise reports it maybe
// delivered.
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
		SettleBy: e.pass.settleBy,
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
// conv holds an open lane op; knownDue says the drain's fast path already
// found one, so the probe is not run again. write writes the drain's one hook
// context with the lane text; legacy is the count of legacy nudges that
// context also carries, which the pass's deadline answer counts beside its
// own ops. The pass records its delivery by hookLaneSettleBy, which leaves
// the drain the archive margin to ack or release them. With nothing due it
// costs the one keyed probe and writes nothing. The drain rings the lane host
// itself (hookLaneRing) when it is done.
func hookDrainLane(sessions beads.Store, conv string, knownDue bool, provider string, payload []byte, legacy int, write func(laneText string) error, stderr io.Writer) hookLaneOutcome {
	if !knownDue && !hookLaneDue(sessions, conv, stderr) {
		return hookLaneOutcome{}
	}
	ctx, cancel := hookLaneContext()
	defer cancel()
	return hookLanePass{
		sessions: sessions,
		conv:     conv,
		fence:    hookLaneFence(),
		leaseTTL: hookLaneLeaseTTL(time.Now()),
		settleBy: hookLaneSettleBy(),
		provider: provider,
		payload:  payload,
		clk:      clock.Real{},
		stderr:   stderr,
		emit:     write,
		ackAllowed: func(laneOps int) bool {
			return hookDeadlineAllowsNudgeAck(fmt.Sprintf("%d injected nudge(s)", legacy+laneOps), stderr)
		},
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
