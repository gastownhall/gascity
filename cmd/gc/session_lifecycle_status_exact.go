package main

import (
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// exactSessionLifecycleStatusReason describes why an exact-key status shadow
// result is usable or deliberately parked. It never claims legacy parity.
type exactSessionLifecycleStatusReason string

const (
	exactSessionLifecycleStatusReasonCandidate               exactSessionLifecycleStatusReason = "candidate"
	exactSessionLifecycleStatusReasonInvalidInput            exactSessionLifecycleStatusReason = "invalid_input"
	exactSessionLifecycleStatusReasonPrerequisiteUnavailable exactSessionLifecycleStatusReason = "prerequisite_unavailable"
	exactSessionLifecycleStatusReasonNotObserved             exactSessionLifecycleStatusReason = "not_observed"
	exactSessionLifecycleStatusReasonObservationUnavailable  exactSessionLifecycleStatusReason = "observation_unavailable"
)

type exactSessionLifecycleStatusContext uint8

const (
	exactSessionLifecycleStatusContextUnavailable exactSessionLifecycleStatusContext = iota
	exactSessionLifecycleStatusContextDesired
)

type exactSessionLifecycleStatusDisposition string

const (
	exactSessionLifecycleStatusDispositionCandidate exactSessionLifecycleStatusDisposition = "candidate"
	exactSessionLifecycleStatusDispositionPark      exactSessionLifecycleStatusDisposition = "park"
)

// exactSessionLifecycleStatusResult is a detached report from one authoritative
// exact-key read, at most one runtime observation, and an optional fenced heal.
type exactSessionLifecycleStatusResult struct {
	Admission            sessionStartAdmission
	AdmissionVersion     uint64
	ControllerGeneration uint64
	RequestedID          string
	LoadedID             string
	LoadedRevision       int64
	Context              exactSessionLifecycleStatusContext
	ObservedAt           time.Time
	RuntimeLive          bool
	Disposition          exactSessionLifecycleStatusDisposition
	Reason               exactSessionLifecycleStatusReason
	Plan                 *sessionLifecycleStatusPlan
	EffectApplied        bool
	Error                string
}

// exactSessionLifecycleStatusObserver receives a detached exact-status result.
// Its return value is intentionally absent: it cannot affect start ownership.
type exactSessionLifecycleStatusObserver func(exactSessionLifecycleStatusResult)

// exactSessionLifecycleStatusInput is the immutable evidence used by the
// pure exact-key status evaluator.
type exactSessionLifecycleStatusInput struct {
	Admission            sessionStartAdmission
	ControllerGeneration uint64
	RequestedID          string
	LoadedRevision       int64
	Context              exactSessionLifecycleStatusContext
	Info                 session.Info
	Observation          worker.LiveObservation
	ObservedAt           time.Time
	StartupTimeout       time.Duration
	HealInputsRowBacked  bool
	UnavailableReason    exactSessionLifecycleStatusReason
	Error                string
}

// evaluateExactSessionLifecycleStatus derives a status candidate only for a
// desired-session observation. It owns no store or provider.
func evaluateExactSessionLifecycleStatus(input exactSessionLifecycleStatusInput) exactSessionLifecycleStatusResult {
	result := exactSessionLifecycleStatusResult{
		Admission:            input.Admission,
		AdmissionVersion:     input.Admission.Version,
		ControllerGeneration: input.ControllerGeneration,
		RequestedID:          input.RequestedID,
		LoadedID:             input.Info.ID,
		LoadedRevision:       input.LoadedRevision,
		Context:              input.Context,
		ObservedAt:           input.ObservedAt,
		Disposition:          exactSessionLifecycleStatusDispositionPark,
		Reason:               exactSessionLifecycleStatusReasonInvalidInput,
		Error:                input.Error,
	}
	if input.RequestedID == "" {
		result.RequestedID = input.Admission.SessionID
	}
	if input.ControllerGeneration == 0 || input.Admission.Version == 0 ||
		input.Admission.SessionID != input.RequestedID || input.Info.ID == "" ||
		input.RequestedID != input.Info.ID || strings.TrimSpace(input.Info.ID) != input.Info.ID {
		return result
	}
	if input.Info.Closed {
		if input.Context != exactSessionLifecycleStatusContextUnavailable ||
			!input.ObservedAt.IsZero() || input.UnavailableReason != "" || input.Error != "" {
			return result
		}
		plan := planSessionLifecycleStatus(sessionLifecycleShadowInput{Info: input.Info})
		result.Disposition = exactSessionLifecycleStatusDispositionCandidate
		result.Reason = exactSessionLifecycleStatusReasonCandidate
		result.Plan = ptrExactSessionLifecycleStatusPlan(plan)
		return result
	}
	if input.Context == exactSessionLifecycleStatusContextUnavailable {
		if input.UnavailableReason != "" {
			result.Reason = input.UnavailableReason
		}
		return result
	}
	if input.Context != exactSessionLifecycleStatusContextDesired || input.UnavailableReason != "" ||
		input.ObservedAt.IsZero() || input.Error != "" {
		return result
	}
	result.RuntimeLive = runtimeObservationLive(input.Observation)

	plan := planSessionLifecycleStatus(sessionLifecycleShadowInput{
		Info:              input.Info,
		RuntimeObserved:   true,
		RuntimeAlive:      input.Observation.Alive,
		ObservedAt:        input.ObservedAt,
		StartupTimeout:    input.StartupTimeout,
		RollbackAvailable: true,
	})
	if plan.Outcome == sessionLifecycleStatusHeal && (!input.HealInputsRowBacked || input.LoadedRevision == 0) {
		result.Reason = exactSessionLifecycleStatusReasonPrerequisiteUnavailable
		return result
	}
	result.Reason = exactSessionLifecycleStatusReasonCandidate
	result.Disposition = exactSessionLifecycleStatusDispositionCandidate
	result.Plan = ptrExactSessionLifecycleStatusPlan(plan)
	return result
}

func ptrExactSessionLifecycleStatusPlan(plan sessionLifecycleStatusPlan) *sessionLifecycleStatusPlan {
	cloned := cloneSessionLifecycleStatusPlan(plan)
	return &cloned
}

const exactSessionLifecycleStatusPanicDiagnosticLimit = 4096

func reportExactSessionLifecycleStatus(stderr io.Writer, observer exactSessionLifecycleStatusObserver, result exactSessionLifecycleStatusResult) {
	if observer == nil {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			prefix := fmt.Sprintf(
				"exact session lifecycle status observer panicked for %s version %d: %v\n",
				result.Admission.SessionID,
				result.Admission.Version,
				recovered,
			)
			stack := debug.Stack()
			remaining := exactSessionLifecycleStatusPanicDiagnosticLimit - len(prefix)
			if remaining < 0 {
				prefix = prefix[:exactSessionLifecycleStatusPanicDiagnosticLimit]
				remaining = 0
			}
			if len(stack) > remaining {
				stack = stack[:remaining]
			}
			fmt.Fprint(stderr, prefix, string(stack)) //nolint:errcheck // observer diagnostics must not affect reconciliation
		}
	}()
	observer(result)
}
