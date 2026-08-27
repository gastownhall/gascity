package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/session"
)

type sessionLifecycleStartShadowInput struct {
	Info                 session.Info
	WakeDecisionObserved bool
	ShouldWake           bool
	ConfigSuppressed     bool
	RuntimeObserved      bool
	RuntimeAlive         bool
	ObservedAt           time.Time
	StartupTimeout       time.Duration
	CircuitOpen          bool
	ProviderUnavailable  bool
	ShadowAdmitted       bool
	ShadowAdmission      sessionLifecycleStartShadowAdmission
}

// sessionLifecycleStartShadowAdmission is the immutable trace-arm token that
// admitted an observation during the synchronous legacy reconcile cycle.
type sessionLifecycleStartShadowAdmission struct {
	Template  string
	Source    TraceSource
	ExpiresAt time.Time
}

// sessionLifecycleStartShadowObservation is the immutable handoff from the
// serial legacy selector to the keyed shadow worker. LegacySelected is copied
// from the exact candidate set the legacy reconciler produced; the shadow does
// not repeat circuit-breaker or provider-health reads.
type sessionLifecycleStartShadowObservation struct {
	Input          sessionLifecycleStartShadowInput
	LegacySelected bool
	Admission      sessionLifecycleStartShadowAdmission
}

func newAdmittedSessionLifecycleStartShadowObservation(
	input sessionLifecycleStartShadowInput,
	legacySelected bool,
	admission sessionLifecycleStartShadowAdmission,
) sessionLifecycleStartShadowObservation {
	observation := newSessionLifecycleStartShadowObservation(input, legacySelected)
	observation.Admission = admission
	return observation
}

func newSessionLifecycleStartShadowObservation(
	input sessionLifecycleStartShadowInput,
	legacySelected bool,
) sessionLifecycleStartShadowObservation {
	return sessionLifecycleStartShadowObservation{
		Input:          cloneSessionLifecycleStartShadowInput(input),
		LegacySelected: legacySelected,
	}
}

func cloneSessionLifecycleStartShadowInput(input sessionLifecycleStartShadowInput) sessionLifecycleStartShadowInput {
	cloned := input
	cloned.Info.Labels = append([]string(nil), input.Info.Labels...)
	cloned.Info.AliasHistory = append([]string(nil), input.Info.AliasHistory...)
	return cloned
}

type sessionLifecycleStartSelectionOutcome uint8

const (
	sessionLifecycleStartSelectionUnknown sessionLifecycleStartSelectionOutcome = iota
	sessionLifecycleStartSelectionNoop
	sessionLifecycleStartSelectionPrepare
	sessionLifecycleStartSelectionPark
)

type sessionLifecycleStartSelectionReason string

const (
	sessionLifecycleStartSelectionReasonUnknown             sessionLifecycleStartSelectionReason = ""
	sessionLifecycleStartSelectionReasonInvalidInput        sessionLifecycleStartSelectionReason = "invalid_input"
	sessionLifecycleStartSelectionReasonTerminal            sessionLifecycleStartSelectionReason = "terminal"
	sessionLifecycleStartSelectionReasonWakeUnknown         sessionLifecycleStartSelectionReason = "wake_unknown"
	sessionLifecycleStartSelectionReasonRuntimeUnknown      sessionLifecycleStartSelectionReason = "runtime_unknown"
	sessionLifecycleStartSelectionReasonObservationUnknown  sessionLifecycleStartSelectionReason = "observation_unknown"
	sessionLifecycleStartSelectionReasonConfigSuppressed    sessionLifecycleStartSelectionReason = "config_suppressed"
	sessionLifecycleStartSelectionReasonNotNeeded           sessionLifecycleStartSelectionReason = "not_needed"
	sessionLifecycleStartSelectionReasonAlreadyRunning      sessionLifecycleStartSelectionReason = "already_running"
	sessionLifecycleStartSelectionReasonFailedCreate        sessionLifecycleStartSelectionReason = "failed_create"
	sessionLifecycleStartSelectionReasonQuarantined         sessionLifecycleStartSelectionReason = "quarantined"
	sessionLifecycleStartSelectionReasonStartInFlight       sessionLifecycleStartSelectionReason = "start_in_flight"
	sessionLifecycleStartSelectionReasonCircuitOpen         sessionLifecycleStartSelectionReason = "circuit_open"
	sessionLifecycleStartSelectionReasonProviderUnavailable sessionLifecycleStartSelectionReason = "provider_unavailable"
	sessionLifecycleStartSelectionReasonReady               sessionLifecycleStartSelectionReason = "ready"
)

type sessionLifecycleStartSelectionPlan struct {
	SessionID string
	Outcome   sessionLifecycleStartSelectionOutcome
	Reason    sessionLifecycleStartSelectionReason
}

func planSessionLifecycleStartSelection(input sessionLifecycleStartShadowInput) sessionLifecycleStartSelectionPlan {
	plan := sessionLifecycleStartSelectionPlan{SessionID: input.Info.ID}
	switch {
	case input.Info.ID == "" || strings.TrimSpace(input.Info.ID) != input.Info.ID:
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonInvalidInput
	case input.Info.Closed:
		plan.Outcome = sessionLifecycleStartSelectionNoop
		plan.Reason = sessionLifecycleStartSelectionReasonTerminal
	case !input.WakeDecisionObserved:
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonWakeUnknown
	case !input.RuntimeObserved:
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonRuntimeUnknown
	case input.ObservedAt.IsZero():
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonObservationUnknown
	case input.ConfigSuppressed:
		plan.Outcome = sessionLifecycleStartSelectionNoop
		plan.Reason = sessionLifecycleStartSelectionReasonConfigSuppressed
	case !input.ShouldWake:
		plan.Outcome = sessionLifecycleStartSelectionNoop
		plan.Reason = sessionLifecycleStartSelectionReasonNotNeeded
	case input.RuntimeAlive:
		plan.Outcome = sessionLifecycleStartSelectionNoop
		plan.Reason = sessionLifecycleStartSelectionReasonAlreadyRunning
	case isFailedCreateSessionInfo(input.Info):
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonFailedCreate
	case sessionIsQuarantinedInfo(input.Info, &clock.Fake{Time: input.ObservedAt}):
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonQuarantined
	case pendingCreateStartInFlightInfo(input.Info, &clock.Fake{Time: input.ObservedAt}, input.StartupTimeout):
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonStartInFlight
	case input.CircuitOpen:
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonCircuitOpen
	case input.ProviderUnavailable:
		plan.Outcome = sessionLifecycleStartSelectionPark
		plan.Reason = sessionLifecycleStartSelectionReasonProviderUnavailable
	default:
		plan.Outcome = sessionLifecycleStartSelectionPrepare
		plan.Reason = sessionLifecycleStartSelectionReasonReady
	}
	return plan
}

type sessionLifecycleStartSelectionComparisonOutcome string

const (
	sessionLifecycleStartSelectionComparisonMatched      sessionLifecycleStartSelectionComparisonOutcome = "matched"
	sessionLifecycleStartSelectionComparisonMismatched   sessionLifecycleStartSelectionComparisonOutcome = "mismatched"
	sessionLifecycleStartSelectionComparisonIncomparable sessionLifecycleStartSelectionComparisonOutcome = "incomparable"
)

type sessionLifecycleStartSelectionComparisonReason string

const (
	sessionLifecycleStartSelectionComparisonReasonEquivalent        sessionLifecycleStartSelectionComparisonReason = "equivalent"
	sessionLifecycleStartSelectionComparisonReasonSelectionMismatch sessionLifecycleStartSelectionComparisonReason = "selection_mismatch"
	sessionLifecycleStartSelectionComparisonReasonShadowUnknown     sessionLifecycleStartSelectionComparisonReason = "shadow_unknown"
	sessionLifecycleStartSelectionComparisonReasonCandidateInvalid  sessionLifecycleStartSelectionComparisonReason = "candidate_invalid"
)

type sessionLifecycleStartSelectionComparison struct {
	Plan           sessionLifecycleStartSelectionPlan
	LegacySelected bool
	Outcome        sessionLifecycleStartSelectionComparisonOutcome
	Reason         sessionLifecycleStartSelectionComparisonReason
}

type sessionLifecycleStartSelectionComparisonObserver func(sessionLifecycleStartSelectionComparison)

func compareSessionLifecycleStartSelection(
	plan sessionLifecycleStartSelectionPlan,
	legacySelected bool,
) sessionLifecycleStartSelectionComparison {
	comparison := sessionLifecycleStartSelectionComparison{
		Plan:           plan,
		LegacySelected: legacySelected,
	}
	expected, comparable, valid := sessionLifecycleStartSelectionExpectation(plan)
	if !valid {
		comparison.Outcome = sessionLifecycleStartSelectionComparisonIncomparable
		comparison.Reason = sessionLifecycleStartSelectionComparisonReasonCandidateInvalid
		return comparison
	}
	if !comparable {
		comparison.Outcome = sessionLifecycleStartSelectionComparisonIncomparable
		comparison.Reason = sessionLifecycleStartSelectionComparisonReasonShadowUnknown
		return comparison
	}
	if expected == legacySelected {
		comparison.Outcome = sessionLifecycleStartSelectionComparisonMatched
		comparison.Reason = sessionLifecycleStartSelectionComparisonReasonEquivalent
		return comparison
	}
	comparison.Outcome = sessionLifecycleStartSelectionComparisonMismatched
	comparison.Reason = sessionLifecycleStartSelectionComparisonReasonSelectionMismatch
	return comparison
}

func sessionLifecycleStartSelectionExpectation(plan sessionLifecycleStartSelectionPlan) (selected, comparable, valid bool) {
	switch plan.Outcome {
	case sessionLifecycleStartSelectionPrepare:
		return true, true, plan.Reason == sessionLifecycleStartSelectionReasonReady
	case sessionLifecycleStartSelectionNoop:
		switch plan.Reason {
		case sessionLifecycleStartSelectionReasonTerminal,
			sessionLifecycleStartSelectionReasonConfigSuppressed,
			sessionLifecycleStartSelectionReasonNotNeeded,
			sessionLifecycleStartSelectionReasonAlreadyRunning:
			return false, true, true
		default:
			return false, false, false
		}
	case sessionLifecycleStartSelectionPark:
		switch plan.Reason {
		case sessionLifecycleStartSelectionReasonFailedCreate,
			sessionLifecycleStartSelectionReasonQuarantined,
			sessionLifecycleStartSelectionReasonStartInFlight,
			sessionLifecycleStartSelectionReasonCircuitOpen,
			sessionLifecycleStartSelectionReasonProviderUnavailable:
			return false, true, true
		case sessionLifecycleStartSelectionReasonInvalidInput,
			sessionLifecycleStartSelectionReasonWakeUnknown,
			sessionLifecycleStartSelectionReasonRuntimeUnknown,
			sessionLifecycleStartSelectionReasonObservationUnknown:
			return false, false, true
		default:
			return false, false, false
		}
	default:
		return false, false, false
	}
}
