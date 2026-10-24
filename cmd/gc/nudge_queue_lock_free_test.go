package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// The lock-free no-op verbs and the poller herd (DES-NQL §5.3, §5.4,
// ga-fsp5w9). They replace package seams through newNQCharHarness, so they
// stay serial.

// holdNudgeQueueLockForTest takes the queue's flock on a descriptor of its own
// and returns the release.
func holdNudgeQueueLockForTest(t *testing.T, city string) func() {
	t.Helper()
	path := nudgequeue.LockPath(city)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	t.Cleanup(release)
	return release
}

func stateInodeForTest(t *testing.T, city string) uint64 {
	t.Helper()
	ino, ok := statIno(nudgequeue.StatePath(city))
	if !ok {
		t.Fatal("state.json is missing")
	}
	return ino
}

// within fails the test when fn takes longer than limit.
func within(t *testing.T, what string, limit time.Duration, fn func()) {
	t.Helper()
	start := time.Now()
	fn()
	if elapsed := time.Since(start); elapsed > limit {
		t.Fatalf("%s took %s, want under %s", what, elapsed, limit)
	}
}

// T3: sibling sessions of one agent, whose only candidates are fenced to a
// live session, take no lock: the poller gate says no work, a claim claims
// nothing, and no skip is counted, all while another process holds the lock.
func TestNudgeSiblingPollersTakeNoLock(t *testing.T) {
	h := newNQCharHarness(t)
	h.live = map[string]struct{}{}
	for i := 1; i <= 51; i++ {
		h.live[fmt.Sprintf("s-%d", i)] = struct{}{}
	}
	for _, id := range []string{"n-1", "n-2", "n-3"} {
		if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item(id, "alpha", -time.Minute, queuedNudgeOptions{SessionID: "s-1"})); err != nil {
			t.Fatal(err)
		}
	}
	restore := nudgequeue.SetLockWaitForTest(2 * time.Second)
	defer restore()
	before := stateInodeForTest(t, h.city)
	holdNudgeQueueLockForTest(t, h.city)
	h.lockHeldByTest = true
	h.rec.Reset()

	now := time.Now()
	for i := 2; i <= 51; i++ {
		sibling := h.target(fmt.Sprintf("s-%d", i), "")
		gate := newNudgePollGate()
		skips := &nudgePollSkips{cityPath: h.city}
		census := h.census
		within(t, "a sibling's gate and skip check", 200*time.Millisecond, func() {
			if gate.hasDue(sibling, now) {
				t.Fatalf("sibling s-%d sees due work fenced to live s-1", i)
			}
			// The poll loop's skip check after a tick that delivered nothing.
			countNudgePollSkip(gate, skips, sibling, nil, now, nil)
		})
		if len(skips.counts) != 0 {
			t.Fatalf("sibling s-%d counted skips %v", i, skips.counts)
		}
		if asked := h.census - census; asked > 1 {
			t.Fatalf("sibling s-%d's gate asked the census %d times, want at most once", i, asked)
		}
		census = h.census
		within(t, "a sibling's claim", 200*time.Millisecond, func() {
			claimed, err := claimDueQueuedNudgeBatchForTarget(h.city, sibling, now)
			if err != nil || len(claimed) != 0 {
				t.Fatalf("sibling s-%d claim = %v, %v; want nothing and no error", i, nqCharItems(claimed), err)
			}
		})
		if asked := h.census - census; asked > 1 {
			t.Fatalf("sibling s-%d's claim asked the census %d times, want at most once", i, asked)
		}
	}
	if after := stateInodeForTest(t, h.city); after != before {
		t.Fatal("siblings rewrote state.json")
	}
	if ops := h.rec.Ops(); len(ops) != 0 {
		t.Fatalf("siblings made %d shadow-store calls", len(ops))
	}
}

// T3b: the gate's census cache only skips. A sibling whose cached census saw
// the fence holder live keeps skipping while the cache is fresh; once the
// holder drained and the cache expired, it re-reads the census, sees the
// adoption, and its claim (with a fresh census) adopts.
func TestNudgePollGateCensusCacheOnlySkips(t *testing.T) {
	h := newNQCharHarness(t)
	h.live = map[string]struct{}{"s-1": {}, "s-2": {}}
	if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-1", "alpha", -time.Minute, queuedNudgeOptions{SessionID: "s-1"})); err != nil {
		t.Fatal(err)
	}
	sibling := h.target("s-2", "")
	gate := newNudgePollGate()
	now := time.Now()
	if gate.hasDue(sibling, now) {
		t.Fatal("gate sees work fenced to live s-1")
	}
	// s-1 drains. Inside the cache window the gate still skips.
	h.live = map[string]struct{}{"s-2": {}}
	census := h.census
	if gate.hasDue(sibling, now.Add(nudgePollCensusTTL/2)) {
		t.Fatal("gate re-read the census inside its cache window")
	}
	if h.census != census {
		t.Fatalf("gate asked the census %d times inside its cache window", h.census-census)
	}
	// Past the window it re-reads and sees the adoption.
	if !gate.hasDue(sibling, now.Add(nudgePollCensusTTL+time.Second)) {
		t.Fatal("gate past its cache window does not see the adoptable nudge")
	}
	claimed, err := claimDueQueuedNudgeBatchForTarget(h.city, sibling, now.Add(nudgePollCensusTTL+time.Second))
	if err != nil || len(claimed) != 1 || claimed[0].SessionID != "s-2" {
		t.Fatalf("claim = %v, %v; want n-1 adopted onto s-2", nqCharItems(claimed), err)
	}
}

// A gate whose cached census says the holder drained still leaves the claim to
// a fresh census: if the holder is live again, the claim adopts nothing.
func TestNudgeClaimAfterAnAdoptingGateReadsAFreshCensus(t *testing.T) {
	h := newNQCharHarness(t)
	h.live = map[string]struct{}{"s-2": {}}
	if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-1", "alpha", -time.Minute, queuedNudgeOptions{SessionID: "s-1"})); err != nil {
		t.Fatal(err)
	}
	sibling := h.target("s-2", "")
	gate := newNudgePollGate()
	now := time.Now()
	if !gate.hasDue(sibling, now) {
		t.Fatal("gate does not see the adoptable nudge")
	}
	h.live = map[string]struct{}{"s-1": {}, "s-2": {}}
	claimed, err := claimDueQueuedNudgeBatchForTarget(h.city, sibling, now)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("claim with a fresh census showing s-1 live = %v, %v; want nothing", nqCharItems(claimed), err)
	}
}

// T7: a verb that would change nothing answers from a lock-free snapshot: it
// takes no lock and writes nothing. A verb whose maintenance changes the state
// does take the lock.
func TestNudgeNoOpVerbsTakeNoLock(t *testing.T) {
	h := newNQCharHarness(t)
	alpha := h.target("s-new", "2")
	later := h.item("n-later", "alpha", 0, queuedNudgeOptions{})
	later.DeliverAfter = time.Now().Add(time.Hour)
	if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), later); err != nil {
		t.Fatal(err)
	}
	restore := nudgequeue.SetLockWaitForTest(300 * time.Millisecond)
	defer restore()
	before := stateInodeForTest(t, h.city)
	release := holdNudgeQueueLockForTest(t, h.city)
	h.lockHeldByTest = true

	now := time.Now()
	within(t, "a claim with nothing claimable", 50*time.Millisecond, func() {
		claimed, err := claimDueQueuedNudgesForTarget(h.city, alpha, now)
		if err != nil || len(claimed) != 0 {
			t.Fatalf("claim = %v, %v", nqCharItems(claimed), err)
		}
	})
	within(t, "ListFor", 50*time.Millisecond, func() {
		pending, _, _, err := listQueuedNudgesForTarget(h.city, alpha, now)
		if err != nil || len(pending) != 1 {
			t.Fatalf("ListFor = %v, %v", nqCharItems(pending), err)
		}
	})
	within(t, "ListForAgent", 50*time.Millisecond, func() {
		pending, _, _, err := listQueuedNudges(h.city, "alpha", now)
		if err != nil || len(pending) != 1 {
			t.Fatalf("ListForAgent = %v, %v", nqCharItems(pending), err)
		}
	})
	within(t, "Lookup", 50*time.Millisecond, func() {
		buckets, err := openNudgeQueue(h.city, beads.NudgesStore{}).Queue.Lookup([]string{"n-later", "n-none"}, now)
		if err != nil || buckets["n-later"] != nudgequeue.BucketPending || len(buckets) != 1 {
			t.Fatalf("Lookup = %v, %v", buckets, err)
		}
	})
	within(t, "an ack of an id the queue does not hold", 50*time.Millisecond, func() {
		if err := ackQueuedNudgesWithOutcome(h.city, []string{"n-none"}, "injected", "", "provider-nudge-return"); err != nil {
			t.Fatal(err)
		}
	})
	if after := stateInodeForTest(t, h.city); after != before {
		t.Fatal("a no-op verb rewrote state.json")
	}

	// A day on, the item has expired: the claim's maintenance changes the
	// state, so it needs the lock, and times out against the held one.
	_, err := claimDueQueuedNudgesForTarget(h.city, alpha, now.Add(defaultQueuedNudgeTTL+time.Hour))
	if !errors.Is(err, nudgequeue.ErrQueueBusy) {
		t.Fatalf("a claim whose maintenance expires an item under a held lock: err = %v, want ErrQueueBusy", err)
	}
	release()
}

// T7 race: the state moves between a claim's dry run and its locked run
// (another writer claims the item first). The locked run claims nothing,
// returns nothing, and does not rewrite the file.
func TestNudgeClaimWhoseItemWasTakenAfterTheDryRun(t *testing.T) {
	h := newNQCharHarness(t)
	alpha := h.target("s-new", "2")
	if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-1", "alpha", -time.Minute, queuedNudgeOptions{})); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var inode uint64
	nudgeClaimAfterDryRun = func() {
		nudgeClaimAfterDryRun = nil
		claimed, err := claimDueQueuedNudgesForTarget(h.city, alpha, now)
		if err != nil || len(claimed) != 1 {
			t.Errorf("the other writer's claim = %v, %v", nqCharItems(claimed), err)
		}
		inode = stateInodeForTest(t, h.city)
	}
	t.Cleanup(func() { nudgeClaimAfterDryRun = nil })
	claimed, err := claimDueQueuedNudgesForTarget(h.city, alpha, now)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("claim after losing the race = %v, %v; want nothing", nqCharItems(claimed), err)
	}
	if after := stateInodeForTest(t, h.city); after != inode {
		t.Fatal("a locked run that changed nothing rewrote state.json")
	}
}

// A claim adopts only the candidates its census-reading dry run judged. A
// nudge fenced to a session created after that census read, queued between
// the dry run and the locked run, has no census of its own: the locked run
// leaves it pending, though it adopts the candidate the dry run judged.
func TestNudgeClaimDoesNotAdoptACandidateItsDryRunDidNotJudge(t *testing.T) {
	h := newNQCharHarness(t)
	alpha := h.target("s-new", "2")
	if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-0", "alpha", -time.Minute, queuedNudgeOptions{SessionID: "s-gone"})); err != nil {
		t.Fatal(err)
	}
	nudgeClaimAfterDryRun = func() {
		nudgeClaimAfterDryRun = nil
		h.live = map[string]struct{}{"s-new": {}, "s-sib": {}, "s-3": {}}
		if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-3", "alpha", -time.Second, queuedNudgeOptions{SessionID: "s-3"})); err != nil {
			t.Errorf("enqueue n-3: %v", err)
		}
	}
	t.Cleanup(func() { nudgeClaimAfterDryRun = nil })
	claimed, err := claimDueQueuedNudgesForTarget(h.city, alpha, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != "n-0" {
		t.Fatalf("claimed %v (census reads %d); want only n-0, the candidate the dry run judged", nqCharItems(claimed), h.census)
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.Pending) != 1 || state.Pending[0].ID != "n-3" || state.Pending[0].SessionID != "s-3" {
		t.Fatalf("pending = %v; want n-3 still fenced to s-3", nqCharItems(state.Pending))
	}
}

// The poll loop's skip counts are flushed at most once per interval and at
// exit, not once per tick.
func TestNudgePollSkipsCoalesce(t *testing.T) {
	city := t.TempDir()
	skips := &nudgePollSkips{cityPath: city}
	now := time.Now()
	read := func() map[string]int64 {
		got, err := openNudgeQueue(city, beads.NudgesStore{}).Skips.DispatchSkips()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for i := 0; i < 10; i++ {
		skips.add("not-delivered", now.Add(time.Duration(i)*time.Second), nil)
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("skips flushed inside the interval: %v", got)
	}
	skips.add("not-delivered", now.Add(nudgePollSkipFlushInterval), nil)
	if got := read(); got["not-delivered"] != 11 {
		t.Fatalf("skips after the interval = %v, want not-delivered=11", got)
	}
	skips.add("not-delivered-error", now.Add(nudgePollSkipFlushInterval+time.Second), nil)
	skips.flush(now.Add(nudgePollSkipFlushInterval+2*time.Second), nil)
	if got := read(); got["not-delivered"] != 11 || got["not-delivered-error"] != 1 {
		t.Fatalf("skips after the exit flush = %v", got)
	}
}

// An enqueue that waited long for the lock still supersedes: its maintenance
// and supersession budget starts when its locked run does, not when the verb
// was called.
func TestNudgeEnqueueSupersedesAfterALongLockWait(t *testing.T) {
	h := newNQCharHarness(t)
	ref := &nudgeReference{Kind: "bead", ID: "mail-1"}
	if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-a", "alpha", -time.Minute, queuedNudgeOptions{Reference: ref})); err != nil {
		t.Fatal(err)
	}
	release := holdNudgeQueueLockForTest(t, h.city)
	h.lockHeldByTest = true
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(nudgeEnqueueMaintenanceBudget + 500*time.Millisecond)
		release()
	}()
	err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-b", "alpha", -time.Minute, queuedNudgeOptions{Reference: ref}))
	<-released
	if err != nil {
		t.Fatal(err)
	}
	h.lockHeldByTest = false
	state := loadNudgeStateForTest(t, h.city)
	if len(state.Pending) != 1 || state.Pending[0].ID != "n-b" {
		t.Fatalf("after a long lock wait pending=%v dead=%v, want n-a superseded by n-b", nqCharItems(state.Pending), nqCharItems(state.Dead))
	}
}

// The poll loop counts a skip after a tick that delivered nothing only while
// the target still has work it may claim, under the reason the tick's error
// names.
func TestCountNudgePollSkipCountsOnlyClaimableWork(t *testing.T) {
	h := newNQCharHarness(t)
	now := time.Now()
	gate := newNudgePollGate()
	skips := &nudgePollSkips{cityPath: h.city}
	self := h.target("s-new", "")
	countNudgePollSkip(gate, skips, self, nil, now, nil)
	if len(skips.counts) != 0 {
		t.Fatalf("an empty queue counted skips %v", skips.counts)
	}
	if err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-1", "alpha", -time.Minute, queuedNudgeOptions{})); err != nil {
		t.Fatal(err)
	}
	countNudgePollSkip(gate, skips, self, nil, now, nil)
	countNudgePollSkip(gate, skips, self, errors.New("delivery failed"), now, nil)
	if skips.counts["not-delivered"] != 1 || skips.counts["not-delivered-error"] != 1 {
		t.Fatalf("skips = %v, want one not-delivered and one not-delivered-error", skips.counts)
	}
}
