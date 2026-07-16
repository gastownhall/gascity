// Package eventstest provides a conformance test suite for events.Provider
// implementations. Each implementation's test file calls RunProviderTests
// with its own factory function.
package eventstest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// rotatableProvider is the small interface a Provider must satisfy
// for the rotation subtest. Providers that don't expose rotation
// (e.g. exec, fake) skip the test rather than failing the suite.
type rotatableProvider interface {
	events.Provider
	ForceRotate() (events.RotationResult, error)
}

type nextResult struct {
	event events.Event
	err   error
}

func startNext(w events.Watcher) <-chan nextResult {
	result := make(chan nextResult, 1)
	go func() {
		e, err := w.Next()
		result <- nextResult{event: e, err: err}
	}()
	return result
}

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

		// Generous deadline: exec-backed providers fork subprocesses per
		// call, which can take seconds on loaded CI runners. Healthy
		// providers deliver in milliseconds regardless.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		w, err := p.Watch(ctx, 0)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		defer w.Close() //nolint:errcheck // test cleanup

		// The watcher is attached before the event is recorded. Providers must
		// deliver that event without requiring an authored settling delay.
		p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-new"})

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

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Watch after the last existing event.
		w, err := p.Watch(ctx, lastSeq)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		defer w.Close() //nolint:errcheck // test cleanup

		// Record after the watcher is positioned at the retained tail.
		p.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "worker-1"})

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

	// WatchReplaysRetainedHistory pins the Watch contract: a watcher attached
	// with afterSeq below the retained head must replay every retained event
	// with Seq > afterSeq, in order, exactly once — including events recorded
	// before Watch was called. This is provider-neutral (no rotation); the
	// FileRecorder-specific resume-across-rotation case lives in RunRotationTests.
	t.Run("WatchReplaysRetainedHistory", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		for i := 0; i < 5; i++ {
			p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: fmt.Sprintf("h-%d", i)})
		}
		all, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) < 5 {
			t.Fatalf("need 5 events, got %d", len(all))
		}
		// Resume from the 2nd event's seq: expect events 3,4,5 (indices 2,3,4).
		afterSeq := all[1].Seq
		want := all[2:]

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		w, err := p.Watch(ctx, afterSeq)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		defer w.Close() //nolint:errcheck // test cleanup

		for i, wantEv := range want {
			type res struct {
				e   events.Event
				err error
			}
			ch := make(chan res, 1)
			go func() { e, err := w.Next(); ch <- res{e, err} }()
			select {
			case r := <-ch:
				if r.err != nil {
					t.Fatalf("Next %d: %v", i, r.err)
				}
				if r.e.Seq != wantEv.Seq {
					t.Fatalf("replay event %d seq = %d, want %d (pre-Watch history skipped?)", i, r.e.Seq, wantEv.Seq)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("replay event %d (seq %d) never delivered", i, wantEv.Seq)
			}
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

// RunRotationTests exercises the events.jsonl rotation contract on
// providers that expose ForceRotate. The test asserts the three
// invariants from the architect's NFRs:
//
//   - (a) Seq is monotonic across rotation. The events.rotated anchor
//     in the new active log has Seq strictly greater than the highest
//     Seq in the archive (FR-03).
//   - (b) ReadAll covers active + archives in a single chronological
//     stream (NFR-04 — archive-aware reads).
//   - (c) A Watch positioned before the rotation continues yielding
//     events from the new active log without gap (designer §8.1).
//
// Providers that don't satisfy ForceRotate skip this test.
func RunRotationTests(t *testing.T, newProvider func(t *testing.T) (events.Provider, func())) {
	t.Helper()

	t.Run("RotationPreservesInvariants", func(t *testing.T) {
		p, cleanup := newProvider(t)
		defer cleanup()

		rec, ok := p.(rotatableProvider)
		if !ok {
			t.Skipf("provider %T does not support ForceRotate", p)
		}

		// Phase 1: write some pre-rotate events.
		for i := 0; i < 5; i++ {
			p.Record(events.Event{Type: events.BeadCreated, Actor: "human"})
		}

		// Phase 2: start a watcher BEFORE rotation. Drain any backlog
		// so the watcher's offset is at end-of-active before we rotate.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		w, err := p.Watch(ctx, 0)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		defer w.Close() //nolint:errcheck // test cleanup

		seen := make([]events.Event, 0, 5)
		for i := 0; i < 5; i++ {
			e, err := w.Next()
			if err != nil {
				t.Fatalf("Next pre %d: %v", i, err)
			}
			seen = append(seen, e)
		}

		// Phase 3: rotate.
		res, err := rec.ForceRotate()
		if err != nil {
			t.Fatalf("ForceRotate: %v", err)
		}
		if !res.Rotated {
			t.Fatal("ForceRotate did not rotate a non-empty log")
		}
		if res.Done != nil {
			<-res.Done
		}

		// (a) The anchor's seq must be strictly greater than the last
		// archived event's seq.
		if res.AnchorSeq <= res.LastSeq {
			t.Errorf("AnchorSeq %d <= LastSeq %d (FR-03 violated)", res.AnchorSeq, res.LastSeq)
		}

		// Phase 4: write more events post-rotate.
		for i := 0; i < 3; i++ {
			p.Record(events.Event{Type: events.BeadClosed, Actor: "human"})
		}

		// (c) The watcher should yield the anchor + the post-rotate
		// events without gap.
		for i := 0; i < 4; i++ { // 1 anchor + 3 post-rotate
			e, err := w.Next()
			if err != nil {
				t.Fatalf("Next post %d: %v", i, err)
			}
			seen = append(seen, e)
		}
		if seen[5].Type != events.EventsRotated {
			t.Errorf("expected anchor at index 5, got %q", seen[5].Type)
		}

		// (b) ReadAll spans active + archives.
		all, err := p.List(events.Filter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) < len(seen) {
			t.Errorf("List returned %d events, watcher saw %d — archive walk missed events",
				len(all), len(seen))
		}
		for i := 1; i < len(all); i++ {
			if all[i].Seq <= all[i-1].Seq {
				t.Errorf("List seq not monotonic at %d: %d <= %d", i, all[i].Seq, all[i-1].Seq)
			}
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

// RunInMemoryWakeTests runs deterministic wake-up tests for in-memory
// providers whose goroutines and synchronization are contained by synctest.
// Wake-up, cancellation, and close must complete without advancing fake time.
func RunInMemoryWakeTests(t *testing.T, newProvider func(t *testing.T) (events.Provider, func())) {
	t.Helper()

	t.Run("RecordWakesEveryBlockedWatcher", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p, cleanup := newProvider(t)
			defer cleanup()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			const watcherCount = 2
			watchers := make([]events.Watcher, watcherCount)
			results := make([]<-chan nextResult, watcherCount)
			for i := range watcherCount {
				w, err := p.Watch(ctx, 0)
				if err != nil {
					t.Fatalf("Watch %d: %v", i, err)
				}
				watchers[i] = w
				results[i] = startNext(w)
			}

			// Establish that both Next calls are blocked before recording.
			synctest.Wait()
			start := time.Now()
			p.Record(events.Event{Type: events.BeadCreated, Actor: "human", Subject: "gc-broadcast"})
			synctest.Wait()
			elapsed := time.Since(start)

			got := make([]nextResult, watcherCount)
			deliveredCount := 0
			for i := range watcherCount {
				select {
				case got[i] = <-results[i]:
					deliveredCount++
				default:
				}
			}

			// Unblock any watcher left behind by a non-broadcast implementation
			// before reporting the contract failure.
			for _, w := range watchers {
				if err := w.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			}
			synctest.Wait()

			if elapsed != 0 {
				t.Fatalf("Record delivery advanced fake time by %v, want 0", elapsed)
			}
			if deliveredCount != watcherCount {
				t.Fatalf("Record delivered to %d of %d blocked watchers without advancing fake time; wake was not broadcast", deliveredCount, watcherCount)
			}
			for i := range watcherCount {
				if got[i].err != nil {
					t.Fatalf("watcher %d Next: %v", i, got[i].err)
				}
				if got[i].event.Subject != "gc-broadcast" {
					t.Fatalf("watcher %d Subject = %q, want %q", i, got[i].event.Subject, "gc-broadcast")
				}
			}
			if got[0].event.Seq != got[1].event.Seq {
				t.Fatalf("watchers received Seq %d and %d, want the same event", got[0].event.Seq, got[1].event.Seq)
			}
		})
	})

	t.Run("ContextCancelUnblocksBlockedWatcher", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p, cleanup := newProvider(t)
			defer cleanup()

			ctx, cancel := context.WithCancel(context.Background())
			w, err := p.Watch(ctx, 0)
			if err != nil {
				t.Fatalf("Watch: %v", err)
			}
			defer w.Close() //nolint:errcheck // test cleanup

			result := startNext(w)
			synctest.Wait()

			start := time.Now()
			cancel()
			synctest.Wait()
			elapsed := time.Since(start)

			select {
			case got := <-result:
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("Next after cancel = %v, want context.Canceled", got.err)
				}
			default:
				t.Fatal("Next remained blocked after context cancellation")
			}
			if elapsed != 0 {
				t.Fatalf("context cancellation advanced fake time by %v, want 0", elapsed)
			}
		})
	})

	t.Run("CloseUnblocksBlockedWatcher", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p, cleanup := newProvider(t)
			defer cleanup()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			w, err := p.Watch(ctx, 0)
			if err != nil {
				t.Fatalf("Watch: %v", err)
			}

			result := startNext(w)
			synctest.Wait()

			start := time.Now()
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			synctest.Wait()
			elapsed := time.Since(start)

			select {
			case got := <-result:
				if got.err == nil {
					t.Fatal("Next after Close returned nil error")
				}
			default:
				cancel()
				synctest.Wait()
				t.Fatal("Next remained blocked after Close")
			}
			if elapsed != 0 {
				t.Fatalf("Close advanced fake time by %v, want 0", elapsed)
			}
		})
	})
}
