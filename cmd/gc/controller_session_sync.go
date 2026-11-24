package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/reconcilekey"
)

// The session-sync socket verb (ga-45119r, #5843). `gc session reset` writes
// its markers through the CLI's one-shot store, straight to the backing,
// around the controller's session cache — so the pass a keyed poke wakes
// decides on a stale cached row for minutes, and a late marker kills the
// freshly-started replacement. session-sync closes that window: it installs
// the row the CLI wrote into the controller's own session cache, enqueues
// that session's wake, and only then replies, so an ok answer is a barrier
// the caller treats as "the next pass sees my write". A poke alone only
// enqueues the wake; it stays non-blocking.

// sessionSyncCommandPrefix introduces the session-sync verb on the
// controller socket.
const sessionSyncCommandPrefix = "session-sync:"

// sessionSyncRequest is the session-sync verb's payload: the session row the
// caller just wrote and wants the controller's cache to see.
type sessionSyncRequest struct {
	SessionID string `json:"session_id"`
}

// sessionSyncReply is the session-sync verb's answer. Outcome is one of:
//
//   - ok: the row is installed in the controller's session cache (or that
//     store is not cached, so its next read is live); the pass the enqueued
//     wake runs sees the caller's write. Generation is the installed row's.
//   - missing: the backing store holds no row for the id.
//   - busy: the refresh could not install the row in time (a local write
//     owned it, or the cache is degraded); a reconcile was requested and the
//     wake is enqueued, so the pass applies the write once the row settles.
//   - failed: the refresh or the store resolution failed; the reason is in
//     Error, and the wake is enqueued all the same.
type sessionSyncReply struct {
	Outcome    string `json:"outcome"`
	Generation string `json:"generation,omitempty"`
	Error      string `json:"error,omitempty"`
}

// session-sync reply outcomes.
const (
	sessionSyncOK      = "ok"
	sessionSyncMissing = "missing"
	sessionSyncBusy    = "busy"
	sessionSyncFailed  = "failed"
)

// The refresh's retry budget: a refresh that keeps losing its fence (a local
// write that may touch the row is in flight, or a reconcile merged mid-read)
// backs off 20ms, doubling per attempt up to 1s, and gives up after 3s —
// inside the client's 5s read leg, leaving room for the reply's write.
const (
	sessionSyncRetryBudget  = 3 * time.Second
	sessionSyncBackoffStart = 20 * time.Millisecond
	sessionSyncBackoffCap   = 1 * time.Second
)

// errSessionSyncBusy reports a refresh that could not install the row within
// its budget: a local write owned the row, or the cache was unavailable. The
// refresh has already requested the cache's own reconcile; the caller still
// enqueues the session's wake, so the row settles without the caller's
// retry.
var errSessionSyncBusy = errors.New("the session row is owned by an in-flight write or the cache is unavailable; a reconcile was requested")

// sessionSyncRefresh is what one session-sync refresh did.
type sessionSyncRefresh struct {
	// Found reports whether the question is settled: the row is installed
	// (or the store is uncached, so every read is live and nothing needed
	// installing). False only with an error, whose class the caller reads.
	Found bool
	// Generation is the installed row's generation, trimmed, "" when the
	// store is uncached.
	Generation string
}

// handleSessionSyncSocketCmd serves the session-sync verb: it refreshes one
// session row from the backing store into the session cache the controller's
// census serves from, enqueues that session's wake, and only then replies —
// so an ok or missing answer is a barrier the caller can treat as "the next
// pass sees my write" (or "the row is gone").
//
// It runs inside the controller, so the store is the one this process's
// runtime registered (registeredResidencyEntry), never the one-shot funnel.
func handleSessionSyncSocketCmd(conn net.Conn, cityPath, payload string, wake *controllerWake) {
	var req sessionSyncRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		writeJSONLine(conn, sessionSyncReply{
			Outcome: sessionSyncFailed,
			Error:   fmt.Sprintf("invalid session-sync request: %v", err),
		})
		return
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		writeJSONLine(conn, sessionSyncReply{
			Outcome: sessionSyncFailed,
			Error:   "session_id is required",
		})
		return
	}
	refresh, err := refreshSessionRowIntoControllerCache(cityPath, sessionID)
	// The session's wake lands after the refresh and before the reply, in
	// every case: a busy or failed refresh still wants the pass, and the
	// reconcile the refresh requested converges with one.
	wake.Enqueue(wakeReasonSocket, reconcilekey.Session(sessionID))
	switch {
	case err == nil:
		writeJSONLine(conn, sessionSyncReply{Outcome: sessionSyncOK, Generation: refresh.Generation})
	case errors.Is(err, beads.ErrNotFound):
		writeJSONLine(conn, sessionSyncReply{Outcome: sessionSyncMissing})
	case errors.Is(err, errSessionSyncBusy):
		writeJSONLine(conn, sessionSyncReply{Outcome: sessionSyncBusy, Error: err.Error()})
	default:
		writeJSONLine(conn, sessionSyncReply{Outcome: sessionSyncFailed, Error: err.Error()})
	}
}

// refreshSessionRowIntoControllerCache installs sessionID's backing row into
// the session-class cache the controller's census serves from, retrying a
// refresh that loses its fence within sessionSyncRetryBudget. It reports the
// installed row's generation. beads.ErrNotFound means the backing holds no
// row; errSessionSyncBusy means the row could not be installed in time and
// the cache's reconcile was requested; any other error is the refresh's or
// the resolution's own, with the cache's reconcile requested too.
//
// The session-class store is resolved by the router's own rule over the same
// topology scopeStores mints the session handle from (placeClass). The cache
// the census serves from lives under that store, below the strict-seam
// projection and any policy layer, so laneSessionsCache — the conversation
// lane host's own baselined peel — reaches it; resolving the store directly
// keeps the handle out of the picture (storeclass: the raw-store debt of a
// peel belongs to laneSessionsCache's baseline, not to a new unwrap).
func refreshSessionRowIntoControllerCache(cityPath, sessionID string) (sessionSyncRefresh, error) {
	entry, ok := registeredResidencyEntry(cityPath)
	if !ok {
		return sessionSyncRefresh{}, fmt.Errorf("no runtime in this controller is serving city %s right now (it is starting or stopping); retry once it is running", cityPath)
	}
	var work beads.Store
	if entry.work != nil {
		work = entry.work()
	}
	sessionsStore := placeClass(cityPath, entry.routes, work, coordclass.ClassSessions)
	if beads.IsNilStore(sessionsStore) {
		return sessionSyncRefresh{}, fmt.Errorf("the runtime serving city %s has no session store: its city work store did not open (see the controller log); restart the controller once the store is reachable", cityPath)
	}
	cache := laneSessionsCache(sessionsStore)
	if cache == nil {
		// An uncached session store reads live: the caller's write is
		// already visible, and the barrier is the wake alone.
		return sessionSyncRefresh{Found: true}, nil
	}
	deadline := time.Now().Add(sessionSyncRetryBudget)
	backoff := sessionSyncBackoffStart
	for {
		rep, err := cache.RefreshRowReport(sessionID, beads.RowRefreshOptions{
			AnnounceUnheldClose: true,
			YieldToLocalWrites:  true,
		})
		switch {
		case err == nil:
			return sessionSyncRefresh{
				Found:      true,
				Generation: strings.TrimSpace(rep.Bead.Metadata["generation"]),
			}, nil
		case errors.Is(err, beads.ErrNotFound):
			return sessionSyncRefresh{}, err
		case errors.Is(err, beads.ErrRowRefreshYielded), errors.Is(err, beads.ErrRowRefreshFenced):
			// A local write owns the row, or a merge raced the read: retry
			// within the budget, and past it hand the row to the cache's
			// own reconcile rather than answer ok for a row that may
			// still move.
			if time.Now().Add(backoff).After(deadline) {
				cache.RequestReconcile()
				return sessionSyncRefresh{}, fmt.Errorf("%w: %w", errSessionSyncBusy, err)
			}
			time.Sleep(backoff)
			backoff *= 2
			if backoff > sessionSyncBackoffCap {
				backoff = sessionSyncBackoffCap
			}
		case errors.Is(err, beads.ErrCacheUnavailable):
			cache.RequestReconcile()
			return sessionSyncRefresh{}, fmt.Errorf("%w: %w", errSessionSyncBusy, err)
		default:
			cache.RequestReconcile()
			return sessionSyncRefresh{}, err
		}
	}
}
