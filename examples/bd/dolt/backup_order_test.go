package dolt_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// backupFakeGC is a gc test double for mol-dog-backup.sh. It serves
// `gc rig list --json` and the `gc bd --city C [--rig R] ...` verbs the backup
// order uses (sql SELECT DATABASE(), backup status/init/sync), plus the mail
// and nudge calls, and logs every call. Per-scope behavior is configured
// through environment variables so one double covers every case:
//
//	FAKE_RIG_LIST_JSON       gc rig list --json output (default: no rigs)
//	FAKE_SCOPE_DBS           "city=prod rig:api=apidb" (default city=prod)
//	FAKE_NOT_BD_SCOPES       scopes gc refuses as not bd-backed (file provider)
//	FAKE_UNREACHABLE_SCOPES  scopes whose bd calls fail
//	FAKE_UNSUPPORTED_SCOPES  scopes where backup is refused (proxied, bd v1.3.0)
//	FAKE_CONFIGURED_DBS      databases that already have a backup destination
//	FAKE_STATUS_FAIL_DBS     databases whose backup status fails
//	FAKE_INIT_FAIL_DBS       databases whose backup init fails
//	FAKE_FAILURE_DETAIL      text appended to the unreachable, status, and init errors
//	FAKE_SYNC_FAIL_UNTIL     fail each database's first N syncs (default 0)
//	FAKE_SYNC_STDERR         stderr line a failing sync prints (default: a closed connection)
//	FAKE_SYNC_FIRST_STDERR   overrides FAKE_SYNC_STDERR on a database's first sync
//	FAKE_SYNC_STARTED/FAKE_SYNC_RELEASE  block sync until the release file exists
//	FAKE_MAIL_EXIT           exit status of every gc mail call (default 0)
type backupFakeGC struct {
	logPath string
}

func writeBackupFakeGC(t *testing.T, binDir string) backupFakeGC {
	t.Helper()
	logPath := filepath.Join(binDir, "gc.log")
	stateDir := filepath.Join(binDir, "fake-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir fake state: %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "gc"), fmt.Sprintf(`#!/usr/bin/env bash
set -u
log=%s
state=%s
printf 'gc %%s\n' "$*" >> "$log"
if [ "${1:-}" = "rig" ] && [ "${2:-}" = "list" ]; then
  printf '%%s\n' "${FAKE_RIG_LIST_JSON:-{\"rigs\":[]\}}"
  exit 0
fi
if [ "${1:-}" = "mail" ]; then
  exit "${FAKE_MAIL_EXIT:-0}"
fi
if [ "${1:-}" != "bd" ]; then
  exit 0
fi
# Real gc prints config warnings on stderr; the order must parse only stdout.
printf 'warning: fake gc stderr noise\n' >&2
shift
scope=city
if [ "${1:-}" = "--city" ]; then shift 2; fi
if [ "${1:-}" = "--rig" ]; then scope="rig:$2"; shift 2; fi
case " ${FAKE_NOT_BD_SCOPES:-} " in
  *" $scope "*) printf 'gc bd: only supported for bd-backed beads providers (resolved "file" for %%s)\n' "$scope" >&2; exit 1 ;;
esac
case " ${FAKE_UNREACHABLE_SCOPES:-} " in
  *" $scope "*) printf 'fake gc: %%s unreachable%%s\n' "$scope" "${FAKE_FAILURE_DETAIL:+ $FAKE_FAILURE_DETAIL}" >&2; exit 1 ;;
esac
db=prod
for pair in ${FAKE_SCOPE_DBS:-}; do
  case "$pair" in "$scope="*) db="${pair#*=}" ;; esac
done
case "${1:-} ${2:-}" in
  "sql --csv")
    printf 'DATABASE()\n%%s\n' "$db"
    ;;
  "backup "*)
    case " ${FAKE_UNSUPPORTED_SCOPES:-} " in
      *" $scope "*)
        printf '{"code":"proxy.backup.unsupported","error":"backup %%s is not supported in proxied-server mode","mutates":false,"schema_version":1}\n' "$2"
        exit 1
        ;;
    esac
    case "$2" in
      status)
        case " ${FAKE_STATUS_FAIL_DBS:-} " in
          *" $db "*) printf 'Error: backup status failed for %%s%%s\n' "$db" "${FAKE_FAILURE_DETAIL:+ $FAKE_FAILURE_DETAIL}" >&2; exit 1 ;;
        esac
        configured=false
        case " ${FAKE_CONFIGURED_DBS:-} " in *" $db "*) configured=true ;; esac
        [ -f "$state/$db.configured" ] && configured=true
        printf '{"backup":{},"dolt":{"configured":%%s}}\n' "$configured"
        ;;
      init)
        case " ${FAKE_INIT_FAIL_DBS:-} " in
          *" $db "*) printf 'Error: failed to add backup destination for %%s%%s\n' "$db" "${FAKE_FAILURE_DETAIL:+ $FAKE_FAILURE_DETAIL}" >&2; exit 1 ;;
        esac
        printf '%%s\n' "$3" > "$state/$db.configured"
        printf '{"backup_name":"default","backup_url":"%%s","initialized":true}\n' "$3"
        ;;
      sync)
        printf 'x' >> "$state/$db.syncs"
        if [ -n "${FAKE_SYNC_STARTED:-}" ]; then
          : > "$FAKE_SYNC_STARTED"
          while [ ! -f "$FAKE_SYNC_RELEASE" ]; do sleep 0.05; done
        fi
        attempts=$(wc -c < "$state/$db.syncs" | tr -d ' ')
        if [ "$attempts" -le "${FAKE_SYNC_FAIL_UNTIL:-0}" ]; then
          sync_err='Error: backup sync failed: Error 1105 (HY000): connection was closed'
          if [ -n "${FAKE_SYNC_STDERR:-}" ]; then sync_err=$FAKE_SYNC_STDERR; fi
          if [ "$attempts" -eq 1 ] && [ -n "${FAKE_SYNC_FIRST_STDERR:-}" ]; then sync_err=$FAKE_SYNC_FIRST_STDERR; fi
          printf '%%s\n' "$sync_err" >&2
          exit 1
        fi
        printf '{"synced":true,"duration":"1ms"}\n'
        ;;
    esac
    ;;
esac
exit 0
`, shellQuote(logPath), shellQuote(stateDir)))
	return backupFakeGC{logPath: logPath}
}

func (f backupFakeGC) log(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	return string(data)
}

func syncAttempts(t *testing.T, binDir, db string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(binDir, "fake-state", db+".syncs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read sync attempts for %s: %v", db, err)
	}
	return len(data)
}

// runBackupOrder runs mol-dog-backup.sh against a city whose bead scopes are
// served by the fake gc. It never sets any Dolt endpoint: the order must not
// need one.
func runBackupOrder(t *testing.T, binDir, cityPath string, extraEnv ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("bash", filepath.Join(root, "assets", "scripts", "mol-dog-backup.sh"))
	cmd.Env = append(filteredEnv(
		"PATH", "GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST",
		"GC_DOLT_DATA_DIR", "GC_BACKUP_DATABASES", "GC_BACKUP_OFFSITE_PATH",
		"GC_BACKUP_OFFSITE_TIMEOUT", "GC_BACKUP_ARTIFACT_DIR", "GC_ESCALATE_SCRIPT",
		"GC_ESCALATE_SEARCH_PACKS", "GC_ESCALATION_RECIPIENT", "GC_SYSTEM_PACKS_DIR",
		"DOLT_ESCALATE_SCRIPT", "GC_MAINTENANCE_DONE_TARGET", "GC_ORDER_OUTCOME_FILE",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustRunBackupOrder(t *testing.T, binDir, cityPath string, extraEnv ...string) string {
	t.Helper()
	out, err := runBackupOrder(t, binDir, cityPath, extraEnv...)
	if err != nil {
		t.Fatalf("mol-dog-backup.sh failed: %v\n%s", err, out)
	}
	return out
}

type backupOutcome struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Scopes  []struct {
		Scope  string `json:"scope"`
		Reason string `json:"reason"`
	} `json:"scopes"`
}

func readBackupOutcome(t *testing.T, path string) (backupOutcome, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) || (err == nil && len(strings.TrimSpace(string(data))) == 0) {
		return backupOutcome{}, false
	}
	if err != nil {
		t.Fatalf("read outcome file: %v", err)
	}
	var out backupOutcome
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("outcome file is not the typed declaration: %v\n%s", err, data)
	}
	return out, true
}

func TestBackupOrderSyncsEveryScopeThroughGCBdAndPublishesOffsite(t *testing.T) {
	cityPath := resolvedTempDir(t)
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	offsiteDir := filepath.Join(t.TempDir(), "offsite")
	for _, dir := range []string{artifactDir, offsiteDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)
	rsyncLog := writeBackupFakeRsyncTool(t, binDir, 0)
	// A dolt binary that fails loudly proves the order never dials Dolt itself.
	writeExecutable(t, filepath.Join(binDir, "dolt"), "#!/bin/sh\necho 'dolt must not be called' >&2\nexit 97\n")

	out := mustRunBackupOrder(t, binDir, cityPath,
		`FAKE_RIG_LIST_JSON={"rigs":[{"name":"hq","hq":true},{"name":"api","hq":false}]}`,
		"FAKE_SCOPE_DBS=city=prod rig:api=apidb",
		"FAKE_CONFIGURED_DBS=prod apidb",
		"GC_BACKUP_OFFSITE_PATH="+offsiteDir,
	)
	if !strings.Contains(out, "synced: 2/2") || !strings.Contains(out, "offsite: ok") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	log := gc.log(t)
	for _, want := range []string{
		"gc bd --city " + cityPath + " backup sync --json",
		"gc bd --city " + cityPath + " --rig api backup sync --json",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("gc log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "--rig hq") {
		t.Fatalf("the HQ row is the city scope, not a rig:\n%s", log)
	}
	rsync, err := os.ReadFile(rsyncLog)
	if err != nil {
		t.Fatalf("read rsync log: %v", err)
	}
	if !strings.Contains(string(rsync), artifactDir+"/ "+offsiteDir+"/") {
		t.Fatalf("offsite rsync should publish the artifact dir:\n%s", rsync)
	}
}

func TestBackupOrderRegistersMissingDestinationUnderArtifactDir(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)

	out := mustRunBackupOrder(t, binDir, cityPath,
		`FAKE_RIG_LIST_JSON={"rigs":[{"name":"archive","hq":false}]}`,
		"FAKE_SCOPE_DBS=city=prod rig:archive=archive",
		"FAKE_CONFIGURED_DBS=prod",
	)
	if !strings.Contains(out, "synced: 2/2") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	if !strings.Contains(out, "auto-configured missing backup destination") {
		t.Fatalf("auto-configuration must be logged loudly:\n%s", out)
	}
	log := gc.log(t)
	artifactURL := "file://" + filepath.Join(cityPath, ".dolt-backup", "archive")
	if !strings.Contains(log, "--rig archive backup init "+artifactURL+" --json") {
		t.Fatalf("gc log missing backup init for archive -> %s:\n%s", artifactURL, log)
	}
	if strings.Contains(log, "gc bd --city "+cityPath+" backup init") {
		t.Fatalf("the city scope already has a destination; init must not run for it:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".dolt-backup", "archive")); err != nil {
		t.Fatalf("backup artifact dir for archive should be created: %v", err)
	}
}

func TestBackupOrderCountsFailedDestinationRegistration(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)
	outcomeFile := filepath.Join(t.TempDir(), "outcome.json")

	out := mustRunBackupOrder(t, binDir, cityPath,
		`FAKE_RIG_LIST_JSON={"rigs":[{"name":"archive","hq":false}]}`,
		"FAKE_SCOPE_DBS=city=prod rig:archive=archive",
		"FAKE_CONFIGURED_DBS=prod",
		"FAKE_INIT_FAIL_DBS=archive",
		"GC_ORDER_OUTCOME_FILE="+outcomeFile,
	)
	if !strings.Contains(out, "synced: 1/2") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	if got := syncAttempts(t, binDir, "archive"); got != 0 {
		t.Fatalf("sync must not run for a scope whose destination could not be registered (ran %d times)", got)
	}
	log := gc.log(t)
	for _, want := range []string{"1/2 databases failed to sync", "archive(backup init failed)", "failed to add backup destination for archive"} {
		if !strings.Contains(log, want) {
			t.Fatalf("failure escalation missing %q:\n%s", want, log)
		}
	}
	outcome, ok := readBackupOutcome(t, outcomeFile)
	if !ok || outcome.Outcome != "partial" || len(outcome.Scopes) != 1 || outcome.Scopes[0].Scope != "rig:archive" {
		t.Fatalf("outcome = %+v (declared %v), want partial naming rig:archive", outcome, ok)
	}
}

func TestBackupOrderEscalatesFailedSyncWithDiagnostic(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)

	out := mustRunBackupOrder(t, binDir, cityPath,
		"FAKE_CONFIGURED_DBS=prod",
		"FAKE_SYNC_FAIL_UNTIL=99",
		"GC_DOLT_BACKUP_SYNC_ATTEMPTS=2",
	)
	if !strings.Contains(out, "synced: 0/1") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	if got := syncAttempts(t, binDir, "prod"); got != 2 {
		t.Fatalf("backup sync attempted %d times, want GC_DOLT_BACKUP_SYNC_ATTEMPTS=2", got)
	}
	log := gc.log(t)
	for _, want := range []string{
		"mail send human -s Dolt backup: 1/1 databases failed to sync [MEDIUM]",
		"connection was closed",
		"read_timeout_millis",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("failure escalation missing %q:\n%s", want, log)
		}
	}
}

func TestBackupOrderEscalatesFirstSyncFailureWithCredentialsRedacted(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)

	out := mustRunBackupOrder(t, binDir, cityPath,
		"FAKE_CONFIGURED_DBS=prod",
		"FAKE_SYNC_FAIL_UNTIL=99",
		"GC_DOLT_BACKUP_SYNC_ATTEMPTS=2",
		`FAKE_SYNC_FIRST_STDERR=sync failed Password=first-secret postgres://user:first-url-secret@example.test/repo {"token":"json-secret"} --password cli-secret Authorization: Bearer bearer-secret`,
		"FAKE_SYNC_STDERR=later failure TOKEN=second-secret",
	)
	if got := syncAttempts(t, binDir, "prod"); got != 2 {
		t.Fatalf("backup sync attempted %d times, want GC_DOLT_BACKUP_SYNC_ATTEMPTS=2", got)
	}
	log := gc.log(t)
	if !strings.Contains(log, "mail send human -s Dolt backup: 1/1 databases failed to sync [MEDIUM]") {
		t.Fatalf("sync failure did not escalate:\n%s", log)
	}
	for _, secret := range []string{"first-secret", "first-url-secret", "json-secret", "cli-secret", "bearer-secret", "second-secret"} {
		if strings.Contains(log, secret) || strings.Contains(out, secret) {
			t.Fatalf("sync stderr credential %q leaked into the order output or escalation:\nlog:\n%s\noutput:\n%s", secret, log, out)
		}
	}
	for _, want := range []string{
		"sync failed Password=[redacted]",
		"postgres://[redacted]@example.test/repo",
		`"token":"[redacted]"`,
		"--password [redacted]",
		"Authorization: [redacted]",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("escalation missing the first attempt's redacted diagnostic %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "later failure") {
		t.Fatalf("escalation must report the first failed attempt, not a later one:\n%s", log)
	}
}

func TestBackupOrderRaisesUnchangedSyncFailureAsOneIncident(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)
	failing := []string{"FAKE_CONFIGURED_DBS=prod", "FAKE_SYNC_FAIL_UNTIL=99", "GC_DOLT_BACKUP_SYNC_ATTEMPTS=1"}
	const notified = "mail send human -s Dolt backup: 1/1 databases failed to sync [MEDIUM]"

	mustRunBackupOrder(t, binDir, cityPath, failing...)
	mustRunBackupOrder(t, binDir, cityPath, failing...)
	if got := strings.Count(gc.log(t), notified); got != 1 {
		t.Fatalf("an unchanged sync failure notified %d times over two runs, want 1:\n%s", got, gc.log(t))
	}
	states, err := filepath.Glob(filepath.Join(cityPath, ".gc", "incidents", "*.state"))
	if err != nil {
		t.Fatalf("glob incident state: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("incident state files = %v, want one incident for the sync failure", states)
	}

	mustRunBackupOrder(t, binDir, cityPath, append(failing, "FAKE_SYNC_STDERR=Error: backup sync failed: remote rejected the push")...)
	if got := strings.Count(gc.log(t), notified); got != 2 {
		t.Fatalf("a changed sync failure notified %d times in total, want a second notification:\n%s", got, gc.log(t))
	}
}

// TestBackupOrderAlertsWhenASyncFailureReturnsAfterRecovery pins the recovery
// half of the incident. A clean run resolves it, so the same failure coming
// back inside the reminder interval notifies again rather than being
// suppressed as unchanged.
func TestBackupOrderAlertsWhenASyncFailureReturnsAfterRecovery(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)
	const notified = "mail send human -s Dolt backup: 1/1 databases failed to sync [MEDIUM]"
	// The fake counts sync attempts across runs, so FAKE_SYNC_FAIL_UNTIL=99
	// fails every later run and 0 lets the next one succeed.
	run := func(failUntil string) {
		t.Helper()
		mustRunBackupOrder(t, binDir, cityPath,
			"FAKE_CONFIGURED_DBS=prod", "GC_DOLT_BACKUP_SYNC_ATTEMPTS=1", "FAKE_SYNC_FAIL_UNTIL="+failUntil)
	}

	run("99")
	run("0")
	if got := strings.Count(gc.log(t), notified); got != 1 {
		t.Fatalf("fail then recover notified %d times, want 1:\n%s", got, gc.log(t))
	}
	run("99")
	if got := strings.Count(gc.log(t), notified); got != 2 {
		t.Fatalf("a failure that returned after a clean run notified %d times in total, want 2:\n%s", got, gc.log(t))
	}
}

// Argument parsers an escalate.sh override written to the 1.5.0 contract,
// before incident keys, can have. Each is the shell an override runs for an
// argument it does not take, which it sees as $1.
const (
	// The 1.5.0 escalate.sh's own parser, which overrides were copied from.
	escalateRefusalV150 = `echo "escalate: unknown argument $1" >&2; exit 2`
	// A GNU getopt parser names the option it does not know and exits 1.
	escalateRefusalGetopt = `echo "escalate: unrecognized option '$1'" >&2; exit 1`
	// A usage message that names no flag, under the usage-error status.
	escalateRefusalUsage = `echo "usage: escalate.sh --subject S [--message M]" >&2; exit 2`
	// A lenient parser skips what it does not take.
	escalateSkipsUnknown = `shift`
)

// What an override does once it has read its arguments.
const (
	escalateDelivers  = `printf 'SENT %s\n' "$subject" >> "$ESCALATE_OVERRIDE_LOG"`
	escalateSendFails = `echo "escalate: pager unreachable" >&2; exit 1`
)

// writeEscalateOverride writes an escalate.sh override and returns its path,
// its log, and the environment that points the order at it. The override
// takes the 1.5.0 contract's flags (--subject, --message, --severity), runs
// refusal for any other argument, then runs deliver. Its log holds a
// "CALL <flags>" line per call and a "SENT <subject>" line per delivered
// alert.
func writeEscalateOverride(t *testing.T, refusal, deliver string) (script, log string, env []string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "escalate.sh")
	log = filepath.Join(dir, "escalate.log")
	writeExecutable(t, script, `#!/usr/bin/env bash
flags=""
for arg in "$@"; do
    case "$arg" in --*) flags="$flags $arg" ;; esac
done
printf 'CALL%s\n' "$flags" >> "$ESCALATE_OVERRIDE_LOG"
subject=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        --subject) subject="$2"; shift 2 ;;
        --message|-m|--severity) shift 2 ;;
        *) `+refusal+` ;;
    esac
done
`+deliver+"\n")
	return script, log, []string{"GC_ESCALATE_SCRIPT=" + script, "ESCALATE_OVERRIDE_LOG=" + log}
}

func readEscalateLog(t *testing.T, log string) string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read escalate override log: %v", err)
	}
	return string(data)
}

// failingSyncRun is the environment of a backup run whose one database fails
// its one sync attempt, followed by extra.
func failingSyncRun(extra ...string) []string {
	return append([]string{"FAKE_CONFIGURED_DBS=prod", "FAKE_SYNC_FAIL_UNTIL=99", "GC_DOLT_BACKUP_SYNC_ATTEMPTS=1"}, extra...)
}

// TestBackupOrderAlertsThroughAnEscalateOverrideThatPredatesIncidentKeys
// covers a city whose escalate.sh override was written before incident keys
// and refuses --incident-key as a usage error. The order sends the sync
// failure again in the 1.5.0 form, so it still arrives, undeduplicated, and
// the run says so in one line.
func TestBackupOrderAlertsThroughAnEscalateOverrideThatPredatesIncidentKeys(t *testing.T) {
	cases := []struct {
		name    string
		refusal string
	}{
		{"1.5.0 escalate.sh parser, exit 2", escalateRefusalV150},
		{"getopt parser naming the flag, exit 1", escalateRefusalGetopt},
		{"usage error naming no flag, exit 2", escalateRefusalUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := resolvedTempDir(t)
			binDir := t.TempDir()
			writeBackupFakeGC(t, binDir)
			script, log, env := writeEscalateOverride(t, tc.refusal, escalateDelivers)

			out := mustRunBackupOrder(t, binDir, cityPath, failingSyncRun(env...)...)

			want := "CALL --subject --message --incident-key --fingerprint\n" +
				"CALL --subject --message\n" +
				"SENT Dolt backup: 1/1 databases failed to sync [MEDIUM]\n"
			if got := readEscalateLog(t, log); got != want {
				t.Fatalf("override calls:\n%s\nwant the refused keyed call, then one call in the 1.5.0 form that delivers:\n%s", got, want)
			}
			if got := strings.Count(out, "predates incident keys"); got != 1 {
				t.Fatalf("%d warnings that the override predates incident keys, want 1:\n%s", got, out)
			}
			if !strings.Contains(out, script+" predates incident keys") {
				t.Fatalf("the warning must name the override:\n%s", out)
			}
		})
	}
}

// TestBackupOrderClearsNoIncidentOnAnEscalateOverrideThatPredatesIncidentKeys:
// such an override refuses a clean run's --resolve-incident too. It keeps no
// incidents, so there is nothing to clear or resend: the run sends nothing
// and reports the override in one line instead of its usage error.
func TestBackupOrderClearsNoIncidentOnAnEscalateOverrideThatPredatesIncidentKeys(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	writeBackupFakeGC(t, binDir)
	_, log, env := writeEscalateOverride(t, escalateRefusalV150, escalateDelivers)

	out := mustRunBackupOrder(t, binDir, cityPath, append([]string{"FAKE_CONFIGURED_DBS=prod"}, env...)...)

	if !strings.Contains(out, "synced: 1/1") {
		t.Fatalf("want a clean run:\n%s", out)
	}
	if got, want := readEscalateLog(t, log), "CALL --resolve-incident\n"; got != want {
		t.Fatalf("override calls:\n%s\nwant only the clean run's resolve:\n%s", got, want)
	}
	if got := strings.Count(out, "predates incident keys"); got != 1 {
		t.Fatalf("%d warnings that the override predates incident keys, want 1:\n%s", got, out)
	}
	if strings.Contains(out, "unknown argument") {
		t.Fatalf("the override's usage error must be replaced by the warning:\n%s", out)
	}
}

// TestBackupOrderDoesNotResendWhenAnEscalateOverrideFailsToSend: an override
// that fails for a reason other than the incident flags is not sent the
// alert again, and its own error reaches the order log.
func TestBackupOrderDoesNotResendWhenAnEscalateOverrideFailsToSend(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	writeBackupFakeGC(t, binDir)
	_, log, env := writeEscalateOverride(t, escalateSkipsUnknown, escalateSendFails)

	out := mustRunBackupOrder(t, binDir, cityPath, failingSyncRun(env...)...)

	if got, want := readEscalateLog(t, log), "CALL --subject --message --incident-key --fingerprint\n"; got != want {
		t.Fatalf("override calls:\n%s\nwant the one keyed call:\n%s", got, want)
	}
	if strings.Contains(out, "predates incident keys") {
		t.Fatalf("a failed send is not a refusal of the incident flags:\n%s", out)
	}
	if !strings.Contains(out, "escalate: pager unreachable") {
		t.Fatalf("the override's error must reach the order log:\n%s", out)
	}
}

// TestBackupOrderNeverResendsThroughTheCoreEscalateScript: the core
// escalate.sh takes the incident flags, so its failure, even under the
// usage-error status, is a failed send and the alert is not sent again
// without them.
func TestBackupOrderNeverResendsThroughTheCoreEscalateScript(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)

	out := mustRunBackupOrder(t, binDir, cityPath, failingSyncRun("FAKE_MAIL_EXIT=2")...)

	if got := strings.Count(gc.log(t), "gc mail send "); got != 1 {
		t.Fatalf("%d mail sends, want the one failed keyed escalation:\n%s", got, gc.log(t))
	}
	if strings.Contains(out, "predates incident keys") {
		t.Fatalf("the core escalate.sh takes the incident flags:\n%s", out)
	}
}

// TestBackupOrderRedactsEveryCredentialShape pins the redactor against each
// credential shape a sync error can carry. Each shape runs in its own city so
// a match in one row cannot mask a miss in another, and both the escalation
// and the order log must keep the key and lose the value.
func TestBackupOrderRedactsEveryCredentialShape(t *testing.T) {
	cases := []struct {
		name     string
		stderr   string
		secret   string
		redacted string
	}{
		{"authorization with any scheme", "Authorization: Basic basic-cred-sentinel", "basic-cred-sentinel", "Authorization: [redacted]"},
		{"quoted multi-word value", `password: "two-word secret-sentinel-dq"`, "secret-sentinel-dq", "password: [redacted]"},
		{"spaces around the separator", "token = space-sentinel", "space-sentinel", "token = [redacted]"},
		{"prefixed JSON token key", `{"access_token":"json-prefix-sentinel"}`, "json-prefix-sentinel", `"access_token":"[redacted]"`},
		{"prefixed JSON secret key", `{"client_secret": "json-secret-sentinel"}`, "json-secret-sentinel", `"client_secret": "[redacted]"`},
		{"secret access key", "aws_secret_access_key=aws-sentinel", "aws-sentinel", "aws_secret_access_key=[redacted]"},
		{"prefixed flag", "--db-password cli-prefix-sentinel", "cli-prefix-sentinel", "--db-password [redacted]"},
		{"JSON authorization", `"Authorization": "Bearer json-auth-sentinel"`, "json-auth-sentinel", `"Authorization": "[redacted]"`},
		{"URL userinfo with @ in the password", "https://user:p@ss-sentinel@example.test/repo", "ss-sentinel", "https://[redacted]@example.test/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := resolvedTempDir(t)
			binDir := t.TempDir()
			gc := writeBackupFakeGC(t, binDir)

			out := mustRunBackupOrder(t, binDir, cityPath,
				"FAKE_CONFIGURED_DBS=prod",
				"FAKE_SYNC_FAIL_UNTIL=99",
				"GC_DOLT_BACKUP_SYNC_ATTEMPTS=1",
				"FAKE_SYNC_STDERR=Error: backup sync failed: "+tc.stderr,
			)
			log := gc.log(t)
			if strings.Contains(log, tc.secret) || strings.Contains(out, tc.secret) {
				t.Fatalf("credential %q leaked:\nlog:\n%s\noutput:\n%s", tc.secret, log, out)
			}
			if !strings.Contains(log, tc.redacted) || !strings.Contains(out, tc.redacted) {
				t.Fatalf("want %q in both the escalation and the order log:\nlog:\n%s\noutput:\n%s", tc.redacted, log, out)
			}
		})
	}
}

// TestBackupOrderRedactsEveryFailureDiagnostic pins that redaction covers
// every diagnostic the escalation carries, not only sync stderr. A failed
// backup status, an unreachable scope, and a failed destination registration
// each reach the mail through the same filter.
func TestBackupOrderRedactsEveryFailureDiagnostic(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)

	out := mustRunBackupOrder(t, binDir, cityPath,
		`FAKE_RIG_LIST_JSON={"rigs":[{"name":"api","hq":false},{"name":"archive","hq":false}]}`,
		"FAKE_SCOPE_DBS=city=prod rig:api=apidb rig:archive=archive",
		"FAKE_STATUS_FAIL_DBS=prod",
		"FAKE_UNREACHABLE_SCOPES=rig:api",
		"FAKE_INIT_FAIL_DBS=archive",
		"FAKE_FAILURE_DETAIL=token=diag-token-sentinel Authorization: Basic diag-basic-sentinel",
	)
	if !strings.Contains(out, "synced: 0/3") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	log := gc.log(t)
	for _, secret := range []string{"diag-token-sentinel", "diag-basic-sentinel"} {
		if strings.Contains(log, secret) || strings.Contains(out, secret) {
			t.Fatalf("diagnostic credential %q leaked:\nlog:\n%s\noutput:\n%s", secret, log, out)
		}
	}
	for _, want := range []string{
		"backup status failed for prod token=[redacted] Authorization: [redacted]",
		"rig:api unreachable token=[redacted] Authorization: [redacted]",
		"failed to add backup destination for archive token=[redacted] Authorization: [redacted]",
		"common credential shapes redacted",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("escalation missing %q:\n%s", want, log)
		}
	}
}

func TestBackupOrderRetriesMarginalSyncFailure(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)

	out := mustRunBackupOrder(t, binDir, cityPath, "FAKE_CONFIGURED_DBS=prod", "FAKE_SYNC_FAIL_UNTIL=1")
	if !strings.Contains(out, "synced: 1/1") {
		t.Fatalf("a sync that succeeds on retry must count as synced:\n%s", out)
	}
	if got := syncAttempts(t, binDir, "prod"); got != 2 {
		t.Fatalf("backup sync attempted %d times, want 2 (one failure then one success)", got)
	}
	if strings.Contains(gc.log(t), "databases failed to sync") {
		t.Fatalf("a scope that succeeded on retry must not escalate:\n%s", gc.log(t))
	}
}

func TestBackupOrderReportsUnsupportedBackupAsSkippedWithoutEscalating(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)
	outcomeFile := filepath.Join(t.TempDir(), "outcome.json")

	out := mustRunBackupOrder(t, binDir, cityPath,
		`FAKE_RIG_LIST_JSON={"rigs":[{"name":"api","hq":false}]}`,
		"FAKE_SCOPE_DBS=city=prod rig:api=apidb",
		"FAKE_CONFIGURED_DBS=apidb",
		"FAKE_UNSUPPORTED_SCOPES=city",
		"GC_ORDER_OUTCOME_FILE="+outcomeFile,
	)
	if !strings.Contains(out, "synced: 1/2") || !strings.Contains(out, "unsupported: 1") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	if strings.Contains(gc.log(t), "mail send") {
		t.Fatalf("a scope bd cannot back up yet must not escalate every run:\n%s", gc.log(t))
	}
	outcome, ok := readBackupOutcome(t, outcomeFile)
	if !ok || outcome.Outcome != "partial" || len(outcome.Scopes) != 1 ||
		outcome.Scopes[0].Scope != "city" || outcome.Scopes[0].Reason != "bd-backup-unsupported" {
		t.Fatalf("outcome = %+v (declared %v), want partial with city bd-backup-unsupported", outcome, ok)
	}
}

func TestBackupOrderUnreachableScopeIsFailedAndDeclared(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)
	outcomeFile := filepath.Join(t.TempDir(), "outcome.json")

	out := mustRunBackupOrder(t, binDir, cityPath,
		`FAKE_RIG_LIST_JSON={"rigs":[{"name":"api","hq":false}]}`,
		"FAKE_SCOPE_DBS=city=prod rig:api=apidb",
		"FAKE_CONFIGURED_DBS=prod apidb",
		"FAKE_UNREACHABLE_SCOPES=rig:api",
		"GC_ORDER_OUTCOME_FILE="+outcomeFile,
	)
	if !strings.Contains(out, "synced: 1/2") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	if !strings.Contains(gc.log(t), "rig:api(unreachable)") {
		t.Fatalf("failure escalation should name the unreachable scope:\n%s", gc.log(t))
	}
	outcome, ok := readBackupOutcome(t, outcomeFile)
	if !ok || outcome.Outcome != "partial" || outcome.Scopes[0].Scope != "rig:api" {
		t.Fatalf("outcome = %+v (declared %v), want partial naming rig:api", outcome, ok)
	}
}

func TestBackupOrderHonorsDatabaseFilter(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	_ = writeBackupFakeGC(t, binDir)

	out := mustRunBackupOrder(t, binDir, cityPath,
		`FAKE_RIG_LIST_JSON={"rigs":[{"name":"api","hq":false}]}`,
		"FAKE_SCOPE_DBS=city=prod rig:api=apidb",
		"FAKE_CONFIGURED_DBS=prod apidb",
		"GC_BACKUP_DATABASES=apidb",
	)
	if !strings.Contains(out, "synced: 1/1") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	if got := syncAttempts(t, binDir, "prod"); got != 0 {
		t.Fatalf("GC_BACKUP_DATABASES=apidb must not sync prod (synced %d times)", got)
	}
}

func TestBackupOrderEscalatesOffsiteFailureWithConfiguredBound(t *testing.T) {
	cityPath := resolvedTempDir(t)
	offsiteDir := filepath.Join(t.TempDir(), "offsite")
	if err := os.MkdirAll(filepath.Join(cityPath, ".dolt-backup"), 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)
	_ = writeBackupFakeRsyncTool(t, binDir, 1)
	timeoutLog := writeRecordingTimeout(t, binDir)

	out := mustRunBackupOrder(t, binDir, cityPath,
		"FAKE_CONFIGURED_DBS=prod",
		"GC_BACKUP_OFFSITE_PATH="+offsiteDir,
		"GC_BACKUP_OFFSITE_TIMEOUT=17",
	)
	if !strings.Contains(out, "synced: 1/1") || !strings.Contains(out, "offsite: failed") {
		t.Fatalf("offsite failure should stay non-fatal and visible in the summary:\n%s", out)
	}
	timeoutData, err := os.ReadFile(timeoutLog)
	if err != nil {
		t.Fatalf("read timeout log: %v", err)
	}
	if !strings.Contains(string(timeoutData), "--kill-after=2 17 rsync -a --delete") {
		t.Fatalf("offsite rsync did not use GC_BACKUP_OFFSITE_TIMEOUT=17:\n%s", timeoutData)
	}
	log := gc.log(t)
	for _, want := range []string{
		"mail send human -s Dolt backup: offsite publication failed [MEDIUM]",
		"Status: failed. Bound: 17s (raise with GC_BACKUP_OFFSITE_TIMEOUT).",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("offsite failure escalation missing %q:\n%s", want, log)
		}
	}
}

func TestBackupOrderRejectsUnusableOffsiteTimeout(t *testing.T) {
	// 0 is the dangerous one: GNU `timeout 0` drops the bound entirely while
	// the python3 fallback expires immediately. Both it and a non-numeric value
	// must fall back to the documented 300s default.
	for _, configured := range []string{"0", "not-a-number"} {
		t.Run(configured, func(t *testing.T) {
			cityPath := resolvedTempDir(t)
			offsiteDir := filepath.Join(t.TempDir(), "offsite")
			if err := os.MkdirAll(filepath.Join(cityPath, ".dolt-backup"), 0o755); err != nil {
				t.Fatalf("mkdir artifact dir: %v", err)
			}
			binDir := t.TempDir()
			_ = writeBackupFakeGC(t, binDir)
			_ = writeBackupFakeRsyncTool(t, binDir, 0)
			timeoutLog := writeRecordingTimeout(t, binDir)

			out := mustRunBackupOrder(t, binDir, cityPath,
				"FAKE_CONFIGURED_DBS=prod",
				"GC_BACKUP_OFFSITE_PATH="+offsiteDir,
				"GC_BACKUP_OFFSITE_TIMEOUT="+configured,
			)
			if !strings.Contains(out, "offsite: ok") {
				t.Fatalf("offsite rsync should still run with an unusable bound:\n%s", out)
			}
			timeoutData, err := os.ReadFile(timeoutLog)
			if err != nil {
				t.Fatalf("read timeout log: %v", err)
			}
			if !strings.Contains(string(timeoutData), "--kill-after=2 300 rsync -a --delete") {
				t.Fatalf("GC_BACKUP_OFFSITE_TIMEOUT=%q should fall back to 300s:\n%s", configured, timeoutData)
			}
		})
	}
}

func TestBackupOrderSkipsConcurrentRunBeforeBackupSync(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not installed; skipping")
	}
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	_ = writeBackupFakeGC(t, binDir)
	startedFile := filepath.Join(binDir, "sync-started")
	releaseFile := filepath.Join(binDir, "sync-release")

	firstDone := make(chan struct{})
	var firstOut string
	var firstErr error
	go func() {
		firstOut, firstErr = runBackupOrder(t, binDir, cityPath,
			"FAKE_CONFIGURED_DBS=prod",
			"FAKE_SYNC_STARTED="+startedFile,
			"FAKE_SYNC_RELEASE="+releaseFile,
		)
		close(firstDone)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(startedFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first backup run did not reach backup sync")
		}
		time.Sleep(25 * time.Millisecond)
	}

	secondOut, secondErr := runBackupOrder(t, binDir, cityPath,
		"FAKE_CONFIGURED_DBS=prod",
		"GC_DOLT_BACKUP_LOCK_WAIT_SECONDS=0",
	)
	if err := os.WriteFile(releaseFile, []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("release first backup run: %v", err)
	}
	if secondErr != nil {
		t.Fatalf("second backup run failed: %v\n%s", secondErr, secondOut)
	}
	if !strings.Contains(secondOut, "already running") {
		t.Fatalf("second backup run should skip while the lock is held:\n%s", secondOut)
	}
	select {
	case <-firstDone:
	case <-time.After(10 * time.Second):
		t.Fatal("first backup run did not finish after release")
	}
	if firstErr != nil {
		t.Fatalf("first backup run failed: %v\n%s", firstErr, firstOut)
	}
	if got := syncAttempts(t, binDir, "prod"); got != 1 {
		t.Fatalf("backup sync count = %d, want 1 while the concurrent run skipped", got)
	}
}

func writeBackupFakeRsyncTool(t *testing.T, binDir string, exitCode int) string {
	t.Helper()
	logPath := filepath.Join(binDir, "rsync.log")
	writeExecutable(t, filepath.Join(binDir, "rsync"), fmt.Sprintf(`#!/bin/sh
printf 'rsync %%s\n' "$*" >> %s
exit %d
`, shellQuote(logPath), exitCode))
	return logPath
}

func writeRecordingTimeout(t *testing.T, binDir string) string {
	t.Helper()
	logPath := filepath.Join(binDir, "timeout.log")
	writeExecutable(t, filepath.Join(binDir, "timeout"), fmt.Sprintf(`#!/bin/sh
printf 'timeout %%s\n' "$*" >> %s
[ "$1" = "--kill-after=2" ] && shift
shift
exec "$@"
`, shellQuote(logPath)))
	return logPath
}

func TestBackupOrderSkipsNonBdStoresQuietly(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	gc := writeBackupFakeGC(t, binDir)
	outcomeFile := filepath.Join(t.TempDir(), "outcome.json")

	out := mustRunBackupOrder(t, binDir, cityPath, "FAKE_NOT_BD_SCOPES=city", "GC_ORDER_OUTCOME_FILE="+outcomeFile)
	if !strings.Contains(out, "synced: 0/0") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	if strings.Contains(gc.log(t), "mail send") {
		t.Fatalf("a non-bd city must not escalate:\n%s", gc.log(t))
	}
	outcome, ok := readBackupOutcome(t, outcomeFile)
	if !ok || outcome.Outcome != "skipped" || outcome.Scopes[0].Reason != "not a bd bead store" {
		t.Fatalf("outcome = %+v (declared %v), want skipped with not a bd bead store", outcome, ok)
	}
}

// resolvedTempDir returns a t.TempDir with symlinks resolved: the order
// resolves the city path physically (pwd -P), and macOS temp dirs live under
// the /var -> /private/var symlink.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temp dir): %v", err)
	}
	return dir
}
