package main

// gc session submit under lane authority (slice 7b, SESSION-START-025): when
// [ops] authoritative lists message, the command accepts the operator's
// message as POST /submit does, through the one acceptance both share
// (lanesubmit.Accept), for every session the API accepts.
//
// It first calls the API under the key it falls back under (--key, else
// minted), whatever ops.content says, with resume: true. When the call fails
// so the command falls back (api.ShouldFallback), it accepts the message
// locally under that key, so a lost answer finds the API's op instead of
// sending again: the key is read before anything is created. With no API it
// accepts under --key, or under a key Accept mints.
//
// It never delivers through the legacy path. The acceptance runs the
// resume first, so nothing runs ahead of the hold the message consumes, and
// cancels an op this run wrote whose resume the session refuses. Then, for
// an open lane op on a keystroke-mode runtime, the CLI's content-only inline
// pass delivers a message at an idle boundary (never an interrupt_now, a
// deliver-now unit, which is control); otherwise the controller's lane host
// delivers it, and a submit to a protocol-mode session waits for the
// controller. The command reports the op by its state, in one vocabulary
// whoever wrote it (queued, submitted, a replay of a closed op, or a refusal
// with its failure envelope under --json), and rings the controller's lane
// host once.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/lanesubmit"
)

// Failure-envelope codes only the lane submit reports.
const (
	// cliOpCodePendingInteraction refuses a new message to a session that
	// waits on a pending interaction, as the API's 409 pending_interaction.
	cliOpCodePendingInteraction = "pending_interaction"
	// cliOpCodePendingCheckUnavailable refuses a new message to a session
	// whose look for a pending interaction did not answer, as the API's 503
	// pending_check_unavailable. Nothing was written; a rerun may succeed.
	cliOpCodePendingCheckUnavailable = "pending_check_unavailable"
	// cliOpCodeResumeRefused is the session's refusal of the message's
	// resume.
	cliOpCodeResumeRefused = "resume_refused"
)

// sessionSubmitLane is one gc session submit under lane authority.
type sessionSubmitLane struct {
	cityPath, target, message string
	intent                    session.SubmitIntent
	// key is --key.
	key    string
	asJSON bool
	stdout io.Writer
	stderr io.Writer

	cfg      *config.City
	sessions beads.Store
	st       *opjournal.Store
	sp       runtime.Provider
}

// submitSessionLane runs s: the API first, under the key the fallback then
// accepts under, and the local acceptance when the API call falls back.
func submitSessionLane(s *sessionSubmitLane) int {
	key := s.key
	if c := apiClient(s.cityPath); c != nil {
		if key == "" {
			minted, err := legacyop.MintKey()
			if err != nil {
				// Without a key a lost answer could not be told apart
				// from a message never sent.
				return s.refuse(cliOpCodeUnavailable, s.unavailable(fmt.Errorf("minting a key: %w", err)), "", "")
			}
			key = minted
		}
		if code, fallBack := submitViaAPI(c, s.target, s.message, s.intent, s.key, key, s.asJSON, s.stdout, s.stderr); !fallBack {
			return code
		}
	}
	return s.fallback(key)
}

// fallback accepts the message locally under key ("" mints one): the lane
// preflight, before the session resolves, then lanesubmit.Accept.
func (s *sessionSubmitLane) fallback(key string) int {
	cfg, err := loadCityConfig(s.cityPath, configWarnWriter(s.asJSON, s.stderr))
	if err != nil {
		fmt.Fprintf(s.stderr, "gc session submit: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	s.cfg = cfg
	store, code := openCityStore(s.stderr, "gc session submit")
	if store == nil {
		return code
	}
	s.sessions = cliSessionStore(store, cfg, s.cityPath)
	if err := laneStoreCapability(s.sessions); err != nil {
		return s.refuse(cliOpCodeUnavailable, err.Error(), "", key)
	}
	if s.st, err = opjournal.NewLaneStore(beads.SessionStore{Store: s.sessions}, cliOpClock); err != nil {
		return s.refuse(cliOpCodeUnavailable, s.unavailable(err), "", key)
	}
	acc, err := lanesubmit.Accept(context.Background(), s.st, sessionFrontDoor(s.sessions), lanesubmit.Request{
		Key:    key,
		Text:   s.message,
		Intent: s.intent,
		Resume: true,
		Resolve: func(context.Context) (string, error) {
			return resolveSessionIDMaterializingNamed(s.cityPath, cfg, s.sessions, s.target)
		},
		Pending: s.pending,
	})
	if err != nil {
		return s.refuseAccept(err, key)
	}
	return s.answer(acc)
}

// provider opens the city's session provider once.
func (s *sessionSubmitLane) provider() (runtime.Provider, error) {
	if s.sp != nil {
		return s.sp, nil
	}
	sp, err := newSessionProvider()
	if err != nil {
		return nil, err
	}
	s.sp = sp
	return sp, nil
}

// pending is the CLI's front door, the API's rejectPendingInteraction rule
// and check (lanesubmit.CheckPending): a new message to session id while it
// waits on a pending interaction is refused (session.PendingInteractionError),
// writing nothing, and so is one whose look fails, does not answer within
// lanesubmit.PendingCheckTimeout, or panics (*lanesubmit.PendingCheckError,
// refused pending_check_unavailable).
func (s *sessionSubmitLane) pending(ctx context.Context, id string) error {
	sp, err := s.provider()
	if err != nil {
		return err
	}
	handle, err := workerHandleForSessionWithConfig(s.cityPath, s.sessions, sp, s.cfg, id)
	if err != nil {
		return err
	}
	return lanesubmit.CheckPending(ctx, id, func(ctx context.Context) (*runtime.PendingInteraction, error) {
		p, _, err := handle.PendingStatus(ctx)
		if err != nil || p == nil {
			return nil, err
		}
		return &runtime.PendingInteraction{RequestID: p.RequestID, Kind: p.Kind, Prompt: p.Prompt, Options: p.Options, Metadata: p.Metadata}, nil
	})
}

// answer reports acc by its op's state, whoever wrote the op: an open legacy
// op of this request (written before message was listed) is in progress, a
// closed op replays, and an open lane op is delivered or queued (deliver).
func (s *sessionSubmitLane) answer(acc lanesubmit.Accepted) int {
	op := acc.Op
	switch {
	case acc.Legacy && op.Status != opjournal.StatusClosed:
		return s.refuse(cliOpCodeInProgress, fmt.Sprintf("op %s is in progress under its legacy path; rerun later with --key %s to see its outcome", op.ID, acc.Key), op.ID, acc.Key)
	case op.Status == opjournal.StatusClosed:
		return s.replay(acc)
	}
	return s.deliver(acc)
}

// replay reports a closed op found under the key, as the API path reports a
// replayed answer.
func (s *sessionSubmitLane) replay(acc lanesubmit.Accepted) int {
	op := acc.Op
	if !cliOpReplaysAsSuccess(op.Kind, op.Outcome) {
		return s.refuse(cliOpCodeAlreadyFailed, fmt.Sprintf("op %s closed %s; rerun without --key to act again", op.ID, cliOpOutcomeText(op.Outcome)), op.ID, acc.Key)
	}
	fmt.Fprintf(s.stderr, "gc session submit: replayed op %s; resume was not reapplied (outside the op's request)\n", op.ID) //nolint:errcheck // best-effort stderr
	return emitSessionSubmitResult(s.stdout, s.stderr, s.target, s.intent, op.Outcome.Result.Via == legacyop.ViaLegacyQueued, s.asJSON,
		sessionSubmitOp{id: op.ID, key: acc.Key, replayed: true})
}

// deliver runs the inline pass for an open lane op (inlinePass), reports
// the op as it then reads, and rings the controller's lane host once.
func (s *sessionSubmitLane) deliver(acc lanesubmit.Accepted) int {
	op := s.inlinePass(acc.Op)
	var code int
	switch arm, _ := op.Outcome.Effective(); {
	case op.Status != opjournal.StatusClosed:
		code = emitSessionSubmitResult(s.stdout, s.stderr, s.target, s.intent, true, s.asJSON, sessionSubmitOp{id: op.ID, key: acc.Key})
	case arm == opjournal.ArmSucceeded:
		code = emitSessionSubmitResult(s.stdout, s.stderr, s.target, s.intent, false, s.asJSON, sessionSubmitOp{id: op.ID, key: acc.Key})
	default:
		code = s.refuse(cliOpCodeFailed, fmt.Sprintf("op %s closed %s", op.ID, cliOpOutcomeText(op.Outcome)), op.ID, acc.Key)
	}
	cliLaneRingHost(s.cityPath, op.Conversation, op.ID, s.st, s.warnf)
	return code
}

// inlinePass runs the CLI's content-only inline pass (cliLaneInlinePass)
// over op's conversation when the session's runtime types live, never for
// an interrupt_now, and returns op as read after it, its warnings given
// (cliLaneWarnPass). A read that fails leaves op as accepted, open.
func (s *sessionSubmitLane) inlinePass(op opjournal.Op) opjournal.Op {
	ran, res, passErr := s.pass(op)
	cur, err := s.st.Get(op.ID)
	if err != nil {
		if ran {
			s.warnf("reading op %s after the inline delivery failed (%v); it waits for the controller", op.ID, err)
		} else {
			s.warnf("reading op %s after its acceptance failed (%v); it waits for the controller", op.ID, err)
		}
		return op
	}
	if ran {
		cliLaneWarnPass(s.warnf, cur, res, passErr)
	}
	return cur
}

// pass runs the inline pass for op when it may run: ran is false for an
// interrupt_now, and for a runtime that does not type live, whose lane host
// delivers.
func (s *sessionSubmitLane) pass(op opjournal.Op) (ran bool, res opjournal.PassResult, err error) {
	if s.intent == session.SubmitIntentInterruptNow {
		return false, res, nil
	}
	sp, err := s.provider()
	if err != nil {
		s.warnf("the inline delivery did not run (%v); op %s waits for the controller", err, op.ID)
		return false, res, nil
	}
	info, err := sessionFrontDoor(s.sessions).Get(op.Conversation)
	if err != nil {
		s.warnf("the inline delivery did not run (reading session %s: %v); op %s waits for the controller", op.Conversation, err, op.ID)
		return false, res, nil
	}
	target := resolveNudgeTargetFromSessionInfo(s.cityPath, s.cfg, info)
	if !laneTypesLive(sp, target.sessionName) {
		return false, res, nil
	}
	res, err = cliLaneInlinePass(context.Background(), target, s.sessions, sp, s.st, s.warnf)
	return true, res, err
}

// refuseAccept reports a refusal of lanesubmit.Accept under key.
func (s *sessionSubmitLane) refuseAccept(err error, key string) int {
	var (
		mint    *lanesubmit.MintError
		kerr    *lanesubmit.KeyError
		journal *lanesubmit.JournalError
		read    *lanesubmit.ReadError
		kind    *lanesubmit.KindError
		resume  *lanesubmit.ResumeError
		check   *lanesubmit.PendingCheckError
		reuse   *opjournal.KeyReuseError
	)
	switch {
	case errors.As(err, &kerr):
		return s.refuse(cliOpCodeInvalidKey, fmt.Sprintf("invalid --key (%v)", kerr.Err), "", key)
	case errors.As(err, &mint):
		return s.refuse(cliOpCodeUnavailable, s.unavailable(fmt.Errorf("minting a key: %w", mint.Err)), "", "")
	case errors.As(err, &journal) && errors.As(journal.Err, &reuse):
		return s.refuse(cliOpCodeKeyMismatch, fmt.Sprintf("the key names a different request (op %s); rerun with another --key", reuse.Existing.ID), reuse.Existing.ID, key)
	case errors.As(err, &journal):
		return s.refuse(cliOpCodeUnavailable, s.unavailable(journal.Err), "", key)
	case errors.As(err, &read):
		return s.refuse(cliOpCodeUnavailable, s.unavailable(fmt.Errorf("reading op %s: %w", read.ID, read.Err)), "", key)
	case errors.As(err, &kind):
		return s.refuse(cliOpCodeKeyMismatch, fmt.Sprintf("the key names a different request (op %s, a %s op); rerun with another --key", kind.Op.ID, kind.Op.Kind), kind.Op.ID, key)
	case errors.As(err, &resume):
		return s.refuseResume(resume, key)
	case errors.As(err, &check):
		return s.refuse(cliOpCodePendingCheckUnavailable, err.Error(), "", key)
	case errors.Is(err, session.ErrPendingInteraction):
		return s.refuse(cliOpCodePendingInteraction, err.Error(), "", key)
	}
	fmt.Fprintf(s.stderr, "gc session submit: %v\n", err) //nolint:errcheck // best-effort stderr
	return 1
}

// refuseResume reports the session's refusal of the message's resume, and
// what became of the op: none was written, the op this run wrote was
// canceled, may still deliver as its cancel did not close it, or stands as
// an earlier request's.
func (s *sessionSubmitLane) refuseResume(e *lanesubmit.ResumeError, key string) int {
	if e.Op == nil {
		return s.refuse(cliOpCodeResumeRefused, fmt.Sprintf("resuming %s: %v; the message did not run", s.target, e.Err), "", key)
	}
	op := e.Op
	var what string
	switch {
	case e.Canceled():
		what = fmt.Sprintf("op %s, which this run wrote, was canceled: rerun without --key to send the message again", op.ID)
	case e.Owned:
		what = fmt.Sprintf("op %s, which this run wrote, may still deliver the message, as its cancel did not close it: rerun with --key %s to see what becomes of it", op.ID, op.ClientKey)
	default:
		what = fmt.Sprintf("op %s stands, as an earlier request accepted it: rerun with --key %s to see what becomes of it", op.ID, op.ClientKey)
	}
	return s.refuse(cliOpCodeResumeRefused, fmt.Sprintf("resuming %s for op %s: %v; %s", s.target, op.ID, e.Err, what), op.ID, op.ClientKey)
}

// unavailable is the refusal of a message whose op cannot be written or
// read.
func (s *sessionSubmitLane) unavailable(cause any) string {
	return fmt.Sprintf("op journal cannot record this request (%v); the message did not run", cause)
}

// refuse reports a refusal on stderr, and under --json as the failure
// envelope, through the printer gc session submit's legacy path and gc
// session nudge's lane refusals use (cliOp.refuse). It returns the exit
// code, 1.
func (s *sessionSubmitLane) refuse(code, message, opID, key string) int {
	(&cliOp{req: cliOpRequest{verb: "gc session submit", kind: opjournal.KindMessage, asJSON: s.asJSON, stdout: s.stdout, stderr: s.stderr}}).refuse(code, message, opID, key)
	return 1
}

func (s *sessionSubmitLane) warnf(format string, args ...any) {
	fmt.Fprintf(s.stderr, "gc session submit: warning: "+format+"\n", args...) //nolint:errcheck // best-effort stderr
}
