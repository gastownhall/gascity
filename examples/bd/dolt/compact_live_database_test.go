package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fake-Dolt coverage for the compactor's live-database gate: after every
// history rewrite (flatten, full GC, bare GC) the database must still own its
// data directory and still be served, or the run quarantines it and stops.

const liveGateReason = "live database unavailable after compaction"

const liveProbeQuery = "query=SELECT CONCAT(DATABASE(), ':', HASHOF('HEAD'))"

func liveGateEnv(extra ...string) []string {
	return append([]string{
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
	}, extra...)
}

func addCompactDatabases(t *testing.T, fixture compactScriptFixture, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(fixture.dataDir, name, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir dolt db %s: %v", name, err)
		}
	}
}

func beadsMarker(fixture compactScriptFixture, kind string) string {
	return compactStateMarkerPath(fixture, kind)
}

func beadsLiveProbes(log string) int {
	return strings.Count(log, "db=beads "+liveProbeQuery)
}

// doltLogEntries splits the fake dolt log into one entry per query; a
// multi-line query (the flatten transaction) stays one entry.
func doltLogEntries(log string) []string {
	var entries []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "db=") || len(entries) == 0 {
			entries = append(entries, line)
			continue
		}
		entries[len(entries)-1] += "\n" + line
	}
	return entries
}

// beadsLogAfter returns the dolt log entries after the first beads query
// containing marker, or fails when no such query exists.
func beadsLogAfter(t *testing.T, log, marker string) string {
	t.Helper()
	entries := doltLogEntries(log)
	for i, entry := range entries {
		if strings.HasPrefix(entry, "db=beads ") && strings.Contains(entry, marker) {
			return strings.Join(entries[i+1:], "\n")
		}
	}
	t.Fatalf("dolt log has no %q query on db=beads:\n%s", marker, log)
	return ""
}

// compactExitCode returns the exit status of a compact run that failed.
func compactExitCode(t *testing.T, err error, out string) int {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("compact exit = %v, want a nonzero exit\n%s", err, out)
	}
	return exitErr.ExitCode()
}

func requireFailedRun(t *testing.T, err error, out string) {
	t.Helper()
	if code := compactExitCode(t, err, out); code != 1 {
		t.Fatalf("compact exit = %d, want 1\n%s", code, out)
	}
}

// beadsReportedSuccess reports whether a beads pass printed its success line.
func beadsReportedSuccess(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "compact: db=beads ") && strings.HasSuffix(line, "— ok") {
			return true
		}
	}
	return false
}

func requireNoMarker(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		data, _ := os.ReadFile(path)
		t.Fatalf("unexpected marker %s (stat err=%v):\n%s", path, err, data)
	}
}

// requireLiveGateQuarantine checks the quarantine marker, its alert, and the
// run abort left by a live-gate failure on beads during step.
func requireLiveGateQuarantine(t *testing.T, fixture compactScriptFixture, out, step string) string {
	t.Helper()
	marker := beadsMarker(fixture, "compact-quarantine")
	if got := compactMarkerValue(t, marker, "reason"); got != liveGateReason {
		t.Fatalf("quarantine reason = %q, want %q\n%s", got, liveGateReason, out)
	}
	if got := compactMarkerValue(t, marker, "live_gate_failed_step"); got != step {
		t.Fatalf("live_gate_failed_step = %q, want %q", got, step)
	}
	if got := compactMarkerValue(t, marker, "live_gate_data_dir"); got != filepath.Join(fixture.dataDir, "beads", ".dolt") {
		t.Fatalf("live_gate_data_dir = %q", got)
	}
	if got := compactMarkerValue(t, marker, "decision"); got != "preserve_marker_restore_before_clearing" {
		t.Fatalf("decision = %q", got)
	}
	if !strings.Contains(compactMarkerValue(t, marker, "recovery"), "restore") {
		t.Fatalf("marker must carry restore-before-clear guidance")
	}
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", marker, "compact-quarantine", liveGateReason)
	if want := "compact: db=beads live-database gate failed after " + step + " — aborting run"; !strings.Contains(out, want) {
		t.Fatalf("output missing run abort diagnostic %q:\n%s", want, out)
	}
	return marker
}

// requireNotReached checks the run abort names exactly the selected databases
// the run never reached.
func requireNotReached(t *testing.T, out, step, notReached string) {
	t.Helper()
	want := "compact: db=beads live-database gate failed after " + step + " — aborting run; selected databases not reached: " + notReached
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("output missing run abort diagnostic %q:\n%s", want, out)
}

// AC-1: a data directory that disappears during a rewrite quarantines the
// database and fails the run even though SQL still answers.
func TestCompactScriptLiveGateQuarantinesVanishedDataDir(t *testing.T) {
	for _, tc := range []struct {
		name    string
		faultOn string
		step    string
	}{
		{name: "post-flatten full GC", faultOn: "DOLT_GC", step: "post-flatten full GC"},
		{name: "flatten", faultOn: "DOLT_RESET", step: "flatten"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			addCompactDatabases(t, fixture, "zed")
			out, err := fixture.run(t, "success", liveGateEnv(
				"GC_FAKE_DOLT_LIVE_FAULT=drop_dir",
				"GC_FAKE_DOLT_LIVE_FAULT_ON="+tc.faultOn,
			)...)
			requireFailedRun(t, err, out)
			marker := requireLiveGateQuarantine(t, fixture, out, tc.step)
			for key, want := range map[string]string{
				"live_gate_data_dir_before":     "present",
				"live_gate_data_dir_after":      "absent",
				"live_gate_pre_compaction_head": "headcommit",
			} {
				if got := compactMarkerValue(t, marker, key); got != want {
					t.Fatalf("%s = %q, want %q", key, got, want)
				}
			}
			if beadsReportedSuccess(out) {
				t.Fatalf("vanished database must not report compaction success:\n%s", out)
			}
			requireNoMarker(t, beadsMarker(fixture, "compact-pending-gc"))
			if after := beadsLogAfter(t, readCompactDoltLog(t, fixture), tc.faultOn); strings.TrimSpace(after) != "" {
				t.Fatalf("no statement may follow a vanished data directory:\n%s", after)
			}

			// A later run refuses the database and reports the live-gate
			// evidence with restore guidance; the reason never auto-clears.
			addCompactDatabases(t, fixture, "beads")
			rerun, err := fixture.run(t, "success", liveGateEnv()...)
			requireFailedRun(t, err, rerun)
			for _, want := range []string{
				"compact: db=beads live-gate evidence: live_gate_failed_step=" + tc.step,
				"live_gate_data_dir_after=absent",
				"restore it from its latest verified backup",
			} {
				if !strings.Contains(rerun, want) {
					t.Fatalf("rerun output missing %q:\n%s", want, rerun)
				}
			}
			if got := compactMarkerValue(t, marker, "reason"); got != liveGateReason {
				t.Fatalf("rerun replaced the live-gate quarantine reason with %q", got)
			}
		})
	}
}

// AC-2: a database that keeps its directory but stops being served is
// quarantined whatever the rewrite's own exit status was.
func TestCompactScriptLiveGateQuarantinesUnservedDatabase(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		faultOn  string
		step     string
		attempts string
	}{
		{name: "full GC succeeded", mode: "success", faultOn: "DOLT_GC", step: "post-flatten full GC", attempts: "3"},
		{name: "full GC failed", mode: "gc_failure", faultOn: "DOLT_GC", step: "post-flatten full GC", attempts: "2"},
		{name: "flatten succeeded", mode: "success", faultOn: "DOLT_RESET", step: "flatten", attempts: "3"},
		{name: "flatten failed", mode: "flatten_failure", faultOn: "DOLT_RESET", step: "flatten", attempts: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, tc.mode, liveGateEnv(
				"GC_FAKE_DOLT_LIVE_FAULT=unserve",
				"GC_FAKE_DOLT_LIVE_FAULT_ON="+tc.faultOn,
				"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS="+tc.attempts,
			)...)
			requireFailedRun(t, err, out)
			marker := requireLiveGateQuarantine(t, fixture, out, tc.step)
			if got := compactMarkerValue(t, marker, "live_gate_data_dir_after"); got != "present" {
				t.Fatalf("live_gate_data_dir_after = %q, want present", got)
			}
			if got := compactMarkerValue(t, marker, "live_gate_probe_failure"); !strings.Contains(got, "database not found") {
				t.Fatalf("live_gate_probe_failure = %q, want the probe error", got)
			}
			requireNoMarker(t, beadsMarker(fixture, "compact-pending-gc"))
			after := beadsLogAfter(t, readCompactDoltLog(t, fixture), tc.faultOn)
			if tc.mode == "flatten_failure" {
				// The restore attempt still runs before the gate.
				after = beadsLogAfter(t, after, "query=SELECT HASHOF('HEAD')")
			}
			var probes []string
			for _, entry := range doltLogEntries(strings.TrimSpace(after)) {
				if !strings.Contains(entry, liveProbeQuery) {
					t.Fatalf("only live probes may follow an unserved database, got %q:\n%s", entry, after)
				}
				probes = append(probes, entry)
			}
			if want, _ := strconv.Atoi(tc.attempts); len(probes) != want {
				t.Fatalf("live probes after the fault = %d, want %s:\n%s", len(probes), tc.attempts, after)
			}
		})
	}

	for _, tc := range []struct {
		reply string
		fail  bool
	}{
		{reply: "wrong_db", fail: true},
		{reply: "empty", fail: true},
		{reply: "no_head", fail: true},
		{reply: "upper_db", fail: false},
	} {
		t.Run("probe reply "+tc.reply, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, "success", liveGateEnv("GC_FAKE_DOLT_LIVE_PROBE_REPLY="+tc.reply)...)
			if !tc.fail {
				if err != nil {
					t.Fatalf("case-insensitive database name must pass the gate: %v\n%s", err, out)
				}
				requireNoMarker(t, beadsMarker(fixture, "compact-quarantine"))
				if beadsLiveProbes(readCompactDoltLog(t, fixture)) == 0 {
					t.Fatalf("gate never probed the database")
				}
				return
			}
			requireFailedRun(t, err, out)
			requireLiveGateQuarantine(t, fixture, out, "flatten")
		})
	}
}

// AC-3: a live-gate failure stops the whole run at once, with no further
// statements or bookkeeping, and names the selected databases not reached.
func TestCompactScriptLiveGateAbortsRunBeforeLaterDatabases(t *testing.T) {
	for _, tc := range []struct {
		name       string
		extraDBs   []string
		onlyDBs    string
		notReached string
	}{
		{name: "selected databases remain", extraDBs: []string{"mid", "zed"}, onlyDBs: "beads,zed", notReached: "zed"},
		{name: "every database remains", extraDBs: []string{"mid", "zed"}, notReached: "mid zed"},
		{name: "last database", notReached: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			addCompactDatabases(t, fixture, tc.extraDBs...)
			out, err := fixture.run(t, "success", liveGateEnv(
				"GC_FAKE_DOLT_LIVE_FAULT=unserve",
				"GC_DOLT_COMPACT_ONLY_DBS="+tc.onlyDBs,
			)...)
			requireFailedRun(t, err, out)
			requireLiveGateQuarantine(t, fixture, out, "post-flatten full GC")
			requireNotReached(t, out, "post-flatten full GC", tc.notReached)
			after := beadsLogAfter(t, readCompactDoltLog(t, fixture), "DOLT_GC")
			for _, entry := range doltLogEntries(strings.TrimSpace(after)) {
				if !strings.HasPrefix(entry, "db=beads "+liveProbeQuery) {
					t.Fatalf("statement after the live-gate failure: %q\n%s", entry, after)
				}
			}
			if beadsReportedSuccess(out) {
				t.Fatalf("live-gate abort must not report success:\n%s", out)
			}
			for _, forbidden := range []string{"database(s) failed compaction", "db=mid", "db=zed"} {
				if strings.Contains(out, forbidden) {
					t.Fatalf("live-gate abort reached %q:\n%s", forbidden, out)
				}
			}
		})
	}
}

// AC-4: a probe that fails transiently and recovers within the configured
// attempts leaves no quarantine; the rewrite result alone decides the run.
func TestCompactScriptLiveGateRetriesTransientProbeFailure(t *testing.T) {
	t.Run("rewrite succeeded", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		addCompactDatabases(t, fixture, "zed")
		out, err := fixture.run(t, "success", liveGateEnv(
			"GC_FAKE_DOLT_LIVE_FAULT=unserve",
			"GC_FAKE_DOLT_LIVE_FAULT_PROBES=1",
		)...)
		if err != nil {
			t.Fatalf("recovered probe must not fail the run: %v\n%s", err, out)
		}
		requireNoMarker(t, beadsMarker(fixture, "compact-quarantine"))
		if !strings.Contains(out, "compact: db=beads live probe recovered on attempt 2/3 after post-flatten full GC") {
			t.Fatalf("output missing probe recovery:\n%s", out)
		}
		log := readCompactDoltLog(t, fixture)
		if got := beadsLiveProbes(beadsLogAfter(t, log, "DOLT_GC")); got != 2 {
			t.Fatalf("beads live probes after GC = %d, want 2:\n%s", got, log)
		}
		if !strings.Contains(log, "db=zed query=CALL DOLT_GC('--full'") {
			t.Fatalf("run must go on to zed after a recovered probe:\n%s", log)
		}
		if !beadsReportedSuccess(out) {
			t.Fatalf("recovered database must report its compaction success:\n%s", out)
		}
	})

	t.Run("rewrite failed", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		addCompactDatabases(t, fixture, "zed")
		out, err := fixture.run(t, "gc_failure", liveGateEnv(
			"GC_FAKE_DOLT_LIVE_FAULT=unserve",
			"GC_FAKE_DOLT_LIVE_FAULT_PROBES=1",
		)...)
		requireFailedRun(t, err, out)
		requireNoMarker(t, beadsMarker(fixture, "compact-quarantine"))
		if got := compactMarkerValue(t, beadsMarker(fixture, "compact-pending-gc"), "reason"); got != "flatten succeeded but full GC failed" {
			t.Fatalf("pending-GC reason = %q", got)
		}
		log := readCompactDoltLog(t, fixture)
		if got := beadsLiveProbes(beadsLogAfter(t, log, "DOLT_GC")); got != 2 {
			t.Fatalf("beads live probes after GC = %d, want 2:\n%s", got, log)
		}
		if !strings.Contains(out, "compact: 2 database(s) failed compaction") {
			t.Fatalf("ordinary failures must still be counted for every database:\n%s", out)
		}
	})
}

// AC-5: every full-GC and bare-GC entry path gets the same gate and abort,
// before any caller-side push or marker clearing.
func TestCompactScriptLiveGateCoversEveryRewriteMode(t *testing.T) {
	pendingGC := func(t *testing.T, fixture compactScriptFixture) string {
		return writeCompactStateMarker(t, fixture, "compact-pending-gc",
			"db=beads\nreason=flatten succeeded but full GC failed\ncreated_at="+time.Now().UTC().Format("2006-01-02T15:04:05Z")+
				"\nremote=origin\nexpected_remote_head=headcommit\nexpected_remote_head_verified=1\ncompacted_from_head=headcommit\nlocal_branch=main\nremote_branch=main\n")
	}
	for _, tc := range []struct {
		name      string
		mode      string
		args      []string
		env       []string
		setup     func(t *testing.T, fixture compactScriptFixture) string
		step      string
		gcQuery   string
		baseline  bool
		markerKey string
	}{
		{name: "gc-only", mode: "success", args: []string{"--gc-only"}, step: "gc-only reclaim", gcQuery: "DOLT_GC('--full'", baseline: true},
		{name: "bare GC", mode: "success", env: []string{"GC_DOLT_COMPACT_BARE_GC=1"}, step: "bare GC", gcQuery: "DOLT_GC('--archive-level=0')", baseline: true},
		{
			name: "shared-history bare GC", mode: "remote_success", step: "shared-history bare GC",
			gcQuery: "DOLT_GC('--archive-level=0')", baseline: true,
			setup: func(t *testing.T, fixture compactScriptFixture) string {
				setCompactWatermark(t, fixture, "")
				return ""
			},
		},
		{name: "shared-history pending-GC retry", mode: "remote_success", step: "shared-history pending-GC retry", gcQuery: "DOLT_GC('--full'", baseline: true, setup: pendingGC},
		{name: "pending-GC retry", mode: "remote_success", env: []string{"GC_DOLT_COMPACT_ALLOW_FEDERATED=1"}, step: "pending-GC retry", gcQuery: "DOLT_GC('--full'", baseline: true, setup: pendingGC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			addCompactDatabases(t, fixture, "zed")
			pendingMarker := ""
			if tc.setup != nil {
				pendingMarker = tc.setup(t, fixture)
			}
			env := append(liveGateEnv("GC_FAKE_DOLT_LIVE_FAULT=unserve"), tc.env...)
			out, err := fixture.runWithArgs(t, tc.mode, tc.args, env...)
			requireFailedRun(t, err, out)
			marker := requireLiveGateQuarantine(t, fixture, out, tc.step)
			requireNotReached(t, out, tc.step, "zed")
			if got := compactMarkerValue(t, marker, "live_gate_pre_compaction_head"); got != "headcommit" {
				t.Fatalf("live_gate_pre_compaction_head = %q, want the probed baseline", got)
			}
			log := readCompactDoltLog(t, fixture)
			before := strings.SplitN(log, tc.gcQuery, 2)[0]
			if tc.baseline && beadsLiveProbes(before) != 1 {
				t.Fatalf("GC-first pass must probe a baseline once before GC:\n%s", log)
			}
			after := beadsLogAfter(t, log, tc.gcQuery)
			for _, forbidden := range []string{"DOLT_PUSH", "db=zed "} {
				if strings.Contains(after, forbidden) {
					t.Fatalf("live-gate failure reached %s:\n%s", forbidden, after)
				}
			}
			if pendingMarker != "" {
				if _, err := os.Stat(pendingMarker); err != nil {
					t.Fatalf("pending-GC marker must be kept after a live-gate failure: %v", err)
				}
			}
		})
	}

	t.Run("baseline unavailable", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		addCompactDatabases(t, fixture, "zed")
		out, err := fixture.runWithArgs(t, "success", []string{"--gc-only"}, liveGateEnv("GC_FAKE_DOLT_LIVE_PROBE_REPLY=empty")...)
		requireFailedRun(t, err, out)
		requireNoMarker(t, beadsMarker(fixture, "compact-quarantine"))
		log := readCompactDoltLog(t, fixture)
		if strings.Contains(log, "db=beads query=CALL DOLT_GC") {
			t.Fatalf("GC must not run without a live baseline:\n%s", log)
		}
		if !strings.Contains(log, "db=zed query=CALL DOLT_GC('--full'") {
			t.Fatalf("baseline failure is ordinary; the run must go on to zed:\n%s", log)
		}
		if !strings.Contains(out, "compact: 1 database(s) failed gc-only reclaim") {
			t.Fatalf("baseline failure must be counted as an ordinary failure:\n%s", out)
		}
	})
}
