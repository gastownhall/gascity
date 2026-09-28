package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
)

func cityEntries(names ...string) []supervisor.CityEntry {
	entries := make([]supervisor.CityEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, supervisor.CityEntry{Path: "/cities/" + name, Name: name})
	}
	return entries
}

func startedNames(entries []supervisor.CityEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.EffectiveName())
	}
	return names
}

func equalNames(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestSupervisorBootOrderIsDeterministicWithoutPriority pins the property the
// old loop did not have at all: a repeatable boot order.
//
// The start list was built by ranging a map, so which city booted first was
// randomized per process. With a serial boot that is only confusing — the last
// city in the order waits out every city before it, and which one that is
// changes on every restart. With a bounded pool it is worse: the first wave is
// then a random pick, so the city an operator most wants up is up first only by
// luck.
func TestSupervisorBootOrderIsDeterministicWithoutPriority(t *testing.T) {
	entries := cityEntries("zulu", "alpha", "mike")
	got := startedNames(sortCityStartOrder(entries, nil))
	if !equalNames(got, "alpha", "mike", "zulu") {
		t.Fatalf("boot order = %v, want lexical order", got)
	}

	// Same set, different input order: the output must not depend on it.
	shuffled := startedNames(sortCityStartOrder(cityEntries("mike", "zulu", "alpha"), nil))
	if !equalNames(shuffled, got...) {
		t.Fatalf("boot order = %v for a reordered input, want %v: order must not depend on map iteration", shuffled, got)
	}
}

// TestSupervisorBootOrderPutsPriorityCitiesFirst pins the wave-1 guarantee: the
// cities an operator names boot first, in the order they named them, and
// everything else follows in stable lexical order.
func TestSupervisorBootOrderPutsPriorityCitiesFirst(t *testing.T) {
	entries := cityEntries("alpha", "mike", "zulu", "bravo")
	got := startedNames(sortCityStartOrder(entries, []string{"zulu", "mike"}))
	if !equalNames(got, "zulu", "mike", "alpha", "bravo") {
		t.Fatalf("boot order = %v, want the named cities first in the order named, then the rest lexically", got)
	}
}

// TestSupervisorBootOrderIgnoresUnmatchedPriorityNames proves a stale or
// misspelled entry in the priority list costs nothing: it names no city, so it
// orders nothing, and every registered city still boots.
func TestSupervisorBootOrderIgnoresUnmatchedPriorityNames(t *testing.T) {
	entries := cityEntries("alpha", "bravo")
	got := startedNames(sortCityStartOrder(entries, []string{"a-city-that-was-unregistered", "bravo"}))
	if !equalNames(got, "bravo", "alpha") {
		t.Fatalf("boot order = %v, want the unmatched name skipped and bravo promoted", got)
	}
}

func TestSupervisorBootPriorityParsesCommaList(t *testing.T) {
	t.Setenv(supervisorBootPriorityEnv, " first-city , second-city ,, ")
	got := supervisorBootPriority()
	if !equalNames(got, "first-city", "second-city") {
		t.Fatalf("priority list = %v, want the two named cities with blanks dropped", got)
	}

	t.Setenv(supervisorBootPriorityEnv, "")
	if got := supervisorBootPriority(); len(got) != 0 {
		t.Fatalf("priority list = %v for an unset knob, want empty: the SDK names no city of its own", got)
	}
}

func TestSupervisorBootConcurrencyKnob(t *testing.T) {
	for _, row := range []struct {
		name string
		set  bool
		env  string
		want int
	}{
		{name: "unset defaults to two", want: defaultSupervisorBootConcurrency},
		{name: "explicit bound", set: true, env: "5", want: 5},
		{name: "one is the sequential safety valve", set: true, env: "1", want: 1},
		{name: "zero clamps to sequential", set: true, env: "0", want: 1},
		{name: "negative clamps to sequential", set: true, env: "-3", want: 1},
		{name: "garbage falls back to the default", set: true, env: "lots", want: defaultSupervisorBootConcurrency},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.set {
				t.Setenv(supervisorBootConcurrencyEnv, row.env)
			} else {
				t.Setenv(supervisorBootConcurrencyEnv, "")
			}
			if got := supervisorBootConcurrency(); got != row.want {
				t.Fatalf("supervisorBootConcurrency() = %d, want %d", got, row.want)
			}
		})
	}
}

// gatedStarter is a start function that blocks every call until it is released,
// so the pool's peak concurrency is observable without a single sleep. Each call
// registers its arrival and then waits; the test releases them once it has seen
// as many arrivals as it expects.
type gatedStarter struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	arrived  chan struct{}
	release  chan struct{}
	started  []string
}

func newGatedStarter(capacity int) *gatedStarter {
	return &gatedStarter{arrived: make(chan struct{}, capacity), release: make(chan struct{})}
}

func (g *gatedStarter) start(entry supervisor.CityEntry) {
	g.mu.Lock()
	g.inFlight++
	if g.inFlight > g.peak {
		g.peak = g.inFlight
	}
	g.started = append(g.started, entry.EffectiveName())
	g.mu.Unlock()

	g.arrived <- struct{}{}
	<-g.release

	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
}

func (g *gatedStarter) peakConcurrency() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

func (g *gatedStarter) names() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.started))
	copy(out, g.started)
	return out
}

// TestStartCityWorkersRespectsBound proves the pool actually runs cities in
// parallel AND actually bounds them. Both halves matter: an unbounded fan-out
// would spike host IO across every city's store at once, and a pool that never
// exceeds one is the serial boot this change exists to delete.
//
// The starter blocks, so the peak is read at a moment when the pool is provably
// saturated rather than at whatever instant a sleep happened to sample.
func TestStartCityWorkersRespectsBound(t *testing.T) {
	entries := cityEntries("a", "b", "c", "d", "e")
	starter := newGatedStarter(len(entries))

	done := make(chan struct{})
	go func() {
		startCityWorkers(context.Background(), entries, 2, io.Discard, starter.start)
		close(done)
	}()

	// Two cities reach the starter; the pool must not admit a third.
	<-starter.arrived
	<-starter.arrived
	if got := starter.peakConcurrency(); got != 2 {
		t.Fatalf("peak concurrency = %d, want exactly the bound of 2", got)
	}
	select {
	case <-starter.arrived:
		t.Fatal("a third city started while the bound of 2 was saturated")
	default:
	}

	close(starter.release)
	<-done

	if got := starter.peakConcurrency(); got > 2 {
		t.Fatalf("peak concurrency = %d over the whole run, want at most the bound of 2", got)
	}
	if got := starter.names(); len(got) != len(entries) {
		t.Fatalf("started %v, want all %d cities", got, len(entries))
	}
}

// TestStartCityWorkersBoundOneIsSequential pins the safety valve. Setting the
// bound to 1 must reproduce the old serial boot exactly — one city in flight at
// a time — so an operator on a small host has a single knob back to known
// behavior.
func TestStartCityWorkersBoundOneIsSequential(t *testing.T) {
	entries := cityEntries("a", "b", "c")
	var inFlight, peak atomic.Int64
	var order []string

	startCityWorkers(context.Background(), entries, 1, io.Discard, func(entry supervisor.CityEntry) {
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		order = append(order, entry.EffectiveName())
		inFlight.Add(-1)
	})

	if got := peak.Load(); got != 1 {
		t.Fatalf("peak concurrency at bound 1 = %d, want 1", got)
	}
	if !equalNames(order, "a", "b", "c") {
		t.Fatalf("bound-1 start order = %v, want the submitted order preserved", order)
	}
}

// TestStartCityWorkersFinishesEveryCityWhenOnePanics is the failure-isolation
// contract, and it is why the recovery belongs to the POOL rather than to the
// closure the pool is handed.
//
// Serially, a panic inside one city's start unwound to the supervisor's
// reconcile-level recover and abandoned every city after it in that pass. From a
// worker goroutine that same panic has no reconcile-level recover above it — it
// takes down the whole supervisor process. So the pool recovers each start
// itself: the panicking city is named in the log and skipped, and its siblings
// keep booting.
func TestStartCityWorkersFinishesEveryCityWhenOnePanics(t *testing.T) {
	entries := cityEntries("a", "boom", "c", "d")
	var mu sync.Mutex
	attempted := map[string]bool{}
	var stderr lockedBuffer

	startCityWorkers(context.Background(), entries, 2, &stderr, func(entry supervisor.CityEntry) {
		mu.Lock()
		attempted[entry.EffectiveName()] = true
		mu.Unlock()
		if entry.EffectiveName() == "boom" {
			panic("city start blew up")
		}
	})

	for _, entry := range entries {
		if !attempted[entry.EffectiveName()] {
			t.Fatalf("city %q was never attempted after a sibling panicked; attempted = %v", entry.EffectiveName(), attempted)
		}
	}
	if got := stderr.String(); !strings.Contains(got, "city 'boom': start panicked: city start blew up") {
		t.Fatalf("supervisor log = %q, want the panicking city named: a recovered-and-silent start is a city that vanishes", got)
	}
}

// TestSelectCitiesToStartMarksEveryCandidateQueued closes the re-entry race the
// bounded pool opens.
//
// The start list is built from the registry under lock and the workers then run
// outside it, which is what makes the boot parallel. But the supervisor's
// reconcile pass is itself re-entrant — it fires on a ticker, on SIGHUP and on
// the reload socket command — and it selects cities by skipping any that already
// carry an initStatus entry. Serially that was safe by accident: the loop set
// "loading_config" before it did anything slow, and nothing else ran until the
// loop returned. With workers in flight a second pass can reach this selection
// while the first pass's cities are still booting, so the marker has to be set
// in the SAME locked pass that selects them — before any worker exists.
func TestSelectCitiesToStartMarksEveryCandidateQueued(t *testing.T) {
	cr := newCityRegistry()
	cr.Add("/cities/running", &managedCity{name: "running"})
	cr.BatchUpdate(func(
		_ map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		initStatus["/cities/initializing"] = cityInitProgress{name: "initializing", status: "loading_config"}
	})

	desired := map[string]supervisor.CityEntry{}
	for _, entry := range cityEntries("running", "initializing", "zulu", "alpha") {
		desired[entry.Path] = entry
	}

	got := selectCitiesToStart(cr, desired, nil)
	if names := startedNames(got); !equalNames(names, "alpha", "zulu") {
		t.Fatalf("selected %v, want only the two cities that are neither running nor initializing, in deterministic order", names)
	}
	for _, entry := range got {
		progress, ok := initStatusEntry(cr, entry.Path)
		if !ok {
			t.Fatalf("city %q was selected to start with no initStatus entry: a concurrent reconcile pass would start it a second time", entry.EffectiveName())
		}
		if progress.status != cityInitStatusQueued {
			t.Fatalf("initStatus[%s].status = %q, want %q", entry.Path, progress.status, cityInitStatusQueued)
		}
		if progress.name != entry.EffectiveName() {
			t.Fatalf("initStatus[%s].name = %q, want %q so the city is identifiable while queued", entry.Path, progress.name, entry.EffectiveName())
		}
	}

	// The marker's whole job: the next pass, arriving while the workers are
	// still running, selects nothing.
	if again := selectCitiesToStart(cr, desired, nil); len(again) != 0 {
		t.Fatalf("a re-entrant reconcile pass selected %v while their starts were still in flight", startedNames(again))
	}
}

// TestSelectCitiesToStartAppliesBootPriority proves the selection pass is also
// where boot order is decided, so the pool's first wave is the operator's choice
// rather than a map-iteration accident.
func TestSelectCitiesToStartAppliesBootPriority(t *testing.T) {
	cr := newCityRegistry()
	desired := map[string]supervisor.CityEntry{}
	for _, entry := range cityEntries("alpha", "mike", "zulu") {
		desired[entry.Path] = entry
	}

	got := startedNames(selectCitiesToStart(cr, desired, []string{"zulu"}))
	if !equalNames(got, "zulu", "alpha", "mike") {
		t.Fatalf("selected order = %v, want the priority city first", got)
	}
}

// TestReleaseQueuedCityStartClearsAMarkerLeftMidStart pins the release as
// UNCONDITIONAL, which is the whole of its value.
//
// A conditional release keyed on the queued status only ever fired for the
// early returns that happen before the config load, because startOneCity
// replaces the marker with "loading_config" and then a phase name within
// milliseconds. A panic anywhere in the ~470 lines after that transition
// skipped both normal clears -- recordInitFailure and publishManagedCity --
// and the conditional release then no-opped on the marker it found. The city
// was left permanently mid-init: selectCitiesToStart skips any path holding an
// initStatus entry, so it was never selected again for the life of the
// supervisor, with no backoff and no rescue, while the API reported it forever
// starting.
//
// Deleting unconditionally is safe because reconcileCities runs on the
// supervisor's single loop goroutine and startCityWorkers does not return until
// its workers do: no other selection pass can be running while this worker is
// alive, so there is no other marker for the delete to race.
func TestReleaseQueuedCityStartClearsAMarkerLeftMidStart(t *testing.T) {
	for _, status := range []string{cityInitStatusQueued, "loading_config", "creating_session_provider", "running_pool_on_boot"} {
		t.Run(status, func(t *testing.T) {
			cr := newCityRegistry()
			cr.BatchUpdate(func(
				_ map[string]*managedCity,
				initStatus map[string]cityInitProgress,
				_ map[string]*initFailRecord,
				_ map[string]*panicRecord,
			) {
				initStatus["/cities/wedged"] = cityInitProgress{name: "wedged", status: status}
			})

			releaseQueuedCityStart(cr, "/cities/wedged")

			if progress, ok := initStatusEntry(cr, "/cities/wedged"); ok {
				t.Fatalf("initStatus survived as %+v after an abandoned start; the city can never be selected again", progress)
			}
		})
	}
}

// TestStartCityWorkersStopsSubmittingOnShutdown pins the shutdown behavior a
// parallel boot needs and a serial one got for free.
//
// Serially, a SIGTERM arriving mid-boot took effect after the city then
// starting: the loop was inside one city and the supervisor's shutdown case ran
// next. With a queued list and a worker pool, a shutdown request would still
// boot every city left in the list -- and a fleet boot is minutes -- so systemd
// hits TimeoutStopSec and SIGKILLs the supervisor mid-boot, skipping session
// preservation entirely.
//
// In-flight cities are deliberately NOT canceled: bounding stop latency to the
// in-flight wave keeps a half-built city from being abandoned with its stores
// open. Only the not-yet-submitted entries are dropped, and they are returned
// so the caller can release their queued markers.
func TestStartCityWorkersStopsSubmittingOnShutdown(t *testing.T) {
	entries := cityEntries("a", "b", "c", "d", "e")
	starter := newGatedStarter(len(entries))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var skipped []supervisor.CityEntry
	done := make(chan struct{})
	go func() {
		skipped = startCityWorkers(ctx, entries, 2, io.Discard, starter.start)
		close(done)
	}()

	// Saturate the pool, then ask for shutdown while the submit loop is blocked
	// handing over the third city.
	<-starter.arrived
	<-starter.arrived
	cancel()
	close(starter.release)

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("startCityWorkers never returned after shutdown was requested")
	}

	started := starter.names()
	if len(started) != 2 {
		t.Fatalf("started %v after shutdown was requested; only the in-flight wave may run", started)
	}
	if names := startedNames(skipped); !equalNames(names, "c", "d", "e") {
		t.Fatalf("skipped = %v, want the three cities that were never submitted, so their queued markers can be released", names)
	}
}

// TestStartCityWorkersRunsEverythingWhenNotShuttingDown is the control for the
// row above: with a live context every city is submitted and nothing is
// reported skipped, so "skipped" cannot quietly become the normal outcome.
func TestStartCityWorkersRunsEverythingWhenNotShuttingDown(t *testing.T) {
	entries := cityEntries("a", "b", "c")
	var mu sync.Mutex
	var started []string

	skipped := startCityWorkers(context.Background(), entries, 2, io.Discard, func(entry supervisor.CityEntry) {
		mu.Lock()
		started = append(started, entry.EffectiveName())
		mu.Unlock()
	})

	if len(skipped) != 0 {
		t.Fatalf("skipped = %v with a live context, want none", startedNames(skipped))
	}
	if len(started) != len(entries) {
		t.Fatalf("started %v, want all %d cities", started, len(entries))
	}
}
