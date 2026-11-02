package session

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// scriptedScan is one scan's answer, given once release is closed.
type scriptedScan struct {
	release chan struct{}
	found   []runtime.LiveRuntime
}

// scriptedScanFake is a Fake whose process-table scans answer in turn from
// scans, and which records the roots it is asked to terminate.
type scriptedScanFake struct {
	*runtime.Fake
	scans []scriptedScan

	mu         sync.Mutex
	started    int
	terminated []runtime.LiveRuntime
}

func (f *scriptedScanFake) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	f.mu.Lock()
	scan := f.scans[f.started]
	f.started++
	f.mu.Unlock()
	<-scan.release
	return scan.found, nil
}

func (f *scriptedScanFake) TerminateRuntime(live runtime.LiveRuntime) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, live)
	return nil
}

// A scan the cleanup gave up on may still answer, but its roots are stale by
// then: nothing is terminated from them, and the next cleanup, once the scan
// has returned and released its key, terminates from a fresh scan of its own.
func TestKillExistingOrphansTerminatesFromAFreshScanNotAnAbandonedOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sessionID = "sid-abandoned"
		city := t.TempDir()
		staleRoot := runtime.LiveRuntime{SessionID: sessionID, City: city, PID: 100}
		freshRoot := runtime.LiveRuntime{SessionID: sessionID, City: city, PID: 200}
		abandoned := scriptedScan{release: make(chan struct{}), found: []runtime.LiveRuntime{staleRoot}}
		fresh := scriptedScan{release: make(chan struct{}), found: []runtime.LiveRuntime{freshRoot}}
		close(fresh.release)
		sp := &scriptedScanFake{Fake: runtime.NewFake(), scans: []scriptedScan{abandoned, fresh}}
		var answerOnce sync.Once
		answerAbandoned := func() { answerOnce.Do(func() { close(abandoned.release) }) }
		t.Cleanup(answerAbandoned)
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

		ctx, cancel := context.WithCancel(context.Background())
		done := killOrphansAsync(ctx, mgr, sessionID)
		synctest.Wait()
		cancel()
		synctest.Wait()
		requireUnanswered(t, done, context.Canceled)

		answerAbandoned()
		synctest.Wait()
		sp.mu.Lock()
		lateTerminated := append([]runtime.LiveRuntime(nil), sp.terminated...)
		sp.mu.Unlock()
		if len(lateTerminated) != 0 {
			t.Fatalf("terminated %v from the abandoned scan's late answer", lateTerminated)
		}

		if err := mgr.killExistingOrphans(context.Background(), sessionID); err != nil {
			t.Fatalf("cleanup after the abandoned scan returned: %v", err)
		}
		sp.mu.Lock()
		defer sp.mu.Unlock()
		if sp.started != 2 {
			t.Errorf("scans started = %d, want the abandoned one and a fresh one", sp.started)
		}
		if len(sp.terminated) != 1 || sp.terminated[0].PID != freshRoot.PID {
			t.Errorf("terminated %v, want only the fresh scan's root pid %d", sp.terminated, freshRoot.PID)
		}
	})
}
