package dolt_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const pullScript = "commands/pull/run.sh"

func TestPullUsesLiveSQLWhenManagedServerReachable(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writeSyncFakeDolt(t, binDir)
	bdLog := writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_PULL_ALLOW_REMOTE_APP=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gc dolt pull failed: %v\n%s", err, out)
	}

	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("pull called gc-beads-bd while server was reachable: %q", data)
	}

	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"SELECT name, url FROM dolt_remotes ORDER BY name",
		"CALL DOLT_PULL('origin', 'main')",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("dolt log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
}

func TestPullReportsLiveSQLRemoteLookupFailure(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, pullScript)

	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}

	binDir := t.TempDir()
	doltLog := writeSyncFakeDoltRemoteLookupFailure(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", script, "--db", "app")
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gc dolt pull succeeded despite remote lookup failure:\n%s", out)
	}
	if !strings.Contains(string(out), "app: ERROR: failed to query remotes") {
		t.Fatalf("output missing remote lookup failure:\n%s", out)
	}
	if strings.Contains(string(out), "skipped (no remote)") {
		t.Fatalf("remote lookup failure should not be reported as no remote:\n%s", out)
	}

	data, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("read fake dolt log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "SELECT name, url FROM dolt_remotes ORDER BY name") {
		t.Fatalf("dolt log missing remote lookup:\n%s", log)
	}
	if strings.Contains(log, "CALL DOLT_PULL(") {
		t.Fatalf("pull should not run after remote lookup failure:\n%s", log)
	}
}

// ---------------------------------------------------------------------------
// gp-f2yq: the pull path carries the same two guards as sync — one server-side
// DOLT_PULL / DOLT_FETCH per database at a time, and the server-side call is
// killed (and proven gone) when the client bound expires — plus its own
// validated bound, GC_DOLT_PULL_TIMEOUT_SECS.
// ---------------------------------------------------------------------------

// runPull runs `gc dolt pull <args>` against a one-DB ("app") SQL-mode city
// with the fake dolt already installed in binDir.
func runPull(t *testing.T, binDir string, env []string, args ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	script := filepath.Join(root, pullScript)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append(append(filteredEnv("PATH"),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		// upstream's pull refuses a sole non-file:// remote without this opt-in
		"GC_DOLT_PULL_ALLOW_REMOTE_APP=1",
	), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writePullFakeDolt installs a fake dolt for the SQL pull path: remote lookup
// answered, the processlist answered by `processlistArm`, DOLT_PULL by
// `pullArm` (complete case arm bodies), KILLs logged and marked.
func writePullFakeDolt(t *testing.T, dir, processlistArm, pullArm string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	killedPrefix := filepath.Join(dir, "killed-") // KILL marks only the addressed session (codex r10)
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"SELECT name, url FROM dolt_remotes\"*)\n" +
		"    printf 'name,url\\norigin,https://example.invalid/repo\\n' ; exit 0 ;;\n" +
		"  *\"information_schema.processlist\"*) " + processlistArm + " ;;\n" +
		"  *\"CALL DOLT_PULL(\"*) " + pullArm + " ;;\n" +
		"  *\"KILL \"*) k=\"$*\" ; : > \"" + killedPrefix + "${k##* }\" ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

func TestPullInFlightSkipsNeverPulls(t *testing.T) {
	binDir := t.TempDir()
	logPath := writePullFakeDolt(t, binDir, "printf 'Id,Time,db\\n51,900,app\\n' ; exit 0", "exit 0")
	out, err := runPull(t, binDir, nil, "--db", "app")
	if err == nil {
		t.Fatalf("a pull skipped for an in-flight session must exit non-zero.\nout:\n%s", out)
	}
	log := readLog(t, logPath)
	if strings.Contains(log, "CALL DOLT_PULL(") {
		t.Fatalf("a remote operation already in flight for the db must NOT start a pull.\nout:\n%s\nlog:\n%s", out, log)
	}
	if !strings.Contains(out, "app: pull already in flight for 900s (session 51) — skipped") {
		t.Fatalf("expected the in-flight skip line.\nout:\n%s", out)
	}
}

// The same rule for pull: a session attributed to another database blocks
// too (attribution is not proof of the target — codex r9), and the query text
// carries no database name (constant text; codex r6).
func TestPullInFlightOtherDatabaseCountsToo(t *testing.T) {
	binDir := t.TempDir()
	logPath := writePullFakeDolt(t, binDir, "printf 'Id,Time,db\\n52,300,other\\n' ; exit 0", "exit 0")
	out, err := runPull(t, binDir, nil, "--db", "app")
	log := readLog(t, logPath)
	if err == nil {
		t.Fatalf("a pull skipped for an in-flight session must exit non-zero.\nout:\n%s", out)
	}
	if strings.Contains(log, "CALL DOLT_PULL(") {
		t.Fatalf("a remote operation in flight anywhere on the server must block this pull.\nout:\n%s\nlog:\n%s", out, log)
	}
	if !strings.Contains(out, "app: pull already in flight for 300s (session 52) — skipped") {
		t.Fatalf("expected the in-flight skip line.\nout:\n%s", out)
	}
	if q := processlistQuery(t, log); strings.Contains(q, "app") || strings.Contains(q, "LOWER(db)") {
		t.Fatalf("the processlist query must carry no database literal.\nquery:\n%s", q)
	}
}

// writeRecordingGtimeout installs a fake `gtimeout` first on PATH (runtime.sh
// prefers it) that logs its arguments (`--kill-after=2 SECS cmd…`) and then
// runs the command, so a test can prove WHICH bound reached WHICH call.
func writeRecordingGtimeout(t *testing.T, dir string) string {
	t.Helper()
	logPath := filepath.Join(dir, "gtimeout.log")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\nshift 2\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gtimeout"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake gtimeout: %v", err)
	}
	return logPath
}

// assertBounded pins that some bounded invocation ran under `secs` seconds and
// carried `needle` in its command line.
func assertBounded(t *testing.T, tlog, secs, needle string) {
	t.Helper()
	for _, line := range strings.Split(tlog, "\n") {
		if strings.HasPrefix(line, "--kill-after=2 "+secs+" ") && strings.Contains(line, needle) {
			return
		}
	}
	t.Fatalf("no bounded call under %ss carrying %q.\ngtimeout log:\n%s", secs, needle, tlog)
}

// The same marker-collision rule for pull: an ordinary error whose echoed
// statement carries gc-remote-op-lock-held is a pull failure, not a refusal.
func TestPullGateMarkerInOrdinaryErrorIsNotRefusal(t *testing.T) {
	binDir := t.TempDir()
	logPath := writePullFakeDolt(t, binDir, "printf 'Id,Time,db\\n' ; exit 0",
		"printf 'id\\n64\\n' ; printf 'error on line 1 for query CALL DOLT_PULL(''gc-remote-op-lock-held'', ''main''): Error 1105 (HY000): remote not found\\n' >&2 ; exit 1")
	out, err := runPull(t, binDir, nil, "--db", "app")
	if err == nil {
		t.Fatalf("a failed pull must exit non-zero.\nout:\n%s", out)
	}
	if strings.Contains(out, "already in flight") {
		t.Fatalf("an ordinary error echoing the marker must not read as a gate refusal.\nout:\n%s", out)
	}
	if !strings.Contains(out, "pull failed (exit 1)") || !strings.Contains(out, "remote not found") {
		t.Fatalf("expected the ordinary pull error with dolt's stderr replayed.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

func TestPullTimeoutKillsItsServerSideSession(t *testing.T) {
	binDir := t.TempDir()
	tlogPath := writeRecordingGtimeout(t, binDir)
	started := filepath.Join(binDir, "pull-started")
	killed := filepath.Join(binDir, "killed-78")
	processlist := "if [ -f \"" + killed + "\" ]; then printf 'Id,Time,db\\n'; " +
		"elif [ -f \"" + started + "\" ]; then printf 'Id,Time,db\\n78,120,app\\n'; " +
		"else printf 'Id,Time,db\\n'; fi ; exit 0"
	pull := ": > \"" + started + "\" ; printf 'id\\n78\\n' ; printf 'context deadline exceeded\\n' >&2 ; exit 124"
	logPath := writePullFakeDolt(t, binDir, processlist, pull)
	out, err := runPull(t, binDir, []string{"GC_DOLT_PULL_TIMEOUT_SECS=7"}, "--db", "app")
	if err == nil {
		t.Fatalf("a timed-out pull must exit non-zero.\nout:\n%s", out)
	}
	log := readLog(t, logPath)
	pullAt := strings.Index(log, "CALL DOLT_PULL(")
	killAt := strings.Index(log, "KILL 78\n")
	if pullAt < 0 || killAt < 0 || killAt < pullAt {
		t.Fatalf("the session the pull printed about itself must be KILLed after the bound expires.\nlog:\n%s", log)
	}
	if !strings.Contains(out, "app: pull timed out after 7s") {
		t.Fatalf("expected the pull timeout line naming the bound.\nout:\n%s", out)
	}
	if !strings.Contains(out, "app: server-side pull killed (session 78 no longer in flight)") {
		t.Fatalf("expected the kill line proven by the processlist after KILL.\nout:\n%s", out)
	}
	if strings.Contains(out, "pulled from") {
		t.Fatalf("a timed-out pull must not be reported as pulled.\nout:\n%s", out)
	}
	// The configured bound is what the pull ran under (not the 120 s the
	// metadata queries use), proven by the recorded gtimeout arguments
	// (codex r7: a fake that exits 124 on its own satisfied the line alone).
	tlog := readLog(t, tlogPath)
	assertBounded(t, tlog, "7", "CALL DOLT_PULL(")
	assertBounded(t, tlog, "120", "information_schema.processlist")
}

func TestPullIsAttributedAndSelfIdentifying(t *testing.T) {
	binDir := t.TempDir()
	logPath := writePullFakeDolt(t, binDir, "printf 'Id,Time,db\\n' ; exit 0", "exit 0")
	out, err := runPull(t, binDir, nil, "--db", "app")
	if err != nil {
		t.Fatalf("gc dolt pull failed: %v\n%s", err, out)
	}
	var pullLine string
	for _, line := range strings.Split(readLog(t, logPath), "\n") {
		if strings.Contains(line, "CALL DOLT_PULL(") {
			pullLine = line
			break
		}
	}
	if pullLine == "" {
		t.Fatalf("no pull issued.\nout:\n%s", out)
	}
	if !strings.Contains(pullLine, "--use-db app") || !strings.Contains(pullLine, "SELECT CONNECTION_ID() AS id;") {
		t.Fatalf("the pull must run with --use-db app and print its own connection id first.\nline: %s", pullLine)
	}
	assertGateBeforeCall(t, pullLine, "app", "CALL DOLT_PULL(")
	if !strings.Contains(out, "app: pulled from https://example.invalid/repo") {
		t.Fatalf("expected the pulled line.\nout:\n%s", out)
	}
}

// TestPullRejectsInvalidTimeout: GC_DOLT_PULL_TIMEOUT_SECS is validated at
// startup the way the sync bounds are — empty / non-numeric / all-zero aborts
// with exit 2 before any database is touched (GNU `timeout 0` would run the
// pull unbounded).
func TestPullRejectsInvalidTimeout(t *testing.T) {
	binDir := t.TempDir()
	logPath := writePullFakeDolt(t, binDir, "printf 'Id,Time,db\\n' ; exit 0", "exit 0")
	for _, bad := range []string{"abc", "", "0", "00", "-5"} {
		out, err := runPull(t, binDir, []string{"GC_DOLT_PULL_TIMEOUT_SECS=" + bad}, "--db", "app")
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("pull timeout %q: want exit 2, got err=%v\nout: %s", bad, err, out)
		}
		if !strings.Contains(out, "invalid GC_DOLT_PULL_TIMEOUT_SECS") {
			t.Errorf("pull timeout %q: want validation message\nout: %s", bad, out)
		}
	}
	if readLog(t, logPath) != "" {
		t.Fatalf("an invalid bound must abort before dolt is ever invoked.\nlog:\n%s", readLog(t, logPath))
	}
}

// GNU timeout's SIGKILL escalation exits 137: the same cleanup as 124.
func TestPullClientExit137StillKillsServerSideSession(t *testing.T) {
	binDir := t.TempDir()
	started := filepath.Join(binDir, "pull-started")
	killed := filepath.Join(binDir, "killed-79")
	processlist := "if [ -f \"" + killed + "\" ]; then printf 'Id,Time,db\\n'; " +
		"elif [ -f \"" + started + "\" ]; then printf 'Id,Time,db\\n79,130,app\\n'; " +
		"else printf 'Id,Time,db\\n'; fi ; exit 0"
	pull := ": > \"" + started + "\" ; printf 'id\\n79\\n' ; exit 137"
	logPath := writePullFakeDolt(t, binDir, processlist, pull)
	out, err := runPull(t, binDir, nil, "--db", "app")
	if err == nil {
		t.Fatalf("exit 137 must exit non-zero.\nout:\n%s", out)
	}
	if !strings.Contains(out, "pull timed out after 120s") || !strings.Contains(out, "client exit 137") {
		t.Fatalf("exit 137 must be reported as the bound expiring.\nout:\n%s", out)
	}
	if !strings.Contains(readLog(t, logPath), "KILL 79\n") || !strings.Contains(out, "app: server-side pull killed (session 79 no longer in flight)") {
		t.Fatalf("the recorded session must be KILLed after exit 137.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

// A bound with leading zeros is accepted as the integer it is, and reported so.
func TestPullTimeoutLeadingZerosAreCanonical(t *testing.T) {
	binDir := t.TempDir()
	tlogPath := writeRecordingGtimeout(t, binDir)
	logPath := writePullFakeDolt(t, binDir, "printf 'Id,Time,db\\n' ; exit 0", "printf 'id\\n80\\n' ; exit 124")
	out, err := runPull(t, binDir, []string{"GC_DOLT_PULL_TIMEOUT_SECS=0007"}, "--db", "app")
	if err == nil {
		t.Fatalf("a timed-out pull must exit non-zero.\nout:\n%s", out)
	}
	if !strings.Contains(out, "app: pull timed out after 7s") {
		t.Fatalf("0007 must be reported as 7s.\nout:\n%s", out)
	}
	var pullLine string
	for _, line := range strings.Split(readLog(t, logPath), "\n") {
		if strings.Contains(line, "CALL DOLT_PULL(") {
			pullLine = line
		}
	}
	if pullLine == "" {
		t.Fatalf("no pull issued.\nout:\n%s", out)
	}
	// The canonical value (7, not 0007) is the bound the pull ran under.
	assertBounded(t, readLog(t, tlogPath), "7", "CALL DOLT_PULL(")
}

// The server refused the pull's gate (another session holds the database's
// lock): reported as in flight, exit non-zero, nothing KILLed.
func TestPullGateRefusedSkips(t *testing.T) {
	binDir := t.TempDir()
	logPath := writePullFakeDolt(t, binDir, "printf 'Id,Time,db\\n' ; exit 0",
		"printf 'id\\n62\\n' ; printf 'error on line 1 for query SELECT IF(GET_LOCK(...)) AS gate: Error 3141 (HY000): Invalid JSON text in argument 1 to function json_extract: \"gc-remote-op-lock-held\"\\n' >&2 ; exit 1")
	out, err := runPull(t, binDir, nil, "--db", "app")
	if err == nil {
		t.Fatalf("a refused gate must exit non-zero.\nout:\n%s", out)
	}
	if strings.Contains(readLog(t, logPath), "KILL ") {
		t.Fatalf("a refused gate sent no CALL: nothing to KILL.\nlog:\n%s", readLog(t, logPath))
	}
	want := "app: pull already in flight — the server refused a second one (session lock gc_remote_op:app held) — skipped"
	if !strings.Contains(out, want) || strings.Contains(out, "pulled from") || strings.Contains(out, "pull failed") {
		t.Fatalf("expected %q and no success/failure line.\nout:\n%s", want, out)
	}
}

// The bound's verdict outranks the gate marker too: a client that printed the
// refusal and then stalled until the bound killed it is a dead client whose
// session (ours by the id it printed) is KILLed, not a quiet skip.
func TestPullTimeoutOutranksGateText(t *testing.T) {
	binDir := t.TempDir()
	started := filepath.Join(binDir, "pull-started")
	killed := filepath.Join(binDir, "killed-81")
	processlist := "if [ -f \"" + killed + "\" ]; then printf 'Id,Time,db\\n'; " +
		"elif [ -f \"" + started + "\" ]; then printf 'Id,Time,db\\n81,9,app\\n'; " +
		"else printf 'Id,Time,db\\n'; fi ; exit 0"
	pull := ": > \"" + started + "\" ; printf 'id\\n81\\n' ; printf 'Error 3141 (HY000): Invalid JSON text in argument 1 to function json_extract: \"gc-remote-op-lock-held\"\\n' >&2 ; exit 124"
	logPath := writePullFakeDolt(t, binDir, processlist, pull)
	out, err := runPull(t, binDir, nil, "--db", "app")
	if err == nil {
		t.Fatalf("a timed-out pull must exit non-zero.\nout:\n%s", out)
	}
	if !strings.Contains(out, "pull timed out after 120s") || strings.Contains(out, "server refused") {
		t.Fatalf("the timeout must outrank the gate text.\nout:\n%s", out)
	}
	if !strings.Contains(readLog(t, logPath), "KILL 81\n") {
		t.Fatalf("the recorded session must be KILLed.\nlog:\n%s", readLog(t, logPath))
	}
}
