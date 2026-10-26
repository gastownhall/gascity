package legacyop

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// ExecutionBound is how long a legacy executor may hold an open op: a legacy
// op's start_by and finish_by are its producer's clock plus this bound, and
// the sweeper, or a same-key Admit, closes an op still open after finish_by.
const ExecutionBound = 15 * time.Minute

// Legacy executors and the operator principal.
const (
	// ExecutorAPI records the API's ops; only the API adopts its orphans.
	ExecutorAPI = opjournal.ExecutorLegacyAPI
	// ExecutorCLI records CLI ops, which are never adopted.
	ExecutorCLI = opjournal.ExecutorLegacyCLI
	// PrincipalOperator is the principal of every API request and of a CLI
	// run outside an agent session.
	PrincipalOperator = "operator"
)

const (
	conversationPrincipalPrefix = "conversation:"
	mintedKeyPrefix             = "k-"
	mintedKeyBytes              = 16
)

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ConversationPrincipal is the principal of a request made from inside an
// agent session: conversation:<id>.
func ConversationPrincipal(conversationID string) string {
	return conversationPrincipalPrefix + conversationID
}

// MintKey returns a fresh client key: "k-" and 26 lowercase base32
// characters (128 random bits).
func MintKey() (string, error) {
	var b [mintedKeyBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("minting an op key: %w", err)
	}
	return mintedKeyPrefix + strings.ToLower(keyEncoding.EncodeToString(b[:])), nil
}

// Refusals Admit returns. Callers map them to their wire with errors.Is.
var (
	// ErrBusy reports a request whose op is being admitted or executed
	// elsewhere: in flight in this process, or open under another executor.
	// It is returned as *BusyError.
	ErrBusy = errors.New("legacyop: op in progress")
	// ErrUnavailable reports, in require mode, an op store that cannot
	// record the request. The request's own effect did not run; a caller
	// may have prepared its target first (the API materializes a
	// configured named session so the op can name it).
	ErrUnavailable = errors.New("legacyop: op journal unavailable")
)

// BusyError is ErrBusy for one op. Since is when the op was created, zero
// for a request still being admitted in this process.
type BusyError struct {
	ID    string
	Since time.Time
}

// Error names the op.
func (e *BusyError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Since.IsZero() {
		return fmt.Sprintf("op %s is in progress", e.ID)
	}
	return fmt.Sprintf("op %s is in progress (since %s)", e.ID, e.Since.UTC().Format(time.RFC3339))
}

// Unwrap returns ErrBusy.
func (e *BusyError) Unwrap() error { return ErrBusy }

// Request is one legacy request to record. Key is the client's key; empty
// means the recorder mints one. After stays empty in O0.
type Request struct {
	Conversation string
	Principal    string
	Actor        string
	Key          string
	Payload      string
	Causation    string
	Kind         opjournal.Kind
	Policy       opjournal.Policy
	Scope        opjournal.Scope
	After        string
}

// Decision is what Admit decided.
type Decision int

// Decisions.
const (
	// Execute runs the legacy path. Admission.Recorded says whether an op
	// row backs it.
	Execute Decision = iota + 1
	// Replay answers from a stored outcome (Admission.Op, closed) or from
	// the in-flight answer of the request already running here
	// (Admission.Cached), without executing.
	Replay
)

// Admission is Admit's answer for one request.
type Admission struct {
	Decision Decision
	// Key is the supplied or minted client key.
	Key string
	// Minted says the recorder minted Key.
	Minted bool
	// Recorded says an op row backs this request.
	Recorded bool
	// Adopted says Execute re-runs an orphaned op (attempt+1).
	Adopted bool
	// Op is the op: the created or adopted one for Execute, the closed one
	// for a stored replay, the preview (id and digest) for an in-flight
	// replay; zero when not recorded.
	Op opjournal.Op
	// Cached is the holder's answer for an in-flight replay.
	Cached any
	// Entry is the in-flight entry this request holds, or nil.
	Entry *Entry
	// Unrecordable is the journal's refusal of a request auto runs
	// unrecorded because the journal rejects the request itself (its key,
	// or its payload: too large, not valid UTF-8, or empty), so the caller
	// can say why; nil otherwise, a store failure included (Degraded hears
	// of that).
	Unrecordable error

	rec   *Recorder
	store *opjournal.Store
	done  atomic.Bool
}

// DeliveryKey is the runtime delivery key of the admitted op's delivery:
// gc:<op id>:1 for an Execute admission an op row backs, "" otherwise. A
// legacy op is delivered on its own and closes on a definitive refusal, so
// every execution of it, an adopted one included, uses this one key, and a
// deduplicating runtime replays the first delivery instead of delivering
// twice (DES-O0API §6.1, §17).
func (a *Admission) DeliveryKey() string {
	if a == nil || a.Decision != Execute || !a.Recorded || a.Op.ID == "" {
		return ""
	}
	return opjournal.RuntimeDeliveryKey(opjournal.FormatDeliveryKey(a.Op.ID, 1))
}

// Recorder records one surface's requests under one gate mode. Executor is
// ExecutorAPI or ExecutorCLI; Admit refuses any other. Open opens
// the op store lazily; the caller caches it. Admit opens it once per
// admission and Finish closes through that store; a background close retry
// calls Open again for each attempt, so it closes through whatever store Open
// answers by then. Inflight is the API's in-flight registry; nil (the CLI)
// means never adopt. Neither surface adopts an op past its finish_by: a
// same-key Admit closes it, as the sweeper would. Degraded, when set, is told
// of every store failure the recorder degrades past but one QuietCannotRecord
// keeps quiet. Healthy, when set, is told of every close the store answers,
// before the op's entry is released: a background retry's close lands after
// Finish has returned, out of the caller's sight. Logger receives the
// recorder's rate-limited degrade lines and late-result lines; nil is
// slog.Default(). QuietCannotRecord, set for a gate that is auto only by its
// builtin default, treats a store that cannot record ops at all
// (Degrade.StoreCannotRecord) as off: that degrade is logged at debug and not
// passed to Degraded, while a store error, contention or a later-stage
// failure still degrades loudly. Done, when closed (its owner
// shutting down), ends any background close retry, leaving the op to the
// sweeper; the entry stays held until the op's finish_by has passed, so a
// same-key retry in this process does not adopt an op whose effect may
// already have landed, and closes it once the entry is released. nil never
// closes. CrashPoints, when set, hears each crash point of a recorded
// execution (CrashPoint); production recorders carry none.
type Recorder struct {
	Mode     gate.Mode
	Executor opjournal.ExecutorKind
	Open     func() (*opjournal.Store, error)
	Inflight *Inflight
	Clock    clock.Clock
	Degraded func(Degrade)
	Healthy  func(stage string)
	Logger   *slog.Logger
	Done     <-chan struct{}

	QuietCannotRecord bool

	CrashPoints CrashPointFunc

	// sleep waits between close retries; nil is time.Sleep.
	sleep func(time.Duration)
	// after runs f once d has passed; nil is time.AfterFunc.
	after func(d time.Duration, f func())

	logMu  sync.Mutex
	logged map[degradeLogKey]time.Time
}

// Admit decides how to run req (DES-O0API §5.1, refined so a minted key's
// ambiguous create executes as created, see ownAmbiguousCreate, and so the
// API closes an op past its finish_by rather than adopting it, see
// closeExpired). It returns an error only for a refusal the caller maps to
// its wire: *BusyError (ErrBusy) and opjournal.ErrKeyReuse in every mode;
// ErrUnavailable, opjournal.ErrPayloadTooLarge and
// opjournal.ErrInvalidRequest in require mode only. A recorder whose Executor
// is not a legacy surface is a programming error: opjournal.ErrInvalidRequest
// in every recording mode, before any store I/O. In off it does no store I/O
// and mints nothing.
func (r *Recorder) Admit(req Request) (*Admission, error) {
	if !r.recording() {
		return &Admission{Decision: Execute, Key: req.Key, rec: r}, nil
	}
	if r.Executor != ExecutorAPI && r.Executor != ExecutorCLI {
		return nil, fmt.Errorf("%w: a legacy recorder's executor is %s or %s, not %q",
			opjournal.ErrInvalidRequest, ExecutorAPI, ExecutorCLI, r.Executor)
	}
	now := r.Clock.Now()
	key, minted := req.Key, false
	if key == "" {
		k, err := MintKey()
		if err != nil {
			return r.cannotRecord(req.Kind, key, false, StageOpen, ReasonStoreUnavailable, err)
		}
		key, minted = k, true
	}
	st, err := r.Open()
	if err != nil {
		return r.cannotRecord(req.Kind, key, minted, StageOpen, openReason(err), err)
	}
	oreq := opjournal.Request{
		Conversation: req.Conversation,
		Kind:         req.Kind,
		Executor:     r.Executor,
		MintedKey:    minted,
		ClientKey:    key,
		Producer:     opjournal.Producer{Principal: req.Principal, Actor: req.Actor},
		Payload:      req.Payload,
		Policy:       req.Policy,
		Scope:        req.Scope,
		After:        req.After,
		StartBy:      now.Add(ExecutionBound),
		FinishBy:     now.Add(ExecutionBound),
		Causation:    req.Causation,
	}
	preview, err := st.Preview(oreq)
	if err != nil {
		return r.unrecordable(req, key, minted, err)
	}
	var entry *Entry
	if r.Inflight != nil {
		held, ok := r.Inflight.Reserve(preview.ID, preview.PayloadDigest)
		if !ok {
			return r.inflightAnswer(req.Kind, key, minted, preview, held)
		}
		entry = held
	}
	release := func() {
		if entry != nil {
			r.Inflight.Release(preview.ID, entry)
		}
	}
	// A panic from here until the entry passes to the returned Admission
	// would leave no Admission to Finish: release the entry and re-panic.
	defer func() {
		if p := recover(); p != nil {
			release()
			panic(p)
		}
	}()
	op, created, err := st.Propose(oreq)
	// Decide expiry on a clock read after Reserve: an entry released past its
	// op's finish_by was released before Reserve took it, so this read finds
	// the op expired and closes it rather than adopting it.
	now = r.Clock.Now()
	switch {
	case errors.Is(err, opjournal.ErrKeyReuse):
		release()
		r.count(req.Kind, StageAdmit, ResultKeyReuse, "")
		return nil, err
	case err != nil:
		release()
		return r.cannotRecord(req.Kind, key, minted, StagePropose, proposeReason(err), err)
	case created, ownAmbiguousCreate(minted, op, r.Executor):
		r.count(req.Kind, StageAdmit, ResultRecorded, "")
		adm := r.execute(st, key, minted, op, entry, false)
		_ = r.crashPoint(CrashPointAdmitted, op)
		return adm, nil
	case op.Status == opjournal.StatusClosed:
		release()
		return r.replay(key, minted, op), nil
	case op.Executor.Legacy() && now.After(op.FinishBy):
		// Healthy hears of the close while the entry is still held.
		adm, err := r.closeExpired(st, key, minted, op, now)
		release()
		return adm, err
	case op.Executor == ExecutorAPI && r.Inflight != nil:
		return r.adopt(st, key, minted, op, entry, release, now)
	default:
		release()
		r.count(req.Kind, StageAdmit, ResultBusy, "")
		return nil, &BusyError{ID: op.ID, Since: op.CreatedAt}
	}
}

// ownAmbiguousCreate reports whether op, found by Propose without creating
// it, is this call's own row from a create that committed but answered an
// error: a key minted in this call (128 random bits) names no other op, so an
// open op at attempt 0 under this recorder's executor can only be that row.
// Admit then executes it as created, rather than answering busy (the CLI) or
// adopting its own op (the API).
func ownAmbiguousCreate(minted bool, op opjournal.Op, executor opjournal.ExecutorKind) bool {
	return minted && op.Status != opjournal.StatusClosed && op.Attempt == 0 && op.Executor == executor
}

// recording reports whether the mode records at all.
func (r *Recorder) recording() bool { return r.Mode == gate.Auto || r.Mode == gate.Require }

// execute is an Execute admission over a recorded op in st.
func (r *Recorder) execute(st *opjournal.Store, key string, minted bool, op opjournal.Op, entry *Entry, adopted bool) *Admission {
	return &Admission{Decision: Execute, Key: key, Minted: minted, Recorded: true, Adopted: adopted, Op: op, Entry: entry, rec: r, store: st}
}

// replay is a Replay admission over a closed op.
func (r *Recorder) replay(key string, minted bool, op opjournal.Op) *Admission {
	r.count(op.Kind, StageAdmit, ResultReplayed, "")
	return &Admission{Decision: Replay, Key: key, Minted: minted, Recorded: true, Op: op, rec: r}
}

// inflightAnswer answers a request whose op this process is already admitting
// or running: the holder's answer once it has one, a key reuse when the
// digests differ, else busy.
func (r *Recorder) inflightAnswer(kind opjournal.Kind, key string, minted bool, preview opjournal.Op, held *Entry) (*Admission, error) {
	switch {
	case held.Digest != preview.PayloadDigest:
		r.count(kind, StageAdmit, ResultKeyReuse, "")
		return nil, fmt.Errorf("%w: op %s is in flight with digest %s", opjournal.ErrKeyReuse, preview.ID, held.Digest)
	case held.accepted && held.replay != nil:
		r.count(kind, StageAdmit, ResultReplayed, "")
		return &Admission{Decision: Replay, Key: key, Minted: minted, Recorded: true, Op: preview, Cached: held.replay, rec: r}, nil
	default:
		r.count(kind, StageAdmit, ResultBusy, "")
		return nil, &BusyError{ID: preview.ID}
	}
}

// closeExpired closes an open op whose finish_by has passed, as the sweeper
// would, and replays it. Both surfaces do this; the API never adopts an op
// past its finish_by, because its executor may have delivered. The close
// re-decides on the row it writes, so an op an API adopted after Propose read
// it stays open: busy, and no close health, since nothing closed.
func (r *Recorder) closeExpired(st *opjournal.Store, key string, minted bool, op opjournal.Op, now time.Time) (*Admission, error) {
	cur, _, err := st.CloseExpired(op, now, string(r.Executor))
	if err != nil {
		r.degrade(op.Kind, StageClose, DegradeReason(err), err)
		r.count(op.Kind, StageAdmit, ResultBusy, "")
		return nil, &BusyError{ID: op.ID, Since: op.CreatedAt}
	}
	if cur.Status != opjournal.StatusClosed {
		r.count(op.Kind, StageAdmit, ResultBusy, "")
		return nil, &BusyError{ID: op.ID, Since: op.CreatedAt}
	}
	r.closeHealthy()
	return r.replay(key, minted, cur), nil
}

// adopt re-runs an open legacy:api op that is not in flight here and not past
// its finish_by: its API died (DES-O0API D9).
func (r *Recorder) adopt(st *opjournal.Store, key string, minted bool, op opjournal.Op, entry *Entry, release func(), now time.Time) (*Admission, error) {
	adopted, err := st.Adopt(op.ID, op.Attempt, string(ExecutorAPI), now.Add(ExecutionBound))
	if err == nil {
		r.count(op.Kind, StageAdopt, ResultAdopted, "")
		adm := r.execute(st, key, minted, adopted, entry, true)
		_ = r.crashPoint(CrashPointAdmitted, adopted)
		return adm, nil
	}
	release()
	if errors.Is(err, opjournal.ErrAlreadyTerminal) {
		if closedOp, getErr := st.Get(op.ID); getErr == nil && closedOp.Status == opjournal.StatusClosed {
			return r.replay(key, minted, closedOp), nil
		}
	}
	if !errors.Is(err, opjournal.ErrAttemptMoved) && !errors.Is(err, opjournal.ErrAlreadyTerminal) {
		r.degrade(op.Kind, StageAdopt, DegradeReason(err), err)
	}
	r.count(op.Kind, StageAdopt, ResultBusy, "")
	return nil, &BusyError{ID: op.ID, Since: op.CreatedAt}
}

// unrecordable answers a request Preview refuses: run it unrecorded in auto,
// refuse it in require.
func (r *Recorder) unrecordable(req Request, key string, minted bool, err error) (*Admission, error) {
	reason := ReasonInvalidRequest
	switch {
	case errors.Is(err, opjournal.ErrPayloadTooLarge):
		reason = ReasonPayloadTooLarge
	case opjournal.ValidateClientKey(key) != nil:
		reason = ReasonInvalidKey
	}
	if r.Mode == gate.Require {
		r.count(req.Kind, StageAdmit, ResultRefused, reason)
		return nil, err
	}
	r.count(req.Kind, StageAdmit, ResultUnrecorded, reason)
	return &Admission{Decision: Execute, Key: key, Minted: minted, Unrecordable: err, rec: r}, nil
}

// cannotRecord answers a request the store cannot record: degrade, then run
// it unrecorded in auto or refuse it in require.
func (r *Recorder) cannotRecord(kind opjournal.Kind, key string, minted bool, stage, reason string, err error) (*Admission, error) {
	r.degrade(kind, stage, DegradeReason(err), err)
	if r.Mode == gate.Require {
		r.count(kind, StageAdmit, ResultRefused, reason)
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	r.count(kind, StageAdmit, ResultUnrecorded, reason)
	return &Admission{Decision: Execute, Key: key, Minted: minted, rec: r}, nil
}

// Unrecorded is an Execute admission for a request the caller knows it cannot
// record (an unmaterialized named target, a session with no generation or
// whose row cannot be read), counted under reason. The caller decides that
// only in auto; require resolves the cause first or refuses.
func (r *Recorder) Unrecorded(kind opjournal.Kind, key, reason string) *Admission {
	if r.recording() {
		r.count(kind, StageAdmit, ResultUnrecorded, reason)
	}
	return &Admission{Decision: Execute, Key: key, rec: r}
}

// Refused counts a request the caller refuses in require because it knows it
// cannot record it (Unrecorded's causes), under reason, as Admit counts a
// require refusal. It counts nothing in another mode.
func (r *Recorder) Refused(kind opjournal.Kind, reason string) {
	if r.Mode == gate.Require {
		r.count(kind, StageAdmit, ResultRefused, reason)
	}
}

// Accept stores the request's answer in its in-flight entry, so a same-key
// request replays it while this one runs. answer must be non-nil: the replay
// carries the open op, which has no outcome to build an answer from, so a nil
// answer is ignored and a same-key request stays busy. It does nothing for a
// request without an entry.
func (a *Admission) Accept(answer any) {
	if a == nil || a.Entry == nil || a.rec == nil || a.rec.Inflight == nil {
		return
	}
	a.rec.Inflight.Accept(a.Op.ID, a.Entry, answer)
}

// Result is a legacy path's verdict for Finish. LeaveOpen keeps a
// client-keyed op open after a retryable error, so a same-key retry before
// its finish_by adopts it (DES-O0API D10), and one after closes it
// failed{deadline_in_flight}; Outcome then says what the error was. Only a
// recorder that adopts (one with an in-flight registry) honors LeaveOpen; the
// CLI closes the op with Outcome.
type Result struct {
	Outcome   opjournal.Outcome
	LeaveOpen bool
}

// Finished says what Finish did.
type Finished struct {
	// Closed says this call closed the op with the result's outcome; Op is
	// the closed op.
	Closed bool
	Op     opjournal.Op
	// Late says the op was already closed with another outcome (the
	// sweeper, or an adopter, got there first).
	Late bool
	// Background, when not nil, is a close retry still running; it is
	// closed when the retry ends. Only a recorder with an in-flight
	// registry (the API) retries in the background.
	Background <-chan struct{}
}

// Finish closes the op from res (or, on the API, deliberately leaves it
// open) and releases the in-flight entry (DES-O0API §5.2). It acts once per
// admission and nothing for a Replay. A close that keeps failing degrades; the API then
// retries in the background until finish_by, holding the entry, and the CLI
// gives up and leaves the op to the sweeper.
func (a *Admission) Finish(res Result) Finished {
	if a == nil || a.Decision != Execute || a.done.Swap(true) {
		return Finished{}
	}
	if !a.Recorded {
		return Finished{}
	}
	// done is already set, so the handler's recovery cannot Finish again: a
	// panic or a runtime.Goexit below (the close's, or a crash-point
	// hook's) releases the entry here and keeps unwinding.
	var f Finished
	a.releaseOnUnwind(func() { f = a.finish(res) })
	return f
}

// finish is Finish's work for a recorded execution, once.
func (a *Admission) finish(res Result) Finished {
	r := a.rec
	_ = r.crashPoint(CrashPointEffected, a.Op)
	// Only a recorder that adopts (the API, with an in-flight registry)
	// leaves an op open for a same-key retry; CLI ops always close
	// (DES-O0API §8.2).
	if res.LeaveOpen && r.Inflight != nil {
		a.release()
		r.count(a.Op.Kind, StageClose, ResultLeftOpen, "")
		return Finished{}
	}
	op, err := r.closeWithRetries(a.store, a.Op, res.Outcome)
	switch {
	case err == nil:
		_ = r.crashPoint(CrashPointClosed, op)
		r.closeHealthy()
		a.release()
		r.count(a.Op.Kind, StageClose, ResultClosed, "")
		return Finished{Closed: true, Op: op}
	case errors.Is(err, opjournal.ErrAlreadyTerminal):
		r.closeHealthy()
		a.release()
		r.late(a.Op, res.Outcome, err)
		return Finished{Late: true}
	case errors.Is(err, opjournal.ErrInvalidRequest):
		// The legacy path's outcome is invalid: a bug in the caller, not a
		// store failure, so no degrade and no retry.
		a.release()
		r.logger().Error("op journal: invalid outcome; op left to the sweeper", "op", a.Op.ID,
			"surface", surface(r.Executor), "arm", res.Outcome.Arm, "reason", res.Outcome.Reason, "err", err)
		r.count(a.Op.Kind, StageClose, ResultFailed, "")
		return Finished{}
	}
	r.degrade(a.Op.Kind, StageClose, DegradeReason(err), err)
	if r.Inflight == nil {
		a.release()
		r.count(a.Op.Kind, StageClose, ResultFailed, "")
		return Finished{}
	}
	done := make(chan struct{})
	go a.closeInBackground(res.Outcome, done)
	return Finished{Background: done}
}

// closeRetries and closeRetryGap bound Finish's foreground close.
const (
	closeRetries  = 3
	closeRetryGap = 50 * time.Millisecond
	// backgroundGapMax caps the background retry's backoff.
	backgroundGapMax = 5 * time.Second
)

// closeWithRetries closes op with outcome, retrying a failed close
// closeRetries times, or fewer once Done has closed. ErrAlreadyTerminal and
// a refused outcome are final. Each attempt first reports CrashPointClosing,
// whose error fails the attempt.
func (r *Recorder) closeWithRetries(st *opjournal.Store, op opjournal.Op, outcome opjournal.Outcome) (opjournal.Op, error) {
	var err error
	for i := range closeRetries {
		if i > 0 && !r.wait(closeRetryGap) {
			break
		}
		var closedOp opjournal.Op
		if err = r.crashPoint(CrashPointClosing, op); err == nil {
			closedOp, err = st.CloseKnown(op, outcome, string(r.Executor))
		}
		if err == nil || errors.Is(err, opjournal.ErrAlreadyTerminal) || errors.Is(err, opjournal.ErrInvalidRequest) {
			return closedOp, err
		}
	}
	return opjournal.Op{}, err
}

// closeInBackground keeps closing the op, backing off and reopening the
// store for each attempt, until it closes, its finish_by passes (the sweeper,
// or a same-key Admit, closes it then) or the recorder's Done closes, then
// releases the entry. When Done ends the retry before finish_by, the entry is
// held until finish_by has passed: the request ran in this process and its
// effect may have landed, so a same-key retry must find it in flight rather
// than adopt it (DES-O0API I3, §12). A close it lands reports no
// CrashPointClosed: Finish has returned, so the surface may have answered.
func (a *Admission) closeInBackground(outcome opjournal.Outcome, done chan<- struct{}) {
	defer close(done)
	// The retry outlives Finish's unwind release: a panic or a
	// runtime.Goexit in it (a crash-point hook's) releases the entry here.
	a.releaseOnUnwind(func() { a.retryClose(outcome) })
}

// retryClose is closeInBackground's work.
func (a *Admission) retryClose(outcome opjournal.Outcome) {
	r := a.rec
	gap := closeRetryGap
	for !r.Clock.Now().After(a.Op.FinishBy) && r.wait(gap) {
		err := r.closeOpened(a.Op, outcome)
		switch {
		case err == nil:
			r.closeHealthy()
			a.release()
			r.count(a.Op.Kind, StageClose, ResultClosed, "")
			return
		case errors.Is(err, opjournal.ErrAlreadyTerminal):
			r.closeHealthy()
			a.release()
			r.late(a.Op, outcome, err)
			return
		}
		gap = min(2*gap, backgroundGapMax)
	}
	r.count(a.Op.Kind, StageClose, ResultFailed, "")
	a.releaseAtFinishBy()
}

// closeOpened closes op with outcome through the store Open answers now,
// after reporting CrashPointClosing, whose error fails the attempt.
func (r *Recorder) closeOpened(op opjournal.Op, outcome opjournal.Outcome) error {
	if err := r.crashPoint(CrashPointClosing, op); err != nil {
		return err
	}
	st, err := r.Open()
	if err != nil {
		return err
	}
	_, err = st.CloseKnown(op, outcome, string(r.Executor))
	return err
}

// releaseAtFinishBy releases the entry once the op's finish_by has passed,
// re-checking the clock when the hold ends: a same-key Admit closes an op
// only past its finish_by, so an entry released at finish_by itself would let
// it adopt the op.
func (a *Admission) releaseAtFinishBy() {
	r := a.rec
	hold := a.Op.FinishBy.Sub(r.Clock.Now())
	if hold < 0 {
		a.release()
		return
	}
	if r.after != nil {
		r.after(hold+time.Nanosecond, a.releaseAtFinishBy)
		return
	}
	time.AfterFunc(hold+time.Nanosecond, a.releaseAtFinishBy)
}

// release returns the request's in-flight entry.
func (a *Admission) release() {
	if a.Entry != nil && a.rec.Inflight != nil {
		a.rec.Inflight.Release(a.Op.ID, a.Entry)
	}
}

// releaseOnUnwind runs f and releases the entry when f does not return: a
// panic or a runtime.Goexit inside it releases the entry on its way through
// and keeps unwinding. A release f made first is harmless: Release acts only
// on the entry's own hold.
func (a *Admission) releaseOnUnwind(f func()) {
	returned := false
	defer func() {
		if !returned {
			a.release()
		}
	}()
	f()
	returned = true
}

// wait sleeps d and reports whether to go on: false once Done has closed.
func (r *Recorder) wait(d time.Duration) bool {
	if r.sleep != nil {
		r.sleep(d)
		return !r.stopped()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Done:
		return false
	}
}

// stopped reports whether Done has closed.
func (r *Recorder) stopped() bool {
	select {
	case <-r.Done:
		return true
	default:
		return false
	}
}

// openReason is the unrecorded reason for a store that did not open.
func openReason(err error) string {
	if errors.Is(err, opjournal.ErrNoNamespace) {
		return ReasonNoNamespace
	}
	return ReasonStoreUnavailable
}

// proposeReason is the unrecorded reason for a failed Propose.
func proposeReason(err error) string {
	if errors.Is(err, opjournal.ErrBeadTypeUnregistered) {
		return ReasonTypeUnregistered
	}
	return ReasonProposeError
}

// DegradeReason is the reason class a degrade reports for err: a capability
// or namespace gap, contention, or a store error. Every surface that degrades
// past an op-store failure classifies it here, so the classes cannot drift.
func DegradeReason(err error) string {
	switch {
	case errors.Is(err, beads.ErrConditionalWriteUnsupported):
		return DegradeConditionalWritesUnsupported
	case errors.Is(err, opjournal.ErrBeadTypeUnregistered):
		return DegradeTypeUnregistered
	case errors.Is(err, opjournal.ErrNoNamespace):
		return DegradeNoNamespace
	case errors.Is(err, opjournal.ErrContention):
		return DegradeContention
	default:
		return DegradeStoreError
	}
}
