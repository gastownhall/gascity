package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The session lifecycle read budget: how many reads `gc session new`,
// `gc session suspend` and `gc session close` send their stores. Each is a
// one-shot command an operator or a script runs per lifecycle step, and on a
// bd-backed city each read is a bd subprocess.
//
// Every command runs here in process against a FILE-BACKED city, with
// observeOpenedStore wrapping each store it opens in a recorder, as in
// hook_budget_test.go: the file store only answers the operations so the
// command takes its real path. Reads counts the recorded Gets and Lists,
// SessionGets the Gets of the session the command targets, and Requests prices
// the recorded operations with beadstest.WireModel. The ceilings are ratchets:
// they are the counts measured when the command last shed a read, and come
// down as it sheds more.

// lifecycleBudgetTemplate is the agent template every scenario's session is
// created from; lifecycleBudgetAlias the alias the seeded session carries.
const (
	lifecycleBudgetTemplate = "worker"
	lifecycleBudgetAlias    = "probe"
)

type lifecycleBudgetFixture struct {
	cityDir   string
	sessionID string
}

// newLifecycleBudgetFixture builds a file-backed city whose one agent template
// lifecycleBudgetTemplate runs on the fake session provider, with no
// controller, so every command takes its direct path.
func newLifecycleBudgetFixture(t *testing.T) lifecycleBudgetFixture {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core")
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte(fmt.Sprintf("workspace_name = %q\n", namedSessionTestWorkspace)), 0o644); err != nil {
		t.Fatalf("WriteFile(.gc/site.toml): %v", err)
	}
	writeCatalogFile(t, cityDir, "agents/"+lifecycleBudgetTemplate+"/agent.toml", "provider = \"codex\"\nstart_command = \"echo\"\n")
	t.Setenv("GC_CITY", cityDir)
	return lifecycleBudgetFixture{cityDir: cityDir}
}

// seedSession creates the session the suspend and close scenarios target.
func (f *lifecycleBudgetFixture) seedSession(t *testing.T) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{lifecycleBudgetTemplate}, lifecycleBudgetAlias, "", "", true, false, 0, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew = %d; stderr=%s", code, stderr.String())
	}
	f.sessionID = onlySessionBead(t, f.cityDir).ID
	resetHookBudgetProcessState(t)
}

// recordLifecycleCommand runs fn with every store it opens wrapped in a
// recorder. Unlike recordHookCommand it opens no invocation store memo:
// run() opens one only for the hook verbs, not for these.
func recordLifecycleCommand(t *testing.T, fn func(stdout, stderr io.Writer) int) hookBudgetRun {
	t.Helper()
	resetHookBudgetProcessState(t)
	var (
		mu        sync.Mutex
		recorders []*beadstest.OpRecordingStore
	)
	prev := observeOpenedStore
	t.Cleanup(func() { observeOpenedStore = prev })
	observeOpenedStore = func(_ string, store beads.Store) beads.Store {
		rec := beadstest.NewOpRecordingStore(store)
		mu.Lock()
		recorders = append(recorders, rec)
		mu.Unlock()
		return hookBudgetRecordedStore{OpRecordingStore: rec, opened: store}
	}
	var stdout, stderr bytes.Buffer
	code := fn(&stdout, &stderr)
	observeOpenedStore = prev
	resetHookBudgetProcessState(t)

	mu.Lock()
	defer mu.Unlock()
	run := hookBudgetRun{opens: len(recorders), stdout: stdout.String(), stderr: stderr.String(), code: code}
	for _, rec := range recorders {
		run.ops = append(run.ops, rec.Ops()...)
	}
	return run
}

// lifecycleBudgetCeiling bounds one scenario: Reads the Gets and Lists,
// SessionGets the Gets of the targeted session, Requests the modeled wire
// total.
type lifecycleBudgetCeiling struct {
	Reads       int
	SessionGets int
	Requests    int
}

type lifecycleBudgetScenario struct {
	name string
	// seeded scenarios target a session created before the recording.
	seeded  bool
	run     func(f lifecycleBudgetFixture, stdout, stderr io.Writer) int
	ceiling lifecycleBudgetCeiling
	// wantStdout is a substring the command's output must carry, proving the
	// scenario took the path it names.
	wantStdout string
}

func lifecycleBudgetScenarios() []lifecycleBudgetScenario {
	return []lifecycleBudgetScenario{
		// new checks only the config reservations of the alias and the
		// session name; Manager.CreateSession reads their live holders under
		// the reservation lock. The concrete identity's holders are the
		// CLI's own read.
		{name: "new", wantStdout: "created from template", ceiling: lifecycleBudgetCeiling{Reads: 13, SessionGets: 0, Requests: 27}, run: func(_ lifecycleBudgetFixture, stdout, stderr io.Writer) int {
			return cmdSessionNew([]string{lifecycleBudgetTemplate}, "", "", "", true, false, 0, stdout, stderr)
		}},
		{name: "new --alias", wantStdout: "created from template", ceiling: lifecycleBudgetCeiling{Reads: 8, SessionGets: 0, Requests: 23}, run: func(_ lifecycleBudgetFixture, stdout, stderr io.Writer) int {
			return cmdSessionNew([]string{lifecycleBudgetTemplate}, lifecycleBudgetAlias, "", "", true, false, 0, stdout, stderr)
		}},
		// suspend and close read the session once before the write: their
		// resolution's record builds the worker handle. The other Gets of the
		// session are the worker's operation-event identity reads and the
		// manager's fresh reads under the mutation lock; close's work release
		// reads it once more before the close. An alias resolution misses its
		// bead-id lookup twice (#7829 drops the repeat).
		{name: "suspend by id", seeded: true, wantStdout: "suspended", ceiling: lifecycleBudgetCeiling{Reads: 8, SessionGets: 7, Requests: 24}, run: func(f lifecycleBudgetFixture, stdout, stderr io.Writer) int {
			return cmdSessionSuspend([]string{f.sessionID}, "", stdout, stderr, false)
		}},
		{name: "suspend by alias", seeded: true, wantStdout: "suspended", ceiling: lifecycleBudgetCeiling{Reads: 12, SessionGets: 7, Requests: 28}, run: func(_ lifecycleBudgetFixture, stdout, stderr io.Writer) int {
			return cmdSessionSuspend([]string{lifecycleBudgetAlias}, "", stdout, stderr, false)
		}},
		{name: "close by id", seeded: true, wantStdout: "closed", ceiling: lifecycleBudgetCeiling{Reads: 15, SessionGets: 9, Requests: 35}, run: func(f lifecycleBudgetFixture, stdout, stderr io.Writer) int {
			return cmdSessionClose([]string{f.sessionID}, "", stdout, stderr, false)
		}},
		{name: "close by alias", seeded: true, wantStdout: "closed", ceiling: lifecycleBudgetCeiling{Reads: 19, SessionGets: 9, Requests: 39}, run: func(_ lifecycleBudgetFixture, stdout, stderr io.Writer) int {
			return cmdSessionClose([]string{lifecycleBudgetAlias}, "", stdout, stderr, false)
		}},
	}
}

func TestSessionLifecycleReadBudget(t *testing.T) {
	model := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: hookBudgetLedgerRows}
	for _, sc := range lifecycleBudgetScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			f := newLifecycleBudgetFixture(t)
			if sc.seeded {
				f.seedSession(t)
			}
			run := recordLifecycleCommand(t, func(stdout, stderr io.Writer) int { return sc.run(f, stdout, stderr) })
			if run.code != 0 {
				t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
			}
			if !strings.Contains(run.stdout, sc.wantStdout) {
				t.Fatalf("stdout does not carry %q; the scenario did not take its path:\n%s", sc.wantStdout, run.stdout)
			}
			cost := model.Price(run.opens, run.ops)
			reads := cost.Gets + cost.Lists
			sessionGets := 0
			if f.sessionID != "" {
				sessionGets = countGetsOf(run.ops, f.sessionID)
			}
			t.Logf("reads=%d sessionGets=%d %s", reads, sessionGets, cost)
			if cost.Unkeyed != 0 {
				t.Errorf("%d listing requests walk the whole ledger, want none", cost.Unkeyed)
			}
			if reads > sc.ceiling.Reads {
				t.Errorf("sent %d reads (Gets and Lists), ceiling %d", reads, sc.ceiling.Reads)
			}
			if sessionGets > sc.ceiling.SessionGets {
				t.Errorf("read the session bead %s %d times, ceiling %d", f.sessionID, sessionGets, sc.ceiling.SessionGets)
			}
			if cost.Requests > sc.ceiling.Requests {
				t.Errorf("modeled %d requests at %d ledger rows, ceiling %d", cost.Requests, hookBudgetLedgerRows, sc.ceiling.Requests)
			}
			if t.Failed() || testing.Verbose() {
				t.Logf("recorded operations:\n%s", describeHookBudgetOps(run.ops))
			}
		})
	}
}

// TestSessionLifecycleActsOnTheSessionAsItIsAtTheWrite renames the targeted
// session after its resolution and hands its old runtime name to a live
// successor, as another process could while the command builds its runtime
// provider. The worker handle is built from the record resolution read, but
// the lifecycle write and close's work release read the session again: Stop
// reaches the current runtime name, never the successor's, and close releases
// the work held under that name while the successor keeps its own.
func TestSessionLifecycleActsOnTheSessionAsItIsAtTheWrite(t *testing.T) {
	for _, verb := range []string{"suspend", "close"} {
		for _, target := range []string{"id", "alias"} {
			t.Run(verb+" by "+target, func(t *testing.T) {
				cityDir := t.TempDir()
				writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"
[beads]
provider = "file"
[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
				t.Setenv("GC_CITY", cityDir)
				t.Setenv("GC_DIR", t.TempDir())
				t.Setenv("GC_BEADS", "file")
				t.Setenv("GC_SESSION", "lifecycle-rename")
				store, err := openCityStoreAt(cityDir)
				if err != nil {
					t.Fatal(err)
				}
				original, err := store.Create(beads.Bead{
					Type: session.BeadType, Labels: []string{session.LabelSession},
					Metadata: map[string]string{"session_name": "worker-before", "alias": "probe", "template": "worker", "state": "active"},
				})
				if err != nil {
					t.Fatal(err)
				}
				sp := runtime.NewFake()
				var currentWork, successorWork beads.Bead
				renamed := false
				prevRegistry := runtimeRegistry
				runtimeRegistry = runtimeRegistry.Clone()
				t.Cleanup(func() { runtimeRegistry = prevRegistry })
				if err := runtimeRegistry.Register("lifecycle-rename", func(_ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
					if renamed {
						return sp, nil
					}
					renamed = true
					if err := store.SetMetadata(original.ID, "session_name", "worker-after"); err != nil {
						return nil, err
					}
					if _, err := store.Create(beads.Bead{
						Type: session.BeadType, Labels: []string{session.LabelSession},
						Metadata: map[string]string{"session_name": "worker-before", "template": "worker", "state": "active"},
					}); err != nil {
						return nil, err
					}
					if currentWork, err = store.Create(beads.Bead{Title: "current work", Type: "task", Assignee: "worker-after"}); err != nil {
						return nil, err
					}
					if successorWork, err = store.Create(beads.Bead{Title: "successor work", Type: "task", Assignee: "worker-before"}); err != nil {
						return nil, err
					}
					inProgress := "in_progress"
					for _, id := range []string{currentWork.ID, successorWork.ID} {
						if err := store.Update(id, beads.UpdateOpts{Status: &inProgress}); err != nil {
							return nil, err
						}
					}
					if err := sp.Start(context.Background(), "worker-after", runtime.Config{}); err != nil {
						return nil, err
					}
					return sp, nil
				}); err != nil {
					t.Fatal(err)
				}

				arg := original.ID
				if target == "alias" {
					arg = "probe"
				}
				run := cmdSessionSuspend
				if verb == "close" {
					run = cmdSessionClose
				}
				var stdout, stderr bytes.Buffer
				if code := run([]string{arg}, "", &stdout, &stderr, false); code != 0 {
					t.Fatalf("%s exit=%d stdout=%s stderr=%s", verb, code, &stdout, &stderr)
				}
				if !renamed {
					t.Fatal("the command built no runtime provider, so nothing changed between resolution and the write")
				}
				if got := sp.CountCalls("Stop", "worker-after"); got == 0 {
					t.Errorf("Stop(worker-after) was not called: the write must act on the session's current runtime name")
				}
				if got := sp.CountCalls("Stop", "worker-before"); got != 0 {
					t.Errorf("Stop(worker-before) calls = %d, want 0: that runtime name now belongs to a live successor", got)
				}
				after, err := store.Get(original.ID)
				if err != nil {
					t.Fatal(err)
				}
				if verb == "suspend" {
					if got := after.Metadata["state"]; got != string(session.StateSuspended) {
						t.Errorf("state after suspend = %q, want %q", got, session.StateSuspended)
					}
					return
				}
				if after.Status != "closed" {
					t.Errorf("status after close = %q, want closed", after.Status)
				}
				current, err := store.Get(currentWork.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.Assignee != "" || current.Status != "open" {
					t.Errorf("closed session's work stranded: assignee=%q status=%q, want released (empty, open)", current.Assignee, current.Status)
				}
				successor, err := store.Get(successorWork.ID)
				if err != nil {
					t.Fatal(err)
				}
				if successor.Assignee != "worker-before" || successor.Status != "in_progress" {
					t.Errorf("live successor's work released: assignee=%q status=%q, want worker-before, in_progress", successor.Assignee, successor.Status)
				}
			})
		}
	}
}

// TestSessionNewRejectsAHeldIdentity creates a session, then runs `gc session
// new` again for its alias, or for its session name. The CLI checks only the
// config reservations of either: Manager.CreateSession reads their live
// holders under the city's identifier locks, so the second create reports the
// collision and no second session exists.
func TestSessionNewRejectsAHeldIdentity(t *testing.T) {
	for _, kind := range []string{"alias", "session_name"} {
		t.Run(kind, func(t *testing.T) {
			cityDir := t.TempDir()
			cityTOML := `[workspace]
name = "test-city"
[beads]
provider = "file"
[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`
			alias := "held-alias"
			if kind == "session_name" {
				alias = ""
				cityTOML += "tmux_alias = \"held-name\"\n"
			}
			writePhase0InterfaceCity(t, cityDir, cityTOML)
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_DIR", t.TempDir())
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", "fake")
			for i, wantCode := range []int{0, 1} {
				var stdout, stderr bytes.Buffer
				code := cmdSessionNew([]string{"worker"}, alias, "", "", true, false, time.Second, &stdout, &stderr)
				if code != wantCode {
					t.Fatalf("create %d exit = %d, want %d; stdout=%s stderr=%s", i+1, code, wantCode, &stdout, &stderr)
				}
				if wantCode != 0 {
					if !strings.Contains(stderr.String(), "already belongs to") {
						t.Fatalf("second create did not report the held %s: stderr=%s", kind, &stderr)
					}
					t.Logf("second create: %s", strings.TrimSpace(stderr.String()))
				}
			}
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := store.List(beads.ListQuery{Type: session.BeadType, TierMode: beads.TierBoth})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("%d session beads, want 1", len(rows))
			}
		})
	}
}
