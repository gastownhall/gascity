package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// newWorkerSiteCity returns a bootstrapped city directory (city.toml and
// .gc/) that a worker-site descriptor has not yet been written into.
func newWorkerSiteCity(t *testing.T) string {
	t.Helper()
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"box-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

func TestWorkerSiteResolveWorkerSpecReturnsTheGuard(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	t.Setenv("GC_BEADS", "file")
	spec := runtime.WorkerSpec{Runtime: "fake", Transport: config.SessionTransportTmux}

	local, err := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "box-city", cityDir)
	if err != nil {
		t.Fatalf("local resolveWorkerSpec: %v", err)
	}
	if _, isGuard := local.(*guard.Provider); isGuard {
		t.Fatal("a local city resolved the guard, want its configured runtime")
	}

	makeWorkerSite(t, cityDir)
	sp, err := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "box-city", cityDir)
	if err != nil {
		t.Fatalf("worker-site resolveWorkerSpec: %v", err)
	}
	if _, isGuard := sp.(*guard.Provider); !isGuard {
		t.Fatalf("a worker site resolved %T, want *guard.Provider", sp)
	}

	// Through the composed session provider too: nothing it returns can
	// start a runtime.
	composed, err := newSessionProviderForCity(&config.City{}, cityDir)
	if err != nil {
		t.Fatalf("newSessionProviderForCity: %v", err)
	}
	if err := composed.Start(context.Background(), "s-1", runtime.Config{}); !errors.Is(err, guard.ErrControllerOwnedRuntime) {
		t.Fatalf("composed provider Start = %v, want guard.ErrControllerOwnedRuntime", err)
	}
}

// TestWorkerSiteSendersDialNothing proves every sender this slice converts
// returns its typed result on a worker site without reaching a socket: the
// counting dialer stays at zero, and pokeController and
// reloadControllerConfig return the bare sentinel, which only their
// short-circuit returns (the controller sender would fail with its own error
// and the supervisor fallback wraps the sentinel).
func TestWorkerSiteSendersDialNothing(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	dials := countSocketDials(t)

	// The control: on a local city the poke reaches the supervisor fallback,
	// so the counter is wired to the dial the senders use.
	if err := pokeController(cityDir); err == nil || errors.Is(err, errWorkerSiteNoController) {
		t.Fatalf("local pokeController = %v, want a dial failure", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("local pokeController dials = %d, want 1 (the supervisor fallback)", got)
	}
	dials.Store(0)

	makeWorkerSite(t, cityDir)

	if !cityUsesManagedReconciler(cityDir) {
		t.Error("cityUsesManagedReconciler = false on a worker site, want true")
	}
	if err := pokeController(cityDir); err != errWorkerSiteNoController { //nolint:errorlint // identity proves the short-circuit
		t.Errorf("pokeController = %v, want the bare errWorkerSiteNoController", err)
	}
	if err := reloadControllerConfig(cityDir); err != errWorkerSiteNoController { //nolint:errorlint // identity proves the short-circuit
		t.Errorf("reloadControllerConfig = %v, want the bare errWorkerSiteNoController", err)
	}
	if err := pokeSupervisor(); !errors.Is(err, errWorkerSiteNoController) {
		t.Errorf("pokeSupervisor = %v, want errWorkerSiteNoController", err)
	}
	pingNudgeWakeSocket(cityDir)

	if got := dials.Load(); got != 0 {
		t.Errorf("worker-site senders dialed %d times, want 0", got)
	}
}

// TestWorkerSiteSessionNudgeToADormantTargetRecordsAWake is the A10
// regression: on a worker site the managed-reconciler probe answers true
// without probing, so a nudge to a dormant session records a wake request
// for the controller and never starts a runtime from the box.
func TestWorkerSiteSessionNudgeToADormantTargetRecordsAWake(t *testing.T) {
	resetControlPlaneForTest(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	makeWorkerSite(t, dir)
	dials := countSocketDials(t)
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sleepUnheld(t, store, info.ID)

	target := nudgeTarget{
		cityPath:    dir,
		cfg:         &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}},
		sessionID:   info.ID,
		sessionName: info.SessionName,
		identity:    "worker",
		agent:       config.Agent{Name: "worker", Provider: "claude"},
	}
	beforeCalls := len(fake.Calls)

	var stdout, stderr bytes.Buffer
	if code := deliverSessionNudgeWithWorker(target, store, fake, "check deploy status", nudgeDeliveryImmediate, false, &stdout, &stderr); code != 0 {
		t.Fatalf("deliverSessionNudgeWithWorker = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Queued nudge for "+info.ID) {
		t.Fatalf("stdout = %q, want the queued confirmation", stdout.String())
	}
	updated, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := updated.Metadata["wake_request"]; got != string(session.WakeCauseExplicit) {
		t.Fatalf("wake_request = %q, want %q", got, session.WakeCauseExplicit)
	}
	for _, call := range fake.Calls[beforeCalls:] {
		switch call.Method {
		case "Start", "Nudge", "NudgeNow":
			t.Fatalf("a worker-site nudge to a dormant session reached the runtime: %+v", call)
		}
	}
	// This counts only dials through the gc socket seam (the supervisor and
	// the wake ping). enqueueController's keyed send to the controller socket
	// still dials directly until CN-1b2 converts sendControllerCommandWithTimeouts.
	if got := dials.Load(); got != 0 {
		t.Fatalf("the nudge dialed %d times through the gc socket seam, want 0", got)
	}
}

func TestWorkerSiteEventsLocalFallbackIsRefused(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "events.jsonl"), []byte(`{"seq":1,"type":"session.woke","ts":"2026-10-16T00:00:00Z","actor":"gc"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := eventsAPIScope{cityName: "box-city", cityPath: cityDir, localOnly: true}

	// The control: a local city that is stopped reads its own log.
	var stdout, stderr bytes.Buffer
	if code := doEvents(scope, "", "", nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "session.woke") {
		t.Fatalf("local doEvents = %d, stdout=%q stderr=%q; want the local log", code, stdout.String(), stderr.String())
	}

	makeWorkerSite(t, cityDir)
	_, handled, err := readLocalCityEvents(scope, stoppedCityLocalFallbackError(scope), "", "", io.Discard)
	var refusal errWorkerSiteLocalState
	if !handled || !errors.As(err, &refusal) || refusal.what != "events" {
		t.Fatalf("readLocalCityEvents = (handled %v, %v), want errWorkerSiteLocalState{events}", handled, err)
	}
	if _, handled, err := readLocalCityHeadIndex(scope, stoppedCityLocalFallbackError(scope)); !handled || !errors.As(err, &refusal) {
		t.Fatalf("readLocalCityHeadIndex = (handled %v, %v), want errWorkerSiteLocalState{events}", handled, err)
	}
	for name, run := range map[string]func(io.Writer, io.Writer) int{
		"events": func(o, e io.Writer) int { return doEvents(scope, "", "", nil, o, e) },
		"seq":    func(o, e io.Writer) int { return doEventsSeq(scope, o, e) },
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(&stdout, &stderr); code == 0 {
			t.Errorf("gc events %s on a worker site = 0, want a refusal", name)
		}
		if strings.Contains(stdout.String(), "session.woke") {
			t.Errorf("gc events %s printed the box's local log: %s", name, stdout.String())
		}
		if !strings.Contains(stderr.String(), "worker site") {
			t.Errorf("gc events %s stderr = %q, want the worker-site refusal", name, stderr.String())
		}
	}
}

// TestWorkerSiteResolveClassStoreRefusesUnattachedBindings opens a real split
// city whose infrastructure classes sit on a sqlite-beads binding. On a worker
// site a class resolves only to a binding a descriptor ledger lists.
func TestWorkerSiteResolveClassStoreRefusesUnattachedBindings(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	resetControlPlaneForTest(t)
	routes := cliStorageRoutes(cityPath)
	bindingStore, relocated := routes.storeFor(coordclassFor(config.BeadClassSessions))
	if !relocated {
		t.Fatal("fixture: sessions is not relocated to the binding")
	}
	work := beads.NewMemStore()

	assertRefused := func(t *testing.T, class string) {
		t.Helper()
		store := resolveClassStore(routes, work, cfg, cityPath, class, nil)
		_, err := store.Get("any-1")
		var refusal errWorkerSiteLocalState
		if !errors.As(err, &refusal) || refusal.what != class+" store" {
			t.Fatalf("%s store Get = %v, want errWorkerSiteLocalState{%q}", class, err, class+" store")
		}
	}

	t.Run("a sqlite-beads binding no ledger lists", func(t *testing.T) {
		resetControlPlaneForTest(t)
		writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, nil))
		for _, class := range []string{config.BeadClassSessions, config.BeadClassGraph, config.BeadClassMessaging, config.BeadClassOrders, config.BeadClassNudges} {
			assertRefused(t, class)
		}
		if got := resolveClassStore(routes, work, cfg, cityPath, config.BeadClassWork, nil); got != beads.Store(work) {
			t.Fatalf("the work class resolved %T, want the work store the city ledger serves", got)
		}
	})

	t.Run("a binding left out of the bundle", func(t *testing.T) {
		resetControlPlaneForTest(t)
		writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, map[string][]string{"archive": {"orders", "nudges"}}))
		assertRefused(t, config.BeadClassSessions)
	})

	t.Run("a listed binding opens", func(t *testing.T) {
		resetControlPlaneForTest(t)
		writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, map[string][]string{
			"infra": {"graph", "sessions", "messaging", "orders", "nudges"},
		}))
		if got := resolveClassStore(routes, work, cfg, cityPath, config.BeadClassSessions, nil); got != bindingStore {
			t.Fatalf("an attached binding resolved %T, want the binding's store", got)
		}
	})

	t.Run("a local city is unchanged", func(t *testing.T) {
		resetControlPlaneForTest(t)
		if err := os.Remove(filepath.Join(cityPath, ".gc", "control-plane.toml")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		forgetControlPlane(cityPath)
		if got := resolveClassStore(routes, work, cfg, cityPath, config.BeadClassSessions, nil); got != bindingStore {
			t.Fatalf("a local split city resolved %T, want the binding's store", got)
		}
	})
}

func TestWorkerSiteStartRefusesController(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	makeWorkerSite(t, cityDir)
	for name, controllerMode := range map[string]bool{"supervisor": false, "foreground": true} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := doStartWithNameOverrideJSON([]string{cityDir}, controllerMode, &stdout, &stderr, "", false); code == 0 {
				t.Fatalf("gc start on a worker site = 0; stdout=%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
				t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
			}
		})
	}
}

func TestWorkerSiteSupervisorRunRefusesController(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	makeWorkerSite(t, cityDir)
	t.Setenv("GC_CITY", cityDir)
	ran := false
	prev := runSupervisorFunc
	runSupervisorFunc = func(io.Writer, io.Writer) int { ran = true; return 0 }
	t.Cleanup(func() { runSupervisorFunc = prev })

	var stdout, stderr bytes.Buffer
	if code := doSupervisorRun(&stdout, &stderr); code == 0 {
		t.Fatal("gc supervisor run on a worker site = 0")
	}
	if ran {
		t.Fatal("gc supervisor run started the supervisor loop on a worker site")
	}
	if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
		t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
	}

	// From a fresh process whose flag is not set yet: the ambient city
	// resolves the plane.
	clearControlPlaneState()
	stderr.Reset()
	if code := doSupervisorRun(&stdout, &stderr); code == 0 || ran {
		t.Fatalf("gc supervisor run from the box's city = %d (ran %v), want a refusal", code, ran)
	}
}

func TestWorkerSiteSupervisorRunOnALocalCityRuns(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	t.Setenv("GC_CITY", cityDir)
	ran := false
	prev := runSupervisorFunc
	runSupervisorFunc = func(io.Writer, io.Writer) int { ran = true; return 0 }
	t.Cleanup(func() { runSupervisorFunc = prev })
	if code := doSupervisorRun(io.Discard, io.Discard); code != 0 || !ran {
		t.Fatalf("gc supervisor run on a local city = %d (ran %v), want the loop", code, ran)
	}
}

// TestWorkerSiteSessionWaitSleepSucceedsAfterItsHold is the durable-write
// caller: the hold is written, the poke has no controller to reach, and the
// command reports success with a notice.
func TestWorkerSiteSessionWaitSleepSucceedsAfterItsHold(t *testing.T) {
	const sessionID, depID = "gc-1", "gc-dep"
	now := time.Date(2026, time.October, 16, 9, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: sessionID, Title: "worker session", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, CreatedAt: now, UpdatedAt: now, Revision: 1, Metadata: map[string]string{"session_name": "worker", "continuation_epoch": "1"}},
		{ID: depID, Title: "blocking work", Type: "task", Status: "open", CreatedAt: now, UpdatedAt: now, Revision: 1},
	}, nil)
	deps := func(poke func() error) sessionWaitDeps {
		return sessionWaitDeps{
			sessions:       sessionFrontDoor(store),
			dependencies:   waitDependencyReaderFunc(store.Get),
			now:            func() time.Time { return now },
			pokeController: poke,
		}
	}

	var stdout, stderr bytes.Buffer
	code := doSessionWait(sessionID, []string{depID}, false, "until the work lands", true, &stdout, &stderr, deps(func() error {
		return fmt.Errorf("connecting to supervisor: %w", errWorkerSiteNoController)
	}))
	if code != 0 {
		t.Fatalf("doSessionWait --sleep = %d on a worker site, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "draining to sleep") {
		t.Fatalf("stdout = %q, want the sleep confirmation", stdout.String())
	}
	if lines := strings.Split(strings.TrimSpace(stderr.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "worker site") {
		t.Fatalf("stderr = %q, want one worker-site notice line", stderr.String())
	}
	if got := mustGetBead(t, store, sessionID).Metadata["wait_hold"]; got == "" {
		t.Fatal("the wait hold was not written before the poke")
	}

	// Any other poke failure stays fatal.
	stdout.Reset()
	stderr.Reset()
	if code := doSessionWait(sessionID, []string{depID}, false, "again", true, &stdout, &stderr, deps(func() error { return errors.New("controller refused") })); code == 0 {
		t.Fatal("doSessionWait --sleep = 0 on an ordinary poke failure, want 1")
	}
}

func TestWorkerSiteConvoyPokeSucceedsWithANotice(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	makeWorkerSite(t, cityDir)
	dials := countSocketDials(t)
	t.Setenv("GC_CITY", cityDir)

	var stderr bytes.Buffer
	cmd := newConvoyPokeCmd(io.Discard, &stderr)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("gc convoy poke = %v on a worker site, want success; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "worker site") {
		t.Fatalf("stderr = %q, want the worker-site notice", stderr.String())
	}
	// pokeControlDispatch short-circuits before its control-dispatcher send, so
	// neither that direct controller dial nor the seam's dials happen; this
	// counter sees only the seam's.
	if got := dials.Load(); got != 0 {
		t.Fatalf("gc convoy poke dialed %d times through the gc socket seam, want 0", got)
	}
}

// TestWorkerSiteDrainCheckAnswersFromTheRow reads drain state through the
// guard: its meta refusal is "meta not applicable", so the row alone answers.
func TestWorkerSiteDrainCheckAnswersFromTheRow(t *testing.T) {
	dops := newDrainOps(guard.New())
	for _, tc := range []struct {
		row  bool
		want int
	}{{row: true, want: 0}, {row: false, want: 1}} {
		var stderr bytes.Buffer
		if code := doRuntimeDrainCheck(dops, drainCheckRow(tc.row, nil), "worker", "worker", false, io.Discard, &stderr); code != tc.want {
			t.Errorf("row draining=%v: code = %d, want %d; stderr=%s", tc.row, code, tc.want, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("row draining=%v: stderr = %q, want empty", tc.row, stderr.String())
		}
	}
	// A row that cannot be read is still unknown: the guard answers nothing.
	if code := doRuntimeDrainCheck(dops, drainCheckRow(false, errors.New("ledger unreachable")), "worker", "worker", false, io.Discard, io.Discard); code != drainCheckUnknownExit {
		t.Errorf("unreadable row: code = %d, want %d", code, drainCheckUnknownExit)
	}
}

// writeMalformedDescriptor makes cityDir hold a descriptor that does not
// decode, as a newer gc's descriptor would look to this one, without marking
// any invocation.
func writeMalformedDescriptor(t *testing.T, cityDir string) {
	t.Helper()
	writeControlPlaneBytes(t, cityDir, []byte("schema = ["))
}

// TestWorkerSiteMalformedDescriptorFailsClosedAtTheSeams: presence makes a
// worker site, so a descriptor that does not decode leaves every seam unable
// to act or dial, whether or not the command went through the gate.
func TestWorkerSiteMalformedDescriptorFailsClosedAtTheSeams(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	t.Setenv("GC_BEADS", "file")
	writeMalformedDescriptor(t, cityDir)
	dials := countSocketDials(t)
	spec := runtime.WorkerSpec{Runtime: "fake", Transport: config.SessionTransportTmux}

	sp, err := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "box-city", cityDir)
	if _, isGuard := sp.(*guard.Provider); err != nil || !isGuard {
		t.Fatalf("resolveWorkerSpec = (%T, %v), want the guard", sp, err)
	}
	if !cityUsesManagedReconciler(cityDir) {
		t.Error("cityUsesManagedReconciler = false, want true")
	}
	for name, poke := range map[string]func(string) error{
		"pokeController":         pokeController,
		"reloadControllerConfig": reloadControllerConfig,
		"pokeControlDispatch":    pokeControlDispatch,
	} {
		if err := poke(cityDir); err != errWorkerSiteNoController { //nolint:errorlint // identity proves the short-circuit
			t.Errorf("%s = %v, want the bare errWorkerSiteNoController", name, err)
		}
	}
	pingNudgeWakeSocket(cityDir)
	var descErr *errControlPlaneDescriptor
	if err := refuseControllerOnWorkerSite(cityDir); !errors.As(err, &descErr) {
		t.Errorf("refuseControllerOnWorkerSite = %v, want the descriptor error", err)
	}
	if err := workerSiteClassRefusal(cityDir, config.BeadClassSessions, config.StorageWorkBinding); !errors.As(err, &descErr) {
		t.Errorf("workerSiteClassRefusal = %v, want the descriptor error", err)
	}
	if got := dials.Load(); got != 0 {
		t.Errorf("seams dialed %d times on a malformed descriptor, want 0", got)
	}

	// An exempt command resolves the city, and its invocation is marked: the
	// city-less senders and a provider built with no city path refuse too.
	prev := controlPlaneGateExempt
	t.Cleanup(func() { controlPlaneGateExempt = prev })
	controlPlaneGateExempt = true
	t.Setenv("GC_CITY", cityDir)
	if _, err := resolveContext(); err != nil {
		t.Fatalf("exempt resolveContext = %v, want the city", err)
	}
	if _, err := dialGCSocket(supervisorSocketPath(), time.Millisecond); !errors.Is(err, errWorkerSiteNoController) {
		t.Errorf("dialGCSocket after an exempt resolution = %v, want errWorkerSiteNoController", err)
	}
	if sp, _ := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "", ""); !isGuardProvider(sp) {
		t.Errorf("resolveWorkerSpec with no city path = %T, want the guard", sp)
	}
	if got := dials.Load(); got != 0 {
		t.Errorf("dialed %d times after the exempt resolution, want 0", got)
	}
}

func isGuardProvider(sp runtime.Provider) bool {
	_, ok := sp.(*guard.Provider)
	return ok
}

// TestWorkerSiteMalformedDescriptorThroughRun runs the verbs that find their
// city from the session environment, not the context resolver: each names
// the descriptor and acts on nothing.
func TestWorkerSiteMalformedDescriptorThroughRun(t *testing.T) {
	for _, tc := range []struct {
		args []string
		// hook verbs exit 0 by design: a hook that fails breaks the agent.
		wantExit0 bool
		wantCode  int // when non-zero, the exact exit code
	}{
		{args: []string{"handoff", "--auto", "context cycle"}},
		{args: []string{"runtime", "request-restart"}},
		// Unknown (3), not the Local answer "not draining" (1).
		{args: []string{"runtime", "drain-check"}, wantCode: drainCheckUnknownExit},
		{args: []string{"nudge", "drain", "--inject"}, wantExit0: true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			resetControlPlaneForTest(t)
			f := newHookBudgetFixture(t)
			countBeads := func() int {
				t.Helper()
				all, err := openNudgeBeadStore(f.cityDir).List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
				if err != nil {
					t.Fatalf("counting beads: %v", err)
				}
				return len(all)
			}
			before := countBeads()
			writeMalformedDescriptor(t, f.cityDir)
			dials := countSocketDials(t)

			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			if tc.wantExit0 != (code == 0) || (tc.wantCode != 0 && code != tc.wantCode) {
				t.Fatalf("exit = %d, want exit-0 %v (code %d); stderr=%s", code, tc.wantExit0, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), "control-plane descriptor") {
				t.Fatalf("stderr does not name the descriptor:\n%s", stderr.String())
			}
			if got := dials.Load(); got != 0 {
				t.Fatalf("dialed %d gc sockets through dialGCSocket, want 0", got)
			}
			// Count with the descriptor gone, as a Local reader would.
			if err := os.Remove(filepath.Join(f.cityDir, ".gc", "control-plane.toml")); err != nil {
				t.Fatal(err)
			}
			forgetControlPlane(f.cityDir)
			resetHookBudgetProcessState(t)
			if after := countBeads(); after != before {
				t.Fatalf("the command wrote %d beads on a malformed descriptor, want none", after-before)
			}
			if stdout.Len() != 0 && !tc.wantExit0 {
				t.Fatalf("stdout = %q, want nothing", stdout.String())
			}
		})
	}
}

// TestWorkerSiteIsDecidedPerCity is the multi-city host: the supervisor looks
// up a worker-site city and then a Local one in the same process. The Local
// city keeps its own provider, its dials and its controller.
func TestWorkerSiteIsDecidedPerCity(t *testing.T) {
	resetControlPlaneForTest(t)
	boxCity := newWorkerSiteCity(t)
	localCity := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	writeControlPlaneFixture(t, boxCity, testDescriptor(boxCity, nil))
	spec := runtime.WorkerSpec{Runtime: "fake", Transport: config.SessionTransportTmux}

	// The host's lookups for the box city.
	if sp, _ := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "box", boxCity); !isGuardProvider(sp) {
		t.Fatalf("the box city resolved %T, want the guard", sp)
	}
	if err := refuseControllerOnWorkerSite(boxCity); !errors.Is(err, errWorkerSiteRefusesController) {
		t.Fatalf("refuseControllerOnWorkerSite(box) = %v, want the refusal", err)
	}
	if workerSiteProcess.Load() {
		t.Fatal("a host's lookup of one city marked the whole process a worker site")
	}

	dials := countSocketDials(t)
	if sp, err := resolveWorkerSpec(nil, spec, config.SessionConfig{}, "local", localCity); err != nil || isGuardProvider(sp) {
		t.Fatalf("the Local city resolved (%T, %v), want its configured runtime", sp, err)
	}
	if err := refuseControllerOnWorkerSite(localCity); err != nil {
		t.Fatalf("refuseControllerOnWorkerSite(local) = %v, want nil", err)
	}
	if err := pokeController(localCity); err == nil || errors.Is(err, errWorkerSiteNoController) {
		t.Fatalf("pokeController(local) = %v, want a dial failure", err)
	}
	pingNudgeWakeSocket(localCity)
	if got := dials.Load(); got != 2 {
		t.Fatalf("the Local city dialed %d times, want 2 (the supervisor fallback and the wake ping)", got)
	}
}

// TestWorkerSitePingNudgeWakeSocketDialsOnlyLocally is the wake ping's own
// control: one counted dial on the Local plane, none on a worker site whose
// plane only the ping's city path says.
func TestWorkerSitePingNudgeWakeSocketDialsOnlyLocally(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	dials := countSocketDials(t)
	pingNudgeWakeSocket(cityDir)
	if got := dials.Load(); got != 1 {
		t.Fatalf("Local wake ping dials = %d, want 1", got)
	}
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	pingNudgeWakeSocket(cityDir)
	if got := dials.Load(); got != 1 {
		t.Fatalf("worker-site wake ping reached the dialer: dials = %d, want 1", got)
	}
}

// TestWorkerSiteControllerStopDialsNothing: gc stop's controller client dials
// through the city seam. A file stands at the socket path so the client gets
// past its stat to the dial.
func TestWorkerSiteControllerStopDialsNothing(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	sock := controllerSocketPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dials := countSocketDials(t)
	if res := sendControllerStop(cityDir, false); res.outcome == controllerStopAcknowledged {
		t.Fatal("the counting dialer acknowledged a stop")
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("Local controller stop dials = %d, want 1", got)
	}
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	res := sendControllerStop(cityDir, false)
	if res.outcome != controllerStopDefinitePreEntryUnavailable {
		t.Fatalf("worker-site stop outcome = %v, want definite pre-entry unavailable", res.outcome)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("worker-site controller stop reached the dialer: dials = %d, want 1", got)
	}
}

// TestWorkerSiteStorageFunnelRefusesUnattachedBindings drives the one-shot
// storage funnel itself on a split city whose infrastructure classes sit on a
// sqlite-beads binding the descriptor does not attach: every door (the class
// store, the by-id resolver, the graph binding) gets a standing refusal, work
// ids still resolve to work, and the binding is never opened.
func TestWorkerSiteStorageFunnelRefusesUnattachedBindings(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	resetControlPlaneForTest(t)
	stderr := captureCLIStorageStderr(t)

	// Seed a graph bead into the binding while the city is still Local.
	graphStore, relocated := cliStorageRoutes(cityPath).storeFor(coordclassFor(config.BeadClassGraph))
	if !relocated {
		t.Fatal("fixture: graph is not relocated")
	}
	graphBead, err := graphStore.Create(beads.Bead{Title: "lives only in the binding", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the binding: %v", err)
	}
	if !bdIDIsClassReserved(graphBead.ID) {
		t.Fatalf("fixture: %q is not in a reserved class namespace", graphBead.ID)
	}
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, nil))
	work := beads.NewMemStore()
	workBead, err := work.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	assertWorkerSiteRefusal := func(t *testing.T, what string, err error) {
		t.Helper()
		var refusal errWorkerSiteLocalState
		if !errors.As(err, &refusal) {
			t.Fatalf("%s = %v, want errWorkerSiteLocalState", what, err)
		}
	}

	routes := cliStorageRoutes(cityPath)
	store, relocated := routes.storeFor(coordclassFor(config.BeadClassSessions))
	if !relocated {
		t.Fatal("a worker site's unattached sessions class resolved to the work store")
	}
	_, err = store.Get("any-1")
	assertWorkerSiteRefusal(t, "the funnel's sessions store Get", err)
	if !storeref.IsStandingRefusal(err) {
		t.Fatalf("the funnel's refusal %v is not a standing refusal", err)
	}
	if !strings.Contains(stderr.String(), "worker site") {
		t.Fatalf("funnel stderr = %q, want the worker-site refusal once", stderr.String())
	}

	_, err = classRoutedStoreForID(cityPath, graphBead.ID, work)
	assertWorkerSiteRefusal(t, "classRoutedStoreForID(graph id)", err)
	if got, err := classRoutedStoreForID(cityPath, workBead.ID, work); err != nil || got != beads.Store(work) {
		t.Fatalf("classRoutedStoreForID(work id) = (%T, %v), want the work store", got, err)
	}
	graph := cityGraphClassBinding(cityPath)
	if graph == nil {
		t.Fatal("cityGraphClassBinding = nil, want the refused store, not the work store")
	}
	_, err = graph.Get(graphBead.ID)
	assertWorkerSiteRefusal(t, "the graph binding's Get", err)
	_, err = resolveClassStore(routes, work, cfg, cityPath, config.BeadClassGraph, nil).Get(graphBead.ID)
	assertWorkerSiteRefusal(t, "resolveClassStore(graph).Get", err)
	if got := resolveClassStore(routes, work, cfg, cityPath, config.BeadClassWork, nil); got != beads.Store(work) {
		t.Fatalf("the work class resolved %T, want the work store", got)
	}
}

// TestWorkerSiteStorageFunnelCreatesNoBindingRoot is the box as the export
// leaves it: a sqlite-beads binding configured, nothing on disk. The Local
// control shows the funnel would create the binding root; on a worker site it
// creates nothing.
func TestWorkerSiteStorageFunnelCreatesNoBindingRoot(t *testing.T) {
	for _, workerSite := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "worker site"}[workerSite], func(t *testing.T) {
			resetControlPlaneForTest(t)
			bindingRoot := filepath.Join(t.TempDir(), "store")
			cityPath := oneShotCLICity(t, bindingRoot)
			stubInfraControllerPing(t, 0)
			captureCLIStorageStderr(t)
			if workerSite {
				writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, nil))
			}
			_ = cliStorageRoutes(cityPath)
			_, err := os.Stat(bindingRoot)
			if created := err == nil; created == workerSite {
				t.Fatalf("binding root created = %v on worker site %v", created, workerSite)
			}
		})
	}
}

// TestWorkerSiteClassStoreRefusesABoxStorageThatDisagrees: the descriptor
// puts sessions on the infra ledger but the box's [storage] leaves it on
// work. The work binding is attached, but not for sessions, so the class
// refuses instead of opening the wrong ledger.
func TestWorkerSiteClassStoreRefusesABoxStorageThatDisagrees(t *testing.T) {
	resetControlPlaneForTest(t)
	cityPath := oneShotCLICity(t, "")
	captureCLIStorageStderr(t)
	writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, map[string][]string{"infra": {"sessions"}}))
	work := beads.NewMemStore()
	routes := cliStorageRoutes(cityPath)
	_, err := resolveClassStore(routes, work, nil, cityPath, config.BeadClassSessions, nil).Get("any-1")
	var refusal errWorkerSiteLocalState
	if !errors.As(err, &refusal) || refusal.what != "sessions store" {
		t.Fatalf("sessions store Get = %v, want errWorkerSiteLocalState{sessions store}", err)
	}
	if got := resolveClassStore(routes, work, nil, cityPath, config.BeadClassWork, nil); got != beads.Store(work) {
		t.Fatalf("the work class resolved %T, want the work store", got)
	}
}

func TestWorkerSiteRegisterRefuses(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	t.Setenv("GC_HOME", t.TempDir())
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	reached := false
	prev := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(string, string, io.Writer, io.Writer) (bool, int) {
		reached = true
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = prev })

	var stdout, stderr bytes.Buffer
	if code := registerCityWithSupervisorNamed(cityDir, "", &stdout, &stderr, "gc register", false); code == 0 || reached {
		t.Fatalf("register on a worker site = %d (reached the registry write %v), want a refusal", code, reached)
	}
	if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
		t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
	}
}

// TestWorkerSiteSupervisorStartAndInstallRefuse: HOME is moved, so if the
// worker-site refusal were missing the home-override guard would stop the
// command before it forks or installs anything.
func TestWorkerSiteSupervisorStartAndInstallRefuse(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("the home-override backstop only applies on linux/darwin")
	}
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for name, cmd := range map[string]func(io.Writer, io.Writer) int{
		"start":   doSupervisorStart,
		"install": doSupervisorInstall,
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := cmd(&stdout, &stderr); code == 0 {
				t.Fatalf("gc supervisor %s on a worker site = 0", name)
			}
			if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
				t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
			}
		})
	}
}

// TestWorkerSiteSupervisorRefusesToStartAWorkerSiteCity is the supervisor's
// per-city start: a registered city holding a descriptor is refused before
// any of its init runs, whatever city the supervisor was started in. The
// process cached the city as Local before the descriptor was written, so the
// start attempt must read the file as it is now.
func TestWorkerSiteSupervisorRefusesToStartAWorkerSiteCity(t *testing.T) {
	resetControlPlaneForTest(t)
	t.Setenv("GC_HOME", t.TempDir())
	cityDir := newWorkerSiteCity(t)
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	// Cache the city as Local with the descriptor moved aside, then put it
	// back without telling the cache.
	descriptorPath := filepath.Join(cityDir, citylayout.ControlPlaneDescriptorPath)
	if err := os.Rename(descriptorPath, descriptorPath+".aside"); err != nil {
		t.Fatal(err)
	}
	if plane, err := controlPlaneFor(cityDir); err != nil || plane.Worker() {
		t.Fatalf("controlPlaneFor without the descriptor = (%+v, %v), want Local", plane, err)
	}
	if err := os.Rename(descriptorPath+".aside", descriptorPath); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityDir, "box-city"); err != nil {
		t.Fatal(err)
	}
	cr := newCityRegistry()
	var stdout, stderr bytes.Buffer
	startOneCity(supervisor.CityEntry{Path: cityDir, Name: "box-city"}, reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
		t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
	}
	if cr.Has(cityDir) {
		t.Fatal("the supervisor published a managed city for a worker site")
	}
	if workerSiteProcess.Load() {
		t.Fatal("refusing one managed city marked the supervisor process a worker site")
	}
}

func TestWorkerSiteRestartRefuses(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	var stdout, stderr bytes.Buffer
	if code := cmdRestartJSON([]string{cityDir}, &stdout, &stderr, false); code == 0 {
		t.Fatalf("gc restart on a worker site = 0; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), errWorkerSiteRefusesController.Error()) {
		t.Fatalf("stderr = %q, want the worker-site refusal", stderr.String())
	}
}

// TestRequireBootstrappedCityMalformedDescriptorHasOneRemedy: the city is
// there, so the gc init hint would contradict the descriptor's own remedy.
func TestRequireBootstrappedCityMalformedDescriptorHasOneRemedy(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	writeMalformedDescriptor(t, cityDir)
	_, err := requireBootstrappedCity(cityDir)
	var descErr *errControlPlaneDescriptor
	if !errors.As(err, &descErr) {
		t.Fatalf("requireBootstrappedCity = %v, want the descriptor error", err)
	}
	if strings.Contains(err.Error(), "gc init") {
		t.Fatalf("error = %v, want no gc init hint", err)
	}
}

// TestWorkerSiteV2DrainAckSucceedsOnItsRowAck: under v2 the row is the ack,
// so the guard's refusal to mirror it into a runtime is a warning after the
// row commit, not a failure. Under legacy there is no row ack and the guard's
// refusal stays loud (D-DRAINACK).
func TestWorkerSiteV2DrainAckSucceedsOnItsRowAck(t *testing.T) {
	for _, tc := range []struct {
		reconciler string
		wantCode   int
	}{{reconciler: "v2", wantCode: 0}, {reconciler: "", wantCode: 1}} {
		t.Run("reconciler="+tc.reconciler, func(t *testing.T) {
			resetControlPlaneForTest(t)
			cityPath, store, id := drainAckCity(t, tc.reconciler)
			writeControlPlaneFixture(t, cityPath, testDescriptor(cityPath, nil))
			prevPoke := drainAckPokeController
			drainAckPokeController = func(string, reconcilekey.Key) error { return errWorkerSiteNoController }
			t.Cleanup(func() { drainAckPokeController = prevPoke })

			target := sessionRuntimeTarget{cityPath: cityPath, display: "worker", sessionName: "worker"}
			var stdout, stderr bytes.Buffer
			code := runtimeDrainAck(newDrainOps(guard.New()), target, false, "tok-a", false, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("drain-ack = %d, want %d; stderr=%s", code, tc.wantCode, stderr.String())
			}
			if tc.wantCode != 0 {
				return
			}
			if ack := rowAck(t, store, id); ack == "" {
				t.Fatal("the row ack was not written")
			}
			if !strings.Contains(stderr.String(), "row ack is in force") {
				t.Fatalf("stderr = %q, want the not-applicable warning", stderr.String())
			}
		})
	}
}

// TestWorkerSiteStorageFunnelFailsClosedOnAMalformedDescriptor: the storage
// funnel fails closed on a descriptor that does not decode. It opens and
// creates no binding in the box, and the relocated class refuses with the
// descriptor error as a standing refusal.
func TestWorkerSiteStorageFunnelFailsClosedOnAMalformedDescriptor(t *testing.T) {
	resetControlPlaneForTest(t)
	bindingRoot := filepath.Join(t.TempDir(), "store")
	cityPath := oneShotCLICity(t, bindingRoot)
	stubInfraControllerPing(t, 0)
	captureCLIStorageStderr(t)
	writeControlPlaneBytes(t, cityPath, []byte("schema = ["))

	routes := cliStorageRoutes(cityPath)
	if _, err := os.Stat(bindingRoot); err == nil {
		t.Fatal("the funnel created the binding root on a malformed descriptor")
	}
	store, relocated := routes.storeFor(coordclassFor(config.BeadClassSessions))
	if !relocated {
		t.Fatal("sessions resolved to the work store")
	}
	_, err := store.Get("any-1")
	var descErr *errControlPlaneDescriptor
	if !errors.As(err, &descErr) || !storeref.IsStandingRefusal(err) {
		t.Fatalf("sessions Get = %v, want a standing descriptor refusal", err)
	}
	if !strings.Contains(err.Error(), "control-plane descriptor") {
		t.Fatalf("err = %v", err)
	}
}

// TestWorkerSiteCityStartRereadsTheDescriptor is the supervisor's per-city
// start after an operator removes a stray descriptor: the refusal is decided
// from the file as it is now, not from the plane the process cached.
func TestWorkerSiteCityStartRereadsTheDescriptor(t *testing.T) {
	resetControlPlaneForTest(t)
	cityDir := newWorkerSiteCity(t)
	writeControlPlaneFixture(t, cityDir, testDescriptor(cityDir, nil))
	if err := refuseCityStartOnWorkerSite(cityDir); !errors.Is(err, errWorkerSiteRefusesController) {
		t.Fatalf("refuseCityStartOnWorkerSite with the descriptor = %v, want the refusal", err)
	}
	if err := os.Remove(filepath.Join(cityDir, citylayout.ControlPlaneDescriptorPath)); err != nil {
		t.Fatal(err)
	}
	if err := refuseCityStartOnWorkerSite(cityDir); err != nil {
		t.Fatalf("refuseCityStartOnWorkerSite after the descriptor was removed = %v, want nil", err)
	}
	if workerSiteProcess.Load() {
		t.Fatal("the per-city start check marked the process a worker site")
	}
}
