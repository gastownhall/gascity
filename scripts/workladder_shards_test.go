package scripts_test

import "testing"

// workladderSuite is the unit lane's //internal/workladder:workladder_test,
// whose property sims each run for seconds to minutes.
var workladderSuite = shardedGoTest{
	build:      "internal/workladder/BUILD.bazel",
	rule:       "workladder_test",
	dir:        "internal/workladder",
	heavyTests: "internal/workladder/heavy_tests.txt",
	tags:       map[string]bool{"linux": true, "amd64": true},
}

// TestWorkladderShardsKeepHeavyTestsApart guards workladder_test's shards
// against the medium timeout (300 s): with two or three of the sims in
// internal/workladder/heavy_tests.txt in one shard, a loaded worker ran that
// shard past 300 s (ga-9yad1bw0).
func TestWorkladderShardsKeepHeavyTestsApart(t *testing.T) {
	checkHeavyTestsApart(t, workladderSuite)
}
