package session

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// wantOrphanScanBudget is how long a start waits for its pre-start orphan scan
// when its context allows longer. These tests run on synctest's fake clock.
const wantOrphanScanBudget = 10 * time.Second

// orphanScanWatchdog is far past wantOrphanScanBudget. It exists only so that an
// unbounded wait fails the test with a message instead of deadlocking the
// synctest bubble.
const orphanScanWatchdog = 5 * time.Minute

// gatedScanProvider is a Fake whose process-table scans are scripted per call.
// A scan for which wedged reports true (scans count from 1) behaves like a
// wedged /proc read or a stalled tmux server: it ignores everything until the
// test opens the gate, then answers with the roots answer returns for it.
// Everything a scan goroutine touches is atomic, guarded or set before the
// first scan, because an abandoned scan returns after its caller has moved on.
type gatedScanProvider struct {
	*runtime.Fake
	city   string
	wedged func(n int64) bool
	// answer is the roots scan n (counting from 1) returns, stamped with the
	// scanned session and the city; nil answers no roots.
	answer   func(n int64) []runtime.LiveRuntime
	scanErr  error
	startErr error // Start fails with it after counting, if set

	release chan struct{}
	once    sync.Once
	entered chan struct{} // receives once per scan that blocks on the gate

	scans  atomic.Int64
	starts atomic.Int64

	mu         sync.Mutex
	terminated []int
	terminate  func(live runtime.LiveRuntime) error
}

func newGatedScanProvider(city string, wedged func(n int64) bool) *gatedScanProvider {
	return &gatedScanProvider{
		Fake:    runtime.NewFake(),
		city:    city,
		wedged:  wedged,
		release: make(chan struct{}),
		entered: make(chan struct{}, 16),
	}
}

// Scan scripts for newGatedScanProvider.
func neverWedged(int64) bool  { return false }
func alwaysWedged(int64) bool { return true }
func wedgedFrom(first int64) func(int64) bool {
	return func(n int64) bool { return n >= first }
}

func (p *gatedScanProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	n := p.scans.Add(1)
	if p.wedged(n) {
		p.entered <- struct{}{}
		<-p.release
	}
	if p.answer == nil {
		return nil, p.scanErr
	}
	out := p.answer(n)
	for i := range out {
		if out[i].SessionID == "" {
			out[i].SessionID = id
		}
		if out[i].City == "" {
			out[i].City = p.city
		}
	}
	return out, p.scanErr
}

func (p *gatedScanProvider) TerminateRuntime(live runtime.LiveRuntime) error {
	p.mu.Lock()
	p.terminated = append(p.terminated, live.PID)
	hook := p.terminate
	p.mu.Unlock()
	if hook != nil {
		return hook(live)
	}
	return nil
}

func (p *gatedScanProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.starts.Add(1)
	if p.startErr != nil {
		return p.startErr
	}
	return p.Fake.Start(ctx, name, cfg)
}

// unblock opens the gate. It is idempotent so tests can both call it and defer
// it: the deferred call frees a wedged scan when a failed assertion ends the
// test early, which would otherwise deadlock the bubble.
func (p *gatedScanProvider) unblock() {
	p.once.Do(func() { close(p.release) })
}

func (p *gatedScanProvider) terminatedPIDs() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.terminated...)
}

// orphanAt is one untracked same-session root, the shape the pre-start kill
// terminates.
func orphanAt(pid int) []runtime.LiveRuntime {
	return []runtime.LiveRuntime{{PID: pid}}
}

// killWithWatchdog runs killExistingOrphans on its own goroutine and fails the
// test if it has not returned within orphanScanWatchdog of fake time.
func killWithWatchdog(ctx context.Context, t *testing.T, mgr *Manager, sessionID string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- mgr.killExistingOrphans(ctx, sessionID) }()
	select {
	case err := <-done:
		return err
	case <-time.After(orphanScanWatchdog):
		t.Fatalf("killExistingOrphans(%s) still blocked %v after it began: the pre-start orphan scan is unbounded", sessionID, orphanScanWatchdog)
		return nil
	}
}

func wantUnansweredScan(t *testing.T, err error, cause error) {
	t.Helper()
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("err = %v, want it to wrap runtime.ErrRuntimeUnavailable so the start defers", err)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to wrap %v", err, cause)
	}
}

// AC-1: a scan that never answers ends the start's wait at the 10-second
// budget, unavailable, and nothing it would have reported is terminated, not
// even once its late answer lands.
func TestKillExistingOrphansDefersWhenScanNeverFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, alwaysWedged)
		sp.answer = func(int64) []runtime.LiveRuntime { return orphanAt(100) }
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		start := time.Now()

		err := killWithWatchdog(context.Background(), t, mgr, "sid-wedged")

		wantUnansweredScan(t, err, context.DeadlineExceeded)
		if elapsed := time.Since(start); elapsed != wantOrphanScanBudget {
			t.Fatalf("returned after %v, want exactly the %v budget", elapsed, wantOrphanScanBudget)
		}
		sp.unblock()
		synctest.Wait()
		if got := sp.terminatedPIDs(); len(got) != 0 {
			t.Fatalf("terminated %v from an unfinished scan, want nothing", got)
		}
	})
}

// AC-1: a start whose own deadline comes before the budget ends the wait at
// that deadline.
func TestKillExistingOrphansBudgetIsCappedByContextDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, alwaysWedged)
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		const startDeadline = 2 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), startDeadline)
		defer cancel()
		start := time.Now()

		err := killWithWatchdog(ctx, t, mgr, "sid-wedged")

		wantUnansweredScan(t, err, context.DeadlineExceeded)
		if elapsed := time.Since(start); elapsed != startDeadline {
			t.Fatalf("returned after %v, want the start's own %v deadline", elapsed, startDeadline)
		}
	})
}

// AC-1: a start canceled while its scan is out stops waiting at once.
func TestKillExistingOrphansDefersWhenStartCanceledMidScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, alwaysWedged)
		sp.answer = func(int64) []runtime.LiveRuntime { return orphanAt(100) }
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-sp.entered
			time.Sleep(time.Second)
			cancel()
		}()
		start := time.Now()

		err := killWithWatchdog(ctx, t, mgr, "sid-wedged")

		wantUnansweredScan(t, err, context.Canceled)
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("returned after %v, want the moment of cancellation (1s)", elapsed)
		}
		sp.unblock()
		synctest.Wait()
		if got := sp.terminatedPIDs(); len(got) != 0 {
			t.Fatalf("terminated %v from an unfinished scan, want nothing", got)
		}
	})
}

// AC-1: a start whose context has already ended launches no scan.
func TestKillExistingOrphansWithEndedContextStartsNoScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, neverWedged)
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := killWithWatchdog(ctx, t, mgr, "sid-ended")

		wantUnansweredScan(t, err, context.Canceled)
		if got := sp.scans.Load(); got != 0 {
			t.Fatalf("%d scans launched under an ended context, want 0", got)
		}
	})
}

// AC-1: a scan that answers inside the budget is used: its orphan is
// terminated and the start proceeds.
func TestKillExistingOrphansUsesAnAnswerWithinTheBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, alwaysWedged)
		sp.answer = func(int64) []runtime.LiveRuntime { return orphanAt(100) }
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		go func() {
			<-sp.entered
			time.Sleep(wantOrphanScanBudget - time.Second)
			sp.unblock()
		}()

		err := killWithWatchdog(context.Background(), t, mgr, "sid-slow")
		if err != nil {
			t.Fatalf("killExistingOrphans = %v, want nil for a scan answered within the budget", err)
		}
		if got := sp.terminatedPIDs(); len(got) != 1 || got[0] != 100 {
			t.Fatalf("terminated %v, want the answered orphan 100", got)
		}
	})
}

// AC-2: while one scan of a city's session is out, a retry through the same
// Manager or a newly constructed one (the API and worker factory build
// Managers per request) starts no scan and answers unavailable at once, under
// any spelling of the same city path.
func TestKillExistingOrphansRetryWhileScanOutstandingStartsNoScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, alwaysWedged)
		defer sp.unblock()
		store := beads.NewMemStore()
		mgr := newTestManager(t, store, sp, WithCityPath(city))
		wantUnansweredScan(t, killWithWatchdog(context.Background(), t, mgr, "sid-wedged"), context.DeadlineExceeded)

		for _, tc := range []struct {
			name string
			mgr  *Manager
		}{
			{name: "same manager", mgr: mgr},
			{name: "new manager", mgr: newTestManager(t, store, sp, WithCityPath(city))},
			{name: "new manager, equivalent city path", mgr: newTestManager(t, store, sp, WithCityPath(filepath.Join(city, "sub", "..")+string(filepath.Separator)))},
		} {
			before := time.Now()

			err := killWithWatchdog(context.Background(), t, tc.mgr, "sid-wedged")

			wantUnansweredScan(t, err, runtime.ErrBoundedCallOutstanding)
			if elapsed := time.Since(before); elapsed != 0 {
				t.Fatalf("%s: the retry took %v, want an immediate answer", tc.name, elapsed)
			}
			if got := sp.scans.Load(); got != 1 {
				t.Fatalf("%s: %d scans launched, want only the outstanding one", tc.name, got)
			}
		}
	})
}

// AC-2: the latch is per city and session: a wedged scan of one session holds
// neither another session of its city nor the same session ID in another city.
func TestKillExistingOrphansKeysLatchByCityAndSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		wedged := newGatedScanProvider(city, alwaysWedged)
		defer wedged.unblock()
		store := beads.NewMemStore()
		wantUnansweredScan(t, killWithWatchdog(context.Background(), t, newTestManager(t, store, wedged, WithCityPath(city)), "sid-wedged"), context.DeadlineExceeded)

		otherCity := t.TempDir()
		for _, tc := range []struct {
			name, city, sessionID string
		}{
			{name: "other session, same city", city: city, sessionID: "sid-other"},
			{name: "same session, other city", city: otherCity, sessionID: "sid-wedged"},
		} {
			healthy := newGatedScanProvider(tc.city, neverWedged)
			healthy.answer = func(int64) []runtime.LiveRuntime { return orphanAt(300) }
			mgr := newTestManager(t, store, healthy, WithCityPath(tc.city))

			err := killWithWatchdog(context.Background(), t, mgr, tc.sessionID)
			if err != nil {
				t.Fatalf("%s: killExistingOrphans = %v, want an independent scan", tc.name, err)
			}
			if got := healthy.terminatedPIDs(); healthy.scans.Load() != 1 || len(got) != 1 || got[0] != 300 {
				t.Fatalf("%s: scans=%d terminated=%v, want one scan and its orphan 300 terminated", tc.name, healthy.scans.Load(), got)
			}
		}
	})
}

// AC-3: an abandoned scan's late answer is no one's: once it returns and frees
// its key, the next start scans afresh and terminates only what that fresh
// scan reports.
func TestKillExistingOrphansNeverActsOnAnAbandonedScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, func(n int64) bool { return n == 1 })
		sp.answer = func(n int64) []runtime.LiveRuntime {
			if n == 1 {
				return orphanAt(100) // the abandoned scan's late root A
			}
			return orphanAt(200) // the fresh scan's root B
		}
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		wantUnansweredScan(t, killWithWatchdog(context.Background(), t, mgr, "sid-late"), context.DeadlineExceeded)
		sp.unblock()
		synctest.Wait()

		err := killWithWatchdog(context.Background(), t, mgr, "sid-late")
		if err != nil {
			t.Fatalf("killExistingOrphans after the late return = %v, want a fresh scan's answer", err)
		}
		if got := sp.scans.Load(); got != 2 {
			t.Fatalf("%d scans, want the abandoned one and one fresh scan", got)
		}
		if got := sp.terminatedPIDs(); len(got) != 1 || got[0] != 200 {
			t.Fatalf("terminated %v, want only the fresh scan's root 200, never the abandoned scan's 100", got)
		}
	})
}

// AC-3: a scan that returns frees its key: the next start scans afresh.
func TestKillExistingOrphansFreesTheKeyWhenTheScanReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, neverWedged)
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		for i := 0; i < 3; i++ {
			if err := killWithWatchdog(context.Background(), t, mgr, "sid-seq"); err != nil {
				t.Fatalf("start %d: killExistingOrphans = %v, want nil", i, err)
			}
		}
		if got := sp.scans.Load(); got != 3 {
			t.Fatalf("%d scans for 3 sequential starts, want 3", got)
		}
	})
}

// AC-4: a completed scan keeps the existing fences and fail-closed handling:
// only the untracked roots of this session in this city that are not city
// infrastructure are terminated, a scan error still lets the roots it
// returned be terminated (it is a completed scan, not an unanswered one, even
// when it wraps runtime.ErrRuntimeUnavailable), a LiveUnlisted claim refuses
// with nothing terminated, and failed terminations are reported together.
func TestKillExistingOrphansCompletedScanKeepsFencesAndFailClosed(t *testing.T) {
	otherCity := t.TempDir()
	roots := []runtime.LiveRuntime{
		{PID: 100},                         // an orphan
		{PID: 101, IsTracked: true},        // the provider's own runtime
		{PID: 102, SessionID: "sid-other"}, // another session
		{PID: 103, City: otherCity},        // another city
		{PID: 104},                         // a second orphan
	}
	for _, tc := range []struct {
		name           string
		scanErr        error
		extra          []runtime.LiveRuntime
		terminateErr   error
		wantTerminated []int
		wantErr        func(error) bool
	}{
		{name: "clean scan", wantTerminated: []int{100, 104}, wantErr: func(err error) bool { return err == nil }},
		{name: "scan error", scanErr: errors.New("partial scan failed"), wantTerminated: []int{100, 104}, wantErr: func(err error) bool { return err == nil }},
		{name: "scan error wrapping unavailable", scanErr: errors.Join(errors.New("tmux list running"), runtime.ErrRuntimeUnavailable), wantTerminated: []int{100, 104}, wantErr: func(err error) bool { return err == nil }},
		{
			name: "live unlisted root", extra: []runtime.LiveRuntime{{PID: 105, LiveUnlisted: true}},
			wantErr: func(err error) bool {
				return errors.Is(err, runtime.ErrRuntimeUnavailable) && strings.Contains(err.Error(), "pid=105")
			},
		},
		{
			name: "terminations fail", terminateErr: errors.New("operation not permitted"), wantTerminated: []int{100, 104},
			wantErr: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "2 orphaned runtime(s) not confirmed dead") && !errors.Is(err, runtime.ErrRuntimeUnavailable)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			sp := newGatedScanProvider(city, neverWedged)
			sp.scanErr = tc.scanErr
			sp.answer = func(int64) []runtime.LiveRuntime {
				return append(append([]runtime.LiveRuntime(nil), roots...), tc.extra...)
			}
			if tc.terminateErr != nil {
				sp.terminate = func(runtime.LiveRuntime) error { return tc.terminateErr }
			}
			mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

			err := mgr.killExistingOrphans(context.Background(), "sid-done")

			if !tc.wantErr(err) {
				t.Fatalf("killExistingOrphans = %v, not the error this case expects", err)
			}
			if got := sp.terminatedPIDs(); intsString(got) != intsString(tc.wantTerminated) {
				t.Fatalf("terminated %v, want %v", got, tc.wantTerminated)
			}
		})
	}
}

// AC-4: a start whose context ends during cleanup starts no further
// termination, and reports the cancellation with the termination failures
// that came before it.
func TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone(t *testing.T) {
	city := t.TempDir()
	sp := newGatedScanProvider(city, neverWedged)
	sp.answer = func(int64) []runtime.LiveRuntime {
		return []runtime.LiveRuntime{{PID: 100}, {PID: 101}, {PID: 102}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	termErr := errors.New("operation not permitted")
	sp.terminate = func(runtime.LiveRuntime) error {
		cancel()
		return termErr
	}
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

	err := mgr.killExistingOrphans(ctx, "sid-cancel")

	if !errors.Is(err, context.Canceled) || !errors.Is(err, termErr) {
		t.Fatalf("killExistingOrphans = %v, want the cancellation and the earlier termination failure", err)
	}
	if got := sp.terminatedPIDs(); len(got) != 1 || got[0] != 100 {
		t.Fatalf("terminated %v, want only the termination begun before the cancellation (100)", got)
	}
}

func intsString(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

// cancelOnWedge returns a context the test cancels the moment a wedged scan of
// sp begins, so a gate's wait on that scan ends deterministically, without
// wall time; the scan itself stays wedged until the test ends.
func cancelOnWedge(t *testing.T, sp *gatedScanProvider) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(sp.unblock)
	go func() {
		select {
		case <-sp.entered:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}

// seedSuspendedGatedTarget creates a session whose first scan (CreateSession's)
// answers, and suspends it, so a later Start or StartRuntimeOnly takes the
// resume path and reaches the pre-start orphan gate on a wedged scan.
func seedSuspendedGatedTarget(t *testing.T) (*Manager, *gatedScanProvider, Info) {
	t.Helper()
	city := t.TempDir()
	sp := newGatedScanProvider(city, wedgedFrom(2))
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	return mgr, sp, info
}

// AC-5: at every gated start, a pre-start orphan scan that cannot answer
// starts no runtime, its runtime-unavailable error reaches the caller, and a
// direct create rolls back the row and explicit name it just wrote.
func TestPreStartOrphanScanDefersEveryGatedStart(t *testing.T) {
	t.Run("Start", func(t *testing.T) {
		mgr, sp, info := seedSuspendedGatedTarget(t)
		startsBefore := sp.starts.Load()
		ctx := cancelOnWedge(t, sp)

		err := mgr.Start(ctx, testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})

		wantGateDeferred(t, err, sp, 2, startsBefore)
	})

	t.Run("StartRuntimeOnly", func(t *testing.T) {
		mgr, sp, info := seedSuspendedGatedTarget(t)
		startsBefore := sp.starts.Load()
		by := runtimeOnlyActor(t, mgr, info.ID)
		inc := mintUnder(t, mgr, by.Lease, info.ID)
		ctx := cancelOnWedge(t, sp)

		err := mgr.StartRuntimeOnly(ctx, by, info.ID, inc, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})

		wantGateDeferred(t, err, sp, 2, startsBefore)
	})

	t.Run("stale-key fresh restart", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store, "session_key", "k-stale", "resume_flag", "--resume")
		city := t.TempDir()
		sp := newGatedScanProvider(city, wedgedFrom(2))
		sp.startErr = runtime.ErrSessionDiedDuringStartup
		mgr := newTestManager(t, store, sp, WithCityPath(city))
		ctx := cancelOnWedge(t, sp)

		err := mgr.Start(ctx, testActor(mgr, ActorOperator), b.ID, "claude --resume k-stale", runtime.Config{})

		// The resume's own Start (the one that died) precedes the fresh
		// restart's gate; nothing may start after it.
		wantGateDeferred(t, err, sp, 2, 1)
	})

	t.Run("CreateSession", func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, alwaysWedged)
		store := beads.NewMemStore()
		mgr := newTestManager(t, store, sp, WithCityPath(city))
		ctx := cancelOnWedge(t, sp)

		_, err := mgr.CreateSession(ctx, CreateOptions{ExplicitName: "sky", Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})

		wantGateDeferred(t, err, sp, 1, 0)
		rows, listErr := store.List(beads.ListQuery{Type: BeadType, IncludeClosed: true})
		if listErr != nil {
			t.Fatalf("listing rows: %v", listErr)
		}
		if len(rows) != 1 {
			t.Fatalf("%d rows after the refused create, want the one rolled-back row", len(rows))
		}
		if row := rows[0]; row.Status != "closed" || row.Metadata["session_name"] != "" || row.Metadata["session_name_explicit"] != "" {
			t.Fatalf("row status=%q session_name=%q session_name_explicit=%q, want it closed with its explicit name cleared", row.Status, row.Metadata["session_name"], row.Metadata["session_name_explicit"])
		}
		// The name is free again: a create of a new row (its own scan key)
		// takes it.
		mgr = newTestManager(t, store, newGatedScanProvider(city, neverWedged), WithCityPath(city))
		if _, err := mgr.CreateSession(context.Background(), CreateOptions{ExplicitName: "sky", Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}}); err != nil {
			t.Fatalf("re-creating the rolled-back explicit name: %v", err)
		}
	})
}

// wantGateDeferred checks a start refused at its pre-start orphan gate: the
// error wraps runtime.ErrRuntimeUnavailable, the gate's scan (number gateScan)
// was the last one launched, and no provider Start came after startsBefore.
func wantGateDeferred(t *testing.T, err error, sp *gatedScanProvider, gateScan, startsBefore int64) {
	t.Helper()
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !strings.Contains(err.Error(), "pre-start orphan") {
		t.Fatalf("err = %v, want the pre-start orphan gate's error wrapping runtime.ErrRuntimeUnavailable", err)
	}
	if got := sp.scans.Load(); got != gateScan {
		t.Fatalf("%d scans launched, want %d (the gate's)", got, gateScan)
	}
	if got := sp.starts.Load(); got != startsBefore {
		t.Fatalf("provider Start called %d times, want %d: nothing may start after an unanswered orphan scan", got, startsBefore)
	}
}
