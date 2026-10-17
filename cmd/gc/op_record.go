package main

// CLI shadow ops (DES-O0API §8, OJ-3): the glue that records a CLI session
// verb as an operation-journal op beside its legacy execution.
//
// A verb resolves its target, then starts its op (startCLIOp) before its first
// legacy write and finishes it from the legacy result. The op is an acceptance
// record executed by legacy:cli: the CLI never adopts an op (it cannot tell
// whether another CLI process is still running one), so every CLI op closes,
// and an op this process cannot close is left to the controller's op sweeper,
// which closes it at finish_by. The gate is resolved once per process from
// the loaded city config, as every rollout gate is; with it off nothing is
// recorded and no store is opened.
//
// Output (DES-O0API §8.3): stdout stays the command's own; JSON results gain
// op_id, key and replayed when an op backs the run. The key, minted when
// --key is absent, is printed only in JSON and on failures, never on a
// successful plain-text run. A replayed success prints the original success
// output and names the op on stderr; a refusal names the op and the key.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

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
)

// Refusal reasons the CLI verbs close an op with, after refused:.
const (
	cliOpRefusedWillNotStart   = "will_not_start"
	cliOpRefusedSessionStarted = "session_starting"
)

// cliOpKeyFlagUsage is the --key flag's help text.
const cliOpKeyFlagUsage = "idempotency key: rerunning with the same key reports the first run's outcome instead of acting again (default: a new key each run)"

// addOpKeyFlag adds --key to a recorded CLI verb.
func addOpKeyFlag(cmd *cobra.Command, key *string) {
	cmd.Flags().StringVar(key, "key", "", cliOpKeyFlagUsage)
}

// cliOpenOpStore opens the op store over the CLI's sessions-class store. A
// var so tests can pin the id prefix of a store that reports none.
var cliOpenOpStore = func(sessStore beads.Store) (*opjournal.Store, error) {
	return opjournal.NewStore(beads.SessionStore{Store: sessStore}, cliOpClock)
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
		{Err: session.ErrSessionStarting, Why: cliOpRefusedSessionStarted},
		{Err: session.ErrRuntimeLeaseBusy, Why: cliOpRefusedSessionStarted},
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
	// recorded for (a session with no generation): auto runs it unrecorded,
	// require refuses it.
	unrecordable string
	asJSON       bool
	stdout       io.Writer
	stderr       io.Writer
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
)

// cliOp is one CLI verb's op.
type cliOp struct {
	req    cliOpRequest
	adm    *legacyop.Admission
	warned map[string]bool
}

// startCLIOp records req under its kind's gate, resolved from cfg, over the
// op store opened lazily on sessStore. It reports every refusal itself.
func startCLIOp(cfg *config.City, sessStore beads.Store, req cliOpRequest) (*cliOp, cliOpStart) {
	op := &cliOp{req: req}
	rec := &legacyop.Recorder{
		Mode:     op.mode(cfg),
		Executor: legacyop.ExecutorCLI,
		Open:     func() (*opjournal.Store, error) { return cliOpenOpStore(sessStore) },
		Clock:    cliOpClock,
		Degraded: op.warnDegrade,
		// The CLI prints its own one-line warning per stage (warnDegrade).
		Logger: slog.New(slog.DiscardHandler),
	}
	if req.unrecordable != "" && rec.Mode == rollout.Require {
		op.refuse(cliOpCodeUnavailable, fmt.Sprintf("op journal cannot record this request (%s); nothing was done", req.unrecordable), "", req.key)
		return op, cliOpRefused
	}
	if req.unrecordable != "" {
		op.adm = rec.Unrecorded(req.kind, req.key, req.unrecordable)
		return op, cliOpExecute
	}
	principal, actor := cliPrincipal(cliOpLookupEnv)
	adm, err := rec.Admit(legacyop.Request{
		Conversation: req.conversation,
		Principal:    principal,
		Actor:        actor,
		Key:          req.key,
		Kind:         req.kind,
		Policy:       req.policy,
		Scope:        req.scope,
	})
	if err != nil {
		op.refuseAdmit(err)
		return op, cliOpRefused
	}
	op.adm = adm
	if adm.Decision != legacyop.Replay {
		if !adm.Recorded && req.key != "" {
			if kerr := opjournal.ValidateClientKey(req.key); kerr != nil {
				fmt.Fprintf(req.stderr, "%s: warning: invalid --key (%v); running unrecorded, so a rerun acts again\n", req.verb, kerr) //nolint:errcheck // best-effort stderr
			}
		}
		return op, cliOpExecute
	}
	if cliOpReplaysAsSuccess(adm.Op) {
		if !req.asJSON {
			fmt.Fprintf(req.stderr, "%s: replayed op %s\n", req.verb, adm.Op.ID) //nolint:errcheck // best-effort stderr
		}
		return op, cliOpReplayed
	}
	op.refuse(cliOpCodeAlreadyFailed,
		fmt.Sprintf("op %s closed %s; rerun without --key to act again", adm.Op.ID, cliOpOutcomeText(adm.Op.Outcome)),
		adm.Op.ID, adm.Key)
	return op, cliOpRefused
}

// mode is the gate mode of the op's kind, resolved from cfg and the env
// break-glass. A gate that does not resolve (a misspelled config value) is
// off, with a warning: a config typo never records, and never refuses.
func (o *cliOp) mode(cfg *config.City) rollout.Mode {
	if cfg == nil {
		cfg = &config.City{}
	}
	flags, err := rollout.Resolve(cfg, rollout.ResolveOptions{LookupEnv: cliOpLookupEnv})
	if err != nil {
		fmt.Fprintf(o.req.stderr, "%s: warning: op journal: %v; not recording\n", o.req.verb, err) //nolint:errcheck // best-effort stderr
		return rollout.Off
	}
	spec, err := opjournal.Lookup(o.req.kind)
	if err != nil {
		fmt.Fprintf(o.req.stderr, "%s: warning: op journal: %v; not recording\n", o.req.verb, err) //nolint:errcheck // best-effort stderr
		return rollout.Off
	}
	switch spec.Class {
	case opjournal.ClassContent:
		return flags.OpsContent()
	case opjournal.ClassControl:
		return flags.OpsControl()
	default:
		return flags.OpsLifecycle()
	}
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

// cliOpReplaysAsSuccess reports whether a closed op replays as the command's
// success: it succeeded, or a kill found nothing to kill.
func cliOpReplaysAsSuccess(op opjournal.Op) bool {
	arm, reason := op.Outcome.Effective()
	return arm == opjournal.ArmSucceeded ||
		(op.Kind == opjournal.KindKill && arm == opjournal.ArmSkipped && reason == opjournal.ReasonNotNeeded)
}

// cliOpOutcomeText renders an outcome as arm{reason}.
func cliOpOutcomeText(o opjournal.Outcome) string {
	arm, reason := o.Effective()
	if reason == "" {
		return string(arm)
	}
	return string(arm) + "{" + reason + "}"
}

// refuseAdmit reports an Admit refusal.
func (o *cliOp) refuseAdmit(err error) {
	var busy *legacyop.BusyError
	switch {
	case errors.As(err, &busy):
		o.refuse(cliOpCodeInProgress, fmt.Sprintf("%v; rerun later or without --key", busy), busy.ID, o.req.key)
	case errors.Is(err, opjournal.ErrKeyReuse):
		o.refuse(cliOpCodeKeyMismatch, fmt.Sprintf("the key names a different request (%v); rerun with another --key", err), "", o.req.key)
	case o.req.key != "" && opjournal.ValidateClientKey(o.req.key) != nil:
		o.refuse(cliOpCodeInvalidKey, fmt.Sprintf("invalid --key (%v)", err), "", o.req.key)
	default:
		o.refuse(cliOpCodeUnavailable, fmt.Sprintf("op journal cannot record this request (%v); nothing was done", err), "", o.req.key)
	}
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
// degraded past. A require-mode refusal is reported by refuse instead.
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
		consequence = "the controller's op sweeper closes the op at its deadline"
	}
	fmt.Fprintf(o.req.stderr, "%s: warning: op journal %s failed (%s): %v; %s\n", o.req.verb, d.Stage, d.Reason, d.Err, consequence) //nolint:errcheck // best-effort stderr
}

// finish closes the op with res. It acts once; later calls do nothing.
func (o *cliOp) finish(res legacyop.Result) {
	if o == nil || o.adm == nil {
		return
	}
	o.adm.Finish(res)
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

// abandon closes an op the command left without a result as failed{other}.
// Defer it after starting the op: it does nothing once the op finished.
func (o *cliOp) abandon() {
	o.finishOutcome(opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonOther})
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
	if o == nil || o.adm == nil || !o.adm.Recorded {
		return
	}
	result.OpID = o.adm.Op.ID
	result.Key = o.adm.Key
	result.Replayed = o.adm.Decision == legacyop.Replay
}

// cliOpRefusedOutcome is failed{refused:<why>}.
func cliOpRefusedOutcome(why string) opjournal.Outcome {
	return opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.RefusedReasonPrefix + why}
}

// cliOpWakeConflictReason turns a wake conflict state into its refused
// reason token: lowercase, with anything outside [a-z0-9_] as '_'.
func cliOpWakeConflictReason(state string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		default:
			return '_'
		}
	}, state)
}

// cliOpSessionScope is the session(gen) scope of a kill, from the row's
// generation, and "" or the unrecordable reason when it has none.
func cliOpSessionScope(info session.Info, infoErr error) (opjournal.Scope, string) {
	if infoErr != nil {
		return opjournal.Scope{}, legacyop.ReasonNoGeneration
	}
	gen, err := strconv.ParseInt(strings.TrimSpace(info.Generation), 10, 64)
	if err != nil || gen < 1 {
		return opjournal.Scope{}, legacyop.ReasonNoGeneration
	}
	return opjournal.Scope{Kind: opjournal.ScopeSession, Generation: gen}, ""
}
