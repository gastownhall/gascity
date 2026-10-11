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

// Regression test: the compiled formula's preview only carries iteration-1
// refs, so a later iteration's groups (pre-review-ci.iteration.2.repair-...)
// matched nothing and sorted after the run's final steps. aliasVariants must
// contribute iteration-agnostic forms so every iteration ranks at the authored
// step position (stable sort keeps iteration order within the tie).
func TestAliasVariantsIncludeIterationAgnosticForm(t *testing.T) {
	rank := aliasVariants("mol-adopt-pr-v2.pre-review-ci.iteration.1.repair-pre-review-ci-failures", "mol-adopt-pr-v2")
	group := aliasVariants("pre-review-ci.iteration.2.repair-pre-review-ci-failures.attempt.2", "")
	for _, r := range rank {
		for _, g := range group {
			if r == g {
				return
			}
		}
	}
	t.Fatalf("no shared alias between rank side %v and group side %v", rank, group)
}
