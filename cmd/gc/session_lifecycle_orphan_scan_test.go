package main

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// orphanScanWatchdog is far past any wave deadline in these tests. It exists
// only so a wedged start fails the test with a message instead of deadlocking
// the synctest bubble.
const orphanScanWatchdog = 5 * time.Minute

// orphanScanWedgedProvider is a Fake whose process-table scan wedges until
// released, as a stalled process-table read would, so a prepared start's
// pre-start orphan cleanup gets no answer.
type orphanScanWedgedProvider struct {
	*runtime.Fake
	releaseOnce sync.Once
	released    chan struct{}
	scanBegan   chan struct{}         // one buffered send per scan begun
	scanRoots   []runtime.LiveRuntime // the answer the scan gives once released
	scanErr     error                 // the answer the scan gives once released
}

func newOrphanScanWedgedProvider() *orphanScanWedgedProvider {
	return &orphanScanWedgedProvider{
		Fake:      runtime.NewFake(),
		released:  make(chan struct{}),
		scanBegan: make(chan struct{}, 8),
	}
}

func (p *orphanScanWedgedProvider) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	p.scanBegan <- struct{}{}
	<-p.released
	return p.scanRoots, p.scanErr
}

func (p *orphanScanWedgedProvider) release() {
	p.releaseOnce.Do(func() { close(p.released) })
}

// TestExecutePreparedStartWaveDefersUnfinishedOrphanScan: a prepared start
// whose pre-start orphan scan cannot answer is classified and committed as a
// deferral — no startup-health failure, no pending-create rollback, no
// provider Start — whether the scan's own budget expires first, the start's
// shorter deadline does, or the start is canceled mid-scan. Without the scan
// bound, the wave abandons the start instead of classifying it.
func TestExecutePreparedStartWaveDefersUnfinishedOrphanScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, tc := range []struct {
			name           string
			slug           string
			startupTimeout time.Duration
			cancelMidScan  bool
		}{
			{name: "scan budget expires first", slug: "budget", startupTimeout: 30 * time.Second},
			{name: "start deadline first", slug: "deadline", startupTimeout: 2 * time.Second},
			{name: "start canceled mid-scan", slug: "cancel", startupTimeout: 10 * time.Second, cancelMidScan: true},
		} {
			store := beads.NewMemStore()
			name := "worker-" + tc.slug
			clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
			bead, err := store.Create(beads.Bead{
				Title: name, Type: sessionBeadType,
				Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"session_name":         name,
					"template":             "worker",
					"state":                "creating",
					"pending_create_claim": "true",
					"generation":           "1",
					"instance_token":       "tok-" + tc.slug,
					"wake_attempts":        "2",
					"last_woke_at":         clk.Now().Format(time.RFC3339),
				},
			})
			if err != nil {
				t.Fatalf("%s: Create session: %v", tc.name, err)
			}
			id := bead.ID
			city := t.TempDir()
			sp := newOrphanScanWedgedProvider()
			defer sp.release()
			item, releaseLease := leasedStart(t, city, preparedStart{
				candidate: startCandidate{
					info: sessiontest.SeedBead(t, bead),
					tp:   TemplateParams{Command: "claude", SessionName: name, TemplateName: "worker"},
				},
				cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
			})
			defer releaseLease()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			results := make(chan []startResult, 1)
			go func() {
				results <- executePreparedStartWaveForCity(ctx, []preparedStart{item}, city, sp, store, nil, tc.startupTimeout, 1)
			}()
			if tc.cancelMidScan {
				<-sp.scanBegan
				cancel()
			}

			var wave []startResult
			select {
			case wave = <-results:
			case <-time.After(orphanScanWatchdog):
				t.Fatalf("%s: the wave still had not returned: the wedged orphan scan is unbounded", tc.name)
			}
			if len(wave) != 1 {
				t.Fatalf("%s: len(results) = %d, want 1", tc.name, len(wave))
			}
			result := wave[0]
			if result.outcome != TraceOutcomeDeferred || result.err != nil || result.rollbackPending {
				t.Fatalf("%s: result = %#v, want a deferral with no error and no rollback", tc.name, result)
			}
			if result.abandoned {
				t.Fatalf("%s: the wave abandoned the start instead of classifying the deferred scan (result = %#v)", tc.name, result)
			}
			if starts := sp.CountCalls("Start", name); starts != 0 {
				t.Fatalf("%s: provider Starts = %d, want 0: an unanswered scan starts nothing", tc.name, starts)
			}

			// The wedged scan's goroutine must drain before the commit reads
			// the row, so it cannot race the assertions.
			sp.release()
			synctest.Wait()
			if commitStartResult(result, sessionFrontDoor(store), clk, events.Discard, 0, ioDiscard{}, ioDiscard{}) {
				t.Fatalf("%s: the deferred start counted as a committed wake", tc.name)
			}
			got, err := store.Get(id)
			if err != nil {
				t.Fatalf("%s: Get session: %v", tc.name, err)
			}
			if got.Metadata["wake_attempts"] != "2" {
				t.Fatalf("%s: wake_attempts = %q, want 2: a deferred scan accrues no startup-health failure", tc.name, got.Metadata["wake_attempts"])
			}
			if got.Metadata["pending_create_claim"] != "true" {
				t.Fatalf("%s: pending_create_claim = %q, want true: no pending-create rollback", tc.name, got.Metadata["pending_create_claim"])
			}
		}
	})
}
