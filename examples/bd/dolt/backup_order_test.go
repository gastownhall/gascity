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
//	FAKE_INIT_FAIL_DBS       databases whose backup init fails
//	FAKE_SYNC_FAIL_UNTIL     fail each database's first N syncs (default 0)
//	FAKE_SYNC_NOOP_DBS      databases whose existing manifest remains unchanged
//	FAKE_SYNC_STARTED/FAKE_SYNC_RELEASE  block sync until the release file exists
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
  *" $scope "*) printf 'fake gc: %%s unreachable\n' "$scope" >&2; exit 1 ;;
esac
db=prod
for pair in ${FAKE_SCOPE_DBS:-}; do
  case "$pair" in "$scope="*) db="${pair#*=}" ;; esac
done
case "${1:-} ${2:-}" in
  "sql --csv")
    case "${3:-}" in
      *hashof*) printf "hashof('HEAD')\n%%s\n" "${FAKE_SOURCE_HEAD:-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}" ;;
      *) printf 'DATABASE()\n%%s\n' "$db" ;;
    esac
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
        configured=false
        case " ${FAKE_CONFIGURED_DBS:-} " in *" $db "*) configured=true ;; esac
        [ -f "$state/$db.configured" ] && configured=true
        url="${FAKE_BACKUP_URL:-file://${GC_BACKUP_ARTIFACT_DIR:-$GC_CITY_PATH/.dolt-backup}/$db}"
        printf '{"backup":{},"dolt":{"configured":%%s,"backup_url":"%%s"}}\n' "$configured" "$url"
        ;;
      init)
        case " ${FAKE_INIT_FAIL_DBS:-} " in
          *" $db "*) printf 'Error: failed to add backup destination for %%s\n' "$db" >&2; exit 1 ;;
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
          printf 'Error: backup sync failed: Error 1105 (HY000): connection was closed\n' >&2
          exit 1
        fi
        artifact="${GC_BACKUP_ARTIFACT_DIR:-$GC_CITY_PATH/.dolt-backup}"
        mkdir -p "$artifact/$db"
        case " ${FAKE_SYNC_NOOP_DBS:-} " in
          *" $db "*) [ -f "$artifact/$db/manifest" ] || printf 'backup=%%s\n' "$db" > "$artifact/$db/manifest" ;;
          *) printf 'backup=%%s attempt=%%s\n' "$db" "$attempts" > "$artifact/$db/manifest" ;;
        esac
        printf '{"synced":true,"duration":"1ms"}\n'
        ;;
    esac
    ;;
esac
exit 0
`, shellQuote(logPath), shellQuote(stateDir)))
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
case "${1:-} ${2:-}" in
  "backup restore")
    [ "${FAKE_RESTORE_FAIL:-0}" = 1 ] && exit 1
    mkdir -p snapshot
    ;;
  "sql --result-format")
    printf "hashof('HEAD')\n%s\n" "${FAKE_BACKUP_HEAD:-${FAKE_SOURCE_HEAD:-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}}"
    ;;
  *) exit 1 ;;
esac
`)
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
		"GC_CITY_RUNTIME_DIR="+filepath.Join(cityPath, ".gc", "runtime"),
		"GC_PACK_STATE_DIR="+filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"),
		"GC_DOLT_BACKUP_RECEIPT_DIR="+filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "backup-receipts"),
		"GC_BACKUP_ARTIFACT_DIR="+filepath.Join(cityPath, ".dolt-backup"),
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

func TestBackupOrderReceiptsDistinguishNoopChangedAndFailedScopes(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	writeBackupFakeGC(t, binDir)
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	for _, db := range []string{"hq", "office"} {
		path := filepath.Join(artifactDir, db, "manifest")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("old "+db), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-24 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	baseEnv := []string{
		`FAKE_RIG_LIST_JSON={"rigs":[{"name":"office","hq":false}]}`,
		"FAKE_SCOPE_DBS=city=hq rig:office=office",
		"FAKE_CONFIGURED_DBS=hq office",
	}
	out := mustRunBackupOrder(t, binDir, cityPath, append(baseEnv, "FAKE_SYNC_NOOP_DBS=office")...)
	if !strings.Contains(out, "synced: 2/2") {
		t.Fatalf("backup summary: %s", out)
	}
	receiptDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "backup-receipts")
	readReceipt := func(db string) []string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(receiptDir, db))
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(data))
		if len(fields) != 9 || fields[0] != "v3" || fields[6] != fields[7] && fields[1] != "failure" && fields[1] != "unverified" {
			t.Fatalf("invalid %s receipt: %q", db, data)
		}
		return fields
	}
	if got := readReceipt("office")[1]; got != "noop" {
		t.Fatalf("unchanged office outcome = %q, want noop", got)
	}
	if got := readReceipt("hq")[1]; got != "success" {
		t.Fatalf("changed hq outcome = %q, want success", got)
	}
	out = mustRunBackupOrder(t, binDir, cityPath, append(baseEnv,
		"GC_BACKUP_DATABASES=office", "FAKE_SYNC_NOOP_DBS=office", "FAKE_RESTORE_FAIL=1")...)
	if !strings.Contains(out, "synced: 1/1") || readReceipt("office")[1] != "noop" {
		t.Fatalf("previously verified no-op should reuse its manifest evidence: %s", out)
	}
	copiedArtifacts := filepath.Join(cityPath, "copied-artifacts")
	if err := os.MkdirAll(filepath.Join(copiedArtifacts, "office"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalManifest, err := os.ReadFile(filepath.Join(artifactDir, "office", "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copiedArtifacts, "office", "manifest"), originalManifest, 0o644); err != nil {
		t.Fatal(err)
	}
	out = mustRunBackupOrder(t, binDir, cityPath, append(baseEnv,
		"GC_BACKUP_DATABASES=office", "GC_BACKUP_ARTIFACT_DIR="+copiedArtifacts,
		"FAKE_SYNC_NOOP_DBS=office", "FAKE_RESTORE_FAIL=1")...)
	if !strings.Contains(out, "synced: 0/1") || readReceipt("office")[1] != "unverified" {
		t.Fatalf("destination change must force restore even for copied manifest: %s", out)
	}
	out = mustRunBackupOrder(t, binDir, cityPath, append(baseEnv,
		"GC_BACKUP_DATABASES=office", "FAKE_SYNC_NOOP_DBS=office")...)
	if !strings.Contains(out, "synced: 1/1") {
		t.Fatalf("restore original office proof: %s", out)
	}
	beforeOffice, err := os.ReadFile(filepath.Join(receiptDir, "office"))
	if err != nil {
		t.Fatal(err)
	}
	out = mustRunBackupOrder(t, binDir, cityPath, append(baseEnv,
		"GC_BACKUP_DATABASES=hq", "FAKE_SYNC_FAIL_UNTIL=99", "GC_DOLT_BACKUP_SYNC_ATTEMPTS=1")...)
	if !strings.Contains(out, "synced: 0/1") || readReceipt("hq")[1] != "failure" {
		t.Fatalf("failed hq sync did not persist failure: %s", out)
	}
	afterOffice, err := os.ReadFile(filepath.Join(receiptDir, "office"))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterOffice) != string(beforeOffice) {
		t.Fatal("failed hq sync rewrote office receipt")
	}
	out = mustRunBackupOrder(t, binDir, cityPath, append(baseEnv,
		"GC_BACKUP_DATABASES=office", "FAKE_SYNC_NOOP_DBS=office",
		"FAKE_SOURCE_HEAD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"FAKE_BACKUP_HEAD=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")...)
	if !strings.Contains(out, "synced: 0/1") || readReceipt("office")[1] != "unverified" {
		t.Fatalf("changed source with unchanged manifest must fail closed: %s", out)
	}
	out = mustRunBackupOrder(t, binDir, cityPath, append(baseEnv,
		"GC_BACKUP_DATABASES=hq", "FAKE_SOURCE_HEAD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")...)
	if !strings.Contains(out, "synced: 1/1") || readReceipt("hq")[1] != "success" || readReceipt("hq")[6] != strings.Repeat("b", 32) {
		t.Fatalf("changed source and matching restored backup should be fresh: %s", out)
	}
	out = mustRunBackupOrder(t, binDir, cityPath, append(baseEnv,
		"GC_BACKUP_DATABASES=hq", "FAKE_BACKUP_URL=file:///different-backup")...)
	if !strings.Contains(out, "synced: 0/1") || readReceipt("hq")[1] != "failure" {
		t.Fatalf("different backup destination must not credit local manifest: %s", out)
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
	receipt, err := os.ReadFile(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "backup-receipts", "prod"))
	if err != nil || !strings.HasPrefix(string(receipt), "v3 skipped ") {
		t.Fatalf("unsupported scope needs durable skipped receipt: %q (%v)", receipt, err)
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
	script := fmt.Sprintf(`#!/bin/sh
printf 'timeout %%s\n' "$*" >> %s
[ "$1" = "--kill-after=2" ] && shift
shift
exec "$@"
`, shellQuote(logPath))
	for _, name := range []string{"timeout", "gtimeout"} {
		writeExecutable(t, filepath.Join(binDir, name), script)
	}
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
