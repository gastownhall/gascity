//go:build integration

package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNudgeRecoversStagedDraftSwallowedByPasteIngest reproduces the staged-draft
// stall against a REAL, isolated tmux server (its own -L socket, killed on
// cleanup). A codex-shaped fake TUI collapses the pasted nudge into
// "[Pasted Content N chars]" and eats the first few submits, the way a loaded
// codex TUI still ingesting a large paste does. Before the fix every submit in
// the ~2.4s confirm window was eaten, NudgeSession gave up unconfirmed, and the
// draft stayed staged forever. The fake TUI logs every submit it takes, so each
// case can assert both the verdict and that the message was taken exactly once.
func TestNudgeRecoversStagedDraftSwallowedByPasteIngest(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server)")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for the fake TUI")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "nudge_faketui_codex.py"))
	if err != nil {
		t.Fatalf("resolving fake TUI: %v", err)
	}

	start := func(t *testing.T, socket string, swallow int, busyDelay time.Duration) (*Tmux, string) {
		t.Helper()
		logPath := filepath.Join(t.TempDir(), "tui.log")
		tm := NewTmuxWithConfig(Config{
			SocketName:        socket,
			NudgeReadyTimeout: 10 * time.Second,
			NudgeLockTimeout:  30 * time.Second,
		})
		_, _ = tm.run("kill-server")
		t.Cleanup(func() { _, _ = tm.run("kill-server") })
		cmd := fmt.Sprintf("python3 %s %s %d %.3f 30", shellQuote(script), shellQuote(logPath), swallow, busyDelay.Seconds())
		if _, err := tm.run("new-session", "-d", "-s", "draft", "-x", "120", "-y", "30", cmd); err != nil {
			t.Skipf("cannot create tmux session (tmux unavailable?): %v", err)
		}
		// GC_PROVIDER=codex routes NudgeSession through the verified submit
		// path, codex's Escape+Enter submit sequence, and its draft marker.
		if err := tm.SetEnvironment("draft", "GC_PROVIDER", "codex"); err != nil {
			t.Fatalf("SetEnvironment: %v", err)
		}
		// Let the TUI paint its first frame before any keystroke.
		poll := time.NewTicker(100 * time.Millisecond)
		defer poll.Stop()
		deadline := time.After(5 * time.Second)
		for {
			if out, err := tm.CapturePane("draft", 40); err == nil && strings.Contains(out, "fake-codex-tui") {
				return tm, logPath
			}
			select {
			case <-poll.C:
			case <-deadline:
				t.Fatal("fake TUI never painted")
				return nil, ""
			}
		}
	}

	count := func(t *testing.T, logPath, event string) int {
		t.Helper()
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("reading TUI log: %v", err)
		}
		return strings.Count(string(raw), "\t"+event+"\t")
	}

	largeNudge := "gc-staged-draft-probe " + strings.Repeat("work the claimed bead ", 60)

	t.Run("submits eaten past the confirm window: recovered, submitted once", func(t *testing.T) {
		const swallow = submitEnterMaxSends + 2
		tm, logPath := start(t, "gcstageddraftbusy", swallow, 100*time.Millisecond)

		if err := tm.NudgeSession("draft", largeNudge); err != nil {
			t.Fatalf("NudgeSession = %v, want nil (recovery should land the staged draft)", err)
		}
		if got := count(t, logPath, "SUBMIT"); got != 1 {
			t.Fatalf("TUI submits = %d, want exactly 1", got)
		}
		if got := count(t, logPath, "SWALLOWED"); got != swallow {
			t.Fatalf("TUI swallowed submits = %d, want %d", got, swallow)
		}
	})

	t.Run("draft clears but busy never renders: delivered-unobserved, submitted once", func(t *testing.T) {
		tm, logPath := start(t, "gcstageddraftclear", submitEnterMaxSends+1, 60*time.Second)

		began := time.Now()
		err := tm.NudgeSession("draft", largeNudge)
		t.Logf("NudgeSession returned after %v: %v", time.Since(began), err)
		if !errors.Is(err, ErrNudgeSubmitDeliveredUnobserved) {
			t.Fatalf("err = %v, want ErrNudgeSubmitDeliveredUnobserved", err)
		}
		if got := count(t, logPath, "SUBMIT"); got != 1 {
			t.Fatalf("TUI submits = %d, want exactly 1", got)
		}
	})

	// CONTROL: the same eaten submits, but the draft is short enough to show
	// inline, so no staged-draft marker is on screen. Recovery must not fire:
	// exactly the ordinary window's submits reach the pane and the verdict
	// stays unconfirmed, as before the fix.
	t.Run("no staged-draft marker: no extra submits", func(t *testing.T) {
		tm, logPath := start(t, "gcstageddraftnone", submitEnterMaxSends+2, 100*time.Millisecond)

		err := tm.NudgeSession("draft", "gc-staged-draft-short-probe")
		if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
			t.Fatalf("err = %v, want ErrNudgeSubmitUnconfirmed", err)
		}
		if got := count(t, logPath, "SWALLOWED"); got != submitEnterMaxSends {
			t.Fatalf("TUI saw %d submits, want exactly the ordinary window's %d", got, submitEnterMaxSends)
		}
		if got := count(t, logPath, "SUBMIT"); got != 0 {
			t.Fatalf("TUI submits = %d, want 0", got)
		}
	})
}
