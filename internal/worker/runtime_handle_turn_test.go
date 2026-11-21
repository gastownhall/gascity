package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func newTurnRuntimeHandle(t *testing.T, sp runtime.Provider) *RuntimeHandle {
	t.Helper()
	if err := sp.Start(context.Background(), "turn-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "turn-worker", ProviderName: "claude"})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	return h
}

func TestRuntimeHandleTurnsComeFromItsProvider(t *testing.T) {
	fake := runtime.NewFake()
	h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: fake}))
	at := time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)
	want := runtime.TurnHistory{Current: runtime.TurnState{TurnID: "acp-1"}, CurrentStart: at, ObservedAt: at}
	fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"turn-worker": {fullTurnCaps}}
	fake.TurnHistoryScripts = map[string][]runtime.TurnHistory{"turn-worker": {want}}

	caps, err := h.TurnCapabilities(context.Background())
	if err != nil {
		t.Fatalf("TurnCapabilities: %v", err)
	}
	if got := capsRow(caps); got != "protocol recorded/recorded/atomic/exact/true keystroke none" {
		t.Fatalf("TurnCapabilities = %q, want the provider's, as a protocol source, replacing no turn", got)
	}
	got, err := h.CurrentTurn(context.Background())
	if err != nil {
		t.Fatalf("CurrentTurn: %v", err)
	}
	assertTurnHistory(t, got, want)

	fake.TurnHistoryErrors = map[string][]error{"turn-worker": {runtime.ErrRuntimeUnavailable}}
	if _, err := h.CurrentTurn(context.Background()); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("CurrentTurn on a failed read = %v, want ErrRuntimeUnavailable", err)
	}
}

// A runtime handle has no transcript: tmux, which is a transcript source for a
// session handle, is none here.
func TestRuntimeHandleTmuxReportsNoTurnSource(t *testing.T) {
	h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}))
	caps, err := h.TurnCapabilities(context.Background())
	if err != nil {
		t.Fatalf("TurnCapabilities: %v", err)
	}
	if got := capsRow(caps); got != "none none/none/none/none/false keystroke none" {
		t.Fatalf("TurnCapabilities = %q, want none", got)
	}
	if _, err := h.CurrentTurn(context.Background()); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
		t.Fatalf("CurrentTurn = %v, want ErrTurnIdentityUnsupported", err)
	}
	if _, err := h.WatchTurns(context.Background()); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
		t.Fatalf("WatchTurns = %v, want ErrTurnIdentityUnsupported", err)
	}
}

func TestRuntimeHandleT3BridgeTurnSourceIsDerived(t *testing.T) {
	fake := runtime.NewFake()
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	h := newTurnRuntimeHandle(t, sp)
	fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"turn-worker": {v1TurnCaps}}
	want := "none none/none/none/none/false protocol none"
	if _, ok := sp.(runtime.TurnObservationProvider); ok {
		want = "protocol recorded/inferred/checked/correlated/false protocol none"
	}
	caps, err := h.TurnCapabilities(context.Background())
	if err != nil || capsRow(caps) != want {
		t.Fatalf("TurnCapabilities = %q, %v; want %q", capsRow(caps), err, want)
	}
}

func TestRuntimeHandleWatchTurns(t *testing.T) {
	backend := newTurnScriptBackend(true)
	h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileACP, backend))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := h.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); first.Err != nil || first.History.Current.Active() {
		t.Fatalf("first update = %+v, want the idle history", first)
	}
	backend.set(runtime.TurnHistory{Current: runtime.TurnState{TurnID: "acp-1"}}, nil)
	backend.state.events <- runtime.TurnEvent{Kind: runtime.TurnEventStarted, Session: "turn-worker", TurnID: "acp-1"}
	if got := receiveTurnUpdate(t, ch); got.History.Current.TurnID != "acp-1" {
		t.Fatalf("update after the hint = %+v, want acp-1 running", got)
	}
	cancel()
	assertWatchCloses(t, ch)
}

func newGatedHistoryBackend() (gatedHistoryBackend, func() int) {
	calls, mu := 0, &sync.Mutex{}
	b := gatedHistoryBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, armed: &atomic.Bool{}, entered: make(chan struct{}, 4), release: make(chan struct{}), mu: mu, calls: &calls}
	return b, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// assertReadersShareOneRead runs first's read until the gated backend holds
// it, then second's beside it, and asserts second joins first's read rather
// than answering early, and that both read the shared history.
func assertReadersShareOneRead(t *testing.T, backend gatedHistoryBackend, calls func() int, first, second TurnHandle) {
	t.Helper()
	type answer struct {
		hist runtime.TurnHistory
		err  error
	}
	answers := make(chan answer, 2)
	read := func(h TurnHandle) {
		hist, err := h.CurrentTurn(context.Background())
		answers <- answer{hist, err}
	}
	backend.armed.Store(true)
	go read(first)
	<-backend.entered // the first read is in flight
	go read(second)
	select {
	case a := <-answers:
		t.Fatalf("a reader answered before the shared read returned: %+v, %v", a.hist, a.err)
	case <-time.After(200 * time.Millisecond):
	}
	close(backend.release)
	for range 2 {
		a := <-answers
		if a.err != nil || a.hist.Current.TurnID != "acp-1" {
			t.Fatalf("concurrent reader = %+v, %v; want the shared history", a.hist, a.err)
		}
	}
	if n := calls(); n != 1 {
		t.Fatalf("TurnHistory calls = %d, want the readers to share one", n)
	}
}

// Kills a RuntimeHandle read that fails a concurrent reader of its session:
// readers of one key share the outstanding read.
func TestRuntimeHandleConcurrentReadersShareOneRead(t *testing.T) {
	backend, calls := newGatedHistoryBackend()
	h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileACP, backend))
	assertReadersShareOneRead(t, backend, calls, h, h)
}

// Kills turn keys that differ between a SessionHandle and a Factory's
// RuntimeHandle of one city's session: both scope their key to the city, so
// they share one read.
func TestSessionAndRuntimeHandlesOfOneCitySessionShareOneRead(t *testing.T) {
	backend, calls := newGatedHistoryBackend()
	sp := runtime.NewFakeProfile(runtime.ProfileACP, backend)
	factory, err := NewFactory(FactoryConfig{
		Store:                   beads.NewMemStore(),
		Provider:                sp,
		CityPath:                t.TempDir(),
		StaleKeyDetectionWaiter: func(context.Context, string) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	session, err := factory.Session(SessionSpec{Template: "probe", Title: "Probe", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := session.Start(context.Background(), handleActor(t, session, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := session.manager.Get(session.currentSessionID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rh, err := factory.RuntimeHandle(info.SessionName, "claude", "", nil)
	if err != nil {
		t.Fatalf("RuntimeHandle: %v", err)
	}
	assertReadersShareOneRead(t, backend, calls, session, rh)
}

// partialHistoryBackend is a protocol backend whose TurnHistory fails with a
// partial history beside its error.
type partialHistoryBackend struct {
	runtime.FullFake
	err error
}

func (b partialHistoryBackend) TurnCapabilities(string) runtime.TurnCapabilities { return fullTurnCaps }

func (b partialHistoryBackend) TurnHistory(context.Context, string) (runtime.TurnHistory, error) {
	return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "partial"}, ObservedAt: time.Now()}, b.err
}

// Kills a RuntimeHandle read that returns the provider's partial history
// beside its error: a failed read is the zero history, never a turn.
func TestRuntimeHandleFailedReadIsTheZeroHistory(t *testing.T) {
	for _, readErr := range []error{runtime.ErrRuntimeUnavailable, runtime.ErrSessionNotFound, errors.New("provider failed")} {
		h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileACP, partialHistoryBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, err: readErr}))
		got, err := h.CurrentTurn(context.Background())
		assertNoHistory(t, got, err, readErr)
	}
}
