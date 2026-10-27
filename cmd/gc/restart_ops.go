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
// R closes from the row write, before the runtime hint and the poke; N closes
// succeeded{via: legacy.queued} once the handoff mail lands, before the row
// write. Both close before the restart they request can stop this process.
// R's key is --key, else minted; there is no (generation, purpose) key, since
// a relaunch in place runs a second agent process under one generation and
// token. `gc handoff --auto` (a hook path), `--target` and a self handoff of a
// session the controller does not restart record nothing.

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

// noticeRequest is N's request before R is known: a notice to self, scoped
// to the context the fresh restart opens, with no after.
//
// R's request already warned of a city config that did not load, so N's
// does not warn again.
func (o *restartOps) noticeRequest(verb string) cliOpRequest {
	req := o.request(verb, opjournal.KindMessage)
	req.cfgErr = nil
	req.payload, req.policy = mailNotifyText(o.sender), mailNotifyOpPolicy
	req.scope = opjournal.Scope{Kind: opjournal.ScopeContext, Epoch: o.epoch + 1}
	return req
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

// startHandoff checks N, starts R, and then, when R is recorded and
// executing, starts N under <R.ID>:notice. A refusal of N after R started
// closes R failed{refused:notice_refused}: nothing ran.
func (o *restartOps) startHandoff(verb string) cliOpStart {
	if o == nil {
		return cliOpExecute
	}
	notice := newCLIOp(o.cfg, o.sessStore, o.noticeRequest(verb))
	if !notice.preflight() {
		return cliOpRefused
	}
	if start := o.startRestart(verb); start != cliOpExecute {
		return start
	}
	if !o.restart.recordedExecution() {
		return cliOpExecute
	}
	notice.req.presetKey = o.restart.adm.Op.ID + handoffNoticeKeySuffix
	notice.req.causation = o.restart.adm.Op.ID
	switch notice.start() {
	case cliOpRefused:
		o.restart.finishOutcome(cliOpRefusedOutcome(cliOpRefusedNoticeRefused))
		return cliOpRefused
	case cliOpExecute:
		o.notice = notice
	}
	return cliOpExecute
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
// starting the ops.
func (o *restartOps) end() {
	if o == nil {
		return
	}
	o.notice.end()
	o.restart.end()
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
