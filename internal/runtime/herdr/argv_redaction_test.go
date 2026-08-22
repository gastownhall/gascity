package herdr

import (
	"context"
	"strings"
	"testing"
)

// sentinel stands in for a credential. It is synthetic; no real value belongs in
// a test, a log, or a bead.
const sentinel = "sk-test-NOT-A-REAL-CREDENTIAL-8f3a21"

// TestRedactArgsKeepsKeysAndDropsSecretValues pins what an error message may say
// about a herdr argv. The key has to survive — "which variable did herdr choke
// on" is the whole diagnostic value of printing the argv — while the value must
// not, unless it is one the argv-safety allow list already lets travel in a
// command line.
func TestRedactArgsKeepsKeysAndDropsSecretValues(t *testing.T) {
	args := []string{
		"workspace", "create", "--label", "rig-a", "--cwd", "/data/projects/x",
		"--env", "GC_RIG=hauler",
		"--env", "ANTHROPIC_AUTH_TOKEN=" + sentinel,
		"--no-focus",
	}
	got := strings.Join(redactArgs(args), " ")

	if strings.Contains(got, sentinel) {
		t.Errorf("redacted argv still carries the credential value: %s", got)
	}
	if !strings.Contains(got, "ANTHROPIC_AUTH_TOKEN=") {
		t.Errorf("redacted argv dropped the variable name, losing the diagnostic: %s", got)
	}
	// GC_RIG is on the argv-safe allow list, so it was already legible in
	// /proc/<pid>/cmdline; redacting it would cost diagnostic value and buy
	// nothing.
	if !strings.Contains(got, "GC_RIG=hauler") {
		t.Errorf("redacted argv dropped an argv-safe value: %s", got)
	}
	// Non-env arguments are not secrets and must survive intact.
	for _, want := range []string{"workspace", "create", "--label", "rig-a", "--cwd", "/data/projects/x", "--no-focus"} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted argv dropped non-env argument %q: %s", want, got)
		}
	}
}

// TestRedactArgsLeavesNonEnvArgumentsAlone guards the obvious over-reach: a
// bare KEY=VALUE that is not the value of an --env flag is a positional
// argument, not an environment assignment, and rewriting it would corrupt the
// message.
// The assignment is deliberately not the last element: a scan that walks pairs
// stops one short of the end, so a KEY=VALUE parked in the final slot is never
// examined and would pass this test for the wrong reason.
func TestRedactArgsLeavesNonEnvArgumentsAlone(t *testing.T) {
	args := []string{"pane", "run", "--", "make", "BUILD_TAG=release", "all"}
	got := strings.Join(redactArgs(args), " ")
	if !strings.Contains(got, "BUILD_TAG=release") {
		t.Errorf("redacted a positional KEY=VALUE that is not an --env assignment: %s", got)
	}
}

// TestRedactArgsHandlesTrailingEnvFlag pins the boundary case: a dangling
// --env with no operand must not panic or eat past the end of the slice.
func TestRedactArgsHandlesTrailingEnvFlag(t *testing.T) {
	got := strings.Join(redactArgs([]string{"tab", "create", "--env"}), " ")
	if got != "tab create --env" {
		t.Errorf("trailing --env mangled: %q", got)
	}
}

// TestClientRunErrorsOmitCredentials is the end-to-end assertion behind the
// unit test above: a herdr invocation that fails must not put the credential
// into the error it returns. Errors from this client reach logs, events and
// bead notes, so a value here outlives the process it leaked from — which makes
// it a worse exposure than argv, not a lesser one.
//
// A binary name that cannot exist drives the transport-failure branch without
// needing herdr installed.
func TestClientRunErrorsOmitCredentials(t *testing.T) {
	c := &client{session: "gc-test", bin: "herdr-does-not-exist-" + t.Name()}
	args := []string{"workspace", "create", "--env", "ANTHROPIC_AUTH_TOKEN=" + sentinel}

	if _, err := c.run(context.Background(), args...); err == nil {
		t.Fatal("run against a missing binary returned no error")
	} else if strings.Contains(err.Error(), sentinel) {
		t.Errorf("run error leaks the credential: %v", err)
	}

	if _, err := c.runRaw(context.Background(), args...); err == nil {
		t.Fatal("runRaw against a missing binary returned no error")
	} else if strings.Contains(err.Error(), sentinel) {
		t.Errorf("runRaw error leaks the credential: %v", err)
	}
}
