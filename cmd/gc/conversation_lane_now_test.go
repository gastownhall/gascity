package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// nowMsg is a deliver-now prompt to the fixture's conversation.
func (f *laneFixture) nowMsg(mods ...func(*opjournal.Request)) opjournal.Request {
	return f.msg("now-1", opjournal.RolePrompt, "stop and do this", append([]func(*opjournal.Request){
		func(r *opjournal.Request) { r.Policy.Message.When = opjournal.WhenNow },
	}, mods...)...)
}

// laneNowTarget aims a deliver-now at turn of generation 1.
func laneNowTarget(turn string) func(*opjournal.Request) {
	return func(r *opjournal.Request) { r.Policy.Message.NowGeneration, r.Policy.Message.NowTurn = 1, turn }
}

func laneNowID(t *testing.T, st *opjournal.Store, m opjournal.Op) string {
	t.Helper()
	id, err := opjournal.NowInterruptID(st.Prefix(), m)
	if err != nil {
		t.Fatalf("NowInterruptID: %v", err)
	}
	return id
}

// replacementExec is an executor whose turns a test scripts directly, for
// replacementAfter.
func replacementExec(t *testing.T, caps worker.TurnCapabilities, transport string, protocol bool, read func(context.Context) (runtime.TurnHistory, error)) *conversationLaneExec {
	t.Helper()
	e := &conversationLaneExec{deps: laneExecDeps{cityPath: t.TempDir()}, info: session.Info{ID: "gc-1"}, protocol: protocol}
	if read != nil {
		e.turnHandle = laneStubTurns{caps: caps, read: read}
		e.turnCaps = caps
		e.traits = laneTurnTraitsOf(caps, transport)
	}
	e.lastTurn = laneTurnLook{live: opjournal.TriYes, turn: "t1", known: []string{"t0", "t1"}}
	return e
}

// laneReadOf is a turn read that answers hist, err.
func laneReadOf(hist runtime.TurnHistory, err error) func(context.Context) (runtime.TurnHistory, error) {
	return func(context.Context) (runtime.TurnHistory, error) { return hist, err }
}

// replacementAfter reads where the step left the session: idle (or a read
// lagging on the replaced turn) is the boundary, a newer turn is not, and a
// source it cannot read, or a read that failed, leaves a keystroke runtime
// at the session layer's boundary and a protocol runtime unknown. The
// replaced turn is known on every path, read or not. The executor's own
// look moves to what the step read.
func TestLaneNowReplacementAfter(t *testing.T) {
	inferred := worker.TurnCapabilities{
		TurnCapabilities: runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndInferred, Fence: runtime.TurnFenceChecked},
		Source:           worker.TurnSourceTranscript,
	}
	noEnds := inferred
	noEnds.End = runtime.TurnEndNone
	acp := laneAtomicTurnCaps
	t1Ended := runtime.EndedTurn{TurnID: "t1"}
	reads := map[string]func(context.Context) (runtime.TurnHistory, error){
		"idle":          laneReadOf(runtime.TurnHistory{Recent: []runtime.EndedTurn{t1Ended}}, nil),
		"newer":         laneReadOf(runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t2"}, Recent: []runtime.EndedTurn{t1Ended}}, nil),
		"idle, t0 only": laneReadOf(runtime.TurnHistory{Recent: []runtime.EndedTurn{{TurnID: "t0"}}}, nil),
		"lagging":       laneReadOf(runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}}, nil),
		"error":         laneReadOf(runtime.TurnHistory{}, errors.New("transcript unreadable")),
	}
	boundary := opjournal.TurnAfter{Boundary: opjournal.TriYes, Live: opjournal.TriNo, Known: []string{"t1"}}
	newer := opjournal.TurnAfter{Boundary: opjournal.TriNo, Live: opjournal.TriYes, Turn: "t2", Known: []string{"t2", "t1"}}
	keystrokeUnread := opjournal.TurnAfter{Boundary: opjournal.TriYes, Live: opjournal.TriUnknown, Known: []string{"t1"}}
	protocolUnread := opjournal.TurnAfter{Known: []string{"t1"}}
	for _, tc := range []struct {
		name      string
		caps      worker.TurnCapabilities
		transport string
		protocol  bool
		read      string // "" for no source
		replaced  string
		want      opjournal.TurnAfter
	}{
		{name: "transcript, idle", caps: inferred, read: "idle", replaced: "t1", want: boundary},
		{name: "transcript, newer turn", caps: inferred, read: "newer", replaced: "t1", want: newer},
		{name: "transcript, lagging", caps: inferred, read: "lagging", replaced: "t1", want: boundary},
		{name: "transcript, idle without the replaced turn", caps: inferred, read: "idle, t0 only", replaced: "t1", want: opjournal.TurnAfter{Boundary: opjournal.TriYes, Live: opjournal.TriNo, Known: []string{"t0", "t1"}}},
		{name: "transcript, read fails", caps: inferred, read: "error", replaced: "t1", want: keystrokeUnread},
		{name: "transcript without ends", caps: noEnds, read: "newer", replaced: "t1", want: keystrokeUnread},
		{name: "acp, idle", caps: acp, transport: "acp", protocol: true, read: "idle", replaced: "t1", want: boundary},
		{name: "acp, newer turn", caps: acp, transport: "acp", protocol: true, read: "newer", replaced: "t1", want: newer},
		{name: "t3, lagging", caps: acp, protocol: true, read: "lagging", replaced: "t1", want: boundary},
		{name: "t3, read fails", caps: acp, protocol: true, read: "error", replaced: "t1", want: protocolUnread},
		{name: "session scope, a turn runs", caps: acp, protocol: true, read: "lagging", want: opjournal.TurnAfter{Boundary: opjournal.TriNo, Live: opjournal.TriYes, Turn: "t1", Known: []string{"t1"}}},
		{name: "session scope, read fails", caps: inferred, read: "error", want: opjournal.TurnAfter{Boundary: opjournal.TriYes, Live: opjournal.TriUnknown}},
		{name: "no source, keystroke", want: keystrokeUnread, replaced: "t1"},
		{name: "no source, protocol", protocol: true, want: protocolUnread, replaced: "t1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := replacementExec(t, tc.caps, tc.transport, tc.protocol, reads[tc.read])
			got := e.replacementAfter(context.Background(), tc.replaced)
			if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", tc.want) {
				t.Fatalf("after = %+v, want %+v", got, tc.want)
			}
			if e.lastTurn.live != got.Live || e.lastTurn.turn != got.Turn {
				t.Fatalf("executor look %+v after %+v", e.lastTurn, got)
			}
			for _, id := range append([]string{"t0", "t1"}, got.Known...) {
				if !slices.Contains(e.lastTurn.known, id) {
					t.Fatalf("executor look %+v lost known turn %s", e.lastTurn, id)
				}
			}
		})
	}
}

// nowCase runs a deliver-now unit through the live executor on a checked
// transcript source: the probe sees turn t1 running until the interrupt
// settles it, and the session idle after.
type nowCase struct {
	*interruptCase
	stepped atomic.Bool
}

func newNowCase(t *testing.T) *nowCase {
	t.Helper()
	c := &nowCase{interruptCase: newInterruptCase(t, laneCheckedTurnCaps)}
	c.read = func(context.Context) (runtime.TurnHistory, error) {
		switch {
		case slices.ContainsFunc(c.f.sp.SnapshotCalls(), func(call runtime.Call) bool { return call.Method == "Nudge" || call.Method == "NudgeNow" }):
			// The message started t2, so its link is decided at once.
			return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t2"}, CurrentStart: laneT0.Add(time.Second), Recent: []runtime.EndedTurn{{TurnID: "t1"}}}, nil
		case c.stepped.Load():
			return runtime.TurnHistory{Recent: []runtime.EndedTurn{{TurnID: "t1"}}}, nil
		default:
			return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}}, nil
		}
	}
	return c
}

// answerWith makes the worker answer err, after settling the turn when
// settles says so.
func (c *nowCase) answerWith(err error, settles bool) {
	c.answer = func(worker.InterruptRequest) (worker.InterruptResult, error) {
		if settles {
			c.stepped.Store(true)
		}
		return worker.InterruptResult{}, err
	}
}

// The replacement step's request: ForReplacement, the unit's target or the
// turn the fresh read found, the incarnation's fence with AllowUnfenced,
// and no on_failure; a settled step at the boundary sends the message, and
// both close in the pass.
func TestLaneNowReplacementRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		mods []func(*opjournal.Request)
	}{
		{"untargeted", nil},
		{"targeted", []func(*opjournal.Request){laneNowTarget("t1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newNowCase(t)
			c.answerWith(nil, true)
			m := c.f.propose(t, c.st, c.f.nowMsg(tc.mods...))
			res := c.pass(t)
			if res.Took != opjournal.TakeNow || res.Done != opjournal.DoneAccepted {
				t.Fatalf("pass %+v", res)
			}
			reqs := c.requests()
			if len(reqs) != 1 {
				t.Fatalf("requests %+v", reqs)
			}
			r := reqs[0]
			if !r.ForReplacement || r.Turn != "t1" || r.Fence != c.fence() || !r.AllowUnfenced || r.OnFailure != "" {
				t.Fatalf("request %+v", r)
			}
			assertOutcome(t, c.f.get(t, c.st, laneNowID(t, c.st, m)), opjournal.ArmSucceeded, "", "t1", opjournal.DegradationUnfencedInterrupt)
			got := c.f.get(t, c.st, m.ID)
			if r := got.Outcome.Result; got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded || r.Turn != "t2" || r.TurnLink != opjournal.LinkCorrelated {
				t.Fatalf("message %s %+v, want succeeded linked to t2, never the replaced t1", got.Status, got.Outcome)
			}
			if n := c.f.nudges(); len(n) != 1 || n[0].Method != "NudgeNow" || n[0].Message != "stop and do this" {
				t.Fatalf("runtime nudges %+v, want the message once, at once", n)
			}
		})
	}
}

// The replacement mapping: a turn that settled but whose session was not
// prepared is failed{replacement_not_prepared} naming the turn, its ref
// kept; a refusal (Replace none, or restart until OJ-17b) is
// failed{unsupported}, its ref restored; a dispatch that may have run is
// Unknown, its ref kept, as for a standalone interrupt; every other answer
// maps as a standalone interrupt's does, and a standalone interrupt still
// maps ErrReplacementNotPrepared to Unknown.
func TestLaneNowReplacementResultMapping(t *testing.T) {
	notPrepared := fmt.Errorf("x: %w", session.ErrReplacementNotPrepared)
	// A protocol interrupt whose dispatch failed may have reached the
	// runtime (session.InterruptTurnOn).
	maybeSent := fmt.Errorf("x: %w: %w", session.ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable)
	e := replacementExec(t, worker.TurnCapabilities{}, "", false, nil)
	for _, tc := range []struct {
		name string
		err  error
		want opjournal.ControlResult
	}{
		{"settled", nil, opjournal.Settled{
			Turn: "t1", Via: laneViaLive, Degradation: opjournal.DegradationUnfencedInterrupt,
			After: opjournal.TurnAfter{Boundary: opjournal.TriYes, Live: opjournal.TriUnknown, Known: []string{"t1"}},
		}},
		{"not prepared", notPrepared, opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonReplacementNotPrepared, Turn: "t1"}},
		{"refused", fmt.Errorf("x: %w", session.ErrTurnInterruptUnsupported), opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}},
		{"identity unsupported", fmt.Errorf("x: %w", runtime.ErrTurnIdentityUnsupported), opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}},
		{"not current", &runtime.TurnNotCurrentError{Session: "s", TurnID: "t1"}, opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}},
		{"not ended", session.TurnNotEndedError("turn", errors.New("busy")), opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary, Turn: "t1"}},
		{"unavailable", runtime.ErrRuntimeUnavailable, opjournal.NotDelivered{Transient: true, Unavailable: true, Cause: runtime.ErrRuntimeUnavailable}},
		{"maybe sent", maybeSent, opjournal.Unknown{Cause: maybeSent}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := laneInterruptResult(context.Background(), worker.InterruptResult{}, tc.err, "t1", false)
			got := e.replacementResult(context.Background(), out, tc.err, "t1")
			if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", tc.want) {
				t.Fatalf("result = %#v, want %#v", got, tc.want)
			}
		})
	}
	if got := laneInterruptResult(context.Background(), worker.InterruptResult{}, notPrepared, "t1", false); fmt.Sprintf("%T", got) != "opjournal.Unknown" {
		t.Fatalf("standalone not prepared = %#v, want Unknown", got)
	}
	for _, tc := range []struct {
		name    string
		err     error
		reason  string
		turn    string // the interrupt's recorded turn
		keepRef bool
	}{
		{"not prepared keeps the ref", notPrepared, opjournal.ReasonReplacementNotPrepared, "t1", true},
		// A step that may have run is never sent again (N5): the message
		// waits for the boundary.
		{"maybe sent keeps the ref", maybeSent, opjournal.ReasonAmbiguousDelivery, "", true},
		{"refusal restores it", fmt.Errorf("x: %w", session.ErrTurnInterruptUnsupported), opjournal.ReasonUnsupported, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newNowCase(t)
			c.answerWith(tc.err, true)
			m := c.f.propose(t, c.st, c.f.nowMsg())
			c.pass(t)
			iid := laneNowID(t, c.st, m)
			assertOutcome(t, c.f.get(t, c.st, iid), opjournal.ArmFailed, tc.reason, tc.turn, "")
			ref := c.ints.current(c.f.id)
			if kept := ref == (opjournal.InterruptRef{Op: iid, Turn: "t1"}); kept != tc.keepRef {
				t.Fatalf("ref %+v, want kept %v", ref, tc.keepRef)
			}
			if got := c.f.get(t, c.st, m.ID); got.Status != opjournal.StatusAccepted || len(c.f.nudges()) != 0 {
				t.Fatalf("message %s nudges %d, want it waiting for the boundary", got.Status, len(c.f.nudges()))
			}
		})
	}
}

// Deliver sends a replacement at once on a runtime that does not
// deduplicate, and keyed by default on one that does; a direct unit's
// message goes as any boundary delivery, and a steer as before.
func TestLaneNowDeliverySend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile runtime.Profile
		d       opjournal.Delivery
		want    worker.NudgeDelivery
	}{
		{"replacement", runtime.ProfileTmux, opjournal.Delivery{Replacement: true}, worker.NudgeDeliveryImmediate},
		{"replacement, deduplicating", runtime.ProfileT3Bridge, opjournal.Delivery{Replacement: true}, worker.NudgeDeliveryDefault},
		{"direct", runtime.ProfileTmux, opjournal.Delivery{}, worker.NudgeDeliveryDefault},
		{"steer", runtime.ProfileTmux, opjournal.Delivery{Steer: true}, worker.NudgeDeliveryImmediate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTurnLaneFixture(t, tc.profile)
			e, v := f.exec(t, f.laneStore(t))
			var mu sync.Mutex
			var reqs []worker.NudgeRequest
			recordRequests(e, &mu, &reqs)
			if _, err := e.Probe(context.Background(), v); err != nil {
				t.Fatalf("Probe: %v", err)
			}
			d := tc.d
			d.Fence, d.Key, d.Text, d.Mode = laneFence(v), "gc-op-x:2", "stop and do this", opjournal.LinkStarts
			e.Deliver(context.Background(), d)
			mu.Lock()
			defer mu.Unlock()
			if len(reqs) != 1 || reqs[0].Delivery != tc.want || reqs[0].DeliveryKey != laneDeliveryKeyPrefix+"gc-op-x:2" {
				t.Fatalf("requests %+v, want one %s send under the key", reqs, tc.want)
			}
		})
	}
}

// atomicNowFixture is an ACP-like fake runtime with atomic turn fencing and
// a scripted turn read.
func atomicNowFixture(t *testing.T, fences []string) *turnLaneFixture {
	t.Helper()
	atomicCaps := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact}
	f := newTurnLaneFixture(t, runtime.ProfileACP, func(meta map[string]string) { meta["transport"] = "acp" })
	f.laneFixture.sp.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{f.name: {atomicCaps}}
	f.laneFixture.sp.TurnScripts = map[string][]string{f.name: fences}
	return f
}

// atomicNowPass runs one pass on f, the session's turns read by read.
func atomicNowPass(t *testing.T, f *turnLaneFixture, st *opjournal.Store, read func(context.Context) (runtime.TurnHistory, error)) opjournal.PassResult {
	t.Helper()
	atomicCaps := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact}
	e, v := f.exec(t, st)
	e.turns = func() (worker.TurnHandle, error) {
		return laneStubTurns{
			caps: worker.TurnCapabilities{TurnCapabilities: atomicCaps, Source: worker.TurnSourceProtocol, Interrupt: runtime.TurnInterruptKeystroke},
			read: read,
		}, nil
	}
	res, err := f.lane(t, st).Pass(context.Background(), v, e)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return res
}

// runtimeInputs are the interrupts and nudges the fake runtime received.
func runtimeInputs(f *turnLaneFixture) []string {
	var out []string
	for _, c := range f.laneFixture.sp.SnapshotCalls() {
		switch c.Method {
		case "InterruptTurn":
			out = append(out, "interrupt "+c.Key+":"+c.Value)
		case "Interrupt", "SendKeys":
			out = append(out, c.Method)
		case "Nudge", "NudgeNow":
			out = append(out, c.Method)
		}
	}
	return out
}

// The acceptance case on an atomic fake (ACP): with t1 running, one turn
// interrupt of t1, then one nudge; the interrupt succeeds on t1 marked
// unfenced_interrupt (a protocol turn interrupt drops the input fence, the
// 2026-10-19 ruling), and the message links to the turn it started, exact
// on the serial source, never t1.
func TestLaneNowOnAnAtomicFake(t *testing.T) {
	f := atomicNowFixture(t, []string{"t1", ""})
	st := f.laneStore(t)
	m := f.propose(t, st, f.nowMsg(laneNowTarget("t1")))
	read := func(context.Context) (runtime.TurnHistory, error) {
		inputs := runtimeInputs(f)
		switch {
		case slices.ContainsFunc(inputs, func(s string) bool { return s == "Nudge" || s == "NudgeNow" }):
			return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t2"}, CurrentStart: laneT0, Recent: []runtime.EndedTurn{{TurnID: "t1"}}}, nil
		case len(inputs) > 0:
			return runtime.TurnHistory{Recent: []runtime.EndedTurn{{TurnID: "t1"}}}, nil
		default:
			return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}}, nil
		}
	}
	res := atomicNowPass(t, f, st, read)
	if res.Took != opjournal.TakeNow || res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass %+v", res)
	}
	if got := runtimeInputs(f); len(got) != 2 || got[0] != "interrupt t1:sent" || (got[1] != "Nudge" && got[1] != "NudgeNow") {
		t.Fatalf("runtime inputs %v, want t1's interrupt then one nudge", got)
	}
	assertOutcome(t, f.get(t, st, laneNowID(t, st, m)), opjournal.ArmSucceeded, "", "t1", opjournal.DegradationUnfencedInterrupt)
	r := f.get(t, st, m.ID).Outcome.Result
	if r.Turn != "t2" || r.TurnLink != opjournal.LinkExact || r.TurnBasis != opjournal.BasisACPSerial {
		t.Fatalf("message result %+v, want t2 exact by acp_serial", r)
	}
}

// T5 on the atomic fake: a unit aimed at t1 while t2 runs sends no
// interrupt; its interrupt closes skipped{stale}, and the message waits for
// the boundary, then goes.
func TestLaneNowNeverInterruptsANewerTurn(t *testing.T) {
	f := atomicNowFixture(t, []string{""})
	st := f.laneStore(t)
	m := f.propose(t, st, f.nowMsg(laneNowTarget("t1")))
	busy := true
	// The runtime is busy by the quiescence rule too while t2 runs.
	f.laneFixture.sp.SetActivity(f.name, time.Now())
	read := func(context.Context) (runtime.TurnHistory, error) {
		if busy {
			return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t2"}, Recent: []runtime.EndedTurn{{TurnID: "t1"}}}, nil
		}
		return runtime.TurnHistory{Recent: []runtime.EndedTurn{{TurnID: "t2"}, {TurnID: "t1"}}}, nil
	}
	atomicNowPass(t, f, st, read)
	assertOutcome(t, f.get(t, st, laneNowID(t, st, m)), opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
	res := atomicNowPass(t, f, st, read)
	if res.Took != opjournal.TakeNone || !res.Busy || !laneWaitsFor(res, opjournal.WaitBusy) {
		t.Fatalf("pass while t2 runs %+v", res)
	}
	if got := runtimeInputs(f); len(got) != 0 {
		t.Fatalf("runtime inputs %v, want none while t2 runs", got)
	}
	busy = false
	f.laneFixture.sp.SetActivity(f.name, time.Now().Add(-time.Minute))
	atomicNowPass(t, f, st, read)
	if got := f.get(t, st, m.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded || got.Outcome.Degradation != "" {
		t.Fatalf("message %s %+v", got.Status, got.Outcome)
	}
	if got := runtimeInputs(f); slices.ContainsFunc(got, func(s string) bool { return s != "Nudge" && s != "NudgeNow" }) {
		t.Fatalf("runtime inputs %v, want only the message", got)
	}
}

// An idle session gets the message at once: no interrupt, the interrupt
// skipped{stale}, the message delivered in the same pass.
func TestLaneNowIdleDeliversAtOnce(t *testing.T) {
	f := atomicNowFixture(t, []string{""})
	st := f.laneStore(t)
	m := f.propose(t, st, f.nowMsg())
	idle := func(context.Context) (runtime.TurnHistory, error) {
		return runtime.TurnHistory{Recent: []runtime.EndedTurn{{TurnID: "t0"}}}, nil
	}
	res := atomicNowPass(t, f, st, idle)
	if res.Took != opjournal.TakeNow || res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass %+v", res)
	}
	if got := runtimeInputs(f); len(got) != 1 || got[0] != "Nudge" {
		t.Fatalf("runtime inputs %v, want one default nudge", got)
	}
	assertOutcome(t, f.get(t, st, laneNowID(t, st, m)), opjournal.ArmSkipped, opjournal.ReasonStale, "", "")
	if got := f.get(t, st, m.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("message %s %+v", got.Status, got.Outcome)
	}
}

// A session whose turn cannot be replaced (Replace none): the worker
// refuses with nothing sent, the interrupt closes failed{unsupported}, and
// the message goes at the next boundary marked interrupt_unsupported.
func TestLaneNowUnsupportedWaitsForTheBoundary(t *testing.T) {
	c := newNowCase(t)
	c.answerWith(fmt.Errorf("interrupting for a replacement, which it takes as %q: %w", worker.TurnReplaceNone, session.ErrTurnInterruptUnsupported), false)
	m := c.f.propose(t, c.st, c.f.nowMsg())
	res := c.pass(t)
	if res.Took != opjournal.TakeNow || res.Done != opjournal.DoneReleased || !res.More {
		t.Fatalf("pass %+v", res)
	}
	assertOutcome(t, c.f.get(t, c.st, laneNowID(t, c.st, m)), opjournal.ArmFailed, opjournal.ReasonUnsupported, "", "")
	if len(c.f.nudges()) != 0 {
		t.Fatalf("nudges %+v while the turn runs", c.f.nudges())
	}
	c.stepped.Store(true) // the turn ended on its own
	res = c.pass(t)
	if res.Took != opjournal.TakeContent || res.Done != opjournal.DoneAccepted {
		t.Fatalf("boundary pass %+v", res)
	}
	got := c.f.get(t, c.st, m.ID)
	if got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded || got.Outcome.Degradation != opjournal.DegradationInterruptUnsupported {
		t.Fatalf("message %s %+v", got.Status, got.Outcome)
	}
	if n := c.f.nudges(); len(n) != 1 || n[0].Method != "Nudge" {
		t.Fatalf("nudges %+v, want one boundary nudge", n)
	}
}

// TestLaneHostJournalsTheTurnADeliverNowDelivers pins DES-O1 §7.7 for a
// deliver-now unit to an idle session (the direct path): the worker that ran
// the unit stays until its observer has journaled the turn the message
// started, start and end, then exits.
func TestLaneHostJournalsTheTurnADeliverNowDelivers(t *testing.T) {
	l := newLaneHostTurns(t)
	st := l.f.laneStore(t)
	l.f.propose(t, st, l.f.nowMsg())
	ready := new(atomic.Bool)
	ready.Store(true)
	l.hh.h.newExec = func(laneExecDeps, laneRow) opjournal.Executor {
		return laneReadyExec{clk: l.f.clk, ready: ready}
	}
	l.hh.start()
	if p := l.hh.nextPass(t); p.out.res.Took != opjournal.TakeNow || p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the unit taken and its message delivered", p.out.res)
	}
	l.hh.nextPass(t)
	l.send(laneIdle())
	l.batch(t, "the first read")
	l.noExit(t, "before the delivered turn started")
	l.send(laneRunning("t1"))
	l.batch(t, "the delivered turn's start")
	l.noExit(t, "while the delivered turn runs")
	l.send(laneIdle(laneEnded("t1", runtime.TurnStatusCompleted)))
	l.batch(t, "the delivered turn's end")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker did not exit once the delivered turn ended")
	}
	if got, want := laneFactList(t, l.store, l.f.id, 1), []string{"turn.started:t1", "turn.ended:t1:completed"}; !slices.Equal(got, want) {
		t.Fatalf("facts = %q, want %q", got, want)
	}
}

// laneReplaceExec is a ready executor whose runtime runs turn t0, with no
// boundary, until a replacement step settles t0 at the boundary, so a
// deliver-now unit takes the replace path.
type laneReplaceExec struct {
	laneReadyExec
	replaced *atomic.Bool
}

func (e laneReplaceExec) Probe(ctx context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	p, err := e.laneReadyExec.Probe(ctx, conv)
	if !e.replaced.Load() {
		p.Boundary, p.TurnLive, p.Turn = opjournal.TriNo, opjournal.TriYes, "t0"
	}
	return p, err
}

func (e laneReplaceExec) Interrupt(_ context.Context, c opjournal.InterruptCall) opjournal.ControlResult {
	if !c.ForReplacement {
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
	}
	e.replaced.Store(true)
	return opjournal.Settled{Turn: "t0", Via: "test", After: opjournal.TurnAfter{Boundary: opjournal.TriYes, Live: opjournal.TriNo, Known: []string{"t0"}}}
}

// TestLaneHostKeepsTheDeliverNowTurnPastTheReplacedEnd pins DES-O1 §7.7 for
// a deliver-now unit that replaces the turn the observer saw running: the
// replaced turn's end, folded first, does not end the worker's wait for the
// turn the message started, and both turns are journaled, the replaced
// one's end credited to the unit's interrupt.
func TestLaneHostKeepsTheDeliverNowTurnPastTheReplacedEnd(t *testing.T) {
	l := newLaneHostTurns(t, string(opjournal.KindMessage))
	st := l.f.laneStore(t)
	ready, replaced := new(atomic.Bool), new(atomic.Bool)
	ready.Store(true)
	l.hh.h.newExec = func(laneExecDeps, laneRow) opjournal.Executor {
		return laneReplaceExec{laneReadyExec: laneReadyExec{clk: l.f.clk, ready: ready}, replaced: replaced}
	}
	l.hh.start()
	if !l.hh.h.Poke(l.f.id) {
		t.Fatal("Poke refused")
	}
	l.hh.nextPass(t)
	l.send(laneRunning("t0"))
	l.batch(t, "the running turn")
	l.f.clk.Advance(10 * time.Second)
	m := l.f.propose(t, st, l.f.nowMsg())
	if !l.hh.h.Poke(l.f.id) {
		t.Fatal("Poke refused")
	}
	if p := l.hh.nextPass(t); p.out.err != nil || p.out.res.Took != opjournal.TakeNow || p.out.res.Done != opjournal.DoneAccepted || !replaced.Load() {
		t.Fatalf("pass = %+v, %v (replaced %v), want the unit to replace t0 and deliver its message", p.out.res, p.out.err, replaced.Load())
	}
	take := l.f.clk.Now()
	l.hh.nextPass(t)
	t0 := laneEnded("t0", runtime.TurnStatusCancelled)
	l.send(laneIdle(t0))
	l.batch(t, "the replaced turn's end")
	l.noExit(t, "at the replaced turn's end, before the turn its message started")
	l.send(laneRunningAt("t1", take.Add(time.Second), t0))
	l.batch(t, "the delivered turn's start")
	l.noExit(t, "while the delivered turn runs")
	l.send(laneIdle(laneCompletedAt("t1", take.Add(time.Second)), t0))
	l.batch(t, "the delivered turn's end")
	if l.hh.nextExit(t) != l.f.id {
		t.Fatal("the worker did not exit once the delivered turn ended")
	}
	want := []string{"turn.started:t0", "turn.ended:t0:interrupted:" + laneNowID(t, st, m), "turn.started:t1", "turn.ended:t1:completed"}
	if got := laneFactList(t, l.store, l.f.id, 1); !slices.Equal(got, want) {
		t.Fatalf("facts = %q, want %q", got, want)
	}
}
