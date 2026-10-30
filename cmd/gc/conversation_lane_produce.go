package main

// The CLI's conversation-lane notices (DES-O1 §7.5): mail --notify, sling
// --nudge and the hook-claim continuation propose lane notices when [ops]
// authoritative lists message. Three rules bound what they promise.
//
// Rule 1, routing (routeCLILaneNotice). Every notice reads its target's row
// in the city's sessions-class store, never in a rig bead's store. A row the
// read cannot answer for is a refusal; a target with no row, or one no lane
// serves (a protocol-mode session without a host stamp, laneServesSession),
// keeps the producer's legacy path, as gc session nudge's does. A proposal
// the op journal cannot record is a refusal too (cliLaneRefusal): the
// producer reports it (gc sling and gc mail send warn, as they do when their
// legacy delivery fails, and gc hook --claim warns on stderr) and neither
// delivers nor queues anything.
//
// Rule 2, one notice per event. Each notice is proposed under a minted key,
// its cause (the message, bead or root it announces) in causation. Nothing
// coalesces a repeated event into an earlier notice, and nothing answers
// from a closed one: a re-sling or a second claim of a root is a second
// notice, as it is a second queue item on the legacy path. The open notices
// of one conversation are delivered together at the boundary (D8).
//
// Rule 3, best-effort withdrawal (mail_notice_withdraw.go). A mail notice is
// canceled when gc mail or the API's mail provider reads, marks read,
// archives or deletes its message, and when SessionStart archives an
// injected handoff. Any other consumer of a message (an exec mail provider,
// a direct bd edit, a read before the notice is proposed) can leave its
// notice open; the agent then finds no new mail and moves on.
//
// After the Propose the producer rings the controller's lane host
// (lane:<conversation>:<op id>) once, best effort; the lane host delivers
// every notice. gc session submit's local fallback is not a producer here:
// it accepts the operator's message as the API does
// (conversation_lane_submit.go).

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// errCLILaneUnavailable classifies a CLI lane proposal the op journal could
// not record: nothing was written, nothing delivered.
var errCLILaneUnavailable = errors.New("op_journal_unavailable")

// cliLaneRefusal is the refusal of a CLI lane proposal the op journal could
// not record for cause: a *cliLaneRefusalError, which wraps
// errCLILaneUnavailable.
func cliLaneRefusal(cause any) error {
	return &cliLaneRefusalError{cause: cause}
}

// cliLaneRefusalError is a CLI lane proposal the op journal could not record
// for cause, reported after errCLILaneUnavailable's code.
type cliLaneRefusalError struct{ cause any }

func (e *cliLaneRefusalError) Error() string {
	return errCLILaneUnavailable.Error() + ": " + cliLaneUnavailable(e.cause)
}

func (e *cliLaneRefusalError) Unwrap() error { return errCLILaneUnavailable }

// cliLaneNoticeRoute is one CLI notice for routeCLILaneNotice.
type cliLaneNoticeRoute struct {
	// target names the notice's session: its row id when the producer
	// resolved one in city, else its session name or claim identity. Its
	// cityPath and cfg are the city's.
	target nudgeTarget
	// city is the city's store, whose sessions class holds the session rows;
	// never a rig bead's store. The caller owns and closes it.
	city beads.Store
	sp   runtime.Provider
	// req is the notice without its conversation (cliLaneNotice).
	req opjournal.Request
	// sessionScoped scopes the notice to the session's current generation.
	sessionScoped bool
}

// routeCLILaneNotice is Rule 1, the one routing rule of every CLI notice:
// when [ops] authoritative lists message, off a worker site, it reads n's
// target row in the city's sessions-class store (cliLaneNoticeRow) and
// proposes the notice there when a lane serves the session (cliLaneProduce).
// It returns handled false, having written nothing, when the lane is off,
// the target has no row, or no lane serves the session: the producer then
// takes its legacy path. A city store that did not open, and a row the read
// could not answer for, are refusals (cliLaneRefusal), handled true.
func routeCLILaneNotice(n cliLaneNoticeRoute) (cliLaneProduced, error) {
	cfg, cityPath := n.target.cfg, n.target.cityPath
	if !cliLaneExecutesMessage(cfg) || onWorkerSite(cityPath) {
		return cliLaneProduced{}, nil
	}
	if isNilBeadStore(n.city) {
		return cliLaneProduced{handled: true}, cliLaneRefusal("the city store did not open")
	}
	info, found, err := cliLaneNoticeRow(n.target, cliSessionStore(n.city, cfg, cityPath))
	if err != nil {
		return cliLaneProduced{handled: true}, cliLaneRefusal(err)
	}
	if !found {
		return cliLaneProduced{}, nil
	}
	return cliLaneProduce(cliLaneProduction{
		target:        resolveNudgeTargetFromSessionInfo(cityPath, cfg, info),
		store:         n.city,
		sp:            n.sp,
		req:           n.req,
		sessionScoped: n.sessionScoped,
	})
}

// cliLaneNoticeRow reads target's open session row in sessions, the city's
// sessions-class store: the row target's session id names, else the one its
// session name resolves to (resolveSessionIDWithConfig, which materializes
// nothing). found is false when neither names an open row. Any other read
// error is returned: it is never taken for a missing row.
func cliLaneNoticeRow(target nudgeTarget, sessions beads.Store) (info session.Info, found bool, err error) {
	front := sessionFrontDoor(sessions)
	if target.sessionID != "" {
		info, err := front.Get(target.sessionID)
		switch {
		case err == nil && !info.Closed:
			return info, true, nil
		case err != nil && !errors.Is(err, beads.ErrNotFound) && !errors.Is(err, session.ErrSessionNotFound):
			return session.Info{}, false, fmt.Errorf("reading session %s: %w", target.sessionID, err)
		}
	}
	if target.sessionName == "" {
		return session.Info{}, false, nil
	}
	id, err := resolveSessionIDWithConfig(target.cityPath, target.cfg, sessions, target.sessionName)
	switch {
	case errors.Is(err, session.ErrSessionNotFound):
		return session.Info{}, false, nil
	case err != nil:
		return session.Info{}, false, fmt.Errorf("reading session %q: %w", target.sessionName, err)
	}
	info, err = front.Get(id)
	if err != nil {
		return session.Info{}, false, fmt.Errorf("reading session %s: %w", id, err)
	}
	return info, !info.Closed, nil
}

// cliLaneProduction is one CLI notice's lane op, its session row found in
// the city's sessions-class store (routeCLILaneNotice).
type cliLaneProduction struct {
	target nudgeTarget
	// store is the city's store; the op store opens over its sessions class.
	store beads.Store
	sp    runtime.Provider
	// req is the op without its conversation; an empty ClientKey is minted.
	req opjournal.Request
	// sessionScoped scopes the op to the session's current generation.
	sessionScoped bool
}

// cliLaneProduced is what cliLaneProduce did: handled false when the
// producer takes its legacy path, nothing written; else the op as read
// after the Propose and whether the Propose created it.
type cliLaneProduced struct {
	handled bool
	op      opjournal.Op
	created bool
}

// cliLaneKeyReuseError refuses a CLI lane proposal whose key names a
// different request; it unwraps to the journal's *opjournal.KeyReuseError.
type cliLaneKeyReuseError struct {
	key   string
	reuse *opjournal.KeyReuseError
}

func (e *cliLaneKeyReuseError) Error() string {
	return fmt.Sprintf("the key %q names a different request (op %s)", e.key, e.reuse.Existing.ID)
}

func (e *cliLaneKeyReuseError) Unwrap() error { return e.reuse }

// cliLaneProduce proposes p's op on the conversation lane when a lane host
// can type into p's target session (laneServesSession), and rings the lane
// host. It returns handled false, having written nothing, when the row is
// gone or no lane serves the session. A proposal it cannot write is an error
// wrapping errCLILaneUnavailable, and one whose key names a different
// request a *cliLaneKeyReuseError, handled true.
func cliLaneProduce(p cliLaneProduction) (cliLaneProduced, error) {
	if p.target.sessionID == "" || isNilBeadStore(p.store) {
		return cliLaneProduced{}, nil
	}
	sessions := cliSessionStore(p.store, p.target.cfg, p.target.cityPath)
	unavailable := func(err error) (cliLaneProduced, error) {
		return cliLaneProduced{handled: true}, cliLaneRefusal(err)
	}
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: sessions}, cliOpClock)
	if err != nil {
		return unavailable(err)
	}
	view, absent, err := readConversationLaneViewOnly(sessionFrontDoor(sessions), p.target.sessionID)
	switch {
	case err != nil:
		return unavailable(fmt.Errorf("reading session %s: %w", p.target.sessionID, err))
	case absent:
		return cliLaneProduced{}, nil
	}
	serves, err := laneServesSession(p.sp, st, p.target.sessionName, view)
	if err != nil {
		return unavailable(err)
	}
	if !serves {
		return cliLaneProduced{}, nil
	}
	req := p.req
	req.Conversation = p.target.sessionID
	req.Executor = opjournal.ExecutorLane
	req.Kind = opjournal.KindMessage
	if req.ClientKey == "" {
		key, err := legacyop.MintKey()
		if err != nil {
			return unavailable(err)
		}
		req.ClientKey, req.MintedKey = key, true
	}
	if p.sessionScoped {
		req.Scope = opjournal.Scope{Kind: opjournal.ScopeSession, Generation: view.Generation}
	}
	op, created, err := st.Propose(req)
	var reuse *opjournal.KeyReuseError
	switch {
	case errors.As(err, &reuse):
		return cliLaneProduced{handled: true}, &cliLaneKeyReuseError{key: req.ClientKey, reuse: reuse}
	case err != nil:
		return unavailable(err)
	}
	if op.Status != opjournal.StatusClosed {
		// Best effort: the controller's anti-entropy scan finds the op too.
		_, _ = cliLaneRing(p.target.cityPath, p.target.sessionID, op.ID)
	}
	return cliLaneProduced{handled: true, op: op, created: created}, nil
}

// cliLaneNotice is the lane request of a CLI notice from principal under a
// minted key, caused by causation (Rule 2), delivered at the boundary with
// wake.
func cliLaneNotice(principal, causation, text string, wake opjournal.Wake) opjournal.Request {
	return opjournal.Request{
		Kind:      opjournal.KindMessage,
		Executor:  opjournal.ExecutorLane,
		Producer:  opjournal.Producer{Principal: principal, Actor: opjournal.ActorBackground},
		Payload:   text,
		Causation: causation,
		Policy:    opjournal.Policy{Message: opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: wake}},
	}
}
