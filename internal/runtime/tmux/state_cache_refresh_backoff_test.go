package tmux

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// These tests pin the refresh backoff (#7170). A failed refresh leaves
// fetchedAt alone, so once the TTL lapses every reader that arrives without
// overlapping another spawns tmux again; with the server unable to answer, that
// is one spawn per reader per poll for as long as the outage lasts. After a
// failure the cache instead holds TTL-expiry refreshes for a window that
// doubles per consecutive failure from 2s up to a cap of 15s (never more than
// half of staleTTL), answers readers from the observation it already has, and
// lets one fetch through when an Invalidate or EvictSession newer than the
// failed fetch shows the observation is out of date.
//
// The cache clock is fake and the fetcher is a stub that records the instant of
// every call, so each test asserts the exact schedule of tmux spawns. Nothing
// sleeps and no tmux runs.

const (
	// backoffStep is how often a simulated reader polls.
	backoffStep = 100 * time.Millisecond
	// backoffSession and backoffOther are the sessions the healthy fleet lists.
	backoffSession = "agent-1"
	backoffOther   = "agent-2"
)

// errBackoffFetch stands for a refresh that fails without proving the server
// gone: tmux killed by the fetch timeout on a saturated box.
var errBackoffFetch = errors.New("tmux list-panes: signal: killed")

// suppressedRE reads the count of held reads out of a backoff log line.
var suppressedRE = regexp.MustCompile(`(\d+) reads suppressed`)

// backoffClock is the cache clock, advanced by hand. It is atomic because the
// interleaving tests read it from reader goroutines.
type backoffClock struct {
	base time.Time
	ns   atomic.Int64
}

// Now reports the current fake instant.
func (c *backoffClock) Now() time.Time { return c.base.Add(c.offset()) }

func (c *backoffClock) offset() time.Duration   { return time.Duration(c.ns.Load()) }
func (c *backoffClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// backoffHarness is a StateCache over the fake clock and a scripted fetcher.
type backoffHarness struct {
	t     *testing.T
	clock *backoffClock
	cache *StateCache
	logs  bytes.Buffer

	mu      sync.Mutex
	fetches []time.Duration // clock offset of every FetchState call
	failing error           // what respond returns while set
	// script, when set, answers every FetchState call itself; a script that
	// only wants to intercept some calls falls back to respond.
	script func(ctx context.Context, call int) (runtimeStateSnapshot, error)
}

func newBackoffHarness(t *testing.T) *backoffHarness {
	t.Helper()
	h := &backoffHarness{
		t:     t,
		clock: &backoffClock{base: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)},
	}
	h.cache = NewStateCache(h, defaultCacheTTL)
	h.cache.now = h.clock.Now
	t.Cleanup(captureLog(&h.logs))
	return h
}

// backoffFleet is the healthy fleet: both sessions running, the first attached
// and active.
func backoffFleet() runtimeStateSnapshot {
	return runtimeStateSnapshot{Sessions: map[string]sessionRuntimeState{
		backoffSession: {Running: true, Attached: true, Activity: 1_700_000_000},
		backoffOther:   {Running: true},
	}}
}

// FetchState records the call, then answers from the script when there is one
// and from respond otherwise.
func (h *backoffHarness) FetchState(ctx context.Context) (runtimeStateSnapshot, error) {
	h.mu.Lock()
	h.fetches = append(h.fetches, h.clock.offset())
	call := len(h.fetches)
	script := h.script
	h.mu.Unlock()
	if script != nil {
		return script(ctx, call)
	}
	return h.respond()
}

// respond answers a fetch: the healthy fleet, or the failure set by fail.
func (h *backoffHarness) respond() (runtimeStateSnapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failing != nil {
		return runtimeStateSnapshot{}, h.failing
	}
	return backoffFleet(), nil
}

func (h *backoffHarness) fail(err error) {
	h.mu.Lock()
	h.failing = err
	h.mu.Unlock()
}

func (h *backoffHarness) heal() { h.fail(nil) }

func (h *backoffHarness) schedule() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.fetches)
}

func (h *backoffHarness) read() bool { return h.cache.IsRunning(backoffSession) }

// prime makes the first read, which fetches the healthy fleet at offset 0.
func (h *backoffHarness) prime() {
	h.t.Helper()
	if !h.read() {
		h.t.Fatal("IsRunning = false on the priming read, want the fleet listed")
	}
	requireSchedule(h.t, h.schedule(), secs(0))
}

// run polls up to the offset until: every backoffStep the clock advances and
// readers readers read in turn, each finishing before the next starts, so
// singleflight never coalesces them.
func (h *backoffHarness) run(until time.Duration, readers int) {
	h.t.Helper()
	for h.clock.offset() < until {
		h.clock.advance(backoffStep)
		for range readers {
			h.read()
		}
	}
}

// secs builds a fetch schedule from whole-second offsets.
func secs(offsets ...int) []time.Duration {
	out := make([]time.Duration, len(offsets))
	for i, s := range offsets {
		out[i] = time.Duration(s) * time.Second
	}
	return out
}

// requireSchedule fails unless the fetch offsets are exactly want. A cache that
// respawns tmux on every poll yields thousands of entries, so only the head is
// printed.
func requireSchedule(t *testing.T, got, want []time.Duration) {
	t.Helper()
	if slices.Equal(got, want) {
		return
	}
	head := got[:min(len(got), len(want)+4)]
	t.Fatalf("fetch offsets = %v (%d fetches, first %d shown), want %v", head, len(got), len(head), want)
}

// waitSignal waits for a test goroutine to reach a rendezvous point; the
// timeout only bounds a hang.
func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitGroup is waitSignal for a sync.WaitGroup.
func waitGroup(t *testing.T, wg *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	waitSignal(t, done, what)
}

// With every fetch failing, N readers polling for a minute spawn tmux on the
// backed-off schedule, not N times per poll: the first failure at the 2s TTL
// holds 2s, and each further failure doubles the hold up to the 15s cap.
func TestStateCacheBackoff_FetchScheduleFollowsConsecutiveFailures(t *testing.T) {
	h := newBackoffHarness(t)
	h.prime()
	h.fail(errBackoffFetch)

	h.run(62*time.Second, 5)

	requireSchedule(t, h.schedule(), secs(0, 2, 4, 8, 16, 31, 46, 61))
}

// A fetch that succeeds ends the streak: the next outage starts over at the
// shortest hold.
func TestStateCacheBackoff_SuccessRestartsTheSchedule(t *testing.T) {
	h := newBackoffHarness(t)
	h.prime()
	h.fail(errBackoffFetch)
	h.run(17*time.Second, 1) // failures at 2, 4, 8, 16: the fourth holds 15s, to 31s

	h.heal()
	h.run(31*time.Second, 1) // the hold ends at 31s and that fetch succeeds
	if !h.read() {
		t.Fatal("IsRunning = false after the held fetch succeeded, want the fleet listed")
	}
	h.fail(errBackoffFetch)
	h.run(48*time.Second, 1)

	// The new outage fails first when the TTL lapses at 33s, and holds 2s, 4s, 8s
	// again. Had the streak survived the success, the 33s failure would be the
	// fifth and hold 15s.
	requireSchedule(t, h.schedule(), secs(0, 2, 4, 8, 16, 31, 33, 35, 39, 47))
}

// The hold is a fixed schedule, not a multiple of the TTL: an operator who
// shortens or lengthens GC_TMUX_CACHE_TTL changes when the first refresh fails,
// not how fast failures back off.
func TestStateCacheBackoff_HoldDoesNotScaleWithTheTTL(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		run  time.Duration
		want []time.Duration
	}{
		{
			name: "short ttl",
			ttl:  500 * time.Millisecond,
			run:  30 * time.Second,
			want: []time.Duration{0, 500 * time.Millisecond, 2500 * time.Millisecond, 6500 * time.Millisecond, 14500 * time.Millisecond, 29500 * time.Millisecond},
		},
		{name: "long ttl", ttl: 10 * time.Second, run: 25 * time.Second, want: secs(0, 10, 12, 16, 24)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBackoffHarness(t)
			h.cache.ttl = tc.ttl
			h.prime()
			h.fail(errBackoffFetch)

			h.run(tc.run, 3)

			requireSchedule(t, h.schedule(), tc.want)
		})
	}
}

// A cache that never primed backs off too, whether the failure is a missing
// server (which primes an empty snapshot) or anything else (which primes
// nothing): both used to spawn tmux on every read.
func TestStateCacheBackoff_UnprimedFailuresBackOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"no server", errFetchNoServer},
		{"other failure", errBackoffFetch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBackoffHarness(t)
			h.fail(tc.err)

			h.read()
			h.run(45*time.Second, 3)

			requireSchedule(t, h.schedule(), secs(0, 2, 6, 14, 29, 44))
		})
	}
}

// Inside a hold a reader gets the observation the cache already has, at once:
// the last-known-good snapshot with the refresh error still set and the
// snapshot's age untouched.
func TestStateCacheBackoff_HeldReadsAnswerFromTheCurrentObservation(t *testing.T) {
	h := newBackoffHarness(t)
	h.prime()
	primedAt := h.cache.observation().fetchedAt
	h.fail(errBackoffFetch)
	h.run(2*time.Second, 1) // the first failure, at 2s, holds until 4s
	fetched := len(h.schedule())

	for range 20 {
		if !h.read() {
			t.Fatal("IsRunning = false inside the hold, want the last-known-good snapshot")
		}
	}

	obs := h.cache.observation()
	if !errors.Is(obs.lastErr, errBackoffFetch) {
		t.Fatalf("lastErr = %v inside the hold, want the refresh error still set", obs.lastErr)
	}
	if !obs.fetchedAt.Equal(primedAt) {
		t.Fatalf("fetchedAt = %v, want %v: a failed refresh must not make the snapshot younger", obs.fetchedAt, primedAt)
	}
	if got := len(h.schedule()) - fetched; got != 0 {
		t.Fatalf("%d fetches for 20 held reads, want none: a fetch that would fail inside the hold must not start", got)
	}
}

// The bool paths keep the staleTTL cliff to the instant: last-known-good at
// exactly staleTTL, all-absent the moment it is exceeded, with the hold that
// spans the cliff (16s to 31s) answering from the cache rather than spawning
// tmux to find out.
func TestStateCacheBackoff_StaleCliffInstantIsUnchanged(t *testing.T) {
	h := newBackoffHarness(t)
	h.prime()
	h.fail(errBackoffFetch)

	h.run(defaultStaleTTL, 1)

	if !h.read() {
		t.Fatal("IsRunning = false at exactly staleTTL, want the last-known-good snapshot")
	}
	if attached, ok := h.cache.SessionAttached(backoffSession); !ok || !attached {
		t.Fatalf("SessionAttached = (%t, %t) at exactly staleTTL, want (true, true)", attached, ok)
	}
	if _, ok := h.cache.SessionActivity(backoffSession); !ok {
		t.Fatal("SessionActivity ok = false at exactly staleTTL, want the last-known-good activity")
	}

	h.clock.advance(time.Millisecond)

	if h.read() {
		t.Fatal("IsRunning = true past staleTTL, want the all-absent cliff")
	}
	if _, ok := h.cache.SessionAttached(backoffSession); ok {
		t.Fatal("SessionAttached ok = true past staleTTL, want the caller to fall back to a direct read")
	}
	if _, ok := h.cache.SessionActivity(backoffSession); ok {
		t.Fatal("SessionActivity ok = true past staleTTL, want the caller to fall back to a direct read")
	}
	requireSchedule(t, h.schedule(), secs(0, 2, 4, 8, 16))
}

// A hold is never longer than half of staleTTL, so at least two attempts fit in
// the window the snapshot stays trusted whatever staleTTL is configured to, and
// the cap is the one in force: 15s for the default, half of staleTTL below it.
func TestStateCacheBackoff_HoldNeverExceedsHalfTheStaleTTL(t *testing.T) {
	for _, staleTTL := range []time.Duration{defaultStaleTTL, 10 * time.Second, time.Second} {
		t.Run(staleTTL.String(), func(t *testing.T) {
			h := newBackoffHarness(t)
			h.cache.staleTTL = staleTTL
			h.prime()
			h.fail(errBackoffFetch)

			h.run(3*time.Minute, 1)

			// The first gap is the TTL lapsing; the rest are holds between failures.
			fetches := h.schedule()
			var widest time.Duration
			for i := 2; i < len(fetches); i++ {
				widest = max(widest, fetches[i]-fetches[i-1])
			}
			if widest > staleTTL/2 {
				t.Fatalf("widest hold = %v, want at most half of staleTTL %v", widest, staleTTL)
			}
			if want := min(15*time.Second, staleTTL/2); widest != want {
				t.Fatalf("widest hold = %v over %d fetches, want the cap %v to be reached", widest, len(fetches), want)
			}
		})
	}
}

// After an outage past the cliff the bool paths stay absent until the held
// fetch, so recovery lags the server by at most the cap, and no fetch starts
// before the hold ends.
func TestStateCacheBackoff_RecoveryWaitsForTheHoldNoLongerThanTheCap(t *testing.T) {
	h := newBackoffHarness(t)
	h.prime()
	h.fail(errBackoffFetch)
	h.run(40*time.Second, 2) // failures at 2, 4, 8, 16, 31: the fifth holds to 46s

	h.heal()
	h.run(45900*time.Millisecond, 2)
	requireSchedule(t, h.schedule(), secs(0, 2, 4, 8, 16, 31))

	h.run(46*time.Second, 2)
	requireSchedule(t, h.schedule(), secs(0, 2, 4, 8, 16, 31, 46))
	if !h.read() {
		t.Fatal("IsRunning = false after the held fetch succeeded, want the fleet listed")
	}
	if obs := h.cache.observation(); obs.lastErr != nil {
		t.Fatalf("lastErr = %v after a successful refresh, want nil", obs.lastErr)
	}
	if lag := 46*time.Second - 40*time.Second; lag > 15*time.Second {
		t.Fatalf("recovery lagged the server by %v, want at most the 15s cap", lag)
	}
}

// An Invalidate or EvictSession newer than the failed fetch means the
// observation is out of date, so it permits exactly one fetch inside the hold
// however many readers arrive, and the hold then stands again: that fetch is
// one more consecutive failure, so it holds 4s from where it failed.
func TestStateCacheBackoff_NewerChangePermitsExactlyOneFetch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*backoffHarness)
	}{
		{"Invalidate", func(h *backoffHarness) { h.cache.Invalidate() }},
		{"EvictSession", func(h *backoffHarness) { h.cache.EvictSession(backoffOther) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBackoffHarness(t)
			h.prime()
			h.fail(errBackoffFetch)
			h.run(2*time.Second, 1) // the first failure, at 2s, holds until 4s
			h.clock.advance(500 * time.Millisecond)

			tc.change(h)
			for range 10 {
				h.read()
			}

			requireSchedule(t, h.schedule(), []time.Duration{0, 2 * time.Second, 2500 * time.Millisecond})
			h.run(6400*time.Millisecond, 2)
			requireSchedule(t, h.schedule(), []time.Duration{0, 2 * time.Second, 2500 * time.Millisecond})
			h.run(6500*time.Millisecond, 2)
			requireSchedule(t, h.schedule(), []time.Duration{0, 2 * time.Second, 2500 * time.Millisecond, 6500 * time.Millisecond})
		})
	}
}

// An eviction inside a hold takes effect at once: the evicted session reads
// absent without waiting for a fetch, and the rest of the fleet still answers
// from the held snapshot.
func TestStateCacheBackoff_EvictionInsideAHoldIsImmediate(t *testing.T) {
	h := newBackoffHarness(t)
	h.prime()
	h.fail(errBackoffFetch)
	h.run(2*time.Second, 1)

	h.cache.EvictSession(backoffOther)

	if h.cache.IsRunning(backoffOther) {
		t.Fatal("IsRunning(evicted) = true inside the hold, want false at once")
	}
	if !h.read() {
		t.Fatal("IsRunning(other session) = false inside the hold, want the held snapshot")
	}
}

// A dirty flag left by an older change does not defeat the hold. The one fetch
// that could observe the Invalidate failed and the flag stays set until a
// success, but readers arriving with no newer change are held.
func TestStateCacheBackoff_DirtyFlagFromAnOlderChangeDoesNotDefeatTheHold(t *testing.T) {
	h := newBackoffHarness(t)
	h.prime()
	h.fail(errBackoffFetch)
	h.clock.advance(time.Second)
	h.cache.Invalidate()

	for range 10 {
		h.read()
	}

	requireSchedule(t, h.schedule(), []time.Duration{0, time.Second})
	if !h.cache.observation().dirty {
		t.Fatal("dirty cleared by a failed refresh, want it kept until a refresh succeeds")
	}
	h.run(2900*time.Millisecond, 3) // the failure at 1s holds 2s, to 3s
	requireSchedule(t, h.schedule(), []time.Duration{0, time.Second})
	h.run(3*time.Second, 3)
	requireSchedule(t, h.schedule(), []time.Duration{0, time.Second, 3 * time.Second})
}

// Each newer change permits its own fetch: the allowance is per change, not
// once per hold.
func TestStateCacheBackoff_EachNewerChangePermitsItsOwnFetch(t *testing.T) {
	h := newBackoffHarness(t)
	h.prime()
	h.fail(errBackoffFetch)
	h.run(2*time.Second, 1)

	for range 3 {
		h.clock.advance(backoffStep)
		h.cache.Invalidate()
		for range 10 {
			h.read()
		}
	}

	requireSchedule(t, h.schedule(), []time.Duration{
		0, 2 * time.Second, 2100 * time.Millisecond, 2200 * time.Millisecond, 2300 * time.Millisecond,
	})
}

// Two fetches in flight at once (a dirty read forgets the flight in progress)
// that both fail are one outage, not two: the hold is the first failure's 2s.
func TestStateCacheBackoff_OverlappingFailuresAreOneOutage(t *testing.T) {
	h := newBackoffHarness(t)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	h.script = func(ctx context.Context, call int) (runtimeStateSnapshot, error) {
		if call == 2 || call == 3 { // the two overlapping fetches
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return runtimeStateSnapshot{}, ctx.Err()
			}
		}
		return h.respond()
	}
	h.prime()
	h.fail(errBackoffFetch)
	h.clock.advance(2 * time.Second)
	h.cache.Invalidate()

	var readers sync.WaitGroup
	for range 2 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			h.read()
		}()
		waitSignal(t, entered, "a fetch to start")
	}
	close(release)
	waitGroup(t, &readers, "the overlapping reads to return")

	// A second failure would have doubled the hold to 4s.
	requireSchedule(t, h.schedule(), secs(0, 2, 2))
	h.run(3900*time.Millisecond, 1)
	requireSchedule(t, h.schedule(), secs(0, 2, 2))
	h.run(4*time.Second, 1)
	requireSchedule(t, h.schedule(), secs(0, 2, 2, 4))
}

// A change that lands while a fetch is failing says nothing about what that
// fetch saw, so it permits exactly one more fetch inside the hold that
// failure opened.
func TestStateCacheBackoff_ChangeDuringAFailingFetchPermitsOneMoreFetch(t *testing.T) {
	h := newBackoffHarness(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.script = func(ctx context.Context, call int) (runtimeStateSnapshot, error) {
		if call == 2 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return runtimeStateSnapshot{}, ctx.Err()
			}
			return runtimeStateSnapshot{}, errBackoffFetch
		}
		return h.respond()
	}
	h.prime()
	h.fail(errBackoffFetch)
	h.clock.advance(2 * time.Second)
	failing := make(chan struct{})
	go func() {
		defer close(failing)
		h.read() // the TTL has lapsed: this fetch starts, then waits
	}()
	waitSignal(t, entered, "the failing fetch to start")
	h.cache.Invalidate() // lands mid-fetch
	close(release)
	waitSignal(t, failing, "the failing read to return")

	for range 10 {
		h.read()
	}

	requireSchedule(t, h.schedule(), secs(0, 2, 2))
	h.run(5900*time.Millisecond, 1) // the permitted fetch failed too: 4s from 2s
	requireSchedule(t, h.schedule(), secs(0, 2, 2))
	h.run(6*time.Second, 1)
	requireSchedule(t, h.schedule(), secs(0, 2, 2, 6))
}

// A permitted fetch that succeeds while a change supersedes it publishes what
// it saw minus the evicted session, leaves the cache dirty, and ends the
// failure streak: the next outage holds 2s again, not what the old streak
// would have earned.
func TestStateCacheBackoff_SupersededRecoveryEndsTheStreak(t *testing.T) {
	h := newBackoffHarness(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.script = func(ctx context.Context, call int) (runtimeStateSnapshot, error) {
		if call == 3 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return runtimeStateSnapshot{}, ctx.Err()
			}
			return backoffFleet(), nil // observed before the eviction: still lists it
		}
		return h.respond()
	}
	h.prime()
	h.fail(errBackoffFetch)
	h.run(2*time.Second, 1) // the first failure, at 2s, holds until 4s
	h.clock.advance(500 * time.Millisecond)
	h.cache.Invalidate() // permits one fetch inside the hold
	recovering := make(chan struct{})
	go func() {
		defer close(recovering)
		h.read()
	}()
	waitSignal(t, entered, "the permitted fetch to start")
	h.cache.EvictSession(backoffOther) // supersedes it mid-flight
	h.heal()
	close(release)
	waitSignal(t, recovering, "the permitted read to return")

	obs := h.cache.observation()
	if obs.lastErr != nil {
		t.Fatalf("lastErr = %v after a successful refresh, want nil", obs.lastErr)
	}
	if !obs.dirty {
		t.Fatal("cache clean after a superseded refresh, want dirty")
	}
	if _, listed := obs.state.Sessions[backoffOther]; listed {
		t.Fatal("published snapshot lists the session evicted mid-fetch, want it filtered")
	}

	// Dirty and no longer in a hold: the next read fetches at once. It fails,
	// the first failure of a new streak, and holds 2s.
	h.fail(errBackoffFetch)
	h.read()
	want := []time.Duration{0, 2 * time.Second, 2500 * time.Millisecond, 2500 * time.Millisecond}
	requireSchedule(t, h.schedule(), want)
	h.run(4400*time.Millisecond, 1)
	requireSchedule(t, h.schedule(), want)
	h.run(4500*time.Millisecond, 1)
	requireSchedule(t, h.schedule(), append(want, 4500*time.Millisecond))
}

// A failure from a fetch that began before a newer fetch published carries no
// news and opens no hold: the newer fetch saw the server answer after it
// began, and the next TTL lapse must refresh on time.
func TestStateCacheBackoff_StaleFailureAfterANewerPublishOpensNoHold(t *testing.T) {
	h := newBackoffHarness(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.script = func(ctx context.Context, call int) (runtimeStateSnapshot, error) {
		if call == 2 { // the older, slower fetch: it fails once released
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return runtimeStateSnapshot{}, ctx.Err()
			}
			return runtimeStateSnapshot{}, errBackoffFetch
		}
		return h.respond()
	}
	h.prime()
	h.clock.advance(2 * time.Second)
	h.cache.Invalidate()
	older := make(chan struct{})
	go func() {
		defer close(older)
		h.read() // dirty: starts fetch 2, which waits
	}()
	waitSignal(t, entered, "the older fetch to start")
	h.read() // dirty: forgets that flight; fetch 3 succeeds and publishes
	h.clock.advance(1500 * time.Millisecond)
	close(release)
	waitSignal(t, older, "the older read to return")

	// Fetch 3 published at 2s, so the TTL lapses at 4s. A hold opened by the
	// older failure at 3.5s would swallow that refresh.
	h.run(4*time.Second, 1)
	requireSchedule(t, h.schedule(), secs(0, 2, 2, 4))
}

// A read the hold answers without fetching must classify exactly like the read
// whose fetch failed: backoff changes how often tmux is asked, never what a
// given observation means. Every shape classifyCacheObservation distinguishes
// is covered, through both the tri-state and the bool path.
func TestStateCacheBackoff_HeldReadsClassifyLikeTheFetchingRead(t *testing.T) {
	timeout := errors.New("tmux list-panes: signal: killed")
	pastStale := func(h *observeHarness) { h.advance(defaultStaleTTL + time.Second) }
	for _, tc := range []struct {
		name       string
		primed     bool
		failure    error
		socketDead bool
		prepare    func(h *observeHarness)
	}{
		{
			name: "last-known-good inside staleTTL", primed: true, failure: timeout,
			prepare: func(h *observeHarness) { h.advance(10 * time.Second) },
		},
		{
			name: "dirty snapshot lists the session", primed: true, failure: timeout,
			prepare: func(h *observeHarness) { h.advance(3 * time.Second); h.p.cache.Invalidate() },
		},
		{
			name: "dirty snapshot dropped the session", primed: true, failure: timeout,
			prepare: func(h *observeHarness) { h.p.cache.EvictSession("worker-1") },
		},
		{
			name: "dirty snapshot dropped the session, server dead", primed: true, failure: errFetchNoServer, socketDead: true,
			prepare: func(h *observeHarness) { h.p.cache.EvictSession("worker-1") },
		},
		{name: "past staleTTL", primed: true, failure: timeout, prepare: pastStale},
		{name: "past staleTTL, no server, socket dead", primed: true, failure: errFetchNoServer, socketDead: true, prepare: pastStale},
		{name: "past staleTTL, no server, socket alive", primed: true, failure: errFetchNoServer, prepare: pastStale},
		{name: "unprimed", failure: timeout},
		{name: "unprimed, no server, socket dead", failure: errFetchNoServer, socketDead: true},
		{name: "unprimed, no server, socket alive", failure: errFetchNoServer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *observeHarness
			if tc.primed {
				h = newObserveHarness(t, "city", map[string]bool{"worker-1": true})
				h.prime(t)
			} else {
				h = newObserveHarness(t, "city", nil)
			}
			h.fetcher.setResult(nil, tc.failure)
			if tc.prepare != nil {
				tc.prepare(h)
			}
			if tc.socketDead {
				h.socketErr = nil
			}

			read := func() (runtime.Liveness, bool, error) {
				got, err := h.observe("worker-1")
				return got, h.p.IsRunning("worker-1"), err
			}
			want, wantRunning, wantErr := read() // the read whose refresh fails
			attempts := h.fetcher.getCalls()

			for i := range 5 {
				got, running, err := read()
				if got != want || running != wantRunning {
					t.Fatalf("held read %d = (%+v, running %t), want the fetching read's (%+v, running %t)", i+1, got, running, want, wantRunning)
				}
				if (err == nil) != (wantErr == nil) || (err != nil && err.Error() != wantErr.Error()) {
					t.Fatalf("held read %d error = %v, want the fetching read's %v", i+1, err, wantErr)
				}
				if errors.Is(err, tc.failure) != errors.Is(wantErr, tc.failure) {
					t.Fatalf("held read %d error = %v, want the refresh error wrapped exactly as the fetching read's %v", i+1, err, wantErr)
				}
			}
			if got := h.fetcher.getCalls() - attempts; got != 0 {
				t.Fatalf("%d fetches for 5 held reads, want none", got)
			}
		})
	}
}

// A failing refresh logs once per hold, not once per read, and the line carries
// how many reads the previous hold answered without fetching. Recovery logs
// once.
func TestStateCacheBackoff_LogsOncePerHoldAndOnRecovery(t *testing.T) {
	h := newBackoffHarness(t)
	h.prime()
	h.fail(errBackoffFetch)

	h.run(20*time.Second, 2) // failures at 2, 4, 8, 16

	var failed []string
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(line, "refresh failed") {
			failed = append(failed, line)
		}
	}
	if len(failed) != 4 {
		t.Fatalf("%d failure lines for 4 failed refreshes, want one per hold:\n%s", len(failed), strings.Join(failed[:min(len(failed), 6)], "\n"))
	}
	// Two readers poll every 100ms. The reader that fetches ends a hold with its
	// partner still held, and the 19, 39 and 79 polls between the failures at
	// 2, 4, 8 and 16s are held by both: 2*19+1, 2*39+1, 2*79+1.
	for i, want := range []string{"0", "39", "79", "159"} {
		m := suppressedRE.FindStringSubmatch(failed[i])
		if m == nil || m[1] != want {
			t.Fatalf("failure line %d = %q, want it to report %s reads suppressed", i+1, failed[i], want)
		}
	}

	h.heal()
	h.run(40*time.Second, 2) // the hold ends at 31s and that fetch succeeds

	var recovered []string
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(line, "recovered") {
			recovered = append(recovered, line)
		}
	}
	if len(recovered) != 1 || !strings.Contains(recovered[0], "after 4 failed") {
		t.Fatalf("recovery lines = %q, want exactly one reporting the 4 failed refreshes", recovered)
	}
	if got := strings.Count(h.logs.String(), "refresh failed"); got != 4 {
		t.Fatalf("%d failure lines after recovery, want still 4", got)
	}
}
