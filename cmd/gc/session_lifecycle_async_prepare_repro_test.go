package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// syncPrepareHoldWindow is how long a synchronous planner call must stay
// inside a blocked preparation before TestSyncPlannedStartsPrepareOnThePlanner
// accepts that preparation runs on the planner goroutine.
const syncPrepareHoldWindow = 300 * time.Millisecond

// prepareBlockingStore blocks every Get made from inside
// prepareStartCandidateForCity until unblock is closed, and closes entered the
// first time preparation reaches the store.
type prepareBlockingStore struct {
	beads.Store
	entered   chan struct{}
	enterOnce sync.Once
	unblock   chan struct{}
}

func (s *prepareBlockingStore) Get(id string) (beads.Bead, error) {
	if inPrepare() {
		s.enterOnce.Do(func() { close(s.entered) })
		<-s.unblock
	}
	return s.Store.Get(id)
}

// TestAsyncPlannedStartsReturnWhilePrepareBlocked: in async start mode a slow
// session preparation runs off the reconciler tick, so executePlannedStartsTraced
// returns while preparation is still blocked.
func TestAsyncPlannedStartsReturnWhilePrepareBlocked(t *testing.T) {
	e := newCapacityEnv(t, false, "worker-a")
	c := e.pendingCreate(t, "worker-a", "", nil)
	store := &prepareBlockingStore{Store: e.store, entered: make(chan struct{}), unblock: make(chan struct{})}
	var unblockOnce sync.Once
	release := func() { unblockOnce.Do(func() { close(store.unblock) }) }
	defer release()

	var tracker asyncStartTracker
	opts := append([]startExecutionOption(nil), e.startOptions...)
	opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker))

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...)
	}()

	select {
	case <-store.entered:
	case <-time.After(hangBudget):
		release()
		t.Fatal("preparation never reached the store")
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		release()
		<-returned
		tracker.wait(hangBudget)
		t.Fatal("executePlannedStartsTraced held the reconciler tick while async preparation was blocked; want it to return and prepare in the async start goroutine")
	}
	release()
	if !tracker.wait(hangBudget) {
		t.Fatal("async starts did not finish within the hang budget")
	}
}

// While an async start's off-tick preparation holds its runtime name's lease,
// a second planner call defers that name rather than preparing it twice.
func TestAsyncPlannedStartsSecondCallDefersWhilePrepareHoldsTheLease(t *testing.T) {
	e := newCapacityEnv(t, false, "worker-a")
	c := e.pendingCreate(t, "worker-a", "", nil)
	writes := newWriteCountingStore(e.store)
	store := &prepareBlockingStore{Store: writes, entered: make(chan struct{}), unblock: make(chan struct{})}
	var unblockOnce sync.Once
	release := func() { unblockOnce.Do(func() { close(store.unblock) }) }
	defer release()

	var tracker asyncStartTracker
	opts := append([]startExecutionOption(nil), e.startOptions...)
	opts = append(opts, withAsyncStartExecution(), withAsyncStartTracker(&tracker))

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...)
	}()
	select {
	case <-store.entered:
	case <-time.After(hangBudget):
		release()
		t.Fatal("preparation never reached the store")
	}
	// The first call is back: its preparation runs off its tick, holding the
	// name's lease from admission through the commit.
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		release()
		<-returned
		tracker.wait(hangBudget)
		t.Fatal("first planner call still held its tick while async preparation was blocked")
	}
	before := writes.count(c.info.ID)
	logFrom := len(e.log.String())
	if woken := executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, opts...); woken != 0 {
		t.Fatalf("second planner call woken = %d, want 0 (the name is deferred by the runtime lease)", woken)
	}
	if !strings.Contains(e.log.String()[logFrom:], "outcome=deferred_by_runtime_lease") {
		t.Fatalf("second planner call did not defer the name by runtime lease:\n%s", e.log.String())
	}
	if got := writes.count(c.info.ID); got != before {
		t.Fatalf("second planner call wrote %d time(s) to %s, want none", got-before, c.info.ID)
	}
	release()
	if !tracker.wait(hangBudget) {
		t.Fatal("async starts did not finish within the hang budget")
	}
	if n := e.sp.CountCalls("Start", "worker-a"); n != 1 {
		t.Fatalf("provider Start calls = %d, want exactly 1\n%s", n, e.log.String())
	}
}

// writeCountingStore counts the writes made to each bead id.
type writeCountingStore struct {
	beads.Store
	mu     sync.Mutex
	writes map[string]int
}

func newWriteCountingStore(store beads.Store) *writeCountingStore {
	return &writeCountingStore{Store: store, writes: map[string]int{}}
}

func (s *writeCountingStore) count(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[id]
}

func (s *writeCountingStore) wrote(id string) {
	s.mu.Lock()
	s.writes[id]++
	s.mu.Unlock()
}

func (s *writeCountingStore) Update(id string, opts beads.UpdateOpts) error {
	s.wrote(id)
	return s.Store.Update(id, opts)
}

func (s *writeCountingStore) SetMetadata(id, key, value string) error {
	s.wrote(id)
	return s.Store.SetMetadata(id, key, value)
}

func (s *writeCountingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.wrote(id)
	return s.Store.SetMetadataBatch(id, kvs)
}

// While a dependency's admitted start is preparing off the tick — before its
// PreWake marker exists — dependents are blocked on it: the admission itself
// is in-flight work, whether or not the dependency's older runtime still
// reports alive. The block is visible in a later wave of the same call, in a
// later batch of the same wave, on the next planner call, and to a
// synchronous caller holding the registry read-only.
func TestAsyncPrepareDependentsWaitForAdmittedDependency(t *testing.T) {
	for _, olderRuntime := range []bool{false, true} {
		for _, site := range []string{"later_wave", "later_batch", "next_tick", "control_dispatcher"} {
			name := site
			if olderRuntime {
				name += "/older_runtime_alive"
			}
			t.Run(name, func(t *testing.T) {
				e := newCapacityEnv(t, false, "db", "app", "z1", "z2")
				maxWakes := 8
				e.cfg.Daemon.MaxWakesPerTick = &maxWakes
				for i := range e.cfg.Agents {
					if e.cfg.Agents[i].Name == "app" {
						e.cfg.Agents[i].DependsOn = []string{"db"}
					}
				}
				db := e.pendingCreate(t, "db", "", nil)
				// The fillers are created before app so the wake-fairness
				// order (creation time) keeps app out of db's batch.
				var z1, z2 startCandidate
				if site == "later_batch" {
					z1 = e.pendingCreate(t, "z1", "", nil)
					z2 = e.pendingCreate(t, "z2", "", nil)
				}
				app := e.pendingCreate(t, "app", "", nil)
				if olderRuntime {
					// The dependency's previous incarnation is still up while
					// its replacement start is admitted: liveness alone would
					// answer alive.
					if err := e.sp.Start(context.Background(), "db", runtime.Config{Command: "test-cmd"}); err != nil {
						t.Fatal(err)
					}
				}
				store := &prepareBlockingStore{Store: e.store, entered: make(chan struct{}), unblock: make(chan struct{})}
				var unblockOnce sync.Once
				release := func() { unblockOnce.Do(func() { close(store.unblock) }) }
				defer release()

				var tracker asyncStartTracker
				asyncOpts := append([]startExecutionOption(nil), e.startOptions...)
				asyncOpts = append(asyncOpts, withAsyncStartExecution(), withAsyncStartTracker(&tracker))

				// One planner call admits db (its goroutine blocks in prepare,
				// before PreWake) and must not admit app wherever app is
				// evaluated.
				candidates := []startCandidate{db, app}
				wantFirstWoken := 1
				if site == "later_batch" {
					candidates = []startCandidate{db, z1, z2, app}
					wantFirstWoken = 3
				}
				returned := make(chan int, 1)
				go func() {
					returned <- executePlannedStartsTraced(context.Background(), candidates, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, asyncOpts...)
				}()
				select {
				case <-store.entered:
				case <-time.After(hangBudget):
					release()
					t.Fatal("preparation never reached the store")
				}
				if woken := <-returned; woken != wantFirstWoken {
					t.Fatalf("first planner call woken = %d, want %d: app must wait for db's admitted start\n%s", woken, wantFirstWoken, e.log.String())
				}
				if got := e.log.String(); !strings.Contains(got, "session=app") || !strings.Contains(got, "outcome=blocked_on_dependencies") {
					t.Fatalf("app was not blocked on db's admitted start:\n%s", got)
				}

				appAgain := e.refreshed(app)
				if site == "next_tick" {
					if woken := executePlannedStartsTraced(context.Background(), []startCandidate{appAgain}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, asyncOpts...); woken != 0 {
						t.Fatalf("next planner call woken = %d, want 0 while db's start is in flight", woken)
					}
				}
				if site == "control_dispatcher" {
					syncOpts := append([]startExecutionOption(nil), e.startOptions...)
					syncOpts = append(syncOpts, withAdmittedStartLookup(tracker.startInFlight))
					if woken := executePlannedStartsTraced(context.Background(), []startCandidate{appAgain}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, syncOpts...); woken != 0 {
						t.Fatalf("synchronous planner call woken = %d, want 0 while db's start is in flight", woken)
					}
				}

				if n := e.sp.CountCalls("Start", "app"); n != 0 {
					t.Fatalf("app Start calls = %d, want 0 while db's start was in flight", n)
				}
				release()
				if !tracker.wait(hangBudget) {
					t.Fatal("async starts did not finish within the hang budget")
				}
				if n := e.sp.CountCalls("Start", "db"); n != 1 {
					t.Fatalf("db Start calls = %d, want 1", n)
				}

				// db's start has committed and db reports alive: the next pass
				// starts app.
				var finalTracker asyncStartTracker
				finalOpts := append([]startExecutionOption(nil), e.startOptions...)
				finalOpts = append(finalOpts, withAsyncStartExecution(), withAsyncStartTracker(&finalTracker))
				if woken := executePlannedStartsTraced(context.Background(), []startCandidate{e.refreshed(app)}, e.cfg, e.desiredState, e.sp, e.store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, finalOpts...); woken != 1 {
					t.Fatalf("final planner call woken = %d, want 1 once db is started and alive", woken)
				}
				if !finalTracker.wait(hangBudget) {
					t.Fatal("final async start did not finish within the hang budget")
				}
				if n := e.sp.CountCalls("Start", "app"); n != 1 {
					t.Fatalf("app Start calls = %d, want 1 after db committed", n)
				}
			})
		}
	}
}

// TestSyncPlannedStartsPrepareOnThePlanner: without async start execution —
// the control dispatcher's call and the one-shot gc start — preparation runs
// inline on the planner goroutine, before provider Start, and a prepare
// failure is handled on that goroutine before the call returns. This pins
// today's synchronous order (AC-8); these tests pass by design, since AC-1
// kept the synchronous pass inline.
func TestSyncPlannedStartsPrepareOnThePlanner(t *testing.T) {
	t.Run("blocked preparation holds the planner call", func(t *testing.T) {
		e := newCapacityEnv(t, false, "worker-a")
		c := e.pendingCreate(t, "worker-a", "", nil)
		store := &prepareBlockingStore{Store: e.store, entered: make(chan struct{}), unblock: make(chan struct{})}
		var unblockOnce sync.Once
		release := func() { unblockOnce.Do(func() { close(store.unblock) }) }
		defer release()

		returned := make(chan int, 1)
		go func() {
			// Sync mode is e.startOptions alone: no withAsyncStartExecution.
			returned <- executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)
		}()
		select {
		case <-store.entered:
		case <-time.After(hangBudget):
			release()
			t.Fatal("preparation never reached the store")
		}
		select {
		case woken := <-returned:
			t.Fatalf("planner call returned (woken=%d) while its preparation was still blocked", woken)
		case <-time.After(syncPrepareHoldWindow):
		}
		if n := e.sp.CountCalls("Start", "worker-a"); n != 0 {
			t.Fatalf("Start calls = %d while preparation is blocked, want 0 (prepare runs before Start)", n)
		}
		release()
		if woken := <-returned; woken != 1 {
			t.Fatalf("planner call woken = %d, want 1 after the preparation finished", woken)
		}
		if n := e.sp.CountCalls("Start", "worker-a"); n != 1 {
			t.Fatalf("Start calls = %d, want 1 after the preparation finished", n)
		}
	})
	t.Run("failed preparation is handled before the call returns", func(t *testing.T) {
		e := newCapacityEnv(t, false, "worker-a")
		c := e.pendingCreate(t, "worker-a", "", nil)
		store := &prepareFailingStore{Store: e.store, err: errors.New("store unreachable during prepare")}

		woken := executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, e.sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)

		if woken != 0 {
			t.Fatalf("planner call woken = %d, want 0 (the failed prepare started nothing)", woken)
		}
		if n := e.sp.CountCalls("Start", "worker-a"); n != 0 {
			t.Fatalf("Start calls = %d, want 0", n)
		}
		log := e.log.String()
		if strings.Contains(log, "outcome="+string(TraceOutcomeStartEnqueued)) {
			t.Fatalf("synchronous pass enqueued a start:\n%s", log)
		}
		if !strings.Contains(log, "session=worker-a") || !strings.Contains(log, "outcome=failed") {
			t.Fatalf("lifecycle log lacks the failed outcome line:\n%s", log)
		}
		if _, release, err := tryRuntimeLease(nil, e.city, "worker-a", "", 0); err != nil {
			t.Fatalf("runtime name worker-a is not takeable after the failed prepare: %v", err)
		} else {
			release()
		}
		failed := 0
		for _, r := range e.records(TraceSiteLifecycleStartRun) {
			if r.OutcomeCode == TraceOutcomeFailed && r.SessionName == "worker-a" {
				failed++
			}
		}
		if failed != 1 {
			t.Fatalf("failed reconciler.start.execute records = %d, want 1\n%s", failed, log)
		}
	})
}
