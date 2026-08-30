package herdr

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// redactedValue replaces a credential in rendered error text.
const redactedValue = runtime.RedactedValue

// envFlag precedes each KEY=VALUE pair this client passes to herdr. The match
// is on a whole argv element, so prose cannot trip it: an agent's prompt or
// nudge reaches herdr as one operand, and an element that is exactly "--env"
// is one this client emitted.
const envFlag = "--env"

// argvSeparator ends this client's own flags and begins the launch argv herdr
// passes to the agent executable. Only [client.startAgentKind] emits one.
const argvSeparator = "--"

// Errors from this client reach logs, the event bus and bead notes, so a
// credential rendered into one outlives the process that leaked it — a durable
// exposure, unlike argv, which dies with the process. Every error path here
// renders the failed argv, and some also render text herdr wrote, so both need
// scrubbing.
//
// Two rules, deliberately different, because the two things being scrubbed are
// known in different ways.
//
// Values this client's own flag grammar identifies are found here, not
// declared: the element after `--env`, and everything after the `--` separator.
// That is not inference — the client emits both shapes itself. The two are
// rendered differently because they are known differently. An `--env` value is
// replaced in place, keeping its key, since the key is the diagnostic and is
// not the secret. A launch argument's tail is withheld as a counted group,
// because it comes from splitting a user-authored command and no element in it
// is known to be inert. Both then go into the secret set, which is what scrubs
// them from stderr and from herdr's own message.
//
// Doing this inside [client.run] makes both channels safe through every verb,
// including ones not yet written. Note that it holds only while `--env` is the
// sole way to give a herdr pane an environment; a private env channel would be
// better and would make this rule vestigial.
//
// Anything else must be declared by whoever built the argv, never recovered
// from it. A herdr argv arrives as a flat []string, and asking which element is
// a credential is guesswork in both directions: `pane run <pane> <text>`
// carries a shell command when Start launches a raw command and an agent's
// prose nudge when pasteAndSubmit delivers one, so no positional rule separates
// them, and scanning content for assignments reads the prose instead.

// redactText is [runtime.RedactSecrets].
func redactText(text string, secrets []string) string { return runtime.RedactSecrets(text, secrets) }

// redactedArgv returns a copy of a herdr argv safe to render, plus the
// credential values that have to be scrubbed from the rest of the error too.
// declared holds values the caller knows it put in and this file could not
// identify on its own.
//
// The copy matters: run hands the same slice to exec, so rewriting in place
// would corrupt the invocation and not merely its error text.
func redactedArgv(args, declared []string) (safe, secrets []string) {
	safe = make([]string, len(args))
	copy(safe, args)

	for i, arg := range args {
		if arg == argvSeparator && i+1 < len(args) {
			// The tail is the agent's own launch argv, straight from a
			// user-authored command string, so a credential in it can sit in any
			// element. Withheld as a group and counted rather than listed: the
			// count is the diagnostic, and the verb, agent name and kind are all
			// before the separator.
			//
			// Also added to the secret set, because withholding covers only the
			// argv we render and herdr quotes the operand back at us on its
			// ordinary failure paths. That is safe here only because
			// [runtime.RedactSecrets] refuses to substitute short values: these
			// are mostly flag tokens, and substituting a "-p" would delete it
			// from unrelated text. Tokens under that floor stay legible in the
			// echo, which is the same trade made everywhere else — nothing that
			// short is a credential.
			secrets = append(secrets, args[i+1:]...)
			safe = append(safe[:i+1], fmt.Sprintf("<%d args withheld>", len(args)-i-1))
			break
		}
		if arg != envFlag || i+1 >= len(args) {
			continue
		}
		key, value, ok := strings.Cut(args[i+1], "=")
		if !ok || !runtime.ArgvSecretEnvValue(key, value) {
			continue
		}
		// The key survives: "which variable did herdr choke on" is the whole
		// diagnostic value of printing an argv, and the key is not the secret.
		safe[i+1] = key + "=" + redactedValue
		secrets = append(secrets, value)
	}
	secrets = append(secrets, declared...)

	for i := range safe {
		safe[i] = redactText(safe[i], secrets)
	}
	return safe, secrets
}

// redacted returns a copy of the herdr-reported error with its message scrubbed,
// preserving Code so herdrErrorCode still matches.
//
// herdr echoes the offending operand on the ordinary failure paths — an unknown
// flag or a rejected assignment comes back as `invalid value 'KEY=sk-…'`.
// Rendering that verbatim next to a redacted argv puts the credential right back
// in the message the argv redaction just removed it from.
func (e *herdrError) redacted(secrets []string) *herdrError {
	if e == nil {
		return nil
	}
	return &herdrError{Code: e.Code, Message: redactText(e.Message, secrets)}
}
