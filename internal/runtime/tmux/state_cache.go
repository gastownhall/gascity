package tmux

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"golang.org/x/sync/singleflight"
)

// defaultCacheTTL is the default time-to-live for cached session state.
const defaultCacheTTL = 2 * time.Second

// defaultStaleTTL is the maximum age of cached data before it is considered
// too stale to trust. After this duration, IsRunning returns false for all
// sessions and logs a degraded warning.
const defaultStaleTTL = 30 * time.Second

// fetchTimeout is the hard timeout for a single runtime-state fetch.
const fetchTimeout = 3 * time.Second

// Backoff bounds for the process-table snapshot after it fails.
//
// The snapshot is a full-OS `ps` scan, and it fails by losing a CPU race: the
// box is saturated, the scan does not finish inside fetchTimeout, and the
// context kills it. Re-attempting it on the very next refresh spends another
// fetchTimeout and forks another full-OS scan into the contention that just
// starved the last one, so a loaded box keeps paying for a probe that cannot
// succeed while it stays loaded. That is a feedback loop, and it was observed
// running for days: a supervisor logged the degrade 74 times an hour, one
// futile scan every ~48s, and the probe never recovered on its own until the
// process was restarted.
//
// Backing off does not weaken the answer. A failed snapshot already degrades
// optimistically (see processAlive), and holding that degraded state longer is
// the same answer held longer — while the machine gets the CPU back that lets
// the next attempt actually finish.
const (
	processSnapshotBackoffBase = 15 * time.Second
	processSnapshotBackoffMax  = 2 * time.Minute
)

// StateFetcher abstracts tmux subprocess calls for testability.
type StateFetcher interface {
	// FetchState returns a runtime-state snapshot for live sessions.
	// Corpse-only sessions (every pane pane_dead=1 under remain-on-exit) are
	// kept with Running: false so their window activity is recorded; they
	// still contribute no liveness.
	// The returned snapshot is handed to StateCache, which publishes it to
	// lock-free readers, so the fetcher must not retain or mutate its maps.
	FetchState(ctx context.Context) (runtimeStateSnapshot, error)
}

type paneRuntimeState struct {
	Command string
	PID     string
}

type sessionRuntimeState struct {
	Running bool
	Panes   []paneRuntimeState
	// Attached reports whether any client is attached to the session.
	Attached bool
	// Activity is the session's most recent window-activity timestamp (unix
	// seconds), taken as the MAX over every one of its panes' #{window_activity}
	// — the same value the per-session `list-windows -t <s>` read returns. Zero
	// means the snapshot carries no activity for the session.
	Activity int64
	// ID is the session object's #{session_id} (for example "$3"). A session
	// re-created under the same name gets a new one, but ids are unique only
	// within one server's lifetime: a restarted server numbers from "$0" again.
	// Empty when tmux reported no well-formed id.
	ID string
	// Created is the session object's #{session_created} (decimal unix
	// seconds), which tells a reused ID apart. Empty when tmux reported no
	// decimal value or the row predates the field.
	Created string
}

type processRuntimeState struct {
	PID  string
	PPID string
	// Command is the process identity used for name matching. Linux sources it
	// from ps comm; Darwin joins a separate comm snapshot onto the args snapshot.
	Command string
	Args    string
}

type processSnapshot struct {
	byPID    map[string]processRuntimeState
	children map[string][]string
}

type runtimeStateSnapshot struct {
	Sessions  map[string]sessionRuntimeState
	Processes processSnapshot
	// ProcessesAvailable reports whether the OS process-table snapshot was
	// fetched successfully. tmux list-panes establishes session liveness on its
	// own; the process snapshot is only a secondary refinement (matching pane
	// PIDs to processNames). When the full-OS ps scan loses the CPU race to a
	// busy fleet it is marked unavailable rather than discarding the
	// authoritative tmux liveness, and processAlive degrades optimistically.
	ProcessesAvailable bool
}

type exactProcessScan struct {
	runtimes []runtime.LiveRuntime
	complete bool
}

// StateCache caches tmux runtime state to avoid spawning N subprocess calls per
// status check or reconciler pass. Concurrent callers are coalesced via
// singleflight so at most one tmux/process snapshot refresh runs at a time.
type StateCache struct {
	mu sync.RWMutex
	// state is the published snapshot. It is copy-on-write: readers copy it
	// under mu and then read its maps after releasing the lock (IsRunning,
	// ProcessAlive), so once published its maps must never be mutated in
	// place. Writers replace a map wholesale under mu instead.
	state     runtimeStateSnapshot
	fetchedAt time.Time
	// startedAt is when the fetch that produced state began, on the cache
	// clock, so a fresh read can tell whether state postdates an effect.
	startedAt time.Time
	lastError error
	dirty     bool // set by Invalidate(); cleared by a refresh no invalidation superseded
	// generation advances on every Invalidate and EvictSession, so a refresh
	// can tell whether it was superseded while its fetch was in flight.
	generation uint64
	// publishedGeneration is the start generation of the fetch that produced
	// state. A fetch that started earlier than that is older than what readers
	// already see and is discarded rather than published over it.
	publishedGeneration uint64
	// evictedAt records, per evicted session, the generation its eviction
	// produced. A superseded fetch that started before that generation may
	// still list the killed session, so it is filtered out before publishing.
	// Entries at or below publishedGeneration can never filter again and are
	// pruned on publish.
	evictedAt map[string]uint64
	// primedByNoServer reports that the published snapshot is the empty one
	// primed by an unprimed no-server failure, not a fleet the server listed.
	primedByNoServer bool
	ttl              time.Duration
	staleTTL         time.Duration
	sf               singleflight.Group
	fetcher          StateFetcher
	// now is the cache clock. Nil selects time.Now; tests inject a fake.
	now func() time.Time
	// scanMu guards scanBySessionID.
	scanMu sync.RWMutex
	// scanBySessionID is an instance-owned seam so fresh liveness tests can
	// model exact and partial process-table scans without mutable global state.
	scanBySessionID func(string, time.Time) exactProcessScan
}

// cacheObservation is one read of the cache after its refresh trigger ran:
// the published snapshot plus what is known about how far to trust it.
type cacheObservation struct {
	state     runtimeStateSnapshot
	fetchedAt time.Time
	startedAt time.Time
	// lastErr is the error of the most recent refresh attempt; nil after a
	// success.
	lastErr error
	// dirty reports that an Invalidate or EvictSession landed after the
	// published fetch began, so the snapshot may predate a known Start or Stop.
	dirty bool
	// primedByNoServer reports that the snapshot came from the unprimed
	// no-server prime rather than from a server's answer.
	primedByNoServer bool
}

// primed reports whether the observation holds a published snapshot.
func (o cacheObservation) primed() bool {
	return o.state.Sessions != nil && !o.fetchedAt.IsZero()
}

// NewStateCache creates a new cache with the given fetcher and TTL.
// staleTTL defaults to 30s.
func NewStateCache(fetcher StateFetcher, ttl time.Duration) *StateCache {
	var scanBySessionID func(string, time.Time) exactProcessScan
	if goruntime.GOOS == "linux" || goruntime.GOOS == "darwin" {
		scanBySessionID = func(id string, incarnationStartedAt time.Time) exactProcessScan {
			runtimes, err := proctable.ScanBySessionIDSince(id, incarnationStartedAt)
			return exactProcessScan{runtimes: runtimes, complete: err == nil}
		}
	}
	return &StateCache{
		fetcher:         fetcher,
		ttl:             ttl,
		staleTTL:        defaultStaleTTL,
		scanBySessionID: scanBySessionID,
	}
}

// freshState forces a post-invalidation cache generation and reports whether
// that generation, or a newer generation that superseded it, published a
// usable, non-stale snapshot.
func (c *StateCache) freshState() (runtimeStateSnapshot, bool) {
	c.Invalidate()
	for {
		obs, generation := c.observationAtGeneration()
		if c.freshObservation(obs) {
			return obs.state, true
		}

		c.refresh(generation)

		obs, currentGeneration := c.observationAtGeneration()
		if currentGeneration != generation {
			continue
		}
		return obs.state, c.freshObservation(obs)
	}
}

// freshObservation reports whether obs is a clean, successful publish that is
// not yet stale.
func (c *StateCache) freshObservation(obs cacheObservation) bool {
	return obs.lastErr == nil && !obs.dirty && obs.primed() &&
		c.clock().Sub(obs.fetchedAt) <= c.staleTTL
}

func (c *StateCache) setScanBySessionID(scan func(string, time.Time) exactProcessScan) {
	c.scanMu.Lock()
	c.scanBySessionID = scan
	c.scanMu.Unlock()
}

func (c *StateCache) scanSessionID(id string, incarnationStartedAt time.Time) (exactProcessScan, bool) {
	c.scanMu.RLock()
	scan := c.scanBySessionID
	c.scanMu.RUnlock()
	if scan == nil {
		return exactProcessScan{}, false
	}
	return scan(id, incarnationStartedAt), true
}

// ObserveFreshLiveness forces a new tmux snapshot and combines it with an
// exact GC_SESSION_ID process-table scan. Absence is complete only when both
// sources were fully observed in that post-invalidation generation.
func (p *Provider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	name := strings.TrimSpace(target.SessionName)
	if name == "" || p.cache == nil {
		return runtime.Liveness{}
	}

	state, cacheComplete := p.cache.freshState()
	session, panePresent := state.Sessions[name]
	panePresent = panePresent && session.Running
	processNames := nonEmptyProcessNames(target.ProcessNames)
	processAlive := len(processNames) > 0 && state.processAlive(name, processNames)

	var (
		exactProcessAlive bool
		scanComplete      bool
	)
	if sessionID := strings.TrimSpace(target.SessionID); sessionID != "" {
		result, scanned := p.cache.scanSessionID(sessionID, target.IncarnationStartedAt)
		scanComplete = scanned && result.complete
		for _, live := range result.runtimes {
			if live.SessionID == sessionID {
				exactProcessAlive = true
				break
			}
		}
	}

	positive := panePresent || processAlive || exactProcessAlive
	complete := cacheComplete && scanComplete
	if len(processNames) > 0 && panePresent && !state.ProcessesAvailable {
		complete = false
	}
	return runtime.Liveness{Running: positive, Alive: positive, Complete: complete}
}

// IsRunning reports whether the named session exists in the cached set.
// If the cache is stale, a refresh is triggered (coalesced via singleflight).
// On refresh failure, the last-known-good cache is preserved up to staleTTL.
func (c *StateCache) IsRunning(name string) bool {
	state := c.currentState()
	session, ok := state.Sessions[name]
	return ok && session.Running
}

// ProcessAlive reports whether the named session has a process matching one of
// processNames according to the cached runtime snapshot. An empty processNames
// slice preserves Provider.ProcessAlive's "no check possible" behavior.
func (c *StateCache) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	return c.currentState().processAlive(name, processNames)
}

// SessionAttached reports the cached attachment state for the named session.
// ok is false when the snapshot holds no row for it — never observed, evicted,
// or the whole snapshot aged past staleTTL — so the caller falls back to a
// direct per-session read instead of reading "unknown" as "detached".
func (c *StateCache) SessionAttached(name string) (attached bool, ok bool) {
	session, ok := c.currentState().Sessions[name]
	if !ok {
		return false, false
	}
	return session.Attached, true
}

// SessionActivity reports the cached max window-activity timestamp for the
// named session. ok is false when the snapshot holds no activity for it, so
// the caller falls back to a direct per-session read rather than reporting a
// zero timestamp — "not observed" and "last active at the epoch" drive
// different reconciler decisions.
func (c *StateCache) SessionActivity(name string) (time.Time, bool) {
	session, ok := c.currentState().Sessions[name]
	if !ok || session.Activity == 0 {
		return time.Time{}, false
	}
	return time.Unix(session.Activity, 0), true
}

func (c *StateCache) currentState() runtimeStateSnapshot {
	// A nil cache knows nothing: report an empty snapshot so every reader
	// treats the session as absent and falls back to its direct probe. Tests
	// (and any future zero-value Provider) build a Tmux without a cache.
	if c == nil {
		return runtimeStateSnapshot{}
	}
	obs, hit := c.observeRefreshing()
	if hit {
		return obs.state
	}
	// If the cache is older than staleTTL, report all sessions as not running.
	// Note: fetchedAt is preserved on failure (never zeroed), so this only
	// triggers after staleTTL of real wall-clock time since last success.
	if !obs.primed() || c.clock().Sub(obs.fetchedAt) > c.staleTTL {
		return runtimeStateSnapshot{}
	}
	return obs.state
}

// observe returns the published snapshot with its refresh outcome, running
// the same refresh trigger as currentState but never applying the staleTTL
// cliff: the caller decides what a stale or failed observation means.
func (c *StateCache) observe() cacheObservation {
	obs, _ := c.observeRefreshing()
	return obs
}

// observeRefreshing reads the cache, refreshing it first unless it is a hit,
// and reports whether it was one.
func (c *StateCache) observeRefreshing() (cacheObservation, bool) {
	obs, generation := c.observationAtGeneration()

	// Cache hit: fresh data, not invalidated.
	if obs.primed() && !obs.dirty && c.clock().Sub(obs.fetchedAt) < c.ttl {
		return obs, true
	}

	// Stale, empty, or dirty — trigger a refresh. Calls from the same
	// generation coalesce, while an invalidation advances the key so a
	// fresh call never joins a pre-invalidation fetch.
	c.refresh(generation)

	// Read the (potentially updated) cache.
	return c.observation(), false
}

func (c *StateCache) observation() cacheObservation {
	obs, _ := c.observationAtGeneration()
	return obs
}

// observationAtGeneration reads the cache together with the generation it was
// read at, under one lock.
func (c *StateCache) observationAtGeneration() (cacheObservation, uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cacheObservation{
		state:            c.state,
		fetchedAt:        c.fetchedAt,
		startedAt:        c.startedAt,
		lastErr:          c.lastError,
		dirty:            c.dirty,
		primedByNoServer: c.primedByNoServer,
	}, c.generation
}

func (c *StateCache) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

// cacheAnswer is how far one cache observation can answer for a session.
type cacheAnswer int

const (
	// cacheAnswerSnapshot: answer from the published snapshot.
	cacheAnswerSnapshot cacheAnswer = iota
	// cacheAnswerAbsent: confirmed absent whatever the snapshot holds.
	cacheAnswerAbsent
	// cacheAnswerUnknown: the snapshot cannot be trusted either way.
	cacheAnswerUnknown
)

// classifyCacheObservation decides how far obs can answer for name. It never
// answers absent while the bool path (IsRunning, which keeps the staleTTL
// cliff) would still report name running, so the two forms cannot disagree:
//   - a successful last refresh over a primed snapshot answers from it;
//   - a failed refresh inside staleTTL over a snapshot a server listed answers
//     from that snapshot when it is clean (#4082), and is unknown when it is
//     dirty and still lists name;
//   - otherwise the snapshot cannot hold name live (past staleTTL, unprimed,
//     primed only by the no-server fallback, or name not listed). A
//     no-server failure whose socket serverDead confirms gone is then
//     absent, since a session cannot outlive its server; anything else is
//     unknown.
//
// serverDead is consulted only for a no-server failure.
func classifyCacheObservation(obs cacheObservation, name string, now time.Time, staleTTL time.Duration, serverDead func() bool) cacheAnswer {
	if obs.lastErr == nil && obs.primed() {
		return cacheAnswerSnapshot
	}
	trusted := obs.primed() && !obs.primedByNoServer && now.Sub(obs.fetchedAt) <= staleTTL
	if trusted && !obs.dirty {
		return cacheAnswerSnapshot
	}
	if trusted && obs.state.Sessions[name].Running {
		return cacheAnswerUnknown
	}
	if isNoServerError(obs.lastErr) && serverDead() {
		return cacheAnswerAbsent
	}
	return cacheAnswerUnknown
}

// observeSince returns the published snapshot once a successful fetch that
// started at or after since has produced it, refreshing when the published one
// is older. It refreshes at most twice: a fetch already in flight before since
// is joined rather than restarted, so the second refresh is the one that starts
// after since. It never invalidates the cache. ok is false when no such fetch
// succeeded; obs.lastErr then says why.
func (c *StateCache) observeSince(since time.Time) (obs cacheObservation, ok bool) {
	for attempt := 0; ; attempt++ {
		var generation uint64
		obs, generation = c.observationAtGeneration()
		if obs.lastErr == nil && obs.primed() && !obs.startedAt.Before(since) {
			return obs, true
		}
		if attempt == 2 {
			return obs, false
		}
		c.refresh(generation)
	}
}

// Invalidate marks the cache as dirty, forcing the next IsRunning call
// to trigger a refresh. The session data and fetchedAt are preserved as
// last-known-good until the refresh completes — even if the refresh fails.
func (c *StateCache) Invalidate() {
	c.mu.Lock()
	c.dirty = true
	c.generation++
	c.mu.Unlock()
}

// EvictSession removes a specific session from the cache and marks it dirty.
// Used by Stop to immediately reflect the killed session without waiting for
// the next refresh cycle (which may race with singleflight coalescing).
//
// The published Sessions map may still be held by readers that dropped the
// lock, so the eviction publishes a copy rather than deleting in place:
// an in-place delete is a concurrent map read/write, a fatal runtime error
// that recover cannot catch.
func (c *StateCache) EvictSession(name string) {
	c.mu.Lock()
	if _, ok := c.state.Sessions[name]; ok {
		sessions := maps.Clone(c.state.Sessions)
		delete(sessions, name)
		c.state.Sessions = sessions
	}
	c.dirty = true
	c.generation++
	if c.evictedAt == nil {
		c.evictedAt = make(map[string]uint64)
	}
	c.evictedAt[name] = c.generation
	c.mu.Unlock()
}

// refresh executes a single fetch, coalesced with concurrent refreshes keyed
// by the same cache generation. If the fetch fails, the last-known-good cache
// is preserved and the error is logged.
func (c *StateCache) refresh(generation uint64) {
	key := "refresh:" + strconv.FormatUint(generation, 10)
	_, _, _ = c.sf.Do(key, func() (interface{}, error) {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()

		c.mu.RLock()
		startGeneration := c.generation
		c.mu.RUnlock()

		start := c.clock()
		state, err := c.fetcher.FetchState(ctx)
		elapsed := c.clock().Sub(start)

		if err != nil {
			log.Printf("tmux state cache: refresh failed in %v: %v", elapsed, err)
			c.mu.Lock()
			c.lastError = err
			// Two distinct failure regimes, keyed on whether the cache was ever
			// primed (fetchedAt set by a prior success):
			//
			//   UNPRIMED + genuine no-server (a fresh city with no tmux server
			//   yet): initialize to an EMPTY snapshot so the cache is primed.
			//   Without this, currentState() sees a nil Sessions map and forces
			//   a fresh list-panes spawn plus a failure log on EVERY IsRunning()
			//   call — a re-spawn/log storm in the exact steady state (no server)
			//   where nothing will change until one is started. An empty primed
			//   snapshot correctly reports all sessions not-running and holds as
			//   a cache hit until the TTL lapses.
			//
			//   PRIMED then now-unreachable: preserve last-known-good (do NOT
			//   touch fetchedAt or sessions) until the staleTTL cliff. A server
			//   that was up then briefly vanished (supervisor restart, socket
			//   stall) must not wipe a good snapshot and drain healthy pool slots
			//   — that is #4082's intent.
			if c.fetchedAt.IsZero() && isNoServerError(err) {
				c.state = runtimeStateSnapshot{Sessions: make(map[string]sessionRuntimeState)}
				c.fetchedAt = c.clock()
				c.primedByNoServer = true
				c.publishedGeneration = startGeneration
				// Stay dirty if an invalidation (e.g. the Start that brings
				// the server up) landed mid-fetch, as a successful refresh does.
				c.dirty = c.generation != startGeneration
			}
			c.mu.Unlock()
			return nil, err
		}

		verbose := os.Getenv("GC_LOG_TMUX_CACHE") == "true"
		c.mu.Lock()
		defer c.mu.Unlock()
		if startGeneration < c.publishedGeneration {
			// A fetch keyed by a newer generation has already published;
			// this observation is older than what readers see.
			if verbose {
				log.Printf("tmux state cache: discarded refresh from generation %d after %v (generation %d already published)", startGeneration, elapsed, c.publishedGeneration)
			}
			return nil, nil
		}
		// An Invalidate or EvictSession landed while this fetch was in
		// flight. Discarding the fetch is not safe: under steady invalidation
		// every fetch is superseded, nothing is ever published, and once
		// staleTTL passes currentState reports every session absent even
		// though tmux answered each fetch. Publish what the server was seen to
		// hold, minus sessions evicted since the fetch began (Stop kills then
		// evicts, and an older fetch may still list the killed session), and
		// leave the cache dirty so the next read observes the change that
		// superseded this one.
		superseded := c.generation != startGeneration
		if superseded {
			state.Sessions = withoutEvictedSince(state.Sessions, c.evictedAt, startGeneration)
		}
		// Successful refresh is noisy on the session loop; opt-in via env var
		// keeps it available for diagnostics without polluting normal CLI use.
		if verbose {
			log.Printf("tmux state cache: refreshed %d sessions in %v (superseded=%t)", len(state.Sessions), elapsed, superseded)
		}

		c.state = state
		c.fetchedAt = c.clock()
		c.startedAt = start
		c.lastError = nil
		c.primedByNoServer = false
		c.dirty = superseded
		c.publishedGeneration = startGeneration
		for name, evictedGeneration := range c.evictedAt {
			if evictedGeneration <= startGeneration {
				delete(c.evictedAt, name)
			}
		}
		return nil, nil
	})
}

// withoutEvictedSince returns sessions minus every session whose eviction
// generation is later than since. It copies before deleting: the fetcher hands
// its map over, but a map shared with a previously published snapshot must
// never be mutated in place (see StateCache.state).
func withoutEvictedSince(sessions map[string]sessionRuntimeState, evictedAt map[string]uint64, since uint64) map[string]sessionRuntimeState {
	filtered := sessions
	cloned := false
	for name, generation := range evictedAt {
		if generation <= since {
			continue
		}
		if _, ok := filtered[name]; !ok {
			continue
		}
		if !cloned {
			filtered = maps.Clone(sessions)
			cloned = true
		}
		delete(filtered, name)
	}
	return filtered
}

// tmuxFetcher implements StateFetcher using a real Tmux instance.
type tmuxFetcher struct {
	tm *Tmux
	// snapshotGate bounds re-attempts of the process-table snapshot after it
	// fails. Its zero value attempts immediately, so a bare &tmuxFetcher{tm:
	// tm} behaves exactly as it did before this gate existed.
	snapshotGate processSnapshotGate
}

// processSnapshotGate decides whether the full-OS process scan may be attempted
// now, and records what happened when it was.
//
// It is a property of the fetcher rather than of the cache because the cache
// cannot see this failure at all: FetchState degrades rather than erroring when
// the scan fails, so refresh() books the result as a success — it stamps
// fetchedAt, clears dirty, and has no idea the expensive half of the fetch just
// died. Nothing above this point knows there is anything to back off from.
type processSnapshotGate struct {
	mu          sync.Mutex
	failures    int
	nextAttempt time.Time
}

// allow reports whether the scan may be attempted at now.
func (g *processSnapshotGate) allow(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.nextAttempt.IsZero() || !now.Before(g.nextAttempt)
}

// failed records a failed attempt and returns the window before the next one.
//
// The window doubles from processSnapshotBackoffBase and is capped, so a box
// that stays saturated settles at one attempt per processSnapshotBackoffMax
// instead of one per refresh.
func (g *processSnapshotGate) failed(now time.Time) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	window := processSnapshotBackoffBase << min(g.failures, 8)
	if window > processSnapshotBackoffMax || window <= 0 {
		window = processSnapshotBackoffMax
	}
	g.failures++
	g.nextAttempt = now.Add(window)
	return window
}

// succeeded clears the backoff, and reports whether it was clearing one — so
// the caller can say the probe recovered exactly once rather than on every
// healthy refresh forever after.
func (g *processSnapshotGate) succeeded() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	recovered := g.failures > 0
	g.failures = 0
	g.nextAttempt = time.Time{}
	return recovered
}

// FetchState runs one tmux pane snapshot and one process-table snapshot.
// Corpse-only sessions (remain-on-exit has kept only dead panes, pane_dead=1)
// are kept with Running: false so their window activity is recorded; they
// still contribute no liveness — they represent exited processes, not
// running ones.
func (f *tmuxFetcher) FetchState(ctx context.Context) (runtimeStateSnapshot, error) {
	out, err := f.tm.runCtx(ctx, "list-panes", "-a", "-F", "#{session_name}\t#{pane_dead}\t#{pane_current_command}\t#{pane_pid}\t#{session_attached}\t#{window_activity}\t#{session_id}\t#{session_created}")
	if err != nil {
		if errors.Is(err, ErrNoCurrentTarget) {
			// The server ANSWERED and holds zero sessions. gc configures
			// exit-empty off, so an empty-but-alive server is a normal steady
			// state for any city between agents, and tmux replies to a
			// target-taking command like list-panes with "no current target"
			// rather than empty output. That is a successful observation of an
			// empty fleet — identical to the out == "" case below — so it must
			// prime the cache. Treating it as ErrNoServer (which it wraps, for
			// the idempotent-teardown callers) left the supervisor's cache
			// permanently unprimed: a tmux subprocess and a "refresh failed"
			// log line on EVERY IsRunning, plus a staleTTL cliff that reported
			// the whole city not-running. See ga-jnavd.
			return runtimeStateSnapshot{Sessions: make(map[string]sessionRuntimeState)}, nil
		}
		if isNoServerError(err) {
			// An unreachable tmux server is an observation FAILURE, not the
			// fact "no sessions exist". Returning an empty *success* here let
			// refresh() overwrite the cache's last-known-good and instantly
			// report every session as not-running, so a brief server blip (a
			// supervisor restart, a transient socket stall) drove the
			// reconciler to drain/close healthy pool slots. Surface it as
			// runtime.ErrRuntimeUnavailable instead: refresh() then preserves
			// last-known-good until the existing staleTTL cliff, bounding the
			// trust window. Genuine session ends evict from the cache via
			// Stop()/EvictSession, so they are not masked by this preservation
			// (the only residual is an externally-killed LAST session, whose
			// cleanup is delayed by at most staleTTL — the intended trade).
			// isNoServerError still matches the wrapped error (it contains the
			// original "no server running" cause), so downstream absorbers are
			// unaffected.
			return runtimeStateSnapshot{}, fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, err)
		}
		return runtimeStateSnapshot{}, err
	}
	state := runtimeStateSnapshot{
		Sessions: make(map[string]sessionRuntimeState),
	}
	if out == "" {
		return state, nil
	}

	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 8)
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		name := parts[0]
		session := state.Sessions[name]
		// Attachment and activity are read from EVERY pane row, dead panes
		// included. A remain-on-exit corpse window is still a window, and the
		// per-session `list-windows -t <s> -F '#{window_activity}'` read this
		// replaces reports it, so skipping those rows would make a session look
		// older than tmux says it is. Every window has at least one pane and all
		// panes of a window carry their window's activity, so the max over pane
		// rows is exactly the max over windows.
		// #{session_attached} is the NUMBER of attached clients, not a 0/1
		// flag: a session with two clients reads "2". Anything but "0" (or a
		// missing/blank field) is attached — matching parseAttachedClients,
		// which the direct per-session probe uses.
		if len(parts) > 4 {
			if attached := strings.TrimSpace(parts[4]); attached != "" && attached != "0" {
				session.Attached = true
			}
		}
		if len(parts) > 5 {
			if activity, convErr := strconv.ParseInt(strings.TrimSpace(parts[5]), 10, 64); convErr == nil && activity > session.Activity {
				session.Activity = activity
			}
		}
		if len(parts) > 6 && validSessionObjectID(strings.TrimSpace(parts[6])) {
			session.ID = strings.TrimSpace(parts[6])
		}
		if len(parts) > 7 && decimalRe.MatchString(strings.TrimSpace(parts[7])) {
			session.Created = strings.TrimSpace(parts[7])
		}
		if parts[1] == "1" {
			// A dead pane contributes no liveness: Running stays as-is so a
			// session whose panes are all corpses is still reported not-running.
			state.Sessions[name] = session
			continue
		}
		var pane paneRuntimeState
		if len(parts) > 2 {
			pane.Command = strings.TrimSpace(parts[2])
		}
		if len(parts) > 3 {
			pane.PID = strings.TrimSpace(parts[3])
		}
		session.Running = true
		if pane.Command != "" || pane.PID != "" {
			session.Panes = append(session.Panes, pane)
		}
		state.Sessions[name] = session
	}
	// Skipping is the same outcome as failing — process detail unavailable,
	// sessions retained — reached without spending fetchTimeout and a full-OS
	// scan to rediscover it. Silent by design: the whole point is to stop
	// paying per refresh, and a line per skip would simply move the cost from
	// CPU to the log.
	if !f.snapshotGate.allow(time.Now()) {
		state.ProcessesAvailable = false
		return state, nil
	}
	processes, err := fetchProcessSnapshot(ctx)
	if err != nil {
		// Degrade, do NOT discard: tmux list-panes above already established
		// session liveness. The process snapshot is a secondary refinement
		// (matching pane PIDs to processNames). A full-OS ps scan that loses the
		// CPU race to a busy/KO fleet must never throw away authoritative tmux
		// liveness — that is what was starving the controller's reconcile and
		// cold-pool-spawner. Keep the sessions; mark process detail unavailable
		// so processAlive degrades optimistically instead of reporting dead.
		window := f.snapshotGate.failed(time.Now())
		log.Printf("tmux state cache: process snapshot degraded, retaining tmux session liveness (next attempt in %v): %v", window, err)
		state.ProcessesAvailable = false
		return state, nil
	}
	if f.snapshotGate.succeeded() {
		log.Printf("tmux state cache: process snapshot recovered")
	}
	state.Processes = processes
	state.ProcessesAvailable = true
	return state, nil
}

func (s runtimeStateSnapshot) processAlive(sessionName string, processNames []string) bool {
	session, ok := s.Sessions[sessionName]
	if !ok || !session.Running {
		return false
	}
	names := processNameSet(processNames)
	if len(names) == 0 {
		return false
	}
	if !s.ProcessesAvailable {
		// The OS process snapshot failed (e.g. the ps scan timed out under
		// fleet load). tmux confirms the session/pane is alive; we cannot verify
		// the inner process, so degrade optimistically rather than report it
		// dead. A failed secondary probe must never trigger a reap/respawn.
		return true
	}
	for _, pane := range session.Panes {
		if pane.processAlive(names, s.Processes) {
			return true
		}
	}
	return false
}

func (p paneRuntimeState) processAlive(names map[string]struct{}, processes processSnapshot) bool {
	if _, ok := names[p.Command]; ok && p.Command != "" {
		return true
	}
	if p.PID == "" {
		return false
	}
	if isSupportedShell(p.Command) {
		return processes.hasDescendantWithNames(p.PID, names, 0)
	}
	if processes.processMatchesNames(p.PID, names) {
		return true
	}
	return processes.hasDescendantWithNames(p.PID, names, 0)
}

func processNameSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
			// The kimi entry point now sets COMM and argv[0] to kimi-code.
			// Keep existing provider process_names valid for both CLIs.
			if name == "kimi" {
				set["kimi-code"] = struct{}{}
			}
		}
	}
	return set
}

func processMatchesNameSet(command, args string, names map[string]struct{}) bool {
	if len(names) == 0 {
		return false
	}
	command = filepath.Base(strings.TrimSpace(command))
	if _, ok := names[command]; ok && command != "" {
		return true
	}
	argv := strings.Fields(strings.TrimSpace(args))
	if len(argv) == 0 {
		return false
	}
	argv0 := filepath.Base(argv[0])
	if _, ok := names[argv0]; ok {
		return true
	}
	if _, isInterpreter := knownInterpreters[argv0]; !isInterpreter {
		return false
	}
	for _, token := range argv[1:] {
		token = strings.TrimSpace(token)
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		if _, isRunner := runnerSubcommands[token]; isRunner {
			continue
		}
		base := filepath.Base(token)
		if _, ok := names[base]; ok {
			return true
		}
		baseNoExt := strings.TrimSuffix(base, filepath.Ext(base))
		if _, ok := names[baseNoExt]; ok {
			return true
		}
		break
	}
	return false
}

var knownInterpreters = map[string]struct{}{
	"node": {}, "bun": {}, "npx": {}, "deno": {},
}

var runnerSubcommands = map[string]struct{}{
	"run": {}, "exec": {}, "x": {},
}

const maxProcessDescendantDepth = 10

func isSupportedShell(command string) bool {
	for _, shell := range supportedShells {
		if command == shell {
			return true
		}
	}
	return false
}

func newProcessSnapshot(processes []processRuntimeState) processSnapshot {
	snapshot := processSnapshot{
		byPID:    make(map[string]processRuntimeState, len(processes)),
		children: make(map[string][]string),
	}
	for _, process := range processes {
		process.PID = strings.TrimSpace(process.PID)
		process.PPID = strings.TrimSpace(process.PPID)
		process.Command = strings.TrimSpace(process.Command)
		process.Args = strings.TrimSpace(process.Args)
		if process.PID == "" {
			continue
		}
		snapshot.byPID[process.PID] = process
		if process.PPID != "" {
			snapshot.children[process.PPID] = append(snapshot.children[process.PPID], process.PID)
		}
	}
	return snapshot
}

func fetchProcessSnapshot(ctx context.Context) (processSnapshot, error) {
	if goruntime.GOOS == "darwin" {
		return fetchDarwinProcessSnapshot(ctx)
	}
	out, err := exec.CommandContext(ctx, "ps", processSnapshotPSArgs()...).Output()
	if err != nil {
		return processSnapshot{}, fmt.Errorf("fetching process snapshot: %w", err)
	}
	return parseProcessSnapshot(string(out)), nil
}

func fetchDarwinProcessSnapshot(ctx context.Context) (processSnapshot, error) {
	argsOut, err := exec.CommandContext(ctx, "ps", processSnapshotPSArgs()...).Output()
	if err != nil {
		return processSnapshot{}, fmt.Errorf("fetching Darwin process args snapshot: %w", err)
	}
	commOut, err := exec.CommandContext(ctx, "ps", darwinCommandSnapshotPSArgs()...).Output()
	if err != nil {
		return processSnapshot{}, fmt.Errorf("fetching Darwin process command snapshot: %w", err)
	}
	return parseDarwinProcessSnapshot(string(argsOut), string(commOut)), nil
}

// processSnapshotPSArgs returns the platform-appropriate `ps` arguments for
// the process snapshot. macOS's ps does not accept the BSD column-width
// suffix (e.g. `pid:10=`) that Linux ps supports; on Darwin we omit the widths
// and fetch comm separately so both args and command identity are safe to parse
// as trailing columns. On Linux we keep the wide-column form so the fast
// fixed-column parser is exercised.
func processSnapshotPSArgs() []string {
	if goruntime.GOOS == "darwin" {
		return []string{"-eo", "pid=,ppid=,args="}
	}
	return []string{"-eo", "pid:10=,ppid:10=,comm:64=,args="}
}

func darwinCommandSnapshotPSArgs() []string {
	return []string{"-eo", "pid=,ppid=,comm="}
}

func parseProcessSnapshot(out string) processSnapshot {
	processes := make([]processRuntimeState, 0)
	for _, line := range strings.Split(out, "\n") {
		process, ok := parseProcessSnapshotLine(line)
		if !ok {
			continue
		}
		processes = append(processes, process)
	}
	return newProcessSnapshot(processes)
}

func parseDarwinProcessSnapshot(argsOut, commOut string) processSnapshot {
	processesByPID := make(map[string]processRuntimeState)
	pidOrder := make([]string, 0)
	upsert := func(process processRuntimeState) {
		if _, ok := processesByPID[process.PID]; !ok {
			pidOrder = append(pidOrder, process.PID)
		}
		processesByPID[process.PID] = process
	}

	for _, line := range strings.Split(commOut, "\n") {
		process, ok := parseDarwinCommandSnapshotLine(line)
		if !ok {
			continue
		}
		upsert(process)
	}
	for _, line := range strings.Split(argsOut, "\n") {
		process, ok := parseProcessSnapshotLineDarwin(line)
		if !ok {
			continue
		}
		if existing, ok := processesByPID[process.PID]; ok && existing.PPID == process.PPID {
			existing.Args = process.Args
			if existing.Command == "" {
				existing.Command = process.Command
			}
			upsert(existing)
			continue
		}
		upsert(process)
	}

	processes := make([]processRuntimeState, 0, len(pidOrder))
	for _, pid := range pidOrder {
		processes = append(processes, processesByPID[pid])
	}
	return newProcessSnapshot(processes)
}

func parseProcessSnapshotLine(line string) (processRuntimeState, bool) {
	if goruntime.GOOS == "darwin" {
		return parseProcessSnapshotLineDarwin(line)
	}
	return parseProcessSnapshotLineFixedColumns(line)
}

func parseProcessSnapshotLineFixedColumns(line string) (processRuntimeState, bool) {
	const (
		pidWidth     = 10
		ppidStart    = pidWidth + 1
		ppidWidth    = 10
		commandStart = ppidStart + ppidWidth + 1
		commandWidth = 64
		argsStart    = commandStart + commandWidth + 1
	)
	if len(line) < commandStart+commandWidth {
		return processRuntimeState{}, false
	}
	process := processRuntimeState{
		PID:     strings.TrimSpace(line[:pidWidth]),
		PPID:    strings.TrimSpace(line[ppidStart : ppidStart+ppidWidth]),
		Command: strings.TrimSpace(line[commandStart : commandStart+commandWidth]),
	}
	if len(line) > argsStart {
		process.Args = strings.TrimSpace(line[argsStart:])
	}
	if process.PID == "" || process.PPID == "" || process.Command == "" {
		return processRuntimeState{}, false
	}
	return process, true
}

// parseProcessSnapshotLineDarwin parses one line of
// `ps -eo pid=,ppid=,args=` output on macOS.
//
// Line layout (SEP = single space):
//
//	<pid right-aligned> SEP <ppid right-aligned> SEP <args>
//
// PID/PPID column widths are dynamic, so only those two numeric fields are
// parsed as whitespace-delimited tokens. The args column is last and is kept
// verbatim aside from outer whitespace. Command is derived from argv[0] as a
// fallback; the Darwin fetch path joins a separate comm snapshot when available.
func parseProcessSnapshotLineDarwin(line string) (processRuntimeState, bool) {
	pid, remaining, ok := takeWhitespaceDelimitedToken(line)
	if !ok {
		return processRuntimeState{}, false
	}
	ppid, remaining, ok := takeWhitespaceDelimitedToken(remaining)
	if !ok {
		return processRuntimeState{}, false
	}
	args := strings.TrimSpace(remaining)
	argv := strings.Fields(args)
	if len(argv) == 0 {
		return processRuntimeState{}, false
	}
	command := filepath.Base(argv[0])
	if pid == "" || ppid == "" || command == "" {
		return processRuntimeState{}, false
	}
	return processRuntimeState{
		PID:     pid,
		PPID:    ppid,
		Command: command,
		Args:    args,
	}, true
}

// parseDarwinCommandSnapshotLine parses one line of
// `ps -eo pid=,ppid=,comm=` output on macOS. The comm column is requested in a
// separate snapshot so it is the final field and can contain whitespace safely.
func parseDarwinCommandSnapshotLine(line string) (processRuntimeState, bool) {
	pid, remaining, ok := takeWhitespaceDelimitedToken(line)
	if !ok {
		return processRuntimeState{}, false
	}
	ppid, remaining, ok := takeWhitespaceDelimitedToken(remaining)
	if !ok {
		return processRuntimeState{}, false
	}
	command := strings.TrimSpace(remaining)
	if pid == "" || ppid == "" || command == "" {
		return processRuntimeState{}, false
	}
	return processRuntimeState{
		PID:     pid,
		PPID:    ppid,
		Command: command,
	}, true
}

func takeWhitespaceDelimitedToken(input string) (token string, remaining string, ok bool) {
	i := 0
	for i < len(input) && (input[i] == ' ' || input[i] == '\t') {
		i++
	}
	if i >= len(input) {
		return "", "", false
	}
	start := i
	for i < len(input) && input[i] != ' ' && input[i] != '\t' {
		i++
	}
	return input[start:i], input[i:], true
}

func (s processSnapshot) processMatchesNames(pid string, names map[string]struct{}) bool {
	process, ok := s.byPID[pid]
	if !ok {
		return false
	}
	return processMatchesNameSet(process.Command, process.Args, names)
}

func (s processSnapshot) hasDescendantWithNames(pid string, names map[string]struct{}, depth int) bool {
	if len(names) == 0 || depth > maxProcessDescendantDepth {
		return false
	}
	for _, childPID := range s.children[pid] {
		if s.processMatchesNames(childPID, names) {
			return true
		}
		if s.hasDescendantWithNames(childPID, names, depth+1) {
			return true
		}
	}
	return false
}

// isNoServerError checks if the error is a "no server running" error.
func isNoServerError(err error) bool {
	return errors.Is(err, ErrNoServer) || (err != nil && strings.Contains(err.Error(), "no server running"))
}

// cacheTTLFromEnv reads GC_TMUX_CACHE_TTL from the environment and parses
// it as a duration. Returns defaultCacheTTL if the env var is unset, empty,
// or cannot be parsed. Accepts:
//   - integer: interpreted as milliseconds (e.g., "2000" = 2s)
//   - Go duration string: (e.g., "2s", "500ms")
func cacheTTLFromEnv() time.Duration {
	v := os.Getenv("GC_TMUX_CACHE_TTL")
	if v == "" {
		return defaultCacheTTL
	}

	// Try Go duration string first (e.g., "2s", "500ms").
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}

	// Try integer milliseconds (e.g., "2000").
	if strings.TrimSpace(v) == v {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}

	log.Printf("tmux state cache: invalid GC_TMUX_CACHE_TTL=%q, using default %v", v, defaultCacheTTL)
	return defaultCacheTTL
}
