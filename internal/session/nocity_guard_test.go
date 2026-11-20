package session

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// noCityGateProbeEnv runs TestNoCityGateFailsTheRun as the probe in a child
// test binary: "undeclared" refuses with nothing declared, "declared" under
// ExpectNoCityRefusalsForTest.
const noCityGateProbeEnv = "GC_TEST_NOCITY_GATE_PROBE"

// TestNoCityGateFailsTheRun pins the gate (FailOnNoCityRefusals in this
// package's TestMain) on a child test binary: a refusal no test declared
// fails a run whose tests all pass, and reports itself; a declared one does
// not.
func TestNoCityGateFailsTheRun(t *testing.T) {
	if mode := os.Getenv(noCityGateProbeEnv); mode != "" {
		if mode == "declared" {
			ExpectNoCityRefusalsForTest(t)
		}
		_ = RefuseWithoutCity("", "the gate's self-test probe")
		return
	}
	for _, tc := range []struct {
		mode string
		fail bool
	}{{"undeclared", true}, {"declared", false}} {
		t.Run(tc.mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNoCityGateFailsTheRun$", "-test.count=1", "-test.v")
			cmd.Env = append(bazeltest.HelperProcessEnv(os.Environ()), noCityGateProbeEnv+"="+tc.mode)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if err != nil && !errors.As(err, &exit) {
				t.Fatalf("running the probe: %v\n%s", err, out)
			}
			got := string(out)
			if !strings.Contains(got, "--- PASS: TestNoCityGateFailsTheRun") {
				t.Fatalf("the probe test did not pass on its own:\n%s", got)
			}
			reported := strings.Contains(got, "no-city guard:") && strings.Contains(got, "the gate's self-test probe")
			if failed := err != nil; failed != tc.fail || reported != tc.fail {
				t.Fatalf("probe run failed %t, reported %t; want both %t:\n%s", failed, reported, tc.fail, got)
			}
		})
	}
}
