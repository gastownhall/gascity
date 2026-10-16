package t3bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.TurnObservationProvider = (*Provider)(nil)

// v1TurnCapabilities is what T3 protocol V1 tells about every session's
// turns (X11 §5.1): starts are recorded by T3 and polled, and ends are rated
// recorded too, with the residual below; the fence is a fresh read before an
// untargeted interrupt; a delivery is linked to its turn only by correlating
// requestedAt with the message's createdAt; and latestTurn holds one turn,
// so a short turn between polls can be missed.
//
// Residual the static row does not express: on the bridge's T3 fork, the
// snapshot does not keep every end. T3 clears latestTurn at most turn ends
// and, short of a thread revert, restores it only when a turn's diff
// completes, which happens only in a git workspace; that restore records an
// interrupted turn, or a failed one whose diff was already captured, as
// completed. A reader must therefore take a turn that was Current and is
// then neither Current nor in Recent as ended, without knowing how, and must
// not read a Recent status of completed as success.
var v1TurnCapabilities = runtime.TurnCapabilities{
	Start:    runtime.TurnStartRecorded,
	End:      runtime.TurnEndRecorded,
	Fence:    runtime.TurnFenceChecked,
	Link:     runtime.TurnLinkCorrelated,
	Lossless: false,
}

// TurnCapabilities implements [runtime.TurnObservationProvider]. Every V1
// session has the same capabilities.
func (p *Provider) TurnCapabilities(string) runtime.TurnCapabilities {
	return v1TurnCapabilities
}

// TurnHistory implements [runtime.TurnObservationProvider] from one uncached
// snapshot. Current is the turn ObserveTurn reports, started at
// latestTurn.requestedAt when latestTurn is that turn, whatever state
// latestTurn reads; unlike ObserveTurn, the read fails when that turn's id
// fails [runtime.IsTurnIdentity]. Recent holds latestTurn once it ended and
// is not the active turn, the only turn V1 keeps. A running latestTurn that
// is not the active turn, including one named by the stale activeTurnId an
// absent-status session keeps, is indeterminate until T3's next session-set.
// When that stale id names an ended latestTurn, Recent holds it with
// latestTurn's state, which may be the completed or interrupted mark T3
// wrote while the turn still ran rather than how it ended. No delivery keys
// are reported: the fork never fills a user message's turnId (DES-T3I F13).
// A snapshot that cannot be read as a turn state fails with
// [runtime.ErrRuntimeUnavailable], never idle.
func (p *Provider) TurnHistory(ctx context.Context, name string) (runtime.TurnHistory, error) {
	thread, err := p.freshSessionThread(ctx, name)
	if err != nil {
		return runtime.TurnHistory{}, err
	}
	observedAt := time.Now()
	threadID, rawTurnID, current, err := threadTurn(thread)
	if err != nil {
		return runtime.TurnHistory{}, err
	}
	if current.Active() && !runtime.IsTurnIdentity(current.TurnID) {
		return runtime.TurnHistory{}, fmt.Errorf("%w: t3 thread %s has an active turn id that is no turn identity", runtime.ErrRuntimeUnavailable, threadID)
	}
	h := runtime.TurnHistory{Current: current, ObservedAt: observedAt}
	latest, ok, err := threadLatestTurn(thread)
	if err != nil {
		return runtime.TurnHistory{}, err
	}
	if !ok {
		return h, nil
	}
	if rawTurnID != "" && latest.rawID == rawTurnID {
		// The session is authoritative over latestTurn.state: T3 marks the
		// active turn completed at its first finalized assistant message and
		// interrupted when a turn-scoped interrupt is requested, while the
		// session still runs it.
		h.CurrentStart = latest.requestedAt
		return h, nil
	}
	if latest.status == "" {
		return runtime.TurnHistory{}, fmt.Errorf("%w: t3 thread %s reports latest turn %q running, but turn %q is active", runtime.ErrRuntimeUnavailable, threadID, latest.rawID, rawTurnID)
	}
	id := threadID + "/" + latest.rawID
	if !runtime.IsTurnIdentity(id) {
		return runtime.TurnHistory{}, fmt.Errorf("%w: t3 thread %s has a latest turn id that is no turn identity", runtime.ErrRuntimeUnavailable, threadID)
	}
	h.Recent = []runtime.EndedTurn{{
		TurnID:    id,
		StartedAt: latest.requestedAt,
		EndedAt:   latest.completedAt,
		Status:    latest.status,
	}}
	return h, nil
}

// t3LatestTurn is a parsed V1 latestTurn.
type t3LatestTurn struct {
	rawID string
	// status is how the turn ended; empty while it runs.
	status      runtime.TurnStatus
	requestedAt time.Time
	// completedAt is zero when T3 reports none.
	completedAt time.Time
}

// t3TurnEndStatus maps V1's ended latestTurn states to how the turn ended;
// "running" maps to the empty status.
var t3TurnEndStatus = map[string]runtime.TurnStatus{
	"running":     "",
	"interrupted": runtime.TurnStatusCancelled,
	"completed":   runtime.TurnStatusCompleted,
	"error":       runtime.TurnStatusFailed,
}

// threadLatestTurn parses thread's latestTurn; ok is false when the thread
// has none. A latestTurn that is not an object, or has a missing or
// malformed turnId, state, requestedAt or completedAt, fails with
// [runtime.ErrRuntimeUnavailable].
func threadLatestTurn(thread map[string]interface{}) (turn t3LatestTurn, ok bool, err error) {
	threadID, _ := thread["id"].(string)
	raw, present := thread["latestTurn"]
	if !present || raw == nil {
		return t3LatestTurn{}, false, nil
	}
	malformed := func(field string, v interface{}) error {
		return fmt.Errorf("%w: t3 thread %s has a malformed latestTurn %s %v", runtime.ErrRuntimeUnavailable, threadID, field, v)
	}
	latest, isObject := raw.(map[string]interface{})
	if !isObject {
		return t3LatestTurn{}, false, malformed("value", raw)
	}
	id, _ := latest["turnId"].(string)
	if id == "" {
		return t3LatestTurn{}, false, malformed("turnId", latest["turnId"])
	}
	state, _ := latest["state"].(string)
	status, known := t3TurnEndStatus[state]
	if !known {
		return t3LatestTurn{}, false, malformed("state", latest["state"])
	}
	requested, _ := latest["requestedAt"].(string)
	requestedAt, err := time.Parse(time.RFC3339Nano, requested)
	if err != nil {
		return t3LatestTurn{}, false, malformed("requestedAt", latest["requestedAt"])
	}
	var completedAt time.Time
	switch v := latest["completedAt"].(type) {
	case nil:
	case string:
		if completedAt, err = time.Parse(time.RFC3339Nano, v); err != nil {
			return t3LatestTurn{}, false, malformed("completedAt", v)
		}
	default:
		return t3LatestTurn{}, false, malformed("completedAt", v)
	}
	return t3LatestTurn{rawID: id, status: status, requestedAt: requestedAt, completedAt: completedAt}, true, nil
}
