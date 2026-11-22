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
	nudge     string
	notBefore time.Time
	fail      error
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
		p.SetStartupNudgeHandbackAt(name, runtime.StartupNudge{Text: p.nudge, NotBefore: p.notBefore})
	}
	return nil
}

func (p *handbackStartProvider) TakeStartupNudgeHandback(name string) (runtime.StartupNudge, bool) {
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

// Kills a session-manager start that queues a handed-back startup nudge
// ready at once although its runtime gave it an earliest send time (an exec
// pack without an idle boundary holds its first turn ready_delay_ms past the
// launch): the queued submit is not delivered before NotBefore.
func TestSessionStartQueuesTheHandbackNotBefore(t *testing.T) {
	stubSubmitPoller(t)
	notBefore := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	sp := &handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge, notBefore: notBefore}
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantHandbacksQueued(t, mgr, info.ID, 1)
	if got := queuedHandbacks(t, mgr, info.ID)[0].DeliverAfter; got.Before(notBefore) {
		t.Fatalf("queued DeliverAfter = %s, want no earlier than the handback's NotBefore %s", got, notBefore)
	}
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
	if items := StartupHandbackItems(turn, time.Time{}, "", "", "m"); len(items) != 1 || items[0].Reference != nil {
		t.Fatalf("an unbound turn = %+v, want one plain submit", items)
	}
}

// noMetaProvider is a Fake that keeps no session metadata: every GetMeta
// answers runtime.ErrMetaUnsupported.
type noMetaProvider struct{ *runtime.Fake }

func (noMetaProvider) GetMeta(string, string) (string, error) {
	return "", runtime.ErrMetaUnsupported
}

// Kills a turn bound to the start's token on a runtime that keeps no session
// metadata (an exec pack whose get-meta answers exit 2): no claim can ever
// read its launch, so it held the turn, and every send behind it, until the
// turn expired. Such a turn is queued as a plain submit.
func TestStartupTurnLaunchIDWithoutMetadataIsUnbound(t *testing.T) {
	sp := noMetaProvider{runtime.NewFake()}
	if got := StartupTurnLaunchID(sp, "s-1", "tok-start"); got != "" {
		t.Fatalf("on a runtime without metadata = %q, want no launch: the turn is a plain submit", got)
	}
}

// Kills a one-shot initial message carried inside a turn a claim may drop,
// or split from text it was not composed into.
func TestStartupHandbackItemsSplitTheInitialMessage(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	base := nudgequeue.Item{ID: "t", SessionID: "s-1", ContinuationEpoch: "2", Source: nudgequeue.SourceSubmit, CreatedAt: now, DeliverAfter: now.Add(time.Second)}
	composed := base
	composed.Message = AppendInitialMessage(handbackStartNudge, "hello")
	items := StartupHandbackItems(composed, time.Time{}, "L1", "hello", "m")
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
	notBefore := now.Add(time.Minute)
	held := StartupHandbackItems(composed, notBefore, "L1", "hello", "m")
	if len(held) != 2 || !held[0].DeliverAfter.Equal(notBefore) || !held[1].DeliverAfter.Equal(notBefore) {
		t.Fatalf("items held until %s = %+v, want the turn and the message both deliverable no earlier", notBefore, held)
	}
	if early := StartupHandbackItems(composed, now, "L1", "hello", "m"); !early[0].DeliverAfter.Equal(base.DeliverAfter) {
		t.Fatalf("a NotBefore before the item's deliver-after moved it to %s, want %s", early[0].DeliverAfter, base.DeliverAfter)
	}
	alone := base
	alone.Message = AppendInitialMessage("", "hello")
	if items := StartupHandbackItems(alone, time.Time{}, "L1", "hello", "m"); len(items) != 1 || items[0].Reference != nil || items[0].Message != "hello" {
		t.Fatalf("a message with no turn = %+v, want the message alone", items)
	}
	plain := base
	plain.Message = handbackStartNudge + "\n\nUser message:\nhello"
	if items := StartupHandbackItems(plain, time.Time{}, "L1", "hello", "m"); len(items) != 1 || items[0].Message != plain.Message {
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

// blindAfterStartProvider is a handback leaf whose reads lose sight of a
// runtime its Start brought up, as the tmux state cache does once it passes
// its TTL: after the first Start, IsRunning reports nothing running and a
// fresh liveness read cannot answer. The runtime stays up.
type blindAfterStartProvider struct {
	*handbackStartProvider
	blind bool
}

func (p *blindAfterStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.handbackStartProvider.Start(ctx, name, cfg); err != nil {
		return err
	}
	p.blind = true
	return nil
}

func (p *blindAfterStartProvider) IsRunning(name string) bool {
	return !p.blind && p.Fake.IsRunning(name)
}

func (p *blindAfterStartProvider) ObserveLivenessSince(name string, processNames []string, _ time.Time) (runtime.Liveness, error) {
	if p.blind {
		return runtime.Liveness{}, runtime.ErrRuntimeUnavailable
	}
	return runtime.ObserveLivenessWithError(p.Fake, name, processNames)
}

// Kills a start that drops the first turn of a launch that may still stand
// when it returns an error after its Start succeeded: the turn is queued,
// bound to that launch, and a claim drops it only once another launch runs.
func TestAStartThatFailsAfterItsLaunchQueuesTheTurn(t *testing.T) {
	t.Run("stale-key recovery that cannot read the runtime", func(t *testing.T) {
		stubSubmitPoller(t)
		sp := &blindAfterStartProvider{handbackStartProvider: &handbackStartProvider{Fake: runtime.NewFake()}}
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
		hints := runtime.Config{WorkDir: t.TempDir()}
		info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Command: "claude", WorkDir: hints.WorkDir, Provider: "claude", Resume: ProviderResume{ResumeFlag: "--resume", SessionIDFlag: "--session-id"}, Hints: hints, ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		for _, it := range queuedHandbacks(t, mgr, info.ID) {
			t.Fatalf("queued %+v before the resume", it)
		}
		sp.blind, sp.nudge = false, handbackStartNudge
		err = mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, "claude --resume "+info.SessionKey, hints)
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Start = %v; want the recovery's unanswerable read", err)
		}
		if !sp.Fake.IsRunning(info.SessionName) {
			t.Fatal("the resume's runtime is gone; want it still up")
		}
		wantOneStartupTurn(t, mgr, info.ID, liveToken(t, sp.Fake, info.SessionName))
	})
	t.Run("runtime-only stale-key recovery that cannot read the runtime", func(t *testing.T) {
		stubSubmitPoller(t)
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store, "session_key", "key-1", "resume_flag", "--resume")
		sp := &blindAfterStartProvider{handbackStartProvider: &handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge}}
		mgr := newTestManager(t, store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
		err := startRuntimeOnly(t, mgr, b.ID, "claude --resume key-1", runtime.Config{})
		// The recovery's liveness read is unanswerable: it stops there rather than
		// relaunch over a runtime that may be up, and the turn stays queued for it.
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("StartRuntimeOnly = %v; want the recovery's unanswerable read", err)
		}
		wantOneStartupTurn(t, mgr, b.ID, liveToken(t, sp.Fake, "s-mint"))
	})
}

// remintingHandbackProvider is a handback leaf under which another start
// re-mints the created row right after the Start: the create's confirm finds
// the row no longer records its incarnation.
type remintingHandbackProvider struct {
	*handbackStartProvider
	store beads.Store
}

func (p *remintingHandbackProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.handbackStartProvider.Start(ctx, name, cfg); err != nil {
		return err
	}
	return p.store.SetMetadataBatch(cfg.Env["GC_SESSION_ID"], map[string]string{"instance_token": "tok-newer"})
}

// Kills a create that drops the first turn of a runtime it leaves running
// for the row's newer owner: a confirm refused as superseded queues the
// turn, bound to the launch that stands.
func TestACreateWhoseRowWasReMintedQueuesTheTurn(t *testing.T) {
	stubSubmitPoller(t)
	store := openLeaseStore(t, t.TempDir())
	sp := &remintingHandbackProvider{handbackStartProvider: &handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge}, store: store}
	mgr := newTestManager(t, store, sp)
	_, err := mgr.CreateSession(context.Background(), CreateOptions{ExplicitName: "named-worker", Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude"})
	if !errors.Is(err, ErrMintSuperseded) {
		t.Fatalf("CreateSession over a re-minted row = %v, want ErrMintSuperseded", err)
	}
	all, err := store.List(beads.ListQuery{Type: BeadType})
	if err != nil || len(all) != 1 {
		t.Fatalf("rows %v (%v), want the created row", all, err)
	}
	wantOneStartupTurn(t, mgr, all[0].ID, liveToken(t, sp.Fake, "named-worker"))
}

// confirmFailingStore fails every metadata batch write once armed: a store
// (with no conditional writes) that loses the create's confirm with a plain
// error.
type confirmFailingStore struct {
	*beads.MemStore
	armed bool
}

var errConfirmStoreDown = errors.New("database is locked")

func (s *confirmFailingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if s.armed {
		return errConfirmStoreDown
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

// armingHandbackProvider is a handback leaf whose Start, once it succeeded,
// arms store: the create's confirm then fails with a plain store error.
type armingHandbackProvider struct {
	*handbackStartProvider
	store *confirmFailingStore
}

func (p *armingHandbackProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.handbackStartProvider.Start(ctx, name, cfg); err != nil {
		return err
	}
	p.store.armed = true
	return nil
}

// Kills a create that queues the first turn of a launch it then takes down:
// a confirm that fails with a plain store error (not a re-mint) stops the
// runtime and rolls the row back, so no turn is queued for it.
func TestACreateWhoseConfirmFailsQueuesNoTurn(t *testing.T) {
	stubSubmitPoller(t)
	store := &confirmFailingStore{MemStore: beads.NewMemStore()}
	sp := &armingHandbackProvider{handbackStartProvider: &handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge}, store: store}
	mgr := newTestManager(t, store, sp)
	_, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if !errors.Is(err, errConfirmStoreDown) || errors.Is(err, ErrMintSuperseded) {
		t.Fatalf("CreateSession with a failing confirm = %v; want the store's error, not a re-mint", err)
	}
	all, err := store.List(beads.ListQuery{Type: BeadType, IncludeClosed: true})
	if err != nil || len(all) != 1 {
		t.Fatalf("rows %v (%v); want the created row", all, err)
	}
	if all[0].Status != "closed" {
		t.Fatalf("row %s is %q; want it rolled back (closed)", all[0].ID, all[0].Status)
	}
	if name := all[0].Metadata["session_name"]; name == "" || sp.IsRunning(name) {
		t.Fatalf("runtime %q still running; want the create to stop it", name)
	}
	for _, it := range queuedHandbacks(t, mgr, all[0].ID) {
		t.Fatalf("queued %+v; want no turn for a launch the create took down", it)
	}
}

// typedLive returns the texts calls typed into a session.
func typedLive(calls []runtime.Call) []string {
	var typed []string
	for _, call := range calls {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			typed = append(typed, call.Message)
		}
	}
	return typed
}

// Kills a direct send to a running session that types ahead of a first turn
// an earlier start queued (every un-welded exec launch hands its first turn
// back, so the window lasts until the queue delivers it): a Send and a
// wait-idle nudge to a session whose startup turn is still queued type
// nothing and leave their text behind it; a turn that expired or belongs to
// another session holds nothing; once the turn is gone, the send types live.
func TestSendToARunningSessionWaitsBehindItsQueuedStartupTurn(t *testing.T) {
	stubSubmitPoller(t)
	sp := &handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge}
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if items := queuedHandbacks(t, mgr, info.ID); len(items) != 1 || !nudgequeue.IsStartupTurn(items[0]) {
		t.Fatalf("queued %+v; want the create's startup turn", items)
	}
	city, err := mgr.CityDir()
	if err != nil {
		t.Fatal(err)
	}
	hints := runtime.Config{WorkDir: info.WorkDir}
	send := func(message string) (bool, []string) {
		t.Helper()
		before := len(sp.SnapshotCalls())
		outcome, err := mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, message, BuildResumeCommand(info), hints)
		if err != nil {
			t.Fatalf("Send(%q): %v", message, err)
		}
		return outcome.Queued, typedLive(sp.SnapshotCalls()[before:])
	}
	if queued, typed := send("hello"); !queued || len(typed) != 0 {
		t.Fatalf("Send behind a queued startup turn = queued %v, typed %q; want it queued, nothing typed", queued, typed)
	}
	b, sessName, err := mgr.sessionBead(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := len(sp.SnapshotCalls())
	delivered, err := mgr.tryWaitIdleNudgeLocked(context.Background(), testActor(mgr, ActorOperator), info.ID, b, deferredNudgeSource, sessName, "reminder", BuildResumeCommand(info), hints)
	if err != nil || delivered {
		t.Fatalf("wait-idle nudge behind a queued startup turn = delivered %v, %v; want undelivered, for its caller to queue", delivered, err)
	}
	if typed := typedLive(sp.SnapshotCalls()[before:]); len(typed) != 0 {
		t.Fatalf("wait-idle nudge typed %q ahead of the startup turn", typed)
	}
	if items := queuedHandbacks(t, mgr, info.ID); len(items) != 2 || !nudgequeue.IsStartupTurn(items[0]) || items[1].Message != "hello" {
		t.Fatalf("queued %+v; want the startup turn, then the message", items)
	}

	setTurns := func(edit func(turn *nudgequeue.Item)) {
		t.Helper()
		if err := nudgequeue.WithState(city.Path(), func(state *nudgequeue.State) error {
			var kept []nudgequeue.Item
			for _, item := range state.Pending {
				if nudgequeue.IsStartupTurn(item) {
					edit(&item)
				}
				if item.ID != "" {
					kept = append(kept, item)
				}
			}
			state.Pending = kept
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setTurns(func(turn *nudgequeue.Item) { turn.SessionID = "another-session" })
	if queued, typed := send("to me"); queued || len(typed) != 1 {
		t.Fatalf("Send beside another session's turn = queued %v, typed %q; want it typed live", queued, typed)
	}
	setTurns(func(turn *nudgequeue.Item) {
		turn.SessionID = info.ID
		turn.ExpiresAt = time.Now().Add(-time.Minute)
	})
	if queued, typed := send("expired"); queued || len(typed) != 1 {
		t.Fatalf("Send behind an expired turn = queued %v, typed %q; want it typed live", queued, typed)
	}
	setTurns(func(turn *nudgequeue.Item) { *turn = nudgequeue.Item{} })
	if queued, typed := send("after"); queued || len(typed) != 1 || typed[0] != "after" {
		t.Fatalf("Send once the turn is gone = queued %v, typed %q; want it typed live", queued, typed)
	}
}

// claimedStartupTurnBatch is the delivery key moveStartupTurns pins the
// turns it claims to.
const claimedStartupTurnBatch = "nudge-batch-startup-turn"

// moveStartupTurns moves the startup turns queued under cityPath into the
// in-flight bucket, pinned to claimedStartupTurnBatch, as the queue's claim
// of them does.
func moveStartupTurns(t *testing.T, cityPath string) {
	t.Helper()
	if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error {
		var kept []nudgequeue.Item
		for _, item := range state.Pending {
			if nudgequeue.IsStartupTurn(item) {
				item.DeliveryBatch = claimedStartupTurnBatch
				item.ClaimedAt = time.Now().UTC()
				item.LeaseUntil = time.Now().Add(time.Minute)
				state.InFlight = append(state.InFlight, item)
				continue
			}
			kept = append(kept, item)
		}
		state.Pending = kept
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// idleHandbackProvider is a handbackStartProvider whose session always
// reads at its idle boundary (runtime.IdleSnapshotProvider).
type idleHandbackProvider struct{ *handbackStartProvider }

func (idleHandbackProvider) SnapshotIdle(string) (bool, error) { return true, nil }

// Kills a live-only delivery (the conversation lane's message op, a mail
// notice, a sling nudge) that types into a running session ahead of the
// startup turn the queue still holds for it: SendLiveOnly and a wait-idle
// TryWaitIdleNudgeLiveOnly report it undelivered and type nothing, so the
// lane waits and the mail and sling paths fall back to the queue, behind the
// turn. TestSendsWaitBehindAStartupTurnTheQueueIsDelivering covers the turn
// once the queue has claimed it.
func TestLiveOnlySendsWaitBehindAPendingStartupTurn(t *testing.T) {
	stubSubmitPoller(t)
	sp := &idleHandbackProvider{&handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge}}
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleErrors[info.SessionName] = nil // an idle claude session: the wait-idle nudge would type
	if items := queuedHandbacks(t, mgr, info.ID); len(items) != 1 || !nudgequeue.IsStartupTurn(items[0]) {
		t.Fatalf("queued %+v; want the create's startup turn", items)
	}
	wantLiveOnlyHeld(context.Background(), t, mgr, sp.Fake, info.ID, "behind a pending turn")
}

// wantLiveOnlyHeld fails unless every live-only delivery to session id made
// under ctx (SendLiveOnly, SendImmediateLiveOnly, TryWaitIdleNudgeLiveOnly)
// reports undelivered with no error and types nothing into sp; what names
// the case.
func wantLiveOnlyHeld(ctx context.Context, t *testing.T, mgr *Manager, sp *runtime.Fake, id, what string) {
	t.Helper()
	before := len(sp.SnapshotCalls())
	if sent, err := mgr.SendLiveOnly(ctx, id, "lane message", nil); err != nil || sent {
		t.Fatalf("%s: SendLiveOnly = %v, %v; want undelivered", what, sent, err)
	}
	if sent, err := mgr.SendImmediateLiveOnly(ctx, id, "lane steer", nil); err != nil || sent {
		t.Fatalf("%s: SendImmediateLiveOnly = %v, %v; want undelivered", what, sent, err)
	}
	if waited, err := mgr.TryWaitIdleNudgeLiveOnly(ctx, id, "mail", "mail notice", nil); err != nil || waited {
		t.Fatalf("%s: TryWaitIdleNudgeLiveOnly = %v, %v; want undelivered", what, waited, err)
	}
	if typed := typedLive(sp.SnapshotCalls()[before:]); len(typed) != 0 {
		t.Fatalf("%s: live-only sends typed %q ahead of the startup turn", what, typed)
	}
}

// Kills a send that types into a running session while the queue delivers
// its startup turn: once the claim moved the turn in flight, anything typed
// before the queue's own send of it lands reaches the agent first. A live-only
// delivery, unkeyed or under another delivery's key (the conversation lane's
// own), reports undelivered; a Send queues its text behind the turn and a
// wake-capable wait-idle nudge reports undelivered, for its caller to queue;
// none types. Only the queue's delivery of the turn itself, made under the
// delivery key the claim pinned it to, goes through, live-only and wait-idle
// alike.
func TestSendsWaitBehindAStartupTurnTheQueueIsDelivering(t *testing.T) {
	stubSubmitPoller(t)
	sp := &idleHandbackProvider{&handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge}}
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleErrors[info.SessionName] = nil // an idle claude session: the wait-idle nudge would type
	city, err := mgr.CityDir()
	if err != nil {
		t.Fatal(err)
	}
	moveStartupTurns(t, city.Path())

	wantLiveOnlyHeld(context.Background(), t, mgr, sp.Fake, info.ID, "unkeyed, with the turn in flight")
	wantLiveOnlyHeld(runtime.WithNudgeDeliveryKey(context.Background(), "lane:op-1"), t, mgr, sp.Fake, info.ID, "under another delivery's key, with the turn in flight")

	hints := runtime.Config{WorkDir: info.WorkDir}
	before := len(sp.SnapshotCalls())
	outcome, err := mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), hints)
	if err != nil || !outcome.Queued {
		t.Fatalf("Send with the turn in flight = %+v, %v; want it queued behind the turn", outcome, err)
	}
	b, sessName, err := mgr.sessionBead(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := mgr.tryWaitIdleNudgeLocked(context.Background(), testActor(mgr, ActorOperator), info.ID, b, deferredNudgeSource, sessName, "reminder", BuildResumeCommand(info), hints)
	if err != nil || delivered {
		t.Fatalf("wait-idle nudge with the turn in flight = delivered %v, %v; want undelivered, for its caller to queue", delivered, err)
	}
	if typed := typedLive(sp.SnapshotCalls()[before:]); len(typed) != 0 {
		t.Fatalf("Send and wait-idle nudge typed %q ahead of the turn in flight", typed)
	}
	if items := queuedHandbacks(t, mgr, info.ID); len(items) != 1 || items[0].Message != "hello" {
		t.Fatalf("queued %+v; want the message behind the turn in flight", items)
	}

	own := runtime.WithNudgeDeliveryKey(context.Background(), claimedStartupTurnBatch)
	before = len(sp.SnapshotCalls())
	if sent, err := mgr.SendLiveOnly(own, info.ID, handbackStartNudge, nil); err != nil || !sent {
		t.Fatalf("the queue's delivery of the turn, SendLiveOnly under its key = %v, %v; want delivered", sent, err)
	}
	if waited, err := mgr.TryWaitIdleNudgeLiveOnly(own, info.ID, "queue", handbackStartNudge, nil); err != nil || !waited {
		t.Fatalf("the queue's delivery of the turn, TryWaitIdleNudgeLiveOnly under its key = %v, %v; want delivered", waited, err)
	}
	if typed := typedLive(sp.SnapshotCalls()[before:]); len(typed) != 2 {
		t.Fatalf("the queue's delivery of the turn typed %q; want both sends", typed)
	}
}

// snapshotOnlyQueue is a deferred-submit queue whose listing must not be
// used for the startup-turn barrier: its ListForAgent runs the city queue's
// maintenance (and here fails), while Snapshot is a pure read that fails
// with snapshotErr when set.
type snapshotOnlyQueue struct {
	state       nudgequeue.State
	snapshotErr error
	enqueued    []nudgequeue.Item
	listed      int
}

func (q *snapshotOnlyQueue) EnqueueDeferred(item nudgequeue.Item) error {
	q.enqueued = append(q.enqueued, item)
	return nil
}

func (q *snapshotOnlyQueue) ListForAgent(string, time.Time) (pending, inFlight, dead []nudgequeue.Item, err error) {
	q.listed++
	return nil, nil, nil, errors.New("maintenance lock busy")
}

func (q *snapshotOnlyQueue) Snapshot() (nudgequeue.State, error) { return q.state, q.snapshotErr }

// Kills a startup-turn barrier read that takes the queue's maintaining
// listing on every send (the city's file queue runs a maintenance pass, a
// flock and a state.json write under it, so a busy lock turned a
// deliverable send into a queued one): the barrier reads the queue's
// snapshot, never ListForAgent, and finds the turn there under the
// session's alias, and under an alias it had (the claim reads those too),
// as under its id.
func TestStartupTurnBarrierReadsTheQueueSnapshot(t *testing.T) {
	stubSubmitPoller(t)
	q := &snapshotOnlyQueue{}
	sp := runtime.NewFake()
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithDeferredSubmitQueue(q))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual", "alias": "helper-1", "alias_history": "helper-0"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	hints := runtime.Config{WorkDir: info.WorkDir}
	send := func(message string) (bool, []string) {
		t.Helper()
		before := len(sp.SnapshotCalls())
		outcome, err := mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, message, BuildResumeCommand(info), hints)
		if err != nil {
			t.Fatalf("Send(%q): %v", message, err)
		}
		return outcome.Queued, typedLive(sp.SnapshotCalls()[before:])
	}
	if queued, typed := send("no turn"); queued || len(typed) != 1 {
		t.Fatalf("Send with no turn queued = queued %v, typed %q; want it typed live: the barrier must not read the failing listing", queued, typed)
	}
	turn := nudgequeue.Item{ID: "turn", SessionID: info.ID, Message: handbackStartNudge, Source: nudgequeue.SourceSubmit, Reference: nudgequeue.StartupTurnReference("L1")}
	for _, key := range []string{"helper-1", "helper-0"} {
		turn.Agent = key
		q.state.Pending = []nudgequeue.Item{turn}
		if queued, typed := send("behind"); !queued || len(typed) != 0 {
			t.Fatalf("Send behind a turn queued under %q = queued %v, typed %q; want it queued", key, queued, typed)
		}
	}
	if q.listed != 0 {
		t.Fatalf("the barrier called ListForAgent %d time(s); want the snapshot alone", q.listed)
	}
}

// Kills a startup-turn barrier that fails open when it cannot read the
// queue: a send typed then can reach the agent ahead of a first turn the
// queue holds. With a queue whose Snapshot fails, SendLiveOnly,
// SendImmediateLiveOnly and TryWaitIdleNudgeLiveOnly report undelivered and
// Send queues its text; nothing is typed.
func TestStartupTurnBarrierHoldsSendsWhenTheQueueCannotBeRead(t *testing.T) {
	stubSubmitPoller(t)
	q := &snapshotOnlyQueue{}
	sp := &idleHandbackProvider{&handbackStartProvider{Fake: runtime.NewFake()}}
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithDeferredSubmitQueue(q))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.WaitForIdleErrors[info.SessionName] = nil // an idle claude session: the wait-idle nudge would type
	q.snapshotErr = errors.New("reading state.json: input/output error")
	wantLiveOnlyHeld(context.Background(), t, mgr, sp.Fake, info.ID, "with the queue unreadable")
	before := len(sp.SnapshotCalls())
	outcome, err := mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	if err != nil || !outcome.Queued {
		t.Fatalf("Send with the queue unreadable = %+v, %v; want it queued", outcome, err)
	}
	if typed := typedLive(sp.SnapshotCalls()[before:]); len(typed) != 0 {
		t.Fatalf("Send typed %q with the queue unreadable", typed)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].Message != "hello" {
		t.Fatalf("enqueued %+v; want the message queued", q.enqueued)
	}
}

// getFailingStore is a store whose Get fails with err.
type getFailingStore struct {
	beads.Store
	err error
}

func (s getFailingStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

// Kills a barrier for sends that type into a session's runtime without the
// Manager (worker.RuntimeHandle: the queue's deliveries, the conversation
// lane, mail and sling notices to a running session) that differs from the
// Manager's own: StartupTurnQueued holds a send behind the session's pending
// or claimed startup turn and passes the queue's delivery of that turn, under
// the key the claim pinned it to; a row that does not exist holds nothing,
// and one that cannot be read holds the send.
func TestStartupTurnQueuedIsTheBarrierForRuntimeOnlySends(t *testing.T) {
	stubSubmitPoller(t)
	sp := &handbackStartProvider{Fake: runtime.NewFake(), nudge: handbackStartNudge}
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ctx := context.Background()
	if !mgr.StartupTurnQueued(ctx, info.ID) {
		t.Fatal("StartupTurnQueued with the turn pending = false, want true")
	}
	city, err := mgr.CityDir()
	if err != nil {
		t.Fatal(err)
	}
	moveStartupTurns(t, city.Path())
	if !mgr.StartupTurnQueued(ctx, info.ID) {
		t.Fatal("StartupTurnQueued with the turn in flight, unkeyed = false, want true")
	}
	if mgr.StartupTurnQueued(runtime.WithNudgeDeliveryKey(ctx, claimedStartupTurnBatch), info.ID) {
		t.Fatal("StartupTurnQueued for the queue's delivery of the turn = true, want false")
	}
	if mgr.StartupTurnQueued(ctx, "gc-no-such-session") {
		t.Fatal("StartupTurnQueued for a session with no row = true, want false: no queue item is fenced to it")
	}
	unreadable := newTestManager(t, getFailingStore{Store: beads.NewMemStore(), err: errors.New("store unavailable")}, sp)
	if !unreadable.StartupTurnQueued(ctx, info.ID) {
		t.Fatal("StartupTurnQueued with the row unreadable = false, want true: an unknown row holds the send")
	}
}
