package runproj

import "testing"

// Regression test: iteration retries carry .attempt.N-suffixed step ids
// (repair-pre-review-ci-failures.attempt.1). aliasVariants must include the
// attempt-stripped form so those groups rank at their authored step position
// instead of falling to +Inf and sorting after the run's final steps.

func TestAliasVariantsIncludeAttemptStrippedForm(t *testing.T) {
	variants := aliasVariants("pre-review-ci.repair-pre-review-ci-failures.attempt.1", "")
	want := externalizeID("pre-review-ci.repair-pre-review-ci-failures")
	for _, v := range variants {
		if v == want {
			return
		}
	}
	t.Fatalf("aliasVariants = %v, want %q included", variants, want)
}
