package tmux

import (
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// HasBackgroundWork reports whether session's agent has work running outside
// its turn: the background shells Claude Code's footer counts under its input
// box (claudePaneReportsBackgroundShells), or the background terminals Codex's
// status area reports (codexPaneReportsBackgroundTerminals). It implements
// [runtime.BackgroundWorkObserver]; the idle sleep asks it after WaitForIdle
// succeeds, because such a pane shows the ready prompt and no spinner while
// the work it would kill is still running. It reads the same rows the
// explicit stop (StopBackgroundWork) reads, so what the stop would have
// stopped is what holds the sleep. A pane with no input box on screen reports
// no work. Sessions that run neither Codex nor Claude return
// runtime.ErrInteractionUnsupported.
func (t *Tmux) HasBackgroundWork(session string) (bool, error) {
	target, err := t.agentPaneTarget(session)
	if err != nil {
		return false, err
	}
	switch family := sessionlog.ProviderFamily(t.providerEnv(session)); {
	case family == "codex" || (family != "claude" && t.targetLooksLikeProvider(session, "codex")):
		out, err := t.CaptureVisiblePane(target)
		if err != nil || out == "" {
			return false, err
		}
		running, _ := codexPaneReportsBackgroundTerminals(strings.Split(out, "\n"))
		return running, nil
	case family == "claude" || t.targetLooksLikeProvider(session, "claude"):
		lines, err := t.captureInputLines(target)
		if err != nil {
			return false, err
		}
		shells, _ := claudePaneReportsBackgroundShells(lines)
		return shells > 0, nil
	default:
		return false, runtime.ErrInteractionUnsupported
	}
}
