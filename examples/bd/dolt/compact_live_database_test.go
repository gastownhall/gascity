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
	entry := filepath.Join(fixture.dataDir, "beads", ".dolt")
	for _, want := range []string{
		"reason=" + compactLiveGateReason,
		"live_gate_failed_step=post-flatten full GC",
		"live_gate_failure=data-dir entry",
		"live_gate_data_dir_entry_after=absent",
		"live_gate_pre_compaction_head=headcommit",
		"compact: db=beads quarantine recovery: restore the database from a backup only if " + entry + " is gone",
		"restart the Dolt server (gc dolt restart) instead",
		`gc dolt sql -q "USE beads; SELECT HASHOF('HEAD')"`,
		"pre-compaction HEAD=headcommit",
		"gc bd list --rig <rig> --limit 1",
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
	// The pre-compaction HEAD only tells whether the flatten landed: rolling
	// the database back to it discards the flatten and every later write.
	if strings.Contains(out, "to its pre-compaction HEAD") {
		t.Fatalf("quarantine refusal told the operator to roll the database back to its pre-compaction HEAD:\n%s", out)
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

func TestCompactScriptLiveGateCoversEveryGCEntryPath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		args      []string
		env       []string
		pendingGC bool
		step      string
	}{
		{name: "gc-only reclaim", mode: "success", args: []string{"--gc-only"}, step: "gc-only reclaim"},
		{name: "bare GC", mode: "success", env: []string{"GC_DOLT_COMPACT_BARE_GC=1"}, step: "bare GC"},
		{name: "shared-history bare GC", mode: "remote_success", step: "shared-history bare GC"},
		{name: "pending-GC retry", mode: "success", pendingGC: true, step: "pending-GC retry"},
		{name: "shared-history pending-GC retry", mode: "remote_success", pendingGC: true, step: "shared-history pending-GC retry"},
	} {
		// vanish removes the data-dir entry while SQL still answers; unserved
		// keeps the entry and fails every probe after the GC, not the baseline.
		for _, fault := range []struct {
			action   string
			dirAfter string
		}{
			{action: "vanish", dirAfter: "absent"},
			{action: "unserved", dirAfter: "present"},
		} {
			t.Run(tc.name+"/"+fault.action, func(t *testing.T) {
				fixture := newCompactScriptFixture(t)
				var pendingMarker string
				if tc.pendingGC {
					pendingMarker = writeCompactPendingGCMarker(t, fixture, "beads")
				}
				setCompactBeadsLiveFault(t, fixture, "gc", fault.action)

				env := append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"}, tc.env...)
				out, err := fixture.runWithArgs(t, tc.mode, tc.args, env...)
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatalf("compact exit = %v, want exit status 1:\n%s", err, out)
				}
				assertCompactBeadsLiveGateMarker(t, fixture, map[string]string{
					"live_gate_steps":                 tc.step,
					"live_gate_failed_step":           tc.step,
					"live_gate_data_dir_entry_before": "present",
					"live_gate_data_dir_entry_after":  fault.dirAfter,
					"live_gate_pre_compaction_head":   "headcommit",
				})
				assertCompactBeadsQuarantineAlert(t, fixture, "mayor", compactBeadsQuarantineMarkerPath(fixture), "compact-quarantine", compactLiveGateReason)
				want := "compact: db=beads live database gate failed after " + tc.step + " — aborting the run; selected databases not reached: none"
				if !strings.Contains(out, want) {
					t.Fatalf("compact output missing %q:\n%s", want, out)
				}
				if pendingMarker != "" {
					if _, err := os.Stat(pendingMarker); err != nil {
						t.Fatalf("live gate failure cleared the pending-GC marker: %v\n%s", err, out)
					}
				}
				log := readCompactDoltLog(t, fixture)
				if pushes := compactDoltLogQueries(log, "beads", "DOLT_PUSH"); len(pushes) != 0 {
					t.Fatalf("compact pushed a database that failed its live gate: %q", pushes)
				}
				// The baseline probe precedes the GC; only the failed gate's
				// probes follow it.
				lines := strings.Split(strings.TrimSpace(log), "\n")
				gcAt := -1
				for i, line := range lines {
					if strings.Contains(line, "CALL DOLT_GC(") {
						gcAt = i
					}
				}
				if gcAt < 1 || lines[gcAt-1] != "db=beads query="+compactLiveProbeQuery {
					t.Fatalf("compact did not probe the database before its GC:\n%s", log)
				}
				if gcAt == len(lines)-1 {
					t.Fatalf("compact did not probe the database after its GC:\n%s", log)
				}
				for _, line := range lines[gcAt+1:] {
					if line != "db=beads query="+compactLiveProbeQuery {
						t.Fatalf("compact issued %q after the failed gate\nlog:\n%s", line, log)
					}
				}
			})
		}
	}
}

func TestCompactScriptLiveGateBaselineFailureIsAnOrdinaryFailure(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.liveFaultDir, "unserved-beads"), []byte("always\n"), 0o644); err != nil {
		t.Fatalf("write unserved fault: %v", err)
	}

	out, err := fixture.runWithArgs(t, "success", []string{"--gc-only"})
	if err == nil {
		t.Fatalf("compact succeeded although the database was not served before its GC:\n%s", out)
	}
	want := "compact: db=beads live database baseline probe failed before gc-only reclaim: probe failed rc=1: database not found: beads — not starting the rewrite"
	if !strings.Contains(out, want) {
		t.Fatalf("compact output missing %q:\n%s", want, out)
	}
	if !strings.Contains(out, "compact: 1 database(s) failed gc-only reclaim") {
		t.Fatalf("compact output missing the ordinary failure tally:\n%s", out)
	}
	if _, err := os.Stat(compactBeadsQuarantineMarkerPath(fixture)); !os.IsNotExist(err) {
		t.Fatalf("baseline probe failure quarantined the database (stat err=%v):\n%s", err, out)
	}
	if gcCalls := compactDoltLogQueries(readCompactDoltLog(t, fixture), "beads", "CALL DOLT_GC("); len(gcCalls) != 0 {
		t.Fatalf("compact ran GC on a database it could not probe: %q", gcCalls)
	}
}

func TestCompactScriptLiveGateNeverProbesWithoutARewrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		args []string
		env  []string
	}{
		{name: "scheduled dry run", mode: "success", env: []string{"GC_DOLT_COMPACT_DRY_RUN=1"}},
		{name: "gc-only dry run", mode: "success", args: []string{"--gc-only", "--dry-run"}},
		{name: "bare GC dry run", mode: "success", env: []string{"GC_DOLT_COMPACT_BARE_GC=1", "GC_DOLT_COMPACT_DRY_RUN=1"}},
		{name: "shared-history dry run", mode: "remote_success", env: []string{"GC_DOLT_COMPACT_DRY_RUN=1"}},
		{name: "below threshold", mode: "below_threshold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)

			env := append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"}, tc.env...)
			out, err := fixture.runWithArgs(t, tc.mode, tc.args, env...)
			if err != nil {
				t.Fatalf("compact failed on a pass with no rewrite: %v\n%s", err, out)
			}
			if probes := compactDoltLogQueries(readCompactDoltLog(t, fixture), "beads", compactLiveProbeQuery); len(probes) != 0 {
				t.Fatalf("compact probed a database it did not rewrite: %q\n%s", probes, out)
			}
			if _, err := os.Stat(compactBeadsQuarantineMarkerPath(fixture)); !os.IsNotExist(err) {
				t.Fatalf("compact wrote a quarantine marker without a rewrite (stat err=%v):\n%s", err, out)
			}
			assertCompactNoPendingRemediationMarkers(t, fixture, out)
			if strings.Contains(out, "live database") || strings.Contains(out, "live probe") {
				t.Fatalf("compact reported on the live-database gate without a rewrite:\n%s", out)
			}
		})
	}

	t.Run("existing quarantine refusal", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		marker := compactBeadsQuarantineMarkerPath(fixture)
		if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
			t.Fatalf("mkdir quarantine dir: %v", err)
		}
		if err := os.WriteFile(marker, []byte("db=beads\nreason=post-flatten row count decreased\n"), 0o600); err != nil {
			t.Fatalf("write quarantine marker: %v", err)
		}

		out, err := fixture.runWithArgs(t, "success", []string{"--gc-only"})
		if err == nil {
			t.Fatalf("compact reclaimed a quarantined database:\n%s", out)
		}
		if probes := compactDoltLogQueries(readCompactDoltLog(t, fixture), "beads", compactLiveProbeQuery); len(probes) != 0 {
			t.Fatalf("compact probed a database it refused: %q\n%s", probes, out)
		}
		if reason := compactMarkerValue(t, marker, "reason"); reason != "post-flatten row count decreased" {
			t.Fatalf("refusal rewrote the quarantine reason to %q", reason)
		}
		assertCompactNoPendingRemediationMarkers(t, fixture, out)
	})

	// Control: the same fixture probes once a pass does rewrite.
	t.Run("a rewrite probes", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
		if err != nil {
			t.Fatalf("compact failed: %v\n%s", err, out)
		}
		if probes := compactDoltLogQueries(readCompactDoltLog(t, fixture), "beads", compactLiveProbeQuery); len(probes) == 0 {
			t.Fatalf("compact rewrote beads without probing it:\n%s", out)
		}
	})
}

func TestCompactScriptLiveGateKeepsOrdinaryRewriteFailures(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mode          string
		wantOut       []string
		probeAfter    string
		wantPendingGC bool
		wantTally     string
	}{
		{
			name:       "flatten reset fails",
			mode:       "flatten_failure",
			wantOut:    []string{"reset exploded", "already at pre-flatten HEAD=headcommit"},
			probeAfter: "DOLT_RESET('--soft'",
			wantTally:  "compact: 2 database(s) failed compaction",
		},
		{
			name:       "flatten commit fails and HEAD is restored",
			mode:       "commit_failure_after_reset",
			wantOut:    []string{"commit rejected after reset", "restored pre-flatten HEAD=headcommit"},
			probeAfter: "DOLT_RESET('--hard', 'headcommit')",
			wantTally:  "compact: 2 database(s) failed compaction",
		},
		{
			name:          "post-flatten full GC fails",
			mode:          "gc_failure",
			wantOut:       []string{"gc exploded"},
			probeAfter:    "CALL DOLT_GC(",
			wantPendingGC: true,
			wantTally:     "compact: 2 database(s) failed compaction",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			if err := os.MkdirAll(filepath.Join(fixture.dataDir, "zed", ".dolt"), 0o755); err != nil {
				t.Fatalf("mkdir zed data dir: %v", err)
			}

			out, err := fixture.run(t, tc.mode, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("compact exit = %v, want exit status 1:\n%s", err, out)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Fatalf("compact output missing %q:\n%s", want, out)
				}
			}
			if !strings.Contains(out, tc.wantTally) {
				t.Fatalf("compact output missing the failure tally %q:\n%s", tc.wantTally, out)
			}
			if strings.Contains(out, "aborting the run") {
				t.Fatalf("an ordinary rewrite failure aborted the run:\n%s", out)
			}
			if _, err := os.Stat(compactBeadsQuarantineMarkerPath(fixture)); !os.IsNotExist(err) {
				t.Fatalf("an ordinary rewrite failure quarantined a live database (stat err=%v):\n%s", err, out)
			}
			_, pendingErr := os.Stat(compactBeadsPendingGCMarkerPath(fixture))
			if tc.wantPendingGC != (pendingErr == nil) {
				t.Fatalf("pending-GC marker present=%v, want %v (stat err=%v):\n%s", pendingErr == nil, tc.wantPendingGC, pendingErr, out)
			}

			// A multi-statement query continues on the log lines after its
			// db= line, so the failed step is located in the raw log. beads
			// runs first, so its step is the first match.
			log := readCompactDoltLog(t, fixture)
			lines := strings.Split(log, "\n")
			failedAt := -1
			for i, line := range lines {
				if strings.Contains(line, tc.probeAfter) {
					failedAt = i
					break
				}
			}
			if failedAt < 0 {
				t.Fatalf("compact never issued %q:\n%s", tc.probeAfter, log)
			}
			probed := false
			for _, line := range lines[failedAt+1:] {
				if line == "db=beads query="+compactLiveProbeQuery {
					probed = true
					break
				}
			}
			if !probed {
				t.Fatalf("compact did not probe beads after the failed rewrite step %q:\n%s", tc.probeAfter, log)
			}
			if len(compactDoltLogQueries(log, "zed", "")) == 0 {
				t.Fatalf("compact did not go on to zed after beads failed:\n%s", log)
			}
		})
	}
}

func TestCompactScriptLiveProbeSettingsAreValidatedBeforeAnyQuery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "zero attempts", key: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS", value: "0", want: "must be a positive integer"},
		{name: "all-zero attempts", key: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS", value: "000", want: "must be a positive integer"},
		{name: "negative attempts", key: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS", value: "-1", want: "must be a positive integer"},
		{name: "word attempts", key: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS", value: "three", want: "must be a positive integer"},
		{name: "overlong attempts", key: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS", value: "99999999999999999999", want: "must be a positive integer"},
		{name: "negative delay", key: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS", value: "-1", want: "must be a non-negative integer"},
		{name: "fractional delay", key: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS", value: "1.5", want: "must be a non-negative integer"},
		{name: "overlong delay", key: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS", value: "99999999999999999999", want: "must be a non-negative integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)

			out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", tc.key+"="+tc.value)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("compact exit = %v, want exit status 2:\n%s", err, out)
			}
			want := "compact: invalid " + tc.key + "=" + tc.value + " (" + tc.want + ")"
			if !strings.Contains(out, want) {
				t.Fatalf("compact output missing %q:\n%s", want, out)
			}
			if log := readCompactDoltLog(t, fixture); strings.TrimSpace(log) != "" {
				t.Fatalf("compact queried Dolt before rejecting %s:\n%s", tc.key, log)
			}
		})
	}

	for _, tc := range []struct {
		name      string
		env       []string
		recovered string
	}{
		{name: "empty values mean the defaults", env: []string{"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=", "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS="}, recovered: "recovered on attempt 2/3"},
		{name: "leading zeros are decimal", env: []string{"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=09", "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=00"}, recovered: "recovered on attempt 2/9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			setCompactBeadsLiveFault(t, fixture, "gc", "transient:1")

			out, err := fixture.run(t, "success", append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"}, tc.env...)...)
			if err != nil {
				t.Fatalf("compact rejected a valid live-probe setting: %v\n%s", err, out)
			}
			if !strings.Contains(out, "compact: db=beads live probe "+tc.recovered) {
				t.Fatalf("compact output missing %q:\n%s", tc.recovered, out)
			}
		})
	}
}
