// Package herdrtest holds the opt-in gate shared by every test tier that
// drives a real herdr server. The gate lives outside package herdr because
// those tiers live in two packages: the provider's own journeys under
// internal/runtime/herdr, and the controller's event-driven liveness journeys
// under cmd/gc. One predicate in one place keeps them from diverging into two
// different answers to "should this run here?".
package herdrtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// SkipReason reports why the live herdr tier should be skipped, or "" when it
// should run. It is pure so the gating decision itself is testable without a
// t.Skip that unwinds the calling goroutine.
func SkipReason(short, herdrInstalled bool, fastUnit, liveTests string) string {
	if short {
		return "skipping live herdr test in -short mode"
	}
	if !herdrInstalled {
		return "herdr not installed"
	}
	if strings.TrimSpace(liveTests) == "1" {
		return ""
	}
	if strings.TrimSpace(fastUnit) == "0" {
		return ""
	}
	return "skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`"
}

// RequireLive gates a live herdr journey: one that places panes, forces agent
// status reports, bounces the server, or asserts on the wire event stream.
//
// Presence of the binary is not the precondition these tests actually need.
// What they need is a herdr whose behavior matches the contract they assert,
// and that is not something a guard can probe cheaply: herdr 0.8.0 made the
// agent registry detection-based, so a plain shell pane is never registered and
// agent lookups correctly report not-found. Gating on the binary alone made the
// result depend on which herdr happened to be installed, and since CI has no
// herdr, a version bump turns every local `make test` red while CI stays green.
//
// So this tier is opt-in. `make test` runs ./... with GC_FAST_UNIT=1 and no
// -short, and TESTING.md places live journeys in explicit profile lanes rather
// than the fast unit sweep. `make test-herdr-live` is the lane that runs them;
// scripts/test-integration-shard also sets GC_FAST_UNIT=0.
func RequireLive(t *testing.T) {
	t.Helper()
	_, err := exec.LookPath("herdr")
	if reason := SkipReason(
		testing.Short(),
		err == nil,
		os.Getenv("GC_FAST_UNIT"),
		os.Getenv("GC_HERDR_LIVE_TESTS"),
	); reason != "" {
		t.Skip(reason)
	}
}

// Poll waits for cond to hold at the live herdr boundary. A real herdr server
// exposes no completion signal for "the pane binding landed" or "the agent
// registered", so this is the one shape of waiting the live tier needs: a
// context-aware ticker with a bound, reporting the last observation it saw
// rather than a bare timeout. It is the single owner of that pattern for the
// live tier, so no test open-codes a poll loop of its own, and no test waits a
// fixed duration hoping the boundary caught up.
func Poll(t *testing.T, what string, bound time.Duration, cond func() (bool, string)) {
	t.Helper()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(bound)
	for {
		ok, observed := cond()
		if ok {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			if ok, _ := cond(); ok {
				return
			}
			t.Fatalf("%s did not happen within %v; last observed: %s", what, bound, observed)
		}
	}
}

// ReportAgent forces an agent status through herdr's report-agent API, retrying
// until the pane binding exists (pane resolves it, returning "" until it does).
// report-agent sets only the pane's agent label, which is a kind and never an
// identity; the event stream builds its pane-to-session map from registered
// names, so each report is followed by `agent rename <pane> <name>` to register
// the gc session name on the pane. herdr refuses that rename on a pane with no
// label and clears the name when a later report changes the label, so --agent
// carries the same gc session name on every call.
//
// The subprocess lives here rather than in each live test so the live tier has
// one place that shells out to herdr.
func ReportAgent(t *testing.T, herdrSession, agentName, state string, pane func() string) {
	t.Helper()
	var last string
	Poll(t, fmt.Sprintf("forcing agent %q to state %q", agentName, state), 10*time.Second, func() (bool, string) {
		paneID := pane()
		if paneID == "" {
			last = fmt.Sprintf("no pane binding recorded for %q yet", agentName)
			return false, last
		}
		if err := reportAndRegister(herdrSession, paneID, agentName, state); err != nil {
			last = err.Error()
			return false, last
		}
		return true, ""
	})
}

// ReportAgentBestEffort is ReportAgent without test-fatal semantics, safe to
// call from a helper goroutine where a t.Fatalf would be lost.
func ReportAgentBestEffort(herdrSession, agentName, state string, pane func() string) {
	paneID := pane()
	if paneID == "" {
		return
	}
	_ = reportAndRegister(herdrSession, paneID, agentName, state)
}

// reportAndRegister reports state onto the pane under the agentName label, then
// registers agentName as the pane's agent name (see ReportAgent for why both).
func reportAndRegister(herdrSession, paneID, agentName, state string) error {
	ctx, cancel := context.WithTimeout(context.Background(), reportAgentAttemptBound)
	defer cancel()
	if out, err := reportAgentCmd(ctx, herdrSession, paneID, agentName, state).CombinedOutput(); err != nil {
		return fmt.Errorf("pane report-agent %s %s: %w: %s", paneID, state, err, out)
	}
	if out, err := exec.CommandContext(ctx, "herdr", "--session", herdrSession, "agent", "rename", paneID, agentName).CombinedOutput(); err != nil {
		return fmt.Errorf("agent rename %s %s: %w: %s", paneID, agentName, err, out)
	}
	return nil
}

// reportAgentAttemptBound bounds ONE report-agent invocation. Without it the
// subprocess is unbounded, and an unbounded call inside Poll's condition means
// Poll's own bound is never reached: the deadline case cannot be selected while
// the condition is still running, so a hung herdr would hold the journey until
// the outer test timeout and report the wrong failure. It is well under the
// 10s poll bound so several attempts still fit inside one Poll.
const reportAgentAttemptBound = 2 * time.Second

func reportAgentCmd(ctx context.Context, herdrSession, paneID, agentName, state string) *exec.Cmd {
	return exec.CommandContext(ctx, "herdr", "--session", herdrSession, "pane", "report-agent", paneID,
		"--source", "gctest", "--agent", agentName, "--state", state)
}
