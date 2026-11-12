package scripts_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// The helper that runs the worker-env scripts (runRBEScript) is a HANG
// DETECTOR: every assertion about a script's behavior comes after it returns,
// and a passing script never waits the budget out. See TESTING.md "Test
// deadline rule" for why such a ceiling is derived, not a latency assertion.
//
// Measured on ga-izzzy5t: a publish/fetch call takes 4-82 ms on an idle host
// and 14-397 ms with ~113 runnable processes on 32 cores, yet a pre-push
// scripts shard killed one with "signal: killed" at the old 30 s ceiling while
// two unrelated tests in the same shard (no shared command) each stalled for
// 89-105 s. The budget therefore has to clear a host stall and still report a
// wedged child before the shard's own alarm does.

const (
	// measuredHostStallCeiling is the longest stall of a trivial test seen in
	// the retained failing shard (TestGoTestObservableWritesDeterministicNormalizedTiming,
	// 105.08 s, in ga-izzzy5t's pm-intake evidence).
	measuredHostStallCeiling = 105 * time.Second
	// scriptsShardAlarm is the go-test timeout every //scripts shard runs
	// under; past it the whole shard dies with an unattributed alarm.
	scriptsShardAlarm = 300 * time.Second
)

func TestRBEScriptHangBudgetClearsAHostStallButReportsBeforeTheShardAlarm(t *testing.T) {
	if rbeScriptHangBudget <= measuredHostStallCeiling {
		t.Fatalf("rbeScriptHangBudget = %s, want more than the measured host stall %s: a stalled host would be reported as a wedged script", rbeScriptHangBudget, measuredHostStallCeiling)
	}
	if rbeScriptHangBudget >= scriptsShardAlarm {
		t.Fatalf("rbeScriptHangBudget = %s, want less than the shard alarm %s: a wedged script would take the shard down unattributed", rbeScriptHangBudget, scriptsShardAlarm)
	}
}

func TestRunRBEScriptWithinLetsASlowScriptFinish(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "slow.sh")
	writeExecutable(t, script, "#!/bin/sh\nsleep 0.3\necho finished \"$1\"\n")

	stdout, stderr, err := runRBEScriptWithin(testutil.ExecRaceTimeout, dir, nil, script, "arg")
	if err != nil {
		t.Fatalf("slow script = %v, stderr %q", err, stderr)
	}
	if !strings.Contains(stdout, "finished arg") {
		t.Fatalf("slow script stdout = %q, want it to reach its end", stdout)
	}
}

func TestRunRBEScriptWithinKillsAWedgedScriptAndNamesTheBudget(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "wedged.sh")
	writeExecutable(t, script, "#!/bin/sh\necho started >&2\nexec sleep 60\n")

	// The budget is the scenario here (a value fed to the system), so it stays
	// small; the 60 s child must be cut off far sooner than it would finish.
	const budget = 500 * time.Millisecond
	begin := time.Now()
	_, stderr, err := runRBEScriptWithin(budget, dir, nil, script)
	elapsed := time.Since(begin)

	if err == nil {
		t.Fatal("wedged script returned success, want it killed at the budget")
	}
	if elapsed >= testutil.ExecRaceTimeout {
		t.Fatalf("wedged script held the helper for %s, want it cut off near the %s budget", elapsed, budget)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wedged script error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	for _, want := range []string{"hang budget", budget.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("wedged script error = %q, want it to name %q instead of a bare signal", err, want)
		}
	}
	if !strings.Contains(stderr, "started") {
		t.Fatalf("wedged script stderr = %q, want the output so far kept for diagnosis", stderr)
	}
}
