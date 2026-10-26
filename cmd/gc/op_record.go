package main

// CLI shadow ops (DES-O0API §8, OJ-3): the glue that records a CLI session
// verb as an operation-journal op beside its legacy execution.
//
// A verb resolves its target, then starts its op (startCLIOp) before its first
// legacy write and finishes it from the legacy result. The op is an acceptance
// record executed by legacy:cli: the CLI never adopts an op (it cannot tell
// whether another CLI process is still running one), so every CLI op closes,
// and an op this process cannot close is left to the controller's op sweeper,
// which closes it past finish_by. The sweeper latches the ops gates at the
// controller's start, and with every one off it stops once a pass leaves no
// work, so such an op may wait for the controller's next start; gc doctor
// reports it until then. The gate is resolved once per process from the
// loaded city config, as every rollout gate is; with it off nothing is
// recorded and no store is opened.
//
// The content verbs (OJ-5) record message ops under ops.content: gc session
// nudge and a mail notify under the caller's principal, the notify keyed by
// its cause (mail:<message id>), and gc session submit under the operator on
// its API and local paths alike. Submit mints its key before it calls the
// API, which records the op as legacy:api; a fallback after a lost answer
// proposes that op id locally, so it replays a closed op and reports an open
// one as accepted rather than delivering again. A queued delivery closes its
// op succeeded{via: legacy.queued} at enqueue (§8.5). The delivered text is
// the legacy path's in every mode (I11).
//
// Output (DES-O0API §8.3): stdout stays the command's own; JSON results gain
// op_id, key and replayed when an op backs the run. The key, minted when --key
// is absent, is printed only in JSON and on failures, never on a successful
// plain-text run, except submit's report that the API accepted the message,
// which names the key a rerun reports its outcome under. A replayed success
// prints the command's success output rebuilt from the op, so what only the
// first run saw is absent from its JSON (close's and wake's
// wait_nudges_withdrawn, suspend's mode), and names the op on stderr, with any
// execution option it was given that the op's digest leaves out (kill's
// --force) noted as not reapplied; a refusal, and a recorded run that fails,
// names the op and the key on stderr and, under --json, in the failure
// envelope.
//
// A replay finds its op only once the target resolves to the row the op
// names. A closed session's alias no longer resolves to it, so a same-key
// rerun of a close (or of a reset that rolled back) must name the session by
// its ID: by the alias it finds no session, or, once another session holds
// the alias (a configured named session is created again under its name), it
// acts on that one. A run that named its target by alias names the ID on
// stderr. A wake of a closed row named by its ID is refused before its op is
// proposed, as front-door refusals are, so it leaves its key free for the wake
// of the configured named session by alias, which reopens that row. A key that
// already names an op on that row (a wake the row has closed since) is
// answered from that op instead, as by the alias: a same-key retry by the ID
// replays that wake.
//
// A kill's op is scoped to the session generation the row names before the
// runtime lease. Under the lease, a recorded kill refuses a row that has moved
// to another generation since (a start ran while it waited for the lease):
// it stops only the run its op names. A same-key rerun after the session
// restarted on its own row as a later generation answers from that earlier
// op, noting the restart. A pool seat the reconciler replaced is a new session
// on a new row, though it may keep the killed seat's alias, so a same-key
// rerun of that kill must name the killed session by its ID: by the alias, it
// would kill the replacement.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

// Failure-envelope codes a CLI op refusal reports.
const (
	cliOpCodeInProgress    = "op_in_progress"
	cliOpCodeAlreadyFailed = "op_already_failed"
	cliOpCodeUnavailable   = "op_journal_unavailable"
	cliOpCodeInvalidKey    = "invalid_key"
	cliOpCodeKeyMismatch   = "idempotency_mismatch"
	// cliOpCodeFailed is a recorded run whose legacy path failed.
	cliOpCodeFailed = "op_failed"
)

// Refusal reasons the CLI verbs close an op with, after refused:.
const (
	cliOpRefusedWillNotStart    = "will_not_start"
	cliOpRefusedSessionStarting = "session_starting"
	cliOpRefusedSessionInactive = "session_inactive"
	cliOpRefusedSessionClosed   = "session_closed"
	// cliOpRefusedWakeConflict stands in for a wake conflict state that
	// leaves no token characters.
	cliOpRefusedWakeConflict = "wake_conflict"
	// cliOpRefusedTargetMoved closes an op admitted for a target the verb
	// did not end up acting on: a row a wake's resolution did not return, or
	// a generation a start moved a kill's session past.
	cliOpRefusedTargetMoved = "target_moved"
)

// The --key flag's help text, around a verb's note on what its replay
// reports (addOpKeyFlag), and the verbs' notes.
const (
	cliOpKeyUsage    = "idempotency key: while the op journal records this command (its [ops] gate is auto or require), rerunning with the same key reports the first run's outcome instead of acting again"
	cliOpKeyOffUsage = "; ignored when the gate is off (default: a new key each run)"
	// A replay rebuilds a close's or wake's result, and a suspend's, from
	// the op, which holds neither the withdrawn count nor the mode.
	cliOpKeyNoWithdrawnNote = " (without the first run's wait_nudges_withdrawn in --json)"
	cliOpKeyNoModeNote      = " (without the first run's mode in --json)"
	cliOpKeyRestartedNote   = ", even after the session restarted under the same ID"
	// cliOpSweptNote says what becomes of an op this run leaves open.
	cliOpSweptNote = "the controller's op sweeper closes it past its deadline; one that started with every ops gate off may not sweep again until its next start, and gc doctor reports the op until then"
)

// addOpKeyFlag adds --key to a recorded CLI verb. replayNote, when set,
// follows the replay's description in the flag's help.
func addOpKeyFlag(cmd *cobra.Command, key *string, replayNote string) {
	cmd.Flags().StringVar(key, "key", "", cliOpKeyUsage+replayNote+cliOpKeyOffUsage)
}

// cliOpenOpStore opens the op store over the CLI's sessions-class store. A
// var so tests can pin the id prefix of a store that reports none.
var cliOpenOpStore = func(sessStore beads.Store) (*opjournal.Store, error) {
	return opjournal.NewStore(beads.SessionStore{Store: sessStore}, cliOpClock)
}

// cliOpenOpStoreUnder is cliOpenOpStore for a gate that is auto only by its
// builtin default when builtinAuto is set: such a gate records only over a
// SQLite sessions store, so over any other it answers
// *opjournal.NotRecordedByDefaultError without opening or probing anything.
func cliOpenOpStoreUnder(builtinAuto bool, sessStore beads.Store) (*opjournal.Store, error) {
	if builtinAuto {
		if err := opjournal.CheckRecordsByDefault(beads.SessionStore{Store: sessStore}); err != nil {
			return nil, err
		}
	}
	return cliOpenOpStore(sessStore)
}

// cliOpClock is the clock CLI ops are proposed and closed under. A var so
// tests can move past an op's finish_by.
var cliOpClock clock.Clock = clock.Real{}

// cliOpLookupEnv reads the environment the CLI's principal and gates come
// from. A var so tests inject a map instead of mutating the process env.
var cliOpLookupEnv = os.LookupEnv

// sessionVerbSentinels maps the session lifecycle verbs' typed errors onto op
// outcomes (DES-O0API §7.2). The CLI closes every op, so a retryable error
// closes with its refused:* reason.
var sessionVerbSentinels = legacyop.Sentinels{
	Unsupported: []error{session.ErrTurnInterruptUnsupported, session.ErrInteractionUnsupported},
	Stale:       []error{session.ErrNoPendingInteraction, session.ErrInteractionMismatch},
	NotNeeded:   []error{session.ErrSessionClosed, session.ErrSessionInactive},
	NotASession: []error{session.ErrNotSession, session.ErrNotSessionBead},
	ProviderUnavailable: []error{
		runtime.ErrRuntimeUnavailable,
	},
	Refusals: []legacyop.Refusal{
		{Err: session.ErrSessionStarting, Why: cliOpRefusedSessionStarting},
		{Err: session.ErrRuntimeLeaseBusy, Why: cliOpRefusedSessionStarting},
		{Err: session.ErrSessionStopping, Why: "session_stopping"},
		{Err: session.ErrSessionKillPending, Why: "kill_pending"},
		{Err: session.ErrIllegalTransition, Why: "illegal_transition"},
	},
}

// cliOpRequest is one CLI verb's request to record.
type cliOpRequest struct {
	// verb prefixes the command's stderr lines ("gc session wake").
	verb string
	kind opjournal.Kind
	// key is --key; empty mints one.
	key string
	// conversation is the resolved session id.
	conversation string
	scope        opjournal.Scope
	policy       opjournal.Policy
	// unrecordable, when set, is the legacyop.Reason* the request cannot be
	// recorded for (a session with no generation, or whose row did not
	// read): auto runs it unrecorded, require refuses it. unrecordableErr is
	// the error behind it, if any.
	unrecordable    string
	unrecordableErr error
	// cfgErr is the city config's load error, when it did not load: the
	// config's [ops] gates were not read.
	cfgErr error
	// unreplayed names the execution options outside the op's digest this
	// run was given (kill's --force); a replay says they were not reapplied.
	unreplayed []string
	// payload is a message op's text and causation the id of what caused it
	// (a mail notify's message); both are empty for the lifecycle verbs.
	payload   string
	causation string
	// asOperator records the request under the operator principal whoever
	// runs it: gc session submit is an operator verb on its API and local
	// paths alike, so both derive one op id (DES-O0API §3.3).
	asOperator bool
	// presetKey is a key the command minted itself, used when key (--key) is
	// empty: submit mints its key before it calls the API, so its local
	// fallback proposes the op id the API would have recorded.
	presetKey string
	// acceptsAPIOp answers an open legacy:api op under the key as the API
	// having accepted the request (cliOpAcceptedByAPI) instead of
	// op_in_progress, and admits again, which replays it, one the API closed
	// after admission read it open: submit's local fallback after an API call
	// whose answer was lost.
	acceptsAPIOp bool
	asJSON       bool
	stdout       io.Writer
	stderr       io.Writer
}

// unrecordableCause is why the request cannot be recorded: its reason, with
// the error behind it when there is one.
func (r cliOpRequest) unrecordableCause() string {
	if r.unrecordableErr == nil {
		return r.unrecordable
	}
	return fmt.Sprintf("%s: %v", r.unrecordable, r.unrecordableErr)
}

// cliOpStart is what startCLIOp decided.
type cliOpStart int

const (
	// cliOpExecute runs the legacy path; finish the op from its result.
	cliOpExecute cliOpStart = iota + 1
	// cliOpReplayed answers a closed, successful op: print the command's
	// success output (annotated) and exit 0 without acting.
	cliOpReplayed
	// cliOpRefused reported a refusal; the command exits 1 without acting.
	cliOpRefused
	// cliOpAcceptedByAPI found the request's op open under the API's
	// executor (cliOp.apiOp): the API accepted it and runs it, so the
	// command reports that and exits 0 without acting.
	cliOpAcceptedByAPI
)

// cliOp is one CLI verb's op.
type cliOp struct {
	req cliOpRequest
	rec *legacyop.Recorder
	// sessStore is the sessions-class store the op store opens over. A
	// command that resolves its gate before it opens its stores (submit)
	// binds it later, before it starts the op.
	sessStore beads.Store
	// opened is the op store the recorder opened first, and openErr the
	// error that open failed with: every later open reuses the one answer.
	opened  *opjournal.Store
	openErr error
	adm     *legacyop.Admission
	warned  map[string]bool
	// closedWith is the outcome finish closed the op with; nil until then.
	closedWith *opjournal.Outcome
	// restartedAs is the generation a kill's session restarted as since the
	// earlier kill its key names; 0 when the op is not that earlier kill.
	restartedAs int64
	// apiOp is the open legacy:api op a cliOpAcceptedByAPI start found.
	apiOp opjournal.Op
}

// newCLIOp resolves req's gate from cfg and builds its recorder over the op
// store opened lazily on sessStore. It records nothing yet.
func newCLIOp(cfg *config.City, sessStore beads.Store, req cliOpRequest) *cliOp {
	op := &cliOp{req: req, sessStore: sessStore}
	mode, builtinAuto := op.mode(cfg)
	op.rec = &legacyop.Recorder{
		Mode:     mode,
		Executor: legacyop.ExecutorCLI,
		Open: func() (*opjournal.Store, error) {
			if op.opened == nil && op.openErr == nil {
				op.opened, op.openErr = cliOpenOpStoreUnder(builtinAuto, op.sessStore)
			}
			return op.opened, op.openErr
		},
		Clock:    cliOpClock,
		Degraded: op.warnDegrade,
		// The CLI prints its own one-line warning per stage (warnDegrade) and
		// reports a late or unrecordable close itself (finish).
		Logger: slog.New(slog.DiscardHandler),

		// Under a gate that is auto only by its builtin default, a store that
		// cannot record ops at all (a bd without --if-revision, a store with
		// no id prefix), or one the default does not record on (not
		// SQLite-backed, cliOpenOpStoreUnder), runs the request unrecorded
		// without a warning, as if the gate were off: a warning there would
		// print on every recorded command of a default city. A run that
		// asked for a --key still warns, as it does not get the dedup it
		// asked for.
		QuietCannotRecord: builtinAuto && req.key == "",
		CrashPoints:       opsFailpointState.hook(legacyop.SurfaceCLI),
	}
	return op
}

// startCLIOp records req, resolved under cfg, over sessStore (newCLIOp, then
// start). It reports every refusal itself.
func startCLIOp(cfg *config.City, sessStore beads.Store, req cliOpRequest) (*cliOp, cliOpStart) {
	op := newCLIOp(cfg, sessStore, req)
	return op, op.start()
}

// cliCityConfig is the city config a session verb runs under, with the error
// its op's request carries as cfgErr when there is no config: cityErr when the
// city did not resolve, so the verb says its [ops] gates were not read, else
// loadCityConfig's own. Config warnings go to warn.
func cliCityConfig(cityPath string, cityErr error, warn io.Writer) (*config.City, error) {
	if cityErr != nil {
		return nil, cityErr
	}
	return loadCityConfig(cityPath, warn)
}

// preflight opens the op store and checks the request's key and payload ahead
// of a verb's resolution, which may itself write (wake materializes a named
// session's row): under require a store that cannot record, and then an
// invalid key or a payload the journal refuses whatever the conversation (too
// large, not valid UTF-8, or empty: legacyop.Recorder.Check), refuse before
// anything is written. A refusal only proposing the op can
// raise, such as a journal write that fails, still comes after that write
// (admitWake). The refusal goes through the recorder, which counts (and
// degrades) it as it does at admission; preflight reports false after
// reporting the refusal. A request already known to be unrecordable
// (cliOpRequest.unrecordable) is refused here too, as start would refuse it.
// Other modes check nothing here.
func (o *cliOp) preflight() bool {
	if o.rec.Mode != rollout.Require {
		return true
	}
	if o.req.unrecordable != "" {
		o.refuseUnrecordable()
		return false
	}
	if _, err := o.rec.Open(); err != nil {
		_, aerr := o.rec.Admit(legacyop.Request{Key: o.requestKey(), Kind: o.req.kind})
		if !errors.Is(aerr, legacyop.ErrUnavailable) {
			aerr = err
		}
		o.refuseAdmit(aerr)
		return false
	}
	if err := o.rec.Check(legacyop.Request{Key: o.requestKey(), Payload: o.req.payload, Kind: o.req.kind}); err != nil {
		o.refuseAdmit(err)
		return false
	}
	return true
}

// unavailableMessage is the require refusal of a request the journal cannot
// record.
func (o *cliOp) unavailableMessage(cause any) string {
	return fmt.Sprintf("op journal cannot record this request (%v); the %s did not run", cause, o.req.kind)
}

// refuseUnrecordable counts and reports the require refusal of a request
// known to be unrecordable (cliOpRequest.unrecordable).
func (o *cliOp) refuseUnrecordable() {
	o.rec.Refused(o.req.kind, o.req.unrecordable)
	o.refuse(cliOpCodeUnavailable, o.unavailableMessage(o.req.unrecordableCause()), "", o.requestKey())
}

// start records the op. It reports every refusal itself.
func (o *cliOp) start() cliOpStart {
	req := o.req
	rec := o.rec
	if req.unrecordable != "" && rec.Mode == rollout.Require {
		o.refuseUnrecordable()
		return cliOpRefused
	}
	if req.unrecordable != "" {
		if req.key != "" && rec.Mode == rollout.Auto {
			fmt.Fprintf(req.stderr, "%s: warning: --key not recorded (%s); running unrecorded, so a rerun acts again\n", req.verb, req.unrecordableCause()) //nolint:errcheck // best-effort stderr
		}
		o.adm = rec.Unrecorded(req.kind, o.requestKey(), req.unrecordable)
		return cliOpExecute
	}
	principal, actor := o.principal()
	o.rescopeRestartedKill(principal)
	areq := legacyop.Request{
		Conversation: req.conversation,
		Principal:    principal,
		Actor:        actor,
		Key:          o.requestKey(),
		Payload:      req.payload,
		Causation:    req.causation,
		Kind:         req.kind,
		Policy:       req.policy,
		Scope:        o.req.scope,
	}
	adm, err := rec.Admit(areq)
	if err != nil {
		switch o.apiOpState(err) {
		case cliAPIOpOpen:
			return cliOpAcceptedByAPI
		case cliAPIOpClosed:
			// A closed op does not reopen, so admitting again replays it.
			adm, err = rec.Admit(areq)
		}
	}
	if err != nil {
		o.refuseAdmit(err)
		return cliOpRefused
	}
	o.adm = adm
	if adm.Decision != legacyop.Replay {
		o.warnUnrecordedKey(adm)
		return cliOpExecute
	}
	if cliOpReplaysAsSuccess(adm.Op.Kind, adm.Op.Outcome) {
		note := "replayed op " + adm.Op.ID + o.restartNote()
		if len(req.unreplayed) > 0 {
			note += fmt.Sprintf("; %s was not reapplied (outside the op's request)", strings.Join(req.unreplayed, ", "))
		}
		fmt.Fprintf(req.stderr, "%s: %s\n", req.verb, note) //nolint:errcheck // best-effort stderr
		return cliOpReplayed
	}
	rerun := "rerun to act again"
	if req.key != "" {
		rerun = "rerun without --key to act again"
	}
	o.refuse(cliOpCodeAlreadyFailed,
		fmt.Sprintf("op %s closed %s%s; %s", adm.Op.ID, cliOpOutcomeText(adm.Op.Outcome), o.restartNote(), rerun),
		adm.Op.ID, adm.Key)
	return cliOpRefused
}

// warnUnrecordedKey warns, in auto, that a run under a client --key the
// journal declined to record (an invalid key, or a request it rejects: a
// payload too large, not valid UTF-8, or empty) is not recorded, so a
// same-key rerun acts again. A store failure was reported by warnDegrade.
func (o *cliOp) warnUnrecordedKey(adm *legacyop.Admission) {
	req := o.req
	if adm.Recorded || req.key == "" || o.rec.Mode != rollout.Auto {
		return
	}
	if kerr := opjournal.ValidateClientKey(req.key); kerr != nil {
		fmt.Fprintf(req.stderr, "%s: warning: invalid --key (%v); running unrecorded, so a rerun acts again\n", req.verb, kerr) //nolint:errcheck // best-effort stderr
		return
	}
	if adm.Unrecordable == nil {
		return
	}
	fmt.Fprintf(req.stderr, "%s: warning: --key not recorded (%v); running unrecorded, so a rerun acts again\n", req.verb, adm.Unrecordable) //nolint:errcheck // best-effort stderr
}

// principal is the request's principal and actor: the operator for an
// operator verb (asOperator), else the caller's (cliPrincipal).
func (o *cliOp) principal() (principal, actor string) {
	if o.req.asOperator {
		return legacyop.PrincipalOperator, opjournal.ActorOperator
	}
	return cliPrincipal(cliOpLookupEnv)
}

// requestKey is the client key the op is admitted under: --key, else the key
// the command minted itself (presetKey), else empty, which the recorder mints.
func (o *cliOp) requestKey() string {
	if o.req.key != "" {
		return o.req.key
	}
	return o.req.presetKey
}

// cliAPIOp is what the op behind a busy refusal is to a request that accepts
// an open legacy:api op (acceptsAPIOp).
type cliAPIOp int

const (
	// cliAPIOpNone leaves the refusal as it is: it is not busy, the request
	// accepts no API op, or the op does not read or another executor holds
	// it.
	cliAPIOpNone cliAPIOp = iota
	// cliAPIOpOpen is the API's acceptance of the request: it runs the op.
	cliAPIOpOpen
	// cliAPIOpClosed is an op the API closed after Admit read it open.
	cliAPIOpClosed
)

// apiOpState reads the op behind err, a busy refusal, for a request that
// accepts an open legacy:api op (acceptsAPIOp), and keeps it as apiOp when
// the API holds it open.
func (o *cliOp) apiOpState(err error) cliAPIOp {
	var busy *legacyop.BusyError
	if !o.req.acceptsAPIOp || !errors.As(err, &busy) {
		return cliAPIOpNone
	}
	st, oerr := o.rec.Open()
	if oerr != nil {
		return cliAPIOpNone
	}
	op, gerr := st.Get(busy.ID)
	switch {
	case gerr != nil || op.Executor != opjournal.ExecutorLegacyAPI:
		return cliAPIOpNone
	case op.Status == opjournal.StatusClosed:
		return cliAPIOpClosed
	}
	o.apiOp = op
	return cliAPIOpOpen
}

// rescopeRestartedKill scopes a kill under a client key to the earlier kill
// that key already names when the session has since restarted as a later
// generation, so a retry of that kill answers from it instead of killing the
// new run or refusing the key as another request. Any other op under the key
// leaves the scope alone and stays the key reuse it is; so does a read that
// fails, which the admission that follows makes again and reports.
func (o *cliOp) rescopeRestartedKill(principal string) {
	req := o.req
	if req.kind != opjournal.KindKill || req.scope.Kind != opjournal.ScopeSession {
		return
	}
	prior, found, _ := o.keyedOp(req.conversation, principal)
	if !found || prior.Kind != opjournal.KindKill || prior.Scope.Kind != opjournal.ScopeSession || prior.Scope.Generation >= req.scope.Generation {
		return
	}
	o.restartedAs = req.scope.Generation
	o.req.scope = prior.Scope
}

// keyedOp reads the op the request's client --key already names on
// conversation under principal: the op its admission answers from. found is
// false when there is none: the gate records nothing, the key was minted (it
// names no earlier op), or no op is stored at the key's id. err is any other
// failure to read it, the op store's open included.
func (o *cliOp) keyedOp(conversation, principal string) (prior opjournal.Op, found bool, err error) {
	if o.req.key == "" || !o.recording() {
		return opjournal.Op{}, false, nil
	}
	st, err := o.rec.Open()
	if err != nil {
		return opjournal.Op{}, false, err
	}
	id, err := opjournal.DeriveID(st.Prefix(), conversation, principal, o.req.key)
	if err != nil {
		return opjournal.Op{}, false, err
	}
	prior, err = st.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return opjournal.Op{}, false, nil
	}
	return prior, err == nil, err
}

// restartNote notes, after a message about a rescoped kill's earlier op,
// that the session has restarted since.
func (o *cliOp) restartNote() string {
	if o.restartedAs == 0 {
		return ""
	}
	return fmt.Sprintf("; the session has since restarted as generation %d", o.restartedAs)
}

// retarget starts the op over for conversation, the target the verb now
// acts on, closing the op it admitted for another target first: the verb did
// not act on that one.
func (o *cliOp) retarget(conversation string) cliOpStart {
	if o.adm != nil {
		o.finishOutcome(cliOpRefusedOutcome(cliOpRefusedTargetMoved))
		o.adm, o.closedWith = nil, nil
	}
	o.req.conversation = conversation
	return o.start()
}

// recording reports whether the op's gate records at all (auto or require).
func (o *cliOp) recording() bool {
	return o.rec.Mode == rollout.Auto || o.rec.Mode == rollout.Require
}

// recordedExecution reports whether an op row backs the run this process
// executes: admitted to execute, and recorded.
func (o *cliOp) recordedExecution() bool {
	return o != nil && o.adm != nil && o.adm.Recorded && o.adm.Decision == legacyop.Execute
}

// killScopeMoved checks a recorded kill's session row, read under the
// runtime lease, against the generation its op is scoped to, read before the
// lease. It returns an error naming both when the row names another
// generation now: a start ran while the kill waited for the lease, so the
// runtime the kill would stop is not the run its op names. A kill that is not
// recorded, or a row that does not read with a generation, passes: the
// kill's own fresh read then decides, as it does unrecorded.
func (o *cliOp) killScopeMoved(front *session.Store, id string) error {
	if !o.recordedExecution() || o.req.scope.Kind != opjournal.ScopeSession {
		return nil
	}
	info, err := front.Get(id)
	now, unrecordable, _ := cliOpSessionScope(info, err)
	if unrecordable != "" || now.Generation == o.req.scope.Generation {
		return nil
	}
	rerun := "rerun the kill"
	if o.req.key != "" {
		rerun += " without --key"
	}
	return fmt.Errorf("session %s is at generation %d, not generation %d, which op %s was recorded for: a start ran while this kill waited for its runtime lease, so nothing was killed (%s to kill the new run)",
		id, now.Generation, o.req.scope.Generation, o.adm.Op.ID, rerun)
}

// noteReplayByID tells a recorded run under a client --key that named its
// target otherwise than by the session ID it resolved to (target) that a
// same-key rerun replays it only by that ID. Call it once the run has left
// the target's alias free (a close, a reset that rolled back): by the alias,
// a rerun acts on whatever session holds it by then, such as a configured
// named session created again under its name.
func (o *cliOp) noteReplayByID(target string) {
	if !o.recordedExecution() || o.req.key == "" || target == o.req.conversation {
		return
	}
	fmt.Fprintf(o.req.stderr, "%s: note: a rerun with --key %s replays this run only by the session ID %s: by %s it acts on whatever session holds that name then\n", o.req.verb, o.req.key, o.req.conversation, target) //nolint:errcheck // best-effort stderr
}

// mode is the gate mode of the op's kind, resolved from cfg and the env
// break-glass, and whether that mode is auto only by the gate's builtin
// default. A gate that does not resolve (a misspelled config value) is off,
// with a warning: a config typo never records, and never refuses. A config
// that did not load at all leaves each gate at its builtin default unless
// its env break-glass is set, with a warning that the config's gates were
// not read; a malformed env value is ignored with a warning.
func (o *cliOp) mode(cfg *config.City) (rollout.Mode, bool) {
	if cfg == nil {
		if o.req.cfgErr != nil {
			fmt.Fprintf(o.req.stderr, "%s: warning: op journal: city config did not load (%v); its [ops] gates were not read, so each takes its builtin default unless its environment override is set\n", o.req.verb, o.req.cfgErr) //nolint:errcheck // best-effort stderr
		}
		cfg = &config.City{}
	}
	flags, err := rollout.Resolve(cfg, rollout.ResolveOptions{LookupEnv: cliOpLookupEnv})
	if err != nil {
		fmt.Fprintf(o.req.stderr, "%s: warning: op journal: %v; not recording\n", o.req.verb, err) //nolint:errcheck // best-effort stderr
		return rollout.Off, false
	}
	spec, err := opjournal.Lookup(o.req.kind)
	if err != nil {
		fmt.Fprintf(o.req.stderr, "%s: warning: op journal: %v; not recording\n", o.req.verb, err) //nolint:errcheck // best-effort stderr
		return rollout.Off, false
	}
	key, mode := rollout.KeyOpsLifecycle, flags.OpsLifecycle()
	switch spec.Class {
	case opjournal.ClassContent:
		key, mode = rollout.KeyOpsContent, flags.OpsContent()
	case opjournal.ClassControl:
		key, mode = rollout.KeyOpsControl, flags.OpsControl()
	}
	for _, n := range flags.Notices() {
		if n.Kind == rollout.NoticeInvalidEnvIgnored && n.FlagKey == key {
			fmt.Fprintf(o.req.stderr, "%s: warning: op journal: %s\n", o.req.verb, n.Message) //nolint:errcheck // best-effort stderr
		}
	}
	return mode, flags.BuiltinAuto(key)
}

// cliPrincipal is the CLI's principal and actor (DES-O0API §3.3): the agent
// session's conversation when GC_SESSION_ID names a valid one, else the
// operator. An invalid GC_SESSION_ID reads as absent.
func cliPrincipal(lookup func(string) (string, bool)) (principal, actor string) {
	if id, ok := lookup("GC_SESSION_ID"); ok && opjournal.ValidateConversation(id) == nil {
		return legacyop.ConversationPrincipal(id), opjournal.ActorAgent
	}
	return legacyop.PrincipalOperator, opjournal.ActorOperator
}

// cliOpReplaysAsSuccess reports whether an op of kind closed with outcome
// replays as the command's success: it succeeded, or a kill found nothing to
// kill (a named session's, which exits 0).
func cliOpReplaysAsSuccess(kind opjournal.Kind, outcome opjournal.Outcome) bool {
	arm, reason := outcome.Effective()
	return arm == opjournal.ArmSucceeded ||
		(kind == opjournal.KindKill && arm == opjournal.ArmSkipped && reason == opjournal.ReasonNotNeeded)
}

// cliOpOutcomeText renders an outcome as arm{reason}.
func cliOpOutcomeText(o opjournal.Outcome) string {
	arm, reason := o.Effective()
	if reason == "" {
		return string(arm)
	}
	return string(arm) + "{" + reason + "}"
}

// refuseAdmit reports an Admit refusal. A store that cannot record is
// reported before an invalid key: the recorder opens the store first.
func (o *cliOp) refuseAdmit(err error) {
	var busy *legacyop.BusyError
	switch {
	case errors.As(err, &busy):
		rerun := "rerun later"
		if o.req.key != "" {
			rerun = "rerun later or without --key"
		}
		o.refuse(cliOpCodeInProgress, fmt.Sprintf("%v%s; %s", busy, o.restartNote(), rerun), busy.ID, o.requestKey())
	case errors.Is(err, opjournal.ErrKeyReuse):
		o.refuse(cliOpCodeKeyMismatch, fmt.Sprintf("the key names a different request (%v); rerun with another --key", err), cliOpReusedID(err), o.requestKey())
	case errors.Is(err, legacyop.ErrUnavailable):
		o.refuse(cliOpCodeUnavailable, o.unavailableMessage(err), "", o.requestKey())
	case o.requestKey() != "" && opjournal.ValidateClientKey(o.requestKey()) != nil:
		o.refuse(cliOpCodeInvalidKey, fmt.Sprintf("invalid --key (%v)", err), "", o.requestKey())
	default:
		o.refuse(cliOpCodeUnavailable, o.unavailableMessage(err), "", o.requestKey())
	}
}

// cliOpReusedID is the id of the op a key reuse names, when err says.
func cliOpReusedID(err error) string {
	var reuse *opjournal.KeyReuseError
	if errors.As(err, &reuse) {
		return reuse.Existing.ID
	}
	return ""
}

// cliOpFailure is the failure envelope of an op refusal: the shared
// schemas/failure.schema.json shape, plus the op and key it concerns.
type cliOpFailure struct {
	jsonSchemaErrorPayload
	OpID string `json:"op_id,omitempty"`
	Key  string `json:"key,omitempty"`
}

// refuse reports a refusal on stderr, naming the op and the key, and under
// --json as the failure envelope on stdout.
func (o *cliOp) refuse(code, message, opID, key string) {
	line := message
	if key != "" {
		line = fmt.Sprintf("%s (key %s)", message, key)
	}
	fmt.Fprintf(o.req.stderr, "%s: %s\n", o.req.verb, line) //nolint:errcheck // best-effort stderr
	o.writeFailure(code, message, opID, key)
}

// writeFailure writes the failure envelope on stdout under --json.
func (o *cliOp) writeFailure(code, message, opID, key string) {
	if !o.req.asJSON {
		return
	}
	if err := writeCLIJSONLine(o.req.stdout, cliOpFailure{
		jsonSchemaErrorPayload: jsonSchemaErrorPayload{
			SchemaVersion: "1",
			Error:         jsonSchemaErrorDetail{Code: code, Message: o.req.verb + ": " + message, ExitCode: 1},
		},
		OpID: opID,
		Key:  key,
	}); err != nil {
		fmt.Fprintf(o.req.stderr, "%s: writing JSON result: %v\n", o.req.verb, err) //nolint:errcheck // best-effort stderr
	}
}

// warnDegrade prints one warning per stage for a store failure the recorder
// degraded past. A require-mode refusal is reported by refuse instead. A
// store the builtin default treats as off never reaches it
// (QuietCannotRecord in newCLIOp).
func (o *cliOp) warnDegrade(d legacyop.Degrade) {
	if d.Mode == rollout.Require && (d.Stage == legacyop.StageOpen || d.Stage == legacyop.StagePropose) {
		return
	}
	if o.warned[d.Stage] {
		return
	}
	if o.warned == nil {
		o.warned = map[string]bool{}
	}
	o.warned[d.Stage] = true
	consequence := "running unrecorded"
	if d.Stage == legacyop.StageClose {
		consequence = cliOpSweptNote
	}
	fmt.Fprintf(o.req.stderr, "%s: warning: op journal %s failed (%s): %v; %s\n", o.req.verb, d.Stage, d.Reason, d.Err, consequence) //nolint:errcheck // best-effort stderr
}

// finish closes the op with res. It acts once; later calls do nothing. A
// close someone else got to first (the op sweeper past finish_by), or one
// the store refused as invalid, is reported; a store failure was reported
// by warnDegrade already.
func (o *cliOp) finish(res legacyop.Result) {
	if o == nil || o.adm == nil || o.closedWith != nil {
		return
	}
	outcome := res.Outcome
	o.closedWith = &outcome
	if !o.adm.Recorded || o.adm.Decision != legacyop.Execute {
		return
	}
	done := o.adm.Finish(res)
	switch {
	case done.Closed:
	case done.Late:
		fmt.Fprintf(o.req.stderr, "%s: warning: op %s was already closed (by the op sweeper past its deadline); this run's outcome %s was not recorded\n", o.req.verb, o.adm.Op.ID, cliOpOutcomeText(outcome)) //nolint:errcheck // best-effort stderr
	case !o.warned[legacyop.StageClose]:
		fmt.Fprintf(o.req.stderr, "%s: warning: op %s left open: the journal refused its outcome %s; %s\n", o.req.verb, o.adm.Op.ID, cliOpOutcomeText(outcome), cliOpSweptNote) //nolint:errcheck // best-effort stderr
	}
}

// finishErr closes the op from the legacy error err (nil is success).
func (o *cliOp) finishErr(err error) {
	if o == nil {
		return
	}
	o.finish(sessionVerbSentinels.SyncResult(o.req.kind, err, true))
}

// finishOutcome closes the op with outcome.
func (o *cliOp) finishOutcome(outcome opjournal.Outcome) {
	o.finish(legacyop.Result{Outcome: outcome})
}

// end closes an op the command left without a result as failed{other}, and
// names a recorded op that did not succeed, with its key, on stderr and in
// the --json failure envelope. Defer it after starting the op.
func (o *cliOp) end() {
	if o == nil || o.adm == nil {
		return
	}
	o.finishOutcome(opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonOther})
	if !o.adm.Recorded || o.adm.Decision != legacyop.Execute || cliOpReplaysAsSuccess(o.req.kind, *o.closedWith) {
		return
	}
	message := fmt.Sprintf("op %s (key %s): %s", o.adm.Op.ID, o.adm.Key, cliOpOutcomeText(*o.closedWith))
	fmt.Fprintf(o.req.stderr, "%s: %s\n", o.req.verb, message) //nolint:errcheck // best-effort stderr
	o.writeFailure(cliOpCodeFailed, message, o.adm.Op.ID, o.adm.Key)
}

// outcome is the replayed op's stored outcome; zero unless replayed.
func (o *cliOp) outcome() opjournal.Outcome {
	if o == nil || o.adm == nil || o.adm.Decision != legacyop.Replay {
		return opjournal.Outcome{}
	}
	return o.adm.Op.Outcome
}

// annotate adds the op's id, key and replay flag to a JSON result when an op
// backs the run.
func (o *cliOp) annotate(result *sessionActionResult) {
	result.OpID, result.Key, result.Replayed = o.jsonFields()
}

// jsonFields are the op_id, key and replayed fields of a JSON result: the
// op's id, its client key and whether the run replayed it, when an op backs
// the run, else zero.
func (o *cliOp) jsonFields() (opID, key string, replayed bool) {
	if o == nil || o.adm == nil || !o.adm.Recorded {
		return "", "", false
	}
	return o.adm.Op.ID, o.adm.Key, o.adm.Decision == legacyop.Replay
}

// replayedQueued reports whether the replayed op's message was queued rather
// than delivered (succeeded{via: legacy.queued}).
func (o *cliOp) replayedQueued() bool {
	return o.outcome().Result.Via == legacyop.ViaLegacyQueued
}

// Refusal reasons the message verbs close an op with, after refused:.
const (
	// cliOpRefusedPendingInteraction is an immediate nudge refused because
	// its session waits on a permission prompt it would answer.
	cliOpRefusedPendingInteraction = "pending_interaction"
	// cliOpRefusedACPNotOwned is an immediate nudge to an ACP session whose
	// connection this process does not own.
	cliOpRefusedACPNotOwned = "acp_connection_not_owned"
)

// cliMessageRefusal is a message verb's refusal of its request: its error
// text is the refusal's, and its op closes failed{refused:<why>}.
type cliMessageRefusal struct {
	why string
	err error
}

// Error is the refusal's text.
func (e *cliMessageRefusal) Error() string { return e.err.Error() }

// Unwrap returns the refusal's error.
func (e *cliMessageRefusal) Unwrap() error { return e.err }

// finishMessage closes a message op from its legacy result: delivered,
// queued, refused (cliMessageRefusal) or another error's outcome.
func (o *cliOp) finishMessage(queued bool, err error) {
	o.finishMessageInterrupted(queued, err, nil)
}

// finishMessageInterrupted is finishMessage for a delivery a turn fence ran
// for: interrupt is its report (nil when none ran), and an interrupt the
// runtime accepted unfenced is recorded as the API records it.
func (o *cliOp) finishMessageInterrupted(queued bool, err error, interrupt *session.TurnInterruptReport) {
	if o == nil {
		return
	}
	o.finish(cliMessageOpResult(queued, err, interrupt))
}

// cliMessageOpResult is the op result of a CLI message path's legacy result:
// a refusal (cliMessageRefusal) closes failed{refused:<why>}, anything else
// as api.MessageOpResult maps it for the API too. Message ops always close.
func cliMessageOpResult(queued bool, err error, interrupt *session.TurnInterruptReport) legacyop.Result {
	if refusal := (*cliMessageRefusal)(nil); errors.As(err, &refusal) {
		return legacyop.Result{Outcome: cliOpRefusedOutcome(refusal.why)}
	}
	return api.MessageOpResult(queued, err, interrupt)
}

// cliOpRefusedOutcome is failed{refused:<why>}.
func cliOpRefusedOutcome(why string) opjournal.Outcome {
	return opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.RefusedReasonPrefix + why}
}

// cliOpKillFailure is the outcome of a kill that exits 1. A kill's
// already-inactive session is not "nothing to do" here: only a named
// session's gone runtime exits 0 (skipped{not_needed}), so any other
// inactive or closed target closes failed and replays as the failure.
func cliOpKillFailure(err error) opjournal.Outcome {
	switch {
	case errors.Is(err, session.ErrSessionClosed):
		return cliOpRefusedOutcome(cliOpRefusedSessionClosed)
	case errors.Is(err, session.ErrSessionInactive):
		return cliOpRefusedOutcome(cliOpRefusedSessionInactive)
	}
	return sessionVerbSentinels.SyncResult(opjournal.KindKill, err, true).Outcome
}

// cliOpWakeConflictReason turns a wake conflict state into its refused
// reason token: lowercase, with anything outside [a-z0-9_] as '_', cut to
// fit a token, and wake_conflict when nothing is left.
func cliOpWakeConflictReason(state string) string {
	why := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		default:
			return '_'
		}
	}, state)
	if why == "" {
		return cliOpRefusedWakeConflict
	}
	if len(why) > opjournal.MaxRefusedWhyLen {
		why = why[:opjournal.MaxRefusedWhyLen]
	}
	return why
}

// cliOpSessionScope is the session(gen) scope of a kill, from the row's
// generation, and "" or the unrecordable reason when it has none: a row that
// did not read is unreadable_target, with its read error; one that read
// without a generation is no_generation.
func cliOpSessionScope(info session.Info, infoErr error) (opjournal.Scope, string, error) {
	if infoErr != nil {
		return opjournal.Scope{}, legacyop.ReasonUnreadableTarget, infoErr
	}
	gen, err := strconv.ParseInt(strings.TrimSpace(info.Generation), 10, 64)
	if err != nil || gen < 1 {
		return opjournal.Scope{}, legacyop.ReasonNoGeneration, nil
	}
	return opjournal.Scope{Kind: opjournal.ScopeSession, Generation: gen}, "", nil
}
