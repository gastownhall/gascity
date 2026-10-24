package tmux

import "testing"

// capturedLoginExpiredPane is a verbatim capture-pane snapshot taken from the
// live gascity--builder session at 2026-09-03T09:16 PDT during ga-6tfjok. The
// turn failed on auth in 0s and the pane fell straight back to the idle prompt.
const capturedLoginExpiredPane = `
  These are discovery-time hints, not execution gates — the vendor loads every
  skill from the sink directory regardless of what this appendix lists.


● Login expired · Please run /login

✻ Cooked for 0s · done 8:46 AM

❯ Check your hook for work and act on it.

● Login expired · Please run /login

✻ Crunched for 0s · done 8:46 AM
                                        ✔ Update installed · Restart to update
────────────────────────────────────────────────────────────────────────────────
❯ 
────────────────────────────────────────────────────────────────────────────────

  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents
`

func paneLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// TestBusyIndicatorCorrectlyRejectsAuthDeadPane DISPROVES the alternative
// hypothesis that the 4h silence was a busy-indicator false negative (i.e. the
// pane really was working and paneContainsBusyIndicator failed to match a new
// spinner glyph). It is not: on the real captured pane the classifier correctly
// reports "not busy". The defect is that "not busy" is under-specified, NOT that
// busy detection is broken.
func TestBusyIndicatorCorrectlyRejectsAuthDeadPane(t *testing.T) {
	if paneContainsBusyIndicator(paneLines(capturedLoginExpiredPane)) {
		t.Fatal("busy=true on an auth-dead pane: hypothesis H-alt2 would hold")
	}
	t.Log("DISPROVED H-alt2: busy detection is correct; the pane is genuinely idle")
}

// TestProductionCanClassifyAuthDeadPane proves paneContainsAuthFailure
// separates "idle because the submit was lost" (retryable, ga-bwm) from
// "idle because every turn dies on auth" (never retryable) on the real
// captured pane. Both reach submitEnterAndConfirm as a bare `false` from the
// busy closure; without this classifier both exit as
// ErrNudgeSubmitUnconfirmed, which adapter.go downgrades to a warning.
func TestProductionCanClassifyAuthDeadPane(t *testing.T) {
	if !paneContainsAuthFailure(paneLines(capturedLoginExpiredPane)) {
		t.Fatal("paneContainsAuthFailure = false on a real captured auth-dead pane, want true")
	}
}
