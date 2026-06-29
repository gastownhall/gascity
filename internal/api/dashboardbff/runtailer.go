package dashboardbff

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	mu      sync.Mutex
	cities  map[string]*cityRunTailer
	ctx     context.Context
	wg      *sync.WaitGroup
	enabled bool
}

func newRunTailerManager(deps Deps) *runTailerManager {
	return &runTailerManager{
		deps:   deps,
		httpc:  &http.Client{Timeout: runSessionsFetchTimeout},
		cities: make(map[string]*cityRunTailer),
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
		t = &cityRunTailer{name: name, eventsPath: eventsPath, mgr: m, readyCh: make(chan struct{})}
		m.cities[name] = t
	}
	if m.enabled && m.ctx != nil && !t.started {
		t.started = true
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			t.loop(m.ctx)
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

	mu      sync.RWMutex
	summary runproj.RunSummary
	marks   map[string]runproj.LaneProgressMark
	beads   []beads.Bead
	lastSeq uint64
	ready   bool
}

// loop cold-replays the event log, publishes the bead-derived summary, then
// tails newly appended events and republishes on each change. All folding and
// summary-building happens on loop-owned locals; only the publish takes the lock.
func (t *cityRunTailer) loop(ctx context.Context) {
	proj := runproj.NewProjector()

	// Capture the active log size BEFORE the cold replay so the tail resumes from
	// exactly there. Any event appended during (or just after) the replay lands in
	// [offset, EOF) and is re-read by the first tail poll; the seq filter drops
	// the overlap the replay already folded. This makes the resume race-free —
	// closing readyCh before computing the offset (the previous design) let an
	// append between the two jump the tail past the new event, dropping it.
	offset := fileSize(t.eventsPath)
	loadErr := proj.ColdLoad(t.eventsPath)
	marks := t.build(proj, nil, loadErr)
	close(t.readyCh)

	poll := time.NewTicker(runTailPollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			// A log shorter than our offset means it rotated (active file renamed
			// to an archive, fresh active file created): reset to the top. The
			// pre-rotation events are already folded and now live in an archive, so
			// re-reading from 0 only yields the new active file's fresh events.
			if fileSize(t.eventsPath) < offset {
				offset = 0
			}
			evts, newOffset, err := events.ReadFrom(t.eventsPath, offset)
			if err != nil {
				continue
			}
			offset = newOffset
			fresh := eventsAfter(evts, proj.LastSeq())
			if len(fresh) == 0 {
				continue
			}
			if proj.Apply(fresh) {
				marks = t.build(proj, marks, nil)
			}
		}
	}
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
	summary := runproj.BuildRunSummary(proj.Beads())
	if loadErr != nil {
		// A read failure must surface as a partial snapshot, not a silently empty
		// "no runs" view.
		summary.LanesPartial = true
	}

	inFlight := make([]runproj.RunLane, 0, len(summary.Lanes)+len(summary.BlockedLanes))
	inFlight = append(inFlight, summary.Lanes...)
	inFlight = append(inFlight, summary.BlockedLanes...)
	marks := runproj.AdvanceProgressMarks(prevMarks, inFlight)

	// Publish the deterministic warm bead slice + fold cursor alongside the
	// summary so the detail endpoint can project any one run off the same warm
	// projection (BuildRunDetail does its own member selection). proj.Beads()
	// returns a fresh first-seen-ordered slice; the bead values are immutable
	// after decode, so the snapshot is safe to read concurrently.
	beadSlice := proj.Beads()
	lastSeq := proj.LastSeq()

	t.mu.Lock()
	t.summary = summary
	t.marks = marks
	t.beads = beadSlice
	t.lastSeq = lastSeq
	t.ready = true
	t.mu.Unlock()
	return marks
}

// runDetailSnapshotVersion is the synthesized run-snapshot shape version the
// bead-derived detail projection emits (the OSS-local analog of the supervisor's
// snapshot_version). It matches the golden generator's snapshot_version.
const runDetailSnapshotVersion = 1

// detail projects one run into the run-detail DTO off the warm bead snapshot,
// layering request-time session links from one loopback /v0 sessions read. It
// waits briefly for the cold replay on a city's first request, like
// enrichedSummary. The bool reports whether the cold replay had completed (a
// not-found run during warming is reported as warming, not a hard 404).
func (t *cityRunTailer) detail(ctx context.Context, runID string) (runproj.FormulaRunDetail, bool, error) {
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

	sessions, sessionsAvailable := t.mgr.fetchSessions(ctx, t.name)
	var (
		d   runproj.FormulaRunDetail
		err error
	)
	if sessionsAvailable {
		d, err = runproj.BuildRunDetailWithSessions(beadSlice, runID, runDetailSnapshotVersion, int64(lastSeq), sessions)
	} else {
		d, err = runproj.BuildRunDetail(beadSlice, runID, runDetailSnapshotVersion, int64(lastSeq))
	}
	return d, ready, err
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

// fetchSessions reads GET {base}/v0/city/{name}/sessions over loopback and
// projects the items into the dashboard session shape (equivalent to the
// frontend normalizeSessions). Any failure returns (nil, false) so health
// degrades to unavailable rather than failing the load.
func (m *runTailerManager) fetchSessions(ctx context.Context, name string) ([]runproj.DashboardSession, bool) {
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

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
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
		detail, ready, err := t.detail(r.Context(), r.PathValue("runId"))
		if err != nil {
			var unsupported *runproj.UnsupportedRunError
			if errors.As(err, &unsupported) {
				writeJSON(w, http.StatusUnprocessableEntity, runDetailErrorBody{
					Error:  unsupported.Message,
					Reason: string(unsupported.Reason),
				})
				return
			}
			// The run root is absent from the warm projection. While the cold replay
			// is still in flight the fold may be incomplete, so report warming (the
			// client retries) rather than a hard 404 for a run that may yet appear.
			if !ready {
				writeError(w, http.StatusServiceUnavailable, "run view is warming")
				return
			}
			writeError(w, http.StatusNotFound, "unknown run")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	})
}

// cityRunTailer resolves the city to its run tailer, returning false for an
// unknown city (so the handler can 404). Starting the fold loop is lazy.
func (p *Plane) cityRunTailer(name string) (*cityRunTailer, bool) {
	path, ok := p.resolveCityPath(name)
	if !ok {
		return nil, false
	}
	eventsPath := filepath.Join(path, ".gc", "events.jsonl")
	return p.runTailers.ensure(name, eventsPath), true
}
