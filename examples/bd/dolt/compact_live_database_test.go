package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A scheduled compaction can leave a live database out of the data dir: the
// server comes back without it, and nothing in the compactor notices. These tests pin the live-database gate: after any flatten
// or GC the database must still have its data-dir entry and still be served
// with a resolvable HEAD, or compact quarantines it and aborts the run before
// touching another database.

const (
	compactLiveProbeQuery     = "CONCAT(DATABASE(), ':', HASHOF('HEAD'))"
	compactLiveDatabaseReason = "live database unavailable after compaction"
)

func compactBeadsQuarantineMarkerPath(fixture compactScriptFixture) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
}

// compactDoltLogLinesFor returns the fake dolt log lines issued against db
// that contain substr.
func compactDoltLogLinesFor(log, db, substr string) []string {
	var lines []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "db="+db+" ") && strings.Contains(line, substr) {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertCompactQueryBefore fails unless the first fake dolt log line containing
// earlier comes before the first line containing later.
func assertCompactQueryBefore(t *testing.T, log, earlier, later string) {
	t.Helper()
	ei, li := -1, -1
	for i, line := range strings.Split(log, "\n") {
		if ei < 0 && strings.Contains(line, earlier) {
			ei = i
		}
		if li < 0 && strings.Contains(line, later) {
			li = i
		}
	}
	if ei < 0 {
		t.Fatalf("dolt log missing %q:\n%s", earlier, log)
	}
	if li < 0 {
		t.Fatalf("dolt log missing %q:\n%s", later, log)
	}
	if ei > li {
		t.Fatalf("want %q before %q (lines %d > %d)\n%s", earlier, later, ei, li, log)
	}
}

// addCompactFixtureZedDatabase adds a second database, discovered after beads,
// so a test can prove an aborted run never reaches it.
func addCompactFixtureZedDatabase(t *testing.T, fixture compactScriptFixture) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "zed", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir zed dolt db: %v", err)
	}
}

func TestCompactScriptConfirmsDatabaseStillServedAfterCompaction(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	log := readCompactDoltLog(t, fixture)
	if got := compactDoltLogLinesFor(log, "beads", compactLiveProbeQuery); len(got) != 1 {
		t.Fatalf("compact must probe the compacted database exactly once after GC, got %d:\n%s", len(got), log)
	}
	assertCompactQueryBefore(t, log, "DOLT_GC('--full')", compactLiveProbeQuery)
	if _, err := os.Stat(compactBeadsQuarantineMarkerPath(fixture)); !os.IsNotExist(err) {
		t.Fatalf("a served database must not be quarantined (stat err=%v):\n%s", err, out)
	}
}

func TestCompactScriptAbortsRunWhenDatabaseLeavesDataDirDuringCompaction(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	addCompactFixtureZedDatabase(t, fixture)

	out, err := fixture.run(t, "db_dir_vanishes_after_gc",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_DOLT_COMPACT_ALERT_TO=dolt-ops")
	if err == nil {
		t.Fatalf("compact succeeded although the compacted database left the data dir:\n%s", out)
	}
	for _, want := range []string{
		"db=beads is no longer served after full GC",
		"data_dir_entry_missing",
		"aborting this run",
		"zed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	log := readCompactDoltLog(t, fixture)
	if got := compactDoltLogLinesFor(log, "zed", ""); len(got) != 0 {
		t.Fatalf("compact must not touch any other database after losing one, got:\n%s", strings.Join(got, "\n"))
	}

	marker := compactBeadsQuarantineMarkerPath(fixture)
	if got := compactMarkerValue(t, marker, "reason"); got != compactLiveDatabaseReason {
		t.Fatalf("quarantine reason = %q, want %q", got, compactLiveDatabaseReason)
	}
	assertCompactMarkerHasEvidence(t, marker,
		"compaction_step=full GC",
		"live_probe_failure=data_dir_entry_missing path="+filepath.Join(fixture.dataDir, "beads", ".dolt"),
		"data_dir_entry_before=present",
		"pre_compaction_head=headcommit",
		"decision=abort_run_database_not_served",
	)
	assertCompactBeadsQuarantineAlert(t, fixture, "dolt-ops", marker, "compact-quarantine", compactLiveDatabaseReason)
}

func TestCompactScriptAbortsRunWhenServerStopsServingDatabaseAfterCompaction(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	addCompactFixtureZedDatabase(t, fixture)

	out, err := fixture.run(t, "db_unserved_after_gc",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=2",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
	if err == nil {
		t.Fatalf("compact succeeded although the server stopped serving the compacted database:\n%s", out)
	}
	if !strings.Contains(out, "db=beads is no longer served after full GC") {
		t.Fatalf("output missing lost-database report:\n%s", out)
	}

	log := readCompactDoltLog(t, fixture)
	if got := compactDoltLogLinesFor(log, "beads", compactLiveProbeQuery); len(got) != 2 {
		t.Fatalf("compact must probe GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=2 times, got %d:\n%s", len(got), log)
	}
	if got := compactDoltLogLinesFor(log, "zed", ""); len(got) != 0 {
		t.Fatalf("compact must not touch any other database after losing one, got:\n%s", strings.Join(got, "\n"))
	}

	marker := compactBeadsQuarantineMarkerPath(fixture)
	if got := compactMarkerValue(t, marker, "reason"); got != compactLiveDatabaseReason {
		t.Fatalf("quarantine reason = %q, want %q", got, compactLiveDatabaseReason)
	}
	failure := compactMarkerValue(t, marker, "live_probe_failure")
	if !strings.Contains(failure, "server_probe_failed") || !strings.Contains(failure, "cannot resolve default branch head") {
		t.Fatalf("live_probe_failure should carry the server error, got %q", failure)
	}
	if got := compactMarkerValue(t, marker, "live_probe_attempts"); got != "2" {
		t.Fatalf("live_probe_attempts = %q, want 2", got)
	}

	// The quarantine holds: the next run refuses the database instead of
	// compacting it again.
	if err := os.WriteFile(fixture.doltLog, nil, 0o644); err != nil {
		t.Fatalf("reset dolt log: %v", err)
	}
	out, err = fixture.run(t, "success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_ONLY_DBS=beads")
	if err == nil {
		t.Fatalf("compact must refuse a database quarantined as no longer served:\n%s", out)
	}
	log = readCompactDoltLog(t, fixture)
	for _, forbidden := range []string{"DOLT_RESET", "DOLT_GC"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("quarantined database must not be compacted again (%s):\n%s", forbidden, log)
		}
	}
}

func TestCompactScriptRetriesTransientLiveDatabaseProbe(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "db_unserved_once_after_gc",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
	if err != nil {
		t.Fatalf("compact failed although the database came back on retry: %v\n%s", err, out)
	}
	if !strings.Contains(out, "live-database probe attempt 1/3 failed") {
		t.Fatalf("output missing transient probe retry:\n%s", out)
	}
	if _, err := os.Stat(compactBeadsQuarantineMarkerPath(fixture)); !os.IsNotExist(err) {
		t.Fatalf("a database served on retry must not be quarantined (stat err=%v):\n%s", err, out)
	}
}

func TestCompactScriptGCOnlyAbortsWhenDatabaseLeavesDataDir(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	addCompactFixtureZedDatabase(t, fixture)

	out, err := fixture.runWithArgs(t, "db_dir_vanishes_after_gc", []string{"--gc-only"},
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
	if err == nil {
		t.Fatalf("gc-only succeeded although the database left the data dir:\n%s", out)
	}
	if !strings.Contains(out, "db=beads is no longer served after gc-only full GC") {
		t.Fatalf("output missing lost-database report:\n%s", out)
	}
	log := readCompactDoltLog(t, fixture)
	if got := compactDoltLogLinesFor(log, "zed", ""); len(got) != 0 {
		t.Fatalf("gc-only must not touch any other database after losing one, got:\n%s", strings.Join(got, "\n"))
	}
	marker := compactBeadsQuarantineMarkerPath(fixture)
	if got := compactMarkerValue(t, marker, "reason"); got != compactLiveDatabaseReason {
		t.Fatalf("quarantine reason = %q, want %q", got, compactLiveDatabaseReason)
	}
}

func TestCompactScriptBareGCAbortsWhenServerStopsServingDatabase(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	addCompactFixtureZedDatabase(t, fixture)

	out, err := fixture.run(t, "db_unserved_after_gc",
		"GC_DOLT_COMPACT_BARE_GC=1",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
	if err == nil {
		t.Fatalf("bare-gc succeeded although the server stopped serving the database:\n%s", out)
	}
	if !strings.Contains(out, "db=beads is no longer served after bare GC") {
		t.Fatalf("output missing lost-database report:\n%s", out)
	}
	log := readCompactDoltLog(t, fixture)
	if got := compactDoltLogLinesFor(log, "zed", ""); len(got) != 0 {
		t.Fatalf("bare-gc must not touch any other database after losing one, got:\n%s", strings.Join(got, "\n"))
	}
}

func TestCompactScriptSkipsLiveDatabaseProbeWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		env  []string
	}{
		{name: "dry-run", mode: "success", env: []string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_DOLT_COMPACT_DRY_RUN=1"}},
		{name: "below-threshold", mode: "below_threshold", env: []string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, tc.mode, tc.env...)
			if err != nil {
				t.Fatalf("compact failed: %v\n%s", err, out)
			}
			if log := readCompactDoltLog(t, fixture); strings.Contains(log, compactLiveProbeQuery) {
				t.Fatalf("compact must not probe a database it did not mutate:\n%s", log)
			}
		})
	}
}

func TestCompactScriptRejectsInvalidLiveProbeSettings(t *testing.T) {
	for _, env := range []string{
		"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=0",
		"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=many",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=-1",
	} {
		t.Run(env, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, "success", env)
			if err == nil {
				t.Fatalf("compact accepted %s:\n%s", env, out)
			}
			name := strings.SplitN(env, "=", 2)[0]
			if !strings.Contains(out, "invalid "+name) {
				t.Fatalf("output should name the invalid setting %s:\n%s", name, out)
			}
			if log := readCompactDoltLog(t, fixture); strings.Contains(log, "DOLT_GC") {
				t.Fatalf("invalid settings must stop compact before any GC:\n%s", log)
			}
		})
	}
}
