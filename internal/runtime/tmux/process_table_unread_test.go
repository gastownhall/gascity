//go:build linux || darwin

package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// A held session whose pane runs something other than an agent name (the
// gc-osc7501 relay, a version-as-argv[0] Claude) is judged by the process
// table. When that table cannot be read (the scan overran fetchTimeout, or a
// native store open held the environment lock past it) the agent is
// unverified, not dead: Start refuses the name as for a live agent, carrying
// the runtime.ErrRuntimeUnavailable cause, logs the failed read, and kills and
// recreates nothing. Kills reading an unread process table as a zombie.
func TestStartHeldNameWithUnreadProcessTableKillsNothing(t *testing.T) {
	confineStagedFiles(t)
	readsSince := shimProcessTableTools(t, true)
	fx := &heldNameTmux{
		createErr:   ErrSessionExists,
		paneCommand: programstatus.CompanionName,
		panePID:     "999999999",
		liveToken:   heldNameToken,
	}
	p := newHeldNameProvider(fx)
	var logged []string
	p.tm.snapshotFailures.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	err := p.Start(context.Background(), "gc-test", heldNameConfig(false, []string{"claude"}))

	t.Logf("tmux calls: %q", fx.calls)
	if !errors.Is(err, runtime.ErrSessionExists) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("Start = %v, want runtime.ErrSessionExists carrying runtime.ErrRuntimeUnavailable", err)
	}
	if reads := readsSince(); len(reads) == 0 {
		t.Fatal("Start never read the process table, so it never judged the held agent by it")
	}
	if kills := tmuxVerbCount(fx.calls, "kill-session"); kills != 0 {
		t.Errorf("Start issued %d kill-session over an agent it could not see, want 0", kills)
	}
	if creates := tmuxVerbCount(fx.calls, "start-server") + tmuxVerbCount(fx.calls, "new-session"); creates != 1 {
		t.Errorf("Start issued %d creates, want only the one that found the name held", creates)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "process snapshot failed") {
		t.Errorf("logged %q, want the failed process-table read logged", logged)
	}
}

// While the process table cannot be read, a wrapped pane's agent cannot be
// seen. The startup wait keeps looking, reading the table on every look, and
// times out if it never answers, rather than taking the unread table for the
// agent; the failures are logged once. The zombie detectors (IsRuntimeRunning,
// IsAgentAlive, CheckSessionHealth) never read the unverified agent as dead.
func TestWaitForCommandWaitsWhileProcessTableUnread(t *testing.T) {
	readsSince := shimProcessTableTools(t, true)
	tm := NewTmux()
	tm.exec = &wrappedWaitExecutor{paneCommand: programstatus.CompanionName, deadPID: "999999999", livePID: "999999999"}
	var logged []string
	tm.snapshotFailures.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	if err := tm.WaitForCommand(context.Background(), "gc-test-relay", supportedShells, time.Second); err == nil {
		t.Fatal("WaitForCommand = nil over an unread process table, want a timeout: no agent was seen")
	}
	if reads := readsSince(); len(reads) < 2 {
		t.Fatalf("WaitForCommand read the process table %d times %q, want a read on every look", len(reads), reads)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "process snapshot failed") {
		t.Fatalf("logged %q, want one line for every failed read in the wait", logged)
	}
	if !tm.IsRuntimeRunning("gc-test-relay", []string{"sleep"}) || !tm.IsAgentAlive("gc-test-relay") {
		t.Error("IsRuntimeRunning or IsAgentAlive read the unverified agent as dead, want it alive")
	}
	if got := tm.CheckSessionHealth("gc-test-relay", 0); got != SessionHealthy {
		t.Errorf("CheckSessionHealth = %v over an unread process table, want %v", got, SessionHealthy)
	}
}

// Kills a log line per failed process-table read, and a count of unlogged
// failures that is lost or never reset. The first failure logs; later ones
// within processSnapshotFailureLogEvery are counted and reported by the next
// line.
func TestProcessSnapshotFailureLogRateLimited(t *testing.T) {
	var logged []string
	l := &processSnapshotFailureLog{logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }}
	rewind := func() { l.at = l.at.Add(-processSnapshotFailureLogEvery) }
	failure := errors.New("fetching process snapshot: signal: killed")

	for range 3 {
		l.note("process snapshot failed", failure)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "tmux: process snapshot failed: ") || !strings.Contains(logged[0], "signal: killed") || strings.Contains(logged[0], "more failures") {
		t.Fatalf("logged %q, want one line naming the read and its failure", logged)
	}
	rewind()
	l.note("process snapshot failed", failure)
	if len(logged) != 2 || !strings.Contains(logged[1], "2 more failures since") {
		t.Fatalf("logged %q, want a second line counting the 2 unlogged failures", logged)
	}
	rewind()
	l.note("process snapshot failed", failure)
	if len(logged) != 3 || strings.Contains(logged[2], "more failures") {
		t.Fatalf("logged %q, want a third line with the count reset", logged)
	}
}
