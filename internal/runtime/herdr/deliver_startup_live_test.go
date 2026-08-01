package herdr

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDeliverNudgeSurvivesStartupRace is the regression for the swallowed
// startup prompt: delivering to a freshly-launched agent, before its TUI is
// listening, must still result in a submitted turn.
//
// Bare `agent prompt` reports success on a swallowed submit, so the failure is
// silent — the agent idles forever with work it never began. deliverNudge now
// waits for interactive_ready and lets herdr's --wait confirm each attempt,
// retrying a stall.
//
// Against the unfixed path this fails at whichever race it loses first: the
// agent is not registered yet (agent_not_found) or the submit is swallowed and
// no turn ever runs. Both are the same defect — delivering before the TUI is
// listening — and both are what the readiness wait plus --wait confirmation fix.
//
// Live: needs herdr ≥0.7.5 and claude. Skipped in -short.
func TestDeliverNudgeSurvivesStartupRace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live herdr test in -short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}

	session := fmt.Sprintf("gctest-startup-%d", time.Now().UnixNano()%100000)
	c := newClient(session, t.TempDir())
	if err := c.startServer(); err != nil {
		t.Fatalf("startServer: %v", err)
	}
	t.Cleanup(func() { _ = c.stopServer() })

	ctx := context.Background()
	// Place a pane and launch an agent into it, then deliver immediately —
	// the exact ordering Start uses, which is what loses the race.
	wsID, _, pane, err := c.workspaceCreate(ctx, "startup-probe", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("workspaceCreate: %v", err)
	}
	if wsID == "" || pane == "" {
		t.Fatalf("workspaceCreate returned empty ids: workspace=%q pane=%q", wsID, pane)
	}
	if err := c.paneRun(ctx, pane, "claude"); err != nil {
		t.Fatalf("paneRun(claude): %v", err)
	}

	// The proof must be the agent's ANSWER, never the echoed prompt: a
	// swallowed submit still leaves the prompt text visible in the input, so
	// matching any token from the prompt passes even when nothing ran. Ask for
	// a value that appears only once the model has actually replied.
	const answer = "8686"
	if err := c.deliverNudge(ctx, pane, "What is 4343 times 2? Reply with only the number."); err != nil {
		t.Fatalf("deliverNudge raced the boot and never confirmed: %v", err)
	}

	// Confirm the turn actually ran, not merely that delivery returned nil.
	// deliverNudge's --wait already established that the submit moved the agent
	// off idle, so wait on herdr's own completion signal for that turn to finish
	// rather than polling the screen (TESTING.md: wait for facts, not elapsed
	// time), then read once.
	if _, err := c.run(ctx, "agent", "wait", pane, "--until", "idle",
		"--timeout", strconv.Itoa(int(turnCompletionBudget/time.Millisecond))); err != nil {
		t.Fatalf("agent never returned to idle after the startup nudge: %v", err)
	}
	screen, err := c.paneRead(ctx, pane, "visible", 40)
	if err != nil {
		t.Fatalf("paneRead: %v", err)
	}
	if !strings.Contains(screen, answer) {
		t.Fatalf("startup nudge never produced a turn (no %q in screen); screen:\n%s", answer, screen)
	}
}

// turnCompletionBudget bounds the wait for the startup turn to finish. It is a
// safety deadline, not the expected duration (TESTING.md deadline rule).
const turnCompletionBudget = 120 * time.Second

// TestDeliverNudgeConfirmsSubmitOnUnregisteredPane covers the other half of
// delivery: a pane herdr has not classified as an agent (a raw `exec /bin/sh
// -c` session, a bare shell, or an agent still mid-registration — ephemeral
// wisps spend a window there). That path cannot use `agent prompt`, and firing
// a single Enter without confirming leaves the text pasted-but-unsubmitted,
// which is the same stranded-turn defect with a human required to press Enter.
//
// This is a guard, not a reproduction: it passes against the unconfirmed path
// too, because a bare shell commits a paste immediately and has no TUI
// paste/submit race to lose. The stranding was observed in practice on panes
// mid-registration, which is inherently timing-dependent; what this pins is
// that confirming the submit does not break ordinary delivery.
//
// Live: needs herdr ≥0.7.5. Skipped in -short.
func TestDeliverNudgeConfirmsSubmitOnUnregisteredPane(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live herdr test in -short mode")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}

	session := fmt.Sprintf("gctest-fallback-%d", time.Now().UnixNano()%100000)
	c := newClient(session, t.TempDir())
	if err := c.startServer(); err != nil {
		t.Fatalf("startServer: %v", err)
	}
	t.Cleanup(func() { _ = c.stopServer() })

	ctx := context.Background()
	wsID, _, pane, err := c.workspaceCreate(ctx, "fallback-probe", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("workspaceCreate: %v", err)
	}
	if wsID == "" || pane == "" {
		t.Fatalf("workspaceCreate returned empty ids: workspace=%q pane=%q", wsID, pane)
	}

	// The pane holds a bare shell: herdr registers no agent for it, so delivery
	// takes the paste+confirm path. Proof of submission is the command's output,
	// which only exists if the shell actually ran it.
	const marker = "FALLBACK_SUBMITTED_OK"
	if err := c.deliverNudge(ctx, pane, "echo "+marker+"_DONE"); err != nil {
		t.Fatalf("deliverNudge on an unregistered pane never confirmed: %v", err)
	}
	screen, err := c.paneRead(ctx, pane, "visible", 40)
	if err != nil {
		t.Fatalf("paneRead: %v", err)
	}
	if !strings.Contains(screen, marker+"_DONE") {
		t.Fatalf("nudge was not submitted (no command output); screen:\n%s", screen)
	}
}
