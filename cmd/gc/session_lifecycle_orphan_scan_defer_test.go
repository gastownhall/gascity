package main

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
)

// preStartOrphanScanBound is how long the session manager waits for its
// pre-start orphan scan. These tests run on synctest's fake clock.
const preStartOrphanScanBound = 10 * time.Second

// orphanScanHangProvider is a postStartProvider whose pre-start orphan scan
// hangs the way a wedged process-table read does: it ignores every context and
// returns only on release. onScan, when set, runs as the scan begins.
type orphanScanHangProvider struct {
	*postStartProvider
	onScan func()
}

func (p *orphanScanHangProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	if p.onScan != nil {
		p.onScan()
	}
	p.hang("FindRuntimesBySessionID")
	return p.Fake.FindRuntimesBySessionID(id)
}

// TestRunPreparedStartCandidateDefersWhenThePreStartOrphanScanHangs: a
// reconciler start whose pre-start orphan scan never answers gives up on it
// within the scan's budget, or sooner when its start context ends, and defers
// the start: no provider Start, no wake failure and no pending-create
// rollback. The scan's late answer changes nothing.
func TestRunPreparedStartCandidateDefersWhenThePreStartOrphanScanHangs(t *testing.T) {
	for _, tc := range []struct {
		name           string
		startupTimeout time.Duration
		cancelOnScan   bool
		// within is how long the attempt may take: the scan budget, or the
		// start context's earlier end.
		within time.Duration
	}{
		{name: "scan outlives its budget", startupTimeout: postStartTimeout, within: preStartOrphanScanBound},
		{name: "start deadline expires mid-scan", startupTimeout: 2 * time.Second, within: 2 * time.Second},
		{name: "start canceled mid-scan", startupTimeout: postStartTimeout, cancelOnScan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				sp := &orphanScanHangProvider{postStartProvider: newPostStartProvider()}
				if tc.cancelOnScan {
					sp.onScan = cancel
				}
				defer sp.release()
				store, bead, before, item := postStartBeadItem(t, clk, workDir)

				sp.city = t.TempDir()
				item, release := leasedStart(t, sp.city, item)
				defer release()
				done := make(chan startResult, 1)
				begin := time.Now()
				go func() {
					done <- runPreparedStartCandidate(ctx, item, sp.city, sp, store, nil, tc.startupTimeout, immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
				}()
				var result startResult
				select {
				case result = <-done:
				case <-time.After(postStartWatchdog):
					t.Fatalf("runPreparedStartCandidate had not returned %v into the attempt: the pre-start orphan scan wait is unbounded; hung provider calls: %s", postStartWatchdog, sp.hungIn())
				}
				elapsed := time.Since(begin)

				if got := sp.hungIn(); got != "FindRuntimesBySessionID" {
					t.Fatalf("hung provider calls = %s, want exactly the pre-start orphan scan", got)
				}
				if elapsed > tc.within {
					t.Fatalf("the attempt took %v, want it back within %v", elapsed, tc.within)
				}
				if got := sp.CountCalls("Start", "worker"); got != 0 {
					t.Fatalf("Start calls = %d, want 0 while the orphan scan has not answered", got)
				}
				assertUnavailableStartRecoveryDeferred(t, store, clk, bead.ID, before, result)

				settled := mustGetBead(t, store, bead.ID)
				releaseAndSettle(t, sp.postStartProvider)
				requireBeadUnchanged(t, store, settled)
				if got := sp.CountCalls("Start", "worker"); got != 0 {
					t.Fatalf("Start calls after the scan answered late = %d, want 0", got)
				}
			})
		})
	}
}
