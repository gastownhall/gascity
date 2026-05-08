package reviewquorum

import "strings"

var transientFailureReasons = map[string]struct{}{
	"rate_limited":           {},
	"provider_rate_limited":  {},
	"temporary_unavailable":  {},
	"provider_unavailable":   {},
	"provider_timeout":       {},
	"transport_interrupted":  {},
	"transient_provider_err": {},
}

// IsTransientFailure reports whether class/reason should be treated as a
// retryable or soft-failable reviewer-lane failure.
func IsTransientFailure(failureClass, failureReason string) bool {
	class := normalizeToken(failureClass)
	reason := normalizeToken(failureReason)
	if class == FailureClassTransient {
		return true
	}
	_, ok := transientFailureReasons[reason]
	return ok
}

// ClassifyFailure normalizes a lane failure into the durable failure contract.
func ClassifyFailure(failureClass, failureReason string) (class, reason string) {
	class = normalizeToken(failureClass)
	reason = normalizeToken(failureReason)
	if class == FailureClassNone && reason == "" {
		return FailureClassNone, ""
	}
	if reason == "" {
		reason = "unspecified"
	}
	if IsTransientFailure(class, reason) {
		return FailureClassTransient, reason
	}
	if class == FailureClassHard {
		return FailureClassHard, reason
	}
	if class == "" {
		return FailureClassHard, reason
	}
	return FailureClassHard, "invalid_failure_class"
}

func normalizeToken(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
