package main

// The controller's ledger feed: CD slice 1a's second half.
//
// With [beads] ledger_feed = "auto", every city or rig cache whose store
// carries a durable mutation journal gets a feed (internal/beads/ledgerfeed).
// The feed tails the journal while the scope has it active, and waits while
// it is inactive or unavailable (a proxied store demoted at boot, a scope
// that turns the journal on later), reported as on the re-scan until its
// first good read; it never turns the journal on.
// Each page names the rows that changed; the feed refreshes every distinct id
// the cache owns through the cache (RefreshRowReport), so bead.* events keep
// exactly one author, the cache's onChange, and a refresh notifies only a
// real change, which is the re-scan's rule. A write the controller made
// through the cache comes back as a journal row whose refresh finds nothing
// new, so it neither notifies nor wakes anything (ga-yoix1): a refresh that
// lands while such a write is in flight yields to it
// (RowRefreshOptions.YieldToLocalWrites) and is retried. A write the
// controller makes around the cache (straight to the backing) is out of band
// to the cache and is refreshed like any other. A close is announced once,
// by whichever sees it first: the cache's own closing write, the re-scan
// that evicts the row the cache held open, or the refresh. The cache
// remembers each close it announced, by the row's revision, so a later
// refresh does not announce it again; a close it never announced is
// announced, however recently the cache wrote the row. A refresh more than a
// minute after the announcement, or one that reads another version of the
// row (a later write to it, or a close announced from a lagged read),
// announces it again. A page that shows a row open (an open post-image)
// marks it seen open (RowRefreshOptions.SeenOpen): a close read after it is
// a new close, announced even over a row the cache holds closed unless the
// cache holds that very version, and handed to the autoclose sweep. On a
// backing without revisions the cache tells closes apart by updated_at,
// which the open image overrides, so a reopen and close inside one second
// are not lost; there the controller's own close of a row a page showed open
// may be announced twice, as may, on any backing, a reclose read over a
// closed row the cache installed from a bus event or a lagged read, whose
// revision is stale: bead.closed delivery is at least once.
//
// Under the legacy reconciler the feed wakes the reconciler once per page
// with at least one changed row (critic A2: the allocator always, plus the
// changed session rows and the control dispatcher for graph rows). Under v2
// it never calls Enqueue (critic D7): the planner learns of the changes from
// the cache-reconcile events the refreshes record (controllerWake.OnBeadEvent).
//
// A gap in the journal, a refresh that failed or found the cache not live,
// and the first good read after the journal was inactive each request a
// forced re-scan (CachingStore.RequestReconcile), whose list read comes
// after the rows committed. The re-scan lists active rows only, so it covers
// every create and update but not the close of a row the cache never held.
// That close is announced once by the refresh (critic A3), also when a read
// or write of the cache installed the closed row first without announcing
// it (the cache reports such a row unheld), which sends it
// down the live-confirmed close path (applyInferredClose), and handed to the
// autoclose sweep as a backstop. A row is read as closing on a create or
// close op or an open post-image, and on a closed post-image whose closed_at
// sits within ledgerFeedCloseWindow of the row's ts: bd journals a close made
// by an update (bd update --status closed) as op update. An edit inside that
// window carries the same image, so the feed remembers the rows it read
// closed for ledgerFeedClosedKeep and does not read such an edit as their
// close again. A row whose refresh was dropped (an error, a fence, a cache
// not live) is kept for a bounded retry, so its close is still announced
// once the refresh succeeds. A closing row whose refresh failed (an error, a
// cache not live) is handed to the sweep at once; a fenced one, mostly the
// controller's own write in flight, is left to the retry. A refresh that
// yields to such a write spends none of the row's retries, and one that
// starts while the write is in flight reads nothing: the row waits for the
// write, for a minute at most. A create fences only the rows the cache does
// not hold, but a Tx or graph apply fences every row, so such writes back to
// back, or one whose backing call hangs, keep those rows waiting until the
// limit; a refresh's own backing read takes no context, so a hung read
// stalls the feed until it returns. A row whose retries or wait run out is
// logged and falls back to a re-scan and, if closing, the sweep. A gap
// skips its rows outright, so the close of a never-held row inside it is
// announced only if an edit of the row lands within its close's window.
//
// The feed reads nothing while its scope's gate is closed (a quiescent city
// or a suspended rig), and every read and Apply counts as store work, so the
// quiescence retire waits for it. Its state is held in memory and served
// over the controller socket (ledgerFeedStatusCommand); there is no status
// file and no wire change.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/ledgerfeed"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/steveyegge/beads/journalops"
)

const (
	// wakeReasonLedgerFeed is the reason the feed gives Enqueue.
	wakeReasonLedgerFeed = "ledger-feed"
	// ledgerFeedStatusCommand asks the controller socket for
	// ledgerFeedStatusFor.
	ledgerFeedStatusCommand = "ledger-feed"

	// ledgerFeedBaselineTimeout bounds the head read taken before a cache's
	// PrimeActive. One that takes longer is retried by the feed's run as a
	// late baseline, followed by a forced re-scan.
	ledgerFeedBaselineTimeout = 5 * time.Second
	// ledgerFeedPageLimit, ledgerFeedPoll and ledgerFeedIdlePoll are the
	// tailer's pacing: an immediate re-read after a full page, then Poll
	// doubling to IdlePoll while idle.
	ledgerFeedPageLimit = ledgerfeed.DefaultPageLimit
	ledgerFeedPoll      = ledgerfeed.DefaultPoll
	ledgerFeedIdlePoll  = ledgerfeed.DefaultIdlePoll

	// ledgerFeedWakeSessionCap caps the session keys one page's wake names;
	// the allocator key covers the rest.
	ledgerFeedWakeSessionCap = 64
	// ledgerFeedActorCap caps the distinct journal actors counted; the rest
	// are counted under ledgerFeedActorOther.
	ledgerFeedActorCap   = 16
	ledgerFeedActorOther = "other"
	// ledgerFeedSampleCap bounds the row-age and apply-duration samples the
	// percentiles are read from.
	ledgerFeedSampleCap = 1024
	// ledgerFeedLogEvery rate-limits each of the feed's warnings: a refresh
	// error, and a row whose retries ran out.
	ledgerFeedLogEvery = time.Minute
	// ledgerFeedRetryCap bounds the dropped rows kept for a retry, and
	// ledgerFeedRetryLimit the drops one row may take, before the feed gives
	// the row up to a re-scan (and, for a close, the autoclose sweep). A
	// refresh that yielded to the cache's own write in flight is not a drop:
	// the row waits for the write, for ledgerFeedYieldLimit from its first
	// yield at most. A graph apply holds that fence for seconds; a wait past
	// the limit is a write that does not return, or one write after another.
	ledgerFeedRetryCap   = 4096
	ledgerFeedRetryLimit = 8
	ledgerFeedYieldLimit = time.Minute
	// ledgerFeedCloseWindow is how near an update row's closed post-image
	// must put its closed_at to the row's ts for the row to be read as the
	// close itself: bd stamps both in the committing transaction and stores
	// closed_at to the second. An edit of a row closed longer ago shows no
	// evidence of a recent close.
	ledgerFeedCloseWindow = time.Minute
	// ledgerFeedClosedCap bounds the rows the feed remembers reading closed,
	// each for ledgerFeedClosedKeep, so that an edit inside a close's window
	// is not read as that close again.
	ledgerFeedClosedCap  = 4096
	ledgerFeedClosedKeep = 2 * ledgerFeedCloseWindow
)

// The journal ops the feed reads, from beads' frozen issueops vocabulary
// (EventCreate, EventClose, EventCommentWrite).
const (
	journalOpCreate  = "create"
	journalOpClose   = "close"
	journalOpComment = "comment"
)

// The feed's states, as the status verb reports them.
const (
	ledgerFeedStateStarting    = "starting"
	ledgerFeedStateLate        = "late"
	ledgerFeedStateTailing     = "tailing"
	ledgerFeedStateGated       = "gated"
	ledgerFeedStateInactive    = "inactive"
	ledgerFeedStateUnavailable = "unavailable"
	ledgerFeedStateRetrying    = "retrying"
	ledgerFeedStateStopped     = "stopped"
	ledgerFeedStateScanOnly    = "scan-only"
)

// errLedgerFeedGated reports a journal read or Apply refused because the
// scope's gate is closed.
var errLedgerFeedGated = errors.New("ledger feed: scope gate closed")

// ledgerFeedHooks are the controller services a feed uses.
type ledgerFeedHooks struct {
	// work counts one piece of store work in flight (beginStoreWork).
	work func() (end func())
	// deferClose owes id a check by the autoclose sweep.
	deferClose func(id string)
}

func (h ledgerFeedHooks) beginWork() func() {
	if h.work == nil {
		return func() {}
	}
	return h.work()
}

// ledgerFeedSet is one controller state's feeds and scan-only scopes.
type ledgerFeedSet struct {
	cityPath string
	hooks    ledgerFeedHooks
	// wake is the controller wake, stored by startBeadEventWatcher once the
	// entry point has wired it.
	wake atomic.Pointer[controllerWake]
	// rowsChanged, stored by the conversation-lane host as it starts, hears
	// the rows each refresh pass changed, under either reconciler: the
	// host's ledger-feed doorbell (DES-O1 §7.3).
	rowsChanged atomic.Pointer[func([]beads.Bead)]

	mu sync.Mutex
	// mode is the mode of the latest wrap request.
	mode string
	// entries are the feeds and scan-only scopes of live caches; an entry
	// whose cache was stopped is pruned on the next add or status read.
	entries []*ledgerFeedEntry
}

// ledgerFeedEntry is one scope's status source: a feed, or the reason the
// scope stays on the re-scan.
type ledgerFeedEntry struct {
	scope, lane, reason string
	cache               *beads.CachingStore
	feed                *ledgerFeed
}

// ledgerFeedSets maps a normalized city path to its controller state's set,
// so the socket handler, which knows only the city path, can read it.
var ledgerFeedSets sync.Map // normalized city path → *ledgerFeedSet

func newLedgerFeedSet(cityPath string, hooks ledgerFeedHooks) *ledgerFeedSet {
	return &ledgerFeedSet{cityPath: cityPath, hooks: hooks, mode: config.BeadsLedgerFeedOff}
}

// registerLedgerFeedSet publishes set for its city until ctx ends.
func registerLedgerFeedSet(ctx context.Context, set *ledgerFeedSet) {
	key := normalizePathForCompare(set.cityPath)
	ledgerFeedSets.Store(key, set)
	if ctx == nil || ctx.Done() == nil {
		return
	}
	go func() {
		<-ctx.Done()
		ledgerFeedSets.CompareAndDelete(key, set)
	}()
}

// setWake stores the controller wake the feeds wake through. Nil-safe.
func (s *ledgerFeedSet) setWake(w *controllerWake) {
	if s == nil {
		return
	}
	s.wake.Store(w)
}

// setRowsChanged stores fn as the listener for the rows each refresh pass
// changed. Nil-safe.
func (s *ledgerFeedSet) setRowsChanged(fn func([]beads.Bead)) {
	if s == nil {
		return
	}
	s.rowsChanged.Store(&fn)
}

func (s *ledgerFeedSet) setMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
}

func (s *ledgerFeedSet) add(e *ledgerFeedEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.entries = append(s.entries, e)
}

// snapshot prunes stopped caches' entries and returns the mode and the rest.
func (s *ledgerFeedSet) snapshot() (string, []*ledgerFeedEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return s.mode, slices.Clone(s.entries)
}

func (s *ledgerFeedSet) pruneLocked() {
	s.entries = slices.DeleteFunc(s.entries, func(e *ledgerFeedEntry) bool {
		if e.cache == nil {
			return false
		}
		select {
		case <-e.cache.Stopped():
			return true
		default:
			return false
		}
	})
}

// legacyWakeOf returns w when it is a legacy wake, and nil under v2, whose
// planner learns of the feed's changes from their events (critic D7).
func legacyWakeOf(w *controllerWake) *controllerWake {
	if w == nil || w.planner != nil {
		return nil
	}
	return w
}

// ledgerFeedRequest asks wrapWithCachingStoreFeed to tail one scope.
type ledgerFeedRequest struct {
	set   *ledgerFeedSet
	scope string
	mode  string
	// admit is the scope's gate, the reconcile gate's predicate.
	admit func() bool
}

// ledgerFeedRequest builds the feed request for one scope under cfg's mode,
// or nil when cs has no feed set.
func (cs *controllerState) ledgerFeedRequest(cfg *config.City, scope string, admit func() bool) *ledgerFeedRequest {
	if cs == nil || cs.ledgerFeeds == nil {
		return nil
	}
	mode := config.BeadsLedgerFeedOff
	if cfg != nil {
		mode = cfg.Beads.NormalizedLedgerFeed()
	}
	cs.ledgerFeeds.setMode(mode)
	return &ledgerFeedRequest{set: cs.ledgerFeeds, scope: scope, mode: mode, admit: admit}
}

// ledgerFeedAttachment is the attach decision for one cache: a feed, or a
// scan-only entry recording why there is none.
type ledgerFeedAttachment struct {
	entry *ledgerFeedEntry
}

// attachLedgerFeed decides whether store's cache gets a feed (it needs mode
// auto, a background-refreshed cache with a cancellable ctx, and a store that
// carries a journal, active or not). It returns nil when req is nil or the
// mode is off.
func attachLedgerFeed(ctx context.Context, req *ledgerFeedRequest, store beads.Store, backgroundRefresh bool) *ledgerFeedAttachment {
	if req == nil || req.set == nil || req.mode != config.BeadsLedgerFeedAuto {
		return nil
	}
	entry := &ledgerFeedEntry{scope: req.scope}
	journal, lane, reason := ledgerFeedLane(store)
	entry.lane = lane
	switch {
	case !backgroundRefresh:
		entry.reason = "suspended at start"
	case ctx == nil || ctx.Done() == nil:
		entry.reason = "no background refresh"
	case journal == nil:
		entry.reason = reason
	default:
		entry.feed = newLedgerFeed(req, journal, lane)
	}
	return &ledgerFeedAttachment{entry: entry}
}

// ledgerFeedLane finds the journal a feed would tail through store, active
// or not, and names the lane, or says why there is none. Whether the journal
// is active is the reads' answer (ErrEventsJournalInactive), so a scope
// whose journal turns on after its store was built is tailed without a
// rebuild.
func ledgerFeedLane(store beads.Store) (journalops.Journal, string, string) {
	carrier := beads.EventsJournalCarrier(store)
	if journal, ok := carrier.(journalops.Journal); ok {
		return journal, fmt.Sprintf("%T", carrier), ""
	}
	return nil, fmt.Sprintf("%T", store), fmt.Sprintf("lane %T cannot be tailed", store)
}

// observe returns the onChange the cache is built with: onChange, watched by
// the feed's miss detector when there is a feed.
func (a *ledgerFeedAttachment) observe(onChange cacheOnChange) cacheOnChange {
	if a == nil || a.entry.feed == nil {
		return onChange
	}
	return a.entry.feed.miss.observe(onChange)
}

// bind registers the entry over cache and, for a feed, takes its baseline:
// the head read that must come before the cache's first read of the scope.
func (a *ledgerFeedAttachment) bind(ctx context.Context, set *ledgerFeedSet, cache *beads.CachingStore) {
	if a == nil {
		return
	}
	a.entry.cache = cache
	if f := a.entry.feed; f != nil {
		f.cache = cache
		f.baseline(ctx)
	}
	set.add(a.entry)
}

// start runs the feed, when there is one, until its cache stops or ctx ends.
func (a *ledgerFeedAttachment) start(ctx context.Context) {
	if a == nil || a.entry.feed == nil {
		return
	}
	go a.entry.feed.run(ctx)
}

// cacheOnChange is a CachingStore's onChange.
type cacheOnChange = func(source beads.ChangeSource, eventType, beadID, runID, sessionID, stepID string, dependsOnStepIDs *[]string, payload json.RawMessage)

// ledgerFeed tails one scope's journal into its cache.
type ledgerFeed struct {
	set    *ledgerFeedSet
	scope  string
	lane   string
	admit  func() bool
	cache  *beads.CachingStore
	src    *gatedJournal
	tailer *ledgerfeed.Tailer
	miss   *ledgerFeedMissDetector
	now    func() time.Time
	// late is set when the baseline before PrimeActive failed; run retries it.
	late    bool
	started time.Time

	// refreshMu serializes the page applies and the retry passes.
	refreshMu sync.Mutex

	mu sync.Mutex
	// readOK is set by the feed's first good journal read; until then a
	// feed waiting for its journal is reported as on the re-scan.
	readOK             bool
	state              string
	stateSince         time.Time
	reason             string
	stats              ledgerFeedCounters
	lastRefreshErrorAt time.Time
	lastGiveUpAt       time.Time
	// retries are the dropped rows awaiting a retry, by id.
	retries map[string]ledgerFeedRetry
	// closed are the rows refreshed closed on close evidence, by when
	// (dropSeenCloses).
	closed map[string]time.Time
}

// ledgerFeedRetry is one dropped row awaiting a retry.
type ledgerFeedRetry struct {
	// closing carries the page's evidence of a recent close or open state.
	closing bool
	// seenOpen carries the page's open post-image (RowRefreshOptions.SeenOpen).
	seenOpen bool
	// drops counts the refreshes of the row the feed dropped, but for the
	// ones that yielded to a local write in flight.
	drops int
	// firstYield is when a refresh of the row first yielded to a local write
	// in flight, or zero.
	firstYield time.Time
}

// ledgerFeedCounters are a feed's own counters; the tailer keeps the rest.
type ledgerFeedCounters struct {
	ids, refreshed, fenced, unavailable, refreshErrors, unowned int64
	unheldCloses, wakes, retryGiveUps                           int64
	wakeTimes                                                   []time.Time
	actorRows                                                   map[string]int64
	ages, applies                                               []time.Duration
}

func newLedgerFeed(req *ledgerFeedRequest, journal journalops.Journal, lane string) *ledgerFeed {
	admit := req.admit
	if admit == nil {
		admit = func() bool { return true }
	}
	f := &ledgerFeed{
		set:     req.set,
		scope:   req.scope,
		lane:    lane,
		admit:   admit,
		now:     time.Now,
		started: time.Now(),
		state:   ledgerFeedStateStarting,
	}
	f.stateSince = f.started
	f.miss = newLedgerFeedMissDetector(f.now)
	f.miss.progress = func() (int64, int64) {
		st := f.tailer.Stats()
		return st.Seq, st.Head
	}
	f.src = &gatedJournal{
		inner:  journal,
		admit:  admit,
		work:   req.set.hooks.beginWork,
		idle:   ledgerFeedIdlePoll,
		gated:  func() { f.miss.noteUnhealthy(f.now()); f.setState(ledgerFeedStateGated, errLedgerFeedGated) },
		readOK: f.noteReadOK,
		resync: func() { f.miss.noteUnhealthy(f.now()); f.cache.RequestReconcile() },
	}
	f.tailer = &ledgerfeed.Tailer{
		Source:    f.src,
		Apply:     f.apply,
		OnGap:     f.onGap,
		OnError:   f.onError,
		PageLimit: ledgerFeedPageLimit,
		Poll:      ledgerFeedPoll,
		IdlePoll:  ledgerFeedIdlePoll,
	}
	return f
}

// baseline reads the journal head before the cache's first read of the
// scope. It never waits on a closed gate; a failure or a timeout leaves the
// feed late, and run retries the baseline and then forces a re-scan.
func (f *ledgerFeed) baseline(ctx context.Context) {
	bctx, cancel := context.WithTimeout(ctx, ledgerFeedBaselineTimeout)
	defer cancel()
	if _, err := f.tailer.Baseline(bctx); err != nil {
		f.late = true
		f.miss.noteUnhealthy(f.now())
		f.setState(ledgerFeedStateLate, err)
		return
	}
	f.setState(ledgerFeedStateTailing, nil)
}

// run tails until the cache stops, ctx ends, or the journal refuses for good.
// The tail never starts from seq 0: without a baseline it takes one first.
func (f *ledgerFeed) run(parent context.Context) {
	ctx, cancel := contextUntil(parent, f.cache.Stopped())
	defer cancel()
	f.src.wait.Store(true)
	if f.late {
		if !f.lateBaseline(ctx) {
			return
		}
		f.miss.noteUnhealthy(f.now())
		f.cache.RequestReconcile()
		f.setState(ledgerFeedStateTailing, nil)
	}
	go f.retryLoop(ctx)
	err := f.tailer.Run(ctx)
	if ctx.Err() != nil {
		return
	}
	f.stop(err)
}

// retryLoop retries the dropped rows every poll until ctx ends, so a retry
// does not wait for the next journal row.
func (f *ledgerFeed) retryLoop(ctx context.Context) {
	tick := time.NewTicker(ledgerFeedPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if f.pendingRetries() > 0 {
				f.retryPending()
			}
		}
	}
}

func (f *ledgerFeed) pendingRetries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.retries)
}

// retryPending refreshes the rows kept for a retry, while the gate admits it.
func (f *ledgerFeed) retryPending() {
	end := f.set.hooks.beginWork()
	defer end()
	if !f.admit() {
		return
	}
	f.refreshMu.Lock()
	defer f.refreshMu.Unlock()
	f.mu.Lock()
	ids := make([]string, 0, len(f.retries))
	closing := make(map[string]bool, len(f.retries))
	seenOpen := make(map[string]bool, len(f.retries))
	for id, r := range f.retries {
		ids = append(ids, id)
		closing[id], seenOpen[id] = r.closing, r.seenOpen
	}
	f.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	counts := f.refreshRows(ids, closing, seenOpen, f.now())
	f.mu.Lock()
	f.foldCountsLocked(counts)
	f.mu.Unlock()
}

// stop records a tail the journal ended for good. The miss detector stops
// judging: a stopped feed's silence proves nothing.
func (f *ledgerFeed) stop(err error) {
	f.miss.disable()
	f.setState(ledgerFeedStateStopped, err)
}

// lateBaseline retries the baseline until it succeeds, reporting false when
// ctx ended or the journal refused for good.
func (f *ledgerFeed) lateBaseline(ctx context.Context) bool {
	for {
		if _, err := f.tailer.Baseline(ctx); err == nil {
			return true
		} else if ctx.Err() != nil {
			return false
		} else if stop := f.onError(err); stop != nil {
			f.stop(stop)
			return false
		}
		if err := sleepUntil(ctx, ledgerFeedIdlePoll); err != nil {
			return false
		}
	}
}

// onError classifies a failed step for the tailer (its OnError contract):
// only a backend that cannot journal stops the tail; every other failure is
// retried. A closed store is retried too: the feed ends with its cache
// (closeBeadStoreHandle stops the cache before closing the store), and a
// proxied store's read can surface a replaced leaf's ErrStoreClosed while
// the store itself stays open and re-pins. An inactive journal owes a
// re-scan at the first good read after it, since unjournaled writes leave no
// seq gap.
func (f *ledgerFeed) onError(err error) error {
	f.miss.noteUnhealthy(f.now())
	switch {
	case errors.Is(err, beads.ErrEventsJournalUnsupported):
		return err
	case errors.Is(err, beads.ErrEventsJournalInactive):
		f.src.needResync.Store(true)
		f.setState(ledgerFeedStateInactive, err)
	case errors.Is(err, beads.ErrEventsJournalUnavailable):
		f.setState(ledgerFeedStateUnavailable, err)
	case errors.Is(err, errLedgerFeedGated):
		f.setState(ledgerFeedStateGated, err)
	default:
		f.setState(ledgerFeedStateRetrying, err)
	}
	return nil
}

// onGap resyncs the cache from its source of truth. Returning before the
// re-scan runs is safe for the active rows: the tailer read gap.To's head
// before calling this, so a scan requested now lists every row active at or
// below it. The scan lists no closed rows, so the close of a row the cache
// never held, made inside the skipped range, is not recovered (and not
// handed to the autoclose sweep) unless an edit of the row lands within its
// close's window.
func (f *ledgerFeed) onGap(_ context.Context, gap ledgerfeed.Gap) error {
	f.miss.noteUnhealthy(f.now())
	f.cache.RequestReconcile()
	log.Printf("ledger-feed: %s: %s gap at seq %d, re-scanning and continuing from seq %d", f.scope, gap.Kind, gap.From.Seq, gap.To.Seq)
	return nil
}

// apply refreshes every distinct id a page names that the cache owns, and
// the rows kept for a retry. A row whose refresh is dropped is kept for a
// retry rather than stalling the feed; only a closed gate fails the page.
func (f *ledgerFeed) apply(_ context.Context, page journalops.Page) error {
	end := f.set.hooks.beginWork()
	defer end()
	if !f.admit() {
		return errLedgerFeedGated
	}
	f.refreshMu.Lock()
	defer f.refreshMu.Unlock()
	start := f.now()
	plan := planLedgerFeedPage(page.Rows)
	f.dropSeenCloses(&plan, start)
	ids, closing, seenOpen := f.withRetries(plan)
	counts := f.refreshRows(ids, closing, seenOpen, start)
	now := f.now()
	f.notePage(plan, page.Rows, counts, now, now.Sub(start))
	return nil
}

// withRetries adds the rows kept for a retry to a page's ids, carrying their
// close evidence and open images.
func (f *ledgerFeed) withRetries(plan ledgerFeedPagePlan) ([]string, map[string]bool, map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.retries) == 0 {
		return plan.ids, plan.closing, plan.seenOpen
	}
	ids := slices.Clone(plan.ids)
	closing, seenOpen := maps.Clone(plan.closing), maps.Clone(plan.seenOpen)
	if closing == nil {
		closing = make(map[string]bool, len(f.retries))
	}
	if seenOpen == nil {
		seenOpen = make(map[string]bool, len(f.retries))
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	extra := make([]string, 0, len(f.retries))
	for id, r := range f.retries {
		if r.closing {
			closing[id] = true
		}
		if r.seenOpen {
			seenOpen[id] = true
		}
		if _, ok := seen[id]; !ok {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	return append(ids, extra...), closing, seenOpen
}

// refreshRows refreshes ids through the cache, keeps the dropped ones for a
// retry, wakes the legacy reconciler once when a refresh changed a row, and
// hands the changed rows to the set's rowsChanged listener.
// A close the refresh announced only on the page's word, of a row the cache
// never held or held closed already, is handed to the autoclose sweep. The
// caller holds refreshMu and counts the work.
func (f *ledgerFeed) refreshRows(ids []string, closing, seenOpen map[string]bool, now time.Time) ledgerFeedCounters {
	var (
		changed   []beads.Bead
		reconcile bool
		counts    ledgerFeedCounters
		deferred  []string
	)
	refreshChanged := make(map[string]bool, len(ids))
	// drop keeps a row whose refresh failed with err for a retry. A closing
	// row goes to the sweep at once when the drop was a failure (the sweep
	// reads the row live whatever the retry finds), but a fenced one is left
	// to the retry, which announces and defers a never-held close itself: a
	// fence is mostly the controller's own write in flight, and deferring
	// each would flood the sweep. A yield to that write spends none of the
	// row's drops (keepForRetry). A row given up goes to the sweep either way.
	drop := func(id string, err error) {
		fenced := errors.Is(err, beads.ErrRowRefreshFenced)
		kept := f.keepForRetry(id, closing[id], seenOpen[id], errors.Is(err, beads.ErrRowRefreshYielded), now)
		if closing[id] && (!fenced || !kept) {
			deferred = append(deferred, id)
		}
		if kept {
			return
		}
		counts.retryGiveUps++
		reconcile = true
		f.logGiveUp(id, err)
	}
	for _, id := range ids {
		if !f.cache.OwnsBeadID(id) {
			counts.unowned++
			f.forgetRetry(id)
			continue
		}
		rep, err := f.cache.RefreshRowReport(id, beads.RowRefreshOptions{AnnounceUnheldClose: closing[id], SeenOpen: seenOpen[id], YieldToLocalWrites: true})
		switch {
		case err == nil || errors.Is(err, beads.ErrNotFound):
			counts.refreshed++
			f.noteRefreshed(id, closing[id], rep, now)
		case errors.Is(err, beads.ErrRowRefreshFenced):
			// A newer local write, event or scan owns the row, or a local
			// write that may touch it is in flight (ErrRowRefreshYielded);
			// the retry finds which.
			counts.fenced++
			drop(id, err)
			continue
		case errors.Is(err, beads.ErrCacheUnavailable):
			counts.unavailable++
			reconcile = true
			drop(id, err)
			continue
		default:
			counts.refreshErrors++
			reconcile = true
			f.logRefreshError(id, err)
			drop(id, err)
			continue
		}
		refreshChanged[id] = rep.Event != ""
		if rep.Event != "" {
			changed = append(changed, rep.Bead)
		}
		if rep.Event == "bead.closed" && (!rep.Held || rep.Reclosed) {
			if !rep.Held {
				counts.unheldCloses++
			}
			deferred = append(deferred, id)
		}
	}
	f.miss.noteJournal(ids, refreshChanged, now)
	if f.set.hooks.deferClose != nil {
		for _, id := range deferred {
			f.set.hooks.deferClose(id)
		}
	}
	if reconcile {
		f.miss.noteUnhealthy(now)
		f.cache.RequestReconcile()
	}
	if fn := f.set.rowsChanged.Load(); fn != nil && *fn != nil && len(changed) > 0 {
		(*fn)(changed)
	}
	if wake := legacyWakeOf(f.set.wake.Load()); wake != nil && len(changed) > 0 {
		keys := ledgerFeedWakeKeys(changed)
		wake.Enqueue(wakeReasonLedgerFeed, keys...)
		counts.wakes++
	}
	return counts
}

// keepForRetry keeps a dropped row for a retry, reporting false when its
// retries ran out or the retry set is full: the caller gives it up. A
// refresh that yielded to a local write in flight is not counted as a drop;
// the row waits for the write, for ledgerFeedYieldLimit from its first
// yield at most.
func (f *ledgerFeed) keepForRetry(id string, closing, seenOpen, yielded bool, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, kept := f.retries[id]
	if !kept && len(f.retries) >= ledgerFeedRetryCap {
		return false
	}
	r.closing = r.closing || closing
	r.seenOpen = r.seenOpen || seenOpen
	switch {
	case !yielded:
		r.drops++
	case r.firstYield.IsZero():
		r.firstYield = now
	}
	if r.drops >= ledgerFeedRetryLimit || (!r.firstYield.IsZero() && now.Sub(r.firstYield) >= ledgerFeedYieldLimit) {
		delete(f.retries, id)
		return false
	}
	if f.retries == nil {
		f.retries = make(map[string]ledgerFeedRetry)
	}
	f.retries[id] = r
	return true
}

func (f *ledgerFeed) forgetRetry(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.retries, id)
}

// noteRefreshed records a refresh that succeeded: the row's retry is done,
// a row found closed on close evidence is remembered as read closed, and a
// row found open or missing is forgotten.
func (f *ledgerFeed) noteRefreshed(id string, closing bool, rep beads.RowRefresh, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.retries, id)
	switch {
	case !rep.Found || rep.Bead.Status != "closed":
		delete(f.closed, id)
	case closing:
		f.rememberClosedLocked(id, now)
	}
}

// rememberClosedLocked records id as read closed at now. A full set sheds
// its expired entries first, then its oldest. Caller holds f.mu.
func (f *ledgerFeed) rememberClosedLocked(id string, now time.Time) {
	if f.closed == nil {
		f.closed = make(map[string]time.Time)
	}
	if _, known := f.closed[id]; !known && len(f.closed) >= ledgerFeedClosedCap {
		var oldest string
		var oldestAt time.Time
		for seen, at := range f.closed {
			switch {
			case now.Sub(at) > ledgerFeedClosedKeep:
				delete(f.closed, seen)
			case oldest == "" || at.Before(oldestAt):
				oldest, oldestAt = seen, at
			}
		}
		if len(f.closed) >= ledgerFeedClosedCap {
			delete(f.closed, oldest)
		}
	}
	f.closed[id] = now
}

// dropSeenCloses drops the evidence of the page's ids whose only evidence is
// a closed post-image and which this feed read closed within
// ledgerFeedClosedKeep: that is an edit inside the window of a close already
// read, and a re-scan may have evicted the row since, so refreshing it as
// closing would announce the close again.
func (f *ledgerFeed) dropSeenCloses(plan *ledgerFeedPagePlan, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range plan.closedImage {
		if at, ok := f.closed[id]; ok && now.Sub(at) <= ledgerFeedClosedKeep {
			delete(plan.closing, id)
		}
	}
}

// notePage folds one applied page into the counters.
func (f *ledgerFeed) notePage(plan ledgerFeedPagePlan, rows []journalops.Row, counts ledgerFeedCounters, now time.Time, took time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &f.stats
	s.ids += int64(len(plan.ids))
	f.foldCountsLocked(counts)
	if s.actorRows == nil {
		s.actorRows = make(map[string]int64)
	}
	for actor, n := range plan.actors {
		if _, known := s.actorRows[actor]; !known && actor != ledgerFeedActorOther && len(s.actorRows) >= ledgerFeedActorCap {
			actor = ledgerFeedActorOther
		}
		s.actorRows[actor] += n
	}
	for _, row := range rows {
		if ts, err := time.Parse(time.RFC3339Nano, row.TS); err == nil {
			s.ages = appendSample(s.ages, max(now.Sub(ts), 0))
		}
	}
	s.applies = appendSample(s.applies, took)
	if f.state != ledgerFeedStateTailing {
		f.setStateLocked(ledgerFeedStateTailing, nil, now)
	}
}

// foldCountsLocked adds one refresh pass's counters. Caller holds f.mu.
func (f *ledgerFeed) foldCountsLocked(counts ledgerFeedCounters) {
	s := &f.stats
	s.refreshed += counts.refreshed
	s.fenced += counts.fenced
	s.unavailable += counts.unavailable
	s.refreshErrors += counts.refreshErrors
	s.unowned += counts.unowned
	s.unheldCloses += counts.unheldCloses
	s.retryGiveUps += counts.retryGiveUps
	if counts.wakes > 0 {
		now := f.now()
		s.wakes += counts.wakes
		s.wakeTimes = append(pruneBefore(s.wakeTimes, now.Add(-time.Minute)), now)
		if len(s.wakeTimes) > ledgerFeedSampleCap {
			s.wakeTimes = s.wakeTimes[len(s.wakeTimes)-ledgerFeedSampleCap:]
		}
	}
}

func (f *ledgerFeed) logRefreshError(id string, err error) {
	if f.logDue(&f.lastRefreshErrorAt) {
		log.Printf("ledger-feed: %s: refreshing %s: %v (kept for a retry; a re-scan is requested)", f.scope, id, err)
	}
}

// logGiveUp reports a row given up after its last dropped refresh, err.
func (f *ledgerFeed) logGiveUp(id string, err error) {
	if f.logDue(&f.lastGiveUpAt) {
		log.Printf("ledger-feed: %s: gave up retrying %s: %v (a re-scan is requested; a close goes to the autoclose sweep)", f.scope, id, err)
	}
}

// logDue reports whether a warning last logged at *last is due again,
// recording now as its time when it is.
func (f *ledgerFeed) logDue(last *time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if now.Sub(*last) < ledgerFeedLogEvery {
		return false
	}
	*last = now
	return true
}

// noteReadOK marks a feed that was failing healthy again on a good read.
func (f *ledgerFeed) noteReadOK() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readOK = true
	switch f.state {
	case ledgerFeedStateGated, ledgerFeedStateInactive, ledgerFeedStateUnavailable, ledgerFeedStateRetrying:
		f.setStateLocked(ledgerFeedStateTailing, nil, f.now())
	}
}

// setState records the feed's state, logging only transitions.
func (f *ledgerFeed) setState(state string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setStateLocked(state, err, f.now())
}

func (f *ledgerFeed) setStateLocked(state string, err error, now time.Time) {
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	if state == f.state {
		f.reason = reason
		return
	}
	prev := f.state
	f.state, f.stateSince, f.reason = state, now, reason
	if err != nil {
		log.Printf("ledger-feed: %s: %s -> %s: %v", f.scope, prev, state, err)
	} else {
		log.Printf("ledger-feed: %s: %s -> %s", f.scope, prev, state)
	}
}

// gatedJournal is the feed's journal read: it reads only while the scope's
// gate admits it, and counts every read as store work. Before the feed runs
// (wait false) a closed gate fails the read; afterwards the read waits.
type gatedJournal struct {
	inner journalops.Journal
	admit func() bool
	work  func() func()
	idle  time.Duration
	// gated, readOK and resync report to the feed: a read held by the gate,
	// a good read, and the good read a resync was owed at.
	gated, readOK, resync func()

	wait       atomic.Bool
	needResync atomic.Bool
}

// ReadEventsJournalPage implements journalops.Journal.
func (g *gatedJournal) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	for {
		end := g.work()
		if g.admit() {
			page, err := g.inner.ReadEventsJournalPage(ctx, since, limit)
			end()
			if err == nil {
				if g.needResync.CompareAndSwap(true, false) {
					g.resync()
				}
				g.readOK()
			}
			return page, err
		}
		end()
		if !g.wait.Load() {
			return journalops.Page{}, errLedgerFeedGated
		}
		g.gated()
		if err := sleepUntil(ctx, g.idle); err != nil {
			return journalops.Page{}, err
		}
	}
}

// ledgerFeedPagePlan is what one page asks of the feed.
type ledgerFeedPagePlan struct {
	// ids are the distinct ids the page's rows name, in first-appearance
	// order, comments skipped.
	ids []string
	// closing marks the ids with evidence of a recent close or an open
	// state (rowCloseEvidence).
	closing map[string]bool
	// closedImage marks the closing ids whose only evidence is a closed
	// post-image, which an edit inside a close's window carries too.
	closedImage map[string]bool
	// seenOpen marks the ids the page shows open (an open post-image): a
	// close read after it is a new close (RowRefreshOptions.SeenOpen).
	seenOpen map[string]bool
	// actors counts rows per actor, capped.
	actors map[string]int64
}

// planLedgerFeedPage reads one page's rows into a plan.
func planLedgerFeedPage(rows []journalops.Row) ledgerFeedPagePlan {
	plan := ledgerFeedPagePlan{closing: map[string]bool{}, closedImage: map[string]bool{}, seenOpen: map[string]bool{}, actors: map[string]int64{}}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		actor := row.Actor
		if _, known := plan.actors[actor]; !known && len(plan.actors) >= ledgerFeedActorCap {
			actor = ledgerFeedActorOther
		}
		plan.actors[actor]++
		if row.IssueID == "" || row.Op == journalOpComment {
			continue
		}
		if _, ok := seen[row.IssueID]; !ok {
			seen[row.IssueID] = struct{}{}
			plan.ids = append(plan.ids, row.IssueID)
		}
		plan.noteEvidence(row)
	}
	return plan
}

// noteEvidence folds one row's close evidence into the plan. Strong evidence
// and an open post-image for an id outweigh a closed post-image in either
// order, and an open post-image marks the id seen open.
func (p *ledgerFeedPagePlan) noteEvidence(row journalops.Row) {
	switch rowCloseEvidence(row) {
	case closeEvidenceOpenImage:
		p.seenOpen[row.IssueID] = true
		fallthrough
	case closeEvidenceStrong:
		p.closing[row.IssueID] = true
		delete(p.closedImage, row.IssueID)
	case closeEvidenceClosedImage:
		if !p.closing[row.IssueID] {
			p.closing[row.IssueID] = true
			p.closedImage[row.IssueID] = true
		}
	}
}

// closeEvidence is what one journal row says of a recent close or an open
// state.
type closeEvidence int

const (
	// closeEvidenceNone: the row says neither.
	closeEvidenceNone closeEvidence = iota
	// closeEvidenceStrong: a create or close op.
	closeEvidenceStrong
	// closeEvidenceOpenImage: an open post-image, a create's included, which
	// also shows the row open (ledgerFeedPagePlan.seenOpen).
	closeEvidenceOpenImage
	// closeEvidenceClosedImage: a closed post-image whose closed_at sits
	// within ledgerFeedCloseWindow of the row's ts.
	closeEvidenceClosedImage
)

// rowCloseEvidence reads one row's close evidence. bd journals a close made
// by an update (bd update --status closed) as op update, so a closed
// post-image counts when its closed_at is as recent as the row. A reopen is
// an update whose post-image is open.
func rowCloseEvidence(row journalops.Row) closeEvidence {
	if row.Op == journalOpClose {
		return closeEvidenceStrong
	}
	image, ok := parsePostImage(row.IssueJSON)
	switch {
	case ok && image.Status != "" && image.Status != "closed":
		return closeEvidenceOpenImage
	case row.Op == journalOpCreate:
		return closeEvidenceStrong
	case !ok || image.Status == "":
		return closeEvidenceNone
	case image.closedNear(row.TS):
		return closeEvidenceClosedImage
	}
	return closeEvidenceNone
}

// postImage is the part of a row's post-image the feed reads.
type postImage struct {
	Status   string `json:"status"`
	ClosedAt string `json:"closed_at"`
}

// parsePostImage decodes a row's post-image, reporting false for a row with
// none or one that does not decode.
func parsePostImage(issueJSON string) (postImage, bool) {
	var image postImage
	if issueJSON == "" || json.Unmarshal([]byte(issueJSON), &image) != nil {
		return postImage{}, false
	}
	return image, true
}

// closedNear reports whether the image's closed_at sits within
// ledgerFeedCloseWindow of ts, either side.
func (p postImage) closedNear(ts string) bool {
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return false
	}
	closedAt, err := time.Parse(time.RFC3339Nano, p.ClosedAt)
	if err != nil {
		return false
	}
	d := at.Sub(closedAt)
	return d >= -ledgerFeedCloseWindow && d <= ledgerFeedCloseWindow
}

// ledgerFeedWakeKeys are the keys one page's legacy wake names (critic A2):
// the allocator always, each changed session row up to the cap, and the
// control dispatcher when a changed row belongs to a graph.
func ledgerFeedWakeKeys(changed []beads.Bead) []reconcilekey.Key {
	keys := []reconcilekey.Key{reconcilekey.Allocator()}
	sessions := make(map[string]struct{})
	dispatch := false
	for _, b := range changed {
		if session.IsSessionBeadOrRepairable(b) && len(sessions) < ledgerFeedWakeSessionCap {
			if _, dup := sessions[b.ID]; !dup {
				sessions[b.ID] = struct{}{}
				keys = append(keys, reconcilekey.Session(b.ID))
			}
		}
		if b.Metadata[beadmeta.KindMetadataKey] != "" || b.Metadata[beadmeta.RootBeadIDMetadataKey] != "" {
			dispatch = true
		}
	}
	if dispatch {
		keys = append(keys, reconcilekey.ControlDispatch())
	}
	return keys
}

// contextUntil returns a context canceled when parent is or done closes.
func contextUntil(parent context.Context, done <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// sleepUntil waits d or until ctx ends.
func sleepUntil(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func appendSample(samples []time.Duration, d time.Duration) []time.Duration {
	if len(samples) >= ledgerFeedSampleCap {
		samples = samples[1:]
	}
	return append(samples, d)
}

func pruneBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	return times[i:]
}

// percentileMS is the nearest-rank p-th percentile of samples, in ms.
func percentileMS(samples []time.Duration, p float64) int64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	idx := int(float64(len(sorted))*p+0.999999) - 1
	idx = min(max(idx, 0), len(sorted)-1)
	return sorted[idx].Milliseconds()
}

// ledgerFeedStatus is the controller's answer to ledgerFeedStatusCommand.
// An empty Mode means the controller has no bead stores to report.
type ledgerFeedStatus struct {
	Mode   string                  `json:"mode"`
	Scopes []ledgerFeedScopeStatus `json:"scopes"`
}

// ledgerFeedScopeStatus is one scope's feed, or why it has none.
type ledgerFeedScopeStatus struct {
	Scope         string `json:"scope"`
	Lane          string `json:"lane"`
	State         string `json:"state"`
	StateAgeMS    int64  `json:"state_age_ms"`
	Reason        string `json:"reason,omitempty"`
	Seq           int64  `json:"seq"`
	Head          int64  `json:"head"`
	Lag           int64  `json:"lag"`
	Pages         int64  `json:"pages"`
	Rows          int64  `json:"rows"`
	IDs           int64  `json:"ids"`
	Refreshed     int64  `json:"refreshed"`
	Fenced        int64  `json:"fenced"`
	Unavailable   int64  `json:"unavailable"`
	RefreshErrors int64  `json:"refresh_errors"`
	Unowned       int64  `json:"unowned"`
	UnheldCloses  int64  `json:"unheld_closes"`
	// ReadOK reports a good journal read since the feed started; a feed
	// without one is waiting for its journal and the scope is on the re-scan.
	ReadOK            bool             `json:"read_ok"`
	PendingRetries    int64            `json:"pending_retries"`
	RetryGiveUps      int64            `json:"retry_give_ups"`
	Wakes             int64            `json:"wakes"`
	WakesLastMinute   int64            `json:"wakes_last_minute"`
	ReconcileRequests int64            `json:"reconcile_requests"`
	Gaps              map[string]int64 `json:"gaps,omitempty"`
	Misses            int64            `json:"misses"`
	ScanAfterRefresh  int64            `json:"scan_after_refresh"`
	RecentMisses      []string         `json:"recent_misses,omitempty"`
	ActorRows         map[string]int64 `json:"actor_rows,omitempty"`
	AgeP50MS          int64            `json:"age_p50_ms"`
	AgeP99MS          int64            `json:"age_p99_ms"`
	ApplyP99MS        int64            `json:"apply_p99_ms"`
	Errors            int64            `json:"errors"`
	LastError         string           `json:"last_error,omitempty"`
	LastErrorAgeMS    int64            `json:"last_error_age_ms,omitempty"`
	UptimeMS          int64            `json:"uptime_ms"`
}

// ledgerFeedStatusFor reads cityPath's feeds at now.
func ledgerFeedStatusFor(cityPath string, now time.Time) ledgerFeedStatus {
	v, ok := ledgerFeedSets.Load(normalizePathForCompare(cityPath))
	if !ok {
		return ledgerFeedStatus{}
	}
	return v.(*ledgerFeedSet).status(now)
}

func (s *ledgerFeedSet) status(now time.Time) ledgerFeedStatus {
	mode, entries := s.snapshot()
	out := ledgerFeedStatus{Mode: mode, Scopes: make([]ledgerFeedScopeStatus, 0, len(entries))}
	for _, e := range entries {
		if e.feed == nil {
			out.Scopes = append(out.Scopes, ledgerFeedScopeStatus{Scope: e.scope, Lane: e.lane, State: ledgerFeedStateScanOnly, Reason: e.reason})
			continue
		}
		out.Scopes = append(out.Scopes, e.feed.status(now))
	}
	sort.SliceStable(out.Scopes, func(i, j int) bool { return out.Scopes[i].Scope < out.Scopes[j].Scope })
	return out
}

func (f *ledgerFeed) status(now time.Time) ledgerFeedScopeStatus {
	ts := f.tailer.Stats()
	misses, scanAfterRefresh, recent := f.miss.report(now)
	st := ledgerFeedScopeStatus{
		Scope:            f.scope,
		Lane:             f.lane,
		Seq:              ts.Seq,
		Head:             ts.Head,
		Lag:              ts.Lag,
		Pages:            ts.Pages,
		Rows:             ts.RowsApplied,
		Errors:           ts.Errors,
		Misses:           misses,
		ScanAfterRefresh: scanAfterRefresh,
		RecentMisses:     recent,
		UptimeMS:         now.Sub(f.started).Milliseconds(),
	}
	if len(ts.Gaps) > 0 {
		st.Gaps = make(map[string]int64, len(ts.Gaps))
		for kind, n := range ts.Gaps {
			st.Gaps[string(kind)] = n
		}
	}
	if ts.LastError != nil {
		st.LastError = ts.LastError.Error()
		st.LastErrorAgeMS = now.Sub(ts.LastErrorAt).Milliseconds()
	}
	if f.cache != nil {
		st.ReconcileRequests = f.cache.Stats().ReconcileRequests
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.stats
	st.State, st.Reason, st.ReadOK = f.state, f.reason, f.readOK
	st.StateAgeMS = now.Sub(f.stateSince).Milliseconds()
	st.IDs, st.Refreshed, st.Fenced = s.ids, s.refreshed, s.fenced
	st.Unavailable, st.RefreshErrors, st.Unowned = s.unavailable, s.refreshErrors, s.unowned
	st.UnheldCloses, st.Wakes = s.unheldCloses, s.wakes
	st.PendingRetries, st.RetryGiveUps = int64(len(f.retries)), s.retryGiveUps
	st.WakesLastMinute = int64(len(pruneBefore(s.wakeTimes, now.Add(-time.Minute))))
	if len(s.actorRows) > 0 {
		st.ActorRows = make(map[string]int64, len(s.actorRows))
		for actor, n := range s.actorRows {
			st.ActorRows[actor] = n
		}
	}
	st.AgeP50MS = percentileMS(s.ages, 0.50)
	st.AgeP99MS = percentileMS(s.ages, 0.99)
	st.ApplyP99MS = percentileMS(s.applies, 0.99)
	return st
}

// queryLedgerFeedStatus asks the city's controller; one that does not
// answer, or predates the command, is an error.
func queryLedgerFeedStatus(cityPath string) (ledgerFeedStatus, error) {
	var st ledgerFeedStatus
	resp, err := sendControllerCommandWithTimeouts(cityPath, ledgerFeedStatusCommand, 500*time.Millisecond, 500*time.Millisecond, 2*time.Second)
	if err == nil {
		err = json.Unmarshal(resp, &st)
	}
	return st, err
}

// The miss detector's bounds.
const (
	// ledgerFeedMissWindow is how far back a journal row for an id excuses a
	// scan change of it, and how far back an unhealthy feed voids one.
	ledgerFeedMissWindow = 5 * time.Minute
	// ledgerFeedMissGrace is how long after a scan change a journal row for
	// its id still excuses it: the scan can list a row before the feed reads
	// its journal row.
	ledgerFeedMissGrace = 10 * time.Second
	// ledgerFeedMissCap bounds the ids and pending scan changes remembered;
	// past it they are dropped, which the detector treats as unhealthy.
	ledgerFeedMissCap = 1 << 16
	// ledgerFeedRecentMissCap is how many missed ids doctor lists.
	ledgerFeedRecentMissCap = 8
)

// ledgerFeedMissDetector counts the creates and closes the re-scan found
// that the feed never saw a journal row for: a writer that does not journal
// (BD_EVENTS_JOURNAL=0, an old bd, raw SQL, a Dolt merge). It also counts
// the scan changes of ids whose last refresh found nothing new, the E3
// visibility suspect. Updates are not watched: ready-projection churn makes
// them noisy. While the feed is unhealthy (a late baseline, a gap, a read
// error, a closed gate, a dropped refresh, a requested re-scan) its silence
// proves nothing, so a scan change within the window of one is voided. A
// feed that is behind has simply not read the row yet, so a change waits
// for the checkpoint to pass the head the feed had seen when it was found.
type ledgerFeedMissDetector struct {
	now func() time.Time
	// progress reports the feed's checkpoint seq and the journal head it
	// last saw; nil reads as caught up.
	progress func() (seq, head int64)

	mu          sync.Mutex
	seen        map[string]ledgerFeedJournalSeen
	pending     []ledgerFeedScanChange
	unhealthyAt time.Time
	expiredAt   time.Time
	disabled    bool
	misses      int64
	scanAfter   int64
	recent      []string
}

// ledgerFeedJournalSeen is when the feed last saw an id's journal row, and
// whether its refresh changed the cache (unknown counts as changed).
type ledgerFeedJournalSeen struct {
	at      time.Time
	changed bool
}

// ledgerFeedScanChange is a scan create or close awaiting its verdict.
type ledgerFeedScanChange struct {
	id string
	at time.Time
	// head is the journal head the feed had seen when the scan reported the
	// change; the change is judged only once the checkpoint has passed it.
	head int64
}

func newLedgerFeedMissDetector(now func() time.Time) *ledgerFeedMissDetector {
	return &ledgerFeedMissDetector{now: now, seen: map[string]ledgerFeedJournalSeen{}}
}

// observe wraps onChange to watch the scan's creates and closes.
func (d *ledgerFeedMissDetector) observe(onChange cacheOnChange) cacheOnChange {
	return func(source beads.ChangeSource, eventType, beadID, runID, sessionID, stepID string, dependsOnStepIDs *[]string, payload json.RawMessage) {
		if source == beads.ChangeScan && (eventType == "bead.created" || eventType == "bead.closed") {
			d.noteScan(beadID, d.now())
		}
		onChange(source, eventType, beadID, runID, sessionID, stepID, dependsOnStepIDs, payload)
	}
}

// noteScan records a scan change of id at at.
func (d *ledgerFeedMissDetector) noteScan(id string, at time.Time) {
	_, head := d.readProgress()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.disabled {
		return
	}
	if seen, ok := d.seen[id]; ok && !seen.at.Before(at.Add(-ledgerFeedMissWindow)) {
		if !seen.changed {
			d.scanAfter++
		}
		return
	}
	if len(d.pending) >= ledgerFeedMissCap {
		d.unhealthyAt = at
		return
	}
	d.pending = append(d.pending, ledgerFeedScanChange{id: id, at: at, head: head})
}

// readProgress reads the feed's checkpoint and last seen head. It is called
// without d.mu held.
func (d *ledgerFeedMissDetector) readProgress() (seq, head int64) {
	if d.progress == nil {
		return 0, 0
	}
	return d.progress()
}

// noteJournal records the ids one page named, and which of them a refresh
// changed (an id absent from changed was not refreshed, and counts as
// changed), then settles what is due.
func (d *ledgerFeedMissDetector) noteJournal(ids []string, changed map[string]bool, at time.Time) {
	seq, _ := d.readProgress()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.seen)+len(ids) > ledgerFeedMissCap {
		d.seen = map[string]ledgerFeedJournalSeen{}
		d.unhealthyAt = at
	}
	resolved := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		c, refreshed := changed[id]
		d.seen[id] = ledgerFeedJournalSeen{at: at, changed: c || !refreshed}
		resolved[id] = struct{}{}
	}
	d.pending = slices.DeleteFunc(d.pending, func(p ledgerFeedScanChange) bool {
		_, ok := resolved[p.id]
		return ok
	})
	d.settleLocked(at, seq)
}

// disable stops judging and drops what is pending: the feed has stopped.
func (d *ledgerFeedMissDetector) disable() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disabled = true
	d.pending = nil
}

// noteUnhealthy records that the feed's silence proves nothing at at.
func (d *ledgerFeedMissDetector) noteUnhealthy(at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if at.After(d.unhealthyAt) {
		d.unhealthyAt = at
	}
}

// report settles what is due at now and returns the counters.
func (d *ledgerFeedMissDetector) report(now time.Time) (misses, scanAfterRefresh int64, recent []string) {
	seq, _ := d.readProgress()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.settleLocked(now, seq)
	return d.misses, d.scanAfter, slices.Clone(d.recent)
}

// settleLocked decides every scan change whose grace has passed and whose
// head the checkpoint seq has passed, and expires journal sightings older
// than the window. A feed that is behind has simply not read the row yet.
func (d *ledgerFeedMissDetector) settleLocked(now time.Time, seq int64) {
	d.pending = slices.DeleteFunc(d.pending, func(p ledgerFeedScanChange) bool {
		if now.Sub(p.at) < ledgerFeedMissGrace || seq < p.head {
			return false
		}
		if d.unhealthyAt.IsZero() || d.unhealthyAt.Before(p.at.Add(-ledgerFeedMissWindow)) {
			d.misses++
			d.recent = append(d.recent, p.id)
			if len(d.recent) > ledgerFeedRecentMissCap {
				d.recent = d.recent[len(d.recent)-ledgerFeedRecentMissCap:]
			}
		}
		return true
	})
	if now.Sub(d.expiredAt) < ledgerFeedMissGrace {
		return
	}
	d.expiredAt = now
	cutoff := now.Add(-ledgerFeedMissWindow - ledgerFeedMissGrace)
	for id, seen := range d.seen {
		if seen.at.Before(cutoff) {
			delete(d.seen, id)
		}
	}
}

// ledgerFeedCityScope names the city scope in the feed's status.
const ledgerFeedCityScope = "city"

// ledgerFeedRigScope names a rig's scope in the feed's status.
func ledgerFeedRigScope(rigName string) string {
	return "rig:" + rigName
}
