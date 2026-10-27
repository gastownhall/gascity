package main

// Restart ops (OJ-6; DES-O0API §1, D11; COJ §3.5, §3.7): `gc runtime
// request-restart` and a restartable self `gc handoff` record their request
// as operation-journal ops beside SG-3's row-first legacy execution.
//
// request-restart records R = restart{fresh} under ops.lifecycle. A self
// handoff of a session the controller restarts records R, then N =
// message{notice} to self under ops.content: N rides R, proposed only once R
// is recorded and executing, keyed <R.ID>:notice with causation R.ID. Both
// are shadow records executed by legacy:cli, so they are two admissions,
// restart first, and no guarded batch: the legacy effects (the mail bead, the
// row marker) can never join one. N's scope is context(E+1), E being the
// row's continuation_epoch, and it declares no after while R is shadow: an
// after on an op the CLI closes at acceptance says nothing (DES-O1 §13).
//
// N closes succeeded{via: legacy.queued} once the handoff mail lands, before
// the row write; R closes from the row write, before the runtime hint and the
// poke. The row write is itself the request, so a controller pass that acts
// on it first (a patrol tick, a ledger-feed wake) can stop this process
// before R closes, leaving R to the op sweeper.
// R's key is --key, else minted; there is no (generation, purpose) key, since
// a relaunch in place runs a second agent process under one generation and
// token. `gc handoff --auto` (a hook path), `--target` and a self handoff of a
// session the controller does not restart record nothing; the last warns that
// its --key is not recorded.
//
// Because N rides R, ops.content=require does not require N on its own: with
// ops.lifecycle off, or R run unrecorded, N is neither checked nor proposed,
// and the handoff runs as it does unrecorded. Once R is recorded, a refusal
// of N closes R failed{refused:notice_refused}, and nothing runs. N stays a
// legacy:cli shadow message op even where [ops].authoritative lists message:
// it is an exception to that setting until the notice gets a lane producer.

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/session"
)

// Refusal reasons a restart op closes with, after refused:.
const (
	// cliOpRefusedRowContended: the session row moved under every attempt
	// to record the restart.
	cliOpRefusedRowContended = "row_contended"
	// cliOpRefusedNoInstanceToken: the request named no incarnation.
	cliOpRefusedNoInstanceToken = "no_instance_token"
	// cliOpRefusedHandoffMailFailed: the handoff mail was not created, so
	// the restart was not requested.
	cliOpRefusedHandoffMailFailed = "handoff_mail_failed"
	// cliOpRefusedNoticeRefused: the journal refused the handoff's notice op,
	// so nothing ran.
	cliOpRefusedNoticeRefused = "notice_refused"
)

// handoffNoticeKeySuffix derives the handoff notice's key from its restart
// op's id, as deliver-now derives <message>:interrupt.
const handoffNoticeKeySuffix = ":notice"

// restartOpPolicy is the restart op's policy: a fresh restart, as `gc
// session reset` records.
var restartOpPolicy = opjournal.Policy{Restart: opjournal.RestartPolicy{Mode: opjournal.RestartFresh}}

// errHandoffMailFailed closes a handoff's notice op when its mail was not
// created; createHandoffMail reported why.
var errHandoffMailFailed = errors.New("the handoff mail was not created")

// restartOps is one in-session restart request's ops: R, and for a handoff N.
// A nil *restartOps records nothing; every method is nil-safe.
type restartOps struct {
	cfg       *config.City
	cfgErr    error
	sessStore beads.Store
	// key is --key; empty mints one.
	key string
	// conversation is the session row's id, resolved by name; unrecordable
	// and unrecordableErr say why it did not resolve (cliOpRequest).
	conversation    string
	unrecordable    string
	unrecordableErr error
	// epoch is the row's continuation_epoch (1 when unset or not positive).
	epoch int64
	// sender is the session's address, which the notice names.
	sender  string
	asJSON  bool
	jsonOut io.Writer
	stderr  io.Writer

	restart, notice *cliOp
	// replayedNotice is the id of the notice op a replayed R's first run
	// recorded, read only for a --json result (recordedNoticeID).
	replayedNotice string
}

// newRestartOps resolves sessionName's row on sessStore for a restart
// request's ops. A row that does not resolve leaves the request unrecordable:
// auto runs it unrecorded, require refuses it.
func newRestartOps(cfg *config.City, cfgErr error, sessStore beads.Store, key, sessionName, sender string, asJSON bool, jsonOut, stderr io.Writer) *restartOps {
	o := &restartOps{
		cfg: cfg, cfgErr: cfgErr, sessStore: sessStore, key: key, sender: sender,
		epoch: session.DefaultContinuationEpoch, asJSON: asJSON, jsonOut: jsonOut, stderr: stderr,
	}
	if sessStore == nil {
		o.unrecordable = legacyop.ReasonStoreUnavailable
		return o
	}
	id, err := resolveSessionID(sessStore, sessionName)
	switch {
	case errors.Is(err, session.ErrSessionNotFound):
		o.unrecordable, o.unrecordableErr = legacyop.ReasonUnmaterializedTarget, err
		return o
	case err != nil:
		o.unrecordable, o.unrecordableErr = legacyop.ReasonUnreadableTarget, err
		return o
	}
	b, err := sessStore.Get(id)
	if err != nil {
		o.unrecordable, o.unrecordableErr = legacyop.ReasonUnreadableTarget, err
		return o
	}
	o.conversation = id
	if e, err := strconv.ParseInt(strings.TrimSpace(b.Metadata["continuation_epoch"]), 10, 64); err == nil && e > 0 {
		o.epoch = e
	}
	return o
}

// request is the base request of one of the ops.
func (o *restartOps) request(verb string, kind opjournal.Kind) cliOpRequest {
	return cliOpRequest{
		verb: verb, kind: kind, conversation: o.conversation,
		unrecordable: o.unrecordable, unrecordableErr: o.unrecordableErr,
		cfgErr: o.cfgErr, asJSON: o.asJSON, stdout: o.jsonOut, stderr: o.stderr,
	}
}

// restartRequest is R's request.
func (o *restartOps) restartRequest(verb string) cliOpRequest {
	req := o.request(verb, opjournal.KindRestart)
	req.key, req.policy = o.key, restartOpPolicy
	return req
}

// noticeRequest is N's request once R is recorded: a notice to self under
// <R.ID>:notice with causation R.ID, scoped to the context the fresh restart
// opens, with no after.
//
// R's request already warned of a city config that did not load, so N's
// does not warn again, and R names the run in its one --json record, so N
// reports on stderr only.
func (o *restartOps) noticeRequest(verb string) cliOpRequest {
	req := o.request(verb, opjournal.KindMessage)
	req.cfgErr, req.asJSON = nil, false
	req.payload, req.policy = mailNotifyText(o.sender), mailNotifyOpPolicy
	req.scope = opjournal.Scope{Kind: opjournal.ScopeContext, Epoch: o.epoch + 1}
	req.presetKey, req.causation = o.noticeKey(), o.restart.adm.Op.ID
	return req
}

// noticeKey is N's key, derived from R's op id.
func (o *restartOps) noticeKey() string {
	return o.restart.adm.Op.ID + handoffNoticeKeySuffix
}

// startRestart starts R. It reports every refusal itself.
func (o *restartOps) startRestart(verb string) cliOpStart {
	if o == nil {
		return cliOpExecute
	}
	var start cliOpStart
	o.restart, start = startCLIOp(o.cfg, o.sessStore, o.restartRequest(verb))
	return start
}

// startHandoff starts R and then, only when R is recorded and executing,
// starts N: nothing about N is checked before, so N's gate never refuses a
// handoff whose R is not recorded. A refusal of N closes R (refuseNotice).
func (o *restartOps) startHandoff(verb string) cliOpStart {
	if o == nil {
		return cliOpExecute
	}
	start := o.startRestart(verb)
	if start == cliOpReplayed && o.asJSON {
		o.replayedNotice = o.recordedNoticeID(verb)
	}
	if start != cliOpExecute || !o.restart.recordedExecution() {
		return start
	}
	notice := newCLIOp(o.cfg, o.sessStore, o.noticeRequest(verb))
	switch notice.start() {
	case cliOpRefused:
		o.refuseNotice()
		return cliOpRefused
	case cliOpExecute:
		o.notice = notice
	}
	return cliOpExecute
}

// refuseNotice closes R failed{refused:notice_refused} once the journal has
// refused N, which reported why: nothing ran. It names R and R's key, on
// stderr and in the run's one --json record, as end names a recorded R that
// failed.
func (o *restartOps) refuseNotice() {
	r := o.restart
	r.finishOutcome(cliOpRefusedOutcome(cliOpRefusedNoticeRefused))
	rerun := "rerun to act again"
	if o.key != "" {
		rerun = "rerun without --key to act again"
	}
	message := fmt.Sprintf("op %s (key %s): %s: the journal refused the handoff's notice op, so nothing ran; %s",
		r.adm.Op.ID, r.adm.Key, cliOpOutcomeText(*r.closedWith), rerun)
	fmt.Fprintf(o.stderr, "%s: %s\n", r.req.verb, message) //nolint:errcheck // best-effort stderr
	r.writeFailure(cliOpCodeFailed, message, r.adm.Op.ID, r.adm.Key)
}

// recordedNoticeID is the id of the notice op a replayed R's first run
// recorded under <R.ID>:notice, read through R's op store; "" when that run
// recorded none. A read that fails is warned of, and the --json result then
// leaves the notice out.
func (o *restartOps) recordedNoticeID(verb string) string {
	st, err := o.restart.rec.Open()
	if err != nil {
		o.warnNoticeUnread(verb, err)
		return ""
	}
	principal, _ := o.restart.principal()
	id, err := opjournal.DeriveID(st.Prefix(), o.conversation, principal, o.noticeKey())
	if err == nil {
		_, err = st.Get(id)
	}
	switch {
	case err == nil:
		return id
	case !errors.Is(err, beads.ErrNotFound):
		o.warnNoticeUnread(verb, err)
	}
	return ""
}

// warnNoticeUnread warns that a replay's --json result leaves out the notice
// op its first run recorded, as reading it failed with err.
func (o *restartOps) warnNoticeUnread(verb string, err error) {
	fmt.Fprintf(o.stderr, "%s: warning: reading the first run's notice op: %v; the JSON result leaves out notice_op_id\n", verb, err) //nolint:errcheck // best-effort stderr
}

// mailSent closes N from the handoff mail's result, and R too when the mail
// failed.
func (o *restartOps) mailSent(err error) {
	if o == nil {
		return
	}
	o.notice.finishMessage(err == nil, err)
	if err != nil {
		o.restart.finishOutcome(cliOpRefusedOutcome(cliOpRefusedHandoffMailFailed))
	}
}

// finishRestart closes R from the row write's result.
func (o *restartOps) finishRestart(err error) {
	if o == nil {
		return
	}
	o.restart.finish(restartOpOutcome(err))
}

// end closes whatever the command left open as failed{other}; defer it after
// starting the ops. A run writes one --json record: N fails only beside R,
// whose record names the run's op, so N's failure goes to stderr only
// (noticeRequest).
func (o *restartOps) end() {
	if o == nil {
		return
	}
	o.notice.end()
	o.restart.end()
}

// unrecordedKey warns that --key is not recorded on a self handoff of a
// session the controller does not restart, which records no ops.
func (o *restartOps) unrecordedKey(verb string) {
	if o == nil || o.key == "" {
		return
	}
	fmt.Fprintf(o.stderr, "%s: warning: --key not recorded (the controller does not restart this session, so the handoff records no ops); a rerun sends the mail again\n", verb) //nolint:errcheck // best-effort stderr
}

// handoffOpFields are the op fields of a handoff's JSON result.
type handoffOpFields struct {
	opID, noticeOpID, key string
	replayed              bool
}

// jsonFields are the ops' JSON fields, zero when no op backs the run.
func (o *restartOps) jsonFields() handoffOpFields {
	if o == nil {
		return handoffOpFields{}
	}
	var f handoffOpFields
	f.opID, f.key, f.replayed = o.restart.jsonFields()
	f.noticeOpID, _, _ = o.notice.jsonFields()
	if f.replayed {
		f.noticeOpID = o.replayedNotice
	}
	return f
}

// restartOpOutcome is R's outcome from the row write's result (§3.6): a
// recorded or coalesced request succeeded{via: legacy}; a row of another
// incarnation skipped{stale}; a closed row, a contended one and a request
// with no incarnation failed{refused:*}; anything else as the session verbs
// map it.
func restartOpOutcome(err error) legacyop.Result {
	switch {
	case errors.Is(err, session.ErrStaleRestartRequest):
		return legacyop.Result{Outcome: opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}}
	case errors.Is(err, session.ErrSessionClosed):
		return legacyop.Result{Outcome: cliOpRefusedOutcome(cliOpRefusedSessionClosed)}
	case errors.Is(err, session.ErrRestartRequestContended):
		return legacyop.Result{Outcome: cliOpRefusedOutcome(cliOpRefusedRowContended)}
	case errors.Is(err, session.ErrRestartRequestUnfenced):
		return legacyop.Result{Outcome: cliOpRefusedOutcome(cliOpRefusedNoInstanceToken)}
	}
	return sessionVerbSentinels.SyncResult(opjournal.KindRestart, err, true)
}

// handoffReplayNote notes on stderr that a replayed handoff sends no mail.
func handoffReplayNote(verb string, stderr io.Writer) {
	fmt.Fprintf(stderr, "%s: the first run's mail is not sent again\n", verb) //nolint:errcheck // best-effort stderr
}
