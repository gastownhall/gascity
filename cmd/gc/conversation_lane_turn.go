package main

// The live executor's turn evidence (DES-O1 §4.8, X11 §5.4): what its probe
// reads of the session's turns through worker.TurnHandle.CurrentTurn
// (Probe.Turn, TurnLive and KnownTurns, and a protocol runtime's boundary),
// and the reads its Deliver makes within the link wait for CorrelateTurn.
// Nothing here reads delivered text, and nothing writes a turn fact: the
// observer (TI-3) is the only writer of those (D-TURN).

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// The link wait's read pacing (DES-O1 §4.8): one read at once after the
// effect, then reads backing off from laneLinkBackoffStart to
// laneLinkBackoffMax until the link is decided or Caps.LinkWait passes.
const (
	laneLinkBackoffStart = 100 * time.Millisecond
	laneLinkBackoffMax   = time.Second
)

// laneTurnTraits are what the executor can learn of a session's turns,
// fixed for the session's generation (worker.TurnCapabilities).
type laneTurnTraits struct {
	// readable: CurrentTurn reads the session's turns in this process.
	readable bool
	// protocol: the runtime itself reports them (its CurrentTurn says
	// whether a turn is in progress), so a protocol-mode runtime has a
	// boundary to deliver at.
	protocol bool
	// starts: the source records turn starts, so a delivery can be linked
	// to the turn it started (TurnEvidence.HasSource).
	starts bool
	// sameClock and serial are TurnEvidence's: a transcript's and ACP's
	// starts are on this host's clock, and only gc's input starts an ACP
	// turn.
	sameClock bool
	serial    bool
}

// laneTurnTraitsOf reads a session's turn traits from its capabilities and
// its transport.
func laneTurnTraitsOf(caps worker.TurnCapabilities, transport string) laneTurnTraits {
	starts := caps.Start == runtime.TurnStartRecorded
	switch caps.Source {
	case worker.TurnSourceTranscript:
		return laneTurnTraits{readable: true, starts: starts, sameClock: true}
	case worker.TurnSourceProtocol:
		acp := transport == "acp"
		return laneTurnTraits{readable: true, protocol: true, starts: starts, sameClock: acp, serial: acp}
	default:
		return laneTurnTraits{}
	}
}

// laneTurnLook is one probe's read of the session's turns.
type laneTurnLook struct {
	// live is whether a turn was in progress; unknown when nothing was read
	// or the read failed.
	live opjournal.Tri
	// turn is the turn in progress, "" when none is or live is unknown.
	turn string
	// known are the ids of the current and recent turns the read returned.
	known []string
}

// laneTurnLookOf is the look a CurrentTurn answer gives. A failed read,
// including a source that cannot identify turns, tells nothing: it is never
// read as "no turn in progress".
func laneTurnLookOf(hist runtime.TurnHistory, err error) laneTurnLook {
	if err != nil {
		return laneTurnLook{live: opjournal.TriUnknown}
	}
	look := laneTurnLook{live: opjournal.TriNo}
	if hist.Current.Active() {
		look.live, look.turn = opjournal.TriYes, hist.Current.TurnID
		look.known = append(look.known, hist.Current.TurnID)
	}
	for _, t := range hist.Recent {
		if t.TurnID != "" && !slices.Contains(look.known, t.TurnID) {
			look.known = append(look.known, t.TurnID)
		}
	}
	return look
}

// boundary is a protocol runtime's input boundary (DES-T3I §14 item 6): yes
// when no turn is in progress, no while one is, unknown when the read said
// nothing.
func (l laneTurnLook) boundary() opjournal.Tri {
	switch l.live {
	case opjournal.TriNo:
		return opjournal.TriYes
	case opjournal.TriYes:
		return opjournal.TriNo
	default:
		return opjournal.TriUnknown
	}
}

// readTurnTraits resolves the handle the executor reads turns through and
// reads the session's turn traits, once per executor. A session that is
// gone has none.
func (e *conversationLaneExec) readTurnTraits(ctx context.Context) error {
	h, err := e.turns()
	if err == nil {
		var caps worker.TurnCapabilities
		if caps, err = h.TurnCapabilities(ctx); err == nil {
			e.turnHandle = h
			e.traits = laneTurnTraitsOf(caps, e.target.sessionTransport())
			return nil
		}
	}
	if errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, session.ErrSessionNotFound) {
		e.turnHandle, e.traits = nil, laneTurnTraits{}
		return nil
	}
	return err
}

// readTurns reads the session's turns once through the executor's turn
// handle.
func (e *conversationLaneExec) readTurns(ctx context.Context) (runtime.TurnHistory, error) {
	if e.turnHandle == nil {
		return runtime.TurnHistory{}, runtime.ErrTurnIdentityUnsupported
	}
	return e.turnHandle.CurrentTurn(ctx)
}

// linkEvidence gathers the turn evidence of the delivery rec records, after
// its effect (DES-O1 §4.8): reads of the session's turns, the first at once,
// then backing off, until CorrelateTurn decides the link or the caps' link
// wait passes; no read runs past the wait. It starts from the probe's look,
// which the lane sets again from its own probe.
func (e *conversationLaneExec) linkEvidence(ctx context.Context, rec opjournal.DeliveryRecord) opjournal.TurnEvidence {
	ev := opjournal.TurnEvidence{
		HasSource:   e.traits.starts && e.turnHandle != nil,
		SameClock:   e.traits.sameClock,
		Serial:      e.traits.serial,
		LiveAtProbe: e.lastTurn.live,
		LiveTurn:    e.lastTurn.turn,
		Known:       slices.Clone(e.lastTurn.known),
	}
	if !ev.HasSource {
		return ev
	}
	deadline := e.now().Add(e.lastCaps.LinkWait)
	backoff := laneLinkBackoffStart
	for {
		remaining := deadline.Sub(e.now())
		if remaining <= 0 {
			return ev
		}
		rctx, cancel := context.WithTimeout(ctx, remaining)
		hist, err := e.readTurns(rctx)
		cancel()
		ev = laneNoteTurnRead(ev, rec.Generation, hist, err)
		if _, decided := opjournal.CorrelateTurn(rec, ev); decided {
			return ev
		}
		if !e.wait(ctx, min(backoff, deadline.Sub(e.now()))) {
			return ev
		}
		backoff = min(2*backoff, laneLinkBackoffMax)
	}
}

// laneNoteTurnRead adds one read after the effect to ev: a failed read sets
// ReadErr and adds no turn; a successful one clears it, appends the turns it
// showed, in generation gen and oldest start first, and is the first read
// unless an earlier one succeeded.
func laneNoteTurnRead(ev opjournal.TurnEvidence, gen int64, hist runtime.TurnHistory, err error) opjournal.TurnEvidence {
	if err != nil {
		ev.ReadErr = err
		return ev
	}
	ev.ReadErr = nil
	for i := len(hist.Recent) - 1; i >= 0; i-- {
		t := hist.Recent[i]
		ev.Seen = append(ev.Seen, opjournal.SeenTurn{ID: t.TurnID, Generation: gen, StartedAt: t.StartedAt})
	}
	if hist.Current.Active() {
		ev.Seen = append(ev.Seen, opjournal.SeenTurn{ID: hist.Current.TurnID, Generation: gen, StartedAt: hist.CurrentStart, Running: true})
	}
	if !ev.FirstRead.Done {
		ev.FirstRead = opjournal.FirstTurnRead{Done: true, Current: hist.Current.TurnID}
	}
	return ev
}

// laneLinkWait waits d, reporting false when ctx ended first.
func laneLinkWait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	return sleepUntil(ctx, d) == nil
}
