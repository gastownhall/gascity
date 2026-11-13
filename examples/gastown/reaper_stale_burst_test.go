//go:build unix

package gastown_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reaperStaleBurstRun is what one reaper run did with a city scope holding a
// number of stale issues.
type reaperStaleBurstRun struct {
	// closed is how many of the stale issues a bd close named.
	closed int
	// gcLog is every gc call, the summary and escalations included.
	gcLog string
	// output is what the run printed, stdout and stderr, which a manual
	// `gc order run reaper` shows the operator.
	output string
	// outcome is the order outcome the run declared, or "".
	outcome string
}

// runReaperStaleBurst runs the core reaper over a city scope whose Step 5
// query answers count stale issues (ga-s0 .. ga-s<count-1>), with env
// layered over the fixture's.
func runReaperStaleBurst(t *testing.T, count int, env map[string]string) reaperStaleBurstRun {
	t.Helper()
	return runReaperStaleBurstIn(t, t.TempDir(), count, env)
}

// runReaperStaleBurstIn is runReaperStaleBurst on cityDir, whose incident
// state a later run sees.
func runReaperStaleBurstIn(t *testing.T, cityDir string, count int, env map[string]string) reaperStaleBurstRun {
	t.Helper()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	writeFreshBackupState(t, cityDir)
	binDir := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	outcome := filepath.Join(t.TempDir(), "outcome.json")

	var rows strings.Builder
	rows.WriteString(`id,close_mode\n`)
	for i := 0; i < count; i++ {
		fmt.Fprintf(&rows, `ga-s%d,bare\n`, i)
	}
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\ncitydb\n'
    ;;
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
    ;;
  *"SELECT id, CASE WHEN COALESCE(assignee"*"citydb"*"issues"*)
    printf '`+rows.String()+`'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)
	base := map[string]string{
		"BD_CALL_LOG":           bdLog,
		"GC_CALL_LOG":           gcLog,
		"GC_CITY":               cityDir,
		"GC_CITY_PATH":          cityDir,
		"GC_DOLT_HOST":          "127.0.0.1",
		"GC_DOLT_PORT":          "3307",
		"GC_DOLT_USER":          "root",
		"GC_DOLT_PASSWORD":      "",
		"GC_ORDER_OUTCOME_FILE": outcome,
		"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	for k, v := range env {
		base[k] = v
	}
	out, err := runScriptResult(t, coreScriptPath("reaper.sh"), base)
	if err != nil {
		t.Fatalf("reaper.sh failed: %v\n%s", err, out)
	}

	run := reaperStaleBurstRun{output: string(out)}
	bdData, _ := os.ReadFile(bdLog)
	for _, line := range strings.Split(string(bdData), "\n") {
		if !strings.Contains(line, "stale:auto-closed by reaper") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "ga-s") {
				run.closed++
			}
		}
	}
	gcData, _ := os.ReadFile(gcLog)
	run.gcLog = string(gcData)
	outcomeData, _ := os.ReadFile(outcome)
	run.outcome = string(outcomeData)
	return run
}

// TestReaperHoldsAStaleIssueBurst: a city scope whose Step 5 finds more
// stale issues than GC_REAPER_STALE_ISSUE_BURST_MAX (default 25) closes none,
// escalates as an anomaly naming the count and the one-run release, says so in
// its summary and its order outcome, and lists the held issues where a manual
// run shows them. After a storage split one city's first reaper run closed 98
// stale work beads at once (ga-efurfu).
func TestReaperHoldsAStaleIssueBurst(t *testing.T) {
	run := runReaperStaleBurst(t, 98, nil)
	if run.closed != 0 {
		t.Fatalf("the reaper closed %d of a 98-issue burst; want none:\n%s", run.closed, run.gcLog)
	}
	release := "gc order run reaper --var GC_REAPER_STALE_ISSUE_BURST_ACK=98"
	for _, want := range []string{
		"stale_issues_held:98",
		"closed:0",
		"ESCALATION: Reaper anomalies detected [MEDIUM]",
		"stale-issue burst held=98",
		release,
		"GC_REAPER_STALE_ISSUE_BURST_MAX=25",
		// The escalation warns that the persistent order env is no one-run
		// channel: an ACK left there releases a later burst of that size.
		"[[orders.overrides]]",
	} {
		if !strings.Contains(run.gcLog, want) {
			t.Errorf("the run does not report %q:\n%s", want, run.gcLog)
		}
	}
	if !strings.Contains(run.outcome, `"partial"`) || !strings.Contains(run.outcome, "step 5 held 98 stale issue(s)") {
		t.Errorf("the order outcome does not name the held step: %q", run.outcome)
	}
	for _, want := range []string{"held 98 stale issue(s) in citydb", "ga-s0 ga-s1 ", " ga-s97", release} {
		if !strings.Contains(run.output, want) {
			t.Errorf("the run's output does not list the held burst (%q):\n%s", want, run.output)
		}
	}
}

// TestReaperStaleIssueBurstRelease: what releases a held burst and what does
// not. The acknowledgement names a count, so a backlog whose size moved since
// an operator reviewed it is held again rather than closed on a stale review,
// and an acknowledgement that releases nothing says why.
func TestReaperStaleIssueBurstRelease(t *testing.T) {
	for _, tc := range []struct {
		name       string
		count      int
		env        map[string]string
		wantClosed int
		wantHeld   bool
		// wantOutput is what the run says about the acknowledgement.
		wantOutput string
	}{
		{name: "at the default max", count: 25, wantClosed: 25},
		{name: "one over the default max", count: 26, wantHeld: true},
		{name: "acknowledged at its exact count", count: 30, env: map[string]string{"GC_REAPER_STALE_ISSUE_BURST_ACK": "30"}, wantClosed: 30, wantOutput: "closing 30 stale issue(s) in citydb, over GC_REAPER_STALE_ISSUE_BURST_MAX=25, as GC_REAPER_STALE_ISSUE_BURST_ACK=30 acknowledges"},
		{name: "acknowledged at a smaller count", count: 30, env: map[string]string{"GC_REAPER_STALE_ISSUE_BURST_ACK": "29"}, wantHeld: true, wantOutput: "GC_REAPER_STALE_ISSUE_BURST_ACK=29 does not match the 30 stale issue(s) in citydb"},
		{name: "acknowledged at a larger count", count: 30, env: map[string]string{"GC_REAPER_STALE_ISSUE_BURST_ACK": "31"}, wantHeld: true, wantOutput: "GC_REAPER_STALE_ISSUE_BURST_ACK=31 does not match the 30 stale issue(s) in citydb"},
		{name: "acknowledged with a value that is not a count", count: 30, env: map[string]string{"GC_REAPER_STALE_ISSUE_BURST_ACK": "all"}, wantHeld: true, wantOutput: "GC_REAPER_STALE_ISSUE_BURST_ACK=all is not a count"},
		{name: "under a raised max", count: 30, env: map[string]string{"GC_REAPER_STALE_ISSUE_BURST_MAX": "40"}, wantClosed: 30},
		{name: "with the guard off", count: 30, env: map[string]string{"GC_REAPER_STALE_ISSUE_BURST_MAX": "0"}, wantClosed: 30},
		{name: "with a max that is not a number", count: 30, env: map[string]string{"GC_REAPER_STALE_ISSUE_BURST_MAX": "lots"}, wantHeld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runReaperStaleBurst(t, tc.count, tc.env)
			if run.closed != tc.wantClosed {
				t.Errorf("closed %d stale issue(s), want %d:\n%s", run.closed, tc.wantClosed, run.gcLog)
			}
			held := strings.Contains(run.gcLog, fmt.Sprintf("stale_issues_held:%d", tc.count))
			escalated := strings.Contains(run.gcLog, "stale-issue burst held=")
			if held != tc.wantHeld || escalated != tc.wantHeld {
				t.Errorf("held %t, escalated %t; want both %t:\n%s", held, escalated, tc.wantHeld, run.gcLog)
			}
			if !tc.wantHeld && !strings.Contains(run.gcLog, "stale_issues_held:0") {
				t.Errorf("the summary does not report stale_issues_held:0:\n%s", run.gcLog)
			}
			if tc.wantOutput != "" && !strings.Contains(run.output, tc.wantOutput) {
				t.Errorf("the run's output does not say %q:\n%s", tc.wantOutput, run.output)
			}
			if tc.wantOutput == "" {
				for _, note := range []string{"does not match", "is not a count", "acknowledges"} {
					if strings.Contains(run.output, note) {
						t.Errorf("the run's output speaks of an acknowledgement (%q) though none was given:\n%s", note, run.output)
					}
				}
			}
			release := fmt.Sprintf("gc order run reaper --var GC_REAPER_STALE_ISSUE_BURST_ACK=%d", tc.count)
			if listed := strings.Contains(run.output, release); listed != tc.wantHeld {
				t.Errorf("the run's output names the release command %q: %t, want %t:\n%s", release, listed, tc.wantHeld, run.output)
			}
		})
	}
}

// TestReaperStaleIssueBurstDryRun: a dry run closes and holds nothing, but
// reports and lists the burst a real run would hold, so a check before a
// storage split sees it coming instead of reading stale_issues_held:0.
func TestReaperStaleIssueBurstDryRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     int
		env       map[string]string
		wantWould int
	}{
		{name: "a burst", count: 98, wantWould: 98},
		{name: "at the default max", count: 25},
		{name: "an acknowledged burst", count: 98, env: map[string]string{"GC_REAPER_STALE_ISSUE_BURST_ACK": "98"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GC_REAPER_DRY_RUN": "1"}
			for k, v := range tc.env {
				env[k] = v
			}
			run := runReaperStaleBurst(t, tc.count, env)
			if run.closed != 0 {
				t.Errorf("a dry run closed %d stale issue(s):\n%s", run.closed, run.gcLog)
			}
			for _, want := range []string{fmt.Sprintf("would_hold_stale_issues:%d", tc.wantWould), "stale_issues_held:0", "(dry run)"} {
				if !strings.Contains(run.gcLog, want) {
					t.Errorf("the dry run's summary does not report %q:\n%s", want, run.gcLog)
				}
			}
			if strings.Contains(run.gcLog, "stale-issue burst held=") || run.outcome != "" {
				t.Errorf("a dry run recorded a hold (outcome %q):\n%s", run.outcome, run.gcLog)
			}
			listed := strings.Contains(run.output, fmt.Sprintf("would hold %d stale issue(s) in citydb", tc.count))
			if listed != (tc.wantWould > 0) || (listed && !strings.Contains(run.output, " ga-s97")) {
				t.Errorf("the dry run listed the burst %t, want %t:\n%s", listed, tc.wantWould > 0, run.output)
			}
		})
	}
}

// TestReaperStaleIssueBurstStaysOneIncidentAsItGrows: a held backlog that
// grows between runs is the same incident, so it is not re-sent every 30
// minutes; releasing it resolves the incident, so the next burst notifies.
func TestReaperStaleIssueBurstStaysOneIncidentAsItGrows(t *testing.T) {
	cityDir := t.TempDir()
	sent := func(run reaperStaleBurstRun) int {
		return strings.Count(run.gcLog, "stale-issue burst held=")
	}
	if first := runReaperStaleBurstIn(t, cityDir, 98, nil); sent(first) != 1 {
		t.Fatalf("the first held burst sent %d escalation(s), want 1:\n%s", sent(first), first.gcLog)
	}
	if grown := runReaperStaleBurstIn(t, cityDir, 99, nil); sent(grown) != 0 || !strings.Contains(grown.gcLog, "stale_issues_held:99") {
		t.Fatalf("a held burst that grew by one was sent again (%d) or not held:\n%s", sent(grown), grown.gcLog)
	}
	if released := runReaperStaleBurstIn(t, cityDir, 99, map[string]string{"GC_REAPER_STALE_ISSUE_BURST_ACK": "99"}); released.closed != 99 {
		t.Fatalf("the acknowledged burst closed %d, want 99:\n%s", released.closed, released.gcLog)
	}
	if again := runReaperStaleBurstIn(t, cityDir, 40, nil); sent(again) != 1 {
		t.Fatalf("a new burst after a release sent %d escalation(s), want 1:\n%s", sent(again), again.gcLog)
	}
}
