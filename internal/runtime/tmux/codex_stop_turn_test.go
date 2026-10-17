package tmux

import (
	"context"
	"os"
	"slices"
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
}

// SendKeys reaches the pane; an Escape or Ctrl-C aborts the turn, which Codex
// records in its rollout.
func (p *codexPaneSessionProvider) SendKeys(name string, keys ...string) error {
	if err := p.tp.SendKeys(name, keys...); err != nil {
		return err
	}
	if slices.Contains(keys, "Escape") || slices.Contains(keys, "C-c") {
		p.recordTurnAborted()
	}
	return nil
}

func (p *codexPaneSessionProvider) Interrupt(name string) error {
	if err := p.tp.Interrupt(name); err != nil {
		return err
	}
	p.recordTurnAborted()
	return nil
}

// recordTurnAborted appends Codex's turn_aborted marker to the rollout, as
// Codex does when an interrupt lands.
func (p *codexPaneSessionProvider) recordTurnAborted() {
	p.t.Helper()
	rollout := p.pane.rollout
	f, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		p.t.Fatalf("opening rollout: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<turn_aborted>\nThe user interrupted the previous turn on purpose.\n</turn_aborted>"}]}}` + "\n"); err != nil {
		p.t.Fatalf("writing rollout: %v", err)
	}
	// Stamp the write time explicitly. Linux filesystems take file times from
	// the kernel's coarse clock, which can trail time.Now() by a few
	// milliseconds, so a write made microseconds after the interrupt began can
	// read as older than it. Real Codex writes this marker long after GC's
	// interrupt keys, well clear of that granularity.
	now := time.Now()
	if err := os.Chtimes(rollout, now, now); err != nil {
		p.t.Fatalf("stamping rollout: %v", err)
	}
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
// interrupted pane with one background terminal running.
func newCodexPaneSession(t *testing.T) (*session.Manager, session.Info, session.Actor, *fakeCodexPane) {
	t.Helper()
	pane := newFakeCodexPane(t)
	sp := &codexPaneSessionProvider{Fake: runtime.NewFake(), tp: &Provider{tm: newCodexStopTmux(pane)}, pane: pane, t: t}
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
	return mgr, info, session.Actor{Kind: session.ActorOperator, City: city}, pane
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
