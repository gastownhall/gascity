package api

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// afterSeqRecordingProvider is an events.Provider test double that reports a
// fixed LatestSeq and records every afterSeq passed to Watch. Tests use it to
// assert a caller attached at head (afterSeq == latestSeq) rather than at
// Watch(0) — the cursor that this PR redefines as "replay the entire retained
// history across archives", which triggers an archive gunzip/backfill.
type afterSeqRecordingProvider struct {
	mu        sync.Mutex
	latestSeq uint64
	latestErr error
	watchArgs []uint64
}

func (p *afterSeqRecordingProvider) Record(events.Event) {}

func (p *afterSeqRecordingProvider) List(events.Filter) ([]events.Event, error) { return nil, nil }

func (p *afterSeqRecordingProvider) LatestSeq() (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latestSeq, p.latestErr
}

func (p *afterSeqRecordingProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	p.mu.Lock()
	p.watchArgs = append(p.watchArgs, afterSeq)
	p.mu.Unlock()
	return newBlockingWatcher(ctx), nil
}

func (p *afterSeqRecordingProvider) Close() error { return nil }

func (p *afterSeqRecordingProvider) watchedAfterSeqs() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint64(nil), p.watchArgs...)
}

// blockingWatcher is a minimal events.Watcher whose Next blocks until the
// parent context is canceled or Close is called, mirroring a real watcher so
// the multiplexer's fan-in goroutine does not spin.
type blockingWatcher struct {
	ctx  context.Context
	done chan struct{}
	once sync.Once
}

func newBlockingWatcher(ctx context.Context) *blockingWatcher {
	return &blockingWatcher{ctx: ctx, done: make(chan struct{})}
}

func (w *blockingWatcher) Next() (events.Event, error) {
	select {
	case <-w.ctx.Done():
		return events.Event{}, w.ctx.Err()
	case <-w.done:
		return events.Event{}, errors.New("watcher closed")
	}
}

func (w *blockingWatcher) Close() error {
	w.once.Do(func() { close(w.done) })
	return nil
}

// TestGlobalEventStreamPrecheckAttachesAtHeadNotZero guards the iteration-3
// review finding: the mandatory global-SSE precheck used to attach with
// mux.Watch(ctx, nil), and nil per-city cursors default to Watch(0), which this
// PR redefines as a full retained-history replay across archives. Because
// Multiplexer.Watch eagerly drives each child's Next, that bare probe could
// gunzip/decode archived batches for every city just to discard them. The
// precheck must instead attach at each city's head cursor.
func TestGlobalEventStreamPrecheckAttachesAtHeadNotZero(t *testing.T) {
	alpha := newFakeState(t)
	alpha.cityName = "alpha"
	alphaProv := &afterSeqRecordingProvider{latestSeq: 5}
	alpha.eventProv = alphaProv

	beta := newFakeState(t)
	beta.cityName = "beta"
	betaProv := &afterSeqRecordingProvider{latestSeq: 3}
	beta.eventProv = betaProv

	sm := newTestSupervisorMux(t, map[string]*fakeState{
		"alpha": alpha,
		"beta":  beta,
	})

	if err := sm.precheckGlobalEventStream(context.Background(), &SupervisorEventStreamInput{}); err != nil {
		t.Fatalf("precheckGlobalEventStream: %v", err)
	}

	// afterSeq < latestSeq is exactly the condition that triggers a full
	// retained-history archive backfill (internal/events/recorder.go). Attaching
	// at afterSeq == latestSeq keeps the probe cheap.
	assertAttachedAtHead := func(name string, prov *afterSeqRecordingProvider, want uint64) {
		t.Helper()
		got := prov.watchedAfterSeqs()
		if len(got) != 1 {
			t.Fatalf("%s: Watch called %d times, want exactly 1: %v", name, len(got), got)
		}
		if got[0] != want {
			t.Errorf("%s: precheck attached at afterSeq=%d, want %d (head); afterSeq<latest would trigger archive backfill", name, got[0], want)
		}
	}
	assertAttachedAtHead("alpha", alphaProv, 5)
	assertAttachedAtHead("beta", betaProv, 3)
}

// TestResolveGlobalStreamCursors pins the shared cursor-resolution helper used
// by both the precheck and streamGlobalEvents so neither can regress into a
// Watch(0) full-history flood.
func TestResolveGlobalStreamCursors(t *testing.T) {
	newMux := func() *events.Multiplexer {
		mux := events.NewMultiplexer()
		mux.Add("alpha", &afterSeqRecordingProvider{latestSeq: 5})
		mux.Add("beta", &afterSeqRecordingProvider{latestSeq: 3})
		return mux
	}

	t.Run("head start resolves every city to its latest cursor", func(t *testing.T) {
		cursors, err := resolveGlobalStreamCursors(newMux(), "")
		if err != nil {
			t.Fatalf("resolveGlobalStreamCursors: %v", err)
		}
		if cursors["alpha"] != 5 || cursors["beta"] != 3 {
			t.Fatalf("cursors = %v, want alpha=5 beta=3", cursors)
		}
	})

	t.Run("resume preserves present cities and floors omitted cities to latest", func(t *testing.T) {
		// A resume cursor that names alpha at 2 but omits the registered beta.
		resume := events.FormatCursor(map[string]uint64{"alpha": 2})
		cursors, err := resolveGlobalStreamCursors(newMux(), resume)
		if err != nil {
			t.Fatalf("resolveGlobalStreamCursors: %v", err)
		}
		if cursors["alpha"] != 2 {
			t.Errorf("alpha = %d, want 2 (resume position preserved)", cursors["alpha"])
		}
		if cursors["beta"] != 3 {
			t.Errorf("beta = %d, want 3 (omitted city floored to latest, not Watch(0))", cursors["beta"])
		}
	})

	t.Run("fails closed when latest cursor errors", func(t *testing.T) {
		mux := events.NewMultiplexer()
		mux.Add("alpha", &afterSeqRecordingProvider{latestErr: errors.New("boom")})
		if _, err := resolveGlobalStreamCursors(mux, ""); err == nil {
			t.Fatal("expected error when LatestCursor fails, got nil")
		}
	})
}
