// Package eventstest provides a conformance test suite for events.Provider
// implementations. Each implementation's test file calls RunProviderTests
// with its own factory function.
package eventstest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// RunProviderTests runs the core conformance suite against a Provider implementation.
// The newProvider function must return a fresh, empty provider and a cleanup closure.
func RunProviderTests(t *testing.T, newProvider func(t *testing.T) (events.Provider, func())) {
	t.Helper()

	// --- Record + List round-trip ---

	t.Run("RecordAndListRoundTrip", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{
			Type:    events.BeadCreated,
			Actor:   "human",
			Subject: "gc-1",
			Message: "Build Tower of Hanoi",
		})

		got, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("List returned %d events, want 1", len(got))
		}
		e := got[0]
		if e.Type != events.BeadCreated {
			t.Errorf("Type = %q, want %q", e.Type, events.BeadCreated)
		}
		if e.Actor != "human" {
			t.Errorf("Actor = %q, want %q", e.Actor, "human")
		}
		if e.Subject != "gc-1" {
			t.Errorf("Subject = %q, want %q", e.Subject, "gc-1")
		}
		if e.Message != "Build Tower of Hanoi" {
			t.Errorf("Message = %q, want %q", e.Message, "Build Tower of Hanoi")
		}
	})

	t.Run("RecordAutoFillsSeq", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		p.Record(events.Event{Type: events.BeadClosed, Actor: "human"})

		got, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("List returned %d events, want 2", len(got))
		}
		if got[0].Seq == 0 {
			t.Error("first event Seq is 0, want non-zero")
		}
		if got[1].Seq == 0 {
			t.Error("second event Seq is 0, want non-zero")
		}
		if got[1].Seq <= got[0].Seq {
			t.Errorf("Seq not monotonically increasing: %d <= %d", got[1].Seq, got[0].Seq)
		}
	})

	t.Run("RecordAutoFillsTimestamp", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})

		got, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("List returned %d events, want 1", len(got))
		}
		if got[0].Ts.IsZero() {
			t.Error("Ts is zero, want auto-filled")
		}
		if time.Since(got[0].Ts).Abs() > 5*time.Second {
			t.Errorf("Ts = %v, want within 5s of now", got[0].Ts)
		}
	})

	t.Run("RecordPreservesExplicitTimestamp", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		explicit := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Ts: explicit})

		got, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("List returned %d events, want 1", len(got))
		}
		if !got[0].Ts.Equal(explicit) {
			t.Errorf("Ts = %v, want %v", got[0].Ts, explicit)
		}
	})

	t.Run("RecordPreservesAllFields", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{
			Type:    events.SessionWoke,
			Actor:   "controller",
			Subject: "worker-1",
			Message: "agent started successfully",
		})

		got, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("List returned %d events, want 1", len(got))
		}
		e := got[0]
		if e.Type != events.SessionWoke {
			t.Errorf("Type = %q, want %q", e.Type, events.SessionWoke)
		}
		if e.Actor != "controller" {
			t.Errorf("Actor = %q, want %q", e.Actor, "controller")
		}
		if e.Subject != "worker-1" {
			t.Errorf("Subject = %q, want %q", e.Subject, "worker-1")
		}
		if e.Message != "agent started successfully" {
			t.Errorf("Message = %q, want %q", e.Message, "agent started successfully")
		}
	})

	t.Run("RecordMultipleEvents", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		p.Record(events.Event{Type: events.BeadClosed, Actor: "human"})
		p.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})

		got, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("List returned %d events, want 3", len(got))
		}
	})

	// --- List filtering ---

	t.Run("ListEmptyFilter", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		p.Record(events.Event{Type: events.BeadClosed, Actor: "human"})

		got, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("List returned %d events, want 2", len(got))
		}
	})

	t.Run("ListFilterByType", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		p.Record(events.Event{Type: events.BeadClosed, Actor: "human"})
		p.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})

		got, err := p.List(events.Filter{Type: events.BeadCreated})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("List(type=bead.created) returned %d events, want 1", len(got))
		}
		if got[0].Type != events.BeadCreated {
			t.Errorf("Type = %q, want %q", got[0].Type, events.BeadCreated)
		}
	})

	t.Run("ListFilterByActor", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		p.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
		p.Record(events.Event{Type: events.BeadClosed, Actor: "human"})

		got, err := p.List(events.Filter{Actor: "gc"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("List(actor=gc) returned %d events, want 1", len(got))
		}
		if got[0].Actor != "gc" {
			t.Errorf("Actor = %q, want %q", got[0].Actor, "gc")
		}
	})

	t.Run("ListFilterByAfterSeq", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		p.Record(events.Event{Type: events.BeadClosed, Actor: "human"})
		p.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})

		// Get all events to find seq values.
		all, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List(all): %v", err)
		}
		if len(all) < 2 {
			t.Fatalf("need at least 2 events, got %d", len(all))
		}

		// Filter after the first event's seq.
		got, err := p.List(events.Filter{AfterSeq: all[0].Seq})
		if err != nil {
			t.Fatalf("List(AfterSeq): %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("List(AfterSeq=%d) returned %d events, want 2", all[0].Seq, len(got))
		}
		for _, e := range got {
			if e.Seq <= all[0].Seq {
				t.Errorf("event Seq %d should be > %d", e.Seq, all[0].Seq)
			}
		}
	})

	t.Run("ListFilterBySince", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		// Use a UTC time base so shell-backed test providers that compare
		// RFC3339 strings do not see mixed-offset timestamps.
		now := time.Now().UTC()
		past := now.Add(-2 * time.Hour)
		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Ts: past})
		p.Record(events.Event{Type: events.SessionWoke, Actor: "gc"}) // auto-filled = now

		since := now.Add(-1 * time.Hour)
		got, err := p.List(events.Filter{Since: since})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("List(Since) returned %d events, want 1", len(got))
		}
		if got[0].Type != events.SessionWoke {
			t.Errorf("Type = %q, want %q", got[0].Type, events.SessionWoke)
		}
	})

	t.Run("ListFilterBySubject", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "actor-a", Subject: "gc-1"})
		p.Record(events.Event{Type: events.BeadClosed, Actor: "actor-a", Subject: "gc-2"})
		p.Record(events.Event{Type: events.BeadUpdated, Actor: "actor-b", Subject: "gc-1"})

		got, err := p.List(events.Filter{Subject: "gc-1"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("List(Subject) returned %d events, want 2", len(got))
		}
		for _, e := range got {
			if e.Subject != "gc-1" {
				t.Errorf("Subject = %q, want gc-1", e.Subject)
			}
		}
	})

	t.Run("ListFilterByUntil", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		cutoff := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
		before := cutoff.Add(-time.Minute)
		after := cutoff.Add(time.Minute)
		p.Record(events.Event{Type: events.BeadCreated, Actor: "actor-a", Subject: "before", Ts: before})
		p.Record(events.Event{Type: events.BeadUpdated, Actor: "actor-a", Subject: "boundary", Ts: cutoff})
		p.Record(events.Event{Type: events.BeadClosed, Actor: "actor-a", Subject: "after", Ts: after})

		got, err := p.List(events.Filter{Until: cutoff})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("List(Until) returned %d events, want 2", len(got))
		}
		if got[0].Subject != "before" {
			t.Errorf("got[0].Subject = %q, want before", got[0].Subject)
		}
		if got[1].Subject != "boundary" {
			t.Errorf("got[1].Subject = %q, want boundary", got[1].Subject)
		}
	})

	t.Run("ListFilterByLimit", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		for _, subject := range []string{"gc-1", "gc-2", "gc-3", "gc-4"} {
			p.Record(events.Event{Type: events.BeadCreated, Actor: "actor-a", Subject: subject})
		}

		got, err := p.List(events.Filter{Limit: 2})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("List(Limit) returned %d events, want 2", len(got))
		}
		if got[0].Subject != "gc-1" {
			t.Errorf("got[0].Subject = %q, want gc-1", got[0].Subject)
		}
		if got[1].Subject != "gc-2" {
			t.Errorf("got[1].Subject = %q, want gc-2", got[1].Subject)
		}
	})

	t.Run("ListFilterCombined", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		base := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
		p.Record(events.Event{Type: events.MailSent, Actor: "seed", Subject: "seed", Ts: base})                           // seq 1
		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1", Ts: base.Add(2 * time.Hour)})    // after Until
		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1", Ts: base.Add(-2 * time.Hour)})   // before Since
		p.Record(events.Event{Type: events.BeadClosed, Actor: "human", Subject: "gc-1", Ts: base.Add(10 * time.Minute)})  // wrong Type
		p.Record(events.Event{Type: events.BeadCreated, Actor: "agent", Subject: "gc-1", Ts: base.Add(20 * time.Minute)}) // wrong Actor
		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-2", Ts: base.Add(30 * time.Minute)}) // wrong Subject
		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1", Ts: base.Add(40 * time.Minute)}) // match 1
		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1", Ts: base.Add(50 * time.Minute)}) // match 2
		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1", Ts: base.Add(55 * time.Minute)}) // limited out

		// Get all to find seq of first event.
		all, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List(all): %v", err)
		}
		if len(all) < 1 {
			t.Fatal("need at least 1 event")
		}

		got, err := p.List(events.Filter{
			Type:     events.BeadCreated,
			Actor:    "human",
			Subject:  "gc-1",
			Since:    base.Add(-time.Hour),
			Until:    base.Add(time.Hour),
			AfterSeq: all[0].Seq,
			Limit:    2,
		})
		if err != nil {
			t.Fatalf("List(combined): %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("List(all predicates) returned %d events, want 2", len(got))
		}
		for _, e := range got {
			if e.Type != events.BeadCreated || e.Actor != "human" || e.Subject != "gc-1" {
				t.Fatalf("event = %+v, want bead.created by human for gc-1", e)
			}
			if e.Ts.Before(base.Add(-time.Hour)) || e.Ts.After(base.Add(time.Hour)) {
				t.Fatalf("event Ts = %s, want within combined window", e.Ts)
			}
		}
	})

	t.Run("ListNoMatch", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})

		got, err := p.List(events.Filter{Type: events.MailSent})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("List(no-match) returned %d events, want 0", len(got))
		}
	})

	t.Run("ListEmptyProvider", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		got, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("List(empty) returned %d events, want 0", len(got))
		}
	})

	// --- LatestSeq ---

	t.Run("LatestSeqEmpty", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		seq, err := p.LatestSeq()
		if err != nil {
			t.Fatalf("LatestSeq: %v", err)
		}
		if seq != 0 {
			t.Errorf("LatestSeq(empty) = %d, want 0", seq)
		}
	})

	t.Run("LatestSeqAfterRecords", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		p.Record(events.Event{Type: events.BeadClosed, Actor: "human"})
		p.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})

		seq, err := p.LatestSeq()
		if err != nil {
			t.Fatalf("LatestSeq: %v", err)
		}

		// Get all events to verify the seq matches the last event.
		all, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) == 0 {
			t.Fatal("expected events")
		}
		maxSeq := all[len(all)-1].Seq
		if seq != maxSeq {
			t.Errorf("LatestSeq = %d, want %d (highest event Seq)", seq, maxSeq)
		}
	})

	t.Run("LatestSeqMonotonic", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		seq1, err := p.LatestSeq()
		if err != nil {
			t.Fatalf("LatestSeq(1): %v", err)
		}

		p.Record(events.Event{Type: events.BeadClosed, Actor: "human"})
		seq2, err := p.LatestSeq()
		if err != nil {
			t.Fatalf("LatestSeq(2): %v", err)
		}

		if seq2 < seq1 {
			t.Errorf("LatestSeq decreased: %d < %d", seq2, seq1)
		}
	})

	// --- Watch ---

	t.Run("WatchExistingEvents", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1"})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		w, err := p.Watch(ctx, 0)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		defer w.Close() //nolint:errcheck // test cleanup

		e, err := w.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if e.Subject != "gc-1" {
			t.Errorf("Subject = %q, want %q", e.Subject, "gc-1")
		}
	})

	t.Run("WatchNewEvents", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		w, err := p.Watch(ctx, 0)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		defer w.Close() //nolint:errcheck // test cleanup

		// Record in a goroutine after a short delay.
		go func() {
			time.Sleep(50 * time.Millisecond)
			p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-new"})
		}()

		e, err := w.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if e.Subject != "gc-new" {
			t.Errorf("Subject = %q, want %q", e.Subject, "gc-new")
		}
	})

	t.Run("WatchAfterSeq", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-1"})
		p.Record(events.Event{Type: events.BeadClosed, Actor: "human", Subject: "gc-1"})

		// Get all to find seq of last event.
		all, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) < 2 {
			t.Fatalf("need 2 events, got %d", len(all))
		}
		lastSeq := all[len(all)-1].Seq

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		// Watch after the last existing event.
		w, err := p.Watch(ctx, lastSeq)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		defer w.Close() //nolint:errcheck // test cleanup

		// Record a new event.
		go func() {
			time.Sleep(50 * time.Millisecond)
			p.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "worker-1"})
		}()

		e, err := w.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if e.Subject != "worker-1" {
			t.Errorf("Subject = %q, want %q", e.Subject, "worker-1")
		}
		if e.Seq <= lastSeq {
			t.Errorf("Seq = %d, want > %d", e.Seq, lastSeq)
		}
	})

	t.Run("WatchContextCancel", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		ctx, cancel := context.WithCancel(context.Background())

		w, err := p.Watch(ctx, 0)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		defer w.Close() //nolint:errcheck // test cleanup

		cancel()
		_, err = w.Next()
		if err == nil {
			t.Fatal("Next after cancel should return error")
		}
		// Accept either context.Canceled or context.DeadlineExceeded.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Next after cancel = %v, want context.Canceled or DeadlineExceeded", err)
		}
	})

	// --- Close ---

	t.Run("CloseNoError", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		if err := p.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	})
}

// RunConcurrencyTests runs concurrency-specific tests. Only valid for
// in-process providers (FileRecorder, Fake) where goroutines share the
// same provider instance.
func RunConcurrencyTests(t *testing.T, newProvider func(t *testing.T) (events.Provider, func())) {
	t.Helper()

	t.Run("ConcurrentRecordSafe", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		const goroutines = 10
		const eventsPerGoroutine = 10
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			go func() {
				defer wg.Done()
				for i := 0; i < eventsPerGoroutine; i++ {
					p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
				}
			}()
		}
		wg.Wait()

		got, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		total := goroutines * eventsPerGoroutine
		if len(got) != total {
			t.Errorf("List returned %d events, want %d", len(got), total)
		}

		// All seq values should be unique.
		seen := make(map[uint64]bool, total)
		for _, e := range got {
			if seen[e.Seq] {
				t.Errorf("duplicate seq: %d", e.Seq)
			}
			seen[e.Seq] = true
		}
	})
}
