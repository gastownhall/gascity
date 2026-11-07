package scripts_test

import "testing"

// beadsSuite and gastownSuite are the integration-packages lane's
// (//test:integration_packages under --config=integration) longest sharded
// targets. The lane is as slow as its slowest shard: unsharded, beads_test
// alone ran 181-350 s whenever it re-ran (bazel.yml runs 37856059872,
// 37861575630), more than double the lane's cmd/gc build-and-test chain.
var (
	beadsSuite = shardedGoTest{
		build:      "internal/beads/BUILD.bazel",
		rule:       "beads_test",
		dir:        "internal/beads",
		heavyTests: "internal/beads/heavy_tests.txt",
		tags:       integrationLaneTags,
	}
	gastownSuite = shardedGoTest{
		build:      "examples/gastown/BUILD.bazel",
		rule:       "gastown_test",
		dir:        "examples/gastown",
		heavyTests: "examples/gastown/heavy_tests.txt",
		tags:       integrationLaneTags,
	}
)

// TestBeadsShardsKeepHeavyTestsApart guards beads_test's per-shard time in
// the integration-packages lane the way TestIntegrationShardsKeepHeavyTestsApart
// guards //test/integration's.
func TestBeadsShardsKeepHeavyTestsApart(t *testing.T) {
	checkHeavyTestsApart(t, beadsSuite)
}

// TestGastownShardsKeepHeavyTestsApart guards gastown_test's per-shard time
// in the integration-packages lane.
func TestGastownShardsKeepHeavyTestsApart(t *testing.T) {
	checkHeavyTestsApart(t, gastownSuite)
}
