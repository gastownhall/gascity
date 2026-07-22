package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestControllerStatePauseResumeBeadEventWorkersUsesCutoverCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	provider := &watchCursorEventsProvider{
		Fake:       events.NewFake(),
		watchAfter: make(chan uint64, 1),
	}
	cs := &controllerState{
		cacheCtx:  ctx,
		eventProv: provider,
	}
	cs.startBeadEventWatcher(ctx)
	select {
	case afterSeq := <-provider.watchAfter:
		if afterSeq != 0 {
			t.Fatalf("initial Watch afterSeq = %d, want empty-log cursor 0", afterSeq)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("initial bead-event watcher did not start")
	}

	if !cs.pauseBeadEventWorkers() {
		t.Fatal("pauseBeadEventWorkers rejected a live controller")
	}
	if cs.beginBeadEventWorker() {
		cs.endBeadEventWorker()
		t.Fatal("beginBeadEventWorker accepted work while reload was paused")
	}
	if !cs.resumeBeadEventWorkers(41, true) {
		t.Fatal("resumeBeadEventWorkers rejected a live controller")
	}

	select {
	case afterSeq := <-provider.watchAfter:
		if afterSeq != 41 {
			t.Fatalf("resumed Watch afterSeq = %d, want cutover cursor 41", afterSeq)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("resumed bead-event watcher did not start")
	}

	cs.stopBeadEventWorkers()
}

func TestControllerStatePauseDrainsWatcherSpawnedAutoclose(t *testing.T) {
	cs := &controllerState{}
	cs.beadEventLoopWorkers.Add(1)

	loopsCanceled := make(chan struct{})
	cs.beadEventWatchCancel = func() { close(loopsCanceled) }
	pauseResult := make(chan bool, 1)
	go func() {
		pauseResult <- cs.pauseBeadEventWorkers()
	}()
	select {
	case <-loopsCanceled:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("pause did not enter the long-lived loop drain phase")
	}

	if !cs.beginBeadEventWorker() {
		t.Fatal("watcher-owned autoclose was rejected while the watcher loop was draining")
	}
	cs.beadEventLoopWorkers.Done()
	waitForBeadEventWorkerState(t, cs, beadEventWorkersPausingAutoclose)
	select {
	case <-pauseResult:
		t.Fatal("pause returned before watcher-owned autoclose drained")
	default:
	}

	cs.endBeadEventWorker()
	select {
	case resumable := <-pauseResult:
		if !resumable {
			t.Fatal("pause reported non-resumable without a final stop")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("pause did not finish after autoclose drained")
	}
	waitForBeadEventWorkerState(t, cs, beadEventWorkersPaused)
}

func TestControllerStateFinalStopRacingReloadPausePreventsWorkerRestart(t *testing.T) {
	cs := &controllerState{}
	if !cs.beginBeadEventWorker() {
		t.Fatal("beginBeadEventWorker rejected work before pause")
	}

	workerRelease := make(chan struct{})
	go func() {
		defer cs.endBeadEventWorker()
		<-workerRelease
	}()

	pauseCancelCalled := make(chan struct{})
	cs.beadEventWatchCancel = func() { close(pauseCancelCalled) }

	pauseResult := make(chan bool, 1)
	go func() {
		pauseResult <- cs.pauseBeadEventWorkers()
	}()
	select {
	case <-pauseCancelCalled:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("reload pause did not cancel the active watcher")
	}

	stopDone := make(chan struct{})
	go func() {
		cs.stopBeadEventWorkers()
		close(stopDone)
	}()
	waitForBeadEventWorkerState(t, cs, beadEventWorkersStoppingAutoclose)

	close(workerRelease)
	select {
	case resumable := <-pauseResult:
		if resumable {
			t.Fatal("pause reported resumable after final stop won the race")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("reload pause did not drain after worker completion")
	}
	select {
	case <-stopDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("final stop did not drain after worker completion")
	}

	if cs.resumeBeadEventWorkers(99, true) {
		t.Fatal("resumeBeadEventWorkers restarted after final stop")
	}
	if cs.beginBeadEventWorker() {
		cs.endBeadEventWorker()
		t.Fatal("beginBeadEventWorker accepted work after final stop")
	}
}

func waitForBeadEventWorkerState(t *testing.T, cs *controllerState, want beadEventWorkerState) {
	t.Helper()
	deadline := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		cs.beadEventLifecycleMu.Lock()
		got := cs.beadEventWorkerState
		cs.beadEventLifecycleMu.Unlock()
		if got == want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("bead-event worker state = %d, want %d", got, want)
		}
	}
}

type watchCursorEventsProvider struct {
	*events.Fake
	watchAfter chan uint64
}

func (p *watchCursorEventsProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	select {
	case p.watchAfter <- afterSeq:
	default:
	}
	return p.Fake.Watch(ctx, afterSeq)
}
