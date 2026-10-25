package dolt_test

import (
	"os"
	"strings"
	"testing"
)

// Fake-Dolt coverage for ga-skyxjl.3: every CALL DOLT_GC the compactor issues
// (bare, --full after a flatten, pending-GC retry, --gc-only) first probes the
// server for live sessions on that database and skips, fail-closed, when any
// are connected. The probe connects without --use-db so its own connection is
// not counted in the processlist. --force overrides the gate.

const (
	// quiescenceProbeLogLine is the dolt.log line of the beads probe: empty
	// db= because the probe selects no database.
	quiescenceProbeLogLine = "db= query=SELECT COUNT(*) FROM information_schema.processlist WHERE db = 'beads'"
	quiescenceBareGC       = "CALL DOLT_GC()"
	quiescenceFullGC       = "CALL DOLT_GC('--full')"
)

func assertQuiescenceSkipped(t *testing.T, out, log string) {
	t.Helper()
	if strings.Contains(log, "CALL DOLT_GC") {
		t.Fatalf("GC ran against a database with live sessions:\n%s", log)
	}
	if !strings.Contains(log, quiescenceProbeLogLine) {
		t.Fatalf("GC was skipped without probing live sessions on the beads database:\n%s", log)
	}
	if !strings.Contains(out, "db=beads") || !strings.Contains(out, "live session") || !strings.Contains(out, "--force") {
		t.Fatalf("skip must name the database, say why, and point at --force:\n%s", out)
	}
}

func TestCompactScriptBareGCSkipsDatabaseWithLiveSessions(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_BARE_GC=1", "GC_FAKE_DOLT_LIVE_SESSIONS=beads=2")
	if err != nil {
		t.Fatalf("a skipped bare GC is routine, not a failure: %v\n%s", err, out)
	}
	assertQuiescenceSkipped(t, out, readCompactDoltLog(t, fixture))
}

func TestCompactScriptBareGCProbesBeforeGCWhenQuiescent(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	// Sessions on another database must not block this one.
	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_BARE_GC=1", "GC_FAKE_DOLT_LIVE_SESSIONS=other=3")
	if err != nil {
		t.Fatalf("quiescent bare GC failed: %v\n%s", err, out)
	}
	log := readCompactDoltLog(t, fixture)
	probe, gc := strings.Index(log, quiescenceProbeLogLine), strings.Index(log, quiescenceBareGC)
	if probe < 0 || gc < 0 || probe > gc {
		t.Fatalf("live-session probe must run before CALL DOLT_GC() (probe at %d, gc at %d):\n%s", probe, gc, log)
	}
}

func TestCompactScriptGCOnlySkipsDatabaseWithLiveSessions(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.runWithArgs(t, "below_threshold", []string{"--gc-only"},
		"GC_FAKE_DOLT_LIVE_SESSIONS=beads=1")
	if err == nil {
		t.Fatalf("an operator-requested reclaim that was skipped must exit non-zero:\n%s", out)
	}
	assertQuiescenceSkipped(t, out, readCompactDoltLog(t, fixture))
}

func TestCompactScriptPendingGCRetrySkipsAndKeepsMarker(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	marker := writeCompactStateMarker(t, fixture, "compact-pending-gc",
		"reason=writer race during flatten deferred full GC\nremote=origin\nexpected_remote_head=headcommit\nexpected_remote_head_verified=1\ncompacted_from_head=headcommit\n")
	out, err := fixture.run(t, "remote_success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_FAKE_DOLT_LIVE_SESSIONS=beads=1")
	if err == nil {
		t.Fatalf("a pending-GC retry that could not run must not report success:\n%s", out)
	}
	assertQuiescenceSkipped(t, out, readCompactDoltLog(t, fixture))
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("pending-GC marker must survive a skipped retry: %v", statErr)
	}
}

func TestCompactScriptFlattenDefersFullGCAsPendingWhenLiveSessions(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_FAKE_DOLT_LIVE_SESSIONS=beads=1")
	if err == nil {
		t.Fatalf("a flatten whose full GC was skipped must not report success:\n%s", out)
	}
	assertQuiescenceSkipped(t, out, readCompactDoltLog(t, fixture))
	if _, statErr := os.Stat(compactStateMarkerPath(fixture, "compact-pending-gc")); statErr != nil {
		t.Fatalf("skipped full GC after a flatten must leave a pending-GC marker for the next run: %v", statErr)
	}
}

func TestCompactScriptGCGateFailsClosedWhenProbeFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  []string
	}{
		{name: "bare-gc", env: []string{"GC_DOLT_COMPACT_BARE_GC=1"}},
		{name: "gc-only", args: []string{"--gc-only"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, _ := fixture.runWithArgs(t, "below_threshold", tc.args,
				append(tc.env, "GC_FAKE_DOLT_PROBE_FAIL=1")...)
			log := readCompactDoltLog(t, fixture)
			if strings.Contains(log, "CALL DOLT_GC") {
				t.Fatalf("an unconfirmed probe must be treated as not quiescent, but GC ran:\n%s", log)
			}
			if !strings.Contains(out, "db=beads") || !strings.Contains(out, "live-session probe failed") {
				t.Fatalf("probe failure must be reported against the database:\n%s", out)
			}
		})
	}
}

func TestCompactScriptForceOverridesLiveSessionGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{name: "bare-gc", args: []string{"--force"}, env: []string{"GC_DOLT_COMPACT_BARE_GC=1"}, want: quiescenceBareGC},
		{name: "gc-only", args: []string{"--gc-only", "--force"}, want: quiescenceFullGC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.runWithArgs(t, "below_threshold", tc.args,
				append(tc.env, "GC_FAKE_DOLT_LIVE_SESSIONS=beads=4")...)
			if err != nil {
				t.Fatalf("--force run failed: %v\n%s", err, out)
			}
			if log := readCompactDoltLog(t, fixture); !strings.Contains(log, tc.want) {
				t.Fatalf("--force must let %s run despite live sessions:\n%s", tc.want, log)
			}
			if !strings.Contains(out, "db=beads") || !strings.Contains(out, "--force") {
				t.Fatalf("a forced GC must be logged as such:\n%s", out)
			}
		})
	}
}

func TestCompactScriptDryRunDoesNotProbeLiveSessions(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.runWithArgs(t, "success", []string{"--dry-run"},
		"GC_DOLT_COMPACT_BARE_GC=1", "GC_FAKE_DOLT_LIVE_SESSIONS=beads=2")
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	if log := readCompactDoltLog(t, fixture); strings.Contains(log, "processlist") {
		t.Fatalf("dry run mutates nothing and must not probe:\n%s", log)
	}
}
