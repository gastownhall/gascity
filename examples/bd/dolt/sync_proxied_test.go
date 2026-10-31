package dolt_test

// `gc dolt sync` on a bd-owned proxied city. bd starts and owns every scope's
// sql-server, so sync has no managed port to dial and must not touch dolt or
// bd directly. It reaches each bead scope (the city and every active rig)
// through `gc bd --city <city> [--rig <rig>] sql --csv`, one statement per
// call, and applies the same remote-selection and fast-forward rules as the
// managed path.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// proxiedSyncFakeGC is a gc test double for the proxied sync path. It serves
// `gc rig list --json` and `gc bd --city C [--rig R] sql --csv <query>`, logs
// every call, and records each DOLT_PUSH per database. Like real bd in
// proxied-server mode, a multi-statement query only answers "OK". Behavior is
// configured through environment variables:
//
//	FAKE_RIG_LIST_JSON       gc rig list --json output (default: no rigs)
//	FAKE_RIG_LIST_FAIL       non-empty: gc rig list exits 1
//	FAKE_SCOPE_DBS           "city=hq rig:api=apidb" (default city=hq)
//	FAKE_NOT_BD_SCOPES       scopes gc refuses as not bd-backed (file provider)
//	FAKE_UNREACHABLE_SCOPES  scopes whose bd calls fail
//	FAKE_REMOTE_URLS         "hq=git+ssh://host/repo" (default file:///example.invalid/<db>)
//	FAKE_NO_REMOTE_DBS       databases with no configured remote
//	FAKE_AHEAD / FAKE_BEHIND commit counts the classify queries report (default 1 / 0)
//	FAKE_PUSH_FAIL_DBS       databases whose DOLT_PUSH fails
type proxiedSyncFakeGC struct {
	logPath  string
	stateDir string
}

func writeProxiedSyncFakeGC(t *testing.T, binDir string) proxiedSyncFakeGC {
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
  [ -z "${FAKE_RIG_LIST_FAIL:-}" ] || { echo "fake gc: rig list failed" >&2; exit 1; }
  printf '%%s\n' "${FAKE_RIG_LIST_JSON:-{\"rigs\":[]\}}"
  exit 0
fi
if [ "${1:-}" != "bd" ]; then
  echo "fake gc: unexpected command: $*" >&2
  exit 98
fi
# Real gc prints config warnings on stderr; sync must parse only stdout.
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
db=hq
for pair in ${FAKE_SCOPE_DBS:-}; do
  case "$pair" in "$scope="*) db="${pair#*=}" ;; esac
done
if [ "${1:-} ${2:-}" != "sql --csv" ] || [ "$#" -ne 3 ]; then
  echo "fake gc: unexpected bd verb: $*" >&2
  exit 98
fi
q=$3
case "$q" in
  *";"*) printf 'OK\n'; exit 0 ;;
esac
case "$q" in
  "SELECT DATABASE()")
    printf 'DATABASE()\n%%s\n' "$db" ;;
  "SELECT name, url FROM dolt_remotes ORDER BY name")
    printf 'name,url\n'
    case " ${FAKE_NO_REMOTE_DBS:-} " in *" $db "*) exit 0 ;; esac
    url="file:///example.invalid/$db"
    for pair in ${FAKE_REMOTE_URLS:-}; do
      case "$pair" in "$db="*) url="${pair#*=}" ;; esac
    done
    printf 'origin,%%s\n' "$url" ;;
  "SELECT active_branch()")
    printf 'active_branch()\nmain\n' ;;
  "CALL DOLT_FETCH("*)
    printf 'status\n0\n' ;;
  *"dolt_log('remotes/"*"..main')"*)
    printf 'n\n%%s\n' "${FAKE_AHEAD:-1}" ;;
  *"dolt_log('main..remotes/"*)
    printf 'n\n%%s\n' "${FAKE_BEHIND:-0}" ;;
  "CALL DOLT_PUSH("*)
    case " ${FAKE_PUSH_FAIL_DBS:-} " in
      *" $db "*) printf 'Error: query error: Error 1105 (HY000): permission denied for %%s\n' "$db" >&2; exit 1 ;;
    esac
    printf '%%s\n' "$q" >> "$state/$db.pushes"
    printf 'status,message\n0,\n' ;;
  *)
    echo "fake gc: unexpected query: $q" >&2
    exit 98 ;;
esac
exit 0
`, shellQuote(logPath), shellQuote(stateDir)))
	return proxiedSyncFakeGC{logPath: logPath, stateDir: stateDir}
}

func (f proxiedSyncFakeGC) log(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	return string(data)
}

// pushes returns the DOLT_PUSH statements the fake recorded for db.
func (f proxiedSyncFakeGC) pushes(t *testing.T, db string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.stateDir, db+".pushes"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read pushes for %s: %v", db, err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// newProxiedSyncCity lays out a proxied city and installs the fake gc. dolt and
// bd on PATH fail loudly, so any direct use of them shows up as a failure.
func newProxiedSyncCity(t *testing.T) (cityPath, binDir string, fake proxiedSyncFakeGC) {
	t.Helper()
	cityPath = t.TempDir()
	writeProxiedScope(t, cityPath)
	binDir = t.TempDir()
	fake = writeProxiedSyncFakeGC(t, binDir)
	return cityPath, binDir, fake
}

func runProxiedSync(t *testing.T, binDir, cityPath string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	return execSyncScript(filepath.Join(root, syncScript), append(syncFilteredEnv(),
		"PATH="+binDir+string(os.PathListSeparator)+hostileToolPath(t),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
	), extraEnv, args)
}

func rigListJSON(rigs ...string) string {
	return `{"rigs":[` + strings.Join(rigs, ",") + `]}`
}

func rigEntry(name, path string, hq, suspended bool) string {
	return fmt.Sprintf(`{"name":%q,"path":%q,"hq":%t,"suspended":%t}`, name, path, hq, suspended)
}

func TestSyncProxiedPushesEveryActiveScopeThroughGCBD(t *testing.T) {
	cityPath, binDir, fake := newProxiedSyncCity(t)
	rigs := rigListJSON(
		rigEntry("city", cityPath, true, false),
		rigEntry("api", filepath.Join(cityPath, "api"), false, false),
		rigEntry("cold", filepath.Join(cityPath, "cold"), false, true),
	)

	out, err := runProxiedSync(t, binDir, cityPath, []string{
		"FAKE_RIG_LIST_JSON=" + rigs,
		"FAKE_SCOPE_DBS=city=hq rig:api=apidb rig:cold=colddb",
	})
	if err != nil {
		t.Fatalf("proxied sync failed: %v\n%s", err, out)
	}
	if strings.Contains(out, proxiedNoOpMessage) {
		t.Fatalf("proxied sync still reports the lifecycle no-op:\n%s", out)
	}
	for _, want := range []string{
		"  hq: pushed main -> origin:main (file:///example.invalid/hq)",
		"  apidb: pushed main -> origin:main (file:///example.invalid/apidb)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	for _, db := range []string{"hq", "apidb"} {
		got := fake.pushes(t, db)
		if len(got) != 1 || got[0] != "CALL DOLT_PUSH('origin', 'main')" {
			t.Fatalf("%s pushes = %q, want one fast-forward push", db, got)
		}
	}

	log := fake.log(t)
	if !strings.Contains(log, "gc bd --city "+cityPath+" --rig api sql --csv") {
		t.Fatalf("rig scope not reached through gc bd --rig:\n%s", log)
	}
	// The suspended rig stays cold (any bd call would restart its proxy) and
	// the HQ entry is the city scope, not a second rig.
	for _, unwanted := range []string{"--rig cold", "--rig city"} {
		if strings.Contains(log, unwanted) {
			t.Fatalf("sync touched %q:\n%s", unwanted, log)
		}
	}
	// bd answers a multi-statement batch with a bare "OK", so every query
	// has to be a single statement against the scope's own database.
	if strings.Contains(log, "USE `") {
		t.Fatalf("sync sent a USE batch through gc bd sql:\n%s", log)
	}
}

func TestSyncProxiedClassifiesBeforePushing(t *testing.T) {
	cases := []struct {
		name          string
		ahead, behind int
		wantPush      bool
		wantErr       bool
		wantLine      string
	}{
		{"up to date", 0, 0, false, false, "  hq: up-to-date with origin:main"},
		{"behind", 0, 3, false, false, "  hq: behind 3 — pull needed (gc dolt pull)"},
		{"diverged", 2, 3, false, true, "  hq: diverged (2 ahead / 3 behind) — manual reconcile"},
		{"ahead", 4, 0, true, false, "  hq: pushed main -> origin:main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, binDir, fake := newProxiedSyncCity(t)
			out, err := runProxiedSync(t, binDir, cityPath, []string{
				fmt.Sprintf("FAKE_AHEAD=%d", tc.ahead),
				fmt.Sprintf("FAKE_BEHIND=%d", tc.behind),
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t\n%s", err, tc.wantErr, out)
			}
			if !strings.Contains(out, tc.wantLine) {
				t.Fatalf("output missing %q:\n%s", tc.wantLine, out)
			}
			if got := len(fake.pushes(t, "hq")) > 0; got != tc.wantPush {
				t.Fatalf("pushed = %t, want %t\n%s", got, tc.wantPush, out)
			}
			if !strings.Contains(fake.log(t), "CALL DOLT_FETCH('origin', 'main')") {
				t.Fatalf("sync classified without fetching first:\n%s", fake.log(t))
			}
		})
	}
}

func TestSyncProxiedForceSkipsClassification(t *testing.T) {
	cityPath, binDir, fake := newProxiedSyncCity(t)
	out, err := runProxiedSync(t, binDir, cityPath, []string{"FAKE_AHEAD=2", "FAKE_BEHIND=3"}, "--force")
	if err != nil {
		t.Fatalf("proxied sync --force failed: %v\n%s", err, out)
	}
	got := fake.pushes(t, "hq")
	if len(got) != 1 || got[0] != "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')" {
		t.Fatalf("hq pushes = %q, want one force push", got)
	}
	if strings.Contains(fake.log(t), "DOLT_FETCH") {
		t.Fatalf("--force fetched before pushing:\n%s", fake.log(t))
	}
}

func TestSyncProxiedDryRunDoesNotPush(t *testing.T) {
	cityPath, binDir, fake := newProxiedSyncCity(t)
	out, err := runProxiedSync(t, binDir, cityPath, nil, "--dry-run")
	if err != nil {
		t.Fatalf("proxied sync --dry-run failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "  hq: would push main -> origin:main (file:///example.invalid/hq) [ahead 1]") {
		t.Fatalf("dry run did not report the classification:\n%s", out)
	}
	if got := fake.pushes(t, "hq"); len(got) != 0 {
		t.Fatalf("dry run pushed: %q", got)
	}
}

// A sole network remote is ambiguous by policy on every topology: sync never
// sends private data to a non-local remote unless GC_DOLT_REMOTE_<DB> names it.
func TestSyncProxiedHonorsRemoteSelectionPolicy(t *testing.T) {
	network := []string{"FAKE_REMOTE_URLS=hq=git+ssh://git@example.invalid/hq.git"}

	t.Run("network remote without override is skipped", func(t *testing.T) {
		cityPath, binDir, fake := newProxiedSyncCity(t)
		out, err := runProxiedSync(t, binDir, cityPath, network)
		if err != nil {
			t.Fatalf("proxied sync failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "  hq: skipped (no local remote; set GC_DOLT_REMOTE_HQ to push to a non-local remote)") {
			t.Fatalf("output missing the policy skip:\n%s", out)
		}
		if got := fake.pushes(t, "hq"); len(got) != 0 {
			t.Fatalf("pushed to a network remote without an override: %q", got)
		}
	})

	t.Run("override selects the network remote", func(t *testing.T) {
		cityPath, binDir, fake := newProxiedSyncCity(t)
		out, err := runProxiedSync(t, binDir, cityPath, append(network, "GC_DOLT_REMOTE_HQ=origin"))
		if err != nil {
			t.Fatalf("proxied sync failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "  hq: pushed main -> origin:main (git+ssh://git@example.invalid/hq.git)") {
			t.Fatalf("output missing the push line:\n%s", out)
		}
		if got := fake.pushes(t, "hq"); len(got) != 1 {
			t.Fatalf("hq pushes = %q, want one", got)
		}
	})

	t.Run("no remote is skipped", func(t *testing.T) {
		cityPath, binDir, _ := newProxiedSyncCity(t)
		out, err := runProxiedSync(t, binDir, cityPath, []string{"FAKE_NO_REMOTE_DBS=hq"})
		if err != nil {
			t.Fatalf("proxied sync failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "  hq: skipped (no remote)") {
			t.Fatalf("output missing the no-remote skip:\n%s", out)
		}
	})
}

func TestSyncProxiedFailuresExitNonZeroAndNameTheScope(t *testing.T) {
	cityPath, binDir, fake := newProxiedSyncCity(t)
	rigs := rigListJSON(
		rigEntry("api", filepath.Join(cityPath, "api"), false, false),
		rigEntry("web", filepath.Join(cityPath, "web"), false, false),
		rigEntry("down", filepath.Join(cityPath, "down"), false, false),
		rigEntry("notes", filepath.Join(cityPath, "notes"), false, false),
	)
	out, err := runProxiedSync(t, binDir, cityPath, []string{
		"FAKE_RIG_LIST_JSON=" + rigs,
		"FAKE_SCOPE_DBS=city=hq rig:api=apidb rig:web=webdb",
		"FAKE_PUSH_FAIL_DBS=apidb",
		"FAKE_UNREACHABLE_SCOPES=rig:down",
		"FAKE_NOT_BD_SCOPES=rig:notes",
	})
	if err == nil {
		t.Fatalf("proxied sync succeeded despite failed scopes:\n%s", out)
	}
	for _, want := range []string{
		"  hq: pushed main -> origin:main",
		"  webdb: pushed main -> origin:main",
		"  apidb: ERROR: push failed (exit 1)",
		"  apidb: Error: query error: Error 1105 (HY000): permission denied for apidb",
		"  rig:down: ERROR: bead store unreachable",
		"  rig:notes: skipped (not a bd bead store)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	summary := lines[len(lines)-1]
	if !strings.HasPrefix(summary, "sync: 2/4 database(s) failed: ") ||
		!strings.Contains(summary, "apidb (push failed (exit 1))") ||
		!strings.Contains(summary, "rig:down (bead store unreachable)") {
		t.Fatalf("last line is not the failure summary: %q\n%s", summary, out)
	}
	if got := fake.pushes(t, "webdb"); len(got) != 1 {
		t.Fatalf("a failing scope stopped the healthy one: webdb pushes = %q", got)
	}
}

func TestSyncProxiedRigListFailureStillSyncsCityAndFails(t *testing.T) {
	cityPath, binDir, fake := newProxiedSyncCity(t)
	out, err := runProxiedSync(t, binDir, cityPath, []string{"FAKE_RIG_LIST_FAIL=1"})
	if err == nil {
		t.Fatalf("proxied sync hid a rig discovery failure:\n%s", out)
	}
	if !strings.Contains(out, "ERROR: gc rig list failed") {
		t.Fatalf("output does not name the rig discovery failure:\n%s", out)
	}
	if got := fake.pushes(t, "hq"); len(got) != 1 {
		t.Fatalf("city scope not synced when rig discovery failed: %q", got)
	}
}

func TestSyncProxiedHonorsDBFilterAndNoSyncMarker(t *testing.T) {
	cityPath, binDir, fake := newProxiedSyncCity(t)
	apiPath := filepath.Join(cityPath, "api")
	webPath := filepath.Join(cityPath, "web")
	if err := os.MkdirAll(filepath.Join(webPath, ".beads", "dolt", "webdb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webPath, ".beads", "dolt", "webdb", ".no-sync"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"FAKE_RIG_LIST_JSON=" + rigListJSON(
			rigEntry("api", apiPath, false, false),
			rigEntry("web", webPath, false, false),
			// A rig sharing the city's database is synced once.
			rigEntry("twin", filepath.Join(cityPath, "twin"), false, false),
		),
		"FAKE_SCOPE_DBS=city=hq rig:api=apidb rig:web=webdb rig:twin=hq",
	}

	out, err := runProxiedSync(t, binDir, cityPath, env)
	if err != nil {
		t.Fatalf("proxied sync failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "  webdb: skipped (.no-sync)") {
		t.Fatalf("output missing the .no-sync skip:\n%s", out)
	}
	if got := fake.pushes(t, "webdb"); len(got) != 0 {
		t.Fatalf("pushed a .no-sync database: %q", got)
	}
	if got := fake.pushes(t, "hq"); len(got) != 1 {
		t.Fatalf("shared database hq pushes = %q, want exactly one", got)
	}

	_, binDir2, fake2 := newProxiedSyncCity(t)
	out, err = runProxiedSync(t, binDir2, cityPath, env, "--db", "apidb")
	if err != nil {
		t.Fatalf("proxied sync --db failed: %v\n%s", err, out)
	}
	if got := fake2.pushes(t, "apidb"); len(got) != 1 {
		t.Fatalf("--db apidb pushes = %q, want one\n%s", got, out)
	}
	if got := fake2.pushes(t, "hq"); len(got) != 0 {
		t.Fatalf("--db apidb also pushed hq: %q", got)
	}
}

// --gc purges by walking the managed data directory and calling bd directly,
// which a proxied city does not support, so sync refuses it before touching
// any scope.
func TestSyncProxiedRefusesGC(t *testing.T) {
	cityPath, binDir, fake := newProxiedSyncCity(t)
	out, err := runProxiedSync(t, binDir, cityPath, nil, "--gc")
	if err == nil {
		t.Fatalf("proxied sync accepted --gc:\n%s", out)
	}
	if !strings.Contains(out, "--gc is not supported on bd-owned proxied scopes") {
		t.Fatalf("output does not explain the --gc refusal:\n%s", out)
	}
	if data, readErr := os.ReadFile(fake.logPath); readErr == nil && len(data) > 0 {
		t.Fatalf("sync reached a scope before refusing --gc:\n%s", data)
	}
}
