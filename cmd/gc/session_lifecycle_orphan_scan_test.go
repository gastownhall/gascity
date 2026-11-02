package main

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
)

// wedgedOrphanScanProvider is a Fake whose pre-start orphan scan does not
// answer until answerLate is called (at the latest when the test ends): it
// calls onScan, blocks, then answers as the Fake does.
type wedgedOrphanScanProvider struct {
	*runtime.Fake
	onScan func()

	release     chan struct{}
	releaseOnce sync.Once
}

func newWedgedOrphanScanProvider(t *testing.T) *wedgedOrphanScanProvider {
	p := &wedgedOrphanScanProvider{Fake: runtime.NewFake(), release: make(chan struct{})}
	t.Cleanup(p.answerLate)
	return p
}

// answerLate lets every blocked scan return.
func (p *wedgedOrphanScanProvider) answerLate() {
	p.releaseOnce.Do(func() { close(p.release) })
}

func (p *wedgedOrphanScanProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	if p.onScan != nil {
		p.onScan()
	}
	<-p.release
	return p.Fake.FindRuntimesBySessionID(id)
}

// A prepared start whose pre-start orphan scan cannot answer is deferred
// without waiting out the wave: no startup-health failure, no pending-create
// rollback, and no provider Start, not even once the wedged scan answers late.
// That holds when the scan budget runs out under a live start context, and
// when the start's own context expires or is canceled during the scan.
func TestExecutePreparedStartWaveDefersUnfinishedOrphanScan(t *testing.T) {
	for _, tc := range []struct {
		name           string
		startupTimeout time.Duration
		cancelOnScan   bool
	}{
		{name: "scan budget runs out under a live start context", startupTimeout: time.Minute},
		{name: "start deadline expires during the scan", startupTimeout: 200 * time.Millisecond},
		{name: "start canceled during the scan", startupTimeout: 10 * time.Second, cancelOnScan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := beads.NewMemStore()
				clk := &clock.Fake{Time: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
				workDir := t.TempDir()
				bead, before := createStartRecoveryFixture(t, store, clk, workDir)
				sp := newWedgedOrphanScanProvider(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.cancelOnScan {
					sp.onScan = cancel
				}

				began := time.Now()
				result := executeStartRecoveryUnder(ctx, t, store, sp, bead, workDir, tc.startupTimeout)
				if waited, waveDeadline := time.Since(began), tc.startupTimeout+waveDeadlineSlack; waited >= waveDeadline {
					t.Errorf("start wave waited %v, want the start to report before the wave's own deadline %v", waited, waveDeadline)
				}
				sp.answerLate()
				synctest.Wait()

				assertUnavailableStartRecoveryDeferred(t, store, clk, bead.ID, before, result)
				if got := sp.CountCalls("Start", "worker"); got != 0 {
					t.Fatalf("Start calls = %d, want none after an unanswered orphan scan, even once it answers", got)
				}
			})
		})
	}
}
