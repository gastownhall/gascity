package dolt_test

// Fast-forward-only sync classification (gc-6ommo). `gc dolt sync` must not
// blind-push a shared multi-writer DB: it fetches, classifies local vs the
// remote-tracking ref, and pushes only when the local branch is strictly ahead
// (a fast-forward). behind / diverged refuse with an actionable status; a
// fetch timeout skips without pushing; a first push (remote ref absent) is a
// fast-forward and pushes. --force still bypasses classification.
//
// The classification queries are verified against real Dolt 2.1.0:
//   ahead  = SELECT COUNT(*) FROM dolt_log('remotes/<remote>/<br>..<br>')
//   behind = SELECT COUNT(*) FROM dolt_log('<br>..remotes/<remote>/<br>')
// and an absent remote ref yields "branch not found: remotes/...".

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ffSyncCmd builds a `gc dolt sync` invocation against an idle reachable
// server with the fake dolt in binDir first on PATH. Later `env` entries
// override earlier ones (Go keeps the last value of a duplicated key).
func ffSyncCmd(t *testing.T, binDir string, env []string, args ...string) *exec.Cmd {
	t.Helper()
	return packScriptCmd(t, syncScript, binDir, syncFilteredEnv(), env, args...)
}

// packScriptCmd builds `sh <script> <args>` for a one-DB ("app") SQL-mode city
// against an idle reachable server, with the fake dolt in binDir first on
// PATH and the fake bd installed: the ONE command-construction site the sync
// and pull runners share (the repository's resource census counts os/exec
// call sites and must not grow). Later `env` entries override earlier ones.
func packScriptCmd(t *testing.T, script, binDir string, baseEnv, env []string, args ...string) *exec.Cmd {
	t.Helper()
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	t.Cleanup(cleanup)

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", append([]string{filepath.Join(root, script)}, args...)...)
	cmd.Env = append(append(baseEnv,
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	), env...)
	return cmd
}

// runFFSyncEnv runs `gc dolt sync <args>` through ffSyncCmd with extraEnv
// appended after the base sync env (so an override such as GC_DOLT_REMOTE_<DB>
// takes effect), and returns combined output plus the command's error so
// callers that must assert the sync itself did not fail can do so.
func runFFSyncEnv(t *testing.T, binDir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	out, err := ffSyncCmd(t, binDir, extraEnv, args...).CombinedOutput()
	return string(out), err
}

// runFFSync is runFFSyncEnv with no extra environment variables, discarding
// the command error: none of its callers assert on the sync's exit status,
// only on its output.
func runFFSync(t *testing.T, binDir string, args ...string) string {
	t.Helper()
	out, _ := runFFSyncEnv(t, binDir, nil, args...)
	return out
}

// runFFSyncFails runs sync and requires a NON-ZERO exit: every skip (in
// flight, gate refused, processlist failure, timeout, malformed answer) is a
// database that did not sync, and the script's exit code says so (codex r10:
// a `return 0` on a failure path passed the output-only assertions).
func runFFSyncFails(t *testing.T, binDir string, args ...string) string {
	t.Helper()
	out, err := ffSyncCmd(t, binDir, nil, args...).CombinedOutput()
	if err == nil {
		t.Fatalf("a run that skipped or failed a database must exit non-zero.\nout:\n%s", out)
	}
	return string(out)
}

// fakeDoltHeader is the shared preamble for an IDLE server: log argv, answer
// the remote-lookup + active_branch metadata queries the sync path issues
// before classification, and answer the single-flight processlist query with
// its header only (nothing in flight, so the fetch proceeds).
func fakeDoltHeader(logPath, branch string) string {
	return fakeDoltPreamble(logPath, branch) + fakeDoltProcesslistIdleArm
}

// fakeDoltProcesslistIdleArm answers the single-flight processlist query with
// the `Id,Time,db` header and no rows. The header is load-bearing: the parser
// (remote_op_sessions_parse, runtime.sh) treats an answer without it as "not a
// processlist answer" and the fetch is skipped, fail closed.
const fakeDoltProcesslistIdleArm = "  *\"information_schema.processlist\"*) printf 'Id,Time,db\\n' ; exit 0 ;;\n"

// fakeDoltPreamble logs argv and answers the remote-lookup + active_branch
// metadata queries; the caller appends the processlist / fetch / push arms.
func fakeDoltPreamble(logPath, branch string) string {
	return "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"SELECT name, url FROM dolt_remotes ORDER BY name\"*)\n" +
		"    printf 'name,url\\norigin,file:///example.invalid/repo\\n' ; exit 0 ;;\n" +
		"  *\"SELECT active_branch()\"*)\n" +
		"    printf 'active_branch()\\n" + branch + "\\n' ; exit 0 ;;\n"
}

func installFFFakeDolt(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return filepath.Join(dir, "dolt.log")
}

// writeSyncFakeDoltClassify: fetch succeeds; the ahead/behind range queries
// report the given counts; DOLT_PUSH is logged and succeeds.
func writeSyncFakeDoltClassify(t *testing.T, dir string, ahead, behind int) string {
	t.Helper()
	branch := "main"
	logPath := filepath.Join(dir, "dolt.log")
	aheadPat := "dolt_log('remotes/origin/" + branch + ".." + branch + "')"
	behindPat := "dolt_log('" + branch + "..remotes/origin/" + branch + "')"
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) exit 0 ;;\n" +
		"  *\"" + aheadPat + "\"*) printf 'n\\n" + fmt.Sprintf("%d", ahead) + "\\n' ; exit 0 ;;\n" +
		"  *\"" + behindPat + "\"*) printf 'n\\n" + fmt.Sprintf("%d", behind) + "\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// writeSyncFakeDoltFetchTimeout: the DOLT_FETCH call exits 124 (timeout).
func writeSyncFakeDoltFetchTimeout(t *testing.T, dir, branch string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) printf 'context deadline exceeded\\n' >&2 ; exit 124 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// writeSyncFakeDoltFirstPush models a brand-new branch absent on the remote:
// DOLT_FETCH errors "invalid ref spec" (exit 1) — the real Dolt 2.1.0 signal
// for a branch that does not exist on a populated remote (an empty remote
// instead errors "no branches found in remote"). Both are first-push signals;
// the push then creates the branch (a fast-forward). No classify query runs
// because fetch never establishes a remote-tracking ref.
func writeSyncFakeDoltFirstPush(t *testing.T, dir, branch string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) printf 'fetch failed: invalid ref spec\\n' >&2 ; exit 1 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

func pushed(log string) bool { return strings.Contains(log, "DOLT_PUSH") }

func readLog(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	return string(data)
}

func TestSyncAheadOnlyFastForwardPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 0)
	out := runFFSync(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if !strings.Contains(log, "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("ahead-only should fast-forward push.\nout:\n%s\nlog:\n%s", out, log)
	}
	if strings.Contains(log, "--force") {
		t.Fatalf("ahead-only push must not use --force.\nlog:\n%s", log)
	}
}

func TestSyncBehindRefusesAndDoesNotPush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 0, 3)
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("behind DB must NOT be pushed.\nout:\n%s", out)
	}
	if !strings.Contains(out, "behind") {
		t.Fatalf("expected a 'behind' status.\nout:\n%s", out)
	}
}

func TestSyncDivergedRefusesAndDoesNotPush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 3)
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("diverged DB must NOT be pushed.\nout:\n%s", out)
	}
	if !strings.Contains(out, "diverged") {
		t.Fatalf("expected a 'diverged' status.\nout:\n%s", out)
	}
}

func TestSyncUpToDateSkipsPush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 0, 0)
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("up-to-date DB must NOT be pushed.\nout:\n%s", out)
	}
	if !strings.Contains(out, "up-to-date") {
		t.Fatalf("expected an 'up-to-date' status.\nout:\n%s", out)
	}
}

func TestSyncFetchTimeoutSkipsNeverPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeout(t, binDir, "main")
	out := runFFSyncFails(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch timed out") {
		t.Fatalf("expected a 'fetch timed out' status.\nout:\n%s", out)
	}
}

func TestSyncFirstPushWhenRemoteRefAbsentPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFirstPush(t, binDir, "main")
	out := runFFSync(t, binDir, "--db", "app")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("first push (absent remote ref) must push.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

func TestSyncForceStillPushesWhenDiverged(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 3)
	out := runFFSync(t, binDir, "--db", "app", "--force")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") {
		t.Fatalf("--force must bypass classification and force-push.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

// writeSyncFakeDoltEmptyRemoteFirstPush models a first-ever push to an empty
// remote: DOLT_FETCH errors "no branches found in remote" (the other Dolt 2.1.0
// first-push signal, distinct from "invalid ref spec" for a new branch on a
// populated remote). The push then creates the branch (a fast-forward).
func writeSyncFakeDoltEmptyRemoteFirstPush(t *testing.T, dir, branch string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) printf 'fetch failed: no branches found in remote\\n' >&2 ; exit 1 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

func TestSyncEmptyRemoteFirstPushPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltEmptyRemoteFirstPush(t, binDir, "main")
	out := runFFSync(t, binDir, "--db", "app")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("first push to an empty remote must push.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

// --- gc-fqi7kq: deterministic, opt-in-gated remote selection ---
//
// find_remote_sql (and the CLI-mode equivalent) must resolve the SAME remote
// for a given database regardless of what order dolt_remotes rows arrive in;
// must never auto-select a non-file:// remote when a file:// alternative
// exists, or when no local alternative exists at all (skip with a stated
// reason instead); and must honor an explicit GC_DOLT_REMOTE_<DB> override —
// even over a non-local remote. These tests run --force --dry-run so the
// fake dolt only needs to answer the remote-lookup + active_branch queries;
// ff-classification is already covered by the tests above.

// writeSyncFakeDoltMultiRemote installs a fake dolt that answers the
// remote-lookup query with remoteRows ("name,url" pairs) in exactly the
// order given, and active_branch() with "main". Callers only need the
// installed binary (they assert on gc dolt sync's stdout), not the log
// path, so unlike the other fixture builders in this file this one returns
// nothing.
func writeSyncFakeDoltMultiRemote(t *testing.T, dir string, remoteRows []string) {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	reply := "name,url\\n" + strings.Join(remoteRows, "\\n") + "\\n"
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes ORDER BY name"*)
    printf '` + reply + `'
    ;;
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

// TestSyncRemoteSelectionIsOrderIndependent is the RED test from gc-fqi7kq: a
// db whose dolt_remotes returns rows in varying order must resolve the same
// remote every time. Mirrors the real incident's remote set (a git+https
// upstream plus two file:// backups) across three distinct row orderings.
func TestSyncRemoteSelectionIsOrderIndependent(t *testing.T) {
	t.Parallel()
	origin := "origin,git+https://github.com/gastownhall/beads"
	usb := "usb,file:///mnt/usb/beads"
	usbSnap := "usb_snap_20260820,file:///mnt/usb/beads_snap"
	orders := [][]string{
		{origin, usb, usbSnap},
		{usbSnap, usb, origin},
		{usb, origin, usbSnap},
	}
	const want = "-> usb:main (file:///mnt/usb/beads)"
	for i, rows := range orders {
		binDir := t.TempDir()
		writeSyncFakeDoltMultiRemote(t, binDir, rows)
		out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
		if !strings.Contains(out, want) {
			t.Fatalf("order %d: expected remote 'usb' selected regardless of row order.\nrows: %v\nout:\n%s", i, rows, out)
		}
	}
}

// TestSyncMultiRemoteAmbiguousNonLocalSkipsAndNeverPushes covers the case
// where every configured remote is non-local: a default (non-opt-in) sync
// must report the database as skipped, with a stated reason, and never
// select or push to either remote.
func TestSyncMultiRemoteAmbiguousNonLocalSkipsAndNeverPushes(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"mirror,git+https://example.invalid/mirror",
		"origin,git+https://github.com/gastownhall/beads",
	})
	out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "no local remote") {
		t.Fatalf("expected an ambiguous-remote skip with a stated reason.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("ambiguous remotes must never be auto-selected.\nout:\n%s", out)
	}
}

// TestSyncMultiRemotePrefersLocalOverGitHttpsRemote covers the case where a
// git+https remote is configured alongside a file:// alternative: the
// git+https remote must never be the one selected/pushed.
func TestSyncMultiRemotePrefersLocalOverGitHttpsRemote(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
	})
	out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
	if !strings.Contains(out, "-> usb:main (file:///mnt/usb/beads)") {
		t.Fatalf("expected the local (file://) remote to be preferred over git+https.\nout:\n%s", out)
	}
	if strings.Contains(out, "-> origin:") {
		t.Fatalf("git+https remote must never be auto-selected when a local alternative exists.\nout:\n%s", out)
	}
}

// TestSyncRemoteEnvOverridePinsNonLocalRemote verifies that GC_DOLT_REMOTE_<DB>
// pins remote selection even to a non-local (git+https) remote that the
// default policy would otherwise skip past in favor of a file:// alternative.
func TestSyncRemoteEnvOverridePinsNonLocalRemote(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
	})

	out, err := runFFSyncEnv(t, binDir, []string{"GC_DOLT_REMOTE_APP=origin"}, "--db", "app", "--force", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "-> origin:main (git+https://github.com/gastownhall/beads)") {
		t.Fatalf("GC_DOLT_REMOTE_APP=origin should pin selection to origin even though usb (file://) is available.\nout:\n%s", out)
	}
}

// TestSyncSoleNonLocalRemoteSkipsAndNeverPushes covers the case the original
// fix (ga-fqi7kq) missed: select_remote's sr_count -eq 1 branch returned the
// sole candidate unconditionally, with no locality check at all, so a
// database whose only configured remote is non-local (e.g. a public
// git+https upstream) was auto-selected and pushed to. The locality rule
// that already applies when there are multiple remotes must apply
// identically when there is exactly one.
func TestSyncSoleNonLocalRemoteSkipsAndNeverPushes(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
	})

	out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "no local remote") {
		t.Fatalf("a sole non-local remote must be skipped with a stated reason, same as the multi-remote ambiguous case.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("a sole non-local remote must never be auto-selected or pushed to.\nout:\n%s", out)
	}
}

// TestSyncSQLUnknownRemoteOverrideFailsWithStatedReason covers the one path
// where select_remote returns non-zero: GC_DOLT_REMOTE_<DB> names a remote
// that is not configured. The database must fail (not silently fall back to
// the default policy), the stderr must name the offending override, and the
// trailing failure summary must attribute the failure to the refusal rather
// than to a remote *query* failure that did not happen.
func TestSyncSQLUnknownRemoteOverrideFailsWithStatedReason(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
	})

	out, err := runFFSyncEnv(t, binDir, []string{"GC_DOLT_REMOTE_APP=nope"}, "--db", "app", "--force", "--dry-run")
	if err == nil {
		t.Fatalf("an override naming an unconfigured remote must fail the database.\nout:\n%s", out)
	}
	if !strings.Contains(out, "GC_DOLT_REMOTE override 'nope' does not match any configured remote") {
		t.Fatalf("expected the specific unknown-override error.\nout:\n%s", out)
	}
	if !strings.Contains(out, "remote selection refused") {
		t.Fatalf("summary must attribute the failure to the refusal, not to a remote-query failure.\nout:\n%s", out)
	}
	if strings.Contains(out, "failed to query remotes") {
		t.Fatalf("the remote query succeeded; it must not be blamed.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("a refused override must never fall back to selecting a remote.\nout:\n%s", out)
	}
}

// --- CLI-mode (.dolt/remotes.json) equivalents of the SQL cases above ---
//
// sync_database_cli feeds select_remote from remotes_json_pairs instead of
// dolt_remotes, so the same policy has a second, independently-parsed
// candidate source. These mirror the SQL cases against it: prefer local,
// skip a sole non-local remote, and honor the override. runSync forces CLI
// mode by pointing the script at an unreachable SQL port.

// writeSyncCLIRemotes creates the data/<db>/.dolt/remotes.json fixture that
// sync_database_cli parses, and returns the city path it was created under.
func writeSyncCLIRemotes(t *testing.T, remotesJSON string) string {
	t.Helper()
	cityPath := t.TempDir()
	dbDir := filepath.Join(cityPath, "data", "app")
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "remotes.json"), []byte(remotesJSON), 0o644); err != nil {
		t.Fatalf("write remotes: %v", err)
	}
	return cityPath
}

// TestSyncCLIMultiRemotePrefersLocal is the CLI-mode twin of
// TestSyncMultiRemotePrefersLocalOverGitHttpsRemote: with a git+https remote
// listed first and a file:// alternative available, the local one wins.
func TestSyncCLIMultiRemotePrefersLocal(t *testing.T) {
	t.Parallel()
	cityPath := writeSyncCLIRemotes(t, `{"remotes":[{"name":"origin","url":"git+https://github.com/gastownhall/beads"},{"name":"usb","url":"file:///mnt/usb/beads"}]}`)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	out, err := runSync(t, binDir, cityPath, []string{"GC_DOLT_REMOTE_APP="}, "--db", "app", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "-> usb:main (file:///mnt/usb/beads)") {
		t.Fatalf("CLI mode should prefer the local (file://) remote over git+https.\nout:\n%s", out)
	}
	if strings.Contains(out, "-> origin:") {
		t.Fatalf("git+https remote must never be auto-selected when a local alternative exists.\nout:\n%s", out)
	}
}

// TestSyncCLISoleNonLocalRemoteSkips is the CLI-mode twin of
// TestSyncSoleNonLocalRemoteSkipsAndNeverPushes: a sole non-local remote is
// not exempt from the locality rule just because there was nothing to
// disambiguate.
func TestSyncCLISoleNonLocalRemoteSkips(t *testing.T) {
	t.Parallel()
	cityPath := writeSyncCLIRemotes(t, `{"remotes":[{"name":"origin","url":"git+https://github.com/gastownhall/beads"}]}`)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	out, err := runSync(t, binDir, cityPath, []string{"GC_DOLT_REMOTE_APP="}, "--db", "app", "--dry-run")
	if err != nil {
		t.Fatalf("a skip is not a failure; sync should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "no local remote") {
		t.Fatalf("a sole non-local remote must be skipped with a stated reason.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("a sole non-local remote must never be auto-selected or pushed to.\nout:\n%s", out)
	}
}

// TestSyncCLIRemoteOverridePinsNonLocal is the CLI-mode twin of
// TestSyncRemoteEnvOverridePinsNonLocalRemote: an explicit override pins a
// non-local remote the default policy would otherwise pass over.
func TestSyncCLIRemoteOverridePinsNonLocal(t *testing.T) {
	t.Parallel()
	cityPath := writeSyncCLIRemotes(t, `{"remotes":[{"name":"origin","url":"git+https://github.com/gastownhall/beads"},{"name":"usb","url":"file:///mnt/usb/beads"}]}`)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	out, err := runSync(t, binDir, cityPath, []string{"GC_DOLT_REMOTE_APP=origin"}, "--db", "app", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "-> origin:main (git+https://github.com/gastownhall/beads)") {
		t.Fatalf("GC_DOLT_REMOTE_APP=origin should pin selection to origin even though usb (file://) is available.\nout:\n%s", out)
	}
}

// TestSyncRejectsInvalidFetchTimeout covers the GC_DOLT_SYNC_FETCH_TIMEOUT_SECS
// validator (the twin of the push-timeout validator): the bound is checked at
// startup before any database is touched, and an empty / non-numeric / all-zero
// value aborts with exit 2 rather than running the fetch unbounded.
func TestSyncRejectsInvalidFetchTimeout(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir) // never invoked: the validator aborts first
	cityPath := t.TempDir()
	for _, bad := range []string{"abc", "", "0", "00", "-5"} {
		cmd := exec.Command("sh", script, "--db", "app")
		cmd.Env = append(syncFilteredEnv(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"GC_CITY_PATH="+cityPath,
			"GC_PACK_DIR="+root,
			"GC_DOLT_DATA_DIR="+filepath.Join(cityPath, "data"),
			"GC_DOLT_PORT=1",
			"GC_DOLT_USER=root",
			"GC_DOLT_PASSWORD=",
			"GC_DOLT_SYNC_FETCH_TIMEOUT_SECS="+bad,
		)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("fetch timeout %q: want exit 2, got err=%v\nout: %s", bad, err, out)
		}
		if !strings.Contains(string(out), "invalid GC_DOLT_SYNC_FETCH_TIMEOUT_SECS") {
			t.Errorf("fetch timeout %q: want validation message\nout: %s", bad, out)
		}
	}
}

// ---------------------------------------------------------------------------
// gp-f2yq: ONE server-side DOLT_FETCH per database at a time, and the
// server-side call dies with the client bound.
//
// A `CALL DOLT_FETCH` runs INSIDE the sql-server. When the client's wall-clock
// bound expired only the client died; the server-side fetch kept running, and
// the 15-minute patrol stacked one more on every run (boomtown 2026-09-11: 169
// of 178 sql-server sessions in DOLT_FETCH, one per run for two days). The
// fakes below answer the single-flight processlist query, print the fetch's
// own connection id the way the real CLI does before the procedure starts,
// and log KILLs; marker files in binDir let the processlist answer change as
// the fetch starts and as it is killed, the way the live server's does.
// ---------------------------------------------------------------------------

// writeSyncFakeDoltProcesslist installs a fake dolt whose single-flight
// processlist answer is `arm` (a complete case arm body: printf/exit); a fetch,
// if one is ever issued, succeeds silently and is logged.
func writeSyncFakeDoltProcesslist(t *testing.T, dir, arm string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltPreamble(logPath, "main") +
		"  *\"information_schema.processlist\"*) " + arm + " ;;\n" +
		"  *\"CALL DOLT_FETCH(\"*) exit 0 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// writeSyncFakeDoltFetchTimeoutKill models the live sequence: the processlist
// is empty before the fetch; the fetch prints its connection id (sessionID;
// nothing when empty) and then hits the bound (exit 124), after which the
// processlist lists `listedAfterFetch` (Id,Time,db rows); every KILL is logged
// and afterwards the processlist is empty again — unless stayAlive, in which
// case the rows keep being listed after the KILL (a KILL that did not take).
func writeSyncFakeDoltFetchTimeoutKill(t *testing.T, dir, sessionID string, listedAfterFetch []string, stayAlive bool) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	started := filepath.Join(dir, "fetch-started")
	// KILL marks only the ADDRESSED session (`killed-<id>`), so a KILL of the
	// wrong id never reads as a successful cleanup (codex r10).
	killedPrefix := filepath.Join(dir, "killed-")
	killed := killedPrefix + sessionID
	idEmit := ""
	if sessionID != "" {
		idEmit = "printf 'id\\n" + sessionID + "\\n' ; "
	}
	rows := ""
	for _, r := range listedAfterFetch {
		rows += r + "\\n"
	}
	afterKill := "printf 'Id,Time,db\\n'"
	if stayAlive {
		afterKill = "printf 'Id,Time,db\\n" + rows + "'"
	}
	body := fakeDoltPreamble(logPath, "main") +
		"  *\"information_schema.processlist\"*)\n" +
		"    if [ -f \"" + killed + "\" ]; then " + afterKill + "\n" +
		"    elif [ -f \"" + started + "\" ]; then printf 'Id,Time,db\\n" + rows + "'\n" +
		"    else printf 'Id,Time,db\\n'; fi\n" +
		"    exit 0 ;;\n" +
		"  *\"CALL DOLT_FETCH(\"*) : > \"" + started + "\" ; " + idEmit + "printf 'context deadline exceeded\\n' >&2 ; exit 124 ;;\n" +
		"  *\"KILL \"*) k=$(printf '%s' \"$*\" | sed -n 's/.*KILL \\([0-9][0-9]*\\).*/\\1/p') ; : > \"" + killedPrefix + "${k}\" ; exit 0 ;;\n" +
		fakeDoltRunLockFreeArm +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// fakeDoltRunLockFreeArm answers the kill helper's run-lock holder query
// (gp-f2yq round 2) with "free" (0): the helper never confirms a kill, or an
// ended session, without that answer — a test that wants a live holder
// writes its own arm.
const fakeDoltRunLockFreeArm = "  *\"COALESCE(IS_USED_LOCK(\"*) printf 'holder\\n0\\n' ; exit 0 ;;\n"

// fetched reports whether the fake dolt was asked to run the fetch procedure.
// It matches the CALL itself, not the bare procedure name: the single-flight
// processlist query names DOLT_FETCH inside its LIKE pattern too.
func fetched(log string) bool { return strings.Contains(log, "CALL DOLT_FETCH(") }

func TestSyncFetchInFlightSkipsNeverFetches(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf 'Id,Time,db\\n42,7200,app\\n' ; exit 0")
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if fetched(log) {
		t.Fatalf("a fetch already in flight for the db must NOT start another.\nout:\n%s\nlog:\n%s", out, log)
	}
	if pushed(log) {
		t.Fatalf("a fetch already in flight must NEVER push.\nout:\n%s", out)
	}
	if !strings.Contains(out, "app: fetch already in flight for 7200s (session 42)") || !strings.Contains(out, "NOT pushed") {
		t.Fatalf("expected the in-flight skip line naming the oldest session's age and id.\nout:\n%s", out)
	}
	// The processlist query is CONSTANT text: no database name is interpolated
	// into it. A store named `dolt_fetch` (a valid name) would otherwise make
	// the query match its own text and skip on every run (codex r6, evidence
	// 04g). There is no per-database filter at all: attribution is not proof
	// of a statement's target (TestSyncFetchInFlightOtherDatabaseCountsToo).
	// The COMPLETE constant query is the spec (codex r14: a `NOT` slipped
	// into the WHERE clause passed a substring check — the fake answers the
	// same rows either way, the server would not).
	const processlistSQL = "SELECT Id, Time, COALESCE(db, '') AS db FROM information_schema.processlist WHERE UPPER(Info) REGEXP '(^|[^A-Z0-9_])DOLT_(FETCH|PULL)([^A-Z0-9_]|$)' ORDER BY Time DESC, Id ASC"
	if q := processlistQuery(t, log); !strings.HasSuffix(q, " -q "+processlistSQL) || strings.Contains(q, "app") {
		t.Fatalf("the processlist query must be exactly the constant text %q (no database literal).\nquery:\n%s", processlistSQL, q)
	}
	// The predicate is the whole-identifier token test on the statement text,
	// not a LIKE prefix and not a grammar of spellings: any statement naming
	// DOLT_FETCH or DOLT_PULL counts — bare `CALL DOLT_FETCH`, `CALL\`dolt_fetch\`()`,
	// comments, qualifiers, any case (verified on Dolt 2.1.10, evidence 04f).
	if !strings.Contains(log, "UPPER(Info) REGEXP '(^|[^A-Z0-9_])DOLT_(FETCH|PULL)([^A-Z0-9_]|$)'") {
		t.Fatalf("the in-flight predicate must be the whole-identifier REGEXP on UPPER(Info).\nlog:\n%s", log)
	}
}

// processlistQuery returns the single-flight processlist invocation the fake
// dolt logged (one `$*` line per invocation).
func processlistQuery(t *testing.T, log string) string {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "information_schema.processlist") {
			return line
		}
	}
	t.Fatalf("no processlist query in the log:\n%s", log)
	return ""
}

// A session attributed to ANOTHER database blocks this one too: the processlist
// DB column is the connection's --use-db, not the statement's target (a client
// on --use-db other running `USE app; CALL DOLT_FETCH` is attributed to other —
// codex r9; evidence 04h b), so attribution proves nothing and every in-flight
// remote operation on the server counts, fail closed. A quoted db column is
// still validated as one CSV field.
func TestSyncFetchInFlightOtherDatabaseCountsToo(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf 'Id,Time,db\\n44,500,other\\n45,10,\"other-two\"\\n' ; exit 0")
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if fetched(log) {
		t.Fatalf("a remote operation in flight anywhere on the server must block this fetch (attribution is not proof of its target).\nout:\n%s\nlog:\n%s", out, log)
	}
	if !strings.Contains(out, "fetch already in flight for 500s (session 44)") {
		t.Fatalf("expected the in-flight skip line naming the oldest session.\nout:\n%s", out)
	}
}

// A session attributed in another spelling (APP) blocks too — every attribution
// does; the processlist shows the client's spelling.
func TestSyncFetchInFlightCaseInsensitiveDatabase(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf 'Id,Time,db\\n46,20,APP\\n' ; exit 0")
	out := runFFSyncFails(t, binDir, "--db", "app")
	if fetched(readLog(t, logPath)) {
		t.Fatalf("a session attributed to APP is this database's (app): it must block the fetch.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch already in flight for 20s (session 46)") {
		t.Fatalf("expected the in-flight skip line.\nout:\n%s", out)
	}
}

// A session attributed to a revision-qualified name (`app/main`: the client
// connected with --use-db app/main — Dolt's branch-qualified database) counts,
// and so does another database's revision: attribution is not proof of the
// target (codex r8, r9; evidence 04h).
func TestSyncFetchInFlightRevisionQualifiedDatabaseCounts(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf 'Id,Time,db\\n47,40,APP/feature/x\\n' ; exit 0")
	out := runFFSyncFails(t, binDir, "--db", "app")
	if fetched(readLog(t, logPath)) {
		t.Fatalf("a session attributed to APP/feature/x is this database's: it must block the fetch.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch already in flight for 40s (session 47)") {
		t.Fatalf("expected the in-flight skip line.\nout:\n%s", out)
	}
	binDir = t.TempDir()
	logPath = writeSyncFakeDoltProcesslist(t, binDir, "printf 'Id,Time,db\\n48,5,other/main\\n' ; exit 0")
	out = runFFSyncFails(t, binDir, "--db", "app")
	if fetched(readLog(t, logPath)) {
		t.Fatalf("another database's revision (other/main) blocks too: attribution is not proof of the target.\nout:\n%s", out)
	}
}

// The gate-refusal verdict is Dolt's Error 3141 with the marker echoed as
// json_extract's argument — not the marker anywhere in stderr: the CLI echoes
// the failing statement in every batch diagnostic, so an ordinary error on a
// fetch whose remote is literally named gc-remote-op-lock-held must be
// reported as the fetch error it is, never as "already in flight" (codex r8).
func TestSyncGateMarkerInOrdinaryErrorIsNotRefusal(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "dolt.log")
	body := fakeDoltHeader(logPath, "main") +
		"  *\"CALL DOLT_FETCH(\"*) printf 'id\\n63\\n' ; printf 'error on line 1 for query CALL DOLT_FETCH(''gc-remote-op-lock-held'', ''main''): Error 1105 (HY000): remote not found\\n' >&2 ; exit 1 ;;\n" +
		"esac\nexit 0\n"
	installFFFakeDolt(t, binDir, body)
	out := runFFSyncFails(t, binDir, "--db", "app")
	if strings.Contains(out, "already in flight") {
		t.Fatalf("an ordinary error echoing the marker must not read as a gate refusal.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch failed (exit 1)") || !strings.Contains(out, "remote not found") {
		t.Fatalf("expected the ordinary fetch error with dolt's stderr replayed.\nout:\n%s", out)
	}
	if pushed(readLog(t, logPath)) {
		t.Fatalf("a failed fetch must NEVER push.\nout:\n%s", out)
	}
}

// An in-flight DOLT_FETCH with no database attribution (an older gc dolt sync
// issued it, or an operator did, without --use-db) may be this database's:
// it counts, fail closed.
func TestSyncFetchInFlightUnattributedSkips(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf 'Id,Time,db\\n43,30,\\n' ; exit 0")
	out := runFFSyncFails(t, binDir, "--db", "app")
	if fetched(readLog(t, logPath)) {
		t.Fatalf("an unattributed in-flight fetch must block this db's fetch.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch already in flight for 30s (session 43)") {
		t.Fatalf("expected the in-flight skip line.\nout:\n%s", out)
	}
}

func TestSyncProcesslistQueryFailureSkipsNeverFetches(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf 'processlist: boom\\n' >&2 ; exit 1")
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if fetched(log) || pushed(log) {
		t.Fatalf("a failed processlist query must skip without fetching or pushing (fail closed).\nout:\n%s\nlog:\n%s", out, log)
	}
	if !strings.Contains(out, "app: ERROR: processlist query failed (exit 1)") || !strings.Contains(out, "app: processlist: boom") {
		t.Fatalf("expected the processlist failure line with dolt's stderr replayed.\nout:\n%s", out)
	}
}

// An answer without the `Id,Time,db` header is not a processlist answer (an
// empty stdout, a banner, a wrapper that swallowed the result): it must not
// be read as "nothing in flight".
func TestSyncProcesslistAnswerWithoutHeaderSkips(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "exit 0")
	out := runFFSyncFails(t, binDir, "--db", "app")
	if fetched(readLog(t, logPath)) {
		t.Fatalf("a header-less processlist answer must skip the fetch (fail closed).\nout:\n%s", out)
	}
	if !strings.Contains(out, "processlist query failed") || !strings.Contains(out, "not a processlist answer") {
		t.Fatalf("expected the unrecognized-answer skip line.\nout:\n%s", out)
	}
}

// A header that is not exactly `Id,Time,db` (three fields, each bare or
// quoted as a whole) is not a processlist answer either: `"Id,Time,db"` is
// ONE quoted field and `Id",Time,db` a broken one (codex r11 — stripping every
// quote before the compare accepted both as an empty processlist).
func TestSyncProcesslistMalformedHeaderSkips(t *testing.T) {
	for _, hdr := range []string{`"Id,Time,db"`, `Id",Time,db`, `Id,Time`, `Id,Time,db,Info`} {
		binDir := t.TempDir()
		logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf '"+strings.ReplaceAll(hdr, `"`, `\"`)+"\\n' ; exit 0")
		out := runFFSyncFails(t, binDir, "--db", "app")
		if fetched(readLog(t, logPath)) {
			t.Fatalf("header %q: not a processlist answer, the fetch must be skipped.\nout:\n%s", hdr, out)
		}
		if !strings.Contains(out, "not a processlist answer") {
			t.Fatalf("header %q: expected the unrecognized-answer skip line.\nout:\n%s", hdr, out)
		}
	}
	// A header quoted field by field is fine.
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf '\"Id\",\"Time\",\"db\"\\n' ; exit 0")
	out := runFFSync(t, binDir, "--db", "app")
	if !fetched(readLog(t, logPath)) {
		t.Fatalf("a header with each field quoted is a processlist answer with no rows: the fetch must run.\nout:\n%s", out)
	}
}

func TestSyncFetchTimeoutKillsItsServerSideSession(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeoutKill(t, binDir, "77", []string{"77,60,app"}, false)
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch timed out") {
		t.Fatalf("expected the 'fetch timed out' status.\nout:\n%s", out)
	}
	fetchAt := strings.Index(log, "CALL DOLT_FETCH(")
	killAt := guardedKillAt(t, log, "77")
	if fetchAt < 0 || killAt < 0 || killAt < fetchAt {
		t.Fatalf("the session the fetch printed about itself must be KILLed after the fetch times out.\nlog:\n%s", log)
	}
	if !strings.Contains(out, "app: server-side fetch killed (session 77 no longer in flight)") {
		t.Fatalf("expected the kill line proven by the processlist after KILL.\nout:\n%s", out)
	}
}

// The verdict is the processlist AFTER the KILL, not KILL's exit code (Dolt
// 2.1.10 answers KILL of any id, gone or not, with exit 0 and no text).
func TestSyncFetchTimeoutKillNotConfirmedReportsStillInFlight(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeoutKill(t, binDir, "77", []string{"77,60,app"}, true)
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	assertGuardedKill(t, log, "77")
	if !strings.Contains(out, "app: server-side fetch NOT killed (session 77 still in flight after KILL)") {
		t.Fatalf("a session still listed after KILL must be reported as NOT killed.\nout:\n%s", out)
	}
}

// The configured fetch bound is what the fetch runs under, proven by the
// recorded gtimeout arguments (codex r7 found the pull side unproven; the sync
// side had the same gap): a script that hardcoded the default would fail this.
func TestSyncFetchBoundReachesTheFetch(t *testing.T) {
	binDir := t.TempDir()
	tlogPath := writeRecordingGtimeout(t, binDir)
	logPath := writeSyncFakeDoltFetchTimeoutKill(t, binDir, "77", []string{"77,60,app"}, false)
	outB, err := ffSyncCmd(t, binDir, []string{"GC_DOLT_SYNC_FETCH_TIMEOUT_SECS=9"}, "--db", "app").CombinedOutput()
	out := string(outB)
	if err == nil {
		t.Fatalf("a timed-out fetch must exit non-zero.\nout:\n%s", out)
	}
	if !strings.Contains(out, "app: fetch timed out after 9s") {
		t.Fatalf("expected the timeout line naming the configured bound.\nout:\n%s", out)
	}
	assertGuardedKill(t, readLog(t, logPath), "77")
	assertBounded(t, readLog(t, tlogPath), "9", "CALL DOLT_FETCH(")
}

// No random bytes, no run: with an `od` that fails, sync stops before any
// dolt call with exit 2 — a nonce that could repeat (pid-epoch) would let a
// restarted server's reused id pass the ownership check (codex r14).
func TestSyncRefusesToRunWithoutARandomNonce(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf 'Id,Time,db\\n' ; exit 0")
	if err := os.WriteFile(filepath.Join(binDir, "od"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake od: %v", err)
	}
	outB, err := ffSyncCmd(t, binDir, nil, "--db", "app").CombinedOutput()
	out := string(outB)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("want exit 2 before any database is touched, got err=%v\nout:\n%s", err, out)
	}
	if !strings.Contains(out, "cannot read 8 random bytes") || !strings.Contains(out, "gc dolt sync: no run nonce") {
		t.Fatalf("expected the nonce refusal lines.\nout:\n%s", out)
	}
	if readLog(t, logPath) != "" {
		t.Fatalf("no dolt call may run without a nonce.\nlog:\n%s", readLog(t, logPath))
	}
}

// The CALL refused itself: the session that sent it no longer held this run's
// lock (the client reconnected between the gate and the CALL — codex r12).
// Reported on its own line, nothing KILLed, nothing pushed, non-zero exit.
func TestSyncFetchLostRunLockBeforeCallSkipsKillsNothing(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "dolt.log")
	body := fakeDoltHeader(logPath, "main") +
		"  *\"CALL DOLT_FETCH(\"*) printf 'id\\n65\\n' ; printf 'error on line 1 for query CALL DOLT_FETCH(IF(...)): Error 3141 (HY000): Invalid JSON text in argument 1 to function json_extract: \"gc-remote-op-lost\"\\n' >&2 ; exit 1 ;;\n" +
		"  *\"KILL \"*) : > \"" + filepath.Join(binDir, "killed") + "\" ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	installFFFakeDolt(t, binDir, body)
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) || strings.Contains(log, "KILL ") {
		t.Fatalf("a CALL that refused itself ran no procedure: nothing to kill, nothing to push.\nout:\n%s\nlog:\n%s", out, log)
	}
	if !strings.Contains(out, "app: fetch not sent — this session lost the run lock between the gate and the CALL") || !strings.Contains(out, "NOT pushed") {
		t.Fatalf("expected the lost-lock line.\nout:\n%s", out)
	}
}

// The KILL is guarded by this run's lock in its own batch: when the id no
// longer holds it — a restarted server reused the number for someone else —
// the batch stops before the KILL and a session still listed is reported as
// NOT ours; a session already gone is reported as ended (codex r12).
func TestSyncFetchTimeoutKillGuardedByRunLock(t *testing.T) {
	notOwner := "printf 'error on line 1 for query SELECT IF(IS_USED_LOCK(...)) AS own: Error 3141 (HY000): Invalid JSON text in argument 1 to function json_extract: \"gc-remote-op-not-owner\"\\n' >&2 ; exit 1 ;;\n"
	for _, tc := range []struct{ name, rowsAfter, want string }{
		{"reused id still listed", "77,60,app\\n", "app: server-side fetch NOT killed: session 77 does not hold this run's lock (gc_remote_op_run:app:"},
		{"session already gone", "", "app: server-side fetch already ended (session 77 is gone; nothing killed)"},
	} {
		binDir := t.TempDir()
		logPath := filepath.Join(binDir, "dolt.log")
		started := filepath.Join(binDir, "fetch-started")
		body := fakeDoltPreamble(logPath, "main") +
			"  *\"information_schema.processlist\"*)\n" +
			"    if [ -f \"" + started + "\" ]; then printf 'Id,Time,db\\n" + tc.rowsAfter + "'\n" +
			"    else printf 'Id,Time,db\\n'; fi\n" +
			"    exit 0 ;;\n" +
			"  *\"CALL DOLT_FETCH(\"*) : > \"" + started + "\" ; printf 'id\\n77\\n' ; printf 'context deadline exceeded\\n' >&2 ; exit 124 ;;\n" +
			"  *\"KILL \"*) " + notOwner +
			fakeDoltRunLockFreeArm +
			"esac\nexit 0\n"
		installFFFakeDolt(t, binDir, body)
		out := runFFSyncFails(t, binDir, "--db", "app")
		log := readLog(t, logPath)
		if pushed(log) {
			t.Fatalf("%s: a fetch timeout must NEVER push.\nout:\n%s", tc.name, out)
		}
		assertGuardedKill(t, log, "77")
		if !strings.Contains(out, tc.want) || strings.Contains(out, "no longer in flight") {
			t.Fatalf("%s: expected %q and never a kill confirmation.\nout:\n%s", tc.name, tc.want, out)
		}
	}
}

// The verdict after KILL is a processlist read too: an answer with a malformed
// row (here a fourth column, which an earlier parser dropped as "another
// database" — codex r7) must refuse to confirm the kill, never report the
// session gone.
func TestSyncFetchTimeoutKillVerdictRefusesMalformedAnswer(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeoutKill(t, binDir, "77", []string{"77,60,app,extra"}, true)
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	assertGuardedKill(t, log, "77")
	if strings.Contains(out, "no longer in flight") {
		t.Fatalf("a malformed processlist answer after KILL must not confirm the kill.\nout:\n%s", out)
	}
	if !strings.Contains(out, "kill NOT confirmed: the processlist query failed after KILL") || !strings.Contains(out, "malformed processlist row") {
		t.Fatalf("expected the kill-not-confirmed line with the malformed-row reason.\nout:\n%s", out)
	}
}

// No connection id captured (the client died before the server answered):
// nothing is KILLed. Absence from the earlier processlist read does not prove
// that a session listed now is ours (an operator's pull can appear in the
// same window), so the in-flight remote operations on the server are
// reported for the operator and the run fails.
func TestSyncFetchTimeoutWithoutIDKillsNothingAndReportsUnproven(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeoutKill(t, binDir, "", []string{"88,61,app", "89,5,"}, false)
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	if strings.Contains(log, "KILL ") {
		t.Fatalf("without a session id, ownership is unproven: nothing may be KILLed.\nlog:\n%s", log)
	}
	want := "app: server-side fetch NOT killed: this client never learned its session id, and ownership of the in-flight remote operation(s) on the server (Id 88 89) cannot be proven"
	if !strings.Contains(out, want) {
		t.Fatalf("expected %q\nout:\n%s", want, out)
	}
}

func TestSyncFetchTimeoutWithoutIDAndNothingListedKillsNothing(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeoutKill(t, binDir, "", nil, false)
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if strings.Contains(log, "KILL ") {
		t.Fatalf("nothing in flight after the timeout: nothing to KILL.\nlog:\n%s", log)
	}
	if !strings.Contains(out, "app: server-side fetch already ended (nothing in flight to kill)") {
		t.Fatalf("expected the already-ended line.\nout:\n%s", out)
	}
}

// The fetch statement attributes its session to the database (--use-db fills
// the processlist DB column; a `USE` inside the query does not) and prints its
// own connection id first, so the KILL operand is the session's own word.
func TestSyncFetchIsAttributedAndSelfIdentifying(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 1, 0)
	out := runFFSync(t, binDir, "--db", "app")
	var fetchLine string
	for _, line := range strings.Split(readLog(t, logPath), "\n") {
		if strings.Contains(line, "CALL DOLT_FETCH(") {
			fetchLine = line
			break
		}
	}
	if fetchLine == "" {
		t.Fatalf("no fetch issued.\nout:\n%s", out)
	}
	if !strings.Contains(fetchLine, "--use-db app") {
		t.Fatalf("the fetch must run with --use-db app so the server attributes the session.\nline: %s", fetchLine)
	}
	// The server-side gate is the id: this session takes the database's lock
	// and this run's lock (timeout 0) and prints its own connection id in the
	// same statement, or the batch stops before the CALL is sent (verified on
	// Dolt 2.1.10).
	assertGateBeforeCall(t, fetchLine, "app", "CALL DOLT_FETCH(")
}

// runNonceRe is the run nonce's shape: 16 hex digits from /dev/urandom, or
// pid-epoch when the device is unavailable.
const runNonceRe = `([0-9a-f]{16}|[0-9]+-[0-9]+)`

// guardedKillAt returns the index in the fake dolt log of the COMPLETE guarded
// KILL batch for id (the fixtures' database is always `app`) — `PREPARE gc_kill FROM 'KILL <id>'; SELECT
// IF(IS_USED_LOCK('<this run's lock for db>') = <id>, 1, JSON_EXTRACT(
// 'gc-remote-op-not-owner', '$')) AS own; EXECUTE gc_kill; DEALLOCATE PREPARE
// gc_kill` — where the run lock is the one the fetch/pull gate took (its nonce
// is read from the gate line), or -1. A KILL of another id, an unguarded KILL,
// a guard on another lock or a guard that is not a comparison to the id do
// not count (codex r10, r13).
func guardedKillAt(t *testing.T, log, id string) int {
	t.Helper()
	const db = "app"
	gate := regexp.MustCompile(regexp.QuoteMeta("GET_LOCK('gc_remote_op_run:"+db+":") + runNonceRe + regexp.QuoteMeta("', 0)"))
	m := gate.FindStringSubmatch(log)
	if m == nil {
		t.Fatalf("no gate with a run lock for %s in the log.\nlog:\n%s", db, log)
	}
	batch := "PREPARE gc_kill FROM 'KILL " + id + "'; SELECT IF(IS_USED_LOCK('gc_remote_op_run:" + db + ":" + m[1] + "') = " + id + ", 1, JSON_EXTRACT('gc-remote-op-not-owner', '$')) AS own; EXECUTE gc_kill; DEALLOCATE PREPARE gc_kill\n"
	return strings.Index(log, batch)
}

// assertGuardedKill requires the complete guarded KILL batch for id in the log.
func assertGuardedKill(t *testing.T, log, id string) {
	t.Helper()
	if guardedKillAt(t, log, id) < 0 {
		t.Fatalf("expected the complete guarded KILL batch for session %s (prepared KILL, ownership check on this run's lock, EXECUTE, DEALLOCATE).\nlog:\n%s", id, log)
	}
}

// assertGateBeforeCall pins the batch shape `… GET_LOCK('gc_remote_op:<db>', 0) … CONNECTION_ID() … AS id; CALL …`.
func assertGateBeforeCall(t *testing.T, line, db, call string) {
	t.Helper()
	// The COMPLETE gate expression is the spec (codex r9: a check on the
	// GET_LOCK substring alone accepted `>= 0`, which lets the CALL through
	// while another session holds the lock): the lock name lowercased on the
	// server (`app` and `APP` are one database and must be one lock), timeout
	// 0, this run's own lock (`gc_remote_op_run:<db>:<16 hex>`), the `= 1`
	// tests, CONNECTION_ID() as the true branch — the gate statement IS the
	// id the KILL targets, so the recorded id held the locks by construction
	// (the mayor's gate r1: a separate id statement before the gate recorded
	// a session that a reconnect could leave lockless) — and the marked JSON
	// error branch. The CALL's FIRST ARGUMENT re-proves that the session
	// still holds the same run lock (codex r12: a pooled client that
	// reconnected between the gate and the CALL would otherwise fetch on a
	// lockless session).
	gateRe := regexp.MustCompile(regexp.QuoteMeta("SELECT IF(GET_LOCK(CONCAT('gc_remote_op:', LOWER('"+db+"')), 0) = 1 AND GET_LOCK('gc_remote_op_run:"+db+":") + runNonceRe + regexp.QuoteMeta("', 0) = 1, CONNECTION_ID(), JSON_EXTRACT('gc-remote-op-lock-held', '$')) AS id;"))
	callRe := regexp.MustCompile(regexp.QuoteMeta(call+"IF(IS_USED_LOCK('gc_remote_op_run:"+db+":") + runNonceRe + regexp.QuoteMeta("') = CONNECTION_ID(), '") + `[^']+` + regexp.QuoteMeta("', JSON_EXTRACT('gc-remote-op-lost', '$')), "))
	gateM, callM := gateRe.FindStringSubmatchIndex(line), callRe.FindStringSubmatchIndex(line)
	if strings.Contains(line, "SELECT CONNECTION_ID()") {
		t.Fatalf("no standalone SELECT CONNECTION_ID() may precede the gate: the id is the gate statement's own answer.\nline: %s", line)
	}
	if gateM == nil || callM == nil || gateM[0] >= callM[0] {
		t.Fatalf("the batch must be the complete gate returning the id %s, then the CALL whose first argument is the ownership check %s.\nline: %s", gateRe, callRe, line)
	}
	if line[gateM[2]:gateM[3]] != line[callM[2]:callM[3]] {
		t.Fatalf("the CALL must re-check the SAME run lock the gate took (%q vs %q).\nline: %s", line[gateM[2]:gateM[3]], line[callM[2]:callM[3]], line)
	}
	if !strings.Contains(line, "JSON_EXTRACT('gc-remote-op-lock-held', '$')") {
		t.Fatalf("the gate's else branch must raise the marked error.\nline: %s", line)
	}
}

// A processlist answer with a row that is not `digits,digits,…` (a NULL Time,
// a truncated line) is a session whose state is unknown: the whole answer is
// refused and the fetch skipped, fail closed — never "nothing in flight".
func TestSyncProcesslistMalformedRowSkips(t *testing.T) {
	for _, row := range []string{
		`42,NULL,app`,     // a NULL Time
		`"12,34",60,app`,  // a quoted field that field-splitting would read as Id 12, Time 34
		`"4""2",60,app`,   // a quoted, escaped Id
		`42`,              // a truncated line
		`abc,60,app`,      // a non-numeric Id
		` 42,60,app`,      // leading whitespace
		`42,60,app,extra`, // an extra column: the db field is not ONE CSV field (codex r7 — was read as "another database" and dropped)
		`42,60,"app`,      // an unterminated quote
		`42,60,ap"p`,      // a stray quote in an unquoted field
		`42,60,"app"x`,    // text after the closing quote
	} {
		binDir := t.TempDir()
		logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf 'Id,Time,db\\n"+strings.ReplaceAll(row, `"`, `\"`)+"\\n' ; exit 0")
		out := runFFSyncFails(t, binDir, "--db", "app")
		log := readLog(t, logPath)
		if fetched(log) || pushed(log) {
			t.Fatalf("row %q: a malformed processlist row must skip without fetching or pushing.\nout:\n%s\nlog:\n%s", row, out, log)
		}
		if !strings.Contains(out, "processlist query failed") || !strings.Contains(out, "malformed processlist row") {
			t.Fatalf("row %q: expected the malformed-row skip line.\nout:\n%s", row, out)
		}
	}
}

// GNU timeout escalates to SIGKILL after --kill-after and then exits 137, not
// 124. The client is just as dead and the server-side fetch just as alive: the
// recorded session must still be KILLed.
func TestSyncFetchClientExit137StillKillsServerSideSession(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "dolt.log")
	started := filepath.Join(binDir, "fetch-started")
	killedPrefix := filepath.Join(binDir, "killed-")
	killed := killedPrefix + "91"
	body := fakeDoltPreamble(logPath, "main") +
		"  *\"information_schema.processlist\"*)\n" +
		"    if [ -f \"" + killed + "\" ]; then printf 'Id,Time,db\\n'\n" +
		"    elif [ -f \"" + started + "\" ]; then printf 'Id,Time,db\\n91,70,app\\n'\n" +
		"    else printf 'Id,Time,db\\n'; fi\n" +
		"    exit 0 ;;\n" +
		"  *\"CALL DOLT_FETCH(\"*) : > \"" + started + "\" ; printf 'id\\n91\\n' ; exit 137 ;;\n" +
		"  *\"KILL \"*) k=$(printf '%s' \"$*\" | sed -n 's/.*KILL \\([0-9][0-9]*\\).*/\\1/p') ; : > \"" + killedPrefix + "${k}\" ; exit 0 ;;\n" +
		fakeDoltRunLockFreeArm +
		"esac\nexit 0\n"
	installFFFakeDolt(t, binDir, body)
	out := runFFSync(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("exit 137 must NEVER push.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch timed out") || !strings.Contains(out, "client exit 137") {
		t.Fatalf("exit 137 is the bound's SIGKILL escalation and must be reported as a timeout.\nout:\n%s", out)
	}
	if guardedKillAt(t, log, "91") < 0 || !strings.Contains(out, "server-side fetch killed (session 91 no longer in flight)") {
		t.Fatalf("the recorded session must be KILLed after exit 137.\nout:\n%s\nlog:\n%s", out, log)
	}
}

// Two runners that both read "nothing in flight" cannot both fetch: the fetch
// batch takes the server's session lock for the database before its CALL, and
// when another session holds it the server stops the batch with the marked
// error. The script reports the refusal, pushes nothing, and has nothing to
// KILL (our CALL was never sent).
func TestSyncFetchGateRefusedSkipsNeverPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "dolt.log")
	body := fakeDoltHeader(logPath, "main") +
		"  *\"CALL DOLT_FETCH(\"*) printf 'id\\n61\\n' ; printf 'error on line 1 for query SELECT IF(GET_LOCK(...)) AS gate: Error 3141 (HY000): Invalid JSON text in argument 1 to function json_extract: \"gc-remote-op-lock-held\"\\n' >&2 ; exit 1 ;;\n" +
		"esac\nexit 0\n"
	installFFFakeDolt(t, binDir, body)
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("a refused gate must NEVER push.\nout:\n%s", out)
	}
	if strings.Contains(log, "KILL ") {
		t.Fatalf("a refused gate sent no CALL: nothing to KILL.\nlog:\n%s", log)
	}
	want := "app: fetch already in flight — the server refused a second one (session lock gc_remote_op:app held) — skipped (NOT pushed)"
	if !strings.Contains(out, want) {
		t.Fatalf("expected %q\nout:\n%s", want, out)
	}
	if strings.Contains(out, "fetch failed (exit 1)") {
		t.Fatalf("a gate refusal is a skip, not a fetch failure.\nout:\n%s", out)
	}
}

// The bound's verdict outranks anything the client printed: a client the
// bound killed may have written the first-push signal ("no branches found in
// remote" / "invalid ref spec") before stalling. Exit 124 or 137 with that
// text is a timeout — the recorded session is KILLed and nothing is pushed —
// never a first push.
func TestSyncFetchTimeoutOutranksFirstPushText(t *testing.T) {
	for _, tc := range []struct {
		code int
		text string
	}{
		{124, "no branches found in remote"},
		{137, "fetch failed: invalid ref spec"},
	} {
		binDir := t.TempDir()
		logPath := filepath.Join(binDir, "dolt.log")
		started := filepath.Join(binDir, "fetch-started")
		killedPrefix := filepath.Join(binDir, "killed-")
		killed := killedPrefix + "93"
		body := fakeDoltPreamble(logPath, "main") +
			"  *\"information_schema.processlist\"*)\n" +
			"    if [ -f \"" + killed + "\" ]; then printf 'Id,Time,db\\n'\n" +
			"    elif [ -f \"" + started + "\" ]; then printf 'Id,Time,db\\n93,61,app\\n'\n" +
			"    else printf 'Id,Time,db\\n'; fi\n" +
			"    exit 0 ;;\n" +
			"  *\"CALL DOLT_FETCH(\"*) : > \"" + started + "\" ; printf 'id\\n93\\n' ; printf '" + tc.text + "\\n' >&2 ; exit " + fmt.Sprint(tc.code) + " ;;\n" +
			"  *\"KILL \"*) k=$(printf '%s' \"$*\" | sed -n 's/.*KILL \\([0-9][0-9]*\\).*/\\1/p') ; : > \"" + killedPrefix + "${k}\" ; exit 0 ;;\n" +
			fakeDoltRunLockFreeArm +
			"esac\nexit 0\n"
		installFFFakeDolt(t, binDir, body)
		out := runFFSyncFails(t, binDir, "--db", "app")
		log := readLog(t, logPath)
		if pushed(log) {
			t.Fatalf("exit %d with %q: a dead client must NEVER push.\nout:\n%s\nlog:\n%s", tc.code, tc.text, out, log)
		}
		if !strings.Contains(out, "fetch timed out") || strings.Contains(out, "first push") {
			t.Fatalf("exit %d with %q: the timeout must outrank the first-push text.\nout:\n%s", tc.code, tc.text, out)
		}
		if guardedKillAt(t, log, "93") < 0 || !strings.Contains(out, "server-side fetch killed (session 93 no longer in flight)") {
			t.Fatalf("exit %d with %q: the recorded session must be KILLed.\nout:\n%s\nlog:\n%s", tc.code, tc.text, out, log)
		}
	}
}

// ---------------------------------------------------------------------------
// gp-f2yq round 2 (the mayor's codex gate r1): the session id the bound's
// KILL targets comes from the SAME statement that takes the two locks, and the
// kill helper never returns 0 while any session holds this run's lock.
// ---------------------------------------------------------------------------

// The gate statement is the id: `SELECT IF(GET_LOCK(db) = 1 AND GET_LOCK(run)
// = 1, CONNECTION_ID(), <marked error>) AS id` is the FIRST statement after
// USE, and no standalone `SELECT CONNECTION_ID()` precedes it — a pooled
// client that reconnected between a separate id statement and the gate would
// otherwise record an id that never held the locks, and the timeout path
// would report "already ended" while the reconnected session fetched on with
// both locks (the mayor's codex gate r1 on 1b4b4964d).
func TestSyncFetchIDIsTheGateStatement(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 1, 0)
	out := runFFSync(t, binDir, "--db", "app")
	line := fetchLineOf(t, readLog(t, logPath), "CALL DOLT_FETCH(")
	if line == "" {
		t.Fatalf("no fetch issued.\nout:\n%s", out)
	}
	assertIDIsTheGate(t, line, "app", "CALL DOLT_FETCH(")
}

// fetchLineOf returns the first fake-dolt log line carrying call, or "".
func fetchLineOf(t *testing.T, log, call string) string {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, call) {
			return line
		}
	}
	return ""
}

// assertIDIsTheGate pins the round-2 batch shape: `USE \`<db>\`; <the complete
// gate returning CONNECTION_ID() AS id>; CALL …` with NO standalone
// `SELECT CONNECTION_ID()` anywhere in the batch. The id and the locks are one
// statement, so the recorded id is the lock holder by construction.
func assertIDIsTheGate(t *testing.T, line, db, call string) {
	t.Helper()
	if strings.Contains(line, "SELECT CONNECTION_ID()") {
		t.Fatalf("no standalone SELECT CONNECTION_ID() may precede the gate: the id must come from the gate statement itself.\nline: %s", line)
	}
	gateRe := regexp.MustCompile(regexp.QuoteMeta("USE `"+db+"`; SELECT IF(GET_LOCK(CONCAT('gc_remote_op:', LOWER('"+db+"')), 0) = 1 AND GET_LOCK('gc_remote_op_run:"+db+":") + runNonceRe + regexp.QuoteMeta("', 0) = 1, CONNECTION_ID(), JSON_EXTRACT('gc-remote-op-lock-held', '$')) AS id; "+call))
	if !gateRe.MatchString(line) {
		t.Fatalf("the batch must be USE, then the gate returning CONNECTION_ID() AS id when both locks are taken, then the CALL: %s\nline: %s", gateRe, line)
	}
}

// killHelperEnv runs kill_remote_op_session alone (runtime.sh sourced, the
// fake dolt in binDir first on PATH, this run's nonce fixed) and returns the
// helper's stderr and exit code — the return code IS the rule under test.
func runKillHelper(t *testing.T, binDir, label, db, id string) (string, int) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; REMOTE_OP_RUN_NONCE=0123456789abcdef; kill_remote_op_session "$1" "$2" "$3"`, "sh", label, db, id)
	cmd.Env = append(filteredEnv("PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_CITY_PATH", "GC_PACK_DIR"),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+t.TempDir(),
		"GC_PACK_DIR="+root,
		"GC_DOLT_PORT=1",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	rc := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("kill helper: %v\n%s", err, out)
		}
		rc = ee.ExitCode()
	}
	return string(out), rc
}

// writeKillHelperFakeDolt installs a fake dolt for the helper alone: the
// processlist answered by processlistArm, every KILL batch by killArm, the
// run-lock holder query by holderArm (complete case arm bodies).
func writeKillHelperFakeDolt(t *testing.T, dir, processlistArm, killArm, holderArm string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"information_schema.processlist\"*) " + processlistArm + " ;;\n" +
		"  *\"KILL \"*) " + killArm + " ;;\n" +
		"  *\"COALESCE(IS_USED_LOCK(\"*) " + holderArm + " ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// The kill helper's verdict, as a table (the mayor's codex gate r1): the
// helper reads who holds THIS run's lock after the KILL attempt and never
// returns 0 while any session holds it — a guarded id that is gone means the
// session ended on its own ONLY when the lock is free; a live holder other
// than the recorded id (a record from before the id came from the gate
// statement, a server that restarted and reused the number) is reported NOT
// killed, named for the operator, exit 1. The holder answer is parsed
// strictly (`holder` header, one all-digit row; 0 = free): anything else
// refuses to confirm, exit 1.
func TestKillRemoteOpSessionNeverReturnsZeroWhileTheRunLockIsHeld(t *testing.T) {
	const runLock = "gc_remote_op_run:app:0123456789abcdef"
	killed := "exit 0"
	notOwner := "printf 'error on line 1 for query SELECT IF(IS_USED_LOCK(...)) AS own: Error 3141 (HY000): Invalid JSON text in argument 1 to function json_extract: \"gc-remote-op-not-owner\"\\n' >&2 ; exit 1"
	none := "printf 'Id,Time,db\\n' ; exit 0"
	listed77 := "printf 'Id,Time,db\\n77,60,app\\n' ; exit 0"
	holder := func(v string) string { return "printf 'holder\\n" + v + "\\n' ; exit 0" }
	for _, tc := range []struct {
		name, id, processlist, kill, holder string
		wantRC                              int
		want, never                         string
	}{
		{"killed, gone, lock free", "77", none, killed, holder("0"), 0, "app: server-side fetch killed (session 77 no longer in flight)", "NOT killed"},
		{"guarded, gone, lock free (the server restarted)", "77", none, notOwner, holder("0"), 0, "app: server-side fetch already ended (session 77 is gone; nothing killed)", "NOT killed"},
		{"guarded, gone, another session holds this run's lock", "77", none, notOwner, holder("91"), 1, "app: server-side fetch NOT killed: session 77 is gone but session 91 holds this run's lock (" + runLock + ")", "already ended"},
		{"killed, gone, another session holds this run's lock", "77", none, killed, holder("91"), 1, "app: server-side fetch NOT killed: session 77 is gone but session 91 holds this run's lock (" + runLock + ")", "no longer in flight"},
		{"guarded, still listed", "77", listed77, notOwner, holder("77"), 1, "app: server-side fetch NOT killed: session 77 does not hold this run's lock (" + runLock + ")", "already ended"},
		{"holder query fails", "77", none, killed, "printf 'holder: boom\\n' >&2 ; exit 1", 1, "app: server-side fetch kill NOT confirmed: the run-lock holder query failed", "no longer in flight"},
		{"holder answer is not a holder answer", "77", none, killed, "printf 'nothing here\\n' ; exit 0", 1, "app: server-side fetch kill NOT confirmed: the run-lock holder query failed", "no longer in flight"},
		{"holder answer is empty", "77", none, killed, "exit 0", 1, "app: server-side fetch kill NOT confirmed: the run-lock holder query failed", "no longer in flight"},
		{"no id, nothing listed, lock free", "", none, killed, holder("0"), 0, "app: server-side fetch already ended (nothing in flight to kill)", "NOT killed"},
		{"no id, nothing listed, a session holds this run's lock", "", none, killed, holder("91"), 1, "app: server-side fetch NOT killed: this client never learned its session id, and session 91 holds this run's lock (" + runLock + ")", "already ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			logPath := writeKillHelperFakeDolt(t, binDir, tc.processlist, tc.kill, tc.holder)
			out, rc := runKillHelper(t, binDir, "fetch", "app", tc.id)
			if rc != tc.wantRC {
				t.Fatalf("exit %d, want %d.\nout:\n%s", rc, tc.wantRC, out)
			}
			if !strings.Contains(out, tc.want) || strings.Contains(out, tc.never) {
				t.Fatalf("expected %q and never %q.\nout:\n%s", tc.want, tc.never, out)
			}
			log := readLog(t, logPath)
			holderQ := "SELECT COALESCE(IS_USED_LOCK('" + runLock + "'), 0) AS holder\n"
			holderAt := strings.Index(log, holderQ)
			if holderAt < 0 {
				t.Fatalf("the helper must ask who holds this run's lock with exactly %q.\nlog:\n%s", strings.TrimSpace(holderQ), log)
			}
			if tc.id != "" {
				// The helper alone issues no gate, so the complete guarded batch is
				// spelled out with the fixed nonce (guardedKillAt reads it from a
				// gate line).
				batch := "PREPARE gc_kill FROM 'KILL " + tc.id + "'; SELECT IF(IS_USED_LOCK('" + runLock + "') = " + tc.id + ", 1, JSON_EXTRACT('gc-remote-op-not-owner', '$')) AS own; EXECUTE gc_kill; DEALLOCATE PREPARE gc_kill\n"
				if killAt := strings.Index(log, batch); killAt < 0 || holderAt < killAt {
					t.Fatalf("the complete guarded KILL batch must run, and the holder is read AFTER it.\nlog:\n%s", log)
				}
			}
			if tc.id == "" && strings.Contains(log, "KILL ") {
				t.Fatalf("without an id nothing may be KILLed.\nlog:\n%s", log)
			}
		})
	}
}

// The runner path of the same rule: the fetch's recorded id is gone (the KILL
// batch stopped at its guard) but a live session other than the recorded id
// holds this run's lock — the reconnected client's session, fetching on with
// both locks. The run must say NOT killed, name that session, and never
// "already ended" (RED on 1b4b4964d, which printed already ended).
func TestSyncFetchTimeoutKillRefusesWhenAnotherSessionHoldsTheRunLock(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "dolt.log")
	started := filepath.Join(binDir, "fetch-started")
	body := fakeDoltPreamble(logPath, "main") +
		"  *\"information_schema.processlist\"*) printf 'Id,Time,db\\n' ; exit 0 ;;\n" +
		"  *\"CALL DOLT_FETCH(\"*) : > \"" + started + "\" ; printf 'id\\n77\\n' ; printf 'context deadline exceeded\\n' >&2 ; exit 124 ;;\n" +
		"  *\"KILL \"*) printf 'error on line 1 for query SELECT IF(IS_USED_LOCK(...)) AS own: Error 3141 (HY000): Invalid JSON text in argument 1 to function json_extract: \"gc-remote-op-not-owner\"\\n' >&2 ; exit 1 ;;\n" +
		"  *\"COALESCE(IS_USED_LOCK(\"*) printf 'holder\\n91\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	installFFFakeDolt(t, binDir, body)
	out := runFFSyncFails(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	assertGuardedKill(t, log, "77")
	want := "app: server-side fetch NOT killed: session 77 is gone but session 91 holds this run's lock (gc_remote_op_run:app:"
	if !strings.Contains(out, want) || strings.Contains(out, "already ended") || strings.Contains(out, "no longer in flight") {
		t.Fatalf("expected %q and never an ended/killed line.\nout:\n%s", want, out)
	}
}
