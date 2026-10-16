package worker

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

func TestCurrentTurnReadsClaudeTranscripts(t *testing.T) {
	cases := []struct {
		name    string
		lines   []string
		want    func(t *testing.T) runtime.TurnHistory
		wantErr error
	}{
		{
			name: "completed",
			lines: []string{
				claudePrompt(claudeTurn1, "2026-10-13T05:00:00.123Z"), claudeReply("2026-10-13T05:00:04.000Z"), claudeTurnDuration("2026-10-13T05:00:05.456Z"),
				claudePrompt(claudeTurn2, "2026-10-13T05:01:00.789Z"), claudeReply("2026-10-13T05:01:02.000Z"), claudeTurnDuration("2026-10-13T05:01:03.001Z"),
			},
			want: func(t *testing.T) runtime.TurnHistory {
				return runtime.TurnHistory{Recent: []runtime.EndedTurn{
					{TurnID: claudeTurn2, StartedAt: mustTurnTime(t, "2026-10-13T05:01:00.789Z"), EndedAt: mustTurnTime(t, "2026-10-13T05:01:03.001Z"), Status: runtime.TurnStatusCompleted},
					{TurnID: claudeTurn1, StartedAt: mustTurnTime(t, "2026-10-13T05:00:00.123Z"), EndedAt: mustTurnTime(t, "2026-10-13T05:00:05.456Z"), Status: runtime.TurnStatusCompleted},
				}}
			},
		},
		{
			// The interrupt marker carries the next prompt's id: it closes the
			// open turn as canceled and is never the next turn's.
			name: "interrupt with the next prompt's id",
			lines: []string{
				claudePrompt(claudeTurn1, "2026-10-13T05:00:00.123Z"),
				claudeInterrupt(claudeTurn2, "2026-10-13T05:00:10.000Z"),
				claudePrompt(claudeTurn2, "2026-10-13T05:00:10.050Z"),
			},
			want: func(t *testing.T) runtime.TurnHistory {
				return runtime.TurnHistory{
					Current:      runtime.TurnState{TurnID: claudeTurn2},
					CurrentStart: mustTurnTime(t, "2026-10-13T05:00:10.050Z"),
					Recent: []runtime.EndedTurn{
						{TurnID: claudeTurn1, StartedAt: mustTurnTime(t, "2026-10-13T05:00:00.123Z"), EndedAt: mustTurnTime(t, "2026-10-13T05:00:10.000Z"), Status: runtime.TurnStatusCancelled},
					},
				}
			},
		},
		{
			name:    "torn tail",
			lines:   []string{claudePrompt(claudeTurn1, "2026-10-13T05:00:00.123Z"), claudeReply("2026-10-13T05:00:04.000Z"), claudeTurnDuration("2026-10-13T05:00:05.456Z"), `{"type":"user","timestamp":"2026-10-13T05:01`},
			wantErr: runtime.ErrRuntimeUnavailable,
		},
		{
			// An older CLI records prompts without promptId: boundaries, not
			// identity, so the read says so even between turns, never idle.
			name:    "turns recorded without ids, between turns",
			lines:   []string{claudePromptWithoutID("2026-10-13T05:00:00.123Z"), claudeReply("2026-10-13T05:00:04.000Z"), claudeTurnDuration("2026-10-13T05:00:05.456Z")},
			wantErr: runtime.ErrTurnIdentityUnsupported,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
			s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "claude", "")
			s.handle.turns.timing = clock.timing()
			path := s.writeTranscript(t, "claude", tc.lines...)
			ageTranscript(t, path, clock.now()) // not quiet: the overlay stays out of it
			assertCurrentTurn(t, s.handle, clock.now(), tc.want, tc.wantErr)
		})
	}
}

func TestCurrentTurnReadsCodexTranscripts(t *testing.T) {
	cases := []struct {
		name    string
		lines   []string
		want    func(t *testing.T) runtime.TurnHistory
		wantErr error
	}{
		{
			// StartedAt is the task_started line's envelope timestamp, never its
			// whole-second started_at.
			name: "started and complete",
			lines: []string{
				codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), codexComplete(codexTurn1, "2026-10-08T02:22:30.500Z"),
				codexStarted(codexTurn2, "2026-10-08T02:23:01.007Z", 1791426181),
			},
			want: func(t *testing.T) runtime.TurnHistory {
				return runtime.TurnHistory{
					Current:      runtime.TurnState{TurnID: codexTurn2},
					CurrentStart: mustTurnTime(t, "2026-10-08T02:23:01.007Z"),
					Recent: []runtime.EndedTurn{
						{TurnID: codexTurn1, StartedAt: mustTurnTime(t, "2026-10-08T02:22:22.191Z"), EndedAt: mustTurnTime(t, "2026-10-08T02:22:30.500Z"), Status: runtime.TurnStatusCompleted},
					},
				}
			},
		},
		{
			name:  "aborted",
			lines: []string{codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), codexAborted(codexTurn1, "2026-10-08T02:22:27.401Z")},
			want: func(t *testing.T) runtime.TurnHistory {
				return runtime.TurnHistory{Recent: []runtime.EndedTurn{
					{TurnID: codexTurn1, StartedAt: mustTurnTime(t, "2026-10-08T02:22:22.191Z"), EndedAt: mustTurnTime(t, "2026-10-08T02:22:27.401Z"), Status: runtime.TurnStatusCancelled},
				}}
			},
		},
		{
			name:    "torn tail",
			lines:   []string{codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), `{"timestamp":"2026-10-08T02:22:30.500Z","type":"event_msg","payload":{"type":"task_comp`},
			wantErr: runtime.ErrRuntimeUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idle := newIdleScript()
			clock := newManualTurnClock(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
			s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "codex", "")
			s.handle.turns.timing = clock.timing()
			path := s.writeTranscript(t, "codex", tc.lines...)
			ageTranscript(t, path, clock.now().Add(-time.Hour))
			assertCurrentTurn(t, s.handle, clock.now(), tc.want, tc.wantErr)
		})
	}
}

// A transcript whose window, cut off by the scan budget, holds no turn record
// is unavailable (X11 I8), never idle.
func TestCurrentTurnStartBeyondBudgetIsUnavailable(t *testing.T) {
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "codex", "")
	path := s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
	filler := `{"timestamp":"2026-10-08T02:22:23.000Z","type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 1<<20) + `"}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	for range 17 { // past the 16 MiB scan budget
		if _, err := f.WriteString(filler); err != nil {
			t.Fatalf("WriteString: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := s.handle.CurrentTurn(context.Background())
	assertNoHistory(t, got, err, runtime.ErrRuntimeUnavailable)
	if !errors.Is(err, sessionlog.ErrTurnStartBeyondBudget) {
		t.Fatalf("CurrentTurn error = %v, want it to name ErrTurnStartBeyondBudget", err)
	}
}

func TestTurnHistoryFromScan(t *testing.T) {
	at := time.Date(2026, 10, 13, 5, 0, 0, 123456789, time.UTC)
	observed := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	t.Run("ids verbatim, millisecond times, statuses mapped", func(t *testing.T) {
		scan := sessionlog.TurnScan{
			Open:    true,
			Current: sessionlog.ScannedTurn{ID: "open", StartedAt: at},
			Recent: []sessionlog.ScannedTurn{
				{ID: "failed", StartedAt: at, EndedAt: at, Status: sessionlog.TurnStatusFailed},
				{ID: "closed-by-next", StartedAt: at, EndedAt: at},
			},
		}
		got, err := turnHistoryFromScan(scan, observed)
		if err != nil {
			t.Fatalf("turnHistoryFromScan: %v", err)
		}
		ms := at.Truncate(time.Millisecond)
		if got.Current.TurnID != "open" || !got.CurrentStart.Equal(ms) || !got.ObservedAt.Equal(observed) {
			t.Fatalf("current = %+v", got)
		}
		if got.Recent[0].Status != runtime.TurnStatusFailed || got.Recent[1].Status != "" || !got.Recent[0].StartedAt.Equal(ms) {
			t.Fatalf("recent = %+v", got.Recent)
		}
	})
	t.Run("turns recorded without ids are not identity, even with none open", func(t *testing.T) {
		got, err := turnHistoryFromScan(sessionlog.TurnScan{Recent: []sessionlog.ScannedTurn{{EndedAt: at}, {ID: "kept", EndedAt: at}}, MissingIDs: true}, observed)
		if !errors.Is(err, runtime.ErrTurnIdentityUnsupported) || len(got.Recent) != 0 {
			t.Fatalf("turnHistoryFromScan = %+v, %v; want ErrTurnIdentityUnsupported and no history", got, err)
		}
	})
	t.Run("an open turn without an id is not idle", func(t *testing.T) {
		_, err := turnHistoryFromScan(sessionlog.TurnScan{Open: true, MissingIDs: true}, observed)
		if !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
			t.Fatalf("turnHistoryFromScan error = %v, want ErrTurnIdentityUnsupported", err)
		}
	})
}

// A transcript source whose transcript is not there (not written yet, or
// gone) is unavailable through the public read, never unsupported (X11 §8).
func TestCurrentTurnMissingTranscriptIsUnavailable(t *testing.T) {
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "codex", "")
	got, err := s.handle.CurrentTurn(context.Background())
	assertNoHistory(t, got, err, runtime.ErrRuntimeUnavailable)
	path := s.writeCompletedTurn(t, "codex")
	if _, err := s.handle.CurrentTurn(context.Background()); err != nil {
		t.Fatalf("CurrentTurn once written: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, err = s.handle.CurrentTurn(context.Background())
	assertNoHistory(t, got, err, runtime.ErrRuntimeUnavailable)
}

func TestCurrentTurnWithoutSourceIsUnsupported(t *testing.T) {
	s := newTurnSession(t, profiled(runtime.ProfileExec, runtime.FullFake{Fake: runtime.NewFake()}), "claude", "")
	if _, err := s.handle.CurrentTurn(context.Background()); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
		t.Fatalf("CurrentTurn = %v, want ErrTurnIdentityUnsupported", err)
	}
	if _, err := s.handle.WatchTurns(context.Background()); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
		t.Fatalf("WatchTurns = %v, want ErrTurnIdentityUnsupported", err)
	}
}

func TestCurrentTurnProtocolSourceReadsTheRuntime(t *testing.T) {
	fake := runtime.NewFake()
	s := newTurnSession(t, profiled(runtime.ProfileACP, runtime.FullFake{Fake: fake}), "claude", "")
	at := time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)
	want := runtime.TurnHistory{Current: runtime.TurnState{TurnID: "acp-1"}, CurrentStart: at, ObservedAt: at}
	fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{s.info.SessionName: {fullTurnCaps}}
	fake.TurnHistoryScripts = map[string][]runtime.TurnHistory{s.info.SessionName: {want}}
	got, err := s.handle.CurrentTurn(context.Background())
	if err != nil {
		t.Fatalf("CurrentTurn: %v", err)
	}
	assertTurnHistory(t, got, want)

	fake.TurnHistoryErrors = map[string][]error{s.info.SessionName: {runtime.ErrRuntimeUnavailable}}
	if _, err := s.handle.CurrentTurn(context.Background()); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("CurrentTurn on a failed read = %v, want ErrRuntimeUnavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.handle.CurrentTurn(ctx); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("CurrentTurn under an ended ctx = %v, want ErrRuntimeUnavailable (bounded, unanswered)", err)
	}
}

// gatedHistoryBackend is a protocol backend whose TurnHistory, once armed,
// blocks until released, counting its calls.
type gatedHistoryBackend struct {
	runtime.FullFake
	armed   *atomic.Bool
	entered chan struct{}
	release chan struct{}
	mu      *sync.Mutex
	calls   *int
}

func (b gatedHistoryBackend) TurnCapabilities(string) runtime.TurnCapabilities { return fullTurnCaps }

func (b gatedHistoryBackend) TurnHistory(ctx context.Context, _ string) (runtime.TurnHistory, error) {
	if !b.armed.Load() {
		return runtime.TurnHistory{ObservedAt: time.Now()}, nil
	}
	b.mu.Lock()
	*b.calls++
	b.mu.Unlock()
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return runtime.TurnHistory{Current: runtime.TurnState{TurnID: "acp-1"}, ObservedAt: time.Now()}, nil
	case <-ctx.Done():
		return runtime.TurnHistory{}, ctx.Err()
	}
}

// Kills a second concurrent reader of one session failing as unavailable:
// readers of a key share the outstanding read.
func TestCurrentTurnConcurrentReadersShareOneRead(t *testing.T) {
	calls, mu := 0, &sync.Mutex{}
	backend := gatedHistoryBackend{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, armed: &atomic.Bool{}, entered: make(chan struct{}, 4), release: make(chan struct{}), mu: mu, calls: &calls}
	s := newTurnSession(t, profiled(runtime.ProfileACP, backend), "claude", "")
	backend.armed.Store(true)
	type answer struct {
		hist runtime.TurnHistory
		err  error
	}
	answers := make(chan answer, 2)
	read := func() {
		h, err := s.handle.CurrentTurn(context.Background())
		answers <- answer{h, err}
	}
	go read()
	<-backend.entered // the first read is in flight
	go read()
	// The second reader joins the outstanding read rather than fail: no
	// answer before the read returns.
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
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("TurnHistory calls = %d, want the readers to share", calls)
	}
}

// Kills a turn read that shares a key, or an in-flight limit, with liveness
// observation: a wedged liveness read of the same session does not block it.
func TestCurrentTurnKeyIsApartFromLivenessObservation(t *testing.T) {
	fake := runtime.NewFake()
	s := newTurnSession(t, profiled(runtime.ProfileACP, runtime.FullFake{Fake: fake}), "claude", "")
	fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{s.info.SessionName: {fullTurnCaps}}
	fake.TurnHistoryScripts = map[string][]runtime.TurnHistory{s.info.SessionName: {{Current: runtime.TurnState{TurnID: "acp-1"}, ObservedAt: time.Now()}}}
	scope := s.handle.turnObservationScope()
	wedge := newWedgedObservation()
	defer wedge.unblock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Liveness keys name the city and the session (cmd/gc
	// postStartObservationKey); hold one, and the turn key's own namespace
	// under the other bounded map, both wedged.
	for _, key := range []string{scope + "\x00" + s.info.SessionName, turnObservationKey("history", scope, s.info.SessionName)} {
		started := make(chan struct{})
		go ObserveBounded(ctx, key, func(ctx context.Context) (int, error) { //nolint:errcheck // a wedged observation
			close(started)
			return wedge.observe(ctx)
		})
		<-started
	}
	got, err := s.handle.CurrentTurn(context.Background())
	if err != nil || got.Current.TurnID != "acp-1" {
		t.Fatalf("CurrentTurn beside wedged liveness reads = %+v, %v; want the history", got, err)
	}
}

// The idle overlay (X11 §5.2): after 2 s of quiet, two idle readings with no
// growth between them end the open turn, inferred, at the observer's clock.
// Composing the capabilities takes no reading.
func TestCurrentTurnIdleOverlay(t *testing.T) {
	growErr := errors.New("pane unreadable")
	cases := []struct {
		name      string
		quiet     time.Duration
		answers   []idleAnswer
		grow      bool // the transcript grows before the second reading
		wantEnded bool
		wantCalls int
	}{
		{name: "quiet and idle twice", quiet: 3 * time.Second, answers: []idleAnswer{{idle: true}, {idle: true}}, wantEnded: true, wantCalls: 2},
		{name: "not quiet long enough", quiet: time.Second, answers: []idleAnswer{{idle: true}}, wantCalls: 0},
		{name: "busy reading", quiet: 3 * time.Second, answers: []idleAnswer{{idle: true}, {idle: false}}, wantCalls: 2},
		{name: "first reading busy", quiet: 3 * time.Second, answers: []idleAnswer{{idle: false}}, wantCalls: 1},
		{name: "snapshot error", quiet: 3 * time.Second, answers: []idleAnswer{{idle: true, err: growErr}}, wantCalls: 1},
		{name: "growth between the readings", quiet: 3 * time.Second, answers: []idleAnswer{{idle: true}, {idle: true}}, grow: true, wantCalls: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idle := newIdleScript(tc.answers...)
			clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
			s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "claude", "")
			s.handle.turns.timing = clock.timing()
			path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
			ageTranscript(t, path, clock.now().Add(-tc.quiet))
			if tc.grow {
				idle.state.before = func(call int) {
					if call == 2 {
						appendTranscript(t, path, claudeReply("2026-10-13T05:59:59.000Z")+"\n")
					}
				}
			}
			got, err := s.handle.CurrentTurn(context.Background())
			if err != nil {
				t.Fatalf("CurrentTurn: %v", err)
			}
			if calls := idle.idleCalls(); calls != tc.wantCalls {
				t.Fatalf("SnapshotIdle calls = %d, want %d", calls, tc.wantCalls)
			}
			if !tc.wantEnded {
				if got.Current.TurnID != claudeTurn1 || len(got.Recent) != 0 {
					t.Fatalf("history = %+v, want the turn still open", got)
				}
				return
			}
			if got.Current.Active() || len(got.Recent) != 1 {
				t.Fatalf("history = %+v, want the turn ended", got)
			}
			ended := got.Recent[0]
			if ended.TurnID != claudeTurn1 || !ended.Inferred || ended.Status != "" || !ended.EndedAt.Equal(clock.now()) ||
				!ended.StartedAt.Equal(mustTurnTime(t, "2026-10-13T05:59:50.123Z")) {
				t.Fatalf("ended = %+v, want %s inferred at the observer clock", ended, claudeTurn1)
			}
			// The unchanged transcript keeps the inferred end without new
			// readings, at the same EndedAt.
			clock.advance(time.Minute)
			again, err := s.handle.CurrentTurn(context.Background())
			if err != nil || len(again.Recent) != 1 || !again.Recent[0].EndedAt.Equal(ended.EndedAt) || idle.idleCalls() != tc.wantCalls {
				t.Fatalf("second read = %+v, %v (calls %d); want the same inferred end", again, err, idle.idleCalls())
			}
			// New entries reopen it.
			appendTranscript(t, path, claudeReply("2026-10-13T06:00:30.000Z")+"\n")
			reopened, err := s.handle.CurrentTurn(context.Background())
			if err != nil || reopened.Current.TurnID != claudeTurn1 {
				t.Fatalf("after growth = %+v, %v; want the turn open again", reopened, err)
			}
		})
	}
}

// Kills an inferred end later than the observation that reports it: the
// clock moves during the gap between the two readings.
func TestCurrentTurnInferredEndIsNotAfterObservedAt(t *testing.T) {
	idle := newIdleScript(idleAnswer{idle: true}, idleAnswer{idle: true})
	clock := newManualTurnClock(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "claude", "")
	s.handle.turns.timing = &turnTiming{
		now:      clock.now,
		idleGap:  turnIdleGap,
		noNotify: true,
		after: func(d time.Duration) <-chan time.Time {
			clock.advance(d)
			fire := make(chan time.Time, 1)
			fire <- clock.now()
			return fire
		},
	}
	path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
	ageTranscript(t, path, clock.now().Add(-time.Minute))
	got, err := s.handle.CurrentTurn(context.Background())
	if err != nil || len(got.Recent) != 1 || !got.Recent[0].Inferred {
		t.Fatalf("CurrentTurn = %+v, %v; want the inferred end", got, err)
	}
	if got.Recent[0].EndedAt.After(got.ObservedAt) {
		t.Fatalf("EndedAt %s after ObservedAt %s", got.Recent[0].EndedAt, got.ObservedAt)
	}
}

func TestCurrentTurnOverlaySkipsRecordedEnds(t *testing.T) {
	idle := newIdleScript()
	clock := newManualTurnClock(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "codex", "")
	s.handle.turns.timing = clock.timing()
	path := s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
	ageTranscript(t, path, clock.now().Add(-time.Hour))
	got, err := s.handle.CurrentTurn(context.Background())
	if err != nil || got.Current.TurnID != codexTurn1 {
		t.Fatalf("CurrentTurn = %+v, %v; want the open turn", got, err)
	}
	// Codex records every end: the read takes no snapshot.
	if calls := idle.idleCalls(); calls != 0 {
		t.Fatalf("SnapshotIdle calls = %d, want 0 for a family that records its ends", calls)
	}
}

// Interrupts keep today's paths: a session-scoped interrupt still calls
// StopTurn's sequence on a session whose turns are observed.
func TestTurnObservationLeavesInterruptUnchanged(t *testing.T) {
	fake := runtime.NewFake()
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: fake}), "claude", "")
	s.writeCompletedTurn(t, "claude")
	if _, err := s.handle.CurrentTurn(context.Background()); err != nil {
		t.Fatalf("CurrentTurn: %v", err)
	}
	fake.Calls = nil
	if err := s.handle.Interrupt(context.Background(), InterruptRequest{}); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if lastCall(fake.Calls, "Interrupt") == nil {
		t.Fatalf("calls = %+v, want the keystroke interrupt StopTurn sends", fake.Calls)
	}
}
