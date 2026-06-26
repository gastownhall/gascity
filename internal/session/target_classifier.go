package session

import "fmt"

// Session target classification, extracted from the API resolver's
// precedence ladder (SESSION-ID-003/004 plus the API-level steps around
// them). Pure decision ladder over caller-gathered lookup facts: the caller
// owns the store lookups, config access, and error values; this package
// owns the precedence order and the terminal classification.
//
// Lookup facts are gathered on demand, exactly like the lifecycle-timer and
// exit deciders: the classifier returns a gather action naming the next
// vector, the caller performs that one lookup and decides again. That
// preserves the resolver's short-circuit I/O — an exact-ID hit never runs
// the live scan, a live hit never runs the path-alias scan.
//
// The classifier is side-effect free and holds no IDs or errors; the caller
// keeps the payload for whichever step the decision names as Source.
//
// Two result kinds from the long-form design are deliberately absent.
// Ambiguity is not a distinct kind: ambiguous lookups surface as the step
// error the caller holds (session.ErrAmbiguous, configured-name conflicts),
// preserving the existing conflict projections. The repair-pending
// diagnostic is deferred with the repair-lifecycle work: today's gather
// lookups still normalize empty-type beads exactly as the inline resolver
// did, and that baseline moves only when the read-path repair fix lands.

// TargetLookupFact is the gathered outcome of one lookup vector: Unknown
// until gathered, then match, no-match, or error.
type TargetLookupFact int

// Lookup fact states.
const (
	LookupUnknown TargetLookupFact = iota
	LookupMatch
	LookupNoMatch
	LookupError
)

// TargetNamedFact is the outcome of the configured named-session lookup,
// which has four states: its "matched" flag makes some errors terminal even
// when the identity ultimately resolves nowhere.
type TargetNamedFact int

// Configured named-session lookup fact states.
const (
	NamedLookupUnknown TargetNamedFact = iota
	NamedLookupMatch
	// NamedLookupTerminalError covers both lookup errors and matched-but-
	// unresolvable outcomes (conflicts, reserved identities on
	// non-materializing surfaces): the ladder must not fall through to
	// ordinary live matching.
	NamedLookupTerminalError
	NamedLookupNoMatch
)

// TargetStep identifies one lookup vector of the precedence ladder.
type TargetStep int

// Precedence ladder steps, in order.
const (
	TargetStepNone TargetStep = iota
	TargetStepExactID
	TargetStepConfiguredName
	TargetStepLive
	TargetStepPathAlias
	TargetStepClosedNamedSpec
	TargetStepClosed
)

// String names the precedence step so diagnostics and test failures read as
// ladder steps rather than bare ints.
func (s TargetStep) String() string {
	switch s {
	case TargetStepNone:
		return "none"
	case TargetStepExactID:
		return "exact-id"
	case TargetStepConfiguredName:
		return "configured-name"
	case TargetStepLive:
		return "live"
	case TargetStepPathAlias:
		return "path-alias"
	case TargetStepClosedNamedSpec:
		return "closed-named-spec"
	case TargetStepClosed:
		return "closed"
	default:
		return fmt.Sprintf("TargetStep(%d)", int(s))
	}
}

// TargetAction is what the caller must do next.
type TargetAction int

// Classifier actions.
const (
	// TargetGather means perform the lookup named by Gather and decide
	// again with the fact filled in.
	TargetGather TargetAction = iota
	// TargetDone means the classification is terminal; act on Result.
	TargetDone
)

// TargetResultKind is the terminal classification of a target token.
type TargetResultKind int

// Terminal result kinds.
const (
	// TargetSelected: the step named by Source matched; the caller returns
	// the ID it held for that step.
	TargetSelected TargetResultKind = iota
	// TargetNotFound: no surface-legal candidate.
	TargetNotFound
	// TargetRejectedByConfig: a live named-session bead matched but its
	// configured identity is absent from current config.
	TargetRejectedByConfig
	// TargetError: the step named by Source failed; the caller returns the
	// error it held for that step.
	TargetError
)

// TargetFacts are the inputs for classifying one target token on one
// surface. Lookup fields start Unknown and are gathered on demand.
type TargetFacts struct {
	// TemplateForm reports the token parses as a template:<name> factory
	// target, which is never a live session target.
	TemplateForm bool
	// AllowClosed reports the surface may fall through to closed lookup
	// after every live vector misses.
	AllowClosed bool
	// ExactID is the direct session bead ID lookup.
	ExactID TargetLookupFact
	// ConfiguredName is the configured named-session lookup.
	ConfiguredName TargetNamedFact
	// Live is the ordinary live lookup: open exact session_name, then open
	// exact current alias.
	Live TargetLookupFact
	// LiveConfigOrphan reports the live match is a named-session bead whose
	// configured identity is absent from current config. Meaningful only
	// when Live is LookupMatch.
	LiveConfigOrphan bool
	// PathAlias is the live path-alias (Title) lookup.
	PathAlias TargetLookupFact
	// ClosedNamedSpec reports whether the token names a configured
	// named-session spec; on allow-closed surfaces a match rejects the
	// token before closed lookup runs.
	ClosedNamedSpec TargetLookupFact
	// Closed is the closed-session lookup: closed session_name, then
	// closed alias.
	Closed TargetLookupFact
}

// TargetDecision is the outcome of one classification pass.
type TargetDecision struct {
	// Action is what the caller must do next.
	Action TargetAction
	// Gather names the lookup to perform when Action is TargetGather.
	Gather TargetStep
	// Result is the terminal classification when Action is TargetDone.
	Result TargetResultKind
	// Source names the step that produced a terminal Selected or Error
	// result, so the caller can return the ID or error it held for it.
	Source TargetStep
}

// DecideSessionTarget classifies a session target token. It performs no
// I/O; lookups it needs are requested one at a time via gather actions.
func DecideSessionTarget(f TargetFacts) TargetDecision {
	if f.TemplateForm {
		return targetDone(TargetNotFound, TargetStepNone)
	}
	switch f.ExactID {
	case LookupUnknown:
		return targetGather(TargetStepExactID)
	case LookupMatch:
		return targetDone(TargetSelected, TargetStepExactID)
	case LookupError:
		return targetDone(TargetError, TargetStepExactID)
	}
	switch f.ConfiguredName {
	case NamedLookupUnknown:
		return targetGather(TargetStepConfiguredName)
	case NamedLookupMatch:
		return targetDone(TargetSelected, TargetStepConfiguredName)
	case NamedLookupTerminalError:
		return targetDone(TargetError, TargetStepConfiguredName)
	}
	switch f.Live {
	case LookupUnknown:
		return targetGather(TargetStepLive)
	case LookupMatch:
		if f.LiveConfigOrphan {
			return targetDone(TargetRejectedByConfig, TargetStepLive)
		}
		return targetDone(TargetSelected, TargetStepLive)
	case LookupError:
		return targetDone(TargetError, TargetStepLive)
	}
	switch f.PathAlias {
	case LookupUnknown:
		return targetGather(TargetStepPathAlias)
	case LookupMatch:
		return targetDone(TargetSelected, TargetStepPathAlias)
	case LookupError:
		return targetDone(TargetError, TargetStepPathAlias)
	}
	if !f.AllowClosed {
		return targetDone(TargetNotFound, TargetStepNone)
	}
	switch f.ClosedNamedSpec {
	case LookupUnknown:
		return targetGather(TargetStepClosedNamedSpec)
	case LookupMatch:
		return targetDone(TargetNotFound, TargetStepClosedNamedSpec)
	case LookupError:
		return targetDone(TargetError, TargetStepClosedNamedSpec)
	}
	switch f.Closed {
	case LookupUnknown:
		return targetGather(TargetStepClosed)
	case LookupMatch:
		return targetDone(TargetSelected, TargetStepClosed)
	case LookupError:
		return targetDone(TargetError, TargetStepClosed)
	}
	return targetDone(TargetNotFound, TargetStepNone)
}

func targetGather(step TargetStep) TargetDecision {
	return TargetDecision{Action: TargetGather, Gather: step}
}

func targetDone(result TargetResultKind, source TargetStep) TargetDecision {
	return TargetDecision{Action: TargetDone, Result: result, Source: source}
}
