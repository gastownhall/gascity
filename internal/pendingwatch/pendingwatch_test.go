package pendingwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeCity is a controllable probe set: sessions, their screens and
// activity clocks, and a log of published transitions.
type fakeCity struct {
	mu         sync.Mutex
	sessions   []Session
	partial    bool
	listErr    error
	pending    map[string]*runtime.PendingInteraction
	activity   map[string]time.Time
	noActivity bool
	now        time.Time
	probeErr   map[string]error
	unsupp     map[string]bool
	probes     map[string]int
	failNext   int
	published  []Transition
	onPublish  func(Transition)
}

func newFakeCity(sessions ...Session) *fakeCity {
	return &fakeCity{
		sessions: sessions,
		pending:  map[string]*runtime.PendingInteraction{},
		activity: map[string]time.Time{},
		now:      t0.Add(time.Hour),
		probeErr: map[string]error{},
		unsupp:   map[string]bool{},
		probes:   map[string]int{},
	}
}

func (f *fakeCity) deps() Deps {
	return Deps{
		List: func() (Listing, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return Listing{Sessions: append([]Session(nil), f.sessions...), Partial: f.partial}, f.listErr
		},
		Activity: func(name string) (time.Time, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.noActivity {
				return time.Time{}, false
			}
			at, ok := f.activity[name]
			return at, ok
		},
		Now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.now
		},
		Probe: func(_ context.Context, name string) Observation {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.probes[name]++
			if f.unsupp[name] {
				return Observation{}
			}
			if err := f.probeErr[name]; err != nil {
				return Observation{Supported: true, Err: err}
			}
			var p *runtime.PendingInteraction
			if cur := f.pending[name]; cur != nil {
				cp := *cur
				p = &cp
			}
			return Observation{Pending: p, Supported: true}
		},
		Publish: func(tr Transition) error {
			f.mu.Lock()
			if f.failNext > 0 {
				f.failNext--
				f.mu.Unlock()
				return errors.New("log unavailable")
			}
			f.published = append(f.published, tr)
			hook := f.onPublish
			f.mu.Unlock()
			if hook != nil {
				hook(tr)
			}
			return nil
		},
	}
}

func (f *fakeCity) set(name string, p *runtime.PendingInteraction, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending[name] = p
	f.activity[name] = at
}

func (f *fakeCity) setNow(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

func (f *fakeCity) transitions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.published))
	for _, tr := range f.published {
		s := tr.Type + " " + tr.Session.ID
		if tr.Type == events.SessionPending {
			s += " " + tr.Pending.RequestID
		} else {
			s += " " + tr.Previous.RequestID + " " + tr.Reason
		}
		out = append(out, s)
	}
	return out
}

func (f *fakeCity) probeCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probes[name]
}

func assertTransitions(t *testing.T, f *fakeCity, want ...string) {
	t.Helper()
	got := f.transitions()
	if len(got) != len(want) {
		t.Fatalf("transitions = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transitions = %q, want %q", got, want)
		}
	}
}

var t0 = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

func approval(id string) *runtime.PendingInteraction {
	return &runtime.PendingInteraction{RequestID: id, Kind: "approval", Prompt: "Allow?", Options: []string{"yes", "no"}}
}

func TestPassPublishesEachTransitionExactlyOnce(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"})
	w := New(f.deps(), nil)
	ctx := context.Background()

	f.set("w1", nil, t0)
	w.Pass(ctx)
	f.set("w1", approval("r1"), t0.Add(time.Second))
	w.Pass(ctx)
	w.Pass(ctx)
	f.set("w1", approval("r1"), t0.Add(2*time.Second)) // output moved, same prompt
	w.Pass(ctx)
	f.set("w1", approval("r2"), t0.Add(3*time.Second))
	w.Pass(ctx)
	f.set("w1", nil, t0.Add(4*time.Second))
	w.Pass(ctx)
	w.Pass(ctx)

	assertTransitions(t, f,
		"session.pending s-1 r1",
		"session.pending_cleared s-1 r1 replaced",
		"session.pending s-1 r2",
		"session.pending_cleared s-1 r2 resolved",
	)
}

func TestPassSkipsCaptureWhenActivityUnchanged(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"}, Session{ID: "s-2", Name: "w2"})
	f.set("w1", nil, t0)
	f.set("w2", nil, t0)
	w := New(f.deps(), nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		w.Pass(ctx)
	}
	if got := f.probeCount("w1"); got != 1 {
		t.Fatalf("captures of an unchanged session over 5 passes = %d, want 1", got)
	}
	f.set("w2", nil, t0.Add(time.Second))
	w.Pass(ctx)
	if got := f.probeCount("w2"); got != 2 {
		t.Fatalf("captures after w2's output moved = %d, want 2", got)
	}
	if got := f.probeCount("w1"); got != 1 {
		t.Fatalf("w1 recaptured with unchanged activity: %d", got)
	}
}

// Activity stamps are whole seconds (tmux), so a prompt painted later in the
// second of a capture that found none leaves the stamp unchanged. That capture
// must not let the next pass skip the session.
func TestPassRecapturesWhenTheCaptureSharesTheStampSecond(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"})
	f.set("w1", nil, t0)
	f.setNow(t0.Add(300 * time.Millisecond))
	w := New(f.deps(), nil)
	ctx := context.Background()
	w.Pass(ctx)

	f.set("w1", approval("r1"), t0) // painted at t0+600ms: the stamp stays t0
	for i := 2; i <= 10; i++ {
		f.setNow(t0.Add(time.Duration(i) * time.Second))
		w.Pass(ctx)
	}
	assertTransitions(t, f, "session.pending s-1 r1")
	if got := f.probeCount("w1"); got != 2 {
		t.Fatalf("captures over 10 passes = %d, want 2 (the same-second one, then one trusted)", got)
	}
}

func TestPassProbesEveryPassWhenActivityUnknown(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"})
	f.noActivity = true
	w := New(f.deps(), nil)
	for i := 0; i < 3; i++ {
		w.Pass(context.Background())
	}
	if got := f.probeCount("w1"); got != 3 {
		t.Fatalf("captures with no activity clock = %d, want 3", got)
	}
}

func TestPokeForcesRecaptureOfAnsweredSession(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"})
	f.set("w1", approval("r1"), t0)
	w := New(f.deps(), nil)
	ctx := context.Background()
	w.Pass(ctx)
	// Answered, but the activity clock has not moved yet.
	f.set("w1", nil, t0)
	w.Pass(ctx)
	assertTransitions(t, f, "session.pending s-1 r1")
	w.Poke("s-1")
	w.Pass(ctx)
	assertTransitions(t, f, "session.pending s-1 r1", "session.pending_cleared s-1 r1 resolved")
}

func TestPassClearsGoneSessionsOnlyOnCompleteListings(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"})
	f.set("w1", approval("r1"), t0)
	w := New(f.deps(), nil)
	ctx := context.Background()
	w.Pass(ctx)

	f.mu.Lock()
	f.sessions = nil
	f.partial = true
	f.mu.Unlock()
	w.Pass(ctx)
	assertTransitions(t, f, "session.pending s-1 r1")

	f.mu.Lock()
	f.partial = false
	f.mu.Unlock()
	w.Pass(ctx)
	w.Pass(ctx)
	assertTransitions(t, f, "session.pending s-1 r1", "session.pending_cleared s-1 r1 session_gone")
}

func TestProbeErrorKeepsAnnouncedState(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"})
	f.set("w1", approval("r1"), t0)
	w := New(f.deps(), nil)
	ctx := context.Background()
	w.Pass(ctx)
	f.mu.Lock()
	f.probeErr["w1"] = errors.New("capture failed")
	f.pending["w1"] = nil
	f.activity["w1"] = t0.Add(time.Second)
	f.mu.Unlock()
	w.Pass(ctx)
	assertTransitions(t, f, "session.pending s-1 r1")
	f.mu.Lock()
	delete(f.probeErr, "w1")
	f.mu.Unlock()
	// The failed probe recorded no activity, so the next pass recaptures
	// even though the clock has not moved since.
	w.Pass(ctx)
	assertTransitions(t, f, "session.pending s-1 r1", "session.pending_cleared s-1 r1 resolved")
}

func TestFailedPublishIsRetriedNotLost(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"})
	f.set("w1", approval("r1"), t0)
	f.failNext = 1
	w := New(f.deps(), nil)
	ctx := context.Background()
	w.Pass(ctx)
	assertTransitions(t, f)
	w.Pass(ctx)
	w.Pass(ctx)
	assertTransitions(t, f, "session.pending s-1 r1")
}

func TestUnsupportedSessionIsProbedOnce(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"})
	f.noActivity = true
	f.unsupp["w1"] = true
	w := New(f.deps(), nil)
	for i := 0; i < 3; i++ {
		w.Pass(context.Background())
	}
	if got := f.probeCount("w1"); got != 1 {
		t.Fatalf("probes of an unsupported session = %d, want 1", got)
	}
	assertTransitions(t, f)
}

func pendingEvent(seq uint64, typ, session, request, reason string) events.Event {
	var raw []byte
	if typ == events.SessionPending {
		raw, _ = json.Marshal(map[string]string{"session_id": session, "request_id": request, "kind": "approval"})
	} else {
		raw, _ = json.Marshal(map[string]string{"session_id": session, "request_id": request, "kind": "approval", "reason": reason})
	}
	return events.Event{Seq: seq, Type: typ, Subject: session, SessionID: session, Payload: raw}
}

func TestRebuildLastEventPerSessionWins(t *testing.T) {
	got := Rebuild([]events.Event{
		pendingEvent(5, events.SessionPendingCleared, "s-1", "r1", ReasonReplaced),
		pendingEvent(1, events.SessionPending, "s-1", "r1", ""),
		pendingEvent(6, events.SessionPending, "s-1", "r2", ""),
		pendingEvent(2, events.SessionPending, "s-2", "q1", ""),
		pendingEvent(3, events.SessionPendingCleared, "s-2", "q1", ReasonResolved),
		pendingEvent(4, events.SessionPending, "s-3", "x1", ""),
		{Seq: 7, Type: "bead.closed", Payload: []byte(`{}`)},
	})
	want := map[string]Published{"s-1": {RequestID: "r2", Kind: "approval"}, "s-3": {RequestID: "x1", Kind: "approval"}}
	if len(got) != len(want) || got["s-1"] != want["s-1"] || got["s-3"] != want["s-3"] {
		t.Fatalf("Rebuild = %+v, want %+v", got, want)
	}
}

// A restarted controller starts from the log: an interaction announced before
// the restart and still pending is not announced again, and one that was
// answered while the controller was down is cleared exactly once.
func TestRestartFromEventLogEmitsNoDuplicates(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"}, Session{ID: "s-2", Name: "w2"})
	f.set("w1", approval("r1"), t0)
	f.set("w2", approval("q1"), t0)
	log := events.NewFake()
	deps := f.deps()
	publish := deps.Publish
	deps.Publish = func(tr Transition) error {
		if err := publish(tr); err != nil {
			return err
		}
		payload := map[string]string{"session_id": tr.Session.ID, "request_id": tr.Pending.RequestID, "kind": tr.Pending.Kind}
		if tr.Type == events.SessionPendingCleared {
			payload = map[string]string{"session_id": tr.Session.ID, "request_id": tr.Previous.RequestID, "kind": tr.Previous.Kind, "reason": tr.Reason}
		}
		raw, _ := json.Marshal(payload)
		log.Record(events.Event{Type: tr.Type, Subject: tr.Session.ID, SessionID: tr.Session.ID, Payload: raw})
		return nil
	}
	New(deps, nil).Pass(context.Background())
	assertTransitions(t, f, "session.pending s-1 r1", "session.pending s-2 q1")

	// Controller down: s-2's prompt is answered.
	f.set("w2", nil, t0.Add(time.Second))

	published, err := RebuildFrom(context.Background(), log, time.Time{})
	if err != nil {
		t.Fatalf("RebuildFrom: %v", err)
	}
	restarted := New(deps, published)
	restarted.Pass(context.Background())
	restarted.Pass(context.Background())
	assertTransitions(t, f,
		"session.pending s-1 r1", "session.pending s-2 q1",
		"session.pending_cleared s-2 q1 resolved",
	)
}

func TestRebuildSinceReachesBeforeTheOldestSession(t *testing.T) {
	now := t0.Add(24 * time.Hour)
	for name, tc := range map[string]struct {
		listing Listing
		want    time.Time
	}{
		"oldest session": {
			listing: Listing{Sessions: []Session{
				{ID: "s-1", Created: t0.Add(2 * time.Hour)},
				{ID: "s-2", Created: t0},
				{ID: "s-3", Created: t0.Add(time.Hour)},
			}},
			want: t0.Add(-RebuildSlack),
		},
		"no sessions":                {want: now.Add(-RebuildSlack)},
		"session stamped after now":  {listing: Listing{Sessions: []Session{{ID: "s-1", Created: now.Add(time.Minute)}}}, want: now.Add(-RebuildSlack)},
		"partial listing":            {listing: Listing{Sessions: []Session{{ID: "s-1", Created: t0}}, Partial: true}},
		"a session's creation unset": {listing: Listing{Sessions: []Session{{ID: "s-1", Created: t0}, {ID: "s-2"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := RebuildSince(tc.listing, now); !got.Equal(tc.want) {
				t.Fatalf("RebuildSince = %v, want %v", got, tc.want)
			}
		})
	}
}

// recordingLog is an event log that records the filter of every List.
type recordingLog struct {
	*events.Fake
	mu      sync.Mutex
	filters []events.Filter
}

func (l *recordingLog) List(filter events.Filter) ([]events.Event, error) {
	l.mu.Lock()
	l.filters = append(l.filters, filter)
	l.mu.Unlock()
	return l.Fake.List(filter)
}

func (l *recordingLog) reads() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.filters)
}

func stamped(e events.Event, at time.Time) events.Event {
	e.Ts = at
	return e
}

func TestRebuildFromReadsBothTypesOnceSinceTheBound(t *testing.T) {
	log := &recordingLog{Fake: events.NewFake()}
	log.Record(stamped(pendingEvent(0, events.SessionPending, "s-old", "o1", ""), t0.Add(-time.Second)))
	log.Record(stamped(pendingEvent(0, events.SessionPending, "s-1", "r1", ""), t0))
	log.Record(stamped(events.Event{Type: "bead.closed", Payload: []byte(`{}`)}, t0.Add(time.Second)))
	log.Record(stamped(pendingEvent(0, events.SessionPendingCleared, "s-1", "r1", ReasonResolved), t0.Add(2*time.Second)))
	log.Record(stamped(pendingEvent(0, events.SessionPending, "s-2", "q1", ""), t0.Add(3*time.Second)))

	got, err := RebuildFrom(context.Background(), log, t0)
	if err != nil {
		t.Fatalf("RebuildFrom: %v", err)
	}
	if want := map[string]Published{"s-2": {RequestID: "q1", Kind: "approval"}}; !maps.Equal(got, want) {
		t.Fatalf("RebuildFrom = %+v, want %+v", got, want)
	}
	if n := log.reads(); n != 1 {
		t.Fatalf("RebuildFrom read the log %d times, want once", n)
	}
}

func TestRebuildFromReturnsOnceCtxEnds(t *testing.T) {
	log := &recordingLog{Fake: events.NewFake()}
	log.Record(pendingEvent(0, events.SessionPending, "s-1", "r1", ""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := RebuildFrom(ctx, log, time.Time{})
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("RebuildFrom = %+v, %v; want nil, %v", got, err, context.Canceled)
	}
	if n := log.reads(); n != 0 {
		t.Fatalf("RebuildFrom read the log %d times after ctx ended, want none", n)
	}
}

// inFlightLog is an event log whose plain List misses a segment still being
// rotated, as the file log's does; ListInFlight folds it in.
type inFlightLog struct {
	*events.Fake
	rotating []events.Event
}

func (l *inFlightLog) ListInFlight(filter events.Filter) ([]events.Event, error) {
	listed, err := l.List(filter)
	if err != nil {
		return nil, err
	}
	return append(events.ApplyFilter(l.rotating, filter), listed...), nil
}

func TestRebuildFromFoldsInTheSegmentBeingRotated(t *testing.T) {
	log := &inFlightLog{Fake: events.NewFake(), rotating: []events.Event{pendingEvent(1, events.SessionPending, "s-1", "r1", "")}}
	got, err := RebuildFrom(context.Background(), log, time.Time{})
	if err != nil {
		t.Fatalf("RebuildFrom: %v", err)
	}
	if want := map[string]Published{"s-1": {RequestID: "r1", Kind: "approval"}}; !maps.Equal(got, want) {
		t.Fatalf("RebuildFrom = %+v, want %+v", got, want)
	}
}

// segmentName is the file log's name for a rotated segment: kind is "archive"
// or "rotating", at is the rotation instant.
func segmentName(kind string, at time.Time, first, last uint64) string {
	return fmt.Sprintf("events.jsonl.%s-%s-seq-%d-%d", kind, at.UTC().Format("20060102T150405Z"), first, last)
}

func writeEventLines(t *testing.T, path string, evs ...events.Event) {
	t.Helper()
	var buf []byte
	for _, e := range evs {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("encoding event %d: %v", e.Seq, err)
		}
		buf = append(append(buf, line...), '\n')
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

// On the file log the rebuild includes the segment a rotation has not yet
// compressed, and never opens an archive rotated away before the bound.
func TestRebuildFromFileLogReadsTheRotatingSegmentAndSkipsOlderArchives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	since := t0
	// Not gzip: opening it fails the read.
	if err := os.WriteFile(filepath.Join(dir, segmentName("archive", since.Add(-2*time.Hour), 1, 2)+".gz"), []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeEventLines(t, filepath.Join(dir, segmentName("rotating", since.Add(10*time.Minute), 3, 4)),
		stamped(pendingEvent(3, events.SessionPending, "s-1", "r1", ""), since.Add(time.Minute)),
		stamped(pendingEvent(4, events.SessionPending, "s-2", "q1", ""), since.Add(2*time.Minute)),
	)
	writeEventLines(t, path,
		stamped(pendingEvent(5, events.SessionPendingCleared, "s-2", "q1", ReasonResolved), since.Add(11*time.Minute)),
		stamped(pendingEvent(6, events.SessionPending, "s-3", "x1", ""), since.Add(12*time.Minute)),
	)
	log := events.NewReadOnlyFileProvider(path, io.Discard)

	got, err := RebuildFrom(context.Background(), log, since)
	if err != nil {
		t.Fatalf("RebuildFrom: %v", err)
	}
	want := map[string]Published{"s-1": {RequestID: "r1", Kind: "approval"}, "s-3": {RequestID: "x1", Kind: "approval"}}
	if !maps.Equal(got, want) {
		t.Fatalf("RebuildFrom = %+v, want %+v", got, want)
	}
	// Unbounded, the same read opens the old archive and fails.
	if got, err := RebuildFrom(context.Background(), log, time.Time{}); err == nil {
		t.Fatalf("RebuildFrom over the whole log = %+v, want the unreadable archive's error", got)
	}
}

// Run publishes without any caller-side polling: a poke drives a pass.
func TestRunPassesOnPoke(t *testing.T) {
	f := newFakeCity(Session{ID: "s-1", Name: "w1"})
	f.set("w1", nil, t0)
	got := make(chan Transition, 4)
	f.onPublish = func(tr Transition) { got <- tr }
	w := New(f.deps(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx, time.Hour) }()

	f.set("w1", approval("r1"), t0.Add(time.Second))
	w.Poke("s-1")
	tr := <-got
	if tr.Type != events.SessionPending || tr.Pending.RequestID != "r1" {
		t.Fatalf("first transition = %+v, want session.pending r1", tr)
	}
	cancel()
	<-done
}
