package storebindingtest

// Startup-turn conformance: a startup turn is the first turn a runtime handed
// back from a start, queued bound to the launch that produced it
// (nudgequeue.StartupTurnReference). The claim judges a session's startup
// turns against the launch it runs now (ClaimTarget.LiveLaunch): the live
// launch's turn goes out alone and before the session's other items, a turn
// whose launch is gone is dead-lettered and never delivered, and a turn the
// claim cannot vouch for is held, neither delivered nor dropped.

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/storebinding"
)

const (
	// startupSession is the session the startup cases claim for.
	startupSession = "session-live"
	// startupLaunchOne and startupLaunchTwo are two launches of it.
	startupLaunchOne = "launch-one"
	startupLaunchTwo = "launch-two"
	// startupReaderWait bounds a reader that has to reach the queue from
	// inside a claim's ask: a claim that asks inside its exclusive section
	// blocks that reach, and the case fails instead of hanging.
	startupReaderWait = 5 * time.Second
)

// startupTurn is a startup turn of launch for startupSession's key.
func startupTurn(id, launch string, created time.Time) nudgequeue.Item {
	return nudgequeue.Item{
		ID:           id,
		Agent:        ConformanceNudgeAgent,
		SessionID:    startupSession,
		Source:       nudgequeue.SourceSubmit,
		Message:      "begin your work",
		Reference:    nudgequeue.StartupTurnReference(launch),
		CreatedAt:    created,
		DeliverAfter: created,
		ExpiresAt:    created.Add(24 * time.Hour),
	}
}

// launchTwosTurn is t-2, launch two's startup turn, queued a second after
// now and due at now: a later start's turn.
func launchTwosTurn(now time.Time) nudgequeue.Item {
	item := startupTurn("t-2", startupLaunchTwo, now.Add(time.Second))
	item.DeliverAfter = now
	return item
}

// startupReminder is an ordinary nudge for startupSession.
func startupReminder(id string, created time.Time) nudgequeue.Item {
	return nudgeItem(id, startupSession, created)
}

// launchReader answers launch, known.
func launchReader(launch string) func() (string, bool) {
	return func() (string, bool) { return launch, true }
}

// startupTarget is startupSession's claim target with reader.
func startupTarget(reader func() (string, bool)) storebinding.ClaimTarget {
	return storebinding.ClaimTarget{
		QueueKeys:  []string{ConformanceNudgeAgent},
		SessionID:  startupSession,
		LiveLaunch: reader,
	}
}

func enqueueDeferred(r Runner, queue storebinding.NudgeQueue, items ...nudgequeue.Item) {
	r.Helper()
	for _, item := range items {
		if err := queue.EnqueueDeferred(item); err != nil {
			r.Fatalf("EnqueueDeferred(%s): %v", item.ID, err)
		}
	}
}

func enqueueItems(r Runner, queue storebinding.NudgeQueue, items ...nudgequeue.Item) {
	r.Helper()
	for _, item := range items {
		if err := queue.Enqueue(item); err != nil {
			r.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
}

// claimExactly claims for target at now with ClaimDue and requires exactly
// ids, in that order.
func claimExactly(r Runner, queue storebinding.NudgeQueue, target storebinding.ClaimTarget, now time.Time, why string, ids ...string) {
	r.Helper()
	claimed, err := queue.ClaimDue(target, now)
	if err != nil {
		r.Fatalf("ClaimDue (%s): %v", why, err)
	}
	if fmt.Sprint(nudgeIDs(claimed)) != fmt.Sprint(append([]string{}, ids...)) {
		r.Fatalf("ClaimDue (%s) = %v, want %v", why, nudgeIDs(claimed), append([]string{}, ids...))
	}
}

// ackAll acks ids as delivered.
func ackAll(r Runner, queue storebinding.NudgeQueue, ids ...string) {
	r.Helper()
	if err := queue.Ack(ids, "injected", "", "commit"); err != nil {
		r.Fatalf("Ack(%v): %v", ids, err)
	}
}

// requireDead requires id in the dead bucket with reason as its LastError.
func requireDead(r Runner, queue storebinding.NudgeQueue, now time.Time, id, reason string) {
	r.Helper()
	_, _, dead, err := queue.ListForAgent(ConformanceNudgeAgent, now)
	if err != nil {
		r.Fatalf("ListForAgent: %v", err)
	}
	for _, item := range dead {
		if item.ID == id {
			if item.LastError != reason {
				r.Fatalf("%s is dead with %q, want %q", id, item.LastError, reason)
			}
			return
		}
	}
	r.Fatalf("%s is not dead (dead = %v), want it dead with %q", id, nudgeIDs(dead), reason)
}

// requireNothingDead requires an empty dead bucket.
func requireNothingDead(r Runner, queue storebinding.NudgeQueue, now time.Time) {
	r.Helper()
	_, _, dead, err := queue.ListForAgent(ConformanceNudgeAgent, now)
	if err != nil {
		r.Fatalf("ListForAgent: %v", err)
	}
	if len(dead) != 0 {
		r.Fatalf("dead = %v, want nothing dropped", nudgeIDs(dead))
	}
}

// requirePending requires ids among the pending items.
func requirePending(r Runner, queue storebinding.NudgeQueue, now time.Time, ids ...string) {
	r.Helper()
	pending, _, _, err := queue.ListForAgent(ConformanceNudgeAgent, now)
	if err != nil {
		r.Fatalf("ListForAgent: %v", err)
	}
	for _, id := range ids {
		if !containsID(nudgeIDs(pending), id) {
			r.Fatalf("pending = %v, want %s still pending", nudgeIDs(pending), id)
		}
	}
}

// withinReaderWait runs fn in a goroutine and reports whether it finished
// before startupReaderWait.
func withinReaderWait(fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(startupReaderWait):
		return false
	}
}

// replacedLaunch is the shared setup of the replaced-launch cases: T1 of
// launch one is claimed, launch two's T2 is queued, and settle returns T1
// from its claim the way the case names.
func runReplacedLaunchCase(r Runner, suite NudgesSuite, settle func(Runner, storebinding.NudgeQueue, time.Time) time.Time) {
	queue := mustQueue(r, suite)
	now := time.Now().UTC()
	enqueueDeferred(r, queue, startupTurn("t-1", startupLaunchOne, now))
	claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now, "launch one claims its turn", "t-1")
	enqueueDeferred(r, queue, launchTwosTurn(now))
	at := settle(r, queue, now)
	claimExactly(r, queue, startupTarget(launchReader(startupLaunchTwo)), at, "launch two claims", "t-2")
	requireDead(r, queue, at, "t-1", nudgequeue.StaleLaunchReason)
	ackAll(r, queue, "t-2")
	for _, launch := range []string{startupLaunchTwo, startupLaunchOne} {
		claimExactly(r, queue, startupTarget(launchReader(launch)), at.Add(time.Minute), "after the live turn's ack, with "+launch)
	}
	requireDead(r, queue, at.Add(time.Minute), "t-1", nudgequeue.StaleLaunchReason)
}

// runNudgeStartupTests covers the startup turn: bound to its launch,
// delivered first and alone, and dropped only on proof its launch is gone.
func runNudgeStartupTests(r Runner, suite NudgesSuite) {
	r.Helper()

	r.Run("ClaimTakesTheLiveLaunchsTurnFirst", func(r Runner) {
		for _, verb := range []string{"ClaimDue", "ClaimBatch"} {
			queue := mustQueue(r, suite)
			now := time.Now().UTC()
			enqueueItems(r, queue, startupReminder("n-reminder", now.Add(-time.Minute)))
			enqueueDeferred(r, queue, startupTurn("t-1", startupLaunchOne, now))
			target := startupTarget(launchReader(startupLaunchOne))
			claim := queue.ClaimDue
			if verb == "ClaimBatch" {
				claim = queue.ClaimBatch
			}
			claimed, err := claim(target, now)
			if err != nil {
				r.Fatalf("%s: %v", verb, err)
			}
			if fmt.Sprint(nudgeIDs(claimed)) != "[t-1]" {
				r.Fatalf("%s = %v, want the live launch's turn alone, ahead of the reminder due before it", verb, nudgeIDs(claimed))
			}
			if verb == "ClaimBatch" && claimed[0].DeliveryBatch == "" {
				r.Fatalf("ClaimBatch claimed the turn with no delivery batch")
			}
			ackAll(r, queue, "t-1")
			claimed, err = claim(target, now)
			if err != nil || fmt.Sprint(nudgeIDs(claimed)) != "[n-reminder]" {
				r.Fatalf("%s after the turn's ack = %v, %v; want the reminder", verb, nudgeIDs(claimed), err)
			}
		}
	})

	r.Run("ATurnPinnedWithOtherItemsIsClaimedAloneUnderAKeyOfItsOwn", func(r Runner) {
		// Only an older gc's claim pins a turn with other items: it claimed
		// them together and crashed, or failed its repin.
		const shared = "nudge-batch-shared"
		for _, verb := range []string{"ClaimDue", "ClaimBatch"} {
			queue := mustQueue(r, suite)
			now := time.Now().UTC()
			reminder := startupReminder("n-reminder", now.Add(-time.Minute))
			reminder.DeliveryBatch = shared
			turn := startupTurn("t-1", startupLaunchOne, now)
			turn.DeliveryBatch = shared
			enqueueItems(r, queue, reminder)
			enqueueDeferred(r, queue, turn)
			target := startupTarget(launchReader(startupLaunchOne))
			claim := queue.ClaimDue
			if verb == "ClaimBatch" {
				claim = queue.ClaimBatch
			}
			claimed, err := claim(target, now)
			if err != nil || fmt.Sprint(nudgeIDs(claimed)) != "[t-1]" {
				r.Fatalf("%s = %v, %v; want the live launch's turn alone", verb, nudgeIDs(claimed), err)
			}
			if claimed[0].DeliveryBatch == shared || (verb == "ClaimBatch" && claimed[0].DeliveryBatch == "") {
				r.Fatalf("%s claimed the turn under batch %q; want a key of its own, never the shared %q", verb, claimed[0].DeliveryBatch, shared)
			}
			ackAll(r, queue, "t-1")
			claimed, err = claim(target, now)
			if err != nil || fmt.Sprint(nudgeIDs(claimed)) != "[n-reminder]" {
				r.Fatalf("%s after the turn's ack = %v, %v; want the reminder", verb, nudgeIDs(claimed), err)
			}
		}
	})

	r.Run("AReleasedTurnOfAReplacedLaunchIsDroppedNotRedelivered", func(r Runner) {
		runReplacedLaunchCase(r, suite, func(r Runner, queue storebinding.NudgeQueue, now time.Time) time.Time {
			if err := queue.ReleaseClaims([]string{"t-1"}, time.Time{}); err != nil {
				r.Fatalf("ReleaseClaims: %v", err)
			}
			return now
		})
	})

	r.Run("AFailedTurnOfAReplacedLaunchIsDroppedNotRedelivered", func(r Runner) {
		runReplacedLaunchCase(r, suite, func(r Runner, queue storebinding.NudgeQueue, now time.Time) time.Time {
			if _, err := queue.RecordFailure([]string{"t-1"}, errors.New("transient delivery failure"), now); err != nil {
				r.Fatalf("RecordFailure: %v", err)
			}
			return now
		})
	})

	r.Run("ALapsedTurnOfAReplacedLaunchIsDroppedNotRedelivered", func(r Runner) {
		runReplacedLaunchCase(r, suite, func(Runner, storebinding.NudgeQueue, time.Time) time.Time {
			return time.Now().UTC().Add(3 * time.Minute)
		})
	})

	r.Run("AnInFlightTurnOfAReplacedLaunchDiesAtTheReplacementsClaim", func(r Runner) {
		for _, late := range []string{"Ack", "ReleaseClaims", "RecordFailure"} {
			queue := mustQueue(r, suite)
			now := time.Now().UTC()
			enqueueDeferred(r, queue, startupTurn("t-1", startupLaunchOne, now))
			claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now, late+": launch one claims its turn", "t-1")
			enqueueDeferred(r, queue, launchTwosTurn(now))
			claimExactly(r, queue, startupTarget(launchReader(startupLaunchTwo)), now, late+": launch two claims while t-1 is in flight", "t-2")
			requireDead(r, queue, now, "t-1", nudgequeue.StaleLaunchReason)
			var err error
			switch late {
			case "Ack":
				err = queue.Ack([]string{"t-1"}, "injected", "", "commit")
			case "ReleaseClaims":
				err = queue.ReleaseClaims([]string{"t-1"}, time.Time{})
			case "RecordFailure":
				_, err = queue.RecordFailure([]string{"t-1"}, errors.New("transient delivery failure"), now)
			}
			if err != nil {
				r.Fatalf("%s(t-1): %v", late, err)
			}
			requireDead(r, queue, now, "t-1", nudgequeue.StaleLaunchReason)
			ackAll(r, queue, "t-2")
			claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now.Add(time.Minute), late+": after both")
		}
	})

	r.Run("AnUnknownLiveLaunchHoldsTheSession", func(r Runner) {
		readers := map[string]func() (string, bool){
			"an unreadable launch": func() (string, bool) { return "", false },
			"no reader":            nil,
		}
		for name, reader := range readers {
			queue := mustQueue(r, suite)
			now := time.Now().UTC()
			enqueueItems(r, queue, startupReminder("n-reminder", now.Add(-time.Minute)))
			enqueueDeferred(r, queue, startupTurn("t-1", startupLaunchOne, now))
			for _, claim := range []func(storebinding.ClaimTarget, time.Time) ([]nudgequeue.Item, error){queue.ClaimDue, queue.ClaimBatch} {
				claimed, err := claim(startupTarget(reader), now)
				if err != nil {
					r.Fatalf("%s: claim: %v", name, err)
				}
				if len(claimed) != 0 {
					r.Fatalf("%s: claim = %v, want nothing: an unvouched turn holds its session", name, nudgeIDs(claimed))
				}
			}
			requireNothingDead(r, queue, now)
			requirePending(r, queue, now, "t-1", "n-reminder")
			claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now, name+": then the known launch", "t-1")
		}
	})

	r.Run("ATurnFirstSeenAfterTheReadIsHeldNotDropped", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		enqueueDeferred(r, queue, startupTurn("t-1", startupLaunchOne, now))
		reader := func() (string, bool) {
			reached := withinReaderWait(func() {
				if err := queue.EnqueueDeferred(launchTwosTurn(now)); err != nil {
					r.Errorf("EnqueueDeferred(t-2) during the ask: %v", err)
				}
			})
			if !reached {
				r.Errorf("the claim asked LiveLaunch inside its exclusive section: a queue write from the reader did not finish")
			}
			return startupLaunchOne, true
		}
		claimExactly(r, queue, startupTarget(reader), now, "a turn queued during the ask")
		requireNothingDead(r, queue, now)
		requirePending(r, queue, now, "t-1", "t-2")
		claimExactly(r, queue, startupTarget(launchReader(startupLaunchTwo)), now, "then launch two", "t-2")
		requireDead(r, queue, now, "t-1", nudgequeue.StaleLaunchReason)
	})

	r.Run("LiveLaunchIsAskedOnceAndOnlyWhenATurnIsDue", func(r Runner) {
		var mu sync.Mutex
		asks := 0
		reader := func() (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			asks++
			return startupLaunchOne, true
		}
		count := func() int {
			mu.Lock()
			defer mu.Unlock()
			n := asks
			asks = 0
			return n
		}
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		enqueueItems(r, queue, startupReminder("n-reminder", now))
		claimExactly(r, queue, startupTarget(reader), now, "no turn", "n-reminder")
		later := startupTurn("t-later", startupLaunchOne, now)
		later.DeliverAfter = now.Add(time.Hour)
		enqueueDeferred(r, queue, later)
		if _, err := queue.ClaimBatch(startupTarget(reader), now); err != nil {
			r.Fatalf("ClaimBatch: %v", err)
		}
		claimExactly(r, queue, startupTarget(reader), now, "a turn not yet due")
		if n := count(); n != 0 {
			r.Fatalf("LiveLaunch asked %d times with no due turn, want 0", n)
		}
		claimExactly(r, queue, startupTarget(reader), now.Add(time.Hour), "a due turn", "t-later")
		if n := count(); n != 1 {
			r.Fatalf("LiveLaunch asked %d times by one claim of a due turn, want 1", n)
		}
	})

	r.Run("TheNewestTurnOfOneLaunchWins", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		enqueueDeferred(r, queue,
			startupTurn("t-0", "launch-zero", now.Add(-time.Second)),
			startupTurn("t-1", startupLaunchOne, now),
			startupTurn("t-2", startupLaunchOne, now.Add(time.Second)))
		claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now.Add(time.Second), "two turns of one launch", "t-2")
		requireDead(r, queue, now.Add(time.Second), "t-1", nudgequeue.SupersededReason)
		requireDead(r, queue, now.Add(time.Second), "t-0", nudgequeue.StaleLaunchReason)
	})

	r.Run("TheBarrierReleases", func(r Runner) {
		for _, release := range []string{"Ack", "a permanent failure", "a stale drop", "expiry"} {
			queue := mustQueue(r, suite)
			now := time.Now().UTC()
			turn := startupTurn("t-1", startupLaunchOne, now)
			turn.DeliverAfter = now.Add(time.Hour)
			if release == "expiry" {
				turn.ExpiresAt = now.Add(30 * time.Minute)
			}
			enqueueItems(r, queue, startupReminder("n-reminder", now))
			enqueueDeferred(r, queue, turn)
			claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now, release+": a turn not yet due holds the reminder")
			due := now.Add(time.Hour)
			switch release {
			case "Ack":
				claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), due, release+": the turn", "t-1")
				ackAll(r, queue, "t-1")
				claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), due, release+": then the reminder", "n-reminder")
			case "a permanent failure":
				if _, err := queue.RecordFailure([]string{"t-1"}, storebinding.ErrNudgeSessionFenceMismatch, now); err != nil {
					r.Fatalf("RecordFailure: %v", err)
				}
				claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now, release+": then the reminder", "n-reminder")
			case "a stale drop":
				claimExactly(r, queue, startupTarget(launchReader(startupLaunchTwo)), due, release+": the same claim delivers the reminder", "n-reminder")
				requireDead(r, queue, due, "t-1", nudgequeue.StaleLaunchReason)
			case "expiry":
				claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now.Add(31*time.Minute), release+": then the reminder", "n-reminder")
			}
		}
	})

	r.Run("TheInitialMessageWaitsBehindItsTurnAndOutlivesIt", func(r Runner) {
		message := func(id string, turn nudgequeue.Item) nudgequeue.Item {
			item := turn
			item.ID, item.Message, item.Reference = id, "the initial message", nil
			item.CreatedAt = turn.CreatedAt.Add(time.Nanosecond)
			return item
		}
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		t1 := startupTurn("t-1", startupLaunchOne, now)
		enqueueDeferred(r, queue, t1, message("m-1", t1))
		claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now, "the turn first", "t-1")
		ackAll(r, queue, "t-1")
		claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now, "then its message", "m-1")
		ackAll(r, queue, "m-1")

		queue = mustQueue(r, suite)
		enqueueDeferred(r, queue, t1, message("m-1", t1))
		t2 := launchTwosTurn(now)
		enqueueDeferred(r, queue, t2)
		claimExactly(r, queue, startupTarget(launchReader(startupLaunchTwo)), now.Add(time.Second), "the replacement's turn", "t-2")
		requireDead(r, queue, now.Add(time.Second), "t-1", nudgequeue.StaleLaunchReason)
		ackAll(r, queue, "t-2")
		claimExactly(r, queue, startupTarget(launchReader(startupLaunchTwo)), now.Add(time.Second), "the dead turn's message, delivered", "m-1")
	})

	r.Run("AnotherSessionsTurnIsNeitherClaimedNorDroppedNorAdopted", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		foreign := startupTurn("t-foreign", startupLaunchOne, now)
		foreign.SessionID = "session-gone"
		adoptable := nudgeItem("n-adoptable", "session-gone", now)
		enqueueDeferred(r, queue, foreign)
		enqueueItems(r, queue, startupReminder("n-own", now), adoptable)
		target := startupTarget(launchReader(startupLaunchTwo))
		claimExactly(r, queue, target, now, "a strict fence", "n-own")
		target.LiveFenceSessions = func() (map[string]struct{}, bool) {
			return map[string]struct{}{startupSession: {}}, true
		}
		claimExactly(r, queue, target, now, "a census that adopts the gone session", "n-adoptable")
		requireNothingDead(r, queue, now)
		requirePending(r, queue, now, "t-foreign")
	})

	r.Run("DueKeysHonoursTheBarrier", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		keyed := func(item nudgequeue.Item, agent string) nudgequeue.Item {
			item.Agent, item.SessionID = agent, ""
			return item
		}
		notDue := keyed(startupTurn("t-notdue", startupLaunchOne, now), "alpha/notdue")
		notDue.DeliverAfter = now.Add(time.Hour)
		enqueueDeferred(r, queue,
			notDue,
			keyed(startupTurn("t-due", startupLaunchOne, now), "alpha/due"),
			keyed(startupTurn("t-leased", startupLaunchOne, now), "alpha/leased"),
			keyed(startupTurn("t-lapsed", startupLaunchOne, now.Add(-time.Hour)), "alpha/lapsed"))
		enqueueItems(r, queue,
			keyed(nudgeItem("n-notdue", "", now), "alpha/notdue"),
			keyed(nudgeItem("n-leased", "", now), "alpha/leased"),
			keyed(nudgeItem("n-lapsed", "", now), "alpha/lapsed"),
			keyed(nudgeItem("n-other", "", now), "alpha/other"))
		claim := func(key string, at time.Time, want string) {
			r.Helper()
			claimed, err := queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{key}, LiveLaunch: launchReader(startupLaunchOne)}, at)
			if err != nil || fmt.Sprint(nudgeIDs(claimed)) != "["+want+"]" {
				r.Fatalf("ClaimDue(%s) = %v, %v; want %s", key, nudgeIDs(claimed), err, want)
			}
		}
		claim("alpha/leased", now, "t-leased")
		claim("alpha/lapsed", now.Add(-time.Hour), "t-lapsed")
		keys, err := queue.DueKeys(now.Add(30 * time.Second))
		if err != nil {
			r.Fatalf("DueKeys: %v", err)
		}
		want := []string{"alpha/due", "alpha/lapsed", "alpha/other"}
		if fmt.Sprint(keys) != fmt.Sprint(want) {
			r.Fatalf("DueKeys = %v, want %v: a key held behind a turn not yet due or in flight is not due; a due turn or a lapsed lease is", keys, want)
		}
	})

	r.Run("EnqueueDeferredSupersedesNothing", func(r Runner) {
		queue := mustQueue(r, suite)
		now := time.Now().UTC()
		enqueueDeferred(r, queue, startupTurn("t-1", startupLaunchOne, now), launchTwosTurn(now))
		requirePending(r, queue, now, "t-1", "t-2")
		requireNothingDead(r, queue, now)
	})

	r.Run("ConcurrentClaimsNeverDeliverAStaleTurnTwice", func(r Runner) {
		for _, late := range []string{"Ack", "ReleaseClaims"} {
			queue := mustQueue(r, suite)
			now := time.Now().UTC()
			enqueueDeferred(r, queue, startupTurn("t-1", startupLaunchOne, now))
			reader := func() (string, bool) {
				reached := withinReaderWait(func() {
					claimed, err := queue.ClaimDue(startupTarget(launchReader(startupLaunchOne)), now)
					if err != nil || fmt.Sprint(nudgeIDs(claimed)) != "[t-1]" {
						r.Errorf("%s: the slow claimant's ClaimDue = %v, %v; want t-1", late, nudgeIDs(claimed), err)
					}
					if err := queue.EnqueueDeferred(launchTwosTurn(now)); err != nil {
						r.Errorf("%s: EnqueueDeferred(t-2): %v", late, err)
					}
				})
				if !reached {
					r.Errorf("%s: the claim asked LiveLaunch inside its exclusive section", late)
				}
				return startupLaunchTwo, true
			}
			claimExactly(r, queue, startupTarget(reader), now, late+": launch two's claim races the slow claimant")
			requireDead(r, queue, now, "t-1", nudgequeue.StaleLaunchReason)
			var err error
			if late == "Ack" {
				err = queue.Ack([]string{"t-1"}, "injected", "", "commit")
			} else {
				err = queue.ReleaseClaims([]string{"t-1"}, time.Time{})
			}
			if err != nil {
				r.Fatalf("%s(t-1): %v", late, err)
			}
			requireDead(r, queue, now, "t-1", nudgequeue.StaleLaunchReason)
			claimExactly(r, queue, startupTarget(launchReader(startupLaunchTwo)), now, late+": launch two", "t-2")
			ackAll(r, queue, "t-2")
			claimExactly(r, queue, startupTarget(launchReader(startupLaunchOne)), now.Add(time.Minute), late+": nothing more")
		}
	})
}
