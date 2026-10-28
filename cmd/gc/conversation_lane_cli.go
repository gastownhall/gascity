package main

// gc session nudge as a conversation-lane producer, and the CLI inline pass
// (DES-O1 §7.5, §7.6; OJ-12x).
//
// When [ops] authoritative lists message, a nudge to a session row a lane
// host can type into (laneServesSession) is one executor=lane message op:
// no legacy op, no queue item, and ops.content does not govern it. A request
// whose op cannot be written is refused; the causes known before the target
// resolves (a store that cannot host lanes, an invalid key, a payload the
// journal refuses) are refused before the resolution, which may materialize
// a configured named session's row (cliLaneNudgePreflight).
//
// In its live delivery modes (wait-idle, immediate) the nudge then runs one
// ConversationLane pass over its conversation with the live executor, under
// lane host cli and a 30 s lease: a session at a boundary takes the nudge at
// once, with or without a running controller, and so does a busy one whose
// runtime an immediate nudge can steer (Caps.Steer). The pass never waits
// for a boundary; a busy session it cannot deliver to leaves the op to the
// controller's lane host. It is a full pass: it may close any op of the
// conversation by rule, run control before content, and carry other
// producers' notices in its batch. The CLI hosts no observer and writes no
// turn fact; it never requests a wake itself (the lane host's pass does,
// after the ring). Queue mode runs no pass and makes no runtime call.
//
// After it reports, a lane-routed nudge rings the controller's lane host
// (lane:<conversation>:<op id>) once, best effort, so the host refreshes the
// row in its cache and delivers what waited.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The CLI inline pass's lane (DES-O1 §7.6).
const (
	// cliLaneHostName names the CLI's lane in lease nonces and transitions
	// (lane:cli).
	cliLaneHostName = "cli"
	// cliLaneLeaseTTL is the CLI's lease: a CLI killed after its Take
	// returns the batch to the lane host this long (plus the skew) later.
	cliLaneLeaseTTL = 30 * time.Second
	// cliLaneRingTimeout bounds each leg of the CLI's ring.
	cliLaneRingTimeout = time.Second
	// cliReconcilerModeLeg bounds each leg of the inline pass's ask for the
	// controller's latched session reconciler (controllerStopReading).
	cliReconcilerModeLeg = 500 * time.Millisecond
)

// cliLaneExecutesMessage reports whether [ops] authoritative lists message.
func cliLaneExecutesMessage(cfg *config.City) bool {
	return cfg != nil && cfg.Ops.LaneExecutes(string(opjournal.KindMessage))
}

// laneServesSession reports whether a conversation lane can type into the
// session named sessName: a keystroke-mode runtime (laneTypesLive), or a
// protocol-mode one whose runtime row carries a host stamp serving message
// for view's generation (hostedElsewhere), whose lane then delivers.
func laneServesSession(sp runtime.Provider, st *opjournal.Store, sessName string, view opjournal.ConversationView) (bool, error) {
	if laneTypesLive(sp, sessName) {
		return true, nil
	}
	row, _, found, err := st.Runtime(view.ID)
	if err != nil {
		return false, fmt.Errorf("reading conversation %s's runtime row: %w", view.ID, err)
	}
	return found && hostedElsewhere(row.Host, view.Generation), nil
}

// cliLaneNudgePolicy is a lane nudge's policy for mode (DES-O1 §7.5):
// wait-idle a prompt at the boundary, immediate a prompt that steers and
// falls back to the boundary, queue a notice at the boundary that never wakes
// a session (a queued nudge never did). All are at_least_once.
func cliLaneNudgePolicy(mode nudgeDeliveryMode) opjournal.Policy {
	m := opjournal.MessagePolicy{Role: opjournal.RolePrompt, When: opjournal.WhenBoundary, Wake: opjournal.WakeIfNeeded}
	switch mode {
	case nudgeDeliveryImmediate:
		m.When, m.OnUnsupported = opjournal.WhenSteer, opjournal.FallbackBoundary
	case nudgeDeliveryQueue:
		m.Role, m.Wake = opjournal.RoleNotice, opjournal.WakeLiveOnly
	}
	return opjournal.Policy{Guarantee: opjournal.GuaranteeAtLeastOnce, Message: m}
}

// cliLaneCheckRequest is the journal's refusal of a lane nudge's key and
// payload, known before the target resolves: its code and message, or ok.
// It runs the journal's own key and payload checks; Propose stays the
// authority.
func cliLaneCheckRequest(key, payload string) (code, message string, ok bool) {
	if key != "" {
		if err := opjournal.ValidateClientKey(key); err != nil {
			return cliOpCodeInvalidKey, fmt.Sprintf("invalid --key (%v)", err), false
		}
	}
	if err := opjournal.ValidatePayload(opjournal.KindMessage, payload); err != nil {
		return cliOpCodeUnavailable, cliLaneUnavailable(err), false
	}
	return "", "", true
}

// cliLaneUnavailable is the refusal of a lane nudge whose op cannot be
// written.
func cliLaneUnavailable(cause any) string {
	return fmt.Sprintf("op journal cannot record this request (%v); the nudge did not run", cause)
}

// cliLaneNudgePreflight refuses, before the target resolves, a lane nudge
// whose op cannot be written: no nudge store, a sessions store that cannot
// host lanes or open as a lane store, an invalid key or a payload the
// journal refuses. It reports
// the refusal and answers errMessageOpRefused.
func cliLaneNudgePreflight(cityPath string, cfg *config.City, store beads.NudgesStore, req cliOpRequest) error {
	refusal := &cliOp{req: req}
	if store.Store == nil {
		refusal.refuse(cliOpCodeUnavailable, cliLaneUnavailable("the nudge store did not open"), "", req.key)
		return errMessageOpRefused
	}
	sessions := cliSessionStore(store.Store, cfg, cityPath)
	if err := laneStoreCapability(sessions); err != nil {
		refusal.refuse(cliOpCodeUnavailable, err.Error(), "", req.key)
		return errMessageOpRefused
	}
	if _, err := opjournal.NewLaneStore(beads.SessionStore{Store: sessions}, cliOpClock); err != nil {
		refusal.refuse(cliOpCodeUnavailable, cliLaneUnavailable(err), "", req.key)
		return errMessageOpRefused
	}
	if code, message, ok := cliLaneCheckRequest(req.key, req.payload); !ok {
		refusal.refuse(code, message, "", req.key)
		return errMessageOpRefused
	}
	return nil
}

// resolveSessionNudgeTarget is resolveNudgeTarget for gc session nudge,
// recorded as req: when [ops] authoritative lists message it first runs the
// lane preflight (cliLaneNudgePreflight), else OJ-5's (messageOpPreflight).
func resolveSessionNudgeTarget(identifier string, req cliOpRequest, warningWriter ...io.Writer) (nudgeTarget, error) {
	return resolveNudgeTargetAfter(identifier, false, func(cityPath string, cfg *config.City, store beads.NudgesStore) error {
		if cliLaneExecutesMessage(cfg) {
			return cliLaneNudgePreflight(cityPath, cfg, store, req)
		}
		return messageOpPreflight(cityPath, cfg, store, req)
	}, warningWriter...)
}

// cliLaneRing rings the controller's lane host for op opID of conversation
// conv and returns its reply; a seam for tests.
var cliLaneRing = func(cityPath, conv, opID string) (string, error) {
	reply, err := sendControllerCommandWithTimeouts(cityPath, laneSocketPrefix+conv+":"+opID, cliLaneRingTimeout, cliLaneRingTimeout, cliLaneRingTimeout)
	return strings.TrimSpace(string(reply)), err
}

// cliLaneNudge is one lane-routed gc session nudge.
type cliLaneNudge struct {
	target   nudgeTarget
	sessions beads.Store
	sp       runtime.Provider
	st       *opjournal.Store
	message  string
	mode     nudgeDeliveryMode
	asJSON   bool
	stdout   io.Writer
	stderr   io.Writer
	// key is the op's client key, --key or minted.
	key string
}

// refusal is the cliOp a lane nudge reports its refusals through, so its
// text and JSON envelope are OJ-5's.
func (n *cliLaneNudge) refusal() *cliOp {
	return &cliOp{req: cliOpRequest{verb: "gc session nudge", kind: opjournal.KindMessage, asJSON: n.asJSON, stdout: n.stdout, stderr: n.stderr}}
}

func (n *cliLaneNudge) warnf(format string, args ...any) {
	fmt.Fprintf(n.stderr, "gc session nudge: warning: "+format+"\n", args...) //nolint:errcheck // best-effort stderr
}

// deliverSessionNudgeLane carries out a nudge on the conversation lane when
// [ops] authoritative lists message, the target names a session row and a
// lane host can type into the session. It reports handled false, having
// written and printed nothing, when the nudge takes the legacy path instead.
func deliverSessionNudgeLane(target nudgeTarget, store beads.Store, sp runtime.Provider, message string, mode nudgeDeliveryMode, key string, jsonOutput bool, stdout, stderr io.Writer) (handled bool, code int) {
	if !cliLaneExecutesMessage(target.cfg) || target.sessionID == "" {
		return false, 0
	}
	n := &cliLaneNudge{target: target, sp: sp, message: message, mode: mode, asJSON: jsonOutput, stdout: stdout, stderr: stderr, key: key}
	if isNilBeadStore(store) {
		n.refusal().refuse(cliOpCodeUnavailable, cliLaneUnavailable("the nudge store did not open"), "", key)
		return true, 1
	}
	n.sessions = cliSessionStore(store, target.cfg, target.cityPath)
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: n.sessions}, cliOpClock)
	if err != nil {
		n.refusal().refuse(cliOpCodeUnavailable, cliLaneUnavailable(err), "", key)
		return true, 1
	}
	n.st = st
	view, absent, err := readConversationLaneViewOnly(sessionFrontDoor(n.sessions), target.sessionID)
	switch {
	case err != nil:
		n.refusal().refuse(cliOpCodeUnavailable, cliLaneUnavailable(fmt.Errorf("reading session %s: %w", target.sessionID, err)), "", key)
		return true, 1
	case absent:
		n.refusal().refuse(cliOpCodeUnavailable, cliLaneUnavailable(fmt.Sprintf("session %s has no session row", target.sessionID)), "", key)
		return true, 1
	}
	serves, err := laneServesSession(sp, st, target.sessionName, view)
	if err != nil {
		n.refusal().refuse(cliOpCodeUnavailable, cliLaneUnavailable(err), "", key)
		return true, 1
	}
	if !serves {
		return false, 0
	}
	return true, n.run()
}

// run proposes the op, runs the inline pass in a live mode, reports and
// rings.
func (n *cliLaneNudge) run() int {
	minted := n.key == ""
	if minted {
		k, err := legacyop.MintKey()
		if err != nil {
			n.refusal().refuse(cliOpCodeUnavailable, cliLaneUnavailable(err), "", "")
			return 1
		}
		n.key = k
	}
	principal, actor := cliPrincipal(cliOpLookupEnv)
	op, created, err := n.st.Propose(opjournal.Request{
		Conversation: n.target.sessionID,
		Kind:         opjournal.KindMessage,
		Executor:     opjournal.ExecutorLane,
		MintedKey:    minted,
		ClientKey:    n.key,
		Producer:     opjournal.Producer{Principal: principal, Actor: actor},
		Payload:      n.message,
		Policy:       cliLaneNudgePolicy(n.mode),
	})
	if err != nil {
		var reuse *opjournal.KeyReuseError
		if errors.As(err, &reuse) {
			n.refusal().refuse(cliOpCodeKeyMismatch, fmt.Sprintf("the key names a different request (op %s); rerun with another --key", reuse.Existing.ID), reuse.Existing.ID, n.key)
			return 1
		}
		n.refusal().refuse(cliOpCodeUnavailable, cliLaneUnavailable(err), "", n.key)
		return 1
	}
	laneFailpointHit(cliLaneHostName, laneFailPropose)
	if !created {
		if code, done := n.replay(op); done {
			return code
		}
	}
	var res opjournal.PassResult
	var passErr error
	if n.mode != nudgeDeliveryQueue {
		res, passErr = n.pass(context.Background())
	}
	code := n.report(op.ID, !created, res, passErr)
	n.ring(op.ID)
	return code
}

// replay answers a key that names an existing op: a closed op replays its
// outcome and runs nothing, an open legacy op is in progress under its own
// path, and an open lane op goes on as a created one would (done false). A
// legacy op answers the key gc session submit used, whose op a wait-idle lane
// nudge's request matches (api.MessageOpPolicy). The lane path never takes,
// closes or rewrites a legacy op. A delivery whose runtime answer broke the
// executor contract replays with the contract warning.
func (n *cliLaneNudge) replay(op opjournal.Op) (code int, done bool) {
	switch {
	case op.Executor != opjournal.ExecutorLane && op.Status != opjournal.StatusClosed:
		n.refusal().refuse(cliOpCodeInProgress, fmt.Sprintf("op %s is in progress under its legacy path; rerun later or without --key", op.ID), op.ID, n.key)
		return 1, true
	case op.Status != opjournal.StatusClosed:
		fmt.Fprintf(n.stderr, "gc session nudge: replayed op %s\n", op.ID) //nolint:errcheck // best-effort stderr
		return 0, false
	case !cliOpReplaysAsSuccess(op.Kind, op.Outcome):
		n.refusal().refuse(cliOpCodeAlreadyFailed, fmt.Sprintf("op %s closed %s; rerun without --key to act again", op.ID, cliOpOutcomeText(op.Outcome)), op.ID, n.key)
		return 1, true
	}
	fmt.Fprintf(n.stderr, "gc session nudge: replayed op %s\n", op.ID) //nolint:errcheck // best-effort stderr
	if op.Executor != opjournal.ExecutorLane && op.Outcome.Result.Via == legacyop.ViaLegacyQueued {
		return n.writeQueued(op.ID, true, " in "+openNudgeQueue(n.target.cityPath, beads.NudgesStore{}).Location), true
	}
	if op.Outcome.Degradation == opjournal.DegradationExecutorContract {
		n.warnContractBreach(op.ID, "the op records degradation "+op.Outcome.Degradation)
	}
	return n.writeDelivered(op.ID, true), true
}

// pass runs one inline pass over the conversation (DES-O1 §7.6): the live
// executor under the CLI's lease, one Pass and no loop.
func (n *cliLaneNudge) pass(ctx context.Context) (opjournal.PassResult, error) {
	lane, err := opjournal.NewLane(n.st, cliLaneHostName, cliOpClock)
	if err != nil {
		return opjournal.PassResult{}, err
	}
	armLaneFailpoints(lane, cliLaneHostName)
	// The CLI holds no latch, so it reads the stop request by the mode the
	// controller latched, asked right before the row read so the answer is
	// as fresh as the row: a request it holds back waits for the
	// controller's lane host, which reads by the same mode.
	reading, modeErr := controllerStopReading(n.target.cityPath, cliReconcilerModeLeg)
	view, row, _, err := readConversationLaneView(sessionFrontDoor(n.sessions), n.target.sessionID, reading)
	if err != nil {
		return opjournal.PassResult{}, fmt.Errorf("reading conversation %s: %w", n.target.sessionID, err)
	}
	if note := unknownModeStopNote(row, modeErr); note != "" {
		n.warnf("%s", note)
	}
	live := newConversationLaneExec(laneExecDeps{
		cityPath: n.target.cityPath,
		cfg:      n.target.cfg,
		sessions: n.sessions,
		sp:       n.sp,
		lane:     n.st,
		clk:      cliOpClock,
	}, row)
	exec := &cliLaneExec{conversationLaneExec: live, ownsInput: live.target.sessionTransport() != "acp"}
	res, err := lane.Pass(ctx, view, exec)
	laneFailpointAfterPass(cliLaneHostName, res)
	return res, err
}

// report prints the op's state after this run, read from its row, and
// returns the exit code. A pass error is a warning: the op is accepted. So
// is a delivery this pass took whose outcome the runtime could not confirm:
// the Done released the op with the attempt maybe delivered, and the lane
// host delivers it again, marked as a possible repeat.
func (n *cliLaneNudge) report(id string, replayed bool, res opjournal.PassResult, passErr error) int {
	op, err := n.st.Get(id)
	if err != nil {
		n.warnf("reading op %s after the inline delivery failed (%v); it waits for the controller", id, err)
		return n.writeQueued(id, replayed, " in the conversation lane")
	}
	taken := slices.Contains(res.Taken, id)
	if passErr != nil {
		n.warnPassError(op, taken, res.Done, passErr)
	}
	if taken && res.Done == opjournal.DoneReleased && op.Status != opjournal.StatusClosed && op.MaybeDelivered == op.Attempt {
		n.warnf("the delivery of op %s may have reached the session but was not confirmed (%s); the controller delivers it again after its retry backoff, marked as a possible repeat", id, op.LastError)
	}
	switch {
	case op.Status != opjournal.StatusClosed:
		return n.writeQueued(id, replayed, " in the conversation lane"+cliLaneQueuedNote(op, res))
	case op.Outcome.Arm == opjournal.ArmSucceeded:
		return n.writeDelivered(id, replayed)
	default:
		n.refusal().refuse(cliOpCodeFailed, fmt.Sprintf("op %s closed %s", id, cliOpOutcomeText(op.Outcome)), id, n.key)
		return 1
	}
}

// warnPassError warns of the inline pass's error, given op as read after it,
// whether the pass took op and how its Done ended.
func (n *cliLaneNudge) warnPassError(op opjournal.Op, taken bool, done opjournal.DoneArm, passErr error) {
	switch {
	case taken && done != "":
		// The Done landed, so the error is the executor's answer breaking
		// its contract.
		n.warnContractBreach(op.ID, passErr)
	case op.Status == opjournal.StatusClosed:
	case taken:
		n.warnf("recording the delivery of op %s failed (%v); the controller takes it again after its lease lapses, and it may repeat (recorded on the op)", op.ID, passErr)
	case op.Status == opjournal.StatusExecuting:
		n.warnf("the inline delivery did not run (%v); another lane host holds it, and op %s is taken again if that delivery does not finish", passErr, op.ID)
	default:
		n.warnf("the inline delivery did not run (%v); op %s waits for the controller", passErr, op.ID)
	}
}

// warnContractBreach warns that the runtime's answer to the delivery of op
// id broke the executor contract; why is the breach, or on a replay the op's
// record of it.
func (n *cliLaneNudge) warnContractBreach(id string, why any) {
	n.warnf("the runtime's answer to the delivery of op %s broke the executor contract (%v)", id, why)
}

// ring rings the lane host for op id once, after the result is written, and
// warns when the op is still open and no lane host answered.
func (n *cliLaneNudge) ring(id string) {
	reply, err := cliLaneRing(n.target.cityPath, n.target.sessionID, id)
	if err == nil && reply == "ok" {
		return
	}
	op, gerr := n.st.Get(id)
	if gerr == nil && op.Status == opjournal.StatusClosed {
		return
	}
	why := reply
	if err != nil {
		why = err.Error()
	}
	n.warnf("no controller lane host answered (%s); op %s waits in the conversation lane for a lane host serving the session", why, id)
}

func (n *cliLaneNudge) writeDelivered(id string, replayed bool) int {
	if n.asJSON {
		return writeCLIJSONLineOrExit(n.stdout, n.stderr, "gc session nudge", n.json(false, id, replayed))
	}
	fmt.Fprintf(n.stdout, "Nudged %s\n", n.target.agentKey()) //nolint:errcheck // best-effort stdout
	return 0
}

// writeQueued reports a queued nudge; where is the rest of the plain line
// after the target (" in the conversation lane" and its note).
func (n *cliLaneNudge) writeQueued(id string, replayed bool, where string) int {
	if n.asJSON {
		return writeCLIJSONLineOrExit(n.stdout, n.stderr, "gc session nudge", n.json(true, id, replayed))
	}
	fmt.Fprintf(n.stdout, "Queued nudge for %s%s\n", n.target.agentKey(), where) //nolint:errcheck // best-effort stdout
	return 0
}

func (n *cliLaneNudge) json(queued bool, id string, replayed bool) sessionNudgeJSON {
	outcome := "delivered"
	if queued {
		outcome = "queued"
	}
	return sessionNudgeJSON{
		SchemaVersion: "1",
		OK:            true,
		Target:        n.target.agentKey(),
		SessionID:     n.target.sessionID,
		SessionName:   n.target.sessionName,
		Delivery:      string(n.mode),
		Queued:        queued,
		Outcome:       outcome,
		OpID:          id,
		Key:           n.key,
		Replayed:      replayed,
	}
}

// cliLaneQueuedNote says why the pass left op waiting, from its wait in
// res; none in queue mode, which runs no pass, and none for a wait it does
// not name.
func cliLaneQueuedNote(op opjournal.Op, res opjournal.PassResult) string {
	for _, w := range res.Waits {
		if w.ID != op.ID {
			continue
		}
		switch w.Reason {
		case opjournal.WaitBusy, opjournal.WaitBoundaryUnknown:
			return " (the session is mid-turn; the controller delivers it at the next idle boundary)"
		case opjournal.WaitPendingInteraction:
			return " (the session is waiting on a permission prompt; the controller delivers it after the prompt is answered)"
		case opjournal.WaitNotRunning, opjournal.WaitDormant, opjournal.WaitNoSession:
			if op.Policy.Message.Wake == opjournal.WakeLiveOnly {
				return " (the session is not running; the nudge is delivered the next time it runs)"
			}
			return " (the session is not running; the controller wakes it to deliver the nudge)"
		case opjournal.WaitContentBlock:
			return fmt.Sprintf(" (the session cannot take input now: %s; the controller delivers it once that clears)", w.Detail)
		case opjournal.WaitContentHold:
			return " (input to the session is on hold; the controller delivers it once the hold is lifted)"
		case opjournal.WaitNotLive:
			return " (this process cannot type into the session; the session's lane host delivers it)"
		case opjournal.WaitInFlight:
			if op.Status == opjournal.StatusExecuting {
				return " (a delivery of this nudge is in flight; if it does not finish, the controller delivers it again after its lease lapses)"
			}
			return " (another delivery to the session is in flight; the controller delivers this one after it)"
		case opjournal.WaitUnavailable:
			return " (the session's runtime did not answer; the controller delivers it once it does)"
		case opjournal.WaitIncarnation:
			return " (the session's runtime is being replaced; the controller delivers it to the new one)"
		case opjournal.WaitBlocked, opjournal.WaitBatchFull, opjournal.WaitRetryBackoff, opjournal.WaitAfterPending:
			return " (earlier input to the session goes first; the controller delivers it after that)"
		}
		return ""
	}
	return ""
}

// cliLaneExec is the live executor as the CLI hosts it: the CLI's lease,
// and no live input where this process cannot reach the session's input.
type cliLaneExec struct {
	*conversationLaneExec
	// ownsInput is false for an ACP session, whose connection the
	// controller holds.
	ownsInput bool
}

var _ opjournal.Executor = (*cliLaneExec)(nil)

// Probe implements opjournal.Executor: without the session's input it
// reports a runtime this process cannot type into, so the pass takes
// nothing and asks no wake; otherwise the live executor's probe under the
// CLI's lease.
func (e *cliLaneExec) Probe(ctx context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	if !e.ownsInput {
		return opjournal.Probe{
			Available: true,
			Fence:     laneFence(conv),
			TurnLive:  opjournal.TriUnknown,
			Caps:      opjournal.Caps{Input: opjournal.FenceNone, Text: opjournal.TextReminder, LeaseTTL: cliLaneLeaseTTL},
		}, nil
	}
	p, err := e.conversationLaneExec.Probe(ctx, conv)
	if err != nil {
		return p, err
	}
	p.Caps.LeaseTTL = cliLaneLeaseTTL
	return p, nil
}
