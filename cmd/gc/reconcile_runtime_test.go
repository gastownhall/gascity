package main

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workqueue"
)

// Every runtime test runs inside a testing/synctest bubble: the queue's
// timers, the paced lanes, the backoff and the controllers' simulated work
// share the bubble's virtual clock, and synctest.Wait returns only once every
// worker and lane is blocked again, so each assertion reads settled state.
// The runtime is built over a fake v2Host (a MemStore sessions census) with
// recording controllers; jitter is pinned to zero (rand = 0.5).

// v2Call is one session reconcile a recording controller saw.
type v2Call struct {
	key      string
	lane     workqueue.Lane
	kinds    []string
	failures int
	gen      uint64
	rev      string
}

// v2Recorder is a pair of recording controllers. session and allocator, when
// set, decide each call's outcome.
type v2Recorder struct {
	mu        sync.Mutex
	calls     []v2Call
	allocs    [][]string
	session   func(it workqueue.Item[rowKey], env *reconcileEnv) (time.Duration, error)
	allocator func() error
}

func reasonKinds(rs []workqueue.Reason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Kind
	}
	return out
}

func (rec *v2Recorder) controllers() v2Controllers {
	return v2Controllers{
		session: func(_ context.Context, env *reconcileEnv, it workqueue.Item[rowKey]) (time.Duration, error) {
			rec.mu.Lock()
			rec.calls = append(rec.calls, v2Call{key: it.Key.ID, lane: it.Lane, kinds: reasonKinds(it.Reasons), failures: it.Failures, gen: env.Gen, rev: env.ConfigRev})
			fn := rec.session
			rec.mu.Unlock()
			if fn == nil {
				return 0, nil
			}
			return fn(it, env)
		},
		allocator: func(_ context.Context, _ *reconcileEnv, reasons []workqueue.Reason) error {
			rec.mu.Lock()
			rec.allocs = append(rec.allocs, reasonKinds(reasons))
			fn := rec.allocator
			rec.mu.Unlock()
			if fn == nil {
				return nil
			}
			return fn()
		},
	}
}

func (rec *v2Recorder) setSession(fn func(it workqueue.Item[rowKey], env *reconcileEnv) (time.Duration, error)) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.session = fn
}

func (rec *v2Recorder) setAllocator(fn func() error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.allocator = fn
}

// callsFor returns the reconciles of key, in order.
func (rec *v2Recorder) callsFor(key string) []v2Call {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []v2Call
	for _, c := range rec.calls {
		if c.key == key {
			out = append(out, c)
		}
	}
	return out
}

// keys returns the sorted keys reconciled so far, one entry per reconcile.
func (rec *v2Recorder) keys() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]string, len(rec.calls))
	for i, c := range rec.calls {
		out[i] = c.key
	}
	slices.Sort(out)
	return out
}

func (rec *v2Recorder) allocatorPasses() [][]string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.allocs)
}

// v2Harness is a runtime over a fake host.
type v2Harness struct {
	t     *testing.T
	rt    *v2Runtime
	rec   *v2Recorder
	store beads.Store
	rigs  map[string]beads.Store

	mu        sync.Mutex
	rev       string
	censusN   int                                   // sessions census reads
	censusErr []error                               // returned by the next sessions census reads, in order
	envReads  int                                   // host.snapshotEnv calls
	legs      func() ([]classStoreCandidate, error) // the census legs, when set; else none
	hook      func(prev, next *ObservationSnapshot)
	hookDelay time.Duration // how long setInventoryHook takes to install
	triggers  []string      // safeTick triggers that panicked
	panics    atomic.Int64
	retries   int // host.retryReload calls
}

func v2TestRow(id string) beads.Bead {
	return routerSessionBead(id, map[string]string{"session_name": "worker-" + id})
}

// newV2Harness builds a runtime over store. ctrl, when set, supplies the
// controllers; otherwise h.rec records.
func newV2Harness(t *testing.T, store beads.Store, ctrl func(*v2Metrics) v2Controllers) *v2Harness {
	t.Helper()
	h := &v2Harness{t: t, rec: &v2Recorder{}, store: store, rev: "rev-1"}
	cfg := &config.City{Daemon: config.DaemonConfig{PatrolInterval: "10s"}}
	sp := runtime.NewFake()
	census := memSessionCensus(store)
	host := v2Host{
		sessionsLeg: routerTestLeg,
		sessions: func() ([]session.Info, error) {
			h.mu.Lock()
			h.censusN++
			var err error
			if len(h.censusErr) > 0 {
				err, h.censusErr = h.censusErr[0], h.censusErr[1:]
			}
			h.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return census()
		},
		censusLegs: func() ([]classStoreCandidate, error) {
			h.mu.Lock()
			fn := h.legs
			h.mu.Unlock()
			if fn == nil {
				return nil, nil
			}
			return fn()
		},
		snapshotEnv: func() (*config.City, runtime.Provider, string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.envReads++
			return cfg, sp, h.rev
		},
		setInventoryHook: func(fn func(prev, next *ObservationSnapshot)) {
			h.mu.Lock()
			delay := h.hookDelay
			h.mu.Unlock()
			if delay > 0 {
				<-time.After(delay) // whatever is already running runs meanwhile
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			h.hook = fn
		},
		cityStore: func() beads.Store {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.store
		},
		rigStores: func() map[string]beads.Store {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.rigs
		},
		retryReload: func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.retries++
		},
		beginTrace: func(string) *sessionReconcilerTraceCycle { return nil },
		safeTick: func(fn func(), trigger string) (panicked bool) {
			defer func() {
				if recover() != nil {
					panicked = true
					h.panics.Add(1)
					h.mu.Lock()
					h.triggers = append(h.triggers, trigger)
					h.mu.Unlock()
				}
			}()
			fn()
			return false
		},
	}
	controllers := h.rec.controllers()
	metrics := newV2Metrics()
	if ctrl != nil {
		controllers = ctrl(metrics)
	}
	h.rt = newV2Runtime(host, controllers, metrics)
	h.rt.rand = func() float64 { return 0.5 } // no jitter
	t.Cleanup(h.rt.stop)
	return h
}

func newV2HarnessWithRows(t *testing.T, ids ...string) *v2Harness {
	t.Helper()
	rows := make([]beads.Bead, len(ids))
	for i, id := range ids {
		rows[i] = v2TestRow(id)
	}
	return newV2Harness(t, beads.NewMemStoreFrom(0, rows, nil), nil)
}

func (h *v2Harness) boot(t *testing.T) {
	t.Helper()
	if err := h.rt.boot(context.Background(), nil); err != nil {
		t.Fatalf("boot: %v", err)
	}
	synctest.Wait()
}

// bootAsync boots in the background; the channel yields boot's result.
func (h *v2Harness) bootAsync() <-chan error {
	done := make(chan error, 1)
	go func() { done <- h.rt.boot(context.Background(), nil) }()
	return done
}

func (h *v2Harness) add(id string, r workqueue.Reason) {
	h.rt.sessions.Add(rowKey{Leg: routerTestLeg, ID: id}, workqueue.LaneHot, r)
}

// flip commits an inventory pass that changes name, through the installed
// hook. Like the inventory lane, it routes nothing while no hook is set.
func (h *v2Harness) flip(name string) {
	h.mu.Lock()
	hook := h.hook
	h.mu.Unlock()
	if hook != nil {
		hook(&ObservationSnapshot{}, &ObservationSnapshot{PassSeq: 1, ByName: map[string]RuntimeObservation{name: {}}})
	}
}

func (h *v2Harness) set(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn()
}

func (h *v2Harness) censusReads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.censusN
}

func ready(done <-chan error) (bool, error) {
	select {
	case err := <-done:
		return true, err
	default:
		return false, nil
	}
}

func advance(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

// Kills: no recover around the reconcile (the bubble panics); no requeue (the
// key is lost); requeue without backoff (it runs again at once).
func TestV2WorkerPanicIsRecoveredAndKeyRequeuedWithBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		var first atomic.Bool
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			if !first.Swap(true) {
				panic("reconcile exploded")
			}
			return 0, nil
		})
		h.boot(t)

		if got := len(h.rec.callsFor("s-a")); got != 1 || h.panics.Load() != 1 {
			t.Fatalf("after the panic: %d reconciles, %d panics, want 1 and 1", got, h.panics.Load())
		}
		if st := h.rt.stats(); st.Queue.Deferred != 1 || st.Metrics.Panics != 1 {
			t.Fatalf("stats = deferred %d, panics %d, want the key held for its backoff and 1 panic", st.Queue.Deferred, st.Metrics.Panics)
		}
		advance(499 * time.Millisecond)
		if got := len(h.rec.callsFor("s-a")); got != 1 {
			t.Fatalf("reconciles before the 500ms backoff = %d, want 1", got)
		}
		advance(time.Millisecond)
		calls := h.rec.callsFor("s-a")
		if len(calls) != 2 || !slices.Contains(calls[1].kinds, v2ReasonPanic) || calls[1].failures != 1 {
			t.Fatalf("calls = %+v, want a retry with reason %q and 1 failure at 500ms", calls, v2ReasonPanic)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(h.triggers) != 1 || !strings.Contains(h.triggers[0], routerTestLeg+"/s-a") {
			t.Fatalf("panic triggers = %q, want one naming the key", h.triggers)
		}
	})
}

// Kills: the outcome reported after Done (M01b). A failing reconcile that
// re-adds its own key mid-run is dirty at Done; the backoff gate, closed
// before Done, still holds the requeue for 500ms.
func TestV2FailedReconcileDirtyMidRunStillWaitsForBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		var n atomic.Int32
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			if n.Add(1) == 1 {
				h.add("s-a", workqueue.Reason{Kind: routeReasonReplay})
				return 0, errors.New("failed")
			}
			return 0, nil
		})
		h.boot(t)
		advance(499 * time.Millisecond)
		if got := len(h.rec.callsFor("s-a")); got != 1 {
			t.Fatalf("reconciles before the 500ms backoff = %d, want 1", got)
		}
		advance(time.Millisecond)
		calls := h.rec.callsFor("s-a")
		if len(calls) != 2 || !slices.Contains(calls[1].kinds, routeReasonReplay) || calls[1].failures != 1 {
			t.Fatalf("calls = %+v, want the replay retried with 1 failure at 500ms", calls)
		}
	})
}

// Kills: a retry's backoff counted as enqueue-to-start latency. Only a first
// attempt samples it; the retry, whose deferred add waited 400ms behind the
// gate, does not.
func TestV2LatencySampledOnlyOnFirstAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		var n atomic.Int32
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			if n.Add(1) == 1 {
				return 0, errors.New("failed")
			}
			return 0, nil
		})
		h.boot(t)
		advance(100 * time.Millisecond)
		h.add("s-a", workqueue.Reason{Kind: "event"}) // held behind the gate until 500ms
		advance(400 * time.Millisecond)
		if calls := h.rec.callsFor("s-a"); len(calls) != 2 || calls[1].failures != 1 {
			t.Fatalf("calls = %+v, want the retry at 500ms", calls)
		}
		if m := h.rt.stats().Metrics; m.Reconciles != 2 || m.LatencyP99 != 0 {
			t.Fatalf("%d reconciles, latency p99 %s, want 2 and 0 (the retry not sampled)", m.Reconciles, m.LatencyP99)
		}
	})
}

// Kills: Forget missing on success. Errors back off 500ms, then 1s; a success
// resets the failure count, so the next error backs off 500ms again.
func TestV2WorkerErrorBacksOffSuccessForgets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		outcomes := []error{errors.New("e1"), errors.New("e2"), nil, errors.New("e3")}
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			err := outcomes[0]
			if len(outcomes) > 1 {
				outcomes = outcomes[1:]
			}
			return 0, err
		})
		h.boot(t)
		for _, step := range []struct {
			wait  time.Duration
			calls int
		}{{499 * time.Millisecond, 1}, {time.Millisecond, 2}, {999 * time.Millisecond, 2}, {time.Millisecond, 3}} {
			advance(step.wait)
			if got := len(h.rec.callsFor("s-a")); got != step.calls {
				t.Fatalf("reconciles = %d, want %d", got, step.calls)
			}
		}

		h.add("s-a", workqueue.Reason{Kind: "event"})
		synctest.Wait()
		calls := h.rec.callsFor("s-a")
		if len(calls) != 4 || calls[3].failures != 0 {
			t.Fatalf("calls = %+v, want a fourth reconcile at once with 0 failures after the success", calls)
		}
		advance(499 * time.Millisecond)
		if got := len(h.rec.callsFor("s-a")); got != 4 {
			t.Fatalf("reconciles before the reset 500ms backoff = %d, want 4", got)
		}
		advance(time.Millisecond)
		if got := len(h.rec.callsFor("s-a")); got != 5 {
			t.Fatalf("reconciles after the reset 500ms backoff = %d, want 5", got)
		}
	})
}

// Kills: requeueAfter ignored. A successful reconcile asking to be requeued
// runs again on the hot lane at that deadline (C1.3).
func TestV2WorkerRequeueAfterArmsHotTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		var n atomic.Int32
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			if n.Add(1) == 1 {
				return 10 * time.Second, nil
			}
			return 0, nil
		})
		h.boot(t)
		advance(10*time.Second - time.Millisecond)
		if got := len(h.rec.callsFor("s-a")); got != 1 {
			t.Fatalf("reconciles before requeueAfter = %d, want 1", got)
		}
		advance(time.Millisecond)
		calls := h.rec.callsFor("s-a")
		if len(calls) != 2 || calls[1].lane != workqueue.LaneHot || !reflect.DeepEqual(calls[1].kinds, []string{v2ReasonRequeue}) {
			t.Fatalf("calls = %+v, want a hot requeue at 10s", calls)
		}
	})
}

// Kills: workers reading host.snapshotEnv (F2). A reconcile sees the env the
// runtime last published, not the host's current config.
func TestV2WorkersReadOnlyPublishedEnv(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		h.boot(t)
		h.mu.Lock()
		h.rev = "rev-2" // a reload applied but not yet published
		h.mu.Unlock()
		h.add("s-a", workqueue.Reason{Kind: "event"})
		synctest.Wait()
		h.rt.publishEnv()
		h.add("s-a", workqueue.Reason{Kind: "event"})
		synctest.Wait()

		var got []string
		for _, c := range h.rec.callsFor("s-a") {
			got = append(got, c.rev)
		}
		wantStrings(t, "revisions seen by reconciles", got, []string{"rev-1", "rev-1", "rev-2"})
		h.mu.Lock()
		reads := h.envReads
		h.mu.Unlock()
		if reads != 2 {
			t.Fatalf("host.snapshotEnv calls = %d, want 2 (one per publish)", reads)
		}
	})
}

// Kills: publishEnv editing the published env in place. A reconcile holding
// Gen 1 keeps reading Gen 1 while a later generation is published.
func TestV2PublishedEnvIsNeverMutated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		held := make(chan *reconcileEnv, 1)
		release := make(chan struct{})
		h.rec.setSession(func(_ workqueue.Item[rowKey], env *reconcileEnv) (time.Duration, error) {
			held <- env
			<-release
			return 0, nil
		})
		h.add("s-a", workqueue.Reason{Kind: "event"})
		env := <-held
		before := *env
		h.mu.Lock()
		h.rev = "rev-2"
		h.mu.Unlock()
		next := h.rt.publishEnv()
		if *env != before || env.Gen != 1 || env.ConfigRev != "rev-1" {
			t.Fatalf("held env changed to %+v during publish, want %+v", *env, before)
		}
		if next == env || next.Gen != 2 || next.ConfigRev != "rev-2" || h.rt.env.Load() != next {
			t.Fatalf("published env = %+v, want a new Gen 2 at rev-2", next)
		}
		close(release)
		synctest.Wait()
	})
}

// Kills: the allocator lane losing its pacing (back-to-back passes) or its
// coalescing. Wakes during a pass fold into one next pass, which waits until
// the lane has idled as long as the pass ran (C1.6, target 4).
func TestV2AllocatorLaneCoalescesAndCapsDutyCycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		advance(time.Second) // past the boot pass's minimum gap
		type pass struct{ start, end time.Time }
		var mu sync.Mutex
		var passes []pass
		h.rec.setAllocator(func() error {
			start := time.Now()
			<-time.After(2 * time.Second)
			mu.Lock()
			passes = append(passes, pass{start, time.Now()})
			mu.Unlock()
			return nil
		})
		h.rt.alloc.wake(workqueue.Reason{Kind: "event"})
		synctest.Wait() // the pass is running
		for _, kind := range []string{"a", "b", "c", "d", "e"} {
			h.rt.alloc.wake(workqueue.Reason{Kind: kind})
		}
		advance(8 * time.Second) // both passes, and the idle the duty rule owes the second

		mu.Lock()
		defer mu.Unlock()
		if len(passes) != 2 {
			t.Fatalf("passes = %d, want 2: the wakes during the first pass fold into one", len(passes))
		}
		if gap := passes[1].start.Sub(passes[0].end); gap < 2*time.Second {
			t.Fatalf("idle before the second pass = %s, want at least the 2s the first pass ran", gap)
		}
		allocs := h.rec.allocatorPasses()
		wantStrings(t, "folded reasons", allocs[len(allocs)-1], []string{"a", "b", "c", "d", "e"})
		if duty := h.rt.stats().Metrics.AllocatorDuty; duty > 0.5 {
			t.Fatalf("allocator duty = %.2f, want at most 0.5", duty)
		}
	})
}

// Kills: the allocator retried at wake rate. After a failed pass, non-urgent
// wakes wait for the jittered retry (1s, then 2s); an urgent wake bypasses
// the gate (A2).
func TestV2AllocatorErrorBacksOffAndDefersNonUrgentWakes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		advance(time.Second) // past the boot pass's minimum gap
		h.rec.setAllocator(func() error { return errors.New("allocator failed") })
		passes := func() int { return len(h.rec.allocatorPasses()) }
		base := passes()

		h.rt.alloc.wake(workqueue.Reason{Kind: "event"}) // fails: gate until +1s
		synctest.Wait()
		for range 3 {
			advance(200 * time.Millisecond)
			h.rt.alloc.wake(workqueue.Reason{Kind: "replay"})
			synctest.Wait()
		}
		if got := passes() - base; got != 1 {
			t.Fatalf("passes within the 1s backoff = %d, want 1", got)
		}
		advance(400 * time.Millisecond) // +1s: the retry, which fails again (gate until +3s)
		if got := passes() - base; got != 2 {
			t.Fatalf("passes at the 1s retry = %d, want 2", got)
		}
		if last := h.rec.allocatorPasses()[passes()-1]; !slices.Contains(last, v2ReasonRetry) || !slices.Contains(last, "replay") {
			t.Fatalf("retry pass reasons = %q, want the deferred replay and %q", last, v2ReasonRetry)
		}
		advance(500 * time.Millisecond)
		h.rt.alloc.wake(workqueue.Reason{Kind: "api", Urgent: true})
		synctest.Wait()
		if got := passes() - base; got != 3 {
			t.Fatalf("passes after an urgent wake inside the backoff = %d, want 3", got)
		}
		if st := h.rt.stats().Metrics; st.AllocatorFailures != 3 {
			t.Fatalf("allocator failures = %d, want 3", st.AllocatorFailures)
		}
	})
}

// Kills: readiness at enqueue time; allocator priming ignored. Boot returns
// only once every boot key has finished a reconcile and the allocator has
// completed a successful pass (MAINT-010, MAINT-012, C4.5). Each half holds
// readiness back on its own: first a reconcile still running with the
// allocator primed, then a primed-less allocator with every key done.
func TestV2BootReadyOnlyAfterEveryKeyProcessedOnceAndAllocatorPrimed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a", "s-b", "s-c")
		release := make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releaseOnce)
		h.rec.setSession(func(it workqueue.Item[rowKey], _ *reconcileEnv) (time.Duration, error) {
			if it.Key.ID == "s-c" {
				<-release
			}
			return 0, nil
		})
		done := h.bootAsync()
		synctest.Wait()
		if len(h.rec.allocatorPasses()) != 1 {
			t.Fatalf("allocator passes = %d, want the boot pass to have completed", len(h.rec.allocatorPasses()))
		}
		if ok, _ := ready(done); ok {
			t.Fatal("boot ready while s-c is still reconciling")
		}
		releaseOnce()
		synctest.Wait()
		if ok, err := ready(done); !ok || err != nil {
			t.Fatalf("boot after every key and a primed allocator: ready=%v err=%v, want ready", ok, err)
		}
		wantStrings(t, "boot reconciles", h.rec.keys(), []string{"s-a", "s-b", "s-c"})
		for _, c := range h.rec.callsFor("s-a") {
			if c.lane != workqueue.LaneResync || !reflect.DeepEqual(c.kinds, []string{v2ReasonBoot}) {
				t.Fatalf("boot reconcile = %+v, want the resync lane with reason %q", c, v2ReasonBoot)
			}
		}
	})
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a", "s-b")
		var allocCalls atomic.Int32
		h.rec.setAllocator(func() error {
			if allocCalls.Add(1) == 1 {
				return errors.New("not yet")
			}
			return nil
		})
		done := h.bootAsync()
		synctest.Wait()
		wantStrings(t, "boot reconciles", h.rec.keys(), []string{"s-a", "s-b"})
		if ok, _ := ready(done); ok {
			t.Fatal("boot ready before the allocator completed a successful pass")
		}
		advance(time.Second) // the allocator's retry succeeds
		if ok, err := ready(done); !ok || err != nil {
			t.Fatalf("boot after the allocator primed: ready=%v err=%v, want ready", ok, err)
		}
		if got := h.rt.stats().Metrics.Boot; got != time.Second {
			t.Fatalf("boot duration = %s, want 1s", got)
		}
	})
}

// Kills: ready on a failed census. A sessions census error is not an empty
// city: boot adds nothing, retries with backoff, and is ready only after a
// census that read.
func TestV2BootCensusErrorRetriesAndDoesNotDeclareReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		h.censusErr = []error{errors.New("store down"), errors.New("store down")}
		done := h.bootAsync()
		synctest.Wait()
		if ok, _ := ready(done); ok || h.censusReads() != 1 || len(h.rec.keys()) != 0 || len(h.rec.allocatorPasses()) != 0 {
			t.Fatalf("after a failed census: ready=%v reads=%d reconciles=%d allocator=%d, want not ready, 1 read, nothing run",
				ok, h.censusReads(), len(h.rec.keys()), len(h.rec.allocatorPasses()))
		}
		advance(time.Second)
		if ok, _ := ready(done); ok || h.censusReads() != 2 {
			t.Fatalf("after the 1s retry failed: ready=%v reads=%d, want not ready, 2 reads", ok, h.censusReads())
		}
		advance(2 * time.Second)
		if ok, err := ready(done); !ok || err != nil || h.censusReads() != 3 {
			t.Fatalf("after the 2s retry read: ready=%v err=%v reads=%d, want ready after 3 reads", ok, err, h.censusReads())
		}
		wantStrings(t, "boot reconciles", h.rec.keys(), []string{"s-a"})
	})
}

// Kills: a failed boot preflight read taken as clean, or as a refusal. Like a
// failed sessions census, it is not an empty city: boot runs no pass and
// retries with backoff, and refuses only what a read shows.
func TestV2BootPreflightReadErrorRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		var reads atomic.Int32
		h.rt.host.bootCensus = func() (v2SessionMigration, error) {
			if reads.Add(1) == 1 {
				return v2SessionMigration{}, errors.New("store down")
			}
			return v2SessionMigration{UnknownStates: map[string]int{"archived": 1}}, nil
		}
		done := h.bootAsync()
		synctest.Wait()
		if ok, _ := ready(done); ok || reads.Load() != 1 || h.censusReads() != 0 {
			t.Fatalf("after a failed preflight read: ready=%v preflight reads=%d census reads=%d, want not ready, 1, 0", ok, reads.Load(), h.censusReads())
		}
		advance(time.Second)
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), `1 open row(s) in a state main does not know ("archived"=1)`) {
				t.Fatalf("boot err = %v, want the refusal", err)
			}
		default:
			t.Fatal("boot did not refuse after the retry read")
		}
		if h.censusReads() != 0 || len(h.rec.keys()) != 0 {
			t.Fatalf("census reads=%d reconciles=%q, want none before a refusal", h.censusReads(), h.rec.keys())
		}
	})
}

// Kills: boot hanging on an empty city.
func TestV2BootWithZeroRowsCompletesImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		start := time.Now()
		h.boot(t)
		if d := time.Since(start); d != 0 || len(h.rec.keys()) != 0 {
			t.Fatalf("empty-city boot took %s with %d reconciles, want 0s and none", d, len(h.rec.keys()))
		}
		if allocs := h.rec.allocatorPasses(); len(allocs) != 1 || !slices.Contains(allocs[0], v2ReasonBoot) {
			t.Fatalf("allocator passes = %q, want one boot pass", allocs)
		}
	})
}

// Kills: a retried boot running a second boot pass or starting a second set
// of goroutines (MAINT-005 relies on boot being idempotent).
// The second boot returns at once and keeps the first boot's duration.
func TestV2BootIsIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			<-time.After(2 * time.Second)
			return 0, nil
		})
		h.boot(t)
		h.boot(t)
		if h.censusReads() != 1 || len(h.rt.lanes) != 2 || h.rt.env.Load().Gen != 1 {
			t.Fatalf("after two boots: census reads %d, lanes %d, env gen %d, want 1, 2, 1",
				h.censusReads(), len(h.rt.lanes), h.rt.env.Load().Gen)
		}
		wantStrings(t, "reconciles", h.rec.keys(), []string{"s-a"})
		if got := h.rt.stats().Metrics.Boot; got != 2*time.Second {
			t.Fatalf("boot duration after a second boot = %s, want the first boot's 2s", got)
		}
	})
}

// Kills: the boot retry blind to stop. A boot waiting out its census backoff
// returns errV2Stopped as soon as the runtime stops.
func TestV2BootCensusRetryReturnsOnStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		h.censusErr = []error{errors.New("store down")}
		done := h.bootAsync()
		synctest.Wait()
		start := time.Now()
		h.rt.stop()
		synctest.Wait()
		if ok, err := ready(done); !ok || !errors.Is(err, errV2Stopped) || time.Since(start) != 0 {
			t.Fatalf("boot after stop: returned=%v err=%v after %s, want errV2Stopped at once", ok, err, time.Since(start))
		}
	})
}

// Kills: the boot coverage credited only through rt.sweep (M04). An event gap
// that lands during the boot pass makes the resync lane's first pass replace
// the sweep while the boot reconciles run; boot still becomes ready on them,
// and the superseded sweep is counted.
func TestV2BootCoverageSurvivesASupersedingResync(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a", "s-b")
		var gap atomic.Bool
		h.legs = func() ([]classStoreCandidate, error) {
			if !gap.Swap(true) {
				h.rt.requestResync("bead-event-gap")
			}
			return nil, nil
		}
		release := make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releaseOnce)
		h.rec.setSession(func(it workqueue.Item[rowKey], _ *reconcileEnv) (time.Duration, error) {
			if slices.Contains(reasonKinds(it.Reasons), v2ReasonBoot) {
				<-release
			}
			return 0, nil
		})
		done := h.bootAsync()
		synctest.Wait()
		if h.censusReads() != 2 || h.rt.stats().Metrics.SupersededSweeps != 1 {
			t.Fatalf("census reads %d, superseded sweeps %d, want the resync to have replaced the boot sweep",
				h.censusReads(), h.rt.stats().Metrics.SupersededSweeps)
		}
		releaseOnce()
		synctest.Wait()
		if ok, err := ready(done); !ok || err != nil {
			t.Fatalf("boot after its reconciles finished: ready=%v err=%v, want ready", ok, err)
		}
	})
}

// Kills: the inventory hook installed after the workers start. A flip
// committed while a boot reconcile is in flight still routes the key, which
// runs again with the inventory reason.
func TestV2InventoryFlipDuringBootReconcileRoutesTheKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		h.hookDelay = time.Millisecond
		var flipped atomic.Bool
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			if !flipped.Swap(true) {
				h.flip("worker-s-a")
			}
			return 0, nil
		})
		h.boot(t)
		calls := h.rec.callsFor("s-a")
		if len(calls) != 2 || !slices.Contains(calls[1].kinds, routeReasonInventory) {
			t.Fatalf("calls = %+v, want the boot reconcile and an inventory requeue", calls)
		}
	})
}

// Kills: resync rows sent to the hot lane; the sweep not timed. A resync pass
// rebuilds the router, adds every open row on the resync lane, wakes the
// allocator and records how long until every row it added was reconciled.
func TestV2ResyncEnqueuesAllOnResyncLaneAndReportsSweepTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a", "s-b")
		h.boot(t)
		h.rec.setSession(func(it workqueue.Item[rowKey], _ *reconcileEnv) (time.Duration, error) {
			if it.Key.ID == "s-b" {
				<-time.After(3 * time.Second)
			}
			return 0, nil
		})
		allocBefore := len(h.rec.allocatorPasses())
		h.rt.requestResync("test")
		advance(5 * time.Second)

		for _, id := range []string{"s-a", "s-b"} {
			calls := h.rec.callsFor(id)
			if len(calls) != 2 || calls[1].lane != workqueue.LaneResync || !reflect.DeepEqual(calls[1].kinds, []string{v2ReasonResync}) {
				t.Fatalf("%s calls = %+v, want a resync-lane reconcile with reason %q", id, calls, v2ReasonResync)
			}
		}
		if h.censusReads() != 2 {
			t.Fatalf("census reads = %d, want 2 (boot and the resync)", h.censusReads())
		}
		allocs := h.rec.allocatorPasses()
		if len(allocs) != allocBefore+1 || !slices.Contains(allocs[len(allocs)-1], v2ReasonResync) {
			t.Fatalf("allocator passes after resync = %q, want one more with reason %q", allocs, v2ReasonResync)
		}
		if got := h.rt.stats().Metrics.LastSweep; got != 3*time.Second {
			t.Fatalf("sweep time = %s, want 3s", got)
		}
	})
}

// Kills: aging bypassed (R32). With one worker and the hot lane never empty,
// a resync row is still reconciled within AgingEvery dequeues.
func TestV2ResyncProgressUnderSustainedHotLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-r")
		h.rt.workers = 1
		h.boot(t)
		var churn atomic.Bool
		churn.Store(true)
		h.rec.setSession(func(it workqueue.Item[rowKey], _ *reconcileEnv) (time.Duration, error) {
			<-time.After(time.Second)
			if churn.Load() && strings.HasPrefix(it.Key.ID, "hot-") {
				h.add(it.Key.ID, workqueue.Reason{Kind: "event"}) // dirty: back to the hot tail at Done
			}
			return 0, nil
		})
		for _, id := range []string{"hot-1", "hot-2", "hot-3", "hot-4", "hot-5", "hot-6", "hot-7", "hot-8", "hot-9", "hot-10"} {
			h.add(id, workqueue.Reason{Kind: "event"})
		}
		advance(3 * time.Second)
		h.rt.requestResync("test")
		synctest.Wait()
		if d := h.rt.stats().Queue.Depth[workqueue.LaneResync]; d != 1 {
			t.Fatalf("resync depth = %d, want the row queued behind the hot load", d)
		}
		advance(9 * time.Second) // at most AgingEvery (8) dequeues of 1s, plus the one in flight
		churn.Store(false)
		calls := h.rec.callsFor("s-r")
		if len(calls) != 2 || calls[1].lane != workqueue.LaneResync {
			t.Fatalf("s-r calls = %+v, want its resync reconcile within 8 dequeues under hot load", calls)
		}
		advance(20 * time.Second)
	})
}

// Kills: a lane pass panic escaping its lane (M41, M42), or a panicking
// allocator pass priming readiness (M47) (MAINT-020). The allocator retries
// after its 1s backoff and boot becomes ready only then; a panicking resync
// pass leaves the enqueue-all pending, so the next pass, even one asked for
// a rebuild only, enqueues every row.
func TestV2LanePanicsAreRecoveredAndDoNotPrimeReadiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		var allocN atomic.Int32
		h.rec.setAllocator(func() error {
			if allocN.Add(1) == 1 {
				panic("allocator exploded")
			}
			return nil
		})
		done := h.bootAsync()
		synctest.Wait()
		if ok, _ := ready(done); ok || h.panics.Load() != 1 {
			t.Fatalf("after a panicking allocator pass: ready=%v panics=%d, want not ready and 1", ok, h.panics.Load())
		}
		advance(time.Second)
		if ok, err := ready(done); !ok || err != nil {
			t.Fatalf("after the allocator's retry: ready=%v err=%v, want ready", ok, err)
		}

		var legsN atomic.Int32
		h.set(func() {
			h.legs = func() ([]classStoreCandidate, error) {
				if legsN.Add(1) == 1 {
					panic("census leg exploded")
				}
				return nil, nil
			}
		})
		h.rt.requestResync("bead-event-gap")
		synctest.Wait()
		if h.panics.Load() != 2 || len(h.rec.callsFor("s-a")) != 1 {
			t.Fatalf("after a panicking resync: panics=%d s-a reconciles=%d, want 2 and 1", h.panics.Load(), len(h.rec.callsFor("s-a")))
		}
		h.rt.requestResync("census-error")
		advance(v2ResyncMinGap)
		calls := h.rec.callsFor("s-a")
		if len(calls) != 2 || calls[1].lane != workqueue.LaneResync {
			t.Fatalf("s-a calls = %+v, want the pending enqueue-all after the panic", calls)
		}
	})
}

// Kills: a changed backstop or minimum gap (M11, M12, M13). The allocator
// runs a patrol pass one patrol interval after its last pass; the resync lane
// runs its enqueue-all five minutes after boot, and a wake right after a pass
// waits out the 30s minimum gap.
func TestV2LaneCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		h.boot(t)
		passes := func() int { return len(h.rec.allocatorPasses()) }
		advance(10*time.Second - time.Millisecond)
		if passes() != 1 {
			t.Fatalf("allocator passes before the 10s patrol = %d, want 1", passes())
		}
		advance(time.Millisecond)
		if allocs := h.rec.allocatorPasses(); len(allocs) != 2 || !reflect.DeepEqual(allocs[1], []string{v2ReasonPatrol}) {
			t.Fatalf("allocator passes at 10s = %q, want a patrol pass", allocs)
		}

		advance(5*time.Minute - 10*time.Second - time.Millisecond)
		if h.censusReads() != 1 {
			t.Fatalf("census reads before the 5m backstop = %d, want 1", h.censusReads())
		}
		advance(time.Millisecond)
		calls := h.rec.callsFor("s-a")
		if h.censusReads() != 2 || len(calls) != 2 || !reflect.DeepEqual(calls[1].kinds, []string{v2ReasonResync}) {
			t.Fatalf("at 5m: census reads %d, s-a calls %+v, want the backstop's enqueue-all", h.censusReads(), calls)
		}

		h.rt.requestResync("bead-event-gap")
		advance(30*time.Second - time.Millisecond)
		if h.censusReads() != 2 {
			t.Fatalf("census reads inside the 30s minimum gap = %d, want 2", h.censusReads())
		}
		advance(time.Millisecond)
		if h.censusReads() != 3 {
			t.Fatalf("census reads after the 30s minimum gap = %d, want 3", h.censusReads())
		}
	})
}

// Kills: every resync reason paying for the enqueue-all, or a failed full
// pass forgetting it. A leg error or overflow rebuilds the index alone; a full
// pass whose sessions census fails leaves the enqueue-all pending, so the
// router's follow-up census-error resync runs it.
func TestV2ResyncRebuildsOnlyForLegErrorsAndOverflows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		h.boot(t)
		allocs := len(h.rec.allocatorPasses())
		h.rt.requestResync("census-error")
		synctest.Wait()
		if h.censusReads() != 2 || len(h.rec.callsFor("s-a")) != 1 || len(h.rec.allocatorPasses()) != allocs {
			t.Fatalf("index-only resync: census reads %d, s-a reconciles %d, allocator passes %d, want 2, 1, %d",
				h.censusReads(), len(h.rec.callsFor("s-a")), len(h.rec.allocatorPasses()), allocs)
		}

		h.set(func() { h.censusErr = []error{errors.New("store down")} })
		h.rt.requestResync("bead-event-gap")
		advance(v2ResyncMinGap)
		if h.censusReads() != 3 || len(h.rec.callsFor("s-a")) != 1 {
			t.Fatalf("failed full resync: census reads %d, s-a reconciles %d, want 3 and 1", h.censusReads(), len(h.rec.callsFor("s-a")))
		}
		advance(v2ResyncMinGap)
		calls := h.rec.callsFor("s-a")
		if h.censusReads() != 4 || len(calls) != 2 || calls[1].lane != workqueue.LaneResync {
			t.Fatalf("follow-up resync: census reads %d, s-a calls %+v, want the pending enqueue-all", h.censusReads(), calls)
		}
	})
}

// Kills: controllers handed no enqueue handle. A controller's bound handle
// routes a key through the router like any other trigger.
func TestV2ControllersBindTheEnqueueHandle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &v2Recorder{}
		var enqueue v2Enqueuer
		h := newV2Harness(t, beads.NewMemStoreFrom(0, []beads.Bead{v2TestRow("s-a")}, nil), func(*v2Metrics) v2Controllers {
			c := rec.controllers()
			c.bind = func(e v2Enqueuer) { enqueue = e }
			return c
		})
		h.boot(t)
		if enqueue == nil {
			t.Fatal("bind not called")
		}
		enqueue("api", reconcilekey.SessionNamed("worker-s-a"))
		synctest.Wait()
		calls := rec.callsFor("s-a")
		if len(calls) != 2 || !reflect.DeepEqual(calls[1].kinds, []string{"api"}) {
			t.Fatalf("calls = %+v, want a reconcile with the handle's reason", calls)
		}
	})
}

// Kills: queued keys processed after stop; stop joining while holding the
// runtime lock. Stop closes admission, never starts queued work, waits for the
// in-flight reconcile without holding a lock, and joins every goroutine
// (C1.8, GUAR-013, INC-026).
func TestV2StopClosesAdmissionJoinsWorkersWithoutStartingQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.rt.workers = 1
		h.boot(t)
		release := make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releaseOnce) // before the harness's stop: a failed check must not wedge it
		h.rec.setSession(func(it workqueue.Item[rowKey], _ *reconcileEnv) (time.Duration, error) {
			if it.Key.ID == "x-1" {
				<-release
			}
			return 0, nil
		})
		for _, id := range []string{"x-1", "x-2", "x-3"} {
			h.add(id, workqueue.Reason{Kind: "event"})
		}
		synctest.Wait()
		stopped := make(chan struct{})
		go func() {
			h.rt.stop()
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("stop returned while a reconcile was in flight")
		default:
		}
		if !h.rt.mu.TryLock() {
			t.Fatal("stop holds the runtime lock while joining")
		}
		h.rt.mu.Unlock()
		if _, accepted := h.rt.sessions.Add(rowKey{Leg: routerTestLeg, ID: "x-4"}, workqueue.LaneHot, workqueue.Reason{Kind: "event"}); accepted {
			t.Fatal("Add accepted after stop")
		}
		releaseOnce()
		<-stopped
		wantStrings(t, "reconciles", h.rec.keys(), []string{"x-1"})
		for i, done := range h.rt.lanes {
			select {
			case <-done:
			default:
				t.Fatalf("lane %d still running after stop", i)
			}
		}
	})
}

// Kills: stop waiting forever on a wedged reconcile. A worker still running
// at the shutdown timeout is logged and abandoned.
func TestV2StopAbandonsAReconcileStuckPastTheShutdownTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		var stderr strings.Builder
		var stderrMu sync.Mutex
		h.rt.host.stderr = writerFunc(func(p []byte) (int, error) {
			stderrMu.Lock()
			defer stderrMu.Unlock()
			return stderr.Write(p)
		})
		h.boot(t)
		release := make(chan struct{})
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			<-release
			return 0, nil
		})
		h.add("x-1", workqueue.Reason{Kind: "event"})
		synctest.Wait()
		start := time.Now()
		h.rt.stop()
		if d := time.Since(start); d != 5*time.Second {
			t.Fatalf("stop returned after %s, want the 5s shutdown timeout", d)
		}
		stderrMu.Lock()
		logged := stderr.String()
		stderrMu.Unlock()
		if !strings.Contains(logged, "abandoning") {
			t.Fatalf("stderr = %q, want the abandoned-worker alert", logged)
		}
		close(release)
	})
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Kills: any store write from the skeleton. The trace-only controllers record
// what they were asked and return; boot, routed events, inventory flips and a
// resync write nothing.
func TestV2TraceOnlyControllersWriteNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &writeCountingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{v2TestRow("s-a"), v2TestRow("s-b")}, nil)}
		h := newV2Harness(t, store, defaultV2Controllers)
		if h.rt.ctrl.complete() || h.rt.ctrl.complete() != v2ControllersInBuild {
			t.Fatalf("complete() = %v, v2ControllersInBuild = %v, want both false for the trace-only skeleton",
				h.rt.ctrl.complete(), v2ControllersInBuild)
		}
		h.boot(t)
		h.rt.router.OnBeadEvent(beadEvent(t, events.BeadUpdated, v2TestRow("s-a")), true, true)
		h.rt.router.Enqueue("socket", reconcilekey.SessionNamed("worker-s-b"))
		h.mu.Lock()
		hook := h.hook
		h.mu.Unlock()
		if hook == nil {
			t.Fatal("boot did not install the inventory pass hook")
		}
		hook(&ObservationSnapshot{}, &ObservationSnapshot{PassSeq: 1, ByName: map[string]RuntimeObservation{"worker-s-a": {}}})
		h.rt.requestResync("test")
		advance(time.Second)

		if store.writes != 0 {
			t.Fatalf("store writes = %d, want 0", store.writes)
		}
		m := h.rt.stats().Metrics
		if m.SessionReasons[v2ReasonBoot] != 2 || m.SessionReasons[routeReasonReplay] != 1 || m.SessionReasons["socket"] != 1 ||
			m.SessionReasons[routeReasonInventory] != 1 || m.SessionReasons[v2ReasonResync] != 2 {
			t.Fatalf("traced session reasons = %v, want boot 2, replay 1, socket 1, inventory 1, resync 2", m.SessionReasons)
		}
		if m.AllocatorWakes[v2ReasonBoot] != 1 {
			t.Fatalf("allocator wakes = %v, want the boot pass", m.AllocatorWakes)
		}
	})
}

// Kills: latency, depth or duty math wrong.
func TestV2MetricsLatencyDepthAndDuty(t *testing.T) {
	m := newV2Metrics()
	for i := 1; i <= 100; i++ {
		m.recordReconcile(time.Duration(i)*time.Millisecond, true, false, time.Duration(101-i)*time.Second, v2Succeeded)
	}
	m.recordReconcile(0, true, false, 0, v2Failed)
	t0 := time.Unix(1000, 0)
	m.startAllocator(t0)
	m.recordAllocatorPass(t0.Add(time.Second), time.Second, false)
	m.recordAllocatorPass(t0.Add(5*time.Second), 2*time.Second, true)
	s := m.snapshot(t0.Add(10 * time.Second))
	if s.LatencyP50 != 50*time.Millisecond || s.LatencyP99 != 99*time.Millisecond {
		t.Fatalf("latency p50/p99 = %s/%s, want 50ms/99ms", s.LatencyP50, s.LatencyP99)
	}
	if s.WorkP50 != 50*time.Second || s.WorkP99 != 99*time.Second {
		t.Fatalf("work p50/p99 = %s/%s, want 50s/99s", s.WorkP50, s.WorkP99)
	}
	if s.Reconciles != 101 || s.Failures != 1 || s.AllocatorPasses != 2 || s.AllocatorFailures != 1 || s.AllocatorLastPass != 2*time.Second {
		t.Fatalf("counts = %+v, want 101 reconciles, 1 failure, 2 passes, 1 failed, last pass 2s", s)
	}
	if s.AllocatorDuty != 0.3 {
		t.Fatalf("allocator duty = %v, want 0.3 (3s busy over 10s)", s.AllocatorDuty)
	}
	// The duty is windowed: a pass straddling the window's start counts only
	// its part inside, and older passes not at all.
	m.recordAllocatorPass(t0.Add(12*time.Second), 4*time.Second, false)
	if got := m.snapshot(t0.Add(v2DutyWindow + 10*time.Second)).AllocatorDuty; got != 2.0/300 {
		t.Fatalf("windowed allocator duty = %v, want %v (2s busy over 5m)", got, 2.0/300)
	}
	var w sampleWindow
	for i := range v2MetricsWindow + 10 {
		w.add(time.Duration(i))
	}
	if len(w.samples) != v2MetricsWindow || w.quantile(0) != 10 {
		t.Fatalf("window holds %d samples, min %d, want the last %d", len(w.samples), w.quantile(0), v2MetricsWindow)
	}

	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.add("q-1", workqueue.Reason{Kind: "event"})
		h.add("q-2", workqueue.Reason{Kind: "event"})
		if d := h.rt.stats().Queue.Depth[workqueue.LaneHot]; d != 2 {
			t.Fatalf("hot depth before boot = %d, want 2", d)
		}
		advance(4 * time.Second) // keys routed before boot wait 4s
		h.boot(t)
		if got := h.rt.stats().Metrics; got.LatencyP50 != 4*time.Second || got.Reconciles != 2 {
			t.Fatalf("pre-boot keys: %d reconciles, latency p50 %s, want 2 at 4s", got.Reconciles, got.LatencyP50)
		}
	})
}

// Kills: no stuck-reconcile alert, one at the reload deadline instead of
// twice it, one on every tick while the same reconcile stays stuck, or none
// for a later reconcile that sticks too. The record reports the longest
// reconcile in flight either way.
func TestV2QueueRecordAlertsOnceOnAReconcileStuckPastTwiceTheReloadDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		stderr := &synchronizedBuffer{}
		h.rt.host.stderr = stderr
		var mu sync.Mutex
		release := map[string]chan struct{}{"stuck-1": make(chan struct{}), "stuck-2": make(chan struct{})}
		t.Cleanup(func() {
			for _, ch := range release {
				select {
				case <-ch:
				default:
					close(ch)
				}
			}
		})
		h.rec.setSession(func(it workqueue.Item[rowKey], _ *reconcileEnv) (time.Duration, error) {
			mu.Lock()
			ch := release[it.Key.ID]
			mu.Unlock()
			if ch != nil {
				<-ch
			}
			return 0, nil
		})
		h.boot(t)
		const stuckAfter = 2 * reloadReconcileDeadline // pinned here, not read from the code under test
		alerts := func() int { return strings.Count(stderr.String(), "session reconcile has been running") }
		record := func() map[string]any { return h.rt.queueRecord(time.Now()) }

		h.add("stuck-1", workqueue.Reason{Kind: "api"})
		synctest.Wait()
		advance(stuckAfter - time.Second)
		if got := record()["longest_in_flight_ms"]; got != (stuckAfter-time.Second).Milliseconds() || alerts() != 0 {
			t.Fatalf("at %s in flight: longest_in_flight_ms = %v, alerts = %d; want %d, none", stuckAfter-time.Second, got, alerts(), (stuckAfter - time.Second).Milliseconds())
		}
		advance(time.Second)
		record()
		advance(time.Minute)
		record()
		if alerts() != 1 {
			t.Fatalf("alerts = %d over two records of one stuck reconcile, want 1:\n%s", alerts(), stderr.String())
		}
		close(release["stuck-1"])
		synctest.Wait()
		if got := record()["longest_in_flight_ms"]; got != int64(0) {
			t.Fatalf("longest_in_flight_ms = %v with nothing in flight, want 0", got)
		}
		h.add("stuck-2", workqueue.Reason{Kind: "api"})
		synctest.Wait()
		advance(stuckAfter)
		record()
		if alerts() != 2 {
			t.Fatalf("alerts = %d after a second reconcile stuck, want 2", alerts())
		}
		close(release["stuck-2"])
		synctest.Wait()
	})
}

// Kills: the record not saying what boot waits on: the census (not read
// yet, or failing), a boot key's first reconcile, or the allocator's first
// successful pass.
func TestV2QueueRecordReportsWhatBootWaitsOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		boot := func() any { return h.rt.queueRecord(time.Now())["boot"] }
		if got := boot(); got != v2BootCensus {
			t.Fatalf("boot before boot = %v, want %s", got, v2BootCensus)
		}
		h.censusErr = []error{errors.New("store down")}
		release := make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releaseOnce)
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			<-release
			return 0, nil
		})
		var allocCalls atomic.Int32
		h.rec.setAllocator(func() error {
			if allocCalls.Add(1) == 1 {
				return errors.New("not yet")
			}
			return nil
		})
		done := h.bootAsync()
		synctest.Wait()
		if got := boot(); got != v2BootCensus {
			t.Fatalf("boot on a failing census = %v, want %s", got, v2BootCensus)
		}
		advance(time.Second) // the census retry reads; s-a's boot reconcile blocks; the allocator fails once
		if got := boot(); got != v2BootCoverage {
			t.Fatalf("boot with s-a reconciling = %v, want %s", got, v2BootCoverage)
		}
		releaseOnce()
		synctest.Wait()
		if got := boot(); got != v2BootAllocator {
			t.Fatalf("boot with every key reconciled and the allocator failing = %v, want %s", got, v2BootAllocator)
		}
		advance(time.Second) // the allocator's retry succeeds
		if ok, err := ready(done); !ok || err != nil {
			t.Fatalf("boot: ready=%v err=%v, want ready", ok, err)
		}
		if got := boot(); got != v2BootReady {
			t.Fatalf("boot after ready = %v, want %s", got, v2BootReady)
		}
	})
}

// Kills: a reconcile_queue field read from the wrong source: the router's
// panics, keys out or counters, a lane's depth or age, dirty for deferred,
// failures for panics, the allocator duty, the boot duration, adds that are
// not since the last record, resync requests by reason, holds and the FS gate;
// and the bead-event latency sampling a replay-only row (E1) or the retry of
// a failed item a bead event queued (E3), or every reconcile.
//
// The timeline (fake time, from the bubble's start):
//
//	0s  boot: s-a's boot reconcile takes 2s, the allocator's first pass 1s
//	2s  f-1's bead event: f-1 starts and blocks; a second event makes it dirty
//	    hold; e-1's bead event
//	3s  r-1's replay, p-1's api add, q-1 on the resync lane, a router panic
//	    (boom), two undecodable events
//	4s  release: e-1, r-1, p-1 and q-1 start; p-1 panics; f-1 fails
//	9s  every retry has run
func TestV2QueueRecordFieldsComeFromTheirSources(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		addSession := h.rt.router.sink.addSession
		h.rt.router.sink.addSession = func(k rowKey, r routeReason) {
			if k.ID == "boom" {
				panic("mapping exploded")
			}
			addSession(k, r)
		}
		unblock := make(chan struct{})
		unblockOnce := sync.OnceFunc(func() { close(unblock) })
		t.Cleanup(unblockOnce)
		var sa, f1, p1, allocs atomic.Int32
		h.rec.setSession(func(it workqueue.Item[rowKey], _ *reconcileEnv) (time.Duration, error) {
			switch it.Key.ID {
			case "s-a":
				if sa.Add(1) == 1 {
					<-time.After(2 * time.Second)
				}
			case "f-1":
				if f1.Add(1) == 1 {
					<-unblock
					return 0, errors.New("failed")
				}
			case "p-1":
				if p1.Add(1) <= 2 {
					panic("reconcile exploded")
				}
			}
			return 0, nil
		})
		h.rec.setAllocator(func() error {
			if allocs.Add(1) == 1 {
				<-time.After(time.Second)
			}
			return nil
		})
		record := func() map[string]any { return h.rt.queueRecord(time.Now()) }
		event := func(id string, replay bool) {
			h.rt.router.OnBeadEvent(beadEvent(t, events.BeadUpdated, v2TestRow(id)), replay, true)
		}

		done := h.bootAsync()
		advance(2 * time.Second)
		if ok, err := ready(done); !ok || err != nil {
			t.Fatalf("boot: ready=%v err=%v, want ready", ok, err)
		}
		record() // the boot's adds

		event("f-1", false)
		synctest.Wait()
		event("f-1", false)
		synctest.Wait()
		busy := record()

		h.rt.hold("test")
		event("e-1", false)
		advance(time.Second)
		event("r-1", true)
		h.add("p-1", workqueue.Reason{Kind: wakeReasonAPI})
		h.rt.sessions.Add(rowKey{Leg: routerTestLeg, ID: "q-1"}, workqueue.LaneResync, workqueue.Reason{Kind: v2ReasonResync})
		event("boom", false)
		for range 2 {
			h.rt.router.OnBeadEvent(events.Event{Type: events.BeadUpdated, Payload: []byte("{")}, false, true)
		}
		advance(time.Second)
		held := record()

		h.rt.release("test")
		synctest.Wait()
		unblockOnce()
		advance(5 * time.Second)
		final := record()

		for _, c := range []struct {
			name   string
			record map[string]any
			field  string
			want   any
		}{
			{"busy", busy, "processing", 1},
			{"busy", busy, "dirty", 1},
			{"busy", busy, "deferred", 0},
			{"busy", busy, "adds", map[string]uint64{routeReasonEvent: 2}},
			{"held", held, "depth_hot", 3},
			{"held", held, "depth_resync", 1},
			{"held", held, "oldest_hot_ms", int64(2000)},
			{"held", held, "oldest_resync_ms", int64(1000)},
			{"held", held, "longest_in_flight_ms", int64(2000)},
			{"held", held, "adds", map[string]uint64{routeReasonEvent: 1, routeReasonReplay: 1, wakeReasonAPI: 1, v2ReasonResync: 1}},
			{"held", held, "holds", []string{"test"}},
			{"held", held, "fs_gate", "unarmed"},
			{"final", final, "boot", v2BootReady},
			{"final", final, "boot_ms", int64(2000)},
			{"final", final, "reconcile_failures", uint64(1)},
			{"final", final, "reconcile_panics", uint64(2)},
			{"final", final, "bead_event_latency_p50_ms", int64(0)},    // f-1's first attempt
			{"final", final, "bead_event_latency_p99_ms", int64(2000)}, // e-1, held 2s
			{"final", final, "latency_p99_ms", int64(2000)},
			{"final", final, "work_p99_ms", int64(2000)},
			{"final", final, "allocator_duty", float64(time.Second) / float64(9*time.Second)},
			{"final", final, "allocator_last_pass_ms", int64(0)},
			{"final", final, "router_events", uint64(6)}, // f-1 twice, e-1, boom and the undecodable two
			{"final", final, "router_replays", uint64(1)},
			{"final", final, "router_undecodable", uint64(2)},
			{"final", final, "router_panics", uint64(1)},
			{"final", final, "router_unresolved", uint64(0)},
			{"final", final, "router_keys_out", uint64(11)}, // a row and the allocator per decoded event, boom's row, an allocator per undecodable one
			{"final", final, "resync_requests", map[string]uint64{"router-panic": 1}},
			{"final", final, "holds", []string{}},
			{"final", final, "deferred", 0},
		} {
			if got := c.record[c.field]; !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s record %s = %#v, want %#v", c.name, c.field, got, c.want)
			}
		}
	})
}

// Kills: the bead-event latency sampling a retry (E3). In the runtime a
// retry's first reason is never a bead event (AddRateLimited puts the retry
// reason first), so recordReconcile's own contract is pinned: the bead-event
// window samples only what the overall window samples.
func TestV2MetricsSamplesBeadEventLatencyOnlyOnFirstAttempts(t *testing.T) {
	m := newV2Metrics()
	m.recordReconcile(time.Second, true, true, 0, v2Succeeded)
	m.recordReconcile(5*time.Second, false, true, 0, v2Succeeded) // a retry
	m.recordReconcile(3*time.Second, true, false, 0, v2Succeeded) // not a bead event
	s := m.snapshot(time.Unix(0, 0))
	if s.BeadEventP99 != time.Second || s.LatencyP99 != 3*time.Second {
		t.Fatalf("bead-event p99 = %s, overall p99 = %s; want 1s (the first attempt only), 3s", s.BeadEventP99, s.LatencyP99)
	}
}

// Kills: the bead-event latency timing a reconcile from an older add of
// another kind. A key queued for a resync waits 10s, then a bead event
// merges into it, and it starts 0.5s later: the item's wait is 10.5s, and
// none of it is bead-event latency.
func TestV2BeadEventLatencyIgnoresAnEventMergedIntoAnOlderAdd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		h.rt.hold("test")
		h.rt.sessions.Add(rowKey{Leg: routerTestLeg, ID: "s-1"}, workqueue.LaneResync, workqueue.Reason{Kind: v2ReasonResync})
		advance(10 * time.Second)
		h.rt.router.OnBeadEvent(beadEvent(t, events.BeadUpdated, v2TestRow("s-1")), false, true)
		advance(500 * time.Millisecond)
		h.rt.release("test")
		synctest.Wait()
		got := h.rt.queueRecord(time.Now())
		if got["latency_p99_ms"] != int64(10500) || got["bead_event_latency_p99_ms"] != int64(0) {
			t.Fatalf("latency p99 = %v, bead-event latency p99 = %v; want 10500, 0 (the event did not queue the item)",
				got["latency_p99_ms"], got["bead_event_latency_p99_ms"])
		}
	})
}

// Kills: the record's FS gate state wrong once the gate is armed: open read
// as unarmed (F1), or open and held swapped (F2), or another hold read as
// FS pressure.
func TestV2QueueRecordReportsTheArmedFSGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		p := newFSScript()
		p.set(false, true)
		h.rt.fs.sample = p.sample
		h.boot(t)
		if !h.rt.armFSGate() {
			t.Fatal("armFSGate after boot = false")
		}
		gate := func() any { return h.rt.queueRecord(time.Now())["fs_gate"] }
		advance(v2FSGateInterval)
		h.rt.hold("test")
		if got := gate(); got != "open" {
			t.Fatalf("fs_gate under low pressure and another hold = %v, want open", got)
		}
		h.rt.release("test")
		p.set(true, true)
		advance(v2FSGateInterval)
		if got := gate(); got != "held" {
			t.Fatalf("fs_gate under high pressure = %v, want held", got)
		}
		p.set(false, true)
		advance(v2FSGateInterval)
		if got := gate(); got != "open" {
			t.Fatalf("fs_gate after the pressure passed = %v, want open", got)
		}
	})
}
