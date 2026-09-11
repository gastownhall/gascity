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
	"strings"
	"testing"
)

// runFFSyncEnv sets up a one-DB ("app") SQL-mode city with the fake dolt
// already installed in binDir, runs `gc dolt sync <args>` with extraEnv
// appended after the base sync env (so an override such as
// GC_DOLT_REMOTE_<DB> takes effect), and returns combined output plus the
// command's error so callers that must assert the sync itself did not fail
// (as opposed to merely producing unexpected output) can do so.
func runFFSyncEnv(t *testing.T, binDir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
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
	out := runFFSync(t, binDir, "--db", "app")
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
	killed := filepath.Join(dir, "killed")
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
		"  *\"KILL \"*) : > \"" + killed + "\" ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// fetched reports whether the fake dolt was asked to run the fetch procedure.
// It matches the CALL itself, not the bare procedure name: the single-flight
// processlist query names DOLT_FETCH inside its LIKE pattern too.
func fetched(log string) bool { return strings.Contains(log, "CALL DOLT_FETCH(") }

func TestSyncFetchInFlightSkipsNeverFetches(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf 'Id,Time,db\\n42,7200,app\\n' ; exit 0")
	out := runFFSync(t, binDir, "--db", "app")
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
	if !strings.Contains(log, "db = 'app'") {
		t.Fatalf("the single-flight check must be scoped to this database.\nlog:\n%s", log)
	}
}

// An in-flight DOLT_FETCH with no database attribution (an older gc dolt sync
// issued it, or an operator did, without --use-db) may be this database's:
// it counts, fail closed.
func TestSyncFetchInFlightUnattributedSkips(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltProcesslist(t, binDir, "printf 'Id,Time,db\\n43,30,\\n' ; exit 0")
	out := runFFSync(t, binDir, "--db", "app")
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
	out := runFFSync(t, binDir, "--db", "app")
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
	out := runFFSync(t, binDir, "--db", "app")
	if fetched(readLog(t, logPath)) {
		t.Fatalf("a header-less processlist answer must skip the fetch (fail closed).\nout:\n%s", out)
	}
	if !strings.Contains(out, "processlist query failed") || !strings.Contains(out, "not a processlist answer") {
		t.Fatalf("expected the unrecognized-answer skip line.\nout:\n%s", out)
	}
}

func TestSyncFetchTimeoutKillsItsServerSideSession(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeoutKill(t, binDir, "77", []string{"77,60,app"}, false)
	out := runFFSync(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch timed out") {
		t.Fatalf("expected the 'fetch timed out' status.\nout:\n%s", out)
	}
	fetchAt := strings.Index(log, "CALL DOLT_FETCH(")
	killAt := strings.Index(log, "KILL 77")
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
	out := runFFSync(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	if !strings.Contains(log, "KILL 77") {
		t.Fatalf("KILL must be issued.\nlog:\n%s", log)
	}
	if !strings.Contains(out, "app: server-side fetch NOT killed (session 77 still in flight after KILL)") {
		t.Fatalf("a session still listed after KILL must be reported as NOT killed.\nout:\n%s", out)
	}
}

// No connection id captured (the client died before the server answered):
// every in-flight remote operation attributed to the db is ended — the
// single-flight check found none before ours started, so each is ours or a
// concurrent runner's that raced the same window, and none may survive.
func TestSyncFetchTimeoutWithoutIDKillsEveryAttributedSession(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeoutKill(t, binDir, "", []string{"88,61,app", "89,5,"}, false)
	out := runFFSync(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if pushed(log) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	for _, id := range []string{"88", "89"} {
		if !strings.Contains(log, "KILL "+id) {
			t.Fatalf("session %s must be KILLed.\nlog:\n%s", id, log)
		}
		if !strings.Contains(out, "server-side fetch killed (session "+id+" no longer in flight)") {
			t.Fatalf("expected the kill line for session %s.\nout:\n%s", id, out)
		}
	}
}

func TestSyncFetchTimeoutWithoutIDAndNothingListedKillsNothing(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeoutKill(t, binDir, "", nil, false)
	out := runFFSync(t, binDir, "--db", "app")
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
	if !strings.Contains(fetchLine, "SELECT CONNECTION_ID() AS id;") {
		t.Fatalf("the fetch statement must print its own connection id first.\nline: %s", fetchLine)
	}
}
