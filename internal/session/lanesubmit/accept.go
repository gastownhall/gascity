// Package lanesubmit is the one acceptance of an operator's message onto a
// session's conversation lane (DES-O1 §7.5, slice 7b). When the lane executes
// message, POST /submit and /messages (internal/api) and gc session submit's
// local fallback (cmd/gc) call Accept, and keep only their answers: the API
// its 202, events and poke; the CLI its output, its failure envelope, its
// inline pass and its ring. It also holds the two policies a submit intent
// records its message op under.
package lanesubmit

import (
	"context"
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/session"
)

// previewConversation stands in for the session while the request is
// validated before the session is resolved: resolving a configured named
// session materializes its row, which a refused request must not do.
const previewConversation = "preview"

// ResumeRefusedWhy is the reason an op Accept wrote is canceled with when the
// session refuses its resume after the write.
const ResumeRefusedWhy = "the request's resume was refused"

// Policy is the lane policy of a submit intent: default and follow_up
// deliver at the session's boundary (a lone after_turn message would only
// ride another's boundary) and interrupt_now as a deliver-now unit (D-DN: a
// failed interrupt makes the message wait, and nothing restarts the
// session); /messages is the default. Every one wakes the session if needed
// and is a prompt. A follow_up is therefore the default request on the lane:
// under one key either answers from the other's op.
func Policy(intent session.SubmitIntent) opjournal.Policy {
	when := opjournal.WhenBoundary
	if intent == session.SubmitIntentInterruptNow {
		when = opjournal.WhenNow
	}
	return opjournal.Policy{Message: opjournal.MessagePolicy{When: when, Wake: opjournal.WakeIfNeeded, Role: opjournal.RolePrompt}}
}

// LegacyPolicy is the message policy of a submit intent on the legacy paths
// (DES-O0API §3.1; /messages is the default intent): the API's admission
// under ops.content and gc session submit's local path in a city that does
// not list message record their op under it, so a request the API recorded
// and its CLI fallback derive one op digest. Accept compares a legacy op the
// key names in this shape.
func LegacyPolicy(intent session.SubmitIntent) opjournal.Policy {
	when := opjournal.WhenBoundary
	switch intent {
	case session.SubmitIntentFollowUp:
		when = opjournal.WhenAfterTurn
	case session.SubmitIntentInterruptNow:
		when = opjournal.WhenInterrupt
	}
	return opjournal.Policy{Message: opjournal.MessagePolicy{When: when, Wake: opjournal.WakeIfNeeded, Role: opjournal.RolePrompt}}
}

// Request is an operator's message to a session.
type Request struct {
	// Key is the client key the message is accepted under: the
	// Idempotency-Key, or gc session submit's --key or the key it sent the
	// API. Empty mints one, which names no op yet.
	Key    string
	Text   string
	Intent session.SubmitIntent
	// Resume is resume: true: the message consumes the session's operator
	// hold, stamped at or before its op's proposal.
	Resume bool
	// Resolve returns the session the message goes to, materializing a
	// configured named session's row. Accept calls it once, after the
	// request validates, so a refused request materializes nothing. Its
	// error is Accept's, as is.
	Resolve func(ctx context.Context) (string, error)
	// Pending is the projection's front-door refusal of a message to
	// session id while the session waits on a pending interaction. Accept
	// calls it only for a request no op under its key answers, before
	// anything is written. Its error is Accept's, as is. Every projection
	// runs its runtime's look through CheckPending, the one bounded rule.
	Pending func(ctx context.Context, id string) error
}

// Accepted is the op Accept answers a request from.
type Accepted struct {
	// Op is the op under the request's key: the lane op Propose wrote or
	// found, open or closed, or the legacy op the key names (Legacy).
	Op opjournal.Op
	// Key is the request's client key, or the one Accept minted.
	Key string
	// Owned is an op this request wrote: Propose created it, or found it
	// under a key this request minted (a create that committed and answered
	// an error). Only an owned op is canceled when its resume is refused.
	Owned bool
	// Legacy is a message op a legacy path recorded under the key before
	// message was listed, of this request: nothing was proposed or resumed.
	Legacy bool
}

// Accept accepts req onto session's conversation lane, in this order:
// validate or mint the key; preview the request (opjournal.Store.Preview),
// before the session is resolved; resolve it (req.Resolve); read the op the
// operator's key names there, where an op of another kind is a different
// request and a legacy message op answers only the request it recorded
// (LegacyPolicy); for a request no op under its key answers, the front
// door's pending-interaction refusal (req.Pending), then, with Resume, the
// row's own refusal of the resume (session.Store.ResumeHeldRefusal); propose
// the lane message op; with Resume and the op still open, consume the
// operator's hold stamped by the op's created_at (session.Store.ResumeHeld),
// canceling an owned op whose resume the session refuses. Nothing is written
// before the proposal. Its refusals are *KeyError, *MintError,
// *JournalError, *ReadError, *KindError and *ResumeError, and req.Resolve's
// and req.Pending's errors as they returned them.
func Accept(ctx context.Context, st *opjournal.Store, sessions *session.Store, req Request) (Accepted, error) {
	oreq, minted, err := laneRequest(st, req)
	if err != nil {
		return Accepted{}, err
	}
	conv, err := req.Resolve(ctx)
	if err != nil {
		return Accepted{}, err
	}
	oreq.Conversation = conv
	prior, found, err := keyedOp(st, conv, oreq.ClientKey)
	if err != nil {
		return Accepted{}, err
	}
	if found && prior.Executor != opjournal.ExecutorLane {
		if err := sameLegacyRequest(st, oreq, req.Intent, prior); err != nil {
			return Accepted{}, err
		}
		return Accepted{Op: prior, Key: oreq.ClientKey, Legacy: true}, nil
	}
	if !found {
		if err := checkNew(ctx, sessions, req, conv); err != nil {
			return Accepted{}, err
		}
	}
	op, created, err := st.Propose(oreq)
	if err != nil {
		return Accepted{}, &JournalError{Err: err}
	}
	acc := Accepted{Op: op, Key: oreq.ClientKey, Owned: created || minted}
	if req.Resume && op.Status != opjournal.StatusClosed {
		if err := sessions.ResumeHeld(conv, op.CreatedAt, time.Now().UTC()); err != nil {
			return Accepted{}, refusedResume(st, conv, acc, err)
		}
	}
	return acc, nil
}

// laneRequest is req's lane message op at the preview conversation, its key
// validated or minted (minted reports which), previewed.
func laneRequest(st *opjournal.Store, req Request) (opjournal.Request, bool, error) {
	key, minted := req.Key, false
	if key == "" {
		k, err := legacyop.MintKey()
		if err != nil {
			return opjournal.Request{}, false, &MintError{Err: err}
		}
		key, minted = k, true
	} else if err := opjournal.ValidateClientKey(key); err != nil {
		return opjournal.Request{}, false, &KeyError{Err: err}
	}
	oreq := opjournal.Request{
		Conversation: previewConversation,
		Kind:         opjournal.KindMessage,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    key,
		MintedKey:    minted,
		Producer:     opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:      req.Text,
		Policy:       Policy(req.Intent),
	}
	if _, err := st.Preview(oreq); err != nil {
		return opjournal.Request{}, false, &JournalError{Err: err}
	}
	return oreq, minted, nil
}

// keyedOp reads the message op the operator's key names on conv: found
// false when it names none.
func keyedOp(st *opjournal.Store, conv, key string) (opjournal.Op, bool, error) {
	id, err := opjournal.DeriveID(st.Prefix(), conv, legacyop.PrincipalOperator, key)
	if err != nil {
		return opjournal.Op{}, false, &KeyError{Err: err}
	}
	op, err := st.Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return opjournal.Op{}, false, nil
	case err != nil:
		return opjournal.Op{}, false, &ReadError{ID: id, Err: err}
	case op.Kind != opjournal.KindMessage:
		return opjournal.Op{}, false, &KindError{Op: op}
	}
	return op, true, nil
}

// sameLegacyRequest refuses, as a key that names a different request, a
// request whose key names prior, a legacy message op, unless prior recorded
// it: its digest in the legacy shape (LegacyPolicy of its intent) is
// prior's.
func sameLegacyRequest(st *opjournal.Store, oreq opjournal.Request, intent session.SubmitIntent, prior opjournal.Op) error {
	legacy := oreq
	legacy.Executor = prior.Executor
	legacy.Policy = LegacyPolicy(intent)
	preview, err := st.Preview(legacy)
	if err != nil {
		return &JournalError{Err: err}
	}
	if preview.PayloadDigest != prior.PayloadDigest {
		return &JournalError{Err: &opjournal.KeyReuseError{Existing: prior}}
	}
	return nil
}

// checkNew is the refusal, writing nothing, of a request no op under its
// key answers: the front door's (req.Pending), then, with Resume, the row's
// own refusal of the resume.
func checkNew(ctx context.Context, sessions *session.Store, req Request, conv string) error {
	if err := req.Pending(ctx, conv); err != nil {
		return err
	}
	if !req.Resume {
		return nil
	}
	if err := sessions.ResumeHeldRefusal(conv, time.Now().UTC()); err != nil {
		return &ResumeError{Session: conv, Err: err}
	}
	return nil
}

// refusedResume is the *ResumeError of acc's op, whose resume the session
// refused with err once the op was written; an owned op is canceled first.
func refusedResume(st *opjournal.Store, conv string, acc Accepted, err error) error {
	op := acc.Op
	rerr := &ResumeError{Session: conv, Err: err, Op: &op, Owned: acc.Owned}
	if acc.Owned {
		rerr.Cancel, _, rerr.CancelErr = st.Cancel(op.ID, legacyop.PrincipalOperator, ResumeRefusedWhy)
	}
	return rerr
}
