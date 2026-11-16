package beads

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/platform/filelock"
)

// flockProbe wraps the flock seam: it calls through, counts every attempt and
// signals once on the first contended one. A gated probe also parks every
// attempt after the first on hold, so a test can act while a creator is
// provably mid-wait without sleeping.
type flockProbe struct {
	attempts atomic.Int64
	busy     chan struct{}
	hold     chan struct{}
	unpark   sync.Once
}

func installFlockProbe(t *testing.T, gated bool) *flockProbe {
	t.Helper()
	p := &flockProbe{busy: make(chan struct{}, 1)}
	if gated {
		p.hold = make(chan struct{})
	}
	previous := flockAcquire
	flockAcquire = func(f *os.File, opts filelock.Options) (*filelock.Lock, error) {
		n := p.attempts.Add(1)
		if gated && n > 1 {
			<-p.hold
		}
		lock, err := previous(f, opts)
		if errors.Is(err, filelock.ErrBusy) {
			select {
			case p.busy <- struct{}{}:
			default:
			}
		}
		return lock, err
	}
	t.Cleanup(func() { flockAcquire = previous })
	return p
}

// release unparks a gated probe; it is safe to call more than once.
func (p *flockProbe) release() {
	if p.hold == nil {
		return
	}
	p.unpark.Do(func() { close(p.hold) })
}

// holdForeignSequenceLock takes the store directory's flock from an
// independent descriptor, as another process would, and returns a function
// that lets go. It is also released at cleanup.
func holdForeignSequenceLock(t *testing.T, dir string) (release func()) {
	t.Helper()
	f, label, err := filelock.OpenDirectoryLock(dir)
	if err != nil {
		t.Fatalf("open foreign lock directory: %v", err)
	}
	lock, err := filelock.Acquire(f, filelock.Options{Mode: filelock.Exclusive, NonBlocking: true, Label: label})
	if err != nil {
		_ = f.Close()
		t.Fatalf("take foreign sequence lock: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = lock.Release()
			_ = f.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// setSequenceLockWait lowers the floor-lock wait budget for the test.
func setSequenceLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	previous := sqliteSequenceLockWait
	sqliteSequenceLockWait = d
	t.Cleanup(func() { sqliteSequenceLockWait = previous })
}

// setBlockHeadroom leaves the store's reserved block with exactly remaining
// unissued ids. The store must already hold a reservation.
func setBlockHeadroom(t *testing.T, s *SQLiteStore, remaining int64) {
	t.Helper()
	s.sequenceFloorMu.Lock()
	defer s.sequenceFloorMu.Unlock()
	if !s.sequenceReserved {
		t.Fatal("store holds no reserved block; mint once first")
	}
	s.seq = s.sequenceLimit - remaining
}

func countBeads(t *testing.T, s *SQLiteStore) int {
	t.Helper()
	all, err := s.List(ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return len(all)
}

// within runs fn and fails the test, instead of hanging it, when fn does not
// return inside sequenceFloorPersistDeadline.
func within(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(sequenceFloorPersistDeadline):
		t.Fatalf("%s did not return within %s", what, sequenceFloorPersistDeadline)
		return nil
	}
}

// requireStoreTimeout checks the shape every bounded-wait failure must have: it
// matches the sentinel and the deadline, and its text cannot be mistaken for
// SQLITE_BUSY by isSQLiteBusy's substring match.
func requireStoreTimeout(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrStoreTimeout) {
		t.Fatalf("err = %v, want errors.Is ErrStoreTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want errors.Is context.DeadlineExceeded", err)
	}
	requireNotRetriedAsBusy(t, err)
}

func requireNotRetriedAsBusy(t *testing.T, err error) {
	t.Helper()
	if isSQLiteBusy(err) {
		t.Fatalf("err = %v would be retried by retryOnBusy", err)
	}
	if msg := err.Error(); strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked") {
		t.Fatalf("err = %q carries a busy substring", msg)
	}
}

func readSidecar(t *testing.T, s *SQLiteStore) []byte {
	t.Helper()
	data, err := os.ReadFile(s.sequenceFloorPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read sidecar: %v", err)
	}
	return data
}

// TestSQLiteCreateTopUpWaitsOutsideTheTransaction is AT-S2.1. An auto-id create
// at a low block waits for the directory lock BEFORE its transaction, so the
// single writer connection stays free for other writers while it waits, and the
// wait ends in ErrStoreTimeout with nothing written.
func TestSQLiteCreateTopUpWaitsOutsideTheTransaction(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	seed := mustMint(t, store)
	setBlockHeadroom(t, store, 10)
	before := countBeads(t, store)
	setSequenceLockWait(t, 50*time.Millisecond)
	probe := installFlockProbe(t, true)
	t.Cleanup(probe.release)
	holdForeignSequenceLock(t, dir)

	created := make(chan error, 1)
	go func() {
		_, err := store.Create(Bead{Title: "stalled"})
		created <- err
	}()
	select {
	case <-probe.busy:
	case <-time.After(sequenceFloorPersistDeadline):
		t.Fatal("Create never reached a contended lock attempt")
	}

	// Create is parked mid-wait. An Update needs the writer connection and
	// SQLite's write lock: it completes only if Create holds neither.
	description := "updated while the create waits"
	if err := within(t, "Update during the create's lock wait", func() error {
		return store.Update(seed, UpdateOpts{Description: &description})
	}); err != nil {
		t.Fatalf("Update during the wait: %v", err)
	}

	probe.release()
	var err error
	select {
	case err = <-created:
	case <-time.After(sequenceFloorPersistDeadline):
		t.Fatal("Create did not give up after its wait budget")
	}
	requireStoreTimeout(t, err)
	if got := countBeads(t, store); got != before {
		t.Fatalf("beads = %d after a timed-out create, want %d", got, before)
	}
}

// TestSQLiteCreateWithHeadroomTakesNoFlock is AT-S2.2: with at least the
// headroom margin of ids left, a create needs no lock at all.
func TestSQLiteCreateWithHeadroomTakesNoFlock(t *testing.T) {
	for _, remaining := range []int64{sqliteSequenceTopUpHeadroom, 500} {
		dir := t.TempDir()
		store := openSeqStore(t, dir)
		mustMint(t, store)
		setBlockHeadroom(t, store, remaining)
		probe := installFlockProbe(t, false)
		holdForeignSequenceLock(t, dir)
		if err := within(t, "Create with headroom", func() error {
			_, err := store.Create(Bead{Title: "has headroom"})
			return err
		}); err != nil {
			t.Fatalf("remaining=%d: Create under a foreign lock: %v", remaining, err)
		}
		if got := probe.attempts.Load(); got != 0 {
			t.Fatalf("remaining=%d: flock attempts = %d, want 0", remaining, got)
		}
	}
}

// TestSQLiteCreateTopsUpBeforeTheBlockRunsOut pins the top-up itself: below the
// headroom margin a create reserves the next block before its transaction, with
// one lock attempt, and the ids stay unique and increasing across the gap.
func TestSQLiteCreateTopsUpBeforeTheBlockRunsOut(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	first := autoValue(t, mustMint(t, store))
	setBlockHeadroom(t, store, sqliteSequenceTopUpHeadroom-1)
	floorBefore, err := readSQLiteSequenceFloor(store.sequenceFloorPath)
	if err != nil {
		t.Fatal(err)
	}
	probe := installFlockProbe(t, false)
	second := autoValue(t, mustMint(t, store))
	if got := probe.attempts.Load(); got != 1 {
		t.Fatalf("flock attempts = %d, want 1 (the top-up)", got)
	}
	floorAfter, err := readSQLiteSequenceFloor(store.sequenceFloorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !sequenceRankAbove(floorAfter, floorBefore) {
		t.Fatalf("floor %d after the top-up, want it above %d", floorAfter, floorBefore)
	}
	if second <= first {
		t.Fatalf("ids went %d then %d, want strictly increasing", first, second)
	}
	third := autoValue(t, mustMint(t, store))
	if third <= second {
		t.Fatalf("ids went %d then %d, want strictly increasing", second, third)
	}
	if got := probe.attempts.Load(); got != 1 {
		t.Fatalf("flock attempts = %d after the next mint, want still 1", got)
	}
}

// TestSQLiteTxMintAtBlockBoundaryMakesOneAttempt is AT-S2.3: a mint inside a Tx
// never waits for the directory lock. With the block exhausted and the lock
// held elsewhere it makes exactly one attempt and fails with the sentinel
// alone, nothing committed.
func TestSQLiteTxMintAtBlockBoundaryMakesOneAttempt(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	mustMint(t, store)
	setBlockHeadroom(t, store, 0)
	before := countBeads(t, store)
	probe := installFlockProbe(t, false)
	holdForeignSequenceLock(t, dir)

	err := within(t, "Tx with an in-transaction mint at the block boundary", func() error {
		return store.Tx("", func(tx Tx) error {
			_, err := tx.Create(Bead{Title: "in tx"})
			return err
		})
	})
	if !errors.Is(err, ErrStoreTimeout) {
		t.Fatalf("err = %v, want errors.Is ErrStoreTimeout", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v leaks context.Canceled", err)
	}
	requireNotRetriedAsBusy(t, err)
	if got := probe.attempts.Load(); got != 1 {
		t.Fatalf("flock attempts = %d, want exactly 1", got)
	}
	if got := countBeads(t, store); got != before {
		t.Fatalf("beads = %d after the failed Tx, want %d", got, before)
	}
}

// TestSQLiteSequenceFloorWritersGiveUpOnAHeldLock is AT-S2.4: the operator
// repair and SetSequenceFloor return ErrStoreTimeout instead of hanging when
// another holder keeps the directory lock, leave the sidecar alone, and still
// succeed once the lock is free.
func TestSQLiteSequenceFloorWritersGiveUpOnAHeldLock(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	mustMint(t, store)
	setSequenceLockWait(t, 50*time.Millisecond)
	release := holdForeignSequenceLock(t, dir)
	sidecar := readSidecar(t, store)

	err := within(t, "RaiseSQLiteSequenceFloor under a held lock", func() error {
		_, err := RaiseSQLiteSequenceFloor(dir, sqliteGraphPrefix, 5000)
		return err
	})
	requireStoreTimeout(t, err)
	err = within(t, "SetSequenceFloor under a held lock", func() error {
		return store.SetSequenceFloor(6000)
	})
	requireStoreTimeout(t, err)
	if got := readSidecar(t, store); string(got) != string(sidecar) {
		t.Fatalf("sidecar changed from %q to %q by calls that timed out", sidecar, got)
	}

	release()
	if err := within(t, "RaiseSQLiteSequenceFloor with a free lock", func() error {
		_, err := RaiseSQLiteSequenceFloor(dir, sqliteGraphPrefix, 5000)
		return err
	}); err != nil {
		t.Fatalf("RaiseSQLiteSequenceFloor with a free lock: %v", err)
	}
	if err := within(t, "SetSequenceFloor with a free lock", func() error {
		return store.SetSequenceFloor(6000)
	}); err != nil {
		t.Fatalf("SetSequenceFloor with a free lock: %v", err)
	}
	if got, err := readSQLiteSequenceFloor(store.sequenceFloorPath); err != nil || got < 6000 {
		t.Fatalf("floor = (%d, %v) after the free-lock calls, want at least 6000", got, err)
	}
}

// TestSQLiteConcurrentCreatorsNeverShareOrReorderIDs is AT-S2.5: many creators
// on one handle, across several block boundaries and the top-ups that abandon
// block tails, still see unique ids that increase within each creator's own
// sequence. Run it with -race.
func TestSQLiteConcurrentCreatorsNeverShareOrReorderIDs(t *testing.T) {
	store := openSeqStore(t, t.TempDir())
	const (
		creators   = 4
		perCreator = 2*sqliteSequenceBlockSize/creators + 100
	)
	results := make([][]int64, creators)
	errs := make([]error, creators)
	var wg sync.WaitGroup
	for i := range creators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perCreator {
				created, err := store.Create(Bead{Title: "concurrent"})
				if err != nil {
					errs[i] = err
					return
				}
				n, ok := parseSQLiteAutoIDSuffix(sqliteGraphPrefix, created.ID)
				if !ok {
					errs[i] = errors.New("minted id " + created.ID + " is not a strict auto id")
					return
				}
				results[i] = append(results[i], n)
			}
		}()
	}
	wg.Wait()
	seen := map[int64]int{}
	for i, ids := range results {
		if errs[i] != nil {
			t.Fatalf("creator %d: %v", i, errs[i])
		}
		for j, n := range ids {
			if j > 0 && n <= ids[j-1] {
				t.Fatalf("creator %d: id %d after %d, want strictly increasing", i, n, ids[j-1])
			}
			if prev, dup := seen[n]; dup {
				t.Fatalf("creators %d and %d both minted %d", prev, i, n)
			}
			seen[n] = i
		}
	}
	if want := int(creators * perCreator); len(seen) != want {
		t.Fatalf("distinct ids = %d, want %d", len(seen), want)
	}
}

// TestSQLiteApplyGraphPlanPass1GivesUpOnAHeldLock is AT-S2.6: graph apply mints
// every node id before its transaction and takes the bounded wait there. Under
// a held lock it ends in ErrStoreTimeout with nothing written; a caller that
// cancels while it waits gets its own context error and no sentinel.
func TestSQLiteApplyGraphPlanPass1GivesUpOnAHeldLock(t *testing.T) {
	plan := &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "a", Title: "a"}, {Key: "b", Title: "b"}}}

	t.Run("timeout", func(t *testing.T) {
		dir := t.TempDir()
		store := openSeqStore(t, dir)
		mustMint(t, store)
		setBlockHeadroom(t, store, 0)
		before := countBeads(t, store)
		setSequenceLockWait(t, 50*time.Millisecond)
		holdForeignSequenceLock(t, dir)
		err := within(t, "ApplyGraphPlan Pass 1 under a held lock", func() error {
			_, err := store.ApplyGraphPlan(context.Background(), plan)
			return err
		})
		requireStoreTimeout(t, err)
		if got := countBeads(t, store); got != before {
			t.Fatalf("beads = %d after a timed-out graph apply, want %d", got, before)
		}
	})

	t.Run("caller cancels while waiting", func(t *testing.T) {
		dir := t.TempDir()
		store := openSeqStore(t, dir)
		mustMint(t, store)
		setBlockHeadroom(t, store, 0)
		before := countBeads(t, store)
		setSequenceLockWait(t, time.Minute) // only the caller's cancel can end the wait
		probe := installFlockProbe(t, true)
		t.Cleanup(probe.release)
		holdForeignSequenceLock(t, dir)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		applied := make(chan error, 1)
		go func() {
			_, err := store.ApplyGraphPlan(ctx, plan)
			applied <- err
		}()
		select {
		case <-probe.busy:
		case <-time.After(sequenceFloorPersistDeadline):
			t.Fatal("graph apply never reached a contended lock attempt")
		}
		cancel()
		probe.release()
		var err error
		select {
		case err = <-applied:
		case <-time.After(sequenceFloorPersistDeadline):
			t.Fatal("graph apply did not return after the caller canceled")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the caller's context.Canceled", err)
		}
		if errors.Is(err, ErrStoreTimeout) {
			t.Fatalf("err = %v wraps ErrStoreTimeout for a caller's own cancel", err)
		}
		if got := countBeads(t, store); got != before {
			t.Fatalf("beads = %d after a canceled graph apply, want %d", got, before)
		}
	})
}

// Texts captured at the base commit, before the top-up existed. Every
// condition that stops a mint keeps reporting itself exactly as it always did.
const (
	wrappedMintErrText   = `sqlite bead id sequence holds a negative auto id above its floor: auto id gcg--9223372036854761185 ranks above the persisted floor 98636506; if a copy or import pinned gcg--<n> ids into a store that never wrapped and those rows are stray duplicates, delete every one above the floor, not only this one, and reopen the store; otherwise (they are real beads, or an older build wrapped this store and may have issued and deleted ids above them) set a floor at or above the highest gcg id ever issued with "gc storage repair-sequence" (irreversible: allocation never returns to the positive range)`
	exhaustedMintErrText = `reserving sqlite bead ids: sqlite bead id sequence exhausted at 9223372036854775807: refusing to wrap and re-issue ids; raise the floor into a never-issued range with "gc storage repair-sequence"`
	readOnlyMintErrText  = `minting sqlite bead id on read-only store`
)

// TestSQLiteAutoIDCreateKeepsItsErrorsWhenMintingIsRefused is AT-S2.7. The
// top-up declines every refusal and leaves it to the in-transaction mint, so a
// wrapped store, an exhausted allocator and a read-only store each report the
// error they reported before, and a pinned-id create takes no lock at all.
func TestSQLiteAutoIDCreateKeepsItsErrorsWhenMintingIsRefused(t *testing.T) {
	t.Run("wrapped", func(t *testing.T) {
		store := openSeqStore(t, wrappedStore(t))
		_, err := store.Create(Bead{Title: "would reuse"})
		if !errors.Is(err, ErrSQLiteSequenceWrapped) || errors.Is(err, ErrStoreTimeout) {
			t.Fatalf("err = %v, want ErrSQLiteSequenceWrapped and no sentinel", err)
		}
		if err.Error() != wrappedMintErrText {
			t.Fatalf("err = %q\nwant %q", err.Error(), wrappedMintErrText)
		}
	})

	t.Run("exhausted", func(t *testing.T) {
		store := openSeqStore(t, t.TempDir())
		if err := store.SetSequenceFloor(math.MaxInt64 - 1); err != nil {
			t.Fatalf("SetSequenceFloor: %v", err)
		}
		if got := mustMint(t, store); got != "gcg-9223372036854775807" {
			t.Fatalf("last mint = %q", got)
		}
		_, err := store.Create(Bead{Title: "one too many"})
		if !errors.Is(err, ErrSQLiteSequenceExhausted) || errors.Is(err, ErrStoreTimeout) {
			t.Fatalf("err = %v, want ErrSQLiteSequenceExhausted and no sentinel", err)
		}
		if err.Error() != exhaustedMintErrText {
			t.Fatalf("err = %q\nwant %q", err.Error(), exhaustedMintErrText)
		}
	})

	t.Run("read-only", func(t *testing.T) {
		dir := t.TempDir()
		seed := openSeqStore(t, dir)
		mustPin(t, seed, "gcg-5")
		if err := seed.CloseStore(); err != nil {
			t.Fatal(err)
		}
		opened, err := OpenSQLiteStore(dir, WithSQLiteStoreReadOnly(), WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
		if err != nil {
			t.Fatal(err)
		}
		ro := opened.(*SQLiteStore)
		t.Cleanup(func() { _ = ro.CloseStore() })
		_, err = ro.Create(Bead{Title: "read-only"})
		if err == nil || errors.Is(err, ErrStoreTimeout) {
			t.Fatalf("err = %v, want the read-only refusal and no sentinel", err)
		}
		if err.Error() != readOnlyMintErrText {
			t.Fatalf("err = %q\nwant %q", err.Error(), readOnlyMintErrText)
		}
	})

	t.Run("pinned id under a foreign lock", func(t *testing.T) {
		dir := t.TempDir()
		store := openSeqStore(t, dir)
		mustMint(t, store)
		setBlockHeadroom(t, store, 0)
		probe := installFlockProbe(t, false)
		holdForeignSequenceLock(t, dir)
		if err := within(t, "pinned-id create", func() error {
			_, err := store.CreateWithForeignID(Bead{ID: "gcg-session-feedface", Title: "pinned"})
			return err
		}); err != nil {
			t.Fatalf("pinned-id create under a foreign lock: %v", err)
		}
		if got := probe.attempts.Load(); got != 0 {
			t.Fatalf("flock attempts = %d for a pinned-id create, want 0", got)
		}
	})
}

// TestSQLiteNonMintingTxIgnoresTheDirectoryLock is AT-S2.8: Tx takes no top-up,
// so a transaction that mints nothing completes and commits under a foreign
// lock even at a low block, with no lock attempt.
func TestSQLiteNonMintingTxIgnoresTheDirectoryLock(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	seed := mustMint(t, store)
	setBlockHeadroom(t, store, 10)
	probe := installFlockProbe(t, false)
	holdForeignSequenceLock(t, dir)

	description := "committed by a non-minting tx"
	if err := within(t, "non-minting Tx", func() error {
		return store.Tx("", func(tx Tx) error {
			return tx.Update(seed, UpdateOpts{Description: &description})
		})
	}); err != nil {
		t.Fatalf("non-minting Tx under a foreign lock: %v", err)
	}
	got, err := store.Get(seed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != description {
		t.Fatalf("description = %q, want the Tx's write committed", got.Description)
	}
	if n := probe.attempts.Load(); n != 0 {
		t.Fatalf("flock attempts = %d for a non-minting Tx, want 0", n)
	}
}

// TestSQLiteQueuedCreatorsShareOneWaitBudget is AT-S2.9: the wait deadline is
// fixed on entry, so creators queued behind a top-up that cannot get the lock do
// not each wait a full budget in turn. Asserted by lock-attempt counts, not
// elapsed time: one budget of polling plus a try or two per queued creator,
// where a budget per creator would make about creators times as many.
func TestSQLiteQueuedCreatorsShareOneWaitBudget(t *testing.T) {
	dir := t.TempDir()
	store := openSeqStore(t, dir)
	mustMint(t, store)
	setBlockHeadroom(t, store, 10)
	const (
		creators = 6
		budget   = 200 * time.Millisecond
	)
	setSequenceLockWait(t, budget)
	probe := installFlockProbe(t, false)
	holdForeignSequenceLock(t, dir)

	errs := make([]error, creators)
	var wg sync.WaitGroup
	for i := range creators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = store.Create(Bead{Title: "queued"})
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(sequenceFloorPersistDeadline):
		t.Fatal("queued creators did not all give up")
	}
	for i, err := range errs {
		if !errors.Is(err, ErrStoreTimeout) {
			t.Fatalf("creator %d: err = %v, want ErrStoreTimeout", i, err)
		}
	}
	polls := int64(budget / flockPollInterval)
	got := probe.attempts.Load()
	if got < creators {
		t.Fatalf("flock attempts = %d, want at least one per creator (%d)", got, creators)
	}
	if limit := polls + 3*creators; got > limit {
		t.Fatalf("flock attempts = %d, want at most %d (one %s budget of polling plus a few tries per creator); "+
			"a budget per creator would be about %d", got, limit, budget, creators*polls)
	}
}
