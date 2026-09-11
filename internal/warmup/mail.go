package warmup

import "github.com/gastownhall/gascity/internal/doctor"

// CustomWarmupMail is an optional interface implemented by warm-up-
// eligible doctor checks that need to override the runner's default
// mail subject + body when this check is the sole producer of
// failures in a warm-up cycle.
//
// The runner type-asserts each unique check name found in
// WarmupReport.Failures. When EXACTLY ONE check name accounts for
// every failure AND that check's instance implements
// CustomWarmupMail, the runner calls SoleFailureMail and uses the
// returned (subject, body) verbatim. Otherwise the slice-2 generic
// subject and body are used.
//
// SoleFailureMail receives a defensive copy of the report (the
// runner does NOT pass internal-mutable state). Implementations
// MUST return ASCII-safe strings — the runner does not transcode.
// The 4096-byte body cap (slice-2 FR-06) applies to the returned
// body: the runner truncates with the slice-2 truncation marker
// if the implementation returns more.
//
// Implementations MUST exclude secrets. The runner is
// content-agnostic (slice-2
// TestRunWarmupChecks_MailBody_ExcludesSecretsByDefault); the
// producer is the trust boundary
// (slice-4 TestWarmupMailBodyExcludesSecrets).
type CustomWarmupMail interface {
	SoleFailureMail(report WarmupReport) (subject, body string)
}

// tryCustomSoleFailureMail returns (subject, body, true) when every
// entry in report.Failures shares the same Check name AND the check
// registered for that name implements CustomWarmupMail. Returns
// ("", "", false) otherwise. The registered checks are looked up
// via the runner's check registry (slice 2's RunWarmupChecks
// constructs the registry from opts.Checks).
//
// The body return is truncated to slice-2 FR-06's 4096-byte cap
// with the slice-2 truncation marker if longer.
func tryCustomSoleFailureMail(report WarmupReport, checks []doctor.Check) (subject, body string, ok bool) {
	if len(report.Failures) == 0 {
		return "", "", false
	}
	firstCheck := report.Failures[0].Check
	for _, failure := range report.Failures[1:] {
		if failure.Check != firstCheck {
			return "", "", false
		}
	}
	for _, check := range checks {
		if check == nil || check.Name() != firstCheck {
			continue
		}
		custom, implementsCustom := check.(CustomWarmupMail)
		if !implementsCustom {
			return "", "", false
		}
		// Honor the documented defensive-copy guarantee: implementers
		// receive a copy whose slices do not alias the runner's backing
		// arrays, so they cannot mutate runner-owned report state.
		// WarmupCheckResult is all scalar fields, so copying the slices
		// fully isolates it.
		failures := append([]WarmupCheckResult(nil), report.Failures...)
		scopes := append([]ScopeWarmupResult(nil), report.ScopeResults...)
		for i := range scopes {
			scopes[i].CheckResults = append([]WarmupCheckResult(nil), scopes[i].CheckResults...)
		}
		reportCopy := report
		reportCopy.Failures = failures
		reportCopy.ScopeResults = scopes
		subject, body = custom.SoleFailureMail(reportCopy)
		return subject, truncateWarmupMailBody(body), true
	}
	return "", "", false
}
