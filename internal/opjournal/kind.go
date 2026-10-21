package opjournal

import (
	"fmt"
	"slices"
	"time"
)

// Kind names an op kind. The set is closed: Lookup refuses any kind the
// registry does not define.
type Kind string

// The ten op kinds.
const (
	KindMessage   Kind = "message"
	KindInterrupt Kind = "interrupt"
	KindRespond   Kind = "respond"
	KindCreate    Kind = "create"
	KindWake      Kind = "wake"
	KindSuspend   Kind = "suspend"
	KindResume    Kind = "resume"
	KindRestart   Kind = "restart"
	KindKill      Kind = "kill"
	KindClose     Kind = "close"
)

// Class is an op's class: content, control or lifecycle. It is unrelated to
// coordclass.Class, which names the store that holds a row.
type Class string

// Op classes.
const (
	ClassContent   Class = "content"
	ClassControl   Class = "control"
	ClassLifecycle Class = "lifecycle"
)

// Lane names the platform lane that executes an op.
type Lane string

// Op lanes.
const (
	LaneConversation Lane = "conversation"
	LaneLifecycle    Lane = "lifecycle"
)

// CloseRule says what happens to an open op of a kind when its conversation
// closes. Ops in settling settle normally from facts under either rule. In O0
// the rule is data only.
type CloseRule string

// Conversation-close rules.
const (
	// CloseRuleSkip closes the open op skipped{conversation_closed}.
	CloseRuleSkip CloseRule = "skip"
	// CloseRuleSucceed closes the op succeeded when the conversation row
	// closes; it is the close kind's own rule.
	CloseRuleSucceed CloseRule = "succeed"
)

// Spec is one row of the kind registry.
type Spec struct {
	Kind  Kind
	Class Class
	Lane  Lane
	// DefaultScope is the scope kind an unscoped request gets.
	DefaultScope ScopeKind
	// Scopes lists the scope kinds a request may name.
	Scopes []ScopeKind
	// TurnScoped allows a turn on a session scope.
	TurnScoped bool
	// InteractionScoped requires an interaction (request id) on a session
	// scope.
	InteractionScoped bool
	// When lists the delivery modes a message may name; empty for every other
	// kind.
	When            []When
	PayloadRequired bool
	PayloadAllowed  bool
	AllowsAfter     bool
	// StartWithin and FinishWithin give the default start_by and finish_by,
	// measured from max(now, not_before).
	StartWithin  time.Duration
	FinishWithin time.Duration
	// DefaultPolicy is the policy an omitted field defaults to.
	DefaultPolicy       Policy
	OnConversationClose CloseRule
}

// MaxKinds caps the registry. The journal's value is a small closed set of
// atomic kinds; a registry that grows past this is a design smell, not a
// feature.
const MaxKinds = 12

const (
	lifecycleStartWithin  = 15 * time.Minute
	lifecycleFinishWithin = time.Hour
)

var lifecycleScope = []ScopeKind{ScopeConversation}

// registry is the kind table, in Kinds() order.
var registry = []Spec{
	{
		Kind: KindMessage, Class: ClassContent, Lane: LaneConversation,
		DefaultScope:    ScopeConversation,
		Scopes:          []ScopeKind{ScopeConversation, ScopeContext, ScopeSession},
		When:            []When{WhenBoundary, WhenAfterTurn, WhenInterrupt, WhenIfIdle, WhenNow, WhenSteer},
		PayloadRequired: true, PayloadAllowed: true, AllowsAfter: true,
		StartWithin: 24 * time.Hour, FinishWithin: 24*time.Hour + 10*time.Minute,
		DefaultPolicy: Policy{
			Guarantee: GuaranteeAtLeastOnce,
			Message:   MessagePolicy{When: WhenBoundary, Wake: WakeIfNeeded, Role: RolePrompt, OnUnsupported: FallbackBoundary},
		},
		OnConversationClose: CloseRuleSkip,
	},
	{
		Kind: KindInterrupt, Class: ClassControl, Lane: LaneConversation,
		DefaultScope: ScopeSession, Scopes: []ScopeKind{ScopeSession}, TurnScoped: true,
		StartWithin: time.Minute, FinishWithin: 3 * time.Minute,
		DefaultPolicy: Policy{
			Guarantee: GuaranteeAtLeastOnce,
			Interrupt: InterruptPolicy{OnFailure: OnFailureFail},
		},
		OnConversationClose: CloseRuleSkip,
	},
	{
		Kind: KindRespond, Class: ClassControl, Lane: LaneConversation,
		DefaultScope: ScopeSession, Scopes: []ScopeKind{ScopeSession}, InteractionScoped: true,
		PayloadRequired: true, PayloadAllowed: true,
		StartWithin: time.Hour, FinishWithin: time.Hour + 5*time.Minute,
		DefaultPolicy:       Policy{Guarantee: GuaranteeAtLeastOnce},
		OnConversationClose: CloseRuleSkip,
	},
	{
		Kind: KindCreate, Class: ClassLifecycle, Lane: LaneLifecycle,
		DefaultScope: ScopeConversation, Scopes: lifecycleScope,
		PayloadRequired: true, PayloadAllowed: true,
		StartWithin: lifecycleStartWithin, FinishWithin: lifecycleFinishWithin,
		OnConversationClose: CloseRuleSkip,
	},
	lifecycleSpec(KindWake, Policy{}),
	lifecycleSpec(KindSuspend, Policy{}),
	lifecycleSpec(KindResume, Policy{}),
	lifecycleSpec(KindRestart, Policy{Restart: RestartPolicy{Mode: RestartContinue}}),
	{
		Kind: KindKill, Class: ClassLifecycle, Lane: LaneLifecycle,
		DefaultScope: ScopeSession, Scopes: []ScopeKind{ScopeSession},
		AllowsAfter: true,
		StartWithin: lifecycleStartWithin, FinishWithin: lifecycleFinishWithin,
		OnConversationClose: CloseRuleSkip,
	},
	func() Spec {
		s := lifecycleSpec(KindClose, Policy{})
		s.OnConversationClose = CloseRuleSucceed
		// A close may carry its request's options (the API's
		// {"delete":true}), so the digest tells a close that deletes its row
		// from one that does not.
		s.PayloadAllowed = true
		return s
	}(),
}

// lifecycleSpec is the shared shape of the conversation-scoped lifecycle
// kinds, payload-free unless the kind's row says otherwise.
func lifecycleSpec(k Kind, defaults Policy) Spec {
	return Spec{
		Kind: k, Class: ClassLifecycle, Lane: LaneLifecycle,
		DefaultScope: ScopeConversation, Scopes: lifecycleScope,
		AllowsAfter: true,
		StartWithin: lifecycleStartWithin, FinishWithin: lifecycleFinishWithin,
		DefaultPolicy:       defaults,
		OnConversationClose: CloseRuleSkip,
	}
}

// Lookup returns the registry row for k, or ErrUnknownKind.
func Lookup(k Kind) (Spec, error) {
	for _, s := range registry {
		if s.Kind == k {
			s.Scopes = slices.Clone(s.Scopes)
			s.When = slices.Clone(s.When)
			return s, nil
		}
	}
	return Spec{}, fmt.Errorf("%w: %q", ErrUnknownKind, string(k))
}

// Kinds returns every registered kind in registry order.
func Kinds() []Kind {
	out := make([]Kind, len(registry))
	for i, s := range registry {
		out[i] = s.Kind
	}
	return out
}

// allowsScope reports whether the spec accepts scope kind k.
func (s Spec) allowsScope(k ScopeKind) bool { return slices.Contains(s.Scopes, k) }
