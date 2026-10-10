package session

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// leaseWatchProvider records, at each Start and Stop, whether the session's
// row recorded a runtime lease.
type leaseWatchProvider struct {
	*runtime.Fake
	store   beads.Store
	id      string
	starts  []string
	stops   []string
	watched []bool // whether each Start's ctx ends at the lease's SafeUntil
}

func (p *leaseWatchProvider) holder() string {
	b, _ := p.store.Get(p.id)
	return b.Metadata[RuntimeLeaseHolderKey]
}

func (p *leaseWatchProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.starts = append(p.starts, p.holder())
	_, deadline := ctx.Deadline()
	p.watched = append(p.watched, deadline)
	return p.Fake.Start(ctx, name, cfg)
}

func (p *leaseWatchProvider) Stop(name string) error {
	p.stops = append(p.stops, p.holder())
	return p.Fake.Stop(name)
}

// managerLeaseFixture is a created session whose runtime is down, and a
// Manager over a CAS store with a city runtime dir.
type managerLeaseFixture struct {
	mgr  *Manager
	sp   *leaseWatchProvider
	info Info
	city string
	f    leaseFixture
}

func newManagerLeaseFixture(t *testing.T) managerLeaseFixture {
	t.Helper()
	store := openLeaseStore(t, t.TempDir())
	sp := &leaseWatchProvider{Fake: runtime.NewFake(), store: store}
	info, err := NewManagerWithOptions(store, sp.Fake).CreateSession(context.Background(), CreateOptions{
		Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := sp.Fake.Stop(info.SessionName); err != nil {
		t.Fatal(err)
	}
	sp.id = info.ID
	city := t.TempDir()
	prev := operatorLeaseWait
	operatorLeaseWait = 600 * time.Millisecond
	t.Cleanup(func() { operatorLeaseWait = prev })
	return managerLeaseFixture{
		mgr:  NewManagerWithOptions(store, sp, WithCityPath(city)),
		sp:   sp,
		info: info,
		city: city,
		f:    leaseFixture{store: store, front: NewStore(beads.SessionStore{Store: store}), id: info.ID},
	}
}

func (m managerLeaseFixture) hold(t *testing.T) *RuntimeLease {
	t.Helper()
	l, err := TryRuntimeLease(m.f.front, RuntimeLeaseRequest{City: m.city, Name: m.info.SessionName, ID: m.info.ID, TTL: leaseTTL})
	if err != nil {
		t.Fatalf("holding the lease: %v", err)
	}
	return l
}

func (m managerLeaseFixture) start(ctx context.Context) error {
	return m.mgr.Start(ctx, m.info.ID, BuildResumeCommand(m.info), runtime.Config{WorkDir: m.info.WorkDir}, ResumeOperator)
}

// releaseWithinTheWait releases l in 200ms and, for the rest of the test, has
// an operator wait for a lease up to the package's hang budget: a release
// ends the wait at once, so here the bound is only a hang detector. The
// fixture's 600ms bound left a release about 200ms of slack (the wait's last
// attempt comes a 200ms poll before its bound), and on a loaded disk the
// record-clear commit a release makes before it drops the flock overran it
// (ga-0wsm18).
func releaseWithinTheWait(t *testing.T, l *RuntimeLease) {
	t.Helper()
	t.Cleanup(SetOperatorLeaseWaitForTest(goroutineHangBudget))
	time.AfterFunc(200*time.Millisecond, l.Release)
}

// TestManagerStartRunsUnderTheRuntimeLease: the provider Start runs while the
// row records the Manager's lease, which the start releases.
func TestManagerStartRunsUnderTheRuntimeLease(t *testing.T) {
	m := newManagerLeaseFixture(t)
	if err := m.start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(m.sp.starts) != 1 || m.sp.starts[0] == "" || !m.sp.watched[0] {
		t.Fatalf("lease holders seen at Start = %q (watched %v), want one held, watched lease", m.sp.starts, m.sp.watched)
	}
	if meta := m.f.meta(t); meta[RuntimeLeaseHolderKey] != "" || meta[RuntimeLeaseEpochKey] != "1" {
		t.Fatalf("record after Start = %v, want epoch 1 released", meta)
	}
}

// TestManagerStartWaitsForTheRuntimeLease (O3): a held lease makes Start wait
// outside the session mutation lock, then fail retryably without starting;
// a lease released within the wait lets it start.
func TestManagerStartWaitsForTheRuntimeLease(t *testing.T) {
	m := newManagerLeaseFixture(t)
	held := m.hold(t)
	began := time.Now()
	result := make(chan error, 1)
	go func() { result <- m.start(context.Background()) }()
	// Throughout the wait, the session mutation lock stays takeable.
	for time.Since(began) < 400*time.Millisecond {
		unlocked := make(chan struct{})
		go func() { _ = withSessionMutationLock(m.info.ID, func() error { return nil }); close(unlocked) }()
		select {
		case <-unlocked:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("Start waited for the lease under the session mutation lock")
		}
	}
	err := <-result
	if !errors.Is(err, ErrSessionStarting) || !errors.Is(err, ErrRuntimeLeaseBusy) {
		t.Fatalf("Start under a held lease = %v, want ErrSessionStarting", err)
	}
	if waited := time.Since(began); waited < 400*time.Millisecond || waited > 3*time.Second {
		t.Fatalf("Start waited %v, want about the 600ms operator wait", waited)
	}
	if len(m.sp.starts) != 0 {
		t.Fatalf("provider Start ran under another holder's lease: %q", m.sp.starts)
	}
	releaseWithinTheWait(t, held)
	if err := m.start(context.Background()); err != nil {
		t.Fatalf("Start over a lease released within the wait: %v", err)
	}
	if len(m.sp.starts) != 1 {
		t.Fatalf("provider Starts = %q, want one", m.sp.starts)
	}
}

// TestManagerStartUnderItsCallersLease: a caller holding the lease (D8's
// resume, an interrupt restart) passes it in ctx; the start takes no other.
func TestManagerStartUnderItsCallersLease(t *testing.T) {
	m := newManagerLeaseFixture(t)
	held := m.hold(t)
	defer held.Release()
	if err := m.start(ContextWithRuntimeLease(context.Background(), held)); err != nil {
		t.Fatalf("Start under its caller's lease: %v", err)
	}
	if len(m.sp.starts) != 1 || m.sp.starts[0] != held.holder || !m.sp.watched[0] {
		t.Fatalf("holders at Start = %q (watched %v), want the caller's %q, watched", m.sp.starts, m.sp.watched, held.holder)
	}
}

// TestManagerKillTakesTheRuntimeLease: Kill stops only under the lease, its
// own or its caller's.
func TestManagerKillTakesTheRuntimeLease(t *testing.T) {
	m := newManagerLeaseFixture(t)
	if err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	held := m.hold(t)
	if err := m.mgr.Kill(m.info.ID); !errors.Is(err, ErrSessionStarting) {
		t.Fatalf("Kill under another holder's lease = %v, want ErrSessionStarting", err)
	}
	if len(m.sp.stops) != 0 || !m.sp.IsRunning(m.info.SessionName) {
		t.Fatalf("Kill stopped the runtime under another holder's lease (stops %q)", m.sp.stops)
	}
	if err := m.mgr.KillContext(ContextWithRuntimeLease(context.Background(), held), m.info.ID); err != nil {
		t.Fatalf("KillContext under its caller's lease: %v", err)
	}
	held.Release()
	if err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waited := m.hold(t)
	releaseWithinTheWait(t, waited)
	if err := m.mgr.Kill(m.info.ID); err != nil {
		t.Fatalf("Kill over a lease released within the wait: %v", err)
	}
	if err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.mgr.Kill(m.info.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if len(m.sp.stops) != 3 || m.sp.stops[0] != held.holder || m.sp.stops[2] == "" || m.sp.stops[2] == held.holder {
		t.Fatalf("holders at Stop = %q, want the caller's lease, then Kill's own", m.sp.stops)
	}
}

// TestInterruptLeaseCarriesToTheRestart (D8D-5): an interrupt takes the lease
// before it sends anything, and its stop and restart borrow that one lease.
func TestInterruptLeaseCarriesToTheRestart(t *testing.T) {
	m := newManagerLeaseFixture(t)
	held := m.hold(t)
	if _, _, err := m.mgr.leaseForRestartLocked(context.Background(), m.info.ID, m.info.SessionName); !errors.Is(err, ErrSessionStarting) {
		t.Fatalf("interrupt under another holder's lease = %v, want refused", err)
	}
	held.Release()
	ctx, release, err := m.mgr.leaseForRestartLocked(context.Background(), m.info.ID, m.info.SessionName)
	if err != nil {
		t.Fatal(err)
	}
	again, noop, err := m.mgr.leaseRuntime(ctx, m.info.ID, m.info.SessionName, 0)
	if err != nil || again == nil || again.holder != m.f.meta(t)[RuntimeLeaseHolderKey] {
		t.Fatalf("the restart took %v, %v; want the interrupt's lease", again, err)
	}
	noop()
	if m.f.meta(t)[RuntimeLeaseHolderKey] == "" {
		t.Fatal("the restart's release ended the interrupt's lease")
	}
	if _, _, err := m.mgr.leaseRuntime(ctx, m.info.ID, "another-runtime", 0); err == nil {
		t.Fatal("a borrowed lease served another runtime")
	}
	if _, _, err := m.mgr.leaseRuntime(ctx, "another-session", m.info.SessionName, 0); err == nil {
		t.Fatal("a borrowed lease served another session")
	}
	release()
	if meta := m.f.meta(t); meta[RuntimeLeaseHolderKey] != "" {
		t.Fatalf("record after the interrupt = %v, want released", meta)
	}
}

// TestControllerKillNeverWaits (M1): under WithoutLeaseWait a busy lease is
// the bare ErrRuntimeLeaseBusy at once, not ErrSessionStarting after a wait.
func TestControllerKillNeverWaits(t *testing.T) {
	m := newManagerLeaseFixture(t)
	if err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	held := m.hold(t)
	defer held.Release()
	began := time.Now()
	err := m.mgr.KillContext(WithoutLeaseWait(context.Background()), m.info.ID)
	if !errors.Is(err, ErrRuntimeLeaseBusy) || errors.Is(err, ErrSessionStarting) || time.Since(began) > 300*time.Millisecond {
		t.Fatalf("controller kill under a held lease = %v after %v, want ErrRuntimeLeaseBusy at once", err, time.Since(began))
	}
	if len(m.sp.stops) != 0 {
		t.Fatalf("the controller kill stopped under another holder's lease: %q", m.sp.stops)
	}
}

// TestKillFallsBackToTheFlock: a kill whose row record cannot be taken for a
// reason other than busy (here the row's name moved) is not held hostage: it
// stops under the name's flock alone, which still excludes a holder of it.
func TestKillFallsBackToTheFlock(t *testing.T) {
	m := newManagerLeaseFixture(t)
	if err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	if _, err := m.mgr.leaseForStop(context.Background(), m.info.ID, "renamed", 0); err != nil {
		t.Fatalf("stop on a record it cannot take: %v", err)
	}
	if !strings.Contains(buf.String(), "under the name's flock alone") {
		t.Fatalf("log = %q, want the fallback named", buf.String())
	}
	flock, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: m.city, Name: m.info.SessionName})
	if err != nil {
		t.Fatal(err)
	}
	defer flock.Release()
	if _, err := m.mgr.leaseForStop(WithoutLeaseWait(context.Background()), m.info.ID, m.info.SessionName, 0); !errors.Is(err, ErrRuntimeLeaseBusy) {
		t.Fatalf("stop under a held flock = %v, want busy", err)
	}
}

// TestSuspendAndCloseTakeTheRuntimeLease: an operator's suspend and close
// stop only under the lease, and wait for it outside the mutation lock.
func TestSuspendAndCloseTakeTheRuntimeLease(t *testing.T) {
	for _, op := range []string{"suspend", "close"} {
		t.Run(op, func(t *testing.T) {
			m := newManagerLeaseFixture(t)
			if err := m.start(context.Background()); err != nil {
				t.Fatal(err)
			}
			do := func() error {
				if op == "suspend" {
					return m.mgr.Suspend(m.info.ID)
				}
				_, err := m.mgr.CloseDetailed(m.info.ID)
				return err
			}
			held := m.hold(t)
			if err := do(); !errors.Is(err, ErrSessionStarting) || len(m.sp.stops) != 0 {
				t.Fatalf("%s under a held lease = %v (stops %q), want refused", op, err, m.sp.stops)
			}
			held.Release()
			if err := do(); err != nil {
				t.Fatal(err)
			}
			if len(m.sp.stops) != 1 || m.sp.stops[0] == "" {
				t.Fatalf("holders at the %s's stop = %q, want its own lease", op, m.sp.stops)
			}
		})
	}
}

// TestLeasedClosesRefuseALostLease: the terminal close and the pending-create
// rollback close nothing once the lease was taken over.
func TestLeasedClosesRefuseALostLease(t *testing.T) {
	store := openLeaseStore(t, t.TempDir())
	created, info := seedRollbackPendingSession(t, store)
	front := NewStore(beads.SessionStore{Store: store})
	req := func(city string, now time.Time) RuntimeLeaseRequest {
		return RuntimeLeaseRequest{City: city, Name: "worker-1", ID: created.ID, TTL: leaseTTL, now: func() time.Time { return now }}
	}
	stale, err := TryRuntimeLease(front, req(t.TempDir(), leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	taker, err := TryRuntimeLease(front, req(t.TempDir(), leaseT0.Add(leaseTTL)))
	if err != nil {
		t.Fatal(err)
	}
	if closed, _, err := front.RollbackPendingCreateAtomicallyUnder(info, rollbackClosePatch(), nil, stale); closed || err != nil {
		t.Fatalf("rollback under a lost lease = %v, %v; want refused", closed, err)
	}
	if closed, err := front.CloseWithTerminalPatchUnder(info, ClosePatch(leaseT0, "drained"), "close", leaseT0, stale); closed || !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("close under a lost lease = %v, %v; want ErrRuntimeLeaseLost", closed, err)
	}
	if closed, _, err := front.RollbackPendingCreateAtomicallyUnder(info, rollbackClosePatch(), nil, taker); !closed || err != nil {
		t.Fatalf("rollback under the lease = %v, %v", closed, err)
	}
	if b, _ := store.Get(created.ID); b.Metadata[RuntimeLeaseHolderKey] != "" {
		t.Fatalf("the closed row keeps a lease record: %v", b.Metadata)
	}
}

// TestForceRuntimeLeaseOverridesAHungSharer: an operator's override takes a
// record whose holder still holds the flock but whose record expired, and
// logs it; an unexpired record stays busy.
func TestForceRuntimeLeaseOverridesAHungSharer(t *testing.T) {
	f, city := newLeaseFixture(t), t.TempDir()
	hung, err := TryRuntimeLease(f.front, f.req(city, leaseT0))
	if err != nil {
		t.Fatal(err)
	}
	defer hung.Release()
	if _, err := ForceRuntimeLease(f.front, f.req(city, leaseT0.Add(time.Minute))); !errors.Is(err, ErrRuntimeLeaseBusy) {
		t.Fatalf("override of an unexpired record = %v, want busy", err)
	}
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	l, err := ForceRuntimeLease(f.front, f.req(city, leaseT0.Add(leaseTTL)))
	if err != nil || l.Epoch() != 2 {
		t.Fatalf("override of an expired record: %v, %v", err, l)
	}
	if !strings.Contains(buf.String(), "operator override") {
		t.Fatalf("log = %q, want the override named", buf.String())
	}
	l.Release()
	if meta := f.meta(t); meta[RuntimeLeaseHolderKey] != "" || meta[RuntimeLeaseEpochKey] != "2" {
		t.Fatalf("record after the override = %v", meta)
	}
}

// TestCitySweepTakesNoLease: a stop sweep's kill (gc stop) runs past a held
// lease by design.
func TestCitySweepTakesNoLease(t *testing.T) {
	m := newManagerLeaseFixture(t)
	if err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	held := m.hold(t)
	defer held.Release()
	if err := m.mgr.KillContext(CitySweepContext(context.Background()), m.info.ID); err != nil {
		t.Fatalf("sweep kill under a held lease: %v", err)
	}
	if len(m.sp.stops) != 1 || m.sp.stops[0] != held.holder {
		t.Fatalf("holders at the sweep's stop = %q, want the other holder's record untouched", m.sp.stops)
	}
}

// TestInterruptTakesTheLeaseBeforeInterrupting: an interrupt-now submit on a
// running session is refused under another holder's lease before it sends
// any interrupt.
func TestInterruptTakesTheLeaseBeforeInterrupting(t *testing.T) {
	m := newManagerLeaseFixture(t)
	if err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	held := m.hold(t)
	defer held.Release()
	_, err := m.mgr.Submit(context.Background(), m.info.ID, "stop that", BuildResumeCommand(m.info), runtime.Config{WorkDir: m.info.WorkDir}, SubmitIntentInterruptNow, ResumeOperator)
	if !errors.Is(err, ErrSessionStarting) {
		t.Fatalf("interrupt under a held lease = %v, want ErrSessionStarting", err)
	}
	if n := m.sp.CountCalls("Interrupt", m.info.SessionName) + m.sp.CountCalls("SendKeys", m.info.SessionName); n != 0 {
		t.Fatalf("the refused interrupt sent %d interrupts", n)
	}
}

// TestInterruptRefusesAStopPendingRow: an interrupt-now submit on a row
// whose drain-ack stop is pending sends no interrupt and stops nothing.
func TestInterruptRefusesAStopPendingRow(t *testing.T) {
	m := newManagerLeaseFixture(t)
	if err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.f.store.SetMetadataBatch(m.info.ID, map[string]string{"state": string(StateDraining), "state_reason": DrainAckStopPendingReason}); err != nil {
		t.Fatal(err)
	}
	calls := func() int {
		return m.sp.CountCalls("Interrupt", m.info.SessionName) + m.sp.CountCalls("SendKeys", m.info.SessionName) + m.sp.CountCalls("Stop", m.info.SessionName)
	}
	before := calls()
	_, err := m.mgr.Submit(context.Background(), m.info.ID, "stop that", BuildResumeCommand(m.info), runtime.Config{WorkDir: m.info.WorkDir}, SubmitIntentInterruptNow, ResumeOperator)
	if !errors.Is(err, ErrSessionStopping) {
		t.Fatalf("interrupt of a stop-pending row = %v, want ErrSessionStopping", err)
	}
	if n := calls() - before; n != 0 {
		t.Fatalf("the refused interrupt made %d interrupt or stop calls", n)
	}
}

// TestLeaselessManagerFailsAtUse: a Manager with no city path refuses to
// start or stop a runtime, except in a stop sweep.
func TestLeaselessManagerFailsAtUse(t *testing.T) {
	leaselessManagersAllowed.Store(false)
	defer leaselessManagersAllowed.Store(true)
	m := newManagerLeaseFixture(t)
	mgr := NewManagerWithOptions(m.f.store, m.sp)
	if err := mgr.Start(context.Background(), m.info.ID, BuildResumeCommand(m.info), runtime.Config{WorkDir: m.info.WorkDir}, ResumeOperator); !errors.Is(err, ErrRuntimeLeaseNoCity) {
		t.Fatalf("start without a city = %v, want ErrRuntimeLeaseNoCity", err)
	}
	if err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Kill(m.info.ID); !errors.Is(err, ErrRuntimeLeaseNoCity) {
		t.Fatalf("kill without a city = %v, want ErrRuntimeLeaseNoCity", err)
	}
	if err := mgr.KillContext(CitySweepContext(context.Background()), m.info.ID); err != nil {
		t.Fatalf("a sweep's kill without a city: %v", err)
	}
}

// TestControllerSuspendNeverWaits: a suspend under WithoutLeaseWait (the chat
// auto-suspend tick) neither waits nor retries: busy at once.
func TestControllerSuspendNeverWaits(t *testing.T) {
	m := newManagerLeaseFixture(t)
	if err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	held := m.hold(t)
	defer held.Release()
	began := time.Now()
	err := m.mgr.SuspendContext(WithoutLeaseWait(context.Background()), m.info.ID)
	if !errors.Is(err, ErrRuntimeLeaseBusy) || errors.Is(err, ErrSessionStarting) || time.Since(began) > 300*time.Millisecond {
		t.Fatalf("controller suspend under a held lease = %v after %v, want busy at once", err, time.Since(began))
	}
	if len(m.sp.stops) != 0 {
		t.Fatalf("the refused suspend stopped: %q", m.sp.stops)
	}
}

// TestControllerStartNeverWaits: a start under WithoutLeaseWait is refused at
// once, without the operator's retry loop.
func TestControllerStartNeverWaits(t *testing.T) {
	m := newManagerLeaseFixture(t)
	held := m.hold(t)
	defer held.Release()
	began := time.Now()
	err := m.start(WithoutLeaseWait(context.Background()))
	if !errors.Is(err, ErrRuntimeLeaseBusy) || errors.Is(err, ErrSessionStarting) || time.Since(began) > 300*time.Millisecond {
		t.Fatalf("controller start under a held lease = %v after %v, want busy at once", err, time.Since(began))
	}
}

// TestSameKillFacts: a controller kill's premise holds only while the row
// keeps its incarnation, its hold, and, when it was decided dormant, its
// dormancy.
func TestSameKillFacts(t *testing.T) {
	base := Info{Generation: "2", InstanceToken: "tok", MetadataState: string(StateAsleep)}
	for _, c := range []struct {
		name  string
		move  func(*Info)
		holds bool
	}{
		{"unmoved", func(*Info) {}, true},
		{"closed", func(i *Info) { i.Closed = true }, false},
		{"new generation", func(i *Info) { i.Generation = "3" }, false},
		{"new token", func(i *Info) { i.InstanceToken = "tok-2" }, false},
		{"hold set", func(i *Info) { i.HeldUntil = "2099-01-01T00:00:00Z" }, false},
		{"sleep intent", func(i *Info) { i.SleepIntent = "operator-hold" }, false},
		{"dormant woken", func(i *Info) { i.MetadataState = string(StateActive) }, false},
		{"dormant to dormant", func(i *Info) { i.MetadataState = string(StateSuspended) }, true},
	} {
		fresh := base
		c.move(&fresh)
		if got := SameKillFacts(base, fresh); got != c.holds {
			t.Errorf("%s: SameKillFacts = %v, want %v", c.name, got, c.holds)
		}
	}
	live := base
	live.MetadataState = string(StateActive)
	draining := live
	draining.MetadataState = string(StateDraining)
	if !SameKillFacts(live, draining) {
		t.Error("a live row the controller moved to draining must keep its premise")
	}
}
