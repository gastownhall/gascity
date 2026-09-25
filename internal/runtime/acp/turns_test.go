package acp

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
)

// acpStopReasonCancelled is the ACP wire spelling of the cancel stop reason.
const acpStopReasonCancelled = "cancelled" //nolint:misspell // ACP wire value

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// fakeACPResultCommand is fakeACPShellCommand with a custom session/prompt
// result. result must be a JSON object that is also a Python literal (no
// true/false/null).
func fakeACPResultCommand(t *testing.T, result string) string {
	t.Helper()
	base := fakeACPShellCommand()
	const answer = "respond(msg_id, {})\n'"
	if !strings.HasSuffix(base, answer) {
		t.Fatal("fakeACPShellCommand no longer ends with the prompt answer")
	}
	return strings.TrimSuffix(base, answer) + "respond(msg_id, " + result + ")\n'"
}

// injectConn registers an in-process connection that no process backs, so
// busy state is driven by dispatch/drainPending calls from the test.
func injectConn(t *testing.T, p *Provider) (string, *sessionConn) {
	t.Helper()
	name := testName()
	sc := newSessionConn(nil, &erroringStdin{err: errors.New("no agent")}, nil, 100, nil)
	sc.sessionID = "session-1"
	p.mu.Lock()
	p.conns[name] = sc
	p.mu.Unlock()
	return name, sc
}

func respondTo(sc *sessionConn, id int64, result string) {
	sc.dispatch(JSONRPCMessage{JSONRPC: "2.0", ID: &id, Result: json.RawMessage(result)})
}

func TestTurnRecordsStopReasonAndUsage(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	result := `{"stopReason": "max_tokens", "usage": {"inputTokens": 12, "outputTokens": 34, "totalTokens": 46, "thoughtTokens": 5, "cachedReadTokens": 6, "cachedWriteTokens": 7}}`
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPResultCommand(t, result),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	if err := p.Nudge(name, runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if err := p.WaitForIdle(context.Background(), name, 5*time.Second); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	current, last := sc.turns()
	if current != nil {
		t.Fatalf("current turn = %+v, want none after the prompt response", current)
	}
	if last == nil {
		t.Fatal("no last turn recorded")
	}
	if last.State != turnCompleted || last.StopReason != "max_tokens" || last.Error != "" {
		t.Fatalf("last turn = %+v, want completed with stopReason max_tokens", last)
	}
	want := turnUsage{InputTokens: 12, OutputTokens: 34, TotalTokens: 46, ThoughtTokens: 5, CachedReadTokens: 6, CachedWriteTokens: 7}
	if last.Usage == nil || *last.Usage != want {
		t.Fatalf("usage = %+v, want %+v", last.Usage, want)
	}
	if last.PromptID == 0 || !uuidV4Pattern.MatchString(last.ID) {
		t.Fatalf("turn identity = (%d, %q), want a prompt id and a UUIDv4", last.PromptID, last.ID)
	}
	if last.StartedAt.IsZero() || last.EndedAt.Before(last.StartedAt) {
		t.Fatalf("turn times = %v..%v, want a started turn that ended after it started", last.StartedAt, last.EndedAt)
	}
}

func TestTurnRecordOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		settle func(sc *sessionConn, id int64)
		want   turnRecord
	}{
		{
			name:   "end_turn without usage",
			settle: func(sc *sessionConn, id int64) { respondTo(sc, id, `{"stopReason":"end_turn"}`) },
			want:   turnRecord{State: turnCompleted, StopReason: "end_turn"},
		},
		{
			name:   "cancel",
			settle: func(sc *sessionConn, id int64) { respondTo(sc, id, `{"stopReason":"`+acpStopReasonCancelled+`"}`) },
			want:   turnRecord{State: turnCompleted, StopReason: acpStopReasonCancelled},
		},
		{
			name: "json-rpc error",
			settle: func(sc *sessionConn, id int64) {
				sc.dispatch(JSONRPCMessage{JSONRPC: "2.0", ID: &id, Error: &JSONRPCError{Code: -32603, Message: "boom"}})
			},
			want: turnRecord{State: turnFailed, Error: "boom"},
		},
		{
			name:   "undecodable result",
			settle: func(sc *sessionConn, id int64) { respondTo(sc, id, `{"stopReason":7}`) },
			want:   turnRecord{State: turnFailed},
		},
		{
			name:   "agent exit mid-turn",
			settle: func(sc *sessionConn, _ int64) { sc.drainPending() },
			want:   turnRecord{State: turnFailed, Error: turnFailureAgentExited},
		},
		{
			name:   "prompt never sent",
			settle: func(sc *sessionConn, id int64) { sc.abandonPrompt(id, errors.New("broken pipe")) },
			want:   turnRecord{State: turnFailed, Error: "sending prompt: broken pipe"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := newSessionConn(nil, nil, nil, 10, nil)
			sc.setActivePrompt(41)
			current, _ := sc.turns()
			if current == nil || current.State != turnRunning || current.PromptID != 41 {
				t.Fatalf("current turn = %+v, want running prompt 41", current)
			}
			tc.settle(sc, 41)
			if sc.isBusy() {
				t.Fatal("still busy after the turn settled")
			}
			current, last := sc.turns()
			if current != nil || last == nil {
				t.Fatalf("turns = (%+v, %+v), want only a last turn", current, last)
			}
			if last.PromptID != 41 || !uuidV4Pattern.MatchString(last.ID) || last.EndedAt.IsZero() {
				t.Fatalf("last turn = %+v, want prompt 41 with a UUIDv4 id and an end time", last)
			}
			if last.State != tc.want.State || last.StopReason != tc.want.StopReason || last.Usage != nil {
				t.Fatalf("last turn = %+v, want %+v", last, tc.want)
			}
			if tc.want.Error != "" && last.Error != tc.want.Error {
				t.Fatalf("last turn error = %q, want %q", last.Error, tc.want.Error)
			}
			if tc.want.State == turnFailed && last.Error == "" {
				t.Fatal("failed turn has no error message")
			}
		})
	}
}

func TestTurnRecordIgnoresUnrelatedResponses(t *testing.T) {
	sc := newSessionConn(nil, nil, nil, 10, nil)
	sc.setActivePrompt(9)
	respondTo(sc, 8, `{"stopReason":"end_turn"}`)
	if current, last := sc.turns(); current == nil || last != nil {
		t.Fatalf("turns = (%+v, %+v), want prompt 9 still running", current, last)
	}
}

func TestTurnIDsAreUniquePerTurn(t *testing.T) {
	sc := newSessionConn(nil, nil, nil, 10, nil)
	seen := map[string]bool{}
	for id := int64(1); id <= 3; id++ {
		sc.setActivePrompt(id)
		respondTo(sc, id, `{"stopReason":"end_turn"}`)
		_, last := sc.turns()
		if !uuidV4Pattern.MatchString(last.ID) {
			t.Fatalf("turn id %q is not a UUIDv4", last.ID)
		}
		if seen[last.ID] {
			t.Fatalf("turn id %q reused", last.ID)
		}
		seen[last.ID] = true
	}
}

func TestTurnRecordReturnsCopies(t *testing.T) {
	sc := newSessionConn(nil, nil, nil, 10, nil)
	sc.setActivePrompt(1)
	respondTo(sc, 1, `{"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`)
	_, last := sc.turns()
	last.Usage.InputTokens = 99
	last.StopReason = "mutated"
	if _, again := sc.turns(); again.Usage.InputTokens != 1 || again.StopReason != "end_turn" {
		t.Fatalf("turn record mutated through a returned copy: %+v", again)
	}
}

func TestWaitForIdle(t *testing.T) {
	t.Run("idle returns nil", func(t *testing.T) {
		p := newTestProvider(t)
		name, _ := injectConn(t, p)
		if err := p.WaitForIdle(context.Background(), name, time.Second); err != nil {
			t.Fatalf("WaitForIdle = %v, want nil", err)
		}
	})

	t.Run("returns when the turn ends", func(t *testing.T) {
		p := newTestProvider(t)
		name, sc := injectConn(t, p)
		sc.setActivePrompt(5)
		done := make(chan error, 1)
		go func() { done <- p.WaitForIdle(context.Background(), name, time.Minute) }()
		select {
		case err := <-done:
			t.Fatalf("WaitForIdle returned %v while the prompt was outstanding", err)
		default:
		}
		respondTo(sc, 5, `{"stopReason":"end_turn"}`)
		if err := <-done; err != nil {
			t.Fatalf("WaitForIdle = %v, want nil after the turn ended", err)
		}
	})

	t.Run("times out while busy", func(t *testing.T) {
		p := newTestProvider(t)
		name, sc := injectConn(t, p)
		sc.setActivePrompt(5)
		err := p.WaitForIdle(context.Background(), name, time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitForIdle = %v, want context.DeadlineExceeded", err)
		}
		if err := p.WaitForIdle(context.Background(), name, 0); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitForIdle(timeout 0) = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("honors ctx cancel", func(t *testing.T) {
		p := newTestProvider(t)
		name, sc := injectConn(t, p)
		sc.setActivePrompt(5)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := p.WaitForIdle(ctx, name, time.Minute); !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitForIdle = %v, want context.Canceled", err)
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		p := newTestProvider(t)
		err := p.WaitForIdle(context.Background(), "gc-acp-missing", time.Second)
		if !errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("WaitForIdle = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("dead session", func(t *testing.T) {
		p := newTestProvider(t)
		name, sc := injectConn(t, p)
		sc.drainPending()
		close(sc.done)
		if err := p.WaitForIdle(context.Background(), name, time.Second); !errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("WaitForIdle = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("agent exits while busy", func(t *testing.T) {
		p := newTestProvider(t)
		name, sc := injectConn(t, p)
		sc.setActivePrompt(5)
		done := make(chan error, 1)
		go func() { done <- p.WaitForIdle(context.Background(), name, time.Minute) }()
		// Same order as the monitor goroutine: drain, then close done.
		sc.drainPending()
		close(sc.done)
		if err := <-done; !errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("WaitForIdle = %v, want ErrSessionNotFound when the agent exits mid-turn", err)
		}
	})

	t.Run("drained but not yet reaped", func(t *testing.T) {
		p := newTestProvider(t)
		name, sc := injectConn(t, p)
		sc.drainPending()
		if err := p.WaitForIdle(context.Background(), name, time.Second); !errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("WaitForIdle = %v, want ErrSessionNotFound for a drained connection", err)
		}
	})

	t.Run("starting session is not idle", func(t *testing.T) {
		p := newTestProvider(t)
		name := testName()
		p.mu.Lock()
		p.conns[name] = &sessionConn{done: make(chan struct{}), cancel: func() {}, pending: map[int64]chan JSONRPCMessage{}}
		p.mu.Unlock()
		if err := p.WaitForIdle(context.Background(), name, time.Second); !errors.Is(err, errACPSessionStarting) {
			t.Fatalf("WaitForIdle = %v, want errACPSessionStarting", err)
		}
	})
}

func TestSnapshotIdle(t *testing.T) {
	p := newTestProvider(t)
	name, sc := injectConn(t, p)

	if idle, err := p.SnapshotIdle(name); err != nil || !idle {
		t.Fatalf("SnapshotIdle idle = (%v, %v), want (true, nil)", idle, err)
	}
	sc.setActivePrompt(3)
	if idle, err := p.SnapshotIdle(name); err != nil || idle {
		t.Fatalf("SnapshotIdle busy = (%v, %v), want (false, nil)", idle, err)
	}
	respondTo(sc, 3, `{"stopReason":"end_turn"}`)
	if idle, err := p.SnapshotIdle(name); err != nil || !idle {
		t.Fatalf("SnapshotIdle after turn = (%v, %v), want (true, nil)", idle, err)
	}
	if idle, err := p.SnapshotIdle("gc-acp-missing"); !errors.Is(err, runtime.ErrSessionNotFound) || idle {
		t.Fatalf("SnapshotIdle unknown = (%v, %v), want (false, ErrSessionNotFound)", idle, err)
	}
	sc.drainPending()
	close(sc.done)
	if idle, err := p.SnapshotIdle(name); !errors.Is(err, runtime.ErrSessionNotFound) || idle {
		t.Fatalf("SnapshotIdle dead = (%v, %v), want (false, ErrSessionNotFound)", idle, err)
	}
}

// TestIdleInterfacesSurviveProductionWrapping pins that the idle interfaces
// reach callers through the seam-backed provider production builds and
// through the auto router for a session routed to ACP.
func TestIdleInterfacesSurviveProductionWrapping(t *testing.T) {
	if _, ok := NewSeamBackedWithDir(shortTempDir(t), Config{}).(runtime.IdleWaitProvider); !ok {
		t.Fatal("NewSeamBackedWithDir does not implement runtime.IdleWaitProvider")
	}
	if _, ok := NewSeamBackedWithDir(shortTempDir(t), Config{}).(runtime.IdleSnapshotProvider); !ok {
		t.Fatal("NewSeamBackedWithDir does not implement runtime.IdleSnapshotProvider")
	}

	seam := seamBack(NewProviderWithDir(shortTempDir(t), Config{}))
	name, sc := injectConn(t, seam.raw)
	router := auto.New(runtime.NewFake(), seam)
	router.RouteACP(name)
	var waiter runtime.IdleWaitProvider = router
	var snap runtime.IdleSnapshotProvider = router

	sc.setActivePrompt(11)
	if idle, err := snap.SnapshotIdle(name); err != nil || idle {
		t.Fatalf("auto SnapshotIdle busy = (%v, %v), want (false, nil)", idle, err)
	}
	if err := waiter.WaitForIdle(context.Background(), name, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("auto WaitForIdle busy = %v, want context.DeadlineExceeded", err)
	}
	respondTo(sc, 11, `{"stopReason":"end_turn"}`)
	if err := waiter.WaitForIdle(context.Background(), name, time.Second); err != nil {
		t.Fatalf("auto WaitForIdle idle = %v, want nil", err)
	}
	if idle, err := snap.SnapshotIdle(name); err != nil || !idle {
		t.Fatalf("auto SnapshotIdle idle = (%v, %v), want (true, nil)", idle, err)
	}
}
