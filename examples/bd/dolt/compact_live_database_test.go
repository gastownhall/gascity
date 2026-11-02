package dolt_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const compactLiveReason = "live database unavailable after compaction"

// compactLiveFastProbe keeps live-gate retries from sleeping in tests.
const compactLiveFastProbe = "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0"

func compactQuarantineMarkerPath(fixture compactScriptFixture, db string) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", db)
}

func compactPendingGCMarkerPath(fixture compactScriptFixture, db string) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", db)
}

func addCompactDatabase(t *testing.T, fixture compactScriptFixture, db string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, db, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir %s database: %v", db, err)
	}
}

// assertCompactNothingAfterLiveGate fails if any statement follows the failed
// gate's final probe of the beads database.
func assertCompactNothingAfterLiveGate(t *testing.T, fixture compactScriptFixture) {
	t.Helper()
	lines := compactDoltLogLines(t, fixture)
	last := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "db=beads ") && strings.Contains(line, "SELECT CONCAT(DATABASE(), ':', HASHOF('HEAD'))") {
			last = i
		}
	}
	if last < 0 {
		t.Fatalf("the live gate never probed beads:\n%s", strings.Join(lines, "\n"))
	}
	if last != len(lines)-1 {
		t.Fatalf("no statement may follow a failed live gate:\n%s", strings.Join(lines[last:], "\n"))
	}
}

// installCompactLiveFault wraps the fake dolt so that, after a statement
// against the beads database whose text contains trigger returns (whatever its
// exit status), the shell action runs. The statement's own exit status is
// preserved.
func installCompactLiveFault(t *testing.T, fixture compactScriptFixture, trigger, action string) {
	t.Helper()
	doltPath := filepath.Join(fixture.binDir, "dolt")
	realPath := filepath.Join(fixture.binDir, "dolt.real")
	if _, err := os.Stat(realPath); os.IsNotExist(err) {
		if err := os.Rename(doltPath, realPath); err != nil {
			t.Fatalf("move fake dolt aside: %v", err)
		}
	}
	writeExecutable(t, doltPath, fmt.Sprintf(`#!/usr/bin/env bash
rc=0
%s "$@" || rc=$?
target=""
query=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --use-db) target="$2"; shift 2 ;;
    -q) query="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ "$target" = %s ]; then
  case "$query" in
    *%s*) %s ;;
  esac
fi
exit "$rc"
`, shellQuote(realPath), shellQuote("beads"), shellQuote(trigger), action))
}

// compactLiveRemoveDirAction removes the beads database from the data dir.
func compactLiveRemoveDirAction(fixture compactScriptFixture) string {
	return "rm -rf " + shellQuote(filepath.Join(fixture.dataDir, "beads"))
}

// compactLiveUnserveAction makes the fake server refuse the beads database:
// state is "forever" or the number of refusals before it serves beads again.
func compactLiveUnserveAction(fixture compactScriptFixture, state string) string {
	dir := filepath.Join(fixture.binDir, "live-unserved")
	return fmt.Sprintf("mkdir -p %s && printf '%%s\\n' %s > %s",
		shellQuote(dir), shellQuote(state), shellQuote(filepath.Join(dir, "beads")))
}

func compactDoltLogLines(t *testing.T, fixture compactScriptFixture) []string {
	t.Helper()
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// assertCompactLiveQuarantine checks the live-gate quarantine marker, its
// alert, and the step and data-dir evidence it records.
func assertCompactLiveQuarantine(t *testing.T, fixture compactScriptFixture, out, step, presentAfter string) {
	t.Helper()
	marker := compactQuarantineMarkerPath(fixture, "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("live-gate failure must write a quarantine marker: %v\n%s", err, out)
	}
	if reason := compactMarkerValue(t, marker, "reason"); reason != compactLiveReason {
		t.Fatalf("quarantine reason = %q, want %q", reason, compactLiveReason)
	}
	for key, want := range map[string]string{
		"live_gate_failed_step":             step,
		"live_gate_data_dir_present_before": "1",
		"live_gate_data_dir_present_after":  presentAfter,
		"live_gate_data_dir_entry":          filepath.Join(fixture.dataDir, "beads"),
		"live_gate_pre_compaction_head":     "headcommit",
		"decision":                          "preserve_marker_manual_review_required",
	} {
		if got := compactMarkerValue(t, marker, key); got != want {
			t.Fatalf("marker %s = %q, want %q\n%s", key, got, want, out)
		}
	}
	if !strings.Contains(compactMarkerValue(t, marker, "live_gate_steps"), step) {
		t.Fatalf("marker live_gate_steps must include %s", step)
	}
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", marker, "compact-quarantine", compactLiveReason)
}

// TestCompactScriptRejectsInvalidLiveProbeSettings asserts that a malformed
// live-probe setting stops compact with exit 2 before it takes the lock or
// sends a single statement to the server.
func TestCompactScriptRejectsInvalidLiveProbeSettings(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{name: "zero attempts", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=0", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=0"},
		{name: "all-zero attempts", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=000", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=000"},
		{name: "negative attempts", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=-1", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=-1"},
		{name: "non-numeric attempts", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=three", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=three"},
		{name: "overflowing attempts", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=99999999999999999999", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=99999999999999999999"},
		{name: "negative delay", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=-2", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=-2"},
		{name: "fractional delay", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=1.5", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=1.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", tt.env)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("compact must reject %s with exit 2: got err=%v\n%s", tt.env, err, out)
			}
			if !strings.Contains(out, "compact: "+tt.want) {
				t.Fatalf("output missing %q diagnostic:\n%s", tt.want, out)
			}
			if strings.Contains(out, "another compaction already running") {
				t.Fatalf("validation must run before the compaction lock:\n%s", out)
			}
			if data, err := os.ReadFile(fixture.doltLog); err == nil && len(data) > 0 {
				t.Fatalf("validation must fail before any database query, dolt log:\n%s", data)
			}
		})
	}
}

// TestCompactScriptAcceptsLiveProbeSettings asserts that empty settings fall
// back to the defaults and that leading zeros are read as decimal.
func TestCompactScriptAcceptsLiveProbeSettings(t *testing.T) {
	tests := []struct {
		name string
		env  []string
	}{
		{name: "empty uses defaults", env: []string{"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=", "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS="}},
		{name: "leading zeros are decimal", env: []string{"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=08", "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=00"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			env := append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"}, tt.env...)
			out, err := fixture.run(t, "below_threshold", env...)
			if err != nil {
				t.Fatalf("compact must accept %v: %v\n%s", tt.env, err, out)
			}
			if !strings.Contains(out, "below_threshold=500 — skip") {
				t.Fatalf("compact should reach the threshold skip:\n%s", out)
			}
		})
	}
}

// TestCompactScriptQuarantinesWhenFlattenRemovesDatabaseDirectory asserts
// that a database whose .dolt directory vanishes during the flatten rewrite is
// quarantined even though SQL still answers for it.
func TestCompactScriptQuarantinesWhenFlattenRemovesDatabaseDirectory(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	installCompactLiveFault(t, fixture, "DOLT_RESET", compactLiveRemoveDirAction(fixture))

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", compactLiveFastProbe)
	if err == nil {
		t.Fatalf("compact must fail when the database leaves the data dir:\n%s", out)
	}
	assertCompactLiveQuarantine(t, fixture, out, "flatten", "0")
	if !strings.Contains(out, compactLiveReason) {
		t.Fatalf("output should name the live-gate failure:\n%s", out)
	}
	for _, line := range compactDoltLogLines(t, fixture) {
		if strings.Contains(line, "DOLT_GC") {
			t.Fatalf("no GC may run on a database that left the data dir:\n%s", line)
		}
	}
}

// TestCompactScriptAbortsWhenDatabaseLeavesDataDirAfterGC asserts that a
// database removed from the data dir by its post-flatten full GC is
// quarantined and the run fails.
func TestCompactScriptAbortsWhenDatabaseLeavesDataDirAfterGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	installCompactLiveFault(t, fixture, "DOLT_GC", compactLiveRemoveDirAction(fixture))

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact must fail when GC removes the database from the data dir:\n%s", out)
	}
	assertCompactLiveQuarantine(t, fixture, out, "post-flatten-full-gc", "0")
	if strings.Contains(out, "commits=600->600 duration=") {
		t.Fatalf("a GC that lost the database must not report success:\n%s", out)
	}
}

// TestCompactScriptQuarantinesWhenRewriteLeavesDatabaseUnserved asserts that
// a database the server stops serving after a rewrite is quarantined with the
// probe failure recorded, whether the rewrite statement itself succeeded or
// failed.
func TestCompactScriptQuarantinesWhenRewriteLeavesDatabaseUnserved(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		trigger string
		step    string
	}{
		{name: "flatten succeeded", mode: "success", trigger: "DOLT_RESET", step: "flatten"},
		{name: "flatten failed", mode: "flatten_failure", trigger: "DOLT_RESET", step: "flatten"},
		{name: "full GC succeeded", mode: "success", trigger: "DOLT_GC", step: "post-flatten-full-gc"},
		{name: "full GC failed", mode: "gc_failure", trigger: "DOLT_GC", step: "post-flatten-full-gc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			installCompactLiveFault(t, fixture, tt.trigger, compactLiveUnserveAction(fixture, "forever"))

			out, err := fixture.run(t, tt.mode, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
				"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=2", compactLiveFastProbe)
			if err == nil {
				t.Fatalf("compact must fail when the rewritten database is not served:\n%s", out)
			}
			assertCompactLiveQuarantine(t, fixture, out, tt.step, "1")
			marker := compactQuarantineMarkerPath(fixture, "beads")
			if got := compactMarkerValue(t, marker, "live_gate_probe_failure"); !strings.Contains(got, "database not found: beads") {
				t.Fatalf("marker live_gate_probe_failure = %q, want the server's refusal", got)
			}
		})
	}
}

// TestCompactScriptLiveProbeRecoversWithinConfiguredAttempts asserts that a
// served-HEAD probe that fails transiently and recovers within the configured
// attempts writes no liveness quarantine, leaving the rewrite's own result to
// decide the outcome.
func TestCompactScriptLiveProbeRecoversWithinConfiguredAttempts(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		trigger string
		wantErr bool
	}{
		{name: "flatten and GC succeed", mode: "success", trigger: "DOLT_GC", wantErr: false},
		{name: "full GC fails", mode: "gc_failure", trigger: "DOLT_GC", wantErr: true},
		{name: "flatten fails", mode: "flatten_failure", trigger: "DOLT_RESET", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			installCompactLiveFault(t, fixture, tt.trigger, compactLiveUnserveAction(fixture, "2"))

			out, err := fixture.run(t, tt.mode, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
				"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=3", compactLiveFastProbe)
			if (err != nil) != tt.wantErr {
				t.Fatalf("compact err = %v, want error %v: the rewrite result must decide\n%s", err, tt.wantErr, out)
			}
			if _, statErr := os.Stat(compactQuarantineMarkerPath(fixture, "beads")); !os.IsNotExist(statErr) {
				t.Fatalf("a probe that recovers must not quarantine (stat err=%v):\n%s", statErr, out)
			}
			if strings.Contains(out, compactLiveReason) {
				t.Fatalf("a probe that recovers must not report a live-gate failure:\n%s", out)
			}
			probes := 0
			for _, line := range compactDoltLogLines(t, fixture) {
				if strings.Contains(line, "SELECT CONCAT(DATABASE(), ':', HASHOF('HEAD'))") {
					probes++
				}
			}
			if probes < 3 {
				t.Fatalf("the gate must retry the probe until it recovers, saw %d probes", probes)
			}
		})
	}
}

// TestCompactScriptLiveGateFailureAbortsRemainingDatabases asserts that a
// failed live gate stops the run: no later statement for that database or any
// other, no pending-GC bookkeeping, exit 1, and a diagnostic that names the
// selected databases not reached.
func TestCompactScriptLiveGateFailureAbortsRemainingDatabases(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	addCompactDatabase(t, fixture, "zed")
	addCompactDatabase(t, fixture, "zzz")
	installCompactLiveFault(t, fixture, "DOLT_GC", compactLiveRemoveDirAction(fixture))

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", compactLiveFastProbe,
		"GC_DOLT_COMPACT_ONLY_DBS=beads,zed")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("a failed live gate must exit 1: got err=%v\n%s", err, out)
	}
	assertCompactLiveQuarantine(t, fixture, out, "post-flatten-full-gc", "0")
	assertCompactNothingAfterLiveGate(t, fixture)
	for _, line := range compactDoltLogLines(t, fixture) {
		if strings.HasPrefix(line, "db=zed ") || strings.HasPrefix(line, "db=zzz ") {
			t.Fatalf("no database may be touched after a failed live gate:\n%s", line)
		}
	}
	if _, statErr := os.Stat(compactPendingGCMarkerPath(fixture, "beads")); !os.IsNotExist(statErr) {
		t.Fatalf("a failed live gate must not record pending-GC bookkeeping (stat err=%v)", statErr)
	}
	want := "compact: live-database gate failed for db=beads after post-flatten-full-gc — run aborted; selected databases not reached: zed"
	if !strings.Contains(out, want) {
		t.Fatalf("output missing abort diagnostic %q:\n%s", want, out)
	}
	if strings.Contains(out, "failed compaction") {
		t.Fatalf("an aborted run must not report an ordinary failure count:\n%s", out)
	}
}

// TestCompactScriptLiveGateAbortNamesNoUnreachedDatabase asserts that the
// abort diagnostic says "none" when the failing database was the last one.
func TestCompactScriptLiveGateAbortNamesNoUnreachedDatabase(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	installCompactLiveFault(t, fixture, "DOLT_RESET", compactLiveRemoveDirAction(fixture))

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", compactLiveFastProbe)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("a failed live gate must exit 1: got err=%v\n%s", err, out)
	}
	want := "compact: live-database gate failed for db=beads after flatten — run aborted; selected databases not reached: none"
	if !strings.Contains(out, want) {
		t.Fatalf("output missing abort diagnostic %q:\n%s", want, out)
	}
	assertCompactNothingAfterLiveGate(t, fixture)
}

// TestCompactScriptLiveGateCoversEveryGCEntryPath asserts that every full-GC
// and bare-GC entry path runs the same live gate: a GC that removes the
// database quarantines it and aborts the run before any caller-side push or
// marker clearing.
func TestCompactScriptLiveGateCoversEveryGCEntryPath(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		args          []string
		env           []string
		pendingGC     bool
		step          string
		statementText string
	}{
		{name: "gc-only reclaim", mode: "below_threshold", args: []string{"--gc-only"}, step: "gc-only-reclaim", statementText: "DOLT_GC('--full'"},
		{name: "pending-GC retry", mode: "below_threshold", pendingGC: true, step: "pending-gc-retry", statementText: "DOLT_GC('--full'"},
		{name: "shared-history pending-GC retry", mode: "remote_success", pendingGC: true, step: "shared-history-pending-gc-retry", statementText: "DOLT_GC('--full'"},
		{name: "bare GC", mode: "below_threshold", env: []string{"GC_DOLT_COMPACT_BARE_GC=1"}, step: "bare-gc", statementText: "DOLT_GC('--archive-level=0')"},
		{name: "shared-history bare GC", mode: "remote_success", step: "shared-history-bare-gc", statementText: "DOLT_GC('--archive-level=0')"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			addCompactDatabase(t, fixture, "zed")
			var pendingMarker string
			if tt.pendingGC {
				pendingMarker = writeCompactPendingGCMarker(t, fixture, "beads")
			}
			installCompactLiveFault(t, fixture, "DOLT_GC", compactLiveRemoveDirAction(fixture))

			env := append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", compactLiveFastProbe}, tt.env...)
			out, err := fixture.runWithArgs(t, tt.mode, tt.args, env...)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("a GC that loses the database must exit 1: got err=%v\n%s", err, out)
			}
			if !strings.Contains(strings.Join(compactDoltLogLines(t, fixture), "\n"), tt.statementText) {
				t.Fatalf("path did not run %s:\n%s", tt.statementText, out)
			}
			assertCompactLiveQuarantine(t, fixture, out, tt.step, "0")
			assertCompactNothingAfterLiveGate(t, fixture)
			want := "compact: live-database gate failed for db=beads after " + tt.step + " — run aborted; selected databases not reached: zed"
			if !strings.Contains(out, want) {
				t.Fatalf("output missing abort diagnostic %q:\n%s", want, out)
			}
			if pendingMarker != "" {
				if _, statErr := os.Stat(pendingMarker); statErr != nil {
					t.Fatalf("a failed live gate must keep the pending-GC marker: %v\n%s", statErr, out)
				}
			}
			if strings.Contains(out, "— ok") {
				t.Fatalf("a GC that lost the database must not report success:\n%s", out)
			}

			rerun, err := fixture.runWithArgs(t, "below_threshold", []string{"--gc-only", "--only-db", "beads"}, compactLiveFastProbe)
			if err == nil {
				t.Fatalf("a live-gate quarantine must refuse the next run:\n%s", rerun)
			}
			for _, want := range []string{
				"live-database evidence: live_gate_failed_step=" + tt.step,
				"live_gate_data_dir_present_after=0",
				"live-database recovery: restore the database from a backup or its remote",
			} {
				if !strings.Contains(rerun, want) {
					t.Fatalf("refusal output missing %q:\n%s", want, rerun)
				}
			}
		})
	}
}

// TestCompactScriptDryRunAndSkipIssueNoLiveProbe asserts that the live gate
// stays out of every pass that never reaches rewrite preparation: a dry run or
// a pre-rewrite skip issues no served-HEAD probe, writes no quarantine marker,
// and keeps its existing outcome even when the database would fail the probe.
func TestCompactScriptDryRunAndSkipIssueNoLiveProbe(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		args      []string
		env       []string
		pendingGC bool
		want      string
	}{
		{name: "dry-run flatten", mode: "success", env: []string{"GC_DOLT_COMPACT_DRY_RUN=1"}, want: "— dry-run (would flatten)"},
		{name: "below threshold skip", mode: "below_threshold", want: "below_threshold=500 — skip"},
		{name: "only-db skip", mode: "success", args: []string{"--only-db", "zed"}, want: "db=beads not in GC_DOLT_COMPACT_ONLY_DBS — skip"},
		{name: "dry-run gc-only", mode: "below_threshold", args: []string{"--gc-only", "--dry-run"}, want: "db=beads — dry-run (would reclaim via DOLT_GC --full)"},
		{name: "dry-run bare GC", mode: "below_threshold", env: []string{"GC_DOLT_COMPACT_BARE_GC=1", "GC_DOLT_COMPACT_DRY_RUN=1"}, want: "db=beads — dry-run (would bare GC)"},
		{name: "dry-run shared-history bare GC", mode: "remote_success", env: []string{"GC_DOLT_COMPACT_DRY_RUN=1"}, want: "db=beads — dry-run (would bare GC)"},
		{name: "dry-run pending-GC retry", mode: "below_threshold", env: []string{"GC_DOLT_COMPACT_DRY_RUN=1"}, pendingGC: true, want: "db=beads pending_gc=present — dry-run (would retry DOLT_GC --full)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			addCompactDatabase(t, fixture, "zed")
			var pendingMarker string
			if tt.pendingGC {
				pendingMarker = writeCompactPendingGCMarker(t, fixture, "beads")
			}
			unserved := filepath.Join(fixture.binDir, "live-unserved")
			if err := os.MkdirAll(unserved, 0o755); err != nil {
				t.Fatalf("mkdir live-unserved: %v", err)
			}
			if err := os.WriteFile(filepath.Join(unserved, "beads"), []byte("forever\n"), 0o644); err != nil {
				t.Fatalf("write live-unserved state: %v", err)
			}

			env := append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", compactLiveFastProbe}, tt.env...)
			out, err := fixture.runWithArgs(t, tt.mode, tt.args, env...)
			if err != nil {
				t.Fatalf("pass must keep its existing successful outcome: %v\n%s", err, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Fatalf("output missing existing outcome %q:\n%s", tt.want, out)
			}
			doltLog, err := os.ReadFile(fixture.doltLog)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read dolt log: %v", err)
			}
			for _, line := range strings.Split(string(doltLog), "\n") {
				if strings.HasPrefix(line, "db=beads ") && strings.Contains(line, "HASHOF('HEAD'))") {
					t.Fatalf("pass issued a live probe for beads: %s\n%s", line, out)
				}
			}
			if strings.Contains(out, "live probe") || strings.Contains(out, "live-gate") || strings.Contains(out, compactLiveReason) {
				t.Fatalf("pass reported live-gate activity:\n%s", out)
			}
			for _, db := range []string{"beads", "zed"} {
				if _, statErr := os.Stat(compactQuarantineMarkerPath(fixture, db)); !os.IsNotExist(statErr) {
					t.Fatalf("pass must not write a quarantine marker for %s (stat err=%v)\n%s", db, statErr, out)
				}
			}
			if pendingMarker != "" {
				if _, statErr := os.Stat(pendingMarker); statErr != nil {
					t.Fatalf("dry run must keep the pending-GC marker: %v\n%s", statErr, out)
				}
			}
		})
	}
}

// TestCompactScriptRewriteFailureOnLiveDatabaseKeepsExistingHandling asserts
// that a rewrite failure on a database that passes the live gate keeps the
// existing restoration, failure reporting, and pending-GC marker behavior, and
// that compact goes on to the next database instead of aborting the run.
func TestCompactScriptRewriteFailureOnLiveDatabaseKeepsExistingHandling(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		env           []string
		want          []string
		zedFailure    string
		summary       string
		wantPendingGC bool
	}{
		{
			name:       "flatten failure restores HEAD",
			mode:       "flatten_failure",
			want:       []string{"compact: db=beads flatten failed rc=44 — restoring pre-flatten HEAD=headcommit", "reset exploded"},
			zedFailure: "flatten failed rc=44",
			summary:    "compact: 2 database(s) failed compaction",
		},
		{
			name:          "post-flatten GC failure keeps pending-GC marker",
			mode:          "gc_failure",
			want:          []string{"gc exploded"},
			zedFailure:    "DOLT_GC failed rc=45",
			summary:       "compact: 2 database(s) failed compaction",
			wantPendingGC: true,
		},
		{
			name:       "bare GC failure",
			mode:       "gc_failure",
			env:        []string{"GC_DOLT_COMPACT_BARE_GC=1"},
			want:       []string{"compact: db=beads bare-gc failed rc=45", "gc exploded"},
			zedFailure: "bare-gc failed rc=45",
			summary:    "compact: 2 database(s) failed bare GC",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			addCompactDatabase(t, fixture, "zed")

			env := append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", compactLiveFastProbe}, tt.env...)
			out, err := fixture.run(t, tt.mode, env...)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("a rewrite failure must exit 1: got err=%v\n%s", err, out)
			}
			for _, want := range append(tt.want, tt.summary) {
				if !strings.Contains(out, want) {
					t.Fatalf("output missing existing failure report %q:\n%s", want, out)
				}
			}
			reachedZed := false
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "compact: db=zed ") && strings.Contains(line, tt.zedFailure) {
					reachedZed = true
				}
			}
			if !reachedZed {
				t.Fatalf("a live database's rewrite failure must not stop the run; zed never reported %q:\n%s", tt.zedFailure, out)
			}
			if strings.Contains(out, "run aborted") || strings.Contains(out, compactLiveReason) {
				t.Fatalf("a live database must not trip the live gate:\n%s", out)
			}
			for _, db := range []string{"beads", "zed"} {
				if _, statErr := os.Stat(compactQuarantineMarkerPath(fixture, db)); !os.IsNotExist(statErr) {
					t.Fatalf("a live database must not be quarantined: %s (stat err=%v)\n%s", db, statErr, out)
				}
				_, statErr := os.Stat(compactPendingGCMarkerPath(fixture, db))
				if tt.wantPendingGC && statErr != nil {
					t.Fatalf("GC failure must keep writing the pending-GC marker for %s: %v\n%s", db, statErr, out)
				}
				if !tt.wantPendingGC && !os.IsNotExist(statErr) {
					t.Fatalf("%s must not write a pending-GC marker for %s (stat err=%v)\n%s", tt.name, db, statErr, out)
				}
			}
		})
	}
}
