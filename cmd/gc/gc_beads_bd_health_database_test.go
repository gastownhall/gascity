package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gcBeadsBdHealthHarness runs op_health from the bundled gc-beads-bd prelude
// for a city whose .beads/metadata.json and config.yaml are given. It returns
// the combined output, which ends in "status=<op_health exit code>".
func gcBeadsBdHealthHarness(t *testing.T, metadata, config, gcBin, dataDir string, env ...string) string {
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
set +e
(op_health)
status=$?
set -e
printf 'status=%s\n' "$status"
`
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return string(runShHarness(t, harness, "op_health", append(sanitizedBaseEnv(env...), "GC_BIN="+gcBin)))
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
