package main

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// blockingListProvider stands in for any shutdown-path call that wedges on an
// unresponsive backend (a bd read against a swap-thrashing dolt, a tmux list
// against a starved server): ListRunning blocks until release is closed.
type blockingListProvider struct {
	runtime.Provider
	entered chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func (p *blockingListProvider) ListRunning(prefix string) ([]string, error) {
	if p.once.CompareAndSwap(false, true) {
		close(p.entered)
		<-p.release
	}
	return p.Provider.ListRunning(prefix)
}

// TestStopManagedCityBoundsAForcedStopWhileShutdownIsAlreadyRunning covers
// issue #5256: the city's run loop had already entered CityRuntime.shutdown()
// when its context was canceled, so the forced-stop escalation blocked in
// shutdownOnce.Do behind that in-flight shutdown and its own forced timeout
// never got a chance to fire.
func TestStopManagedCityBoundsAForcedStopWhileShutdownIsAlreadyRunning(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS", "mem:")
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	sp := &blockingListProvider{
		Provider: runtime.NewFake(),
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	cr := &CityRuntime{
		cfg: &config.City{
			Daemon: config.DaemonConfig{ShutdownTimeout: "20ms"},
		},
		sp:                sp,
		rec:               events.Discard,
		stdout:            io.Discard,
		stderr:            io.Discard,
		forceStopShutdown: &atomic.Bool{},
	}
	mc := &managedCity{
		name:   "bright-lights",
		cancel: func() {},
		done:   make(chan struct{}), // the city goroutine never gets to exit
		cr:     cr,
	}

	panicCh := make(chan any, 1)
	shutdownDone := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(sp.release) }) }
	defer func() {
		closeRelease()
		<-shutdownDone
	}()

	// The run loop's deferred shutdown is already in flight and wedged.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
			close(shutdownDone)
		}()
		cr.shutdown()
	}()

	select {
	case <-sp.entered:
	case <-time.After(time.Second):
		closeRelease()
		<-shutdownDone
		select {
		case r := <-panicCh:
			t.Fatalf("cr.shutdown() panicked before reaching provider: %v", r)
		default:
			t.Fatal("shutdown never reached the wedged provider call")
		}
	}

	returned := make(chan error, 1)
	start := time.Now()
	go func() { returned <- stopManagedCity(mc, cityPath, &bytes.Buffer{}) }()

	// grace (20ms) + forced (5x = 100ms) plus generous slack.
	select {
	case err := <-returned:
		closeRelease()
		<-shutdownDone
		select {
		case r := <-panicCh:
			t.Fatalf("cr.shutdown() panicked: %v", r)
		default:
		}
		if err == nil {
			t.Fatal("stopManagedCity err = nil, want non-nil because the city never exited")
		}
	case <-time.After(time.Second):
		t.Fatalf("stop still blocked after %s: the forced-stop timeout "+
			"cannot fire when cr.shutdown() is called synchronously into an "+
			"already-held shutdownOnce (#5256)", time.Since(start))
	}
}
