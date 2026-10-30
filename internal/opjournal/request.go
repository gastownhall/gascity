package opjournal

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxInlinePayload is the largest payload, in bytes, an op carries inline.
const MaxInlinePayload = 16 << 10

// maxTransitions caps an op's stored transition history.
const maxTransitions = 32

// normalizeRequest validates req against its kind and returns the op it
// proposes, with defaults applied, stamped at now, and without id, digest or
// status.
func normalizeRequest(req Request, spec Spec, now time.Time) (Op, error) {
	op := Op{
		Conversation: req.Conversation,
		Kind:         req.Kind,
		Executor:     orDefault(req.Executor, ExecutorLegacy),
		SchemaRev:    SchemaRev,
		ClientKey:    req.ClientKey,
		Payload:      req.Payload,
		Policy:       applyPolicyDefaults(req.Policy, spec),
		Scope:        req.Scope,
		After:        req.After,
		ClientSeq:    req.ClientSeq,
		NotBefore:    utc(req.NotBefore),
		StartBy:      utc(req.StartBy),
		FinishBy:     utc(req.FinishBy),
		Producer:     req.Producer,
		Causation:    req.Causation,
		Correlation:  req.Correlation,
		CreatedAt:    utc(now),
	}
	if op.Scope.Kind == "" {
		op.Scope.Kind = spec.DefaultScope
	}
	base := op.CreatedAt
	if op.NotBefore.After(base) {
		base = op.NotBefore
	}
	if op.StartBy.IsZero() {
		// A defaulted start_by never lands after an explicit finish_by, nor
		// before not_before; a finish_by before not_before stays refused.
		op.StartBy = base.Add(spec.StartWithin)
		if !op.FinishBy.IsZero() && op.FinishBy.Before(op.StartBy) {
			op.StartBy = op.FinishBy
			if op.StartBy.Before(base) {
				op.StartBy = base
			}
		}
	}
	if op.FinishBy.IsZero() {
		op.FinishBy = base.Add(spec.FinishWithin)
		if op.FinishBy.Before(op.StartBy) {
			op.FinishBy = op.StartBy
		}
	}
	if err := validateRequestShape(op, spec); err != nil {
		return Op{}, err
	}
	return op, nil
}

// utc strips the monotonic reading and the location so times round-trip.
func utc(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.Round(0).UTC()
}

// applyPolicyDefaults fills every empty field of the kind's own sub-policy and
// guarantee from the registry.
func applyPolicyDefaults(p Policy, spec Spec) Policy {
	d := spec.DefaultPolicy
	if p.Guarantee == "" {
		p.Guarantee = d.Guarantee
	}
	switch spec.Kind {
	case KindMessage:
		m := &p.Message
		m.When = orDefault(m.When, d.Message.When)
		m.Wake = orDefault(m.Wake, d.Message.Wake)
		m.Role = orDefault(m.Role, d.Message.Role)
		m.OnUnsupported = orDefault(m.OnUnsupported, d.Message.OnUnsupported)
	case KindInterrupt:
		p.Interrupt.OnFailure = orDefault(p.Interrupt.OnFailure, d.Interrupt.OnFailure)
	case KindRestart:
		p.Restart.Mode = orDefault(p.Restart.Mode, d.Restart.Mode)
	}
	return p
}

func orDefault[T ~string](v, d T) T {
	if v == "" {
		return d
	}
	return v
}

// validateRequestShape checks every producer-owned field of op against its
// kind. The policy must already be defaulted.
func validateRequestShape(op Op, spec Spec) error {
	if err := validateConversation(op.Conversation); err != nil {
		return err
	}
	if !op.Executor.Valid() {
		return invalidf("executor %q is not lane, legacy, legacy:api or legacy:cli", op.Executor)
	}
	if err := validateText("client key", op.ClientKey, maxClientKeyLen); err != nil {
		return err
	}
	if err := validateProducer(op.Producer); err != nil {
		return err
	}
	if err := validatePayload(op.Payload, spec); err != nil {
		return err
	}
	if err := validatePolicy(op.Policy, spec); err != nil {
		return err
	}
	if err := validateScope(op.Scope, spec); err != nil {
		return err
	}
	if spec.Kind == KindRespond {
		if err := validateRespondShape(op); err != nil {
			return err
		}
	}
	if op.After != "" {
		if !spec.AllowsAfter {
			return invalidf("%s ops take no after", spec.Kind)
		}
		if !validOpID(op.After) {
			return invalidf("after %q is not an op id", op.After)
		}
		if op.ID != "" && op.After == op.ID {
			return invalidf("op %s cannot come after itself", op.ID)
		}
	}
	if op.CreatedAt.IsZero() || op.StartBy.IsZero() || op.FinishBy.IsZero() {
		return invalidf("created_at, start_by and finish_by are required")
	}
	if !op.NotBefore.IsZero() && op.StartBy.Before(op.NotBefore) {
		return invalidf("start_by %s is before not_before %s", formatTime(op.StartBy), formatTime(op.NotBefore))
	}
	if op.FinishBy.Before(op.StartBy) {
		return invalidf("finish_by %s is before start_by %s", formatTime(op.FinishBy), formatTime(op.StartBy))
	}
	if err := validateOptionalText("causation", op.Causation); err != nil {
		return err
	}
	return validateOptionalText("correlation", op.Correlation)
}

func validateProducer(p Producer) error {
	if err := validateText("principal", p.Principal, maxPrincipalLen); err != nil {
		return err
	}
	if len(p.Actor) > maxActorLen || !actorRe.MatchString(p.Actor) {
		return invalidf("actor %q is not a token of [a-z0-9._:/-] up to %d bytes", p.Actor, maxActorLen)
	}
	if p.OwnerEpoch < 0 {
		return invalidf("owner epoch %d is negative", p.OwnerEpoch)
	}
	return nil
}

// ValidatePayload refuses a payload Propose would refuse for an op of kind,
// whatever the op's conversation: over MaxInlinePayload
// (ErrPayloadTooLarge), not valid UTF-8, empty for a kind that requires a
// payload, or set for a kind that takes none (ErrInvalidRequest). An unknown
// kind is ErrUnknownKind. Producers check a payload with it before they know
// the conversation.
func ValidatePayload(kind Kind, payload string) error {
	spec, err := Lookup(kind)
	if err != nil {
		return err
	}
	return validatePayload(payload, spec)
}

func validatePayload(payload string, spec Spec) error {
	if len(payload) > MaxInlinePayload {
		return fmt.Errorf("%w: %d bytes, max %d", ErrPayloadTooLarge, len(payload), MaxInlinePayload)
	}
	if !utf8.ValidString(payload) {
		return invalidf("payload is not valid UTF-8")
	}
	if spec.PayloadRequired && payload == "" {
		return invalidf("%s ops require a payload", spec.Kind)
	}
	if !spec.PayloadAllowed && payload != "" {
		return invalidf("%s ops take no payload", spec.Kind)
	}
	return nil
}

// validatePolicy requires the kind's own sub-policy to be complete and valid
// and every other sub-policy to be zero.
func validatePolicy(p Policy, spec Spec) error {
	rest := p
	rest.Guarantee = ""
	if spec.Lane == LaneConversation {
		if !slices.Contains(guarantees, p.Guarantee) {
			return invalidf("guarantee %q is not at_least_once or at_most_once", p.Guarantee)
		}
	} else if p.Guarantee != "" {
		return invalidf("%s ops take no guarantee", spec.Kind)
	}
	switch spec.Kind {
	case KindMessage:
		m := p.Message
		if !slices.Contains(spec.When, m.When) {
			return invalidf("message when %q is not one of %v", m.When, spec.When)
		}
		if !slices.Contains(wakes, m.Wake) {
			return invalidf("message wake %q is not if_needed or live_only", m.Wake)
		}
		if !slices.Contains(roles, m.Role) {
			return invalidf("message role %q is not prompt or notice", m.Role)
		}
		if !slices.Contains(fallbacks, m.OnUnsupported) {
			return invalidf("message on_unsupported %q is not boundary or fail", m.OnUnsupported)
		}
		if err := validateNowTarget(m); err != nil {
			return err
		}
		rest.Message = MessagePolicy{}
	case KindInterrupt:
		if !slices.Contains(onFailures, p.Interrupt.OnFailure) {
			return invalidf("interrupt on_failure %q is not fail or restart", p.Interrupt.OnFailure)
		}
		rest.Interrupt = InterruptPolicy{}
	case KindRestart:
		if !slices.Contains(restartModes, p.Restart.Mode) {
			return invalidf("restart mode %q is not continue or fresh", p.Restart.Mode)
		}
		rest.Restart = RestartPolicy{}
	case KindKill:
		if p.Kill.Grace < 0 {
			return invalidf("kill grace %s is negative", p.Kill.Grace)
		}
		rest.Kill = KillPolicy{}
	}
	if rest != (Policy{}) {
		return invalidf("%s ops carry another kind's policy: %+v", spec.Kind, rest)
	}
	return nil
}

// validateNowTarget checks a deliver-now target: only a when: now message
// names one, its generation is >= 1 when set, and a turn needs a generation.
func validateNowTarget(m MessagePolicy) error {
	if m.NowGeneration == 0 && m.NowTurn == "" {
		return nil
	}
	if m.When != WhenNow {
		return invalidf("now_gen and now_turn are valid only with when: now, not %q", m.When)
	}
	if m.NowGeneration < 0 {
		return invalidf("now_gen %d is negative", m.NowGeneration)
	}
	if m.NowTurn != "" {
		if m.NowGeneration < 1 {
			return invalidf("now_turn %q needs a now_gen >= 1", m.NowTurn)
		}
		if err := validateText("now turn", m.NowTurn, maxClientKeyLen); err != nil {
			return err
		}
	}
	return nil
}

func validateScope(s Scope, spec Spec) error {
	if !spec.allowsScope(s.Kind) {
		return invalidf("%s ops take scope %v, not %q", spec.Kind, spec.Scopes, s.Kind)
	}
	switch s.Kind {
	case ScopeConversation:
		if s != (Scope{Kind: ScopeConversation}) {
			return invalidf("conversation scope carries no epoch, generation, turn or interaction")
		}
	case ScopeContext:
		if s.Epoch < 1 || s.Generation != 0 || s.Turn != "" || s.Interaction != "" {
			return invalidf("context scope needs an epoch >= 1 and nothing else")
		}
	case ScopeSession:
		if s.Generation < 1 || s.Epoch != 0 {
			return invalidf("session scope needs a generation >= 1 and no epoch")
		}
		if s.Turn != "" {
			if !spec.TurnScoped {
				return invalidf("%s ops take no turn", spec.Kind)
			}
			if err := validateText("scope turn", s.Turn, maxClientKeyLen); err != nil {
				return err
			}
		}
		if spec.InteractionScoped {
			if err := validateText("scope interaction", s.Interaction, maxClientKeyLen); err != nil {
				return err
			}
		} else if s.Interaction != "" {
			return invalidf("%s ops take no interaction", spec.Kind)
		}
	}
	return nil
}

// validateOutcome checks a terminal outcome. cancel is the op's recorded
// cancel, which a canceled outcome requires.
func validateOutcome(o Outcome, cancel Cancel) error {
	if !o.Arm.Valid() {
		return invalidf("outcome arm %q is not succeeded, failed, skipped or canceled", o.Arm)
	}
	coalesced := o.Arm == ArmSkipped && o.Reason == ReasonCoalescedInto
	switch o.Arm {
	case ArmSucceeded:
		if o.Reason != "" {
			return invalidf("a succeeded outcome has no reason")
		}
		if o.Degradation != "" {
			if err := validateToken("degradation", o.Degradation); err != nil {
				return err
			}
		}
	case ArmFailed, ArmSkipped:
		if err := validateToken("outcome reason", o.Reason); err != nil {
			return err
		}
	case ArmCanceled:
		if o.Reason != "" {
			return invalidf("a canceled outcome has no reason; the cancel record carries it")
		}
		if !cancel.Recorded() {
			return invalidf("a canceled outcome needs a recorded cancel")
		}
	}
	if o.Arm != ArmSucceeded && o.Degradation != "" {
		return invalidf("only a succeeded outcome carries a degradation")
	}
	if coalesced != (o.Result.CoalescedInto != "") {
		return invalidf("result.coalesced_into is set exactly when the outcome is skipped{coalesced_into}")
	}
	if coalesced && !validOpID(o.Result.CoalescedInto) {
		return invalidf("coalesced_into %q is not an op id", o.Result.CoalescedInto)
	}
	r := o.Result
	if r.Generation < 0 || r.ContextEpoch < 0 {
		return invalidf("result generation and epoch must be >= 0")
	}
	if err := validateOptionalText("result turn", r.Turn); err != nil {
		return err
	}
	if err := validateOptionalText("result via", r.Via); err != nil {
		return err
	}
	return validateDeliveryResult(o)
}

// The runtime acks and link confidences this build knows. A stored value
// outside them is a newer build's, read as ErrUnsupportedValue.
var (
	ackKinds        = []AckKind{AckDelivered, AckDeliveredUnobserved, AckInBand}
	linkConfidences = []LinkConfidence{LinkExact, LinkCorrelated, LinkAmbiguous, LinkNone}
)

// maxMessageIDLen bounds a runtime's message id in the delivery record.
const maxMessageIDLen = 255

// validateDeliveryResult checks an outcome's delivery record and turn link:
// only a succeeded outcome carries them; a record has a seq, a time and a
// known ack; a link needs a record and a basis, and names a turn exactly when
// its confidence is not none.
func validateDeliveryResult(o Outcome) error {
	r := o.Result
	d := r.Delivery
	if !d.IsZero() {
		if o.Arm != ArmSucceeded {
			return invalidf("only a succeeded outcome carries a delivery record")
		}
		if d.Seq < 1 || d.At.IsZero() {
			return invalidf("a delivery record needs a seq >= 1 and a time")
		}
		if !slices.Contains(ackKinds, d.Ack) {
			return invalidf("delivery ack %q is not delivered, delivered_unobserved or in_band", d.Ack)
		}
		if !storableTime(d.At) || !storableTime(d.MessageAt) {
			return invalidf("delivery record times must fall in years 0 through 9999")
		}
		if d.MessageID != "" {
			if err := validateText("delivery message id", d.MessageID, maxMessageIDLen); err != nil {
				return err
			}
		}
	}
	if r.TurnLink == "" {
		if r.TurnBasis != "" {
			return invalidf("a turn basis needs a turn link")
		}
		return nil
	}
	if d.IsZero() {
		return invalidf("a turn link needs a delivery record")
	}
	if !slices.Contains(linkConfidences, r.TurnLink) {
		return invalidf("turn link %q is not exact, correlated, ambiguous or none", r.TurnLink)
	}
	if err := validateToken("turn basis", string(r.TurnBasis)); err != nil {
		return err
	}
	if (r.TurnLink == LinkNone) != (r.Turn == "") {
		return invalidf("a %s turn link names a turn exactly when it is not none", r.TurnLink)
	}
	return nil
}

// validateState checks the platform-owned fields of op.
func validateState(op Op, spec Spec) error {
	if !op.Status.Valid() {
		return invalidf("status %q is not a known status", op.Status)
	}
	if spec.Lane == LaneLifecycle && op.Status == StatusExecuting {
		return invalidf("lifecycle ops never execute")
	}
	if op.Executor.Legacy() && op.Status != StatusSubmitted && op.Status != StatusClosed {
		return invalidf("a %s op is %s; a legacy op stays submitted until it closes", op.Executor, op.Status)
	}
	if op.Status == StatusClosed {
		if err := validateOutcome(op.Outcome, op.Cancel); err != nil {
			return err
		}
	} else if op.Outcome != (Outcome{}) {
		return invalidf("an op in %s has no outcome", op.Status)
	}
	if !op.Lease.IsZero() {
		if err := op.Lease.validate(); err != nil {
			return err
		}
		if spec.Lane == LaneLifecycle {
			return invalidf("lifecycle ops are never leased")
		}
		if op.Executor.Legacy() {
			return invalidf("a %s op carries no lease; only lanes lease, and only lane ops", op.Executor)
		}
		if op.Lease.Lane != string(spec.Lane) || op.Lease.Conversation != op.Conversation {
			return invalidf("lease %s is not the %s lane's lease on conversation %s", op.Lease, spec.Lane, op.Conversation)
		}
	}
	if op.Attempt < 0 {
		return invalidf("attempt %d is negative", op.Attempt)
	}
	if op.Cancel.Recorded() {
		if err := validateText("cancel by", op.Cancel.By, maxPrincipalLen); err != nil {
			return err
		}
		if op.Cancel.At.IsZero() {
			return invalidf("a recorded cancel needs a time")
		}
		if err := validateOptionalText("cancel reason", op.Cancel.Reason); err != nil {
			return err
		}
	} else if op.Cancel != (Cancel{}) {
		return invalidf("a cancel without by is not recorded")
	}
	if err := validateLaneState(op, spec); err != nil {
		return err
	}
	if len(op.Transitions) > maxTransitions {
		return invalidf("%d transitions, max %d", len(op.Transitions), maxTransitions)
	}
	for _, t := range op.Transitions {
		if !t.Status.Valid() || t.At.IsZero() {
			return invalidf("transition %+v needs a known status and a time", t)
		}
		if err := validateText("transition by", t.By, maxPrincipalLen); err != nil {
			return err
		}
	}
	return nil
}

// maxLastErrorLen bounds gc.op.last_error.
const maxLastErrorLen = 512

// validateLaneState checks the conversation lane's bookkeeping fields: only a
// conversation-lane lane op carries them, and each is in range.
func validateLaneState(op Op, spec Spec) error {
	laneFields := !op.RetryAt.IsZero() || op.Failures != 0 || op.MaybeDelivered != 0 ||
		op.DeliveryKey != "" || op.LastError != "" ||
		!op.Outcome.Result.Delivery.IsZero() || op.Outcome.Result.TurnLink != ""
	if !laneFields {
		return nil
	}
	if op.Executor.Legacy() || spec.Lane != LaneConversation {
		return invalidf("a %s %s op carries no conversation-lane bookkeeping", op.Executor, op.Kind)
	}
	if op.Failures < 0 {
		return invalidf("failures %d is negative", op.Failures)
	}
	if op.MaybeDelivered < 0 || op.MaybeDelivered > op.Attempt {
		return invalidf("maybe_delivered %d is outside 0..attempt (%d)", op.MaybeDelivered, op.Attempt)
	}
	if op.DeliveryKey != "" {
		if err := validateDeliveryKey(op.DeliveryKey); err != nil {
			return err
		}
	}
	if op.LastError != "" {
		return validateLastError(op.LastError)
	}
	return nil
}

// validateDeliveryKey checks a delivery key: <op id>:<n>, n a canonical
// integer >= 1.
func validateDeliveryKey(key string) error {
	head, n, ok := splitDeliveryKey(key)
	if !ok || !validOpID(head) || n < 1 {
		return invalidf("delivery key %q is not <op id>:<n> with n >= 1", key)
	}
	return nil
}

// splitDeliveryKey splits a delivery key into its head op id and number. ok
// is false when the number is not a canonical integer.
func splitDeliveryKey(key string) (head string, n int64, ok bool) {
	i := strings.LastIndexByte(key, ':')
	if i < 0 {
		return "", 0, false
	}
	head, num := key[:i], key[i+1:]
	v, err := strconv.ParseInt(num, 10, 64)
	if err != nil || strconv.FormatInt(v, 10) != num {
		return "", 0, false
	}
	return head, v, true
}

// validateLastError checks gc.op.last_error: <token>[: <text>], at most
// maxLastErrorLen bytes of UTF-8 without control characters.
func validateLastError(v string) error {
	switch {
	case len(v) > maxLastErrorLen:
		return invalidf("last error is %d bytes, max %d", len(v), maxLastErrorLen)
	case !utf8.ValidString(v):
		return invalidf("last error is not valid UTF-8")
	case hasControl(v):
		return invalidf("last error contains control characters")
	}
	token, _, _ := strings.Cut(v, ": ")
	return validateToken("last error token", token)
}

// appendTransition appends t, keeping the first transition and the most
// recent ones within maxTransitions.
func appendTransition(ts []Transition, t Transition) []Transition {
	out := append(slices.Clone(ts), t)
	if len(out) > maxTransitions {
		out = append(out[:1:1], out[len(out)-(maxTransitions-1):]...)
	}
	return out
}
