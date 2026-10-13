package events

import (
	"context"
	"fmt"
	"testing"
)

// StreamCursor and MuxWatcher.StayOpen let a long-lived merged stream survive
// its city set emptying and refilling, and keep its composite cursor free of
// departed cities. The membership property tests in
// multiplexer_membership_test.go drive them through membershipConsumer, a
// consumer shaped like the supervisor SSE handler.

// membershipConsumer is the consumer side of a merged stream, shaped like the
// supervisor SSE handler: it delivers each event once per city position,
// tracks the composite cursor, and resyncs the watched city set.
type membershipConsumer struct {
	t      *testing.T
	w      *MuxWatcher
	cursor *StreamCursor
	got    map[string][]uint64 // delivered seqs per city, in order
}

func newMembershipConsumer(t *testing.T, providers map[string]Provider, resume string) *membershipConsumer {
	t.Helper()
	m := NewMultiplexer()
	start := ParseCursor(resume)
	if start == nil {
		start = map[string]uint64{}
	}
	for city, p := range providers {
		m.Add(city, p)
		if _, ok := start[city]; !ok {
			seq, err := p.LatestSeq()
			if err != nil {
				t.Fatalf("LatestSeq(%s): %v", city, err)
			}
			start[city] = seq
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w, err := m.Watch(ctx, start)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	w.StayOpen()
	t.Cleanup(func() { _ = w.Close() })
	return &membershipConsumer{t: t, w: w, cursor: NewStreamCursor(start), got: map[string][]uint64{}}
}

// sync reconciles the watched cities with providers the way the supervisor
// stream does: a city with a known position resumes there, a new one starts at
// its head.
func (c *membershipConsumer) sync(providers map[string]Provider) {
	c.t.Helper()
	c.cursor.Retain(providers)
	started, err := c.w.Sync(providers, func(city string, p Provider) (uint64, error) {
		if seq, ok := c.cursor.Resume(city); ok {
			return seq, nil
		}
		return p.LatestSeq()
	})
	if err != nil {
		c.t.Fatalf("Sync: %v", err)
	}
	c.cursor.Attach(started)
}

// next delivers the next event, dropping any the cursor already delivered.
func (c *membershipConsumer) next() TaggedEvent {
	c.t.Helper()
	for {
		te := nextWithin(c.t, c.w)
		if !c.cursor.Advance(te.City, te.Seq) {
			continue
		}
		c.got[te.City] = append(c.got[te.City], te.Seq)
		return te
	}
}

// Detaching every city must not end a stream that stays open: a city that
// starts later is attached and delivered.
func TestMuxWatcherStayOpenSurvivesDetachingEveryCity(t *testing.T) {
	a := newSignalingProvider()
	c := newMembershipConsumer(t, map[string]Provider{"city-a": a}, "")

	c.sync(map[string]Provider{})
	waitClosed(t, a.lastWatcherClosed(), "detached city-a watcher")

	b := NewFake()
	c.sync(map[string]Provider{"city-b": b})
	b.Record(Event{Type: SessionWoke, Actor: "b1"})
	if te := c.next(); te.City != "city-b" || te.Actor != "b1" {
		t.Fatalf("Next() = %+v, want city-b b1 after every city detached", te)
	}
}

// Closing a watcher that stays open still ends Next.
func TestMuxWatcherStayOpenEndsOnClose(t *testing.T) {
	c := newMembershipConsumer(t, map[string]Provider{"city-a": NewFake()}, "")
	if err := c.w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := nextResultWithin(t, c.w); err == nil {
		t.Fatal("Next() after Close error = nil, want an error")
	}
}

// StreamCursor bookkeeping: departed cities leave the composite cursor but are
// remembered (bounded) for an in-stream return.
func TestStreamCursorPrunesAndBoundsDepartedCities(t *testing.T) {
	cur := NewStreamCursor(map[string]uint64{"a": 1, "b": 2})
	if !cur.Advance("b", 3) || cur.Advance("b", 3) || cur.Advance("b", 2) {
		t.Fatal("Advance must accept a new seq once and reject delivered ones")
	}
	cur.Retain(map[string]Provider{"a": NewFake()})
	if got := cur.Format(); got != "a:1" {
		t.Fatalf("Format() = %q, want a:1", got)
	}
	if seq, ok := cur.Resume("b"); !ok || seq != 3 {
		t.Fatalf("Resume(b) = %d,%v, want 3,true", seq, ok)
	}
	// A buffered event from a departed city advances its remembered position
	// without putting it back into the composite cursor.
	if !cur.Advance("b", 4) {
		t.Fatal("Advance(b,4) for a departed city = false, want true")
	}
	if got := cur.Format(); got != "a:1" {
		t.Fatalf("Format() after departed advance = %q, want a:1", got)
	}
	cur.Attach(map[string]uint64{"b": 4})
	if got := cur.Format(); got != "a:1,b:4" {
		t.Fatalf("Format() after return = %q, want a:1,b:4", got)
	}

	for i := 0; i < maxDepartedCities+10; i++ {
		city := fmt.Sprintf("gone-%03d", i)
		cur.Attach(map[string]uint64{city: 1})
		cur.Retain(map[string]Provider{"a": NewFake(), "b": NewFake()})
	}
	if n := cur.departedLen(); n != maxDepartedCities {
		t.Fatalf("departed cities remembered = %d, want the bound %d", n, maxDepartedCities)
	}
	if _, ok := cur.Resume("gone-000"); ok {
		t.Fatal("the oldest departed city is still remembered past the bound")
	}
	if seq, ok := cur.Resume(fmt.Sprintf("gone-%03d", maxDepartedCities+9)); !ok || seq != 1 {
		t.Fatalf("newest departed city Resume = %d,%v, want 1,true", seq, ok)
	}
}
