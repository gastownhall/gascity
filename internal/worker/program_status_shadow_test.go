package worker

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// shadowRow is a valid agent-pane row under the shadow switch whose record
// reads st since since.
func shadowRow(st runtime.ProgramState, since time.Time) runtime.ProgramPaneRow {
	row := runtime.ProgramPaneRow{
		Mode: runtime.ProgramStatusShadow, CaptureGate: true, PaneID: "%4",
		PanePipe: true, Tap: "w1.%4", TapLiveness: runtime.ProgramTapAlive,
		RecordForm: runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{
			Nonce: "w1.%4", State: st, App: "claude", Since: since, Seq: 3, Progress: -1,
			TurnStarts: 1, Probed: true, LastError: true, Reported: true,
		},
	}
	if st != runtime.ProgramWorking {
		row.Record.TurnEnds, row.Record.QuietSince = 1, since
	}
	return row
}

// Kills a wait-idle pre-read (wait_idle_preread) judged on the state cache's
// snapshot of the record (ga-f4ovjm.77: 9 harms in the second shadow, all on
// a reading up to the cache's TTL old): the runtime handle's pre-read
// records the snapshot it decided on beside the record read fresh with it,
// and delivers as with the switch off.
func TestRuntimeHandleWaitIdlePrereadJudgesTheFreshRecord(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		sp.WaitForIdleErrors = map[string]error{"worker": nil}
		mode := runtime.ProgramStatusOff
		if shadow {
			mode = runtime.ProgramStatusShadow
		}
		cached := shadowRow(runtime.ProgramWorking, time.Now())
		fresh := shadowRow(runtime.ProgramIdle, time.Now())
		cached.Mode, fresh.Mode = mode, mode
		sp.SetProgramCachedPane("worker", cached)
		sp.SetProgramPane("worker", fresh)
		var got []runtime.ProgramStatusComparison
		if shadow {
			sp.ProgramShadowSink = func(c runtime.ProgramStatusComparison) { got = append(got, c) }
		}
		handle, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: runtime.FullFake{Fake: sp}, SessionName: "worker", ProviderName: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		result, err := handle.Nudge(context.Background(), testActor(t, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Delivery: NudgeDeliveryWaitIdle})
		if err != nil || !result.Delivered {
			t.Fatalf("shadow %v: Nudge = %+v, %v; want delivered, as with the switch off", shadow, result, err)
		}
		if !shadow {
			if len(got) != 0 {
				t.Fatalf("the switch off recorded %+v", got)
			}
			continue
		}
		var pre []runtime.ProgramStatusComparison
		for _, c := range got {
			if c.Point == runtime.ProgramPointWaitIdlePreread {
				pre = append(pre, c)
			}
		}
		if len(pre) != 1 || pre[0].Scrape != runtime.ProgramVerdictIdle || pre[0].Program != runtime.ProgramVerdictIdle || pre[0].Status.State != runtime.ProgramIdle {
			t.Fatalf("pre-reads %+v, want one wait_idle_preread idle/idle on the pane's own idle record", pre)
		}
	}
}

// Kills an observation (observation) judged on a reading the worker's view
// does not decide with (review of ga-f4ovjm.77: the record read fresh, past
// the state cache's snapshot): under on, SessionHandle.State takes its phase
// from the reading it carries (ProgramPhase), so a turn that ended inside
// the cache's TTL is busy there. The record is judged on that reading, with
// no read of its own, and its busy is the phase on settles on.
func TestSessionHandleStateObservesTheReadingItsPhaseDecidesWith(t *testing.T) {
	handle, fake, name := startedProgramStatusHandle(t)
	ctx := context.Background()
	snapshot := shadowRow(runtime.ProgramWorking, time.Now())
	fake.SetProgramCachedPane(name, snapshot)
	fake.SetProgramPane(name, shadowRow(runtime.ProgramIdle, time.Now()))
	var got []runtime.ProgramStatusComparison
	fake.ProgramShadowSink = func(c runtime.ProgramStatusComparison) {
		if c.Point == runtime.ProgramPointObservation {
			got = append(got, c)
		}
	}
	state, err := handle.State(ctx)
	if err != nil || state.Phase != PhaseReady || state.Program.State != runtime.ProgramWorking {
		t.Fatalf("shadow: %+v, %v; want ready carrying the snapshot's working reading", state, err)
	}
	if len(got) != 1 || got[0].Scrape != runtime.ProgramVerdictIdle || got[0].Program != runtime.ProgramVerdictBusy || got[0].Status != state.Program {
		t.Fatalf("recorded %+v, want observation idle/busy on the reading State carries", got)
	}
	if fresh := fake.CountCalls("ProgramStatusShadowRead", name); fresh != 0 {
		t.Fatalf("the observation took %d fresh reads, want none", fresh)
	}

	snapshot.Mode = runtime.ProgramStatusOn
	fake.SetProgramCachedPane(name, snapshot)
	got = nil
	if state, err := handle.State(ctx); err != nil || state.Phase != PhaseBusy {
		t.Fatalf("on: %+v, %v; want busy from the snapshot's working reading, as the shadow judged it", state, err)
	}
	if len(got) != 0 {
		t.Fatalf("on recorded %+v, want nothing", got)
	}
}
