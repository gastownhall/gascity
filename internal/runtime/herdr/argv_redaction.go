package herdr

import (
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// redactedValue replaces a credential in rendered error text.
const redactedValue = "<redacted>"

// argvSecretValues collects the credential values a herdr argv carries.
//
// Errors from this client reach logs, the event bus and bead notes, so a
// credential rendered into one outlives the process that leaked it — a durable
// exposure, unlike argv, which dies with the process. This is the set of values
// that must not survive into any of that text, whether the text is our own
// rendering of the argv or output the herdr subprocess handed back.
//
// Two shapes carry credentials:
//
//   - The operand of an `--env` flag, which gascity always builds as KEY=VALUE
//     (workspaceCreate, tabCreate).
//   - An assignment embedded in a command string. `pane run` takes a whole
//     shell command as a single argv element — `exec /bin/sh -c 'K=V claude'` —
//     so an env-prefix assignment there is one token inside one element rather
//     than an element of its own. launchSpecFor routes any command containing
//     "=" down exactly that path, which is what makes this shape reachable.
//
// Whether a value is a credential is [runtime.ArgvSecretEnvValue]'s call, and
// that predicate is an allow list: an unrecognized key is assumed secret. So a
// non-credential assignment in a command string is redacted too. That is the
// direction to err in — over-redaction costs a line of diagnostics, and
// under-redaction is the bug this file exists to prevent.
func argvSecretValues(args []string) []string {
	var secrets []string
	add := func(assignment string) {
		key, value, ok := strings.Cut(assignment, "=")
		if ok && runtime.ArgvSecretEnvValue(key, value) {
			secrets = append(secrets, value)
		}
	}
	for i, arg := range args {
		if arg == "--env" {
			continue
		}
		if i > 0 && args[i-1] == "--env" {
			// The whole operand is one assignment; a value may contain spaces.
			add(arg)
			continue
		}
		for _, token := range strings.Fields(arg) {
			add(strings.Trim(token, `'"`))
		}
	}
	return secrets
}

// redactText replaces every one of secrets with [redactedValue].
//
// It substitutes values rather than rewriting assignments, which is what lets
// one pass cover both the argv we render and the text herdr wrote. The keys
// survive either way: "which variable did herdr choke on" is the entire
// diagnostic value of printing the argv, and the key is not the secret.
//
// A short secret value can appear inside unrelated text and take it with it.
// That is over-redaction, so it is left alone rather than guarded by a
// minimum-length rule: the guard would be a judgement call in Go, and it would
// fail open on exactly the values most worth hiding.
func redactText(text string, secrets []string) string {
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, redactedValue)
	}
	return text
}

// redactArgs returns a copy of a herdr argv safe to put in an error message.
//
// Values of argv-safe variables survive: they are already legible in
// /proc/<pid>/cmdline, so hiding them costs diagnostics and buys nothing.
//
// Callers pass the argv on every error path rather than redacting once up
// front, so the allocation lands only when something has already failed.
func redactArgs(args []string) []string {
	secrets := argvSecretValues(args)
	if len(secrets) == 0 {
		return args
	}
	out := make([]string, len(args))
	for i, arg := range args {
		out[i] = redactText(arg, secrets)
	}
	return out
}

// redacted returns a copy of the herdr-reported error with its message scrubbed
// of the credentials argv carried, preserving Code so herdrErrorCode still
// matches.
//
// herdr echoes the offending operand on the ordinary failure paths — an
// unknown flag or a rejected assignment comes back as `invalid value
// 'KEY=sk-…'`. Rendering that verbatim next to a redacted argv puts the
// credential right back in the message the argv redaction just removed it from.
func (e *herdrError) redacted(secrets []string) *herdrError {
	if e == nil {
		return nil
	}
	return &herdrError{Code: e.Code, Message: redactText(e.Message, secrets)}
}
