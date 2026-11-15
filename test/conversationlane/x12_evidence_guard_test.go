package conversationlane_test

// The X12 evidence variants (BUILD.bazel x12_evidence_*_test) select their
// tests by -test.run. Without --config=integration the integration-tagged
// files are not compiled, so the selection matches nothing and the variant
// would pass with no test run. This untagged guard is in every variant's
// selection and fails such a run.

import (
	"os"
	"testing"
)

// x12EvidenceEnv selects X12's evidence tier.
const x12EvidenceEnv = "GC_X12_EVIDENCE"

// x12IntegrationBuild is set by main_test.go, which only the integration
// build compiles.
var x12IntegrationBuild bool

func TestX12EvidenceNeedsTheIntegrationBuild(t *testing.T) {
	if os.Getenv(x12EvidenceEnv) == "1" && !x12IntegrationBuild {
		t.Fatalf("%s=1, but this binary was built without the integration tag, so it holds none of X12's tests: run the evidence with --config=integration", x12EvidenceEnv)
	}
}
