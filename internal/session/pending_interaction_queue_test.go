package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// pendingTestApproval is the permission prompt these tests raise.
var pendingTestApproval = &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "Allow Bash?"}

// dialogAfterInterruptProvider raises a permission prompt as the session's
// turn is interrupted: the pending look before the interrupt sees nothing,
// and every look after it sees the prompt.
type dialogAfterInterruptProvider struct{ *runtime.Fake }

func (p *dialogAfterInterruptProvider) Interrupt(name string) error {
	if err := p.Fake.Interrupt(name); err != nil {
		return err
	}
	p.SetPendingInteraction(name, pendingTestApproval)
	return nil
}

func (p *dialogAfterInterruptProvider) SendKeys(name string, keys ...string) error {
	if err := p.Fake.SendKeys(name, keys...); err != nil {
		return err
	}
	if len(keys) == 1 && keys[0] == "Escape" {
		p.SetPendingInteraction(name, pendingTestApproval)
	}
	return nil
}

// stubSubmitPoller keeps a deferred submit from starting its poller process.
func stubSubmitPoller(t *testing.T) {
	t.Helper()
	orig := startSessionSubmitPoller
	startSessionSubmitPoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startSessionSubmitPoller = orig })
}

// queuedMessages lists the messages queued for session id under cityPath.
func queuedMessages(t *testing.T, cityPath, id string) []nudgequeue.Item {
	t.Helper()
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	var out []nudgequeue.Item
	for _, item := range state.Pending {
		if item.SessionID == id {
			out = append(out, item)
		}
	}
	return out
}

func newPendingQueueSession(t *testing.T, sp runtime.Provider, provider string) (*Manager, Info, string) {
	t.Helper()
	stubSubmitPoller(t)
	cityPath := t.TempDir()
	mgr := NewManagerWithOptions(beads.NewMemStore(), sp, WithCityPath(cityPath))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: provider, WorkDir: t.TempDir(), Provider: provider, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return mgr, info, cityPath
}

// TestSendQueuesBehindPendingInteraction (row A14): a default, non-immediate
// send to a session waiting on a permission prompt is queued for delivery
// after it is answered, as a send the runtime defers for a busy turn is,
// never refused and dropped. An immediate send still refuses.
func TestSendQueuesBehindPendingInteraction(t *testing.T) {
	sp := runtime.NewFake()
	mgr, info, cityPath := newPendingQueueSession(t, sp, "claude")
	sp.SetPendingInteraction(info.SessionName, pendingTestApproval)
	hints := runtime.Config{WorkDir: info.WorkDir}

	out, err := mgr.Send(context.Background(), testActor(mgr, ActorBackground), info.ID, "you have mail", BuildResumeCommand(info), hints)
	if err != nil || !out.Queued {
		t.Fatalf("Send = %+v, %v; want queued behind the pending interaction", out, err)
	}
	if got := sp.CountCalls("Nudge", info.SessionName) + sp.CountCalls("NudgeNow", info.SessionName); got != 0 {
		t.Fatalf("typed %d nudges into a pending prompt, want none", got)
	}
	items := queuedMessages(t, cityPath, info.ID)
	if len(items) != 1 || items[0].Message != "you have mail" || items[0].Source != deferredNudgeSource {
		t.Fatalf("queued %+v, want the message queued as a deferred nudge", items)
	}

	_, err = mgr.SendImmediate(context.Background(), testActor(mgr, ActorBackground), info.ID, "now", BuildResumeCommand(info), hints)
	if !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("SendImmediate = %v, want ErrPendingInteraction", err)
	}
	if got := len(queuedMessages(t, cityPath, info.ID)); got != 1 {
		t.Fatalf("queued %d items, want the refused immediate send not queued", got)
	}
}

// TestSendQueuesWhenRuntimeRefusesBehindPendingInteraction (row A14): a
// prompt that opens after the manager's look is refused by the runtime; a
// default send queues it all the same.
func TestSendQueuesWhenRuntimeRefusesBehindPendingInteraction(t *testing.T) {
	sp := runtime.NewFake()
	mgr, info, cityPath := newPendingQueueSession(t, sp, "claude")
	sp.NudgeErrors = map[string]error{info.SessionName: fmt.Errorf("%w: approval open", runtime.ErrNudgeRefusedPendingInteraction)}

	out, err := mgr.Send(context.Background(), testActor(mgr, ActorBackground), info.ID, "late", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	if err != nil || !out.Queued {
		t.Fatalf("Send = %+v, %v; want queued behind the runtime's refusal", out, err)
	}
	if items := queuedMessages(t, cityPath, info.ID); len(items) != 1 || items[0].Message != "late" {
		t.Fatalf("queued %+v, want the refused send queued", items)
	}
}

// TestInterruptNowQueuesWhenDialogAppearsAfterInterrupt (rows A21, B3): the
// up-front refusal saw no prompt, the interrupt went out, and a permission
// prompt opened as the turn ended. The turn is already interrupted, so the
// message is queued for after the prompt is answered rather than dropped,
// and the input-box clear that would land in the prompt is not sent.
func TestInterruptNowQueuesWhenDialogAppearsAfterInterrupt(t *testing.T) {
	sp := &dialogAfterInterruptProvider{Fake: runtime.NewFake()}
	mgr, info, cityPath := newPendingQueueSession(t, sp, "claude")

	out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil || !out.Queued {
		t.Fatalf("Submit(interrupt_now) = %+v, %v; want the message queued behind the new prompt", out, err)
	}
	if sp.CountCalls("Interrupt", info.SessionName) == 0 {
		t.Fatalf("calls = %v, want the interrupt sent", callMethods(sp.Calls))
	}
	for _, method := range []string{"ClearInput", "NudgeNow", "Nudge"} {
		if got := sp.CountCalls(method, info.SessionName); got != 0 {
			t.Fatalf("calls = %v, want no %s into the pending prompt", callMethods(sp.Calls), method)
		}
	}
	items := queuedMessages(t, cityPath, info.ID)
	if len(items) != 1 || items[0].Message != "replace the turn" || items[0].Source != nudgequeue.SourceSubmit {
		t.Fatalf("queued %+v, want the replacement queued as a submit", items)
	}
}

// TestInterruptNowQueuesWhenFinalSendRefusedBehindPending (row A21): a
// prompt that opens after the post-interrupt look is refused by the runtime
// at the final send; the message is queued, not dropped.
func TestInterruptNowQueuesWhenFinalSendRefusedBehindPending(t *testing.T) {
	sp := runtime.NewFake()
	mgr, info, cityPath := newPendingQueueSession(t, sp, "claude")
	sp.NudgeErrors = map[string]error{info.SessionName: fmt.Errorf("%w: approval open", runtime.ErrNudgeRefusedPendingInteraction)}

	out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil || !out.Queued {
		t.Fatalf("Submit(interrupt_now) = %+v, %v; want queued behind the refusal", out, err)
	}
	if items := queuedMessages(t, cityPath, info.ID); len(items) != 1 || items[0].Message != "replace the turn" {
		t.Fatalf("queued %+v, want the replacement queued", items)
	}
}

// TestInterruptNowHardRestartQueuesBehindPending (row A22): a hard-restart
// family's replacement runtime came up showing a prompt that refuses the
// send; the session was already replaced, so the message is queued.
func TestInterruptNowHardRestartQueuesBehindPending(t *testing.T) {
	sp := runtime.NewFake()
	mgr, info, cityPath := newPendingQueueSession(t, sp, "pi")
	sp.NudgeErrors = map[string]error{info.SessionName: fmt.Errorf("%w: approval open", runtime.ErrNudgeRefusedPendingInteraction)}

	out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil || !out.Queued {
		t.Fatalf("Submit(interrupt_now) = %+v, %v; want queued behind the refusal", out, err)
	}
	if sp.CountCalls("Stop", info.SessionName) == 0 {
		t.Fatalf("calls = %v, want the hard restart", callMethods(sp.Calls))
	}
	if items := queuedMessages(t, cityPath, info.ID); len(items) != 1 || items[0].Message != "replace the turn" {
		t.Fatalf("queued %+v, want the replacement queued", items)
	}
}

// TestInterruptNowStillRefusesUpFrontBehindPending: a prompt already open
// refuses interrupt_now before any key, as before.
func TestInterruptNowStillRefusesUpFrontBehindPending(t *testing.T) {
	sp := runtime.NewFake()
	mgr, info, cityPath := newPendingQueueSession(t, sp, "claude")
	sp.SetPendingInteraction(info.SessionName, pendingTestApproval)

	_, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("Submit(interrupt_now) = %v, want ErrPendingInteraction", err)
	}
	if sp.CountCalls("Interrupt", info.SessionName) != 0 {
		t.Fatalf("calls = %v, want nothing sent", callMethods(sp.Calls))
	}
	if items := queuedMessages(t, cityPath, info.ID); len(items) != 0 {
		t.Fatalf("queued %+v, want nothing queued for a refused request", items)
	}
}

// TestStopTurnSkipsKeysBehindPending (row B4): a stop is allowed under a
// pending interaction and sends its interrupt, but none of its follow-on
// keys (the input clear, the background-work stop), which would land in the
// prompt. The stop still succeeds and logs the skip.
func TestStopTurnSkipsKeysBehindPending(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			logs := captureLog(t)
			mgr, sp, info := newStopTestSession(t, provider)
			sp.SetPendingInteraction(info.SessionName, pendingTestApproval)

			if err := mgr.StopTurn(info.ID); err != nil {
				t.Fatalf("StopTurn = %v, want the stop allowed", err)
			}
			if sp.CountCalls("Interrupt", info.SessionName)+sp.CountCalls("SendKeys", info.SessionName) == 0 {
				t.Fatalf("calls = %v, want the interrupt sent", callMethods(sp.Calls))
			}
			for _, method := range []string{"ClearInput", "StopBackgroundWork"} {
				if got := sp.CountCalls(method, info.SessionName); got != 0 {
					t.Fatalf("calls = %v, want no %s behind the pending prompt", callMethods(sp.Calls), method)
				}
			}
			if !strings.Contains(logs.String(), "pending interaction") {
				t.Fatalf("log = %q, want the skipped follow-on keys logged", logs.String())
			}
		})
	}
}
