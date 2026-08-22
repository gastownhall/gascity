package herdr

import (
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// redactedValue replaces a credential in rendered error text.
const redactedValue = "<redacted>"

// Errors from this client reach logs, the event bus and bead notes, so a
// credential rendered into one outlives the process that leaked it — a durable
// exposure, unlike argv, which dies with the process. Every error path here
// renders the failed argv, and some also render text herdr wrote, so both need
// scrubbing.
//
// What must be scrubbed is supplied by the caller that built the argv, never
// inferred from the argv itself. A herdr argv arrives at [client.run] as a flat
// []string, and recovering "which element is a credential" from that is
// guesswork in both directions. `pane run <pane> <text>` is a shell command
// when Start launches a raw command and an agent's prose nudge when
// pasteAndSubmit delivers one, so no positional rule separates them; scanning
// content for KEY=VALUE instead reads the prose. Over-redaction is not merely
// noisy — [redactText] substitutes values, so a nudge mentioning `mode=no`
// would delete "no" from every error the client renders, including the "not
// found" that isAgentNotFound and [runtime.IsSessionGone] match on, turning a
// tolerated missing session into a hard failure.
//
// The producers know exactly what they put in: workspaceCreate and tabCreate
// hold the env map, and the raw launch holds the command string it built. They
// say; this file only substitutes.

// secretEnvValues returns the values of env that must not appear in error text.
// [runtime.ArgvSecretEnvValue] is the single shared predicate and an allow
// list: an unrecognized key is assumed to carry credential material. Values of
// argv-safe variables stay legible — they are already readable in
// /proc/<pid>/cmdline, so hiding them costs diagnostics and buys nothing.
func secretEnvValues(env map[string]string) []string {
	var secrets []string
	for k, v := range env {
		if runtime.ArgvSecretEnvValue(k, v) {
			secrets = append(secrets, v)
		}
	}
	return secrets
}

// redactText replaces every one of secrets with [redactedValue].
//
// It substitutes values rather than rewriting assignments, which is what lets
// one pass cover both the argv we render and the text herdr wrote. Keys survive
// either way: "which variable did herdr choke on" is the entire diagnostic
// value of printing an argv, and the key is not the secret.
func redactText(text string, secrets []string) string {
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, redactedValue)
	}
	return text
}

// redactArgs returns a copy of a herdr argv with secrets replaced, leaving the
// caller's slice untouched — run hands the same slice to exec, so rewriting in
// place would corrupt the invocation and not merely its error text.
func redactArgs(args, secrets []string) []string {
	if len(secrets) == 0 {
		return args
	}
	out := make([]string, len(args))
	for i, arg := range args {
		out[i] = redactText(arg, secrets)
	}
	return out
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
