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

// fakeDoltHeader is the shared preamble: log argv and answer the remote-lookup
// + active_branch metadata queries the sync path issues before classification.
func fakeDoltHeader(logPath, branch string) string {
	return "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"SELECT name, url FROM dolt_remotes ORDER BY name\"*)\n" +
		"    printf 'name,url\\norigin,https://example.invalid/repo\\n' ; exit 0 ;;\n" +
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
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"mirror,git+https://example.invalid/mirror",
		"origin,git+https://github.com/gastownhall/beads",
	})
	out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "none local") {
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
