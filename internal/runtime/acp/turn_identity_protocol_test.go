//go:build integration

package acp

import (
	"errors"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills a turn history that disagrees with the reference agent's turns: over
// nine answered prompts the ring holds the last eight newest first, the
// oldest dropped, each completed.
func TestACPProtocolTurnHistoryRing(t *testing.T) {
	s := startProtocolFake(t)
	ctx := t.Context()
	if st, err := s.p.ObserveTurn(ctx, s.name); err != nil || st.Active() {
		t.Fatalf("idle = %+v, %v; want no turn", st, err)
	}
	var ids []string
	for range turnHistoryLen + 1 {
		s.nudge(t, "hello")
		s.waitIdle(t)
		ids = append(ids, s.lastTurn(t).ID)
	}
	h, err := s.p.TurnHistory(ctx, s.name)
	if err != nil {
		t.Fatalf("TurnHistory: %v", err)
	}
	var got []string
	for _, ended := range h.Recent {
		got = append(got, ended.TurnID)
		if ended.Status != runtime.TurnStatusCompleted {
			t.Errorf("turn %q status %q, want completed", ended.TurnID, ended.Status)
		}
	}
	want := slices.Clone(ids[1:])
	slices.Reverse(want)
	if !slices.Equal(got, want) {
		t.Fatalf("recent = %q, want %q", got, want)
	}
}

// Kills a turn-scoped interrupt that reaches the reference agent when stale,
// or misses its running turn: a stale id leaves methods.jsonl without
// session/cancel and signals.jsonl empty, and the current id is canceled,
// fenced, with its end observed.
func TestACPProtocolInterruptTurn(t *testing.T) {
	s := startProtocolFake(t, "--on-cancel", "reply", "--turn-delay", "1h")
	ctx := t.Context()
	s.nudge(t, "long turn")
	waitForPrompt(t, s)
	st, err := s.p.ObserveTurn(ctx, s.name)
	if err != nil || !st.Active() {
		t.Fatalf("running = %+v, %v; want the turn", st, err)
	}
	if _, err := s.p.InterruptTurn(ctx, s.name, "stale"); !errors.Is(err, runtime.ErrTurnNotCurrent) {
		t.Fatalf("stale InterruptTurn = %v, want ErrTurnNotCurrent", err)
	}
	if n := s.methodCount(t, methodSessionCancel); n != 0 {
		t.Fatalf("stale interrupt sent %d session/cancel (%v)", n, s.methods(t))
	}
	ack, err := s.p.InterruptTurn(ctx, s.name, st.TurnID)
	if err != nil || !ack.Fenced {
		t.Fatalf("InterruptTurn = %+v, %v; want sent, fenced", ack, err)
	}
	if after, err := s.p.WaitTurnEnded(ctx, s.name, st.TurnID, protocolWait); err != nil || after.Active() {
		t.Fatalf("WaitTurnEnded = %+v, %v; want the turn ended", after, err)
	}
	if n := s.methodCount(t, methodSessionCancel); n != 1 {
		t.Fatalf("session/cancel count = %d, want 1 (%v)", n, s.methods(t))
	}
	if signals := s.jsonlRecords(t, "signals.jsonl"); len(signals) != 0 {
		t.Fatalf("signals.jsonl = %v, want none", signals)
	}
	if turn := s.lastTurn(t); turn.ID != st.TurnID || turn.StopReason != acpStopReasonCancelled {
		t.Fatalf("last turn = %+v, want %q canceled", turn, st.TurnID)
	}
}
