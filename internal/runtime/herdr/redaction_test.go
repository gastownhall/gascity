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

// TestRedactedArgvFindsEnvValuesStructurally pins the rule that makes the env
// channel safe without anyone declaring anything: the element after `--env` is a
// KEY=VALUE pair by construction, because this client is what emitted it. The
// key has to survive — "which variable did herdr choke on" is the whole
// diagnostic value of printing an argv — while the value must not.
func TestRedactedArgvFindsEnvValuesStructurally(t *testing.T) {
	args := []string{
		"workspace", "create", "--label", "rig-a", "--cwd", "/data/projects/x",
		"--env", "GC_RIG=hauler",
		"--env", "ANTHROPIC_AUTH_TOKEN=" + sentinel,
		"--no-focus",
	}
	safe, secrets := redactedArgv(args, nil)
	got := strings.Join(safe, " ")

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
	// The values are returned as well as removed, because the same set has to
	// scrub stderr and the error envelope, which this file never sees.
	if len(secrets) != 1 || secrets[0] != sentinel {
		t.Errorf("redactedArgv secrets = %q, want just the credential", secrets)
	}
}

// TestRedactedArgvToleratesATrailingFlag pins both bounds checks, which fail
// differently and so need different assertions. This is a rendering path for an
// error that already happened, so neither failure is one a caller can act on.
//
// A trailing --env would index past the end: an index panic replacing a
// diagnosable failure with a crash. A trailing -- is legal Go — args[i+1:] is an
// empty slice — so dropping that check costs no panic and instead invents an
// argument out of nothing, appending a "<0 args withheld>" element to an argv
// that had nothing after the separator.
func TestRedactedArgvToleratesATrailingFlag(t *testing.T) {
	for _, args := range [][]string{
		{"workspace", "create", "--env"},
		{"agent", "start", "a", "--"},
	} {
		safe, secrets := redactedArgv(args, nil)
		if strings.Join(safe, " ") != strings.Join(args, " ") {
			t.Errorf("redactedArgv(%q) rewrote a trailing flag: %q", args, safe)
		}
		if len(secrets) != 0 {
			t.Errorf("redactedArgv(%q) invented secrets from nothing: %q", args, secrets)
		}
	}
	// The control: one more element after the separator and both the marker and
	// the secret do appear, so the assertions above are about the boundary and
	// not about the rule being off.
	safe, secrets := redactedArgv([]string{"agent", "start", "a", "--", sentinel}, nil)
	if !strings.Contains(strings.Join(safe, " "), "<1 args withheld>") {
		t.Fatalf("the withholding rule never fired, so the boundary cases prove nothing: %q", safe)
	}
	if len(secrets) != 1 {
		t.Fatalf("redactedArgv secrets = %q, want the one withheld argument", secrets)
	}
}

// TestLaunchArgsAfterTheSeparatorAreWithheld covers the other half of the
// user-authored launch command. launchSpecFor sends a command with no shell
// metacharacters down the Argv path, so `claude --api-key sk-…` — no "=", so
// nothing routes it to Raw — reaches herdr as `agent start … -- <args…>` and
// would otherwise render into every error verbatim.
//
// Withholding alone is not enough — herdr quotes the operand back on its
// ordinary failure paths, so the tail also has to reach the secret set. The
// floor is what makes that safe: a short flag token like "--print" is left
// alone rather than deleted from unrelated text, which the last assertion pins.
func TestLaunchArgsAfterTheSeparatorAreWithheld(t *testing.T) {
	c := &client{session: "gc-test", bin: writeFakeHerdr(t, echoArgvScript)}

	_, err := c.startAgentKind(context.Background(), "agent-a", "claude", "%12",
		[]string{"--print", "--api-key", sentinel})
	if err == nil {
		t.Fatal("startAgentKind against a failing herdr returned no error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("launch argv leaked a credential: %v", err)
	}
	if !strings.Contains(err.Error(), "<3 args withheld>") {
		t.Errorf("launch argv was not withheld as a counted group: %v", err)
	}
	// The control: herdr's own text reached the message, and everything before
	// the separator — which is the whole diagnostic — is intact.
	if !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("herdr's own output never reached the error, so this proves nothing: %v", err)
	}
	for _, want := range []string{"agent", "start", "agent-a", "--kind", "claude", "--pane", "%12"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("withholding the launch argv also dropped %q: %v", want, err)
		}
	}
	// Putting the tail in the secret set costs nothing above the floor. "--print"
	// occurs only in what herdr echoed, so its survival is what separates
	// scrubbing the credential from shredding the operand.
	if !strings.Contains(err.Error(), "--print") {
		t.Errorf("a sub-floor launch flag was substituted out of herdr's echo: %v", err)
	}
}

// TestRedactedArgvDoesNotMutateItsInput pins the copy. The caller still holds
// the real argv — run passes the same slice to exec — so a redactor that rewrote
// in place would corrupt the invocation rather than only its error text, and no
// assertion on the returned value would notice.
func TestRedactedArgvDoesNotMutateItsInput(t *testing.T) {
	pair := "ANTHROPIC_AUTH_TOKEN=" + sentinel
	args := []string{"workspace", "create", "--env", pair}
	redactedArgv(args, []string{sentinel})
	if args[3] != pair {
		t.Errorf("redactedArgv rewrote the caller's argv: %q", args[3])
	}
}

// TestShortValuesAreRedactedInArgvButNotSubstituted pins the substitution floor,
// and it is a control-flow guard, not a cosmetic one. Not every unrecognized
// variable holds a long random string: GC_STARTUP_PROMPT_DELIVERED is set to "1"
// on this provider's own named-session path. Substituting a value that short
// into free text is blind to word boundaries, so it would rewrite the label, the
// cwd, an argv-safe GC_RUNTIME_EPOCH=1, and — the part that breaks behavior —
// herdr's "agent not found", which isAgentNotFound and runtime.IsSessionGone
// both decide by matching.
//
// So the short value is still withheld structurally, where the grammar knows
// exactly which bytes it is, and simply not hunted for anywhere else.
func TestShortValuesAreRedactedInArgvButNotSubstituted(t *testing.T) {
	args := []string{
		"workspace", "create", "--label", "rig-1", "--cwd", "/data/projects/x1",
		"--env", "GC_STARTUP_PROMPT_DELIVERED=1",
		"--env", "GC_RUNTIME_EPOCH=1",
	}
	safe, secrets := redactedArgv(args, nil)
	got := strings.Join(safe, " ")

	if !strings.Contains(got, "GC_STARTUP_PROMPT_DELIVERED="+redactedValue) {
		t.Errorf("short value was not withheld structurally: %s", got)
	}
	for _, want := range []string{"--label rig-1", "--cwd /data/projects/x1", "GC_RUNTIME_EPOCH=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("substituting a short value mangled %q: %s", want, got)
		}
	}
	if got := redactText("agent not found: %12", secrets); got != "agent not found: %12" {
		t.Errorf("substituting a short value rewrote the text control flow reads: %s", got)
	}
}

// TestIdentityValuesStayLegible pins BEADS_ACTOR alongside GC_AGENT. The two
// carry the same string — lifecycle sets both from AssigneeIdentifier — so
// listing one and not the other would declare a public identity secret, and
// substitution would then blank it out of every placement error including the
// GC_AGENT copy standing next to it.
func TestIdentityValuesStayLegible(t *testing.T) {
	const identity = "rig-a/agent-a"
	args := []string{
		"tab", "create",
		"--env", "GC_AGENT=" + identity,
		"--env", "BEADS_ACTOR=" + identity,
	}
	safe, secrets := redactedArgv(args, nil)
	got := strings.Join(safe, " ")

	if len(secrets) != 0 {
		t.Fatalf("an identity was treated as a credential: %q", secrets)
	}
	for _, want := range []string{"GC_AGENT=" + identity, "BEADS_ACTOR=" + identity} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted argv dropped %q: %s", want, got)
		}
	}
}

// TestPaneRunCommandWithholdsTheWholeCommand pins the raw-launch contract, the
// one place a producer still has to declare. A session's configured command is a
// user-authored shell string, so a credential in it can sit anywhere a shell
// would still honor: after an `env` wrapper, past a `&&`, inside a nested
// `sh -c`, or spelled with quotes that keep the value from matching its own
// rendering. Any of those defeats a scanner, so the operand is withheld whole.
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
// Both plain entry points are used, with nothing declared, because that is the
// claim: an `--env` credential is safe through a verb whose author never thought
// about redaction.
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

// TestWorkspaceAndTabCreateOmitCredentials pins that end to end for the two
// verbs that actually put credentials in a herdr argv. They are the reason this
// file exists: the pane's environment is how ANTHROPIC_API_KEY,
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

// TestClientDecodeErrorsOmitCredentials covers the remaining error path in run:
// herdr exits clean but hands back something that is not an envelope. It
// renders the argv like every other path and so needs the same scrub.
func TestClientDecodeErrorsOmitCredentials(t *testing.T) {
	c := &client{session: "gc-test", bin: writeFakeHerdr(t, "#!/bin/sh\necho 'not json at all'\n")}

	_, err := c.run(context.Background(), "workspace", "create", "--env", "ANTHROPIC_AUTH_TOKEN="+sentinel)
	if err == nil {
		t.Fatal("run against a herdr returning garbage succeeded")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("expected the decode branch, got: %v", err)
	}
	assertRedactedArgvError(t, "run decode", err)
}

// TestSetupCommandFailureOmitsCredentials covers the one leak that is not the
// client's. A failed pre_start command's output tail is appended to the error,
// and that command runs with the session env on its environment: `set -x` traces
// every expansion, and a failing curl prints the header it sent. The error is as
// durable as any other — it reaches logs, events and bead notes.
func TestSetupCommandFailureOmitsCredentials(t *testing.T) {
	env := map[string]string{"SOME_NEW_TOKEN": sentinel, "GC_RIG": "hauler"}
	p := &Provider{}

	err := p.runSetupCommand(context.Background(), `echo "auth: $SOME_NEW_TOKEN rig=$GC_RIG"; exit 1`, env)
	if err == nil {
		t.Fatal("a pre_start command exiting 1 returned no error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("pre_start failure leaks a credential from its own output: %v", err)
	}
	// The control: the tail reached the error, so redaction removed the value
	// rather than the output never arriving.
	if !strings.Contains(err.Error(), "rig=hauler") {
		t.Fatalf("the command's output never reached the error, so this proves nothing: %v", err)
	}
}

// TestSetupCommandFailureOmitsInheritedCredentials is the same leak from the
// other environment. runSetupCommand builds c.Env from os.Environ() before
// appending the session env, so a pre_start command echoes the controller's own
// credentials just as readily as the session's — a failing
// `curl -H "Authorization: Bearer $GITHUB_TOKEN"` prints the header it sent.
// Scrubbing only the session env leaves that one in a durable error.
func TestSetupCommandFailureOmitsInheritedCredentials(t *testing.T) {
	t.Setenv("SOME_INHERITED_TOKEN", sentinel)
	p := &Provider{}

	err := p.runSetupCommand(context.Background(),
		`echo "auth: $SOME_INHERITED_TOKEN home=$HOME"; exit 1`,
		map[string]string{"GC_RIG": "hauler"})
	if err == nil {
		t.Fatal("a pre_start command exiting 1 returned no error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("pre_start failure leaks an inherited credential: %v", err)
	}
	// The control: HOME is on the argv-safe allow list precisely so that
	// scrubbing the inherited environment does not rewrite every path in the
	// output. If this fails, the scrub is over-reaching.
	if !strings.Contains(err.Error(), "home="+os.Getenv("HOME")) {
		t.Errorf("scrubbing the inherited environment redacted an inert value: %v", err)
	}
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
