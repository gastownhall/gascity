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

// TestSecretEnvValuesFollowsTheAllowList pins which of a pane's env values are
// withheld from error text. The predicate is an allow list, so an unrecognized
// name is assumed to carry credential material; argv-safe values stay legible,
// since they are already readable in /proc/<pid>/cmdline and hiding them costs
// diagnostics for nothing.
func TestSecretEnvValuesFollowsTheAllowList(t *testing.T) {
	got := secretEnvValues(map[string]string{
		"ANTHROPIC_AUTH_TOKEN": sentinel,
		"SOME_NEW_TOKEN":       "unknown-name-so-assumed-secret",
		"GC_RIG":               "hauler",
		"ANTHROPIC_API_KEY":    "", // withheld variable: no value, nothing to hide
	})
	want := map[string]bool{sentinel: true, "unknown-name-so-assumed-secret": true}
	if len(got) != len(want) {
		t.Fatalf("secretEnvValues = %q, want the two secret values", got)
	}
	for _, v := range got {
		if !want[v] {
			t.Errorf("secretEnvValues included %q, which is not a credential", v)
		}
	}
}

// TestRedactArgsKeepsKeysAndDropsSecretValues pins what an error message may say
// about a herdr argv. The key has to survive — "which variable did herdr choke
// on" is the whole diagnostic value of printing the argv — while the value must
// not.
func TestRedactArgsKeepsKeysAndDropsSecretValues(t *testing.T) {
	env := map[string]string{"GC_RIG": "hauler", "ANTHROPIC_AUTH_TOKEN": sentinel}
	args := []string{
		"workspace", "create", "--label", "rig-a", "--cwd", "/data/projects/x",
		"--env", "GC_RIG=hauler",
		"--env", "ANTHROPIC_AUTH_TOKEN=" + sentinel,
		"--no-focus",
	}
	got := strings.Join(redactArgs(args, secretEnvValues(env)), " ")

	if strings.Contains(got, sentinel) {
		t.Errorf("redacted argv still carries the credential value: %s", got)
	}
	// The marker, not just the absence of the value: a redactor that dropped the
	// assignment altogether would also pass the check above, and a reader could
	// not tell a redacted value from an empty one.
	if !strings.Contains(got, "ANTHROPIC_AUTH_TOKEN="+redactedValue) {
		t.Errorf("redacted argv dropped the variable name, losing the diagnostic: %s", got)
	}
	if !strings.Contains(got, "GC_RIG=hauler") {
		t.Errorf("redacted argv dropped an argv-safe value: %s", got)
	}
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
	redactArgs(args, []string{sentinel})
	if args[3] != secret {
		t.Errorf("redactArgs rewrote the caller's argv: %q", args[3])
	}
}

// TestPaneRunCommandWithholdsTheWholeCommand pins the raw-launch contract. A
// session's configured command is a user-authored shell string, so a credential
// in it can sit anywhere a shell would still honor: after an `env` wrapper,
// past a `&&`, inside a nested `sh -c`, or spelled with quotes that keep the
// value from matching its own rendering. Any of those defeats a scanner, so the
// operand is withheld whole.
func TestPaneRunCommandWithholdsTheWholeCommand(t *testing.T) {
	for _, command := range []string{
		"exec /bin/sh -c 'ANTHROPIC_API_KEY=" + sentinel + " claude'",
		"exec /bin/sh -c 'env ANTHROPIC_API_KEY=" + sentinel + " claude'",
		"exec /bin/sh -c 'cd /data && ANTHROPIC_API_KEY=" + sentinel + " ./run.sh'",
		"exec /bin/sh -c 'sh -c '\\''ANTHROPIC_API_KEY=" + sentinel + " claude'\\'''",
	} {
		c := &client{session: "gc-test", bin: writeFakeHerdr(t, echoArgvScript)}
		err := c.paneRunCommand(context.Background(), "%12", command)
		if err == nil {
			t.Fatal("paneRunCommand against a failing herdr returned no error")
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("raw launch leaked a credential from %q: %v", command, err)
		}
		// The control: herdr's own text reached the message, so redaction is
		// what removed the credential rather than the credential never arriving.
		if !strings.Contains(err.Error(), "unexpected argument") {
			t.Fatalf("herdr's own output never reached the error, so this proves nothing: %v", err)
		}
		// The error still has to say what failed and where.
		if !strings.Contains(err.Error(), "pane run") || !strings.Contains(err.Error(), "%12") {
			t.Errorf("raw-launch error stopped naming the verb and pane: %v", err)
		}
	}
}

// TestPaneRunLeavesPastedTextAlone pins the other side of that split. The same
// verb delivers an agent's prompt or nudge through pasteAndSubmit, and prose is
// not a command: treating any word containing "=" as an assignment would make
// its tail a secret, and redaction substitutes values, so that string would then
// vanish from every error the client renders.
func TestPaneRunLeavesPastedTextAlone(t *testing.T) {
	c := &client{session: "gc-test", bin: writeFakeHerdr(t, echoArgvScript)}
	text := "Rerun the drain with mode=no so nothing merges, then report."

	err := c.paneRun(context.Background(), "%12", text)
	if err == nil {
		t.Fatal("paneRun against a failing herdr returned no error")
	}
	if !strings.Contains(err.Error(), text) {
		t.Errorf("pasted prose was rewritten in the error: %v", err)
	}
}

// TestPromptRedactionDoesNotBreakNotFoundMatching is why that matters.
// deliverNudge and deliverStartupTurn degrade to the paste+Enter path when
// herdr reports the agent is not registered, and isAgentNotFound decides that by
// matching the message text; runtime.IsSessionGone matches "not found" the same
// way to tell a benign missing session from a real failure. Redacting "no" out
// of a nudge would take the "no" in "not found" with it and flip both verdicts —
// a redactor breaking control-flow branches it has no business touching.
//
// Both delivery verbs are covered: the prompt operand of `agent prompt` and the
// pasted operand of `pane run`.
func TestPromptRedactionDoesNotBreakNotFoundMatching(t *testing.T) {
	const text = "Rerun the drain with mode=no so nothing merges."
	script := "#!/bin/sh\necho 'agent not found: %12' >&2\nexit 1\n"

	for _, tc := range []struct {
		name string
		call func(c *client) error
	}{
		{"agent prompt", func(c *client) error {
			return c.agentPrompt(context.Background(), "%12", text)
		}},
		{"pane run paste", func(c *client) error {
			return c.paneRun(context.Background(), "%12", text)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(&client{session: "gc-test", bin: writeFakeHerdr(t, script)})
			if err == nil {
				t.Fatal("call against a failing herdr returned no error")
			}
			if !isAgentNotFound(err) {
				t.Errorf("redaction mangled the message isAgentNotFound reads, so the fallback is dead: %v", err)
			}
		})
	}
}

// TestClientRunErrorsOmitCredentials is the end-to-end assertion behind the unit
// tests above: a herdr invocation that fails must not put the credential into
// the error it returns. Errors from this client reach logs, events and bead
// notes, so a value here outlives the process it leaked from — which makes it a
// worse exposure than argv, not a lesser one.
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

	if _, err := c.runWithSecrets(context.Background(), []string{sentinel}, args...); err == nil {
		t.Fatal("run against a missing binary returned no error")
	} else {
		assertRedactedArgvError(t, "run", err)
	}

	if _, err := c.runRawWithSecrets(context.Background(), []string{sentinel}, args...); err == nil {
		t.Fatal("runRaw against a missing binary returned no error")
	} else {
		assertRedactedArgvError(t, "runRaw", err)
	}
}

// TestWorkspaceAndTabCreateOmitCredentials pins that the two verbs which
// actually put credentials in a herdr argv declare them. They are the reason
// this file exists: the pane's environment is how ANTHROPIC_API_KEY,
// ANTHROPIC_AUTH_TOKEN and GC_INSTANCE_TOKEN reach an agent.
func TestWorkspaceAndTabCreateOmitCredentials(t *testing.T) {
	env := map[string]string{"ANTHROPIC_AUTH_TOKEN": sentinel, "GC_RIG": "hauler"}

	for _, tc := range []struct {
		name string
		call func(c *client) error
	}{
		{"workspace create", func(c *client) error {
			_, _, err := c.workspaceCreate(context.Background(), "rig-a", "/data/projects/x", env)
			return err
		}},
		{"tab create", func(c *client) error {
			_, _, err := c.tabCreate(context.Background(), "ws-1", "agent-a", "/data/projects/x", env)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(&client{session: "gc-test", bin: writeFakeHerdr(t, echoArgvScript)})
			if err == nil {
				t.Fatal("create against a failing herdr returned no error")
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("leaks the credential: %v", err)
			}
			if !strings.Contains(err.Error(), "ANTHROPIC_AUTH_TOKEN="+redactedValue) {
				t.Errorf("does not name the redacted assignment, so this test proves nothing: %v", err)
			}
			if !strings.Contains(err.Error(), "GC_RIG=hauler") {
				t.Errorf("redacted an argv-safe value: %v", err)
			}
		})
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
	secrets := []string{sentinel}

	for _, tc := range []struct {
		name string
		// script echoes the whole argv back the way a CLI reports a bad operand.
		script string
		// echoed is text only the subprocess can have produced, so finding it
		// proves the echo reached the message and redaction is what removed the
		// credential from it — rather than the credential never arriving.
		echoed string
	}{
		{name: "stderr", script: echoArgvScript, echoed: "unexpected argument"},
		{
			name:   "error envelope",
			script: "#!/bin/sh\nprintf '{\"error\":{\"code\":\"invalid_argument\",\"message\":\"invalid value in %s\"}}' \"$*\"\n",
			echoed: "invalid value in",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &client{session: "gc-test", bin: writeFakeHerdr(t, tc.script)}

			_, runErr := c.runWithSecrets(context.Background(), secrets, args...)
			if runErr == nil {
				t.Fatal("run against a failing herdr returned no error")
			}
			assertEchoRedacted(t, "run", runErr, tc.echoed)

			_, rawErr := c.runRawWithSecrets(context.Background(), secrets, args...)
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

	_, err := c.runWithSecrets(context.Background(), []string{sentinel},
		"agent", "start", "--env", "ANTHROPIC_AUTH_TOKEN="+sentinel)
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

// TestClientDecodeErrorsOmitCredentials covers the remaining error path in run:
// herdr exits clean but hands back something that is not an envelope. It
// renders the argv like every other path and so needs the same scrub.
func TestClientDecodeErrorsOmitCredentials(t *testing.T) {
	c := &client{session: "gc-test", bin: writeFakeHerdr(t, "#!/bin/sh\necho 'not json at all'\n")}

	_, err := c.runWithSecrets(context.Background(), []string{sentinel},
		"workspace", "create", "--env", "ANTHROPIC_AUTH_TOKEN="+sentinel)
	if err == nil {
		t.Fatal("run against a herdr returning garbage succeeded")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("expected the decode branch, got: %v", err)
	}
	assertRedactedArgvError(t, "run decode", err)
}

// echoArgvScript is a herdr that rejects its argv the way a CLI reports a bad
// operand: by quoting it back.
const echoArgvScript = "#!/bin/sh\necho \"error: unexpected argument '$*' found\" >&2\nexit 2\n"

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
