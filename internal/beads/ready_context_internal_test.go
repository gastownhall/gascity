package beads

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

type observedErrContext struct {
	context.Context
	once    sync.Once
	checked chan struct{}
}

type cancelOnErrCheckContext struct {
	context.Context
	cancel   context.CancelFunc
	cancelAt int64
	checks   atomic.Int64
}

func (c *cancelOnErrCheckContext) Err() error {
	if c.checks.Add(1) >= c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

type countingErrContext struct {
	context.Context
	checks atomic.Int64
}

func (c *countingErrContext) Err() error {
	c.checks.Add(1)
	return c.Context.Err()
}

func TestCachingStoreCountContextCancelsWhileWaitingForLock(t *testing.T) {
	store := NewCachingStoreForTest(NewMemStore(), nil)
	store.mu.Lock()
	locked := true
	defer func() {
		if locked {
			store.mu.Unlock()
		}
	}()

	base, cancel := context.WithCancel(context.Background())
	ctx := &observedErrContext{Context: base, checked: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := store.Count(ctx, ListQuery{Status: "open"})
		done <- err
	}()

	select {
	case <-ctx.checked:
	case <-time.After(testutil.GoroutineRaceTimeout):
		store.mu.Unlock()
		locked = false
		select {
		case <-done:
		case <-time.After(testutil.GoroutineRaceTimeout):
		}
		t.Fatal("Count did not check context before waiting for the cache lock")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Count error = %v, want context.Canceled", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		store.mu.Unlock()
		locked = false
		select {
		case <-done:
		case <-time.After(testutil.GoroutineRaceTimeout):
		}
		t.Fatal("Count waited for the cache lock after context cancellation")
	}
}

func TestSortBeadsReadyOrderContextStopsAfterCancellation(t *testing.T) {
	rows := make([]Bead, 128)
	for i := range rows {
		priority := len(rows) - i
		rows[i] = Bead{ID: fmt.Sprintf("gc-%03d", i), Priority: &priority}
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelOnErrCheckContext{Context: base, cancel: cancel, cancelAt: 8}

	err := sortBeadsReadyOrderContext(ctx, rows)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sortBeadsReadyOrderContext error = %v, want context.Canceled", err)
	}
	if checks := ctx.checks.Load(); checks < ctx.cancelAt {
		t.Fatalf("context checks = %d, want at least %d", checks, ctx.cancelAt)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("context returned cancellation without closing Done")
	}
}

func TestSortBeadsReadyOrderBackgroundUsesNonCancellableFastPath(t *testing.T) {
	rows := make([]Bead, 128)
	for i := range rows {
		priority := len(rows) - i
		rows[i] = Bead{ID: fmt.Sprintf("gc-%03d", i), Priority: &priority}
	}
	ctx := &countingErrContext{Context: context.Background()}

	if err := sortBeadsReadyOrderContext(ctx, rows); err != nil {
		t.Fatalf("sortBeadsReadyOrderContext: %v", err)
	}
	if checks := ctx.checks.Load(); checks != 0 {
		t.Fatalf("uncancellable context checks = %d, want 0", checks)
	}
	for i := 1; i < len(rows); i++ {
		if beadReadyLess(rows[i], rows[i-1]) {
			t.Fatalf("rows are not sorted at index %d: %+v before %+v", i, rows[i-1], rows[i])
		}
	}
}

func TestCachedReadyRowsBackgroundUsesCanonicalOrderWithoutErrChecks(t *testing.T) {
	priorityZero, priorityOne := 0, 1
	created := time.Date(2026, time.July, 15, 9, 0, 0, 0, time.UTC)
	openBeads := []Bead{
		{ID: "gc-c", Status: "open", Priority: &priorityOne, CreatedAt: created},
		{ID: "gc-b", Status: "open", Priority: &priorityZero, CreatedAt: created.Add(time.Minute)},
		{ID: "gc-a", Status: "open", Priority: &priorityZero, CreatedAt: created},
	}
	statusByID := map[string]string{"gc-a": "open", "gc-b": "open", "gc-c": "open"}
	ctx := &countingErrContext{Context: context.Background()}

	rows, err := cachedReadyRows(ctx, ReadyQuery{Limit: 2}, statusByID, openBeads, nil, true)
	if err != nil {
		t.Fatalf("cachedReadyRows: %v", err)
	}
	gotIDs := make([]string, len(rows))
	for i := range rows {
		gotIDs[i] = rows[i].ID
	}
	if len(gotIDs) != 2 || gotIDs[0] != "gc-a" || gotIDs[1] != "gc-b" {
		t.Fatalf("cachedReadyRows IDs = %v, want [gc-a gc-b]", gotIDs)
	}
	if checks := ctx.checks.Load(); checks != 0 {
		t.Fatalf("uncancellable context checks = %d, want 0", checks)
	}
}

func TestMemStoreReadyLockedSkipsChecksForUncancellableContext(t *testing.T) {
	store := NewMemStore()
	bead, err := store.Create(Bead{Title: "ready"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ctx := &countingErrContext{Context: context.Background()}

	store.mu.Lock()
	rows, err := store.readyLocked(ctx, ReadyQuery{})
	store.mu.Unlock()
	if err != nil {
		t.Fatalf("readyLocked: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != bead.ID {
		t.Fatalf("readyLocked rows = %+v, want %s", rows, bead.ID)
	}
	if checks := ctx.checks.Load(); checks != 0 {
		t.Fatalf("uncancellable context checks = %d, want 0", checks)
	}
}

func TestMemStoreReadyLockedStopsDuringCancellableScan(t *testing.T) {
	store := NewMemStore()
	for i := 0; i < 32; i++ {
		if _, err := store.Create(Bead{Title: fmt.Sprintf("ready-%02d", i)}); err != nil {
			t.Fatalf("Create bead %d: %v", i, err)
		}
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelOnErrCheckContext{Context: base, cancel: cancel, cancelAt: 8}

	store.mu.Lock()
	rows, err := store.readyLocked(ctx, ReadyQuery{})
	store.mu.Unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readyLocked error = %v, want context.Canceled (rows = %d)", err, len(rows))
	}
	if checks := ctx.checks.Load(); checks < ctx.cancelAt {
		t.Fatalf("context checks = %d, want at least %d", checks, ctx.cancelAt)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("context returned cancellation without closing Done")
	}
}

func (c *observedErrContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

func TestMemStoreReadyContextCancelsWhileWaitingForLock(t *testing.T) {
	store := NewMemStore()
	store.mu.Lock()
	locked := true
	defer func() {
		if locked {
			store.mu.Unlock()
		}
	}()

	base, cancel := context.WithCancel(context.Background())
	ctx := &observedErrContext{Context: base, checked: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := store.ReadyContext(ctx)
		done <- err
	}()
	select {
	case <-ctx.checked: // the first pre-lock context check observed an active context
	case <-time.After(testutil.GoroutineRaceTimeout):
		store.mu.Unlock()
		locked = false
		select {
		case <-done:
		case <-time.After(testutil.GoroutineRaceTimeout):
		}
		t.Fatal("ReadyContext did not check context before waiting for the lock")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadyContext error = %v, want context.Canceled", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		store.mu.Unlock()
		locked = false
		select {
		case <-done:
		case <-time.After(testutil.GoroutineRaceTimeout):
		}
		t.Fatal("ReadyContext waited for the lock after context cancellation")
	}
}

func TestFileStoreReadyContextReportsUnsupported(t *testing.T) {
	store := &FileStore{MemStore: NewMemStore()}
	rows, err := store.ReadyContext(context.Background())
	if !errors.Is(err, ErrReadyContextUnsupported) {
		t.Fatalf("ReadyContext error = %v, want ErrReadyContextUnsupported", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ReadyContext rows = %+v, want none for context-blind file refresh", rows)
	}
}
