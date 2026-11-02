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
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// orphanScanWedgedProvider is a Fake whose pre-start process-table scan never
// returns until released, as a wedged process-table read would. Its other
// operations answer normally, so a start under it proceeds until the
// pre-start orphan sweep.
type orphanScanWedgedProvider struct {
	*runtime.Fake
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
	unblockOnce sync.Once
	// after is what the wedged scan answers once released; the wave defers
	// long before then, so it stays empty.
	after []runtime.LiveRuntime
	err   error
}

func (p *orphanScanWedgedProvider) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	p.enteredOnce.Do(func() { close(p.entered) })
	<-p.release
	return p.after, p.err
}

func (p *orphanScanWedgedProvider) unblock() {
	p.unblockOnce.Do(func() { close(p.release) })
}

// orphanScanStartWave runs one prepared start of a seeded session row through
// the wave under sp, on the fake clock, and fails the test if the wave does
// not return well past the scan budget: a wedged pre-start orphan scan must
// defer the candidate, not hold the wave.
func orphanScanStartWave(ctx context.Context, t *testing.T, sp *orphanScanWedgedProvider, store beads.Store, startupTimeout time.Duration) startResult {
	t.Helper()
	city := t.TempDir()
	rowMgr := newSessionManagerWithConfig("", store, runtime.NewFake(), nil)
	info, err := rowMgr.CreateSession(context.Background(), sessionpkg.CreateOptions{
		BeadOnly: true, Template: "worker", Title: "Worker", Command: "claude",
		WorkDir: t.TempDir(), Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	item, release := leasedStart(t, city, preparedStart{
		candidate: startCandidate{
			info: sessiontest.SeedBead(t, bead),
			tp:   TemplateParams{Command: "claude", SessionName: bead.Metadata["session_name"], TemplateName: "worker"},
		},
		cfg: runtime.Config{Command: "claude", WorkDir: info.WorkDir},
	})
	defer release()

	results := make(chan []startResult, 1)
	go func() {
		results <- executePreparedStartWaveForCity(ctx, []preparedStart{item}, city, sp, store, nil, startupTimeout, 1)
	}()
	select {
	case got := <-results:
		if len(got) != 1 {
			t.Fatalf("len(results) = %d, want 1", len(got))
		}
		return got[0]
	case <-time.After(5 * time.Minute):
		t.Fatalf("the start wave never returned: the wedged pre-start orphan scan holds it (startup timeout %v)", startupTimeout)
		return startResult{}
	}
}

// requireOrphanScanDeferral checks the wave deferred the candidate without an
// attributed error, a rollback, an abandonment, or any provider Start.
func requireOrphanScanDeferral(t *testing.T, result startResult, sp *orphanScanWedgedProvider) {
	t.Helper()
	if result.err != nil || result.outcome != TraceOutcomeDeferred || result.rollbackPending || result.abandoned {
		t.Fatalf("result = %#v, want deferred without error, rollback, or abandonment", result)
	}
	if starts := sp.CountCalls("Start", result.prepared.candidate.name()); starts != 0 {
		t.Fatalf("Start calls = %d, want 0", starts)
	}
}

// An unfinished pre-start orphan scan defers the candidate: the wave returns a
// deferred result within its deadline (never abandoning the candidate to the
// wave deadline), whether the scan budget, the start's own earlier deadline,
// or a canceled start context ends the wait.
func TestExecutePreparedStartWaveDefersUnfinishedOrphanScan(t *testing.T) {
	synctestSub := func(name string, fn func(t *testing.T, sp *orphanScanWedgedProvider, store beads.Store)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := beads.NewMemStore()
				sp := &orphanScanWedgedProvider{Fake: runtime.NewFake(), entered: make(chan struct{}), release: make(chan struct{})}
				defer sp.unblock()
				fn(t, sp, store)
			})
		})
	}

	// The scan budget (10s) bounds a start with no deadline of its own.
	synctestSub("scan budget bounds a start with no deadline of its own", func(t *testing.T, sp *orphanScanWedgedProvider, store beads.Store) {
		requireOrphanScanDeferral(t, orphanScanStartWave(context.Background(), t, sp, store, 0), sp)
	})

	// The start's own earlier startup timeout bounds it below the budget.
	synctestSub("the start's own earlier deadline bounds the scan", func(t *testing.T, sp *orphanScanWedgedProvider, store beads.Store) {
		requireOrphanScanDeferral(t, orphanScanStartWave(context.Background(), t, sp, store, time.Second), sp)
	})

	// A start context canceled while the scan runs defers the same way.
	synctestSub("a start context canceled mid-scan defers", func(t *testing.T, sp *orphanScanWedgedProvider, store beads.Store) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-sp.entered
			cancel()
		}()
		requireOrphanScanDeferral(t, orphanScanStartWave(ctx, t, sp, store, 30*time.Second), sp)
	})

	// A pending create keeps its row and claim through the deferral, and
	// an unanswered scan accrues no startup health.
	synctestSub("pending create keeps its row and claim through the deferral", func(t *testing.T, sp *orphanScanWedgedProvider, store beads.Store) {
		result := orphanScanStartWave(context.Background(), t, sp, store, 10*time.Second)
		requireOrphanScanDeferral(t, result, sp)
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		if commitStartResult(result, sessionFrontDoor(store), clk, events.Discard, 0, ioDiscard{}, ioDiscard{}) {
			t.Fatal("deferred result counted as committed wake")
		}
		row, err := store.Get(result.prepared.candidate.info.ID)
		if err != nil {
			t.Fatalf("Get session: %v", err)
		}
		if row.Status != "open" || row.Metadata["pending_create_claim"] != "true" {
			t.Fatalf("row after deferral = status %q claim %q, want open with the pending create claim intact", row.Status, row.Metadata["pending_create_claim"])
		}
		if got := startupHealthCount(t, store, row.Metadata["session_name"]); got != 0 {
			t.Fatalf("startup health = %d, want 0: an unanswered scan accrues no startup health", got)
		}
	})
}
