package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// wedgeOnStartProvider starts the session and then wedges the store, the way a
// Dolt that stalls once the runtime is up does: the start phase (lease, record,
// PreWake) already ran against a working store, and the commit and the lease's
// release that come after provider.Start are the ones that hang.
type wedgeOnStartProvider struct {
	*runtime.Fake
	wedge func()
}

func (p *wedgeOnStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	err := p.Fake.Start(ctx, name, cfg)
	p.wedge()
	return err
}

// The async start pass hands the async goroutine the slot-only release; the
// lease it took is ended by the start itself, bounded with the commit
// (SESSION-RUNTIME-017). This drives the real executePlannedStartsTraced, so
// the release the pass builds for the goroutine is the one under test: a pass
// that handed over the lease-then-slot composition would run the lease's
// release, which reads and writes the wedged store, before freeing the slot
// and pin the slot behind the store the commit bound gave up on.
func TestExecutePlannedStartsAsyncFreesTheSlotWhenTheCommitAndTheLeaseReleaseHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newCapacityEnv(t, false, "s")
		c := e.pendingCreate(t, "s", "e", nil)
		store := newWedgedStore(e.store)
		defer store.release()
		sp := &wedgeOnStartProvider{Fake: e.sp, wedge: store.arm}

		limiter := newAsyncStartLimiter(1)
		var tracker asyncStartTracker
		var followUps atomic.Int64
		opts := append(append([]startExecutionOption(nil), e.startOptions...),
			withAsyncStartExecution(),
			withAsyncStartLimiter(limiter),
			withAsyncStartTracker(&tracker),
			withAsyncStartFollowUp(func() { followUps.Add(1) }),
		)
		begin := time.Now()

		executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, sp, store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
		finished := tracker.wait(commitWatchdog)
		elapsed := time.Since(begin)

		limiter.mu.Lock()
		inFlight := limiter.inFlight
		limiter.mu.Unlock()
		if store.hung.Load() == 0 {
			t.Fatalf("no store call hung: the scenario no longer reaches the commit under the production lease; log: %s", e.log.String())
		}
		if !finished || inFlight != 0 {
			t.Fatalf("the async start finished=%v with %d starts in flight %v in: the pass handed the goroutine a release that waits on the wedged store (hung: %s); log: %s", finished, inFlight, elapsed, store.callers(), e.log.String())
		}
		if elapsed < commitBound || elapsed > commitBound+time.Second {
			t.Fatalf("the async start finished %v in, want it to give up the commit at the %v bound and free the slot then (hung: %s)", elapsed, commitBound, store.callers())
		}
		if got := followUps.Load(); got != 1 {
			t.Fatalf("follow-up ran %d times, want 1 although the store is wedged (hung: %s)", got, store.callers())
		}
		if !strings.Contains(e.log.String(), "commit_unavailable") {
			t.Fatalf("log = %q, want a commit_unavailable lifecycle line for the abandoned commit", e.log.String())
		}
	})
}
