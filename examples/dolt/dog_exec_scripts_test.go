package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

func runDogScriptCommand(t *testing.T, scriptName, binDir, cityPath, dataDir string, extraEnv ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("bash", filepath.Join(root, "assets", "scripts", scriptName))
	cmd.Env = append(filteredEnv(
		"PATH",
		"GC_CITY_PATH",
		"GC_PACK_DIR",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_PORT",
		"GC_DOLT_HOST",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"GC_BACKUP_DATABASES",
		"GC_BACKUP_OFFSITE_PATH",
		"GC_BACKUP_ARTIFACT_DIR",
		"GC_PHANTOM_DATA_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_PORT=3307",
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runDogScript(t *testing.T, scriptName, binDir, cityPath, dataDir string, extraEnv ...string) string {
	t.Helper()
	out, err := runDogScriptCommand(t, scriptName, binDir, cityPath, dataDir, extraEnv...)
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", scriptName, err, out)
	}
	return out
}

func writeDogFakeGC(t *testing.T, binDir string) string {
	t.Helper()
	logPath := filepath.Join(binDir, "gc.log")
	writeExecutable(t, filepath.Join(binDir, "gc"), fmt.Sprintf(`#!/bin/sh
printf 'gc %s\n' "$*" >> %s
exit 0
`, "%s", shellQuote(logPath)))
	return logPath
}

func TestDogExecScriptsAreBashSyntaxValid(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	root := repoRoot(t)
	for _, scriptName := range []string{
		"mol-dog-backup.sh",
		"mol-dog-doctor.sh",
		"mol-dog-phantom-db.sh",
	} {
		t.Run(scriptName, func(t *testing.T) {
			cmd := exec.Command("bash", "-n", filepath.Join(root, "assets", "scripts", scriptName))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("bash -n failed: %v\n%s", err, out)
			}
		})
	}
}

func TestPhantomDBScriptQuarantinesPhantomsAndRetiredReplacements(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	binDir := t.TempDir()
	_ = writeDogFakeGC(t, binDir)

	for _, path := range []string{
		filepath.Join(dataDir, "valid", ".dolt", "noms"),
		filepath.Join(dataDir, "phantom", ".dolt"),
		filepath.Join(dataDir, "orders.replaced-20260509T010203Z", ".dolt", "noms"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	writeTestFile(t, filepath.Join(dataDir, "valid", ".dolt", "noms", "manifest"), "ok")
	writeTestFile(t, filepath.Join(dataDir, "orders.replaced-20260509T010203Z", ".dolt", "noms", "manifest"), "ok")

	out := runDogScript(t, "mol-dog-phantom-db.sh", binDir, cityPath, dataDir)
	if !strings.Contains(out, "phantoms: 1") || !strings.Contains(out, "retired: 1") || !strings.Contains(out, "quarantined: 2") {
		t.Fatalf("unexpected phantom summary:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "phantom")); !os.IsNotExist(err) {
		t.Fatalf("phantom source should be moved, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "orders.replaced-20260509T010203Z")); !os.IsNotExist(err) {
		t.Fatalf("retired replacement source should be moved, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "valid", ".dolt", "noms", "manifest")); err != nil {
		t.Fatalf("valid database should remain: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, ".quarantine", "*"))
	if err != nil {
		t.Fatalf("glob quarantine: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("quarantined entries = %d, want 2: %v", len(matches), matches)
	}
}

func writeBackupFakeDolt(t *testing.T, binDir, version string, syncExit int, sqlDatabases ...string) string {
	t.Helper()
	logPath := filepath.Join(binDir, "dolt.log")
	dbCSV := "Database\n" + strings.Join(sqlDatabases, "\n") + "\n"
	writeExecutable(t, filepath.Join(binDir, "dolt"), fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
printf 'dolt %%s\n' "$*" >> %s
if [ "${1:-}" = "version" ]; then
  printf 'dolt version %%s\n' %s
  exit 0
fi
case "$*" in
  *"SHOW DATABASES"*)
    printf %%s %s
    exit 0
    ;;
esac
if [ "${1:-}" = "backup" ] && [ "$#" -eq 1 ]; then
  db="$(basename "$PWD")"
  printf '%%s-backup file:///backups/%%s\n' "$db" "$db"
  exit 0
fi
if [ "${1:-}" = "remote" ]; then
  printf 'remote should not be used\n' >&2
  exit 64
fi
if [ "${1:-} ${2:-}" = "backup sync" ]; then
  exit %d
fi
exit 0
`, shellQuote(logPath), shellQuote(version), shellQuote(dbCSV), syncExit))
	return logPath
}

func writeBackupFakeRsync(t *testing.T, binDir string) string {
	t.Helper()
	logPath := filepath.Join(binDir, "rsync.log")
	writeExecutable(t, filepath.Join(binDir, "rsync"), fmt.Sprintf(`#!/bin/sh
printf 'rsync %s\n' "$*" >> %s
exit 0
`, "%s", shellQuote(logPath)))
	return logPath
}

func TestBackupScriptSkipsOldDoltBeforeSync(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(filepath.Join(dataDir, "prod", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	binDir := t.TempDir()
	_ = writeDogFakeGC(t, binDir)
	doltLogPath := writeBackupFakeDolt(t, binDir, "1.86.1", 0)

	out, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir, "GC_BACKUP_DATABASES=prod")
	if err == nil {
		t.Fatalf("old Dolt preflight succeeded; want failure\n%s", out)
	}
	if !strings.Contains(out, "dolt-too-old") {
		t.Fatalf("output missing dolt-too-old skip:\n%s", out)
	}
	doltLog, err := os.ReadFile(doltLogPath)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(doltLog), "backup sync") {
		t.Fatalf("old dolt must not reach backup sync:\n%s", doltLog)
	}
}

func TestBackupOrderTimeoutCoversScriptBudget(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "orders", "mol-dog-backup.toml"))
	if err != nil {
		t.Fatalf("read backup order: %v", err)
	}
	order, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("parse backup order: %v", err)
	}

	const intendedDBs = 10
	required := 30*time.Second + intendedDBs*120*time.Second + 300*time.Second
	if got := order.TimeoutOrDefault(); got < required {
		t.Fatalf("backup order timeout = %s, want at least %s for SQL probe + %d DB syncs + offsite rsync", got, required, intendedDBs)
	}
}

func TestBackupScriptDiscoversNamedBackupsAndSyncsArtifactsOffsite(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	offsiteDir := filepath.Join(cityPath, "offsite")
	for _, path := range []string{
		filepath.Join(dataDir, "prod", ".dolt"),
		artifactDir,
		offsiteDir,
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	binDir := t.TempDir()
	_ = writeDogFakeGC(t, binDir)
	doltLogPath := writeBackupFakeDolt(t, binDir, "1.86.2", 0, "prod")
	rsyncLogPath := writeBackupFakeRsync(t, binDir)

	out := runDogScript(t, "mol-dog-backup.sh", binDir, cityPath, dataDir, "GC_BACKUP_OFFSITE_PATH="+offsiteDir)
	if !strings.Contains(out, "synced: 1/1") || !strings.Contains(out, "offsite: ok") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	doltLog, err := os.ReadFile(doltLogPath)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	for _, want := range []string{"SHOW DATABASES", "backup", "backup sync prod-backup"} {
		if !strings.Contains(string(doltLog), want) {
			t.Fatalf("dolt log missing %q:\n%s", want, doltLog)
		}
	}
	if strings.Contains(string(doltLog), "remote") {
		t.Fatalf("backup discovery should not use dolt remote:\n%s", doltLog)
	}
	rsyncLog, err := os.ReadFile(rsyncLogPath)
	if err != nil {
		t.Fatalf("read rsync log: %v", err)
	}
	if !strings.Contains(string(rsyncLog), artifactDir+"/") {
		t.Fatalf("rsync should use backup artifact dir, log:\n%s", rsyncLog)
	}
	if strings.Contains(string(rsyncLog), dataDir+"/") {
		t.Fatalf("rsync must not use live data dir, log:\n%s", rsyncLog)
	}
}

func TestBackupScriptCountsFailedDatabasesByDatabase(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(filepath.Join(dataDir, "prod", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	_ = writeBackupFakeDolt(t, binDir, "1.86.2", 1)

	out := runDogScript(t, "mol-dog-backup.sh", binDir, cityPath, dataDir, "GC_BACKUP_DATABASES=prod")
	if !strings.Contains(out, "synced: 0/1") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(gcLog), "Backup dog: 1/1 databases failed to sync") {
		t.Fatalf("failure mail should count databases, log:\n%s", gcLog)
	}
}

func TestDoctorScriptChecksBackupArtifactFreshnessPerDatabase(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	freshBackup := filepath.Join(artifactDir, "prod.backup")
	writeTestFile(t, freshBackup, "backup")
	fresh := time.Now()
	if err := os.Chtimes(freshBackup, fresh, fresh); err != nil {
		t.Fatalf("chtimes fresh backup: %v", err)
	}
	staleBackup := filepath.Join(artifactDir, "archive.backup")
	writeTestFile(t, staleBackup, "backup")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleBackup, old, old); err != nil {
		t.Fatalf("chtimes stale backup: %v", err)
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nprod\narchive\n'
    exit 0
    ;;
esac
exit 0
`)

	out := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_BACKUP_STALE_S=1")
	if !strings.Contains(out, "server: ok") {
		t.Fatalf("unexpected doctor output:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(gcLog), "archive backup is") {
		t.Fatalf("doctor did not report stale archive backup artifact, log:\n%s", gcLog)
	}
	if strings.Contains(string(gcLog), "prod backup is") {
		t.Fatalf("fresh prod backup should not be reported stale, log:\n%s", gcLog)
	}
}

func TestDoctorScriptIgnoresDocumentedSystemSchemasForBackupFreshness(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	freshBackup := filepath.Join(artifactDir, "prod.backup")
	writeTestFile(t, freshBackup, "backup")
	fresh := time.Now()
	if err := os.Chtimes(freshBackup, fresh, fresh); err != nil {
		t.Fatalf("chtimes fresh backup: %v", err)
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nprod\nperformance_schema\nsys\n'
    exit 0
    ;;
esac
exit 0
`)

	out := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_BACKUP_STALE_S=1")
	if !strings.Contains(out, "server: ok") {
		t.Fatalf("unexpected doctor output:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	for _, systemDB := range []string{"performance_schema", "sys"} {
		if strings.Contains(string(gcLog), systemDB) {
			t.Fatalf("doctor should ignore %s for backup freshness, log:\n%s", systemDB, gcLog)
		}
	}
}

func TestDoctorScriptDoesNotCreditSharedPrefixBackupToDatabase(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	freshSiblingBackup := filepath.Join(artifactDir, "prod_dev.backup")
	writeTestFile(t, freshSiblingBackup, "backup")
	fresh := time.Now()
	if err := os.Chtimes(freshSiblingBackup, fresh, fresh); err != nil {
		t.Fatalf("chtimes fresh sibling backup: %v", err)
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nprod\nprod_dev\n'
    exit 0
    ;;
esac
exit 0
`)

	out := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_BACKUP_STALE_S=1")
	if !strings.Contains(out, "server: ok") {
		t.Fatalf("unexpected doctor output:\n%s", out)
	}
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(gcLog), "prod backup missing") {
		t.Fatalf("doctor should not credit prod_dev backup to prod, log:\n%s", gcLog)
	}
	if strings.Contains(string(gcLog), "prod_dev backup") {
		t.Fatalf("fresh prod_dev backup should not be reported stale, log:\n%s", gcLog)
	}
}
