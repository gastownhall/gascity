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
		"GC_DOLT_REMOTE_OP_LOCK_ROOT="+t.TempDir(),
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
	killed := filepath.Join(dir, "killed")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"SELECT name, url FROM dolt_remotes\"*)\n" +
		"    printf 'name,url\\norigin,https://example.invalid/repo\\n' ; exit 0 ;;\n" +
		"  *\"information_schema.processlist\"*) " + processlistArm + " ;;\n" +
		"  *\"CALL DOLT_PULL(\"*) " + pullArm + " ;;\n" +
		"  *\"KILL \"*) : > \"" + killed + "\" ; exit 0 ;;\n" +
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

func TestPullTimeoutKillsItsServerSideSession(t *testing.T) {
	binDir := t.TempDir()
	started := filepath.Join(binDir, "pull-started")
	killed := filepath.Join(binDir, "killed")
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
	killAt := strings.Index(log, "KILL 78")
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

// A live runner holding this database's runner lock (here: the test process
// itself) means the pull is skipped without touching the server's remote
// operation path.
func TestPullRunnerLockHeldSkips(t *testing.T) {
	binDir := t.TempDir()
	logPath := writePullFakeDolt(t, binDir, "printf 'Id,Time,db\\n' ; exit 0", "exit 0")
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()
	lockRoot := t.TempDir()
	dir := remoteOpLockDir(lockRoot, port, "app")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	out, err := runPull(t, binDir, []string{fmt.Sprintf("GC_DOLT_PORT=%d", port), "GC_DOLT_REMOTE_OP_LOCK_ROOT=" + lockRoot}, "--db", "app")
	if err == nil {
		t.Fatalf("a pull skipped for a held lock must exit non-zero.\nout:\n%s", out)
	}
	if strings.Contains(readLog(t, logPath), "CALL DOLT_PULL(") {
		t.Fatalf("a held lock must not start a pull.\nout:\n%s", out)
	}
	want := fmt.Sprintf("app: another gc dolt sync/pull (pid %d) holds this database's runner lock — skipped", os.Getpid())
	if !strings.Contains(out, want) {
		t.Fatalf("expected %q\nout:\n%s", want, out)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("a live holder's lock must be left alone: %v", statErr)
	}
}

// GNU timeout's SIGKILL escalation exits 137: the same cleanup as 124.
func TestPullClientExit137StillKillsServerSideSession(t *testing.T) {
	binDir := t.TempDir()
	started := filepath.Join(binDir, "pull-started")
	killed := filepath.Join(binDir, "killed")
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
	if !strings.Contains(readLog(t, logPath), "KILL 79") || !strings.Contains(out, "app: server-side pull killed (session 79 no longer in flight)") {
		t.Fatalf("the recorded session must be KILLed after exit 137.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

// A bound with leading zeros is accepted as the integer it is, and reported so.
func TestPullTimeoutLeadingZerosAreCanonical(t *testing.T) {
	binDir := t.TempDir()
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
}
