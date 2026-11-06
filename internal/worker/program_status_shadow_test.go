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
