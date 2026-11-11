package events

import (
	"fmt"
	"math/rand"
	"testing"
)

// These tests pin the membership properties of a long-lived merged stream
// (Hardening P0-2): cities join, leave and come back while a consumer watches,
// and the consumer reconnects with its composite cursor across those changes.
// Every case is event-driven: tests wait for watcher attach/close facts and for
// delivered events, never for wall-clock time.

func assertContiguous(t *testing.T, got map[string][]uint64, want map[string]uint64) {
	t.Helper()
	for city, n := range want {
		seqs := got[city]
		if uint64(len(seqs)) != n {
			t.Fatalf("city %s delivered %v, want seqs 1..%d exactly once", city, seqs, n)
		}
		for i, seq := range seqs {
			if seq != uint64(i+1) {
				t.Fatalf("city %s delivered %v, want seqs 1..%d with no gap or duplicate", city, seqs, n)
			}
		}
	}
}

// A city removed mid-watch and re-added resumes from the last seq the consumer
// delivered for it: events it wrote while away are delivered, none twice.
func TestMembershipRemoveAndReaddResumesWithoutGapOrDuplicate(t *testing.T) {
	a, b := newSignalingProvider(), newSignalingProvider()
	c := newMembershipConsumer(t, map[string]Provider{"city-a": a, "city-b": b}, "")

	b.Record(Event{Type: SessionWoke, Actor: "b1"})
	c.next()
	c.sync(map[string]Provider{"city-a": a})
	waitClosed(t, b.lastWatcherClosed(), "detached city-b watcher")
	if got := c.cursor.Format(); got != "city-a:0" {
		t.Fatalf("composite cursor after city-b left = %q, want %q (a departed city is pruned)", got, "city-a:0")
	}
	b.Record(Event{Type: SessionWoke, Actor: "b2"})
	b.Record(Event{Type: SessionWoke, Actor: "b3"})

	c.sync(map[string]Provider{"city-a": a, "city-b": b})
	c.next()
	c.next()
	assertContiguous(t, c.got, map[string]uint64{"city-b": 3})
	if got := c.cursor.Format(); got != "city-a:0,city-b:3" {
		t.Fatalf("composite cursor = %q, want %q", got, "city-a:0,city-b:3")
	}
}

// A city that joins mid-watch attaches at the client's resume cursor when the
// cursor names it, so its events since that position are delivered.
func TestMembershipNewCityAttachesAtResumeCursor(t *testing.T) {
	a := NewFake()
	c := newMembershipConsumer(t, map[string]Provider{"city-a": a}, "city-a:0,city-b:1")

	b := NewFake()
	for i := 0; i < 3; i++ {
		b.Record(Event{Type: SessionWoke, Actor: fmt.Sprintf("b%d", i+1)})
	}
	c.sync(map[string]Provider{"city-a": a, "city-b": b})
	if te := c.next(); te.City != "city-b" || te.Seq != 2 {
		t.Fatalf("Next() = city %s seq %d, want city-b seq 2 (resume cursor city-b:1)", te.City, te.Seq)
	}
	if te := c.next(); te.City != "city-b" || te.Seq != 3 {
		t.Fatalf("Next() = city %s seq %d, want city-b seq 3", te.City, te.Seq)
	}
}

// Reconnecting with the last composite id after the membership changed while
// the client was away delivers every event the client missed for the cities
// in its id, exactly once.
func TestMembershipReconnectAcrossMembershipChangeHasNoGapOrDuplicate(t *testing.T) {
	a, b := NewFake(), newSignalingProvider()
	first := newMembershipConsumer(t, map[string]Provider{"city-a": a, "city-b": b}, "")
	a.Record(Event{Type: SessionWoke, Actor: "a1"})
	b.Record(Event{Type: SessionWoke, Actor: "b1"})
	first.next()
	first.next()
	lastID := first.cursor.Format()
	_ = first.w.Close()

	// While the client is away: city-c joins (with history the client never
	// asked for), and the cities in its id keep writing.
	a.Record(Event{Type: SessionWoke, Actor: "a2"})
	b.Record(Event{Type: SessionWoke, Actor: "b2"})
	cc := NewFake()
	cc.Record(Event{Type: SessionWoke, Actor: "c-before-join"})

	second := newMembershipConsumer(t, map[string]Provider{"city-a": a, "city-b": b, "city-c": cc}, lastID)
	second.got = first.got
	second.next()
	second.next()
	cc.Record(Event{Type: SessionWoke, Actor: "c2"})
	if te := second.next(); te.City != "city-c" || te.Seq != 2 {
		t.Fatalf("Next() = city %s seq %d, want city-c seq 2 (a new city starts at its head)", te.City, te.Seq)
	}
	delete(second.got, "city-c")
	assertContiguous(t, second.got, map[string]uint64{"city-a": 2, "city-b": 2})
}

// A city removed and re-added before the consumer drained its buffered events
// resumes from the consumer's last delivered position; the replayed events the
// consumer already holds are dropped, so nothing is delivered twice.
func TestMembershipQuickReaddDropsReplayedDuplicates(t *testing.T) {
	b := newSignalingProvider()
	c := newMembershipConsumer(t, map[string]Provider{"city-b": b}, "")
	b.Record(Event{Type: SessionWoke, Actor: "b1"})
	b.Record(Event{Type: SessionWoke, Actor: "b2"})
	// Drain b1 only, then cycle the city: the re-attached watcher replays b2,
	// and the first watcher may already have queued it too.
	c.next()
	c.sync(map[string]Provider{})
	waitClosed(t, b.lastWatcherClosed(), "detached city-b watcher")
	c.sync(map[string]Provider{"city-b": b})
	b.Record(Event{Type: SessionWoke, Actor: "b3"})
	c.next()
	c.next()
	assertContiguous(t, c.got, map[string]uint64{"city-b": 3})
}

// Randomized membership churn: cities join, leave and return in a seeded
// random order while every city keeps writing. Every city's delivered seqs are
// exactly 1..N — no gap, no duplicate — however the membership moved.
func TestMembershipChurnPropertyNoGapNoDuplicate(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			names := []string{"city-a", "city-b", "city-c"}
			provs := map[string]*signalingProvider{}
			present := map[string]bool{}
			all := map[string]Provider{}
			for _, n := range names {
				provs[n] = newSignalingProvider()
				all[n] = provs[n]
				present[n] = true
			}
			c := newMembershipConsumer(t, all, "")
			recorded := map[string]uint64{}
			delivered := 0
			total := 0
			current := func() map[string]Provider {
				out := map[string]Provider{}
				for n, ok := range present {
					if ok {
						out[n] = provs[n]
					}
				}
				return out
			}
			drainPresent := func() {
				for delivered < total {
					pending := 0
					for n := range recorded {
						if present[n] {
							pending += int(recorded[n]) - len(c.got[n])
						}
					}
					if pending == 0 {
						return
					}
					c.next()
					delivered++
				}
			}
			for step := 0; step < 40; step++ {
				n := names[rng.Intn(len(names))]
				switch op := rng.Intn(3); {
				case op == 0 && present[n]:
					closed := provs[n].lastWatcherClosed()
					present[n] = false
					c.sync(current())
					waitClosed(t, closed, "detached "+n+" watcher")
				case op == 1 && !present[n]:
					present[n] = true
					c.sync(current())
				default:
					provs[n].Record(Event{Type: SessionWoke, Actor: n})
					recorded[n]++
					total++
				}
				drainPresent()
			}
			for _, n := range names {
				present[n] = true
			}
			c.sync(current())
			for len(c.got["city-a"])+len(c.got["city-b"])+len(c.got["city-c"]) < total {
				c.next()
			}
			assertContiguous(t, c.got, recorded)
		})
	}
}
