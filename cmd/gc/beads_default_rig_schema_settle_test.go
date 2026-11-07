package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// behindSchemaRig is a direct-server rig under a doltlite city whose bd store
// already exists: bd init refuses it as already initialized, and bd list fails
// with the #8106 storage_class error until `bd migrate schema` has run.
type behindSchemaRig struct {
	cityPath string
	rigPath  string
	logPath  string
	fakeBd   string
	gcBin    string
}

// schemaRigBdCall is one logged call of the fixture's fake bd.
type schemaRigBdCall struct {
	bin    string
	unlock string
	gcBin  string
	args   string
}

const behindSchemaListError = `Error 1105 (HY000): table "i" does not have column "storage_class"`

// newBehindSchemaRig builds the fixture around a bd that reports 1.3.1-rc.1,
// the release in #8106. migrateBody is the shell the fake bd
// runs for `bd migrate ...`; "$MARKER" names the file whose presence makes
// bd list readable.
func newBehindSchemaRig(t *testing.T, migrateBody string) behindSchemaRig {
	t.Helper()
	clearGCEnv(t)
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "tincan")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"tincan-city\"\n\n[beads]\nprovider = \"doltlite\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"tincan"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	fixtureDir := t.TempDir()
	binDir := filepath.Join(fixtureDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(fixtureDir, "bd-calls")
	marker := filepath.Join(fixtureDir, "schema-current")
	fakeBd := filepath.Join(binDir, "bd")
	script := fmt.Sprintf(`#!/bin/sh
MARKER=%q
printf '%%s|%%s|%%s|%%s\n' "$0" "${BD_ALLOW_REMOTE_MIGRATE-<unset>}" "${GC_BIN-}" "$*" >> %q
case "${1:-}" in
  init)
    echo "Found existing Dolt database 'tincan' for this workspace. This workspace is already initialized; just run bd commands normally. Aborting." >&2
    exit 1
    ;;
  version)
    echo "bd version 1.3.1-rc.1 (fixture)"
    ;;
  migrate)
    %s
    ;;
  list)
    if [ -f "$MARKER" ]; then
      echo '[]'
    else
      echo '%s' >&2
      exit 1
    fi
    ;;
esac
`, marker, logPath, migrateBody, behindSchemaListError)
	if err := os.WriteFile(fakeBd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gcBin := filepath.Join(fixtureDir, "gc")
	if err := os.WriteFile(gcBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldResolve := resolveInvokingExecutable
	resolveInvokingExecutable = func() (string, error) { return gcBin, nil }
	t.Cleanup(func() { resolveInvokingExecutable = oldResolve })
	t.Setenv("GC_BIN", "/tmp/stale-gc")
	t.Setenv("BD_BIN", fakeBd)
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	unsetEnvForTest(t, "BD_ALLOW_REMOTE_MIGRATE")
	return behindSchemaRig{cityPath: cityPath, rigPath: rigPath, logPath: logPath, fakeBd: fakeBd, gcBin: gcBin}
}

// unsetEnvForTest removes key for the rest of the test and restores it after.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func (r behindSchemaRig) invocations(t *testing.T) []schemaRigBdCall {
	t.Helper()
	data, err := os.ReadFile(r.logPath)
	if err != nil {
		t.Fatalf("read bd calls: %v", err)
	}
	var calls []schemaRigBdCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.SplitN(line, "|", 4)
		if len(fields) != 4 {
			t.Fatalf("malformed bd call log line %q", line)
		}
		calls = append(calls, schemaRigBdCall{bin: fields[0], unlock: fields[1], gcBin: fields[2], args: fields[3]})
	}
	return calls
}

// firstSchemaRigCall returns the index of the first invocation whose args start with
// prefix, or -1.
func firstSchemaRigCall(calls []schemaRigBdCall, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c.args, prefix) {
			return i
		}
	}
	return -1
}

func countSchemaRigCalls(calls []schemaRigBdCall, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c.args, prefix) {
			n++
		}
	}
	return n
}

func describeSchemaRigCalls(calls []schemaRigBdCall) string {
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "  %s\n", c.args)
	}
	return b.String()
}

const migrateMakesSchemaCurrent = `touch "$MARKER"; echo "Applied 2 schema migrations"`

func TestInitAndHookDirSettlesBehindSchemaOfAlreadyInitializedDefaultRigBdStore(t *testing.T) {
	rig := newBehindSchemaRig(t, migrateMakesSchemaCurrent)

	if err := initAndHookDir(rig.cityPath, rig.rigPath, "tc"); err != nil {
		t.Fatalf("initAndHookDir should settle the adopted store's schema and pass readiness: %v", err)
	}
	calls := rig.invocations(t)
	initAt, migrateAt, listAt := firstSchemaRigCall(calls, "init "), firstSchemaRigCall(calls, "migrate schema --quiet"), firstSchemaRigCall(calls, "list ")
	if initAt < 0 || migrateAt < 0 || listAt < 0 || initAt >= migrateAt || migrateAt >= listAt {
		t.Fatalf("want bd init, then bd migrate schema --quiet, then readiness bd list; bd calls:\n%s", describeSchemaRigCalls(calls))
	}
	if calls[migrateAt].args != "migrate schema --quiet" {
		t.Fatalf("migration argv = %q, want exactly migrate schema --quiet", calls[migrateAt].args)
	}

	// A second initialization (the next boot) adopts the now-current store
	// again; the migration is bd's no-op and readiness still passes.
	if err := initAndHookDir(rig.cityPath, rig.rigPath, "tc"); err != nil {
		t.Fatalf("re-initializing the settled rig: %v", err)
	}
	if got := countSchemaRigCalls(rig.invocations(t), "migrate schema --quiet"); got != 2 {
		t.Fatalf("migrate schema ran %d times across two initializations, want 2", got)
	}
}

// recordingBdRunner answers bd version with version (or versionErr) and bd
// migrate with migrateOut/migrateErr, recording every call.
type recordingBdRunner struct {
	version    string
	versionErr error
	migrateOut string
	migrateErr error
	calls      []string
}

func (r *recordingBdRunner) run(_, _ string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	switch {
	case len(args) > 0 && args[0] == "version":
		return []byte(r.version), r.versionErr
	case len(args) > 0 && args[0] == "migrate":
		return []byte(r.migrateOut), r.migrateErr
	}
	return nil, fmt.Errorf("unexpected bd call %q", strings.Join(args, " "))
}

func TestSettleAdoptedDefaultRigBdSchemaVersionGate(t *testing.T) {
	for _, tc := range []struct {
		version     string
		wantMigrate bool
	}{
		{"0.62.0", false},
		{"1.0.4", false},
		{"1.0.4-rc.1", false},
		{"1.0.5", true},
		{"1.3.1-rc.1", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			runner := &recordingBdRunner{version: "bd version " + tc.version + " (fixture)\n"}
			var stderr bytes.Buffer
			if err := settleAdoptedDefaultRigBdSchema(runner.run, "/city/rigs/api", &stderr); err != nil {
				t.Fatalf("settle: %v", err)
			}
			migrated := strings.Contains(strings.Join(runner.calls, "\n"), "migrate schema --quiet")
			if migrated != tc.wantMigrate {
				t.Fatalf("bd %s: migrated = %v, want %v; calls %q", tc.version, migrated, tc.wantMigrate, runner.calls)
			}
			if !tc.wantMigrate {
				if !strings.Contains(stderr.String(), tc.version) || !strings.Contains(stderr.String(), "/city/rigs/api") {
					t.Fatalf("skipping on bd %s must warn naming the version and scope; stderr %q", tc.version, stderr.String())
				}
			}
		})
	}
}

func TestSettleAdoptedDefaultRigBdSchemaUnreadableVersionStillMigrates(t *testing.T) {
	for name, runner := range map[string]*recordingBdRunner{
		"probe fails":       {versionErr: errors.New("exit status 2: unknown command")},
		"output unparsable": {version: "garbage\n"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if err := settleAdoptedDefaultRigBdSchema(runner.run, "/city/rigs/api", &stderr); err != nil {
				t.Fatalf("settle: %v", err)
			}
			if want := []string{"version", "migrate schema --quiet"}; strings.Join(runner.calls, ",") != strings.Join(want, ",") {
				t.Fatalf("calls = %q, want %q", runner.calls, want)
			}
			if !strings.Contains(stderr.String(), "bd version") {
				t.Fatalf("an unreadable bd version must be reported; stderr %q", stderr.String())
			}
		})
	}
}

func TestSettleAdoptedDefaultRigBdSchemaForwardsMigrationOutput(t *testing.T) {
	runner := &recordingBdRunner{version: "bd version 1.3.1\n", migrateOut: "Applied 2 schema migrations\n"}
	var stderr bytes.Buffer
	if err := settleAdoptedDefaultRigBdSchema(runner.run, "/city/rigs/api", &stderr); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !strings.Contains(stderr.String(), "Applied 2 schema migrations") {
		t.Fatalf("bd's migration output must stay visible; stderr %q", stderr.String())
	}

	quiet := &recordingBdRunner{version: "bd version 1.3.1\n"}
	stderr.Reset()
	if err := settleAdoptedDefaultRigBdSchema(quiet.run, "/city/rigs/api", &stderr); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("an empty successful migration must add nothing; stderr %q", stderr.String())
	}
}

func TestInitAndHookDirSettlesBehindSchemaOfAlreadyInitializedDefaultRigBdStoreUsesPinnedBdAndCanonicalGCBin(t *testing.T) {
	rig := newBehindSchemaRig(t, migrateMakesSchemaCurrent)
	// A different bd earlier on PATH than the pinned BD_BIN: settlement must
	// never reach it.
	decoyDir := filepath.Join(t.TempDir(), "decoy")
	if err := os.MkdirAll(decoyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(decoyDir, "bd")
	decoyScript := fmt.Sprintf("#!/bin/sh\nprintf '%%s|%%s|%%s|%%s\\n' \"$0\" \"${BD_ALLOW_REMOTE_MIGRATE-<unset>}\" \"${GC_BIN-}\" \"$*\" >> %q\nexit 1\n", rig.logPath)
	if err := os.WriteFile(decoy, []byte(decoyScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{decoyDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	if err := initAndHookDir(rig.cityPath, rig.rigPath, "tc"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}
	wantGCBin, err := filepath.EvalSymlinks(rig.gcBin)
	if err != nil {
		t.Fatal(err)
	}
	calls := rig.invocations(t)
	for _, prefix := range []string{"version", "migrate schema --quiet"} {
		at := firstSchemaRigCall(calls, prefix)
		if at < 0 {
			t.Fatalf("settlement never ran bd %s; bd calls:\n%s", prefix, describeSchemaRigCalls(calls))
		}
		c := calls[at]
		if c.bin != rig.fakeBd {
			t.Errorf("bd %s ran %q, want the pinned BD_BIN %q", prefix, c.bin, rig.fakeBd)
		}
		if c.gcBin != wantGCBin {
			t.Errorf("bd %s saw GC_BIN=%q, want canonical %q", prefix, c.gcBin, wantGCBin)
		}
		if c.unlock != "<unset>" {
			t.Errorf("bd %s saw BD_ALLOW_REMOTE_MIGRATE=%q; gc must add no migration consent", prefix, c.unlock)
		}
	}
	for _, c := range calls {
		if strings.Contains(c.args, "--force") {
			t.Errorf("gc added a force flag: bd %s", c.args)
		}
	}
}

func TestInitAndHookDirSettlesBehindSchemaOfAlreadyInitializedDefaultRigBdStorePreservesOperatorConsent(t *testing.T) {
	rig := newBehindSchemaRig(t, migrateMakesSchemaCurrent)
	t.Setenv("BD_ALLOW_REMOTE_MIGRATE", "1")

	if err := initAndHookDir(rig.cityPath, rig.rigPath, "tc"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}
	calls := rig.invocations(t)
	at := firstSchemaRigCall(calls, "migrate schema --quiet")
	if at < 0 {
		t.Fatalf("settlement never ran; bd calls:\n%s", describeSchemaRigCalls(calls))
	}
	if got := calls[at].unlock; got != "1" {
		t.Fatalf("bd migrate schema saw BD_ALLOW_REMOTE_MIGRATE=%q, want the operator's ambient 1 preserved", got)
	}
}

const remoteBackedRefusal = "Error: refusing to auto-apply 3 pending schema migrations to a remote-backed database"

func TestSettleAdoptedDefaultRigBdSchemaRemoteBackedRefusalWarnsAndContinues(t *testing.T) {
	runner := &recordingBdRunner{version: "bd version 1.3.1\n", migrateErr: errors.New("exit status 1: " + remoteBackedRefusal)}
	var stderr bytes.Buffer
	if err := settleAdoptedDefaultRigBdSchema(runner.run, "/city/rigs/api", &stderr); err != nil {
		t.Fatalf("a remote-backed refusal is bd's decision, not a failed initialization: %v", err)
	}
	if !strings.Contains(stderr.String(), "/city/rigs/api") || !strings.Contains(stderr.String(), remoteBackedRefusal) {
		t.Fatalf("the refusal must be reported with the scope and bd's message; stderr %q", stderr.String())
	}
}

func TestInitAndHookDirSettlesBehindSchemaOfAlreadyInitializedDefaultRigBdStoreRemoteBackedRefusal(t *testing.T) {
	refuse := `echo '` + remoteBackedRefusal + `' >&2; exit 1`
	t.Run("readiness passes", func(t *testing.T) {
		// The store is readable at its current schema; the refusal leaves it be.
		rig := newBehindSchemaRig(t, refuse)
		if err := os.WriteFile(filepath.Join(filepath.Dir(rig.logPath), "schema-current"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := initAndHookDir(rig.cityPath, rig.rigPath, "tc"); err != nil {
			t.Fatalf("initAndHookDir must continue to readiness after a remote-backed refusal: %v", err)
		}
		calls := rig.invocations(t)
		if migrateAt, listAt := firstSchemaRigCall(calls, "migrate schema --quiet"), firstSchemaRigCall(calls, "list "); migrateAt < 0 || listAt < migrateAt {
			t.Fatalf("want the refused migration, then readiness; bd calls:\n%s", describeSchemaRigCalls(calls))
		}
	})
	t.Run("readiness stays authoritative", func(t *testing.T) {
		rig := newBehindSchemaRig(t, refuse)
		err := initAndHookDir(rig.cityPath, rig.rigPath, "tc")
		if err == nil || !strings.Contains(err.Error(), "storage_class") {
			t.Fatalf("err = %v, want readiness's own failure after a remote-backed refusal", err)
		}
		if listAt := firstSchemaRigCall(rig.invocations(t), "list "); listAt < 0 {
			t.Fatal("readiness never ran after the remote-backed refusal")
		}
	})
}

func TestSettleAdoptedDefaultRigBdSchemaOtherFailureNamesTheScope(t *testing.T) {
	cause := errors.New("exit status 1: Error: cannot migrate: dirty working set in table issues")
	runner := &recordingBdRunner{version: "bd version 1.3.1\n", migrateErr: cause}
	err := settleAdoptedDefaultRigBdSchema(runner.run, "/city/rigs/api", io.Discard)
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want the migration failure", err)
	}
	if !strings.Contains(err.Error(), "/city/rigs/api") || !strings.Contains(err.Error(), "dirty working set in table issues") {
		t.Fatalf("err = %q, want it to name the scope and carry bd's diagnostic", err)
	}
}

func TestInitAndHookDirDefaultRigSchemaMigrationFailureStopsBeforeReadiness(t *testing.T) {
	for name, diagnostic := range map[string]string{
		"dirty table":   "Error: cannot migrate: dirty working set in table issues",
		"shared server": "Error: refusing to auto-apply 3 pending schema migrations to a database shared with other clients",
	} {
		t.Run(name, func(t *testing.T) {
			rig := newBehindSchemaRig(t, `echo '`+diagnostic+`' >&2; exit 1`)
			err := initAndHookDir(rig.cityPath, rig.rigPath, "tc")
			if err == nil {
				t.Fatal("initAndHookDir succeeded over a failed schema migration")
			}
			if !strings.Contains(err.Error(), rig.rigPath) || !strings.Contains(err.Error(), diagnostic) {
				t.Fatalf("err = %q, want it to name the scope %s and carry bd's diagnostic", err, rig.rigPath)
			}
			if calls := rig.invocations(t); firstSchemaRigCall(calls, "list ") >= 0 {
				t.Fatalf("readiness ran after a failed schema migration; bd calls:\n%s", describeSchemaRigCalls(calls))
			}
		})
	}
}
