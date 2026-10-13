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
	RefusedReasonPrefix       = "refused:"
)

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
	ReasonHeld, ReasonSessionGone, ReasonClosedExternally, ReasonOther,
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

// MessagePolicy is a message's policy.
type MessagePolicy struct {
	When          When
	Wake          Wake
	Role          Role
	OnUnsupported Fallback
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
// one session generation (with a turn for interrupt, an interaction for
// respond).
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

// Result is what a succeeded (or coalesced) op produced.
type Result struct {
	Generation    int64
	Turn          string
	ContextEpoch  int64
	Via           string
	CoalescedInto string
}

// Outcome is a closed op's single terminal outcome.
type Outcome struct {
	Arm         Arm
	Reason      string
	Degradation string
	Result      Result
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
// finish_by take the kind's defaults.
type Request struct {
	Conversation string
	Kind         Kind
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
	ID            string
	Conversation  string
	Kind          Kind
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

	Status      Status
	ArrivalSeq  uint64
	LaneSeq     uint64
	Attempt     int
	Lease       Lease
	Cancel      Cancel
	Outcome     Outcome
	Transitions []Transition
}
