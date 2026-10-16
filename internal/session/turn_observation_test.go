package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
)

// idleScriptBackend is a FullFake whose SnapshotIdle answers from a script.
type idleScriptBackend struct {
	runtime.FullFake
	idle bool
	err  error
}

func (b idleScriptBackend) SnapshotIdle(string) (bool, error) { return b.idle, b.err }

func newTurnObservationSession(t *testing.T, sp runtime.Provider) (*Manager, Info) {
	t.Helper()
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Command: "agent", WorkDir: t.TempDir(), Provider: "agent",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return mgr, info
}

func TestRuntimeTurnCapabilitiesPassesTheRoutedRuntimeThrough(t *testing.T) {
	recorded := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact, Lossless: true}
	t.Run("observing runtime", func(t *testing.T) {
		fake := runtime.NewFake()
		fake.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		mgr, info := newTurnObservationSession(t, runtime.FullFake{Fake: fake})
		fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{info.SessionName: {recorded}}
		caps, mode, err := mgr.RuntimeTurnCapabilities(info.ID)
		if err != nil || caps != recorded || mode != runtime.TurnInterruptProtocol {
			t.Fatalf("RuntimeTurnCapabilities = %+v, %q, %v; want %+v, protocol", caps, mode, err, recorded)
		}
	})
	t.Run("runtime without turn observation", func(t *testing.T) {
		mgr, info := newTurnObservationSession(t, runtime.NewFake())
		caps, mode, err := mgr.RuntimeTurnCapabilities(info.ID)
		if err != nil || caps != (runtime.TurnCapabilities{}).Normalized() || mode != runtime.TurnInterruptKeystroke {
			t.Fatalf("RuntimeTurnCapabilities = %+v, %q, %v; want none, keystroke", caps, mode, err)
		}
	})
	t.Run("unknown session", func(t *testing.T) {
		mgr, _ := newTurnObservationSession(t, runtime.NewFake())
		if _, _, err := mgr.RuntimeTurnCapabilities("missing"); err == nil {
			t.Fatal("RuntimeTurnCapabilities(missing) = nil error, want the lookup error")
		}
	})
}

func TestRuntimeTurnHistory(t *testing.T) {
	t.Run("observing runtime", func(t *testing.T) {
		fake := runtime.NewFake()
		mgr, info := newTurnObservationSession(t, runtime.FullFake{Fake: fake})
		at := time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)
		want := runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}, ObservedAt: at}
		fake.TurnHistoryScripts = map[string][]runtime.TurnHistory{info.SessionName: {want}}
		got, err := mgr.RuntimeTurnHistory(context.Background(), info.ID)
		if err != nil || got.Current != want.Current || !got.ObservedAt.Equal(at) {
			t.Fatalf("RuntimeTurnHistory = %+v, %v; want %+v", got, err, want)
		}
	})
	t.Run("failed read is the runtime's error", func(t *testing.T) {
		fake := runtime.NewFake()
		mgr, info := newTurnObservationSession(t, runtime.FullFake{Fake: fake})
		fake.TurnHistoryErrors = map[string][]error{info.SessionName: {runtime.ErrRuntimeUnavailable}}
		if _, err := mgr.RuntimeTurnHistory(context.Background(), info.ID); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("RuntimeTurnHistory error = %v, want ErrRuntimeUnavailable", err)
		}
	})
	t.Run("runtime without turn observation", func(t *testing.T) {
		mgr, info := newTurnObservationSession(t, runtime.NewFake())
		if _, err := mgr.RuntimeTurnHistory(context.Background(), info.ID); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
			t.Fatalf("RuntimeTurnHistory error = %v, want ErrTurnIdentityUnsupported", err)
		}
	})
	t.Run("composite backend without turn observation", func(t *testing.T) {
		sp := auto.New(runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), runtime.NewFake())
		mgr, info := newTurnObservationSession(t, sp)
		_, err := mgr.RuntimeTurnHistory(context.Background(), info.ID)
		if !errors.Is(err, runtime.ErrTurnIdentityUnsupported) || !errors.Is(err, runtime.ErrInteractionUnsupported) {
			t.Fatalf("RuntimeTurnHistory error = %v, want ErrTurnIdentityUnsupported wrapping ErrInteractionUnsupported", err)
		}
	})
}

func TestRuntimeIdleSnapshot(t *testing.T) {
	busyErr := errors.New("pane unreadable")
	cases := []struct {
		name          string
		sp            runtime.Provider
		wantIdle      bool
		wantSupported bool
		wantErr       error
	}{
		{"idle", runtime.NewFakeProfile(runtime.ProfileTmux, idleScriptBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, idle: true}), true, true, nil},
		{"busy", runtime.NewFakeProfile(runtime.ProfileTmux, idleScriptBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}}), false, true, nil},
		{"read fails", runtime.NewFakeProfile(runtime.ProfileTmux, idleScriptBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, idle: true, err: busyErr}), false, true, busyErr},
		{"runtime without snapshots", runtime.NewFakeProfile(runtime.ProfileSubprocess, runtime.FullFake{Fake: runtime.NewFake()}), false, false, nil},
		{"composite backend without snapshots", auto.New(runtime.NewFakeProfile(runtime.ProfileSubprocess, runtime.FullFake{Fake: runtime.NewFake()}), runtime.NewFake()), false, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, info := newTurnObservationSession(t, tc.sp)
			idle, supported, err := mgr.RuntimeIdleSnapshot(info.ID)
			if idle != tc.wantIdle || supported != tc.wantSupported || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("RuntimeIdleSnapshot = %v, %v, %v; want %v, %v, %v", idle, supported, err, tc.wantIdle, tc.wantSupported, tc.wantErr)
			}
		})
	}
}

func TestSubscribeRuntimeTurnEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mgr, _ := newTurnObservationSession(t, runtime.FullFake{Fake: runtime.NewFake()})
	ch, err := mgr.SubscribeRuntimeTurnEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeRuntimeTurnEvents: %v", err)
	}
	cancel()
	for range ch { //nolint:revive // drain until the stream closes with ctx
	}

	plain, _ := newTurnObservationSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}))
	if _, err := plain.SubscribeRuntimeTurnEvents(context.Background()); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("SubscribeRuntimeTurnEvents without turn events = %v, want ErrInteractionUnsupported", err)
	}
}
