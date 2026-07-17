package api

import (
	"context"
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

// The run-list/get/steps handlers project the city's append-only event log
// (.gc/events.jsonl) into typed runs. The naive path re-read and re-folded the
// ENTIRE history on every request whose event log had a newer mtime, so a busy
// city paid a full O(history) replay per poll. runProjector replaces that with a
// server-owned per-city warm projection: one asynchronous cold replay off the
// request path, then an incremental byte-offset tail of only newly appended
// events. The Server is cached one-per-city (supervisor.getCityServer), so the
// projection warms once and stays warm for the city's lifetime.
//
// Modeled on the dashboard BFF's cityRunTailer, but deliberately request-driven
// rather than timer-driven: the per-city Server has no shutdown context, so a
// permanently-running poll goroutine would leak. Instead the tail runs on the
// read path under the mutex — it reads only the bytes appended since the last
// cursor (events.ReadFrom), which is strictly cheaper than the full re-fold it
// replaces. The only goroutines are the bounded cold-load replays, which read a
// finite log and exit.

// runColdLoadWait bounds how long a first (cold) request blocks for the
// asynchronous cold replay before returning a truthful warming snapshot. A tiny
// log's replay completes in well under this, so the common first request still
// serves the full projection; only a large/slow replay degrades to warming. A
// var (not const) so tests can shorten it.
var runColdLoadWait = 5 * time.Second

// runSnapshot is one read of the warm projection: the filtered run-participating
// beads, the cumulative bead.* decode-miss count (a silent projection starve the
// caller surfaces as partial), and the warm/refresh state so the caller reports
// warming honestly.
type runSnapshot struct {
	beads        []beads.Bead
	decodeMisses int
	// ready is false only while the FIRST cold replay is still in flight (no good
	// snapshot exists yet). Once a cold load completes it stays true.
	ready bool
	// refreshing is true while a cold replay (first load or a post-rotation reset)
	// is in flight. When ready && refreshing, beads is the last-good snapshot and
	// may be momentarily stale.
	refreshing bool
}

// runProjector owns one city's warm run projection: the folded Projector, the
// tail cursor (byte offset + active-file identity), and the last-good published
// bead slice. All projector mutation is serialized by mu; the cold replay itself
// runs off the lock and only the brief publish takes it.
type runProjector struct {
	eventsPath string

	// coldLoadRead and tailRead are the log readers, indirected so tests can
	// inject a slow/blocking replay (to exercise the warming path) or a failing
	// read (to exercise the error path) without a real corrupt log. Production
	// always uses the real readers. coldLoadRead spans rotated .gz archives and
	// in-flight rotating-* files (see events.ReadFilteredWithInFlight); tailRead
	// is the byte-offset incremental reader (events.ReadFrom).
	coldLoadRead func(string, events.Filter) ([]events.Event, error)
	tailRead     func(string, int64) ([]events.Event, int64, error)
	coldLoadWait time.Duration

	// readyCh is closed once the FIRST cold replay attempt completes (success or
	// failure), unblocking the bounded first-request wait. Post-warm reloads use
	// the refreshing flag, not this channel.
	readyCh chan struct{}

	// coldLoadCount counts cold replays performed. It lets a test prove that many
	// concurrent first callers share ONE replay, and that a warm incremental tail
	// does not re-replay. It carries no production behavior.
	coldLoadCount atomic.Int64

	mu           sync.Mutex
	proj         *runproj.Projector
	offset       int64
	active       os.FileInfo // active log identity, for rotation detection
	beads        []beads.Bead
	decodeMisses int
	refreshing   bool  // a cold replay is currently in flight
	ready        bool  // a cold replay has completed at least once
	loadErr      error // last cold-load failure while no good snapshot exists (503)
}

// newRunProjector returns a cold projector bound to a city's event log. The
// first snapshot() kicks off the asynchronous cold replay.
func newRunProjector(eventsPath string) *runProjector {
	return &runProjector{
		eventsPath:   eventsPath,
		coldLoadRead: events.ReadFilteredWithInFlight,
		tailRead:     events.ReadFrom,
		coldLoadWait: runColdLoadWait,
		readyCh:      make(chan struct{}),
	}
}

// runProjection returns the warm run projection for this city, lazily creating
// and warming it on first use. A city with no resolvable path yields an empty,
// non-warming snapshot (a fresh city has no runs).
func (s *Server) runProjection(ctx context.Context) (runSnapshot, error) {
	cityRoot := strings.TrimSpace(s.state.CityPath())
	if cityRoot == "" {
		return runSnapshot{ready: true}, nil
	}
	eventsPath := filepath.Join(cityRoot, ".gc", "events.jsonl")

	s.runProjMu.Lock()
	if s.runProj == nil {
		s.runProj = newRunProjector(eventsPath)
	}
	rp := s.runProj
	s.runProjMu.Unlock()

	return rp.snapshot(ctx)
}

// snapshot returns the current projection, warming it if needed. On the first
// call it kicks off the asynchronous cold replay and blocks up to coldLoadWait
// (or ctx cancellation) for it — long enough that a small log serves a full
// projection, bounded so a large log degrades to a warming partial instead of
// blocking the request on a full replay. Once warm it applies only appended
// events (or triggers a reset on rotation) and returns immediately. A first-load
// failure with no snapshot yet returns a non-nil error (mapped to 503); a
// post-warm read failure keeps the last-good snapshot and never errors.
func (rp *runProjector) snapshot(ctx context.Context) (runSnapshot, error) {
	rp.mu.Lock()
	rp.ensureLoadingLocked()
	ready := rp.ready
	wait := rp.coldLoadWait
	rp.mu.Unlock()

	if !ready {
		select {
		case <-rp.readyCh:
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}

	rp.mu.Lock()
	defer rp.mu.Unlock()

	if !rp.ready {
		// No good snapshot yet: a cold-load failure surfaces as an error (the
		// caller maps it to 503, preserving the pre-warm contract); otherwise the
		// replay is simply still in flight, reported as a truthful warming partial.
		if rp.loadErr != nil {
			return runSnapshot{}, rp.loadErr
		}
		return runSnapshot{refreshing: true}, nil
	}

	rp.tailLocked()
	return runSnapshot{
		beads:        rp.beads,
		decodeMisses: rp.decodeMisses,
		ready:        true,
		refreshing:   rp.refreshing,
	}, nil
}

// ensureLoadingLocked kicks off a cold replay when there is no good snapshot yet
// and none is in flight. It covers both the first warm-up AND a retry after a
// failed cold load — so a transient cold-load failure recovers on a later request
// instead of pinning the endpoint on a stale 503 (the pre-warm path re-read on
// every request; a one-shot guard would regress that). Concurrent callers share
// the single in-flight replay. Caller holds mu.
func (rp *runProjector) ensureLoadingLocked() {
	if rp.ready || rp.refreshing {
		return
	}
	rp.spawnColdLoadLocked()
}

// spawnColdLoadLocked marks a replay in flight and starts it. The caller has
// already decided a replay is warranted and none is running, so a read storm
// (first warm-up or a rotation reset) triggers at most one concurrent replay.
// Caller holds mu.
func (rp *runProjector) spawnColdLoadLocked() {
	rp.refreshing = true
	go rp.coldLoad()
}

// coldLoad replays the full log into a fresh projector off the lock, then
// publishes it under the lock. It captures the tail cursor from a single stat
// BEFORE the replay so an event appended during the replay is re-read (and
// seq-deduped) by the first tail rather than skipped. A failed first replay
// records loadErr for the 503 path; a failed reload keeps the last-good snapshot
// and lets a later read re-trigger on the still-present rotation.
func (rp *runProjector) coldLoad() {
	rp.coldLoadCount.Add(1)
	cursor := captureRunCursor(rp.eventsPath)
	evts, err := rp.coldLoadRead(rp.eventsPath, events.Filter{})

	// Fold the full history into the fresh projector OFF the lock: proj and evts
	// are goroutine-local until published, so the O(history) replay does not block
	// concurrent readers. Folding under mu would stall every reader at snapshot's
	// mu.Lock — past the bounded warming wait — re-introducing on a rotation reset
	// the exact read-path history stall this projector exists to remove.
	var proj *runproj.Projector
	if err == nil {
		proj = runproj.NewProjector()
		proj.Apply(evts)
	}

	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.refreshing = false
	if err != nil {
		if !rp.ready {
			rp.loadErr = err
		}
		rp.signalReadyLocked()
		return
	}
	rp.proj = proj
	rp.offset = cursor.offset
	rp.active = cursor.active
	rp.ready = true
	rp.loadErr = nil
	rp.publishLocked()
	rp.signalReadyLocked()
}

// tailLocked folds newly appended events into the warm projector, or triggers a
// fresh asynchronous cold replay when the active log rotated or was truncated.
// Caller holds mu.
func (rp *runProjector) tailLocked() {
	if rp.refreshing {
		return // a replay is in flight; serve last-good until it publishes
	}
	info, err := os.Stat(rp.eventsPath)
	if err != nil {
		return // active file briefly absent/unreadable (mid-rotation); retry next read
	}
	if rp.active != nil && !os.SameFile(rp.active, info) {
		// Rotation: the recorder renamed the active log and opened a fresh one.
		// The old offset indexes the old inode, so tailing the fresh file from it
		// would seek past its EOF (dropping events) or read mid-line (mixing
		// streams). Reset via a fresh replay, which reads the rotated archive AND
		// the fresh active file; serve last-good (partial) until it publishes.
		rp.spawnColdLoadLocked()
		return
	}
	if rp.offset > info.Size() {
		// Truncation/shrink on the same identity: the cursor is stale. Rebuild
		// rather than rewind-and-tail so old and new content never mix.
		rp.spawnColdLoadLocked()
		return
	}
	rp.active = info
	evts, newOffset, err := rp.tailRead(rp.eventsPath, rp.offset)
	if err != nil {
		return // transient read error; last-good intact, retry next read
	}
	rp.offset = newOffset
	fresh := eventsAfterSeq(evts, rp.proj.LastSeq())
	if len(fresh) == 0 {
		return
	}
	// Republish when the fold changed OR a bead.* event failed to decode: Apply
	// reports changed=false for a decode-miss-only batch (the fold is unchanged),
	// but the miss must still surface as `partial`. Publishing only on `changed`
	// would strand a decode miss that arrives via the tail — exactly the silent
	// projection-starve signal this endpoint is meant to keep observable — because
	// the offset already advanced past it so a later poll never re-reads it.
	changed := rp.proj.Apply(fresh)
	if changed || rp.proj.DecodeMisses() != rp.decodeMisses {
		rp.publishLocked()
	}
}

// publishLocked recomputes the filtered run-bead slice and decode-miss count from
// the current projector. FilterRunBeads returns a fresh first-seen-ordered slice
// of the immutable-after-decode bead values, so the published snapshot is safe to
// read concurrently without copying. Caller holds mu.
func (rp *runProjector) publishLocked() {
	rp.beads = runproj.FilterRunBeads(rp.proj.Beads())
	rp.decodeMisses = rp.proj.DecodeMisses()
}

// signalReadyLocked closes readyCh exactly once, unblocking first-request
// waiters. Caller holds mu.
func (rp *runProjector) signalReadyLocked() {
	select {
	case <-rp.readyCh:
	default:
		close(rp.readyCh)
	}
}

// runCursor is the tail resume point captured from a single stat: the active
// log's size (the byte offset the tail resumes from) and its identity (for
// rotation detection). Reading both from ONE stat keeps them consistent — split
// across two stats, a rotation between them could pair the old file's larger
// offset with the fresh file's identity and silently drop events.
type runCursor struct {
	offset int64
	active os.FileInfo
}

// captureRunCursor snapshots the active log's size and identity. A missing file
// yields a zero cursor (offset 0, nil identity); the first tail then adopts the
// file once it appears.
func captureRunCursor(path string) runCursor {
	var c runCursor
	if info, err := os.Stat(path); err == nil {
		c.offset = info.Size()
		c.active = info
	}
	return c
}

// eventsAfterSeq keeps only events past the projector's cursor, dropping the
// overlap a from-offset re-read (cold-replay resume or post-rotation rescan)
// re-surfaces. Filters in place; the input slice is caller-local.
func eventsAfterSeq(evts []events.Event, afterSeq uint64) []events.Event {
	out := evts[:0]
	for _, e := range evts {
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	return out
}
