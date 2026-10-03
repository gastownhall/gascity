package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/resilience"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// recordedHook is one on_death hook invocation.
type recordedHook struct {
	Command string
	Dir     string
	Env     map[string]string
}

// hookRecorder is an injected on_death runner. With a gate set, every hook
// blocks until the gate yields, so tests can hold a hook running.
type hookRecorder struct {
	mu         sync.Mutex
	calls      []recordedHook
	gate       chan struct{}
	running    int
	maxRunning int
	finished   int
	out        string
	err        error
}

func (h *hookRecorder) run(command, dir string, env map[string]string) (string, error) {
	h.mu.Lock()
	h.calls = append(h.calls, recordedHook{Command: command, Dir: dir, Env: maps.Clone(env)})
	h.running++
	h.maxRunning = max(h.maxRunning, h.running)
	gate := h.gate
	h.mu.Unlock()
	if gate != nil {
		<-gate
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running--
	h.finished++
	return h.out, h.err
}

func (h *hookRecorder) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.calls))
	for i, c := range h.calls {
		out[i] = c.Command
	}
	return out
}

func (h *hookRecorder) snapshot() []recordedHook {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

// onDeathHandlers maps each name to a hook whose command names it.
func onDeathHandlers(names ...string) map[string]poolDeathInfo {
	h := make(map[string]poolDeathInfo, len(names))
	for _, n := range names {
		h[n] = poolDeathInfo{Command: "release " + n, Dir: "/city", Env: map[string]string{"GC_AGENT": n}}
	}
	return h
}

// setListing replaces the scripted listing; every name gets an incarnation.
func (p *scriptedInventoryProvider) setListing(err error, names ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.names, p.listErr = names, err
	for _, n := range names {
		if _, ok := p.inventory[n]; !ok {
			p.inventory[n] = runtime.InventoryEntry{Incarnation: n + ":1", DeadKnown: true, AttachedKnown: true}
		}
	}
}

// onDeathLaneRuntime is a lane runtime with handlers and a recording runner.
func onDeathLaneRuntime(t *testing.T, sp runtime.Provider, handlers map[string]poolDeathInfo) (*CityRuntime, *hookRecorder, *syncBuffer) {
	t.Helper()
	stderr := newSyncBuffer()
	cr := inventoryLaneTestRuntime(t, sp, stderr)
	hooks := &hookRecorder{}
	cr.poolDeathHookRunner = hooks.run
	cr.publishPoolDeathHandlers(handlers)
	return cr, hooks, stderr
}

// legacyOnDeathRuntime is a lane-less runtime for reconcilePoolDeaths.
func legacyOnDeathRuntime(sp runtime.Provider, handlers map[string]poolDeathInfo, hooks *hookRecorder) *CityRuntime {
	cr := &CityRuntime{sp: sp, poolDeathHookRunner: hooks.run, stderr: io.Discard}
	cr.publishPoolDeathHandlers(handlers)
	return cr
}

// startOnDeathWorkerInBubble starts the worker inside the current bubble; the
// cleanup stops it and waits for it.
func startOnDeathWorkerInBubble(t *testing.T, cr *CityRuntime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := cr.startOnDeathWorker(ctx, cr.inventoryLane)
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// passAndDrain runs one lane pass and waits until the worker is idle.
func passAndDrain(cr *CityRuntime) {
	runTestInventoryPass(cr)
	synctest.Wait()
}

func completePass(seq uint64, names ...string) InventoryPass {
	return InventoryPass{Seq: seq, MergedNames: names, Backends: []BackendPass{{Outcome: OutcomeComplete, Attested: true, Names: names}}}
}

func edgeNames(edges []deathEdge) []string {
	out := make([]string, len(edges))
	for i, e := range edges {
		out[i] = e.name
	}
	return out
}

// Kills: firing for a name that is not a handler name, and a missed edge.
func TestInventoryOnDeath_EdgeOnlyForHandlerNames(t *testing.T) {
	handlers := onDeathHandlers("worker-1", "worker-2")
	attrs := map[string]InventoryAttrs{"worker-1": {Incarnation: "w1:1"}}
	_, prev := detectPoolDeathEdges(nil, completePass(1, "worker-1", "worker-2", "dog-gc-42"), attrs, handlers)
	if _, ok := prev["dog-gc-42"]; ok {
		t.Fatalf("prev = %v, want only handler names tracked", prev)
	}
	edges, _ := detectPoolDeathEdges(prev, completePass(2, "worker-2"), nil, handlers)
	if got := edgeNames(edges); !reflect.DeepEqual(got, []string{"worker-1"}) {
		t.Fatalf("edges = %v, want [worker-1] (dog-gc-42 is a bead-scoped name with no handler)", got)
	}
	if e := edges[0]; e.incarnation != "w1:1" || e.seq != 2 || e.info.Command != "release worker-1" {
		t.Fatalf("edge = %+v, want the last listed incarnation, the detecting pass and the handler", e)
	}
}

// Kills: prev updated on a partial or failed listing (MAINT-022).
func TestInventoryOnDeath_PartialOrFailedSkipsWithoutUpdatingPrev(t *testing.T) {
	handlers := onDeathHandlers("worker-1")
	_, prev := detectPoolDeathEdges(nil, completePass(1, "worker-1"), nil, handlers)
	partial := &runtime.PartialListError{Err: errors.New("no tmux server"), ServerAbsent: true}
	for _, pass := range []InventoryPass{
		{Seq: 2, MergedErr: partial, Backends: []BackendPass{{Outcome: OutcomePartial, Attested: true, Err: partial, ServerAbsent: true}}},
		{Seq: 3, MergedErr: errors.New("down"), Backends: []BackendPass{{Outcome: OutcomeFailed, Attested: true, Err: errors.New("down")}}},
	} {
		var edges []deathEdge
		edges, prev = detectPoolDeathEdges(prev, pass, nil, handlers)
		if len(edges) != 0 {
			t.Fatalf("pass %d: edges = %v, want none from a %s listing", pass.Seq, edgeNames(edges), pass.Backends[0].Outcome)
		}
		if _, ok := prev["worker-1"]; !ok {
			t.Fatalf("pass %d: prev lost worker-1 on a %s listing", pass.Seq, pass.Backends[0].Outcome)
		}
	}
	edges, _ := detectPoolDeathEdges(prev, completePass(4), nil, handlers)
	if got := edgeNames(edges); !reflect.DeepEqual(got, []string{"worker-1"}) {
		t.Fatalf("edges after the next complete listing = %v, want [worker-1]", got)
	}
}

// Kills: false edges at start (OQ-1 kept in PR-3).
func TestInventoryOnDeath_FirstPassDrawsNoEdges(t *testing.T) {
	edges, prev := detectPoolDeathEdges(nil, completePass(1), nil, onDeathHandlers("worker-1", "worker-2"))
	if len(edges) != 0 || len(prev) != 0 {
		t.Fatalf("first pass: edges %v prev %v, want none", edgeNames(edges), prev)
	}
}

// Kills: deaths concluded from a backend that cannot prove absence, and from
// a backend other than the one that listed the name. Under hybrid(tmux,
// ssh-like), the unattested remote's name never fires; the tmux name does.
func TestInventoryOnDeath_UnattestedOrPartialBackendDrawsNoEdge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		local := newScriptedInventoryProvider("worker-1")
		remote := &listOnlyProvider{Fake: runtime.NewFake(), names: []string{"worker-2"}, unattested: true}
		sp := sessionhybrid.New(local, remote, func(string) bool { return false })
		cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1", "worker-2"))
		startOnDeathWorkerInBubble(t, cr)

		passAndDrain(cr)
		remote.names = nil
		passAndDrain(cr)
		if got := hooks.commands(); len(got) != 0 {
			t.Fatalf("hooks = %v, want none for a name gone from an unattested backend", got)
		}
		local.setListing(&runtime.PartialListError{Err: errors.New("no tmux server"), ServerAbsent: true})
		passAndDrain(cr)
		if got := hooks.commands(); len(got) != 0 {
			t.Fatalf("hooks = %v, want none from a partial listing", got)
		}
		local.setListing(nil)
		passAndDrain(cr)
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
			t.Fatalf("hooks = %v, want only the attested tmux name", got)
		}
	})
}

// Kills: a death in the last cadence before a reload never firing. A
// reload rebuilds the handler map from the instances running at that moment
// (computePoolDeathHandlers), so an unlimited pool instance that died just
// before it is gone from the new map. The tick checked deaths before
// reloading and fired; the lane fires with the hook it stored at the last
// sighting. A name still listed but dropped from the map stops being
// tracked, and a later death of it fires nothing.
func TestInventoryOnDeath_DeathJustBeforeReloadStillFires(t *testing.T) {
	before := onDeathHandlers("worker-1", "worker-2")
	after := map[string]poolDeathInfo{} // neither instance is a handler after the reload

	legacyHooks := &hookRecorder{}
	legacySp := newScriptedInventoryProvider("worker-1", "worker-2")
	legacy := legacyOnDeathRuntime(legacySp, before, legacyHooks)
	var prev map[string]bool
	legacy.reconcilePoolDeaths(&prev)
	legacySp.setListing(nil, "worker-2")
	legacy.reconcilePoolDeaths(&prev) // the tick checks deaths, then reloads
	legacy.publishPoolDeathHandlers(after)
	if got := legacyHooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
		t.Fatalf("tick hooks = %v, want worker-1 (deaths are checked before the reload)", got)
	}

	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1", "worker-2")
		cr, hooks, _ := onDeathLaneRuntime(t, sp, before)
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)

		sp.setListing(nil, "worker-2") // worker-1 dies...
		cr.publishPoolDeathHandlers(after)
		passAndDrain(cr) // ...and the reload lands before the lane's next pass
		if got := hooks.snapshot(); len(got) != 1 || !reflect.DeepEqual(got[0], recordedHook{Command: "release worker-1", Dir: "/city", Env: map[string]string{"GC_AGENT": "worker-1"}}) {
			t.Fatalf("lane hooks = %+v, want worker-1's stored hook", got)
		}

		sp.setListing(nil)
		passAndDrain(cr)
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
			t.Fatalf("lane hooks = %v, want nothing for worker-2: it was listed after leaving the map", got)
		}
	})
}

// Kills: a stale hook after reload for a name that is still a handler, and a
// lost death for one the reload dropped. worker-1's command changes on
// reload before it dies: the new hook runs. worker-2 leaves the map and dies
// in the same cadence: its stored hook runs.
func TestInventoryOnDeath_UsesHandlerMapPublishedAtDetection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1", "worker-2")
		cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1", "worker-2"))
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)

		reloaded := map[string]poolDeathInfo{"worker-1": {Command: "release worker-1 --v2", Dir: "/city"}}
		cr.publishPoolDeathHandlers(reloaded)
		sp.setListing(nil)
		passAndDrain(cr)
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1 --v2", "release worker-2"}) {
			t.Fatalf("hooks = %v, want worker-1's reloaded hook and worker-2's stored one", got)
		}
	})
}

// Kills: a hook after a restart (the resume hazard). The name is listed again
// by the time its hook is due, so the fresh re-check skips it.
func TestInventoryOnDeath_SkipsWhenNamePresentAtExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1")
		cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1"))
		passAndDrain(cr)
		sp.setListing(nil)
		passAndDrain(cr) // the edge is queued; no worker yet
		if !cr.onDeathGate().Pending("worker-1") {
			t.Fatal("worker-1 not held after its death was detected")
		}
		sp.setListing(nil, "worker-1") // restarted before its hook ran
		startOnDeathWorkerInBubble(t, cr)
		synctest.Wait()
		if got := hooks.commands(); len(got) != 0 {
			t.Fatalf("hooks = %v, want none for a name present at execution", got)
		}
		if cr.onDeathGate().Pending("worker-1") {
			t.Fatal("worker-1 still held after its skipped hook")
		}
	})
}

// Kills: firing on an erroring re-check, and never firing. The first error
// waits for the next pass; a re-check that then finds the name present skips,
// one that errors again fires on the listing that saw the name die.
func TestInventoryOnDeath_RevalidationErrorDefersThenFires(t *testing.T) {
	down := errors.New("list-sessions timed out")
	for _, tc := range []struct {
		name      string
		second    func(*scriptedInventoryProvider)
		wantHooks []string
	}{
		{"error twice fires", func(*scriptedInventoryProvider) {}, []string{"release worker-1"}},
		{"error then absent fires", func(p *scriptedInventoryProvider) { p.setListing(nil) }, []string{"release worker-1"}},
		{"error then present skips", func(p *scriptedInventoryProvider) { p.setListing(nil, "worker-1") }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sp := newScriptedInventoryProvider("worker-1")
				cr, hooks, stderr := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1"))
				passAndDrain(cr)
				sp.setListing(nil)
				passAndDrain(cr)
				sp.setListing(down) // the re-check errors
				startOnDeathWorkerInBubble(t, cr)
				synctest.Wait()
				if got := hooks.commands(); len(got) != 0 {
					t.Fatalf("hooks after one erroring re-check = %v, want none", got)
				}
				if !cr.onDeathGate().Pending("worker-1") {
					t.Fatal("worker-1 released while its re-check is pending")
				}
				if !strings.Contains(stderr.String(), "on_death worker-1: re-check failed, retrying next pass") {
					t.Fatalf("stderr = %q, want the retry reported", stderr.String())
				}
				tc.second(sp)
				passAndDrain(cr) // the next pass requeues the edge
				if got := hooks.commands(); !slices.Equal(got, tc.wantHooks) {
					t.Fatalf("hooks = %v, want %v", got, tc.wantHooks)
				}
				if cr.onDeathGate().Pending("worker-1") {
					t.Fatal("worker-1 still held after its edge settled")
				}
			})
		})
	}
}

// Kills: the tick or the lane blocked by a hook, and hooks run in parallel.
// While a hook is held running, lane passes keep publishing, the other
// deaths wait their turn, and at most one hook ever runs.
func TestInventoryOnDeath_HooksSerialAndOffTheTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1", "worker-2", "worker-3")
		cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1", "worker-2", "worker-3"))
		hooks.gate = make(chan struct{})
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)
		sp.setListing(nil)
		passAndDrain(cr)

		seq := cr.inventoryLane.cache.Snapshot().PassSeq
		passAndDrain(cr) // the lane is not blocked by the running hook
		if got := cr.inventoryLane.cache.Snapshot().PassSeq; got != seq+1 {
			t.Fatalf("pass seq = %d while a hook runs, want %d", got, seq+1)
		}
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
			t.Fatalf("hooks started = %v, want only the first while it runs", got)
		}
		for range 3 {
			hooks.gate <- struct{}{}
			synctest.Wait()
		}
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1", "release worker-2", "release worker-3"}) {
			t.Fatalf("hooks = %v, want every death in detection order", got)
		}
		if hooks.maxRunning != 1 {
			t.Fatalf("max concurrent hooks = %d, want 1", hooks.maxRunning)
		}
	})
}

// Kills: lost gc-recovery diagnostics, and a reworded hook error. The lane
// reports a hook with the tick's exact lines.
func TestInventoryOnDeath_OutputHandlingMatchesLegacy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1")
		cr, hooks, stderr := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1"))
		hooks.out = "released 0 of 1\n" + config.RecoveryHookMarker + " bd update gc-1 failed\n"
		hooks.err = errors.New("exit status 1")
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)
		sp.setListing(nil)
		passAndDrain(cr)
		want := "on_death worker-1: exit status 1\n" +
			"on_death worker-1: released 0 of 1\n" + config.RecoveryHookMarker + " bd update gc-1 failed\n"
		if got := stderr.String(); got != want {
			t.Fatalf("stderr = %q, want exactly %q", got, want)
		}
	})
}

// Kills: double firing, and no firing without a lane. A runtime without a
// lane fires from the tick; one with a lane never does, and its lane fires
// the same death once.
func TestCityRuntimeTick_PoolDeathCheckOwnedByLaneOrTick(t *testing.T) {
	newRuntime := func(t *testing.T) (*CityRuntime, *hookRecorder) {
		hooks := &hookRecorder{}
		cr := &CityRuntime{
			cityPath: t.TempDir(),
			cityName: "test-city",
			cfg: &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Daemon:    config.DaemonConfig{PatrolInterval: inventoryLaneInterval.String()},
			},
			sp:                  newScriptedInventoryProvider(),
			standaloneCityStore: beads.NewMemStore(),
			poolDeathHookRunner: hooks.run,
			rec:                 events.Discard,
			logPrefix:           "test-city",
			stdout:              io.Discard,
			stderr:              io.Discard,
			buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
				return DesiredStateResult{State: map[string]TemplateParams{}}
			},
		}
		cr.publishPoolDeathHandlers(onDeathHandlers("worker-1"))
		return cr, hooks
	}
	tick := func(cr *CityRuntime, prev map[string]bool) {
		var dirty atomic.Bool
		var lastProviderName string
		cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prev, "test")
	}

	t.Run("no lane: the tick fires", func(t *testing.T) {
		cr, hooks := newRuntime(t)
		tick(cr, map[string]bool{"worker-1": true})
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
			t.Fatalf("hooks = %v, want the tick to fire", got)
		}
	})
	t.Run("lane: only the lane fires", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cr, hooks := newRuntime(t)
			sp := cr.sp.(*scriptedInventoryProvider)
			sp.setListing(nil, "worker-1")
			if cr.initRuntimeInventoryLane() == nil {
				t.Fatal("no lane")
			}
			startOnDeathWorkerInBubble(t, cr)
			passAndDrain(cr)
			sp.setListing(nil)
			tick(cr, map[string]bool{"worker-1": true})
			synctest.Wait()
			if got := hooks.commands(); len(got) != 0 {
				t.Fatalf("hooks after the tick = %v, want none: the lane owns on_death", got)
			}
			passAndDrain(cr)
			if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
				t.Fatalf("hooks = %v, want the lane to fire once", got)
			}
		})
	})
}

// Kills: hooks for shutdown stops. Canceling the worker drops every queued
// edge and releases its name; the running hook finishes.
func TestInventoryOnDeath_ShutdownDropsQueuedEdges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1", "worker-2", "worker-3")
		cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1", "worker-2", "worker-3"))
		hooks.gate = make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		done := cr.startOnDeathWorker(ctx, cr.inventoryLane)
		passAndDrain(cr)
		sp.setListing(nil)
		passAndDrain(cr) // worker-1 running, worker-2 and worker-3 queued

		listings := sp.calls()
		cancel()
		hooks.gate <- struct{}{}
		<-done
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
			t.Fatalf("hooks = %v, want only the hook that was running", got)
		}
		if got := sp.calls() - listings; got != 0 {
			t.Fatalf("re-check listings after shutdown = %d, want the queued edges dropped unexamined", got)
		}
		if hooks.finished != 1 {
			t.Fatalf("finished hooks = %d, want the running hook to complete", hooks.finished)
		}
		for _, n := range []string{"worker-1", "worker-2", "worker-3"} {
			if cr.onDeathGate().Pending(n) {
				t.Fatalf("%s still held after shutdown", n)
			}
		}
	})
}

// Kills: prev reset on a provider swap (legacy parity). The reload stops the
// old provider's sessions; the new provider's first complete pass fires for
// them even though its backend labels differ, and a partial one does not.
func TestInventoryOnDeath_ProviderSwapFiresForStoppedNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1")
		cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1"))
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)

		acp := &listOnlyProvider{Fake: runtime.NewFake(), err: &runtime.PartialListError{Err: errors.New("one socket unanswered")}}
		swapped := sessionauto.New(newScriptedInventoryProvider(), acp)
		cr.serviceStateMu.Lock()
		cr.sp = swapped
		cr.serviceStateMu.Unlock()
		passAndDrain(cr)
		if got := hooks.commands(); len(got) != 0 {
			t.Fatalf("hooks = %v, want none while a new backend lists partially", got)
		}
		acp.err = nil
		passAndDrain(cr)
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
			t.Fatalf("hooks = %v, want the swapped-out name to fire", got)
		}
	})
}

// Kills: no latency win. A name the lane proves gone pokes the reconciler for
// that session; a name gone from an unattested listing does not.
func TestInventoryOnDeath_DeathPokesController(t *testing.T) {
	local := newScriptedInventoryProvider("gc-a")
	remote := &listOnlyProvider{Fake: runtime.NewFake(), names: []string{"gc-ssh"}, unattested: true}
	cr := inventoryLaneTestRuntime(t, sessionhybrid.New(local, remote, func(string) bool { return false }), nil)
	cr.pokeCh = make(chan struct{}, 1)
	poked := func() bool {
		select {
		case <-cr.pokeCh:
			return true
		default:
			return false
		}
	}

	runTestInventoryPass(cr)
	if poked() {
		t.Fatal("poked on a pass with no deaths")
	}
	remote.names = nil
	runTestInventoryPass(cr)
	if poked() {
		t.Fatal("poked for a name gone from an unattested listing")
	}
	local.setListing(nil)
	runTestInventoryPass(cr)
	if !poked() {
		t.Fatal("no poke for a death the lane proved")
	}
}

// Kills: a start racing its name's hook, and a deferral that writes state,
// spends wake budget or takes the endpoint's probe ticket.
func TestExecutePlannedStarts_DefersNameWithPendingOnDeathHook(t *testing.T) {
	e := newCapacityEnv(t, true, "a", "c")
	e.limitToOneWake()
	gate := newOnDeathGate()
	gate.enqueue([]deathEdge{{name: "a"}})
	e.startOptions = append(e.startOptions, withOnDeathGate(gate))
	a := e.pendingCreate(t, "a", "e", nil)
	c := e.pendingCreate(t, "c", "f", nil)
	e.openEndpoint(t)
	e.makeProbeDue()
	before := e.bead(t, a.info.ID)

	if woken := e.start(context.Background(), a, c); woken != 1 {
		t.Fatalf("woken = %d, want 1 (c within max_wakes=1)", woken)
	}
	if e.sp.CountCalls("Start", "a") != 0 || e.sp.CountCalls("Start", "c") != 1 {
		t.Fatalf("Start calls = %+v, want only c", e.sp.SnapshotCalls())
	}
	if after := e.bead(t, a.info.ID); !maps.Equal(after.Metadata, before.Metadata) {
		t.Fatalf("deferred row changed:\nbefore %v\nafter  %v", before.Metadata, after.Metadata)
	}
	if ticket, ok := e.guard.Admit(capacityTestEndpoint, "outside", "outside"); !ok || !ticket.probe {
		t.Fatal("the deferred start took the endpoint's probe ticket")
	} else {
		ticket.Resolve(verdictCapacity)
	}
	if !strings.Contains(e.log.String(), "outcome=deferred_by_on_death_hook") {
		t.Fatalf("stderr = %q, want deferred_by_on_death_hook", e.log.String())
	}
	var deferred bool
	for _, r := range e.records(TraceSiteLifecycleStartRun) {
		if r.OutcomeCode == TraceOutcomeDeferredByOnDeathHook && r.ReasonCode == TraceReasonOnDeathHookPending && r.SessionName == "a" {
			deferred = true
		}
	}
	if !deferred {
		t.Fatal("no deferred_by_on_death_hook trace record for a")
	}
	if st := e.breakerStatus(capacityTestEndpoint); st.State != resilience.StateOpen {
		t.Fatalf("endpoint state = %v, want open (re-tripped by the outside probe)", st.State)
	}
}

// The ordering race the interlock exists for: a death is detected, a restart
// of the same name is attempted while its hook is queued and then while it
// runs, and the restart goes ahead only after the hook completes. The
// release pokes the reconciler, so the deferred start does not wait a patrol.
func TestInventoryOnDeath_RestartDeferredUntilHookCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newCapacityEnv(t, false, "ant", "sky")
		ctx := context.Background()
		for _, n := range []string{"ant", "sky"} {
			if err := e.sp.Start(ctx, n, runtime.Config{Command: "test-cmd"}); err != nil {
				t.Fatalf("start %s: %v", n, err)
			}
		}
		cr, hooks, _ := onDeathLaneRuntime(t, e.sp, onDeathHandlers("ant", "sky"))
		cr.pokeCh = make(chan struct{}, 1)
		hooks.gate = make(chan struct{})
		e.startOptions = append(e.startOptions, withOnDeathGate(cr.onDeathGate()))
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)
		for _, n := range []string{"ant", "sky"} {
			if err := e.sp.Stop(n); err != nil {
				t.Fatalf("stop %s: %v", n, err)
			}
		}
		passAndDrain(cr) // ant's hook runs (held), sky's is queued
		<-cr.pokeCh      // the death poke

		sky := e.pendingCreate(t, "sky", "f", nil)
		startsBefore := e.sp.CountCalls("Start", "sky")
		e.start(ctx, sky)
		if n := e.sp.CountCalls("Start", "sky"); n != startsBefore {
			t.Fatal("sky restarted while its hook was queued")
		}
		hooks.gate <- struct{}{} // ant's hook finishes; sky's starts
		synctest.Wait()
		e.start(ctx, e.refreshed(sky))
		if n := e.sp.CountCalls("Start", "sky"); n != startsBefore {
			t.Fatal("sky restarted while its hook was running")
		}
		hooks.gate <- struct{}{} // sky's hook finishes
		synctest.Wait()
		if cr.onDeathGate().Pending("sky") {
			t.Fatal("sky still held after its hook completed")
		}
		select {
		case <-cr.pokeCh:
		default:
			t.Fatal("releasing sky did not poke the reconciler")
		}
		e.start(ctx, e.refreshed(sky))
		if n := e.sp.CountCalls("Start", "sky"); n != startsBefore+1 {
			t.Fatalf("sky Start calls = %d after its hook completed, want %d", n, startsBefore+1)
		}
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release ant", "release sky"}) {
			t.Fatalf("hooks = %v, want both deaths released before the restart", got)
		}
	})
}

// onDeathStep is one provider state in a differential fixture.
type onDeathStep struct {
	names []string
	err   error
}

// The differential: on fixtures where both rules agree (one attested
// backend), the lane fires exactly the hooks the tick fires, step by step,
// with the same command, directory and environment. The tick runs
// reconcilePoolDeaths once per step; the lane runs one pass per step and
// drains its worker, re-check included.
func TestInventoryOnDeath_LaneMatchesTickOnSameFixtures(t *testing.T) {
	partial := &runtime.PartialListError{Err: errors.New("no tmux server"), ServerAbsent: true}
	down := errors.New("list-sessions timed out")
	rigEnv := map[string]string{"GC_DOLT_PORT": "3308", "GC_DOLT_USER": "rig-user", "GC_DOLT_PASSWORD": "rig-secret"}
	handlers := onDeathHandlers("worker-1", "worker-2")
	handlers["worker-2"] = poolDeathInfo{Command: "release worker-2", Dir: "/city/demo", Env: rigEnv}
	fixtures := []struct {
		name  string
		steps []onDeathStep
	}{
		{"deaths after complete listings", []onDeathStep{{names: []string{"worker-1", "worker-2"}}, {names: []string{"worker-2"}}, {}}},
		{"partial listing skips and keeps prev", []onDeathStep{{names: []string{"worker-1"}}, {err: partial}, {}}},
		{"failed listing skips and keeps prev", []onDeathStep{{names: []string{"worker-1", "worker-2"}}, {err: down}, {names: []string{"worker-1"}}}},
		{"restart between listings draws nothing", []onDeathStep{{names: []string{"worker-1"}}, {names: []string{"worker-1"}}, {names: []string{"worker-1"}}}},
		{"die, restart, die again", []onDeathStep{{names: []string{"worker-1"}}, {}, {names: []string{"worker-1"}}, {}}},
		{"non-handler names never fire", []onDeathStep{{names: []string{"worker-1", "dog-gc-42"}}, {names: []string{"worker-1"}}}},
		{"first listing draws no edges", []onDeathStep{{}, {names: []string{"worker-2"}}, {}}},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			legacyHooks := &hookRecorder{}
			legacySp := newScriptedInventoryProvider()
			legacy := legacyOnDeathRuntime(legacySp, handlers, legacyHooks)
			var prev map[string]bool
			legacyBySteps := make([][]recordedHook, len(fx.steps))
			for i, step := range fx.steps {
				legacySp.setListing(step.err, step.names...)
				before := len(legacyHooks.snapshot())
				legacy.reconcilePoolDeaths(&prev)
				legacyBySteps[i] = sortedHooks(legacyHooks.snapshot()[before:])
			}

			synctest.Test(t, func(t *testing.T) {
				sp := newScriptedInventoryProvider()
				cr, hooks, _ := onDeathLaneRuntime(t, sp, handlers)
				startOnDeathWorkerInBubble(t, cr)
				for i, step := range fx.steps {
					sp.setListing(step.err, step.names...)
					before := len(hooks.snapshot())
					passAndDrain(cr)
					got := sortedHooks(hooks.snapshot()[before:])
					if !reflect.DeepEqual(got, legacyBySteps[i]) {
						t.Fatalf("step %d (%v, %v): lane hooks %+v, tick hooks %+v", i, step.names, step.err, got, legacyBySteps[i])
					}
				}
			})
		})
	}
}

// The deltas from the tick, each by design (the observation cache's rule
// instead of the tick's rule over the merged listing): no hook for a name
// gone from an unattested backend, which the tick fires on; and a hook for a
// death the tick cannot see, either because the name was only ever seen on a
// partial listing or because another backend listed partially.
func TestInventoryOnDeath_DeltasFromTheTick(t *testing.T) {
	partial := &runtime.PartialListError{Err: errors.New("one socket unanswered")}
	unattested := func() runtime.Provider {
		p := newScriptedInventoryProvider()
		p.unattested = true
		return p
	}
	for _, tc := range []struct {
		name       string
		sp         func() runtime.Provider
		steps      func(runtime.Provider) []func()
		tick, lane []string
	}{
		{
			name: "unattested backend: the tick fires, the lane does not",
			sp:   unattested,
			steps: func(sp runtime.Provider) []func() {
				p := sp.(*scriptedInventoryProvider)
				return []func(){func() { p.setListing(nil, "worker-1") }, func() { p.setListing(nil) }}
			},
			tick: []string{"release worker-1"},
		},
		{
			name: "seen only on a partial listing: the lane fires, the tick cannot",
			sp:   func() runtime.Provider { return newScriptedInventoryProvider() },
			steps: func(sp runtime.Provider) []func() {
				p := sp.(*scriptedInventoryProvider)
				return []func(){func() { p.setListing(nil) }, func() { p.setListing(partial, "worker-1") }, func() { p.setListing(nil) }}
			},
			lane: []string{"release worker-1"},
		},
		{
			name: "another backend partial: the lane fires for tmux, the tick skips",
			sp: func() runtime.Provider {
				return sessionauto.New(newScriptedInventoryProvider(), &listOnlyProvider{Fake: runtime.NewFake()})
			},
			steps: func(sp runtime.Provider) []func() {
				bs := sp.(runtime.BackendsProvider).Backends()
				tmux, acp := bs[0].Provider.(*scriptedInventoryProvider), bs[1].Provider.(*listOnlyProvider)
				return []func(){
					func() { tmux.setListing(nil, "worker-1") },
					func() { tmux.setListing(nil); acp.err = partial },
					// The re-check lists through the composite, which is still
					// partial: the hook waits one pass, then fires.
					func() {},
				}
			},
			lane: []string{"release worker-1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handlers := onDeathHandlers("worker-1")
			legacyHooks := &hookRecorder{}
			legacySp := tc.sp()
			legacy := legacyOnDeathRuntime(legacySp, handlers, legacyHooks)
			var prev map[string]bool
			for _, step := range tc.steps(legacySp) {
				step()
				legacy.reconcilePoolDeaths(&prev)
			}
			if got := legacyHooks.commands(); !slices.Equal(got, tc.tick) {
				t.Fatalf("tick hooks = %v, want %v", got, tc.tick)
			}
			synctest.Test(t, func(t *testing.T) {
				sp := tc.sp()
				cr, hooks, _ := onDeathLaneRuntime(t, sp, handlers)
				startOnDeathWorkerInBubble(t, cr)
				for _, step := range tc.steps(sp) {
					step()
					passAndDrain(cr)
				}
				if got := hooks.commands(); !slices.Equal(got, tc.lane) {
					t.Fatalf("lane hooks = %v, want %v", got, tc.lane)
				}
			})
		})
	}
}

func sortedHooks(h []recordedHook) []recordedHook {
	out := slices.Clone(h)
	sort.Slice(out, func(i, j int) bool { return out[i].Command < out[j].Command })
	if len(out) == 0 {
		return nil
	}
	return out
}

// Kills: a duplicate edge for the same death running the hook twice, and a
// second death of a name hidden behind its first, already running hook.
func TestOnDeathGate_DeduplicatesByNameAndIncarnation(t *testing.T) {
	g := newOnDeathGate()
	g.enqueue([]deathEdge{{name: "w", incarnation: "w:1"}, {name: "w", incarnation: "w:1"}, {name: "x"}})
	g.enqueue([]deathEdge{{name: "x"}})
	if len(g.queue) != 2 {
		t.Fatalf("queue = %d edges, want duplicates of queued deaths dropped", len(g.queue))
	}
	e, _ := g.take()
	g.enqueue([]deathEdge{{name: "w", incarnation: "w:1"}, {name: "w", incarnation: "w:2"}})
	if got := fmt.Sprint(len(g.queue), g.held["w"]); got != "2 2" {
		t.Fatalf("queue len, holds on w = %s, want the running death deduplicated and the new incarnation queued", got)
	}
	if g.finish(e) {
		t.Fatal("w released while its second death is queued")
	}

	// x carries no incarnation: a death reported while its hook runs may be
	// a second one, so it is queued, not folded into the running hook.
	x, _ := g.take() // queue: w:2
	g.enqueue([]deathEdge{{name: "x"}})
	if len(g.queue) != 2 || g.held["x"] != 2 {
		t.Fatalf("queue = %d, holds on x = %d; want a death without an incarnation queued behind its running hook", len(g.queue), g.held["x"])
	}
	if g.finish(x) {
		t.Fatal("x released while its second death is queued")
	}
}

// recheckErrProvider lists normally for the lane and fails every exact-name
// re-check.
type recheckErrProvider struct {
	*scriptedInventoryProvider
	err error
	// only, when set, limits the failing re-checks to that name.
	only string
}

func (p *recheckErrProvider) ListRunning(prefix string) ([]string, error) {
	if prefix != "" && (p.only == "" || prefix == p.only) {
		return nil, p.err
	}
	return p.scriptedInventoryProvider.ListRunning(prefix)
}

// Kills: firing on the second erroring re-check although the pass that
// requeued the edge listed the name again (a restart the re-check could not
// see). A requeuing pass that did not list it still fires.
func TestInventoryOnDeath_SecondRecheckErrorSkipsWhenRequeuingPassListedName(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requeue   []string
		wantHooks []string
	}{
		{"requeuing pass lists the name: skipped", []string{"worker-1"}, nil},
		{"requeuing pass omits the name: fired", nil, []string{"release worker-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				scripted := newScriptedInventoryProvider("worker-1")
				sp := &recheckErrProvider{scriptedInventoryProvider: scripted, err: errors.New("list-sessions timed out")}
				cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1"))
				startOnDeathWorkerInBubble(t, cr)
				passAndDrain(cr)
				scripted.setListing(nil)
				passAndDrain(cr) // the death; the first re-check errors
				scripted.setListing(nil, tc.requeue...)
				passAndDrain(cr) // requeues; the second re-check errors
				if got := hooks.commands(); !slices.Equal(got, tc.wantHooks) {
					t.Fatalf("hooks = %v, want %v", got, tc.wantHooks)
				}
				if cr.onDeathGate().Pending("worker-1") {
					t.Fatal("worker-1 still held after its edge settled")
				}
			})
		})
	}
}

// Kills: a re-check without a deadline, and re-checks piling up behind a
// hung listing. A re-check that outlives inventoryListingBound is an error;
// the next one, while the first call is still outstanding, errors at once
// without a second call.
func TestInventoryOnDeath_RecheckIsBoundedAndSingleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1")
		cr, hooks, stderr := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1"))
		passAndDrain(cr)
		sp.setListing(nil)
		passAndDrain(cr) // the edge is queued; no worker yet
		gate := make(chan struct{})
		defer close(gate)
		sp.mu.Lock()
		sp.listGate = gate
		sp.mu.Unlock()
		callsBefore := sp.calls()

		startOnDeathWorkerInBubble(t, cr)
		synctest.Wait() // the re-check is hung
		advanceInventoryLane(inventoryListingBound)
		if !strings.Contains(stderr.String(), "re-check listing exceeded") {
			t.Fatalf("stderr = %q, want the re-check to time out", stderr.String())
		}
		if len(hooks.commands()) != 0 || !cr.onDeathGate().Pending("worker-1") {
			t.Fatal("the hook ran, or worker-1 was released, after one timed-out re-check")
		}
		passAndDrain(cr) // its own listing times out too, then requeues the edge
		if got := sp.calls() - callsBefore; got != 2 {
			t.Fatalf("listing calls = %d, want 2 (the hung re-check and the pass); a second re-check must not call", got)
		}
		if !strings.Contains(stderr.String(), errOnDeathRecheckBusy.Error()) {
			t.Fatalf("stderr = %q, want the second re-check refused while the first is outstanding", stderr.String())
		}
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
			t.Fatalf("hooks = %v, want the hook on the second re-check error", got)
		}
	})
}

// Kills: a prefix match in the re-check. worker-10 is alive, worker-1 is
// dead; ListRunning("worker-1") returns worker-10, which must not count as
// worker-1 present.
func TestInventoryOnDeath_RecheckMatchesExactName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := runtime.NewFake()
		for _, n := range []string{"worker-1", "worker-10"} {
			if err := sp.Start(context.Background(), n, runtime.Config{Command: "agent"}); err != nil {
				t.Fatal(err)
			}
		}
		cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1", "worker-10"))
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)
		if err := sp.Stop("worker-1"); err != nil {
			t.Fatal(err)
		}
		passAndDrain(cr)
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
			t.Fatalf("hooks = %v, want worker-1 to fire while worker-10 lives", got)
		}
	})
}

// Kills: a lane started without its worker, and a stop that does not wait
// for a running hook. The lane started the way run() starts it fires a
// death on its own cadence, and its stop returns only after the hook that
// is running finishes.
func TestRuntimeInventoryLane_StartsAndJoinsOnDeathWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1", "worker-2")
		cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1", "worker-2"))
		stop := cr.runRuntimeInventoryLane(context.Background())
		advanceInventoryLane(inventoryLaneInterval)
		sp.setListing(nil, "worker-2")
		advanceInventoryLane(inventoryLaneInterval)
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
			t.Fatalf("hooks = %v, want the lane's worker to fire worker-1", got)
		}

		hooks.mu.Lock()
		hooks.gate = make(chan struct{})
		hooks.mu.Unlock()
		sp.setListing(nil)
		advanceInventoryLane(inventoryLaneInterval) // worker-2's hook runs, held
		stopped := make(chan struct{})
		go func() {
			stop()
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("stop returned while an on_death hook was running")
		default:
		}
		hooks.gate <- struct{}{}
		synctest.Wait()
		select {
		case <-stopped:
		default:
			t.Fatal("stop did not return after the hook finished")
		}
	})
}

// Kills: the API never learning the runtime's start interlock. Starting the
// lane publishes its gate to the controller state the API reads.
func TestInitRuntimeInventoryLane_PublishesOnDeathGateToAPIState(t *testing.T) {
	cs := &controllerState{cityBeadStore: beads.NewMemStore()}
	cr := &CityRuntime{
		cfg:                 &config.City{Daemon: config.DaemonConfig{PatrolInterval: inventoryLaneInterval.String()}},
		sp:                  newScriptedInventoryProvider(),
		standaloneCityStore: beads.NewMemStore(),
		stderr:              io.Discard,
	}
	cr.setControllerState(cs)
	if cs.OnDeathHookPending("worker-1") {
		t.Fatal("pending before any lane")
	}
	if cr.initRuntimeInventoryLane() == nil {
		t.Fatal("no lane")
	}
	cr.onDeathGate().enqueue([]deathEdge{{name: "worker-1"}})
	if !cs.OnDeathHookPending("worker-1") {
		t.Fatal("the API state does not see the lane's held name")
	}
}

// Kills: a city whose handler names live only on an unattested backend
// losing on_death silently, and a notice repeated every pass.
func TestInventoryOnDeath_NoticesUnattestedHandlerBackendOnce(t *testing.T) {
	local := newScriptedInventoryProvider("gc-a")
	remote := &listOnlyProvider{Fake: runtime.NewFake(), names: []string{"worker-1"}, unattested: true}
	cr, _, stderr := onDeathLaneRuntime(t, sessionhybrid.New(local, remote, func(string) bool { return false }), onDeathHandlers("worker-1"))
	runTestInventoryPass(cr)
	runTestInventoryPass(cr)
	const notice = "on_death: backend remote cannot prove a session gone"
	if got := strings.Count(stderr.String(), notice); got != 1 {
		t.Fatalf("notices = %d in %q, want exactly one", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "(worker-1 and any others)") {
		t.Fatalf("stderr = %q, want the handler name in the notice", stderr.String())
	}
}

// lastStartName is the session name of sp's latest Start call.
func lastStartName(t *testing.T, sp *capacityRefusingProvider) string {
	t.Helper()
	calls := sp.SnapshotCalls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == "Start" {
			return calls[i].Name
		}
	}
	t.Fatal("no Start call")
	return ""
}

// onDeathGateWiring runs a start path three times against an endpoint whose
// probe is due: once refused (to learn the session name), once with the name
// held by the runtime's on_death gate (no start), and once after its hook
// finished (the start goes ahead).
func onDeathGateWiring(t *testing.T, agentName, startCommand string, tick func(*CityRuntime)) {
	t.Helper()
	clk := &clock.Fake{Time: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	cr, sp := newCapacityRefusingRuntime(t, agentName, startCommand, newEndpointCapacityGuard(clk.Now))
	tick(cr)
	if sp.startCalls() != 1 {
		t.Fatalf("first tick Start calls = %d, want 1", sp.startCalls())
	}
	name := lastStartName(t, sp)
	cr.inventoryLane = newRuntimeInventoryLane(inventoryLaneInterval, io.Discard, "gc test")
	gate := cr.onDeathGate()
	gate.enqueue([]deathEdge{{name: name}})
	clk.Advance(cr.capacityGuard.breaker("upstream:broker").Status().BackoffCap)
	sp.refusing = false

	tick(cr)
	if sp.startCalls() != 1 {
		t.Fatalf("Start calls = %d with %s's on_death hook pending, want no new start", sp.startCalls(), name)
	}
	e, _ := gate.take()
	gate.finish(e)
	tick(cr)
	if sp.startCalls() != 2 {
		t.Fatalf("Start calls = %d after the hook finished, want the deferred start", sp.startCalls())
	}
}

// Kills: the main tick's starts not wired to the on_death interlock.
func TestCityRuntimeTick_UsesOnDeathGate(t *testing.T) {
	onDeathGateWiring(t, "worker", "worker-cmd", func(cr *CityRuntime) {
		dirty := &atomic.Bool{}
		lastProviderName := ""
		prev := make(map[string]bool)
		cr.tick(context.Background(), dirty, &lastProviderName, cr.cityPath, &prev, "poke")
		if !cr.waitForAsyncStarts() {
			t.Fatal("async starts did not settle after the main tick")
		}
		// The wait closes the tracker, as at shutdown; reopen it for the
		// next tick.
		cr.asyncStarts.mu.Lock()
		cr.asyncStarts.stopping = false
		cr.asyncStarts.mu.Unlock()
	})
}

// Kills: the control dispatcher's starts not wired to the on_death interlock.
func TestControlDispatcherTick_UsesOnDeathGate(t *testing.T) {
	onDeathGateWiring(t, config.ControlDispatcherAgentName, config.ControlDispatcherStartCommandFor("{{.Agent}}"), func(cr *CityRuntime) {
		cr.controlDispatcherTick(context.Background())
	})
}

// Kills: the tracked set growing without bound on an unattested backend, and
// a provider swap then drawing a burst of long-gone instances' hooks. Each
// reload's departed instance is dropped at once there, because that backend
// could never prove it gone; only the current handler survives to fire.
func TestInventoryOnDeath_UnattestedReloadCyclesDoNotAccumulate(t *testing.T) {
	unattestedPass := func(seq uint64, names ...string) InventoryPass {
		return InventoryPass{Seq: seq, MergedNames: names, Backends: []BackendPass{{Outcome: OutcomeUnattested, Names: names}}}
	}
	var prev map[string]poolDeathSighting
	seq := uint64(0)
	for i := range 200 {
		name := fmt.Sprintf("worker-%d", i)
		handlers := onDeathHandlers(name) // the reload captures the running instance
		seq++
		var edges []deathEdge
		edges, prev = detectPoolDeathEdges(prev, unattestedPass(seq, name), nil, handlers)
		if len(edges) != 0 {
			t.Fatalf("cycle %d: edges %v from an unattested listing", i, edgeNames(edges))
		}
		if len(prev) > 1 {
			t.Fatalf("cycle %d: tracking %d names, want at most the current handler", i, len(prev))
		}
	}
	seq++
	edges, _ := detectPoolDeathEdges(prev, completePass(seq), nil, onDeathHandlers("worker-199"))
	if got := edgeNames(edges); !reflect.DeepEqual(got, []string{"worker-199"}) {
		t.Fatalf("edges after swapping to an attested provider = %v, want only the current handler", got)
	}
}

// Kills: a re-check's second error judged on a stale listing. worker-1's
// edge is requeued by a pass that lists it (restarted), but before the
// worker gets to it a later pass shows it dead again (that death folds into
// the queued edge). The hook runs: the latest listing, not the requeuing
// one, decides.
func TestInventoryOnDeath_SecondRecheckErrorReadsLatestListing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scripted := newScriptedInventoryProvider("worker-1", "worker-2")
		sp := &recheckErrProvider{scriptedInventoryProvider: scripted, err: errors.New("list-sessions timed out"), only: "worker-1"}
		cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1", "worker-2"))
		hooks.gate = make(chan struct{})
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)
		scripted.setListing(nil)
		passAndDrain(cr) // worker-1's re-check errors; worker-2's hook runs, held
		scripted.setListing(nil, "worker-1")
		passAndDrain(cr) // requeues worker-1 while it is listed
		scripted.setListing(nil)
		passAndDrain(cr)  // worker-1 dead again; the new edge folds into the queued one
		close(hooks.gate) // release every hook
		synctest.Wait()
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-2", "release worker-1"}) {
			t.Fatalf("hooks = %v, want worker-1 fired on the latest listing", got)
		}
	})
}

// Kills: on_death executions invisible in gc trace. Every hook execution
// records a runtime_inventory.on_death operation with its outcome.
func TestInventoryOnDeath_RecordsTraceForEachExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1", "worker-2")
		cr, _, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1", "worker-2"))
		cr.trace = newSessionReconcilerTraceManager(cr.cityPath, "test-city", io.Discard)
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)
		sp.setListing(nil)
		passAndDrain(cr)               // worker-1 and worker-2 both die...
		sp.setListing(nil, "worker-3") // ...and worker-3's edge finds it present
		cr.inventoryLane.onDeath.enqueue([]deathEdge{{name: "worker-3", info: poolDeathInfo{Command: "x"}}})
		synctest.Wait()
		if err := cr.trace.Close(); err != nil {
			t.Fatal(err)
		}
		records, err := ReadTraceRecords(traceCityRuntimeDir(cr.cityPath), TraceFilter{})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, r := range records {
			if r.SiteCode == TraceSiteRuntimeInventoryOnDeath {
				got[fmt.Sprint(r.Fields["session_name"])] = fmt.Sprint(r.Fields["on_death_outcome"])
			}
		}
		want := map[string]string{"worker-1": onDeathOutcomeFired, "worker-2": onDeathOutcomeFired, "worker-3": onDeathOutcomeSkippedPresent}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("on_death trace outcomes = %v, want %v", got, want)
		}
	})
}

// panickingRecheckProvider lists normally and panics on every re-check.
type panickingRecheckProvider struct{ *scriptedInventoryProvider }

func (p *panickingRecheckProvider) ListRunning(prefix string) ([]string, error) {
	if prefix != "" {
		panic("re-check exploded")
	}
	return p.scriptedInventoryProvider.ListRunning(prefix)
}

// Kills: a panicking re-check crashing the controller. The panic is an
// erroring re-check: the edge waits, and the single-flight slot is freed.
func TestInventoryOnDeath_PanickingRecheckIsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scripted := newScriptedInventoryProvider("worker-1")
		cr, hooks, stderr := onDeathLaneRuntime(t, &panickingRecheckProvider{scripted}, onDeathHandlers("worker-1"))
		startOnDeathWorkerInBubble(t, cr)
		passAndDrain(cr)
		scripted.setListing(nil)
		passAndDrain(cr)
		if !strings.Contains(stderr.String(), "re-check failed, retrying next pass: re-check listing panicked") {
			t.Fatalf("stderr = %q, want the panic reported as a re-check error", stderr.String())
		}
		passAndDrain(cr) // the second panic is a second error, not "still outstanding"
		if strings.Contains(stderr.String(), errOnDeathRecheckBusy.Error()) {
			t.Fatalf("stderr = %q: the panicked re-check kept its slot", stderr.String())
		}
		if got := hooks.commands(); !reflect.DeepEqual(got, []string{"release worker-1"}) {
			t.Fatalf("hooks = %v, want the hook after two erroring re-checks", got)
		}
	})
}

// Kills: shutdown waiting out a hung re-check. Canceling the worker returns
// at once, without the hook and without waiting for inventoryListingBound.
func TestInventoryOnDeath_ShutdownDoesNotWaitOnHungRecheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("worker-1")
		cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1"))
		passAndDrain(cr)
		sp.setListing(nil)
		passAndDrain(cr)
		gate := make(chan struct{})
		defer close(gate)
		sp.mu.Lock()
		sp.listGate = gate
		sp.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		done := cr.startOnDeathWorker(ctx, cr.inventoryLane)
		synctest.Wait() // the re-check is hung
		started := time.Now()
		cancel()
		<-done
		if waited := time.Since(started); waited != 0 {
			t.Fatalf("shutdown waited %s on a hung re-check", waited)
		}
		if got := hooks.commands(); len(got) != 0 {
			t.Fatalf("hooks = %v, want none at shutdown", got)
		}
		if cr.onDeathGate().Pending("worker-1") {
			t.Fatal("worker-1 still held after shutdown")
		}
	})
}

// Kills: the handler map not published at construction or at reload (the
// tick and the lane read only the published map). The startup map holds a
// seed name; a reload that adds a bounded pool replaces it whole.
func TestCityRuntimePublishesPoolDeathHandlersAtStartAndReload(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	initialCfg, initialRevision := loadCityRuntimeControllerConfig(t, cityPath)
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: initialRevision,
		Cfg:       initialCfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:              newDrainOps(sp),
		PoolDeathHandlers: onDeathHandlers("seed-1"),
		Rec:               events.Discard,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
	})
	cs := newControllerState(context.Background(), initialCfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	if _, ok := cr.publishedPoolDeathHandlers()["seed-1"]; !ok {
		t.Fatalf("published handlers = %v, want the startup map", cr.publishedPoolDeathHandlers())
	}

	stubReloadBeadsLifecycle(t, func(string, string, *config.City, io.Writer) error { return nil })
	f, err := os.OpenFile(tomlPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\n[[agent]]\nname = \"worker\"\nscope = \"city\"\nstart_command = \"true\"\nmin_active_sessions = 0\nmax_active_sessions = 2\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	lastProviderName := "fake"
	if reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual); reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reload failed: %+v", reply)
	}
	got := cr.publishedPoolDeathHandlers()
	if _, ok := got["seed-1"]; ok || len(got) != 2 {
		t.Fatalf("published handlers after reload = %v, want the worker pool's two names and no seed", got)
	}
}

// Kills: a re-check abandoned by shutdown counting as its edge's second
// error and firing the hook while the controller stops.
func TestInventoryOnDeath_CanceledRecheckNeverFires(t *testing.T) {
	sp := newScriptedInventoryProvider()
	cr, hooks, _ := onDeathLaneRuntime(t, sp, onDeathHandlers("worker-1"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := deathEdge{name: "worker-1", info: onDeathHandlers("worker-1")["worker-1"], recheckErrors: onDeathRecheckAttempts - 1}
	if retry := cr.executeDeathEdge(ctx, cr.inventoryLane, &e); !retry {
		t.Fatal("a canceled re-check settled its edge instead of leaving it to be dropped")
	}
	if got := hooks.commands(); len(got) != 0 {
		t.Fatalf("hooks = %v, want none after a canceled re-check", got)
	}
}
