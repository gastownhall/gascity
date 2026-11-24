package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/reconcilekey"
)

// sessionSyncCommandPrefix is the controller socket verb that makes one
// session row written outside this process visible to the controller before
// the caller reports success: "session-sync:<json sessionSyncRequest>".
const sessionSyncCommandPrefix = "session-sync:"

// sessionSyncRequest names the session row a session-sync refreshes.
type sessionSyncRequest struct {
	SessionID string `json:"session_id"`
}

// sessionSyncReply is the controller's answer to a session-sync.
type sessionSyncReply struct {
	// Outcome is one of the sessionSync* outcomes.
	Outcome string `json:"outcome"`
	// Generation is the installed row's generation on an ok reply.
	Generation string `json:"generation,omitempty"`
	// Error says why a busy or failed reply did not confirm the row.
	Error string `json:"error,omitempty"`
}

// The session-sync outcomes.
const (
	// sessionSyncOK: the controller's session cache now holds the row as the
	// backing has it, or the controller does not cache sessions.
	sessionSyncOK = "ok"
	// sessionSyncMissing: the backing has no such row.
	sessionSyncMissing = "missing"
	// sessionSyncBusy: the refresh could not land in time; a re-scan was
	// requested, and the caller may retry.
	sessionSyncBusy = "busy"
	// sessionSyncFailed: the refresh failed; a re-scan was requested.
	sessionSyncFailed = "failed"
)

// Retry bounds for a session-sync refresh that yields to a local write of the
// cache or is fenced by a newer one. Variables so a test can shorten them.
var (
	sessionSyncRefreshBudget     = 3 * time.Second
	sessionSyncRefreshBackoff    = 20 * time.Millisecond
	sessionSyncRefreshMaxBackoff = time.Second
)

// registeredSessionsCache returns the controller's cache over the
// session-class store of the runtime this process registered for cityPath
// (registerResidencyRoutes), and false when that store is not cached. The
// class binding's cache is the one returned when the routes relocate
// sessions, so a refresh never touches an unrelated city or rig cache.
func registeredSessionsCache(cityPath string) (beads.RowRefresher, bool, error) {
	entry, ok := registeredResidencyEntry(cityPath)
	if !ok {
		return nil, false, fmt.Errorf("no runtime in this controller is serving city %s right now (it is starting or stopping); retry once it is running", cityPath)
	}
	var work beads.Store
	if entry.work != nil {
		work = entry.work()
	}
	sessions := scopeStores(cityPath, entry.routes, work).Sessions()
	if sessions == (beads.SessionStore{}) {
		return nil, false, fmt.Errorf("the runtime serving city %s has no session store: its city work store did not open (see the controller log)", cityPath)
	}
	cache, cached := beads.RowRefresherFor(sessions)
	return cache, cached, nil
}

// handleSessionSyncSocketCmd answers one session-sync: it refreshes the
// session row into this controller's session cache, enqueues that session's
// wake whatever the refresh did, and then replies.
func handleSessionSyncSocketCmd(conn net.Conn, cityPath, payload string, wake *controllerWake) {
	var req sessionSyncRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		writeJSONLine(conn, sessionSyncReply{Outcome: sessionSyncFailed, Error: fmt.Sprintf("invalid session-sync request: %v", err)})
		return
	}
	id := strings.TrimSpace(req.SessionID)
	if id == "" {
		writeJSONLine(conn, sessionSyncReply{Outcome: sessionSyncFailed, Error: "session_id is required"})
		return
	}
	var reply sessionSyncReply
	cache, cached, err := registeredSessionsCache(cityPath)
	switch {
	case err != nil:
		reply = sessionSyncReply{Outcome: sessionSyncBusy, Error: err.Error()}
	case !cached:
		// Reads already go to the store: nothing stands between the write
		// and the pass the wake triggers.
		reply = sessionSyncReply{Outcome: sessionSyncOK}
	default:
		reply = syncSessionRow(cache, id, time.Sleep)
	}
	wake.Enqueue(wakeReasonSocket, reconcilekey.Session(id))
	writeJSONLine(conn, reply)
}

// syncSessionRow refreshes row id through cache, retrying a refresh that
// yielded to or was fenced by a local write (doubling backoff from
// sessionSyncRefreshBackoff, capped at sessionSyncRefreshMaxBackoff) within
// sessionSyncRefreshBudget. Every reply but ok and missing requests a
// re-scan, so the row still reaches the cache on the controller's own
// schedule.
func syncSessionRow(cache beads.RowRefresher, id string, sleep func(time.Duration)) sessionSyncReply {
	deadline := time.Now().Add(sessionSyncRefreshBudget)
	backoff := sessionSyncRefreshBackoff
	for {
		rep, err := cache.RefreshRowReport(id, beads.RowRefreshOptions{AnnounceUnheldClose: true, YieldToLocalWrites: true})
		switch {
		case err == nil:
			return sessionSyncReply{Outcome: sessionSyncOK, Generation: rep.Bead.Metadata["generation"]}
		case errors.Is(err, beads.ErrNotFound):
			return sessionSyncReply{Outcome: sessionSyncMissing, Error: err.Error()}
		case errors.Is(err, beads.ErrRowRefreshFenced):
			if remaining := time.Until(deadline); remaining > 0 {
				sleep(min(backoff, remaining))
				backoff = min(2*backoff, sessionSyncRefreshMaxBackoff)
				continue
			}
			cache.RequestReconcile()
			return sessionSyncReply{Outcome: sessionSyncBusy, Error: fmt.Sprintf("refreshing session %s: %v (still contended after %s)", id, err, sessionSyncRefreshBudget)}
		case errors.Is(err, beads.ErrCacheUnavailable):
			cache.RequestReconcile()
			return sessionSyncReply{Outcome: sessionSyncBusy, Error: fmt.Sprintf("refreshing session %s: %v", id, err)}
		default:
			cache.RequestReconcile()
			return sessionSyncReply{Outcome: sessionSyncFailed, Error: fmt.Sprintf("refreshing session %s: %v", id, err)}
		}
	}
}
