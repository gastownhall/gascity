package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/session"
)

// conversationLaneView is the conversation lane's view of a session row
// (DES-O1 §7.2), read from the row alone:
//   - Closed for a closed or archived row;
//   - Generation, Epoch and Token as session.InputFenceFor reads them, Token
//     "" when the row carries no instance token;
//   - Dormant for an asleep or drained row;
//   - ContentHold for a row whose content_hold cell is set, readable or
//     not (session.ContentHeldInfo; OJ-19).
func conversationLaneView(info session.Info) opjournal.ConversationView {
	state := session.State(strings.TrimSpace(info.MetadataState))
	// InputFenceFor reads the generation and epoch only beside a token. The
	// view needs them for a token-less row too, so it reads a copy that
	// names a token and keeps the row's own (possibly empty) token.
	counted := info
	counted.InstanceToken = "-"
	fence, _ := session.InputFenceFor(counted)
	return opjournal.ConversationView{
		ID:          info.ID,
		Closed:      info.Closed || state == session.StateArchived,
		Generation:  fence.Generation,
		Epoch:       fence.Epoch,
		Token:       info.InstanceToken,
		Dormant:     state == session.StateAsleep || state == session.StateDrained,
		ContentHold: session.ContentHeldInfo(info),
	}
}

// laneRow is a conversation's session row as one read gives it to a pass:
// info, and stop, its stop request (session.ReadStopRequest), which info
// carries no field for. The content gate takes both.
type laneRow struct {
	info session.Info
	stop session.StopRequest
}

// laneStopReading is how a lane pass reads a row's stop request under the
// mode a controller latched, its own lane host's or, asked over the socket,
// a pass outside it (controllerStopReading): as residue under legacy, which ignores the
// keys (CONTRACT v5 R1), so a request a rolled-back v2 run left never holds
// content to a session legacy keeps running; as v2's live request otherwise.
func laneStopReading(mode reconcilerMode) session.StopRequestReading {
	if mode == reconcilerLegacy {
		return session.StopRequestsIgnored
	}
	return session.StopRequestsLive
}

// laneControllerMode asks the city's controller for the session reconciler
// it latched (queryControllerReconcilerMode). It is a mutable global test
// seam; tests that swap it MUST NOT call t.Parallel().
var laneControllerMode = queryControllerReconcilerMode

// controllerStopReading is how a lane pass outside the controller (the CLI's
// inline pass, the prompt hook's in-band pass) reads a row's stop request:
// by the mode the city's controller latched, which it asks for with legs of
// leg, never by the on-disk config. So after a rollback to legacy it reads a
// rolled-back v2 run's request as residue as soon as the legacy controller
// answers. When the mode is unknown (no controller answers, one that does
// names no mode this build knows, or there is no time to ask) it reads the
// request live and returns why the mode is unknown: an unknown mode honors
// the keys, deferring content to the controller's lane host rather than
// starting it into a session v2 may be stopping.
func controllerStopReading(cityPath string, leg time.Duration) (session.StopRequestReading, error) {
	mode, known, err := laneControllerMode(cityPath, leg)
	if !known {
		if err == nil {
			err = errors.New("the controller named no session reconciler")
		}
		return session.StopRequestsLive, err
	}
	return laneStopReading(mode), nil
}

// unknownModeStopNote is the note a pass outside the controller gives when
// row's stop request held content only because the controller's latched mode
// was unknown (modeErr, controllerStopReading's error), and "" otherwise: a
// known mode, or a row with no stop request standing, needs no note.
func unknownModeStopNote(row laneRow, modeErr error) string {
	if modeErr == nil || !row.stop.Requested() {
		return ""
	}
	return fmt.Sprintf("the session's stop request is honored because the controller's session reconciler is unknown (%v)", modeErr)
}

// readConversationLaneViewOnly is readConversationLaneView for a caller that
// needs the view alone: the view never depends on the stop request, so the
// read asks the controller for no mode.
func readConversationLaneViewOnly(front *session.Store, conv string) (view opjournal.ConversationView, absent bool, err error) {
	view, _, absent, err = readConversationLaneView(front, conv, session.StopRequestsLive)
	return view, absent, err
}

// readConversationLaneView reads conversation conv's session row live
// through front and returns its view and row, the row's stop request read
// by reading from the same fetch. A row that is missing, or that is not a
// session row, on two reads in a row is a closed conversation: no pass can
// deliver into it, so its open ops close. absent reports that case, for the
// caller to log; one answer of absence is not proof enough to close ops
// irreversibly (DS §0).
func readConversationLaneView(front *session.Store, conv string, reading session.StopRequestReading) (view opjournal.ConversationView, row laneRow, absent bool, err error) {
	for range 2 {
		info, resp, err := front.GetPersistedResponse(conv)
		switch {
		case err == nil:
			return conversationLaneView(info), laneRow{info: info, stop: session.ReadStopRequest(resp.Metadata, reading)}, false, nil
		case errors.Is(err, beads.ErrNotFound), errors.Is(err, session.ErrSessionNotFound):
			continue
		default:
			return opjournal.ConversationView{}, laneRow{}, false, err
		}
	}
	return opjournal.ConversationView{ID: conv, Closed: true}, laneRow{info: session.Info{ID: conv, Closed: true}}, true, nil
}
