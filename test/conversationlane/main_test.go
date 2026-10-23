//go:build integration

package conversationlane_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// The binaries the evidence runs, named by $(rootpath) in BUILD.bazel: the
// test-hooks gc, the only link that honors GC_LANE_FAILPOINT and
// GC_OPS_FAILPOINT, and the fake exec runtime.
const (
	gcBinEnv       = "GC_LANE_GC_BIN"
	fakeRuntimeEnv = "GC_LANE_FAKE_RUNTIME"
)

func init() { x12IntegrationBuild = true }

func TestMain(m *testing.M) {
	if os.Getenv(gcBinEnv) == "" || os.Getenv(fakeRuntimeEnv) == "" {
		if bazeltest.IsBazel() {
			fmt.Fprintf(os.Stderr, "conversationlane: %s and %s must name the test-hooks gc and the fake runtime (BUILD.bazel stages them under --config=integration)\n", gcBinEnv, fakeRuntimeEnv)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "conversationlane: skipped: run `bazel test --config=ci --config=integration //test/conversationlane:conversationlane_test`; only Bazel links the test-hooks gc these tests kill\n")
		os.Exit(0)
	}
	code := m.Run()
	writeX12Report(code)
	os.Exit(code)
}
