package main

// The in-band hook pass (DES-O1 §8, OJ-13): `gc nudge drain --inject` runs
// one opjournal.ConversationLane pass over the hook's own conversation with
// the in-band executor below, so lane notices still reach the agent while the
// controller is down. It runs only where the prompt hook's output reaches the
// model (hookLaneReachesModel); every other family's notices wait for the
// lane host, which types them at the next boundary. The pass takes only
// notices whose `when` is boundary or after_turn; prompts, urgent content and
// if_idle wait for the lane host (DES-O1 §16, question 2), which the planner
// decides from Caps{InBand: true, Live: false}. The lane text joins the drain's
// own text in a context the provider keeps whole only up to a cap
// (hookLaneContextBudget), so the planner takes notices only while their text
// fits the room the drain's own text leaves (Caps.MaxText): a backlog goes out
// over several prompts, and a notice too long for the room waits for the lane
// host.
//
// The executor's effect is the hook's own output: the lane text joins the one
// hook context the drain writes (the clock, any legacy nudges, the lane
// notices, the step reminder). The hook gathers no turn evidence, so a
// delivery links exact only through the hook payload's turn id, and only for
// a family X11's live arm verified (worker.HookPayloadTurnID); every other
// delivery links none (no_source). The pass writes into the hook context only
// while the hook deadline leaves time to record the delivery
// (hookLaneRecordTime): with no time left the probe blocks content, so nothing
// is taken, and a batch taken before the time ran out is released unwritten,
// a transient miss that counts no failure and is not maybe delivered, by the
// hook deadline itself (hookLaneDeadline). A batch whose write fails is
// released the same way: the failed write put no notice in front of the
// model (Deliver). A delivery is recorded only by the deadline less the
// archive margin (hookLaneSettleBy): a Done that has not landed by then
// leaves the batch executing, and the lane takes it again as maybe delivered
// once the hook's lease lapses, so a slow store never records a delivery
// whose output gc hook run then discards.
//
// An empty prompt whose step reminder the controller answered opens no store
// for lanes: the controller is up, and its lane host types the conversation's
// notices at the next boundary (hookLaneDueWithoutController). That leaves
// two kinds of conversation waiting for a prompt whose drain takes the full
// path: a supervisor-hosted city's, which runs no lane host until OJ-12b, and
// that of a runtime taking input by protocol (t3bridge), whose content the
// lane host holds not_live. Otherwise an empty prompt costs one keyed
// list (hookLaneDue), and nothing on a store that cannot host a lane; the lane
// store, the session row and the runtime row are read only when the
// conversation holds an open lane op. The hook writes op rows, the runtime
// row's lane cells and, after an accepted lane-only delivery, the session's
// last-nudge-delivered stamp; never turn facts. The drain rings the
// controller for the journal rows the pass wrote, and its lane host after a
// Done, last of all, once its own output and bookkeeping are done
// (hookLaneRing): the pass's own writes ring nothing (deferJournalRings).

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
	// laneContentBlockHookDeadline blocks content while the hook deadline
	// leaves no time to record a delivery.
	laneContentBlockHookDeadline = "hook_deadline"
	// hookLaneContextBudget is the most bytes of hook context the drain writes
	// with lane notices in it. It is under every cap up to which a prompt
	// hook's context reaches the model whole, each in the unit its provider
	// counts. Claude Code keeps 10,000 characters, and 9000 bytes are at most
	// 9000 characters. Codex keeps a handler's additionalContext while its
	// approximate token count, a quarter of its bytes rounded up (codex-rs
	// approx_token_count), is within the handler's additionalContextLimit,
	// 2,500 when unset as in gc's .codex/hooks.json: 10,000 bytes, however
	// the text tokenizes. Gemini documents no cap. Past a cap the model sees
	// only a preview, and the lane would record as delivered notices the
	// model never saw.
	hookLaneContextBudget = 9000
)

// hookLaneReachesModel reports whether a prompt hook of hookFormat, run by
// provider (as GC_PROVIDER names it), puts its output in front of the model
// of the agent's own conversation as its turn starts: the JSON formats gc
// writes for codex and gemini, and plain text for the claude family, the
// family read from the name (worker.HookFamily). Other families' prompt hooks
// read stdout as a control document (cursor) or drop plain text (copilot), so
// an in-band delivery there would be recorded and never seen. The opencode and
// mimocode plugins (which groq and cerebras run too) drain for every session
// in their process, Task subagents and session-less generate calls as well as
// the agent's own, under the agent's environment, so a delivery there could be
// recorded into a prompt the agent's conversation never sees. Antigravity
// drains before every model call of a turn (PreInvocation) into an ephemeral
// message that later calls drop, so a delivery there could be recorded after
// one call saw it mid-turn. Their notices wait for the lane host.
func hookLaneReachesModel(hookFormat, provider string) bool {
	switch strings.ToLower(strings.TrimSpace(hookFormat)) {
	case hookOutputFormatCodex, hookOutputFormatGemini:
		return true
	case "":
		return worker.HookFamily(provider) == "claude"
	}
	return false
}

// errLaneHookDeadline is why the in-band pass writes nothing when the hook
// deadline leaves no time to record a delivery: its notices wait for the next
// drain or the lane host.
var errLaneHookDeadline = errors.New("hook deadline too close to record an in-band delivery")

// errLaneHookNoRoom is why the in-band pass writes nothing for a delivery
// whose text is over the room the hook context has left for it: its notices
// wait for a later drain or the lane host.
var errLaneHookNoRoom = errors.New("the in-band delivery's text is over the room the hook context has for it")

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

// hookLaneDueWithoutController reports whether an empty prompt's drain takes
// the full path for the in-band pass over conversation conv (hookLaneDue over
// the sessions store of cityPath). It is not asked when the controller answered
// the hook's step reminder (step): the controller is up, and its lane host
// serves the conversation, so the prompt opens no store for lanes. Two kinds
// of conversation the lane host does not serve wait the same way: a
// supervisor-hosted city's, which runs no lane host until OJ-12b, and that of
// a runtime taking input by protocol (t3bridge), whose content the lane host
// holds not_live. There the pass runs when a drain takes the full path anyway
// (a queued nudge, a step reminder, a usage sample), over the store that path
// opened. Without the controller's answer the hook read the step reminder in
// process, so the probe reads through the invocation's store memo.
func hookLaneDueWithoutController(conv, cityPath string, step *wispStepLookup, stderr io.Writer) bool {
	if conv == "" || step.answered() {
		return false
	}
	return hookLaneDue(hookLaneSessionStore(cityPath, nil, stderr), conv, stderr)
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
// and with one that does not parse, whose deadline check refuses before the
// Take.
func hookLaneSettleBy() time.Time {
	deadline, ok, err := inheritedHookDeadline()
	if err != nil || !ok {
		return time.Time{}
	}
	return deadline.Add(-hookArchiveMargin(deadline, hookProcessStart))
}

// hookLaneRecordTime is the pass's deadline check, asked before it takes a
// batch and again before it writes one: nil while the deadline gc hook run
// handed this process leaves the archive margin to record a delivery,
// errLaneHookDeadline once it does not, and the parse error of a deadline
// that does not parse, which leaves no time either.
func hookLaneRecordTime() error {
	ok, err := hookDeadlineLeavesArchiveTime(time.Now(), hookProcessStart)
	switch {
	case err != nil:
		return err
	case !ok:
		return errLaneHookDeadline
	default:
		return nil
	}
}

// hookLaneDeadline is the last instant the pass may release a batch it took
// and did not get in front of the model (Deliver): the deadline gc hook run
// handed this process, when the runner kills it. The release records nothing
// about the hook's output, so it may land past the settle-by time, but not
// once the drain is gone. It is zero without a deadline and with one that
// does not parse, whose check refuses before the Take.
func hookLaneDeadline() time.Time {
	deadline, ok, err := inheritedHookDeadline()
	if err != nil || !ok {
		return time.Time{}
	}
	return deadline
}

// hookLanePayloadTurnID is the turn id a hook payload names for provider; a
// seam so tests can stand in for a family X11's live arm verified.
var hookLanePayloadTurnID = worker.HookPayloadTurnID

// hookLaneTextRoom is the room the hook context leaves for the lane text
// beside around, the drain's own text in it: hookLaneContextBudget less the
// bytes around takes, zero or less once around fills the budget.
func hookLaneTextRoom(around ...string) int {
	room := hookLaneContextBudget
	for _, text := range around {
		room -= len(text)
	}
	return room
}

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
	// deadline is the last instant the pass may release a batch it took and
	// did not get in front of the model (hookLaneDeadline), zero for no bound.
	deadline time.Time
	// room is the most bytes of lane text the hook context has room for
	// (hookLaneTextRoom), at least 1: the probe's caps take no more
	// (Caps.MaxText) and Deliver writes no more.
	room int
	// provider is the session's provider, as GC_PROVIDER names it; payload
	// the hook's stdin.
	provider string
	payload  []byte
	clk      clock.Clock
	stderr   io.Writer
	// emit writes the one hook context, carrying text; the pass calls it at
	// most once, and only when it took a batch.
	emit func(text string) error
	// recordTime is the deadline check (hookLaneRecordTime): the pass takes
	// and writes a batch only while it answers nil.
	recordTime func() error
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
	if exec.refused != nil {
		p.notef("conversation %s: %v; its notices stay out of the hook context and wait for the next drain or the lane host", p.conv, exec.refused)
	}
	if err != nil {
		p.notef("pass over conversation %s: %v", p.conv, err)
	}
	accepted := res.Done == opjournal.DoneAccepted
	if accepted && p.stampDelivered && hookDeadlineStillLeavesTime() {
		stampLastNudgeDeliveredAt(sessionFrontDoor(p.sessions), p.conv, p.clk.Now())
	}
	return hookLaneOutcome{
		emitted:  exec.emitted,
		emitErr:  exec.emitErr,
		accepted: accepted,
		done:     res.Done != "",
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

// hookLaneRing rings the controller for the journal rows the pass wrote,
// rows (deferJournalRings; nil when its store does not ring), whether or not
// it recorded a Done: each such ring refreshes the rows and pokes their
// conversation. After a Done it also rings the lane host for conv, unless a
// row ring's doorbell answered that it poked conv: a ring no controller
// answered, or one it refused, is followed by the plain doorbell. The drain
// calls it last, after its own output, acks and stamps, so the doorbells
// never spend the time those need.
func hookLaneRing(cityPath, conv string, out hookLaneOutcome, rows *journalRingQueue) {
	poked := rows.ring(cityPath)
	if !out.done || conv == "" || poked[conv] {
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
	// emitted and emitErr record the effect for the drain; refused is why
	// the pass did not write: the deadline check's answer, or a text over
	// the room.
	emitted bool
	emitErr error
	refused error
}

var _ opjournal.Executor = (*hookLaneExec)(nil)

// hookLaneCaps are the in-band executor's capabilities: no live input, an
// in-band injection into the starting turn, fenced by the check the probe
// makes, as a system-reminder block of at most room bytes (positive),
// gathering no turn evidence, under a lease of leaseTTL.
func hookLaneCaps(leaseTTL time.Duration, room int) opjournal.Caps {
	return opjournal.Caps{
		InBand:   true,
		Input:    opjournal.FenceChecked,
		Text:     opjournal.TextReminder,
		LinkWait: 0,
		LeaseTTL: leaseTTL,
		MaxText:  room,
	}
}

// Probe implements opjournal.Executor: the hook runs at the start of a turn
// in the runtime itself, so it is available, running and at a boundary.
// Content is blocked when the row's content gate says so, when the hook's
// incarnation is not the one the row names (a zombie runtime), or when the
// hook deadline leaves no time to record a delivery, so the pass takes
// nothing it could not record.
func (e *hookLaneExec) Probe(_ context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	p := opjournal.Probe{
		Available: true,
		Running:   true,
		Boundary:  opjournal.TriYes,
		TurnLive:  opjournal.TriUnknown,
		Caps:      hookLaneCaps(e.pass.leaseTTL, e.pass.room),
	}
	p.ContentBlock = session.ContentGate(e.info, true, e.pass.clk.Now(), session.GateContentAny)
	if p.ContentBlock == "" && !hookLaneFenceMatches(e.pass.fence, conv) {
		p.ContentBlock = laneContentBlockIncarnation
	}
	if p.ContentBlock == "" {
		if err := e.pass.recordTime(); err != nil {
			e.refused = err
			p.ContentBlock = laneContentBlockHookDeadline
		}
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
// the hook context and accepts it, to be recorded by the pass's settleBy. It
// checks first that the text fits the room the context has for it, which the
// probe's caps already hold the batch to, and asks the deadline check again:
// with a text over the room, or once the time to record the delivery is gone,
// it writes nothing and releases the batch as a transient miss, which counts
// no failure and is not maybe delivered, by the pass's deadline. A write that
// fails releases the batch the same way, as unseen: the drain writes the hook
// context in one write to the pipe gc hook run reads, which fails once that
// reader is gone (gc ignores SIGPIPE, so the drain lives to release it), and
// a JSON format that cannot encode its document writes nothing.
func (e *hookLaneExec) Deliver(_ context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	if e.emitted {
		return opjournal.NotDelivered{Transient: true, Cause: errors.New("conversation lane: the hook context was already written"), SettleBy: e.pass.deadline}
	}
	if len(d.Text) > e.pass.room {
		e.refused = fmt.Errorf("%w: %d bytes, room for %d", errLaneHookNoRoom, len(d.Text), e.pass.room)
		return opjournal.NotDelivered{Transient: true, Cause: e.refused, SettleBy: e.pass.deadline}
	}
	if err := e.pass.recordTime(); err != nil {
		e.refused = err
		return opjournal.NotDelivered{Transient: true, Cause: err, SettleBy: e.pass.deadline}
	}
	at := e.pass.clk.Now()
	e.emitted = true
	if err := e.pass.emit(d.Text); err != nil {
		e.emitErr = err
		return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("writing the hook context: %w", err), SettleBy: e.pass.deadline}
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
// context with the lane text, which may take up to room bytes of it
// (hookLaneTextRoom); legacy is the count of legacy nudges that context also
// carries, after whose ack the drain stamps the delivery itself. A context
// with no room left takes no lane text: the pass does not run. The pass takes
// and writes a batch only while the deadline leaves time to record it
// (hookLaneRecordTime), and records its delivery by hookLaneSettleBy, which
// leaves the drain the archive margin to ack or release the legacy nudges.
// With nothing due it costs the one keyed probe and writes nothing. The drain
// rings the lane host itself (hookLaneRing) when it is done, and rings for
// the journal rows the pass wrote then too: sessions' emitter defers them
// (deferJournalRings).
func hookDrainLane(sessions beads.Store, conv string, knownDue bool, payload []byte, legacy, room int, write func(laneText string) error, stderr io.Writer) hookLaneOutcome {
	if !knownDue && !hookLaneDue(sessions, conv, stderr) {
		return hookLaneOutcome{}
	}
	if room <= 0 {
		fmt.Fprintf(stderr, "gc nudge drain: conversation lanes: conversation %s: the hook context leaves no room for lane notices; they wait for the next drain or the lane host\n", conv) //nolint:errcheck // best-effort stderr
		return hookLaneOutcome{}
	}
	ctx, cancel := hookLaneContext()
	defer cancel()
	return hookLanePass{
		sessions:       sessions,
		conv:           conv,
		fence:          hookLaneFence(),
		leaseTTL:       hookLaneLeaseTTL(time.Now()),
		settleBy:       hookLaneSettleBy(),
		deadline:       hookLaneDeadline(),
		room:           room,
		provider:       os.Getenv("GC_PROVIDER"),
		payload:        payload,
		clk:            clock.Real{},
		stderr:         stderr,
		emit:           write,
		recordTime:     hookLaneRecordTime,
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
