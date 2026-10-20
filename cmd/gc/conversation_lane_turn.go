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
	"fmt"
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
// gone has none. A read that is unavailable is errLaneTurnsUnavailable for
// a protocol-mode runtime, whose boundary its turns are, and for a
// keystroke-mode one, whose boundary is the quiescence rule, no turn source
// for this executor, noted.
func (e *conversationLaneExec) readTurnTraits(ctx context.Context) error {
	e.turnHandle, e.traits = nil, laneTurnTraits{}
	h, err := e.turns()
	if err == nil {
		var caps worker.TurnCapabilities
		if caps, err = h.TurnCapabilities(ctx); err == nil {
			e.turnHandle = h
			e.traits = laneTurnTraitsOf(caps, e.target.sessionTransport())
			return nil
		}
	}
	switch {
	case errors.Is(err, runtime.ErrSessionNotFound), errors.Is(err, session.ErrSessionNotFound):
		return nil
	case errors.Is(err, runtime.ErrRuntimeUnavailable) && e.protocol:
		return fmt.Errorf("%w: %w", errLaneTurnsUnavailable, err)
	case errors.Is(err, runtime.ErrRuntimeUnavailable):
		e.note("turns", fmt.Errorf("session %s's turn capabilities unreadable; linking its deliveries none: %w", e.info.ID, err))
		return nil
	default:
		return err
	}
}

// errLaneTurnsUnavailable is a protocol-mode runtime's turn capabilities
// read that was unavailable: the probe answers unavailable, as for a look
// that was.
var errLaneTurnsUnavailable = errors.New("turn capabilities unavailable")

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
// wait passes. Each read is bounded by what is left of the wait and
// abandoned when it passes (linkRead), so no read holds the delivery
// past it, even one that ignores its context. It starts from the probe's
// look, which the lane sets again from its own probe.
//
// A source has evidence to link by only when its starts are recorded and a
// delivery can be told from a turn another input started: by the runtime's
// stamp of the message (rec.MessageAt), by a start on this host's clock
// (SameClock, the history diff's cutoff), or by only gc's input starting
// turns (Serial). A T3 delivery links only by its receipt's stamp (X11 §5.4;
// DES-O1 §4.8 row 3, requested_at), never by a history diff, which would link
// a turn the delivery did not start (a turn T3 restores as latest after the
// probe, or a human's started meanwhile); a turn whose start is not the stamp
// never matches it.
func (e *conversationLaneExec) linkEvidence(ctx context.Context, rec opjournal.DeliveryRecord) opjournal.TurnEvidence {
	ev := e.linkStart(rec)
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
		hist, err := e.linkRead(ctx, remaining)
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

// linkStart is the evidence linkEvidence starts from, before any read: the
// source's traits and the probe's look. A replayed delivery's evidence is
// this alone.
func (e *conversationLaneExec) linkStart(rec opjournal.DeliveryRecord) opjournal.TurnEvidence {
	tellable := e.traits.sameClock || e.traits.serial || !rec.MessageAt.IsZero()
	return opjournal.TurnEvidence{
		HasSource:   e.traits.starts && e.turnHandle != nil && tellable,
		SameClock:   e.traits.sameClock,
		Serial:      e.traits.serial,
		LiveAtProbe: e.lastTurn.live,
		LiveTurn:    e.lastTurn.turn,
		Known:       slices.Clone(e.lastTurn.known),
	}
}

// linkRead is one read of the link wait, given up after d: the read runs
// under a context that ends then, and is abandoned if it has not answered by
// then (worker.ObserveBounded), as a transcript scan that ignores its
// context would not be. While an abandoned read is still running, the
// session's next link read answers unavailable without starting another.
func (e *conversationLaneExec) linkRead(ctx context.Context, d time.Duration) (runtime.TurnHistory, error) {
	rctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return worker.ObserveBounded(rctx, laneLinkReadKey(e.deps.cityPath, e.info.ID), e.readTurns)
}

// laneLinkReadKey keys a session's link reads, apart from its probes'
// observations (laneObservationKey), so a wedged link read never blocks a
// probe.
func laneLinkReadKey(cityPath, sessionID string) string {
	return "lane-link\x00" + cityPath + "\x00" + sessionID
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
