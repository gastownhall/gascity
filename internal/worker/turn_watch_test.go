package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestWatchTurnsTranscript(t *testing.T) {
	idle := newIdleScript(idleAnswer{idle: false})
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, idle), "claude", "")
	s.handle.turns.timing = clock.timing()
	path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:00:00.123Z"), claudeTurnDuration("2026-10-13T05:00:05.456Z"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}

	// Resync first: the full history, idle.
	first := receiveTurnUpdate(t, ch)
	if first.Err != nil || first.History.Current.Active() || len(first.History.Recent) != 1 || first.History.Recent[0].TurnID != claudeTurn1 {
		t.Fatalf("first update = %+v, want the history with %s ended", first, claudeTurn1)
	}
	if ask := clock.nextAsk(t); ask.d != turnPollIdle {
		t.Fatalf("idle wait = %s, want %s", ask.d, turnPollIdle)
	} else {
		// A poll with nothing new sends nothing.
		ask.fire <- clock.now()
	}
	ask := clock.nextAsk(t)
	assertNoPendingUpdate(t, ch)

	// Coalescing: two changes the reader has not taken yet arrive as the
	// latest one.
	appendTranscript(t, path, claudePrompt(claudeTurn2, "2026-10-13T05:01:00.789Z")+"\n")
	ask.fire <- clock.now()
	ask = clock.nextAsk(t)
	if ask.d != turnOverlayEvery {
		t.Fatalf("open-turn wait = %s, want the overlay's %s", ask.d, turnOverlayEvery)
	}
	appendTranscript(t, path, claudeTurnDuration("2026-10-13T05:01:03.001Z")+"\n")
	ask.fire <- clock.now()
	ask = clock.nextAsk(t)
	latest := receiveTurnUpdate(t, ch)
	if latest.Err != nil || latest.History.Current.Active() || len(latest.History.Recent) != 2 || latest.History.Recent[0].TurnID != claudeTurn2 {
		t.Fatalf("coalesced update = %+v, want %s ended", latest, claudeTurn2)
	}
	assertNoPendingUpdate(t, ch)

	// A torn tail is an error update, never an idle history.
	appendTranscript(t, path, `{"type":"user","timestamp":"2026-10-13T05:02`)
	ask.fire <- clock.now()
	ask = clock.nextAsk(t)
	torn := receiveTurnUpdate(t, ch)
	if !errors.Is(torn.Err, runtime.ErrRuntimeUnavailable) || torn.History.Current.Active() || len(torn.History.Recent) != 0 {
		t.Fatalf("torn update = %+v, want an ErrRuntimeUnavailable update with no history", torn)
	}
	if ask.d != turnPollOpen {
		t.Fatalf("wait while failing = %s, want %s", ask.d, turnPollOpen)
	}
	// Completing the record recovers with a full history.
	appendTranscript(t, path, `:00.000Z","message":{"role":"user","content":"a prompt"},"promptId":"`+claudeTurn1+`x","origin":"user","promptSource":"prompt","isSidechain":false}`+"\n")
	ask.fire <- clock.now()
	ask = clock.nextAsk(t)
	recovered := receiveTurnUpdate(t, ch)
	if recovered.Err != nil || recovered.History.Current.TurnID != claudeTurn1+"x" {
		t.Fatalf("recovered update = %+v, want the new open turn", recovered)
	}

	// Rotation: the file is replaced by a shorter one; the follower rescans.
	replacement := path + ".new"
	writeTranscriptLines(t, replacement, claudePrompt(claudeTurn2, "2026-10-13T07:00:00.000Z"), claudeTurnDuration("2026-10-13T07:00:01.000Z"))
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	ask.fire <- clock.now()
	clock.nextAsk(t)
	rotated := receiveTurnUpdate(t, ch)
	if rotated.Err != nil || rotated.History.Current.Active() || len(rotated.History.Recent) != 1 || rotated.History.Recent[0].TurnID != claudeTurn2 ||
		!rotated.History.Recent[0].StartedAt.Equal(mustTurnTime(t, "2026-10-13T07:00:00.000Z")) {
		t.Fatalf("rotated update = %+v, want only the replacement's turn", rotated)
	}

	cancel()
	assertWatchCloses(t, ch)
}

func TestWatchTurnsTranscriptOverlayEndsAQuietTurn(t *testing.T) {
	idle := newIdleScript(idleAnswer{idle: true})
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileTmux, idle), "claude", "")
	timing := clock.timing()
	s.handle.turns.timing = timing
	path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:59.000Z"))
	ageTranscript(t, path, clock.now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); first.History.Current.TurnID != claudeTurn1 {
		t.Fatalf("first update = %+v, want the open turn", first)
	}
	ask := clock.nextAsk(t)
	clock.advance(3 * time.Second)
	ask.fire <- clock.now()
	clock.nextAsk(t)
	ended := receiveTurnUpdate(t, ch)
	if ended.Err != nil || ended.History.Current.Active() || len(ended.History.Recent) != 1 || !ended.History.Recent[0].Inferred {
		t.Fatalf("update = %+v, want the quiet turn ended, inferred", ended)
	}
}

// turnScriptBackend is a FullFake whose TurnHistory and turn events the test
// drives.
type turnScriptBackend struct {
	runtime.FullFake
	state *turnScriptState
}

type turnScriptState struct {
	mu     sync.Mutex
	hist   runtime.TurnHistory
	err    error
	reads  int
	events chan runtime.TurnEvent // nil: no turn events
}

func newTurnScriptBackend(events bool) turnScriptBackend {
	b := turnScriptBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, state: &turnScriptState{}}
	if events {
		b.state.events = make(chan runtime.TurnEvent, 8)
	}
	return b
}

func (b turnScriptBackend) TurnCapabilities(string) runtime.TurnCapabilities { return fullTurnCaps }

func (b turnScriptBackend) TurnHistory(context.Context, string) (runtime.TurnHistory, error) {
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	b.state.reads++
	if b.state.err != nil {
		return runtime.TurnHistory{}, b.state.err
	}
	h := b.state.hist
	h.ObservedAt = time.Now()
	return h, nil
}

func (b turnScriptBackend) SubscribeTurnEvents(ctx context.Context) (<-chan runtime.TurnEvent, error) {
	if b.state.events == nil {
		return nil, runtime.ErrInteractionUnsupported
	}
	out := make(chan runtime.TurnEvent)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-b.state.events:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (b turnScriptBackend) set(h runtime.TurnHistory, err error) {
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	b.state.hist, b.state.err = h, err
}

func (b turnScriptBackend) readCount() int {
	b.state.mu.Lock()
	defer b.state.mu.Unlock()
	return b.state.reads
}

func TestWatchTurnsProtocolHintTriggersARead(t *testing.T) {
	backend := newTurnScriptBackend(true)
	clock := newManualTurnClock(time.Now())
	s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileACP, backend), "claude", "")
	s.handle.turns.timing = clock.timing()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); first.Err != nil || first.History.Current.Active() {
		t.Fatalf("first update = %+v, want the idle history", first)
	}
	if ask := clock.nextAsk(t); ask.d != turnPollIdle {
		t.Fatalf("idle wait with hints = %s, want %s", ask.d, turnPollIdle)
	}
	backend.set(runtime.TurnHistory{Current: runtime.TurnState{TurnID: "acp-1"}}, nil)
	backend.state.events <- runtime.TurnEvent{Kind: runtime.TurnEventStarted, Session: s.info.SessionName, TurnID: "acp-1"}
	if got := receiveTurnUpdate(t, ch); got.History.Current.TurnID != "acp-1" {
		t.Fatalf("update after the hint = %+v, want acp-1 running", got)
	}
	if ask := clock.nextAsk(t); ask.d != turnPollOpen {
		t.Fatalf("open-turn wait = %s, want %s", ask.d, turnPollOpen)
	}
	cancel()
	assertWatchCloses(t, ch)
}

func TestWatchTurnsProtocolPollAloneDeliversEveryChange(t *testing.T) {
	backend := newTurnScriptBackend(false)
	clock := newManualTurnClock(time.Now())
	s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileACP, backend), "claude", "")
	s.handle.turns.timing = clock.timing()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	receiveTurnUpdate(t, ch)
	at := time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)
	steps := []struct {
		hist runtime.TurnHistory
		err  error
	}{
		{hist: runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t1"}, CurrentStart: at}},
		{err: fmt.Errorf("owner gone: %w", runtime.ErrRuntimeUnavailable)},
		{hist: runtime.TurnHistory{Recent: []runtime.EndedTurn{{TurnID: "t1", StartedAt: at, EndedAt: at.Add(time.Second), Status: runtime.TurnStatusCompleted}}}},
		{hist: runtime.TurnHistory{Current: runtime.TurnState{TurnID: "t2"}, Recent: []runtime.EndedTurn{{TurnID: "t1", StartedAt: at, EndedAt: at.Add(time.Second), Status: runtime.TurnStatusCompleted}}}},
	}
	for i, step := range steps {
		ask := clock.nextAsk(t)
		if ask.d != turnPollOpen {
			t.Fatalf("step %d: wait without hints = %s, want %s", i, ask.d, turnPollOpen)
		}
		backend.set(step.hist, step.err)
		ask.fire <- clock.now()
		got := receiveTurnUpdate(t, ch)
		if step.err != nil {
			if !errors.Is(got.Err, runtime.ErrRuntimeUnavailable) || got.History.Current.Active() {
				t.Fatalf("step %d: update = %+v, want the error, never idle", i, got)
			}
			continue
		}
		if got.Err != nil || got.History.Current != step.hist.Current || len(got.History.Recent) != len(step.hist.Recent) {
			t.Fatalf("step %d: update = %+v, want %+v", i, got, step.hist)
		}
	}
	cancel()
	assertWatchCloses(t, ch)
}

func TestPublishTurnUpdateLatestWins(t *testing.T) {
	out := make(chan TurnUpdate, 1)
	publishTurnUpdate(out, TurnUpdate{History: runtime.TurnHistory{Current: runtime.TurnState{TurnID: "a"}}})
	publishTurnUpdate(out, TurnUpdate{History: runtime.TurnHistory{Current: runtime.TurnState{TurnID: "b"}}})
	if got := <-out; got.History.Current.TurnID != "b" {
		t.Fatalf("published = %+v, want the latest", got)
	}
	assertNoPendingUpdate(t, out)
}

// With fsnotify on, a write wakes the watcher before any poll fires.
func TestWatchTurnsTranscriptWakesOnWrite(t *testing.T) {
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "codex", "")
	timing := clock.timing()
	timing.noNotify = false
	s.handle.turns.timing = timing
	path := s.writeCompletedTurn(t, "codex")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); first.Err != nil || first.History.Current.Active() {
		t.Fatalf("first update = %+v, want the idle history", first)
	}
	clock.nextAsk(t) // the poll is never fired
	appendTranscript(t, path, codexStarted(codexTurn2, "2026-10-08T02:23:01.007Z", 1791426181)+"\n")
	if got := receiveTurnUpdate(t, ch); got.History.Current.TurnID != codexTurn2 {
		t.Fatalf("update after the write = %+v, want %s running", got, codexTurn2)
	}
	cancel()
	assertWatchCloses(t, ch)
}

// A session watched before its transcript exists is watched, not refused:
// unavailable updates until the transcript appears, then its history.
func TestWatchTurnsStartsBeforeTheTranscript(t *testing.T) {
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "codex", "")
	s.handle.turns.timing = clock.timing()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns before the transcript: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); !errors.Is(first.Err, runtime.ErrRuntimeUnavailable) || first.History.Current.Active() {
		t.Fatalf("first update = %+v, want unavailable, never idle", first)
	}
	ask := clock.nextAsk(t)
	if ask.d != turnPollOpen {
		t.Fatalf("wait while unavailable = %s, want %s", ask.d, turnPollOpen)
	}
	s.writeCompletedTurn(t, "codex")
	clock.advance(ask.d)
	ask.fire <- clock.now()
	clock.nextAsk(t)
	if got := receiveTurnUpdate(t, ch); got.Err != nil || len(got.History.Recent) != 1 || got.History.Recent[0].TurnID != codexTurn1 {
		t.Fatalf("update once written = %+v, want the transcript's history", got)
	}
	cancel()
	assertWatchCloses(t, ch)
}

// The backstop poll follows a session to its new conversation's transcript,
// and never keeps reading the old conversation's once the key moved.
func TestWatchTurnsFollowsANewConversation(t *testing.T) {
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "codex", "")
	s.handle.turns.timing = clock.timing()
	s.writeCompletedTurn(t, "codex")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); first.Err != nil || first.History.Recent[0].TurnID != codexTurn1 {
		t.Fatalf("first update = %+v, want the first conversation", first)
	}
	// A fresh restart: a new conversation key, its rollout not written yet.
	s.setKey(t, otherConversation)
	ask := clock.nextAsk(t)
	clock.advance(ask.d)
	ask.fire <- clock.now()
	if got := receiveTurnUpdate(t, ch); !errors.Is(got.Err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("update after the key moved = %+v, want unavailable, not the old conversation", got)
	}
	ask = clock.nextAsk(t)
	s.writeTranscript(t, "codex", codexStarted(codexTurn2, "2026-10-08T03:00:00.000Z", 1791429600))
	clock.advance(ask.d)
	ask.fire <- clock.now()
	clock.nextAsk(t)
	if got := receiveTurnUpdate(t, ch); got.Err != nil || got.History.Current.TurnID != codexTurn2 || len(got.History.Recent) != 0 {
		t.Fatalf("update in the new conversation = %+v, want only its open turn", got)
	}
	cancel()
	assertWatchCloses(t, ch)
}

// A transcript whose turns carry no ids watches as unsupported, never as an
// idle history.
func TestWatchTurnsTranscriptWithoutIDs(t *testing.T) {
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "claude", "")
	s.handle.turns.timing = clock.timing()
	s.writeTranscript(t, "claude", claudePromptWithoutID("2026-10-13T05:00:00.123Z"), claudeTurnDuration("2026-10-13T05:00:05.456Z"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if got := receiveTurnUpdate(t, ch); !errors.Is(got.Err, runtime.ErrTurnIdentityUnsupported) || got.History.Current.Active() || len(got.History.Recent) != 0 {
		t.Fatalf("update = %+v, want ErrTurnIdentityUnsupported with no history", got)
	}
	cancel()
	assertWatchCloses(t, ch)
}

// A conversation that opens with a turn run from queued input watches as
// identity from the first update, and the next identified prompt is sent.
func TestWatchTurnsClaudeQueuedHeadFollowsTheNextPrompt(t *testing.T) {
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "claude", "")
	s.handle.turns.timing = clock.timing()
	path := s.writeTranscript(t, "claude",
		claudeQueueOp("enqueue", "2026-10-13T05:00:00.010Z"),
		claudeQueueOp("dequeue", "2026-10-13T05:00:00.020Z"),
		claudeReply("2026-10-13T05:00:01.000Z"),
		claudeTurnDuration("2026-10-13T05:00:02.000Z"),
	)
	ageTranscript(t, path, clock.now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.handle.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); first.Err != nil || first.History.Current.Active() || len(first.History.Recent) != 0 {
		t.Fatalf("first update = %+v, want the idle history", first)
	}
	ask := clock.nextAsk(t)
	appendTranscript(t, path, claudePrompt(claudeTurn1, "2026-10-13T05:00:03.123Z")+"\n")
	ageTranscript(t, path, clock.now())
	ask.fire <- clock.now()
	if got := receiveTurnUpdate(t, ch); got.Err != nil || got.History.Current.TurnID != claudeTurn1 {
		t.Fatalf("update after the prompt = %+v, want %s running", got, claudeTurn1)
	}
	cancel()
	assertWatchCloses(t, ch)
}

// A failure streak whose error changes class publishes the new class: a
// consumer that downgrades on unsupported, or drops a session that is gone,
// hears of it though the streak never succeeded.
func TestWatchTurnsPublishesAChangeOfErrorClass(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, s turnSession)
		want   error
	}{
		{
			name: "unavailable, then unsupported",
			change: func(t *testing.T, s turnSession) {
				s.writeTranscript(t, "claude", claudePromptWithoutID("2026-10-13T05:00:00.123Z"), claudeTurnDuration("2026-10-13T05:00:05.456Z"))
			},
			want: runtime.ErrTurnIdentityUnsupported,
		},
		{
			name: "unavailable, then not found",
			change: func(t *testing.T, s turnSession) {
				if err := s.store.Close(s.info.ID); err != nil {
					t.Fatalf("Close: %v", err)
				}
			},
			want: runtime.ErrSessionNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
			s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "claude", "")
			s.handle.turns.timing = clock.timing()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := s.handle.WatchTurns(ctx)
			if err != nil {
				t.Fatalf("WatchTurns: %v", err)
			}
			if first := receiveTurnUpdate(t, ch); !errors.Is(first.Err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("first update = %+v, want unavailable", first)
			}
			// A second unavailable read publishes nothing.
			ask := clock.nextAsk(t)
			clock.advance(ask.d)
			ask.fire <- clock.now()
			ask = clock.nextAsk(t)
			assertNoPendingUpdate(t, ch)

			tc.change(t, s)
			clock.advance(ask.d)
			ask.fire <- clock.now()
			clock.nextAsk(t)
			if got := receiveTurnUpdate(t, ch); !errors.Is(got.Err, tc.want) || errors.Is(got.Err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("update after the change = %+v, want %v", got, tc.want)
			}
			cancel()
			assertWatchCloses(t, ch)
		})
	}
}

// The protocol twin: closing a session's row leaves its runtime session
// answering, yet the watch reports the session not found, whether it was
// healthy or in an unavailable streak, and reads no history for it.
func TestWatchTurnsProtocolPublishesAClosedSession(t *testing.T) {
	cases := []struct {
		name  string
		start error // the backend's read error before the close
		want  error // the first update's error
	}{
		{name: "healthy, then closed"},
		{name: "unavailable, then closed", start: fmt.Errorf("owner gone: %w", runtime.ErrRuntimeUnavailable), want: runtime.ErrRuntimeUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := newTurnScriptBackend(false)
			backend.set(runtime.TurnHistory{}, tc.start)
			clock := newManualTurnClock(time.Now())
			s := newTurnSession(t, runtime.NewFakeProfile(runtime.ProfileACP, backend), "claude", "")
			s.handle.turns.timing = clock.timing()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := s.handle.WatchTurns(ctx)
			if err != nil {
				t.Fatalf("WatchTurns: %v", err)
			}
			if first := receiveTurnUpdate(t, ch); !errors.Is(first.Err, tc.want) {
				t.Fatalf("first update = %+v, want error %v", first, tc.want)
			}
			// A second read like the first publishes nothing.
			ask := clock.nextAsk(t)
			ask.fire <- clock.now()
			ask = clock.nextAsk(t)
			assertNoPendingUpdate(t, ch)

			if err := s.store.Close(s.info.ID); err != nil {
				t.Fatalf("Close: %v", err)
			}
			reads := backend.readCount()
			ask.fire <- clock.now()
			clock.nextAsk(t)
			if got := receiveTurnUpdate(t, ch); !errors.Is(got.Err, runtime.ErrSessionNotFound) || errors.Is(got.Err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("update after the close = %+v, want ErrSessionNotFound", got)
			}
			if got := backend.readCount(); got != reads {
				t.Fatalf("history reads after the close = %d, want %d: a closed session's runtime is not read", got, reads)
			}
			cancel()
			assertWatchCloses(t, ch)
		})
	}
}

// The transcript twin: a session whose row closes while its transcript is
// still written is not found from the next read on, whatever woke the read: a
// write, or a poll too soon after the last resolution to re-resolve the
// transcript (the idle overlay's). The transcript is not read on.
func TestWatchTurnsTranscriptPublishesAClosedSession(t *testing.T) {
	cases := []struct {
		name   string
		family string
		notify bool     // a write wakes the watcher; else the test fires its poll
		before []string // the transcript before the close
		after  string   // the record written after the close
	}{
		{
			name: "woken by a write", family: "codex", notify: true,
			before: []string{codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), codexComplete(codexTurn1, "2026-10-08T02:22:30.500Z")},
			after:  codexStarted(codexTurn2, "2026-10-08T02:23:01.007Z", 1791426181),
		},
		{
			name: "woken by the overlay's poll", family: "claude",
			before: []string{claudePrompt(claudeTurn1, "2026-10-13T05:00:00.123Z")},
			after:  claudeTurnDuration("2026-10-13T05:00:05.456Z"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
			s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), tc.family, "")
			timing := clock.timing()
			timing.noNotify = !tc.notify
			s.handle.turns.timing = timing
			path := s.writeTranscript(t, tc.family, tc.before...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := s.handle.WatchTurns(ctx)
			if err != nil {
				t.Fatalf("WatchTurns: %v", err)
			}
			if first := receiveTurnUpdate(t, ch); first.Err != nil {
				t.Fatalf("first update = %+v, want the history", first)
			}
			ask := clock.nextAsk(t)
			if err := s.store.Close(s.info.ID); err != nil {
				t.Fatalf("Close: %v", err)
			}
			appendTranscript(t, path, tc.after+"\n")
			if !tc.notify {
				if ask.d >= turnPollOpen {
					t.Fatalf("poll after %s re-resolves the transcript, want one sooner than %s", ask.d, turnPollOpen)
				}
				clock.advance(ask.d)
				ask.fire <- clock.now()
			}
			if got := receiveTurnUpdate(t, ch); !errors.Is(got.Err, runtime.ErrSessionNotFound) || errors.Is(got.Err, runtime.ErrRuntimeUnavailable) ||
				got.History.Current.Active() || len(got.History.Recent) != 0 {
				t.Fatalf("update after the close = %+v, want ErrSessionNotFound with no history", got)
			}
			// Nor does a later read, a backstop poll's included, read the
			// transcript on.
			ask = clock.nextAsk(t)
			clock.advance(turnPollOpen)
			ask.fire <- clock.now()
			clock.nextAsk(t)
			assertNoPendingUpdate(t, ch)
			cancel()
			assertWatchCloses(t, ch)
		})
	}
}

// A watch whose read stalls in the session store closes when its ctx ends,
// not when the store answers, for either source: the read's session reload is
// bounded like the rest of the read.
func TestWatchTurnsClosesWhileTheStoreStalls(t *testing.T) {
	cases := []struct {
		name       string
		sp         runtime.Provider
		provider   string
		transcript bool
	}{
		{name: "protocol", sp: runtime.NewFakeProfile(runtime.ProfileACP, newTurnScriptBackend(false)), provider: "claude"},
		{name: "transcript", sp: profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), provider: "codex", transcript: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTurnSession(t, tc.sp, tc.provider, "")
			if tc.transcript {
				s.writeCompletedTurn(t, tc.provider)
			}
			store := newStallingTurnStore(t, s.store)
			s.handle.manager = sessionpkg.NewManagerWithOptions(store, s.sp, sessionpkg.WithCityPath(t.TempDir()))
			clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
			s.handle.turns.timing = clock.timing()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := s.handle.WatchTurns(ctx)
			if err != nil {
				t.Fatalf("WatchTurns: %v", err)
			}
			if first := receiveTurnUpdate(t, ch); first.Err != nil {
				t.Fatalf("first update = %+v, want the history", first)
			}
			ask := clock.nextAsk(t)
			store.stall(s.info.ID)
			clock.advance(ask.d) // a backstop poll: a transcript re-resolves too
			ask.fire <- clock.now()
			store.awaitStalled(t)
			cancel()
			// Well inside turnWatchReadBound: the watch closes as its ctx
			// ends, not when the read's own bound runs out.
			assertWatchClosesWithin(t, ch, turnWatchReadBound/2)
		})
	}
}

// stallingTurnStore is a store whose reads of one bead, once stalled, block
// until the test ends, as a stalled backend's do.
type stallingTurnStore struct {
	beads.Store
	mu      sync.Mutex
	stalled string        // the bead whose reads block; empty: none
	entered chan struct{} // receives once a read blocks
	release chan struct{} // closed as the test ends
}

func newStallingTurnStore(t *testing.T, store beads.Store) *stallingTurnStore {
	t.Helper()
	s := &stallingTurnStore{Store: store, entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(s.release) })
	return s
}

// stall blocks the reads of id from now on.
func (s *stallingTurnStore) stall(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalled = id
}

func (s *stallingTurnStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	stalled := s.stalled != "" && id == s.stalled
	s.mu.Unlock()
	if stalled {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-s.release
	}
	return s.Store.Get(id)
}

// awaitStalled waits for a read to block.
func (s *stallingTurnStore) awaitStalled(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no read reached the stalled store")
	}
}

// Every read the watcher makes, the first and each one after, runs within
// turnWatchReadBound whatever its source: a read whose store or runtime does
// not answer fails as unavailable instead of holding the watch.
func TestRunTurnWatchBoundsEachRead(t *testing.T) {
	type bound struct {
		why  turnWake
		ok   bool
		left time.Duration
	}
	clock := newManualTurnClock(time.Now())
	reads := make(chan bound, 4)
	loop := turnWatchLoop{
		timing: *clock.timing(),
		read: func(ctx context.Context, why turnWake) (runtime.TurnHistory, error) {
			deadline, ok := ctx.Deadline()
			reads <- bound{why: why, ok: ok, left: time.Until(deadline)}
			return runtime.TurnHistory{}, nil
		},
		wait: func(runtime.TurnHistory, error) time.Duration { return turnPollIdle },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := startTurnWatch(ctx, loop)
	receiveTurnUpdate(t, ch)
	clock.nextAsk(t).fire <- clock.now()
	clock.nextAsk(t)
	for i, want := range []turnWake{turnWakeStart, turnWakePoll} {
		got := <-reads
		if got.why != want || !got.ok || got.left <= 0 || got.left > turnWatchReadBound {
			t.Fatalf("read %d = %+v, want wake %d with a deadline within %s", i, got, want, turnWatchReadBound)
		}
	}
	cancel()
	assertWatchCloses(t, ch)
}

// A protocol watch whose turn-event subscription failed polls alone and says
// so; a runtime that sends no turn events is no news.
func TestTurnHintsLogsAFailedSubscription(t *testing.T) {
	logs := captureTurnLogs(t)
	const fallback = "protocol turn watch falls back to polling"

	events := make(chan runtime.TurnEvent)
	if got := turnHints("s1", events, nil); got != events {
		t.Fatalf("turnHints of a subscription = %v, want its events", got)
	}
	if got := turnHints("s1", nil, fmt.Errorf("acp: %w", runtime.ErrInteractionUnsupported)); got != nil || strings.Contains(logs.String(), fallback) {
		t.Fatalf("turnHints without turn events = %v, logged %q; want nil and no fallback logged", got, logs.String())
	}
	if got := turnHints("s1", nil, errors.New("subscribe refused")); got != nil || !strings.Contains(logs.String(), fallback) || !strings.Contains(logs.String(), "subscribe refused") {
		t.Fatalf("turnHints of a failed subscription = %v, logged %q; want nil and the fallback logged with its error", got, logs.String())
	}
}

// turnLogBuffer is a log sink a test reads while other goroutines may write
// to it.
type turnLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *turnLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *turnLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureTurnLogs points slog.Default() at a buffer for the rest of the test.
// The cleanup restores the log package too: slog.SetDefault with a handler of
// its own also points log's output at that handler and zeroes log's flags,
// and setting the previous default back undoes neither.
func captureTurnLogs(t *testing.T) *turnLogBuffer {
	t.Helper()
	logs := &turnLogBuffer{}
	prev, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return logs
}

func TestTurnErrorClass(t *testing.T) {
	cases := []struct {
		err  error
		want turnErrClass
	}{
		{fmt.Errorf("x: %w", runtime.ErrRuntimeUnavailable), turnErrUnavailable},
		{fmt.Errorf("x: %w", runtime.ErrTurnIdentityUnsupported), turnErrUnsupported},
		{fmt.Errorf("x: %w", runtime.ErrSessionInitializing), turnErrInitializing},
		{fmt.Errorf("x: %w: %w", runtime.ErrSessionNotFound, runtime.ErrRuntimeUnavailable), turnErrNotFound},
		{fmt.Errorf("x: %w: %w", runtime.ErrTurnIdentityUnsupported, runtime.ErrRuntimeUnavailable), turnErrUnsupported},
		{errors.New("other"), turnErrOther},
	}
	for _, tc := range cases {
		if got := turnErrorClass(tc.err); got != tc.want {
			t.Errorf("turnErrorClass(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
