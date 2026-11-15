package session

import (
	"context"
	"errors"
	"testing"

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
