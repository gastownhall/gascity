package dashboardbff

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
)

// The run-view summary is reconstructed from the per-city append-only event log
// (.gc/events.jsonl) instead of the supervisor's slow molecule/feed scans. A
// per-city tailer folds the log into a warm bead-derived RunSummary off the
// request path — cold-replay over the full history (rotated .gz archives
// included), then a read-only byte-offset tail of newly appended events — so a
// request serves a sub-second warm read and layers session health/census at
// request time from one loopback /v0 sessions read. Modeled on the citySampler:
// lazy per-city start, all the heavy work off the lock, a brief publish under
// the lock. The tail is pure-read (events.ReadFrom opens the log read-only), so
// it is never a second writer to the supervisor's own recorder.
var (
	// runTailPollInterval is how often the tail polls the active log for new
	// bytes. A var (not a const) so tests can shorten it.
	runTailPollInterval = 1 * time.Second
	// runColdLoadWait bounds how long a first request blocks for the cold replay
	// before returning a partial (warming) snapshot. A var so tests can shorten it.
	runColdLoadWait = 5 * time.Second
)

const runSessionsFetchTimeout = 10 * time.Second

// ── Tailer manager ────────────────────────────────────────────────────────

type runTailerManager struct {
	deps  Deps
	httpc *http.Client

	// sessionsCache and formulaCache absorb the two per-request loopback reads so
	// a burst of detail/summary GETs collapses onto one upstream fetch per key
	// (single-flight + TTL). See enrichment_cache.go for the contract.
	sessionsCache *singleFlightCache[string, cachedSessions]
	formulaCache  *singleFlightCache[formulaCacheKey, cachedFormulaDetail]

	// sessionsTTL is the sessionsCacheTTL package var captured at construction.
	// The sessions compute closure can run on the tailer loop's DETACHED prime
	// goroutine (which Stop deliberately does not join), so reading the mutable
	// package var there races with a test shortening it; the immutable capture
	// keeps that read race-free while tests keep the set-var-then-construct
	// convention.
	sessionsTTL time.Duration

	mu      sync.Mutex
	cities  map[string]*cityRunTailer
	ctx     context.Context
	wg      *sync.WaitGroup
	enabled bool
}

func newRunTailerManager(deps Deps) *runTailerManager {
	return &runTailerManager{
		deps:          deps,
		httpc:         &http.Client{Timeout: runSessionsFetchTimeout, Transport: deps.SelfReadTransport},
		cities:        make(map[string]*cityRunTailer),
		sessionsCache: newSingleFlightCache[string, cachedSessions](),
		formulaCache:  newSingleFlightCache[formulaCacheKey, cachedFormulaDetail](),
		sessionsTTL:   sessionsCacheTTL,
	}
}

// enable records the lifecycle context and waitgroup so lazily-started city
// tailers stop cleanly on shutdown (shared with the samplers' waitgroup).
func (m *runTailerManager) enable(ctx context.Context, wg *sync.WaitGroup) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctx = ctx
	m.wg = wg
	m.enabled = true
}

// ensure returns the tailer for a city, starting its background fold loop on
// first use once the manager has been enabled (Start called).
func (m *runTailerManager) ensure(name, eventsPath string) *cityRunTailer {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.cities[name]
	if !ok {
		t = &cityRunTailer{name: name, eventsPath: eventsPath, mgr: m, readyCh: make(chan struct{}), snapshotCache: newRunSnapshotCache(), detailMemo: newRunDetailMemo(), unknownRuns: newUnknownRunGrace()}
		m.cities[name] = t
	}
	if m.enabled && m.ctx != nil && !t.started {
		t.started = true
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			t.loop(m.ctx, m.wg)
		}()
	}
	return t
}

// ── Per-city tailer ───────────────────────────────────────────────────────

type cityRunTailer struct {
	name       string
	eventsPath string
	mgr        *runTailerManager

	started bool
	readyCh chan struct{} // closed once the cold replay attempt completes

	// snapshotCache caches the folded run snapshot (and the formula target
	// derived from it) per fold generation so a same-generation repeat request
	// reuses the fold instead of re-scanning the city's beads. detailMemo then
	// caches the built+marshaled detail on top, so an unchanged fold costs ~zero
	// CPU (no re-scan, no re-projection, no re-marshal). See rundetail_memo.go.
	snapshotCache *runSnapshotCache
	detailMemo    *runDetailMemo

	// unknownRuns grants a truly-unknown runId (a run slung but not yet folded
	// into this projection) a warming-grace window on the detail endpoints
	// before the terminal 404. See rundetail_grace.go.
	unknownRuns *unknownRunGrace

	mu      sync.RWMutex
	summary runproj.RunSummary
	marks   map[string]runproj.LaneProgressMark
	beads   []beads.Bead
	lastSeq uint64
	ready   bool

	// subMu guards the per-run detail-stream subscriber registry. It is a distinct
	// lock from mu so a stream broadcast never contends with the hot fold-publish
	// path's RLock/Lock; both are taken only briefly and never across a network
	// write. See rundetail_stream.go.
	subMu sync.Mutex
	subs  map[*detailStreamSub]struct{}
}

// tailState carries the fold cursor across poll iterations: the byte offset into
// the active log, the active file's identity (so a rotation is detected by
// dev/inode rather than a fragile size-shrink check), and the monotonic lane
// progress marks.
type tailState struct {
	offset     int64
	activeInfo os.FileInfo
	marks      map[string]runproj.LaneProgressMark
	// loggedDecodeMisses is the projector's cumulative bead.* decode-miss count
	// already surfaced to the log, so logDecodeMisses only warns on the delta.
	loggedDecodeMisses int
}

// captureTailCursor snapshots the active log's byte size and identity from a
// SINGLE os.Stat so the resume offset and the rotation-detection identity always
// describe the same file. Splitting them across two stats (a size stat then an
// identity stat) let a rotation land between the two and pair the old file's
// larger offset with the fresh file's identity; the first foldNext then saw no
// identity change, ReadFrom seeked past the fresh file's EOF, and every fresh
// event below the stale offset was silently dropped until restart. A rotation
// after this single snapshot is instead caught by foldNext's identity check; a
// rotation before it yields a consistent size+identity for the new active file.
func captureTailCursor(path string) *tailState {
	st := &tailState{}
	if info, err := os.Stat(path); err == nil {
		st.offset = info.Size()
		st.activeInfo = info
	}
	return st
}

// loop cold-replays the event log, publishes the bead-derived summary, then
// tails newly appended events and republishes on each change. All folding and
// summary-building happens on loop-owned locals; only the publish takes the lock.
func (t *cityRunTailer) loop(ctx context.Context, wg *sync.WaitGroup) {
	proj := runproj.NewProjector()

	// Capture the active log size and identity BEFORE the cold replay so the tail
	// resumes from exactly there. Any event appended during (or just after) the
	// replay lands in [offset, EOF) and is re-read by the first tail poll; the seq
	// filter drops the overlap the replay already folded. This makes the resume
	// race-free — closing readyCh before computing the offset (the previous design)
	// let an append between the two jump the tail past the new event, dropping it.
	// captureTailCursor reads the size and identity from one stat so a rotation
	// cannot pair the old file's offset with the fresh file's identity.
	st := captureTailCursor(t.eventsPath)
	loadErr := proj.ColdLoad(t.eventsPath)
	st.marks = t.build(proj, nil, loadErr)
	t.logDecodeMisses(proj, st)
	close(t.readyCh)

	// Best-effort prime the per-city sessions cache now that the fold is warm, so
	// the first detail() serves a fully-warm read instead of paying the loopback
	// sessions fetch inline. It runs in its OWN goroutine, NOT on this poll loop:
	// the prime issues a /v0 sessions loopback read that can block for up to the
	// HTTP client timeout (runSessionsFetchTimeout) when the supervisor API is slow
	// or not yet serving, and the elected single-flight compute detaches from ctx
	// (see enrichment_cache.go), so a caller-side deadline cannot shorten it. Doing
	// it inline here would delay the tail's first foldNext by that long, leaving
	// events appended right after readyCh closed unfolded during the exact startup
	// window this warm-up exists to cover.
	//
	// The prime is tracked in the plane waitgroup so a graceful shutdown still
	// drains a fast, in-flight prime — but it must not PIN shutdown. Because the
	// elected compute detaches from ctx and is bounded only by its own fetch
	// timeout, waiting on that compute inline would keep Plane.Stop's wg.Wait()
	// blocked for up to runSessionsFetchTimeout on a wedged /sessions read, even
	// though the prime is optional and the cache it warms is being torn down. So
	// run the fetch in a child goroutine and stop waiting on it the moment ctx is
	// canceled: Stop returns promptly while the detached fetch drains on its own
	// bounded deadline. The prime degrades silently (the cache falls back to
	// (nil, false) when the loopback isn't serving yet — e.g. mid-start before the
	// /v0 API is up). Formulas stay lazy: they are per-run, compile fast, and are
	// cached with a long TTL once first fetched.
	wg.Add(1)
	go func() {
		defer wg.Done()
		primed := make(chan struct{})
		go func() {
			defer close(primed)
			t.mgr.fetchSessions(ctx, t.name)
		}()
		select {
		case <-primed:
		case <-ctx.Done():
		}
	}()

	poll := time.NewTicker(runTailPollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			t.foldNext(proj, st)
			t.logDecodeMisses(proj, st)
		}
	}
}

// logDecodeMisses surfaces new bead.* payload decode misses since the last poll.
// A silent projection starve — a payload-shape or correlation-spine drift that
// stops bead.* events from decoding — is the exact failure the run-view RCA
// flagged: the view goes blank while every request still returns 200. Logging
// the miss delta makes that loud. Called only from the single loop goroutine, so
// st.loggedDecodeMisses needs no lock.
func (t *cityRunTailer) logDecodeMisses(proj *runproj.Projector, st *tailState) {
	total := proj.DecodeMisses()
	if total <= st.loggedDecodeMisses {
		return
	}
	delta := total - st.loggedDecodeMisses
	st.loggedDecodeMisses = total
	log.Printf("run-tailer: city %q dropped %d bead.* event(s) on decode miss (%d total) — run view may be stale",
		t.name, delta, total)
}

// readRotationCatchUp is the rotation catch-up read, indirected through a
// package var so a test can inject a transient read error and prove foldNext
// retries the catch-up on the next poll instead of losing the just-rotated
// events. Production always uses events.ReadFilteredWithInFlight.
var readRotationCatchUp = events.ReadFilteredWithInFlight

// foldNext performs one tail poll: it folds newly appended events into the
// projector and republishes when a bead snapshot changed. It handles active-log
// rotation by file identity: when the recorder renames the active file to an
// archive and opens a fresh one, the events written to the old active file in
// the poll window before the rename live only in the archive, so a bare offset
// reset (the previous size-shrink heuristic) would drop them — and would also
// fail to fire at all if the fresh file grew back past the stale offset within
// one poll. On a detected rotation it first catches up across archives by
// sequence, then re-tails the fresh active file from the top; the seq filter
// drops the overlap the catch-up already folded.
func (t *cityRunTailer) foldNext(proj *runproj.Projector, st *tailState) {
	info, statErr := os.Stat(t.eventsPath)
	rotated := statErr == nil && st.activeInfo != nil && !os.SameFile(st.activeInfo, info)
	if rotated {
		// ReadFilteredWithInFlight walks the sibling .gz archives (skipping any
		// whose seq window is fully below the cursor without gunzipping), the
		// in-flight events.jsonl.rotating-* files a just-rotated log has not yet
		// been gzipped into, AND the fresh active file. Including the rotating
		// files closes the async-compression window: the recorder renames the old
		// active log to a plain-JSONL rotating-* file and compresses it in the
		// background, so between the rename and the .gz a plain ReadFiltered would
		// miss those pre-rotation events and the offset reset below would advance
		// the tail past them for good. eventsAfter + Apply are seq-idempotent, so
		// the .gz/rotating overlap and the offset-0 re-read are both harmless.
		//
		// Catch up BEFORE advancing the identity or resetting the offset. Those
		// pre-rotation events live only in the archive now, so a transient
		// catch-up read error must leave the OLD identity in place and retry on
		// the next poll — committing the fresh identity here would make the next
		// poll see no rotation (SameFile) and lose that window until restart.
		catchUp, err := readRotationCatchUp(t.eventsPath, events.Filter{AfterSeq: proj.LastSeq()})
		if err != nil {
			return
		}
		if fresh := eventsAfter(catchUp, proj.LastSeq()); len(fresh) > 0 && proj.Apply(fresh) {
			st.marks = t.build(proj, st.marks, nil)
		}
		st.activeInfo = info
		st.offset = 0
	} else if statErr == nil {
		st.activeInfo = info
		// A byte offset beyond the current active file's EOF on the SAME identity
		// is a stale cursor (e.g. one captured against a since-rotated larger
		// file): ReadFrom would seek past EOF and silently skip every event below
		// it. The active log only grows in place — rotation changes identity and
		// is handled above — so offset > size can only mean the offset is stale.
		// Rewind to re-read the fresh file from the top; eventsAfter drops the
		// overlap already folded.
		if st.offset > info.Size() {
			st.offset = 0
		}
	}

	evts, newOffset, err := events.ReadFrom(t.eventsPath, st.offset)
	if err != nil {
		return
	}
	st.offset = newOffset
	fresh := eventsAfter(evts, proj.LastSeq())
	if len(fresh) == 0 {
		return
	}
	sessionChanged := containsSessionEvent(fresh)
	if proj.Apply(fresh) {
		st.marks = t.build(proj, st.marks, nil)
	}
	if sessionChanged {
		// Session lifecycle events don't change the bead fold (proj.Apply ignores
		// them), so build() — and its subscriber notify — may not have fired. But
		// they DO change the live session links the detail projection layers on, so
		// eagerly refresh the sessions enrichment and wake the detail-stream
		// subscribers: an idle run's session-link flip then pushes without waiting
		// for the next bead event or the sessions TTL. Rare session events that land
		// only in the rotation catch-up path recover on the next poll / the TTL.
		t.refreshSessionEnrichment()
	}
}

// sessionEventPrefix is the common prefix of every session lifecycle event
// (session.updated / .woke / .stopped / .crashed / …).
const sessionEventPrefix = "session."

// containsSessionEvent reports whether any freshly-folded event is a session
// lifecycle event. Such events do not change the bead fold, so build() ignores
// them, but they change the live session enrichment the detail projection layers
// on — the reason foldNext refreshes sessions and wakes the detail stream.
func containsSessionEvent(fresh []events.Event) bool {
	for i := range fresh {
		if strings.HasPrefix(fresh[i].Type, sessionEventPrefix) {
			return true
		}
	}
	return false
}

// refreshSessionEnrichment eagerly expires the per-city sessions cache and wakes
// the detail-stream subscribers so a session-link change on an otherwise-idle
// run pushes a fresh frame promptly. Each subscriber rebuilds via detail(),
// which refetches the now-expired sessions (single-flight collapses concurrent
// rebuilds to one loopback read); the per-connection byte-dedupe drops the frame
// when the run's own links did not move — e.g. the event was for an unrelated
// session in the same city. It is naturally rate-limited to at most once per
// tail poll (runTailPollInterval).
//
// The invalidate can be masked by a sessions compute that elected BEFORE it: that
// in-flight compute's deferred publish resets the TTL and bumps the version with a
// value that may predate this session change, so a subscriber joining it can push
// one transiently-stale frame. This matches the cache's eventual-consistency
// contract and self-heals on the next session/bead event or the reset TTL.
func (t *cityRunTailer) refreshSessionEnrichment() {
	t.mgr.sessionsCache.invalidate(t.name)
	t.notifySubscribers()
}

// eventsAfter keeps only events past the projector's cursor, dropping the
// overlap a from-offset re-read (cold-replay resume or post-rotation rescan)
// re-surfaces. Filters in place; the input slice is loop-local.
func eventsAfter(evts []events.Event, afterSeq uint64) []events.Event {
	out := evts[:0]
	for _, e := range evts {
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	return out
}

// build projects the folded beads into a bead-derived RunSummary, advances the
// monotonic thrash marks against the prior generation, and publishes both under
// the lock. It returns the advanced marks for the loop to carry forward.
func (t *cityRunTailer) build(proj *runproj.Projector, prevMarks map[string]runproj.LaneProgressMark, loadErr error) map[string]runproj.LaneProgressMark {
	// Apply the run-bead filter at the projection boundary, mirroring the
	// frontend's runBeadFilter (summary.ts). The pure runproj builders
	// assume already-filtered input, so folding the raw event log straight in —
	// it also carries message, session, and gc:-labeled control beads that can
	// share a run root — would let unrelated beads distort lane status, counts,
	// recent changes, and detail nodes. Filtering once here feeds the same clean
	// slice to both the summary and the detail projection. FilterRunBeads returns
	// a fresh first-seen-ordered slice of the immutable-after-decode bead values,
	// so the published snapshot is safe to read concurrently.
	beadSlice := runproj.FilterRunBeads(proj.Beads())
	summary := runproj.BuildRunSummary(beadSlice)
	if loadErr != nil {
		// A read failure must surface as a partial snapshot, not a silently empty
		// "no runs" view.
		summary.LanesPartial = true
	}

	inFlight := make([]runproj.RunLane, 0, len(summary.Lanes)+len(summary.BlockedLanes))
	inFlight = append(inFlight, summary.Lanes...)
	inFlight = append(inFlight, summary.BlockedLanes...)
	marks := runproj.AdvanceProgressMarks(prevMarks, inFlight)

	// Publish the filtered warm bead slice + fold cursor alongside the summary so
	// the detail endpoint projects any one run off the same clean projection
	// (BuildRunDetail does its own member selection).
	lastSeq := proj.LastSeq()

	t.mu.Lock()
	t.summary = summary
	t.marks = marks
	t.beads = beadSlice
	t.lastSeq = lastSeq
	t.ready = true
	t.mu.Unlock()

	// This is the single change-gated publish point, so it is also the single
	// place a detail-stream broadcast fires: notify every subscriber
	// (non-blocking). A subscriber that has not yet drained its prior notify
	// already has a rebuild pending, so a full buffer is a no-op — the
	// per-connection byte-dedupe collapses the coalesced wakeups into at most one
	// frame per real change.
	t.notifySubscribers()
	return marks
}

// runDetailSnapshotVersion is the synthesized run-snapshot shape version the
// bead-derived detail projection emits (the OSS-local analog of the supervisor's
// snapshot_version). It matches the golden generator's snapshot_version.
const runDetailSnapshotVersion = 1

// detailBuildCount counts every run-detail build+marshal the tailer performs
// (i.e. a detail-memo miss). It exists so a test can prove two requests at the
// same fold generation build once. It carries no production behavior.
var detailBuildCount atomic.Int64

// snapshotFoldCount counts every run-snapshot fold the detail path performs
// (i.e. a snapshot-cache miss). It exists so a test can prove repeated detail()
// calls at the same fold generation fold the run exactly once — a same-generation
// hit must not re-scan the city's beads. It carries no production behavior.
var snapshotFoldCount atomic.Int64

// detail projects one run into the run-detail DTO off the warm bead snapshot,
// layering request-time session links from one loopback /v0 sessions read. It
// waits briefly for the cold replay on a city's first request, like
// enrichedSummary. The bool reports whether the cold replay had completed (a
// not-found run during warming is reported as warming, not a hard 404).
//
// Two caches keyed on the fold generation make a same-generation repeat cheap.
// The snapshot cache (keyed by runID+lastSeq) folds the run's beads once per
// generation and serves the fold + formula target to every later request, so a
// repeat poll re-scans nothing. The detail memo (keyed by runID+lastSeq+
// sessions-version+formula-version/failure) then caches the built+marshaled DTO,
// so an unchanged fold with unchanged enrichments returns the cached bytes with
// zero re-scan, zero re-projection, and zero re-marshal. A new bead event
// (lastSeq++) invalidates both; a bumped enrichment version invalidates only the
// detail memo (the fold is reused) → a rebuild off the cached snapshot.
func (t *cityRunTailer) detail(ctx context.Context, runID string) (runDetailMemoValue, bool, error) {
	select {
	case <-t.readyCh:
	case <-ctx.Done():
	case <-time.After(runColdLoadWait):
	}

	t.mu.RLock()
	beadSlice := t.beads
	lastSeq := t.lastSeq
	ready := t.ready
	t.mu.RUnlock()

	// Fold the run's snapshot ONCE per fold generation and cache it: the snapshot
	// serves both the formula-target extraction (which formula to fetch) and the
	// build, and a same-generation repeat request (the hot dashboard poll) reuses
	// it with no re-scan. The old path scanned the city's beads on EVERY request —
	// even a detail-memo hit re-ran SnapshotForRun before the memo lookup — so the
	// single-scan win only spanned distinct requests, never repeat polls at the
	// same generation. SnapshotForRun still returns an error only when the run root
	// is absent, which stays uncached so a run that appears in a later fold folds
	// then instead of pinning a not-found.
	snapKey := runSnapshotCacheKey{runID: runID, lastSeq: lastSeq}
	snapValue, err := t.snapshotCache.getOrBuild(snapKey, func() (runSnapshotCacheValue, error) {
		snap, buildErr := runproj.SnapshotForRun(beadSlice, runID, runDetailSnapshotVersion, int64(lastSeq))
		if buildErr != nil {
			return runSnapshotCacheValue{}, buildErr
		}
		snapshotFoldCount.Add(1)
		name, target, scopeKind, scopeRef, ok := runproj.FormulaTargetFromSnapshot(snap)
		return runSnapshotCacheValue{snap: snap, name: name, target: target, scopeKind: scopeKind, scopeRef: scopeRef, targetOK: ok}, nil
	})
	if err != nil {
		return runDetailMemoValue{}, ready, err
	}

	// The fold resolved this run's root, so the run is KNOWN to the projection:
	// drop any unknown-run grace marker so a runId that becomes known never
	// lingers in the first-seen map (rundetail_grace.go).
	t.unknownRuns.forget(runID)

	// Resolve the request-time sessions enrichment and its cache version. The
	// version (0 when unavailable) is part of the memo key so a sessions refresh —
	// or an availability flip — rebuilds.
	sessions, sessionsVersion, sessionsAvailable := t.mgr.fetchSessionsVersioned(ctx, t.name)
	if !sessionsAvailable {
		sessions = nil
	}

	// Layer the supervisor's compiled formula detail at request time (like
	// sessions) so a graph.v2 run with a name+target resolves to the authored
	// step order and an "available" formula-detail state instead of a synthetic
	// fetch failure. A run with no fetchable formula, or a genuine fetch failure,
	// leaves formulaDetail nil so the detail state stays honest (missing_* or, for
	// a name+target we could not resolve, fetch_failed). On a fetch failure we keep
	// the reason (not_found for a supervisor 404, else upstream_error) so runproj
	// renders the right operator diagnostic instead of collapsing a missing formula
	// into a generic upstream error.
	var formulaDetail *runproj.FormulaOrderingDetail
	formulaDetailFailure := runproj.FormulaDetailUpstreamError
	var formulaVersion uint64
	if snapValue.targetOK {
		if fetched, failure, version, fetchedOK := t.mgr.fetchFormulaDetailVersioned(ctx, t.name, snapValue.name, snapValue.target, snapValue.scopeKind, snapValue.scopeRef); fetchedOK {
			formulaDetail = fetched
			formulaVersion = version
		} else {
			formulaDetailFailure = failure
			formulaVersion = version
		}
	}

	// Everything that determines the output is now captured in the key. On a hit
	// the memo returns the cached DTO+bytes with zero re-projection and zero
	// re-marshal; on a miss it builds once (single-flighted across concurrent
	// callers), marshals once, stores, and returns.
	key := runDetailMemoKey{
		runID:           runID,
		lastSeq:         lastSeq,
		sessionsVersion: sessionsVersion,
		formulaVersion:  formulaVersion,
		formulaFailure:  formulaDetailFailure,
	}
	value, err := t.detailMemo.getOrBuild(key, func() (runDetailMemoValue, error) {
		d, buildErr := runproj.BuildRunDetailFromSnapshot(snapValue.snap, sessions, formulaDetail, formulaDetailFailure)
		if buildErr != nil {
			return runDetailMemoValue{}, buildErr
		}
		detailBuildCount.Add(1)
		raw, marshalErr := json.Marshal(d)
		if marshalErr != nil {
			return runDetailMemoValue{}, marshalErr
		}
		return runDetailMemoValue{detail: d, bytes: raw}, nil
	})
	return value, ready, err
}

// enrichedSummary returns the warm bead-derived summary with request-time
// session health/census layered on. It waits briefly for the cold replay on a
// city's first request, then degrades to a partial (warming) snapshot.
func (t *cityRunTailer) enrichedSummary(ctx context.Context) runproj.RunSummary {
	select {
	case <-t.readyCh:
	case <-ctx.Done():
	case <-time.After(runColdLoadWait):
	}

	t.mu.RLock()
	base := t.summary
	marks := t.marks
	ready := t.ready
	t.mu.RUnlock()

	sessions, sessionsAvailable := t.mgr.fetchSessions(ctx, t.name)
	enriched := runproj.EnrichRunSummary(base, sessions, sessionsAvailable, time.Now().UnixMilli(), marks)
	if !ready {
		enriched.LanesPartial = true
	}
	return enriched
}

// fetchSessions returns the projected dashboard sessions for a city, served from
// the per-city sessions cache (TTL + single-flight). A cache hit within the TTL
// does no upstream work; concurrent cold-miss callers collapse to one upstream
// fetch; a failed refetch serves the last-good with available=true; a cold miss
// whose fetch fails degrades to (nil, false) exactly as the uncached path did, so
// the caller's honest partial/warming states are preserved. detail() and
// enrichedSummary() consume this.
func (m *runTailerManager) fetchSessions(ctx context.Context, name string) ([]runproj.DashboardSession, bool) {
	items, _, ok := m.fetchSessionsVersioned(ctx, name)
	return items, ok
}

// fetchSessionsVersioned is fetchSessions with the served value's cache version,
// so the run-detail memo can key on it and rebuild when the sessions enrichment
// refreshes (even to an equal value). The version is meaningful only when ok is
// true. detail() consumes this; enrichedSummary() uses the version-less
// fetchSessions.
func (m *runTailerManager) fetchSessionsVersioned(ctx context.Context, name string) ([]runproj.DashboardSession, uint64, bool) {
	got, version, ok := m.sessionsCache.getWithVersion(ctx, name, func(ctx context.Context) (cachedSessions, time.Duration, bool, bool) {
		items, upstreamOK := m.fetchSessionsUpstream(ctx, name)
		if !upstreamOK {
			return cachedSessions{}, 0, false, false
		}
		// A successful sessions read is a positive last-good: serve it stale on a
		// later failed refetch rather than blanking the health card.
		// m.sessionsTTL (not the sessionsCacheTTL var): this closure can run on
		// the detached prime goroutine, so it must read the construction-time
		// capture, never the test-mutable package var.
		return cachedSessions{items: items}, m.sessionsTTL, true, true
	})
	if !ok {
		return nil, 0, false
	}
	return got.items, version, true
}

// fetchSessionsUpstream reads GET {base}/v0/city/{name}/sessions over loopback
// and projects the items into the dashboard session shape (equivalent to the
// frontend normalizeSessions). Any failure returns (nil, false) so the cache
// degrades to unavailable (or serves last-good) rather than failing the load.
func (m *runTailerManager) fetchSessionsUpstream(ctx context.Context, name string) ([]runproj.DashboardSession, bool) {
	base := strings.TrimRight(m.deps.SupervisorBaseURL, "/")
	if base == "" {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v0/city/"+name+"/sessions", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, false
	}
	var env struct {
		Items []runproj.DashboardSession `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false
	}
	if env.Items == nil {
		env.Items = []runproj.DashboardSession{}
	}
	return env.Items, true
}

// formulaNodeRef decodes the ordering-relevant id of a compiled-formula preview
// node or step from the supervisor's formula-detail response.
type formulaNodeRef struct {
	ID string `json:"id"`
}

// fetchFormulaDetailVersioned returns a run's compiled formula detail, served
// from the per-city formula cache keyed by (name, formula, target, scopeKind,
// scopeRef). A success is cached for formulaCacheTTL; a definitive 404
// (genuinely-missing formula) is cached as FormulaDetailNotFound for the shorter
// formulaNotFoundTTL so a newly-added formula appears promptly; a transient
// upstream error is NOT cached — it degrades like a cold miss so a real re-check
// happens on the next GET. On success it returns (detail, "", version, true); on
// a failure it returns (nil, reason, version, false), preserving the NotFound vs
// UpstreamError distinction runproj renders as the operator diagnostic.
//
// The version is the served cache entry's monotonic generation, so the
// run-detail memo rebuilds when the compiled formula refreshes (a re-compile, a
// not-found→available flip, or a re-check). It is meaningful whenever a cache
// entry was served — including a served negative (a cached not-found), since a
// negative→available transition bumps it. A cold-miss degrade returns version 0.
// detail() consumes this.
func (m *runTailerManager) fetchFormulaDetailVersioned(ctx context.Context, name, formula, target, scopeKind, scopeRef string) (*runproj.FormulaOrderingDetail, runproj.RunFormulaDetailFetchFailure, uint64, bool) {
	key := formulaCacheKey{name: name, formula: formula, target: target, scopeKind: scopeKind, scopeRef: scopeRef}
	got, version, ok := m.formulaCache.getWithVersion(ctx, key, func(ctx context.Context) (cachedFormulaDetail, time.Duration, bool, bool) {
		detail, failure, upstreamOK := m.fetchFormulaDetailUpstream(ctx, name, formula, target, scopeKind, scopeRef)
		switch {
		case upstreamOK:
			// A compiled formula is a positive last-good: serve it stale on a later
			// failed refetch.
			return cachedFormulaDetail{detail: detail}, formulaCacheTTL, true, true
		case failure == runproj.FormulaDetailNotFound:
			// A definitive 404 is a real negative result: cache it briefly so a burst
			// of GETs does not re-probe a known-missing formula, but re-check soon.
			// It is NOT stale-serveable, so once formulaNotFoundTTL lapses an errored
			// refetch degrades to upstream_error instead of pinning this stale
			// not-found over a live upstream failure.
			return cachedFormulaDetail{failure: runproj.FormulaDetailNotFound}, formulaNotFoundTTL, true, false
		default:
			// A transient upstream error is not cached; degrade like a cold miss.
			return cachedFormulaDetail{}, 0, false, false
		}
	})
	if !ok {
		// Cold miss whose fetch failed with a non-404 error, or a canceled join
		// with no last-good: the honest reason is upstream_error.
		return nil, runproj.FormulaDetailUpstreamError, 0, false
	}
	if got.detail == nil {
		// A cached not-found (or a cached-then-served negative): not available, and
		// the failure reason travels with the cached value.
		failure := got.failure
		if failure == "" {
			failure = runproj.FormulaDetailUpstreamError
		}
		return nil, failure, version, false
	}
	return got.detail, "", version, true
}

// fetchFormulaDetail preserves the pre-refactor 3-tuple call shape for tests
// and older internal call sites while the versioned cache API remains the
// implementation behind it.
func (m *runTailerManager) fetchFormulaDetail(ctx context.Context, name, formula, target, scopeKind, scopeRef string) (*runproj.FormulaOrderingDetail, runproj.RunFormulaDetailFetchFailure, bool) { //nolint:unparam
	detail, failure, _, ok := m.fetchFormulaDetailVersioned(ctx, name, formula, target, scopeKind, scopeRef)
	return detail, failure, ok
}

// fetchFormulaDetailUpstream reads
// GET {base}/v0/city/{name}/formulas/{formula}?target={target}&scope_kind={kind}&scope_ref={ref}
// over loopback and projects the compiled formula's ordering-relevant preview
// nodes and steps into runproj's FormulaOrderingDetail. The scope is required by
// the endpoint and selects the formula search layer, so a rig-scoped run must
// send its scope or the lookup resolves the wrong layer (or is rejected). On
// success it returns (detail, "", true). On failure it returns (nil, reason,
// false): the reason is FormulaDetailNotFound for a supervisor 404 (the compiled
// formula is genuinely missing) and FormulaDetailUpstreamError for every other
// failure. Mirrors fetchSessionsUpstream; the reason mapping ports the TS
// formulaDetailFetchFailure helper.
func (m *runTailerManager) fetchFormulaDetailUpstream(ctx context.Context, name, formula, target, scopeKind, scopeRef string) (*runproj.FormulaOrderingDetail, runproj.RunFormulaDetailFetchFailure, bool) {
	base := strings.TrimRight(m.deps.SupervisorBaseURL, "/")
	if base == "" {
		return nil, runproj.FormulaDetailUpstreamError, false
	}
	endpoint := base + "/v0/city/" + url.PathEscape(name) + "/formulas/" + url.PathEscape(formula)
	query := url.Values{"target": {target}}
	if scopeKind != "" {
		query.Set("scope_kind", scopeKind)
	}
	if scopeRef != "" {
		query.Set("scope_ref", scopeRef)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, runproj.FormulaDetailUpstreamError, false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return nil, runproj.FormulaDetailUpstreamError, false
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, runproj.FormulaDetailNotFound, false
		}
		return nil, runproj.FormulaDetailUpstreamError, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, runproj.FormulaDetailUpstreamError, false
	}
	var env struct {
		Name    string           `json:"name"`
		Steps   []formulaNodeRef `json:"steps"`
		Preview struct {
			Nodes []formulaNodeRef `json:"nodes"`
		} `json:"preview"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, runproj.FormulaDetailUpstreamError, false
	}
	return &runproj.FormulaOrderingDetail{
		Name:           env.Name,
		PreviewNodeIDs: refIDs(env.Preview.Nodes),
		StepIDs:        refIDs(env.Steps),
	}, "", true
}

// refIDs lifts formula node/step ids into a plain slice, preserving nil (the
// field was absent/null) versus non-nil empty (present-but-empty) so runproj's
// preview-nodes-then-steps ordering fallback stays faithful to the dashboard.
func refIDs(refs []formulaNodeRef) []string {
	if refs == nil {
		return nil
	}
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.ID)
	}
	return ids
}

// ── Route ─────────────────────────────────────────────────────────────────

func (p *Plane) registerRunSummary() {
	p.mux.HandleFunc("GET /api/city/{cityName}/runs/summary", func(w http.ResponseWriter, r *http.Request) {
		t, ok := p.cityRunTailer(r.PathValue("cityName"))
		if !ok {
			writeError(w, http.StatusNotFound, "unknown city")
			return
		}
		writeJSON(w, http.StatusOK, t.enrichedSummary(r.Context()))
	})
}

// runDetailErrorBody carries an UnsupportedRunError's reason to the SPA, which
// renders 'not_run_view' (an honest list-only run) differently from
// 'invalid_snapshot' (a genuine load failure). Typed like the other plane wire
// shapes (it extends the shared { error } body with the discriminating reason).
type runDetailErrorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

func (p *Plane) registerRunDetail() {
	p.mux.HandleFunc("GET /api/city/{cityName}/runs/{runId}/detail", func(w http.ResponseWriter, r *http.Request) {
		t, ok := p.cityRunTailer(r.PathValue("cityName"))
		if !ok {
			writeError(w, http.StatusNotFound, "unknown city")
			return
		}
		runID := r.PathValue("runId")
		value, ready, err := t.detail(r.Context(), runID)
		if err != nil {
			t.writeRunDetailReadError(w, runID, err, ready)
			return
		}
		// Serve the memoized marshaled bytes verbatim — the memo already produced
		// json.Marshal(detail), so writeJSONBytes skips a re-marshal while emitting
		// byte-identical output to writeJSON (same headers, same trailing newline
		// the JSON encoder appends).
		writeJSONBytes(w, http.StatusOK, value.bytes)
	})
}

// runDetailReasonUnknownRun is the runDetailErrorBody reason carried by the
// graced unknown-run 503, so clients can tell "the server is holding a grace
// window for a run it has never seen" apart from the cold-replay warming 503
// (a plain {error} body with no reason and no Retry-After).
const runDetailReasonUnknownRun = "unknown_run"

// unknownRunRetryAfter is the graced 503's Retry-After header value (seconds):
// the poll cadence the server suggests while it holds an unknown run's grace
// window open.
const unknownRunRetryAfter = "5"

// writeRunDetailReadError maps a failed detail() read to the HTTP response —
// shared by the JSON GET and the SSE stream precheck so both endpoints answer
// identically: 422 for an unsupported (v1/wisp) run, 503 while the projection
// is still warming or while a truly-unknown run is inside its warming-grace
// window (the graced variant carries Retry-After and reason unknown_run), 404
// otherwise.
func (t *cityRunTailer) writeRunDetailReadError(w http.ResponseWriter, runID string, err error, ready bool) {
	var unsupported *runproj.UnsupportedRunError
	if errors.As(err, &unsupported) {
		// A definitive answer: the run root EXISTS in the projection but has no
		// run-detail view. Checked BEFORE the grace window below — not_run_view
		// must stay a 422, never a warming 503.
		writeJSON(w, http.StatusUnprocessableEntity, runDetailErrorBody{
			Error:  unsupported.Message,
			Reason: string(unsupported.Reason),
		})
		return
	}
	// The run root is absent from the warm projection. While the cold replay
	// is still in flight the fold may be incomplete, so report warming rather
	// than a hard 404 for a run that may yet appear. Checked BEFORE the grace
	// window below: a warming-phase request must not start (or consume) an
	// unknown run's grace clock — the window is measured from the first
	// POST-warm request (TestRunDetailWarmingDoesNotStartGraceClock). This
	// plain 503 is a retry signal for the short replay, and the SPA loader
	// (supervisor/runDetail.ts loadSupervisorFormulaRunDetail) retries 5xx
	// within its own bounded backoff budget before surfacing it (covered by
	// runDetail.test.ts "retries while the projection is warming").
	if !ready {
		writeError(w, http.StatusServiceUnavailable, "run view is warming")
		return
	}
	// The projection is warm but has never seen this run. A run slung from the
	// CLI stays invisible here until the controller's cache-reconcile emits
	// its bead events (30-120s), and the SPA treats a 404 as terminal — so a
	// truly-unknown run gets a retryable warming 503 for a grace window
	// measured from its first request (rundetail_grace.go). The contract is
	// server-held: the server holds the warming answer for the whole window,
	// Retry-After tells clients how often to poll, and the SPA's run-detail
	// loader polls within its own budget (being extended in a sibling change).
	// The reason unknown_run makes this graced answer distinguishable from the
	// cold-replay warming 503 above. Once the window expires the plain 404
	// below is restored. Only the not-found case is graced: every other
	// failure keeps its existing mapping.
	if errors.Is(err, runproj.ErrRunNotFound) && t.unknownRuns.inGrace(runID) {
		w.Header().Set("Retry-After", unknownRunRetryAfter)
		writeJSON(w, http.StatusServiceUnavailable, runDetailErrorBody{
			Error:  "run view is warming",
			Reason: runDetailReasonUnknownRun,
		})
		return
	}
	writeError(w, http.StatusNotFound, "unknown run")
}

// cityRunTailer resolves the city to its run tailer, returning false for an
// unknown city (so the handler can 404). Starting the fold loop is lazy.
func (p *Plane) cityRunTailer(name string) (*cityRunTailer, bool) {
	path, ok := p.resolveCityPath(name)
	if !ok {
		return nil, false
	}
	return p.runTailers.ensure(name, cityEventsPath(path)), true
}

// cityEventsPath is the single source of truth for a city's append-only event
// log path, so the lazy per-request start and the eager Start-time warm-up
// (eagerWarmTailers) fold the exact same file.
func cityEventsPath(cityRoot string) string {
	return filepath.Join(cityRoot, ".gc", "events.jsonl")
}

// eagerWarmTailers starts the run-view fold for every currently-registered city
// so the cold replay of .gc/events.jsonl happens at startup — in each tailer's
// own background goroutine — instead of on the operator's first click. It is
// non-blocking: ensure spawns the fold goroutine and returns immediately, so
// Start never waits on any city's cold load. A nil resolver or an empty city
// set is a no-op, and cities registered after Start keep the lazy start on
// their first request.
//
// Cost scales with TOTAL registered cities, not active ones: warm-up starts one
// cold-replay goroutine per city at Start and keeps every city's folded bead
// slice resident for the plane's lifetime, so boot CPU/disk (JSON decode + .gz
// archive walks) and baseline memory grow with the registry. Because ColdLoad is
// context-blind (internal/runproj/projector.go), a Stop landing in the boot
// window also waits on the slowest in-flight replay. This is deliberate for the
// current few-city deployments; scaling to a large fleet would want a bounded
// warm-up pool and/or a ctx-aware ColdLoad so Start-time work and shutdown stay
// bounded.
func (p *Plane) eagerWarmTailers() {
	if p.deps.Resolver == nil {
		return
	}
	for _, c := range p.deps.Resolver.Cities() {
		if !ValidCityName(c.Name) || c.Path == "" {
			continue
		}
		p.runTailers.ensure(c.Name, cityEventsPath(c.Path))
	}
}
