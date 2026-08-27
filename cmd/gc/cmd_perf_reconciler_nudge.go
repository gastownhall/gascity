package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	gcruntime "github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type reconcilerPerfNudgeMeasurement struct {
	sample   reconcilerPerfArmSample
	windowNS int64
}

type reconcilerPerfNudgeCall struct {
	name      string
	content   []gcruntime.ContentBlock
	enteredAt time.Time
	err       error
}

type reconcilerPerfNudgeProvider struct {
	*gcruntime.Fake

	mu      sync.Mutex
	calls   []reconcilerPerfNudgeCall
	now     func() time.Time
	block   <-chan struct{}
	entered chan<- struct{}
}

func (p *reconcilerPerfNudgeProvider) Nudge(name string, content []gcruntime.ContentBlock) error {
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	call := reconcilerPerfNudgeCall{
		name:      name,
		content:   append([]gcruntime.ContentBlock(nil), content...),
		enteredAt: now(),
	}
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
	}
	if p.block != nil {
		<-p.block
	}
	call.err = p.Fake.Nudge(name, content)
	p.mu.Lock()
	p.calls = append(p.calls, call)
	p.mu.Unlock()
	return call.err
}

var (
	reconcilerPerfNudgeFixtureTime     = time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	reconcilerPerfNudgeFixtureExpiry   = time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
	reconcilerPerfNudgeFixtureActivity = reconcilerPerfNudgeFixtureTime.Add(-10 * time.Second)
)

func (p *reconcilerPerfNudgeProvider) snapshotNudgeCalls() []reconcilerPerfNudgeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]reconcilerPerfNudgeCall(nil), p.calls...)
}

type reconcilerPerfNudgeFixture struct {
	cityPath             string
	cfg                  *config.City
	store                beads.Store
	provider             *reconcilerPerfNudgeProvider
	info                 session.Info
	item                 queuedNudge
	beforeControllerStop func()
	onStoreInstalled     func()
	measurementNow       func() time.Time
}

var reconcilerPerfNudgeStoreMu sync.Mutex

func (f *reconcilerPerfNudgeFixture) installNudgeStore() func() {
	reconcilerPerfNudgeStoreMu.Lock()
	previous := openNudgeBeadStore
	openNudgeBeadStore = func(string) beads.NudgesStore { return beads.NudgesStore{Store: f.store} }
	if f.onStoreInstalled != nil {
		f.onStoreInstalled()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			openNudgeBeadStore = previous
			reconcilerPerfNudgeStoreMu.Unlock()
		})
	}
}

func (f *reconcilerPerfNudgeFixture) dispatchLegacy() (int, error) {
	restore := f.installNudgeStore()
	defer restore()
	return f.dispatchLegacyWithInstalledStore()
}

func (f *reconcilerPerfNudgeFixture) dispatchLegacyWithInstalledStore() (int, error) {
	snapshot, err := loadSessionBeadSnapshot(f.store)
	if err != nil {
		return 0, err
	}
	return dispatchAllQueuedNudges(f.cityPath, f.cfg, f.store, f.store, f.provider, snapshot, nil)
}

func newReconcilerPerfNudgeFixture(workspacePath, arm, pairID string) (*reconcilerPerfNudgeFixture, error) {
	cityPath := filepath.Join(workspacePath, "nudge", arm, pairID)
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		return nil, fmt.Errorf("creating nudge fixture workspace: %w", err)
	}
	store := beads.NewMemStore()
	provider := &reconcilerPerfNudgeProvider{Fake: gcruntime.NewFake()}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "reconciler-perf"},
		Agents: []config.Agent{{
			Name:         reconcilerPerfStartTemplate,
			StartCommand: "true",
		}},
		Daemon: config.DaemonConfig{NudgeDispatcher: "supervisor"},
	}
	sessionName := "gc-reconciler-perf-nudge-" + pairID
	info, err := sessionFrontDoor(store).CreateSessionInfo(session.CreateSpec{
		Title:     pairID,
		AgentName: reconcilerPerfStartTemplate,
		Metadata: map[string]string{
			"session_name": sessionName,
			"agent_name":   reconcilerPerfStartTemplate,
			"template":     reconcilerPerfStartTemplate,
			"provider":     "fake",
			"state":        string(session.StateActive),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating nudge session: %w", err)
	}
	if err := provider.Start(context.Background(), info.SessionName, gcruntime.Config{Command: "true", WorkDir: cityPath}); err != nil {
		return nil, fmt.Errorf("starting nudge session: %w", err)
	}
	provider.SetAttached(info.SessionName, false)
	provider.SetActivity(info.SessionName, reconcilerPerfNudgeFixtureActivity)
	item := newQueuedNudgeWithOptions(reconcilerPerfStartTemplate, "reconciler performance nudge", "session", reconcilerPerfNudgeFixtureTime, queuedNudgeOptions{SessionID: info.ID})
	item.ID = "reconciler-perf-nudge-" + pairID
	item.CreatedAt = reconcilerPerfNudgeFixtureTime
	item.DeliverAfter = reconcilerPerfNudgeFixtureTime
	item.ExpiresAt = reconcilerPerfNudgeFixtureExpiry
	if err := enqueueQueuedNudgeWithStore(cityPath, beads.NudgesStore{Store: store}, item); err != nil {
		return nil, fmt.Errorf("enqueueing nudge fixture item: %w", err)
	}
	return &reconcilerPerfNudgeFixture{
		cityPath: cityPath,
		cfg:      cfg,
		store:    store,
		provider: provider,
		info:     info,
		item:     item,
	}, nil
}

func measureLegacyReconcilerPerfNudge(ctx context.Context, workspacePath, pairID string) (reconcilerPerfNudgeMeasurement, error) {
	if err := ctx.Err(); err != nil {
		return reconcilerPerfNudgeMeasurement{}, err
	}
	fixture, err := newReconcilerPerfNudgeFixture(workspacePath, "legacy", pairID)
	if err != nil {
		return reconcilerPerfNudgeMeasurement{}, err
	}
	return measureLegacyReconcilerPerfNudgeFixture(ctx, fixture)
}

func measureLegacyReconcilerPerfNudgeFixture(ctx context.Context, fixture *reconcilerPerfNudgeFixture) (reconcilerPerfNudgeMeasurement, error) {
	if err := ctx.Err(); err != nil {
		return reconcilerPerfNudgeMeasurement{}, err
	}
	now := time.Now
	if fixture.measurementNow != nil {
		now = fixture.measurementNow
	}
	restore := fixture.installNudgeStore()
	defer restore()
	neededAt := now()
	delivered, err := fixture.dispatchLegacyWithInstalledStore()
	if err == nil && delivered != 1 {
		err = fmt.Errorf("legacy nudge deliveries = %d, want 1", delivered)
	}
	return fixture.finish(neededAt, now(), err), nil
}

func measureKeyedReconcilerPerfNudge(ctx context.Context, workspacePath, pairID string) (reconcilerPerfNudgeMeasurement, error) {
	if err := ctx.Err(); err != nil {
		return reconcilerPerfNudgeMeasurement{}, err
	}
	fixture, err := newReconcilerPerfNudgeFixture(workspacePath, "keyed", pairID)
	if err != nil {
		return reconcilerPerfNudgeMeasurement{}, err
	}
	return measureKeyedReconcilerPerfNudgeFixture(ctx, fixture)
}

func measureKeyedReconcilerPerfNudgeFixture(ctx context.Context, fixture *reconcilerPerfNudgeFixture) (reconcilerPerfNudgeMeasurement, error) {
	restore := fixture.installNudgeStore()
	results := make(chan sessionStartReconcileResult, 1)
	controller, err := newNudgeKeyController(nudgeKeyControllerOptions{
		Workers: 1, MaxDistinct: 1, MaxRetries: 0,
		Reconcile: func(reconcileCtx context.Context, sessionID string) error {
			return reconcileExactQueuedNudge(reconcileCtx, sessionID, exactQueuedNudgeParams{
				CityPath: fixture.cityPath, Config: fixture.cfg, Provider: fixture.provider,
				SessionStore: fixture.store, NudgeStore: fixture.store,
			})
		},
		Observer: func(result sessionStartReconcileResult) { results <- result },
	})
	if err != nil {
		restore()
		return reconcilerPerfNudgeMeasurement{}, fmt.Errorf("creating keyed nudge controller: %w", err)
	}
	if err := controller.Start(ctx); err != nil {
		controller.Stop()
		restore()
		return reconcilerPerfNudgeMeasurement{}, fmt.Errorf("starting keyed nudge controller: %w", err)
	}
	var stopOnce sync.Once
	stopAndRestore := func() {
		stopOnce.Do(func() {
			if fixture.beforeControllerStop != nil {
				fixture.beforeControllerStop()
			}
			controller.Stop()
			restore()
		})
	}
	defer stopAndRestore()
	finish := func(neededAt, finishedAt time.Time, runErr error) reconcilerPerfNudgeMeasurement {
		stopAndRestore()
		return fixture.finish(neededAt, finishedAt, runErr)
	}

	neededAt := time.Now()
	state, runErr := nudgequeue.LoadState(fixture.cityPath)
	ids := discoverDueExactNudgeSessionIDs(state, neededAt)
	if runErr == nil && (len(ids) != 1 || ids[0] != fixture.info.ID) {
		runErr = fmt.Errorf("due exact nudge IDs = %v, want [%s]", ids, fixture.info.ID)
	}
	if runErr == nil {
		var outcome sessionStartAdmissionOutcome
		outcome, runErr = controller.Admit(fixture.info.ID)
		if runErr == nil && outcome != sessionStartAdmissionAccepted {
			runErr = fmt.Errorf("keyed nudge admission = %s, want accepted", outcome)
		}
	}
	if runErr != nil {
		return finish(neededAt, time.Now(), runErr), nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, reconcilerPerfArmTimeout)
	defer cancel()
	for {
		select {
		case result := <-results:
			if result.Outcome == sessionStartReconcileRetrying {
				continue
			}
			if result.Err != nil {
				return finish(neededAt, time.Now(), result.Err), nil
			}
			if result.Outcome != sessionStartReconcileSucceeded {
				return finish(neededAt, time.Now(), fmt.Errorf("keyed nudge outcome = %s", result.Outcome)), nil
			}
			return finish(neededAt, time.Now(), nil), nil
		case <-waitCtx.Done():
			return finish(neededAt, time.Now(), waitCtx.Err()), nil
		}
	}
}

func (f *reconcilerPerfNudgeFixture) finish(neededAt, finishedAt time.Time, runErr error) reconcilerPerfNudgeMeasurement {
	problems := make([]error, 0, 6)
	if runErr != nil {
		problems = append(problems, runErr)
	}
	calls := f.provider.snapshotNudgeCalls()
	if len(calls) != 1 {
		problems = append(problems, fmt.Errorf("provider Nudge calls = %d, want 1", len(calls)))
	}
	var latency *int64
	outcome := "not_nudged"
	if len(calls) > 0 {
		value := calls[0].enteredAt.Sub(neededAt).Nanoseconds()
		latency = &value
		outcome = "nudge_entered"
		if calls[0].name != f.info.SessionName {
			problems = append(problems, fmt.Errorf("provider Nudge target = %q, want %q", calls[0].name, f.info.SessionName))
		}
		expectedContent := gcruntime.TextContent(formatNudgeInjectOutput([]queuedNudge{f.item}))
		if !reflect.DeepEqual(calls[0].content, expectedContent) {
			problems = append(problems, fmt.Errorf("provider Nudge payload = %#v, want %#v", calls[0].content, expectedContent))
		}
		if value < 0 {
			problems = append(problems, fmt.Errorf("provider Nudge preceded action-needed timestamp"))
		}
		if calls[0].err != nil {
			problems = append(problems, calls[0].err)
		}
	}
	state, err := nudgequeue.LoadState(f.cityPath)
	if err != nil {
		problems = append(problems, fmt.Errorf("listing nudge queue: %w", err))
	} else if len(state.Pending) != 0 || len(state.InFlight) != 0 || len(state.Dead) != 0 {
		problems = append(problems, fmt.Errorf("nudge queue pending/in_flight/dead = %d/%d/%d, want empty", len(state.Pending), len(state.InFlight), len(state.Dead)))
	}
	shadow, ok, err := nudgeFrontDoor(beads.NudgesStore{Store: f.store}).FindIncludingTerminal(f.item.ID)
	if err != nil {
		problems = append(problems, fmt.Errorf("reading nudge shadow: %w", err))
	} else if !ok || shadow.Open || shadow.State != "injected" || shadow.CommitBoundary != "provider-nudge-return" {
		problems = append(problems, fmt.Errorf("nudge shadow = %+v, want closed injected provider-nudge-return", shadow))
	}
	bead, err := f.store.Get(f.info.ID)
	if err != nil {
		problems = append(problems, fmt.Errorf("reading nudged session: %w", err))
	} else if strings.TrimSpace(bead.Metadata[session.MetadataLastNudgeDeliveredAt]) == "" {
		problems = append(problems, fmt.Errorf("session missing %s", session.MetadataLastNudgeDeliveredAt))
	}
	if len(problems) == 0 {
		outcome = "nudged_injected"
	}
	sample := reconcilerPerfArmSample{LatencyNS: latency, Outcome: outcome}
	if joined := errors.Join(problems...); joined != nil {
		sample.Error = joined.Error()
	}
	return reconcilerPerfNudgeMeasurement{sample: sample, windowNS: finishedAt.Sub(neededAt).Nanoseconds()}
}
