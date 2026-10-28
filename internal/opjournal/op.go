package opjournal

import (
	"slices"
	"time"
)

// Status is an op's position on the status axis. The set is closed.
type Status string

// The five op statuses.
const (
	StatusSubmitted Status = "submitted"
	StatusAccepted  Status = "accepted"
	StatusExecuting Status = "executing"
	StatusSettling  Status = "settling"
	StatusClosed    Status = "closed"
)

var statuses = []Status{StatusSubmitted, StatusAccepted, StatusExecuting, StatusSettling, StatusClosed}

// Valid reports whether s is one of the five statuses.
func (s Status) Valid() bool { return slices.Contains(statuses, s) }

// Arm is a closed op's outcome arm. The set is closed; reasons are open.
type Arm string

// The four outcome arms.
const (
	ArmSucceeded Arm = "succeeded"
	ArmFailed    Arm = "failed"
	ArmSkipped   Arm = "skipped"
	ArmCanceled  Arm = "canceled"
)

var arms = []Arm{ArmSucceeded, ArmFailed, ArmSkipped, ArmCanceled}

// Valid reports whether a is one of the four arms.
func (a Arm) Valid() bool { return slices.Contains(arms, a) }

// Known failed reasons. Reasons are open and additive: an unknown failed or
// skipped reason reads as failed{other}. RefusedReasonPrefix starts the
// refused:<why> family.
const (
	ReasonExpiredBeforeStart  = "expired_before_start"
	ReasonDeadlineInFlight    = "deadline_in_flight"
	ReasonNoBoundary          = "no_boundary"
	ReasonProviderUnavailable = "provider_unavailable"
	ReasonAmbiguousDelivery   = "ambiguous_delivery"
	ReasonPoison              = "poison"
	ReasonUnsupported         = "unsupported"
	ReasonUnauthorized        = "unauthorized"
	ReasonCreateFailed        = "create_failed"
	ReasonHeld                = "held"
	ReasonSessionGone         = "session_gone"
	ReasonClosedExternally    = "closed_externally"
	ReasonOther               = "other"
	// ReasonReplacementNotPrepared is a deliver-now unit's interrupt whose
	// turn settled but whose session could not be prepared for the message
	// (X11 D11): the replacement did not complete.
	ReasonReplacementNotPrepared = "replacement_not_prepared"
	// ReasonRestartNotStarted is an interrupt whose restart on failure
	// stopped the runtime and could not start it again: the turn ended
	// with the runtime, and the session has none until a start brings one
	// back. Older builds read it as failed{other}.
	ReasonRestartNotStarted = "restart_not_started"
	RefusedReasonPrefix     = "refused:"
)

// MaxRefusedWhyLen is the longest <why> a refused:<why> reason can carry and
// still be the token every outcome reason must be.
const MaxRefusedWhyLen = maxTokenLen - len(RefusedReasonPrefix)

// Known skipped reasons.
const (
	ReasonSuperseded         = "superseded"
	ReasonCoalescedInto      = "coalesced_into"
	ReasonNotNeeded          = "not_needed"
	ReasonDependencyFailed   = "dependency_failed"
	ReasonStale              = "stale"
	ReasonConversationClosed = "conversation_closed"
	ReasonBusy               = "busy"
)

var knownFailedReasons = []string{
	ReasonExpiredBeforeStart, ReasonDeadlineInFlight, ReasonNoBoundary, ReasonProviderUnavailable,
	ReasonAmbiguousDelivery, ReasonPoison, ReasonUnsupported, ReasonUnauthorized, ReasonCreateFailed,
	ReasonHeld, ReasonSessionGone, ReasonClosedExternally, ReasonOther, ReasonReplacementNotPrepared,
	ReasonRestartNotStarted,
}

var knownSkippedReasons = []string{
	ReasonSuperseded, ReasonCoalescedInto, ReasonNotNeeded, ReasonDependencyFailed,
	ReasonStale, ReasonConversationClosed, ReasonBusy,
}

// ScopeKind names an op's validity scope.
type ScopeKind string

// Scope kinds.
const (
	ScopeConversation ScopeKind = "conversation"
	ScopeContext      ScopeKind = "context"
	ScopeSession      ScopeKind = "session"
)

var scopeKinds = []ScopeKind{ScopeConversation, ScopeContext, ScopeSession}

// When is a message's delivery mode.
type When string

// Message delivery modes.
const (
	WhenBoundary  When = "boundary"
	WhenAfterTurn When = "after_turn"
	WhenInterrupt When = "interrupt"
	WhenIfIdle    When = "if_idle"
	WhenNow       When = "now"
	WhenSteer     When = "steer"
)

var whens = []When{WhenBoundary, WhenAfterTurn, WhenInterrupt, WhenIfIdle, WhenNow, WhenSteer}

// Wake says whether a message may wake its conversation.
type Wake string

// Wake modes.
const (
	WakeIfNeeded Wake = "if_needed"
	WakeLiveOnly Wake = "live_only"
)

var wakes = []Wake{WakeIfNeeded, WakeLiveOnly}

// Role says how a message is presented to the agent.
type Role string

// Message roles.
const (
	RolePrompt Role = "prompt"
	RoleNotice Role = "notice"
)

var roles = []Role{RolePrompt, RoleNotice}

// Fallback is what a steer message does on a runtime that cannot steer.
type Fallback string

// Steer fallbacks.
const (
	FallbackBoundary Fallback = "boundary"
	FallbackFail     Fallback = "fail"
)

var fallbacks = []Fallback{FallbackBoundary, FallbackFail}

// OnFailure is what an interrupt does when no boundary is observed.
type OnFailure string

// Interrupt failure policies.
const (
	OnFailureFail    OnFailure = "fail"
	OnFailureRestart OnFailure = "restart"
)

var onFailures = []OnFailure{OnFailureFail, OnFailureRestart}

// RestartMode is a restart's mode.
type RestartMode string

// Restart modes.
const (
	RestartContinue RestartMode = "continue"
	RestartFresh    RestartMode = "fresh"
)

var restartModes = []RestartMode{RestartContinue, RestartFresh}

// Guarantee is a conversation-lane op's execution guarantee.
type Guarantee string

// Execution guarantees.
const (
	GuaranteeAtLeastOnce Guarantee = "at_least_once"
	GuaranteeAtMostOnce  Guarantee = "at_most_once"
)

var guarantees = []Guarantee{GuaranteeAtLeastOnce, GuaranteeAtMostOnce}

// ExecutorKind names who executes an op, written by Propose and never changed.
// A lane takes, leases and settles only ExecutorLane ops; every other value is
// a legacy executor, which closes its own op (or an adopter or the sweeper
// does). A row without the key reads ExecutorLegacy. The name Executor is left
// for the lane's executor interface (DES-O1 §4.2).
type ExecutorKind string

// Executor kinds.
const (
	// ExecutorLane is an op a conversation or lifecycle lane executes.
	ExecutorLane ExecutorKind = "lane"
	// ExecutorLegacy is a legacy op from an unknown surface: never adopted,
	// closed by its legacy path or the sweeper.
	ExecutorLegacy ExecutorKind = "legacy"
	// ExecutorLegacyAPI is a legacy op the API executes; only the API adopts
	// one.
	ExecutorLegacyAPI ExecutorKind = "legacy:api"
	// ExecutorLegacyCLI is a legacy op a CLI process executes; it is never
	// adopted.
	ExecutorLegacyCLI ExecutorKind = "legacy:cli"
)

var executors = []ExecutorKind{ExecutorLane, ExecutorLegacy, ExecutorLegacyAPI, ExecutorLegacyCLI}

// Valid reports whether e is one of the four executors.
func (e ExecutorKind) Valid() bool { return slices.Contains(executors, e) }

// Legacy reports whether e is a legacy executor: anything but ExecutorLane.
func (e ExecutorKind) Legacy() bool { return e != ExecutorLane }

// Actor values gc's own producers use. Actor is an open token; other
// producers may send their own.
const (
	ActorOperator   = "operator"
	ActorAgent      = "agent"
	ActorBackground = "background"
	ActorController = "controller"
	ActorSweep      = "sweep"
)

// Policy is an op's resolved policy. Only the op kind's own sub-policy may be
// non-zero, and Guarantee applies to conversation-lane kinds only.
type Policy struct {
	Guarantee Guarantee
	Message   MessagePolicy
	Interrupt InterruptPolicy
	Restart   RestartPolicy
	Kill      KillPolicy
}

// MessagePolicy is a message's policy. NowGeneration and NowTurn name the
// session and turn a deliver-now (When: now) targets, as the requester saw
// them; they are valid only with When: now, and absent means the front door's
// resolution (the current session and its current turn when known).
type MessagePolicy struct {
	When          When
	Wake          Wake
	Role          Role
	OnUnsupported Fallback
	NowGeneration int64
	NowTurn       string
}

// InterruptPolicy is an interrupt's policy.
type InterruptPolicy struct {
	OnFailure OnFailure
}

// RestartPolicy is a restart's policy.
type RestartPolicy struct {
	Mode RestartMode
}

// KillPolicy is a kill's policy. A zero Grace means the runtime's configured
// grace.
type KillPolicy struct {
	Grace time.Duration
}

// Scope is an op's validity scope: the conversation, one context epoch, or
// one session generation (with a turn for interrupt, an interaction
// occurrence for respond).
type Scope struct {
	Kind        ScopeKind
	Epoch       int64
	Generation  int64
	Turn        string
	Interaction string
}

// Producer identifies who proposed an op. Principal is a stable logical id
// (an account, a conversation id, an order id), never a bearer, credential or
// box epoch. Actor is an open class token.
type Producer struct {
	Principal  string
	Actor      string
	OwnerEpoch int64
}

// Cancel is a recorded cancel request. The first one recorded wins.
type Cancel struct {
	By     string
	At     time.Time
	Reason string
}

// Recorded reports whether a cancel was recorded.
func (c Cancel) Recorded() bool { return c.By != "" }

// Result is what a succeeded (or coalesced) op produced. Delivery, TurnLink
// and TurnBasis are the conversation lane's delivery record and turn link
// (DES-O1 §4.8): set only on a succeeded lane op, and Turn is then the turn
// CorrelateTurn linked. Turn without a link is legal: legacy and control ops
// record a turn the runtime named. An op the lane's scope rule closed
// skipped{stale} unrun records the generation and epoch that fenced it off
// (FencedOff).
type Result struct {
	Generation    int64
	Turn          string
	ContextEpoch  int64
	Via           string
	CoalescedInto string
	Delivery      RecordedDelivery
	TurnLink      LinkConfidence
	TurnBasis     LinkBasis
}

// RecordedDelivery is the part of a DeliveryRecord stored on each op of one
// delivery. The generation and epoch are Result's; the link mode is not
// stored.
type RecordedDelivery struct {
	// Seq is the lane_seq of the delivery's head op.
	Seq uint64
	// At is the lane host's clock just before the effect's first input.
	At time.Time
	// Ack is the runtime's answer.
	Ack AckKind
	// MessageID and MessageAt are the runtime's id and creation stamp for
	// the delivered message, when it reports them.
	MessageID string
	MessageAt time.Time
}

// IsZero reports whether no delivery is recorded.
func (d RecordedDelivery) IsZero() bool {
	return d.Seq == 0 && d.At.IsZero() && d.Ack == "" && d.MessageID == "" && d.MessageAt.IsZero()
}

// equal compares two delivery records, comparing times as instants.
func (d RecordedDelivery) equal(o RecordedDelivery) bool {
	return d.Seq == o.Seq && d.At.Equal(o.At) && d.Ack == o.Ack && d.MessageID == o.MessageID &&
		d.MessageAt.Equal(o.MessageAt)
}

// Equal reports whether r and o are the same result, comparing times as
// instants rather than by representation.
func (r Result) Equal(o Result) bool {
	return r.Generation == o.Generation && r.Turn == o.Turn && r.ContextEpoch == o.ContextEpoch &&
		r.Via == o.Via && r.CoalescedInto == o.CoalescedInto && r.Delivery.equal(o.Delivery) &&
		r.TurnLink == o.TurnLink && r.TurnBasis == o.TurnBasis
}

// Outcome is a closed op's single terminal outcome.
type Outcome struct {
	Arm         Arm
	Reason      string
	Degradation string
	Result      Result
}

// Equal reports whether o and other are the same outcome, comparing times as
// instants.
func (o Outcome) Equal(other Outcome) bool {
	return o.Arm == other.Arm && o.Reason == other.Reason && o.Degradation == other.Degradation &&
		o.Result.Equal(other.Result)
}

// Effective returns the arm and reason a reader acts on: a failed or skipped
// outcome whose reason this build does not know reads as failed{other}.
func (o Outcome) Effective() (Arm, string) {
	switch o.Arm {
	case ArmFailed:
		if slices.Contains(knownFailedReasons, o.Reason) || isRefusedReason(o.Reason) {
			return ArmFailed, o.Reason
		}
		return ArmFailed, ReasonOther
	case ArmSkipped:
		if slices.Contains(knownSkippedReasons, o.Reason) {
			return ArmSkipped, o.Reason
		}
		return ArmFailed, ReasonOther
	default:
		return o.Arm, o.Reason
	}
}

// FencedOff reports whether op closed skipped{stale} because the lane's scope
// rule fenced it off unrun: its conversation had moved past its scope's
// generation (a session-scoped op) or context epoch (a context-scoped one).
// That close records the incarnation that fenced the op in its Result, past
// its scope's; the executor's close of an op that found nothing to do, and
// the dormant rule's, record none.
func FencedOff(op Op) bool {
	arm, reason := op.Outcome.Effective()
	if op.Status != StatusClosed || arm != ArmSkipped || reason != ReasonStale {
		return false
	}
	switch op.Scope.Kind {
	case ScopeSession:
		return op.Outcome.Result.Generation > op.Scope.Generation
	case ScopeContext:
		return op.Outcome.Result.ContextEpoch > op.Scope.Epoch
	default:
		return false
	}
}

func isRefusedReason(r string) bool {
	return len(r) > len(RefusedReasonPrefix) && r[:len(RefusedReasonPrefix)] == RefusedReasonPrefix
}

// Transition is one entry of an op's append-only status history.
type Transition struct {
	Status Status
	At     time.Time
	By     string
}

// Request is what a producer proposes. Zero policy fields, scope, start_by and
// finish_by take the kind's defaults, and a zero Executor is ExecutorLegacy.
// Executor is outside the request digest, so a retry after a mode flip returns
// the stored op unchanged. MintedKey says the producer minted ClientKey in
// this call, so no prior op can exist and Propose skips its read-first Get.
type Request struct {
	Conversation string
	Kind         Kind
	Executor     ExecutorKind
	MintedKey    bool
	ClientKey    string
	Producer     Producer
	Payload      string
	Policy       Policy
	Scope        Scope
	After        string
	ClientSeq    uint64
	NotBefore    time.Time
	StartBy      time.Time
	FinishBy     time.Time
	Causation    string
	Correlation  string
}

// Op is one op row: the request as accepted plus the platform-owned status,
// counters, lease, cancel, outcome and transitions.
type Op struct {
	ID           string
	Conversation string
	Kind         Kind
	// Executor is who executes the op (see ExecutorKind).
	Executor      ExecutorKind
	SchemaRev     int
	ClientKey     string
	Payload       string
	PayloadDigest string
	Policy        Policy
	Scope         Scope
	After         string
	ClientSeq     uint64
	NotBefore     time.Time
	StartBy       time.Time
	FinishBy      time.Time
	Producer      Producer
	Causation     string
	Correlation   string
	// CreatedAt is the proposer's clock at Propose, the ordering timestamp.
	CreatedAt time.Time

	Status Status
	// ArrivalSeq is bd's platform-owned arrival order, a row column. It is
	// read-only and zero until OJ-10a maps the column; the codec never
	// writes or reads it as gc.op.* metadata, which a producer could forge.
	ArrivalSeq  uint64
	LaneSeq     uint64
	Attempt     int
	Lease       Lease
	Cancel      Cancel
	Outcome     Outcome
	Transitions []Transition

	// The conversation lane's bookkeeping (DES-O1 §4.1). Only lane ops of the
	// conversation lane carry these; a legacy op carries none.
	//
	// RetryAt gates the op after a release; it does not move its order
	// position. Take and close clear it.
	RetryAt time.Time
	// Failures counts failed attempts: executor errors and lease expiries.
	// The op closes failed{poison} at MaxFailures.
	Failures int
	// MaybeDelivered is the number of the last attempt that may have reached
	// the agent, 0 when none may have; it never exceeds Attempt.
	MaybeDelivered int
	// DeliveryKey is the runtime idempotency key of the batch the op was last
	// taken in, <head op id>:<n>. It never appears in delivered text.
	DeliveryKey string
	// LastError is the last release or failure cause, <token>[: <text>].
	LastError string

	// Revision is the row's revision when it was read (or created), decoded
	// from the bead and never encoded. An op returned by a write carries the
	// revision read before that write, so CloseKnown on it takes its
	// fallback.
	Revision int64
}
