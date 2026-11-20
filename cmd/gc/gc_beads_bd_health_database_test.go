package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// gcBeadsBdHealthHarness runs op_health from the bundled gc-beads-bd prelude
// for a city whose .beads/metadata.json and config.yaml are given. It returns
// the combined output, which ends in "status=<op_health exit code>".
func gcBeadsBdHealthHarness(t *testing.T, metadata, config, gcBin, dataDir string, env ...string) string {
	t.Helper()
	return gcBeadsBdOpHarness(t, "op_health", "", metadata, config, gcBin, dataDir, env...)
}

// gcBeadsBdRecoverStubs make op_recover take the gc helper recovery path and
// report that recovery succeeded, so the test sees only what op_recover does
// after the helper returns.
const gcBeadsBdRecoverStubs = `is_remote() { return 1; }
recovery_should_skip_due_to_enospc() { return 1; }
recovery_should_skip_due_to_fsync_stall() { return 1; }
load_recover_managed_from_gc() { GC_RECOVER_PORT=3311; GC_RECOVER_DIAGNOSED_READ_ONLY=false; return 0; }
`

// gcBeadsBdOpHarness runs op from the bundled gc-beads-bd prelude with the
// shared health stubs plus stubs, for a city whose .beads/metadata.json and
// config.yaml are given. It returns the combined output, which ends in
// "status=<op exit code>".
func gcBeadsBdOpHarness(t *testing.T, op, stubs, metadata, config, gcBin, dataDir string, env ...string) string {
	t.Helper()
	cityPath := t.TempDir()
	materializeBuiltinPacksForTest(t, cityPath)
	scriptData, err := os.ReadFile(bundledGcBeadsBdScriptForTest(t))
	if err != nil {
		t.Fatalf("ReadFile(gc-beads-bd): %v", err)
	}
	prelude, _, ok := strings.Cut(string(scriptData), "# --- Main ---")
	if !ok {
		t.Fatal("gc-beads-bd script missing main marker")
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if metadata != "" {
		if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	harness := filepath.Join(t.TempDir(), "health-database.sh")
	body := prelude + `
GC_BIN="` + gcBin + `"
GC_DOLT_HOST=""
GC_CITY_PATH="` + cityPath + `"
DATA_DIR="` + dataDir + `"
DOLT_PORT=3311
DOLT_USER=root
tcp_check() { return 0; }
do_query_probe() { return 0; }
check_read_only() { return 1; }
get_connection_count() { return 1; }
` + stubs + `
set +e
(` + op + `)
status=$?
set -e
printf 'status=%s\n' "$status"
`
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return string(runShHarness(t, harness, op, append(sanitizedBaseEnv(env...), "GC_BIN="+gcBin)))
}

// writeFakeHealthCheckGC writes a gc helper whose `dolt-state health-check`
// records its argv and prints report.
func writeFakeHealthCheckGC(t *testing.T, report string) (gcBin, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")
	gcBin = filepath.Join(dir, "gc-helper")
	script := `#!/bin/sh
[ "$1 $2" = "dolt-state health-check" ] || { echo "unexpected gc call: $*" >&2; exit 2; }
printf '%s\n' "$*" >> "` + argvFile + `"
printf '` + strings.ReplaceAll(report, "\t", `\t`) + `'
`
	if err := os.WriteFile(gcBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return gcBin, argvFile
}

// writeFakeHealthCheckGCReports writes a gc helper whose `dolt-state
// health-check` records its argv and prints withWrite when asked for the
// sample write (--check-read-only) and withoutWrite otherwise, as the real
// helper's report differs between the two.
func writeFakeHealthCheckGCReports(t *testing.T, withoutWrite, withWrite string) (gcBin, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")
	gcBin = filepath.Join(dir, "gc-helper")
	reportFile := filepath.Join(dir, "report.txt")
	writeReportFile := filepath.Join(dir, "report-with-write.txt")
	if err := os.WriteFile(reportFile, []byte(withoutWrite), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(writeReportFile, []byte(withWrite), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
[ "$1 $2" = "dolt-state health-check" ] || { echo "unexpected gc call: $*" >&2; exit 2; }
printf '%s\n' "$*" >> "` + argvFile + `"
case " $* " in
  *" --check-read-only "*) exec cat "` + writeReportFile + `" ;;
esac
exec cat "` + reportFile + `"
`
	if err := os.WriteFile(gcBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return gcBin, argvFile
}

// writeFailingHealthCheckGC writes a gc helper whose `dolt-state
// health-check` records its argv and fails the way the real one does when
// the server does not answer SQL: no report and a non-zero exit.
func writeFailingHealthCheckGC(t *testing.T) (gcBin, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")
	gcBin = filepath.Join(dir, "gc-helper")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + argvFile + `"
echo "gc dolt-state health-check: dial tcp 127.0.0.1:3311: connect: connection refused" >&2
exit 1
`
	if err := os.WriteFile(gcBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return gcBin, argvFile
}

// helperDatabaseProbes say how the direct-driver probes answer the real
// health check for the city database hq. A nil error is a probe that
// succeeds.
type helperDatabaseProbes struct {
	head  error
	write error
}

// helperHealthReport runs the real `gc dolt-state health-check` logic for hq
// over direct-driver probes that answer as p says and returns the report the
// helper prints. dataDir, when set, is the managed data dir it checks.
func helperHealthReport(t *testing.T, p helperDatabaseProbes, checkReadOnly bool, dataDir string) string {
	t.Helper()
	t.Setenv("GC_DOLT_PASSWORD", "test-only")
	oldQuery, oldHead, oldWrite := managedDoltQueryProbeDirectFn, managedDoltDatabaseHeadDirectFn, managedDoltReadOnlyStateDirectFn
	oldReadBack, oldCount := managedDoltProbeReadBackDirectFn, managedDoltConnectionCountDirectFn
	t.Cleanup(func() {
		managedDoltQueryProbeDirectFn, managedDoltDatabaseHeadDirectFn, managedDoltReadOnlyStateDirectFn = oldQuery, oldHead, oldWrite
		managedDoltProbeReadBackDirectFn, managedDoltConnectionCountDirectFn = oldReadBack, oldCount
	})
	managedDoltQueryProbeDirectFn = func(_, _, _ string) error { return nil }
	managedDoltDatabaseHeadDirectFn = func(_, _, _, _ string) (string, error) {
		if p.head != nil {
			return "", p.head
		}
		return "lm41v2vo0p5s281qk22bmmqdun6coqlh", nil
	}
	managedDoltReadOnlyStateDirectFn = func(_, _, _, _ string) (string, error) {
		if p.write != nil {
			return "unknown", p.write
		}
		return "false", nil
	}
	managedDoltProbeReadBackDirectFn = func(_, _, _, _ string) error { return nil }
	managedDoltConnectionCountDirectFn = func(_, _, _ string) (string, error) { return "1", nil }

	report, err := managedDoltHealthCheckFor("127.0.0.1", "3311", "root", checkReadOnly, managedDoltHealthTarget{Database: "hq", DataDir: dataDir})
	if err != nil {
		t.Fatalf("managedDoltHealthCheckFor() error = %v, want the database verdict in-band", err)
	}
	return strings.Join(managedDoltHealthCheckFields(report), "\n") + "\n"
}

// writeHealthCheckGCForProbes writes a gc helper that prints the reports the
// real health check prints for hq over probes that answer as p says, with and
// without the sample write.
func writeHealthCheckGCForProbes(t *testing.T, p helperDatabaseProbes, dataDir string) (gcBin, argvFile string) {
	t.Helper()
	return writeFakeHealthCheckGCReports(t, helperHealthReport(t, p, false, dataDir), helperHealthReport(t, p, true, dataDir))
}

const cityMetadataHQ = `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`

func TestGcBeadsBdHealthFailsClosedWhenCityDatabaseUnavailable(t *testing.T) {
	gcBin, argvFile := writeFakeHealthCheckGC(t,
		"query_ready\ttrue\\nread_only\tunknown\\nconnection_count\t5\\ndatabase\thq\\ndatabase_ready\tfalse\\n"+
			"database_error\tdatabase \"hq\" default branch head unresolvable: Error 1049 (HY000): database not found: hq\\n")
	dataDir := t.TempDir()

	out := gcBeadsBdHealthHarness(t, cityMetadataHQ, "issue_prefix: hq\ngc.endpoint_origin: managed_city\n", gcBin, dataDir)
	if !strings.Contains(out, "status=1") {
		t.Fatalf("op_health output = %s, want failure while the server answers SQL but hq is gone", out)
	}
	if !strings.Contains(out, `dolt city database unavailable: database "hq" default branch head unresolvable: Error 1049 (HY000): database not found: hq`) {
		t.Fatalf("op_health output = %s, want the helper's database error", out)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("ReadFile(argv): %v", err)
	}
	if !strings.Contains(string(argv), "--database hq --data-dir "+dataDir) {
		t.Fatalf("health-check argv = %q, want --database hq --data-dir %s for a managed city", argv, dataDir)
	}
}

func TestGcBeadsBdHealthFailsClosedWhenHelperOmitsDatabaseVerdict(t *testing.T) {
	gcBin, _ := writeFakeHealthCheckGC(t, "query_ready\ttrue\\nread_only\tfalse\\nconnection_count\t5\\n")

	out := gcBeadsBdHealthHarness(t, cityMetadataHQ, "issue_prefix: hq\ngc.endpoint_origin: managed_city\n", gcBin, t.TempDir())
	if !strings.Contains(out, "status=1") || !strings.Contains(out, `health check did not report database "hq"`) {
		t.Fatalf("op_health output = %s, want fail-closed when the database verdict is missing", out)
	}
}

func TestGcBeadsBdHealthPassesWhenCityDatabaseReady(t *testing.T) {
	gcBin, _ := writeFakeHealthCheckGC(t,
		"query_ready\ttrue\\nread_only\tfalse\\nconnection_count\t5\\ndatabase\thq\\ndatabase_ready\ttrue\\n")

	out := gcBeadsBdHealthHarness(t, cityMetadataHQ, "issue_prefix: hq\ngc.endpoint_origin: managed_city\n", gcBin, t.TempDir())
	if !strings.Contains(out, "status=0") {
		t.Fatalf("op_health output = %s, want success", out)
	}
}

func TestGcBeadsBdHealthSkipsDataDirForExternalLoopbackEndpoint(t *testing.T) {
	gcBin, argvFile := writeFakeHealthCheckGC(t,
		"query_ready\ttrue\\nread_only\tfalse\\nconnection_count\t5\\ndatabase\thq\\ndatabase_ready\ttrue\\n")

	out := gcBeadsBdHealthHarness(t, cityMetadataHQ,
		"issue_prefix: hq\ngc.endpoint_origin: city_canonical\ndolt.host: 127.0.0.1\ndolt.port: \"4406\"\n", gcBin, t.TempDir())
	if !strings.Contains(out, "status=0") {
		t.Fatalf("op_health output = %s, want success", out)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("ReadFile(argv): %v", err)
	}
	if !strings.Contains(string(argv), "--database hq") || strings.Contains(string(argv), "--data-dir") {
		t.Fatalf("health-check argv = %q, want --database hq without --data-dir for an external endpoint", argv)
	}
}

func TestGcBeadsBdHealthWithoutCityDatabaseKeepsServerOnlyCheck(t *testing.T) {
	gcBin, argvFile := writeFakeHealthCheckGC(t, "query_ready\ttrue\\nread_only\tfalse\\nconnection_count\t5\\n")

	out := gcBeadsBdHealthHarness(t, "", "", gcBin, t.TempDir())
	if !strings.Contains(out, "status=0") {
		t.Fatalf("op_health output = %s, want success before the city names a database", out)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("ReadFile(argv): %v", err)
	}
	if strings.Contains(string(argv), "--database") || strings.Contains(string(argv), "--data-dir") {
		t.Fatalf("health-check argv = %q, want no database flags", argv)
	}
}

// TestGcBeadsBdHealthPassesDatabaseWithoutDataDirForRemoteServer covers
// AC-5's remote leg: a remote server's databases are not under the local
// DATA_DIR, so the helper receives --database but no --data-dir, and health
// still fails closed when the database is not reported ready.
func TestGcBeadsBdHealthPassesDatabaseWithoutDataDirForRemoteServer(t *testing.T) {
	gcBin, argvFile := writeFakeHealthCheckGC(t,
		"query_ready\ttrue\\nread_only\tunknown\\nconnection_count\t5\\ndatabase\\thq\\ndatabase_ready\tfalse\\n"+
			"database_error\\tdatabase \"hq\" default branch head unresolvable: Error 1049 (HY000): database not found: hq\\n")

	out := gcBeadsBdOpHarness(t, "op_health", `is_remote() { return 0; }
`, cityMetadataHQ, "issue_prefix: hq\ndolt.host: db.example\ndolt.port: \"4406\"\n", gcBin, t.TempDir())
	if !strings.Contains(out, "status=1") || !strings.Contains(out, `"hq"`) {
		t.Fatalf("op_health output = %s, want failure naming hq while the remote server answers SQL", out)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("ReadFile(argv): %v", err)
	}
	if !strings.Contains(string(argv), "--database hq") || strings.Contains(string(argv), "--data-dir") {
		t.Fatalf("health-check argv = %q, want --database hq without --data-dir for a remote server", argv)
	}
}

// TestGcBeadsBdHealthFallbackFailsClosedNotInconclusive covers AC-6: with
// no usable gc helper and a city database that fails its checks, op_health
// exits 1 naming the database instead of downgrading the failing write
// probe to a "read-only probe inconclusive" warning and exiting 0.
func TestGcBeadsBdHealthFallbackFailsClosedNotInconclusive(t *testing.T) {
	binDir := writeFallbackHeadDolt(t, "Error 1105 (HY000): cannot resolve default branch head for database 'hq': 'ghost'")
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := gcBeadsBdOpHarness(t, "op_health", `check_read_only() { return 2; }
`, cityMetadataHQ, "issue_prefix: hq\ngc.endpoint_origin: managed_city\n", "", dataDir,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if !strings.Contains(out, "status=1") {
		t.Fatalf("op_health output = %s, want failure", out)
	}
	if !strings.Contains(out, `dolt city database unavailable: database "hq" default branch head unresolvable`) {
		t.Fatalf("op_health output = %s, want the city database named", out)
	}
	if strings.Contains(out, "read-only probe inconclusive") {
		t.Fatalf("op_health output = %s, a broken city database must not read as an inconclusive read-only probe", out)
	}
}

// writeFallbackHeadDolt writes a dolt CLI that answers only the city
// database HEAD probe, failing it with headErr when non-empty.
func writeFallbackHeadDolt(t *testing.T, headErr string) string {
	t.Helper()
	binDir := t.TempDir()
	answer := `printf 'head\nlm41v2vo0p5s281qk22bmmqdun6coqlh\n'; exit 0`
	if headErr != "" {
		answer = `echo "` + headErr + `" >&2; exit 1`
	}
	script := `#!/bin/sh
case "$*" in
  *"DOLT_HASHOF('HEAD')"*) ` + answer + ` ;;
esac
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir
}

func TestGcBeadsBdHealthFallbackFailsClosedOnUnresolvableHead(t *testing.T) {
	binDir := writeFallbackHeadDolt(t, "Error 1105 (HY000): cannot resolve default branch head for database 'hq': 'ghost'")

	out := gcBeadsBdHealthHarness(t, cityMetadataHQ, "issue_prefix: hq\n", "", t.TempDir(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if !strings.Contains(out, "status=1") {
		t.Fatalf("op_health output = %s, want failure", out)
	}
	if !strings.Contains(out, `dolt city database unavailable: database "hq" default branch head unresolvable:`) ||
		!strings.Contains(out, "cannot resolve default branch head") {
		t.Fatalf("op_health output = %s, want the HEAD failure named", out)
	}
}

func TestGcBeadsBdHealthFallbackFailsClosedWhenRepositoryMissingOnDisk(t *testing.T) {
	binDir := writeFallbackHeadDolt(t, "")
	dataDir := t.TempDir()

	out := gcBeadsBdHealthHarness(t, cityMetadataHQ, "issue_prefix: hq\ngc.endpoint_origin: managed_city\n", "", dataDir,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if !strings.Contains(out, "status=1") || !strings.Contains(out, `database "hq" absent from data dir `+dataDir) {
		t.Fatalf("op_health output = %s, want on-disk absence named", out)
	}

	if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	out = gcBeadsBdHealthHarness(t, cityMetadataHQ, "issue_prefix: hq\ngc.endpoint_origin: managed_city\n", "", dataDir,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if !strings.Contains(out, "status=0") {
		t.Fatalf("op_health output = %s, want success once hq is on disk with a resolvable head", out)
	}
}

func TestGcBeadsBdRecoverFailsWhenCityDatabaseStillUnavailable(t *testing.T) {
	gcBin, argvFile := writeFakeHealthCheckGC(t,
		"query_ready\ttrue\\nread_only\tunknown\\nconnection_count\t5\\ndatabase\thq\\ndatabase_ready\tfalse\\n"+
			"database_error\tdatabase \"hq\" default branch head unresolvable: Error 1049 (HY000): database not found: hq\\n")
	dataDir := t.TempDir()

	// The server is down when recovery starts, so the restart runs and the
	// check after it finds hq still unavailable.
	out := gcBeadsBdOpHarness(t, "op_recover", gcBeadsBdRecoverStubs+"tcp_check() { return 1; }\n", cityMetadataHQ,
		"issue_prefix: hq\ngc.endpoint_origin: managed_city\n", gcBin, dataDir)
	if !strings.Contains(out, "status=1") {
		t.Fatalf("op_recover output = %s, want failure while hq is still unavailable after recovery", out)
	}
	if !strings.Contains(out, "still unavailable after recovery") || !strings.Contains(out, "database not found: hq") {
		t.Fatalf("op_recover output = %s, want the post-recovery database failure named", out)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("ReadFile(argv): %v", err)
	}
	if !strings.Contains(string(argv), "--database hq --data-dir "+dataDir) {
		t.Fatalf("health-check argv = %q, want --database hq --data-dir %s", argv, dataDir)
	}
}

func TestGcBeadsBdRecoverSucceedsWhenCityDatabaseReady(t *testing.T) {
	gcBin, _ := writeFakeHealthCheckGC(t,
		"query_ready\ttrue\\nread_only\tfalse\\nconnection_count\t5\\ndatabase\thq\\ndatabase_ready\ttrue\\n")

	out := gcBeadsBdOpHarness(t, "op_recover", gcBeadsBdRecoverStubs, cityMetadataHQ,
		"issue_prefix: hq\ngc.endpoint_origin: managed_city\n", gcBin, t.TempDir())
	if !strings.Contains(out, "status=0") {
		t.Fatalf("op_recover output = %s, want success once hq is ready", out)
	}
}

func TestGcBeadsBdHealthFallbackAcceptsRepositoryDifferingOnlyInCase(t *testing.T) {
	binDir := writeFallbackHeadDolt(t, "")
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "HQ", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := gcBeadsBdHealthHarness(t, cityMetadataHQ, "issue_prefix: hq\ngc.endpoint_origin: managed_city\n", "", dataDir,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if !strings.Contains(out, "status=0") {
		t.Fatalf("op_health output = %s, want success for data dir entry HQ", out)
	}
}

// TestGcBeadsBdHealthFallbackRejectsRepoDirNamedWithDash covers AC-3 in the
// helper-less fallback: Dolt serves a directory under its own name unless
// DOLT_DBNAME_REPLACE is set, so a repository directory gas-city does not
// hold the database gas_city.
func TestGcBeadsBdHealthFallbackRejectsRepoDirNamedWithDash(t *testing.T) {
	binDir := writeFallbackHeadDolt(t, "")
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "gas-city", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := gcBeadsBdHealthHarness(t,
		`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gas_city"}`,
		"issue_prefix: hq\ngc.endpoint_origin: managed_city\n", "", dataDir,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if !strings.Contains(out, "status=1") || !strings.Contains(out, `database "gas_city" absent from data dir `+dataDir) {
		t.Fatalf("op_health output = %s, want gas_city absent while only gas-city is on disk", out)
	}
}

// gcBeadsBdRecoverObservedStubs record every restart step op_recover takes:
// the gc helper recovery, and the shell stop and start it falls back to.
const gcBeadsBdRecoverObservedStubs = `is_remote() { return 1; }
recovery_should_skip_due_to_enospc() { return 1; }
recovery_should_skip_due_to_fsync_stall() { return 1; }
load_recover_managed_from_gc() { echo "CALLED recover-managed"; GC_RECOVER_MANAGED_USED=true; GC_RECOVER_PORT=3311; GC_RECOVER_DIAGNOSED_READ_ONLY=false; return 0; }
op_stop_impl() { echo "CALLED stop"; }
op_start() { echo "CALLED start"; }
run_preflight_cleanup() { :; }
`

const cityConfigManagedHQ = "issue_prefix: hq\ngc.endpoint_origin: managed_city\n"

// TestGcBeadsBdRecoverSkipsRestartWhenOnlyCityDatabaseUnavailable feeds
// op_recover the reports the real helper prints for faults a restart cannot
// repair (the city database is not in the catalog, its head does not resolve,
// its repository is not on disk) and the same faults through the helper-less
// fallback: recovery is skipped without a restart.
func TestGcBeadsBdRecoverSkipsRestartWhenOnlyCityDatabaseUnavailable(t *testing.T) {
	helperCases := []struct {
		name   string
		probes helperDatabaseProbes
		onDisk bool
		cause  string
	}{
		{
			name: "gc helper not in catalog", onDisk: true, cause: "database not found: hq",
			probes: helperDatabaseProbes{head: doltServerError(1049, "database not found: hq")},
		},
		{
			name: "gc helper head unresolvable", onDisk: true, cause: "cannot resolve default branch head",
			probes: helperDatabaseProbes{head: doltServerError(1105, "cannot resolve default branch head for database 'hq': 'ghost'")},
		},
		{name: "gc helper missing on disk", cause: `database "hq" absent from data dir`},
	}
	for _, tc := range helperCases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			if tc.onDisk {
				if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			gcBin, argvFile := writeHealthCheckGCForProbes(t, tc.probes, dataDir)

			out := gcBeadsBdOpHarness(t, "op_recover", gcBeadsBdRecoverObservedStubs, cityMetadataHQ, cityConfigManagedHQ, gcBin, dataDir)
			assertRecoverSkippedForCityDatabase(t, out, tc.cause)
			assertSkipCheckRanNoSampleWrite(t, argvFile, dataDir)
		})
	}
	t.Run("fallback", func(t *testing.T) {
		binDir := writeFallbackHeadDolt(t, "Error 1105 (HY000): no root value found in session")
		dataDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
			t.Fatal(err)
		}

		out := gcBeadsBdOpHarness(t, "op_recover", gcBeadsBdRecoverObservedStubs, cityMetadataHQ, cityConfigManagedHQ, "", dataDir,
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		assertRecoverSkippedForCityDatabase(t, out, "no root value found in session")
	})
	t.Run("fallback missing on disk", func(t *testing.T) {
		binDir := writeFallbackHeadDolt(t, "")
		dataDir := t.TempDir()

		out := gcBeadsBdOpHarness(t, "op_recover", gcBeadsBdRecoverObservedStubs, cityMetadataHQ, cityConfigManagedHQ, "", dataDir,
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		assertRecoverSkippedForCityDatabase(t, out, `database "hq" absent from data dir `+dataDir)
	})
}

func assertRecoverSkippedForCityDatabase(t *testing.T, out, cause string) {
	t.Helper()
	if !strings.Contains(out, "status=1") || !strings.Contains(out, "dolt recovery skipped: city database 'hq' unavailable") {
		t.Fatalf("op_recover output = %s, want recovery skipped for the city database", out)
	}
	if !strings.Contains(out, cause) {
		t.Fatalf("op_recover output = %s, want the cause %q", out, cause)
	}
	if strings.Contains(out, "CALLED ") {
		t.Fatalf("op_recover output = %s, want no recover-managed, stop or start", out)
	}
}

// assertSkipCheckRanNoSampleWrite checks the first health check op_recover
// ran, the city-database skip check: it names hq and its data dir and asks
// for no sample write.
func assertSkipCheckRanNoSampleWrite(t *testing.T, argvFile, dataDir string) {
	t.Helper()
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("ReadFile(argv): %v", err)
	}
	first, _, _ := strings.Cut(string(argv), "\n")
	if !strings.Contains(first, "--database hq --data-dir "+dataDir) || strings.Contains(first, "--check-read-only") {
		t.Fatalf("skip check argv = %q, want --database hq --data-dir %s without --check-read-only", first, dataDir)
	}
}

func assertRecoverReachedRecoverManaged(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "CALLED recover-managed") || strings.Contains(out, "recovery skipped") {
		t.Fatalf("op_recover output = %s, want recovery to reach recover-managed", out)
	}
}

// fallbackDoltConnectionRefused is what the dolt CLI prints when it cannot
// reach the server.
const fallbackDoltConnectionRefused = "failed to connect to the dolt sql-server at 127.0.0.1:3311: dial tcp 127.0.0.1:3311: connect: connection refused"

func TestGcBeadsBdRecoverRestartsWhenCityDatabaseIsNotTheCause(t *testing.T) {
	readyReport := "query_ready\ttrue\\nread_only\ttrue\\nconnection_count\t5\\ndatabase\thq\\ndatabase_ready\ttrue\\n"
	cases := []struct {
		name       string
		metadata   string
		report     string
		failHelper bool
		stubs      string
		wantArgv   string
		denyArgv   string
	}{
		{name: "city database ready", metadata: cityMetadataHQ, report: readyReport, wantArgv: "--database hq"},
		{name: "no city database named", metadata: "", report: "query_ready\ttrue\\nread_only\ttrue\\nconnection_count\t5\\n", denyArgv: "--database"},
		{name: "server not answering TCP", metadata: cityMetadataHQ, report: readyReport, stubs: "tcp_check() { return 1; }\n"},
		// The real helper exits non-zero when its query probe gets no
		// answer, and the shell query probe fails the same way.
		{name: "server not answering SQL", metadata: cityMetadataHQ, failHelper: true, stubs: "do_query_probe() { return 1; }\n", wantArgv: "--database hq"},
		{
			name: "helper reports no fault verdict", metadata: cityMetadataHQ, wantArgv: "--database hq",
			report: "query_ready\ttrue\\nread_only\tfalse\\nconnection_count\t5\\ndatabase\thq\\ndatabase_ready\tfalse\\n" +
				"database_error\tdatabase \"hq\" default branch head unresolvable: Error 1049 (HY000): database not found: hq\\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gcBin, argvFile string
			if tc.failHelper {
				gcBin, argvFile = writeFailingHealthCheckGC(t)
			} else {
				gcBin, argvFile = writeFakeHealthCheckGC(t, tc.report)
			}
			// Any dolt call the shell fallback makes gets no answer.
			binDir := writeFallbackHeadDolt(t, fallbackDoltConnectionRefused)

			out := gcBeadsBdOpHarness(t, "op_recover", gcBeadsBdRecoverObservedStubs+tc.stubs, tc.metadata, cityConfigManagedHQ, gcBin, t.TempDir(),
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			assertRecoverReachedRecoverManaged(t, out)
			argv, err := os.ReadFile(argvFile)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("ReadFile(argv): %v", err)
			}
			if tc.wantArgv != "" && !strings.Contains(string(argv), tc.wantArgv) {
				t.Fatalf("health-check argv = %q, want %q", argv, tc.wantArgv)
			}
			if tc.denyArgv != "" && strings.Contains(string(argv), tc.denyArgv) {
				t.Fatalf("health-check argv = %q, want no %q", argv, tc.denyArgv)
			}
		})
	}
}

// TestGcBeadsBdRecoverRestartsWhenCityDatabaseFaultIsNotPersistent covers the
// boundary of the city-database skip with the reports the real helper prints:
// a fault a restart may clear reaches recover-managed. The skip check runs no
// sample write, so a failing write path is left to recover-managed, whose own
// server-level probe decides whether to restart.
func TestGcBeadsBdRecoverRestartsWhenCityDatabaseFaultIsNotPersistent(t *testing.T) {
	cases := []struct {
		name   string
		probes helperDatabaseProbes
	}{
		{name: "head probe timed out", probes: helperDatabaseProbes{head: os.ErrDeadlineExceeded}},
		{name: "head probe lost its connection", probes: helperDatabaseProbes{head: mysql.ErrInvalidConn}},
		{name: "head probe answered another errno 1105", probes: helperDatabaseProbes{head: doltServerError(1105, "open /data/hq/.dolt/noms/journal.idx: too many open files")}},
		{name: "sample write timed out", probes: helperDatabaseProbes{write: context.DeadlineExceeded}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
				t.Fatal(err)
			}
			gcBin, argvFile := writeHealthCheckGCForProbes(t, tc.probes, dataDir)

			out := gcBeadsBdOpHarness(t, "op_recover", gcBeadsBdRecoverObservedStubs, cityMetadataHQ, cityConfigManagedHQ, gcBin, dataDir)
			assertRecoverReachedRecoverManaged(t, out)
			assertSkipCheckRanNoSampleWrite(t, argvFile, dataDir)
		})
	}
	t.Run("fallback head probe lost its connection", func(t *testing.T) {
		binDir := writeFallbackHeadDolt(t, fallbackDoltConnectionRefused)
		dataDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
			t.Fatal(err)
		}

		out := gcBeadsBdOpHarness(t, "op_recover", gcBeadsBdRecoverObservedStubs, cityMetadataHQ, cityConfigManagedHQ, "", dataDir,
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		assertRecoverReachedRecoverManaged(t, out)
	})
}
