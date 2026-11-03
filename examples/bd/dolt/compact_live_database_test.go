package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// compactLiveGateReason is the quarantine reason compact records when a
// database it rewrote stops being served.
const compactLiveGateReason = "live database unavailable after compaction"

// compactLiveProbeQuery is the served-HEAD probe the live-database gate issues.
const compactLiveProbeQuery = "SELECT CONCAT(DATABASE(), ':', HASHOF('HEAD'))"

func compactBeadsQuarantineMarkerPath(fixture compactScriptFixture) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
}

func compactBeadsPendingGCMarkerPath(fixture compactScriptFixture) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
}

// setCompactBeadsLiveFault arms a one-shot fake-dolt fault that fires after
// the next rewrite of kind ("gc" or "flatten") on beads finishes.
func setCompactBeadsLiveFault(t *testing.T, fixture compactScriptFixture, kind, action string) {
	t.Helper()
	path := filepath.Join(fixture.liveFaultDir, "after-"+kind+"-beads")
	if err := os.WriteFile(path, []byte(action+"\n"), 0o644); err != nil {
		t.Fatalf("write live fault %s: %v", path, err)
	}
}

func compactDoltLogQueries(log, db, fragment string) []string {
	var matches []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "db="+db+" query=") && strings.Contains(line, fragment) {
			matches = append(matches, line)
		}
	}
	return matches
}

func assertCompactBeadsLiveGateMarker(t *testing.T, fixture compactScriptFixture, want map[string]string) {
	t.Helper()
	marker := compactBeadsQuarantineMarkerPath(fixture)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("live gate quarantine marker %s missing: %v", marker, err)
	}
	if reason := compactMarkerValue(t, marker, "reason"); reason != compactLiveGateReason {
		t.Fatalf("quarantine reason = %q, want %q", reason, compactLiveGateReason)
	}
	for key, value := range want {
		if got := compactMarkerValue(t, marker, key); got != value {
			t.Fatalf("quarantine marker %s = %q, want %q", key, got, value)
		}
	}
}

func TestCompactScriptAbortsWhenDatabaseLeavesDataDirAfterGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactBeadsLiveFault(t, fixture, "gc", "vanish")

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded after the database left the data dir:\n%s", out)
	}
	entry := filepath.Join(fixture.dataDir, "beads", ".dolt")
	assertCompactBeadsLiveGateMarker(t, fixture, map[string]string{
		"live_gate_steps":                 "flatten,post-flatten full GC",
		"live_gate_failed_step":           "post-flatten full GC",
		"live_gate_data_dir_entry":        entry,
		"live_gate_data_dir_entry_before": "present",
		"live_gate_data_dir_entry_after":  "absent",
		"live_gate_pre_compaction_head":   "headcommit",
	})
	if !strings.Contains(out, "live database gate failed after post-flatten full GC") {
		t.Fatalf("compact output missing the live gate diagnostic:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "compact: db=beads ") && strings.HasSuffix(line, "— ok") {
			t.Fatalf("compact reported success for a database that left the data dir: %q\n%s", line, out)
		}
	}
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", compactBeadsQuarantineMarkerPath(fixture), "compact-quarantine", compactLiveGateReason)
	if _, err := os.Stat(compactBeadsPendingGCMarkerPath(fixture)); !os.IsNotExist(err) {
		t.Fatalf("live gate failure left a pending-GC marker (stat err=%v):\n%s", err, out)
	}
}

func TestCompactScriptLiveGateQuarantineRefusalShowsEvidenceAndRestoreGuidance(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactBeadsLiveFault(t, fixture, "gc", "vanish")
	if out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"); err == nil {
		t.Fatalf("compact succeeded after the database left the data dir:\n%s", out)
	}

	out, err := fixture.runWithArgs(t, "success", []string{"--gc-only"})
	if err == nil {
		t.Fatalf("compact reclaimed a database quarantined by the live gate:\n%s", out)
	}
	marker := compactBeadsQuarantineMarkerPath(fixture)
	for _, want := range []string{
		"reason=" + compactLiveGateReason,
		"live_gate_failed_step=post-flatten full GC",
		"live_gate_failure=data-dir entry",
		"live_gate_data_dir_entry_after=absent",
		"live_gate_pre_compaction_head=headcommit",
		"compact: db=beads quarantine recovery: restore the database",
		"then remove " + marker,
		"Compact Quarantine Reasons",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("quarantine refusal missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "rerun gc dolt compact --gc-only") {
		t.Fatalf("quarantine refusal told the operator to GC a database that is not served:\n%s", out)
	}
}

func TestCompactScriptLiveGateQuarantinesWhenFlattenRemovesDataDir(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactBeadsLiveFault(t, fixture, "flatten", "vanish")

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded after the flatten removed the database:\n%s", out)
	}
	assertCompactBeadsLiveGateMarker(t, fixture, map[string]string{
		"live_gate_steps":                 "flatten",
		"live_gate_failed_step":           "flatten",
		"live_gate_data_dir_entry_before": "present",
		"live_gate_data_dir_entry_after":  "absent",
		"live_gate_pre_compaction_head":   "headcommit",
	})
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", compactBeadsQuarantineMarkerPath(fixture), "compact-quarantine", compactLiveGateReason)
	if gcCalls := compactDoltLogQueries(readCompactDoltLog(t, fixture), "beads", "CALL DOLT_GC("); len(gcCalls) != 0 {
		t.Fatalf("compact ran GC on a database the flatten removed: %q", gcCalls)
	}
}

// setCompactBeadsLiveProbeResponse makes the fake dolt answer every live probe
// on beads with response instead of "beads:<head>".
func setCompactBeadsLiveProbeResponse(t *testing.T, fixture compactScriptFixture, response string) {
	t.Helper()
	path := filepath.Join(fixture.liveFaultDir, "response-beads")
	if err := os.WriteFile(path, []byte(response+"\n"), 0o644); err != nil {
		t.Fatalf("write live probe response %s: %v", path, err)
	}
}

func TestCompactScriptLiveGateQuarantinesWhenRewrittenDatabaseIsNotServed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		faultKind  string
		failedStep string
		steps      string
	}{
		{name: "flatten", mode: "success", faultKind: "flatten", failedStep: "flatten", steps: "flatten"},
		{name: "full GC succeeded", mode: "success", faultKind: "gc", failedStep: "post-flatten full GC", steps: "flatten,post-flatten full GC"},
		{name: "full GC failed", mode: "gc_failure", faultKind: "gc", failedStep: "post-flatten full GC", steps: "flatten,post-flatten full GC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			setCompactBeadsLiveFault(t, fixture, tc.faultKind, "unserved")

			out, err := fixture.run(t, tc.mode, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err == nil {
				t.Fatalf("compact succeeded after the rewritten database stopped being served:\n%s", out)
			}
			assertCompactBeadsLiveGateMarker(t, fixture, map[string]string{
				"live_gate_steps":                 tc.steps,
				"live_gate_failed_step":           tc.failedStep,
				"live_gate_failure":               "served-HEAD probe failed",
				"live_gate_data_dir_entry_before": "present",
				"live_gate_data_dir_entry_after":  "present",
				"live_gate_pre_compaction_head":   "headcommit",
			})
			marker := compactBeadsQuarantineMarkerPath(fixture)
			if probe := compactMarkerValue(t, marker, "live_gate_probe_failure"); !strings.Contains(probe, "database not found: beads") {
				t.Fatalf("live_gate_probe_failure = %q, want the probe's server error", probe)
			}
			assertCompactBeadsQuarantineAlert(t, fixture, "mayor", marker, "compact-quarantine", compactLiveGateReason)
			if _, err := os.Stat(compactBeadsPendingGCMarkerPath(fixture)); !os.IsNotExist(err) {
				t.Fatalf("live gate failure left a pending-GC marker (stat err=%v):\n%s", err, out)
			}
		})
	}
}

func TestCompactScriptLiveGateValidatesServedDatabaseAndHead(t *testing.T) {
	for _, tc := range []struct {
		name        string
		response    string
		wantFailure string
	}{
		{name: "other database", response: "zed:headcommit", wantFailure: "probe served database 'zed', not 'beads'"},
		{name: "empty HEAD", response: "beads:", wantFailure: "probe returned unresolvable HEAD ''"},
		{name: "non-hash HEAD", response: "beads:NULL!", wantFailure: "probe returned unresolvable HEAD 'NULL!'"},
		{name: "no separator", response: "beads", wantFailure: "probe returned no database:HEAD answer (got 'beads')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			setCompactBeadsLiveProbeResponse(t, fixture, tc.response)

			out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err == nil {
				t.Fatalf("compact accepted live probe answer %q:\n%s", tc.response, out)
			}
			assertCompactBeadsLiveGateMarker(t, fixture, map[string]string{
				"live_gate_failed_step":   "flatten",
				"live_gate_probe_failure": tc.wantFailure,
			})
		})
	}

	t.Run("database name case and a new HEAD are accepted", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		setCompactBeadsLiveProbeResponse(t, fixture, "BEADS:Compact0Commit9")

		out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
		if err != nil {
			t.Fatalf("compact rejected a served database that differs only in case: %v\n%s", err, out)
		}
		if _, err := os.Stat(compactBeadsQuarantineMarkerPath(fixture)); !os.IsNotExist(err) {
			t.Fatalf("compact quarantined a live database (stat err=%v):\n%s", err, out)
		}
		if probes := compactDoltLogQueries(readCompactDoltLog(t, fixture), "beads", compactLiveProbeQuery); len(probes) != 2 {
			t.Fatalf("live probes after flatten and full GC = %d, want 2: %q", len(probes), probes)
		}
	})
}

func TestCompactScriptLiveGateFailureAbortsTheRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       []string
		unreached string
	}{
		{name: "every database selected", unreached: "yak,zed"},
		{name: "only-dbs filter", env: []string{"GC_DOLT_COMPACT_ONLY_DBS=beads,zed"}, unreached: "zed"},
		{name: "no selected database left", env: []string{"GC_DOLT_COMPACT_ONLY_DBS=beads"}, unreached: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			for _, db := range []string{"yak", "zed"} {
				if err := os.MkdirAll(filepath.Join(fixture.dataDir, db, ".dolt"), 0o755); err != nil {
					t.Fatalf("mkdir %s data dir: %v", db, err)
				}
			}
			setCompactBeadsLiveFault(t, fixture, "gc", "vanish")

			out, err := fixture.run(t, "success", append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"}, tc.env...)...)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("compact exit = %v, want exit status 1:\n%s", err, out)
			}
			want := "compact: db=beads live database gate failed after post-flatten full GC — aborting the run; selected databases not reached: " + tc.unreached
			if !strings.Contains(out, want) {
				t.Fatalf("compact output missing %q:\n%s", want, out)
			}
			if strings.Contains(out, "database(s) failed") {
				t.Fatalf("aborted run still reported the ordinary failure tally:\n%s", out)
			}

			// The failed gate's own probe is the last statement the run issues.
			lines := strings.Split(strings.TrimSpace(readCompactDoltLog(t, fixture)), "\n")
			last := lines[len(lines)-1]
			if last != "db=beads query="+compactLiveProbeQuery {
				t.Fatalf("compact issued statements after the failed gate; last statement %q\nlog:\n%s", last, strings.Join(lines, "\n"))
			}
			for _, db := range []string{"yak", "zed"} {
				if queries := compactDoltLogQueries(strings.Join(lines, "\n"), db, ""); len(queries) != 0 {
					t.Fatalf("compact queried %s after the failed gate: %q", db, queries)
				}
			}
		})
	}
}

func TestCompactScriptLiveGateRetriesATransientProbeFailure(t *testing.T) {
	t.Run("recovery keeps a successful rewrite successful", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		setCompactBeadsLiveFault(t, fixture, "gc", "transient:2")

		out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
		if err != nil {
			t.Fatalf("compact failed although the server recovered within the attempts: %v\n%s", err, out)
		}
		if _, err := os.Stat(compactBeadsQuarantineMarkerPath(fixture)); !os.IsNotExist(err) {
			t.Fatalf("compact quarantined a database that recovered (stat err=%v):\n%s", err, out)
		}
		for _, want := range []string{
			"compact: db=beads live probe attempt 1/3 failed: probe failed rc=1: cannot resolve default branch head for database beads",
			"compact: db=beads live probe attempt 2/3 failed:",
			"compact: db=beads live probe recovered on attempt 3/3",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("compact output missing %q:\n%s", want, out)
			}
		}
		// One probe after the flatten, three after the full GC.
		if probes := compactDoltLogQueries(readCompactDoltLog(t, fixture), "beads", compactLiveProbeQuery); len(probes) != 4 {
			t.Fatalf("live probes = %d, want 4: %q", len(probes), probes)
		}
	})

	t.Run("recovery leaves a failed rewrite an ordinary failure", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		setCompactBeadsLiveFault(t, fixture, "gc", "transient:1")

		out, err := fixture.run(t, "gc_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
		if err == nil {
			t.Fatalf("compact succeeded despite the failed full GC:\n%s", out)
		}
		if _, err := os.Stat(compactBeadsQuarantineMarkerPath(fixture)); !os.IsNotExist(err) {
			t.Fatalf("compact quarantined a database that recovered (stat err=%v):\n%s", err, out)
		}
		if _, err := os.Stat(compactBeadsPendingGCMarkerPath(fixture)); err != nil {
			t.Fatalf("failed full GC on a live database left no pending-GC marker: %v\n%s", err, out)
		}
		if !strings.Contains(out, "compact: 1 database(s) failed compaction") {
			t.Fatalf("compact output missing the ordinary failure tally:\n%s", out)
		}
	})

	t.Run("every configured attempt failing quarantines", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		setCompactBeadsLiveFault(t, fixture, "gc", "transient:2")

		out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=2")
		if err == nil {
			t.Fatalf("compact succeeded although every live probe attempt failed:\n%s", out)
		}
		assertCompactBeadsLiveGateMarker(t, fixture, map[string]string{
			"live_gate_failed_step": "post-flatten full GC",
			"live_gate_failure":     "served-HEAD probe failed",
		})
		if probes := compactDoltLogQueries(readCompactDoltLog(t, fixture), "beads", compactLiveProbeQuery); len(probes) != 3 {
			t.Fatalf("live probes = %d, want 1 after the flatten and 2 after the full GC: %q", len(probes), probes)
		}
	})
}
