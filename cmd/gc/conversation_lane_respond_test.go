package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// prompt shows a pending approval with request id req in the fixture's
// runtime.
func (f *laneFixture) prompt(req string) {
	f.sp.SetPendingInteraction(f.name, &runtime.PendingInteraction{RequestID: req, Kind: "approval", Options: []string{"approve", "deny"}})
}

// respond records the raising of req the runtime shows, as a producer does,
// and proposes principal's answer to it.
func (f *laneFixture) respond(t *testing.T, st *opjournal.Store, req, principal string, p opjournal.RespondPayload) (opjournal.Op, opjournal.Fact) {
	t.Helper()
	r, _, err := st.RecordRaising(f.id, 1, req, opjournal.FactBasisObserved, f.clk.Now())
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
		Producer: opjournal.Producer{Principal: principal, Actor: opjournal.ActorOperator}, Payload: payload,
		Scope: opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1, Interaction: r.Raised.Interaction},
	}), r.Raised
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
// runtime with its request id set, closes succeeded (unfenced: no runtime
// fences a response yet) and resolves its occurrence.
func TestLaneExecRespondAnswersTheRaising(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	f.prompt("tmux-abc")
	op, raised := f.respond(t, st, "tmux-abc", "operator", opjournal.RespondPayload{Action: "approve", Text: "ok"})
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
	if err != nil || res.Took != opjournal.TakeControl || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v; want an accepted respond", res, err)
	}
	calls := f.responses()
	if len(calls) != 1 || calls[0].RequestID != "tmux-abc" || calls[0].Action != "approve" || calls[0].Message != "ok" {
		t.Fatalf("runtime responses %+v", calls)
	}
	got := f.get(t, st, op.ID)
	if got.Outcome.Arm != opjournal.ArmSucceeded || got.Outcome.Result.Via != laneViaLive || got.Outcome.Degradation != opjournal.DegradationUnfencedDelivery {
		t.Fatalf("op outcome %+v", got.Outcome)
	}
	if latest, found, err := st.LatestRaising(f.id, 1, "tmux-abc"); err != nil || !found || !latest.Resolved || latest.Raised.ID != raised.ID {
		t.Fatalf("LatestRaising = %+v, found %v, %v; want the answered raising resolved", latest, found, err)
	}
}

// TestLaneExecTwoIdenticalPromptsGetTwoAnswers proves the second raising of
// a content-hash request id gets its own answer, not a replay of the first.
func TestLaneExecTwoIdenticalPromptsGetTwoAnswers(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	l := f.lane(t, st)
	f.prompt("tmux-abc")
	first, _ := f.respond(t, st, "tmux-abc", "operator", opjournal.RespondPayload{Action: "approve"})
	if res, err := f.pass(context.Background(), t, l, st, nil); err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("first Pass = %+v, %v", res, err)
	}
	f.clk.Advance(time.Second)
	f.prompt("tmux-abc")
	second, raised := f.respond(t, st, "tmux-abc", "operator", opjournal.RespondPayload{Action: "deny"})
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
			op, _ := f.respond(t, st, "tmux-abc", "operator", opjournal.RespondPayload{Action: "approve"})
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

// TestLaneExecRespondWaitsBehindAContentGate proves a gated running runtime
// reads its pending interaction as unknown: the respond waits instead of
// taking the prompt for gone.
func TestLaneExecRespondWaitsBehindAContentGate(t *testing.T) {
	f := newLaneFixture(t, func(meta map[string]string) { meta["restart_requested"] = "true" })
	st := f.laneStore(t)
	f.prompt("tmux-abc")
	op, _ := f.respond(t, st, "tmux-abc", "operator", opjournal.RespondPayload{Action: "approve"})
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
	if err != nil || res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitInteractionUnknown) {
		t.Fatalf("Pass = %+v, %v; want an interaction_unknown wait", res, err)
	}
	if f.get(t, st, op.ID).Status == opjournal.StatusClosed || len(f.responses()) != 0 {
		t.Fatal("a gated respond closed or answered")
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

// TestLaneRespondResultMapping pins how a response's error maps to the
// lane's result.
func TestLaneRespondResultMapping(t *testing.T) {
	stale := opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	for _, tc := range []struct {
		name string
		err  error
		want opjournal.ControlResult
	}{
		{"answered", nil, opjournal.Settled{Via: laneViaLive, Degradation: opjournal.DegradationUnfencedDelivery}},
		{"session unsupported", session.ErrInteractionUnsupported, opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}},
		{"runtime unsupported", runtime.ErrInteractionUnsupported, opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}},
		{"action unavailable", fmt.Errorf("%w: %w", session.ErrInteractionMismatch, runtime.ErrInteractionActionUnavailable), opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: laneRespondRefusedAction}},
		{"response invalid", fmt.Errorf("%w: %w", session.ErrInteractionMismatch, runtime.ErrInteractionResponseInvalid), opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: laneRespondRefusedAction}},
		{"request mismatch", fmt.Errorf("%w: pending interaction %q does not match request %q", session.ErrInteractionMismatch, "a", "b"), stale},
		{"no pending", session.ErrNoPendingInteraction, stale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := laneRespondResult(context.Background(), tc.err); got != tc.want {
				t.Fatalf("laneRespondResult(%v) = %#v, want %#v", tc.err, got, tc.want)
			}
		})
	}
	for _, err := range []error{runtime.ErrSessionNotFound, session.ErrSessionNotFound, runtime.ErrRuntimeUnavailable} {
		if got, ok := laneRespondResult(context.Background(), err).(opjournal.NotDelivered); !ok || !got.Transient {
			t.Fatalf("laneRespondResult(%v) = %#v, want a transient NotDelivered", err, got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{errors.New("approval prompt did not clear"), context.Canceled} {
		if _, ok := laneRespondResult(ctx, err).(opjournal.Unknown); !ok {
			t.Fatalf("laneRespondResult(%v) is not Unknown", err)
		}
	}
}
