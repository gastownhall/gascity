// Fake-Dolt coverage for the live-database gate: every storage-rewriting
// pass (flatten, full GC, bare GC, pending-GC retry, --gc-only) must end by
// proving the database is still served, quarantining the database and
// aborting the run when it is not, instead of reporting the pass as success
// and moving on to the next database.

package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireCompactExitCode fails the test unless compact exited with exactly
// the wanted status.
func requireCompactExitCode(t *testing.T, out string, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("compact exited 0, want %d:\n%s", want, out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("compact error is not an exit status: %v\n%s", err, out)
	}
	if exitErr.ExitCode() != want {
		t.Fatalf("compact exit code = %d, want %d:\n%s", exitErr.ExitCode(), want, out)
	}
}

// addCompactExtraDatabase registers one more discovered database directory
// in the fixture's data dir, so a run has a second (or third) database to
// reach after the first.
func addCompactExtraDatabase(t *testing.T, fixture compactScriptFixture, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, name, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir %s db dir: %v", name, err)
	}
}

// countLiveProbesForDB counts the served-HEAD probes the gate issued for db.
func countLiveProbesForDB(log, db string) int {
	return strings.Count(log, "db="+db+" query=SELECT CONCAT")
}

// readCompactMarkerBody reads a marker file compact wrote.
func readCompactMarkerBody(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read marker %s: %v", path, err)
	}
	return string(data)
}

// compactLiveGateMarkerPath is the integrity-quarantine marker path for beads.
func compactLiveGateMarkerPath(fixture compactScriptFixture) string {
	return compactStateMarkerPath(fixture, "compact-quarantine")
}

// freshCompactTimestamp renders a created_at value that is fresh now.
func freshCompactTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// TestCompactScriptRejectsInvalidLiveProbeSettings: an invalid live-probe
// setting must exit 2 with a validation diagnostic before the compaction
// lock is taken or any database is queried, while empty settings fall back
// to the defaults and valid values (including leading zeros) are accepted.
func TestCompactScriptRejectsInvalidLiveProbeSettings(t *testing.T) {
	invalid := []struct {
		name string
		env  string
		want string
	}{
		{name: "attempts zero", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=0", want: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=0"},
		{name: "attempts all zero", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=000", want: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=000"},
		{name: "attempts negative", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=-1", want: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=-1"},
		{name: "attempts non-numeric", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=abc", want: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=abc"},
		{name: "attempts fractional", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=1.5", want: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=1.5"},
		{name: "delay negative", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=-1", want: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=-1"},
		{name: "delay non-numeric", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=abc", want: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=abc"},
		{name: "delay fractional", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=1.5", want: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=1.5"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, "below_threshold", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", tc.env)
			requireCompactExitCode(t, out, err, 2)
			if !strings.Contains(out, "invalid "+tc.want) {
				t.Fatalf("validation diagnostic must name the setting:\n%s", out)
			}
			if log := readCompactDoltLog(t, fixture); log != "" {
				t.Fatalf("invalid settings must be rejected before any database query:\n%s", log)
			}
		})
	}

	valid := []struct {
		name string
		env  string
	}{
		{name: "attempts empty uses default", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS="},
		{name: "attempts leading zeros", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=007"},
		{name: "delay empty uses default", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS="},
		{name: "delay zero", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0"},
		{name: "delay leading zeros", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=007"},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, "below_threshold", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", tc.env)
			if err != nil {
				t.Fatalf("valid setting rejected: %v\n%s", err, out)
			}
		})
	}
}

// TestCompactScriptLiveGateQuarantinesVanishedDataDir: a rewrite that leaves
// the database out of the data dir — while SQL may even still answer — must
// be quarantined with the live-database reason, alerted through the existing
// quarantine event/mail path, and fail the run, for both entry shapes
// (flatten's post-rewrite full GC and --gc-only).
func TestCompactScriptLiveGateQuarantinesVanishedDataDir(t *testing.T) {
	t.Run("flatten full gc drops the data dir", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "live_vanish_after_gc",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
		requireCompactExitCode(t, out, err, 1)
		if !strings.Contains(out, "live database unavailable after compaction") {
			t.Fatalf("compact must report the live-database quarantine reason:\n%s", out)
		}

		marker := compactLiveGateMarkerPath(fixture)
		body := readCompactMarkerBody(t, marker)
		for _, want := range []string{
			"db=beads",
			"reason=live database unavailable after compaction",
			"live_gate_steps=flatten, post-flatten full GC",
			"live_gate_failed_step=post-flatten full GC",
			"live_gate_dir_present_before=1",
			"live_gate_dir_present_after=0",
			"live_gate_pre_compaction_head=headcommit",
			"decision=preserve_marker_manual_review_required",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("quarantine marker missing %q:\n%s\noutput:\n%s", want, body, out)
			}
		}

		assertCompactBeadsQuarantineAlert(t, fixture, "mayor", marker, "compact-quarantine",
			"live database unavailable after compaction")

		if _, statErr := os.Stat(compactStateMarkerPath(fixture, "compact-pending-gc")); !os.IsNotExist(statErr) {
			t.Fatalf("a failed live gate must not record pending-GC bookkeeping:\n%s", out)
		}
		if strings.Contains(out, "commits=600->600") && strings.Contains(out, "— ok") {
			t.Fatalf("the failed pass must not be reported as success:\n%s", out)
		}
	})

	t.Run("gc-only drops the data dir", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.runWithArgs(t, "live_vanish_after_gc", []string{"--gc-only"},
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
		requireCompactExitCode(t, out, err, 1)
		marker := compactLiveGateMarkerPath(fixture)
		body := readCompactMarkerBody(t, marker)
		for _, want := range []string{
			"reason=live database unavailable after compaction",
			"live_gate_failed_step=gc-only reclaim",
			"live_gate_dir_present_before=1",
			"live_gate_dir_present_after=0",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("quarantine marker missing %q:\n%s\noutput:\n%s", want, body, out)
			}
		}
	})
}

// TestCompactScriptLiveGateQuarantinesUnservedDatabase: when the data-dir
// entry survives but the server stops serving the database, the gate must
// quarantine and fail the run regardless of the rewrite statement's own exit
// status, across every rewrite shape.
func TestCompactScriptLiveGateQuarantinesUnservedDatabase(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		args       []string
		extraEnv   []string
		failedStep string
	}{
		{name: "after flatten", mode: "live_unserved_after_flatten", failedStep: "flatten"},
		{name: "after post-flatten full gc", mode: "live_unserved_after_gc", failedStep: "post-flatten full GC"},
		{name: "gc-only", mode: "live_unserved_after_gc", args: []string{"--gc-only"}, failedStep: "gc-only reclaim"},
		{name: "bare gc", mode: "live_unserved_after_gc", extraEnv: []string{"GC_DOLT_COMPACT_BARE_GC=1"}, failedStep: "bare GC"},
		{name: "failed flatten", mode: "live_flatten_failure_unserved", failedStep: "flatten"},
		{name: "failed gc still unserved", mode: "live_gc_failure_unserved", failedStep: "post-flatten full GC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			env := append([]string{
				"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
				"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
			}, tc.extraEnv...)
			out, err := fixture.runWithArgs(t, tc.mode, tc.args, env...)
			requireCompactExitCode(t, out, err, 1)
			if !strings.Contains(out, "database not found: beads") {
				t.Fatalf("output must carry the probe failure the server reported:\n%s", out)
			}

			body := readCompactMarkerBody(t, compactLiveGateMarkerPath(fixture))
			for _, want := range []string{
				"reason=live database unavailable after compaction",
				"live_gate_failed_step=" + tc.failedStep,
				"live_gate_probe_failure=",
				"live_gate_dir_present_after=1",
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("quarantine marker missing %q:\n%s\noutput:\n%s", want, body, out)
				}
			}
		})
	}
}

// TestCompactScriptLiveGateAbortsRunAndNamesDatabasesNotReached: after a
// failed gate the run stops at once — no statements against later databases,
// no success or retry bookkeeping — and the diagnostic names the selected
// databases that were not reached.
func TestCompactScriptLiveGateAbortsRunAndNamesDatabasesNotReached(t *testing.T) {
	t.Run("names the untouched database", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		addCompactExtraDatabase(t, fixture, "zed")
		out, err := fixture.run(t, "live_vanish_after_gc",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
		requireCompactExitCode(t, out, err, 1)
		if !strings.Contains(out, "aborting the run") {
			t.Fatalf("gate failure must abort the run:\n%s", out)
		}
		if !strings.Contains(out, "not reached after the abort: zed") {
			t.Fatalf("abort diagnostic must name the database not reached:\n%s", out)
		}
		log := readCompactDoltLog(t, fixture)
		if strings.Contains(log, "db=zed query=") {
			t.Fatalf("no statements may run against a database after the abort:\n%s", log)
		}
		if strings.Contains(out, "— ok") {
			t.Fatalf("no database may be reported as successful after the abort:\n%s", out)
		}
	})

	t.Run("respects only-dbs selection", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		addCompactExtraDatabase(t, fixture, "zed")
		addCompactExtraDatabase(t, fixture, "alpha")
		out, err := fixture.run(t, "live_vanish_after_gc",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
			"GC_DOLT_COMPACT_ONLY_DBS=beads,zed")
		requireCompactExitCode(t, out, err, 1)
		if !strings.Contains(out, "not reached after the abort: zed") {
			t.Fatalf("abort diagnostic must name the selected database not reached:\n%s", out)
		}
		if strings.Contains(out, "not reached after the abort: alpha") || strings.Contains(out, "zed alpha") {
			t.Fatalf("a database outside the selection is not \"not reached\":\n%s", out)
		}
	})

	t.Run("none left when the failure is the last database", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "live_vanish_after_gc",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
			"GC_DOLT_COMPACT_ONLY_DBS=beads")
		requireCompactExitCode(t, out, err, 1)
		if !strings.Contains(out, "not reached after the abort: none") {
			t.Fatalf("abort diagnostic must say no databases were left unreached:\n%s", out)
		}
	})
}

// TestCompactScriptLiveGateRetriesTransientProbeFailures: a probe failure
// that recovers within the configured attempts while the directory invariant
// holds is not a quarantine — the rewrite's own result decides the outcome —
// and healthy passes probe a bounded, predictable number of times.
func TestCompactScriptLiveGateRetriesTransientProbeFailures(t *testing.T) {
	t.Run("recovers within attempts", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		addCompactExtraDatabase(t, fixture, "zed")
		out, err := fixture.run(t, "live_probe_transient_once",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
		if err != nil {
			t.Fatalf("a transient probe failure must not fail the run: %v\n%s", err, out)
		}
		if !strings.Contains(out, "live-database probe recovered on attempt=2/3") {
			t.Fatalf("output must log the probe recovery:\n%s", out)
		}
		if !strings.Contains(out, "commits=600->600") || !strings.Contains(out, "— ok") {
			t.Fatalf("the rewrite result must stand:\n%s", out)
		}
		if _, statErr := os.Stat(compactLiveGateMarkerPath(fixture)); !os.IsNotExist(statErr) {
			t.Fatalf("a recovered probe failure must not quarantine:\n%s", out)
		}
		log := readCompactDoltLog(t, fixture)
		for _, db := range []string{"beads", "zed"} {
			if got := countLiveProbesForDB(log, db); got != 3 {
				t.Fatalf("db=%s live probes = %d, want 3 (flatten check + failed then recovered post-GC probe):\n%s", db, got, log)
			}
		}
	})

	t.Run("healthy flatten probes twice per database", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
		if err != nil {
			t.Fatalf("compact failed: %v\n%s", err, out)
		}
		if got := countLiveProbesForDB(readCompactDoltLog(t, fixture), "beads"); got != 2 {
			t.Fatalf("live probes = %d, want 2 (post-flatten check + post-GC check)", got)
		}
	})

	t.Run("healthy gc-only probes twice per database", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.runWithArgs(t, "success", []string{"--gc-only"})
		if err != nil {
			t.Fatalf("compact failed: %v\n%s", err, out)
		}
		if got := countLiveProbesForDB(readCompactDoltLog(t, fixture), "beads"); got != 2 {
			t.Fatalf("live probes = %d, want 2 (baseline + post-GC check)", got)
		}
	})

	t.Run("attempts 1 does not retry", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "live_probe_transient_once",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
			"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=1")
		requireCompactExitCode(t, out, err, 1)
		if _, statErr := os.Stat(compactLiveGateMarkerPath(fixture)); statErr != nil {
			t.Fatalf("an unrecovered probe failure must quarantine (stat err=%v):\n%s", statErr, out)
		}
	})
}

// TestCompactScriptLiveGateCoversEveryRewriteMode: every entry path that
// rewrites storage — --gc-only, bare GC via env, the shared-history bare GC,
// and both pending-GC retry shapes — runs the same gate, quarantines on
// failure, and aborts before any caller-side push or marker clearing.
func TestCompactScriptLiveGateCoversEveryRewriteMode(t *testing.T) {
	t.Run("shared-history bare gc", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "live_unserved_shared_history",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
		requireCompactExitCode(t, out, err, 1)
		body := readCompactMarkerBody(t, compactLiveGateMarkerPath(fixture))
		if !strings.Contains(body, "live_gate_failed_step=shared-history bare GC") {
			t.Fatalf("shared-history bare GC must run the gate:\n%s\noutput:\n%s", body, out)
		}
	})

	t.Run("shared-history pending-GC retry keeps its marker", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		pendingMarker := writeCompactStateMarker(t, fixture, "compact-pending-gc", strings.Join([]string{
			"db=beads",
			"reason=flatten succeeded but full GC failed",
			"created_at=" + freshCompactTimestamp(),
			"remote=origin",
			"expected_remote_head=headcommit",
			"expected_remote_head_verified=1",
			"compacted_from_head=headcommit",
			"local_branch=main",
			"remote_branch=main",
		}, "\n"))
		out, err := fixture.run(t, "live_unserved_shared_history",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
		requireCompactExitCode(t, out, err, 1)
		body := readCompactMarkerBody(t, compactLiveGateMarkerPath(fixture))
		if !strings.Contains(body, "live_gate_failed_step=shared-history pending-GC retry") {
			t.Fatalf("shared-history pending-GC retry must run the gate:\n%s\noutput:\n%s", body, out)
		}
		if _, statErr := os.Stat(pendingMarker); statErr != nil {
			t.Fatalf("a failed gate must not clear the pending-GC marker: %v\n%s", statErr, out)
		}
		if log := readCompactDoltLog(t, fixture); strings.Contains(log, "DOLT_PUSH") {
			t.Fatalf("no push may happen after a failed gate:\n%s", log)
		}
	})

	t.Run("pending-GC retry keeps its marker and skips the push", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		pendingMarker := writeCompactStateMarker(t, fixture, "compact-pending-gc", strings.Join([]string{
			"db=beads",
			"reason=flatten succeeded but full GC failed",
			"created_at=" + freshCompactTimestamp(),
			"compacted_from_head=headcommit",
			"local_branch=main",
			"remote_branch=main",
		}, "\n"))
		out, err := fixture.run(t, "live_unserved_after_gc",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
		requireCompactExitCode(t, out, err, 1)
		body := readCompactMarkerBody(t, compactLiveGateMarkerPath(fixture))
		if !strings.Contains(body, "live_gate_failed_step=pending-GC retry") {
			t.Fatalf("pending-GC retry must run the gate:\n%s\noutput:\n%s", body, out)
		}
		if _, statErr := os.Stat(pendingMarker); statErr != nil {
			t.Fatalf("a failed gate must not clear the pending-GC marker: %v\n%s", statErr, out)
		}
		if log := readCompactDoltLog(t, fixture); strings.Contains(log, "DOLT_PUSH") {
			t.Fatalf("no push may happen after a failed gate:\n%s", log)
		}
	})
}

// TestCompactScriptLiveGateSkipsProbingWithoutRewrite: passes that never
// prepare a rewrite — dry runs, threshold skips, disk-critical holds, an
// existing quarantine refusal, and a push-only pending-push retry — issue no
// live-gate probe and keep their existing skip outcome.
func TestCompactScriptLiveGateSkipsProbingWithoutRewrite(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		args        []string
		extraEnv    []string
		marker      string
		markerBody  string
		wantExit    int
		wantPushLog bool
	}{
		{
			name:     "flatten dry-run",
			mode:     "success",
			extraEnv: []string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_DOLT_COMPACT_DRY_RUN=1"},
			wantExit: 0,
		},
		{
			name:     "gc-only dry-run",
			mode:     "success",
			args:     []string{"--gc-only", "--dry-run"},
			wantExit: 0,
		},
		{
			name:     "below threshold",
			mode:     "below_threshold",
			extraEnv: []string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"},
			wantExit: 0,
		},
		{
			name:     "disk-critical hold",
			mode:     "success",
			extraEnv: []string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_FAKE_DF_MODE=df_critical"},
			wantExit: 0,
		},
		{
			name:       "existing quarantine refusal",
			mode:       "success",
			extraEnv:   []string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"},
			marker:     "compact-quarantine",
			markerBody: "db=beads\nreason=post-flatten HEAD probe failed\ncreated_at=2026-01-01T00:00:00Z\n",
			wantExit:   1,
		},
		{
			name: "push-only pending-push retry",
			mode: "remote_success",
			extraEnv: []string{
				"GC_DOLT_COMPACT_THRESHOLD_COMMITS=5000",
				"GC_DOLT_COMPACT_ALLOW_FEDERATED=1",
			},
			marker: "compact-pending-push",
			markerBody: strings.Join([]string{
				"db=beads",
				"reason=deferred remote push",
				"created_at=" + freshCompactTimestamp(),
				"remote=origin",
				"expected_remote_head=headcommit",
				"expected_remote_head_verified=1",
				"compacted_from_head=headcommit",
				"local_branch=main",
				"remote_branch=main",
			}, "\n"),
			wantExit:    0,
			wantPushLog: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			if tc.marker != "" {
				writeCompactStateMarker(t, fixture, tc.marker, tc.markerBody)
			}
			out, err := fixture.runWithArgs(t, tc.mode, tc.args, tc.extraEnv...)
			if tc.wantExit == 0 && err != nil {
				t.Fatalf("compact failed: %v\n%s", err, out)
			}
			if tc.wantExit != 0 {
				requireCompactExitCode(t, out, err, tc.wantExit)
			}
			log := readCompactDoltLog(t, fixture)
			if got := countLiveProbesForDB(log, "beads"); got != 0 {
				t.Fatalf("no rewrite prepared, but %d live probes ran:\n%s\noutput:\n%s", got, log, out)
			}
			if tc.wantPushLog && !strings.Contains(log, "DOLT_PUSH") {
				t.Fatalf("push-only retry must still push:\n%s\noutput:\n%s", log, out)
			}
		})
	}
}

// TestCompactScriptPreservesOrdinaryFailureBehaviorWhenDatabaseStaysLive: a
// rewrite failure on a database that remains live keeps its existing
// behavior — restoration diagnostics, a pending-GC marker for the deferred
// full GC, no quarantine, no run abort — and later databases still run.
