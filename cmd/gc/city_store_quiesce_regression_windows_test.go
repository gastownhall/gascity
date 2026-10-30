//go:build windows

package main

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

type retiringStoreScheduler interface {
	scheduleCloseBeadStoreHandle(label string, store beads.Store)
}

type retiredBdReadStore struct {
	beads.Store
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once
	bin         string
	env         []string
}

func newRetiredBdReadStore(t *testing.T, bin string, env []string) *retiredBdReadStore {
	t.Helper()
	s := &retiredBdReadStore{
		Store:   beads.NewMemStore(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
		bin:     bin,
		env:     env,
	}
	t.Cleanup(s.unblock)
	return s
}

func (s *retiredBdReadStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	var callErr error
	s.once.Do(func() {
		close(s.entered)
		<-s.release
		cmd := exec.Command(s.bin, "show", "retired-cache")
		cmd.Env = s.env
		callErr = cmd.Run()
	})
	if callErr != nil {
		return nil, callErr
	}
	return s.Store.List(query)
}

func (s *retiredBdReadStore) unblock() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func scheduleRetiredStoreForQuiesceTest(mc *managedCity, label string, store beads.Store) {
	if mc != nil && mc.cr != nil && mc.cr.cs != nil {
		if scheduler, ok := interface{}(mc.cr.cs).(retiringStoreScheduler); ok {
			scheduler.scheduleCloseBeadStoreHandle(label, store)
			return
		}
	}
	scheduleCloseBeadStoreHandle(label, store)
}

func TestQuiesceWaitsForAStoreRetiredByReload(t *testing.T) {
	n := newNativeBdTest(t)
	warmNativeFakeBd(t, n)
	writeDirectProviderOwnedCity(t, n.city)
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(n.city))
	t.Setenv("BD_BIN", n.bdBin)
	t.Setenv(nativeFakeBdLogEnv, n.log)

	backing := newRetiredBdReadStore(t, n.bdBin, n.environ(n.city))
	oldStore := beads.NewCachingStoreForTest(backing, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	oldStore.StartReconciler(ctx, beads.WithStaggerOff(), "")
	select {
	case <-backing.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("retired store did not begin its read")
	}

	done := make(chan struct{})
	close(done)
	mc := managedCityOverStore("reload", oldStore, cancel, done, "5s")
	mc.cr.cs.mu.Lock()
	mc.cr.cs.cityBeadStore = beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	mc.cr.cs.mu.Unlock()
	scheduleRetiredStoreForQuiesceTest(mc, "city bead store", oldStore)

	stopped := make(chan error, 1)
	var stderr bytes.Buffer
	go func() { stopped <- stopManagedCity(mc, n.city, &stderr) }()

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stopManagedCity returned before release with error: %v; stderr=%s", err, &stderr)
		}
	case <-time.After(3 * time.Second):
	}
	for _, call := range n.calls() {
		if strings.Join(call.Args, " ") == "dolt stop" {
			t.Fatalf("provider stop ran before the retired cache completed; calls=%+v; stderr=%s", n.calls(), &stderr)
		}
	}

	backing.unblock()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stopManagedCity after release: %v; stderr=%s", err, &stderr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stopManagedCity did not return after the retired cache completed")
	}
	calls := n.requireArgs([]string{"show", "retired-cache"}, []string{"dolt", "stop"})
	if !calls[1].At.After(calls[0].At) {
		t.Fatalf("provider stop ran at %s before retired cache read at %s", calls[1].At.Format(time.RFC3339Nano), calls[0].At.Format(time.RFC3339Nano))
	}
}

func TestQuiesceTimeoutClosesRecorderAndEventuallyStopsProvider(t *testing.T) {
	previous := cityStoreQuiesceTimeout
	cityStoreQuiesceTimeout = 300 * time.Millisecond
	t.Cleanup(func() { cityStoreQuiesceTimeout = previous })

	n := newNativeBdTest(t)
	warmNativeFakeBd(t, n)
	writeDirectProviderOwnedCity(t, n.city)
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(n.city))
	t.Setenv("BD_BIN", n.bdBin)
	t.Setenv(nativeFakeBdLogEnv, n.log)

	backing := newListBlockingStore(t)
	cached, cancelCity := reconcilingCachingStore(t, backing)
	done := make(chan struct{})
	close(done)
	mc := managedCityOverStore("owned", cached, cancelCity, done, "5s")
	closer := &closerSpy{}
	mc.closer = closer

	var stderr bytes.Buffer
	returned := make(chan error, 1)
	go func() { returned <- stopManagedCity(mc, n.city, &stderr) }()

	var stopErr error
	select {
	case stopErr = <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("stopManagedCity did not return after the quiesce bound")
	}
	if stopErr == nil {
		t.Fatalf("stopManagedCity error = nil, want quiesce timeout; stderr=%s", &stderr)
	}
	if !strings.Contains(stopErr.Error(), `city "owned"`) || !strings.Contains(stopErr.Error(), "bead store reconcile still running after 300ms") {
		t.Fatalf("stopManagedCity error = %q, want city and stuck work details", stopErr.Error())
	}
	if !closer.closed {
		t.Fatal("expected recorder closer to be closed when stop returns the quiesce timeout")
	}
	if backing.releasedAt.Load() != 0 {
		t.Fatal("store work ended before the provider stop path was tested")
	}
	n.requireArgs()
	backing.unblock()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range n.calls() {
			if strings.Join(call.Args, " ") == "dolt stop" {
				passEnded := time.Unix(0, backing.releasedAt.Load())
				if !call.At.After(passEnded) {
					t.Fatalf("provider stop ran at %s before the blocked pass ended at %s", call.At.Format(time.RFC3339Nano), passEnded.Format(time.RFC3339Nano))
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("provider was not stopped after the blocked pass drained")
}

func TestDeferredProviderStopSkipsNewerCityInstance(t *testing.T) {
	previous := cityStoreQuiesceTimeout
	cityStoreQuiesceTimeout = 300 * time.Millisecond
	t.Cleanup(func() { cityStoreQuiesceTimeout = previous })

	n := newNativeBdTest(t)
	warmNativeFakeBd(t, n)
	writeDirectProviderOwnedCity(t, n.city)
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(n.city))
	t.Setenv("BD_BIN", n.bdBin)
	t.Setenv(nativeFakeBdLogEnv, n.log)

	backing := newListBlockingStore(t)
	cached, cancelCity := reconcilingCachingStore(t, backing)
	done := make(chan struct{})
	close(done)
	mc := managedCityOverStore("owned", cached, cancelCity, done, "5s")
	markManagedCityPublished(n.city, mc)

	deferredDone := make(chan struct{})
	var deferredDoneOnce sync.Once
	previousDeferredDone := cityStoreDeferredProviderStopDoneForTest
	cityStoreDeferredProviderStopDoneForTest = func() {
		deferredDoneOnce.Do(func() { close(deferredDone) })
	}
	t.Cleanup(func() { cityStoreDeferredProviderStopDoneForTest = previousDeferredDone })

	var stderr bytes.Buffer
	stopErr := stopManagedCity(mc, n.city, &stderr)
	if stopErr == nil {
		t.Fatalf("stopManagedCity error = nil, want quiesce timeout; stderr=%s", &stderr)
	}
	n.requireArgs()

	restartedDone := make(chan struct{})
	close(restartedDone)
	restarted := managedCityOverStore("owned", beads.NewCachingStoreForTest(beads.NewMemStore(), nil), func() {}, restartedDone, "5s")
	markManagedCityPublished(n.city, restarted)

	backing.unblock()
	select {
	case <-deferredDone:
	case <-time.After(10 * time.Second):
		t.Fatal("deferred provider stop did not finish after the blocked pass drained")
	}
	for _, call := range n.calls() {
		if strings.Join(call.Args, " ") == "dolt stop" {
			t.Fatalf("deferred stop stopped a newer city instance's provider; calls=%+v", n.calls())
		}
	}
}

func TestQuiesceSwapGapDoesNotReportDrained(t *testing.T) {
	previousTimeout := cityStoreQuiesceTimeout
	cityStoreQuiesceTimeout = 150 * time.Millisecond
	t.Cleanup(func() { cityStoreQuiesceTimeout = previousTimeout })

	n := newNativeBdTest(t)
	warmNativeFakeBd(t, n)
	writeDirectProviderOwnedCity(t, n.city)
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(n.city))
	t.Setenv("BD_BIN", n.bdBin)
	t.Setenv(nativeFakeBdLogEnv, n.log)

	backing := newRetiredBdReadStore(t, n.bdBin, n.environ(n.city))
	oldStore := beads.NewCachingStoreForTest(backing, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	oldStore.StartReconciler(ctx, beads.WithStaggerOff(), "")
	select {
	case <-backing.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("old store did not enter its read")
	}

	previousOpen := newControllerStateOpenCityStore
	newControllerStateOpenCityStore = func(string, gate.Mode, beads.NativeTransportMode) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	t.Cleanup(func() { newControllerStateOpenCityStore = previousOpen })
	setControllerStateStoreCloseDelayForTest(t, time.Millisecond)

	swapped := make(chan struct{})
	resume := make(chan struct{})
	updated := make(chan struct{})
	var resumeOnce sync.Once
	controllerStateAfterStoreSwapForTest = func() {
		close(swapped)
		<-resume
	}
	t.Cleanup(func() {
		resumeOnce.Do(func() { close(resume) })
		<-updated
		controllerStateAfterStoreSwapForTest = nil
	})

	done := make(chan struct{})
	mc := managedCityOverStore("swap-gap", oldStore, cancel, done, "20ms")
	mc.cr.cs.cfg = &config.City{}
	mc.cr.cs.cityPath = n.city
	mc.cr.cs.beadStores = map[string]beads.Store{}
	go func() {
		defer close(updated)
		mc.cr.cs.update(&config.City{}, gcruntime.NewFake())
	}()
	select {
	case <-swapped:
	case <-time.After(20 * time.Second):
		t.Fatal("update never reached the post-swap hook")
	}

	if err := quiesceCityStoreWork(mc, io.Discard); err == nil {
		t.Fatal("quiesce reported success while the old store read was blocked")
	}
	var stderr bytes.Buffer
	if err := stopManagedCity(mc, n.city, &stderr); err == nil {
		t.Fatalf("stopManagedCity error = nil, want quiesce timeout; stderr=%s", &stderr)
	}
	for _, call := range n.calls() {
		if strings.Join(call.Args, " ") == "dolt stop" {
			t.Fatalf("provider stopped while the old store read remained in flight; calls=%+v", n.calls())
		}
	}

	backing.unblock()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range n.calls() {
			if strings.Join(call.Args, " ") == "dolt stop" {
				resumeOnce.Do(func() { close(resume) })
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("provider was not stopped after the old store read drained")
}
