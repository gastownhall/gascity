package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/workqueue"
)

// The barrier and FS-gate tests run in synctest bubbles over the runtime
// harness (reconcile_runtime_test.go). A key named s-busy blocks its
// reconcile until the test releases it, standing in for an in-flight
// reconcile.

const v2BusyKey = "s-busy"

// blockBusy makes every s-busy reconcile wait on the returned channel.
func blockBusy(h *v2Harness) chan struct{} {
	unblock := make(chan struct{})
	h.rec.setSession(func(it workqueue.Item[rowKey], _ *reconcileEnv) (time.Duration, error) {
		if it.Key.ID == v2BusyKey {
			<-unblock
		}
		return 0, nil
	})
	return unblock
}

type barrierResult struct {
	reply reloadControlReply
	err   error
}

var watchReload = reloadIntent{Source: reloadSourceWatch}

// runBarrier runs a watch reload through the barrier in the background; the
// channel yields its result.
func runBarrier(h *v2Harness, apply func() reloadControlReply, reply func(reloadControlReply)) <-chan barrierResult {
	done := make(chan barrierResult, 1)
	go func() {
		r, err := h.rt.barrier.run(context.Background(), watchReload, apply, reply)
		done <- barrierResult{r, err}
	}()
	return done
}

// applied is an apply that serves rev on the host and reports it applied.
func applied(h *v2Harness, rev string) func() reloadControlReply {
	return func() reloadControlReply {
		h.set(func() { h.rev = rev })
		return reloadControlReply{Outcome: reloadOutcomeApplied, Revision: rev}
	}
}

// holds returns the active hold names. Every hold covers the session queue
// and both lanes, so it fails the test when the three disagree.
func (h *v2Harness) holds() []string {
	h.t.Helper()
	queue := h.rt.stats().Queue.Holds
	alloc, resync := h.rt.allocGate.names(), h.rt.resyncGate.names()
	if !slices.Equal(queue, alloc) || !slices.Equal(queue, resync) {
		h.t.Errorf("holds disagree: queue %q, allocator %q, resync %q", queue, alloc, resync)
	}
	return queue
}

// names returns the gate's hold names, sorted.
func (g *laneGate) names() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Sorted(maps.Keys(g.holds))
}

func (h *v2Harness) retryCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.retries
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Kills: apply before the in-flight reconcile returns (no wait); resume
// before publish (the reply sees no hold or the old env); a reconcile, an
// allocator pass or a resync pass running against the half-applied config.
func TestReloadBarrierHoldsWaitsAppliesPublishesResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		unblock := blockBusy(h)
		h.boot(t)
		h.add(v2BusyKey, workqueue.Reason{Kind: "event"})
		synctest.Wait()
		passes, reads := len(h.rec.allocatorPasses()), h.censusReads()

		applying, finish := make(chan struct{}), make(chan struct{})
		var replyHolds []string
		var replyGen uint64
		done := runBarrier(h, func() reloadControlReply {
			close(applying)
			<-finish
			return reloadControlReply{Outcome: reloadOutcomeApplied, Revision: "rev-2"}
		}, func(reloadControlReply) {
			replyHolds, replyGen = h.holds(), h.rt.env.Load().Gen
		})
		synctest.Wait()
		if closed(applying) {
			t.Fatal("apply ran while a reconcile was in flight")
		}
		close(unblock)
		synctest.Wait()
		if !closed(applying) {
			t.Fatal("apply did not run once the in-flight reconcile returned")
		}

		// Mid-apply the host already serves part of the new config; nothing may
		// start against it.
		h.set(func() { h.rev = "rev-2" })
		h.add("s-a", workqueue.Reason{Kind: "event"})
		h.rt.alloc.wake(workqueue.Reason{Kind: "event"})
		h.rt.requestResync("event-gap")
		advance(time.Minute)
		if n := len(h.rec.callsFor("s-a")); n != 1 {
			t.Fatalf("s-a reconciled %d times by mid-apply, want only its boot reconcile", n)
		}
		if n, r := len(h.rec.allocatorPasses()), h.censusReads(); n != passes || r != reads {
			t.Fatalf("mid-apply: %d allocator passes and %d census reads, want %d and %d", n, r, passes, reads)
		}

		close(finish)
		synctest.Wait()
		res := <-done
		if res.err != nil || res.reply.Outcome != reloadOutcomeApplied {
			t.Fatalf("run = %+v, want applied", res)
		}
		if !slices.Contains(replyHolds, v2HoldReload) || replyGen != 2 {
			t.Fatalf("at reply: holds %q, env gen %d; want the reload hold still on and gen 2 published", replyHolds, replyGen)
		}
		calls := h.rec.callsFor("s-a")
		if len(calls) < 2 || calls[len(calls)-1].gen != 2 || calls[len(calls)-1].rev != "rev-2" {
			t.Fatalf("s-a calls = %+v, want a reconcile on gen 2 after resume", calls)
		}
		allocs := h.rec.allocatorPasses()
		if len(allocs) <= passes || !slices.Contains(allocs[len(allocs)-1], v2ReasonReload) {
			t.Fatalf("allocator passes = %q, want a pass after resume with reason %q", allocs, v2ReasonReload)
		}
		if got := h.holds(); len(got) != 0 {
			t.Fatalf("holds after run = %q, want none", got)
		}
	})
}

// Kills: apply after the deadline; holds not released on abort; dirty not
// restored; abort before the deadline.
func TestReloadBarrierAbortsAtDeadlineKeepsConfigAndResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		var stderr strings.Builder
		h.rt.host.stderr = &stderr
		unblock := blockBusy(h)
		defer func() { close(unblock); synctest.Wait() }()
		h.boot(t)
		h.add(v2BusyKey, workqueue.Reason{Kind: "event"})
		synctest.Wait()

		var applyRan bool
		var replied reloadControlReply
		done := runBarrier(h, func() reloadControlReply {
			applyRan = true
			return reloadControlReply{Outcome: reloadOutcomeApplied}
		}, func(r reloadControlReply) { replied = r })
		h.add("s-q", workqueue.Reason{Kind: "event"})
		advance(reloadReconcileDeadline - time.Millisecond)
		select {
		case <-done:
			t.Fatal("barrier aborted before its deadline")
		default:
		}
		advance(time.Millisecond)
		res := <-done
		if !errors.Is(res.err, errReloadBarrierTimeout) || res.reply.Outcome != reloadOutcomeFailed || replied.Outcome != reloadOutcomeFailed {
			t.Fatalf("run = %+v, reply %+v; want a timeout error and a failed reply", res, replied)
		}
		if applyRan || h.rt.env.Load().Gen != 1 {
			t.Fatalf("apply ran %v, env gen %d; want the old config kept", applyRan, h.rt.env.Load().Gen)
		}
		if n := h.retryCount(); n != 1 {
			t.Fatalf("reload retries = %d, want 1 (dirty restored)", n)
		}
		if got := h.holds(); len(got) != 0 {
			t.Fatalf("holds after abort = %q, want none", got)
		}
		if calls := h.rec.callsFor("s-q"); len(calls) != 1 || calls[0].gen != 1 {
			t.Fatalf("s-q calls = %+v, want one reconcile on the old env after the abort", calls)
		}
		if !strings.Contains(stderr.String(), "config reload aborted") {
			t.Fatalf("stderr = %q, want the abort alert", stderr.String())
		}
	})
}

// Kills: the env generation bumped, or the hooks run, on a failed or
// superseded reload; the barrier adding a retry of its own on top of
// rejectSuperseded's.
func TestReloadBarrierSupersededReloadPublishesNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		superseded bool
	}{{"superseded", true}, {"failed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newV2HarnessWithRows(t)
				h.boot(t)
				hooks := 0
				h.rt.barrier.afterApply = []reloadHook{func(context.Context, reloadIntent, *reconcileEnv, *reloadControlReply) { hooks++ }}
				h.rt.barrier.afterPublish = h.rt.barrier.afterApply
				r, err := h.rt.barrier.run(context.Background(), watchReload, func() reloadControlReply {
					// rejectSuperseded and every failure return before
					// publishRuntimeConfig, so the host keeps serving rev-1.
					if tc.superseded {
						h.rt.host.retryReload()
					}
					return reloadControlReply{Outcome: reloadOutcomeFailed, Revision: "rev-2", Error: "superseded"}
				}, nil)
				if err != nil || r.Outcome != reloadOutcomeFailed {
					t.Fatalf("run = %+v, %v; want apply's failed reply and no error", r, err)
				}
				if env := h.rt.env.Load(); env.Gen != 1 || env.ConfigRev != "rev-1" {
					t.Fatalf("env = gen %d rev %q, want gen 1 rev-1", env.Gen, env.ConfigRev)
				}
				if hooks != 0 {
					t.Fatalf("hooks ran %d times, want none", hooks)
				}
				want := 0
				if tc.superseded {
					want = 1
				}
				if n := h.retryCount(); n != want {
					t.Fatalf("reload retries = %d, want %d (rejectSuperseded's only)", n, want)
				}
			})
		})
	}
}

// Kills: afterPublish run before afterApply (A3); hooks run before the env
// is published; the reply sent before the hooks amend it; a no-change
// reload skipping the hooks (soft acceptance) or publishing.
func TestReloadBarrierHookOrderApplyAfterApplyAfterPublish(t *testing.T) {
	for _, tc := range []struct {
		outcome reloadOutcome
		want    []string
	}{
		{reloadOutcomeApplied, []string{"apply", "afterApply gen=2", "afterPublish gen=2", "reply amended"}},
		{reloadOutcomeNoChange, []string{"apply", "afterApply gen=1", "afterPublish gen=1", "reply amended"}},
		{reloadOutcomeFailed, []string{"apply", "reply "}},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newV2HarnessWithRows(t)
				h.boot(t)
				var order []string
				hook := func(name string) reloadHook {
					return func(_ context.Context, _ reloadIntent, env *reconcileEnv, r *reloadControlReply) {
						order = append(order, fmt.Sprintf("%s gen=%d", name, env.Gen))
						r.Message = "amended"
					}
				}
				h.rt.barrier.afterApply = []reloadHook{hook("afterApply")}
				h.rt.barrier.afterPublish = []reloadHook{hook("afterPublish")}
				if _, err := h.rt.barrier.run(context.Background(), reloadIntent{Source: reloadSourceManual}, func() reloadControlReply {
					order = append(order, "apply")
					if tc.outcome == reloadOutcomeApplied {
						h.set(func() { h.rev = "rev-2" })
					}
					return reloadControlReply{Outcome: tc.outcome}
				}, func(r reloadControlReply) { order = append(order, "reply "+r.Message) }); err != nil {
					t.Fatalf("run: %v", err)
				}
				if !slices.Equal(order, tc.want) {
					t.Fatalf("order = %q, want %q", order, tc.want)
				}
			})
		})
	}
}

// Kills: holds leaked after a panic in apply (the tick's safeTick catches it).
func TestReloadBarrierReleasesHoldsOnPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		passes := len(h.rec.allocatorPasses())
		replied := false
		func() {
			defer func() { _ = recover() }()
			_, _ = h.rt.barrier.run(context.Background(), watchReload, func() reloadControlReply {
				panic("apply exploded")
			}, func(reloadControlReply) { replied = true })
		}()
		if got := h.holds(); len(got) != 0 || replied {
			t.Fatalf("after the panic: holds %q, replied %v; want no hold and no reply (the tick replies)", got, replied)
		}
		h.add("s-q", workqueue.Reason{Kind: "event"})
		h.rt.alloc.wake(workqueue.Reason{Kind: "event"})
		advance(time.Second) // past the lane's minimum gap
		if len(h.rec.callsFor("s-q")) != 1 || len(h.rec.allocatorPasses()) != passes+1 {
			t.Fatalf("after the panic: s-q calls %d, allocator passes %d; want the queue and the lane running", len(h.rec.callsFor("s-q")), len(h.rec.allocatorPasses())-passes)
		}
	})
}

// Kills: the router left on stale stores after a city or rig store rebuild;
// a resync on every reload.
func TestReloadBarrierStoreSwapRequestsResync(t *testing.T) {
	rig := beads.NewMemStore()
	for _, tc := range []struct {
		name string
		swap func(h *v2Harness)
		want int
	}{
		{"none", func(*v2Harness) {}, 0},
		{"same-stores", func(h *v2Harness) { h.rigs = map[string]beads.Store{"r": rig} }, 0},
		{"city", func(h *v2Harness) { h.store = beads.NewMemStore() }, 1},
		{"rig", func(h *v2Harness) { h.rigs = map[string]beads.Store{"r": beads.NewMemStore()} }, 1},
		{"rig-added", func(h *v2Harness) { h.rigs = map[string]beads.Store{"r": rig, "s": rig} }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newV2HarnessWithRows(t, "s-a")
				h.set(func() { h.rigs = map[string]beads.Store{"r": rig} })
				h.boot(t)
				reads := h.censusReads()
				if _, err := h.rt.barrier.run(context.Background(), watchReload, func() reloadControlReply {
					h.set(func() { tc.swap(h) })
					return reloadControlReply{Outcome: reloadOutcomeApplied}
				}, nil); err != nil {
					t.Fatalf("run: %v", err)
				}
				synctest.Wait()
				want := reads + tc.want
				if got := h.censusReads(); got != want {
					t.Fatalf("census reads after the reload = %d, want %d", got, want)
				}
			})
		})
	}
}

// Kills: the barrier waiting out the FS hold, or ending it.
func TestReloadBarrierIgnoresFSHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		p := newFSScript()
		h.rt.fs.sample = p.sample
		h.boot(t)
		h.rt.armFSGate()
		advance(v2FSGateInterval)
		if !slices.Contains(h.holds(), v2HoldFSPressure) {
			t.Fatalf("holds = %q, want the FS hold", h.holds())
		}
		done := runBarrier(h, applied(h, "rev-2"), nil)
		synctest.Wait()
		select {
		case res := <-done:
			if res.err != nil || h.rt.env.Load().Gen != 2 {
				t.Fatalf("run = %+v, env gen %d; want applied at gen 2", res, h.rt.env.Load().Gen)
			}
		default:
			t.Fatal("the barrier waited on the FS hold")
		}
		if got := h.holds(); !slices.Equal(got, []string{v2HoldFSPressure}) {
			t.Fatalf("holds after the reload = %q, want the FS hold alone", got)
		}
	})
}

// Kills: the allocator or resync pass ignoring a hold; a pass lost across the
// hold; a skipped pass pacing the one the release wakes; waitIdle returning
// while an allocator pass runs.
func TestV2LaneHoldsBlockPassesAndWaitIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		passes, reads := len(h.rec.allocatorPasses()), h.censusReads()

		h.rt.hold("test")
		h.rt.alloc.wake(workqueue.Reason{Kind: "event"})
		h.rt.requestResync("event-gap")
		// Both lanes skip their pass under the hold. The release comes inside
		// the resync lane's 30s minimum gap, so the woken pass runs at once
		// only if the skipped one did not count toward the lane's pacing.
		advance(time.Second)
		if n, r := len(h.rec.allocatorPasses()), h.censusReads(); n != passes || r != reads {
			t.Fatalf("under the hold: %d allocator passes, %d census reads; want %d and %d", n, r, passes, reads)
		}
		h.rt.release("test")
		synctest.Wait()
		allocs := h.rec.allocatorPasses()
		if len(allocs) != passes+1 || !slices.Contains(allocs[passes], "event") || h.censusReads() != reads+1 {
			t.Fatalf("after release: allocator passes %q, census reads %d; want one held pass each", allocs[passes:], h.censusReads()-reads)
		}

		slow := make(chan struct{})
		h.rec.setAllocator(func() error { <-slow; return nil })
		h.rt.alloc.wake(workqueue.Reason{Kind: "event"})
		advance(time.Second) // past the lane's duty-cycle gap
		idle := make(chan error, 1)
		go func() { idle <- h.rt.waitIdle(context.Background()) }()
		synctest.Wait()
		select {
		case <-idle:
			t.Fatal("waitIdle returned while an allocator pass was running")
		default:
		}
		close(slow)
		synctest.Wait()
		if err := <-idle; err != nil {
			t.Fatalf("waitIdle = %v, want nil once the pass returned", err)
		}
	})
}

// fsScript is a scripted FS pressure sampler.
type fsScript struct {
	mu       sync.Mutex
	high, ok bool
	samples  int
}

// newFSScript starts at high, readable pressure.
func newFSScript() *fsScript { return &fsScript{high: true, ok: true} }

func (p *fsScript) sample() (fsPressureStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.samples++
	if !p.ok {
		return fsPressureStatus{}, false
	}
	return fsPressureStatus{Avg60: 90, Threshold: 50, High: p.high}, true
}

func (p *fsScript) set(high, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.high, p.ok = high, ok
}

func (p *fsScript) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.samples
}

// Kills: never forcing a window (starvation); forcing immediately; a
// forced window that does not re-hold. The harness patrol is 10s, so the gate
// holds from the first sample (5s), forces at 5 patrols held (55s) and
// re-holds one patrol later (65s).
func TestWorkerFSGateHoldsAndForcesAWindowAfterFivePatrols(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.rt.fs.sample = newFSScript().sample
		h.boot(t)
		if !h.rt.armFSGate() {
			t.Fatal("armFSGate after boot = false")
		}
		held := func() bool { return slices.Contains(h.holds(), v2HoldFSPressure) }
		advance(v2FSGateInterval)
		h.add("s-q", workqueue.Reason{Kind: "event"})
		synctest.Wait()
		if !held() || len(h.rec.callsFor("s-q")) != 0 {
			t.Fatalf("at 5s: held %v, s-q calls %d; want held and nothing reconciled", held(), len(h.rec.callsFor("s-q")))
		}
		for at := 2 * v2FSGateInterval; at <= 50*time.Second; at += v2FSGateInterval {
			advance(v2FSGateInterval)
			if !held() {
				t.Fatalf("at %s: released, want held until 5 patrols have passed", at)
			}
		}
		for _, step := range []struct {
			by   time.Duration
			held bool
		}{
			{4 * time.Second, true},  // 54s: 49s held
			{time.Second, false},     // 55s: 5 patrols held, forced window
			{9 * time.Second, false}, // 64s: still inside the window
			{time.Second, true},      // 65s: the window ends, pressure still high
			{49 * time.Second, true}, // 114s
			{time.Second, false},     // 115s: the next forced window
		} {
			advance(step.by)
			if held() != step.held {
				t.Fatalf("after +%s: held %v, want %v", step.by, held(), step.held)
			}
		}
		if n := len(h.rec.callsFor("s-q")); n != 1 {
			t.Fatalf("s-q calls = %d, want 1 (in the first forced window)", n)
		}
	})
}

// Kills: failing closed on an unreadable PSI reading; an episode not reset
// when the gate releases.
func TestWorkerFSGateFailsOpenOnUnreadablePressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		var stderr strings.Builder
		h.rt.host.stderr = &stderr
		p := newFSScript()
		h.rt.fs.sample = p.sample
		h.boot(t)
		h.rt.armFSGate()
		advance(v2FSGateInterval)
		if !slices.Contains(h.holds(), v2HoldFSPressure) {
			t.Fatalf("holds = %q, want the FS hold", h.holds())
		}
		h.add("s-q", workqueue.Reason{Kind: "event"})
		p.set(true, false)
		advance(v2FSGateInterval)
		if got := h.holds(); len(got) != 0 || len(h.rec.callsFor("s-q")) != 1 {
			t.Fatalf("unreadable pressure: holds %q, s-q calls %d; want released and s-q reconciled", got, len(h.rec.callsFor("s-q")))
		}
		p.set(true, true)
		advance(v2FSGateInterval)
		if !slices.Contains(h.holds(), v2HoldFSPressure) || strings.Count(stderr.String(), "holding reconciles") != 2 {
			t.Fatalf("holds %q, stderr %q; want a new held episode, logged again", h.holds(), stderr.String())
		}
	})
}

// Kills: the gate armed at boot (F8), or ready set before boot's wait (N25).
// The allocator fails its first three passes, so boot is ready at 7s
// (1s+2s+4s of backoff), after the gate's first sample would have been due; a
// gate armed at boot holds the lane at 5s and boot waits for the forced
// window.
func TestWorkerFSGateNotArmedBeforeReadiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		p := newFSScript()
		h.rt.fs.sample = p.sample
		failures := 0
		h.rec.setAllocator(func() error {
			if failures < 3 {
				failures++
				return errors.New("not yet")
			}
			return nil
		})
		if h.rt.armFSGate() {
			t.Fatal("armFSGate before boot = true")
		}
		done := h.bootAsync()
		advance(time.Second)
		if h.rt.armFSGate() {
			t.Fatal("armFSGate while boot waits for the allocator = true")
		}
		advance(6 * time.Second)
		if ok, err := ready(done); !ok || err != nil {
			t.Fatalf("boot under FS pressure: ready %v, err %v; want ready at 7s", ok, err)
		}
		if got := h.holds(); len(got) != 0 || p.count() != 0 {
			t.Fatalf("before arming: holds %q, %d samples; want none", got, p.count())
		}
		if !h.rt.armFSGate() {
			t.Fatal("armFSGate after boot = false")
		}
		advance(v2FSGateInterval)
		if !slices.Contains(h.holds(), v2HoldFSPressure) || p.count() != 1 {
			t.Fatalf("after arming: holds %q, %d samples; want the FS hold after one sample", h.holds(), p.count())
		}
	})
}

// Kills: an aborted reload retrying its hold at every patrol while the
// reconcile or lane pass that aborted it is still hung (the hold then
// dominates and the city stalls); a deferred reload dropping its retry or
// holding anyway; a manual reload deferred too; a deferral that outlives the
// hung work.
func TestReloadBarrierHungWorkDoesNotDominateHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		hang func(h *v2Harness) chan struct{}
	}{
		{"reconcile", func(h *v2Harness) chan struct{} {
			unblock := blockBusy(h)
			h.add(v2BusyKey, workqueue.Reason{Kind: "event"})
			return unblock
		}},
		{"allocator", func(h *v2Harness) chan struct{} {
			unblock := make(chan struct{})
			h.rec.setAllocator(func() error { <-unblock; return nil })
			h.rt.alloc.wake(workqueue.Reason{Kind: "event"})
			return unblock
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newV2HarnessWithRows(t)
				var pending atomic.Bool
				h.rt.host.retryReload = func() { pending.Store(true) }
				h.boot(t)
				advance(time.Second) // past the allocator lane's minimum gap
				unblock := tc.hang(h)
				synctest.Wait()

				// The maintenance loop: one tick per patrol, each first
				// running the pending reload through the barrier.
				pending.Store(true)
				var ticks atomic.Int64
				stop, stopped := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(stopped)
					for {
						if pending.Swap(false) {
							_, _ = h.rt.barrier.run(context.Background(), watchReload, applied(h, "rev-2"), nil)
						}
						ticks.Add(1)
						select {
						case <-stop:
							return
						case <-time.After(10 * time.Second):
						}
					}
				}()
				const window = 10 * time.Minute
				held := 0
				for i := range int(window / time.Second) {
					advance(time.Second)
					if slices.Contains(h.holds(), v2HoldReload) {
						held++
					}
					if i == 300 {
						h.add("s-q", workqueue.Reason{Kind: "event"})
					}
				}
				close(stop)
				synctest.Wait()
				<-stopped
				// The first attempt holds for the deadline (30s); every later
				// tick defers without a hold.
				if held > int(window/time.Second)/10 {
					t.Fatalf("reload hold on %d of %d seconds, want at most 10%%", held, int(window/time.Second))
				}
				if n := ticks.Load(); n < 55 {
					t.Fatalf("maintenance ticks in %s = %d, want at least 55 (one per 10s patrol)", window, n)
				}
				if n := len(h.rec.callsFor("s-q")); n != 1 {
					t.Fatalf("s-q reconciled %d times, want 1 while the reload is deferred", n)
				}
				if !pending.Load() || h.rt.env.Load().Gen != 1 {
					t.Fatalf("pending %v, env gen %d; want the reload still pending on the old config", pending.Load(), h.rt.env.Load().Gen)
				}

				// A manual reload is never deferred.
				done := make(chan barrierResult, 1)
				go func() {
					r, err := h.rt.barrier.run(context.Background(), reloadIntent{Source: reloadSourceManual}, applied(h, "rev-2"), nil)
					done <- barrierResult{r, err}
				}()
				synctest.Wait()
				if !slices.Contains(h.holds(), v2HoldReload) {
					t.Fatalf("manual reload: holds %q, want the reload hold", h.holds())
				}
				advance(reloadReconcileDeadline)
				if res := <-done; !errors.Is(res.err, errReloadBarrierTimeout) {
					t.Fatalf("manual reload = %+v, want a timeout abort", res)
				}

				// Once the hung work returns, the next reload applies.
				close(unblock)
				synctest.Wait()
				if _, err := h.rt.barrier.run(context.Background(), watchReload, applied(h, "rev-2"), nil); err != nil || h.rt.env.Load().Gen != 2 {
					t.Fatalf("reload after the hang = %v, env gen %d; want applied at gen 2", err, h.rt.env.Load().Gen)
				}
			})
		})
	}
}

// Kills: the hold taken after the idle wait (N5), so a key added while the
// barrier waits runs before apply.
func TestReloadBarrierHoldsBeforeWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		unblock := blockBusy(h)
		h.boot(t)
		h.add(v2BusyKey, workqueue.Reason{Kind: "event"})
		synctest.Wait()
		before := -1
		done := runBarrier(h, func() reloadControlReply {
			before = len(h.rec.callsFor("s-q"))
			return applied(h, "rev-2")()
		}, nil)
		synctest.Wait()
		h.add("s-q", workqueue.Reason{Kind: "event"})
		synctest.Wait()
		close(unblock)
		synctest.Wait()
		if res := <-done; res.err != nil {
			t.Fatalf("run: %v", res.err)
		}
		if before != 0 {
			t.Fatalf("s-q reconciled %d times before apply, want 0 (held while the barrier waits)", before)
		}
		if calls := h.rec.callsFor("s-q"); len(calls) != 1 || calls[0].gen != 2 {
			t.Fatalf("s-q calls = %+v, want one reconcile on gen 2 after resume", calls)
		}
	})
}

// Kills: waitIdle skipping the resync lane (N1), so apply runs while a resync
// pass is rebuilding the router from the old stores.
func TestReloadBarrierWaitsForResyncPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		entered, unblock := make(chan struct{}), make(chan struct{})
		h.set(func() {
			h.legs = func() ([]classStoreCandidate, error) {
				close(entered)
				<-unblock
				return nil, nil
			}
		})
		h.rt.requestResync("event-gap")
		synctest.Wait()
		if !closed(entered) {
			t.Fatal("the resync pass did not start")
		}
		applyRan := false
		done := runBarrier(h, func() reloadControlReply {
			applyRan = true
			return applied(h, "rev-2")()
		}, nil)
		synctest.Wait()
		if applyRan {
			t.Fatal("apply ran while a resync pass was running")
		}
		close(unblock)
		synctest.Wait()
		if res := <-done; res.err != nil || !applyRan {
			t.Fatalf("run = %+v, apply ran %v; want applied once the pass returned", res, applyRan)
		}
	})
}

// Kills: any release clearing the skipped pass (N3): with the FS and reload
// holds overlapping, the reload's release forgets the skip and the FS
// release then wakes nothing, losing both lanes' passes until the backstop.
func TestV2LaneOverlappingHoldsKeepSkippedPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		advance(time.Second)
		passes, reads := len(h.rec.allocatorPasses()), h.censusReads()
		h.rt.hold(v2HoldFSPressure)
		h.rt.hold(v2HoldReload)
		h.rt.alloc.wake(workqueue.Reason{Kind: "event"})
		h.rt.requestResync("event-gap")
		synctest.Wait()
		h.rt.release(v2HoldReload)
		synctest.Wait()
		if n, r := len(h.rec.allocatorPasses()), h.censusReads(); n != passes || r != reads {
			t.Fatalf("under the FS hold: %d allocator passes, %d census reads; want %d and %d", n, r, passes, reads)
		}
		h.rt.release(v2HoldFSPressure)
		synctest.Wait()
		if n, r := len(h.rec.allocatorPasses()), h.censusReads(); n != passes+1 || r != reads+1 {
			t.Fatalf("after the last release: %d allocator passes, %d census reads; want %d and %d", n, r, passes+1, reads+1)
		}
	})
}

// Kills: heldSince not reset when the gate holds again after a forced window
// (N4), which forces the next window one sample later instead of five
// patrols later. Harness patrol 10s: held from 5s, a 10s window every 60s.
func TestWorkerFSGateDutyOverSeveralWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.rt.fs.sample = newFSScript().sample
		h.boot(t)
		h.rt.armFSGate()
		var released []time.Duration
		for at := v2FSGateInterval; at <= 300*time.Second; at += v2FSGateInterval {
			advance(v2FSGateInterval)
			if !slices.Contains(h.holds(), v2HoldFSPressure) {
				released = append(released, at)
			}
		}
		want := []time.Duration{55, 60, 115, 120, 175, 180, 235, 240, 295, 300}
		for i := range want {
			want[i] *= time.Second
		}
		if !slices.Equal(released, want) {
			t.Fatalf("released at %v, want %v", released, want)
		}
	})
}

// Kills: a backstop skipped under a hold dropping the enqueue-all (N2): the
// pass the release wakes would rebuild only.
func TestV2HeldResyncBackstopKeepsEnqueueAll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "s-a")
		h.boot(t)
		n := len(h.rec.callsFor("s-a"))
		h.rt.hold("test")
		advance(v2ResyncInterval) // the backstop comes due under the hold
		if got := len(h.rec.callsFor("s-a")); got != n {
			t.Fatalf("s-a reconciled %d times under the hold, want %d", got, n)
		}
		h.rt.release("test")
		synctest.Wait()
		calls := h.rec.callsFor("s-a")
		if len(calls) != n+1 || !slices.Contains(calls[n].kinds, v2ReasonResync) {
			t.Fatalf("s-a calls after release = %+v, want one resync reconcile (the held backstop's enqueue-all)", calls[n:])
		}
	})
}

// Kills: the env published by apply's reported outcome rather than by what
// the host serves: a panic after publishRuntimeConfig leaving the workers on
// the old config, a failed reply hiding a published config, or a reload that
// changed nothing bumping the generation.
func TestReloadBarrierPublishesEnvByHostState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		func() {
			defer func() { _ = recover() }()
			_, _ = h.rt.barrier.run(context.Background(), watchReload, func() reloadControlReply {
				h.set(func() { h.rev = "rev-2" }) // publishRuntimeConfig ran
				panic("reload tail exploded")
			}, nil)
		}()
		if env := h.rt.env.Load(); env.Gen != 2 || env.ConfigRev != "rev-2" {
			t.Fatalf("after a panic in the tail: env gen %d rev %q, want gen 2 rev-2", env.Gen, env.ConfigRev)
		}
		if _, err := h.rt.barrier.run(context.Background(), watchReload, func() reloadControlReply {
			h.set(func() { h.rev = "rev-3" })
			return reloadControlReply{Outcome: reloadOutcomeFailed}
		}, nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		if env := h.rt.env.Load(); env.Gen != 3 || env.ConfigRev != "rev-3" {
			t.Fatalf("after a failed reply on a changed host: env gen %d rev %q, want gen 3 rev-3", env.Gen, env.ConfigRev)
		}
		if _, err := h.rt.barrier.run(context.Background(), watchReload, func() reloadControlReply {
			return reloadControlReply{Outcome: reloadOutcomeApplied}
		}, nil); err != nil || h.rt.env.Load().Gen != 3 {
			t.Fatalf("unchanged host: run %v, env gen %d; want gen 3 kept", err, h.rt.env.Load().Gen)
		}
	})
}

// Kills: hooks not told the reload's source and softness (P4.4 accepts drift
// only on a soft reload); an aborted soft reload retried as a hard one; the
// soft intent outliving the retry that applied it.
func TestReloadBarrierHooksSeeIntentAndAbortKeepsSoft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		var got []reloadIntent
		hook := func(_ context.Context, in reloadIntent, _ *reconcileEnv, _ *reloadControlReply) {
			got = append(got, in)
		}
		h.rt.barrier.afterApply = []reloadHook{hook}
		h.rt.barrier.afterPublish = []reloadHook{hook}
		check := func(step string, want reloadIntent) {
			t.Helper()
			if !slices.Equal(got, []reloadIntent{want, want}) {
				t.Fatalf("%s: hooks saw %+v, want %+v twice", step, got, want)
			}
			got = nil
		}
		soft := reloadIntent{Source: reloadSourceManual, Soft: true}
		if _, err := h.rt.barrier.run(context.Background(), soft, applied(h, "rev-2"), nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		check("soft manual reload", soft)

		unblock := blockBusy(h)
		h.add(v2BusyKey, workqueue.Reason{Kind: "event"})
		synctest.Wait()
		done := make(chan error, 1)
		go func() {
			_, err := h.rt.barrier.run(context.Background(), soft, applied(h, "rev-3"), nil)
			done <- err
		}()
		advance(reloadReconcileDeadline)
		if err := <-done; !errors.Is(err, errReloadBarrierTimeout) {
			t.Fatalf("run = %v, want a timeout abort", err)
		}
		close(unblock)
		synctest.Wait()
		if _, err := h.rt.barrier.run(context.Background(), watchReload, applied(h, "rev-3"), nil); err != nil {
			t.Fatalf("retry: %v", err)
		}
		check("retry of the aborted soft reload", reloadIntent{Source: reloadSourceWatch, Soft: true})
		if _, err := h.rt.barrier.run(context.Background(), watchReload, applied(h, "rev-4"), nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		check("the next watch reload", watchReload)
	})
}

// Kills: a shutdown during the wait reported as a deadline abort, with a
// retry left pending for a controller that is stopping.
func TestReloadBarrierCanceledWaitLeavesNoRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		unblock := blockBusy(h)
		defer func() { close(unblock); synctest.Wait() }()
		h.boot(t)
		h.add(v2BusyKey, workqueue.Reason{Kind: "event"})
		synctest.Wait()
		ctx, cancel := context.WithCancel(context.Background())
		var replied reloadControlReply
		done := make(chan barrierResult, 1)
		go func() {
			r, err := h.rt.barrier.run(ctx, reloadIntent{Source: reloadSourceManual}, applied(h, "rev-2"), func(r reloadControlReply) { replied = r })
			done <- barrierResult{r, err}
		}()
		advance(time.Second)
		cancel()
		synctest.Wait()
		res := <-done
		if !errors.Is(res.err, context.Canceled) || errors.Is(res.err, errReloadBarrierTimeout) {
			t.Fatalf("run err = %v, want context.Canceled and no timeout", res.err)
		}
		if replied.Outcome != reloadOutcomeFailed || !strings.Contains(replied.Error, "canceled") {
			t.Fatalf("reply = %+v, want a failed reply saying canceled", replied)
		}
		if n := h.retryCount(); n != 0 || len(h.holds()) != 0 || h.rt.env.Load().Gen != 1 {
			t.Fatalf("retries %d, holds %q, env gen %d; want no retry, no hold, gen 1", n, h.holds(), h.rt.env.Load().Gen)
		}
	})
}

// Kills: a hook panic escaping the barrier (no reply from it, the remaining
// hooks' order lost); hooks after the panicking one still running.
func TestReloadBarrierHookPanicRepliesApplied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		h.boot(t)
		later := false
		h.rt.barrier.afterApply = []reloadHook{func(context.Context, reloadIntent, *reconcileEnv, *reloadControlReply) { panic("hook exploded") }}
		h.rt.barrier.afterPublish = []reloadHook{func(context.Context, reloadIntent, *reconcileEnv, *reloadControlReply) { later = true }}
		var replied reloadControlReply
		r, err := h.rt.barrier.run(context.Background(), reloadIntent{Source: reloadSourceManual}, applied(h, "rev-2"), func(r reloadControlReply) { replied = r })
		if err != nil || r.Outcome != reloadOutcomeApplied || replied.Outcome != reloadOutcomeApplied || len(replied.Warnings) != 1 {
			t.Fatalf("run = %+v, %v; reply %+v; want applied with one warning", r, err, replied)
		}
		if later || len(h.holds()) != 0 || h.rt.env.Load().Gen != 2 {
			t.Fatalf("later hook ran %v, holds %q, env gen %d; want skipped, none, 2", later, h.holds(), h.rt.env.Load().Gen)
		}
	})
}

// Kills: a second arm starting a second sampler (N6).
func TestWorkerFSGateArmTwiceIsNoOp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t)
		p := newFSScript()
		h.rt.fs.sample = p.sample
		h.boot(t)
		first := h.rt.armFSGate()
		if second := h.rt.armFSGate(); !first || !second {
			t.Fatalf("armFSGate after boot = %v, then %v; want true twice", first, second)
		}
		advance(v2FSGateInterval)
		if n := p.count(); n != 1 {
			t.Fatalf("samples after one interval = %d, want 1 (one sampler)", n)
		}
	})
}
