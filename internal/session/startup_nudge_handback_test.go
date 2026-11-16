package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

// handbackStartNudge is the startup nudge handbackStartProvider hands back.
const handbackStartNudge = "start working"

// handbackStartProvider models a leaf whose program held the startup nudge
// back behind a dialog through the whole startup ladder (program_status =
// "on", design §10.2 row A20): each Start that succeeds hands nudge back
// once; a Start that fails (fail) hands nothing back, as the tmux leaf clears
// a failed start's handback.
type handbackStartProvider struct {
	*runtime.Fake
	nudge string
	fail  error
}

func (p *handbackStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.fail != nil {
		return p.fail
	}
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	// The launched process's environment, as tmux publishes it.
	if token := cfg.Env[runtime.InstanceTokenEnv]; token != "" {
		if err := p.SetMeta(name, runtime.InstanceTokenEnv, token); err != nil {
			return err
		}
	}
	if p.nudge != "" {
		p.SetStartupNudgeHandback(name, p.nudge)
	}
	return nil
}

func (p *handbackStartProvider) TakeStartupNudgeHandback(name string) (string, bool) {
	return runtime.FullFake{Fake: p.Fake}.TakeStartupNudgeHandback(name)
}

// queuedHandbacks returns the nudge items queued for id in mgr's city.
func queuedHandbacks(t *testing.T, mgr *Manager, id string) []nudgequeue.Item {
	t.Helper()
	city, err := mgr.CityDir()
	if err != nil {
		t.Fatal(err)
	}
	return queuedMessages(t, city.Path(), id)
}

// wantHandbacksQueued fails unless n copies of handbackStartNudge are queued
// for id as a user's submit, delivered verbatim.
func wantHandbacksQueued(t *testing.T, mgr *Manager, id string, n int) {
	const nudge = handbackStartNudge
	t.Helper()
	items := queuedHandbacks(t, mgr, id)
	if len(items) != n {
		t.Fatalf("queued %+v; want %d startup handback(s)", items, n)
	}
	for _, it := range items {
		if it.Message != nudge || it.Source != nudgequeue.SourceSubmit {
			t.Fatalf("queued %+v; want the startup nudge %q as a user's submit", it, nudge)
		}
	}
}

// Kills a session-manager start (create, resume, runtime-only bring-up)
// that drops the startup nudge its runtime handed back, or queues one from
// a start that failed.
func TestSessionStartsQueueTheHandedBackStartupNudge(t *testing.T) {
	const nudge = handbackStartNudge
	t.Run("create", func(t *testing.T) {
		stubSubmitPoller(t)
		sp := &handbackStartProvider{Fake: runtime.NewFake(), nudge: nudge}
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		wantHandbacksQueued(t, mgr, info.ID, 1)
	})
	t.Run("resume", func(t *testing.T) {
		stubSubmitPoller(t)
		sp := &handbackStartProvider{Fake: runtime.NewFake()}
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		sp.nudge = nudge
		if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		wantHandbacksQueued(t, mgr, info.ID, 1)
	})
	t.Run("resume that fails", func(t *testing.T) {
		stubSubmitPoller(t)
		sp := &handbackStartProvider{Fake: runtime.NewFake()}
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		sp.nudge, sp.fail = nudge, errors.New("provider unavailable")
		if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err == nil {
			t.Fatal("Start succeeded; want the provider's failure")
		}
		wantHandbacksQueued(t, mgr, info.ID, 0)
	})
	t.Run("runtime-only", func(t *testing.T) {
		stubSubmitPoller(t)
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store)
		sp := &handbackStartProvider{Fake: runtime.NewFake(), nudge: nudge}
		mgr := newTestManager(t, store, sp)
		if err := startRuntimeOnly(t, mgr, b.ID, "claude", runtime.Config{}); err != nil {
			t.Fatalf("StartRuntimeOnly: %v", err)
		}
		wantHandbacksQueued(t, mgr, b.ID, 1)
	})
}

// Kills a stop with a restart on failure that hard-restarts a session whose
// interrupted program raised a dialog (the settle's error wraps
// ErrPendingInteraction), on either path, a Settle that replaces the waits'
// error included: the restart is withheld, and the error carries the pending
// interaction. A dialog the interrupt itself canceled settles (the leaf's
// settle reports pending only for one raised since), so it never gets here.
func TestStopTurnWithRestartWithheldBehindADialog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		settle func(waitErr error) error
	}{
		{"session-scoped", nil},
		// The turn-scoped settle (the worker's awaitTurnEnd) names the
		// waits' error in its text only.
		{"settle replaces the waits' error", func(waitErr error) error { return TurnNotEndedError("interrupted turn", waitErr) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
			sp.WaitForIdleErrors[info.SessionName] = errSettleBlocked
			report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController), Settle: tc.settle})
			if !errors.Is(err, ErrPendingInteraction) || !errors.Is(err, runtime.ErrTurnNotEnded) || report.Restarted {
				t.Fatalf("StopTurnWith(restart) = %+v, %v; want the pending interaction, the turn not ended, and no restart", report, err)
			}
			if n := countMethod(sp.SnapshotCalls(), "Stop"); n != 0 {
				t.Fatalf("calls = %v, want no Stop", callMethods(sp.SnapshotCalls()))
			}
		})
	}
}

// wantOneStartupTurn fails unless exactly one item is queued for id: the
// startup nudge as a user's submit, bound to the launch of token.
func wantOneStartupTurn(t *testing.T, mgr *Manager, id, token string) {
	t.Helper()
	items := queuedHandbacks(t, mgr, id)
	if len(items) != 1 {
		t.Fatalf("queued %+v; want one startup turn", items)
	}
	launch, ok := nudgequeue.StartupTurnLaunch(items[0])
	if !ok || launch != runtime.LaunchID(token) || items[0].Message != handbackStartNudge || items[0].Source != nudgequeue.SourceSubmit {
		t.Fatalf("queued %+v; want the startup nudge bound to the launch of the live token", items[0])
	}
	if strings.Contains(items[0].Reference.ID, token) {
		t.Fatalf("the queued turn carries its launch's token")
	}
}

// liveToken is the instance token the runtime reports for name.
func liveToken(t *testing.T, sp runtime.Provider, name string) string {
	t.Helper()
	token, err := sp.GetMeta(name, runtime.InstanceTokenEnv)
	if err != nil || token == "" {
		t.Fatalf("live token of %s = %q, %v", name, token, err)
	}
	return token
}

// Kills a start that queues its handed-back first turn unbound, or bound to
// a token the runtime does not run: create, resume and the runtime-only
// bring-up each queue one turn, bound to the live launch's token.
func TestSessionStartsBindTheirStartupTurnToTheLaunch(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		stubSubmitPoller(t)
		sp := &handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge}
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		wantOneStartupTurn(t, mgr, info.ID, liveToken(t, sp, info.SessionName))
	})
	t.Run("resume", func(t *testing.T) {
		stubSubmitPoller(t)
		sp := &handbackStartProvider{Fake: runtime.NewFake()}
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		created := liveToken(t, sp, info.SessionName)
		if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		sp.nudge = handbackStartNudge
		if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		resumed := liveToken(t, sp, info.SessionName)
		if resumed == created {
			t.Fatalf("the resume minted no new token")
		}
		wantOneStartupTurn(t, mgr, info.ID, resumed)
	})
	t.Run("runtime-only", func(t *testing.T) {
		stubSubmitPoller(t)
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store)
		sp := &handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge}
		mgr := newTestManager(t, store, sp)
		if err := startRuntimeOnly(t, mgr, b.ID, "claude", runtime.Config{}); err != nil {
			t.Fatalf("StartRuntimeOnly: %v", err)
		}
		wantOneStartupTurn(t, mgr, b.ID, liveToken(t, sp, "s-mint"))
	})
	t.Run("an unreadable live token binds to the minted one", func(t *testing.T) {
		stubSubmitPoller(t)
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store)
		sp := &handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge}
		sp.GetMetaErrors["s-mint"] = map[string]error{runtime.InstanceTokenEnv: runtime.ErrRuntimeUnavailable}
		mgr := newTestManager(t, store, sp)
		if err := startRuntimeOnly(t, mgr, b.ID, "claude", runtime.Config{}); err != nil {
			t.Fatalf("StartRuntimeOnly: %v", err)
		}
		var minted string
		for _, call := range sp.SnapshotCalls() {
			if call.Method == "Start" {
				minted = call.Config.Env[runtime.InstanceTokenEnv]
			}
		}
		if minted == "" {
			t.Fatal("the start carried no token")
		}
		wantOneStartupTurn(t, mgr, b.ID, minted)
	})
}

// Kills the binding read from the start config rather than the runtime: the
// claim judges a turn by the token the runtime reports, so the turn is bound
// to it, a box whose token differs from the start's included; with no token
// at all the turn is queued as a plain submit.
func TestStartupTurnLaunchIDReadsTheRuntimeFirst(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "s-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if got := StartupTurnLaunchID(fake, "s-1", "tok-start"); got != runtime.LaunchID("tok-start") {
		t.Fatalf("with no live token = %q, want the start token's launch", got)
	}
	if err := fake.SetMeta("s-1", runtime.InstanceTokenEnv, "tok-box"); err != nil {
		t.Fatal(err)
	}
	if got := StartupTurnLaunchID(fake, "s-1", "tok-start"); got != runtime.LaunchID("tok-box") {
		t.Fatalf("with a live token = %q, want the box's launch", got)
	}
	if got := StartupTurnLaunchID(fake, "s-gone", ""); got != "" {
		t.Fatalf("with no token anywhere = %q, want none", got)
	}
	turn := nudgequeue.Item{ID: "t", Message: handbackStartNudge, Source: nudgequeue.SourceSubmit}
	if items := StartupHandbackItems(turn, "", "", "m"); len(items) != 1 || items[0].Reference != nil {
		t.Fatalf("an unbound turn = %+v, want one plain submit", items)
	}
}

// Kills a one-shot initial message carried inside a turn a claim may drop,
// or split from text it was not composed into.
func TestStartupHandbackItemsSplitTheInitialMessage(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	base := nudgequeue.Item{ID: "t", SessionID: "s-1", ContinuationEpoch: "2", Source: nudgequeue.SourceSubmit, CreatedAt: now, DeliverAfter: now.Add(time.Second)}
	composed := base
	composed.Message = AppendInitialMessage(handbackStartNudge, "hello")
	items := StartupHandbackItems(composed, "L1", "hello", "m")
	if len(items) != 2 {
		t.Fatalf("items = %+v, want the turn and the message", items)
	}
	turn, msg := items[0], items[1]
	if launch, ok := nudgequeue.StartupTurnLaunch(turn); !ok || launch != "L1" || turn.Message != handbackStartNudge || turn.ID != "t" {
		t.Fatalf("turn = %+v", turn)
	}
	if msg.Reference != nil || msg.Message != "hello" || msg.ID != "m" || msg.Source != nudgequeue.SourceSubmit ||
		msg.SessionID != "s-1" || msg.ContinuationEpoch != "2" || !msg.DeliverAfter.Equal(turn.DeliverAfter) || !msg.CreatedAt.Equal(now.Add(time.Nanosecond)) {
		t.Fatalf("message = %+v, want an unbound submit a nanosecond after the turn", msg)
	}
	alone := base
	alone.Message = AppendInitialMessage("", "hello")
	if items := StartupHandbackItems(alone, "L1", "hello", "m"); len(items) != 1 || items[0].Reference != nil || items[0].Message != "hello" {
		t.Fatalf("a message with no turn = %+v, want the message alone", items)
	}
	plain := base
	plain.Message = handbackStartNudge + "\n\nUser message:\nhello"
	if items := StartupHandbackItems(plain, "L1", "hello", "m"); len(items) != 1 || items[0].Message != plain.Message {
		t.Fatalf("text that is not the composition = %+v, want it whole", items)
	}
}

func TestInitialMessageCodecRoundTrips(t *testing.T) {
	for _, nudge := range []string{"", handbackStartNudge, "a\n\n---\n\nb"} {
		turn, ok := SplitInitialMessage(AppendInitialMessage(nudge, "hi\nthere"), "hi\nthere")
		if !ok || turn != nudge {
			t.Errorf("split(append(%q)) = %q, %v", nudge, turn, ok)
		}
	}
	for text, msg := range map[string]string{
		"hello":                        "",
		handbackStartNudge:             "hi",
		"User message:\nhi there":      "hi",
		"\n\n---\n\nUser message:\nhi": "hi",
	} {
		if turn, ok := SplitInitialMessage(text, msg); ok {
			t.Errorf("SplitInitialMessage(%q, %q) = %q, true; want no composition", text, msg, turn)
		}
	}
}

// Kills the legacy prepared start leaving the one-shot initial message in a
// turn a claim may drop: the runtime-only start splits a composed nudge into
// the turn and the message, queued in that order.
func TestRuntimeOnlyStartSplitsTheInitialMessage(t *testing.T) {
	stubSubmitPoller(t)
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store, "template_overrides", `{"initial_message":"hello"}`)
	sp := &handbackStartProvider{Fake: runtime.NewFake(), nudge: AppendInitialMessage(handbackStartNudge, "hello")}
	mgr := newTestManager(t, store, sp)
	if err := startRuntimeOnly(t, mgr, b.ID, "claude", runtime.Config{}); err != nil {
		t.Fatalf("StartRuntimeOnly: %v", err)
	}
	items := queuedHandbacks(t, mgr, b.ID)
	if len(items) != 2 || items[0].Message != handbackStartNudge || !nudgequeue.IsStartupTurn(items[0]) ||
		items[1].Message != "hello" || nudgequeue.IsStartupTurn(items[1]) {
		t.Fatalf("queued %+v; want the turn, then the initial message as its own submit", items)
	}
}

// Kills a start that queues a turn when no launch stands: a failed resume
// leaves the leaf's handback dropped, not queued.
func TestAFailedStartQueuesNoStartupTurn(t *testing.T) {
	stubSubmitPoller(t)
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store)
	sp := &handbackStartProvider{Fake: runtime.NewFake()}
	sp.StartErrors = map[string]error{"s-mint": errors.New("provider unavailable")}
	sp.SetStartupNudgeHandback("s-mint", handbackStartNudge)
	mgr := newTestManager(t, store, sp)
	if err := startRuntimeOnly(t, mgr, b.ID, "claude", runtime.Config{}); err == nil {
		t.Fatal("StartRuntimeOnly succeeded; want the provider's failure")
	}
	wantHandbacksQueued(t, mgr, b.ID, 0)
	if _, ok := runtime.TakeStartupNudgeHandback(sp, "s-mint"); ok {
		t.Fatal("a failed start left its handback to take")
	}
}

// staleKeyHandbackProvider is a handback leaf whose resumed agent finds no
// conversation under its key and exits at once: every Start hands its first
// turn back, and a resume's agent is gone by the stale-key check.
type staleKeyHandbackProvider struct {
	*handbackStartProvider
}

func (p staleKeyHandbackProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.handbackStartProvider.Start(ctx, name, cfg); err != nil {
		return err
	}
	if strings.Contains(cfg.Command, "--resume") {
		return p.Stop(name)
	}
	return nil
}

// Kills a stale-key resume that queues a first turn per launch: the resume
// hands one back, its agent dies, and the fresh retry hands back another.
// The start takes the handback once, after the stale-key check, so the one
// wake queues exactly one turn, bound to the relaunch that stands.
func TestStaleKeyResumeQueuesOneStartupTurn(t *testing.T) {
	stubSubmitPoller(t)
	sp := staleKeyHandbackProvider{&handbackStartProvider{Fake: runtime.NewFake()}}
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	hints := runtime.Config{WorkDir: t.TempDir()}
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Command: "claude", WorkDir: hints.WorkDir, Provider: "claude", Resume: ProviderResume{ResumeFlag: "--resume", SessionIDFlag: "--session-id"}, Hints: hints, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.nudge = handbackStartNudge
	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, "claude --resume "+info.SessionKey, hints); err != nil {
		t.Fatalf("Start over a stale key: %v", err)
	}
	wantOneStartupTurn(t, mgr, info.ID, liveToken(t, sp, info.SessionName))
}

// Kills a send that types its message ahead of the first turn, or skips the
// startup dialog pass: a Send that starts a codex session whose runtime
// handed its first turn back dismisses the dialogs once, types nothing,
// queues the message behind the turn, and leaves the dialogs unverified for
// the next send that types.
func TestSendThatStartsTheSessionQueuesBehindTheStartupTurn(t *testing.T) {
	stubSubmitPoller(t)
	sp := &handbackStartProvider{Fake: runtime.NewFake()}
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.nudge = handbackStartNudge
	before := len(sp.SnapshotCalls())
	outcome, err := mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	calls := sp.SnapshotCalls()[before:]
	for _, call := range calls {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			t.Fatalf("Send typed %q live; want it queued behind the startup turn", call.Message)
		}
	}
	if n := countMethod(calls, "DismissKnownDialogs"); n != 1 {
		t.Fatalf("dialog dismissals = %d (calls %v), want the pre-send pass once", n, callMethods(calls))
	}
	if !outcome.Queued {
		t.Fatalf("Send outcome %+v; want queued", outcome)
	}
	items := queuedHandbacks(t, mgr, info.ID)
	if len(items) != 2 || !nudgequeue.IsStartupTurn(items[0]) || items[1].Message != "hello" {
		t.Fatalf("queued %+v; want the startup turn, then the message", items)
	}
	b, err := mgr.store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata[startupDialogVerifiedKey] != "" {
		t.Fatalf("%s = %q; nothing was typed, so the dialogs are not verified", startupDialogVerifiedKey, b.Metadata[startupDialogVerifiedKey])
	}
}

// Kills a hard restart's send, or a wait-idle nudge, that types ahead of the
// first turn its own start queued: the restart queues its message behind the
// turn, and the wait-idle nudge reports itself undelivered for its caller to
// queue, typing nothing.
func TestRestartSendAndWaitIdleNudgeWaitBehindTheStartupTurn(t *testing.T) {
	for _, path := range []string{"restart and send", "wait-idle nudge"} {
		t.Run(path, func(t *testing.T) {
			stubSubmitPoller(t)
			sp := &handbackStartProvider{Fake: runtime.NewFake()}
			mgr := newTestManager(t, beads.NewMemStore(), sp)
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
				t.Fatalf("Suspend: %v", err)
			}
			b, sessName, err := mgr.sessionBead(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			sp.nudge = handbackStartNudge
			before := len(sp.SnapshotCalls())
			hints := runtime.Config{WorkDir: info.WorkDir}
			var queued bool
			if path == "restart and send" {
				queued, err = mgr.restartAndSendLocked(context.Background(), testActor(mgr, ActorOperator), info.ID, b, sessName, "hello", BuildResumeCommand(info), hints)
			} else {
				var delivered bool
				delivered, err = mgr.tryWaitIdleNudgeLocked(context.Background(), testActor(mgr, ActorOperator), info.ID, b, deferredNudgeSource, sessName, "hello", BuildResumeCommand(info), hints)
				queued = !delivered
			}
			if err != nil || !queued {
				t.Fatalf("%s = queued %v, %v; want it left behind the turn", path, queued, err)
			}
			for _, call := range sp.SnapshotCalls()[before:] {
				if call.Method == "Nudge" || call.Method == "NudgeNow" {
					t.Fatalf("%s typed %q live; want it behind the startup turn", path, call.Message)
				}
			}
			items := queuedHandbacks(t, mgr, info.ID)
			if len(items) == 0 || !nudgequeue.IsStartupTurn(items[0]) {
				t.Fatalf("queued %+v; want the startup turn first", items)
			}
			if path == "restart and send" && (len(items) != 2 || items[1].Message != "hello") {
				t.Fatalf("queued %+v; want the message behind the turn", items)
			}
		})
	}
}
