package main

import (
	"errors"
	"strings"

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

// laneStopReading is how the lane host of a controller that latched mode
// reads a row's stop request: as residue under legacy, which ignores the
// keys (CONTRACT v5 R1), so a request a rolled-back v2 run left never holds
// content to a session legacy keeps running; as v2's live request otherwise.
func laneStopReading(mode reconcilerMode) session.StopRequestReading {
	if mode == reconcilerLegacy {
		return session.StopRequestsIgnored
	}
	return session.StopRequestsLive
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
