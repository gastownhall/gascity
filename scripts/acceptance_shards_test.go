package scripts_test

import "testing"

// acceptanceSuite is the acceptance lane's sharded target
// (//test/acceptance:acceptance_test, --config=acceptance builds it with
// --define=gotags=acceptance_a on linux/amd64 workers).
var acceptanceSuite = shardedGoTest{
	build:      "test/acceptance/BUILD.bazel",
	rule:       "acceptance_test",
	dir:        "test/acceptance",
	heavyTests: "test/acceptance/heavy_tests.txt",
	tags:       map[string]bool{"acceptance_a": true, "linux": true, "amd64": true},
}

// TestAcceptanceShardsKeepHeavyTestsApart guards the acceptance lane's wall
// time the way TestIntegrationShardsKeepHeavyTestsApart guards integration's:
// the lane is as slow as its slowest shard, and two of the long tests in
// test/acceptance/heavy_tests.txt in one shard is how a 2-shard target spent
// 496 s in one shard (bazel.yml run 37434077277).
func TestAcceptanceShardsKeepHeavyTestsApart(t *testing.T) {
	checkHeavyTestsApart(t, acceptanceSuite)
}
