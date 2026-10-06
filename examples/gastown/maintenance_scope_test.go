package gastown_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// scopeFixture is a city whose bead scopes are served by the fake `gc bd`
// route (writeMaintenanceGCStub) over a `dolt` stub that plays the store.
// The stub answers every query with an empty result unless the test passes
// extra case arms. It refuses to answer when called outside the gc route
// (GC_FAKE_SCOPE unset): the maintenance orders must never dial Dolt.
type scopeFixture struct {
	cityDir     string
	binDir      string
	gcLog       string
	bdLog       string
	doltLog     string
	outcomeFile string
	env         map[string]string
}

func newScopeFixture(t *testing.T, storeCases string) scopeFixture {
	t.Helper()
	// The orders resolve the city path physically (pwd -P); resolve it here
	// too so path assertions hold where the temp dir is a symlink (macOS).
	cityDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(city dir): %v", err)
	}
	f := scopeFixture{
		cityDir:     cityDir,
		binDir:      t.TempDir(),
		gcLog:       filepath.Join(t.TempDir(), "gc.log"),
		bdLog:       filepath.Join(t.TempDir(), "bd.log"),
		doltLog:     filepath.Join(t.TempDir(), "dolt.log"),
		outcomeFile: filepath.Join(t.TempDir(), "outcome.json"),
	}
	if err := os.MkdirAll(filepath.Join(f.cityDir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	writeExecutable(t, filepath.Join(f.binDir, "dolt"), `#!/bin/sh
if [ -z "${GC_FAKE_SCOPE:-}" ]; then
  printf 'DIRECT DOLT CALL: %s\n' "$*" >> "$DOLT_ARGS_LOG"
  exit 97
fi
printf '%s|%s\n' "$GC_FAKE_SCOPE" "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
`+storeCases+`
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"SELECT * FROM "[!\(]*".issues"*)
    printf '{}\n'
    ;;
  *"SELECT DISTINCT w.id"*)
    printf 'id,owner_id,depth,state,mode\n'
    ;;
  *"SELECT"*)
    printf 'id\n'
    ;;
esac
exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(f.binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)
	f.env = map[string]string{
		"GC_CITY":               f.cityDir,
		"GC_CITY_PATH":          f.cityDir,
		"GC_CALL_LOG":           f.gcLog,
		"BD_CALL_LOG":           f.bdLog,
		"DOLT_ARGS_LOG":         f.doltLog,
		"GC_ORDER_OUTCOME_FILE": f.outcomeFile,
		// A fresh bd backup of the city scope, so the session-prune gate is
		// open unless a test says otherwise.
		"BD_BACKUP_STATUS_JSON": freshBackupStatusJSON(time.Hour, false),
		"PATH":                  f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	return f
}

func (f scopeFixture) read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

func (f scopeFixture) runReaper(t *testing.T) {
	t.Helper()
	runScript(t, coreScriptPath("reaper.sh"), f.env)
	if direct := f.read(t, f.doltLog); strings.Contains(direct, "DIRECT DOLT CALL") {
		t.Fatalf("reaper dialed Dolt directly instead of going through gc bd:\n%s", direct)
	}
}

type scriptOutcome struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Scopes  []struct {
		Scope  string `json:"scope"`
		Reason string `json:"reason"`
	} `json:"scopes"`
}

func (f scopeFixture) outcome(t *testing.T) (scriptOutcome, bool) {
	t.Helper()
	data := f.read(t, f.outcomeFile)
	if strings.TrimSpace(data) == "" {
		return scriptOutcome{}, false
	}
	var out scriptOutcome
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatalf("outcome file is not the typed declaration: %v\n%s", err, data)
	}
	return out, true
}

func (o scriptOutcome) hasScope(scope, reason string) bool {
	for _, s := range o.Scopes {
		if s.Scope == scope && s.Reason == reason {
			return true
		}
	}
	return false
}

var tempNameSuffix = regexp.MustCompile(`\.(tmp\.[A-Za-z0-9]+|[A-Za-z0-9]{10})\b`)

// normalizedBdCalls strips the per-test city path and random temp-file
// suffixes so call logs from different fixtures compare equal.
func normalizedBdCalls(log, cityDir string) string {
	var lines []string
	for _, line := range strings.Split(log, "\n") {
		if !strings.HasPrefix(line, "bd ") && !strings.HasPrefix(line, "rig list") {
			continue
		}
		line = strings.ReplaceAll(line, cityDir, "<city>")
		lines = append(lines, tempNameSuffix.ReplaceAllString(line, ".<tmp>"))
	}
	return strings.Join(lines, "\n")
}

// TestMaintenanceOrdersIgnoreDoltTopology is the no-topology-branching proof:
// whatever the city's beads metadata says (bd-owned proxied, gc-managed
// server, nothing) and whatever Dolt runtime state or port the environment
// carries, the reaper and jsonl-export send the same bd calls through gc and
// never dial Dolt themselves.
func TestMaintenanceOrdersIgnoreDoltTopology(t *testing.T) {
	topologies := []struct {
		name  string
		setup func(t *testing.T, f scopeFixture)
	}{
		{"bd-owned proxied", func(t *testing.T, f scopeFixture) {
			writeScopeMetadata(t, f.cityDir, `{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`)
		}},
		{"gc-managed server with runtime state and projected port", func(t *testing.T, f scopeFixture) {
			writeScopeMetadata(t, f.cityDir, `{"backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
			stateDir := filepath.Join(f.cityDir, ".gc", "runtime", "packs", "dolt")
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			state := `{"running":true,"pid":1,"port":4406,"data_dir":"` + filepath.Join(f.cityDir, ".beads", "dolt") + `"}`
			if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(state), 0o644); err != nil {
				t.Fatal(err)
			}
			f.env["GC_DOLT_PORT"] = "4406"
			f.env["GC_DOLT_HOST"] = "127.0.0.1"
		}},
		{"no metadata", func(_ *testing.T, f scopeFixture) {
			f.env["FAKE_SCOPE_DBS"] = "city=hq " + f.env["FAKE_SCOPE_DBS"]
		}},
	}
	scripts := []struct {
		name string
		path string
		env  func(t *testing.T, f scopeFixture) map[string]string
	}{
		{"reaper", coreScriptPath("reaper.sh"), func(_ *testing.T, _ scopeFixture) map[string]string {
			return nil
		}},
		{"jsonl-export", coreScriptPath("jsonl-export.sh"), func(t *testing.T, f scopeFixture) map[string]string {
			return map[string]string{
				"GC_JSONL_ARCHIVE_REPO":      filepath.Join(f.cityDir, "archive"),
				"GC_JSONL_MAX_PUSH_FAILURES": "99",
				"GIT_CONFIG_GLOBAL":          filepath.Join(t.TempDir(), "gitconfig"),
				"GIT_CONFIG_NOSYSTEM":        "1",
				"GIT_AUTHOR_NAME":            "t",
				"GIT_AUTHOR_EMAIL":           "t@example.invalid",
				"GIT_COMMITTER_NAME":         "t",
				"GIT_COMMITTER_EMAIL":        "t@example.invalid",
			}
		}},
	}
	for _, sc := range scripts {
		t.Run(sc.name, func(t *testing.T) {
			var want string
			for _, topo := range topologies {
				f := newScopeFixture(t, "")
				f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"hq","hq":true},{"name":"api","hq":false}]}`
				f.env["FAKE_SCOPE_DBS"] = strings.TrimSpace(f.env["FAKE_SCOPE_DBS"] + " rig:api=apidb")
				topo.setup(t, f)
				for k, v := range sc.env(t, f) {
					f.env[k] = v
				}
				runScript(t, sc.path, f.env)
				if direct := f.read(t, f.doltLog); strings.Contains(direct, "DIRECT DOLT CALL") {
					t.Fatalf("%s/%s dialed Dolt directly:\n%s", sc.name, topo.name, direct)
				}
				got := normalizedBdCalls(f.read(t, f.gcLog), f.cityDir)
				if !strings.Contains(got, "--rig api") {
					t.Fatalf("%s/%s never visited the rig scope:\n%s", sc.name, topo.name, got)
				}
				if want == "" {
					want = got
					continue
				}
				if got != want {
					t.Fatalf("%s sent different bd calls for topology %q:\n--- got\n%s\n--- want\n%s", sc.name, topo.name, got, want)
				}
			}
		})
	}
}

func writeScopeMetadata(t *testing.T, scopeDir, metadata string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReaperVisitsEveryRigScopeAndClosesExpiredNudgesThere(t *testing.T) {
	f := newScopeFixture(t, `
  *"lbl.label = 'gc:nudge'"*"< UTC_TIMESTAMP()"*)
    if [ "$GC_FAKE_SCOPE" = "rig:api" ]; then
      printf 'id\napi-nudge\n'
    else
      printf 'id\n'
    fi
    ;;`)
	f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false},{"name":"web","hq":false}]}`
	f.env["FAKE_SCOPE_DBS"] = "rig:api=apidb rig:web=webdb"
	f.runReaper(t)

	gcLog := f.read(t, f.gcLog)
	for _, want := range []string{
		"bd --city " + f.cityDir + " --rig api close api-nudge --reason ttl:expired by reaper",
		"--rig web sql --csv",
		"scopes:3",
		"expired:1",
	} {
		if !strings.Contains(gcLog, want) {
			t.Fatalf("gc log missing %q:\n%s", want, gcLog)
		}
	}
	if _, declared := f.outcome(t); declared {
		t.Fatalf("a clean run must not declare a skip:\n%s", f.read(t, f.outcomeFile))
	}
}

func TestReaperReportsUnreachableRigAndStillReapsTheCity(t *testing.T) {
	f := newScopeFixture(t, "")
	f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
	f.env["FAKE_UNREACHABLE_SCOPES"] = "rig:api"
	f.runReaper(t)

	gcLog := f.read(t, f.gcLog)
	if !strings.Contains(gcLog, "scopes:1") || !strings.Contains(gcLog, "rig:api: bead store unreachable through gc bd") {
		t.Fatalf("reaper did not reap the city and report the unreachable rig:\n%s", gcLog)
	}
	out, ok := f.outcome(t)
	if !ok || out.Outcome != "partial" || !out.hasScope("rig:api", "bead store unreachable") {
		t.Fatalf("outcome = %+v (declared %v), want partial naming rig:api", out, ok)
	}
}

func TestReaperDeclaresSkippedWhenNoScopeIsReachable(t *testing.T) {
	f := newScopeFixture(t, "")
	f.env["FAKE_UNREACHABLE_SCOPES"] = "city"
	f.env["FAKE_RIG_LIST_FAIL"] = "1"
	f.runReaper(t)

	out, ok := f.outcome(t)
	if !ok || out.Outcome != "skipped" || !out.hasScope("city", "bead store unreachable") || !out.hasScope("rigs", "rig list unavailable") {
		t.Fatalf("outcome = %+v (declared %v), want skipped naming city and the rig list", out, ok)
	}
}

func TestReaperPurgesClosedWispsThroughBdPurgeOverTheWispsPlane(t *testing.T) {
	f := newScopeFixture(t, "")
	f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
	f.env["FAKE_SCOPE_DBS"] = "rig:api=apidb"
	f.env["BD_PURGE_COUNT"] = "3"
	f.env["GC_REAPER_PURGE_AGE"] = "36h"
	f.runReaper(t)

	bdLog := f.read(t, f.bdLog)
	for _, scope := range []string{"scope=city", "scope=rig:api"} {
		if !strings.Contains(bdLog, scope+" args=purge --wisps-plane --older-than 36h --json --force --limit 500") {
			t.Fatalf("reaper did not purge %s through bd purge over the wisps plane:\n%s", scope, bdLog)
		}
	}
	if !strings.Contains(f.read(t, f.gcLog), "purged:6") {
		t.Fatalf("summary did not add both scopes' purge counts:\n%s", f.read(t, f.gcLog))
	}
}

func TestReaperDryRunPurgePreviewsOnly(t *testing.T) {
	f := newScopeFixture(t, "")
	f.env["GC_REAPER_DRY_RUN"] = "1"
	f.env["BD_PURGE_COUNT"] = "4"
	f.runReaper(t)

	bdLog := f.read(t, f.bdLog)
	if !strings.Contains(bdLog, "args=purge --wisps-plane --older-than 168h --json --dry-run") || strings.Contains(bdLog, "--force") {
		t.Fatalf("dry run must preview the purge without --force:\n%s", bdLog)
	}
	if !strings.Contains(f.read(t, f.gcLog), "would_purge:4") {
		t.Fatalf("dry-run summary missing would_purge:\n%s", f.read(t, f.gcLog))
	}
}

// TestReaperReportsPurgeUnsupportedWithoutEscalating covers a bd whose purge
// cannot select the wisps plane yet (bd v1.3.0): the step is declared skipped
// every run, and nothing escalates.
func TestReaperReportsPurgeUnsupportedWithoutEscalating(t *testing.T) {
	// bd v1.3.0 has neither flag; a build with --wisps-plane but no --limit
	// fails on the second one.
	for _, missing := range []string{"--wisps-plane", "--limit"} {
		t.Run(missing, func(t *testing.T) {
			f := newScopeFixture(t, "")
			f.env["BD_PURGE_FAIL"] = "Error: unknown flag: " + missing
			f.runReaper(t)

			if gcLog := f.read(t, f.gcLog); strings.Contains(gcLog, "ESCALATION") {
				t.Fatalf("an unsupported purge flag must not escalate:\n%s", gcLog)
			}
			out, ok := f.outcome(t)
			if !ok || out.Outcome != "partial" || !out.hasScope("city", "bd-purge-unsupported") {
				t.Fatalf("outcome = %+v (declared %v), want partial with bd-purge-unsupported", out, ok)
			}
		})
	}
}

func TestReaperStaleIssueSelectionIgnoresNullDependencyTargets(t *testing.T) {
	data, err := os.ReadFile(coreScriptPath("reaper.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	// A dependency pointing at a wisp or an external bead has a NULL
	// depends_on_issue_id; without these guards one such row turns the whole
	// NOT IN list to NULL and no stale issue is ever closed.
	for _, want := range []string{"AND d.issue_id IS NOT NULL", "AND d.depends_on_issue_id IS NOT NULL"} {
		if !strings.Contains(script, want) {
			t.Fatalf("stale-issue dependency exclusion missing %q", want)
		}
	}
}

func sessionPruneFixture(t *testing.T, storeCases string) scopeFixture {
	t.Helper()
	f := newScopeFixture(t, storeCases)
	f.env["BD_PRUNE_COUNT"] = "2"
	return f
}

func freshBackupStatusJSON(age time.Duration, fractional bool) string {
	ts := time.Now().UTC().Add(-age).Format(time.RFC3339)
	if fractional {
		ts = time.Now().UTC().Add(-age).Format(time.RFC3339Nano)
	}
	return fmt.Sprintf(`{"backup":{},"dolt":{"configured":true,"last_sync":%q}}`, ts)
}

func TestReaperSessionPruneBackupGateReadsBdBackupStatus(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		wantPrune  bool
		wantAnom   bool
		wantReason string
	}{
		{"fresh dolt backup", map[string]string{"BD_BACKUP_STATUS_JSON": freshBackupStatusJSON(time.Hour, false)}, true, false, ""},
		{"fresh fractional timestamp", map[string]string{"BD_BACKUP_STATUS_JSON": freshBackupStatusJSON(time.Hour, true)}, true, false, ""},
		{"stale backup", map[string]string{"BD_BACKUP_STATUS_JSON": freshBackupStatusJSON(48*time.Hour, false)}, false, true, "session prune skipped: no fresh backup"},
		{"no backup", map[string]string{"BD_BACKUP_STATUS_JSON": `{"backup":{},"dolt":{"configured":false}}`}, false, true, "session prune skipped: no fresh backup"},
		{"backup unsupported by bd for this transport", map[string]string{"BD_BACKUP_STATUS_FAIL": `{"code":"proxy.backup.unsupported","error":"backup status is not supported in proxied-server mode"}`}, false, false, "session prune skipped: bd-backup-unsupported"},
		// A configured Dolt destination is judged on its Dolt sync only: a
		// fresh legacy backup state must not open the gate (doctor's
		// scanBackupFreshness rule).
		{"dolt destination never synced, legacy state fresh", map[string]string{"BD_BACKUP_STATUS_JSON": fmt.Sprintf(`{"backup":{"timestamp":%q},"dolt":{"configured":true}}`, time.Now().UTC().Format(time.RFC3339))}, false, true, "session prune skipped: no fresh backup"},
		{"dolt destination stale, legacy state fresh", map[string]string{"BD_BACKUP_STATUS_JSON": fmt.Sprintf(`{"backup":{"timestamp":%q},"dolt":{"configured":true,"last_sync":%q}}`, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Add(-72*time.Hour).Format(time.RFC3339))}, false, true, "session prune skipped: no fresh backup"},
		{"no dolt destination, legacy state fresh", map[string]string{"BD_BACKUP_STATUS_JSON": fmt.Sprintf(`{"backup":{"timestamp":%q},"dolt":{"configured":false}}`, time.Now().UTC().Format(time.RFC3339))}, true, false, ""},
		{"malformed backup status", map[string]string{"BD_BACKUP_STATUS_JSON": `not json`}, false, true, "session prune skipped: no fresh backup"},
		{"unparseable backup timestamp", map[string]string{"BD_BACKUP_STATUS_JSON": `{"dolt":{"configured":true,"last_sync":"yesterday"}}`}, false, true, "session prune skipped: no fresh backup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := sessionPruneFixture(t, "")
			for k, v := range tc.env {
				f.env[k] = v
			}
			f.runReaper(t)

			bdLog := f.read(t, f.bdLog)
			pruned := strings.Contains(bdLog, "args=prune --pattern gm-* --older-than 720h --force --json")
			if pruned != tc.wantPrune {
				t.Fatalf("prune ran = %v, want %v:\n%s", pruned, tc.wantPrune, bdLog)
			}
			escalated := strings.Contains(f.read(t, f.gcLog), "bulk prune skipped: backup stale or absent")
			if escalated != tc.wantAnom {
				t.Fatalf("backup anomaly escalated = %v, want %v:\n%s", escalated, tc.wantAnom, f.read(t, f.gcLog))
			}
			out, declared := f.outcome(t)
			if tc.wantReason == "" {
				if declared {
					t.Fatalf("unexpected outcome declaration %+v", out)
				}
				return
			}
			if !declared || !out.hasScope("city", tc.wantReason) {
				t.Fatalf("outcome = %+v (declared %v), want city %q", out, declared, tc.wantReason)
			}
		})
	}
}

func TestReaperSessionPrunePatternGateRefusesUnsafePattern(t *testing.T) {
	f := sessionPruneFixture(t, "")
	f.env["BD_BACKUP_STATUS_JSON"] = freshBackupStatusJSON(time.Hour, false)
	f.env["GC_REAPER_SESSION_BEAD_PATTERN"] = "gm-*' OR '1'='1"
	f.runReaper(t)

	if strings.Contains(f.read(t, f.bdLog), "args=prune") {
		t.Fatalf("an unsafe pattern must not reach bd prune:\n%s", f.read(t, f.bdLog))
	}
	if strings.Contains(f.read(t, f.doltLog), "OR '1'='1") {
		t.Fatalf("an unsafe pattern must not be spliced into SQL:\n%s", f.read(t, f.doltLog))
	}
	if !strings.Contains(f.read(t, f.gcLog), "outside the allowed pattern charset") {
		t.Fatalf("pattern refusal was not escalated:\n%s", f.read(t, f.gcLog))
	}
}

func TestReaperSessionPruneTypeScopeGuard(t *testing.T) {
	cases := []struct {
		name      string
		store     string
		wantPrune bool
		wantText  string
	}{
		{"non-session beads match the pattern", `
  *"id LIKE 'gm-%'"*"issue_type != 'session'"*)
    printf 'COUNT(*)\n3\n'
    ;;`, false, "3 non-session bead(s) matching pattern=gm-* would be caught by prune"},
		{"guard count fails", `
  *"id LIKE 'gm-%'"*"issue_type != 'session'"*)
    printf 'table issues does not exist\n' >&2
    exit 1
    ;;`, false, "type-scope guard count could not be computed"},
		{"only sessions match", "", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := sessionPruneFixture(t, tc.store)
			f.env["BD_BACKUP_STATUS_JSON"] = freshBackupStatusJSON(time.Hour, false)
			f.runReaper(t)

			pruned := strings.Contains(f.read(t, f.bdLog), "args=prune --pattern gm-*")
			if pruned != tc.wantPrune {
				t.Fatalf("prune ran = %v, want %v:\n%s", pruned, tc.wantPrune, f.read(t, f.bdLog))
			}
			if tc.wantText != "" && !strings.Contains(f.read(t, f.gcLog), tc.wantText) {
				t.Fatalf("gc log missing %q:\n%s", tc.wantText, f.read(t, f.gcLog))
			}
		})
	}
}

func TestReaperTypeSafeSessionPruneDeletesThroughBd(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "batches")
	f := newScopeFixture(t, `
  *"issue_type='session'"*"LIMIT 500"*)
    n=0
    [ -f "`+stateFile+`" ] && n=$(cat "`+stateFile+`")
    if [ "$n" = "0" ]; then
      printf '1' > "`+stateFile+`"
      printf 'id\ngc-session-1\ngc-session-2\n'
    else
      printf 'id\n'
    fi
    ;;`)
	f.env["GC_REAPER_SESSION_BEAD_PATTERN"] = ""
	f.runReaper(t)

	bdLog := f.read(t, f.bdLog)
	if !strings.Contains(bdLog, "args=delete --force --json --from-file ") {
		t.Fatalf("type-safe session prune did not delete through bd:\n%s", bdLog)
	}
	if strings.Contains(f.read(t, f.doltLog), "DELETE FROM") {
		t.Fatalf("type-safe session prune sent SQL DML:\n%s", f.read(t, f.doltLog))
	}
	if !strings.Contains(f.read(t, f.gcLog), "sessions-pruned:2") {
		t.Fatalf("summary did not count the deleted sessions:\n%s", f.read(t, f.gcLog))
	}
}

func TestReaperWritesNoOutcomeWithoutControllerFile(t *testing.T) {
	f := newScopeFixture(t, "")
	delete(f.env, "GC_ORDER_OUTCOME_FILE")
	f.env["FAKE_UNREACHABLE_SCOPES"] = "city"
	f.runReaper(t)
	if _, err := os.Stat(f.outcomeFile); !os.IsNotExist(err) {
		t.Fatalf("a manual run must not write an outcome file (stat err %v)", err)
	}
}

func jsonlScopeEnv(t *testing.T, f scopeFixture, archive string) {
	t.Helper()
	f.env["GC_JSONL_ARCHIVE_REPO"] = archive
	f.env["GC_JSONL_MAX_PUSH_FAILURES"] = "99"
	f.env["GC_JSONL_SCRUB"] = "false"
	f.env["GIT_CONFIG_GLOBAL"] = filepath.Join(t.TempDir(), "gitconfig")
	f.env["GIT_CONFIG_NOSYSTEM"] = "1"
	f.env["GIT_AUTHOR_NAME"] = "t"
	f.env["GIT_AUTHOR_EMAIL"] = "t@example.invalid"
	f.env["GIT_COMMITTER_NAME"] = "t"
	f.env["GIT_COMMITTER_EMAIL"] = "t@example.invalid"
}

func TestJsonlExportArchivesEveryScopeInBdExportFormat(t *testing.T) {
	f := newScopeFixture(t, `
  *"SELECT * FROM "*"hq"*".issues"*)
    printf '{"rows":[{"id":"hq-1","title":"city work","issue_type":"task"},{"id":"hq-w","title":"a wisp","issue_type":"task","ephemeral":true}]}\n'
    ;;
  *"SELECT * FROM "*"apidb"*".issues"*)
    printf '{"rows":[{"id":"api-1","title":"rig work","issue_type":"bug"}]}\n'
    ;;`)
	f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
	f.env["FAKE_SCOPE_DBS"] = "city=hq rig:api=apidb"
	archive := filepath.Join(f.cityDir, "archive")
	jsonlScopeEnv(t, f, archive)
	runScript(t, coreScriptPath("jsonl-export.sh"), f.env)

	for db, wantIDs := range map[string][]string{"hq": {"hq-1"}, "apidb": {"api-1"}} {
		data, err := exec.Command("git", "-C", archive, "show", "HEAD:"+db+"/issues.jsonl").CombinedOutput()
		if err != nil {
			t.Fatalf("git show %s/issues.jsonl: %v\n%s", db, err, data)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != len(wantIDs) {
			t.Fatalf("%s snapshot has %d records, want %d (wisps-plane rows are not archived):\n%s", db, len(lines), len(wantIDs), data)
		}
		for i, line := range lines {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("%s snapshot line %d is not one JSON object: %v\n%s", db, i, err, line)
			}
			if rec["id"] != wantIDs[i] {
				t.Fatalf("%s snapshot line %d id = %v, want %s", db, i, rec["id"], wantIDs[i])
			}
		}
	}
	if !strings.Contains(f.read(t, f.gcLog), "bd --city "+f.cityDir+" --rig api export --all -o ") {
		t.Fatalf("jsonl-export did not export the rig through gc bd:\n%s", f.read(t, f.gcLog))
	}
}

func TestJsonlExportMovesLegacySnapshotToLegacyDirWithoutDeletingIt(t *testing.T) {
	f := newScopeFixture(t, `
  *"SELECT * FROM "*"beads"*".issues"*)
    printf '{"rows":[{"id":"b-1","title":"one"},{"id":"b-2","title":"two"}]}\n'
    ;;`)
	archive := filepath.Join(f.cityDir, "archive")
	seed := initSeedArchive(t, archive, 2)
	for name, body := range map[string]string{
		"beads/labels.jsonl":       `{"rows":[{"issue_id":"p0","label":"x"}]}`,
		"beads/dependencies.jsonl": `{"rows":[]}`,
		"beads.jsonl":              `{"rows":[{"id":"p0"},{"id":"p1"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(archive, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitIn(t, archive, "add", "-A")
	runGitIn(t, archive, "commit", "-q", "-m", "legacy supplemental files")
	jsonlScopeEnv(t, f, archive)
	runScript(t, coreScriptPath("jsonl-export.sh"), f.env)

	for _, path := range []string{"beads/legacy/issues.jsonl", "beads/legacy/labels.jsonl", "beads/legacy/dependencies.jsonl", "beads/legacy/beads.jsonl"} {
		if out, err := exec.Command("git", "-C", archive, "cat-file", "-e", "HEAD:"+path).CombinedOutput(); err != nil {
			t.Fatalf("legacy snapshot file %s missing from HEAD: %v\n%s", path, err, out)
		}
	}
	legacyIssues, err := exec.Command("git", "-C", archive, "show", "HEAD:beads/legacy/issues.jsonl").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(legacyIssues), `{"rows":[`) {
		t.Fatalf("legacy issues snapshot not preserved verbatim: %v\n%s", err, legacyIssues)
	}
	current, err := exec.Command("git", "-C", archive, "show", "HEAD:beads/issues.jsonl").CombinedOutput()
	if err != nil || strings.Count(strings.TrimSpace(string(current)), "\n") != 1 {
		t.Fatalf("current snapshot should hold 2 bd-export lines: %v\n%s", err, current)
	}
	if out, err := exec.Command("git", "-C", archive, "log", "--format=%H", seed).CombinedOutput(); err != nil {
		t.Fatalf("seed history lost: %v\n%s", err, out)
	}
	// Spike detection read the old {"rows"} baseline (2) and matched it.
	if strings.Contains(f.read(t, f.gcLog), "JSONL spike") {
		t.Fatalf("switching formats must not trip the spike check:\n%s", f.read(t, f.gcLog))
	}
}

func TestJsonlExportDeclaresUnreachableScope(t *testing.T) {
	f := newScopeFixture(t, "")
	f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
	f.env["FAKE_UNREACHABLE_SCOPES"] = "rig:api"
	archive := filepath.Join(f.cityDir, "archive")
	jsonlScopeEnv(t, f, archive)
	runScript(t, coreScriptPath("jsonl-export.sh"), f.env)

	out, ok := f.outcome(t)
	if !ok || out.Outcome != "partial" || !out.hasScope("rig:api", "bead store unreachable") {
		t.Fatalf("outcome = %+v (declared %v), want partial naming rig:api", out, ok)
	}
	if !strings.Contains(f.read(t, f.gcLog), "failed: rig:api") {
		t.Fatalf("summary should list the unreachable scope as failed:\n%s", f.read(t, f.gcLog))
	}
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestMaintenanceOrdersSkipNonBdStoresQuietly covers a city whose beads
// provider is not bd (file- or exec-backed): gc bd refuses the scope, and the
// orders declare it skipped without escalating every cooldown.
func TestMaintenanceOrdersSkipNonBdStoresQuietly(t *testing.T) {
	for _, script := range []string{"reaper.sh", "jsonl-export.sh"} {
		t.Run(script, func(t *testing.T) {
			f := newScopeFixture(t, "")
			f.env["FAKE_NOT_BD_SCOPES"] = "city"
			jsonlScopeEnv(t, f, filepath.Join(f.cityDir, "archive"))
			runScript(t, coreScriptPath(script), f.env)

			if gcLog := f.read(t, f.gcLog); strings.Contains(gcLog, "ESCALATION") {
				t.Fatalf("%s escalated a non-bd city:\n%s", script, gcLog)
			}
			out, ok := f.outcome(t)
			if !ok || out.Outcome != "skipped" || !out.hasScope("city", "not a bd bead store") {
				t.Fatalf("outcome = %+v (declared %v), want skipped with city not a bd bead store", out, ok)
			}
		})
	}
}

func TestReaperPurgesBacklogInBoundedBatches(t *testing.T) {
	f := newScopeFixture(t, "")
	f.env["BD_PURGE_COUNT"] = "500"
	f.env["BD_PURGE_STATE"] = filepath.Join(t.TempDir(), "purge-calls")
	f.env["BD_PURGE_MORE_BATCHES"] = "2"
	f.runReaper(t)

	if got := strings.Count(f.read(t, f.bdLog), "args=purge --wisps-plane"); got != 3 {
		t.Fatalf("purge ran %d batch(es), want 3 (two reported more, the third did not):\n%s", got, f.read(t, f.bdLog))
	}
	if !strings.Contains(f.read(t, f.gcLog), "purged:1500") {
		t.Fatalf("summary did not add every batch:\n%s", f.read(t, f.gcLog))
	}
	if _, declared := f.outcome(t); declared {
		t.Fatalf("a drained backlog must not declare a skip:\n%s", f.read(t, f.outcomeFile))
	}
}

func TestReaperStopsPurgingAtItsBudgetAndDeclaresTheBacklog(t *testing.T) {
	f := newScopeFixture(t, "")
	f.env["BD_PURGE_COUNT"] = "500"
	f.env["BD_PURGE_STATE"] = filepath.Join(t.TempDir(), "purge-calls")
	f.env["BD_PURGE_MORE_BATCHES"] = "99"
	f.env["GC_REAPER_PURGE_BUDGET_SECS"] = "0"
	f.runReaper(t)

	if got := strings.Count(f.read(t, f.bdLog), "args=purge --wisps-plane"); got != 1 {
		t.Fatalf("purge ran %d batch(es) with no budget left, want 1:\n%s", got, f.read(t, f.bdLog))
	}
	out, ok := f.outcome(t)
	if !ok || !out.hasScope("city", "purge backlog remains after this run's purge budget") {
		t.Fatalf("outcome = %+v (declared %v), want the remaining purge backlog declared", out, ok)
	}
	if strings.Contains(f.read(t, f.gcLog), "ESCALATION") {
		t.Fatalf("a purge backlog is not an anomaly:\n%s", f.read(t, f.gcLog))
	}
}

// writeChildAwareBdStub installs a bd double that, like real bd, refuses a
// bare `close` of a bead that still has open children (BD_CHILDREN="p:c ...",
// open until closed), rejects every close of an id in BD_REJECT_CLOSE (a
// bead re-claimed since selection), and records every close in order. A --force close of a
// bead with an open child is logged as FORCED-WITH-OPEN-CHILD so tests can
// assert the reaper never does that.
func writeChildAwareBdStub(t *testing.T, binDir string) (closedLog string) {
	t.Helper()
	closedLog = filepath.Join(t.TempDir(), "closed.log")
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf 'scope=%s args=%s\n' "${GC_FAKE_SCOPE:-}" "$*" >> "${BD_CALL_LOG:-/dev/null}"
case "$1" in
  purge) printf '{"purged_count":0,"has_more":false}\n'; exit 0 ;;
  prune) printf '{"pruned_count":0}\n'; exit 0 ;;
  backup) printf '%s\n' "$BD_BACKUP_STATUS_JSON"; exit 0 ;;
  close) ;;
  *) exit 0 ;;
esac
shift
force=""
ids=""
while [ $# -gt 0 ]; do
  case "$1" in
    --force) force=1 ;;
    --reason) shift ;;
    -*) ;;
    *) ids="$ids $1" ;;
  esac
  shift
done
sep=""
printf '['
for id in $ids; do
  open_child=""
  for pair in ${BD_CHILDREN:-}; do
    parent="${pair%%:*}"
    child="${pair#*:}"
    if [ "$parent" = "$id" ] && ! grep -qx "$child" "`+closedLog+`" 2>/dev/null; then
      open_child="$child"
    fi
  done
  case " ${BD_REJECT_CLOSE:-} " in
    *" $id "*)
      printf 'cannot close %s: claimed by another actor since it was selected\n' "$id" >&2
      continue
      ;;
  esac
  if [ -n "$open_child" ] && [ -z "$force" ]; then
    printf 'cannot close %s: 1 open child issue(s); close children first or use --force to override\n' "$id" >&2
    continue
  fi
  if [ -n "$open_child" ]; then
    printf 'FORCED-WITH-OPEN-CHILD %s\n' "$id" >> "`+closedLog+`.violations"
  fi
  printf '%s\n' "$id" >> "`+closedLog+`"
  printf '%s{"id":"%s"}' "$sep" "$id"
  sep=","
done
printf ']\n'
exit 0
`)
	return closedLog
}

// TestReaperClosesAnOrphanedWispSubtreeLeafFirst is the root closed -> mid
// open -> leaf open chain: bd refuses to close mid while leaf is open, so the
// subtree must be closed deepest-first. An assigned node is closed with
// --force only once its own children are closed.
func TestReaperClosesAnOrphanedWispSubtreeLeafFirst(t *testing.T) {
	f := newScopeFixture(t, `
  *"SELECT COUNT(*) FROM"*"wisps"*"issue_type NOT IN ('message')"*"created_at <"*)
    printf 'COUNT(*)\n4\n'
    ;;
  *"reap_roots"*)
    printf 'id,owner_id,depth,state,mode\n'
    printf 'w-mid,,0,ok,force\n'
    printf 'w-leaf,w-mid,1,ok,bare\n'
    printf 'w-leaf2,w-mid,1,ok,bare\n'
    printf 'w-grandleaf,w-leaf,2,ok,bare\n'
    ;;`)
	closed := writeChildAwareBdStub(t, f.binDir)
	f.env["BD_CHILDREN"] = "w-mid:w-leaf w-mid:w-leaf2 w-leaf:w-grandleaf"
	f.runReaper(t)

	got := strings.Fields(f.read(t, closed))
	want := []string{"w-grandleaf", "w-leaf", "w-leaf2", "w-mid"}
	if len(got) != 4 || got[0] != "w-grandleaf" || got[3] != "w-mid" {
		t.Fatalf("close order = %v, want deepest first ending with the subtree root (%v)\ngc log:\n%s\nbd log:\n%s", got, want, f.read(t, f.gcLog), f.read(t, f.bdLog))
	}
	if v := f.read(t, closed+".violations"); v != "" {
		t.Fatalf("reaper force-closed a bead that still had an open child:\n%s", v)
	}
	gcLog := f.read(t, f.gcLog)
	if strings.Contains(gcLog, "ESCALATION") || !strings.Contains(gcLog, "closed_wisps:4") {
		t.Fatalf("reaper did not close the whole subtree cleanly:\n%s", gcLog)
	}
	if !strings.Contains(f.read(t, f.bdLog), "args=close w-mid --force --reason") {
		t.Fatalf("the assigned subtree root was not force-closed after its children:\n%s", f.read(t, f.bdLog))
	}
}

// A subtree node with a descendant the reaper must not close (too young, not
// a wisp, or in a status the reaper leaves alone) keeps its whole ancestry
// open, without escalating: nothing is ever closed over a live child.
func TestReaperKeepsAncestorsOfALiveDescendantOpen(t *testing.T) {
	f := newScopeFixture(t, `
  *"SELECT COUNT(*) FROM"*"wisps"*"issue_type NOT IN ('message')"*"created_at <"*)
    printf 'COUNT(*)\n4\n'
    ;;
  *"reap_roots"*)
    printf 'id,owner_id,depth,state,mode\n'
    printf 'w-mid,,0,ok,bare\n'
    printf 'w-leaf,w-mid,1,ok,bare\n'
    printf 'w-young,w-leaf,2,keep,bare\n'
    printf 'w-other,,0,ok,bare\n'
    ;;`)
	closed := writeChildAwareBdStub(t, f.binDir)
	f.env["BD_CHILDREN"] = "w-mid:w-leaf w-leaf:w-young"
	// Two runs: the held subtree is reported every run (summary count and
	// outcome scope), never closed, and never escalated.
	for run := 1; run <= 2; run++ {
		for _, path := range []string{f.outcomeFile, closed} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		f.runReaper(t)

		got := strings.Fields(f.read(t, closed))
		if len(got) != 1 || got[0] != "w-other" {
			t.Fatalf("run %d: closed = %v, want only the unrelated w-other (w-mid and w-leaf sit over a live wisp)", run, got)
		}
		gcLog := f.read(t, f.gcLog)
		if strings.Contains(gcLog, "ESCALATION") {
			t.Fatalf("run %d: holding a subtree over a live wisp is not an anomaly:\n%s", run, gcLog)
		}
		if strings.Count(gcLog, "held_wisps:2") != run {
			t.Fatalf("run %d: summary does not report held_wisps:2 on every run:\n%s", run, gcLog)
		}
		out, ok := f.outcome(t)
		if !ok || !out.hasScope("city", "2 stale wisp(s) held open under a live descendant") {
			t.Fatalf("run %d: outcome = %+v (declared %v), want the held subtree named", run, out, ok)
		}
	}
}

// TestReaperNeverClosesAnOwnerOverARejectedChildClose covers a close bd
// rejects at run time (the leaf was re-claimed after selection): the leaf
// stays open, so its owner -- even an assigned one that would go out with
// --force -- must not be closed this run.
func TestReaperNeverClosesAnOwnerOverARejectedChildClose(t *testing.T) {
	f := newScopeFixture(t, `
  *"SELECT COUNT(*) FROM"*"wisps"*"issue_type NOT IN ('message')"*"created_at <"*)
    printf 'COUNT(*)\n3\n'
    ;;
  *"reap_roots"*)
    printf 'id,owner_id,depth,state,mode\n'
    printf 'w-mid,,0,ok,force\n'
    printf 'w-leaf,w-mid,1,ok,bare\n'
    printf 'w-other,,0,ok,bare\n'
    ;;`)
	closed := writeChildAwareBdStub(t, f.binDir)
	f.env["BD_CHILDREN"] = "w-mid:w-leaf"
	f.env["BD_REJECT_CLOSE"] = "w-leaf"
	f.runReaper(t)

	got := strings.Fields(f.read(t, closed))
	if len(got) != 1 || got[0] != "w-other" {
		t.Fatalf("closed = %v, want only w-other: w-mid's child w-leaf is still open", got)
	}
	if v := f.read(t, closed+".violations"); v != "" {
		t.Fatalf("reaper force-closed a bead over its open child:\n%s", v)
	}
	if strings.Contains(f.read(t, f.bdLog), "close w-mid") {
		t.Fatalf("reaper tried to close w-mid after its child's close failed:\n%s", f.read(t, f.bdLog))
	}
	if !strings.Contains(f.read(t, f.gcLog), "w-leaf") {
		t.Fatalf("the rejected close was not reported:\n%s", f.read(t, f.gcLog))
	}
}

// TestReaperDryRunPreviewsTheSessionPrune: without --force bd prune refuses
// (exit 1, "would prune N"), so a dry run must ask for bd's --dry-run preview
// and report its count, not escalate.
func TestReaperDryRunPreviewsTheSessionPrune(t *testing.T) {
	f := sessionPruneFixture(t, "")
	f.env["GC_REAPER_DRY_RUN"] = "1"
	writeExecutable(t, filepath.Join(f.binDir, "bd"), `#!/bin/sh
printf 'args=%s\n' "$*" >> "${BD_CALL_LOG:-/dev/null}"
case "$1" in
  purge) printf '{"dry_run":true,"purge_count":0}\n' ;;
  backup) printf '%s\n' "$BD_BACKUP_STATUS_JSON" ;;
  prune)
    case " $* " in
      *" --dry-run "*) printf '{"dry_run":true,"prune_count":3}\n' ;;
      *" --force "*) printf '{"pruned_count":3}\n' ;;
      *) printf '{"error":"would prune 3 bead(s)"}\n'; exit 1 ;;
    esac
    ;;
esac
exit 0
`)
	f.runReaper(t)

	bdLog := f.read(t, f.bdLog)
	if !strings.Contains(bdLog, "args=prune --pattern gm-* --older-than 720h --dry-run --json") || strings.Contains(bdLog, "--force") {
		t.Fatalf("dry run did not preview the prune with --dry-run:\n%s", bdLog)
	}
	gcLog := f.read(t, f.gcLog)
	if strings.Contains(gcLog, "ESCALATION") {
		t.Fatalf("a dry-run prune preview must not escalate:\n%s", gcLog)
	}
	if !strings.Contains(gcLog, "sessions-pruned:3") {
		t.Fatalf("dry-run summary did not report bd's preview count:\n%s", gcLog)
	}
}

func TestReaperRunBudgetDeclaresPartialAndCarriesWorkForward(t *testing.T) {
	f := newScopeFixture(t, "")
	f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
	f.env["FAKE_SCOPE_DBS"] = "rig:api=apidb"
	f.env["GC_REAPER_RUN_BUDGET_SECS"] = "0"
	f.runReaper(t)

	if strings.Contains(f.read(t, f.gcLog), "--rig api purge") {
		t.Fatalf("a spent run budget must not start new work:\n%s", f.read(t, f.gcLog))
	}
	// Nothing was reaped at all, so the run is "skipped"; each scope it did
	// not reach is named and picked up by the next run.
	out, ok := f.outcome(t)
	if !ok || out.Outcome != "skipped" || !out.hasScope("rig:api", "run budget exhausted before this scope") {
		t.Fatalf("outcome = %+v (declared %v), want skipped carrying the rig to the next run", out, ok)
	}
	if strings.Contains(f.read(t, f.gcLog), "ESCALATION") {
		t.Fatalf("a spent run budget is not an anomaly:\n%s", f.read(t, f.gcLog))
	}
}

func TestReaperRecordsFailedSessionPruneAsAnAnomaly(t *testing.T) {
	f := sessionPruneFixture(t, "")
	writeExecutable(t, filepath.Join(f.binDir, "bd"), `#!/bin/sh
printf 'args=%s\n' "$*" >> "${BD_CALL_LOG:-/dev/null}"
case "$1" in
  purge) printf '{"purged_count":0,"has_more":false}\n' ;;
  backup) printf '%s\n' "$BD_BACKUP_STATUS_JSON" ;;
  prune) printf 'Error: prune failed: database is locked\n' >&2; exit 1 ;;
esac
exit 0
`)
	f.runReaper(t)

	if gcLog := f.read(t, f.gcLog); !strings.Contains(gcLog, "session bead prune failed (pattern=gm-*)") || !strings.Contains(gcLog, "database is locked") {
		t.Fatalf("a failed bd prune must be escalated with bd's error, not counted as 0:\n%s", gcLog)
	}
}
