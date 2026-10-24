package tmux

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// codexPaneSessionProvider is a session runtime whose interrupt keys, boundary
// wait and background-work stop reach a tmux Provider over a fakeCodexPane,
// while the session lifecycle, idle waits and message delivery stay on a
// runtime.Fake. It lets a session.Manager drive the live Codex pane through
// StopTurn and an interrupt_now submit.
type codexPaneSessionProvider struct {
	*runtime.Fake
	tp   *Provider
	pane *fakeCodexPane
	t    *testing.T
	// turn is the turn the pane's Codex is running, "" while it is idle.
	turn string
}

// SendKeys reaches the pane; an Escape or Ctrl-C aborts the running turn
// (abortTurn).
func (p *codexPaneSessionProvider) SendKeys(name string, keys ...string) error {
	if err := p.tp.SendKeys(name, keys...); err != nil {
		return err
	}
	if slices.Contains(keys, "Escape") || slices.Contains(keys, "C-c") {
		p.abortTurn()
	}
	return nil
}

func (p *codexPaneSessionProvider) Interrupt(name string) error {
	if err := p.tp.Interrupt(name); err != nil {
		return err
	}
	p.abortTurn()
	return nil
}

// abortTurn records the running turn aborted in the rollout, as Codex does
// when an interrupt lands. An idle Codex has no turn to abort and records
// nothing.
func (p *codexPaneSessionProvider) abortTurn() {
	p.t.Helper()
	if p.turn == "" {
		return
	}
	appendCodexRollout(p.t, p.pane.rollout, codexTurnAbortedRows(p.turn)...)
	p.turn = ""
}

func (p *codexPaneSessionProvider) WaitForInterruptBoundary(ctx context.Context, name string, since time.Time, timeout time.Duration) error {
	return p.tp.WaitForInterruptBoundary(ctx, name, since, timeout)
}

// StopBackgroundWork reaches the tmux Provider's, when it has one.
func (p *codexPaneSessionProvider) StopBackgroundWork(ctx context.Context, name string) error {
	stopper, ok := any(p.tp).(interface {
		StopBackgroundWork(context.Context, string) error
	})
	if !ok {
		return runtime.ErrInteractionUnsupported
	}
	return stopper.StopBackgroundWork(ctx, name)
}

// newCodexPaneSession creates a Codex session whose pane is the live
// interrupted pane with one background terminal running, while its Codex runs
// a turn.
func newCodexPaneSession(t *testing.T) (*session.Manager, session.Info, session.Actor, *fakeCodexPane) {
	t.Helper()
	mgr, info, by, sp := newCodexPaneSessionRunning(t, "turn-1")
	return mgr, info, by, sp.pane
}

// newCodexPaneSessionRunning is newCodexPaneSession with its Codex running
// turn: the rollout records the turn's start and no end. With turn "" Codex
// is idle, its last turn completed a minute before.
func newCodexPaneSessionRunning(t *testing.T, turn string) (*session.Manager, session.Info, session.Actor, *codexPaneSessionProvider) {
	t.Helper()
	pane := newFakeCodexPane(t)
	sp := &codexPaneSessionProvider{Fake: runtime.NewFake(), tp: &Provider{tm: newCodexStopTmux(pane)}, pane: pane, t: t, turn: turn}
	rollout := []string{codexLifecycleRow("task_started", "turn-0"), codexLifecycleRow("task_complete", "turn-0")}
	if turn != "" {
		rollout = []string{codexLifecycleRow("task_started", turn)}
	}
	writeCodexRollout(t, pane.rollout, time.Now().Add(-time.Minute), rollout...)
	cityPath := t.TempDir()
	mgr := session.NewManagerWithOptions(beads.NewMemStore(), sp, session.WithCityPath(cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	city, err := session.NewCityDir(cityPath)
	if err != nil {
		t.Fatalf("NewCityDir: %v", err)
	}
	return mgr, info, session.Actor{Kind: session.ActorOperator, City: city}, sp
}

// TestCodexStopTurnStopsBackgroundTerminals: an explicit stop (POST
// /session/{id}/stop, a run cancel) ends the Codex turn with Escape and then
// closes the background terminal its command kept running in.
func TestCodexStopTurnStopsBackgroundTerminals(t *testing.T) {
	mgr, info, _, pane := newCodexPaneSession(t)

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}
	if got, want := pane.typedOrPressed(), []string{"key:Escape", "type:/stop", "key:Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Codex pane = %q, want %q", got, want)
	}
	if pane.background != 0 {
		t.Fatalf("background terminals = %d after StopTurn, want 0", pane.background)
	}
}

// TestCodexInterruptNowSubmitLeavesBackgroundTerminalsRunning: a replacement
// message interrupts the turn but must not send /stop, which would also close
// an earlier turn's dev server.
func TestCodexInterruptNowSubmitLeavesBackgroundTerminalsRunning(t *testing.T) {
	mgr, info, by, pane := newCodexPaneSession(t)

	if _, err := mgr.Submit(context.Background(), by, info.ID, "do this instead", session.BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, session.SubmitIntentInterruptNow); err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if got, want := pane.typedOrPressed(), []string{"key:Escape"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Codex pane = %q, want %q: an interrupt_now submit must not send /stop", got, want)
	}
	if pane.background != 1 {
		t.Fatalf("background terminals = %d, want the 1 still running", pane.background)
	}
}

// TestCodexStopTurnOfAnIdleSessionStopsBackgroundTerminals: Codex records
// nothing when Escape reaches it between turns, so a stop of an idle session
// has no turn end to wait for. It succeeds at once, and still closes the
// background terminal an earlier turn left running.
func TestCodexStopTurnOfAnIdleSessionStopsBackgroundTerminals(t *testing.T) {
	mgr, info, _, sp := newCodexPaneSessionRunning(t, "")

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}
	if got, want := sp.pane.typedOrPressed(), []string{"key:Escape", "type:/stop", "key:Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the idle Codex pane = %q, want %q", got, want)
	}
	if sp.pane.background != 0 {
		t.Fatalf("background terminals = %d after StopTurn, want 0", sp.pane.background)
	}
}

// TestCodexInterruptNowSubmitToAnIdleSessionDeliversWithoutRestart: an
// interrupt_now submit to an idle Codex session has no turn end to wait for,
// so it delivers the message to the running session instead of stopping and
// restarting it into a fresh conversation.
func TestCodexInterruptNowSubmitToAnIdleSessionDeliversWithoutRestart(t *testing.T) {
	mgr, info, by, sp := newCodexPaneSessionRunning(t, "")
	before := len(sp.Calls)

	if _, err := mgr.Submit(context.Background(), by, info.ID, "do this instead", session.BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, session.SubmitIntentInterruptNow); err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	var methods []string
	for _, call := range sp.Calls[before:] {
		methods = append(methods, call.Method+":"+call.Message)
	}
	if slices.ContainsFunc(methods, func(m string) bool { return strings.HasPrefix(m, "Stop:") || strings.HasPrefix(m, "Start:") }) {
		t.Fatalf("runtime calls = %q, want no Stop or Start: the idle session must not be restarted", methods)
	}
	if !slices.Contains(methods, "NudgeNow:do this instead") {
		t.Fatalf("runtime calls = %q, want the message delivered to the running session with NudgeNow", methods)
	}
	if got, want := sp.pane.typedOrPressed(), []string{"key:Escape"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the idle Codex pane = %q, want %q", got, want)
	}
}

// TestCodexInterruptBoundarySendsNoKeys: the boundary wait every Codex
// interrupt shares only waits; it types nothing into the pane.
func TestCodexInterruptBoundarySendsNoKeys(t *testing.T) {
	pane := newFakeCodexPane(t)
	tm := newCodexStopTmux(pane)

	if err := tm.WaitForInterruptBoundary(context.Background(), "codex-bg-stop-g", time.Now().Add(-time.Minute), time.Second); err != nil {
		t.Fatalf("WaitForInterruptBoundary: %v", err)
	}
	if got := pane.typedOrPressed(); len(got) != 0 {
		t.Fatalf("keys reaching the Codex pane = %q, want none from the boundary wait", got)
	}
}
