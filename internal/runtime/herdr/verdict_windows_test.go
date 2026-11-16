//go:build windows

package herdr

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// herdr 0.9.1 on Windows reports a CLI error by exiting non-zero, with one error
// envelope as the whole of stderr and nothing on stdout. These tests answer each
// verdict the provider acts on in that shape. Their typed-envelope twins are in
// startup_delivery_confirm_test.go and agent_name_taken_test.go.

// A stalled startup prompt on an idle agent is recovered with one explicit Enter
// and a confirming wait. It is not left for the reconcile-tick nudge, which would
// type its text into the box the stalled prompt already filled.
func TestStartupDeliveryStallRecoversWhenHerdrExitsNonZero(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled_exit")

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "pane send-keys %5 Enter") {
		t.Fatalf("a stall herdr reported by exit status was not recovered with an explicit Enter:\n%s", calls)
	}
	if get := strings.Index(calls, "agent get %5"); get < 0 || get > strings.Index(calls, "pane send-keys %5 Enter") {
		t.Fatalf("recovery Enter was not qualified by reading the agent state first:\n%s", calls)
	}
	if !strings.Contains(calls, "agent wait %5 --until working --until done --until blocked --timeout "+strconv.Itoa(startupEnterRecoveryWaitMS)) {
		t.Fatalf("Enter recovery was not re-confirmed:\n%s", calls)
	}
	if n := strings.Count(calls, "agent prompt"); n != 1 {
		t.Fatalf("stall recovery must not re-prompt; prompts = %d:\n%s", n, calls)
	}
	if v, _ := p.GetMeta("gastown__witness", metaStartupUnconfirmed); v != "" {
		t.Fatalf("recovered delivery recorded an unconfirmed marker: %q", v)
	}
}

// An agent that loses keystrokes while it boots (Copilot CLI loading its
// plugins, measured) loses the recovery Enter too. The Enter is retried, each
// time qualified by the state herdr reports, until the agent takes it.
func TestStartupDeliveryStallRetriesEnterUntilTheAgentTakesIt(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled_exit")
	setState(t, state, "enter_late")

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "pane send-keys %5 Enter"); n != 3 {
		t.Fatalf("recovery Enters = %d; want 3, the third taken:\n%s", n, calls)
	}
	assertEachEnterQualified(t, calls)
	if n := strings.Count(calls, "agent prompt"); n != 1 {
		t.Fatalf("stall recovery must not re-prompt; prompts = %d:\n%s", n, calls)
	}
	if v, _ := p.GetMeta("gastown__witness", metaStartupUnconfirmed); v != "" {
		t.Fatalf("recovered delivery recorded an unconfirmed marker: %q", v)
	}
}

// A retry is qualified by the state herdr reports. Once herdr reports the turn
// running, the recovery ends without another keystroke, even though the wait
// that followed the Enter which submitted it ran out.
func TestStartupDeliveryStallRecoveryStopsOnceHerdrReportsTheTurn(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled_exit")
	setState(t, state, "enter_late")
	setState(t, state, "wait_times_out_exit")

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "pane send-keys %5 Enter"); n != 3 {
		t.Fatalf("recovery Enters = %d; want 3, none after herdr reported the turn:\n%s", n, calls)
	}
	if v, _ := p.GetMeta("gastown__witness", metaStartupUnconfirmed); v != "" {
		t.Fatalf("a turn herdr reports running recorded an unconfirmed marker: %q", v)
	}
}

// The first check after a stall reads the state herdr reports before any
// recovery Enter. herdr reported the stall with the agent idle, so an agent it
// now reports working or done took the turn late: the recovery ends there,
// with no keystroke and no unconfirmed marker, as it would one attempt later.
// Blocked still withholds the Enter and records the strand, because an agent
// already parked on a dialog reads the same.
func TestStartupDeliveryStallFirstCheckConfirmsALateTurn(t *testing.T) {
	for _, tc := range []struct {
		state     string
		confirmed bool
	}{
		{state: "working", confirmed: true},
		{state: "done", confirmed: true},
		{state: "blocked", confirmed: false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, p)
			setState(t, state, "prompt_stalled_exit")
			setState(t, state, "agent_"+tc.state)

			cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
			if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
				t.Fatalf("Start: %v", err)
			}
			calls := fakeCalls(t, state)
			if !strings.Contains(calls, "agent get %5") {
				t.Fatalf("the stall was not qualified by reading the agent state:\n%s", calls)
			}
			if strings.Contains(calls, "send-keys") {
				t.Fatalf("an agent herdr reports %s drew a recovery Enter:\n%s", tc.state, calls)
			}
			v, err := p.GetMeta("gastown__witness", metaStartupUnconfirmed)
			if tc.confirmed {
				if v != "" {
					t.Fatalf("a turn herdr reports %s at the first check recorded an unconfirmed marker: %q", tc.state, v)
				}
				return
			}
			if err != nil || v == "" {
				t.Fatalf("withheld recovery left no sidecar marker (v=%q err=%v)", v, err)
			}
			if !strings.Contains(v, tc.state) || !strings.Contains(v, "recovery Enter withheld") {
				t.Fatalf("marker does not say which state withheld the Enter: %q", v)
			}
		})
	}
}

// assertEachEnterQualified fails unless every recovery Enter in calls follows
// its own agent get.
func assertEachEnterQualified(t *testing.T, calls string) {
	t.Helper()
	lines := strings.Split(calls, "\n")
	qualified := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "agent get %5"):
			qualified = true
		case strings.HasPrefix(line, "pane send-keys %5 Enter"):
			if !qualified {
				t.Fatalf("a recovery Enter was not qualified by reading the agent state first:\n%s", calls)
			}
			qualified = false
		}
	}
}

// When the recovery's own wait also fails the same way, the strand is still
// recorded with the stall verdict, after the Enter was tried.
func TestStartupDeliveryUnconfirmedAfterRecoveryWhenHerdrExitsNonZero(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "prompt_stalled_exit")
	setState(t, state, "wait_times_out_exit")

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "pane send-keys %5 Enter"); n != startupEnterRecoveryAttempts {
		t.Fatalf("recovery Enters = %d; want every one of the %d attempts:\n%s", n, startupEnterRecoveryAttempts, calls)
	}
	assertEachEnterQualified(t, calls)
	v, err := p.GetMeta("gastown__witness", metaStartupUnconfirmed)
	if err != nil || v == "" {
		t.Fatalf("unconfirmed startup delivery left no sidecar marker (v=%q err=%v)", v, err)
	}
	if !strings.Contains(v, "agent_prompt_stalled") || !strings.Contains(v, "after Enter recovery") {
		t.Fatalf("marker does not record a stall that survived the Enter recovery: %q", v)
	}
}

// A name herdr still holds, reported by exit status, is resolved against the
// holder: a live one is adopted, so Start succeeds without a second launch and
// without a startup turn.
func TestStartAdoptsNameTakenWhenHerdrExitsNonZero(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "name_taken_exit")

	cfg := runtime.Config{Command: "claude", Nudge: "Run gc hook --claim --json now."}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start did not adopt a live holder of its name: %v", err)
	}
	calls := fakeCalls(t, state)
	if n := strings.Count(calls, "agent start"); n != 1 {
		t.Fatalf("adopting a live holder must not retry the launch; agent starts = %d:\n%s", n, calls)
	}
	if strings.Contains(calls, "agent prompt") {
		t.Fatalf("adopted a live holder but still delivered a startup turn:\n%s", calls)
	}
}

// An idle wait that runs out, reported by exit status, reads as a timeout rather
// than as an unexpected error.
func TestWaitForIdleOutcomeTimeoutWhenHerdrExitsNonZero(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	setState(t, state, "wait_times_out_exit")
	if got := p.waitForIdleOutcome(context.Background(), "gastown__witness", time.Second); got != idleWaitTimeout {
		t.Fatalf("timed-out wait outcome = %q; want %q", got, idleWaitTimeout)
	}
}
