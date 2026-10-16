package main

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// openShimTestBeadsStore opens a SQLite bead engine below a test temp dir and
// closes it when the test ends.
func openShimTestBeadsStore(t *testing.T) beads.Store {
	t.Helper()
	opened, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	sqlite := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = sqlite.CloseStore() })
	return sqlite
}

type shimFixture struct {
	city  string
	file  *fileNudgeQueue
	beads nudgeQueueShimBeads
	store beads.Store
}

func newShimFixture(t *testing.T) shimFixture {
	t.Helper()
	city := t.TempDir()
	file, ok := openNudgeQueue(city, beads.NudgesStore{}).Queue.(*fileNudgeQueue)
	if !ok {
		t.Fatal("openNudgeQueue did not build the file queue")
	}
	store := openShimTestBeadsStore(t)
	queue, err := storebinding.NewBeadsNudgeQueue(beads.NudgesStore{Store: store})
	if err != nil {
		t.Fatalf("NewBeadsNudgeQueue: %v", err)
	}
	shimBeads, ok := queue.(nudgeQueueShimBeads)
	if !ok {
		t.Fatalf("the beads queue %T does not serve the shim's live read", queue)
	}
	return shimFixture{city: city, file: file, beads: shimBeads, store: store}
}

// writeFileState replaces the file queue's state, as a process still writing
// the file queue would leave it.
func (f shimFixture) writeFileState(t *testing.T, mutate func(*nudgequeue.State)) {
	t.Helper()
	if err := nudgequeue.WithState(f.city, func(state *nudgequeue.State) error {
		mutate(state)
		return nil
	}); err != nil {
		t.Fatalf("writing the file queue: %v", err)
	}
}

func (f shimFixture) fileState(t *testing.T) nudgequeue.State {
	t.Helper()
	state, err := nudgequeue.LoadState(f.city)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return state
}

func (f shimFixture) beadsState(t *testing.T) nudgequeue.State {
	t.Helper()
	state, err := f.beads.Snapshot()
	if err != nil {
		t.Fatalf("beads Snapshot: %v", err)
	}
	return state
}

func (f shimFixture) shim(t *testing.T, direction nudgeQueueShimDirection, now time.Time) nudgeQueueShimReport {
	t.Helper()
	report, err := shimNudgeQueue(context.Background(), direction, f.file, f.beads, now)
	if err != nil {
		t.Fatalf("shimNudgeQueue(%s): %v", direction, err)
	}
	return report
}

// shimItem is a fully populated item: every field a move must keep is set.
func shimItem(id string, now time.Time) nudgequeue.Item {
	return nudgequeue.Item{
		ID:                id,
		BeadID:            "gc-shadow-" + id,
		Agent:             "alpha/worker",
		SessionID:         "gc-session-1",
		ContinuationEpoch: "3",
		Source:            "sling",
		Message:           "check your hook: " + id,
		Reference:         &nudgequeue.Reference{Kind: "bead", ID: "gc-ref-" + id},
		CreatedAt:         now.Add(-time.Minute),
		DeliverAfter:      now.Add(-30 * time.Second),
		ExpiresAt:         now.Add(time.Hour),
		Attempts:          2,
		LastAttemptAt:     now.Add(-10 * time.Second),
		LastError:         "runtime busy",
	}
}

// shimTarget is the claim target shimItem's items are fenced to.
func shimTarget(agent string) storebinding.ClaimTarget {
	return storebinding.ClaimTarget{QueueKeys: []string{agent}, SessionID: "gc-session-1", ContinuationEpoch: "3"}
}

func shimIDs(items []nudgequeue.Item) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	return ids
}

func assertShimReport(t *testing.T, got nudgeQueueShimReport, want nudgeQueueShimReport) {
	t.Helper()
	got.Direction, want.Direction = 0, 0
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shim report = %+v, want %+v", got, want)
	}
}

func TestNudgeQueueShimRoundTripPreservesEveryItemField(t *testing.T) {
	f := newShimFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	pending := shimItem("n-pending", now)
	pinned := shimItem("n-pinned", now)
	pinned.DeliveryBatch = "nudge-batch-7"
	// An in-flight item whose lease ran out moves with its lease cleared.
	stale := shimItem("n-stale-lease", now)
	stale.ClaimedAt = now.Add(-5 * time.Minute)
	stale.LeaseUntil = now.Add(-3 * time.Minute)
	f.writeFileState(t, func(state *nudgequeue.State) {
		state.Pending = []nudgequeue.Item{pending, pinned}
		state.InFlight = []nudgequeue.Item{stale}
	})
	// A moved item keeps every field but its claim and its delivery batch:
	// the destination pins it under a key of its own.
	unpinned := pinned
	unpinned.DeliveryBatch = ""
	want := map[string]nudgequeue.Item{
		pending.ID: pending,
		pinned.ID:  unpinned,
		stale.ID:   nudgeItemForMove(stale),
	}
	assertItems := func(stage string, got []nudgequeue.Item) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: items %v, want %d", stage, shimIDs(got), len(want))
		}
		for _, item := range got {
			if !reflect.DeepEqual(item, want[item.ID]) {
				t.Errorf("%s: %s = %+v\nwant %+v", stage, item.ID, item, want[item.ID])
			}
		}
	}

	assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now), nudgeQueueShimReport{Moved: 3})
	if state := f.fileState(t); len(state.Pending)+len(state.InFlight)+len(state.Dead) != 0 {
		t.Fatalf("file queue after the move = %+v, want empty", state)
	}
	inBeads := f.beadsState(t)
	if len(inBeads.InFlight)+len(inBeads.Dead) != 0 {
		t.Fatalf("beads queue after the move = %+v, want every item pending", inBeads)
	}
	assertItems("in beads", inBeads.Pending)

	assertShimReport(t, f.shim(t, nudgeQueueShimToFile, now), nudgeQueueShimReport{Moved: 3})
	if state := f.beadsState(t); len(state.Pending)+len(state.InFlight)+len(state.Dead) != 0 {
		t.Fatalf("beads queue after the move back = %+v, want every record retired", state)
	}
	back := f.fileState(t)
	if len(back.InFlight)+len(back.Dead) != 0 {
		t.Fatalf("file queue after the move back = %+v, want every item pending", back)
	}
	assertItems("back in the file", back.Pending)
}

func TestNudgeQueueShimRetiresImportedRecordsAsMigrated(t *testing.T) {
	store := openShimTestBeadsStore(t)
	fronts, err := storebinding.NewBeadsNudgeFrontDoors(beads.NudgesStore{Store: store})
	if err != nil {
		t.Fatalf("NewBeadsNudgeFrontDoors: %v", err)
	}
	city := t.TempDir()
	file := openNudgeQueue(city, beads.NudgesStore{}).Queue.(*fileNudgeQueue)
	now := time.Now().UTC()
	if err := fronts.Queue.Enqueue(shimItem("n-migrate", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := shimNudgeQueue(context.Background(), nudgeQueueShimToFile, file, fronts.Queue.(nudgeQueueShimBeads), now); err != nil {
		t.Fatalf("shim: %v", err)
	}
	shadow, ok, err := fronts.Shadows.FindIncludingTerminal("n-migrate")
	if err != nil || !ok {
		t.Fatalf("FindIncludingTerminal = %v, %v", ok, err)
	}
	if shadow.State != "migrated" || shadow.TerminalReason != "imported into the file queue" || shadow.CommitBoundary != "queue-mode-switch" {
		t.Fatalf("migrated record projected %+v", shadow)
	}
}

func TestNudgeQueueShimLeaseHeldItemWaits(t *testing.T) {
	t.Run("file-to-beads", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		held := shimItem("n-held", now)
		held.ClaimedAt = now
		held.LeaseUntil = now.Add(2 * time.Minute)
		f.writeFileState(t, func(state *nudgequeue.State) { state.InFlight = []nudgequeue.Item{held} })

		assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now), nudgeQueueShimReport{LeaseHeld: 1})
		if state := f.beadsState(t); len(state.Pending) != 0 {
			t.Fatalf("beads queue = %v, want the leased item left in the file", shimIDs(state.Pending))
		}
		if state := f.fileState(t); len(state.InFlight) != 1 {
			t.Fatalf("file in flight = %v, want the leased item untouched", shimIDs(state.InFlight))
		}
		// Once the lease runs out, the next pass moves it.
		assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now.Add(3*time.Minute)), nudgeQueueShimReport{Moved: 1})
		if got := shimIDs(f.beadsState(t).Pending); !reflect.DeepEqual(got, []string{"n-held"}) {
			t.Fatalf("beads pending = %v, want the item moved after its lease", got)
		}
	})
	t.Run("beads-to-file", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		if err := f.beads.EnqueueDeferred(shimItem("n-held", now)); err != nil {
			t.Fatalf("EnqueueDeferred: %v", err)
		}
		if claimed, err := f.beads.ClaimDue(shimTarget("alpha/worker"), now); err != nil || len(claimed) != 1 {
			t.Fatalf("ClaimDue = %v, %v", shimIDs(claimed), err)
		}
		assertShimReport(t, f.shim(t, nudgeQueueShimToFile, now), nudgeQueueShimReport{LeaseHeld: 1})
		if state := f.fileState(t); len(state.Pending) != 0 {
			t.Fatalf("file pending = %v, want the leased record left in beads", shimIDs(state.Pending))
		}
		if state := f.beadsState(t); len(state.InFlight) != 1 {
			t.Fatalf("beads in flight = %v, want the claim untouched", shimIDs(state.InFlight))
		}
	})
}

func TestNudgeQueueShimBatchWithOneLeasedMemberDoesNotMove(t *testing.T) {
	t.Run("file-to-beads", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		free := shimItem("n-free", now)
		free.DeliveryBatch = "nudge-batch-k"
		held := shimItem("n-held", now)
		held.DeliveryBatch = "nudge-batch-k"
		held.ClaimedAt = now
		held.LeaseUntil = now.Add(time.Minute)
		loose := shimItem("n-loose", now)
		f.writeFileState(t, func(state *nudgequeue.State) {
			state.Pending = []nudgequeue.Item{free, loose}
			state.InFlight = []nudgequeue.Item{held}
		})
		assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now), nudgeQueueShimReport{Moved: 1, LeaseHeld: 1})
		if got := shimIDs(f.beadsState(t).Pending); !reflect.DeepEqual(got, []string{"n-loose"}) {
			t.Fatalf("beads pending = %v, want only the unpinned item; batch k stays whole in the file", got)
		}
		state := f.fileState(t)
		if got := shimIDs(state.Pending); !reflect.DeepEqual(got, []string{"n-free"}) {
			t.Fatalf("file pending = %v, want batch k's free member kept", got)
		}
		if got := shimIDs(state.InFlight); !reflect.DeepEqual(got, []string{"n-held"}) {
			t.Fatalf("file in flight = %v, want batch k's leased member kept", got)
		}
	})
	t.Run("beads-to-file", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		free := shimItem("n-free", now)
		free.DeliveryBatch = "nudge-batch-k"
		held := shimItem("n-held", now)
		held.DeliveryBatch = "nudge-batch-k"
		held.Agent = "beta/worker"
		for _, item := range []nudgequeue.Item{free, held} {
			if err := f.beads.EnqueueDeferred(item); err != nil {
				t.Fatalf("EnqueueDeferred(%s): %v", item.ID, err)
			}
		}
		if claimed, err := f.beads.ClaimDue(shimTarget("beta/worker"), now); err != nil || len(claimed) != 1 {
			t.Fatalf("ClaimDue = %v, %v", shimIDs(claimed), err)
		}
		assertShimReport(t, f.shim(t, nudgeQueueShimToFile, now), nudgeQueueShimReport{LeaseHeld: 1})
		if state := f.fileState(t); len(state.Pending) != 0 {
			t.Fatalf("file pending = %v, want batch k left whole in beads", shimIDs(state.Pending))
		}
		state := f.beadsState(t)
		if got := shimIDs(state.Pending); !reflect.DeepEqual(got, []string{"n-free"}) {
			t.Fatalf("beads pending = %v, want batch k's free member kept", got)
		}
	})
}

// failingShimFile fails one file-side step once, as a crash between the two
// steps of a move would leave it.
type failingShimFile struct {
	nudgeQueueShimFile
	failRemove bool
	failImport bool
}

var errShimCrash = errors.New("simulated crash")

func (f *failingShimFile) removeMigrated(items []nudgequeue.Item, now time.Time) ([]nudgequeue.Item, error) {
	if f.failRemove {
		f.failRemove = false
		return nil, errShimCrash
	}
	return f.nudgeQueueShimFile.removeMigrated(items, now)
}

func (f *failingShimFile) importMigrated(items []nudgequeue.Item) ([]string, error) {
	if f.failImport {
		f.failImport = false
		if _, err := f.nudgeQueueShimFile.importMigrated(items); err != nil {
			return nil, err
		}
		return nil, errShimCrash
	}
	return f.nudgeQueueShimFile.importMigrated(items)
}

// shimBeadsQueue intercepts the beads queue's Ack.
type shimBeadsQueue struct {
	nudgeQueueShimBeads
	beforeAck func()
}

func (q *shimBeadsQueue) Ack(ids []string, outcome, reason, commitBoundary string) error {
	if q.beforeAck != nil {
		hook := q.beforeAck
		q.beforeAck = nil
		hook()
	}
	return q.nudgeQueueShimBeads.Ack(ids, outcome, reason, commitBoundary)
}

func TestNudgeQueueShimRerunAfterACrashBetweenStepsIsANoOp(t *testing.T) {
	t.Run("file-to-beads", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		first := shimItem("n-crash-a", now)
		first.DeliveryBatch = "nudge-batch-k"
		second := shimItem("n-crash-b", now)
		second.DeliveryBatch = "nudge-batch-k"
		f.writeFileState(t, func(state *nudgequeue.State) { state.Pending = []nudgequeue.Item{first, second} })
		crashing := &failingShimFile{nudgeQueueShimFile: f.file, failRemove: true}

		report, err := shimNudgeQueue(context.Background(), nudgeQueueShimToBeads, crashing, f.beads, now)
		if !errors.Is(err, errShimCrash) {
			t.Fatalf("crashed pass error = %v, want the crash", err)
		}
		assertShimReport(t, report, nudgeQueueShimReport{Failed: 1})
		if got := shimIDs(f.fileState(t).Pending); !reflect.DeepEqual(got, []string{"n-crash-a", "n-crash-b"}) {
			t.Fatalf("file pending after the crash = %v, want the batch still there whole", got)
		}

		report, err = shimNudgeQueue(context.Background(), nudgeQueueShimToBeads, crashing, f.beads, now)
		if err != nil {
			t.Fatalf("re-run: %v", err)
		}
		assertShimReport(t, report, nudgeQueueShimReport{Moved: 1})
		if got := shimIDs(f.beadsState(t).Pending); !reflect.DeepEqual(got, []string{"n-crash-a", "n-crash-b"}) {
			t.Fatalf("beads pending after the re-run = %v, want exactly one record per member", got)
		}
		if state := f.fileState(t); len(state.Pending) != 0 {
			t.Fatalf("file pending after the re-run = %v, want empty", shimIDs(state.Pending))
		}
	})
	t.Run("beads-to-file", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		if err := f.beads.EnqueueDeferred(shimItem("n-crash", now)); err != nil {
			t.Fatalf("EnqueueDeferred: %v", err)
		}
		crashing := &failingShimFile{nudgeQueueShimFile: f.file, failImport: true}

		report, err := shimNudgeQueue(context.Background(), nudgeQueueShimToFile, crashing, f.beads, now)
		if !errors.Is(err, errShimCrash) {
			t.Fatalf("crashed pass error = %v, want the crash", err)
		}
		assertShimReport(t, report, nudgeQueueShimReport{Failed: 1})
		if got := shimIDs(f.beadsState(t).Pending); !reflect.DeepEqual(got, []string{"n-crash"}) {
			t.Fatalf("beads pending after the crash = %v, want the record still live", got)
		}

		report, err = shimNudgeQueue(context.Background(), nudgeQueueShimToFile, crashing, f.beads, now)
		if err != nil {
			t.Fatalf("re-run: %v", err)
		}
		assertShimReport(t, report, nudgeQueueShimReport{Moved: 1})
		if got := shimIDs(f.fileState(t).Pending); !reflect.DeepEqual(got, []string{"n-crash"}) {
			t.Fatalf("file pending after the re-run = %v, want exactly one copy", got)
		}
		if state := f.beadsState(t); len(state.Pending) != 0 {
			t.Fatalf("beads pending after the re-run = %v, want the record retired", shimIDs(state.Pending))
		}
	})
}

func TestNudgeQueueShimMovesAnOldBinaryWriteOnTheNextPass(t *testing.T) {
	t.Run("file-to-beads", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now), nudgeQueueShimReport{})
		if err := f.file.EnqueueDeferred(shimItem("n-late", now)); err != nil {
			t.Fatalf("file EnqueueDeferred: %v", err)
		}
		assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now), nudgeQueueShimReport{Moved: 1})
		if got := shimIDs(f.beadsState(t).Pending); !reflect.DeepEqual(got, []string{"n-late"}) {
			t.Fatalf("beads pending = %v, want the late write moved", got)
		}
	})
	t.Run("beads-to-file", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		assertShimReport(t, f.shim(t, nudgeQueueShimToFile, now), nudgeQueueShimReport{})
		if err := f.beads.Enqueue(shimItem("n-late", now)); err != nil {
			t.Fatalf("beads Enqueue: %v", err)
		}
		assertShimReport(t, f.shim(t, nudgeQueueShimToFile, now), nudgeQueueShimReport{Moved: 1})
		if got := shimIDs(f.fileState(t).Pending); !reflect.DeepEqual(got, []string{"n-late"}) {
			t.Fatalf("file pending = %v, want the late write moved", got)
		}
	})
}

// A quick flip back: the item went file → beads → file, so the beads queue
// remembers its id as retired. Moving it to beads again would be a no-op
// there, so it stays in the file queue and is reported, never dropped.
func TestNudgeQueueShimFlipBackInsideTheRetiredWindowKeepsTheItem(t *testing.T) {
	f := newShimFixture(t)
	now := time.Now().UTC()
	item := shimItem("n-flip", now)
	f.writeFileState(t, func(state *nudgequeue.State) { state.Pending = []nudgequeue.Item{item} })
	assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now), nudgeQueueShimReport{Moved: 1})
	assertShimReport(t, f.shim(t, nudgeQueueShimToFile, now), nudgeQueueShimReport{Moved: 1})

	for pass := 0; pass < 2; pass++ {
		assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now), nudgeQueueShimReport{Retired: 1, RetiredIDs: []string{"n-flip"}})
		if got := shimIDs(f.fileState(t).Pending); !reflect.DeepEqual(got, []string{"n-flip"}) {
			t.Fatalf("pass %d: file pending = %v, want the item kept", pass, got)
		}
		if state := f.beadsState(t); len(state.Pending)+len(state.InFlight) != 0 {
			t.Fatalf("pass %d: beads live = %+v, want no resurrected record", pass, state)
		}
	}
}

func TestNudgeQueueShimNeverMovesDeadLetters(t *testing.T) {
	t.Run("file-to-beads", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		dead := shimItem("n-dead", now)
		dead.DeadAt = now.Add(-time.Minute)
		expired := shimItem("n-expired", now)
		expired.ExpiresAt = now.Add(-time.Second)
		f.writeFileState(t, func(state *nudgequeue.State) {
			state.Dead = []nudgequeue.Item{dead}
			state.Pending = []nudgequeue.Item{expired}
		})
		assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now), nudgeQueueShimReport{})
		if state := f.beadsState(t); len(state.Pending)+len(state.InFlight)+len(state.Dead) != 0 {
			t.Fatalf("beads queue = %+v, want no dead letter moved", state)
		}
		state := f.fileState(t)
		if len(state.Dead) != 1 || len(state.Pending) != 1 {
			t.Fatalf("file queue = %+v, want the dead and the expired item left where they are", state)
		}
	})
	t.Run("beads-to-file", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		if err := f.beads.EnqueueDeferred(shimItem("n-dead", now)); err != nil {
			t.Fatalf("EnqueueDeferred: %v", err)
		}
		if dead, err := f.beads.RecordFailure([]string{"n-dead"}, storebinding.ErrNudgeSessionFenceMismatch, now); err != nil || len(dead) != 1 {
			t.Fatalf("RecordFailure = %v, %v", shimIDs(dead), err)
		}
		expired := shimItem("n-expired", now)
		expired.ExpiresAt = now.Add(-time.Second)
		if err := f.beads.EnqueueDeferred(expired); err != nil {
			t.Fatalf("EnqueueDeferred: %v", err)
		}
		assertShimReport(t, f.shim(t, nudgeQueueShimToFile, now), nudgeQueueShimReport{})
		if state := f.fileState(t); len(state.Pending)+len(state.InFlight)+len(state.Dead) != 0 {
			t.Fatalf("file queue = %+v, want no dead letter moved", state)
		}
		if got := shimIDs(f.beadsState(t).Dead); !reflect.DeepEqual(got, []string{"n-dead", "n-expired"}) {
			t.Fatalf("beads dead = %v, want both dead letters left in beads", got)
		}
	})
}

// racingShimFile lets a process still writing the file queue claim between
// the shim's read and its removal.
type racingShimFile struct {
	nudgeQueueShimFile
	race func()
}

func (f *racingShimFile) removeMigrated(items []nudgequeue.Item, now time.Time) ([]nudgequeue.Item, error) {
	if f.race != nil {
		race := f.race
		f.race = nil
		race()
	}
	return f.nudgeQueueShimFile.removeMigrated(items, now)
}

// A claim racing the shim's read leaves at most a duplicate: the item is
// deliverable from both queues, and is lost from neither.
func TestNudgeQueueShimClaimRacingTheReadLeavesAtMostADuplicate(t *testing.T) {
	target := shimTarget("alpha/worker")
	t.Run("file-to-beads", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		f.writeFileState(t, func(state *nudgequeue.State) { state.Pending = []nudgequeue.Item{shimItem("n-race", now)} })
		var staleClaim []nudgequeue.Item
		racing := &racingShimFile{nudgeQueueShimFile: f.file, race: func() {
			var err error
			if staleClaim, err = f.file.ClaimDue(target, time.Now().UTC()); err != nil {
				t.Fatalf("stale-mode ClaimDue: %v", err)
			}
		}}
		report, err := shimNudgeQueue(context.Background(), nudgeQueueShimToBeads, racing, f.beads, now)
		if err != nil {
			t.Fatalf("shim: %v", err)
		}
		assertShimReport(t, report, nudgeQueueShimReport{Raced: 1})
		if got := shimIDs(staleClaim); !reflect.DeepEqual(got, []string{"n-race"}) {
			t.Fatalf("stale-mode claim = %v, want it to hold the item", got)
		}
		if got := shimIDs(f.fileState(t).InFlight); !reflect.DeepEqual(got, []string{"n-race"}) {
			t.Fatalf("file in flight = %v, want the claimed item kept for its claimant", got)
		}
		if got := shimIDs(f.beadsState(t).Pending); !reflect.DeepEqual(got, []string{"n-race"}) {
			t.Fatalf("beads pending = %v, want the moved copy deliverable: a duplicate, not a loss", got)
		}
	})
	t.Run("beads-to-file", func(t *testing.T) {
		f := newShimFixture(t)
		now := time.Now().UTC()
		if err := f.beads.EnqueueDeferred(shimItem("n-race", now)); err != nil {
			t.Fatalf("EnqueueDeferred: %v", err)
		}
		var staleClaim []nudgequeue.Item
		racing := &shimBeadsQueue{nudgeQueueShimBeads: f.beads}
		racing.beforeAck = func() {
			var err error
			if staleClaim, err = f.beads.ClaimDue(target, time.Now().UTC()); err != nil {
				t.Fatalf("stale-mode ClaimDue: %v", err)
			}
		}
		report, err := shimNudgeQueue(context.Background(), nudgeQueueShimToFile, f.file, racing, now)
		if err != nil {
			t.Fatalf("shim: %v", err)
		}
		assertShimReport(t, report, nudgeQueueShimReport{Moved: 1})
		if got := shimIDs(staleClaim); !reflect.DeepEqual(got, []string{"n-race"}) {
			t.Fatalf("stale-mode claim = %v, want it to hold the record", got)
		}
		if got := shimIDs(f.fileState(t).Pending); !reflect.DeepEqual(got, []string{"n-race"}) {
			t.Fatalf("file pending = %v, want the imported copy deliverable: a duplicate, not a loss", got)
		}
		// The claimant's ack after its delivery is a no-op on the retired
		// record, so the claim is not lost either.
		if err := f.beads.Ack([]string{"n-race"}, "injected", "", ""); err != nil {
			t.Fatalf("claimant Ack: %v", err)
		}
	})
}

func TestNudgeQueueShimRejectsAnUnknownDirectionAndMissingQueues(t *testing.T) {
	f := newShimFixture(t)
	now := time.Now().UTC()
	if _, err := shimNudgeQueue(context.Background(), nudgeQueueShimDirection(0), f.file, f.beads, now); err == nil {
		t.Error("shimNudgeQueue accepted the zero direction")
	}
	if _, err := shimNudgeQueue(context.Background(), nudgeQueueShimToBeads, nil, f.beads, now); err == nil {
		t.Error("shimNudgeQueue accepted a missing file queue")
	}
	if _, err := shimNudgeQueue(context.Background(), nudgeQueueShimToFile, f.file, nil, now); err == nil {
		t.Error("shimNudgeQueue accepted a missing beads queue")
	}
}

func TestNudgeQueueShimStopsBetweenBatchesWhenCanceled(t *testing.T) {
	f := newShimFixture(t)
	now := time.Now().UTC()
	f.writeFileState(t, func(state *nudgequeue.State) {
		state.Pending = []nudgequeue.Item{shimItem("n-a", now), shimItem("n-b", now)}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := shimNudgeQueue(ctx, nudgeQueueShimToBeads, f.file, f.beads, now)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pass error = %v, want context.Canceled", err)
	}
	assertShimReport(t, report, nudgeQueueShimReport{})
	if got := shimIDs(f.fileState(t).Pending); !reflect.DeepEqual(got, []string{"n-a", "n-b"}) {
		t.Fatalf("file pending = %v, want nothing moved", got)
	}
}

// shimPinnedBatchKey is the delivery key pinnedShimBatch's members carry.
const shimPinnedBatchKey = "nudge-batch-k"

// pinnedShimBatch is two members of one delivery batch that a file-mode
// delivery already claimed under shimPinnedBatchKey and released with the pin
// kept.
func pinnedShimBatch(now time.Time) (nudgequeue.Item, nudgequeue.Item) {
	first := shimItem("n-a", now)
	first.DeliveryBatch = shimPinnedBatchKey
	second := shimItem("n-b", now)
	second.DeliveryBatch = shimPinnedBatchKey
	return first, second
}

// assertNoClaimUnderKey fails when the beads queue hands out any item under
// the file batch's key: the moved copy is unpinned, so the beads queue pins it
// under a key of its own, and a part of the file batch can never be sent under
// the file batch's key.
func assertNoClaimUnderKey(t *testing.T, stage string, claimed []nudgequeue.Item, key string) {
	t.Helper()
	for _, item := range claimed {
		if item.DeliveryBatch == key {
			t.Fatalf("%s: beads ClaimBatch handed out %s under the file batch's key %q", stage, item.ID, key)
		}
	}
}

// failingEnqueueBeads fails EnqueueDeferred for one id, once, as a transient
// store error would.
type failingEnqueueBeads struct {
	nudgeQueueShimBeads
	failID string
}

func (q *failingEnqueueBeads) EnqueueDeferred(item nudgequeue.Item) error {
	if item.ID == q.failID {
		q.failID = ""
		return errShimCrash
	}
	return q.nudgeQueueShimBeads.EnqueueDeferred(item)
}

// A transient failure part way through a pinned batch leaves the members
// already enqueued live in beads. They are unpinned there, so beads sends them
// under a fresh key: a part of the batch never goes out under the batch's own
// key, where a key-deduplicating runtime would later replay that part's
// receipt for the rest of the batch and ack it unseen.
func TestNudgeQueueShimMidBatchEnqueueFailureNeverSendsPartUnderTheBatchKey(t *testing.T) {
	f := newShimFixture(t)
	now := time.Now().UTC()
	const key = shimPinnedBatchKey
	first, second := pinnedShimBatch(now)
	f.writeFileState(t, func(state *nudgequeue.State) { state.Pending = []nudgequeue.Item{first, second} })
	target := shimTarget("alpha/worker")

	failing := &failingEnqueueBeads{nudgeQueueShimBeads: f.beads, failID: "n-b"}
	report, err := shimNudgeQueue(context.Background(), nudgeQueueShimToBeads, f.file, failing, now)
	if !errors.Is(err, errShimCrash) {
		t.Fatalf("pass 1 error = %v, want the enqueue failure", err)
	}
	assertShimReport(t, report, nudgeQueueShimReport{Failed: 1})
	claimed, err := f.beads.ClaimBatch(target, now)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if got := shimIDs(claimed); !reflect.DeepEqual(got, []string{"n-a"}) {
		t.Fatalf("pass 1 beads claim = %v, want the one member that reached beads", got)
	}
	assertNoClaimUnderKey(t, "pass 1", claimed, key)
	if err := f.beads.Ack(shimIDs(claimed), "injected", "", ""); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	// The next pass enqueues the rest; n-a is retired in beads now, so the
	// batch stays whole in the file queue and is reported.
	assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now), nudgeQueueShimReport{Retired: 1, RetiredIDs: []string{"n-a"}})
	claimed2, err := f.beads.ClaimBatch(target, now)
	if err != nil {
		t.Fatalf("ClaimBatch 2: %v", err)
	}
	if got := shimIDs(claimed2); !reflect.DeepEqual(got, []string{"n-b"}) {
		t.Fatalf("pass 2 beads claim = %v, want the rest of the batch", got)
	}
	assertNoClaimUnderKey(t, "pass 2", claimed2, key)
	if claimed2[0].DeliveryBatch == claimed[0].DeliveryBatch {
		t.Fatalf("pass 2 reused pass 1's key %q; a deduplicating runtime would ack n-b unseen", claimed[0].DeliveryBatch)
	}
	if got := shimIDs(f.fileState(t).Pending); !reflect.DeepEqual(got, []string{"n-a", "n-b"}) {
		t.Fatalf("file pending = %v, want the batch kept whole", got)
	}
}

// A pinned batch one member of which the beads queue already retired stays
// whole in the file queue, and the member that did reach beads is served
// there under a fresh key, never under the file batch's key.
func TestNudgeQueueShimRetiredMemberKeepsThePinnedBatchWholeInTheFile(t *testing.T) {
	f := newShimFixture(t)
	now := time.Now().UTC()
	const key = shimPinnedBatchKey
	first, second := pinnedShimBatch(now)
	if err := f.beads.EnqueueDeferred(nudgeItemForMove(first)); err != nil {
		t.Fatalf("EnqueueDeferred: %v", err)
	}
	if err := f.beads.Ack([]string{first.ID}, nudgeQueueMigratedOutcome, nudgeQueueMigratedReason, nudgeQueueMigratedBoundary); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	f.writeFileState(t, func(state *nudgequeue.State) { state.Pending = []nudgequeue.Item{first, second} })

	assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, now), nudgeQueueShimReport{Retired: 1, RetiredIDs: []string{"n-a"}})
	file := f.fileState(t)
	if got := shimIDs(file.Pending); !reflect.DeepEqual(got, []string{"n-a", "n-b"}) {
		t.Fatalf("file pending = %v, want both members kept", got)
	}
	for _, item := range file.Pending {
		if item.DeliveryBatch != key {
			t.Fatalf("file member %s batch = %q, want the pin kept on the file side", item.ID, item.DeliveryBatch)
		}
	}
	claimed, err := f.beads.ClaimBatch(shimTarget("alpha/worker"), now)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if got := shimIDs(claimed); !reflect.DeepEqual(got, []string{"n-b"}) {
		t.Fatalf("beads claim = %v, want the member that reached beads", got)
	}
	assertNoClaimUnderKey(t, "retired batch", claimed, key)
}

// A process still writing the file queue claims only the due member of a
// pinned batch between the shim's read and its removal. The removal takes the
// batch whole or not at all, so the file queue keeps the whole batch under its
// key, and the beads copy is unpinned: at most a duplicate, never a part of
// the batch sent under its key.
func TestNudgeQueueShimPartlyClaimedPinnedBatchStaysWholeInTheFile(t *testing.T) {
	f := newShimFixture(t)
	now := time.Now().UTC()
	const key = shimPinnedBatchKey
	first, second := pinnedShimBatch(now)
	second.DeliverAfter = now.Add(5 * time.Minute)
	f.writeFileState(t, func(state *nudgequeue.State) { state.Pending = []nudgequeue.Item{first, second} })
	target := shimTarget("alpha/worker")
	var staleClaim []nudgequeue.Item
	racing := &racingShimFile{nudgeQueueShimFile: f.file, race: func() {
		var err error
		if staleClaim, err = f.file.ClaimDue(target, now); err != nil {
			t.Fatalf("stale-mode ClaimDue: %v", err)
		}
	}}
	report, err := shimNudgeQueue(context.Background(), nudgeQueueShimToBeads, racing, f.beads, now)
	if err != nil {
		t.Fatalf("shim: %v", err)
	}
	assertShimReport(t, report, nudgeQueueShimReport{Raced: 1})
	if got := shimIDs(staleClaim); !reflect.DeepEqual(got, []string{"n-a"}) {
		t.Fatalf("stale-mode claim = %v, want the due member", got)
	}
	file := f.fileState(t)
	if got := shimIDs(file.InFlight); !reflect.DeepEqual(got, []string{"n-a"}) {
		t.Fatalf("file in flight = %v, want the claimed member kept for its claimant", got)
	}
	if got := shimIDs(file.Pending); !reflect.DeepEqual(got, []string{"n-b"}) {
		t.Fatalf("file pending = %v, want the unclaimed member kept with its batch", got)
	}
	for _, item := range append(append([]nudgequeue.Item{}, file.InFlight...), file.Pending...) {
		if item.DeliveryBatch != key {
			t.Fatalf("file member %s batch = %q, want the pin kept on the file side", item.ID, item.DeliveryBatch)
		}
	}
	beadsLive := f.beadsState(t)
	if got := shimIDs(beadsLive.Pending); !reflect.DeepEqual(got, []string{"n-a", "n-b"}) {
		t.Fatalf("beads pending = %v, want the moved copy deliverable: a duplicate, not a loss", got)
	}
	for _, item := range beadsLive.Pending {
		if item.DeliveryBatch != "" {
			t.Fatalf("beads member %s batch = %q, want the moved copy unpinned", item.ID, item.DeliveryBatch)
		}
	}
	// Once the stale claim's lease ends, the next pass removes the batch whole.
	later := now.Add(10 * time.Minute)
	assertShimReport(t, f.shim(t, nudgeQueueShimToBeads, later), nudgeQueueShimReport{Moved: 1})
	if state := f.fileState(t); len(state.Pending)+len(state.InFlight) != 0 {
		t.Fatalf("file queue after the lease = %+v, want the batch removed whole", state)
	}
}

// An old binary re-adds an id with a new creation time between the shim's
// read and its removal (DES-NQ D12: removal matches (id, created_at)). The new
// item is a different item, so it stays for the next pass.
func TestNudgeQueueShimRemovalKeepsAnIDReaddedWithANewCreationTime(t *testing.T) {
	f := newShimFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	original := shimItem("n-readd", now)
	f.writeFileState(t, func(state *nudgequeue.State) { state.Pending = []nudgequeue.Item{original} })
	readded := original
	readded.CreatedAt = now
	readded.Message = "a newer nudge under the same id"
	racing := &racingShimFile{nudgeQueueShimFile: f.file, race: func() {
		f.writeFileState(t, func(state *nudgequeue.State) { state.Pending = []nudgequeue.Item{readded} })
	}}
	report, err := shimNudgeQueue(context.Background(), nudgeQueueShimToBeads, racing, f.beads, now)
	if err != nil {
		t.Fatalf("shim: %v", err)
	}
	assertShimReport(t, report, nudgeQueueShimReport{Raced: 1})
	pending := f.fileState(t).Pending
	if len(pending) != 1 || !pending[0].CreatedAt.Equal(readded.CreatedAt) || pending[0].Message != readded.Message {
		t.Fatalf("file pending = %+v, want the re-added item kept", pending)
	}
}

// partialLookupBeads reports one id of every Lookup as an undecodable record.
type partialLookupBeads struct {
	nudgeQueueShimBeads
	skipID string
}

func (q *partialLookupBeads) Lookup(ids []string, now time.Time) (map[string]nudgequeue.Bucket, error) {
	buckets, err := q.nudgeQueueShimBeads.Lookup(ids, now)
	if err != nil {
		return buckets, err
	}
	delete(buckets, q.skipID)
	return buckets, &beads.PartialResultError{Op: "nudge queue Lookup", Err: storebinding.ErrBeadsNudgeQueueCorrupt}
}

// A partial Lookup fails the batch: the record it skipped is one of the
// batch's own, so nothing is removed from the file queue.
func TestNudgeQueueShimPartialLookupFailsTheBatch(t *testing.T) {
	f := newShimFixture(t)
	now := time.Now().UTC()
	first, second := pinnedShimBatch(now)
	f.writeFileState(t, func(state *nudgequeue.State) { state.Pending = []nudgequeue.Item{first, second} })
	partial := &partialLookupBeads{nudgeQueueShimBeads: f.beads, skipID: "n-b"}
	report, err := shimNudgeQueue(context.Background(), nudgeQueueShimToBeads, f.file, partial, now)
	if !beads.IsPartialResult(err) {
		t.Fatalf("shim error = %v, want the partial Lookup reported", err)
	}
	assertShimReport(t, report, nudgeQueueShimReport{Failed: 1})
	if got := shimIDs(f.fileState(t).Pending); !reflect.DeepEqual(got, []string{"n-a", "n-b"}) {
		t.Fatalf("file pending = %v, want the batch kept whole", got)
	}
}

// One undecodable beads record does not strand the rest: beads → file moves
// every record that decodes and reports the corrupt one as a partial result.
func TestNudgeQueueShimToFileMovesPastACorruptRecord(t *testing.T) {
	f := newShimFixture(t)
	now := time.Now().UTC()
	for _, id := range []string{"n-good-1", "n-good-2", "n-bad"} {
		if err := f.beads.EnqueueDeferred(shimItem(id, now)); err != nil {
			t.Fatalf("EnqueueDeferred(%s): %v", id, err)
		}
	}
	records, err := f.store.List(beads.ListQuery{Label: "gc:nudge-queue", AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, record := range records {
		if record.Metadata["nudge_id"] == "n-bad" {
			if err := f.store.SetMetadata(record.ID, "deliver_after", "not-a-time"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
		}
	}
	report, err := shimNudgeQueue(context.Background(), nudgeQueueShimToFile, f.file, f.beads, now)
	if !beads.IsPartialResult(err) || !errors.Is(err, storebinding.ErrBeadsNudgeQueueCorrupt) {
		t.Fatalf("shim error = %v, want the corrupt record reported as a partial result", err)
	}
	assertShimReport(t, report, nudgeQueueShimReport{Moved: 2})
	if got := shimIDs(f.fileState(t).Pending); !reflect.DeepEqual(got, []string{"n-good-1", "n-good-2"}) {
		t.Fatalf("file pending = %v, want every record that decodes moved", got)
	}
}

// A beads record whose id the file queue already holds as a dead letter is
// not imported. Its record is retired with a reason saying so, and its batch
// is reported apart from the moved ones.
func TestNudgeQueueShimToFileReportsAnIDTheFileHoldsDead(t *testing.T) {
	store := openShimTestBeadsStore(t)
	fronts, err := storebinding.NewBeadsNudgeFrontDoors(beads.NudgesStore{Store: store})
	if err != nil {
		t.Fatalf("NewBeadsNudgeFrontDoors: %v", err)
	}
	queue := fronts.Queue.(nudgeQueueShimBeads)
	city := t.TempDir()
	file := openNudgeQueue(city, beads.NudgesStore{}).Queue.(*fileNudgeQueue)
	f := shimFixture{city: city, file: file, beads: queue, store: store}
	now := time.Now().UTC()
	dead := shimItem("n-dead-in-file", now)
	dead.DeadAt = now.Add(-time.Minute)
	f.writeFileState(t, func(state *nudgequeue.State) { state.Dead = []nudgequeue.Item{dead} })
	for _, id := range []string{"n-dead-in-file", "n-fresh"} {
		if err := queue.EnqueueDeferred(shimItem(id, now)); err != nil {
			t.Fatalf("EnqueueDeferred(%s): %v", id, err)
		}
	}

	assertShimReport(t, f.shim(t, nudgeQueueShimToFile, now), nudgeQueueShimReport{
		Moved:          1,
		AlreadyDead:    1,
		AlreadyDeadIDs: []string{"n-dead-in-file"},
	})
	state := f.fileState(t)
	if got := shimIDs(state.Pending); !reflect.DeepEqual(got, []string{"n-fresh"}) {
		t.Fatalf("file pending = %v, want only the fresh record imported", got)
	}
	if got := shimIDs(state.Dead); !reflect.DeepEqual(got, []string{"n-dead-in-file"}) {
		t.Fatalf("file dead = %v, want the dead letter untouched", got)
	}
	skipped := mustFindShimShadow(t, fronts.Shadows, "n-dead-in-file")
	if skipped.State != nudgeQueueMigratedOutcome || skipped.TerminalReason != nudgeQueueMigratedDeadReason {
		t.Fatalf("skipped record projected %+v, want it retired with reason %q", skipped, nudgeQueueMigratedDeadReason)
	}
	moved := mustFindShimShadow(t, fronts.Shadows, "n-fresh")
	if moved.State != nudgeQueueMigratedOutcome || moved.TerminalReason != nudgeQueueMigratedReason {
		t.Fatalf("moved record projected %+v, want it retired as imported", moved)
	}
}

func mustFindShimShadow(t *testing.T, shadows storebinding.NudgeShadows, id string) nudgequeue.NudgeShadow {
	t.Helper()
	shadow, ok, err := shadows.FindIncludingTerminal(id)
	if err != nil || !ok {
		t.Fatalf("FindIncludingTerminal(%s) = %v, %v", id, ok, err)
	}
	return shadow
}
