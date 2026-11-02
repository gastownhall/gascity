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

// compactLiveRemoveDirAction removes db's directory from the data dir.
func compactLiveRemoveDirAction(fixture compactScriptFixture, db string) string {
	return "rm -rf " + shellQuote(filepath.Join(fixture.dataDir, db))
}

// compactLiveUnserveAction makes the fake server refuse db: state is
// "forever" or the number of refusals before it serves db again.
func compactLiveUnserveAction(fixture compactScriptFixture, db, state string) string {
	dir := filepath.Join(fixture.binDir, "live-unserved")
	return fmt.Sprintf("mkdir -p %s && printf '%%s\\n' %s > %s",
		shellQuote(dir), shellQuote(state), shellQuote(filepath.Join(dir, db)))
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
	installCompactLiveFault(t, fixture, "DOLT_RESET", compactLiveRemoveDirAction(fixture, "beads"))

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
	installCompactLiveFault(t, fixture, "DOLT_GC", compactLiveRemoveDirAction(fixture, "beads"))

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
			installCompactLiveFault(t, fixture, tt.trigger, compactLiveUnserveAction(fixture, "beads", "forever"))

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
			installCompactLiveFault(t, fixture, tt.trigger, compactLiveUnserveAction(fixture, "beads", "2"))

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
