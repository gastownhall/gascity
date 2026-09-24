package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// gateBlockingStore serves the open-work gate a fixed corpus instead of the
// store's real contents, so a test can hold the strict gate shut (or open it)
// for an exact number of ticks. Writes still land in the embedded store; they
// are simply invisible to the gate's index read, which is what lets a test
// re-block an order after a dispatch without the dispatch's own tracking bead
// changing which gate answers.
type gateBlockingStore struct {
	beads.Store

	mu      sync.Mutex
	blocked bool
	corpus  []beads.Bead
}

func newGateBlockingStore(scoped string, createdAt time.Time) *gateBlockingStore {
	return &gateBlockingStore{
		Store:   beads.NewMemStore(),
		blocked: true,
		// An in_progress tracking bead is the shape that passes the first gate
		// (openTracking wants status "open") and holds the STRICT gate shut
		// (openWorkTracking is "not closed") — the exact silent-suppression
		// condition ga-a6zy9 is about.
		corpus: []beads.Bead{{
			ID:        "gate-blocker",
			Title:     "in-flight order run",
			Status:    "in_progress",
			Labels:    []string{"order-run:" + scoped, "order-tracking"},
			CreatedAt: createdAt,
		}},
	}
}

func (s *gateBlockingStore) setBlocked(blocked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked = blocked
}

// setCreatedAt moves the tracking bead forward in time, which is what the
// dispatcher reads as the order's last run. A test uses it to make a warm
// last-run cache stale on purpose.
func (s *gateBlockingStore) setCreatedAt(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.corpus {
		s.corpus[i].CreatedAt = at
	}
}

func (s *gateBlockingStore) List(beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.blocked {
		return nil, nil
	}
	out := make([]beads.Bead, len(s.corpus))
	copy(out, s.corpus)
	return out, nil
}

// suppressedEvents returns every recorded order.suppressed event, in record
// order.
func (r *memRecorder) suppressedEvents() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.Event
	for _, e := range r.events {
		if e.Type == events.OrderSuppressed {
			out = append(out, e)
		}
	}
	return out
}

func decodeSuppressedPayload(t *testing.T, e events.Event) events.OrderSuppressedPayload {
	t.Helper()
	var p events.OrderSuppressedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decoding %s payload %s: %v", e.Type, e.Payload, err)
	}
	return p
}

func suppressedOrderDispatcher(t *testing.T, scoped string, store beads.Store, rec events.Recorder) *memoryOrderDispatcher {
	t.Helper()
	return suppressedOrderDispatcherFor(t, []orders.Order{{
		Name:     scoped,
		Trigger:  "cooldown",
		Interval: "1s",
		Exec:     "true",
	}}, store, rec)
}

func suppressedOrderDispatcherFor(t *testing.T, aa []orders.Order, store beads.Store, rec events.Recorder) *memoryOrderDispatcher {
	t.Helper()
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, rec)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	m, ok := ad.(*memoryOrderDispatcher)
	if !ok {
		t.Fatalf("dispatcher is %T, want *memoryOrderDispatcher", ad)
	}
	return m
}

// TestOrderDispatchEmitsOrderSuppressedAfterConsecutiveGateSuppressions is the
// core ga-a6zy9 assertion: a permanently shut open-work gate must stop being
// silent. Below the threshold nothing is emitted (a briefly in-flight order is
// normal); at the threshold exactly one order.suppressed event carries the
// order name, the streak length and how long it has been running.
func TestOrderDispatchEmitsOrderSuppressedAfterConsecutiveGateSuppressions(t *testing.T) {
	const scoped = "stuck-order"
	start := time.Date(2030, 8, 21, 9, 0, 0, 0, time.UTC)
	store := newGateBlockingStore(scoped, start.Add(-24*time.Hour))
	rec := &memRecorder{}
	m := suppressedOrderDispatcher(t, scoped, store, rec)
	cityPath := t.TempDir()

	now := start
	for i := 0; i < orderOpenWorkSuppressionAlertAfter-1; i++ {
		m.dispatch(context.Background(), cityPath, now)
		m.drain(context.Background())
		now = now.Add(time.Minute)
	}
	if got := len(rec.suppressedEvents()); got != 0 {
		t.Fatalf("order.suppressed events below the threshold = %d, want 0", got)
	}

	m.dispatch(context.Background(), cityPath, now)
	m.drain(context.Background())

	got := rec.suppressedEvents()
	if len(got) != 1 {
		t.Fatalf("order.suppressed events at the threshold = %d, want 1", len(got))
	}
	if got[0].Subject != scoped {
		t.Errorf("event subject = %q, want %q", got[0].Subject, scoped)
	}
	p := decodeSuppressedPayload(t, got[0])
	if p.OrderName != scoped {
		t.Errorf("payload order_name = %q, want %q", p.OrderName, scoped)
	}
	if p.Consecutive != orderOpenWorkSuppressionAlertAfter {
		t.Errorf("payload consecutive = %d, want %d", p.Consecutive, orderOpenWorkSuppressionAlertAfter)
	}
	wantSince := start.UTC().Format(time.RFC3339)
	if p.FirstSuppressed != wantSince {
		t.Errorf("payload first_suppressed = %q, want %q", p.FirstSuppressed, wantSince)
	}
	wantFor := now.Sub(start).Milliseconds()
	if p.SuppressedForMS != wantFor {
		t.Errorf("payload suppressed_for_ms = %d, want %d", p.SuppressedForMS, wantFor)
	}
}

// TestOrderDispatchOrderSuppressedIsRateBoundedNotPerTick pins the flood bound.
// A permanently stuck order is suppressed on EVERY tick forever, so an
// unbounded emitter would turn one wedged order into an unbounded event stream.
// The repeat is wall-clock bounded, not tick-count bounded, so the bound holds
// no matter how fast the controller ticks.
func TestOrderDispatchOrderSuppressedIsRateBoundedNotPerTick(t *testing.T) {
	const scoped = "wedged-order"
	start := time.Date(2030, 8, 21, 9, 0, 0, 0, time.UTC)
	store := newGateBlockingStore(scoped, start.Add(-24*time.Hour))
	rec := &memRecorder{}
	m := suppressedOrderDispatcher(t, scoped, store, rec)
	cityPath := t.TempDir()

	// 100 ticks, 30s apart: 50 minutes of wall clock, comfortably inside one
	// repeat window and far past the alert threshold.
	const ticks = 100
	now := start
	for i := 0; i < ticks; i++ {
		m.dispatch(context.Background(), cityPath, now)
		m.drain(context.Background())
		now = now.Add(30 * time.Second)
	}
	if got := len(rec.suppressedEvents()); got != 1 {
		t.Fatalf("order.suppressed events over %d suppressed ticks inside one repeat window = %d, want 1", ticks, got)
	}

	// Past the repeat window the stall is re-reported, so a stuck order does
	// not go quiet again after its first alert.
	now = start.Add(orderOpenWorkSuppressionRepeat + time.Hour)
	m.dispatch(context.Background(), cityPath, now)
	m.drain(context.Background())

	got := rec.suppressedEvents()
	if len(got) != 2 {
		t.Fatalf("order.suppressed events after the repeat window = %d, want 2", len(got))
	}
	p := decodeSuppressedPayload(t, got[1])
	if p.Consecutive != ticks+1 {
		t.Errorf("repeat payload consecutive = %d, want %d", p.Consecutive, ticks+1)
	}
}

// TestOrderDispatchOpenWorkSuppressionStreakResetsWhenGateOpens proves the
// streak counts CONSECUTIVE suppressions. An order whose gate opens often
// enough to dispatch is healthy, however many suppressed ticks it accumulates
// in between, and must never produce a stall alert.
func TestOrderDispatchOpenWorkSuppressionStreakResetsWhenGateOpens(t *testing.T) {
	const scoped = "flapping-order"
	start := time.Date(2030, 8, 21, 9, 0, 0, 0, time.UTC)
	store := newGateBlockingStore(scoped, start.Add(-24*time.Hour))
	rec := &memRecorder{}
	m := suppressedOrderDispatcher(t, scoped, store, rec)
	cityPath := t.TempDir()

	now := start
	tick := func() {
		m.dispatch(context.Background(), cityPath, now)
		m.drain(context.Background())
		now = now.Add(time.Minute)
	}

	for i := 0; i < orderOpenWorkSuppressionAlertAfter-1; i++ {
		tick()
	}
	store.setBlocked(false)
	tick() // the gate opens and the order dispatches: streak back to zero.
	store.setBlocked(true)
	for i := 0; i < orderOpenWorkSuppressionAlertAfter-1; i++ {
		tick()
	}

	if got := len(rec.suppressedEvents()); got != 0 {
		t.Fatalf("order.suppressed events for an order that dispatched mid-streak = %d, want 0", got)
	}

	tick() // now the second streak reaches the threshold on its own.
	got := rec.suppressedEvents()
	if len(got) != 1 {
		t.Fatalf("order.suppressed events after a full second streak = %d, want 1", len(got))
	}
	if p := decodeSuppressedPayload(t, got[0]); p.Consecutive != orderOpenWorkSuppressionAlertAfter {
		t.Errorf("payload consecutive = %d, want %d (the streak must restart, not resume)", p.Consecutive, orderOpenWorkSuppressionAlertAfter)
	}
}

// TestOrderDispatchOpenWorkSuppressionStreaksAreIndependentPerOrder pins that a
// streak belongs to one order. Keying the state by anything coarser — or
// clearing more than the one order that just passed its gate — hands a busy
// city a healthy order that wipes its wedged neighbour's streak on every tick,
// which is the ga-a6zy9 silence this event exists to end.
func TestOrderDispatchOpenWorkSuppressionStreaksAreIndependentPerOrder(t *testing.T) {
	const wedged, healthy = "wedged-neighbor", "healthy-neighbor"
	start := time.Date(2030, 8, 21, 9, 0, 0, 0, time.UTC)
	// The corpus carries only the wedged order's order-run label, so the same
	// store holds one gate shut and leaves the other open on every tick.
	store := newGateBlockingStore(wedged, start.Add(-24*time.Hour))
	rec := &memRecorder{}
	m := suppressedOrderDispatcherFor(t, []orders.Order{
		{Name: wedged, Trigger: "cooldown", Interval: "1s", Exec: "true"},
		{Name: healthy, Trigger: "cooldown", Interval: "1s", Exec: "true"},
	}, store, rec)
	cityPath := t.TempDir()

	now := start
	for i := 0; i < orderOpenWorkSuppressionAlertAfter; i++ {
		m.dispatch(context.Background(), cityPath, now)
		m.drain(context.Background())
		now = now.Add(time.Minute)
	}

	got := rec.suppressedEvents()
	if len(got) != 1 {
		t.Fatalf("order.suppressed events with a healthy order dispatching alongside = %d, want 1", len(got))
	}
	if got[0].Subject != wedged {
		t.Errorf("event subject = %q, want %q", got[0].Subject, wedged)
	}
	if p := decodeSuppressedPayload(t, got[0]); p.Consecutive != orderOpenWorkSuppressionAlertAfter {
		t.Errorf("payload consecutive = %d, want %d (the neighbour's clears must not touch this streak)",
			p.Consecutive, orderOpenWorkSuppressionAlertAfter)
	}
}

// TestOrderDispatchOpenWorkSuppressionStreakResetsWhenTheOrderStopsBeingDue
// covers the other episode boundary. A gate that is never consulted is no
// evidence it is shut, so an undue tick ends the run. Without that, a condition
// order that wedges and then goes false freezes its streak, and the next
// incident — here ten days later — alerts on its first refusal carrying a
// first_suppressed that spans both.
func TestOrderDispatchOpenWorkSuppressionStreakResetsWhenTheOrderStopsBeingDue(t *testing.T) {
	const scoped = "conditional-order"
	start := time.Date(2030, 8, 21, 9, 0, 0, 0, time.UTC)
	store := newGateBlockingStore(scoped, start.Add(-24*time.Hour))
	rec := &memRecorder{}
	flag := filepath.Join(t.TempDir(), "due")
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatalf("writing the condition flag: %v", err)
	}
	m := suppressedOrderDispatcherFor(t, []orders.Order{{
		Name:    scoped,
		Trigger: "condition",
		Check:   "test -f " + flag,
		Exec:    "true",
	}}, store, rec)
	cityPath := t.TempDir()

	now := start
	tick := func() {
		m.dispatch(context.Background(), cityPath, now)
		m.drain(context.Background())
		now = now.Add(time.Minute)
	}

	for i := 0; i < orderOpenWorkSuppressionAlertAfter-1; i++ {
		tick()
	}
	if got := len(rec.suppressedEvents()); got != 0 {
		t.Fatalf("order.suppressed events below the threshold = %d, want 0", got)
	}

	if err := os.Remove(flag); err != nil {
		t.Fatalf("clearing the condition flag: %v", err)
	}
	tick() // the condition goes false: not due, so the episode ends here.

	now = now.Add(10 * 24 * time.Hour)
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatalf("re-arming the condition flag: %v", err)
	}
	secondEpisode := now
	for i := 0; i < orderOpenWorkSuppressionAlertAfter-1; i++ {
		tick()
	}
	if got := len(rec.suppressedEvents()); got != 0 {
		t.Fatalf("order.suppressed events in a second episode short of the threshold = %d, want 0 "+
			"(the first episode's streak must not resume)", got)
	}

	tick()
	got := rec.suppressedEvents()
	if len(got) != 1 {
		t.Fatalf("order.suppressed events once the second episode reaches the threshold = %d, want 1", len(got))
	}
	p := decodeSuppressedPayload(t, got[0])
	if p.Consecutive != orderOpenWorkSuppressionAlertAfter {
		t.Errorf("payload consecutive = %d, want %d", p.Consecutive, orderOpenWorkSuppressionAlertAfter)
	}
	wantSince := secondEpisode.UTC().Format(time.RFC3339)
	if p.FirstSuppressed != wantSince {
		t.Errorf("payload first_suppressed = %q, want the second episode's start %q — an alert anchored to "+
			"the first episode reports a ten-day stall that never happened", p.FirstSuppressed, wantSince)
	}
}

// TestOrderDispatchOpenWorkSuppressionStreakResetsWhenTheRefreshedLastRunUndoesDue
// covers the second undue exit. An order can pass the trigger check against a
// warm last-run cache and then fail it against the refreshed value read from
// the store; that tick is just as undue as the first kind, and skipping the
// reset there would leave a cooldown order — the common case — merging episodes
// exactly as before.
func TestOrderDispatchOpenWorkSuppressionStreakResetsWhenTheRefreshedLastRunUndoesDue(t *testing.T) {
	const scoped = "cooldown-order"
	start := time.Date(2030, 8, 21, 9, 0, 0, 0, time.UTC)
	store := newGateBlockingStore(scoped, start.Add(-24*time.Hour))
	rec := &memRecorder{}
	m := suppressedOrderDispatcherFor(t, []orders.Order{{
		Name:     scoped,
		Trigger:  "cooldown",
		Interval: "10m",
		Exec:     "true",
	}}, store, rec)
	cityPath := t.TempDir()

	now := start
	tick := func() {
		m.dispatch(context.Background(), cityPath, now)
		m.drain(context.Background())
		now = now.Add(time.Minute)
	}

	for i := 0; i < orderOpenWorkSuppressionAlertAfter-1; i++ {
		tick()
	}
	if got := len(rec.suppressedEvents()); got != 0 {
		t.Fatalf("order.suppressed events below the threshold = %d, want 0", got)
	}

	// Something else ran the order: the store's last run jumps inside the
	// cooldown while the cache still holds the old one, so this tick is due on
	// the cached value and undue on the refreshed one.
	store.setCreatedAt(now)
	tick()

	now = now.Add(10 * 24 * time.Hour)
	secondEpisode := now
	for i := 0; i < orderOpenWorkSuppressionAlertAfter-1; i++ {
		tick()
	}
	if got := len(rec.suppressedEvents()); got != 0 {
		t.Fatalf("order.suppressed events in a second episode short of the threshold = %d, want 0 "+
			"(the refreshed-last-run exit must end the first episode)", got)
	}

	tick()
	got := rec.suppressedEvents()
	if len(got) != 1 {
		t.Fatalf("order.suppressed events once the second episode reaches the threshold = %d, want 1", len(got))
	}
	if p := decodeSuppressedPayload(t, got[0]); p.FirstSuppressed != secondEpisode.UTC().Format(time.RFC3339) {
		t.Errorf("payload first_suppressed = %q, want the second episode's start %q",
			p.FirstSuppressed, secondEpisode.UTC().Format(time.RFC3339))
	}
}

// TestCarryOpenWorkSuppressionFromKeepsStreakAcrossDispatcherRebuild pins that
// an order-set rescan does not erase the evidence. Rebuilding the dispatcher
// resets in-memory state, and a stalled order would restart its streak from
// zero on every rebuild — re-hiding exactly the condition this event exists to
// surface.
func TestCarryOpenWorkSuppressionFromKeepsStreakAcrossDispatcherRebuild(t *testing.T) {
	const scoped = "carried-order"
	start := time.Date(2030, 8, 21, 9, 0, 0, 0, time.UTC)
	prev := &memoryOrderDispatcher{}
	for i := 0; i < 5; i++ {
		prev.noteOpenWorkSuppressed(scoped, start.Add(time.Duration(i)*time.Minute))
	}

	next := &memoryOrderDispatcher{aa: []orders.Order{
		{Name: scoped, Trigger: "cooldown", Interval: "1s", Exec: "true"},
	}}
	next.carryOpenWorkSuppressionFrom(prev)

	payload, alert := next.noteOpenWorkSuppressed(scoped, start.Add(5*time.Minute))
	if alert {
		t.Fatal("a carried 5-tick streak must not alert on its 6th suppression")
	}
	if payload.Consecutive != 6 {
		t.Fatalf("consecutive after carry = %d, want 6", payload.Consecutive)
	}
	if payload.FirstSuppressed != start.UTC().Format(time.RFC3339) {
		t.Fatalf("first_suppressed after carry = %q, want the original streak anchor %q",
			payload.FirstSuppressed, start.UTC().Format(time.RFC3339))
	}
}

// TestReplaceOrderDispatcherCarriesOpenWorkSuppression covers the wiring, not
// just the copier: carryOpenWorkSuppressionFrom only protects a rescanning city
// if replaceOrderDispatcher actually calls it.
func TestReplaceOrderDispatcherCarriesOpenWorkSuppression(t *testing.T) {
	const scoped = "rescanned-order"
	start := time.Date(2030, 8, 21, 9, 0, 0, 0, time.UTC)
	prev := &memoryOrderDispatcher{}
	for i := 0; i < orderOpenWorkSuppressionAlertAfter-1; i++ {
		prev.noteOpenWorkSuppressed(scoped, start.Add(time.Duration(i)*time.Minute))
	}

	next := &memoryOrderDispatcher{aa: []orders.Order{
		{Name: scoped, Trigger: "cooldown", Interval: "1s", Exec: "true"},
	}}
	cr := &CityRuntime{od: prev}
	cr.replaceOrderDispatcher(next)

	if cr.od != orderDispatcher(next) {
		t.Fatalf("replaceOrderDispatcher installed %v, want the new dispatcher", cr.od)
	}
	payload, alert := next.noteOpenWorkSuppressed(scoped, start.Add(orderOpenWorkSuppressionAlertAfter*time.Minute))
	if !alert {
		t.Fatal("the carried streak must reach the threshold on the rebuilt dispatcher's next suppression")
	}
	if payload.Consecutive != orderOpenWorkSuppressionAlertAfter {
		t.Fatalf("consecutive after rebuild = %d, want %d", payload.Consecutive, orderOpenWorkSuppressionAlertAfter)
	}
}

// TestCarryOpenWorkSuppressionFromDropsOrdersTheRebuildNoLongerHas bounds the
// map. clearOpenWorkSuppression is the only delete site and it only ever names
// a live order, so an order removed, renamed, rescoped, disabled or switched to
// no_work_gate while suppressed leaves an entry nothing can reach — carried
// forward across every rebuild for the life of the process.
func TestCarryOpenWorkSuppressionFromDropsOrdersTheRebuildNoLongerHas(t *testing.T) {
	const kept, dropped = "kept-order", "deleted-order"
	start := time.Date(2030, 8, 21, 9, 0, 0, 0, time.UTC)
	prev := &memoryOrderDispatcher{}
	for i := 0; i < 5; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		prev.noteOpenWorkSuppressed(kept, at)
		prev.noteOpenWorkSuppressed(dropped, at)
	}

	next := &memoryOrderDispatcher{aa: []orders.Order{
		{Name: kept, Trigger: "cooldown", Interval: "1s", Exec: "true"},
	}}
	next.carryOpenWorkSuppressionFrom(prev)

	at := start.Add(5 * time.Minute)
	if p, _ := next.noteOpenWorkSuppressed(kept, at); p.Consecutive != 6 {
		t.Errorf("consecutive for an order the rebuild still carries = %d, want 6", p.Consecutive)
	}
	if p, _ := next.noteOpenWorkSuppressed(dropped, at); p.Consecutive != 1 {
		t.Errorf("consecutive for an order the rebuild dropped = %d, want 1 — its stale streak survived the carry",
			p.Consecutive)
	}
}

// TestOrderOpenWorkSuppressionConstantsMatchTheirDocumentedContract pins the two
// numbers themselves. Every other test here loops on the constants, so they pin
// the relationships and not the values: a retune to 200 refusals or a weekly
// repeat would ship green while quietly voiding what the doc comments promise
// operators.
func TestOrderOpenWorkSuppressionConstantsMatchTheirDocumentedContract(t *testing.T) {
	patrol := (&config.DaemonConfig{}).PatrolIntervalDuration()
	if got := time.Duration(orderOpenWorkSuppressionAlertAfter) * patrol; got != 10*time.Minute {
		t.Errorf("grace before the first alert at the default %s patrol interval = %s, want 10m", patrol, got)
	}
	if orderOpenWorkSuppressionRepeat != time.Hour {
		t.Errorf("repeat window = %s, want 1h", orderOpenWorkSuppressionRepeat)
	}
}
