package workqueue

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Every test here runs inside a testing/synctest bubble. Timers, backoff and
// the tests' own waits share the bubble's virtual clock, and synctest.Wait
// returns only once every other goroutine is durably blocked, so a Get that
// has not returned after it is blocked for good, not slow.

// newTestQueue returns a queue whose jitter draw is the midpoint, so backoff
// is exact unless a test injects its own Rand.
func newTestQueue(cfg Config[string]) *Queue[string] {
	if cfg.Rand == nil {
		cfg.Rand = func() float64 { return 0.5 }
	}
	return New(cfg)
}

// tryGet returns the item Get hands out now, or false if Get would block.
func tryGet(q *Queue[string]) (Item[string], bool) {
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		it Item[string]
		ok bool
	}
	got := make(chan result, 1)
	go func() {
		it, ok := q.Get(ctx)
		got <- result{it, ok}
	}()
	synctest.Wait()
	cancel()
	r := <-got
	return r.it, r.ok
}

func mustGet(t *testing.T, q *Queue[string], want string) Item[string] {
	t.Helper()
	it, ok := tryGet(q)
	if !ok {
		t.Fatalf("Get: nothing available, want %q", want)
	}
	if it.Key != want {
		t.Fatalf("Get = %q, want %q", it.Key, want)
	}
	return it
}

func mustBeEmpty(t *testing.T, q *Queue[string], when string) {
	t.Helper()
	if it, ok := tryGet(q); ok {
		t.Fatalf("%s: Get handed out %q (reasons %v), want nothing", when, it.Key, kinds(it))
	}
}

// drain gets and finishes every available key, in order.
func drain(q *Queue[string]) []string {
	var got []string
	for {
		it, ok := tryGet(q)
		if !ok {
			return got
		}
		got = append(got, it.Key)
		q.Done(it.Key)
	}
}

func kinds(it Item[string]) []string {
	var ks []string
	for _, r := range it.Reasons {
		ks = append(ks, r.Kind)
	}
	return ks
}

// advance moves the bubble clock forward by d and lets timers settle.
func advance(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

func hot(kind string) Reason { return Reason{Kind: kind} }

// Kills: no dedupe (two items for one key); no merge (later reasons lost);
// no cap (more than MaxReasons carried).
func TestQueueAddDedupesAndMergesReasons(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		start := time.Now()
		first, _ := q.Add("a", LaneHot, hot("k0"))
		var last uint64
		for i := 1; i < MaxReasons+2; i++ {
			advance(time.Second)
			last, _ = q.Add("a", LaneHot, hot("k"+strconv.Itoa(i)))
		}
		dup, _ := q.Add("a", LaneResync, Reason{Kind: "k0", Detail: "again", Urgent: true})
		if q.Stats().Depth[LaneHot] != 1 || q.Stats().Depth[LaneResync] != 0 {
			t.Fatalf("depth = %v, want one hot item", q.Stats().Depth)
		}

		it := mustGet(t, q, "a")
		wantKinds := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7"}
		if !slices.Equal(kinds(it), wantKinds) || it.Dropped != 2 {
			t.Fatalf("reasons %v dropped %d, want %v dropped 2", kinds(it), it.Dropped, wantKinds)
		}
		if r := it.Reasons[0]; r.Detail != "" || !r.Urgent {
			t.Fatalf("deduped reason = %+v, want the first Detail with the urgent flag merged in", r)
		}
		if !it.AddedAt.Equal(start) || it.FirstSeq != first || it.LastSeq != dup || dup <= last {
			t.Fatalf("AddedAt %v FirstSeq %d LastSeq %d, want %v %d %d", it.AddedAt, it.FirstSeq, it.LastSeq, start, first, dup)
		}
		q.Done("a")
		mustBeEmpty(t, q, "after the merged item")
	})
}

// Kills: dirty bit dropped (a wake during processing is lost); requeue
// before Done (the key is handed out twice at once).
func TestQueueAddDuringProcessingMarksDirtyAndRequeuesAtDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.Add("a", LaneHot, hot("event"))
		inFlight := mustGet(t, q, "a")

		advance(time.Second)
		dirtyAt := time.Now()
		dirtySeq, _ := q.Add("a", LaneResync, hot("resync"))
		q.Add("a", LaneHot, hot("api"))
		q.Add("b", LaneHot, hot("event"))
		if s := q.Stats(); s.Dirty != 1 || s.Processing != 1 {
			t.Fatalf("stats dirty=%d processing=%d, want 1 and 1", s.Dirty, s.Processing)
		}
		mustGet(t, q, "b")
		mustBeEmpty(t, q, "while a is processing")

		q.Done("a")
		again := mustGet(t, q, "a")
		if again.Lane != LaneHot || !slices.Equal(kinds(again), []string{"resync", "api"}) {
			t.Fatalf("requeued lane %d reasons %v, want hot [resync api]", again.Lane, kinds(again))
		}
		if again.FirstSeq != dirtySeq || !again.AddedAt.Equal(dirtyAt) || inFlight.LastSeq >= dirtySeq {
			t.Fatalf("requeued FirstSeq %d AddedAt %v (in-flight LastSeq %d), want %d %v", again.FirstSeq, again.AddedAt, inFlight.LastSeq, dirtySeq, dirtyAt)
		}
		q.Done("a")
		q.Done("b")
		mustBeEmpty(t, q, "after the requeue ran")
	})
}

// Kills: Get handing out a processing key (an Add during processing
// enqueuing it instead of marking it dirty); any lost wake or lost Add; the
// backoff gate letting a non-urgent item out before its retry time; entries
// or counts leaking once all work and backoff have run out.
func TestQueueNeverHandsOutAKeyBeingProcessed(t *testing.T) {
	for seed := uint64(1); seed <= 4; seed++ {
		t.Run("seed"+strconv.FormatUint(seed, 10), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { runSerializationScript(t, seed) })
		})
	}
}

// gateModel is the backoff gate as the script last set it: closed until
// until, by an AddRateLimited made when the queue's seq was closeSeq.
type gateModel struct {
	until    time.Time
	closeSeq uint64
}

func runSerializationScript(t *testing.T, seed uint64) {
	q := newTestQueue(Config[string]{AgingEvery: 3, BaseBackoff: time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	keys := []string{"a", "b", "c", "d", "e"}
	var (
		mu       sync.Mutex
		inHand   = map[string]bool{}
		reconLen = map[string]uint64{} // highest LastSeq handed out per key
		gates    = map[string]gateModel{}
		failing  = true
	)
	// rateLimited and forget keep gates in step with the queue; call with mu
	// held.
	rateLimited := func(k string) {
		q.AddRateLimited(k, hot("retry"))
		q.mu.Lock()
		gates[k] = gateModel{until: q.keys[k].retryAt, closeSeq: q.seq}
		q.mu.Unlock()
	}
	forget := func(k string) {
		q.Forget(k)
		delete(gates, k)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(w)))
			for {
				it, ok := q.Get(ctx)
				if !ok {
					return
				}
				mu.Lock()
				if inHand[it.Key] {
					t.Errorf("worker %d got %q while another worker holds it", w, it.Key)
				}
				// An item formed after its gate closed may leave before the
				// retry time only if urgent. One formed earlier was queued
				// or dirty before the failure and is not this check's to judge.
				if g := gates[it.Key]; !it.Urgent && g.closeSeq < it.FirstSeq && time.Now().Before(g.until) {
					t.Errorf("worker %d got %q (reasons %v, seqs %d-%d) %v before its retry time, not urgent",
						w, it.Key, kinds(it), it.FirstSeq, it.LastSeq, time.Until(g.until))
				}
				inHand[it.Key] = true
				reconLen[it.Key] = max(reconLen[it.Key], it.LastSeq)
				mu.Unlock()
				<-time.After(time.Duration(1+w) * time.Millisecond)
				mu.Lock()
				delete(inHand, it.Key)
				if failing && rng.IntN(4) == 0 {
					rateLimited(it.Key)
				} else {
					forget(it.Key)
				}
				mu.Unlock()
				q.Done(it.Key)
			}
		}()
	}

	rng := rand.New(rand.NewPCG(seed, seed))
	lastAdd := map[string]uint64{}
	held := false
	for range 2000 {
		k := keys[rng.IntN(len(keys))]
		switch op := rng.IntN(100); {
		case op < 68:
			r := hot(strconv.Itoa(rng.IntN(3)))
			lane := Lane(rng.IntN(2))
			if op >= 60 {
				r, lane = Reason{Kind: "urgent", Urgent: true}, LaneHot
			}
			seq, ok := q.Add(k, lane, r)
			if !ok {
				t.Fatalf("Add(%q) refused before shutdown", k)
			}
			lastAdd[k] = seq
		case op < 78:
			q.AddAfter(k, time.Duration(rng.IntN(5))*time.Millisecond, hot("after"))
		case op < 86:
			mu.Lock()
			rateLimited(k)
			mu.Unlock()
		case op < 94:
			mu.Lock()
			forget(k)
			mu.Unlock()
		case !held:
			q.Hold("chaos")
			held = true
		default:
			q.Release("chaos")
			held = false
		}
		if rng.IntN(4) == 0 {
			<-time.After(time.Duration(rng.IntN(3)) * time.Millisecond)
		}
	}
	q.Release("chaos")
	mu.Lock()
	failing = false
	mu.Unlock()
	advance(time.Second) // far longer than the remaining work and backoff

	mu.Lock()
	for k, seq := range lastAdd {
		if reconLen[k] < seq {
			t.Errorf("key %q: last Add seq %d never reconciled (highest LastSeq %d)", k, seq, reconLen[k])
		}
	}
	mu.Unlock()
	s := q.Stats()
	if s.Keys != 0 || s.Depth != [laneCount]int{} || s.Processing != 0 || !s.OldestInFlight.IsZero() ||
		s.Dirty != 0 || s.Deferred != 0 || s.Timers != 0 || len(s.Holds) != 0 {
		t.Errorf("queue not settled: %+v", s)
	}
	cancel()
	wg.Wait()
}

// Kills: aging ignored (resync starves behind hot work); resync served
// first.
func TestQueueHotFirstWithAgingEveryK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{AgingEvery: 4})
		for _, k := range []string{"r1", "r2", "r3"} {
			q.Add(k, LaneResync, hot("resync"))
		}
		for _, k := range []string{"h1", "h2", "h3", "h4", "h5", "h6"} {
			q.Add(k, LaneHot, hot("event"))
		}
		want := []string{"h1", "h2", "h3", "r1", "h4", "h5", "h6", "r2", "r3"}
		if got := drain(q); !slices.Equal(got, want) {
			t.Fatalf("dequeue order %v, want %v", got, want)
		}
	})
}

// Kills: no promotion (a hot Add waits behind the resync lane); promotion
// without lazy deletion (the stale resync slot hands the key out twice).
func TestQueueHotAddPromotesQueuedResyncKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		for _, k := range []string{"r1", "r2", "r3"} {
			q.Add(k, LaneResync, hot("resync"))
		}
		q.Add("h1", LaneHot, hot("event"))
		q.Add("r2", LaneHot, hot("event"))
		if d := q.Stats().Depth; d != [laneCount]int{2, 2} {
			t.Fatalf("depth after promotion = %v, want [2 2]", d)
		}
		// Hold every item in hand so a stale slot would surface as a duplicate.
		var got []string
		for {
			it, ok := tryGet(q)
			if !ok {
				break
			}
			got = append(got, it.Key)
			if it.Key == "r2" && (it.Lane != LaneHot || !slices.Equal(kinds(it), []string{"resync", "event"})) {
				t.Fatalf("promoted item lane %d reasons %v, want hot [resync event]", it.Lane, kinds(it))
			}
		}
		if want := []string{"h1", "r2", "r1", "r3"}; !slices.Equal(got, want) {
			t.Fatalf("dequeue order %v, want %v", got, want)
		}
	})
}

// Kills: last writer wins (a later deadline replaces an earlier one); one
// timer per AddAfter (the key fires once per call).
func TestQueueAddAfterKeepsEarliestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.AddAfter("a", 5*time.Second, hot("t5"))
		q.AddAfter("a", 2*time.Second, hot("t2"))
		q.AddAfter("a", 8*time.Second, hot("t8"))
		if n := q.Stats().Timers; n != 1 {
			t.Fatalf("timers = %d, want 1", n)
		}
		advance(2*time.Second - time.Nanosecond)
		mustBeEmpty(t, q, "before the earliest deadline")
		advance(time.Nanosecond)
		it := mustGet(t, q, "a")
		if !slices.Equal(kinds(it), []string{"t5", "t2", "t8"}) {
			t.Fatalf("timer item reasons %v, want every AddAfter reason merged", kinds(it))
		}
		q.Done("a")
		advance(10 * time.Second)
		mustBeEmpty(t, q, "after the later deadlines passed")
		if n := q.Stats().Timers; n != 0 {
			t.Fatalf("timers = %d after firing, want 0", n)
		}
	})
}

// Kills: Add canceling the pending timer.
func TestQueueImmediateAddDoesNotCancelTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.AddAfter("a", 5*time.Second, hot("requeue-after"))
		q.Add("a", LaneHot, hot("event"))
		if it := mustGet(t, q, "a"); !slices.Equal(kinds(it), []string{"event"}) {
			t.Fatalf("immediate item reasons %v, want [event]", kinds(it))
		}
		q.Done("a")
		advance(5 * time.Second)
		if it := mustGet(t, q, "a"); !slices.Equal(kinds(it), []string{"requeue-after"}) {
			t.Fatalf("timer item reasons %v, want [requeue-after]", kinds(it))
		}
	})
}

// Kills: no cap; jitter outside ±Jitter; Forget a no-op.
func TestQueueRateLimitedBackoffJitterCapAndForget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		draw := 0.5
		q := newTestQueue(Config[string]{
			BaseBackoff: time.Second,
			MaxBackoff:  8 * time.Second,
			Jitter:      0.1,
			Rand:        func() float64 { return draw },
		})
		retryIn := func() time.Duration { return time.Until(q.keys["a"].retryAt) }

		for i, want := range []time.Duration{1, 2, 4, 8, 8, 8} {
			q.AddRateLimited("a", hot("retry"))
			if got := retryIn(); got != want*time.Second {
				t.Fatalf("failure %d: backoff %v, want %v", i+1, got, want*time.Second)
			}
		}
		for _, tc := range []struct {
			draw   float64
			lo, hi time.Duration
		}{
			{0, 7200 * time.Millisecond, 7200 * time.Millisecond},
			{0.9999999, 8799 * time.Millisecond, 8800 * time.Millisecond},
		} {
			draw = tc.draw
			q.AddRateLimited("a", hot("retry"))
			if got := retryIn(); got < tc.lo || got > tc.hi {
				t.Fatalf("draw %v: capped backoff %v, want in [%v, %v]", tc.draw, got, tc.lo, tc.hi)
			}
		}

		// Forget opens the gate: the held retry runs now, not at retryAt.
		draw = 0.5
		q.Forget("a")
		if it := mustGet(t, q, "a"); it.Failures != 0 {
			t.Fatalf("item released by Forget has failures %d, want 0", it.Failures)
		}
		q.AddRateLimited("a", hot("retry"))
		q.Done("a")
		if got := retryIn(); got != time.Second {
			t.Fatalf("backoff after Forget = %v, want the base 1s", got)
		}
		advance(time.Second - time.Nanosecond)
		mustBeEmpty(t, q, "before the retry time")
		advance(time.Nanosecond)
		if it := mustGet(t, q, "a"); it.Failures != 1 || !slices.Equal(kinds(it), []string{"retry"}) {
			t.Fatalf("retry item failures %d reasons %v, want 1 [retry]", it.Failures, kinds(it))
		}
	})
}

// Kills: doubling that stops short of the cap when MaxBackoff/BaseBackoff is
// not a power of two (at the 500ms/5m defaults it stuck at 256s).
func TestQueueBackoffDoublesToTheDefaultCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		want := map[int]time.Duration{1: 500 * time.Millisecond, 10: 256 * time.Second, 11: 5 * time.Minute, 12: 5 * time.Minute}
		for i := 1; i <= 12; i++ {
			q.AddRateLimited("a", hot("retry"))
			got := time.Until(q.keys["a"].retryAt)
			if w, ok := want[i]; ok && got != w {
				t.Fatalf("failure %d: backoff %v, want %v", i, got, w)
			}
		}
	})
}

// Kills: an early timer (armed by an earlier failure) that fires before a
// later failure's retry time and is not re-armed, stranding the retry.
func TestQueueEarlyTimerReArmsAfterUrgentBypassFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{BaseBackoff: time.Second, MaxBackoff: time.Minute})
		q.Add("a", LaneHot, hot("event"))
		mustGet(t, q, "a")
		q.AddRateLimited("a", hot("retry")) // gate and timer at 1s
		q.Done("a")
		q.Add("a", LaneHot, Reason{Kind: "api", Urgent: true})
		if it := mustGet(t, q, "a"); !it.Urgent {
			t.Fatal("urgent item has Urgent false")
		}
		q.AddRateLimited("a", hot("retry")) // gate at 2s; the 1s timer stays
		q.Done("a")

		advance(time.Second)
		mustBeEmpty(t, q, "after the early timer")
		if n := q.Stats().Timers; n != 1 {
			t.Fatalf("timers after the early firing = %d, want 1 re-armed at the retry time", n)
		}
		advance(time.Second - time.Nanosecond)
		mustBeEmpty(t, q, "just before the second retry time")
		advance(time.Nanosecond)
		if it := mustGet(t, q, "a"); it.Failures != 2 || !slices.Equal(kinds(it), []string{"retry"}) {
			t.Fatalf("retry item failures %d reasons %v, want 2 [retry]", it.Failures, kinds(it))
		}
	})
}

// Kills: Forget while processing releasing the deferred record into a dirty
// requeue (a successful reconcile followed by a redundant one).
func TestQueueForgetWhileProcessingDiscardsDeferred(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{BaseBackoff: time.Second, MaxBackoff: time.Minute})
		q.Add("a", LaneHot, hot("event"))
		mustGet(t, q, "a")
		q.AddRateLimited("a", hot("retry"))
		q.Done("a")
		gated, _ := q.Add("a", LaneResync, hot("resync")) // deferred behind the gate
		q.Add("a", LaneHot, Reason{Kind: "api", Urgent: true})
		if it := mustGet(t, q, "a"); it.LastSeq <= gated {
			t.Fatalf("bypass item LastSeq %d, want past the deferred Add's seq %d", it.LastSeq, gated)
		}
		q.Forget("a")
		q.Done("a")
		mustBeEmpty(t, q, "after a successful reconcile")
		if s := q.Stats(); s.Keys != 0 || s.Deferred != 0 || s.Timers != 0 {
			t.Fatalf("stats keys=%d deferred=%d timers=%d, want all 0", s.Keys, s.Deferred, s.Timers)
		}
		advance(2 * time.Second)
		mustBeEmpty(t, q, "after the old retry time")
	})
}

// Kills: urgency lost when the urgent reason is past MaxReasons (a timer
// record full of AddAfter reasons then waits behind the gate, and
// Item.Urgent is false).
func TestQueueUrgencySurvivesTheReasonCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{BaseBackoff: time.Minute, MaxBackoff: time.Hour})
		q.AddRateLimited("a", hot("retry"))
		for i := range MaxReasons {
			q.AddAfter("a", time.Second, hot("k"+strconv.Itoa(i)))
		}
		q.AddAfter("a", time.Second, Reason{Kind: "socket", Urgent: true})
		advance(time.Second)
		it := mustGet(t, q, "a")
		if !it.Urgent || it.Dropped != 1 || slices.Contains(kinds(it), "socket") {
			t.Fatalf("item urgent=%v dropped=%d reasons %v, want urgent, 1 dropped, no socket", it.Urgent, it.Dropped, kinds(it))
		}
	})
}

// Kills: Forget stopping a timer that still carries AddAfter reasons.
func TestQueueForgetKeepsPendingAddAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.AddAfter("a", 5*time.Second, hot("requeue-after"))
		q.Forget("a")
		if n := q.Stats().Timers; n != 1 {
			t.Fatalf("timers after Forget = %d, want 1", n)
		}
		advance(5 * time.Second)
		if it := mustGet(t, q, "a"); !slices.Equal(kinds(it), []string{"requeue-after"}) {
			t.Fatalf("timer item reasons %v, want [requeue-after]", kinds(it))
		}
	})
}

// Kills: idle entries never dropped (the key map grows with every key ever
// seen).
func TestQueueDropsIdleEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.Add("a", LaneHot, hot("event"))
		q.AddAfter("b", time.Second, hot("requeue-after"))
		q.AddRateLimited("c", hot("retry"))
		q.Forget("c")
		advance(time.Second)
		if got := drain(q); len(got) != 3 {
			t.Fatalf("drained %v, want a, b and c", got)
		}
		if n := q.Stats().Keys; n != 0 {
			t.Fatalf("keys after all work finished = %d, want 0", n)
		}
	})
}

// Kills: an out-of-range lane indexing past the lane arrays.
func TestQueueUnknownLaneIsHot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.Add("a", Lane(7), hot("event"))
		if d := q.Stats().Depth; d != [laneCount]int{1, 0} {
			t.Fatalf("depth = %v, want one hot item", d)
		}
		if it := mustGet(t, q, "a"); it.Lane != LaneHot {
			t.Fatalf("lane = %d, want LaneHot", it.Lane)
		}
	})
}

// Kills: the dirty record inheriting the in-flight item's lane instead of
// the lane of its own first Add.
func TestQueueDirtyRequeueTakesItsOwnLane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.Add("a", LaneHot, hot("event"))
		mustGet(t, q, "a")
		q.Add("a", LaneResync, hot("resync"))
		q.Done("a")
		if d := q.Stats().Depth; d != [laneCount]int{0, 1} {
			t.Fatalf("depth after the dirty requeue = %v, want one resync item", d)
		}
	})
}

// Kills: armLocked arming a timer after ShutDown (a gated dirty requeue at
// Done leaks one).
func TestQueueDoneAfterShutDownArmsNoTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.Add("a", LaneHot, hot("event"))
		mustGet(t, q, "a")
		q.Add("a", LaneHot, hot("event"))
		q.AddRateLimited("a", hot("retry"))
		q.ShutDown()
		q.Done("a")
		if n := q.Stats().Timers; n != 0 {
			t.Fatalf("timers after Done past ShutDown = %d, want 0", n)
		}
	})
}

// Kills: gate removed (a failing key retries at event rate); urgent reasons
// not bypassing it.
func TestQueueBackoffGateDefersNonUrgentAddsAndDirtyRequeues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{BaseBackoff: time.Second, MaxBackoff: time.Minute})

		// A failed reconcile with an event that landed while it ran: the
		// dirty requeue waits for the retry time.
		q.Add("a", LaneHot, hot("event"))
		mustGet(t, q, "a")
		q.Add("a", LaneHot, hot("replay"))
		q.AddRateLimited("a", hot("retry"))
		q.Done("a")
		mustBeEmpty(t, q, "dirty requeue behind the gate")

		// Idle-key adds wait too, and an earlier timer does not open the gate.
		q.AddAfter("a", 100*time.Millisecond, hot("requeue-after"))
		advance(500 * time.Millisecond)
		seq, accepted := q.Add("a", LaneResync, hot("resync"))
		if !accepted {
			t.Fatal("gated Add refused, want it accepted and deferred")
		}
		mustBeEmpty(t, q, "idle add behind the gate")
		if s := q.Stats(); s.Deferred != 1 || s.Timers != 1 {
			t.Fatalf("stats deferred=%d timers=%d, want 1 and 1", s.Deferred, s.Timers)
		}

		advance(500*time.Millisecond - time.Nanosecond)
		mustBeEmpty(t, q, "just before the retry time")
		advance(time.Nanosecond)
		it := mustGet(t, q, "a")
		if want := []string{"retry", "replay", "requeue-after", "resync"}; !slices.Equal(kinds(it), want) || it.Lane != LaneHot {
			t.Fatalf("released item lane %d reasons %v, want hot %v", it.Lane, kinds(it), want)
		}
		if it.LastSeq < seq {
			t.Fatalf("released LastSeq %d, want >= the deferred Add's seq %d", it.LastSeq, seq)
		}

		// Second failure. Urgent reasons bypass the closed gate, both for an
		// idle key and for a dirty requeue.
		q.AddRateLimited("a", hot("retry"))
		q.Done("a")
		q.Add("a", LaneHot, Reason{Kind: "api", Urgent: true})
		mustGet(t, q, "a")
		q.Add("a", LaneHot, Reason{Kind: "socket", Urgent: true})
		q.Done("a")
		if it := mustGet(t, q, "a"); !slices.Equal(kinds(it), []string{"socket"}) {
			t.Fatalf("urgent dirty requeue reasons %v, want [socket]", kinds(it))
		}
		q.Done("a")

		// The gate's own retry still runs at its deadline (2s after failure 2).
		advance(2*time.Second - time.Nanosecond)
		mustBeEmpty(t, q, "before the second retry time")
		advance(time.Nanosecond)
		if it := mustGet(t, q, "a"); !slices.Equal(kinds(it), []string{"retry"}) || it.Failures != 2 {
			t.Fatalf("second retry failures %d reasons %v, want 2 [retry]", it.Failures, kinds(it))
		}
	})
}

// Kills: Get ignoring holds; WaitIdle returning while a key is processing.
func TestQueueHoldBlocksGetAndWaitIdleWaitsInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.Add("a", LaneHot, hot("event"))
		q.Add("b", LaneHot, hot("event"))
		mustGet(t, q, "a")
		q.Hold("reload")
		q.Hold("fs-pressure")
		mustBeEmpty(t, q, "while held")

		idle := make(chan error, 1)
		go func() { idle <- q.WaitIdle(context.Background()) }()
		synctest.Wait()
		select {
		case err := <-idle:
			t.Fatalf("WaitIdle returned %v while a was processing", err)
		default:
		}
		q.Done("a")
		synctest.Wait()
		select {
		case err := <-idle:
			if err != nil {
				t.Fatalf("WaitIdle = %v, want nil", err)
			}
		default:
			t.Fatal("WaitIdle still blocked after the in-flight key finished")
		}

		q.Release("reload")
		mustBeEmpty(t, q, "with one hold left")
		q.Release("fs-pressure")
		mustGet(t, q, "b")
	})
}

// Kills: Release without a broadcast (a Get blocked across the hold never
// wakes).
func TestQueueReleaseWakesBlockedGet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		q.Hold("reload")
		got := make(chan string, 1)
		go func() {
			if it, ok := q.Get(ctx); ok {
				got <- it.Key
			}
		}()
		synctest.Wait()
		q.Add("a", LaneHot, hot("event"))
		synctest.Wait()
		select {
		case k := <-got:
			t.Fatalf("Get handed out %q while held", k)
		default:
		}
		q.Release("reload")
		synctest.Wait()
		select {
		case k := <-got:
			if k != "a" {
				t.Fatalf("Get = %q, want a", k)
			}
		default:
			t.Fatal("Get blocked across the hold still blocked after Release")
		}
	})
}

// Kills: WaitIdle ignoring its context.
func TestQueueWaitIdleHonorsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.Add("a", LaneHot, hot("event"))
		mustGet(t, q, "a")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		if err := q.WaitIdle(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitIdle = %v, want DeadlineExceeded", err)
		}
		if waited := time.Since(start); waited != 30*time.Second {
			t.Fatalf("WaitIdle returned after %v, want the 30s deadline", waited)
		}
		q.Done("a")
	})
}

// Kills: the queue drained on shutdown (queued work started).
func TestQueueShutDownStopsAdmissionAndNeverStartsQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.Add("a", LaneHot, hot("event"))
		q.Add("b", LaneHot, hot("event"))
		mustGet(t, q, "a")
		q.AddAfter("c", time.Second, hot("requeue-after"))
		q.ShutDown()

		if seq, ok := q.Add("d", LaneHot, hot("event")); ok || seq != 0 {
			t.Fatalf("Add after ShutDown = (%d, %v), want (0, false)", seq, ok)
		}
		q.AddAfter("d", time.Second, hot("requeue-after"))
		q.AddRateLimited("d", hot("retry"))
		if it, ok := q.Get(context.Background()); ok {
			t.Fatalf("Get after ShutDown handed out %q, want false", it.Key)
		}
		advance(2 * time.Second)
		if s := q.Stats(); s.Depth[LaneHot] != 1 || s.Timers != 0 || s.Dropped != 3 || s.Processing != 1 {
			t.Fatalf("stats depth=%v timers=%d dropped=%d processing=%d, want b still queued, no timers, 3 dropped, a in flight",
				s.Depth, s.Timers, s.Dropped, s.Processing)
		}
		q.Done("a")
		if err := q.WaitIdle(context.Background()); err != nil {
			t.Fatalf("WaitIdle after the in-flight Done = %v", err)
		}
	})
}

// Kills: blocked Get calls not woken by ShutDown (workers never exit).
func TestQueueShutDownReleasesBlockedGetters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		const getters = 4
		returned := make(chan bool, getters)
		for range getters {
			go func() {
				_, ok := q.Get(ctx)
				returned <- ok
			}()
		}
		synctest.Wait()
		q.ShutDown()
		synctest.Wait()
		for i := range getters {
			select {
			case ok := <-returned:
				if ok {
					t.Fatal("blocked Get returned an item after ShutDown")
				}
			default:
				t.Fatalf("%d of %d blocked Get calls still blocked after ShutDown", getters-i, getters)
			}
		}
	})
}

// Kills: depth or oldest-age bookkeeping wrong (the oldest hot item is a
// promoted key at the tail, not the head); key, in-flight age, dirty,
// deferred, timer, add, drop or hold counts wrong.
func TestQueueStatsDepthOldestHoldsDeferred(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newTestQueue(Config[string]{})
		q.Add("x", LaneHot, hot("event"))
		tx := time.Now()
		mustGet(t, q, "x")
		advance(time.Second)
		q.Add("x", LaneHot, hot("event")) // dirty
		t0 := time.Now()
		q.Add("p", LaneResync, hot("resync"))
		q.Add("r", LaneResync, hot("resync"))
		advance(time.Second)
		q.Add("h", LaneHot, hot("event"))
		q.Add("p", LaneHot, hot("event")) // promoted: behind h, but added at t0
		q.AddRateLimited("f", hot("retry"))
		q.Add("f", LaneHot, hot("event")) // deferred
		q.AddAfter("t", time.Minute, hot("requeue-after"))
		q.Hold("reload")
		q.Hold("fs-pressure")

		s := q.Stats()
		if s.Depth != [laneCount]int{2, 1} || s.Processing != 1 || s.Dirty != 1 || s.Deferred != 1 || s.Timers != 2 {
			t.Fatalf("depth=%v processing=%d dirty=%d deferred=%d timers=%d, want [2 1] 1 1 1 2",
				s.Depth, s.Processing, s.Dirty, s.Deferred, s.Timers)
		}
		if s.Keys != 6 || !s.OldestInFlight.Equal(tx) {
			t.Fatalf("keys=%d oldest in flight %v, want 6 and %v", s.Keys, s.OldestInFlight, tx)
		}
		if !s.Oldest[LaneHot].Equal(t0) || !s.Oldest[LaneResync].Equal(t0) {
			t.Fatalf("oldest = %v, want both lanes at %v", s.Oldest, t0)
		}
		wantAdds := map[string]uint64{"resync": 2, "event": 5, "retry": 1, "requeue-after": 1}
		if len(s.Adds) != len(wantAdds) {
			t.Fatalf("adds = %v, want %v", s.Adds, wantAdds)
		}
		for k, n := range wantAdds {
			if s.Adds[k] != n {
				t.Fatalf("adds = %v, want %v", s.Adds, wantAdds)
			}
		}
		if !slices.Equal(s.Holds, []string{"fs-pressure", "reload"}) {
			t.Fatalf("holds = %v, want sorted [fs-pressure reload]", s.Holds)
		}

		q.ShutDown()
		q.Add("x", LaneHot, hot("event"))
		if s := q.Stats(); s.Dropped != 1 || s.Timers != 0 || s.Adds["event"] != 5 {
			t.Fatalf("after ShutDown dropped=%d timers=%d event adds=%d, want 1 0 5", s.Dropped, s.Timers, s.Adds["event"])
		}
		if s := New(Config[string]{}).Stats(); s.Oldest != [laneCount]time.Time{} {
			t.Fatalf("empty queue oldest = %v, want zero", s.Oldest)
		}
	})
}
