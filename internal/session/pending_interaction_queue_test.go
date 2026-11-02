package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// TestQueueSubmitQueuesAUserTurn (row A17): QueueSubmit queues the message
// for the session's next boundary as a user's submit, delivering nothing
// now, for a caller that already accepted a message the session cannot take
// yet.
func TestQueueSubmitQueuesAUserTurn(t *testing.T) {
	sp := runtime.NewFake()
	mgr, info, cityPath := newPendingQueueSession(t, sp, "claude")
	sp.SetPendingInteraction(info.SessionName, pendingTestApproval)

	if err := mgr.QueueSubmit(info.ID, "first prompt"); err != nil {
		t.Fatalf("QueueSubmit: %v", err)
	}
	if got := sp.CountCalls("Nudge", info.SessionName) + sp.CountCalls("NudgeNow", info.SessionName); got != 0 {
		t.Fatalf("typed %d messages, want none", got)
	}
	items := queuedMessages(t, cityPath, info.ID)
	if len(items) != 1 || items[0].Message != "first prompt" || items[0].Source != nudgequeue.SourceSubmit {
		t.Fatalf("queued %+v, want the message queued as a user submit", items)
	}
	if err := mgr.QueueSubmit("gc-missing", "x"); err == nil {
		t.Fatal("QueueSubmit(missing session) = nil, want an error")
	}
}

// TestInterruptNowRefusesAnUnpreparedReplacementBehindALatePrompt (rows A21,
// B3): the up-front refusal saw no prompt, the interrupt went out, and a
// permission prompt opened as the turn ended. None of the replacement's
// preparation keys (Claude's input clear, Gemini's turn reset and clear) is
// typed into the prompt, and the replacement is refused with an error
// wrapping ErrReplacementNotPrepared and ErrPendingInteraction, not queued:
// the queue would deliver it later as a plain message, onto a session never
// prepared for it.
func TestInterruptNowRefusesAnUnpreparedReplacementBehindALatePrompt(t *testing.T) {
	for _, provider := range []string{"claude", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			sp := &dialogAfterInterruptProvider{Fake: runtime.NewFake()}
			mgr, info, cityPath := newPendingQueueSession(t, sp, provider)

			out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
			if !errors.Is(err, ErrReplacementNotPrepared) || !errors.Is(err, ErrPendingInteraction) || out.Queued {
				t.Fatalf("Submit(interrupt_now) = %+v, %v; want ErrReplacementNotPrepared wrapping ErrPendingInteraction, nothing queued", out, err)
			}
			if got := callMethods(inputCalls(sp.Calls, info.SessionName)); !slices.Equal(got, []string{"Interrupt"}) {
				t.Fatalf("sent %v, want only the interrupt: no preparation key and no message into the pending prompt", got)
			}
			if items := queuedMessages(t, cityPath, info.ID); len(items) != 0 {
				t.Fatalf("queued %+v, want the unprepared replacement not queued", items)
			}
		})
	}
}

// TestInterruptNowRefusesAReplacementWhosePreparationMeetsAPrompt (row A21):
// a prompt that opens after the post-interrupt look refuses the
// preparation's own keys instead (Gemini's reset types /rewind through the
// runtime's prompt-guarded immediate nudge). That replacement is not
// prepared either: it is refused with ErrReplacementNotPrepared, nothing is
// typed after the refusal, and nothing is queued.
func TestInterruptNowRefusesAReplacementWhosePreparationMeetsAPrompt(t *testing.T) {
	refusal := fmt.Errorf("%w: approval open", runtime.ErrPendingInteraction)
	for _, tc := range []struct {
		provider string
		refuse   func(sp *runtime.Fake, name string)
		sent     []string // the input calls, the refused preparation key last
	}{
		{"gemini", func(sp *runtime.Fake, name string) { sp.ResetTurnErrors[name] = refusal }, []string{"Interrupt", "ResetInterruptedTurn"}},
		{"claude", func(sp *runtime.Fake, name string) { sp.ClearInputErrors[name] = refusal }, []string{"Interrupt", "ClearInput"}},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			sp := runtime.NewFake()
			mgr, info, cityPath := newPendingQueueSession(t, sp, tc.provider)
			tc.refuse(sp, info.SessionName)

			out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
			if !errors.Is(err, ErrReplacementNotPrepared) || !errors.Is(err, ErrPendingInteraction) || out.Queued {
				t.Fatalf("Submit(interrupt_now) = %+v, %v; want ErrReplacementNotPrepared wrapping ErrPendingInteraction, nothing queued", out, err)
			}
			if got := callMethods(inputCalls(sp.Calls, info.SessionName)); !slices.Equal(got, tc.sent) {
				t.Fatalf("sent %v, want %v: nothing typed after the refused preparation", got, tc.sent)
			}
			if items := queuedMessages(t, cityPath, info.ID); len(items) != 0 {
				t.Fatalf("queued %+v, want the unprepared replacement not queued", items)
			}
		})
	}
}

// TestInterruptNowQueuesWhenFinalSendRefusedBehindPending (row A21): a
// prompt that opens after the preparation is refused by the runtime at the
// final send; the message is queued, not dropped. The preparation ran
// before the send, so the queued replacement needs none later.
func TestInterruptNowQueuesWhenFinalSendRefusedBehindPending(t *testing.T) {
	sp := runtime.NewFake()
	mgr, info, cityPath := newPendingQueueSession(t, sp, "claude")
	sp.NudgeErrors = map[string]error{info.SessionName: fmt.Errorf("%w: approval open", runtime.ErrNudgeRefusedPendingInteraction)}

	out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil || !out.Queued {
		t.Fatalf("Submit(interrupt_now) = %+v, %v; want queued behind the refusal", out, err)
	}
	if i, j := callIndex(sp.Calls, "ClearInput", info.SessionName), callIndex(sp.Calls, "NudgeNow", info.SessionName); i < 0 || j < i {
		t.Fatalf("calls = %v, want the input cleared before the refused send", callMethods(sp.Calls))
	}
	if items := queuedMessages(t, cityPath, info.ID); len(items) != 1 || items[0].Message != "replace the turn" {
		t.Fatalf("queued %+v, want the replacement queued", items)
	}
}

// TestPendingRefusalWhoseQueueingFailsIsNotQueued: a message refused behind
// a pending interaction whose queueing then fails is reported neither queued
// nor deferred, and its error carries the refusal beside the queue's
// failure, so the caller learns it went nowhere. Nothing logs it as queued.
func TestPendingRefusalWhoseQueueingFailsIsNotQueued(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(mgr *Manager, info Info) (SubmitOutcome, error)
	}{
		{"send", func(mgr *Manager, info Info) (SubmitOutcome, error) {
			return mgr.Send(context.Background(), testActor(mgr, ActorBackground), info.ID, "you have mail", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
		}},
		{"interrupt_now", func(mgr *Manager, info Info) (SubmitOutcome, error) {
			return mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubSubmitPoller(t)
			logs := captureLog(t)
			full := errors.New("queue full")
			sp := runtime.NewFake()
			mgr := newTestManager(t, beads.NewMemStore(), sp, WithDeferredSubmitQueue(&recordingDeferredQueue{err: full}))
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			sp.NudgeErrors = map[string]error{info.SessionName: fmt.Errorf("%w: approval open", runtime.ErrNudgeRefusedPendingInteraction)}

			out, err := tc.send(mgr, info)
			if out.Queued || out.Deferred || !errors.Is(err, ErrPendingInteraction) || !errors.Is(err, full) {
				t.Fatalf("%s = %+v, %v; want neither queued nor deferred, and an error wrapping the refusal and the queue's failure", tc.name, out, err)
			}
			if strings.Contains(logs.String(), "queued a message") {
				t.Fatalf("log = %q, want nothing logged as queued", logs.String())
			}
		})
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

// TestStopTurnForReplacementBehindPendingIsNotPrepared (row B4): a stop for
// a replacement whose turn settled with a permission prompt open (it opened
// on the interrupt) types none of the preparation's keys, which would land
// in the prompt, and reports the session not prepared, as a failed
// preparation is: an error wrapping ErrReplacementNotPrepared and
// ErrPendingInteraction, never success. The lane then records
// failed{replacement_not_prepared} instead of typing the message onto an
// unprepared session once the prompt is answered. A plain stop still
// succeeds (TestStopTurnSkipsKeysBehindPending).
func TestStopTurnForReplacementBehindPendingIsNotPrepared(t *testing.T) {
	for _, provider := range []string{"claude", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			logs := captureLog(t)
			sp := runtime.NewFake()
			mgr, info, _ := newFencedStopSessionOn(t, &dialogAfterInterruptProvider{Fake: sp}, sp, provider, nil, runtime.TurnInterruptKeystroke)

			_, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{ForReplacement: true})
			if !errors.Is(err, ErrReplacementNotPrepared) || !errors.Is(err, ErrPendingInteraction) || errors.Is(err, runtime.ErrTurnNotEnded) {
				t.Fatalf("StopTurnWith(replacement, prompt open) = %v, want ErrReplacementNotPrepared wrapping ErrPendingInteraction", err)
			}
			if got := callMethods(inputCalls(sp.SnapshotCalls(), info.SessionName)); !slices.Equal(got, []string{"Interrupt"}) {
				t.Fatalf("sent %v, want only the interrupt: no preparation key into the pending prompt", got)
			}
			if !strings.Contains(logs.String(), "pending interaction") {
				t.Fatalf("log = %q, want the skipped preparation logged", logs.String())
			}
		})
	}
}
