package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// orphanScanBudgetForTest is the pre-start orphan-scan budget the tests set.
// They run on synctest's fake clock, so it costs no wall time. It is the value
// under test, never a hang detector: it is deliberately not derived from
// goroutineHangBudget.
const orphanScanBudgetForTest = 10 * time.Second

// wedgedScanProvider is a runtime fake whose process-table scan hangs, like a
// /proc walk stuck on a unreadable entry, until release is closed. With wedge
// off it scans clean and returns roots. Each expiry test owns its own instance:
// an abandoned scan goroutine outlives the call that started it, so nothing
// here is shared and every counter is atomic.
type wedgedScanProvider struct {
	*runtime.Fake
	city    string
	wedge   atomic.Bool
	release chan struct{}
	once    sync.Once

	finds  atomic.Int64
	terms  atomic.Int64
	starts atomic.Int64

	// roots are returned by an unwedged scan.
	roots []runtime.LiveRuntime
	// onTerminate, when set, runs inside TerminateRuntime.
	onTerminate func(runtime.LiveRuntime)
}

func newWedgedScanProvider(t *testing.T) *wedgedScanProvider {
	t.Helper()
	p := &wedgedScanProvider{Fake: runtime.NewFake(), city: t.TempDir(), release: make(chan struct{})}
	p.wedge.Store(true)
	return p
}

// unwedge lets every blocked scan return. It is idempotent so a test can both
// call it and defer it: a failed assertion must not leave a goroutine blocked
// when the synctest bubble ends.
func (p *wedgedScanProvider) unwedge() { p.once.Do(func() { close(p.release) }) }

func (p *wedgedScanProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.starts.Add(1)
	return p.Fake.Start(ctx, name, cfg)
}

func (p *wedgedScanProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	p.finds.Add(1)
	if p.wedge.Load() {
		<-p.release
		return nil, nil
	}
	out := make([]runtime.LiveRuntime, len(p.roots))
	copy(out, p.roots)
	for i := range out {
		out[i].SessionID = id
		out[i].City = p.city
	}
	return out, nil
}

func (p *wedgedScanProvider) TerminateRuntime(r runtime.LiveRuntime) error {
	p.terms.Add(1)
	if p.onTerminate != nil {
		p.onTerminate(r)
	}
	return nil
}

func createOptsForOrphanScan(t *testing.T) CreateOptions {
	t.Helper()
	return CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}}
}

// seedSuspendedTargetWithProvider creates a session on an unwedged provider and
// suspends it, so a following Start/StartRuntimeOnly takes the resume path and
// reaches the pre-start orphan gate. Counters are cleared afterwards so a test
// sees only the resume attempt.
func seedSuspendedTargetWithProvider(t *testing.T, sp *wedgedScanProvider) (*Manager, Info) {
	t.Helper()
	sp.wedge.Store(false)
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
	mgr.orphanScanBudget = orphanScanBudgetForTest
	info, err := mgr.CreateSession(context.Background(), createOptsForOrphanScan(t))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sp.finds.Store(0)
	sp.terms.Store(0)
	sp.starts.Store(0)
	sp.wedge.Store(true)
	return mgr, info
}

// assertDeferredByUnfinishedScan checks the outcome every start path must have
// when the orphan scan could not finish: an error that reads as "runtime
// unavailable" (so the caller defers instead of crash-looping), nothing
// terminated, no replacement started.
func assertDeferredByUnfinishedScan(t *testing.T, sp *wedgedScanProvider, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("start succeeded although the orphan scan never finished")
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("err = %v, want it to wrap runtime.ErrRuntimeUnavailable", err)
	}
	if !errors.Is(err, runtime.ErrProcessTableScanUnavailable) {
		t.Fatalf("err = %v, want it to wrap runtime.ErrProcessTableScanUnavailable", err)
	}
	if n := sp.terms.Load(); n != 0 {
		t.Fatalf("TerminateRuntime calls = %d, want 0: a scan that never returned cannot say what to kill", n)
	}
	if n := sp.starts.Load(); n != 0 {
		t.Fatalf("Start calls = %d, want 0: the start is deferred", n)
	}
}

func TestManagerOrphanScanBudgetDefaultsToTenSeconds(t *testing.T) {
	mgr := newTestManager(t, beads.NewMemStore(), runtime.NewFake())
	if mgr.orphanScanBudget != 10*time.Second {
		t.Fatalf("orphanScanBudget = %s, want 10s", mgr.orphanScanBudget)
	}
}

func TestKillExistingOrphansDefersWhenScanNeverFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newWedgedScanProvider(t)
		defer sp.unwedge()
		logged := captureLog(t)
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
		mgr.orphanScanBudget = orphanScanBudgetForTest

		start := time.Now()
		err := mgr.killExistingOrphans(context.Background(), "sid-wedged")

		if elapsed := time.Since(start); elapsed != orphanScanBudgetForTest {
			t.Fatalf("returned after %s, want the scan budget %s", elapsed, orphanScanBudgetForTest)
		}
		assertDeferredByUnfinishedScan(t, sp, err)
		if want := "orphan scan for sid-wedged did not finish; deferring start"; !strings.Contains(logged.String(), want) {
			t.Fatalf("log = %q, want a visible line containing %q", logged.String(), want)
		}

		sp.unwedge()
		synctest.Wait()
	})
}

func TestKillExistingOrphansBudgetIsCappedByContextDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newWedgedScanProvider(t)
		defer sp.unwedge()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
		mgr.orphanScanBudget = orphanScanBudgetForTest

		const ctxBudget = 3 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), ctxBudget)
		defer cancel()

		start := time.Now()
		err := mgr.killExistingOrphans(ctx, "sid-short")

		if elapsed := time.Since(start); elapsed != ctxBudget {
			t.Fatalf("returned after %s, want the shorter context deadline %s", elapsed, ctxBudget)
		}
		assertDeferredByUnfinishedScan(t, sp, err)

		sp.unwedge()
		synctest.Wait()
	})
}

func TestKillExistingOrphansRepeatedStartsOfWedgedSessionScanOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newWedgedScanProvider(t)
		defer sp.unwedge()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
		mgr.orphanScanBudget = orphanScanBudgetForTest

		for i := 0; i < 3; i++ {
			assertDeferredByUnfinishedScan(t, sp, mgr.killExistingOrphans(context.Background(), "sid-repeat"))
		}
		if n := sp.finds.Load(); n != 1 {
			t.Fatalf("scans started = %d, want ONE across repeated attempts on a wedged session", n)
		}

		sp.unwedge()
		synctest.Wait()
	})
}

func TestKillExistingOrphansKeysLatchByCityAndSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedged := newWedgedScanProvider(t)
		defer wedged.unwedge()
		mgr := newTestManager(t, beads.NewMemStore(), wedged, WithCityPath(wedged.city))
		mgr.orphanScanBudget = orphanScanBudgetForTest
		assertDeferredByUnfinishedScan(t, wedged, mgr.killExistingOrphans(context.Background(), "sid-shared"))

		// The same session id in another city has its own latch: one city's
		// wedged scan must not defer another city's start.
		other := newWedgedScanProvider(t)
		other.wedge.Store(false)
		otherMgr := newTestManager(t, beads.NewMemStore(), other, WithCityPath(other.city))
		otherMgr.orphanScanBudget = orphanScanBudgetForTest
		if err := otherMgr.killExistingOrphans(context.Background(), "sid-shared"); err != nil {
			t.Fatalf("other city's scan = %v, want it unaffected by a latch held for another city", err)
		}

		wedged.unwedge()
		synctest.Wait()
	})
}

func TestStartDefersWhenOrphanScanNeverFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newWedgedScanProvider(t)
		defer sp.unwedge()
		mgr, info := seedSuspendedTargetWithProvider(t, sp)

		err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})

		assertDeferredByUnfinishedScan(t, sp, err)
		if !strings.Contains(err.Error(), "pre-start orphan cleanup") {
			t.Fatalf("err = %v, want the pre-start orphan cleanup context", err)
		}

		sp.unwedge()
		synctest.Wait()
	})
}

func TestStartRuntimeOnlyDefersWhenOrphanScanNeverFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newWedgedScanProvider(t)
		defer sp.unwedge()
		mgr, info := seedSuspendedTargetWithProvider(t, sp)

		by := runtimeOnlyActor(t, mgr, info.ID)
		err := mgr.StartRuntimeOnly(context.Background(), by, info.ID, mintUnder(t, mgr, by.Lease, info.ID), BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})

		assertDeferredByUnfinishedScan(t, sp, err)
		if !strings.Contains(err.Error(), "pre-start orphan cleanup") {
			t.Fatalf("err = %v, want the pre-start orphan cleanup context", err)
		}

		sp.unwedge()
		synctest.Wait()
	})
}

func TestCreateDefersAndRollsBackWhenOrphanScanNeverFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newWedgedScanProvider(t)
		defer sp.unwedge()
		store := beads.NewMemStore()
		mgr := newTestManager(t, store, sp, WithCityPath(sp.city))
		mgr.orphanScanBudget = orphanScanBudgetForTest

		_, err := mgr.CreateSession(context.Background(), createOptsForOrphanScan(t))

		assertDeferredByUnfinishedScan(t, sp, err)
		open, listErr := store.List(beads.ListQuery{Label: LabelSession})
		if listErr != nil {
			t.Fatalf("List: %v", listErr)
		}
		if len(open) != 0 {
			t.Fatalf("open session beads = %d, want the failed create rolled back", len(open))
		}

		sp.unwedge()
		synctest.Wait()
	})
}

// A scan that RETURNED with roots still gets today's treatment: the terminations
// run. Only a scan that never returned defers without killing.
func TestKillExistingOrphansStillTerminatesWhenScanReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newWedgedScanProvider(t)
		sp.wedge.Store(false)
		sp.roots = []runtime.LiveRuntime{{PID: 4101}}
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
		mgr.orphanScanBudget = orphanScanBudgetForTest

		if err := mgr.killExistingOrphans(context.Background(), "sid-ok"); err != nil {
			t.Fatalf("killExistingOrphans: %v", err)
		}
		if n := sp.terms.Load(); n != 1 {
			t.Fatalf("TerminateRuntime calls = %d, want 1", n)
		}
	})
}

// Termination of an in-flight root cannot be interrupted, but between roots the
// sweep checks the context: a start whose context is done stops terminating
// and reports the left-over roots as unavailable, not as a crash.
func TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone(t *testing.T) {
	sp := newWedgedScanProvider(t)
	sp.wedge.Store(false)
	sp.roots = []runtime.LiveRuntime{{PID: 4101}, {PID: 4102}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sp.onTerminate = func(runtime.LiveRuntime) { cancel() }
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))

	err := mgr.killExistingOrphans(ctx, "sid-two")

	if err == nil {
		t.Fatal("killExistingOrphans = nil, want an error for the root left unterminated")
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap ErrRuntimeUnavailable and context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "1 orphaned runtime(s) not terminated") {
		t.Fatalf("err = %v, want the count of roots left", err)
	}
	if n := sp.terms.Load(); n != 1 {
		t.Fatalf("TerminateRuntime calls = %d, want 1: the second root must not be terminated after the context is done", n)
	}
}

func TestSameCityDifferentSessionHasItsOwnLatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newWedgedScanProvider(t)
		defer sp.unwedge()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
		mgr.orphanScanBudget = orphanScanBudgetForTest
		assertDeferredByUnfinishedScan(t, sp, mgr.killExistingOrphans(context.Background(), "sid-first"))

		// sid-first's scan is still wedged. A different session in the SAME city
		// must get its own scan: the latch is per (city, session id), not per city.
		sp.wedge.Store(false)
		if err := mgr.killExistingOrphans(context.Background(), "sid-second"); err != nil {
			t.Fatalf("second session in the same city = %v, want its own scan, unaffected by the first session's wedged scan", err)
		}
		if n := sp.finds.Load(); n != 2 {
			t.Fatalf("scans started = %d, want 2 (one per session)", n)
		}

		sp.unwedge()
		synctest.Wait()
	})
}

func TestOrphanScanBudgetFieldIsHonored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newWedgedScanProvider(t)
		defer sp.unwedge()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
		const budget = 2 * time.Second
		mgr.orphanScanBudget = budget

		start := time.Now()
		err := mgr.killExistingOrphans(context.Background(), "sid-field")

		if elapsed := time.Since(start); elapsed != budget {
			t.Fatalf("returned after %s, want the manager's configured budget %s", elapsed, budget)
		}
		assertDeferredByUnfinishedScan(t, sp, err)

		sp.unwedge()
		synctest.Wait()
	})
}

// deathThenWedgeProvider fails its first Start with a startup death (so the
// stale-key retry runs) and from then on wedges the process-table scan.
type deathThenWedgeProvider struct {
	*wedgedScanProvider
	armed atomic.Bool
}

func (p *deathThenWedgeProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.armed.CompareAndSwap(true, false) {
		p.starts.Add(1)
		p.wedge.Store(true)
		return fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, name)
	}
	return p.wedgedScanProvider.Start(ctx, name, cfg)
}

func TestStaleKeyRetryDefersWhenOrphanScanNeverFinishes(t *testing.T) {
	wp := newWedgedScanProvider(t)
	defer wp.unwedge()
	wp.wedge.Store(false)
	sp := &deathThenWedgeProvider{wedgedScanProvider: wp}
	store := beads.NewMemStore()
	mgr := newTestManager(t, store, sp, WithCityPath(wp.city))
	mgr.orphanScanBudget = 100 * time.Millisecond

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Title: "", Command: "claude --dangerously", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{
		ResumeFlag:    "--resume",
		SessionIDFlag: "--session-id",
	}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	sessionKey := b.Metadata["session_key"]
	if sessionKey == "" {
		t.Fatal("expected session_key in bead metadata after Create with ResumeFlag")
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	wp.finds.Store(0)
	wp.terms.Store(0)
	wp.starts.Store(0)
	sp.armed.Store(true)

	resumeCmd := "claude --dangerously --resume " + sessionKey
	_, err = mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", resumeCmd, runtime.Config{WorkDir: info.WorkDir})

	if err == nil {
		t.Fatal("Send succeeded although the stale-key retry's orphan scan never finished")
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, runtime.ErrProcessTableScanUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrRuntimeUnavailable and ErrProcessTableScanUnavailable through the retry's wrap site", err)
	}
	if n := wp.terms.Load(); n != 0 {
		t.Fatalf("TerminateRuntime calls = %d, want 0", n)
	}
	if n := wp.starts.Load(); n != 1 {
		t.Fatalf("Start calls = %d, want 1 (the dying first start only; the retry is deferred)", n)
	}
}
