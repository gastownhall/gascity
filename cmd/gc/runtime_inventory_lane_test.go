package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
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
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// inventoryLaneInterval is the patrol interval the lane tests run at.
const inventoryLaneInterval = 10 * time.Second

// scriptedInventoryProvider is a tmux-shaped backend: a scripted listing with
// an optional gate and virtual duration, a batched inventory, and a batched
// environment read. Every call is counted.
type scriptedInventoryProvider struct {
	*runtime.Fake

	mu            sync.Mutex
	names         []string
	listErr       error
	listGate      chan struct{}
	listDuration  time.Duration
	listPanic     bool
	listCalls     int
	unattested    bool
	inventory     map[string]runtime.InventoryEntry
	inventoryErr  error
	inventoryPanc bool
	inventoryHook func(ctx context.Context, call int)
	inventoryN    int
	env           map[string]map[string]string
	envGate       chan struct{}
	envErr        map[string]error
	envCalls      map[string]int
}

func newScriptedInventoryProvider(names ...string) *scriptedInventoryProvider {
	p := &scriptedInventoryProvider{
		Fake:      runtime.NewFake(),
		names:     names,
		inventory: map[string]runtime.InventoryEntry{},
		env:       map[string]map[string]string{},
		envErr:    map[string]error{},
		envCalls:  map[string]int{},
	}
	for _, n := range names {
		p.inventory[n] = runtime.InventoryEntry{Incarnation: n + ":1", DeadKnown: true, AttachedKnown: true}
	}
	return p
}

func (p *scriptedInventoryProvider) ListRunning(string) ([]string, error) {
	p.mu.Lock()
	p.listCalls++
	gate, d, panics := p.listGate, p.listDuration, p.listPanic
	p.listPanic = false
	names := append([]string(nil), p.names...)
	err := p.listErr
	p.mu.Unlock()
	if panics {
		panic("listing exploded")
	}
	if gate != nil {
		<-gate
	}
	if d > 0 {
		<-time.After(d)
	}
	return names, err
}

func (p *scriptedInventoryProvider) ListRunningComplete() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.unattested
}

func (p *scriptedInventoryProvider) RuntimeInventory(ctx context.Context) (map[string]runtime.InventoryEntry, error) {
	p.mu.Lock()
	p.inventoryN++
	hook, call := p.inventoryHook, p.inventoryN
	p.mu.Unlock()
	if hook != nil {
		hook(ctx, call)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inventoryPanc {
		p.inventoryPanc = false
		panic("inventory exploded")
	}
	if p.inventoryErr != nil {
		return nil, p.inventoryErr
	}
	out := make(map[string]runtime.InventoryEntry, len(p.inventory))
	for k, v := range p.inventory {
		out[k] = v
	}
	return out, nil
}

func (p *scriptedInventoryProvider) GetAllEnvironment(name string) (map[string]string, error) {
	p.mu.Lock()
	gate := p.envGate
	p.mu.Unlock()
	if gate != nil {
		<-gate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.envCalls[name]++
	if err := p.envErr[name]; err != nil {
		return nil, err
	}
	return p.env[name], nil
}

func (p *scriptedInventoryProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listCalls
}

func (p *scriptedInventoryProvider) totalEnvCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.envCalls {
		n += c
	}
	return n
}

// listOnlyProvider is a backend with a scripted listing and no inventory
// (acp-, ssh- or k8s-shaped).
type listOnlyProvider struct {
	*runtime.Fake
	names      []string
	err        error
	unattested bool
	listCalls  int
}

func (p *listOnlyProvider) ListRunning(string) ([]string, error) {
	p.listCalls++
	return p.names, p.err
}

func (p *listOnlyProvider) ListRunningComplete() bool { return !p.unattested }

func inventoryLaneTestRuntime(t *testing.T, sp runtime.Provider, stderr io.Writer) *CityRuntime {
	t.Helper()
	if stderr == nil {
		stderr = io.Discard
	}
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: t.TempDir(),
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Daemon:    config.DaemonConfig{PatrolInterval: inventoryLaneInterval.String()},
		},
		sp:                  sp,
		standaloneCityStore: beads.NewMemStore(),
		rec:                 events.Discard,
		logPrefix:           "gc test",
		stdout:              io.Discard,
		stderr:              stderr,
	}
	if cr.initRuntimeInventoryLane() == nil {
		t.Fatal("initRuntimeInventoryLane returned nil with a provider and a store")
	}
	return cr
}

// useInventoryClock points the lane and its cache at clk.
func useInventoryClock(cr *CityRuntime, clk clock.Clock) {
	cr.inventoryLane.clock = clk
	cr.inventoryLane.cache.clock = clk
}

func runTestInventoryPass(cr *CityRuntime) {
	cr.runInventoryPass(context.Background(), "test")
}

// startInventoryLaneInBubble starts the lane inside the current bubble. The
// cleanup stops it, then lets any listing a stopped pass abandoned run out,
// so no goroutine outlives the bubble.
func startInventoryLaneInBubble(t *testing.T, cr *CityRuntime) *runtimeInventoryLane {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := cr.startRuntimeInventoryLane(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
		<-time.After(inventoryListingBound)
	})
	return cr.inventoryLane
}

func advanceInventoryLane(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

func wantInventoryPasses(t *testing.T, lane *runtimeInventoryLane, wakes, cadence int64, when string) {
	t.Helper()
	if gotW, gotC := lane.wakePasses.Load(), lane.cadencePasses.Load(); gotW != wakes || gotC != cadence {
		t.Fatalf("%s: passes wake=%d timer=%d, want wake=%d timer=%d", when, gotW, gotC, wakes, cadence)
	}
}

// Kills: a free-running ticker grid, and back-to-back passes. Passes take 3s,
// so the backstop (one interval after each pass ends) runs at t=10, 23 and
// 36, where a ticker would run at 10, 20 and 30.
func TestInventoryLane_BackstopOnePassPerInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("gc-a")
		sp.listDuration = 3 * time.Second
		cr := inventoryLaneTestRuntime(t, sp, nil)
		lane := startInventoryLaneInBubble(t, cr)

		advanceInventoryLane(inventoryLaneInterval - time.Millisecond)
		wantInventoryPasses(t, lane, 0, 0, "before the first interval")
		advanceInventoryLane(time.Millisecond)
		wantInventoryPasses(t, lane, 0, 1, "at the first interval")
		advanceInventoryLane(10 * time.Second) // t=20: a ticker's second pass
		wantInventoryPasses(t, lane, 0, 1, "one interval after the first pass started")
		advanceInventoryLane(3 * time.Second) // t=23
		wantInventoryPasses(t, lane, 0, 2, "one interval after the first pass ended")
		advanceInventoryLane(13*time.Second - time.Millisecond)
		wantInventoryPasses(t, lane, 0, 2, "just before the third backstop")
		advanceInventoryLane(time.Millisecond)
		wantInventoryPasses(t, lane, 0, 3, "third backstop")
		if got := sp.calls(); got != 3 {
			t.Fatalf("listing calls = %d, want one per pass (3)", got)
		}
	})
}

// Kills: a wake storm becoming back-to-back passes. A wake waits until the
// lane has idled as long as its last pass ran, and every wake in that wait
// joins one pass.
func TestInventoryLane_WakeRespectsDutyCycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("gc-a")
		sp.listDuration = 4 * time.Second
		cr := inventoryLaneTestRuntime(t, sp, nil)
		lane := startInventoryLaneInBubble(t, cr)

		lane.wake() // t=0: runs to t=4
		synctest.Wait()
		advanceInventoryLane(5 * time.Second) // t=5, idle 1s of the 4s gap
		for i := 0; i < 5; i++ {
			lane.wake()
		}
		synctest.Wait()
		wantInventoryPasses(t, lane, 1, 0, "wake storm inside the duty gap")
		advanceInventoryLane(3*time.Second - time.Millisecond)
		wantInventoryPasses(t, lane, 1, 0, "just before the duty gap closes")
		advanceInventoryLane(time.Millisecond)
		wantInventoryPasses(t, lane, 2, 0, "one pass for the whole storm at t=8")
		advanceInventoryLane(4 * time.Second)
		wantInventoryPasses(t, lane, 2, 0, "no pass straight after it")
	})
}

// Kills: goroutine pile-up on a hung listing, and a late result published.
// The listing blocks until the gate opens: the first pass gives up at the
// bound, the next finds it still in flight and lists nothing, and the late
// answer is dropped.
func TestInventoryLane_HungListingIsSingleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("gc-a")
		gate := make(chan struct{})
		sp.listGate = gate
		cr := inventoryLaneTestRuntime(t, sp, nil)
		lane := cr.inventoryLane

		start := time.Now()
		runTestInventoryPass(cr)
		if waited := time.Since(start); waited != inventoryListingBound {
			t.Fatalf("hung pass returned after %v, want the %v bound", waited, inventoryListingBound)
		}
		if got := lane.statusSnapshot().result; got != inventoryResultTimeout {
			t.Fatalf("hung pass result = %q, want %q", got, inventoryResultTimeout)
		}
		runTestInventoryPass(cr)
		if got := lane.statusSnapshot().result; got != inventoryResultInFlight {
			t.Fatalf("second pass result = %q, want %q", got, inventoryResultInFlight)
		}
		if got := sp.calls(); got != 1 {
			t.Fatalf("listing calls = %d while one hangs, want 1", got)
		}

		sp.mu.Lock()
		sp.listGate = nil
		sp.mu.Unlock()
		close(gate)
		synctest.Wait()
		if got := lane.cache.Snapshot(); got.PassSeq != 2 || len(got.ByName) != 0 {
			t.Fatalf("the late listing was published: %+v", got)
		}

		runTestInventoryPass(cr)
		if got := lane.statusSnapshot().result; got != inventoryResultPublished {
			t.Fatalf("pass after the hang result = %q, want %q", got, inventoryResultPublished)
		}
		if got := lane.cache.Snapshot().PassSeq; got != 3 {
			t.Fatalf("published PassSeq = %d, want 3 (PassSeq counts every pass)", got)
		}
	})
}

func inventoryErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Kills: a lane view whose names or error differ from sp.ListRunning(""), for
// single providers and for composites, including a nested one.
func TestInventoryLane_PassEqualsSynchronousListRunning(t *testing.T) {
	absent := &runtime.PartialListError{Err: errors.New("no tmux server"), ServerAbsent: true}
	partial := &runtime.PartialListError{Err: errors.New("one socket unanswered")}
	down := errors.New("list-sessions timed out")
	tmux := func(err error, names ...string) *scriptedInventoryProvider {
		p := newScriptedInventoryProvider(names...)
		p.listErr = err
		return p
	}
	// acp attests its listing since #6879; an ssh-like backend does not.
	acp := func(err error, names ...string) *listOnlyProvider {
		return &listOnlyProvider{Fake: runtime.NewFake(), names: names, err: err}
	}
	sshLike := func(names ...string) *listOnlyProvider {
		return &listOnlyProvider{Fake: runtime.NewFake(), names: names, unattested: true}
	}
	cases := []struct {
		name     string
		sp       runtime.Provider
		outcomes string
	}{
		{"single ok", tmux(nil, "gc-a", "gc-b"), "provider=complete"},
		{"single server absent", tmux(absent), "provider=partial"},
		{"single failed", tmux(down), "provider=failed"},
		{"auto ok", sessionauto.New(tmux(nil, "gc-a"), acp(nil, "gc-c")), "default=complete,acp=complete"},
		{"auto tmux absent", sessionauto.New(tmux(absent), acp(nil, "gc-c")), "default=partial,acp=complete"},
		{"hybrid with an unattested remote", sessionhybrid.New(tmux(nil, "gc-a"), sshLike("gc-ssh"), func(string) bool { return false }), "local=complete,remote=unattested"},
		{"auto acp partial", sessionauto.New(tmux(nil, "gc-a"), acp(partial, "gc-c")), "default=complete,acp=partial"},
		{"auto both failed", sessionauto.New(tmux(down), acp(errors.New("acp down"))), "default=failed,acp=failed"},
		{"auto over hybrid", sessionauto.New(
			sessionhybrid.New(tmux(nil, "gc-a"), &listOnlyProvider{Fake: runtime.NewFake(), err: down}, func(string) bool { return false }),
			acp(nil, "gc-c")), "default/local=complete,default/remote=failed,acp=complete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := inventoryLaneTestRuntime(t, tc.sp, nil)
			runTestInventoryPass(cr)
			pass := cr.inventoryLane.cache.Snapshot().Inventory

			names, err := tc.sp.ListRunning("")
			if !reflect.DeepEqual(pass.MergedNames, names) || inventoryErrText(pass.MergedErr) != inventoryErrText(err) {
				t.Fatalf("lane merged = (%#v, %q), ListRunning = (%#v, %q)", pass.MergedNames, inventoryErrText(pass.MergedErr), names, inventoryErrText(err))
			}
			if runtime.IsPartialListError(pass.MergedErr) != runtime.IsPartialListError(err) ||
				runtime.IsRuntimeServerAbsent(pass.MergedErr) != runtime.IsRuntimeServerAbsent(err) {
				t.Fatalf("lane merged error class (partial %v, absent %v) differs from ListRunning's (%v, %v)",
					runtime.IsPartialListError(pass.MergedErr), runtime.IsRuntimeServerAbsent(pass.MergedErr),
					runtime.IsPartialListError(err), runtime.IsRuntimeServerAbsent(err))
			}
			if got := inventoryBackendOutcomes(pass.Backends); got != tc.outcomes {
				t.Fatalf("backend outcomes = %q, want %q", got, tc.outcomes)
			}
		})
	}
}

// Kills: listing a nested composite's leaves twice for one observation. The
// lane walks Backends() and lists each leaf itself, once.
func TestInventoryLane_NestedCompositeListsEachLeafOnce(t *testing.T) {
	local := newScriptedInventoryProvider("gc-a")
	remote := &listOnlyProvider{Fake: runtime.NewFake(), names: []string{"gc-pod"}}
	acp := &listOnlyProvider{Fake: runtime.NewFake(), names: []string{"gc-c"}}
	sp := sessionauto.New(sessionhybrid.New(local, remote, func(string) bool { return false }), acp)
	cr := inventoryLaneTestRuntime(t, sp, nil)

	runTestInventoryPass(cr)
	if local.calls() != 1 || remote.listCalls != 1 || acp.listCalls != 1 {
		t.Fatalf("leaf listings = local %d, remote %d, acp %d; want exactly one each", local.calls(), remote.listCalls, acp.listCalls)
	}
	snap := cr.inventoryLane.cache.Snapshot()
	for name, want := range map[string]string{"gc-a": "default/local", "gc-pod": "default/remote", "gc-c": "acp"} {
		if got := snap.ByName[name].Backend; got != want {
			t.Errorf("%s backend = %q, want %q", name, got, want)
		}
	}
	if snap.ByName["gc-a"].Incarnation != "gc-a:1" {
		t.Errorf("the nested tmux leaf was not enriched: %+v", snap.ByName["gc-a"])
	}
}

// Kills: an attribution read every pass, and a stale owner after a respawn.
func TestInventoryLane_AttributionOncePerIncarnation(t *testing.T) {
	sp := newScriptedInventoryProvider("gc-a")
	sp.env["gc-a"] = map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok-1"}
	cr := inventoryLaneTestRuntime(t, sp, nil)

	runTestInventoryPass(cr)
	runTestInventoryPass(cr)
	if got := sp.envCalls["gc-a"]; got != 1 {
		t.Fatalf("attribution reads over two passes of one incarnation = %d, want 1", got)
	}
	obs := cr.inventoryLane.cache.Snapshot().ByName["gc-a"]
	if obs.OwnerState != OwnerSession || obs.Owner.SessionID != "gc-1" || obs.InstanceToken != "tok-1" {
		t.Fatalf("owner = %+v, want gc-1 with tok-1", obs)
	}

	sp.mu.Lock()
	sp.inventory["gc-a"] = runtime.InventoryEntry{Incarnation: "gc-a:2", DeadKnown: true, AttachedKnown: true}
	sp.env["gc-a"] = map[string]string{"GC_SESSION_ID": "gc-2", "GC_INSTANCE_TOKEN": "tok-2"}
	sp.mu.Unlock()
	runTestInventoryPass(cr)
	if got := sp.envCalls["gc-a"]; got != 2 {
		t.Fatalf("attribution reads after a respawn = %d, want 2", got)
	}
	if obs := cr.inventoryLane.cache.Snapshot().ByName["gc-a"]; obs.Owner.SessionID != "gc-2" || obs.InstanceToken != "tok-2" {
		t.Fatalf("owner after respawn = %+v, want gc-2", obs)
	}
}

// Kills: the tmux GetMeta ("", nil) poisoning. An attribution read error is
// Unknown and retried next pass, never ownerless; a clean read with no
// GC_SESSION_ID is ownerless and waits inventoryOwnerlessRereadPasses.
func TestInventoryLane_AttributionErrorIsUnknownNotOwnerless(t *testing.T) {
	sp := newScriptedInventoryProvider("gc-err", "gc-bare")
	sp.envErr["gc-err"] = errors.New("show-environment: server busy")
	sp.env["gc-bare"] = map[string]string{"PATH": "/bin"}
	cr := inventoryLaneTestRuntime(t, sp, nil)

	runTestInventoryPass(cr)
	snap := cr.inventoryLane.cache.Snapshot()
	if got := snap.ByName["gc-err"].OwnerState; got != OwnerUnknown {
		t.Fatalf("owner after a read error = %v, want OwnerUnknown", got)
	}
	if got := snap.ByName["gc-bare"].OwnerState; got != OwnerNone {
		t.Fatalf("owner after a clean read without GC_SESSION_ID = %v, want OwnerNone", got)
	}

	sp.mu.Lock()
	delete(sp.envErr, "gc-err")
	sp.env["gc-err"] = map[string]string{"GC_SESSION_ID": "gc-7"}
	sp.mu.Unlock()
	runTestInventoryPass(cr)
	if got := cr.inventoryLane.cache.Snapshot().ByName["gc-err"]; got.OwnerState != OwnerSession || got.Owner.SessionID != "gc-7" {
		t.Fatalf("owner after the retry = %+v, want gc-7", got)
	}
	for i := 2; i < inventoryOwnerlessRereadPasses; i++ {
		runTestInventoryPass(cr)
	}
	if got := sp.envCalls["gc-bare"]; got != 1 {
		t.Fatalf("ownerless reads before the re-read interval = %d, want 1", got)
	}
	runTestInventoryPass(cr)
	if got := sp.envCalls["gc-bare"]; got != 2 {
		t.Fatalf("ownerless reads at the re-read interval = %d, want 2", got)
	}
}

// Kills: unbounded attribution reads after a restart. 150 new incarnations
// take three passes at 64 reads each.
func TestInventoryLane_AttributionBudgetPerPass(t *testing.T) {
	names := make([]string, 150)
	for i := range names {
		names[i] = fmt.Sprintf("gc-%03d", i)
	}
	sp := newScriptedInventoryProvider(names...)
	cr := inventoryLaneTestRuntime(t, sp, nil)

	for pass, want := range []int{64, 128, 150, 150} {
		runTestInventoryPass(cr)
		if got := sp.totalEnvCalls(); got != want {
			t.Fatalf("attribution reads after pass %d = %d, want %d", pass+1, got, want)
		}
	}
}

// Kills: a data race on cr.sp (run under -race), and a provider swap that
// does not bump ProviderGen.
func TestInventoryLane_ReadsProviderUnderServiceLock(t *testing.T) {
	first := newScriptedInventoryProvider("gc-a")
	second := newScriptedInventoryProvider("gc-b")
	cr := inventoryLaneTestRuntime(t, first, nil)
	cfg := cr.cfg

	runTestInventoryPass(cr)
	if got := cr.inventoryLane.cache.Snapshot().Inventory.ProviderGen; got != 1 {
		t.Fatalf("ProviderGen = %d, want 1", got)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			sp := runtime.Provider(first)
			if i%2 == 0 {
				sp = second
			}
			cr.publishRuntimeConfig(cfg, sp, nil, "rev")
		}
	}()
	for i := 0; i < 50; i++ {
		runTestInventoryPass(cr)
	}
	wg.Wait()

	cr.publishRuntimeConfig(cfg, second, nil, "rev")
	runTestInventoryPass(cr)
	gen := cr.inventoryLane.cache.Snapshot().Inventory.ProviderGen
	runTestInventoryPass(cr)
	if got := cr.inventoryLane.cache.Snapshot().Inventory.ProviderGen; got != gen {
		t.Fatalf("ProviderGen moved from %d to %d without a swap", gen, got)
	}
	cr.publishRuntimeConfig(cfg, first, nil, "rev")
	runTestInventoryPass(cr)
	if got := cr.inventoryLane.cache.Snapshot().Inventory.ProviderGen; got != gen+1 {
		t.Fatalf("ProviderGen after a swap = %d, want %d", got, gen+1)
	}
}

// Kills: the lane dying on a panic (MAINT-020). A panic in the pass and a
// panic inside the listing call are both recovered, and the next pass runs.
func TestInventoryLane_PanicRecoveredNextPassRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stderr bytes.Buffer
		sp := newScriptedInventoryProvider("gc-a")
		sp.inventoryPanc = true
		cr := inventoryLaneTestRuntime(t, sp, &stderr)
		lane := startInventoryLaneInBubble(t, cr)

		advanceInventoryLane(inventoryLaneInterval)
		if !strings.Contains(stderr.String(), "reconciler tick panicked (trigger=inventory-lane)") {
			t.Fatalf("stderr = %q, want the recovered pass panic", stderr.String())
		}
		sp.mu.Lock()
		sp.listPanic = true
		sp.mu.Unlock()
		advanceInventoryLane(inventoryLaneInterval)
		if got := lane.statusSnapshot().result; got != inventoryResultPanicked {
			t.Fatalf("pass result = %q, want %q", got, inventoryResultPanicked)
		}
		advanceInventoryLane(inventoryLaneInterval)
		if got := lane.statusSnapshot().result; got != inventoryResultPublished {
			t.Fatalf("pass after the panics = %q, want %q", got, inventoryResultPublished)
		}
		if _, ok := lane.cache.Snapshot().ByName["gc-a"]; !ok {
			t.Fatal("the pass after the panics published nothing")
		}
	})
}

// Kills: a goroutine leak. Canceling the lane's context ends its goroutine.
func TestInventoryLane_StopsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cr := inventoryLaneTestRuntime(t, newScriptedInventoryProvider("gc-a"), nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := cr.startRuntimeInventoryLane(ctx)
		advanceInventoryLane(inventoryLaneInterval)
		cancel()
		select {
		case <-done:
		case <-time.After(time.Hour):
			t.Fatal("the lane goroutine did not exit after cancellation")
		}
	})
}

// Kills: an unattested or idle backend alerted, a failing backend never
// unhealthy, and an alert repeated every pass instead of once per episode.
func TestInventoryLane_HealthStates(t *testing.T) {
	var stderr bytes.Buffer
	tmux := newScriptedInventoryProvider()
	tmux.listErr = &runtime.PartialListError{Err: errors.New("no server"), ServerAbsent: true}
	// An ssh-like remote never attests, so it is never alerted even while its
	// listing fails.
	remote := &listOnlyProvider{Fake: runtime.NewFake(), err: errors.New("ssh exit 255"), unattested: true}
	cr := inventoryLaneTestRuntime(t, sessionhybrid.New(tmux, remote, func(string) bool { return false }), &stderr)
	clk := &clock.Fake{Time: obsTestEpoch}
	useInventoryClock(cr, clk)
	health := func() map[string]string {
		out := map[string]string{}
		for l, h := range cr.inventoryLane.cache.Snapshot().Health {
			out[l] = h.State
		}
		return out
	}
	want := func(when string, states map[string]string) {
		t.Helper()
		if got := health(); !reflect.DeepEqual(got, states) {
			t.Fatalf("%s: health = %v, want %v", when, got, states)
		}
	}

	runTestInventoryPass(cr)
	want("fresh city, no tmux server", map[string]string{"local": backendHealthIdle, "remote": backendHealthUnattested})

	tmux.mu.Lock()
	tmux.listErr, tmux.names = nil, []string{"gc-a"}
	tmux.mu.Unlock()
	clk.Advance(15 * time.Second)
	runTestInventoryPass(cr)
	want("tmux answering", map[string]string{"local": backendHealthHealthy, "remote": backendHealthUnattested})

	tmux.mu.Lock()
	tmux.listErr, tmux.names = errors.New("list-sessions timed out"), nil
	tmux.mu.Unlock()
	clk.Advance(15 * time.Second)
	runTestInventoryPass(cr)
	want("tmux failing", map[string]string{"local": backendHealthDegraded, "remote": backendHealthUnattested})
	clk.Advance(observationUnhealthyAfter)
	runTestInventoryPass(cr)
	want("tmux failing for 5m", map[string]string{"local": backendHealthUnhealthy, "remote": backendHealthUnattested})
	clk.Advance(time.Minute)
	runTestInventoryPass(cr)
	if got := strings.Count(stderr.String(), "backend local unhealthy"); got != 1 {
		t.Fatalf("unhealthy alerts = %d within one episode, want 1:\n%s", got, stderr.String())
	}
	clk.Advance(inventoryUnhealthyRealert)
	runTestInventoryPass(cr)
	if got := strings.Count(stderr.String(), "backend local unhealthy"); got != 2 {
		t.Fatalf("unhealthy alerts after %v = %d, want 2", inventoryUnhealthyRealert, got)
	}
	if strings.Contains(stderr.String(), "backend remote") {
		t.Fatalf("an unattested backend was alerted:\n%s", stderr.String())
	}

	tmux.mu.Lock()
	tmux.listErr = &runtime.PartialListError{Err: errors.New("no server"), ServerAbsent: true}
	tmux.mu.Unlock()
	clk.Advance(15 * time.Second)
	runTestInventoryPass(cr)
	want("server gone after it held sessions", map[string]string{"local": backendHealthUnhealthy, "remote": backendHealthUnattested})
}

// Kills: lane liveness invisible in `gc trace`. The tick record distinguishes
// a lane that never ran from one that just did, and carries the pass result,
// the snapshot and generation ages, and each backend's outcome.
func TestCityRuntimeTick_RecordsInventoryLaneAge(t *testing.T) {
	tmux := newScriptedInventoryProvider("gc-a")
	acp := &listOnlyProvider{Fake: runtime.NewFake(), err: &runtime.PartialListError{Err: errors.New("one socket")}}
	cr := inventoryLaneTestRuntime(t, sessionauto.New(tmux, acp), nil)
	clk := &clock.Fake{Time: obsTestEpoch}
	useInventoryClock(cr, clk)

	before := cr.inventoryLane.tickFields(clk.Now())
	if before["backstop_ran"] != false {
		t.Fatalf("fields before any pass = %v, want backstop_ran=false", before)
	}
	if _, ok := before["inventory_age_ms"]; ok {
		t.Fatalf("fields before any pass = %v, want no snapshot age", before)
	}

	runTestInventoryPass(cr)
	clk.Advance(20 * time.Second)
	runTestInventoryPass(cr) // a refresh: the generation does not move
	clk.Advance(5 * time.Second)
	fields := cr.inventoryLane.tickFields(clk.Now())
	for key, want := range map[string]any{
		"backstop_ran":                     true,
		"backstop_last_reason":             "test",
		"inventory_last_pass_seq":          uint64(2),
		"inventory_pass_seq":               uint64(2),
		"inventory_last_pass_ms":           int64(0),
		"inventory_last_result":            inventoryResultPublished,
		"inventory_age_ms":                 int64(5000),
		"inventory_gen_age_ms":             int64(25000),
		"inventory_gen":                    uint64(1),
		"inventory_backend_outcomes":       "default=complete,acp=partial",
		"inventory_partial_backends":       "acp",
		"inventory_server_absent_backends": "",
		"inventory_all_primed":             false,
	} {
		if got := fields[key]; got != want {
			t.Errorf("tick field %s = %#v, want %#v", key, got, want)
		}
	}
	if fields["inventory_epoch"] == "" {
		t.Error("tick fields carry no epoch")
	}
}

// eventedInventoryProvider adds a session-event stream to the scripted
// backend; subscribed closes on the first subscription.
type eventedInventoryProvider struct {
	*scriptedInventoryProvider
	events     chan runtime.SessionEvent
	subscribed chan struct{}
	once       sync.Once
}

func (p *eventedInventoryProvider) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) { //nolint:unparam // runtime.SessionEventProvider signature
	out := make(chan runtime.SessionEvent)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-p.events:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	p.once.Do(func() { close(p.subscribed) })
	return out, nil
}

// Kills: health frozen during a hung listing while the trace reads healthy.
// A listing that times out, and the in-flight passes behind it, publish a
// failed outcome: facts stay put, FreshInventory refuses the pass at once,
// the backend turns unhealthy after five minutes, the alert fires, and the
// pass record shows it.
func TestInventoryLane_HungListingTurnsBackendUnhealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stderr bytes.Buffer
		sp := newScriptedInventoryProvider("gc-a")
		cr := inventoryLaneTestRuntime(t, sp, &stderr)
		cr.trace = newSessionReconcilerTraceManager(cr.cityPath, "test-city", io.Discard)
		lane := cr.inventoryLane
		runTestInventoryPass(cr)
		seen := lane.cache.Snapshot().ByName["gc-a"].Listed

		gate := make(chan struct{})
		sp.mu.Lock()
		sp.listGate = gate
		sp.mu.Unlock()
		runTestInventoryPass(cr)
		if _, ok := lane.cache.FreshInventory(time.Hour); ok {
			t.Fatal("FreshInventory served a pass whose listing timed out")
		}
		snap := lane.cache.Snapshot()
		if got := snap.ByName["gc-a"].Listed; got != seen {
			t.Fatalf("gc-a Listed = %+v after a timed-out listing, want it untouched (%+v)", got, seen)
		}
		if got := snap.Health[""].State; got != backendHealthDegraded {
			t.Fatalf("health after the timeout = %q, want %q", got, backendHealthDegraded)
		}

		<-time.After(observationUnhealthyAfter)
		runTestInventoryPass(cr)
		if got := lane.statusSnapshot().result; got != inventoryResultInFlight {
			t.Fatalf("pass result = %q, want %q", got, inventoryResultInFlight)
		}
		if got := lane.cache.Snapshot().Health[""].State; got != backendHealthUnhealthy {
			t.Fatalf("health after 5m of hung listing = %q, want %q", got, backendHealthUnhealthy)
		}
		if !strings.Contains(stderr.String(), "backend provider unhealthy") {
			t.Fatalf("stderr = %q, want the unhealthy alert", stderr.String())
		}

		sp.mu.Lock()
		sp.listGate = nil
		sp.mu.Unlock()
		close(gate)
		synctest.Wait()
		if err := cr.trace.Close(); err != nil {
			t.Fatalf("closing the tracer: %v", err)
		}
		records, err := ReadTraceRecords(traceCityRuntimeDir(cr.cityPath), TraceFilter{})
		if err != nil {
			t.Fatalf("ReadTraceRecords: %v", err)
		}
		found := false
		for _, r := range records {
			if r.SiteCode == TraceSiteRuntimeInventoryPass && r.Fields["inventory_result"] == inventoryResultInFlight &&
				r.Fields["inventory_backend_health"] == "provider=unhealthy" && r.Fields["inventory_health_alerts"] == "provider" {
				found = true
			}
		}
		if !found {
			t.Fatal("no runtime_inventory.pass record shows the in-flight pass, the unhealthy backend and its alert")
		}
	})
}

// Kills: a listing bound at or below the tmux subprocess timeout. A tmux
// listing that fails at its own 30s timeout (plus scheduling slop) fails
// inside its call, and the ACP backend's answer still publishes.
func TestInventoryLane_SlowTmuxFailsInsideItsOwnTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tmux := newScriptedInventoryProvider()
		tmux.listDuration = 30*time.Second + time.Millisecond
		tmux.listErr = errors.New("tmux list-sessions: timed out")
		acp := &listOnlyProvider{Fake: runtime.NewFake(), names: []string{"gc-c"}}
		cr := inventoryLaneTestRuntime(t, sessionauto.New(tmux, acp), nil)

		runTestInventoryPass(cr)
		if got := cr.inventoryLane.statusSnapshot().result; got != inventoryResultPublished {
			t.Fatalf("pass result = %q, want %q", got, inventoryResultPublished)
		}
		snap := cr.inventoryLane.cache.Snapshot()
		if got := inventoryBackendOutcomes(snap.Inventory.Backends); got != "default=failed,acp=complete" {
			t.Fatalf("backend outcomes = %q, want default=failed,acp=complete", got)
		}
		if snap.ByName["gc-c"].Listed.Value != ObsYes {
			t.Fatal("the ACP name was not published")
		}
	})
}

// Kills: a wedged attribution read holding the startup prime or shutdown.
// The attribution phase is capped at inventoryAttributionBound, and a
// canceled lane leaves at once.
func TestInventoryLane_WedgedAttributionDelaysNeitherPrimeNorShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		sp := newScriptedInventoryProvider("gc-a", "gc-b")
		sp.envGate = gate
		cr := inventoryLaneTestRuntime(t, sp, nil)

		start := time.Now()
		cr.primeNow(context.Background())
		if waited := time.Since(start); waited != inventoryAttributionBound {
			t.Fatalf("prime took %v with a wedged attribution read, want the %v bound", waited, inventoryAttributionBound)
		}
		snap := cr.inventoryLane.cache.Snapshot()
		if snap.PassSeq != 1 || snap.ByName["gc-a"].OwnerState != OwnerUnknown || snap.ByName["gc-b"].OwnerState != OwnerUnknown {
			t.Fatalf("prime snapshot = %+v, want pass 1 published with owners Unknown", snap)
		}

		other := newScriptedInventoryProvider("gc-c")
		other.envGate = gate
		cr2 := inventoryLaneTestRuntime(t, other, nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := cr2.startRuntimeInventoryLane(ctx)
		cr2.inventoryLane.wake()
		synctest.Wait() // the pass is now waiting on the wedged read
		cancel()
		stopped := time.Now()
		select {
		case <-done:
		case <-time.After(inventoryAttributionBound):
			t.Fatal("the lane waited out the attribution bound after cancellation")
		}
		if waited := time.Since(stopped); waited != 0 {
			t.Fatalf("shutdown took %v with a wedged attribution read, want none", waited)
		}
		close(gate)
		synctest.Wait()
	})
}

// Kills: a name deferred by the attribution budget carrying its previous
// incarnation's owner and token after a respawn.
func TestInventoryLane_DeferredRespawnCarriesNoStaleOwner(t *testing.T) {
	names := make([]string, inventoryAttributionBudget+6)
	for i := range names {
		names[i] = fmt.Sprintf("gc-%03d", i)
	}
	sp := newScriptedInventoryProvider(names...)
	for _, n := range names {
		sp.env[n] = map[string]string{"GC_SESSION_ID": "old-" + n, "GC_INSTANCE_TOKEN": "tok-old"}
	}
	cr := inventoryLaneTestRuntime(t, sp, nil)
	runTestInventoryPass(cr)
	runTestInventoryPass(cr)

	sp.mu.Lock()
	for _, n := range names {
		sp.inventory[n] = runtime.InventoryEntry{Incarnation: n + ":2", DeadKnown: true, AttachedKnown: true}
		sp.env[n] = map[string]string{"GC_SESSION_ID": "new-" + n, "GC_INSTANCE_TOKEN": "tok-new"}
	}
	sp.mu.Unlock()
	runTestInventoryPass(cr)

	snap := cr.inventoryLane.cache.Snapshot()
	deferred := 0
	for _, n := range names {
		obs := snap.ByName[n]
		switch {
		case obs.Owner.SessionID == "new-"+n:
		case obs.OwnerState == OwnerUnknown && obs.InstanceToken == "" && obs.Owner.SessionID == "":
			deferred++
		default:
			t.Fatalf("%s after a respawn = owner %q token %q (state %v), want the new owner or Unknown", n, obs.Owner.SessionID, obs.InstanceToken, obs.OwnerState)
		}
	}
	if deferred != 6 {
		t.Fatalf("deferred names = %d, want 6 (the respawns beyond the budget)", deferred)
	}
}

// Kills: listing facts stamped when the pass finished. A runtime listed by
// a pass was running no later than the listing's start, which is what the
// PR-5 fence compares a PreWake against.
func TestInventoryLane_FactsStampedAtListingStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("gc-a")
		sp.listDuration = 3 * time.Second
		cr := inventoryLaneTestRuntime(t, sp, nil)
		start := time.Now()
		runTestInventoryPass(cr)

		snap := cr.inventoryLane.cache.Snapshot()
		obs := snap.ByName["gc-a"]
		if !obs.Listed.ObservedAt.Equal(start) || !obs.LastListedAt.Equal(start) || !obs.Running.ObservedAt.Equal(start) {
			t.Fatalf("gc-a stamped Listed=%v LastListedAt=%v Running=%v, want the listing start %v",
				obs.Listed.ObservedAt, obs.LastListedAt, obs.Running.ObservedAt, start)
		}
		if want := start.Add(3 * time.Second); !snap.At.Equal(want) {
			t.Fatalf("snapshot At = %v, want the pass finish %v", snap.At, want)
		}
	})
}

// Kills: a failed batched inventory overwriting enrichment facts. The names
// stay listed, and Running keeps its last observation to age out.
func TestInventoryLane_InventoryFailureKeepsFacts(t *testing.T) {
	sp := newScriptedInventoryProvider("gc-a")
	cr := inventoryLaneTestRuntime(t, sp, nil)
	clk := &clock.Fake{Time: obsTestEpoch}
	useInventoryClock(cr, clk)
	runTestInventoryPass(cr)
	seen := cr.inventoryLane.cache.Snapshot().ByName["gc-a"].Running

	sp.mu.Lock()
	sp.inventoryErr = errors.New("list-panes: server busy")
	sp.mu.Unlock()
	clk.Advance(15 * time.Second)
	runTestInventoryPass(cr)
	obs := cr.inventoryLane.cache.Snapshot().ByName["gc-a"]
	if obs.Running != seen {
		t.Fatalf("Running after a failed inventory = %+v, want the last observation %+v", obs.Running, seen)
	}
	if !obs.Listed.ObservedAt.Equal(clk.Now()) {
		t.Fatalf("Listed.ObservedAt = %v, want the listing refreshed at %v", obs.Listed.ObservedAt, clk.Now())
	}
}

// Kills: a provider event storm running inventory passes back to back when
// passes are cheap. Wake passes are at least inventoryMinWakeGap apart.
func TestInventoryLane_WakeMinimumSpacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cr := inventoryLaneTestRuntime(t, newScriptedInventoryProvider("gc-a"), nil)
		lane := startInventoryLaneInBubble(t, cr)
		lane.wake()
		synctest.Wait()
		advanceInventoryLane(inventoryMinWakeGap / 2)
		lane.wake()
		synctest.Wait()
		wantInventoryPasses(t, lane, 1, 0, "wake inside the minimum gap")
		advanceInventoryLane(inventoryMinWakeGap/2 - time.Millisecond)
		wantInventoryPasses(t, lane, 1, 0, "just before the minimum gap")
		advanceInventoryLane(time.Millisecond)
		wantInventoryPasses(t, lane, 2, 0, "at the minimum gap")
	})
}

// Kills: the tick not recording the lane, and a pass never traced. The tick
// writes a runtime_inventory_lane phase record carrying the snapshot's pass,
// and the lane's first pass writes a runtime_inventory.pass record.
func TestCityRuntimeTick_EmitsInventoryLaneRecord(t *testing.T) {
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Daemon:    config.DaemonConfig{PatrolInterval: inventoryLaneInterval.String()},
		},
		sp:                  newScriptedInventoryProvider("gc-a"),
		standaloneCityStore: beads.NewMemStore(),
		rec:                 events.Discard,
		logPrefix:           "test-city",
		stdout:              io.Discard,
		stderr:              io.Discard,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}
	cr.trace = newSessionReconcilerTraceManager(cr.cityPath, "test-city", io.Discard)
	if cr.initRuntimeInventoryLane() == nil {
		t.Fatal("no lane")
	}
	runTestInventoryPass(cr)
	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "test")
	if err := cr.trace.Close(); err != nil {
		t.Fatalf("closing the tracer: %v", err)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(cr.cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var tickRecord, passRecord bool
	for _, r := range records {
		if r.SiteCode == TraceSiteControllerTickPhase && r.Fields["operation_name"] == "runtime_inventory_lane" &&
			r.Fields["inventory_pass_seq"] == float64(1) && r.Fields["inventory_backend_outcomes"] == "provider=complete" {
			tickRecord = true
		}
		if r.SiteCode == TraceSiteRuntimeInventoryPass && r.Fields["inventory_result"] == inventoryResultPublished &&
			r.Fields["inventory_epoch"] == cr.inventoryLane.cache.epoch {
			passRecord = true
		}
	}
	if !tickRecord {
		t.Error("the tick wrote no runtime_inventory_lane record carrying the snapshot's pass")
	}
	if !passRecord {
		t.Error("the lane's first pass wrote no runtime_inventory.pass record")
	}
}

// Kills: run() wiring regressions. The prime pass is published before the
// startup reconcile builds desired state, the session-event pump wakes the
// lane, and run() does not return until the lane goroutine has exited.
func TestCityRuntimeRun_InventoryLaneLifecycle(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// No backstop pass inside the test: the only lane pass after the prime is
	// the one the session-event pump wakes.
	cfg.Daemon.PatrolInterval = "1h"
	sp := &eventedInventoryProvider{
		scriptedInventoryProvider: newScriptedInventoryProvider("gc-stray"),
		events:                    make(chan runtime.SessionEvent, 1),
		subscribed:                make(chan struct{}),
	}
	// The second inventory read is the first lane pass after the prime.
	inPass := make(chan struct{})
	sp.inventoryHook = func(_ context.Context, call int) {
		if call == 2 {
			close(inPass)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cr *CityRuntime
	var primedAtReconcile atomic.Int64
	primedAtReconcile.Store(-1)
	cr, err = newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			if lane := cr.inventoryLane; lane != nil {
				primedAtReconcile.CompareAndSwap(-1, int64(lane.cache.Snapshot().PassSeq))
			} else {
				primedAtReconcile.CompareAndSwap(-1, 0)
			}
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("building the city runtime: %v", err)
	}
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		cr.run(ctx)
	}()
	awaitClose(t, sp.subscribed, "the session-event pump subscribing")
	sp.events <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "gc-stray", Time: time.Now()}
	awaitClose(t, inPass, "a lane pass woken by the session-event pump")
	cancel()
	awaitClose(t, runDone, "run() returning after cancellation")

	if got := primedAtReconcile.Load(); got != 1 {
		t.Fatalf("snapshot PassSeq at the startup reconcile = %d, want the prime pass (1)", got)
	}
	if got := cr.inventoryLane.wakePasses.Load(); got != 1 {
		t.Fatalf("wake passes = %d, want the one the session-event pump woke", got)
	}
}

// Kills: shutdown running while an inventory pass is still in progress. The
// stop run() defers cancels the lane and returns only once its goroutine has
// exited; inside the bubble, "stop is still waiting" is an observable fact.
func TestInventoryLane_StopJoinsTheLane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedInventoryProvider("gc-a")
		release := make(chan struct{})
		sp.inventoryHook = func(ctx context.Context, _ int) {
			<-ctx.Done()
			<-release
		}
		cr := inventoryLaneTestRuntime(t, sp, nil)
		stop := cr.runRuntimeInventoryLane(context.Background())
		cr.inventoryLane.wake()
		synctest.Wait() // the pass is inside RuntimeInventory

		stopped := make(chan struct{})
		go func() {
			stop()
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			close(release)
			t.Fatal("stop returned while the lane pass was still running")
		default:
		}
		close(release)
		synctest.Wait()
		select {
		case <-stopped:
		default:
			t.Fatal("stop did not return after the lane pass finished")
		}
	})
}
