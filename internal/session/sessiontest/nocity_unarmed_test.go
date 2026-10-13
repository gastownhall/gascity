package sessiontest_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/session"
)

// noCityUnarmedProbeEnv runs TestNoCityRefusalWithoutTheGateExits as the
// probe in a child test binary.
const noCityUnarmedProbeEnv = "GC_TEST_NOCITY_UNARMED_PROBE"

// TestNoCityRefusalWithoutTheGateExits pins the gate's other half on this
// package, whose TestMain does not run session.FailOnNoCityRefusals: an
// undeclared refusal has no epilogue to report it, so it reports itself and
// exits the test binary at once, and the test that made it never passes.
func TestNoCityRefusalWithoutTheGateExits(t *testing.T) {
	if os.Getenv(noCityUnarmedProbeEnv) != "" {
		_ = session.RefuseWithoutCity("", "the unarmed self-test probe")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNoCityRefusalWithoutTheGateExits$", "-test.count=1", "-test.v")
	cmd.Env = append(bazeltest.HelperProcessEnv(os.Environ()), noCityUnarmedProbeEnv+"=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("probe run = %v, want exit 1:\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "no-city guard:") || !strings.Contains(got, "the unarmed self-test probe") || strings.Contains(got, "--- PASS") {
		t.Fatalf("probe output, want the report and no test passing:\n%s", got)
	}
}
