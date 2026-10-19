package main

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// laneProvider sets the fixture session's provider.
func laneProvider(provider string) laneFixtureOpt {
	return func(meta map[string]string) { meta["provider"] = provider }
}

// steerMsg is a steer message with fallback fb.
func steerMsg(fb opjournal.Fallback) func(*opjournal.Request) {
	return func(r *opjournal.Request) {
		r.Policy.Message.When = opjournal.WhenSteer
		r.Policy.Message.OnUnsupported = fb
	}
}

// busyLane makes e's runtime look mid-turn.
func busyLane(e *conversationLaneExec) {
	e.idle = func(nudgeTarget, worker.LiveObservation) bool { return false }
}

// recordingDeliverExec records every Delivery the lane hands its executor.
type recordingDeliverExec struct {
	*conversationLaneExec
	got *[]opjournal.Delivery
}

func (e recordingDeliverExec) Deliver(ctx context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	*e.got = append(*e.got, d)
	return e.conversationLaneExec.Deliver(ctx, d)
}

// probeSteer probes f's conversation and returns its Caps.Steer.
func probeSteer(t *testing.T, f *laneFixture) bool {
	t.Helper()
	st := f.laneStore(t)
	v, info := f.view(t)
	p, err := newConversationLaneExec(f.deps(st), info).Probe(context.Background(), v)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return p.Caps.Steer
}

// sessionReadsSteer is the supports_steer session responses report for f's
// session: session.SubmissionCapabilitiesForRuntime over its row.
func sessionReadsSteer(t *testing.T, f *laneFixture) bool {
	t.Helper()
	row, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return session.SubmissionCapabilitiesForRuntime(row.Metadata, true,
		runtime.TurnInterruptModeFor(f.sp, f.name), runtime.SteerModeFor(f.sp, f.name)).SupportsSteer
}

// TestLaneExecSteerCapIsRuntimeTimesFamily kills a probe whose Caps.Steer
// reads only the runtime or only the family: a protocol runtime steers every
// family, a runtime that cannot steer steers nobody, no family takes a
// keystroke steer (Codex reads tmux's trailing Escape as cancel, Claude
// queues typed input for its next turn), and a conversation another host
// serves is steered by none here.
func TestLaneExecSteerCapIsRuntimeTimesFamily(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		mode     runtime.SteerMode
		want     bool
	}{
		{"codex protocol", "codex", runtime.SteerProtocol, true},
		{"gemini protocol", "gemini", runtime.SteerProtocol, true},
		{"codex keystroke", "codex", runtime.SteerKeystroke, false},
		{"claude keystroke", "claude", runtime.SteerKeystroke, false},
		{"codex none", "codex", runtime.SteerNone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t, laneProvider(tc.provider))
			f.sp.SteerModeValue = tc.mode
			if got := probeSteer(t, f); got != tc.want {
				t.Fatalf("Caps.Steer = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("hosted elsewhere", func(t *testing.T) {
		f := newLaneFixture(t, laneProvider("codex"))
		f.sp.SteerModeValue = runtime.SteerProtocol
		st := f.laneStore(t)
		stampHost(t, f, st, 1, opjournal.KindMessage)
		v, info := f.view(t)
		p, err := newConversationLaneExec(f.deps(st), info).Probe(context.Background(), v)
		if err != nil || p.Caps.Live || p.Caps.Steer {
			t.Fatalf("Probe = %+v, %v; want neither live nor steer", p.Caps, err)
		}
	})
}

// TestLaneExecSteerCapAsksAboutTheProbedRow kills a probe that decides
// Caps.Steer apart from the session's row or the runtime's steer mode: the
// lane asks the steer-capability source (session.SupportsSteerForInfo in
// production) once, about the row the pass's view came from and the mode the
// runtime reports for the probed session's name, and Caps.Steer is that
// answer while the runtime is live. It is the agreement test's complement:
// with no family in the keystroke table, only a seam can tell a row-blind
// probe from a row-reading one. The runtime reports keystroke only for the
// probed session's name, so a probe that asks auto or hybrid about another
// name (and so reads another backend's mode) is caught too.
func TestLaneExecSteerCapAsksAboutTheProbedRow(t *testing.T) {
	for _, answer := range []bool{true, false} {
		t.Run(fmt.Sprint(answer), func(t *testing.T) {
			f := newLaneFixture(t, func(m map[string]string) { m["provider"], m["builtin_ancestor"] = "my-wrap", "codex" })
			st := f.laneStore(t)
			v, info := f.view(t)
			deps := f.deps(st)
			deps.sp = steerModeForName{Fake: f.sp, name: f.name, mode: runtime.SteerKeystroke}
			e := newConversationLaneExec(deps, info)
			type ask struct {
				info session.Info
				mode runtime.SteerMode
			}
			var asks []ask
			e.steerable = func(i session.Info, m runtime.SteerMode) bool {
				asks = append(asks, ask{i, m})
				return answer
			}
			p, err := e.Probe(context.Background(), v)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if len(asks) != 1 || asks[0].mode != runtime.SteerKeystroke || asks[0].info.ID != f.id ||
				asks[0].info.Provider != "my-wrap" || session.ProviderFamilyFromInfo(asks[0].info, "") != "codex" {
				t.Fatalf("steer asks = %+v, want one about row %s (my-wrap, family codex) in keystroke mode", asks, f.id)
			}
			if p.Caps.Steer != answer {
				t.Fatalf("Caps.Steer = %v, want the source's answer %v", p.Caps.Steer, answer)
			}
		})
	}
}

// steerModeForName is a runtime that reports mode for the session name and
// [runtime.SteerNone] for every other name, as auto and hybrid answer for
// the backend a name routes to.
type steerModeForName struct {
	*runtime.Fake
	name string
	mode runtime.SteerMode
}

// SteerMode reports s.mode for s.name only.
func (s steerModeForName) SteerMode(name string) runtime.SteerMode {
	if name == s.name {
		return s.mode
	}
	return runtime.SteerNone
}

// TestLaneSteerCapNeedsAKeyedSteer kills a lane that steers a runtime that
// deduplicates by delivery key: a steer goes out through the immediate
// nudge, which no runtime keys yet, so a re-taken steer would be applied
// twice while the planner drops the D-NOTE repeat line for a dedup runtime.
func TestLaneSteerCapNeedsAKeyedSteer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caps      opjournal.Caps
		steerable bool
		want      bool
	}{
		{"live steerable", opjournal.Caps{Live: true}, true, true},
		{"not live", opjournal.Caps{}, true, false},
		{"not steerable", opjournal.Caps{Live: true}, false, false},
		{"dedup", opjournal.Caps{Live: true, Dedup: true}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := laneSteerCap(tc.caps, tc.steerable); got != tc.want {
				t.Fatalf("laneSteerCap(%+v, %v) = %v, want %v", tc.caps, tc.steerable, got, tc.want)
			}
		})
	}
}

// TestLaneExecSteerCapAgreesWithSessionReads kills a lane whose Caps.Steer
// is derived apart from the supports_steer session responses report
// (session.SubmissionCapabilitiesForRuntime over the row): for rows with no
// provider, a wrapped provider, a provider kind, ACP by transport or by
// provider, under every steer mode, the two agree while the lane runs the
// conversation live. While the keystroke table is empty it cannot, on its
// own, catch a Caps.Steer that ignores the row;
// TestLaneExecSteerCapAsksAboutTheProbedRow covers that. A runtime that
// cancels turns by protocol and does not report its turns is the documented
// split (SESSION-RUNTIME-025): supports_steer still reads its steer mode,
// whichever host delivers, while the lane does not run it live and steers
// nothing.
func TestLaneExecSteerCapAgreesWithSessionReads(t *testing.T) {
	rows := map[string]laneFixtureOpt{
		"no provider":      func(map[string]string) {},
		"codex":            laneProvider("codex"),
		"builtin ancestor": func(m map[string]string) { m["provider"], m["builtin_ancestor"] = "my-wrap", "codex" },
		"provider kind":    func(m map[string]string) { m["provider"], m["provider_kind"] = "fast", "claude" },
		"acp transport":    func(m map[string]string) { m["provider"], m["transport"] = "codex", "acp" },
		"acp provider":     laneProvider("acp"),
	}
	for name, opt := range rows {
		for _, mode := range []runtime.SteerMode{runtime.SteerKeystroke, runtime.SteerProtocol, runtime.SteerNone} {
			t.Run(name+"/"+string(mode), func(t *testing.T) {
				f := newLaneFixture(t, opt)
				f.sp.SteerModeValue = mode
				want := sessionReadsSteer(t, f)
				if got := probeSteer(t, f); got != want {
					t.Fatalf("lane Caps.Steer = %v, session reads supports_steer = %v", got, want)
				}
			})
		}
	}
	for _, mode := range []runtime.SteerMode{runtime.SteerKeystroke, runtime.SteerProtocol, runtime.SteerNone} {
		t.Run("protocol interrupt/"+string(mode), func(t *testing.T) {
			f := newLaneFixture(t, laneProvider("codex"))
			f.sp.SteerModeValue = mode
			f.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
			if got, want := sessionReadsSteer(t, f), mode == runtime.SteerProtocol; got != want {
				t.Fatalf("session reads supports_steer = %v, want %v: the runtime's steer mode with the family, not gated on the lane", got, want)
			}
			st := f.laneStore(t)
			v, info := f.view(t)
			p, err := newConversationLaneExec(f.deps(st), info).Probe(context.Background(), v)
			if err != nil || p.Caps.Live || p.Caps.Steer {
				t.Fatalf("Probe = %+v, %v; want neither live nor steer", p.Caps, err)
			}
		})
	}
}

// TestLaneExecProtocolModeSteerWaitsNotLive pins what the split between
// supports_steer and Caps.Steer means for a steer (SESSION-RUNTIME-025): a
// runtime that cancels turns and steers by protocol but does not report its
// turns reads supports_steer true, yet the lane does not run it live, so a
// steer to its busy session whose on_unsupported is fail waits not_live with
// nothing typed, never closing failed{unsupported}.
func TestLaneExecProtocolModeSteerWaitsNotLive(t *testing.T) {
	f := newLaneFixture(t, laneProvider("codex"))
	f.sp.SteerModeValue = runtime.SteerProtocol
	f.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	if !sessionReadsSteer(t, f) {
		t.Fatal("session reads supports_steer = false; the wait must be tested against a session that reports a steer")
	}
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "also check the logs", steerMsg(opjournal.FallbackFail)))
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		busyLane(e)
		return e
	})
	if err != nil || res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitNotLive) {
		t.Fatalf("Pass = %+v, %v; want a not_live wait and nothing taken", res, err)
	}
	if calls := f.nudges(); len(calls) != 0 {
		t.Fatalf("runtime nudges = %+v, want none", calls)
	}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("op = %s outcome %+v, want submitted: a steer the lane cannot run live waits, whatever its on_unsupported", got.Status, got.Outcome)
	}
}

// TestLaneExecBusyDefaultWaits is D-STEER: a default (boundary) message to a
// busy session waits for the boundary, typing nothing, although its runtime
// could steer it.
func TestLaneExecBusyDefaultWaits(t *testing.T) {
	f := newLaneFixture(t, laneProvider("codex"))
	f.sp.SteerModeValue = runtime.SteerProtocol
	if !probeSteer(t, f) {
		t.Fatal("Caps.Steer = false; the wait must be tested against a session the lane could steer")
	}
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "after this turn"))
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		busyLane(e)
		return e
	})
	if err != nil || res.Took != opjournal.TakeNone || !res.Busy || !laneWaitsFor(res, opjournal.WaitBusy) {
		t.Fatalf("Pass = %+v, %v; want a busy wait and nothing taken", res, err)
	}
	if calls := f.nudges(); len(calls) != 0 {
		t.Fatalf("runtime nudges = %+v, want none", calls)
	}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("op status = %s, want submitted", got.Status)
	}
}

// TestLaneExecSteersBusySession proves an explicit steer to a busy session
// its runtime can steer goes into the running turn: the lane hands the
// executor a steer in joins mode, the executor sends it with immediate
// delivery under the Take's fence (the runtime's NudgeNow), and the op closes
// succeeded via steer.
func TestLaneExecSteersBusySession(t *testing.T) {
	f := newLaneFixture(t, laneProvider("codex"))
	f.sp.SteerModeValue = runtime.SteerProtocol
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "also check the logs", steerMsg(opjournal.FallbackBoundary)))
	var mu sync.Mutex
	var reqs []worker.NudgeRequest
	var deliveries []opjournal.Delivery
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		busyLane(e)
		recordRequests(e, &mu, &reqs)
		return recordingDeliverExec{conversationLaneExec: e, got: &deliveries}
	})
	if err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v; want accepted", res, err)
	}
	if len(deliveries) != 1 || !deliveries[0].Steer || deliveries[0].Mode != opjournal.LinkJoins {
		t.Fatalf("deliveries = %+v, want one steer in joins mode", deliveries)
	}
	wantFence := runtime.InputFence{Generation: 1, Epoch: 1, Token: f.token}
	got := f.get(t, st, op.ID)
	if len(reqs) != 1 || reqs[0].Delivery != worker.NudgeDeliveryImmediate || reqs[0].Wake != worker.NudgeWakeLiveOnly || reqs[0].Fence != wantFence ||
		reqs[0].DeliveryKey != "gc:"+got.DeliveryKey {
		t.Fatalf("requests = %+v, want one fenced live-only immediate nudge under key gc:%s", reqs, got.DeliveryKey)
	}
	calls := f.nudges()
	if len(calls) != 1 || calls[0].Method != "NudgeNow" || calls[0].Message != "also check the logs" || calls[0].Fence != wantFence {
		t.Fatalf("runtime nudges = %+v, want one fenced NudgeNow of the text", calls)
	}
	o := got.Outcome
	if o.Arm != opjournal.ArmSucceeded || o.Result.Via != laneViaSteer || o.Degradation != "" || o.Result.TurnLink != opjournal.LinkNone || o.Result.TurnBasis != opjournal.BasisNoSource {
		t.Fatalf("outcome = %+v, want succeeded via steer, no degradation, link none/no_source", o)
	}
	row, err := f.store.Get(f.id)
	if err != nil || row.Metadata[session.MetadataLastNudgeDeliveredAt] == "" {
		t.Fatalf("session row = %v, %v; want the last nudge stamped", row.Metadata, err)
	}
}

// TestLaneExecSteersUnfenced proves a steer to a row with no instance token
// goes out unfenced through the runtime's NudgeNow and closes succeeded via
// steer with unfenced_delivery.
func TestLaneExecSteersUnfenced(t *testing.T) {
	f := newLaneFixture(t, laneProvider("codex"), noLaneToken)
	f.sp.SteerModeValue = runtime.SteerProtocol
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "and the logs", steerMsg(opjournal.FallbackBoundary)))
	var mu sync.Mutex
	var reqs []worker.NudgeRequest
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		busyLane(e)
		recordRequests(e, &mu, &reqs)
		return e
	})
	if err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v; want accepted", res, err)
	}
	if len(reqs) != 1 || !reqs[0].Fence.IsZero() || reqs[0].Delivery != worker.NudgeDeliveryImmediate || !reqs[0].AllowUnfenced {
		t.Fatalf("requests = %+v, want one unfenced immediate nudge", reqs)
	}
	if calls := f.nudges(); len(calls) != 1 || calls[0].Method != "NudgeNow" || calls[0].Fence != (runtime.InputFence{}) {
		t.Fatalf("runtime nudges = %+v, want one unfenced NudgeNow", calls)
	}
	o := f.get(t, st, op.ID).Outcome
	if o.Arm != opjournal.ArmSucceeded || o.Result.Via != laneViaSteer || o.Degradation != opjournal.DegradationUnfencedDelivery {
		t.Fatalf("outcome = %+v, want succeeded via steer with unfenced_delivery", o)
	}
}

// TestLaneExecSteerAtBoundaryIsANormalTurn proves a steer to an idle session
// is delivered as a normal turn: starts mode, default delivery, via live.
func TestLaneExecSteerAtBoundaryIsANormalTurn(t *testing.T) {
	f := newLaneFixture(t, laneProvider("codex"))
	f.sp.SteerModeValue = runtime.SteerProtocol
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "go", steerMsg(opjournal.FallbackBoundary)))
	var mu sync.Mutex
	var reqs []worker.NudgeRequest
	var deliveries []opjournal.Delivery
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		recordRequests(e, &mu, &reqs)
		return recordingDeliverExec{conversationLaneExec: e, got: &deliveries}
	})
	if err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v; want accepted", res, err)
	}
	if len(deliveries) != 1 || deliveries[0].Steer || deliveries[0].Mode != opjournal.LinkStarts {
		t.Fatalf("deliveries = %+v, want one non-steer delivery in starts mode", deliveries)
	}
	if len(reqs) != 1 || reqs[0].Delivery != worker.NudgeDeliveryDefault {
		t.Fatalf("requests = %+v, want one default nudge", reqs)
	}
	if o := f.get(t, st, op.ID).Outcome; o.Arm != opjournal.ArmSucceeded || o.Result.Via != laneViaLive {
		t.Fatalf("outcome = %+v, want succeeded via live", o)
	}
}

// TestLaneExecSteerUnsupportedFamily proves a steer to a busy session whose
// family takes no keystroke steer follows its fallback: boundary waits busy
// with nothing typed, fail closes failed{unsupported}. Codex is here: tmux's
// trailing Escape would cancel its running turn.
func TestLaneExecSteerUnsupportedFamily(t *testing.T) {
	for _, provider := range []string{"gemini", "codex", "claude"} {
		t.Run(provider+"/boundary", func(t *testing.T) {
			f := newLaneFixture(t, laneProvider(provider))
			st := f.laneStore(t)
			op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi", steerMsg(opjournal.FallbackBoundary)))
			res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
				busyLane(e)
				return e
			})
			if err != nil || res.Took != opjournal.TakeNone || !res.Busy {
				t.Fatalf("Pass = %+v, %v; want a busy wait", res, err)
			}
			if len(f.nudges()) != 0 || f.get(t, st, op.ID).Status != opjournal.StatusSubmitted {
				t.Fatal("a steer the session cannot take was typed or closed")
			}
		})
		t.Run(provider+"/fail", func(t *testing.T) {
			f := newLaneFixture(t, laneProvider(provider))
			st := f.laneStore(t)
			op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi", steerMsg(opjournal.FallbackFail)))
			res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
				busyLane(e)
				return e
			})
			if err != nil || res.Took != opjournal.TakeNone {
				t.Fatalf("Pass = %+v, %v; want nothing taken", res, err)
			}
			if o := f.get(t, st, op.ID).Outcome; o.Arm != opjournal.ArmFailed || o.Reason != opjournal.ReasonUnsupported {
				t.Fatalf("outcome = %+v, want failed{unsupported}", o)
			}
			if len(f.nudges()) != 0 {
				t.Fatal("an unsupported steer was typed")
			}
		})
	}
}
