package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// prompt shows a pending approval with request id req in the fixture's
// runtime.
func (f *laneFixture) prompt(req string) {
	f.sp.SetPendingInteraction(f.name, &runtime.PendingInteraction{RequestID: req, Kind: "approval", Options: []string{"approve", "deny"}})
}

// laneTestRequest is the request id of the prompts the respond tests show.
const laneTestRequest = "tmux-abc"

// respond records the raising of laneTestRequest the runtime shows, as a
// producer does, and proposes the operator's answer to it.
func (f *laneFixture) respond(t *testing.T, st *opjournal.Store, p opjournal.RespondPayload) (opjournal.Op, opjournal.Fact) {
	t.Helper()
	r, _, err := st.RecordRaising(f.id, 1, laneTestRequest, opjournal.FactBasisObserved, f.clk.Now())
	if err != nil {
		t.Fatalf("RecordRaising: %v", err)
	}
	key, err := opjournal.RespondKey(r.Raised)
	if err != nil {
		t.Fatalf("RespondKey: %v", err)
	}
	payload, err := opjournal.EncodeRespondPayload(p)
	if err != nil {
		t.Fatalf("EncodeRespondPayload: %v", err)
	}
	return f.propose(t, st, opjournal.Request{
		Conversation: f.id, Kind: opjournal.KindRespond, Executor: opjournal.ExecutorLane, ClientKey: key,
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator}, Payload: payload,
		Scope: opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1, Interaction: r.Raised.Interaction},
	}), r.Raised
}

// respondCall records the raising of laneTestRequest, proposes the
// operator's answer to it and returns the call a pass over v makes for it.
func (f *laneFixture) respondCall(t *testing.T, st *opjournal.Store, v opjournal.ConversationView) opjournal.RespondCall {
	t.Helper()
	answer := opjournal.RespondPayload{Action: "approve"}
	op, raised := f.respond(t, st, answer)
	return opjournal.RespondCall{Fence: laneFence(v), Op: op, Interaction: raised.Interaction, RequestID: laneTestRequest, Response: answer}
}

// responses are the answers the fake runtime received.
func (f *laneFixture) responses() []runtime.Call {
	var out []runtime.Call
	for _, c := range f.sp.Calls {
		if c.Method == "Respond" {
			out = append(out, c)
		}
	}
	return out
}

// TestLaneExecRespondAnswersTheRaising proves a lane respond reaches the
// runtime with its request id, text and metadata set, fenced to the view's
// incarnation where the runtime fences answers, closes succeeded and
// resolves its occurrence. A runtime that cannot fence answers, or a row
// with no token, gets the answer unfenced, recorded as unfenced_delivery.
func TestLaneExecRespondAnswersTheRaising(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    []laneFixtureOpt
		fencing runtime.InputFencing
		fenced  bool
	}{
		{name: "fenced", fenced: true},
		{name: "runtime cannot fence", fencing: runtime.InputFencingNone},
		{name: "no token", opts: []laneFixtureOpt{noLaneToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t, tc.opts...)
			f.sp.InputFencingValue = tc.fencing
			st := f.laneStore(t)
			f.prompt("tmux-abc")
			op, raised := f.respond(t, st, opjournal.RespondPayload{Action: "approve", Text: "ok", Metadata: map[string]string{"source": "lane"}})
			res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
			if err != nil || res.Took != opjournal.TakeControl || res.Done != opjournal.DoneAccepted {
				t.Fatalf("Pass = %+v, %v; want an accepted respond", res, err)
			}
			calls := f.responses()
			if len(calls) != 1 || calls[0].RequestID != "tmux-abc" || calls[0].Action != "approve" || calls[0].Message != "ok" || calls[0].Fence.IsZero() == tc.fenced {
				t.Fatalf("runtime responses %+v; want one, fenced %v", calls, tc.fenced)
			}
			if tc.fenced && (calls[0].Fence.Token != f.token || calls[0].Fence.Generation != 1) {
				t.Fatalf("answer fenced to %v, want the view's incarnation", calls[0].Fence)
			}
			if got := f.sp.Responses[f.name]; len(got) != 1 || got[0].Metadata["source"] != "lane" {
				t.Fatalf("runtime answers %+v; want the payload's metadata", got)
			}
			want := opjournal.DegradationUnfencedDelivery
			if tc.fenced {
				want = ""
			}
			got := f.get(t, st, op.ID)
			if got.Outcome.Arm != opjournal.ArmSucceeded || got.Outcome.Result.Via != laneViaLive || got.Outcome.Degradation != want {
				t.Fatalf("op outcome %+v; want succeeded via live with degradation %q", got.Outcome, want)
			}
			if latest, found, err := st.LatestRaising(f.id, 1, "tmux-abc"); err != nil || !found || !latest.Resolved || latest.Raised.ID != raised.ID {
				t.Fatalf("LatestRaising = %+v, found %v, %v; want the answered raising resolved", latest, found, err)
			}
		})
	}
}

// TestLaneExecRespondFenceMissSendsNothing proves an answer whose fence the
// runtime's incarnation does not hold (a respawn between the probe and the
// key) reaches no prompt: the op is released, not closed, and retries.
func TestLaneExecRespondFenceMissSendsNothing(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	f.prompt("tmux-abc")
	op, _ := f.respond(t, st, opjournal.RespondPayload{Action: "approve"})
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		return promptSwapExec{conversationLaneExec: e, swap: func() {
			// The session respawned as another incarnation under its name.
			f.sp.Zombies[f.name] = true
		}}
	})
	if err != nil || res.Done != opjournal.DoneReleased {
		t.Fatalf("Pass = %+v, %v; want the respond released", res, err)
	}
	if n := len(f.responses()); n != 0 {
		t.Fatalf("%d answers reached the runtime", n)
	}
	if got := f.get(t, st, op.ID); got.Status == opjournal.StatusClosed || got.MaybeDelivered != 0 {
		t.Fatalf("op %s maybe %d %+v; want it open with nothing delivered", got.Status, got.MaybeDelivered, got.Outcome)
	}
}

// TestLaneExecTwoIdenticalPromptsGetTwoAnswers proves the second raising of
// a content-hash request id gets its own answer, not a replay of the first.
func TestLaneExecTwoIdenticalPromptsGetTwoAnswers(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	l := f.lane(t, st)
	f.prompt("tmux-abc")
	first, _ := f.respond(t, st, opjournal.RespondPayload{Action: "approve"})
	if res, err := f.pass(context.Background(), t, l, st, nil); err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("first Pass = %+v, %v", res, err)
	}
	f.clk.Advance(time.Second)
	f.prompt("tmux-abc")
	second, raised := f.respond(t, st, opjournal.RespondPayload{Action: "deny"})
	if second.ID == first.ID || raised.Interaction != "tmux-abc#2" {
		t.Fatalf("second answer %s to %s; first %s", second.ID, raised.Interaction, first.ID)
	}
	if res, err := f.pass(context.Background(), t, l, st, nil); err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("second Pass = %+v, %v", res, err)
	}
	calls := f.responses()
	if len(calls) != 2 || calls[1].RequestID != "tmux-abc" || calls[1].Action != "deny" {
		t.Fatalf("runtime responses %+v", calls)
	}
}

// promptSwapExec swaps the runtime's prompt between the probe and the
// answer, as a prompt that changes under the lane would.
type promptSwapExec struct {
	*conversationLaneExec
	swap func()
}

func (e promptSwapExec) Respond(ctx context.Context, c opjournal.RespondCall) opjournal.ControlResult {
	e.swap()
	return e.conversationLaneExec.Respond(ctx, c)
}

// TestLaneExecRespondStaleWhenThePromptChanged proves an answer whose
// request left the runtime after the probe sends nothing and closes
// skipped{stale}, resolving the occurrence so an identical later prompt is a
// new one.
func TestLaneExecRespondStaleWhenThePromptChanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		swap func(f *laneFixture)
	}{
		{"another prompt", func(f *laneFixture) { f.prompt("tmux-other") }},
		{"no prompt", func(f *laneFixture) { f.sp.SetPendingInteraction(f.name, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			st := f.laneStore(t)
			f.prompt("tmux-abc")
			op, _ := f.respond(t, st, opjournal.RespondPayload{Action: "approve"})
			res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
				return promptSwapExec{conversationLaneExec: e, swap: func() { tc.swap(f) }}
			})
			if err != nil || res.Done != opjournal.DoneRejected {
				t.Fatalf("Pass = %+v, %v; want a rejected respond", res, err)
			}
			if n := len(f.responses()); n != 0 {
				t.Fatalf("%d answers reached the runtime", n)
			}
			if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSkipped || got.Outcome.Reason != opjournal.ReasonStale {
				t.Fatalf("op outcome %+v", got.Outcome)
			}
			if r, _, err := st.RecordRaising(f.id, 1, "tmux-abc", opjournal.FactBasisObserved, f.clk.Now()); err != nil || r.Raised.Interaction != "tmux-abc#2" {
				t.Fatalf("the next identical prompt is %q, %v", r.Raised.Interaction, err)
			}
		})
	}
}

// TestLaneExecRespondAnswersBehindAContentGate proves the content gate does
// not hold control (DES-O1 I7, §10.5): a draining or restart-pending session
// blocked on a prompt gets its answer, as the legacy respond path gives it,
// while content still waits on the gate.
func TestLaneExecRespondAnswersBehindAContentGate(t *testing.T) {
	for _, gate := range []laneFixtureOpt{
		func(meta map[string]string) { meta["restart_requested"] = "true" },
		func(meta map[string]string) { meta["state"] = "draining" },
	} {
		f := newLaneFixture(t, gate)
		st := f.laneStore(t)
		f.prompt("tmux-abc")
		msg := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
		op, _ := f.respond(t, st, opjournal.RespondPayload{Action: "approve"})
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
		if err != nil || res.Took != opjournal.TakeControl || res.Done != opjournal.DoneAccepted || len(f.responses()) != 1 {
			t.Fatalf("Pass = %+v, %v; want the gated session's prompt answered", res, err)
		}
		if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("respond %+v", got.Outcome)
		}
		if f.get(t, st, msg.ID).Status == opjournal.StatusClosed || len(f.nudges()) != 0 {
			t.Fatal("content went past the gate")
		}
	}
}

// TestLaneExecRespondWaitsOnAnotherIncarnation proves a running runtime of
// another incarnation than the row names reads its pending interaction as
// unknown: the respond waits instead of taking the prompt for gone, and
// nothing is answered.
func TestLaneExecRespondWaitsOnAnotherIncarnation(t *testing.T) {
	f := newLaneFixture(t)
	if err := f.sp.SetMeta(f.name, "GC_SESSION_ID", "gc-someone-else"); err != nil {
		t.Fatal(err)
	}
	st := f.laneStore(t)
	f.prompt("tmux-abc")
	op, _ := f.respond(t, st, opjournal.RespondPayload{Action: "approve"})
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
	if err != nil || res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitInteractionUnknown) {
		t.Fatalf("Pass = %+v, %v; want an interaction_unknown wait", res, err)
	}
	if f.get(t, st, op.ID).Status == opjournal.StatusClosed || len(f.responses()) != 0 {
		t.Fatal("a respond to another incarnation closed or answered")
	}
}

// errLaneTestRead is a store read that failed.
var errLaneTestRead = errors.New("store read failed")

// laneFailingGets fails Get of the ids fail names; every other call reaches
// the wrapped store, which also serves the op store's conditional writes and
// id prefix.
type laneFailingGets struct {
	beads.Store
	fail func(id string) bool
}

func (s laneFailingGets) Get(id string) (beads.Bead, error) {
	if s.fail(id) {
		return beads.Bead{}, errLaneTestRead
	}
	return s.Store.Get(id)
}

func (s laneFailingGets) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// TestLaneExecRespondRefusesWithoutAnAnswer proves the executor's own checks
// before the answer: no handle from the probe, a pending look that fails, a
// session that takes no responses, an occurrence that cannot be read or no
// op store to read it from, each sending nothing.
func TestLaneExecRespondRefusesWithoutAnAnswer(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	v, info := f.view(t)
	call := f.respondCall(t, st, v)
	e := newConversationLaneExec(f.deps(st), info)
	if got, ok := e.Respond(context.Background(), call).(opjournal.NotDelivered); !ok || !got.Transient || !errors.Is(got.Cause, errLaneNoProbedHandle) {
		t.Fatalf("Respond without a probe = %#v, want a transient NotDelivered", got)
	}
	f.prompt("tmux-abc")
	if _, err := e.Probe(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	f.sp.PendingErrors[f.name] = errors.New("capture failed")
	if got, ok := e.Respond(context.Background(), call).(opjournal.NotDelivered); !ok || !got.Transient {
		t.Fatalf("Respond over a failed look = %#v, want a transient NotDelivered", got)
	}
	f.sp.PendingErrors[f.name] = runtime.ErrInteractionUnsupported
	if got := e.Respond(context.Background(), call); got != (opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}) {
		t.Fatalf("Respond to a session that takes no responses = %#v", got)
	}
	delete(f.sp.PendingErrors, f.name)
	resolved, err := opjournal.FactID(st.Prefix(), opjournal.Fact{
		Kind: opjournal.FactInteractionResolved, Conversation: f.id, Generation: 1, Interaction: call.Interaction,
	})
	if err != nil {
		t.Fatal(err)
	}
	unreadable, err := opjournal.NewStore(beads.SessionStore{Store: laneFailingGets{Store: f.store, fail: func(id string) bool { return id == resolved }}}, f.clk)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, tc := range []struct {
		name  string
		lane  *opjournal.Store
		cause error
	}{
		{"occurrence unreadable", unreadable, errLaneTestRead},
		{"no op store", nil, errLaneNoOpStore},
	} {
		deps := f.deps(st)
		deps.lane = tc.lane
		e := newConversationLaneExec(deps, info)
		if _, err := e.Probe(context.Background(), v); err != nil {
			t.Fatalf("%s: Probe: %v", tc.name, err)
		}
		if got, ok := e.Respond(context.Background(), call).(opjournal.NotDelivered); !ok || !got.Transient || !errors.Is(got.Cause, tc.cause) {
			t.Fatalf("%s: Respond = %#v, want a transient NotDelivered for %v", tc.name, got, tc.cause)
		}
	}
	if n := len(f.responses()); n != 0 {
		t.Fatalf("%d answers reached the runtime", n)
	}
}

// TestLaneExecRespondCaps proves the probe reports respond for a live
// interaction provider, and clears it when the session says it takes no
// responses.
func TestLaneExecRespondCaps(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	v, info := f.view(t)
	p, err := newConversationLaneExec(f.deps(st), info).Probe(context.Background(), v)
	if err != nil || !p.Caps.Respond {
		t.Fatalf("Probe = caps %+v, %v; want respond", p.Caps, err)
	}
	f.sp.PendingErrors[f.name] = runtime.ErrInteractionUnsupported
	p, err = newConversationLaneExec(f.deps(st), info).Probe(context.Background(), v)
	if err != nil || p.Caps.Respond {
		t.Fatalf("Probe = caps %+v, %v; want no respond for an unsupported session", p.Caps, err)
	}
	f.sp.PendingErrors[f.name] = errors.New("capture failed")
	if _, err = newConversationLaneExec(f.deps(st), info).Probe(context.Background(), v); err == nil {
		t.Fatal("Probe on a failed look = nil error; want the look's error, not a pending interaction")
	}
}

// TestLaneExecRespondUnsupportedSessionFails proves a respond on a session
// that takes no responses closes failed{unsupported}, a capability answer,
// and ends its occurrence: the probe names no prompt there, and that is not
// a look showing the prompt gone (which would close skipped{stale}).
func TestLaneExecRespondUnsupportedSessionFails(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	l := f.lane(t, st)
	f.prompt(laneTestRequest)
	op, raised := f.respond(t, st, opjournal.RespondPayload{Action: "approve"})
	f.sp.PendingErrors[f.name] = runtime.ErrInteractionUnsupported
	if _, err := f.pass(context.Background(), t, l, st, nil); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmFailed || got.Outcome.Reason != opjournal.ReasonUnsupported {
		t.Fatalf("op %s %+v, want failed{unsupported}", got.Status, got.Outcome)
	}
	if latest, found, err := st.LatestRaising(f.id, 1, laneTestRequest); err != nil || !found || !latest.Resolved || latest.Raised.ID != raised.ID {
		t.Fatalf("latest raising %+v found %v, %v; want %s ended", latest, found, err, raised.Interaction)
	}
	if n := len(f.responses()); n != 0 {
		t.Fatalf("%d answers reached the runtime", n)
	}
}

// TestLaneRespondResultMapping pins how a response's error maps to the
// lane's result, over the errors tmux and the session layer return: every
// refusal that sent nothing is definitive or transient, and only an error
// after the key may have landed is Unknown.
func TestLaneRespondResultMapping(t *testing.T) {
	stale := opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	refused := opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: laneRespondRefusedAction}
	mismatch := func(err error) error { return fmt.Errorf("%w: %w", session.ErrInteractionMismatch, err) }
	for _, tc := range []struct {
		name string
		err  error
		want opjournal.ControlResult
	}{
		{"session unsupported", session.ErrInteractionUnsupported, opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}},
		{"runtime unsupported", runtime.ErrInteractionUnsupported, opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}},
		{"action unavailable", mismatch(fmt.Errorf("%w: 0 menu options match action %q", runtime.ErrInteractionActionUnavailable, "deny")), refused},
		{"unknown action", mismatch(fmt.Errorf("%w: unknown action %q", runtime.ErrInteractionResponseInvalid, "answer")), refused},
		{"request mismatch", fmt.Errorf("%w: pending interaction %q does not match request %q", session.ErrInteractionMismatch, "a", "b"), stale},
		{"prompt changed", mismatch(fmt.Errorf("approval prompt changed: expected a, got b: %w", runtime.ErrInteractionGone)), stale},
		{"prompt gone", mismatch(fmt.Errorf("approval a is gone: %w", runtime.ErrInteractionGone)), stale},
		{"runtime prompt gone", fmt.Errorf("approval a is gone: %w", runtime.ErrInteractionGone), stale},
		// ACP: a permission answered or canceled between the look and the
		// answer names no outstanding request; an option it does not offer
		// is the caller's.
		{"acp request gone", mismatch(fmt.Errorf("no outstanding permission request %q: %w", "acp-7", runtime.ErrInteractionGone)), stale},
		{"acp option not offered", mismatch(fmt.Errorf("%w: action %q has no allow_always option for request %q", runtime.ErrInteractionResponseInvalid, "approve_always", "acp-7")), refused},
		{"no pending", session.ErrNoPendingInteraction, stale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := laneRespondResult(context.Background(), tc.err, true); got != tc.want {
				t.Fatalf("laneRespondResult(%v) = %#v, want %#v", tc.err, got, tc.want)
			}
		})
	}
	if got, want := laneRespondResult(context.Background(), nil, true), (opjournal.Settled{Via: laneViaLive, Degradation: opjournal.DegradationUnfencedDelivery}); !reflect.DeepEqual(got, want) {
		t.Fatalf("unfenced answer = %#v, want %#v", got, want)
	}
	if got, want := laneRespondResult(context.Background(), nil, false), (opjournal.Settled{Via: laneViaLive}); !reflect.DeepEqual(got, want) {
		t.Fatalf("fenced answer = %#v, want %#v", got, want)
	}
	notSent := func(cause error) error {
		return fmt.Errorf("fenced input to %q: %w: %w", "s", runtime.ErrInputNotSent, cause)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Nothing was sent, whether or not the call's context has ended.
	for _, err := range []error{
		runtime.ErrSessionNotFound, session.ErrSessionNotFound, runtime.ErrRuntimeUnavailable,
		fmt.Errorf("responding to pending interaction: pre-verify capture failed: %w: %w", runtime.ErrRuntimeUnavailable, errors.New("server busy")),
		fmt.Errorf("responding to pending interaction: send-keys failed: %w: %w", runtime.ErrRuntimeUnavailable, errors.New("no tmux server running")),
		fmt.Errorf("responding to pending interaction: %w", &runtime.FenceMismatchError{Session: "s", Found: runtime.FenceFoundDead}),
		fmt.Errorf("session %q: %w", "s", runtime.ErrInputFenceUnsupported),
		fmt.Errorf("%w: generation 0", runtime.ErrInvalidInputFence),
		notSent(fmt.Errorf("reading the agent pane: %w", context.DeadlineExceeded)),
		fmt.Errorf("send-keys failed: %w", notSent(fmt.Errorf("loading tmux paste buffer: %w", context.Canceled))),
	} {
		for _, c := range []context.Context{context.Background(), ctx} {
			res := laneRespondResult(c, err, false)
			if got, ok := res.(opjournal.NotDelivered); !ok || !got.Transient {
				t.Fatalf("laneRespondResult(%v) under ctx err %v = %#v, want a transient NotDelivered", err, c.Err(), res)
			}
		}
	}
	// The key may have landed: after it, or while its guarded paste ran.
	for _, err := range []error{
		fmt.Errorf("responding to pending interaction: approval prompt did not clear within %s of the answer", "10s"),
		fmt.Errorf("responding to pending interaction: %w", &runtime.FenceMismatchError{Session: "s", Found: runtime.FenceFoundDead, Partial: true}),
		fmt.Errorf("responding to pending interaction: send-keys failed: %w", context.DeadlineExceeded),
		context.Canceled,
	} {
		if _, ok := laneRespondResult(ctx, err, false).(opjournal.Unknown); !ok {
			t.Fatalf("laneRespondResult(%v) is not Unknown", err)
		}
	}
}

// TestLaneExecRespondRestartDoesNotReplayTheAnswer proves an answer made
// before the runtime stopped never reaches the identical prompt it raises
// after a restart in the same generation: the pass that saw the runtime not
// running ended the answer's occurrence.
func TestLaneExecRespondRestartDoesNotReplayTheAnswer(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	l := f.lane(t, st)
	f.prompt(laneTestRequest)
	op, raised := f.respond(t, st, opjournal.RespondPayload{Action: "approve"})
	if err := f.sp.Stop(f.name); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pass(context.Background(), t, l, st, nil); err != nil {
		t.Fatalf("pass over a stopped runtime: %v", err)
	}
	f.started = false
	f.startRuntime(t, f.token)
	f.prompt(laneTestRequest)
	if _, err := f.pass(context.Background(), t, l, st, nil); err != nil {
		t.Fatalf("pass after the restart: %v", err)
	}
	if calls := f.responses(); len(calls) != 0 {
		t.Fatalf("the answer to %s reached the re-raised prompt: %+v", raised.Interaction, calls)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSkipped || got.Outcome.Reason != opjournal.ReasonStale {
		t.Fatalf("op %+v; want skipped{stale}", got.Outcome)
	}
}

// TestLaneExecRespondHoldsTheSessionMutationLock proves the lane answers
// under the lock legacy Manager.Respond takes, so the two paths never both
// pass the pre-verify and type two keys: while the lock is held past the
// call's deadline nothing is sent and the op retries.
func TestLaneExecRespondHoldsTheSessionMutationLock(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	v, info := f.view(t)
	call := f.respondCall(t, st, v)
	e := newConversationLaneExec(f.deps(st), info)
	f.prompt("tmux-abc")
	if _, err := e.Probe(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- session.WithSessionMutationLock(info.ID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	got, ok := e.Respond(ctx, call).(opjournal.NotDelivered)
	cancel()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !ok || !got.Transient || !errors.Is(got.Cause, session.ErrSessionMutationBusy) {
		t.Fatalf("Respond under a held lock = %#v, want a transient NotDelivered", got)
	}
	if n := len(f.responses()); n != 0 {
		t.Fatalf("%d answers reached the runtime under a held lock", n)
	}
	if res := e.Respond(context.Background(), call); !reflect.DeepEqual(res, opjournal.Settled{Via: laneViaLive}) {
		t.Fatalf("Respond once the lock is free = %#v", res)
	}
}

// pendingHook runs onPending at each pending look, before the fake answers
// it: a test ends the call's context during the re-check.
type pendingHook struct {
	*runtime.Fake
	onPending func()
}

func (p *pendingHook) Pending(name string) (*runtime.PendingInteraction, error) {
	if p.onPending != nil {
		p.onPending()
	}
	return p.Fake.Pending(name)
}

// TestLaneExecRespondDeadlineAfterTheRecheckSendsNothing proves a call
// whose context ends during the pending re-check (a slow capture spending
// the lease's budget) sends no answer and retries: the respond stays open,
// never closed ambiguous, so its occurrence is not spent unanswered.
func TestLaneExecRespondDeadlineAfterTheRecheckSendsNothing(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	v, info := f.view(t)
	call := f.respondCall(t, st, v)
	deps := f.deps(st)
	hook := &pendingHook{Fake: f.sp}
	deps.sp = hook
	e := newConversationLaneExec(deps, info)
	f.prompt("tmux-abc")
	if _, err := e.Probe(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hook.onPending = cancel
	res := e.Respond(ctx, call)
	if got, ok := res.(opjournal.NotDelivered); !ok || !got.Transient || !errors.Is(got.Cause, context.Canceled) {
		t.Fatalf("Respond past its deadline = %#v, want a transient NotDelivered", res)
	}
	if n := len(f.responses()); n != 0 {
		t.Fatalf("%d answers reached the runtime past the deadline", n)
	}
	hook.onPending = nil
	if res := e.Respond(context.Background(), call); !reflect.DeepEqual(res, opjournal.Settled{Via: laneViaLive}) {
		t.Fatalf("Respond within its deadline = %#v", res)
	}
}

// noInteractions hides the fake provider's interaction methods: a runtime
// that is no InteractionProvider.
type noInteractions struct{ runtime.Provider }

// TestLaneExecRespondCapsFromTheHandle proves the probe takes respond from
// the session's own answer (PendingStatus), not from the provider's type: a
// provider with no interactions reads no respond.
func TestLaneExecRespondCapsFromTheHandle(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	v, info := f.view(t)
	deps := f.deps(st)
	deps.sp = noInteractions{f.sp}
	p, err := newConversationLaneExec(deps, info).Probe(context.Background(), v)
	if err != nil || p.Caps.Respond || !p.Caps.Live {
		t.Fatalf("Probe = caps %+v, %v; want live with no respond", p.Caps, err)
	}
}

// TestLaneExecRespondRefusesAnEndedOccurrence proves the executor re-reads
// the occurrence it answers under the session mutation lock: an end another
// writer recorded since the plan (another answer, a cancel, a look that saw
// the prompt gone) stops the key, though the runtime still shows the request
// id, which a later identical prompt shares.
func TestLaneExecRespondRefusesAnEndedOccurrence(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	f.prompt(laneTestRequest)
	op, raised := f.respond(t, st, opjournal.RespondPayload{Action: "approve"})
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		return promptSwapExec{conversationLaneExec: e, swap: func() {
			end := opjournal.Fact{
				Kind: opjournal.FactInteractionResolved, Conversation: f.id, Generation: 1, Interaction: raised.Interaction,
				Reason: opjournal.ResolvedCleared, Basis: opjournal.FactBasisObserved, At: f.clk.Now(),
			}
			if _, err := st.RecordFact(end); err != nil {
				t.Errorf("ending %s: %v", raised.Interaction, err)
			}
		}}
	})
	if err != nil || res.Done != opjournal.DoneRejected {
		t.Fatalf("Pass = %+v, %v; want the respond rejected", res, err)
	}
	if n := len(f.responses()); n != 0 {
		t.Fatalf("%d answers reached the runtime for an ended occurrence", n)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSkipped || got.Outcome.Reason != opjournal.ReasonStale {
		t.Fatalf("op outcome %+v; want skipped{stale}", got.Outcome)
	}
}
