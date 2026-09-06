package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// serverLifecycleCityRuntime builds a minimal city runtime over the given
// provider, mirroring the shape the supervisor's managed-stop paths drive:
// stopManagedCity / the run loop's deferred cr.shutdown() are the only ways a
// supervisor-managed city ever stops, so CityRuntime.shutdown() is where the
// managed path either tears the provider server down or leaks it (#5175).
func serverLifecycleCityRuntime(t *testing.T, sp runtime.Provider) *CityRuntime {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	return newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})
}

// markOwnedForTest simulates run() having taken ownership of the city, the
// gate that lets shutdown() tear the shared server down. Tests that drive
// shutdown() directly never call run(), so they set it explicitly; a test
// that omits it is asserting the un-owned (discarded-runtime) behavior.
func markOwnedForTest(cr *CityRuntime) { cr.ownedCity.Store(true) }

// teardownEvents returns, from a recorded event stream, the number of
// TeardownServer calls, the index of the first one (or -1), and the index of
// the last ListRunning (or -1). Every teardown-ordering assertion below reads
// these three, so they live in one place rather than three copy-pasted scans.
func teardownEvents(events []string) (teardowns, firstTeardown, lastListRunning int) {
	firstTeardown, lastListRunning = -1, -1
	for i, e := range events {
		switch e {
		case "TeardownServer":
			teardowns++
			if firstTeardown == -1 {
				firstTeardown = i
			}
		case "ListRunning":
			lastListRunning = i
		}
	}
	return teardowns, firstTeardown, lastListRunning
}

func (p *lifecycleOrderProvider) snapshotEvents() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

// TestCityRuntimeShutdownTearsDownProviderServer pins the managed-stop half
// of #5175: a full (non-preserving) CityRuntime.shutdown() by the runtime that
// owns the city must tear down the provider's shared server after stopping the
// sessions, exactly like the standalone stop path (cmdStopBody ->
// teardownServerForStop) already does. Without it, a supervisor-managed
// `gc stop` reports "City stopped." while the city's tmux server keeps running
// until someone kills it by hand.
func TestCityRuntimeShutdownTearsDownProviderServer(t *testing.T) {
	sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}
	cr := serverLifecycleCityRuntime(t, sp)
	markOwnedForTest(cr)

	cr.shutdown()

	teardowns, firstTeardown, lastListRunning := teardownEvents(sp.snapshotEvents())
	if teardowns != 1 {
		t.Fatalf("shutdown called TeardownServer %d time(s), want exactly 1 (events: %v): the managed stop path must not leak the provider server (#5175)", teardowns, sp.snapshotEvents())
	}
	if lastListRunning == -1 || lastListRunning > firstTeardown {
		t.Fatalf("TeardownServer must come after the session-stop sweep's ListRunning (events: %v)", sp.snapshotEvents())
	}

	// The Once makes "exactly once" a property of this runtime: a second
	// shutdown call must not tear the (next supervisor's) server down again.
	cr.shutdown()
	if teardowns, _, _ := teardownEvents(sp.snapshotEvents()); teardowns != 1 {
		t.Fatalf("a second shutdown called TeardownServer again (%d calls total)", teardowns)
	}
}

// TestCityRuntimeShutdownWithoutOwnershipKeepsServer is the core of the
// draft-and-fix in #5196: a runtime that never reached run() does not own the
// city. The supervisor calls its shutdown() anyway on every init-failure /
// discard path — a failed adoption of an already-running city, a
// controller-lock/socket/token failure — and if that teardown were
// unconditional it would run kill-server on the LIVE owner's shared socket,
// killing every session. An un-owned shutdown must stop nothing's server.
func TestCityRuntimeShutdownWithoutOwnershipKeepsServer(t *testing.T) {
	sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}
	cr := serverLifecycleCityRuntime(t, sp)
	// Deliberately NOT markOwnedForTest: this models the discarded runtime.

	cr.shutdown()

	if teardowns, _, _ := teardownEvents(sp.snapshotEvents()); teardowns != 0 {
		t.Fatalf("an un-owned shutdown tore down the provider server %d time(s) (events: %v); a discarded runtime must never kill the live owner's server", teardowns, sp.snapshotEvents())
	}
}

// TestCityRuntimeShutdownSkipsTeardownWhenListRunningFailed pins the second
// guard: a failed ListRunning yields an empty slice, so gracefulStopAll stops
// nothing. Tearing the server down after that would convert a transient
// listing error into an ungraceful mass kill of sessions we never enumerated.
// The runtime owns the city here, so only the listing failure holds teardown.
func TestCityRuntimeShutdownSkipsTeardownWhenListRunningFailed(t *testing.T) {
	sp := &lifecycleOrderProvider{Fake: runtime.NewFake(), listErr: errors.New("tmux unreachable")}
	cr := serverLifecycleCityRuntime(t, sp)
	markOwnedForTest(cr)

	cr.shutdown()

	teardowns, _, lastListRunning := teardownEvents(sp.snapshotEvents())
	if lastListRunning == -1 {
		t.Fatalf("expected shutdown to attempt a ListRunning sweep (events: %v)", sp.snapshotEvents())
	}
	if teardowns != 0 {
		t.Fatalf("shutdown tore down the server %d time(s) after a failed enumeration (events: %v); kill-server must not run on a fleet we never listed", teardowns, sp.snapshotEvents())
	}
}

// TestCityRuntimeShutdownPreservingSessionsKeepsServer pins the deliberate
// asymmetry: preserve-mode shutdown hands live sessions to the next
// supervisor for re-adoption, and those sessions live inside the provider
// server — tearing it down would kill exactly what preserve mode exists to
// keep. The server must survive a preserving shutdown even by the owning
// runtime.
func TestCityRuntimeShutdownPreservingSessionsKeepsServer(t *testing.T) {
	sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}
	cr := serverLifecycleCityRuntime(t, sp)
	markOwnedForTest(cr)

	cr.preserveSessionsOnShutdown()
	cr.shutdown()

	if teardowns, _, _ := teardownEvents(sp.snapshotEvents()); teardowns != 0 {
		t.Fatalf("preserve-mode shutdown tore down the provider server %d time(s) (events: %v); preserved sessions cannot outlive their server", teardowns, sp.snapshotEvents())
	}
}

// TestCityRuntimeShutdownWithoutServerLifecycleProviderIsANoOp pins that a
// provider without the optional ServerLifecycleProvider extension shuts down
// exactly as before — the teardown hook is a type-asserted no-op, not a new
// requirement on every provider.
func TestCityRuntimeShutdownWithoutServerLifecycleProviderIsANoOp(t *testing.T) {
	cr := serverLifecycleCityRuntime(t, runtime.NewFake())
	markOwnedForTest(cr)
	cr.shutdown() // must not panic
}

// forceLifecycleProvider is a gated-start provider that also records
// ListRunning / TeardownServer ordering and satisfies ServerLifecycleProvider.
// Its first ListRunning releases the gated start and waits for it to land, so
// a force shutdown that abandoned the async-start wait takes a SECOND
// ListRunning to catch the late session — exactly the path the teardown must
// still run after (and only after).
type forceLifecycleProvider struct {
	*gatedStartProvider
	evMu   sync.Mutex
	events []string

	workerStarted chan struct{}
	startedOnce   sync.Once

	// awaitAsyncStart blocks until the async-start goroutine has finished,
	// not merely until its provider Start returned. The two are different
	// moments and the gap between them is what used to make this test flaky:
	// enqueuePreparedStartWaveForCity runs the provider Start, then commits
	// the session's creating -> active transition, and only then releases the
	// tracker. A stop that lands in the gap is rejected by the session state
	// machine ("state \"creating\" does not accept command \"suspend\""), so
	// the late runtime survives a shutdown the test expected to tear it down.
	// Waiting on the tracker puts the second sweep strictly after the commit.
	awaitAsyncStart func() bool
}

func newForceLifecycleProvider() *forceLifecycleProvider {
	return &forceLifecycleProvider{
		gatedStartProvider: newGatedStartProvider(),
		workerStarted:      make(chan struct{}),
	}
}

// Start signals workerStarted once the released gated start has actually
// landed in the fake, so ListRunning can wait on it instead of polling.
func (p *forceLifecycleProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	err := p.gatedStartProvider.Start(ctx, name, cfg)
	if err == nil && name == "worker" {
		p.startedOnce.Do(func() { close(p.workerStarted) })
	}
	return err
}

// setAwaitAsyncStart installs the async-start barrier. The test can only build
// it once the CityRuntime exists, so it is set after construction and read
// under the same lock ListRunning already takes.
func (p *forceLifecycleProvider) setAwaitAsyncStart(fn func() bool) {
	p.evMu.Lock()
	defer p.evMu.Unlock()
	p.awaitAsyncStart = fn
}

func (p *forceLifecycleProvider) record(ev string) {
	p.evMu.Lock()
	p.events = append(p.events, ev)
	p.evMu.Unlock()
}

func (p *forceLifecycleProvider) snapshotEvents() []string {
	p.evMu.Lock()
	defer p.evMu.Unlock()
	return append([]string(nil), p.events...)
}

func (p *forceLifecycleProvider) ListRunning(prefix string) ([]string, error) {
	p.evMu.Lock()
	p.events = append(p.events, "ListRunning")
	call := 0
	for _, e := range p.events {
		if e == "ListRunning" {
			call++
		}
	}
	awaitAsyncStart := p.awaitAsyncStart
	p.evMu.Unlock()

	// The snapshot is taken before the release, so the first sweep still sees
	// an empty fleet and the worker is genuinely late. That is the scenario
	// this test exists for, and it is unchanged.
	running, err := p.Fake.ListRunning(prefix)
	if call == 1 {
		p.release("worker")
		select {
		case <-p.workerStarted:
		case <-time.After(hangBudget):
			p.record("workerStartTimedOut")
		}
		// Provider Start has returned; the session's creating -> active commit
		// has not necessarily landed yet. Wait for the whole async-start
		// goroutine so the second sweep stops a session the state machine will
		// actually accept a stop for.
		if awaitAsyncStart != nil && !awaitAsyncStart() {
			p.record("asyncStartWaitTimedOut")
		}
	}
	return running, err
}

// Stop is recorded so the trace says whether the late sweep even ATTEMPTED a
// stop — an empty lateRunning and a stop that did not take look identical
// otherwise (ga-0q686).
func (p *forceLifecycleProvider) Stop(name string) error {
	p.record("Stop:" + name)
	return p.Fake.Stop(name)
}

func (p *forceLifecycleProvider) ConfigureServer() error { return nil }

func (p *forceLifecycleProvider) TeardownServer() error {
	p.record("TeardownServer")
	return nil
}

var (
	_ runtime.Provider                = (*forceLifecycleProvider)(nil)
	_ runtime.ServerLifecycleProvider = (*forceLifecycleProvider)(nil)
)

// TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep is the force-mode
// ordering proof the single-ListRunning fakes above cannot give: under a force
// stop with an async start still pending, shutdown() takes a second
// ListRunning to catch the late session, and the teardown must land AFTER that
// second sweep — never between the two, when a just-created session is still
// live. It also confirms the owned+enumerated gate lets the teardown through
// on the force path.
func TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 26, 12, 1, 30, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":         "worker",
			"template":             "worker",
			"generation":           "1",
			"continuation_epoch":   "1",
			"instance_token":       "tok-worker",
			"pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := newForceLifecycleProvider()
	cfg := &config.City{
		Daemon: config.DaemonConfig{ShutdownTimeout: "500ms"},
		Agents: []config.Agent{{Name: "worker"}},
	}
	forceStop := &atomic.Bool{}
	forceStop.Store(true)
	cr := &CityRuntime{
		cfg:                 cfg,
		sp:                  sp,
		rec:                 events.Discard,
		standaloneCityStore: store,
		asyncStartLimiter:   newAsyncStartLimiter(maxParallelStartsPerTick(cfg)),
		forceStopShutdown:   forceStop,
		logPrefix:           "gc test",
		stdout:              ioDiscard{},
		stderr:              ioDiscard{},
	}
	markOwnedForTest(cr)
	// hangBudget rather than an unbounded wait: a regression that never
	// commits the start should fail this test loudly instead of hanging it.
	sp.setAwaitAsyncStart(func() bool { return cr.asyncStarts.wait(hangBudget) })

	tp := TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker"}
	if got := executePlannedStartsTraced(
		context.Background(),
		[]startCandidate{{info: sessiontest.SeedBead(t, session), tp: tp}},
		cfg,
		map[string]TemplateParams{"worker": tp},
		sp,
		store,
		"test-city",
		"",
		clk,
		events.Discard,
		time.Minute,
		ioDiscard{},
		ioDiscard{},
		nil,
		withAsyncStartExecution(),
		withAsyncStartLimiter(cr.ensureAsyncStartLimiter()),
		withAsyncStartTracker(&cr.asyncStarts),
	); got != 1 {
		t.Fatalf("woken = %d, want 1", got)
	}
	sp.waitForStarts(t, 1)

	cr.shutdown()

	ev := sp.snapshotEvents()
	teardowns, firstTeardown, lastListRunning := teardownEvents(ev)
	listRunnings := 0
	for _, e := range ev {
		if e == "ListRunning" {
			listRunnings++
		}
		// Either barrier expiring turns the assertions below into a coin flip,
		// so name the expiry instead of letting it resurface as a flake.
		if e == "workerStartTimedOut" || e == "asyncStartWaitTimedOut" {
			t.Fatalf("%s: the late async start did not land within hangBudget (%s) (events: %v)", e, hangBudget, ev)
		}
	}
	if listRunnings < 2 {
		t.Fatalf("ListRunning calls = %d, want a second snapshot for force async-start cleanup (events: %v)", listRunnings, ev)
	}
	if teardowns != 1 {
		t.Fatalf("force shutdown called TeardownServer %d time(s), want exactly 1 (events: %v)", teardowns, ev)
	}
	if firstTeardown < lastListRunning {
		t.Fatalf("TeardownServer landed before the last (late-async) ListRunning (events: %v); a session created between the sweeps would be killed ungracefully", ev)
	}
	if sp.IsRunning("worker") {
		// The four assertions above all print ev; this one did not, so the only
		// assertion that ever fires was the only undiagnosable one (ga-0q686).
		t.Fatalf("force shutdown missed the late async-started runtime (events: %v)", ev)
	}
}
