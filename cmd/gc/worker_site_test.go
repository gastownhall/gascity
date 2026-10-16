package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
	"github.com/gastownhall/gascity/internal/session"
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
	if got := dials.Load(); got != 0 {
		t.Fatalf("the nudge dialed %d gc sockets, want 0", got)
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
	if got := dials.Load(); got != 0 {
		t.Fatalf("gc convoy poke dialed %d gc sockets, want 0", got)
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
