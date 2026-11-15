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

// partialHistoryBackend is a FullFake whose TurnHistory fails with a partial
// history beside the error, as a misbehaving provider might.
type partialHistoryBackend struct{ runtime.FullFake }

func (partialHistoryBackend) TurnHistory(context.Context, string) (runtime.TurnHistory, error) {
	return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "partial"}, ObservedAt: time.Now()}, runtime.ErrRuntimeUnavailable
}

func newTurnObservationSession(t *testing.T, sp runtime.Provider) (*Manager, Info) {
	t.Helper()
	return newTurnObservationSessionOf(t, sp, "agent")
}

// newTurnObservationSessionOf creates a session of provider on sp.
func newTurnObservationSessionOf(t *testing.T, sp runtime.Provider, provider string) (*Manager, Info) {
	t.Helper()
	mgr := newTestManager(t, beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Command: provider, WorkDir: t.TempDir(), Provider: provider,
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return mgr, info
}

func TestRuntimeTurnTraitsPassTheRoutedRuntimeThrough(t *testing.T) {
	recorded := runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded, Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact, Lossless: true}
	none := (runtime.TurnCapabilities{}).Normalized()
	t.Run("observing runtime", func(t *testing.T) {
		fake := runtime.NewFake()
		fake.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		mgr, info := newTurnObservationSession(t, runtime.FullFake{Fake: fake})
		fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{info.SessionName: {recorded}}
		got := mgr.RuntimeTurnTraits(info)
		want := RuntimeTurnTraits{Capabilities: recorded, Interrupt: runtime.TurnInterruptProtocol, IdleSnapshots: true, Replace: TurnReplacementInterrupt}
		if got != want {
			t.Fatalf("RuntimeTurnTraits = %+v, want %+v", got, want)
		}
	})
	interrupt := TurnReplacementInterrupt
	cases := []struct {
		name string
		sp   runtime.Provider
		want RuntimeTurnTraits
	}{
		{"runtime without turn observation", runtime.NewFake(), RuntimeTurnTraits{Capabilities: none, Interrupt: runtime.TurnInterruptKeystroke, Replace: interrupt}},
		{"local backend with snapshots", runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), RuntimeTurnTraits{Capabilities: none, Interrupt: runtime.TurnInterruptKeystroke, IdleSnapshots: true, LocalTranscripts: true, Replace: interrupt}},
		{"local backend without snapshots", runtime.NewFakeProfile(runtime.ProfileSubprocess, runtime.FullFake{Fake: runtime.NewFake()}), RuntimeTurnTraits{Capabilities: none, Interrupt: runtime.TurnInterruptKeystroke, LocalTranscripts: true, Replace: interrupt}},
		{"remote backend", runtime.NewFakeProfile(runtime.ProfileK8s, runtime.FullFake{Fake: runtime.NewFake()}), RuntimeTurnTraits{Capabilities: none, Interrupt: runtime.TurnInterruptKeystroke, Replace: interrupt}},
		{"composite routed to a local backend", auto.New(runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), runtime.NewFake()), RuntimeTurnTraits{Capabilities: none, Interrupt: runtime.TurnInterruptKeystroke, IdleSnapshots: true, LocalTranscripts: true, Replace: interrupt}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, info := newTurnObservationSession(t, tc.sp)
			if got := mgr.RuntimeTurnTraits(info); got != tc.want {
				t.Fatalf("RuntimeTurnTraits = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRuntimeTurnHistory(t *testing.T) {
	t.Run("observing runtime", func(t *testing.T) {
		fake := runtime.NewFake()
		mgr, info := newTurnObservationSession(t, runtime.FullFake{Fake: fake})
		at := time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)
		want := runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}, ObservedAt: at}
		fake.TurnHistoryScripts = map[string][]runtime.TurnHistory{info.SessionName: {want}}
		got, err := mgr.RuntimeTurnHistory(context.Background(), info)
		if err != nil || got.Current != want.Current || !got.ObservedAt.Equal(at) {
			t.Fatalf("RuntimeTurnHistory = %+v, %v; want %+v", got, err, want)
		}
	})
	t.Run("failed read is the runtime's error, with no history", func(t *testing.T) {
		mgr, info := newTurnObservationSession(t, partialHistoryBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}})
		got, err := mgr.RuntimeTurnHistory(context.Background(), info)
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("RuntimeTurnHistory error = %v, want ErrRuntimeUnavailable", err)
		}
		if got.Current.Active() || len(got.Recent) != 0 || !got.ObservedAt.IsZero() {
			t.Fatalf("RuntimeTurnHistory = %+v beside its error, want the zero history", got)
		}
	})
	t.Run("runtime without turn observation", func(t *testing.T) {
		mgr, info := newTurnObservationSession(t, runtime.NewFake())
		if _, err := mgr.RuntimeTurnHistory(context.Background(), info); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
			t.Fatalf("RuntimeTurnHistory error = %v, want ErrTurnIdentityUnsupported", err)
		}
	})
	t.Run("composite backend without turn observation", func(t *testing.T) {
		sp := auto.New(runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), runtime.NewFake())
		mgr, info := newTurnObservationSession(t, sp)
		_, err := mgr.RuntimeTurnHistory(context.Background(), info)
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
			idle, supported, err := mgr.RuntimeIdleSnapshot(info)
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

// Kills a keyed-lookup trait that disagrees with ResolveKeyedTranscriptPath:
// Gemini has no exact by-key lookup, Codex, Claude and OpenCode do, and the
// trait does not depend on whether the session has its key yet.
func TestHasKeyedTranscriptLookup(t *testing.T) {
	for provider, want := range map[string]bool{"claude": true, "codex": true, "gemini": false, "opencode": true} {
		for _, key := range []string{"", "k"} {
			info := Info{ID: "s", Provider: provider, SessionKey: key}
			if got := HasKeyedTranscriptLookup(info, ""); got != want {
				t.Errorf("HasKeyedTranscriptLookup(%s, key %q) = %v, want %v", provider, key, got, want)
			}
		}
	}
	if !HasKeyedTranscriptLookup(Info{ID: "s"}, "claude") {
		t.Error("HasKeyedTranscriptLookup ignores the fallback provider of a row with none")
	}
}
