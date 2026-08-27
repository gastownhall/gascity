package main

import "github.com/gastownhall/gascity/internal/session"

type sessionLifecycleStatusHealSite string

const (
	sessionLifecycleStatusHealSiteOrphan  sessionLifecycleStatusHealSite = "orphan"
	sessionLifecycleStatusHealSiteDesired sessionLifecycleStatusHealSite = "desired"
)

type sessionLifecycleStatusComparisonOutcome string

const (
	sessionLifecycleStatusComparisonMatched      sessionLifecycleStatusComparisonOutcome = "matched"
	sessionLifecycleStatusComparisonMismatched   sessionLifecycleStatusComparisonOutcome = "mismatched"
	sessionLifecycleStatusComparisonIncomparable sessionLifecycleStatusComparisonOutcome = "incomparable"
)

type sessionLifecycleStatusComparisonReason string

const (
	sessionLifecycleStatusComparisonReasonEquivalent       sessionLifecycleStatusComparisonReason = "equivalent"
	sessionLifecycleStatusComparisonReasonPatchMismatch    sessionLifecycleStatusComparisonReason = "patch_mismatch"
	sessionLifecycleStatusComparisonReasonShadowParked     sessionLifecycleStatusComparisonReason = "shadow_parked"
	sessionLifecycleStatusComparisonReasonLegacyError      sessionLifecycleStatusComparisonReason = "legacy_error"
	sessionLifecycleStatusComparisonReasonCandidateInvalid sessionLifecycleStatusComparisonReason = "candidate_invalid"
)

type sessionLifecycleStatusComparison struct {
	Site        sessionLifecycleStatusHealSite
	Candidate   sessionLifecycleStatusPlan
	LegacyPatch session.MetadataPatch
	LegacyError string
	Outcome     sessionLifecycleStatusComparisonOutcome
	Reason      sessionLifecycleStatusComparisonReason
}

type sessionLifecycleStatusComparisonObserver func(sessionLifecycleStatusComparison)

func compareSessionLifecycleStatus(
	site sessionLifecycleStatusHealSite,
	candidate sessionLifecycleStatusPlan,
	legacyPatch session.MetadataPatch,
	legacyErr error,
) sessionLifecycleStatusComparison {
	comparison := sessionLifecycleStatusComparison{
		Site:        site,
		Candidate:   cloneSessionLifecycleStatusPlan(candidate),
		LegacyPatch: cloneSessionLifecycleStatusPatch(legacyPatch),
	}
	if legacyErr != nil {
		comparison.LegacyError = legacyErr.Error()
		comparison.Outcome = sessionLifecycleStatusComparisonIncomparable
		comparison.Reason = sessionLifecycleStatusComparisonReasonLegacyError
		return comparison
	}

	var expected session.MetadataPatch
	switch candidate.Outcome {
	case sessionLifecycleStatusNoop:
	case sessionLifecycleStatusHeal:
		expected = candidate.Patch
	case sessionLifecycleStatusPark:
		comparison.Outcome = sessionLifecycleStatusComparisonIncomparable
		comparison.Reason = sessionLifecycleStatusComparisonReasonShadowParked
		return comparison
	default:
		comparison.Outcome = sessionLifecycleStatusComparisonIncomparable
		comparison.Reason = sessionLifecycleStatusComparisonReasonCandidateInvalid
		return comparison
	}

	if sameSessionLifecycleStatusPatch(expected, legacyPatch) {
		comparison.Outcome = sessionLifecycleStatusComparisonMatched
		comparison.Reason = sessionLifecycleStatusComparisonReasonEquivalent
		return comparison
	}
	comparison.Outcome = sessionLifecycleStatusComparisonMismatched
	comparison.Reason = sessionLifecycleStatusComparisonReasonPatchMismatch
	return comparison
}

func cloneSessionLifecycleStatusPlan(plan sessionLifecycleStatusPlan) sessionLifecycleStatusPlan {
	cloned := plan
	cloned.Patch = cloneSessionLifecycleStatusPatch(plan.Patch)
	return cloned
}

func sameSessionLifecycleStatusPatch(left, right session.MetadataPatch) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		rightValue, ok := right[key]
		if !ok || rightValue != leftValue {
			return false
		}
	}
	return true
}
