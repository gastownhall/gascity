package herdr

import (
	"context"
	"os"
	"path/filepath"
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
	// The marker, not just the absence of the value: a redactor that dropped the
	// assignment altogether would also pass the check above, and a reader could
	// not tell a redacted value from an empty one.
	if !strings.Contains(got, "ANTHROPIC_AUTH_TOKEN="+redactedValue) {
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

// TestRedactArgsDoesNotMutateItsInput pins the copy. The caller still holds the
// real argv — run passes the same slice to exec — so a redactor that rewrote in
// place would corrupt the invocation rather than only its error text, and no
// assertion on the returned value would notice.
func TestRedactArgsDoesNotMutateItsInput(t *testing.T) {
	secret := "ANTHROPIC_AUTH_TOKEN=" + sentinel
	args := []string{"workspace", "create", "--env", secret}
	redactArgs(args)
	if args[3] != secret {
		t.Errorf("redactArgs rewrote the caller's argv: %q", args[3])
	}
}

// TestRedactArgsRedactsEnvPrefixInsideACommandString covers the shape a raw
// launch takes. `pane run` carries an entire shell command as a single argv
// element, so an env-prefix assignment there is a token inside one element
// rather than an element of its own — and launchSpecFor sends every command
// containing "=" down exactly that path. A redactor that only understood the
// `--env KEY=VALUE` flag pair would render this one verbatim.
func TestRedactArgsRedactsEnvPrefixInsideACommandString(t *testing.T) {
	args := []string{"pane", "run", "%12", "exec /bin/sh -c 'ANTHROPIC_API_KEY=" + sentinel + " claude'"}
	got := strings.Join(redactArgs(args), " ")
	if strings.Contains(got, sentinel) {
		t.Errorf("env-prefix assignment in a command string leaked the credential: %s", got)
	}
	if !strings.Contains(got, "ANTHROPIC_API_KEY="+redactedValue) {
		t.Errorf("env-prefix redaction dropped the variable name: %s", got)
	}
	// The rest of the command has to stay readable, or the error stops naming
	// what failed to launch.
	if !strings.Contains(got, "claude") {
		t.Errorf("env-prefix redaction swallowed the command: %s", got)
	}
}

// TestRedactArgsKeepsArgvSafeAssignmentsInCommandStrings is the other half:
// scanning inside a command string must still defer to the allow list, or every
// inert assignment in a failing command turns into <redacted> and the error
// stops being worth printing.
//
// A key the allow list does not know is redacted even here. That is deliberate
// — envArgvSafe is an allow list precisely so an unrecognized name is assumed
// to carry credential material — and it is the direction to err in: over-
// redaction costs a line of diagnostics, under-redaction is the bug.
func TestRedactArgsKeepsArgvSafeAssignmentsInCommandStrings(t *testing.T) {
	args := []string{"pane", "run", "%12", "exec /bin/sh -c 'GC_RIG=hauler make all'"}
	got := strings.Join(redactArgs(args), " ")
	if !strings.Contains(got, "GC_RIG=hauler") {
		t.Errorf("redacted an argv-safe assignment inside a command string: %s", got)
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

// TestRedactArgsHandlesEnvOperandWithoutAnAssignment pins the malformed case:
// an --env operand that is not KEY=VALUE carries nothing to redact and must
// come through untouched rather than being mangled into one.
func TestRedactArgsHandlesEnvOperandWithoutAnAssignment(t *testing.T) {
	got := strings.Join(redactArgs([]string{"tab", "create", "--env", "JUST_A_NAME", "--no-focus"}), " ")
	if got != "tab create --env JUST_A_NAME --no-focus" {
		t.Errorf("malformed --env operand mangled: %q", got)
	}
}

// TestClientRunErrorsOmitCredentials is the end-to-end assertion behind the
// unit tests above: a herdr invocation that fails must not put the credential
// into the error it returns. Errors from this client reach logs, events and
// bead notes, so a value here outlives the process it leaked from — which makes
// it a worse exposure than argv, not a lesser one.
//
// A binary name that cannot exist drives the transport-failure branch without
// needing herdr installed.
//
// Each case asserts the redacted assignment is present, not merely that the
// sentinel is absent. The absence alone is satisfied by an error that stopped
// rendering the argv at all, which would make this test permanently vacuous the
// next time someone simplifies the message.
func TestClientRunErrorsOmitCredentials(t *testing.T) {
	c := &client{session: "gc-test", bin: "herdr-does-not-exist-" + t.Name()}
	args := []string{"workspace", "create", "--env", "ANTHROPIC_AUTH_TOKEN=" + sentinel}

	if _, err := c.run(context.Background(), args...); err == nil {
		t.Fatal("run against a missing binary returned no error")
	} else {
		assertRedactedArgvError(t, "run", err)
	}

	if _, err := c.runRaw(context.Background(), args...); err == nil {
		t.Fatal("runRaw against a missing binary returned no error")
	} else {
		assertRedactedArgvError(t, "runRaw", err)
	}
}

// TestClientErrorsRedactCredentialsHerdrEchoedBack covers the channel the argv
// redaction alone does not close. herdr echoes the offending operand on its
// ordinary failure paths — an unknown flag or a rejected assignment comes back
// as `unexpected argument 'KEY=…'` — and both run and runRaw append that text,
// verbatim, next to the argv they just redacted. Version skew makes this
// routine rather than exotic: --env is a 0.7.5 capability, and an older herdr
// rejects it by quoting the operand.
//
// Both transports are covered for both verbs: stderr from a non-zero exit, and
// a JSON error envelope from a clean one.
func TestClientErrorsRedactCredentialsHerdrEchoedBack(t *testing.T) {
	args := []string{"workspace", "create", "--env", "ANTHROPIC_AUTH_TOKEN=" + sentinel}

	for _, tc := range []struct {
		name string
		// script echoes the whole argv back the way a CLI reports a bad operand.
		script string
		// echoed is text only the subprocess can have produced, so finding it
		// proves the echo reached the message and redaction is what removed the
		// credential from it — rather than the credential never arriving.
		echoed string
	}{
		{
			name:   "stderr",
			script: "#!/bin/sh\necho \"error: unexpected argument '$*' found\" >&2\nexit 2\n",
			echoed: "unexpected argument",
		},
		{
			name:   "error envelope",
			script: "#!/bin/sh\nprintf '{\"error\":{\"code\":\"invalid_argument\",\"message\":\"invalid value in %s\"}}' \"$*\"\n",
			echoed: "invalid value in",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &client{session: "gc-test", bin: writeFakeHerdr(t, tc.script)}

			_, runErr := c.run(context.Background(), args...)
			if runErr == nil {
				t.Fatal("run against a failing herdr returned no error")
			}
			assertEchoRedacted(t, "run", runErr, tc.echoed)

			_, rawErr := c.runRaw(context.Background(), args...)
			if rawErr == nil {
				t.Fatal("runRaw against a failing herdr returned no error")
			}
			assertEchoRedacted(t, "runRaw", rawErr, tc.echoed)
		})
	}
}

// TestHerdrErrorCodeSurvivesRedaction pins that scrubbing the message keeps the
// typed error recoverable. Callers branch on specific herdr failures — Start
// adopts rather than reaps on "agent_name_taken" — so a redaction that returned
// a plain error would silently turn those branches off.
func TestHerdrErrorCodeSurvivesRedaction(t *testing.T) {
	script := "#!/bin/sh\nprintf '{\"error\":{\"code\":\"agent_name_taken\",\"message\":\"rejected %s\"}}' \"$*\"\n"
	c := &client{session: "gc-test", bin: writeFakeHerdr(t, script)}

	_, err := c.run(context.Background(), "agent", "start", "--env", "ANTHROPIC_AUTH_TOKEN="+sentinel)
	if err == nil {
		t.Fatal("run against a failing herdr returned no error")
	}
	if got := herdrErrorCode(err); got != "agent_name_taken" {
		t.Errorf("herdrErrorCode = %q after redaction, want %q (err: %v)", got, "agent_name_taken", err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("redacted envelope error leaks the credential: %v", err)
	}
}

// assertRedactedArgvError requires both halves of the contract: the credential
// is gone, and the assignment it came from is still named.
func assertRedactedArgvError(t *testing.T, what string, err error) {
	t.Helper()
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("%s error leaks the credential: %v", what, err)
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_AUTH_TOKEN="+redactedValue) {
		t.Errorf("%s error does not name the redacted assignment, so this test proves nothing: %v", what, err)
	}
}

// assertEchoRedacted requires the subprocess's own text to be present and
// scrubbed. Without the echoed marker the test would pass against a client that
// never appended herdr's output at all, which is the wrong reason to be green.
func assertEchoRedacted(t *testing.T, what string, err error, echoed string) {
	t.Helper()
	if !strings.Contains(err.Error(), echoed) {
		t.Fatalf("%s error does not carry herdr's own text %q, so the redaction it asserts is untested: %v", what, echoed, err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("%s error leaks a credential herdr echoed back: %v", what, err)
	}
}

// writeFakeHerdr drops an executable stand-in for the herdr CLI and returns its
// path.
func writeFakeHerdr(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herdr")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake herdr: %v", err)
	}
	return path
}
