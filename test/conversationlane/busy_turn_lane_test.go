//go:build integration

package conversationlane_test

// ga-8n8fja's end-to-end evidence: boundary prompts proposed while a Claude
// Code session runs a long turn wait for the turn to end, then reach it once
// each. The city runs real tmux under a socket root of its own, with the
// tmux configuration the controller and the CLI build in production (gc's
// tmuxConfigFromSession: no idle wait before a nudge types), and the named
// session is a claude-family provider whose agent is fakeclaude: a scripted
// Claude Code-like screen that redraws its spinner every second during a
// turn, queues what is submitted into a running turn under the "Press up to
// edit queued messages" hint drawn with the cursor on it, and records every
// event. A lane that reads the turn's boundary from the activity clock alone
// types into the turn (the pane's whole-second, cached activity reads 3 s
// quiet between redraws, and the lane comes back at the quiescence deadline
// it named); each such submit is then unconfirmed and retried.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/opjournal"
)

const (
	// fakeClaudeEnv names the fakeclaude binary ($(rootpath) in BUILD.bazel).
	fakeClaudeEnv = "GC_LANE_FAKE_CLAUDE"
	// fakeClaudeProvider is the city's claude-family provider running
	// fakeclaude.
	fakeClaudeProvider = "fakeclaude"
	// busyTurn is how long the session's long turn runs.
	busyTurn = 60 * time.Second
)

// claudeEvent is one line of fakeclaude's events.jsonl.
type claudeEvent struct {
	At    time.Time `json:"at"`
	Event string    `json:"event"`
	Text  string    `json:"text"`
	Turn  bool      `json:"turn"`
}

// prepareClaudeTUI gives a claude-TUI city its tmux socket root and the
// launcher of its session's agent: fakeclaude, executed under the name
// claude (the claude family's process name, which gc's process probes look
// for), with the city's state dir for its control file and event log.
func (c *laneCity) prepareClaudeTUI() {
	c.t.Helper()
	c.prepareTmux()
	bin, err := filepath.Abs(bazeltest.DataPath(c.t, fakeClaudeEnv))
	if err != nil || bin == "" {
		c.t.Fatalf("locating fakeclaude (%s): %q, %v", fakeClaudeEnv, bin, err)
	}
	named := filepath.Join(c.root, "claude-bin", "claude")
	if err := os.MkdirAll(filepath.Dir(named), 0o755); err != nil {
		c.t.Fatalf("creating %s: %v", filepath.Dir(named), err)
	}
	if err := os.Symlink(bin, named); err != nil {
		c.t.Fatalf("naming fakeclaude claude: %v", err)
	}
	c.writeFile(c.claudeLauncher(), "#!/bin/sh\nexec "+shellQuote(named)+" --state "+shellQuote(c.claudeState())+" \"$@\"\n", 0o755)
}

// claudeLauncher is the claude-TUI provider's command.
func (c *laneCity) claudeLauncher() string { return filepath.Join(c.dir, ".gc", "fake-claude.sh") }

// claudeState is fakeclaude's state dir.
func (c *laneCity) claudeState() string { return filepath.Join(c.state, "claude") }

// claudeProviderTOML is the claude-family provider the named session runs:
// the builtin claude provider with fakeclaude for its command, no prompt
// argument and no bypass-permissions warning to accept.
func (c *laneCity) claudeProviderTOML() string {
	return fmt.Sprintf("\n[providers.%s]\nbase = \"builtin:claude\"\ncommand = %q\nprompt_mode = \"none\"\nemits_permission_warning = false\n",
		fakeClaudeProvider, c.claudeLauncher())
}

// claudeEvents reads fakeclaude's event log.
func (c *laneCity) claudeEvents() []claudeEvent {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.claudeState(), "events.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		c.t.Fatalf("reading fakeclaude's event log: %v", err)
	}
	var out []claudeEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e claudeEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			c.t.Fatalf("fakeclaude event %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// waitClaudeEvent waits for fakeclaude's first event named name after
// skipping the first `after` events, and returns it.
func (c *laneCity) waitClaudeEvent(name string, after int, timeout time.Duration) (claudeEvent, int) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		evs := c.claudeEvents()
		for i := after; i < len(evs); i++ {
			if evs[i].Event == name {
				return evs[i], i
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("fakeclaude recorded no %s within %s; it recorded %+v", name, timeout, evs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestBoundaryPromptsWaitOutABusyClaudeTurn: gc session nudge (wait-idle: a
// boundary prompt, at_least_once) three times while the session runs a
// 60 s turn. Nothing reaches the session while the turn runs: no keystroke,
// no queued message, no Escape. After the turn every prompt is submitted
// exactly once, and every op closes succeeded on its first attempt with
// nothing maybe delivered.
func TestBoundaryPromptsWaitOutABusyClaudeTurn(t *testing.T) {
	parallelCity(t)
	c := newLaneCityWith(t, armSQLite, activityNone, cityOptions{claudeTUI: true})
	c.startController("")
	_, ready := c.waitClaudeEvent("ready", 0, 2*time.Minute)

	c.writeFile(filepath.Join(c.claudeState(), "turn"), fmt.Sprintf("%g\n", busyTurn.Seconds()), 0o644)
	start, startAt := c.waitClaudeEvent("turn_start", ready, 30*time.Second)

	payloads := []string{
		"busy-turn blocker gc-1 closed — routed dependent gc-2 may be ready to claim",
		"busy-turn blocker gc-3 closed — routed dependent gc-4 may be ready to claim",
		"busy-turn blocker gc-5 closed — routed dependent gc-6 may be ready to claim",
	}
	offsets := []time.Duration{3 * time.Second, 20 * time.Second, 40 * time.Second}
	ids := make([]string, len(payloads))
	var lastProposed time.Time
	for i, payload := range payloads {
		time.Sleep(time.Until(start.At.Add(offsets[i])))
		key := fmt.Sprintf("busy-turn-%d", i)
		if r := c.nudge("wait-idle", key, payload); r.code() != 0 {
			t.Fatalf("gc session nudge %s: exit %d\nstdout: %s\nstderr: %s", key, r.code(), r.stdout, r.stderr)
		}
		lastProposed = time.Now()
		ids[i] = c.cliOpID(key)
	}

	end, _ := c.waitClaudeEvent("turn_end", startAt, busyTurn+30*time.Second)
	if !lastProposed.Before(end.At) {
		t.Fatalf("the last prompt was proposed at %s, after the turn ended at %s: the case did not run", lastProposed, end.At)
	}
	ops := c.waitClosed(3*time.Minute, ids...)
	time.Sleep(settleWindow)

	evs := c.claudeEvents()
	for _, e := range evs {
		switch e.Event {
		case "queued", "input_during_turn", "escape":
			t.Errorf("fakeclaude recorded %s at %s (turn %v): %q", e.Event, e.At.Format(time.RFC3339Nano), e.Turn, e.Text)
		}
	}
	for i, payload := range payloads {
		var submits []claudeEvent
		for _, e := range evs {
			if e.Event == "submit" && strings.Contains(e.Text, payload) {
				submits = append(submits, e)
			}
		}
		if len(submits) != 1 {
			t.Errorf("prompt %d was submitted %d times, want once: %+v", i, len(submits), submits)
			continue
		}
		if !submits[0].At.After(end.At) {
			t.Errorf("prompt %d was submitted at %s, before the turn ended at %s", i, submits[0].At.Format(time.RFC3339Nano), end.At.Format(time.RFC3339Nano))
		}
	}
	for i, op := range ops {
		arm, reason := op.Outcome.Effective()
		if arm != opjournal.ArmSucceeded || op.Attempt != 1 || op.MaybeDelivered != 0 || op.Failures != 0 {
			t.Errorf("op %d %s: %s{%s} attempt %d maybe_delivered %d failures %d; want succeeded on its first attempt, nothing maybe delivered",
				i, op.ID, arm, reason, op.Attempt, op.MaybeDelivered, op.Failures)
		}
	}
	t.Logf("fakeclaude events:\n%s", readFile(filepath.Join(c.claudeState(), "events.jsonl")))
}
