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

// TestNudgeConfirmBusyRenderLag guards ga-0hgwyb / ga-civwyz against a REAL,
// isolated tmux server (its own -L socket, killed on cleanup — it never
// touches the gc tmux server or any live agent session).
//
// submitEnterAndConfirm reports an unconfirmed submit whenever the pane's
// busy indicator renders later than the confirm budget, EVEN THOUGH the
// Enter reached the pane and the agent received the message. Before
// ga-civwyz, cmd/gc/cmd_nudge.go classified that as a delivery FAILURE
// (failedQueuedNudge), requeued the item, and re-injected the same reminder
// on the next pass — up to defaultQueuedNudgeMaxAttempts=5 copies of a
// message the session already got on attempt 1. NudgeSession now inspects
// the drained composer and returns ErrNudgeSubmitDeliveredUnobserved
// instead: proven delivery, so the queue must not retry it.
//
// The fake TUI logs every line it actually SUBMITS, so the assertion "err
// says delivered-but-unobserved" and the assertion "the TUI received it
// exactly once" can be made in the same test. That pairing is the whole
// finding.
func TestNudgeConfirmBusyRenderLag(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server)")
	}
	script := fakeTUIScript(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for the fake TUI")
	}

	// startFakeTUI brings up one throwaway tmux server running the fake TUI and
	// returns the Tmux handle plus the TUI's submit log path.
	startFakeTUI := func(t *testing.T, socket, sess string, busyDelay time.Duration) (*Tmux, string) {
		t.Helper()
		logPath := filepath.Join(t.TempDir(), "submits.log")
		tm := NewTmuxWithConfig(Config{
			SocketName:        socket,
			NudgeReadyTimeout: 10 * time.Second,
			NudgeLockTimeout:  10 * time.Second,
		})
		_, _ = tm.run("kill-server")
		t.Cleanup(func() { _, _ = tm.run("kill-server") })
		cmd := fmt.Sprintf("python3 %s %s %.3f 30",
			shellQuote(script), shellQuote(logPath), busyDelay.Seconds())
		if _, err := tm.run("new-session", "-d", "-s", sess, "-x", "100", "-y", "30", cmd); err != nil {
			t.Skipf("cannot create tmux session (tmux unavailable?): %v", err)
		}
		// GC_PROVIDER=claude routes NudgeSession through submitVerifyEligible ->
		// submitEnterAndConfirm, the code path under test.
		if err := tm.SetEnvironment(sess, "GC_PROVIDER", "claude"); err != nil {
			t.Fatalf("SetEnvironment: %v", err)
		}
		// Let the TUI paint its first frame before any keystroke.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if out, err := tm.CapturePane(sess, 40); err == nil && strings.Contains(out, "fake-claude-tui") {
				return tm, logPath
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("fake TUI never painted")
		return nil, ""
	}

	submits := func(t *testing.T, logPath string) []string {
		t.Helper()
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("reading TUI log: %v", err)
		}
		var got []string
		for _, line := range strings.Split(string(raw), "\n") {
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) == 3 && parts[1] == "SUBMIT" {
				got = append(got, parts[2])
			}
		}
		return got
	}

	const msg = "gc-nudge-lag-probe"

	// The composer drains — the fake TUI clears its draft the instant Enter
	// lands — but the busy indicator renders after the confirm budget. This is
	// the production shape: 1201 of these in ~/.gc/supervisor.log over 5 days,
	// and delivery is proven, so it must not be classified as a failure.
	t.Run("indicator later than budget: submitted once, reported delivered-unobserved", func(t *testing.T) {
		tm, logPath := startFakeTUI(t, "gcconfirmlagslow", "lag-slow", 8*time.Second)

		start := time.Now()
		err := tm.NudgeSession("lag-slow", msg)
		span := time.Since(start)
		t.Logf("NudgeSession returned after %v: %v", span, err)

		if !errors.Is(err, ErrNudgeSubmitDeliveredUnobserved) {
			t.Fatalf("err = %v, want ErrNudgeSubmitDeliveredUnobserved", err)
		}
		if errors.Is(err, ErrNudgeSubmitUnconfirmed) {
			t.Fatalf("err = %v must NOT also match ErrNudgeSubmitUnconfirmed: proven delivery must not be retried", err)
		}
		// The confirm budget is what the caller pays before giving up.
		t.Logf("MEASURED confirm budget (paste debounce + confirm loop) = %v", span)

		got := submits(t, logPath)
		if len(got) == 0 {
			t.Fatal("TUI recorded no submit — the message really was undelivered")
		}
		for _, s := range got {
			if !strings.Contains(s, msg) {
				t.Fatalf("unexpected submit %q", s)
			}
		}
		// THE FIX: gc correctly classifies this as delivered-but-unobserved, not
		// a failure — so the queue acks it instead of re-injecting a duplicate.
		t.Logf("CORRECTLY CLASSIFIED: NudgeSession reported %v; the TUI submitted the message %d time(s): %q",
			ErrNudgeSubmitDeliveredUnobserved, len(got), got)
	})

	// CONTROL: identical path, indicator renders inside the budget. Same
	// keystrokes, same code, opposite verdict — so the verdict is a function of
	// render latency alone, not of whether the message was delivered.
	t.Run("indicator inside budget: submitted once, reported confirmed", func(t *testing.T) {
		tm, logPath := startFakeTUI(t, "gcconfirmlagfast", "lag-fast", 100*time.Millisecond)

		if err := tm.NudgeSession("lag-fast", msg); err != nil {
			t.Fatalf("NudgeSession = %v, want nil (indicator inside budget)", err)
		}
		got := submits(t, logPath)
		if len(got) != 1 {
			t.Fatalf("TUI submits = %d (%q), want exactly 1", len(got), got)
		}
		t.Logf("CONTROL: same message, same path, confirmed; TUI submitted %q once", got)
	})
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// fakeTUIScript resolves the in-repo fake TUI, so the reproduction is
// self-contained: no env var, no external file. GC_FAKE_TUI still overrides it
// for ad-hoc experiments.
func fakeTUIScript(t *testing.T) string {
	t.Helper()
	if override := os.Getenv("GC_FAKE_TUI"); override != "" {
		return override
	}
	abs, err := filepath.Abs(filepath.Join("testdata", "nudge_faketui.py"))
	if err != nil {
		t.Fatalf("resolving fake TUI: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fake TUI missing at %s: %v", abs, err)
	}
	return abs
}

// TestNudgeConfirmBudgetThreshold measures where the confirm verdict flips, so
// the fix can be sized against a real number instead of the arithmetic sum of
// submitEnterMaxSends x submitConfirmPollsPerSend x submitConfirmPollInterval.
// Every row submits the message exactly once; only the verdict changes.
func TestNudgeConfirmBudgetThreshold(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux measurement")
	}
	script := fakeTUIScript(t)

	for _, delay := range []time.Duration{
		500 * time.Millisecond,
		1 * time.Second,
		1500 * time.Millisecond,
		2 * time.Second,
		2500 * time.Millisecond,
		3 * time.Second,
	} {
		t.Run(delay.String(), func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "submits.log")
			sess := "thr"
			tm := NewTmuxWithConfig(Config{
				SocketName:        "gcconfirmthr",
				NudgeReadyTimeout: 10 * time.Second,
				NudgeLockTimeout:  10 * time.Second,
			})
			_, _ = tm.run("kill-server")
			t.Cleanup(func() { _, _ = tm.run("kill-server") })
			cmd := fmt.Sprintf("python3 %s %s %.3f 30",
				shellQuote(script), shellQuote(logPath), delay.Seconds())
			if _, err := tm.run("new-session", "-d", "-s", sess, "-x", "100", "-y", "30", cmd); err != nil {
				t.Skipf("cannot create tmux session: %v", err)
			}
			if err := tm.SetEnvironment(sess, "GC_PROVIDER", "claude"); err != nil {
				t.Fatalf("SetEnvironment: %v", err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if out, err := tm.CapturePane(sess, 40); err == nil && strings.Contains(out, "fake-claude-tui") {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}

			start := time.Now()
			err := tm.NudgeSession(sess, "gc-nudge-threshold-probe")
			span := time.Since(start)

			raw, _ := os.ReadFile(logPath)
			n := strings.Count(string(raw), "\tSUBMIT\t")
			verdict := "CONFIRMED"
			if errors.Is(err, ErrNudgeSubmitUnconfirmed) {
				verdict = "UNCONFIRMED"
			} else if err != nil {
				verdict = "ERR:" + err.Error()
			}
			t.Logf("busy-render-lag=%-7s verdict=%-11s span=%-15v submits=%d", delay, verdict, span.Round(time.Millisecond), n)
			if n != 1 {
				t.Errorf("submits = %d, want exactly 1 (the message reached the agent regardless of verdict)", n)
			}
		})
	}
}
