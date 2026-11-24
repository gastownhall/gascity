//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The runtime-owner fence (SESSION-START-029) outside a Manager start: what
// the legacy reconciler records after a warm relaunch, and what a controller
// records at startup for the runtimes it adopted.

// replacedRootPID is above any pid_max: the root a relaunch replaced.
const replacedRootPID = 1 << 30

// recordedOwner decodes a row's runtime_owner record: the uid that recorded
// it and the pid and start time of each recorded root.
type recordedOwner struct {
	UID   int `json:"uid"`
	Roots []struct {
		PID   int    `json:"pid"`
		Start string `json:"start"`
	} `json:"roots"`
}

func readRecordedOwner(t *testing.T, store beads.Store, id string) (recordedOwner, bool) {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	raw := b.Metadata["runtime_owner"]
	if raw == "" {
		return recordedOwner{}, false
	}
	var owner recordedOwner
	if err := json.Unmarshal([]byte(raw), &owner); err != nil {
		t.Fatalf("runtime_owner %q: %v", raw, err)
	}
	return owner, true
}

// hasRoot reports whether owner records pid, at start when start is not "".
func (o recordedOwner) hasRoot(pid int, start string) bool {
	for _, r := range o.Roots {
		if r.PID == pid && (start == "" || r.Start == start) {
			return true
		}
	}
	return false
}

// A launch-only drift's warm relaunch replaces the runtime's root process,
// the one the record of its last start names, so the legacy reconciler
// records the runtime owner again once the relaunch succeeds: the record
// names the relaunched root under the controller's euid, and no longer the
// replaced one. Without it a start under another uid would read every
// recorded root gone and start a second runtime beside the live one.
func TestLaunchDriftRelaunchRecordsTheNewRuntimeOwner(t *testing.T) {
	selfStart, err := pidutil.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("start time of the test process is unreadable here: %v", err)
	}
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	agentCfg := sessionCoreConfigForHashInfo(env.desiredState["worker"], env.sessionInfo(session.ID))
	oldCfg := agentCfg
	oldCfg.Command = "stale-" + agentCfg.Command
	replaced, err := json.Marshal(map[string]any{"uid": os.Geteuid(), "boot": "", "roots": []map[string]any{{"pid": replacedRootPID, "start": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash":    runtime.CoreFingerprint(oldCfg),
		"started_provision_hash": runtime.ProvisionFingerprint(oldCfg),
		"started_launch_hash":    runtime.LaunchFingerprint(oldCfg),
		"runtime_owner":          string(replaced),
	})
	// The relaunched agent's root, which the scan reports for the session.
	env.sp.ExtraRuntimes = []runtime.LiveRuntime{{SessionID: session.ID, City: env.city, PID: os.Getpid(), IsTracked: true}}

	env.reconcile([]beads.Bead{session})

	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Fatalf("Relaunch calls = %d, want 1 (launch-only drift must relaunch); stderr=%s", got, env.stderr.String())
	}
	owner, ok := readRecordedOwner(t, env.store, session.ID)
	if !ok || owner.UID != os.Geteuid() || !owner.hasRoot(os.Getpid(), selfStart) || owner.hasRoot(replacedRootPID, "") {
		t.Fatalf("runtime_owner = %+v (present %v), want it rewritten under uid %d naming the relaunched root %d, not the replaced %d", owner, ok, os.Geteuid(), os.Getpid(), replacedRootPID)
	}
}

// adoptedRuntimeName is the runtime session seedAdoptedRuntime runs: the
// session name fieldRow gives its row.
const adoptedRuntimeName = "s-row"

// adoptedOwnerReport is the start of the line a startup prints when it
// recorded the one seeded runtime's owner.
const adoptedOwnerReport = "Recorded the runtime owner of 1 adopted session(s) in "

// seedAdoptedRuntime puts a session row in store whose runtime sp runs with no
// runtime_owner record, as every runtime is at the restart that deploys the
// fence, and whose process-table scan reports the test process as that
// runtime's root in city. It returns the row's id.
func seedAdoptedRuntime(t *testing.T, store beads.Store, sp *runtime.Fake, city string) string {
	t.Helper()
	id := fieldRow(t, store, "state", "active")
	if err := sp.Start(context.Background(), adoptedRuntimeName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	sp.ExtraRuntimes = []runtime.LiveRuntime{{SessionID: id, City: city, PID: os.Getpid(), IsTracked: true}}
	return id
}

// checkAdoptedOwnerRecorded fails t unless row id's runtime_owner record is
// present exactly when want is, and, when present, is this process's euid's,
// naming the seeded root.
func checkAdoptedOwnerRecorded(t *testing.T, store beads.Store, id string, want bool) {
	t.Helper()
	owner, ok := readRecordedOwner(t, store, id)
	if ok != want || (ok && (owner.UID != os.Geteuid() || !owner.hasRoot(os.Getpid(), ""))) {
		t.Fatalf("runtime_owner = %+v (present %v), want present %v under uid %d naming root %d", owner, ok, want, os.Geteuid(), os.Getpid())
	}
}

// A controller on the legacy reconciler records at startup the runtime owner
// of each running session whose row holds no record of its own, as every
// runtime a restart deploying the fence adopts is; a v2 controller, whose
// starts write no record, records none.
func TestCityRuntimeRecordsAdoptedRuntimeOwnersOnlyUnderTheLegacyReconciler(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   reconcilerMode
		record bool
	}{
		{"legacy", reconcilerLegacy, true},
		{"v2", reconcilerV2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			city := t.TempDir()
			id := seedAdoptedRuntime(t, store, sp, city)
			var stdout, stderr bytes.Buffer
			cr := &CityRuntime{reconcilerDrift: reconcilerModeDrift{running: tc.mode}, sp: sp, cityPath: city, stdout: &stdout, stderr: &stderr}

			cr.recordAdoptedRuntimeOwners(sessionFrontDoor(store))

			checkAdoptedOwnerRecorded(t, store, id, tc.record)
			if reported := strings.Contains(stdout.String(), adoptedOwnerReport); reported != tc.record {
				t.Fatalf("stdout %q, want the recording reported: %v", stdout.String(), tc.record)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr %q, want nothing", stderr.String())
			}
		})
	}
}

// CityRuntime.run records the adopted runtimes' owners in its startup, after
// the adoption barrier and before readiness, under the legacy reconciler and
// not under v2: the startup step, not only the helper, decides it.
func TestCityRuntimeRunRecordsAdoptedRuntimeOwnersAtStartup(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		cityPath := t.TempDir()
		tomlPath := filepath.Join(cityPath, "city.toml")
		writeCityRuntimeConfig(t, tomlPath, "fake")
		cfg, err := config.Load(osFS{}, tomlPath)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		store := beads.NewMemStore()
		sp := runtime.NewFake()
		id := seedAdoptedRuntime(t, store, sp, cityPath)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stdout := &lockedWriter{w: &bytes.Buffer{}}
		cr := newTestCityRuntime(t, CityRuntimeParams{
			CityPath: cityPath,
			CityName: "test-city",
			TomlPath: tomlPath,
			Cfg:      cfg,
			SP:       sp,
			BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
				return DesiredStateResult{State: map[string]TemplateParams{}}
			},
			Dops: newDrainOps(sp),
			Rec:  events.Discard,
			// Stop the runtime before run's shutdown would wait out its
			// graceful stop budget on it.
			OnStarted: func() {
				_ = sp.Stop(adoptedRuntimeName)
				cancel()
			},
			Stdout: stdout,
			Stderr: io.Discard,
		})
		cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
		cs.cityBeadStore = store
		cr.setControllerState(cs)

		cr.run(ctx)

		checkAdoptedOwnerRecorded(t, store, id, true)
		if out := stdout.w.(*bytes.Buffer).String(); !strings.Contains(out, adoptedOwnerReport) {
			t.Fatalf("stdout %q, want the recording reported", out)
		}
	})
	t.Run("v2", func(t *testing.T) {
		var id string
		f := newV2RunFixture(t, "patrol_interval = \"1h\"\n", func(f *v2RunFixture) {
			// Through the sessions store over the primed cache, which the
			// startup reads, as a session row is written.
			id = seedAdoptedRuntime(t, f.cr.sessionsBeadStore().Store, f.sp, f.cityPath)
		})
		t.Cleanup(func() { _ = f.sp.Stop(adoptedRuntimeName) })
		awaitClose(t, f.ready, "readiness")

		checkAdoptedOwnerRecorded(t, f.store, id, false)
	})
}

// The one-shot gc start, which reconciles with the legacy reconciler, records
// the adopted runtimes' owners after its adoption barrier.
func TestStartStandaloneRecordsAdoptedRuntimeOwners(t *testing.T) {
	cityPath := t.TempDir()
	clearInheritedBeadsEnv(t)
	t.Chdir(t.TempDir())
	cityTOML := "[workspace]\nname = \"runtime-owner-city\"\n\n[beads]\nprovider = \"file\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityPath); err != nil {
		t.Fatal(err)
	}
	store, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	id := seedAdoptedRuntime(t, store, sp, cityPath)
	oldBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return sp, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doStartStandalone exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}

	reopened, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	checkAdoptedOwnerRecorded(t, reopened, id, true)
	if !strings.Contains(stdout.String(), adoptedOwnerReport) {
		t.Fatalf("stdout %q, want the recording reported", stdout.String())
	}
}
