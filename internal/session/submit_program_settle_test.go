package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ga-f4ovjm.76: under program_status = "on" the runtime's idle wait after an
// interrupt ends with an error wrapping runtime.ErrPendingInteraction when
// the interrupted program reports a dialog instead of settling (the tmux
// leaf's record-backed interrupt settle). The interrupt is sent; no more keys
// may follow (they would answer the dialog), and the replacing message waits
// behind the dialog instead of a hard restart.

// errSettleBlocked is the idle wait's answer when the program reports a
// dialog after the interrupt.
var errSettleBlocked = fmt.Errorf("session %q: the program reports a dialog after the interrupt: %w", "s", runtime.ErrPendingInteraction)

// interruptNowCalls counts sp's calls by method for session name.
func interruptNowCalls(sp *runtime.Fake, name string) map[string]int {
	got := map[string]int{}
	for _, c := range sp.Calls {
		if c.Name == name {
			got[c.Method]++
		}
	}
	return got
}

// Kills an interrupt_now that restarts the session, clears its input or
// types the message into a dialog its program raised after the interrupt:
// the message is queued behind the dialog instead.
func TestInterruptNowQueuesBehindADialogTheSettleReports(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := newTestManager(t, store, sp)
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: provider, WorkDir: t.TempDir(), Provider: provider, ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			sp.WaitForIdleSequence[info.SessionName] = []error{errSettleBlocked}
			outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
			if err != nil {
				t.Fatalf("Submit(interrupt_now): %v; want the message queued behind the dialog", err)
			}
			if !outcome.Queued {
				t.Fatal("Submit(interrupt_now) not queued behind the dialog")
			}
			calls := interruptNowCalls(sp, info.SessionName)
			for _, m := range []string{"Stop", "ClearInput", "NudgeNow", "Nudge", "WaitForInterruptBoundary"} {
				if calls[m] != 0 {
					t.Fatalf("calls %v: want no %s after the settle reported a dialog", sp.Calls, m)
				}
			}
			// Codex's soft Escape is followed by no Ctrl-C fallback: the
			// one wait ended on the dialog. The tmux leaf answers that wait
			// so once the session noted its Escape as the interrupt
			// (tmux's TestInterruptNowQueuesBehindADialogRaisedAfterTheSoftEscape).
			if calls["WaitForIdle"] != 1 {
				t.Fatalf("%d idle waits; want the one that reported the dialog", calls["WaitForIdle"])
			}
			if provider == "codex" && calls["Interrupt"] != 0 {
				t.Fatalf("calls %v: want no control-c fallback into the dialog", sp.Calls)
			}
		})
	}
}

// Kills a checked stop that sends its follow-on keys after the settle
// reported a dialog: the stop fails as a turn not ended, carrying the
// pending interaction, and sends no clear.
func TestStopTurnSendsNoKeysAfterTheSettleReportsADialog(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleSequence[info.SessionName] = []error{errSettleBlocked}
	err = mgr.StopTurn(info.ID)
	if err == nil {
		t.Fatal("StopTurn settled though the program reports a dialog")
	}
	if !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("StopTurn = %v; want it to carry the pending interaction", err)
	}
	if calls := interruptNowCalls(sp, info.SessionName); calls["ClearInput"] != 0 || calls["SendKeys"] != 0 {
		t.Fatalf("calls %v: want no follow-on keys", sp.Calls)
	}
}
