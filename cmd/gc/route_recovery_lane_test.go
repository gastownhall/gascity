package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storeref"
)

const routeRecoveryTestPool = "gascity/gastown.polecat"

// countingRouteStore counts the store round trips a route-recovery pass issues.
//
// Round trips, not wall clock: on maintainer-city the work ledger answers in
// ~5.4s, so "how many sequential reads does this leg make" IS the leg's latency,
// and it is the only form of that measurement a unit test can make deterministic.
type countingRouteStore struct {
	beads.Store
	lists  int
	gets   int
	writes int
	// scanned records whether any List asked for the whole open corpus, which is
	// the read this slice moved off the tick.
	scanned int
}

func (s *countingRouteStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	if q.AllowScan && len(q.IDs) == 0 {
		s.scanned++
	}
	return s.Store.List(q)
}

func (s *countingRouteStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

func (s *countingRouteStore) SetMetadata(id, key, value string) error {
	s.writes++
	return s.Store.SetMetadata(id, key, value)
}

func (s *countingRouteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.writes++
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *countingRouteStore) reads() int { return s.lists + s.gets }

// unroutedWorkBead is the recoverable shape: open, unassigned, kind-less, a pool
// route in gc.run_target and nothing in gc.routed_to.
func unroutedWorkBead(id string) beads.Bead {
	return beads.Bead{ID: id, Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.RunTargetMetadataKey: routeRecoveryTestPool,
	}}
}

// routeRecoveryRuntime builds a runtime over one counting city store and no rigs.
func routeRecoveryRuntime(t *testing.T, seed ...beads.Bead) (*CityRuntime, *countingRouteStore) {
	t.Helper()
	store := &countingRouteStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
	cr := &CityRuntime{
		cityName:            "city",
		standaloneCityStore: store,
		stderr:              io.Discard,
	}
	return cr, store
}

func beadCreatedEvent(t *testing.T, b beads.Bead) events.Event {
	t.Helper()
	payload, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bead payload: %v", err)
	}
	return events.Event{Type: events.BeadCreated, Subject: b.ID, Payload: payload}
}

// TestRouteRecoverySteadyTickIssuesZeroWorkLedgerReads is the slice's headline
// property, in the unit the incident was measured in.
//
// recover_unrouted_work_routes was 185.3s +/- 0.7s of a ~360s controller tick:
// a full live open-corpus scan of the work ledger, every tick, discovering
// nothing (ga-l7jdg; the mc discriminator found zero restores across 16h and
// zero gc.run_target beads in the open corpus). A tick that names no candidate
// must now touch the ledger zero times.
//
// The control is the same store on the SAME data one line later: the backstop
// scans it and repairs the bead. Without that, "zero reads" would be satisfied
// by a lane that had simply stopped working.
func TestRouteRecoverySteadyTickIssuesZeroWorkLedgerReads(t *testing.T) {
	cr, store := routeRecoveryRuntime(t, unroutedWorkBead("T-1"))

	for range 4 {
		report := cr.recoverUnroutedWorkRoutesDelta()
		if report.legReads != 0 {
			t.Fatalf("steady tick reported %d leg read(s), want 0", report.legReads)
		}
	}
	if store.reads() != 0 {
		t.Fatalf("steady ticks issued %d work-ledger read(s) (%d List, %d Get), want 0", store.reads(), store.lists, store.gets)
	}
	if store.writes != 0 {
		t.Fatalf("steady ticks issued %d write(s), want 0", store.writes)
	}
	// The delta lane really is delta: the repairable bead is still unrepaired.
	if got := mustRoutedTo(t, store, "T-1"); got != "" {
		t.Fatalf("T-1 gc.routed_to = %q after delta-only ticks, want empty", got)
	}

	// Control: the backstop over the identical store DOES scan and DOES repair.
	// A lane that lost the ability to read at all would fail here and pass above.
	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if report.restored != 1 {
		t.Fatalf("backstop restored %d, want 1 — the zero-read assertion above measured a lane that cannot repair", report.restored)
	}
	if store.scanned == 0 {
		t.Fatal("backstop issued no open-corpus scan; the scan was deleted rather than demoted")
	}
	if got := mustRoutedTo(t, store, "T-1"); got != routeRecoveryTestPool {
		t.Fatalf("T-1 gc.routed_to = %q after backstop, want %q", got, routeRecoveryTestPool)
	}
}

// TestRouteRecoveryDeltaRepairsOnlyEventNamedBeadsAndBatchesTheReVerify pins
// both halves of the delta pass: it touches only what the journal named, and it
// re-verifies the whole batch in ONE round trip rather than one per bead.
func TestRouteRecoveryDeltaRepairsOnlyEventNamedBeadsAndBatchesTheReVerify(t *testing.T) {
	seed := []beads.Bead{
		unroutedWorkBead("T-1"), unroutedWorkBead("T-2"),
		unroutedWorkBead("T-3"), unroutedWorkBead("T-4"),
	}
	cr, store := routeRecoveryRuntime(t, seed...)
	lane := cr.routeRecoveryLaneOf()
	named := []string{"T-1", "T-2", "T-3"}
	for _, id := range named {
		lane.observe(beadCreatedEvent(t, unroutedWorkBead(id)))
	}

	report := cr.recoverUnroutedWorkRoutesDelta()
	if report.restored != len(named) {
		t.Fatalf("delta restored %d, want %d", report.restored, len(named))
	}
	if store.scanned != 0 {
		t.Fatalf("delta pass issued %d open-corpus scan(s), want 0", store.scanned)
	}
	// One batched re-verify for the whole candidate set, not one read per bead.
	if store.reads() != 1 {
		t.Fatalf("delta pass issued %d read(s) for %d candidates, want 1 batched IN-list read", store.reads(), len(named))
	}
	// Control: the same measurement on a store that was never batched would
	// scale with the candidate count. Two candidates and three candidates must
	// cost the same one read, and the un-named bead must be untouched.
	if got := mustRoutedTo(t, store, "T-4"); got != "" {
		t.Fatalf("T-4 gc.routed_to = %q, want empty — the delta pass repaired a bead no event named", got)
	}
	for _, id := range named {
		if got := mustRoutedTo(t, store, id); got != routeRecoveryTestPool {
			t.Fatalf("%s gc.routed_to = %q, want %q", id, got, routeRecoveryTestPool)
		}
	}
}

// TestRouteRecoveryDeltaReadCountDoesNotScaleWithCandidates is the batching
// control the design's counting-store precedent requires: the un-batched shape
// must cost strictly more, or the assertion above is not measuring anything.
func TestRouteRecoveryDeltaReadCountDoesNotScaleWithCandidates(t *testing.T) {
	readsFor := func(n int) int {
		var seed []beads.Bead
		cr, store := routeRecoveryRuntime(t)
		lane := cr.routeRecoveryLaneOf()
		for i := range n {
			b := unroutedWorkBead(string(rune('A' + i)))
			seed = append(seed, b)
			lane.observe(beadCreatedEvent(t, b))
		}
		store.Store = beads.NewMemStoreFrom(0, seed, nil)
		cr.recoverUnroutedWorkRoutesDelta()
		return store.reads()
	}
	two, eight := readsFor(2), readsFor(8)
	if two != eight {
		t.Fatalf("2 candidates cost %d read(s) and 8 cost %d; the re-verify is still per-bead", two, eight)
	}
	if eight != 1 {
		t.Fatalf("8 candidates cost %d read(s), want exactly 1 batched re-verify", eight)
	}
	// Control: a single candidate takes the Get fast path, which is one read
	// too — so the equality above is not the trivial "every count is zero".
	if one := readsFor(1); one != 1 {
		t.Fatalf("1 candidate cost %d read(s), want 1", one)
	}
}

// TestRouteRecoveryBackstopHealsWhatTheEventFeedLost is the convergence-doctrine
// control, and the reason the backstop is not optional.
//
// Events CAN be lost: an agent's bd write reaches the journal through a hook
// chain that can be killed, the codex exec host truncates mid-command, archives
// rotate. So the fixture mutates the store BEHIND the lane's back — no event —
// and asserts the two halves of the doctrine separately: the delta lane does not
// see it (proving the delta path is really delta, not a disguised scan), and the
// backstop repairs it exactly once (proving convergence, and idempotency).
func TestRouteRecoveryBackstopHealsWhatTheEventFeedLost(t *testing.T) {
	// The lost event: the store holds a repairable bead the journal never
	// announced, so the lane's feed has no candidate for it and never will.
	cr, store := routeRecoveryRuntime(t, unroutedWorkBead("T-lost"))

	for range 3 {
		if report := cr.recoverUnroutedWorkRoutesDelta(); report.restored != 0 {
			t.Fatalf("delta pass restored %d for an unannounced bead, want 0", report.restored)
		}
	}
	if got := mustRoutedTo(t, store, "T-lost"); got != "" {
		t.Fatalf("T-lost gc.routed_to = %q after delta ticks, want empty (the delta path is scanning)", got)
	}

	first := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if first.restored != 1 {
		t.Fatalf("first backstop restored %d, want 1", first.restored)
	}
	if got := mustRoutedTo(t, store, "T-lost"); got != routeRecoveryTestPool {
		t.Fatalf("T-lost gc.routed_to = %q after backstop, want %q", got, routeRecoveryTestPool)
	}

	writesAfterHeal := store.writes
	second := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if second.restored != 0 {
		t.Fatalf("second backstop restored %d, want 0 (not idempotent)", second.restored)
	}
	if store.writes != writesAfterHeal {
		t.Fatalf("second backstop issued %d extra write(s), want 0", store.writes-writesAfterHeal)
	}
}

// TestRouteRecoveryBackstopHealsABindingResidentLossOnAConvergedCity is the
// binding half of the convergence doctrine, and it is the case that makes the
// doctrine load-bearing rather than decorative.
//
// On a converged split city the operator ruling puts ALL routed work in the
// graph binding, and the runtime plane reads only that binding — delta-only, by
// construction. So if the binding had no convergence scan behind it, a
// binding-resident bead whose gc.routed_to is lost with a dropped journal event
// would be invisible to the pool FOREVER: no tick would name it, and no sweep
// would look. Hourly is the price; never is not an option.
//
// Fixture shape is the same lost-event one the ledger case uses: the store holds
// a repairable bead the journal never announced.
func TestRouteRecoveryBackstopHealsABindingResidentLossOnAConvergedCity(t *testing.T) {
	binding := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("GB-lost")}, nil)}
	work := &countingRouteStore{Store: beads.NewMemStore()}
	topo := assembleResidencyTopology(&config.City{}, work, nil,
		[]storeref.ClassBinding{{
			Classes: []coordclass.Class{coordclass.ClassGraph},
			Leg:     storeref.Leg{Ref: storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: binding},
		}}, nil)
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		t.Fatalf("Plan(RoutedWork): %v", err)
	}
	lane := newRouteRecoveryLane()

	// No event ever names it, so no number of ticks repairs it. This is the
	// delta lane being honestly delta — and it is why the sweep is not optional.
	for range 3 {
		if report := lane.deltaPass(plan, nil); report.restored != 0 {
			t.Fatalf("a delta pass restored %d for an unannounced bead, want 0", report.restored)
		}
	}
	if got := mustRoutedTo(t, binding, "GB-lost"); got != "" {
		t.Fatalf("GB-lost gc.routed_to = %q after delta ticks, want empty (the delta path is scanning)", got)
	}

	first := lane.backstopPass(plan, backstopReasonCadence)
	if first.restored != 1 {
		t.Fatalf("the convergence pass restored %d, want 1 — a binding-resident loss has no other repair path", first.restored)
	}
	if got := mustRoutedTo(t, binding, "GB-lost"); got != routeRecoveryTestPool {
		t.Fatalf("GB-lost gc.routed_to = %q after the convergence pass, want %q", got, routeRecoveryTestPool)
	}

	// Idempotency: converging twice writes once.
	writesAfterHeal := binding.writes
	if second := lane.backstopPass(plan, backstopReasonCadence); second.restored != 0 {
		t.Fatalf("the second convergence pass restored %d, want 0", second.restored)
	}
	if binding.writes != writesAfterHeal {
		t.Fatalf("the second convergence pass issued %d extra write(s), want 0", binding.writes-writesAfterHeal)
	}
}

// TestRouteRecoveryCursorGapForcesTheBackstop pins the schedule half: every way
// the event feed can stop naming every change must make the authoritative scan
// due immediately, not at the next hourly tick.
func TestRouteRecoveryCursorGapForcesTheBackstop(t *testing.T) {
	now := time.Now()
	lane := newRouteRecoveryLane()

	// A lane that has never scanned is due, because nothing has converged yet.
	if reason, due := lane.backstopDue(now); !due || reason != backstopReasonStartup {
		t.Fatalf("fresh lane due=%v reason=%q, want due with reason %q", due, reason, backstopReasonStartup)
	}
	lane.noteBackstopRan(now, backstopReasonCadence, false)
	// Control: right after a clean pass it is NOT due, so "always due" is not
	// what the assertions below are measuring.
	if _, due := lane.backstopDue(now.Add(time.Minute)); due {
		t.Fatal("backstop due one minute after a clean pass; the cadence gate is not gating")
	}
	if _, due := lane.backstopDue(now.Add(routeRecoveryBackstopInterval)); !due {
		t.Fatal("backstop not due at its cadence")
	}

	// A cursor gap makes it due immediately, with the reason recorded.
	lane.force(backstopReasonCursorGap)
	reason, due := lane.backstopDue(now.Add(time.Second))
	if !due || reason != backstopReasonCursorGap {
		t.Fatalf("after a forced gap due=%v reason=%q, want due with reason %q", due, reason, backstopReasonCursorGap)
	}

	// A pass that could not read every leg comes back on the short retry
	// cadence, not the hourly one: the leg it missed is the overdue one.
	lane.noteBackstopRan(now, backstopReasonCadence, true)
	if _, due := lane.backstopDue(now.Add(routeRecoveryBackstopRetryInterval)); !due {
		t.Fatal("a partial pass did not reschedule on the retry cadence")
	}
	if _, due := lane.backstopDue(now.Add(routeRecoveryBackstopRetryInterval / 2)); due {
		t.Fatal("a partial pass rescheduled sooner than the retry cadence; that is a spin")
	}
}

// TestRouteRecoveryCandidateOverflowFallsBackToTheScan pins the other cursor-gap
// shape: a feed that produced more candidates than the lane will hold can no
// longer claim to name everything, so the scan answers instead of candidates
// being silently dropped.
func TestRouteRecoveryCandidateOverflowFallsBackToTheScan(t *testing.T) {
	lane := newRouteRecoveryLane()
	lane.noteBackstopRan(time.Now(), backstopReasonCadence, false)
	for i := range routeRecoveryCandidateCap + 1 {
		lane.observe(beadCreatedEvent(t, unroutedWorkBead(overflowBeadID(i))))
	}
	if reason, due := lane.backstopDue(time.Now()); !due || reason != backstopReasonCursorGap {
		t.Fatalf("after candidate overflow due=%v reason=%q, want due with reason %q", due, reason, backstopReasonCursorGap)
	}
	// Control: under the cap the feed keeps its candidates and does not force.
	small := newRouteRecoveryLane()
	small.noteBackstopRan(time.Now(), backstopReasonCadence, false)
	small.observe(beadCreatedEvent(t, unroutedWorkBead("T-1")))
	if _, due := small.backstopDue(time.Now()); due {
		t.Fatal("a single candidate forced the backstop; overflow is not what the assertion above measured")
	}
	if got := small.takePending(); len(got) != 1 || got[0] != "T-1" {
		t.Fatalf("pending = %v, want [T-1]", got)
	}
}

func overflowBeadID(i int) string {
	return "T-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
}

// TestRouteRecoveryEventFeedNamesCandidatesAndForcesOnAMissingJournal pins the
// feed itself: a live journal produces candidates, and no journal at all forces
// the scan rather than leaving the city with a delta lane that sees nothing.
//
// The wait is a happens-before, not a timeout. The feed loop is
// `Next() -> observe() -> Next()`, so a SECOND Next call proves the first
// event was observed. Polling with a sleep would test the same thing more
// slowly and flake on a loaded machine.
func TestRouteRecoveryEventFeedNamesCandidatesAndForcesOnAMissingJournal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backing := events.NewFake()
	backing.Record(beadCreatedEvent(t, unroutedWorkBead("T-1")))
	// A bead with no recoverable route must not become a candidate: ordinary
	// bead traffic on a busy city has to cost the tick nothing.
	backing.Record(beadCreatedEvent(t, beads.Bead{ID: "T-2", Status: "open"}))
	prov := &observedEventProvider{Provider: backing, observed: make(chan struct{}, 8), after: 2}

	lane := newRouteRecoveryLane()
	lane.noteBackstopRan(time.Now(), backstopReasonCadence, false)
	// The feed watches from the CURRENT head, so rewind it to replay the two
	// events already recorded — the production lane's head is set before the
	// startup scan for the same reason.
	prov.watchFrom = 0
	lane.startEventFeed(ctx, prov)

	select {
	case <-prov.observed:
	case <-time.After(10 * time.Second):
		t.Fatal("the feed never consumed a second event; it is not reaching observe")
	}
	pending := lane.takePending()
	if len(pending) != 1 || pending[0] != "T-1" {
		t.Fatalf("feed produced candidates %v, want exactly [T-1]", pending)
	}
	if _, due := lane.backstopDue(time.Now()); due {
		t.Fatal("a healthy feed forced the backstop")
	}

	// No journal: the lane must say so by forcing the scan.
	blind := newRouteRecoveryLane()
	blind.noteBackstopRan(time.Now(), backstopReasonCadence, false)
	blind.startEventFeed(ctx, nil)
	if reason, due := blind.backstopDue(time.Now()); !due || reason != backstopReasonCursorGap {
		t.Fatalf("a lane with no journal due=%v reason=%q, want due with reason %q", due, reason, backstopReasonCursorGap)
	}
}

// observedEventProvider signals once the feed has asked for its Nth event, which
// is the happens-before for "everything before it was observed".
type observedEventProvider struct {
	events.Provider
	observed  chan struct{}
	after     int
	watchFrom uint64
}

func (p *observedEventProvider) LatestSeq() (uint64, error) { return p.watchFrom, nil }

func (p *observedEventProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	inner, err := p.Provider.Watch(ctx, afterSeq)
	if err != nil {
		return nil, err
	}
	return &observedWatcher{Watcher: inner, owner: p}, nil
}

type observedWatcher struct {
	events.Watcher
	owner *observedEventProvider
	calls int
}

func (w *observedWatcher) Next() (events.Event, error) {
	w.calls++
	if w.calls == w.owner.after {
		select {
		case w.owner.observed <- struct{}{}:
		default:
		}
	}
	return w.Watcher.Next()
}

// recheckDropStore models a candidate the open scan reports but the
// authoritative live re-verify refuses to confirm — decomposition (A)'s shape,
// where a candidate re-enters the set every pass and is never repaired.
type recheckDropStore struct {
	beads.Store
	drop map[string]bool
}

func (s *recheckDropStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(q)
	if err != nil || len(q.IDs) == 0 {
		return rows, err
	}
	kept := rows[:0]
	for _, b := range rows {
		if !s.drop[b.ID] {
			kept = append(kept, b)
		}
	}
	return kept, nil
}

func (s *recheckDropStore) Get(id string) (beads.Bead, error) {
	if s.drop[id] {
		return beads.Bead{}, beads.ErrNotFound
	}
	return s.Store.Get(id)
}

// TestRouteRecoveryQuarantinesACandidateOnlyAfterTwoFailedBackstopPasses pins
// the relic-convergence handoff: a candidate the live re-check keeps refusing is
// marked for the operator instead of being re-read forever in silence — but not
// on the first failure, which is the ordinary claim race the re-check exists for.
func TestRouteRecoveryQuarantinesACandidateOnlyAfterTwoFailedBackstopPasses(t *testing.T) {
	backing := beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("T-relic"), unroutedWorkBead("T-fine")}, nil)
	store := &recheckDropStore{Store: backing, drop: map[string]bool{"T-relic": true}}
	cr := &CityRuntime{cityName: "city", standaloneCityStore: store, stderr: io.Discard}

	first := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if first.quarantined != 0 {
		t.Fatalf("first backstop quarantined %d, want 0 — one failed re-check is the ordinary claim race", first.quarantined)
	}
	if q := quarantineReason(t, backing, "T-relic"); q != "" {
		t.Fatalf("T-relic quarantine reason after one pass = %q, want empty", q)
	}

	second := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if second.quarantined != 1 {
		t.Fatalf("second backstop quarantined %d, want 1", second.quarantined)
	}
	if q := quarantineReason(t, backing, "T-relic"); q != routeRecoveryQuarantineRecheckFailed {
		t.Fatalf("T-relic quarantine reason = %q, want %q", q, routeRecoveryQuarantineRecheckFailed)
	}
	// Control: quarantine never applies to a bead whose re-check passes. T-fine
	// went through the identical passes and is repaired, not marked.
	if q := quarantineReason(t, backing, "T-fine"); q != "" {
		t.Fatalf("T-fine quarantine reason = %q, want empty (its re-check passed)", q)
	}
	if got := mustRoutedTo(t, backing, "T-fine"); got != routeRecoveryTestPool {
		t.Fatalf("T-fine gc.routed_to = %q, want %q", got, routeRecoveryTestPool)
	}

	// Quarantine is a LABEL, not a skip: the bead stays a candidate, and the
	// pass whose re-check finally passes clears the marker on its own.
	store.drop = nil
	third := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if third.restored != 1 {
		t.Fatalf("third backstop restored %d, want 1 — a quarantined bead must re-enter", third.restored)
	}
	if q := quarantineReason(t, backing, "T-relic"); q != "" {
		t.Fatalf("T-relic quarantine reason after a passing re-check = %q, want cleared", q)
	}
	if got := mustRoutedTo(t, backing, "T-relic"); got != routeRecoveryTestPool {
		t.Fatalf("T-relic gc.routed_to = %q, want %q", got, routeRecoveryTestPool)
	}
}

// clearRouteOnWriteStore is the flap fixture: a sibling lane that consumes
// gc.routed_to as fast as this one restores it — the blocked-routed-reaper shape
// the pre-lane scanner would have chased forever at one restore per tick.
type clearRouteOnWriteStore struct {
	beads.Store
	target string
}

func (s *clearRouteOnWriteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.Store.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	if id == s.target {
		if _, ok := kvs[beadmeta.RoutedToMetadataKey]; ok {
			return s.SetMetadata(id, beadmeta.RoutedToMetadataKey, "")
		}
	}
	return nil
}

// TestRouteRecoveryBoundsRestampsAndReportsAFlap covers decomposition (B): if
// the 185s had been a restore/clear loop, an event-driven lane would still see
// churn every pass, and a faster treadmill would be no fix at all. So the lane
// bounds its re-stamps of one bead, stops writing, and says which bead and that
// something else is clearing the route.
func TestRouteRecoveryBoundsRestampsAndReportsAFlap(t *testing.T) {
	backing := beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("T-flap"), unroutedWorkBead("T-calm")}, nil)
	store := &clearRouteOnWriteStore{Store: backing, target: "T-flap"}
	cr := &CityRuntime{cityName: "city", standaloneCityStore: store, stderr: io.Discard}

	for pass := 1; pass <= routeRecoveryFlapLimit; pass++ {
		report := cr.runRouteRecoveryBackstop(backstopReasonCadence)
		if len(report.flapping) != 0 {
			t.Fatalf("pass %d reported a flap at or below the bound (%v)", pass, report.flapping)
		}
	}
	over := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if len(over.flapping) != 1 || over.flapping[0] != "T-flap" {
		t.Fatalf("pass %d reported flapping=%v, want [T-flap]", routeRecoveryFlapLimit+1, over.flapping)
	}
	if over.restored != 0 {
		t.Fatalf("pass %d still restored %d bead(s); the re-stamp bound does not bound", routeRecoveryFlapLimit+1, over.restored)
	}
	if over.outcome() == TraceOutcomeComplete {
		t.Fatal("a flapping pass reported a complete outcome; the trace cannot show the defect")
	}
	if q := quarantineReason(t, backing, "T-flap"); q != routeRecoveryQuarantineRestoreFlap {
		t.Fatalf("T-flap quarantine reason = %q, want %q", q, routeRecoveryQuarantineRestoreFlap)
	}
	// Control: the bead nobody clears is restored once, never reported flapping,
	// and never quarantined — the bound is per bead, not a global circuit breaker.
	if q := quarantineReason(t, backing, "T-calm"); q != "" {
		t.Fatalf("T-calm quarantine reason = %q, want empty", q)
	}
	if got := mustRoutedTo(t, backing, "T-calm"); got != routeRecoveryTestPool {
		t.Fatalf("T-calm gc.routed_to = %q, want %q", got, routeRecoveryTestPool)
	}
}

// TestRouteRecoveryRuntimePlaneReadsTheBindingAndNeverTheLedger is the operator
// invariant as a test (ga-l7jdg, bd memory gascity-runtime-infra-store-invariant).
//
// Every bd operation on the runtime plane — ticks, hooks, claims, sweeps —
// touches the infra/class binding ONLY. A work-ledger leg on the tick is a
// misrouting bug by definition, not a cost to amortize; it is why this leg cost
// 185s of a 360s tick in the first place. So the tick's delta pass reads the
// binding and the ledger sees zero round trips, and the mirror-image convergence
// lane — off the tick, on its own cadence — is the only thing that reads the
// ledger at all.
//
// Each half is the other's control: "the ledger was not read" from a lane that
// reads nothing would be indistinguishable from correctness, and the second
// assertion in each plane is what makes it distinguishable.
func TestRouteRecoveryRuntimePlaneReadsTheBindingAndNeverTheLedger(t *testing.T) {
	newPlan := func(t *testing.T) (storeref.ResolvedPlan, *countingRouteStore, *countingRouteStore, *countingRouteStore) {
		t.Helper()
		work := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("CW-1")}, nil)}
		rig := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("RW-1")}, nil)}
		binding := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("GB-1")}, nil)}
		topo := assembleResidencyTopology(
			&config.City{Rigs: []config.Rig{{Name: "gascity", Path: "rigs/gascity"}}},
			work,
			map[string]beads.Store{"gascity": rig},
			[]storeref.ClassBinding{{
				Classes: []coordclass.Class{coordclass.ClassGraph},
				Leg:     storeref.Leg{Ref: storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: binding},
			}},
			nil,
		)
		plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
		if err != nil {
			t.Fatalf("Plan(RoutedWork): %v", err)
		}
		if !plan.TouchesBinding() {
			t.Fatal("the plan carries no binding leg; this fixture cannot express the invariant")
		}
		return plan, work, rig, binding
	}

	t.Run("runtime plane", func(t *testing.T) {
		plan, work, rig, binding := newPlan(t)
		report := newRouteRecoveryLane().backstopPassOnPlane(plan, backstopReasonCadence, runtimePlane)
		if got := work.reads() + rig.reads(); got != 0 {
			t.Fatalf("the runtime plane issued %d work-ledger/rig round trip(s), want 0 — a ledger leg on the tick is a misrouting bug", got)
		}
		// Control: it read the binding and repaired what lives there, so the zero
		// above is a routing statement and not a lane that does nothing.
		if binding.reads() == 0 || report.restored != 1 {
			t.Fatalf("binding reads=%d restored=%d, want a read and one repair", binding.reads(), report.restored)
		}
		if got := mustRoutedTo(t, binding, "GB-1"); got != routeRecoveryTestPool {
			t.Fatalf("GB-1 gc.routed_to = %q, want %q", got, routeRecoveryTestPool)
		}
		if got := mustRoutedTo(t, work, "CW-1"); got != "" {
			t.Fatalf("CW-1 gc.routed_to = %q, want empty on the runtime plane", got)
		}
	})

	t.Run("reconcile plane converges every leg, binding included", func(t *testing.T) {
		plan, work, rig, binding := newPlan(t)
		report := newRouteRecoveryLane().backstopPassOnPlane(plan, backstopReasonCadence, reconcilePlane)
		if work.reads() == 0 || rig.reads() == 0 {
			t.Fatalf("work reads=%d rig reads=%d, want both non-zero", work.reads(), rig.reads())
		}
		// The binding is NOT exempt. On a converged city every routed bead lives
		// there, so a binding the convergence lane skips is a binding with no
		// convergence at all: the runtime plane is delta-only, and one dropped
		// journal event would strand a bead permanently. One local sqlite scan an
		// hour is the entire price of closing that.
		if binding.reads() == 0 {
			t.Fatal("the convergence lane never read the binding; on a converged city that is the ONLY store holding routed work, and nothing else scans it")
		}
		if report.restored != 3 {
			t.Fatalf("backstop restored %d, want 3 (work, rig and binding legs all converged)", report.restored)
		}
		for _, row := range []struct {
			store *countingRouteStore
			id    string
		}{{work, "CW-1"}, {rig, "RW-1"}, {binding, "GB-1"}} {
			if got := mustRoutedTo(t, row.store, row.id); got != routeRecoveryTestPool {
				t.Fatalf("%s gc.routed_to = %q after the convergence pass, want %q", row.id, got, routeRecoveryTestPool)
			}
		}
	})
}

// TestRouteRecoveryDeltaPassRefusesTheLedgerLeg pins the invariant on the lane
// the tick actually calls, not just on the plane helper.
func TestRouteRecoveryDeltaPassRefusesTheLedgerLeg(t *testing.T) {
	work := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("CW-1")}, nil)}
	binding := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("GB-1")}, nil)}
	topo := assembleResidencyTopology(&config.City{}, work, nil,
		[]storeref.ClassBinding{{
			Classes: []coordclass.Class{coordclass.ClassGraph},
			Leg:     storeref.Leg{Ref: storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: binding},
		}}, nil)
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		t.Fatalf("Plan(RoutedWork): %v", err)
	}

	lane := newRouteRecoveryLane()
	report := lane.deltaPass(plan, []string{"CW-1", "GB-1"})
	if work.reads() != 0 {
		t.Fatalf("the delta pass issued %d work-ledger round trip(s), want 0", work.reads())
	}
	// Control: the binding-resident candidate WAS repaired, so the zero above is
	// the ledger being refused rather than the pass being skipped.
	if report.restored != 1 || binding.reads() == 0 {
		t.Fatalf("delta restored %d with %d binding read(s), want 1 and non-zero", report.restored, binding.reads())
	}
}

// TestRouteRecoverySingleStoreCityKeepsItsOnlyLeg guards the degradation the
// invariant must not break: a city that relocates no class has no binding, and
// there the work store IS the infra store. Reading "bindings only" literally
// would silently disable the delta lane on every such city.
func TestRouteRecoverySingleStoreCityKeepsItsOnlyLeg(t *testing.T) {
	work := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("CW-1")}, nil)}
	topo := assembleResidencyTopology(&config.City{}, work, nil, nil, nil)
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		t.Fatalf("Plan(RoutedWork): %v", err)
	}
	if plan.TouchesBinding() {
		t.Fatal("the single-store fixture grew a binding; it is not testing the degradation")
	}
	report := newRouteRecoveryLane().deltaPass(plan, []string{"CW-1"})
	if report.restored != 1 {
		t.Fatalf("delta restored %d on a single-store city, want 1", report.restored)
	}
}

// TestRouteRecoveryAgreesWithDemandAndSweepOnItsLegs is the reader-agreement row
// for the repair surface, and it is the reason this lane consumes the resolver
// instead of a list of its own.
//
// The D1-D9 divergence class is one shape: two surfaces that must see the same
// bead read different store sets. Route repair is a third such surface — a
// gc.routed_to restored on a leg demand does not read is a repair nobody
// consumes, and a leg demand reads but repair never converges is a bead that
// stays invisible to the pool forever.
//
// The CONVERGENCE lane therefore agrees with the demand plan exactly: every leg,
// in the plan's order, nothing subtracted. The RUNTIME lane is the deliberate
// narrowing — the operator invariant — and its legs must be a strict subset of
// the same plan, never a set of its own.
func TestRouteRecoveryAgreesWithDemandAndSweepOnItsLegs(t *testing.T) {
	topo := assembleResidencyTopology(
		&config.City{Rigs: []config.Rig{{Name: "alpha", Path: "rigs/alpha"}, {Name: "beta", Path: "rigs/beta"}}},
		beads.NewMemStore(),
		map[string]beads.Store{"alpha": beads.NewMemStore(), "beta": beads.NewMemStore()},
		[]storeref.ClassBinding{{
			Classes: []coordclass.Class{coordclass.ClassGraph},
			Leg:     storeref.Leg{Ref: storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: beads.NewMemStore()},
		}},
		nil,
	)

	demand, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		t.Fatalf("Plan(RoutedWork): %v", err)
	}
	sweep, err := storeref.Plan(storeref.AssignedWork{}, topo)
	if err != nil {
		t.Fatalf("Plan(AssignedWork): %v", err)
	}
	if demand.String() != sweep.String() {
		t.Fatalf("demand and sweep disagree before repair is even considered:\n %s\n %s", demand.String(), sweep.String())
	}

	visit := func(plane storePlane) []string {
		t.Helper()
		var seen []string
		if _, walkErr := walkPlaneLegs(demand, plane, func(leg planeLeg) error {
			seen = append(seen, leg.label)
			return nil
		}); walkErr != nil {
			t.Fatalf("walking the repair legs: %v", walkErr)
		}
		return seen
	}

	// The convergence lane reads the demand plan whole — no leg of the surface
	// that COUNTS work is a leg nothing converges.
	converged := visit(reconcilePlane)
	want := []string{"city", "rig alpha", "rig beta", string(storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}))}
	if len(converged) != len(want) {
		t.Fatalf("the convergence lane visited %v, want %v", converged, want)
	}
	for i := range want {
		if converged[i] != want[i] {
			t.Fatalf("the convergence lane visited %v, want %v (order is the resolver's, not this lane's)", converged, want)
		}
	}
	if len(converged) != len(demand.Legs) {
		t.Fatalf("the convergence lane visited %d of the plan's %d legs; a leg it skips is a leg with no convergence", len(converged), len(demand.Legs))
	}

	// The runtime lane narrows to the binding — and it narrows the SAME plan
	// rather than substituting a list of its own.
	runtime := visit(runtimePlane)
	if len(runtime) != 1 || !storeref.IsClassRef(runtime[0]) {
		t.Fatalf("the runtime lane visited %v, want exactly the class binding", runtime)
	}
	// Control: a subset, strictly. Equal sets would mean the invariant is not
	// being applied; a leg outside the plan would mean it is not the resolver's.
	if len(runtime) >= len(converged) {
		t.Fatalf("the runtime lane visited %d leg(s) and the convergence lane %d; the narrowing is not narrowing", len(runtime), len(converged))
	}
	for _, label := range runtime {
		if !slices.Contains(converged, label) {
			t.Fatalf("the runtime lane visited %q, which is on no leg of the demand plan", label)
		}
	}
}

func quarantineReason(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if !isRouteRecoveryQuarantined(b) {
		return ""
	}
	return b.Metadata[beadmeta.RouteQuarantineReasonMetadataKey]
}

// TestRouteRecoveryBackstopAlwaysReportsItselfAndItsAge pins the observability
// contract for the convergence lane. It runs on a background goroutine, so a
// clean pass that logs nothing is indistinguishable from a lane that stopped —
// and this is the lane whose whole job is to notice things nothing else notices.
func TestRouteRecoveryBackstopAlwaysReportsItselfAndItsAge(t *testing.T) {
	var stderr bytes.Buffer
	store := &countingRouteStore{Store: beads.NewMemStore()} // nothing to repair
	cr := &CityRuntime{cityName: "city", standaloneCityStore: store, logPrefix: "gc", stderr: &stderr}

	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if report.restored != 0 {
		t.Fatalf("the fixture had something to repair (restored=%d); it is not testing the QUIET pass", report.restored)
	}
	line := stderr.String()
	if !strings.Contains(line, "route recovery (backstop): pass reason=cadence") {
		t.Fatalf("a quiet backstop logged %q, want a pass line naming why it was due", line)
	}
	for _, want := range []string{"legs=", "reads=", "restored=0", "took="} {
		if !strings.Contains(line, want) {
			t.Fatalf("the backstop line %q is missing %q", line, want)
		}
	}
	// It scanned something: a pass line over zero legs would be a lane reporting
	// health while converging nothing.
	if report.legs == 0 {
		t.Fatalf("the backstop reported %d legs, want at least the city work leg", report.legs)
	}

	// And the age is queryable, which is what the tick's trace record carries.
	at, reason, ran := cr.routeRecoveryLaneOf().lastBackstop()
	if !ran || reason != backstopReasonCadence || time.Since(at) > time.Minute {
		t.Fatalf("lastBackstop = (%s, %q, %t), want a recent cadence pass", at, reason, ran)
	}
	// Control: a lane that has not run reports so, rather than reporting an age
	// of zero that reads as "just converged".
	if _, _, freshRan := newRouteRecoveryLane().lastBackstop(); freshRan {
		t.Fatal("a lane that never scanned reports that it did")
	}
}

// TestRouteRecoveryDeltaCountsCandidatesItCouldNotResolve pins the
// DELIVERED-BUT-OFF-PLANE drop class as a number rather than as silence.
//
// The journal names a bead, the event is delivered, and the bead lives on a leg
// the runtime plane refuses — so the tick cannot repair it and it waits for the
// convergence lane. That is a different failure from a lost event (there the
// journal never named it at all), it is invisible from the restored count, and a
// rising count of it is a routing question worth asking.
func TestRouteRecoveryDeltaCountsCandidatesItCouldNotResolve(t *testing.T) {
	ledger := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("CW-1")}, nil)}
	binding := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{unroutedWorkBead("GB-1")}, nil)}
	topo := assembleResidencyTopology(&config.City{}, ledger, nil,
		[]storeref.ClassBinding{{
			Classes: []coordclass.Class{coordclass.ClassGraph},
			Leg:     storeref.Leg{Ref: storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: binding},
		}}, nil)
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		t.Fatalf("Plan(RoutedWork): %v", err)
	}

	report := newRouteRecoveryLane().deltaPass(plan, []string{"CW-1", "GB-1"})
	if report.dropped != 1 {
		t.Fatalf("delta dropped=%d for one on-plane and one off-plane candidate, want 1", report.dropped)
	}
	if report.fields()["dropped"] != 1 {
		t.Fatalf("delta trace fields = %v, want dropped=1 — the drop class must be visible, not inferred", report.fields())
	}
	// Control: a pass whose candidates all resolve reports no drops at all, so
	// the field is a signal rather than a constant.
	clean := newRouteRecoveryLane().deltaPass(plan, []string{"GB-1"})
	if clean.dropped != 0 {
		t.Fatalf("delta dropped=%d when every candidate resolved, want 0", clean.dropped)
	}
	if _, present := clean.fields()["dropped"]; present {
		t.Fatalf("clean delta trace fields = %v, want no dropped key", clean.fields())
	}
}
