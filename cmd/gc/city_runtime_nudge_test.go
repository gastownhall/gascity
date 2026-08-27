package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type nudgeSessionListCountingStore struct {
	beads.Store
	lists atomic.Int32
}

func (s *nudgeSessionListCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists.Add(1)
	return s.Store.List(query)
}

type nudgeBlockingIsRunningProvider struct {
	runtime.Provider
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *nudgeBlockingIsRunningProvider) IsRunning(name string) bool {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return p.Provider.IsRunning(name)
}

func TestCityRuntimeNudgeKeyControllerOffKeepsLegacy(t *testing.T) {
	cr := newSessionStartCityRuntimeForTest(t, rollout.Off, true)
	cr.cfg.Daemon.NudgeDispatcher = "supervisor"
	if err := cr.ensureNudgeKeyController(context.Background()); err != nil {
		t.Fatalf("ensureNudgeKeyController: %v", err)
	}
	if cr.nudgeKeyController != nil {
		t.Fatal("off mode installed keyed nudge controller")
	}
}

func TestCityRuntimeNudgeDispatchExactOnlySkipsSessionCensus(t *testing.T) {
	dir := t.TempDir()
	cr := newSessionStartCityRuntimeForTest(t, rollout.Auto, true)
	cr.cityPath = dir
	cr.cs.cityPath = dir
	cr.cfg.Daemon.NudgeDispatcher = "supervisor"
	counting := &nudgeSessionListCountingStore{Store: cr.cs.cityBeadStore}
	cr.cs.cityBeadStore = counting

	reconciled := make(chan string, 1)
	controller, err := newNudgeKeyController(nudgeKeyControllerOptions{
		Workers: 1, MaxDistinct: 4, MaxRetries: 0,
		Reconcile: func(_ context.Context, id string) error {
			reconciled <- id
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(controller.Stop)
	cr.nudgeKeyController = controller
	cr.nudgeKeyMode = rollout.Auto
	cr.nudgeKeyFallback = make(map[string]struct{})

	now := time.Now().UTC()
	if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		state.Pending = append(state.Pending, queuedNudge{
			ID: "nudge-exact", Agent: "worker", SessionID: "gc-exact",
			CreatedAt: now, DeliverAfter: now,
		})
		return nil
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	cr.nudgeDispatchTick(context.Background())
	select {
	case got := <-reconciled:
		if got != "gc-exact" {
			t.Fatalf("reconciled %q, want gc-exact", got)
		}
	case <-time.After(hangBudget):
		t.Fatal("exact keyed nudge was not admitted")
	}
	if got := counting.lists.Load(); got != 0 {
		t.Fatalf("session List calls = %d, want 0 on exact-only keyed wake", got)
	}
}

func TestCityRuntimeNudgeDispatchOffUsesLegacyDelivery(t *testing.T) {
	cr, store, provider, info := newNudgeDeliveryRuntime(t, rollout.Off)
	item := newQueuedNudgeWithOptions("worker", "legacy", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{})
	if err := enqueueQueuedNudgeWithStore(cr.cityPath, store, item); err != nil {
		t.Fatalf("enqueue legacy nudge: %v", err)
	}

	cr.nudgeDispatchTick(context.Background())

	if got := provider.CountCalls("Nudge", info.SessionName); got != 1 {
		t.Fatalf("legacy Nudge calls = %d, want 1", got)
	}
	assertTerminalNudgeShadow(t, store, item.ID)
}

func TestCityRuntimeNudgeDispatchKeyedLoadFailureRespectsMode(t *testing.T) {
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		t.Run(string(mode), func(t *testing.T) {
			cr, store, provider, info := newNudgeDeliveryRuntime(t, mode)
			if err := cr.ensureNudgeKeyController(context.Background()); err != nil {
				t.Fatalf("ensureNudgeKeyController: %v", err)
			}
			t.Cleanup(cr.stopNudgeKeyController)

			item := newQueuedNudgeWithOptions("worker", "load failure", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: info.ID})
			if err := enqueueQueuedNudgeWithStore(cr.cityPath, store, item); err != nil {
				t.Fatalf("enqueue nudge: %v", err)
			}

			var stderr bytes.Buffer
			cr.stderr = &stderr
			previous := loadKeyedNudgeQueueState
			loadKeyedNudgeQueueState = func(string) (nudgeQueueState, error) {
				return nudgeQueueState{}, errors.New("keyed queue load failed")
			}
			t.Cleanup(func() { loadKeyedNudgeQueueState = previous })

			cr.nudgeDispatchTick(context.Background())

			if !strings.Contains(stderr.String(), "keyed nudge queue load") {
				t.Fatalf("keyed load failure was not logged: %q", stderr.String())
			}
			if mode == rollout.Auto {
				if got := provider.CountCalls("Nudge", info.SessionName); got != 1 {
					t.Fatalf("Auto Nudge calls = %d, want 1 fresh legacy fallback", got)
				}
				assertTerminalNudgeShadow(t, store, item.ID)
				return
			}

			if got := provider.CountCalls("Nudge", info.SessionName); got != 0 {
				t.Fatalf("Require Nudge calls = %d, want 0", got)
			}
			pending, inFlight, dead, err := listQueuedNudges(cr.cityPath, "worker", time.Now())
			if err != nil {
				t.Fatalf("list queued nudges: %v", err)
			}
			if len(pending) != 1 || pending[0].ID != item.ID || len(inFlight) != 0 || len(dead) != 0 {
				t.Fatalf("Require queue state pending/inflight/dead = %v/%v/%v, want only pending %q", pending, inFlight, dead, item.ID)
			}
			if !cr.nudgeKeyController.TakeAuditRequest() {
				t.Fatal("Require keyed load failure did not request an authoritative audit")
			}
		})
	}
}

func TestCityRuntimeNudgeDispatchMixedOwnershipDeliversEachItemOnce(t *testing.T) {
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		t.Run(string(mode), func(t *testing.T) {
			cr, store, provider, info := newNudgeDeliveryRuntime(t, mode)
			controller, err := newNudgeKeyController(nudgeKeyControllerOptions{
				Workers: 1, MaxDistinct: 4, MaxRetries: 1,
				Reconcile: func(ctx context.Context, id string) error {
					return reconcileExactQueuedNudge(ctx, id, exactQueuedNudgeParams{
						CityPath: cr.cityPath, Config: cr.cfg, Provider: provider,
						SessionStore: store.Store, NudgeStore: store.Store,
					})
				},
			})
			if err != nil {
				t.Fatalf("newNudgeKeyController: %v", err)
			}
			if err := controller.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(controller.Stop)
			cr.nudgeKeyController = controller
			cr.nudgeKeyMode = mode
			cr.nudgeKeyFallback = make(map[string]struct{})

			now := time.Now().Add(-time.Minute)
			exact := newQueuedNudgeWithOptions("worker", "exact", "session", now, queuedNudgeOptions{SessionID: info.ID})
			unkeyed := newQueuedNudgeWithOptions("worker", "unkeyed", "session", now, queuedNudgeOptions{})
			for _, item := range []queuedNudge{exact, unkeyed} {
				if err := enqueueQueuedNudgeWithStore(cr.cityPath, store, item); err != nil {
					t.Fatalf("enqueue %s: %v", item.ID, err)
				}
			}

			cr.nudgeDispatchTick(context.Background())
			awaitCond(t, func() bool {
				pending, inFlight, _, err := listQueuedNudges(cr.cityPath, "worker", time.Now())
				return err == nil && len(pending) == 0 && len(inFlight) == 0
			}, "mixed keyed and unkeyed nudge acknowledgement")

			if got := provider.CountCalls("Nudge", info.SessionName); got != 2 {
				t.Fatalf("Nudge calls = %d, want exactly 2 (one per ownership path)", got)
			}
			assertTerminalNudgeShadow(t, store, exact.ID)
			assertTerminalNudgeShadow(t, store, unkeyed.ID)
		})
	}
}

func TestCityRuntimeAdmitDueExactNudgesOverflowOwnershipByMode(t *testing.T) {
	for _, test := range []struct {
		mode              rollout.Mode
		wantLegacy        bool
		wantSecondExclude bool
	}{
		{mode: rollout.Auto, wantLegacy: true},
		{mode: rollout.Require, wantSecondExclude: true},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			dir := t.TempDir()
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			defer close(release)
			controller, err := newNudgeKeyController(nudgeKeyControllerOptions{
				Workers: 1, MaxDistinct: 1, MaxRetries: 0,
				Reconcile: func(context.Context, string) error {
					started <- struct{}{}
					<-release
					return nil
				},
			})
			if err != nil {
				t.Fatalf("newNudgeKeyController: %v", err)
			}
			if err := controller.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(controller.Stop)
			cr := &CityRuntime{
				cityPath: dir, nudgeKeyController: controller, nudgeKeyMode: test.mode,
				nudgeKeyFallback: make(map[string]struct{}), nudgeWakeCh: make(chan struct{}, 1),
				stderr: io.Discard,
			}
			now := time.Now().UTC()
			if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
				state.Pending = []queuedNudge{
					{ID: "first", Agent: "worker-a", SessionID: "gc-first", CreatedAt: now, DeliverAfter: now},
					{ID: "second", Agent: "worker-b", SessionID: "gc-second", CreatedAt: now.Add(time.Nanosecond), DeliverAfter: now},
				}
				return nil
			}); err != nil {
				t.Fatalf("seed queue: %v", err)
			}

			excluded, legacyNeeded, _, _ := cr.admitDueExactNudges(now)
			select {
			case <-started:
			case <-time.After(hangBudget):
				t.Fatal("first exact key did not begin reconciliation")
			}
			if _, ok := excluded["gc-first"]; !ok {
				t.Fatalf("first accepted key missing from exclusions: %v", excluded)
			}
			_, secondExcluded := excluded["gc-second"]
			if secondExcluded != test.wantSecondExclude {
				t.Fatalf("second excluded = %t, want %t; exclusions=%v", secondExcluded, test.wantSecondExclude, excluded)
			}
			if legacyNeeded != test.wantLegacy {
				t.Fatalf("legacyNeeded = %t, want %t", legacyNeeded, test.wantLegacy)
			}
			if !controller.TakeAuditRequest() {
				t.Fatal("overflow did not leave the authoritative audit requested")
			}
		})
	}
}

func TestCityRuntimeAdmitDueExactNudgesAutoFallbackIsOneShot(t *testing.T) {
	dir := t.TempDir()
	reconciled := make(chan string, 1)
	controller, err := newNudgeKeyController(nudgeKeyControllerOptions{
		Workers: 1, MaxDistinct: 1, MaxRetries: 0,
		Reconcile: func(_ context.Context, id string) error {
			reconciled <- id
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(controller.Stop)
	cr := &CityRuntime{
		cityPath: dir, nudgeKeyController: controller, nudgeKeyMode: rollout.Auto,
		nudgeKeyFallback: map[string]struct{}{"gc-worker": {}}, nudgeWakeCh: make(chan struct{}, 1),
		stderr: io.Discard,
	}
	now := time.Now().UTC()
	if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		state.Pending = []queuedNudge{{
			ID: "fallback", Agent: "worker", SessionID: "gc-worker",
			CreatedAt: now, DeliverAfter: now,
		}}
		return nil
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	excluded, legacyNeeded, _, _ := cr.admitDueExactNudges(now)
	if len(excluded) != 0 || !legacyNeeded {
		t.Fatalf("fallback pass exclusions/legacy = %v/%t, want empty/true", excluded, legacyNeeded)
	}
	excluded, legacyNeeded, _, _ = cr.admitDueExactNudges(now)
	if _, ok := excluded["gc-worker"]; !ok || legacyNeeded {
		t.Fatalf("post-fallback pass exclusions/legacy = %v/%t, want keyed/false", excluded, legacyNeeded)
	}
	select {
	case got := <-reconciled:
		if got != "gc-worker" {
			t.Fatalf("reconciled %q, want gc-worker", got)
		}
	case <-time.After(hangBudget):
		t.Fatal("fallback key was not re-admitted on the next authoritative pass")
	}
}

func TestCityRuntimeNudgePatrolReadmitsDurableExactKey(t *testing.T) {
	dir := t.TempDir()
	reconciled := make(chan string, 2)
	controller, err := newNudgeKeyController(nudgeKeyControllerOptions{
		Workers: 1, MaxDistinct: 1, MaxRetries: 0,
		Reconcile: func(_ context.Context, id string) error {
			reconciled <- id
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(controller.Stop)
	cr := &CityRuntime{
		cityPath: dir, nudgeKeyController: controller, nudgeKeyMode: rollout.Require,
		nudgeKeyFallback: make(map[string]struct{}), stderr: io.Discard,
	}
	now := time.Now().UTC()
	if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		state.Pending = []queuedNudge{{
			ID: "pending", Agent: "worker", SessionID: "gc-worker",
			CreatedAt: now, DeliverAfter: now,
		}}
		return nil
	}); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	for pass := 1; pass <= 2; pass++ {
		excluded, legacyNeeded, _, _ := cr.admitDueExactNudges(now)
		if _, ok := excluded["gc-worker"]; !ok || legacyNeeded {
			t.Fatalf("pass %d exclusions/legacy = %v/%t, want keyed/false", pass, excluded, legacyNeeded)
		}
		select {
		case <-reconciled:
		case <-time.After(hangBudget):
			t.Fatalf("pass %d did not reconcile", pass)
		}
		awaitCond(t, func() bool { return controller.controller.Pending() == 0 }, "nudge key controller to forget completed admission")
	}
}

func TestCityRuntimeStopNudgeKeyControllerDrainsWorker(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWorker)
	controller, err := newNudgeKeyController(nudgeKeyControllerOptions{
		Workers: 1, MaxDistinct: 1, MaxRetries: 0,
		Reconcile: func(context.Context, string) error {
			close(started)
			<-release
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cr := &CityRuntime{nudgeKeyController: controller}
	if _, err := controller.Admit("gc-worker"); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	<-started
	stopped := make(chan struct{})
	go func() {
		cr.stopNudgeKeyController()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned before the keyed nudge worker drained")
	case <-time.After(50 * time.Millisecond):
		// The 50ms window is the negative assertion, not a hang detector.
	}
	releaseWorker()
	select {
	case <-stopped:
	case <-time.After(hangBudget):
		t.Fatal("stop did not return after the keyed nudge worker drained")
	}
}

func TestCityRuntimeNudgeWorkerHoldsGenerationLease(t *testing.T) {
	cr, store, base, info := newNudgeDeliveryRuntime(t, rollout.Auto)
	blocking := &nudgeBlockingIsRunningProvider{
		Provider: base, entered: make(chan struct{}), release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseProvider := func() { releaseOnce.Do(func() { close(blocking.release) }) }
	t.Cleanup(releaseProvider)
	cr.sp = blocking
	cr.cs.sp = blocking
	if err := cr.ensureNudgeKeyController(context.Background()); err != nil {
		t.Fatalf("ensureNudgeKeyController: %v", err)
	}
	t.Cleanup(cr.stopNudgeKeyController)
	item := newQueuedNudgeWithOptions("worker", "lease", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: info.ID})
	if err := enqueueQueuedNudgeWithStore(cr.cityPath, store, item); err != nil {
		t.Fatalf("enqueue nudge: %v", err)
	}

	cr.nudgeDispatchTick(context.Background())
	select {
	case <-blocking.entered:
	case <-time.After(hangBudget):
		t.Fatal("nudge worker did not enter provider observation")
	}
	swapReady := make(chan func(), 1)
	go func() { swapReady <- cr.cs.beginSessionStartGenerationSwap() }()
	select {
	case end := <-swapReady:
		end()
		t.Fatal("generation swap passed an in-flight keyed nudge lease")
	case <-time.After(50 * time.Millisecond):
		// The 50ms window is the negative assertion, not a hang detector.
	}
	releaseProvider()
	select {
	case end := <-swapReady:
		end()
	case <-time.After(hangBudget):
		t.Fatal("generation swap did not continue after keyed nudge lease release")
	}
}

func TestCityRuntimeSessionLifecycleObserverWakesAndDeliversPendingNudge(t *testing.T) {
	cr, store, provider, info := newNudgeDeliveryRuntime(t, rollout.Auto)
	cr.nudgeWakeCh = make(chan struct{}, 1)
	nudgeController, err := newNudgeKeyController(nudgeKeyControllerOptions{
		Workers: 1, MaxDistinct: 2, MaxRetries: 0,
		Reconcile: func(ctx context.Context, id string) error {
			return reconcileExactQueuedNudge(ctx, id, exactQueuedNudgeParams{
				CityPath: cr.cityPath, Config: cr.cfg, Provider: provider,
				SessionStore: store.Store, NudgeStore: store.Store,
			})
		},
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	if err := nudgeController.Start(context.Background()); err != nil {
		t.Fatalf("Start nudge controller: %v", err)
	}
	t.Cleanup(nudgeController.Stop)
	cr.nudgeKeyController = nudgeController
	cr.nudgeKeyMode = rollout.Auto
	cr.nudgeKeyFallback = make(map[string]struct{})

	var lifecycleObserver func(sessionStartReconcileResult)
	previous := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		lifecycleObserver = opts.Observer
		return newSessionStartController(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = previous })
	snapshot, err := loadSessionBeadSnapshot(store.Store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	if err := cr.ensureSessionStartController(context.Background(), snapshot); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)
	if lifecycleObserver == nil {
		t.Fatal("session lifecycle observer was not installed")
	}
	item := newQueuedNudgeWithOptions("worker", "after start", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: info.ID})
	if err := enqueueQueuedNudgeWithStore(cr.cityPath, store, item); err != nil {
		t.Fatalf("enqueue nudge: %v", err)
	}

	lifecycleObserver(sessionStartReconcileResult{
		Admission: sessionStartAdmission{SessionID: info.ID},
		Outcome:   sessionStartReconcileSucceeded,
	})
	select {
	case <-cr.nudgeWakeCh:
	case <-time.After(hangBudget):
		t.Fatal("successful lifecycle reconciliation did not signal nudge dispatch")
	}
	cr.nudgeDispatchTick(context.Background())
	awaitCond(t, func() bool {
		return provider.CountCalls("Nudge", info.SessionName) == 1
	}, "lifecycle-completion nudge delivery")
	awaitCond(t, func() bool {
		shadow, ok, err := nudgeFrontDoor(store).FindIncludingTerminal(item.ID)
		return err == nil && ok && !shadow.Open && shadow.State == "injected"
	}, "lifecycle-completion nudge terminalization")
	assertTerminalNudgeShadow(t, store, item.ID)
}

func newNudgeDeliveryRuntime(t *testing.T, mode rollout.Mode) (*CityRuntime, beads.NudgesStore, *runtime.Fake, session.Info) {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	t.Cleanup(func() { _ = closeBeadStoreHandle(store.Store) })
	provider := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
		Daemon:    config.DaemonConfig{NudgeDispatcher: "supervisor"},
	}
	mgr := newSessionManagerWithConfig(dir, store.Store, provider, cfg)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	provider.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	cs := coherentSessionStartControllerStateForTest(cfg, provider, store.Store, mode)
	cs.cityPath = dir
	cr := &CityRuntime{
		cityPath: dir, cityName: "test-city", cfg: cfg, sp: provider, cs: cs,
		rec: events.Discard, stdout: io.Discard, stderr: io.Discard,
		nudgeWakeCh: make(chan struct{}, 1),
	}
	return cr, store, provider, info
}

func assertTerminalNudgeShadow(t *testing.T, store beads.NudgesStore, id string) {
	t.Helper()
	shadow, ok, err := nudgeFrontDoor(store).FindIncludingTerminal(id)
	if err != nil {
		t.Fatalf("FindIncludingTerminal(%s): %v", id, err)
	}
	if !ok || shadow.Open || shadow.State != "injected" || shadow.CommitBoundary != "provider-nudge-return" {
		t.Fatalf("terminal shadow %s = %+v, want closed injected provider-nudge-return", id, shadow)
	}
}

func TestCityRuntimeNudgeKeyControllerActivationFailureRespectsMode(t *testing.T) {
	previous := newCityNudgeKeyController
	newCityNudgeKeyController = func(nudgeKeyControllerOptions) (*nudgeKeyController, error) { return nil, errors.New("boom") }
	t.Cleanup(func() { newCityNudgeKeyController = previous })
	for _, mode := range []rollout.Mode{rollout.Auto, rollout.Require} {
		t.Run(string(mode), func(t *testing.T) {
			cr := newSessionStartCityRuntimeForTest(t, mode, true)
			cr.cfg.Daemon.NudgeDispatcher = "supervisor"
			err := cr.ensureNudgeKeyController(context.Background())
			if mode == rollout.Auto && err != nil {
				t.Fatalf("auto returned %v", err)
			}
			if mode == rollout.Require && err == nil {
				t.Fatal("require accepted constructor failure")
			}
		})
	}
}

func TestCityRuntimeNudgeKeyControllerLifecycleCompletionSignalsWake(t *testing.T) {
	cr := &CityRuntime{
		nudgeKeyController: &nudgeKeyController{controller: &sessionStartController{}},
		nudgeWakeCh:        make(chan struct{}, 1),
	}
	cr.signalNudgeKeyWake()
	select {
	case <-cr.nudgeWakeCh:
	default:
		t.Fatal("lifecycle completion did not signal keyed nudge wake")
	}
}
