package scripts_test

// ciUnionRequiredChecks: the authoritative union of required contexts for a
// pull request into main, both from classic branch protection and from
// ruleset 14017226 (ci-speed-review-2026-11-20.md §3). Classic protection
// requires Check, the four Analyze legs, CI / required, "bazel test
// (side-by-side)" and "BUILD files in sync"; the ruleset requires the same
// list minus CI / required, plus the storeclass gate. The effective set a
// pull request must clear is the union: 9 contexts.
//
// Refresh by hand (there is no live-endpoint test: see the package doc of
// ci_pr_required_checks_test.go):
//
//	gh api repos/gastownhall/gascity/branches/main/protection \
//	  --jq '.required_status_checks.contexts'
//	gh api repos/gastownhall/gascity/rulesets/14017226 \
//	  --jq '.rules[] | select(.type == "required_status_checks") | .parameters.required_status_checks[].context'
//
// Union the two lists, update ciUnionRequiredChecks below, and update
// ciUnionRequiredCheckProducers in ci_pr_required_checks_test.go to name
// the job (and, for a matrix job, the matrix value) that reports each new
// or renamed context. A context with no producer fails
// TestPRRequiredChecksAllReportOnEveryRelevantEvent immediately, rather
// than stalling a real pull request the way #9114 did.
var ciUnionRequiredChecks = []string{
	"Check",
	"Analyze (actions)",
	"Analyze (go)",
	"Analyze (javascript-typescript)",
	"Analyze (python)",
	"CI / required",
	"bazel test (side-by-side)",
	"BUILD files in sync",
	"storeclass baseline and growth gate",
}
